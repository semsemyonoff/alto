package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/semsemyonoff/ALTO/internal/transcode"
)

// TestJobManager_CompleteStatusMapping verifies that complete() maps engine
// results to the correct terminal JobStatus, including context-canceled
// errors mapping to JobStatusCanceled rather than JobStatusFailed.
func TestJobManager_CompleteStatusMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus JobStatus
		wantErrMsg string
	}{
		{
			name:       "success",
			err:        nil,
			wantStatus: JobStatusDone,
			wantErrMsg: "",
		},
		{
			name:       "failure",
			err:        errors.New("boom"),
			wantStatus: JobStatusFailed,
			wantErrMsg: "boom",
		},
		{
			name:       "context canceled",
			err:        context.Canceled,
			wantStatus: JobStatusCanceled,
			wantErrMsg: "",
		},
		{
			name:       "wrapped context canceled",
			err:        fmt.Errorf("engine: %w", context.Canceled),
			wantStatus: JobStatusCanceled,
			wantErrMsg: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jm := newJobManager(nil, 0, context.Background())
			js, started := jm.start("job1", "/dir1", transcode.Job{}, "", "")
			if !started {
				t.Fatalf("start: expected success")
			}

			jm.complete(js.id, tt.err)

			jm.mu.Lock()
			gotStatus := js.status
			gotErrMsg := js.errMsg
			jm.mu.Unlock()

			if gotStatus != tt.wantStatus {
				t.Errorf("status = %q, want %q", gotStatus, tt.wantStatus)
			}
			if gotErrMsg != tt.wantErrMsg {
				t.Errorf("errMsg = %q, want %q", gotErrMsg, tt.wantErrMsg)
			}

			select {
			case <-js.done:
			default:
				t.Errorf("done channel not closed after complete()")
			}

			jm.mu.Lock()
			_, stillBusy := jm.byDir["/dir1"]
			jm.mu.Unlock()
			if stillBusy {
				t.Errorf("byDir entry not freed after complete()")
			}
		})
	}
}

// TestJobState_MetadataPersistence verifies the new title/sub/job/cancel/
// fanoutDone fields can be set and read back under jm.mu.
func TestJobState_MetadataPersistence(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	job := transcode.Job{ID: "job1", SourceDir: "/dir1"}
	js, started := jm.start("job1", "/dir1", job, "album1", "flac -> opus/Music Balanced")
	if !started {
		t.Fatalf("start: expected success")
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	jm.mu.Lock()
	js.cancel = cancel
	js.fanoutDone = make(chan struct{})
	jm.mu.Unlock()

	jm.mu.Lock()
	gotTitle := js.title
	gotSub := js.sub
	gotJob := js.job
	gotCancel := js.cancel
	gotFanoutDone := js.fanoutDone
	jm.mu.Unlock()

	if gotTitle != "album1" {
		t.Errorf("title = %q, want %q", gotTitle, "album1")
	}
	if gotSub != "flac -> opus/Music Balanced" {
		t.Errorf("sub = %q, want %q", gotSub, "flac -> opus/Music Balanced")
	}
	if gotJob.ID != job.ID || gotJob.SourceDir != job.SourceDir {
		t.Errorf("job = %+v, want %+v", gotJob, job)
	}
	if gotCancel == nil {
		t.Errorf("cancel func not persisted")
	}
	if gotFanoutDone == nil {
		t.Errorf("fanoutDone channel not persisted")
	}
}

// TestJobState_LatestRaceFree exercises concurrent updateLatest writers
// against concurrent readers of js.latest. Run with -race to confirm the
// collapse of subsMu into jobManager.mu introduced no unsynchronized access.
func TestJobState_LatestRaceFree(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	js, started := jm.start("job1", "/dir1", transcode.Job{}, "", "")
	if !started {
		t.Fatalf("start: expected success")
	}

	var wg sync.WaitGroup

	// Concurrent writers.
	for i := range 20 {
		wg.Go(func() {
			js.updateLatest(transcode.ProgressReport{FileIndex: i, TotalFiles: 20})
		})
	}

	// Concurrent readers.
	for range 20 {
		wg.Go(func() {
			jm.mu.Lock()
			_ = js.latest
			jm.mu.Unlock()
		})
	}

	wg.Wait()
}

// blockingEngine blocks in Transcode until block is closed, then returns err.
type blockingEngine struct {
	block chan struct{}
	err   error
}

func (e *blockingEngine) Transcode(_ context.Context, _ transcode.Job, _ chan<- transcode.ProgressReport) error {
	<-e.block
	return e.err
}

// ctxEngine blocks in Transcode until ctx is canceled, then returns ctx.Err().
type ctxEngine struct{}

func (ctxEngine) Transcode(ctx context.Context, _ transcode.Job, _ chan<- transcode.ProgressReport) error {
	<-ctx.Done()
	return ctx.Err()
}

// concurrencyTrackingEngine counts concurrent Transcode calls in flight and
// blocks on release, letting tests assert the achieved concurrency.
type concurrencyTrackingEngine struct {
	release chan struct{}

	mu      sync.Mutex
	current int
	maxSeen int
}

func (e *concurrencyTrackingEngine) Transcode(_ context.Context, _ transcode.Job, _ chan<- transcode.ProgressReport) error {
	e.mu.Lock()
	e.current++
	if e.current > e.maxSeen {
		e.maxSeen = e.current
	}
	e.mu.Unlock()

	<-e.release

	e.mu.Lock()
	e.current--
	e.mu.Unlock()
	return nil
}

// jobStatusFor returns the current status of id, or "" if unknown.
func jobStatusFor(jm *jobManager, id string) JobStatus {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	js, ok := jm.jobs[id]
	if !ok {
		return ""
	}
	return js.status
}

// waitForJobStatus polls until id reaches want or the timeout elapses.
func waitForJobStatus(t *testing.T, jm *jobManager, id string, want JobStatus, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if jobStatusFor(jm, id) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach status %q within %s (last status %q)", id, want, timeout, jobStatusFor(jm, id))
}

// TestJobManager_WorkersOneSerializes verifies that with workers=1 a second
// queued job stays queued while the first is running, and auto-starts once
// the first completes.
func TestJobManager_WorkersOneSerializes(t *testing.T) {
	block := make(chan struct{})
	eng := &blockingEngine{block: block}
	jm := newJobManager(eng, 1, context.Background())
	t.Cleanup(jm.Shutdown)

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "t1", "s1"); !started {
		t.Fatalf("start job1: expected success")
	}
	if _, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, "t2", "s2"); !started {
		t.Fatalf("start job2: expected success")
	}

	waitForJobStatus(t, jm, "job1", JobStatusRunning, 2*time.Second)

	// job2 must stay queued while the single worker is occupied by job1.
	time.Sleep(50 * time.Millisecond)
	if got := jobStatusFor(jm, "job2"); got != JobStatusQueued {
		t.Fatalf("job2 status = %q, want %q (stranded or started early)", got, JobStatusQueued)
	}

	close(block)

	waitForJobStatus(t, jm, "job1", JobStatusDone, 2*time.Second)
	waitForJobStatus(t, jm, "job2", JobStatusDone, 2*time.Second)

	jm.mu.Lock()
	stranded := jm.nextQueuedLocked()
	jm.mu.Unlock()
	if stranded != "" {
		t.Errorf("stranded queued job: %q", stranded)
	}
}

// TestJobManager_WorkersTwoConcurrent verifies that with workers=2 both queued
// jobs run concurrently and neither is left stranded in the queue.
func TestJobManager_WorkersTwoConcurrent(t *testing.T) {
	eng := &concurrencyTrackingEngine{release: make(chan struct{})}
	jm := newJobManager(eng, 2, context.Background())
	t.Cleanup(jm.Shutdown)

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "t1", "s1"); !started {
		t.Fatalf("start job1: expected success")
	}
	if _, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, "t2", "s2"); !started {
		t.Fatalf("start job2: expected success")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		eng.mu.Lock()
		seen := eng.maxSeen
		eng.mu.Unlock()
		if seen >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("both jobs never ran concurrently, maxSeen=%d", seen)
		}
		time.Sleep(5 * time.Millisecond)
	}

	close(eng.release)

	waitForJobStatus(t, jm, "job1", JobStatusDone, 2*time.Second)
	waitForJobStatus(t, jm, "job2", JobStatusDone, 2*time.Second)

	jm.mu.Lock()
	stranded := jm.nextQueuedLocked()
	jm.mu.Unlock()
	if stranded != "" {
		t.Errorf("stranded queued job: %q", stranded)
	}
}

// TestJobManager_CompleteOnlyAfterFanoutDrains verifies that by the time a job's
// done channel is closed, the fanout goroutine has already drained every
// progress report into the log (no progress event is lost or arrives after
// the terminal state is visible).
func TestJobManager_CompleteOnlyAfterFanoutDrains(t *testing.T) {
	reports := []transcode.ProgressReport{
		{CurrentFile: "a.flac", FileIndex: 0, TotalFiles: 2, FilePercent: 100},
		{CurrentFile: "b.flac", FileIndex: 1, TotalFiles: 2, FilePercent: 100},
	}
	eng := &mockEngine{reports: reports}
	jm := newJobManager(eng, 1, context.Background())
	t.Cleanup(jm.Shutdown)

	job := transcode.Job{ID: "job1", Preset: transcode.Preset{Codec: transcode.CodecOpus, Name: "Balanced"}}
	js, started := jm.start("job1", "/dir1", job, "t1", "s1")
	if !started {
		t.Fatalf("start: expected success")
	}

	select {
	case <-js.done:
	case <-time.After(2 * time.Second):
		t.Fatal("job did not complete in time")
	}

	lines := js.log.lines()
	if len(lines) != 4 {
		t.Fatalf("expected 4 log lines (start + 2 progress + complete) once done, got %d: %v", len(lines), lines)
	}

	jm.mu.Lock()
	latest := js.latest
	jm.mu.Unlock()
	if latest == nil || latest.CurrentFile != "b.flac" {
		t.Errorf("latest = %+v, want final report for b.flac", latest)
	}
}

// TestJobManager_OrderPreserved verifies that jm.order reflects registration
// order and is not reordered by dispatch or completion.
func TestJobManager_OrderPreserved(t *testing.T) {
	block := make(chan struct{})
	eng := &blockingEngine{block: block}
	jm := newJobManager(eng, 1, context.Background())
	t.Cleanup(jm.Shutdown)

	ids := []string{"job1", "job2", "job3"}
	for i, id := range ids {
		dir := fmt.Sprintf("/dir%d", i)
		if _, started := jm.start(id, dir, transcode.Job{ID: id}, id, ""); !started {
			t.Fatalf("start %s: expected success", id)
		}
	}

	jm.mu.Lock()
	order := append([]string(nil), jm.order...)
	jm.mu.Unlock()
	if len(order) != 3 || order[0] != "job1" || order[1] != "job2" || order[2] != "job3" {
		t.Fatalf("order = %v, want [job1 job2 job3]", order)
	}

	close(block)
	for _, id := range ids {
		waitForJobStatus(t, jm, id, JobStatusDone, 2*time.Second)
	}

	jm.mu.Lock()
	order = append([]string(nil), jm.order...)
	jm.mu.Unlock()
	if len(order) != 3 || order[0] != "job1" || order[1] != "job2" || order[2] != "job3" {
		t.Fatalf("order after completion = %v, want [job1 job2 job3] (terminal jobs stay in order until eviction)", order)
	}
}

// TestJobManager_WorkersExitOnShutdown verifies that Shutdown returns (workers
// exit) once an in-flight job's context is canceled, and that idle workers
// with nothing queued exit immediately. Run under -race to confirm no leak.
func TestJobManager_WorkersExitOnShutdown(t *testing.T) {
	t.Run("idle workers", func(t *testing.T) {
		jm := newJobManager(&ctxEngine{}, 3, context.Background())

		done := make(chan struct{})
		go func() {
			jm.Shutdown()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Shutdown did not return for idle workers")
		}
	})

	t.Run("in-flight job canceled via parent ctx", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		jm := newJobManager(&ctxEngine{}, 2, ctx)

		if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "t1", "s1"); !started {
			t.Fatalf("start: expected success")
		}
		waitForJobStatus(t, jm, "job1", JobStatusRunning, 2*time.Second)

		cancel()

		done := make(chan struct{})
		go func() {
			jm.Shutdown()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Shutdown did not return after parent ctx cancellation; workers may have leaked")
		}

		waitForJobStatus(t, jm, "job1", JobStatusCanceled, 2*time.Second)
	})
}

// evict forces id's scheduled 30-min eviction to run immediately, so tests
// don't have to wait on the real timer.
func evict(jm *jobManager, id string) {
	jm.mu.Lock()
	delete(jm.jobs, id)
	for i, oid := range jm.order {
		if oid == id {
			jm.order = append(jm.order[:i], jm.order[i+1:]...)
			break
		}
	}
	jm.mu.Unlock()
}

// TestJobManager_CancelQueued verifies that canceling a queued job marks it
// canceled without ever running it, that it stays listed (never removed from
// order) until eviction, and that eviction then removes it from both jobs
// and order.
func TestJobManager_CancelQueued(t *testing.T) {
	block := make(chan struct{})
	eng := &blockingEngine{block: block}
	jm := newJobManager(eng, 1, context.Background())
	t.Cleanup(jm.Shutdown)

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "t1", "s1"); !started {
		t.Fatalf("start job1: expected success")
	}
	if _, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, "t2", "s2"); !started {
		t.Fatalf("start job2: expected success")
	}
	waitForJobStatus(t, jm, "job1", JobStatusRunning, 2*time.Second)

	if got := jm.cancel("job2"); got != cancelResultCanceled {
		t.Fatalf("cancel(job2) = %v, want cancelResultCanceled", got)
	}

	jm.mu.Lock()
	gotStatus := jm.jobs["job2"].status
	_, inByDir := jm.byDir["/dir2"]
	doneCh := jm.jobs["job2"].done
	jm.mu.Unlock()
	if gotStatus != JobStatusCanceled {
		t.Fatalf("job2 status = %q, want %q", gotStatus, JobStatusCanceled)
	}
	if inByDir {
		t.Fatalf("job2 byDir entry not freed by cancel")
	}
	select {
	case <-doneCh:
	default:
		t.Fatalf("job2 done channel not closed by cancel")
	}

	// job2 must never be dispatched even after job1 finishes.
	close(block)
	waitForJobStatus(t, jm, "job1", JobStatusDone, 2*time.Second)
	time.Sleep(50 * time.Millisecond)
	if got := jobStatusFor(jm, "job2"); got != JobStatusCanceled {
		t.Fatalf("job2 status after job1 completed = %q, want %q (must not run)", got, JobStatusCanceled)
	}

	jm.mu.Lock()
	_, stillListed := jm.jobs["job2"]
	inOrderList := false
	for _, id := range jm.order {
		if id == "job2" {
			inOrderList = true
		}
	}
	jm.mu.Unlock()
	if !stillListed || !inOrderList {
		t.Fatalf("canceled queued job2 must stay listed until eviction (jobs present=%v, in order=%v)", stillListed, inOrderList)
	}

	evict(jm, "job2")
	jm.mu.Lock()
	_, stillListed = jm.jobs["job2"]
	inOrderList = false
	for _, id := range jm.order {
		if id == "job2" {
			inOrderList = true
		}
	}
	jm.mu.Unlock()
	if stillListed || inOrderList {
		t.Fatalf("job2 not removed from both jobs and order after eviction (jobs present=%v, in order=%v)", stillListed, inOrderList)
	}
}

// TestJobManager_CancelRunning verifies that canceling a running job fires
// its context, which the engine observes as ctx.Err(), and complete() maps
// that to JobStatusCanceled.
func TestJobManager_CancelRunning(t *testing.T) {
	jm := newJobManager(&ctxEngine{}, 1, context.Background())
	t.Cleanup(jm.Shutdown)

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "t1", "s1"); !started {
		t.Fatalf("start: expected success")
	}
	waitForJobStatus(t, jm, "job1", JobStatusRunning, 2*time.Second)

	if got := jm.cancel("job1"); got != cancelResultCanceled {
		t.Fatalf("cancel(job1) = %v, want cancelResultCanceled", got)
	}

	waitForJobStatus(t, jm, "job1", JobStatusCanceled, 2*time.Second)
}

// TestJobManager_CancelUnknownOrFinished verifies cancel's typed result for
// an unknown id and for a job that has already reached a terminal state.
func TestJobManager_CancelUnknownOrFinished(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	if got := jm.cancel("nope"); got != cancelResultNotFound {
		t.Fatalf("cancel(unknown) = %v, want cancelResultNotFound", got)
	}

	js, started := jm.start("job1", "/dir1", transcode.Job{}, "", "")
	if !started {
		t.Fatalf("start: expected success")
	}
	jm.complete(js.id, nil)

	if got := jm.cancel("job1"); got != cancelResultFinished {
		t.Fatalf("cancel(done) = %v, want cancelResultFinished", got)
	}
}

// TestJobManager_EventsInOrder verifies the global event bus emits update
// events in the same order the job progresses through its lifecycle: queued
// (on start), running (on dispatch), then done (on complete), each with the
// centrally-shaped pct.
func TestJobManager_EventsInOrder(t *testing.T) {
	block := make(chan struct{})
	eng := &blockingEngine{block: block}
	jm := newJobManager(eng, 1, context.Background())
	t.Cleanup(jm.Shutdown)

	ch, snapshot := jm.subscribeEventsWithSnapshot()
	t.Cleanup(func() { jm.unsubscribeEvents(ch) })
	if len(snapshot) != 0 {
		t.Fatalf("initial snapshot = %v, want empty", snapshot)
	}

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "title1", "sub1"); !started {
		t.Fatalf("start: expected success")
	}

	wantSeq := []JobStatus{JobStatusQueued, JobStatusRunning, JobStatusDone}
	for i, want := range wantSeq {
		if want == JobStatusDone {
			close(block)
		}
		select {
		case ev := <-ch:
			if ev.ID != "job1" {
				t.Fatalf("event[%d].ID = %q, want %q", i, ev.ID, "job1")
			}
			if ev.Status != want {
				t.Fatalf("event[%d].Status = %q, want %q", i, ev.Status, want)
			}
			if ev.Title != "title1" || ev.Sub != "sub1" {
				t.Fatalf("event[%d] = %+v, want title/sub preserved", i, ev)
			}
			if want == JobStatusDone && ev.Pct != 100 {
				t.Fatalf("event[%d].Pct = %v, want 100 for done", i, ev.Pct)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event[%d] (status %q)", i, want)
		}
	}
}

// TestJobManager_SubscribeSnapshotAtomic verifies that subscribeEventsWithSnapshot
// captures the current job list atomically with registration: a job started
// before subscribing shows up in the snapshot (not as a duplicate live event),
// and a state change made after subscribing arrives exactly once on the channel.
func TestJobManager_SubscribeSnapshotAtomic(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	js, started := jm.start("job1", "/dir1", transcode.Job{}, "t1", "s1")
	if !started {
		t.Fatalf("start: expected success")
	}

	ch, snapshot := jm.subscribeEventsWithSnapshot()
	t.Cleanup(func() { jm.unsubscribeEvents(ch) })
	if len(snapshot) != 1 || snapshot[0].ID != "job1" || snapshot[0].Status != JobStatusQueued {
		t.Fatalf("snapshot = %+v, want single queued job1", snapshot)
	}

	jm.complete(js.id, nil)

	select {
	case ev := <-ch:
		if ev.ID != "job1" || ev.Status != JobStatusDone || ev.Pct != 100 {
			t.Fatalf("event = %+v, want done job1 at 100", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the post-subscribe completion event")
	}

	select {
	case ev, open := <-ch:
		if open {
			t.Fatalf("unexpected extra event: %+v", ev)
		}
	default:
	}
}

// TestJobManager_SlowEventSubscriberDropped verifies that a subscriber whose
// buffered channel fills up is dropped (its channel closed and removed from
// the subscriber set) rather than allowed to block broadcastEvent.
func TestJobManager_SlowEventSubscriberDropped(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	ch, _ := jm.subscribeEventsWithSnapshot()

	js, started := jm.start("job1", "/dir1", transcode.Job{}, "t1", "s1")
	if !started {
		t.Fatalf("start: expected success")
	}

	// Flood well past the channel's buffer without ever reading from ch.
	done := make(chan struct{})
	go func() {
		for range 200 {
			jm.broadcastEvent(jobEvent{ID: js.id, Status: JobStatusRunning})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcastEvent blocked on a slow subscriber instead of dropping it")
	}

	jm.mu.Lock()
	_, stillSubscribed := jm.eventSubs[ch]
	jm.mu.Unlock()
	if stillSubscribed {
		t.Fatalf("slow subscriber was not dropped from eventSubs")
	}

	select {
	case _, open := <-ch:
		if open {
			// Drain any buffered events; the channel must eventually report closed.
			for range ch {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dropped subscriber's channel was never closed")
	}
}

// TestJobManager_PctShapingDoneWithoutReport verifies that a job which
// completes successfully without ever emitting a progress report still shows
// pct 100 (rather than 0 from a nil latest report).
func TestJobManager_PctShapingDoneWithoutReport(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	js, started := jm.start("job1", "/dir1", transcode.Job{}, "t1", "s1")
	if !started {
		t.Fatalf("start: expected success")
	}

	ch, _ := jm.subscribeEventsWithSnapshot()
	t.Cleanup(func() { jm.unsubscribeEvents(ch) })

	jm.complete(js.id, nil)

	select {
	case ev := <-ch:
		if ev.Pct != 100 {
			t.Fatalf("Pct = %v, want 100 (done with no final report)", ev.Pct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the done event")
	}
}

// TestJobManager_DoneFailedRemainListedUntilEviction verifies that jobs
// reaching JobStatusDone/JobStatusFailed via complete() stay in both jobs and
// order until the shared eviction runs, matching the queued-cancel path.
func TestJobManager_DoneFailedRemainListedUntilEviction(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	jsDone, started := jm.start("done1", "/dirA", transcode.Job{}, "", "")
	if !started {
		t.Fatalf("start done1: expected success")
	}
	jm.complete(jsDone.id, nil)

	jsFailed, started := jm.start("failed1", "/dirB", transcode.Job{}, "", "")
	if !started {
		t.Fatalf("start failed1: expected success")
	}
	jm.complete(jsFailed.id, errors.New("boom"))

	for _, id := range []string{"done1", "failed1"} {
		jm.mu.Lock()
		_, present := jm.jobs[id]
		inOrder := false
		for _, oid := range jm.order {
			if oid == id {
				inOrder = true
			}
		}
		jm.mu.Unlock()
		if !present || !inOrder {
			t.Fatalf("%s must stay listed until eviction (jobs present=%v, in order=%v)", id, present, inOrder)
		}
	}

	evict(jm, "done1")
	evict(jm, "failed1")

	for _, id := range []string{"done1", "failed1"} {
		jm.mu.Lock()
		_, present := jm.jobs[id]
		inOrder := false
		for _, oid := range jm.order {
			if oid == id {
				inOrder = true
			}
		}
		jm.mu.Unlock()
		if present || inOrder {
			t.Fatalf("%s not removed from both jobs and order after eviction (jobs present=%v, in order=%v)", id, present, inOrder)
		}
	}
}
