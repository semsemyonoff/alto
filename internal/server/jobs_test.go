package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
			js, started := jm.start("job1", "/dir1", transcode.Job{}, jobMeta{})
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

// TestJobState_MetadataPersistence verifies the new title/sub/job/cancel
// fields can be set and read back under jm.mu.
func TestJobState_MetadataPersistence(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	job := transcode.Job{ID: "job1", SourceDir: "/dir1"}
	js, started := jm.start("job1", "/dir1", job, jobMeta{title: "album1", sub: "flac -> opus/Music Balanced"})
	if !started {
		t.Fatalf("start: expected success")
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	jm.mu.Lock()
	js.cancel = cancel
	jm.mu.Unlock()

	jm.mu.Lock()
	gotTitle := js.title
	gotSub := js.sub
	gotJob := js.job
	gotCancel := js.cancel
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
}

// TestJobState_LatestRaceFree exercises concurrent updateLatest writers
// against concurrent readers of js.latest. Run with -race to confirm the
// collapse of subsMu into jobManager.mu introduced no unsynchronized access.
func TestJobState_LatestRaceFree(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	js, started := jm.start("job1", "/dir1", transcode.Job{}, jobMeta{})
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

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
		t.Fatalf("start job1: expected success")
	}
	if _, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, jobMeta{title: "t2", sub: "s2"}); !started {
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

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
		t.Fatalf("start job1: expected success")
	}
	if _, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, jobMeta{title: "t2", sub: "s2"}); !started {
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
	js, started := jm.start("job1", "/dir1", job, jobMeta{title: "t1", sub: "s1"})
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
		if _, started := jm.start(id, dir, transcode.Job{ID: id}, jobMeta{title: id}); !started {
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

		if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
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

// listedJob reports whether id is present in the manager and, if so, whether
// snapshotJobs still lists it — the two halves of a tombstone.
func listedJob(jm *jobManager, id string) (present, listed bool) {
	jm.mu.Lock()
	_, present = jm.jobs[id]
	jm.mu.Unlock()
	for _, ev := range jm.snapshotJobs() {
		if ev.ID == id {
			listed = true
		}
	}
	return present, listed
}

// inOrder reports whether id is still in the manager's registration order.
func inOrder(jm *jobManager, id string) bool {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	return slices.Contains(jm.order, id)
}

// TestJobManager_CancelQueued verifies that canceling a queued job marks it
// canceled without ever running it, that it stays listed (never removed from
// order) until eviction, and that eviction then only tombstones it — the row
// survives so GET /api/jobs/{id} keeps reporting the outcome.
func TestJobManager_CancelQueued(t *testing.T) {
	block := make(chan struct{})
	eng := &blockingEngine{block: block}
	jm := newJobManager(eng, 1, context.Background())
	t.Cleanup(jm.Shutdown)

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
		t.Fatalf("start job1: expected success")
	}
	if _, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, jobMeta{title: "t2", sub: "s2"}); !started {
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

	present, listed := listedJob(jm, "job2")
	if !present || !listed {
		t.Fatalf("canceled queued job2 must stay listed until eviction (present=%v, listed=%v)", present, listed)
	}

	jm.evict("job2")
	present, listed = listedJob(jm, "job2")
	if !present || listed {
		t.Fatalf("evicted job2: present=%v listed=%v, want a tombstone (present, unlisted)", present, listed)
	}
	detail, ok := jm.detail("job2")
	if !ok || detail.Status != JobStatusCanceled || !detail.Evicted {
		t.Fatalf("detail(job2) = %+v (ok=%v), want a canceled, evicted tombstone", detail, ok)
	}
}

// TestJobManager_CancelRunning verifies that canceling a running job fires
// its context, which the engine observes as ctx.Err(), and complete() maps
// that to JobStatusCanceled.
func TestJobManager_CancelRunning(t *testing.T) {
	jm := newJobManager(&ctxEngine{}, 1, context.Background())
	t.Cleanup(jm.Shutdown)

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
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

	js, started := jm.start("job1", "/dir1", transcode.Job{}, jobMeta{})
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

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "title1", sub: "sub1"}); !started {
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

	js, started := jm.start("job1", "/dir1", transcode.Job{}, jobMeta{title: "t1", sub: "s1"})
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

	js, started := jm.start("job1", "/dir1", transcode.Job{}, jobMeta{title: "t1", sub: "s1"})
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

	js, started := jm.start("job1", "/dir1", transcode.Job{}, jobMeta{title: "t1", sub: "s1"})
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
// reaching JobStatusDone/JobStatusFailed via complete() stay listed until the
// shared eviction runs, matching the queued-cancel path, and that eviction
// then leaves a tombstone rather than deleting the row.
func TestJobManager_DoneFailedRemainListedUntilEviction(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	jsDone, started := jm.start("done1", "/dirA", transcode.Job{}, jobMeta{})
	if !started {
		t.Fatalf("start done1: expected success")
	}
	jm.complete(jsDone.id, nil)

	jsFailed, started := jm.start("failed1", "/dirB", transcode.Job{}, jobMeta{})
	if !started {
		t.Fatalf("start failed1: expected success")
	}
	jm.complete(jsFailed.id, errors.New("boom"))

	for _, id := range []string{"done1", "failed1"} {
		present, listed := listedJob(jm, id)
		if !present || !listed {
			t.Fatalf("%s must stay listed until eviction (present=%v, listed=%v)", id, present, listed)
		}
	}

	jm.evict("done1")
	jm.evict("failed1")

	for _, id := range []string{"done1", "failed1"} {
		present, listed := listedJob(jm, id)
		if !present || listed {
			t.Fatalf("evicted %s: present=%v listed=%v, want a tombstone (present, unlisted)", id, present, listed)
		}
	}

	if detail, ok := jm.detail("failed1"); !ok || detail.Status != JobStatusFailed || detail.Error != "boom" || !detail.Evicted {
		t.Fatalf("detail(failed1) = %+v (ok=%v), want the failure reason preserved on the tombstone", detail, ok)
	}
}

// TestJobManager_DetailProgress verifies that the detail payload counts
// completed files from the engine's last progress report — the file the engine
// is on is not yet done — while the queue event stays on overall percent.
func TestJobManager_DetailProgress(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	job := transcode.Job{
		ID:    "job1",
		Files: []transcode.FileInfo{{Name: "01.flac"}, {Name: "02.flac"}, {Name: "03.flac"}},
	}
	js, started := jm.start("job1", "/dir1", job, jobMeta{title: "t1", sub: "s1"})
	if !started {
		t.Fatalf("start: expected success")
	}

	detail, ok := jm.detail("job1")
	if !ok {
		t.Fatalf("detail: job not found")
	}
	if detail.TotalFiles != 3 || detail.DoneFiles != 0 {
		t.Errorf("done/total = %d/%d before any report, want 0/3", detail.DoneFiles, detail.TotalFiles)
	}

	js.updateLatest(transcode.ProgressReport{
		CurrentFile: "03.flac", FileIndex: 2, TotalFiles: 3, FilePercent: 50,
	})

	detail, _ = jm.detail("job1")
	if detail.DoneFiles != 2 {
		t.Errorf("done_files = %d, want 2 (the third file is still in flight)", detail.DoneFiles)
	}

	// A done job reports every file complete even if its last report was partial.
	jm.complete("job1", nil)
	detail, _ = jm.detail("job1")
	if detail.DoneFiles != 3 || detail.Pct != 100 {
		t.Errorf("done_files/pct = %d/%v after completion, want 3/100", detail.DoneFiles, detail.Pct)
	}
}

// TestJobManager_DetailTimestampOrdering verifies the three timestamps are
// stamped at their own transition and in order.
func TestJobManager_DetailTimestampOrdering(t *testing.T) {
	block := make(chan struct{})
	jm := newJobManager(&blockingEngine{block: block}, 1, context.Background())
	t.Cleanup(jm.Shutdown)

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
		t.Fatalf("start: expected success")
	}
	waitForJobStatus(t, jm, "job1", JobStatusRunning, 2*time.Second)
	close(block)
	waitForJobStatus(t, jm, "job1", JobStatusDone, 2*time.Second)

	detail, ok := jm.detail("job1")
	if !ok {
		t.Fatalf("detail: job not found")
	}
	if detail.StartedAt == nil || detail.FinishedAt == nil {
		t.Fatalf("started_at = %v, finished_at = %v, want both set on a finished job", detail.StartedAt, detail.FinishedAt)
	}
	if detail.StartedAt.Before(detail.CreatedAt) {
		t.Errorf("started_at %v precedes created_at %v", detail.StartedAt, detail.CreatedAt)
	}
	if detail.FinishedAt.Before(*detail.StartedAt) {
		t.Errorf("finished_at %v precedes started_at %v", detail.FinishedAt, detail.StartedAt)
	}
}

// TestJobManager_DetailUnknownID verifies detail reports a miss rather than a
// zero-valued payload.
func TestJobManager_DetailUnknownID(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	if _, ok := jm.detail("nope"); ok {
		t.Error("detail(nope) reported found, want a miss")
	}
}

// TestJobManager_EvictionKeepsOutcome verifies that every route to a tombstone
// — the timed eviction of a completed job, of a cancel-while-queued job, and
// an explicit remove() — keeps the row answerable with its terminal status and
// evicted: true, while dropping it from the queue list.
func TestJobManager_EvictionKeepsOutcome(t *testing.T) {
	cases := []struct {
		name       string
		tombstone  func(jm *jobManager, id string)
		terminate  func(jm *jobManager, id string)
		wantStatus JobStatus
	}{
		{
			name:       "completed then evicted",
			terminate:  func(jm *jobManager, id string) { jm.complete(id, nil) },
			tombstone:  func(jm *jobManager, id string) { jm.evict(id) },
			wantStatus: JobStatusDone,
		},
		{
			name:       "canceled while queued then evicted",
			terminate:  func(jm *jobManager, id string) { jm.cancel(id) },
			tombstone:  func(jm *jobManager, id string) { jm.evict(id) },
			wantStatus: JobStatusCanceled,
		},
		{
			name:       "removed by the user",
			terminate:  func(jm *jobManager, id string) { jm.complete(id, nil) },
			tombstone:  func(jm *jobManager, id string) { jm.remove(id) },
			wantStatus: JobStatusDone,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jm := newJobManager(nil, 0, context.Background())
			if _, started := jm.start("job1", "/dir1", twoFileJob("job1"), jobMeta{title: "t1", sub: "s1"}); !started {
				t.Fatalf("start: expected success")
			}
			tc.terminate(jm, "job1")
			tc.tombstone(jm, "job1")

			present, listed := listedJob(jm, "job1")
			if !present || listed {
				t.Fatalf("present=%v listed=%v, want a tombstone (present, unlisted)", present, listed)
			}
			if !inOrder(jm, "job1") {
				t.Error("job1 dropped from order, want the row kept until the FIFO bound")
			}
			detail, ok := jm.detail("job1")
			if !ok {
				t.Fatal("detail: tombstone must still answer, not 404")
			}
			if detail.Status != tc.wantStatus || !detail.Evicted {
				t.Errorf("status/evicted = %q/%v, want %q/true", detail.Status, detail.Evicted, tc.wantStatus)
			}
			if _, snapshot := jm.subscribeEventsWithSnapshot(); len(snapshot) != 0 {
				t.Errorf("SSE snapshot = %+v, want no evicted jobs", snapshot)
			}
		})
	}
}

// TestJobManager_RemoveBroadcastsAndKeepsRow verifies remove() still tells
// connected queue panels to drop the row, even though the job itself survives
// as a tombstone.
func TestJobManager_RemoveBroadcastsAndKeepsRow(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
		t.Fatalf("start: expected success")
	}
	jm.complete("job1", nil)

	ch, _ := jm.subscribeEventsWithSnapshot()
	t.Cleanup(func() { jm.unsubscribeEvents(ch) })

	if got := jm.remove("job1"); got != removeResultRemoved {
		t.Fatalf("remove = %v, want removeResultRemoved", got)
	}

	select {
	case ev := <-ch:
		if ev.ID != "job1" || !ev.Removed {
			t.Fatalf("event = %+v, want a remove event for job1", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the remove event")
	}

	if _, ok := jm.detail("job1"); !ok {
		t.Error("detail(job1) after remove: want the tombstone, got a miss")
	}
}

// TestJobManager_EvictedDirAcceptsNewJob verifies the byDir slot a tombstone
// once held is free: an evicted job must never block a new job for the same
// directory.
func TestJobManager_EvictedDirAcceptsNewJob(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
		t.Fatalf("start job1: expected success")
	}
	jm.complete("job1", nil)
	jm.evict("job1")

	if _, started := jm.start("job2", "/dir1", transcode.Job{ID: "job2"}, jobMeta{title: "t2", sub: "s2"}); !started {
		t.Fatal("start job2 on the evicted job's directory: expected success")
	}

	listedIDs := make([]string, 0, 1)
	for _, ev := range jm.snapshotJobs() {
		listedIDs = append(listedIDs, ev.ID)
	}
	if !slices.Equal(listedIDs, []string{"job2"}) {
		t.Errorf("queue lists %v, want only job2", listedIDs)
	}
}

// TestJobManager_EvictionFIFOBound verifies tombstones are bounded: once they
// exceed maxEvictedJobs the oldest are deleted for real, oldest first, while
// the newest ones and any still-listed job survive.
func TestJobManager_EvictionFIFOBound(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())

	// One live job registered first: it must survive the prune untouched.
	if _, started := jm.start("live", "/live", transcode.Job{ID: "live"}, jobMeta{}); !started {
		t.Fatalf("start live: expected success")
	}

	const overflow = 3
	total := maxEvictedJobs + overflow
	for i := range total {
		id := fmt.Sprintf("job%04d", i)
		if _, started := jm.start(id, "/dir/"+id, transcode.Job{ID: id}, jobMeta{}); !started {
			t.Fatalf("start %s: expected success", id)
		}
		jm.complete(id, nil)
		jm.evict(id)
	}

	for i := range overflow {
		id := fmt.Sprintf("job%04d", i)
		if _, ok := jm.detail(id); ok {
			t.Errorf("%s still tracked, want the oldest tombstones dropped first", id)
		}
		if inOrder(jm, id) {
			t.Errorf("%s still in order, want it dropped alongside its row", id)
		}
	}
	for i := overflow; i < total; i++ {
		id := fmt.Sprintf("job%04d", i)
		if _, ok := jm.detail(id); !ok {
			t.Fatalf("%s dropped, want the newest %d tombstones kept", id, maxEvictedJobs)
		}
	}
	if _, ok := jm.detail("live"); !ok {
		t.Error("the queued job was dropped by the tombstone prune")
	}
	if _, listed := listedJob(jm, "live"); !listed {
		t.Error("the queued job vanished from the queue list")
	}
}
