package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/semsemyonoff/ALTO/internal/transcode"
)

// ringBuffer is a fixed-size in-memory circular buffer of log lines.
// The oldest entry is evicted when the buffer is full.
type ringBuffer struct {
	mu    sync.Mutex
	buf   []string
	size  int
	head  int
	count int
}

func newRingBuffer(size int) *ringBuffer {
	if size <= 0 {
		size = 1000
	}
	return &ringBuffer{buf: make([]string, size), size: size}
}

func (rb *ringBuffer) add(line string) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.buf[rb.head] = line
	rb.head = (rb.head + 1) % rb.size
	if rb.count < rb.size {
		rb.count++
	}
}

// lines returns all buffered lines in insertion order (oldest first).
func (rb *ringBuffer) lines() []string {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if rb.count == 0 {
		return nil
	}
	out := make([]string, rb.count)
	start := (rb.head - rb.count + rb.size) % rb.size
	for i := 0; i < rb.count; i++ {
		out[i] = rb.buf[(start+i)%rb.size]
	}
	return out
}

// JobStatus is the lifecycle state of a transcoding job.
type JobStatus string

const (
	JobStatusQueued   JobStatus = "queued"
	JobStatusRunning  JobStatus = "running"
	JobStatusDone     JobStatus = "done"
	JobStatusFailed   JobStatus = "failed"
	JobStatusCanceled JobStatus = "canceled"
)

// jobState holds the mutable state for a single transcoding job.
// All mutable fields (status, errMsg, latest, subs, and the fields below) are
// guarded by jm.mu — jobState has no lock of its own.
type jobState struct {
	id      string
	dirPath string // absolute source directory (used for deduplication)

	// jm is the owning manager, used to lock jm.mu for all mutable-field access.
	jm *jobManager

	// title/sub are display metadata for the queue UI (e.g. dir basename / preset summary).
	title string
	sub   string

	// job is the transcode.Job this jobState was started with.
	job transcode.Job

	// cancel cancels the per-job context passed to the engine; set once the
	// worker has picked the job up and built its cancel context.
	cancel context.CancelFunc

	// Status and error are set once under jobManager.mu before done is closed.
	status JobStatus
	errMsg string

	// progress receives ProgressReports from the engine.
	progress chan transcode.ProgressReport

	// log captures human-readable lines for the tail API.
	log *ringBuffer

	// latest is the most recent progress report, guarded by jm.mu. It feeds
	// the centralized pct-shaping helper used by /api/jobs and the global
	// event bus.
	latest *transcode.ProgressReport

	// done is closed after the engine exits and status is updated.
	done chan struct{}
}

// updateLatest records the most recent progress report for the job. Guarded
// by jm.mu so it is race-free against concurrent reads (e.g. calcJobPercent).
func (js *jobState) updateLatest(p transcode.ProgressReport) {
	js.jm.mu.Lock()
	defer js.jm.mu.Unlock()
	js.latest = &p
}

// jobEvent is the global queue-panel event broadcast on every job lifecycle
// transition (enqueue/start/progress/complete/cancel). Pct is shaped by
// calcJobPercent so a finished job never shows a partial meter.
type jobEvent struct {
	ID     string    `json:"id"`
	Status JobStatus `json:"status"`
	Pct    float64   `json:"pct"`
	Title  string    `json:"title"`
	Sub    string    `json:"sub"`
	// Dir is the absolute source directory (same resolution the transcode dock
	// posts), so the dock can tell whether its album already has an active job
	// and disable START. Empty on `remove` events.
	Dir string `json:"dir,omitempty"`
	// Removed marks a job that has been dropped from the queue list (via
	// remove); it is delivered as a distinct `remove` SSE event rather than an
	// `update` so the queue UI can drop the row. Never set on snapshot/update
	// events.
	Removed bool `json:"removed,omitempty"`
}

// jobManager tracks all active and recently completed transcoding jobs and
// dispatches queued jobs to a bounded pool of workers.
type jobManager struct {
	mu    sync.Mutex
	jobs  map[string]*jobState
	byDir map[string]string // source dir -> job ID
	order []string          // job IDs in registration order; the queue list source of truth

	engine   TranscodeEngine
	workers  int
	cond     *sync.Cond
	wg       sync.WaitGroup
	shutdown bool

	// eventSubs is the process-wide set of global queue-panel subscribers,
	// guarded by mu like everything else. A subscriber whose channel is full
	// is dropped (closed and removed) rather than allowed to block a
	// broadcast or silently lag forever.
	eventSubs map[chan jobEvent]struct{}
}

// calcJobPercent shapes a job's queue-panel percentage: a done job always
// reports 100 regardless of its last progress report; a running/queued/
// failed/canceled job reports its last known overall percent, or 0 if no
// report ever arrived. Callers must hold jm.mu.
func calcJobPercent(js *jobState) float64 {
	if js.status == JobStatusDone {
		return 100
	}
	if js.latest == nil {
		return 0
	}
	return calcOverallPercent(*js.latest)
}

// eventForLocked builds the jobEvent snapshot for js. Callers must hold jm.mu.
func (jm *jobManager) eventForLocked(js *jobState) jobEvent {
	return jobEvent{
		ID:     js.id,
		Status: js.status,
		Pct:    calcJobPercent(js),
		Title:  js.title,
		Sub:    js.sub,
		Dir:    js.dirPath,
	}
}

// snapshotEvent returns the current jobEvent for id, or false if the job is
// unknown (e.g. already evicted). Used by call sites that must release jm.mu
// before they can safely build the event.
func (jm *jobManager) snapshotEvent(id string) (jobEvent, bool) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	js, ok := jm.jobs[id]
	if !ok {
		return jobEvent{}, false
	}
	return jm.eventForLocked(js), true
}

// broadcastEventLocked sends ev to every global event subscriber without
// acquiring jm.mu; callers must already hold it. Sends are non-blocking: a
// subscriber whose buffer is full is dropped (its channel is closed and
// removed from the subscriber set) instead of blocking the broadcast.
func (jm *jobManager) broadcastEventLocked(ev jobEvent) {
	for ch := range jm.eventSubs {
		select {
		case ch <- ev:
		default:
			delete(jm.eventSubs, ch)
			close(ch)
		}
	}
}

// broadcastEvent is broadcastEventLocked for callers that do not already hold jm.mu.
func (jm *jobManager) broadcastEvent(ev jobEvent) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	jm.broadcastEventLocked(ev)
}

// snapshotJobs returns the current jobEvent for every job in order, in the
// same order — the shape used by GET /api/jobs.
func (jm *jobManager) snapshotJobs() []jobEvent {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	out := make([]jobEvent, 0, len(jm.order))
	for _, id := range jm.order {
		if js, ok := jm.jobs[id]; ok {
			out = append(out, jm.eventForLocked(js))
		}
	}
	return out
}

// subscribeEventsWithSnapshot atomically registers a new global event
// subscriber and captures the current job list (in order), both under one
// mu lock, so a caller that renders the snapshot and then streams live
// deltas from the returned channel can never miss or duplicate an update
// that lands between the two steps.
func (jm *jobManager) subscribeEventsWithSnapshot() (chan jobEvent, []jobEvent) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	ch := make(chan jobEvent, 64)
	jm.eventSubs[ch] = struct{}{}
	snapshot := make([]jobEvent, 0, len(jm.order))
	for _, id := range jm.order {
		if js, ok := jm.jobs[id]; ok {
			snapshot = append(snapshot, jm.eventForLocked(js))
		}
	}
	return ch, snapshot
}

// unsubscribeEvents removes ch from the global subscriber set and closes it.
// Safe to call even if broadcastEventLocked already dropped (and closed) ch
// for being slow.
func (jm *jobManager) unsubscribeEvents(ch chan jobEvent) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	if _, ok := jm.eventSubs[ch]; ok {
		delete(jm.eventSubs, ch)
		close(ch)
	}
}

// newJobManager creates a jobManager and, if engine is non-nil, starts workers
// worker goroutines (minimum 1) that dispatch queued jobs to the engine.
// parentCtx is the context each worker derives its per-job cancel context
// from; canceling it (e.g. on server shutdown) cancels in-flight jobs.
func newJobManager(engine TranscodeEngine, workers int, parentCtx context.Context) *jobManager {
	jm := &jobManager{
		jobs:      make(map[string]*jobState),
		byDir:     make(map[string]string),
		eventSubs: make(map[chan jobEvent]struct{}),
	}
	jm.cond = sync.NewCond(&jm.mu)
	jm.engine = engine
	if engine != nil {
		if workers < 1 {
			workers = 1
		}
		jm.workers = workers
		for range workers {
			jm.wg.Add(1)
			go jm.workerLoop(parentCtx)
		}
	}
	return jm
}

// start registers a new job as queued. Returns the new jobState and true on
// success, or the conflicting jobState and false when that directory is
// already queued or transcoding.
func (jm *jobManager) start(id, dirPath string, job transcode.Job, title, sub string) (*jobState, bool) {
	jm.mu.Lock()
	if existingID, busy := jm.byDir[dirPath]; busy {
		js := jm.jobs[existingID]
		jm.mu.Unlock()
		return js, false
	}
	js := &jobState{
		id:       id,
		dirPath:  dirPath,
		jm:       jm,
		title:    title,
		sub:      sub,
		job:      job,
		status:   JobStatusQueued,
		progress: make(chan transcode.ProgressReport, 64),
		log:      newRingBuffer(1000),
		done:     make(chan struct{}),
	}
	jm.jobs[id] = js
	jm.byDir[dirPath] = id
	jm.order = append(jm.order, id)
	jm.broadcastEventLocked(jm.eventForLocked(js))
	jm.mu.Unlock()
	jm.cond.Broadcast()
	return js, true
}

// Shutdown stops accepting new dispatch, wakes all workers so they observe
// the shutdown flag, and waits for them to exit. It does not cancel
// already-running jobs itself — callers cancel the shared parentCtx (passed
// to newJobManager) to propagate cancellation into in-flight engine calls.
func (jm *jobManager) Shutdown() {
	jm.mu.Lock()
	jm.shutdown = true
	jm.mu.Unlock()
	jm.cond.Broadcast()
	jm.wg.Wait()
}

// nextQueuedLocked returns the first job ID in order whose status is queued,
// or "" if none. Callers must hold jm.mu.
func (jm *jobManager) nextQueuedLocked() string {
	for _, id := range jm.order {
		if js, ok := jm.jobs[id]; ok && js.status == JobStatusQueued {
			return id
		}
	}
	return ""
}

// workerLoop repeatedly picks the next queued job, runs it to completion, and
// marks it done. It occupies its worker slot for the whole job, which is what
// bounds concurrency to jm.workers. It exits once jm.shutdown is set and no
// job is being run.
func (jm *jobManager) workerLoop(parentCtx context.Context) {
	defer jm.wg.Done()
	for {
		jm.mu.Lock()
		id := jm.nextQueuedLocked()
		for id == "" && !jm.shutdown {
			jm.cond.Wait()
			id = jm.nextQueuedLocked()
		}
		if id == "" {
			// shutdown with nothing queued.
			jm.mu.Unlock()
			return
		}
		js := jm.jobs[id]
		js.status = JobStatusRunning
		ctx, cancel := context.WithCancel(parentCtx)
		js.cancel = cancel
		job := js.job
		jm.broadcastEventLocked(jm.eventForLocked(js))
		jm.mu.Unlock()

		err := jm.runOneJob(js, job, ctx)
		cancel()
		jm.complete(id, err)
	}
}

// runOneJob runs a single job to completion: it starts the fanout goroutine
// (which drains js.progress into latest/log and SSE subscribers) and calls
// the engine synchronously, then waits for the fanout goroutine to finish
// draining so latest/log are finalized before returning. It never holds jm.mu
// while waiting on the engine or the fanout goroutine.
func (jm *jobManager) runOneJob(js *jobState, job transcode.Job, ctx context.Context) error {
	fanoutDone := make(chan struct{})

	go func() {
		for p := range js.progress {
			js.updateLatest(p)
			if ev, ok := jm.snapshotEvent(js.id); ok {
				jm.broadcastEvent(ev)
			}
			js.log.add(fmt.Sprintf("file %d/%d: %s %.0f%%",
				p.FileIndex+1, p.TotalFiles, p.CurrentFile, p.FilePercent))
		}
		close(fanoutDone)
	}()

	js.log.add(fmt.Sprintf("job %s started: %s -> %s/%s",
		js.id, job.SourceDir, job.Preset.Codec, job.Preset.Name))

	err := jm.engine.Transcode(ctx, job, js.progress)
	close(js.progress) // unblocks the fanout goroutine
	<-fanoutDone

	if err != nil {
		js.log.add(fmt.Sprintf("job %s failed: %v", js.id, err))
	} else {
		js.log.add(fmt.Sprintf("job %s complete", js.id))
	}
	return err
}

// complete marks the job as done or failed, frees the dir slot, and closes done.
// It must be called exactly once per job, after the engine has exited.
func (jm *jobManager) complete(id string, err error) {
	jm.mu.Lock()
	js, ok := jm.jobs[id]
	if ok {
		switch {
		case err == nil:
			js.status = JobStatusDone
		case errors.Is(err, context.Canceled):
			js.status = JobStatusCanceled
		default:
			js.status = JobStatusFailed
			js.errMsg = err.Error()
		}
		delete(jm.byDir, js.dirPath)
		jm.broadcastEventLocked(jm.eventForLocked(js))
	}
	jm.mu.Unlock()
	if ok {
		close(js.done)
		jm.scheduleEviction(id)
	}
}

// cancelResult is the outcome of a cancel(id) call.
type cancelResult int

const (
	cancelResultCanceled cancelResult = iota
	cancelResultNotFound
	cancelResultFinished
)

// cancel cancels a queued or running job. A queued job is marked canceled
// immediately and left in order (the dispatcher only ever picks up
// JobStatusQueued ids, so it is simply skipped); a running job is canceled
// via its stored context.CancelFunc, and the worker's subsequent complete()
// call maps the resulting context.Canceled error to JobStatusCanceled.
// Unknown ids report cancelResultNotFound; already-terminal jobs report
// cancelResultFinished.
func (jm *jobManager) cancel(id string) cancelResult {
	jm.mu.Lock()
	js, ok := jm.jobs[id]
	if !ok {
		jm.mu.Unlock()
		return cancelResultNotFound
	}
	switch js.status {
	case JobStatusQueued:
		js.status = JobStatusCanceled
		delete(jm.byDir, js.dirPath)
		close(js.done)
		jm.broadcastEventLocked(jm.eventForLocked(js))
		jm.mu.Unlock()
		jm.scheduleEviction(id)
		return cancelResultCanceled
	case JobStatusRunning:
		cancel := js.cancel
		jm.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return cancelResultCanceled
	default:
		jm.mu.Unlock()
		return cancelResultFinished
	}
}

// removeResult is the outcome of a remove(id) call.
type removeResult int

const (
	removeResultRemoved removeResult = iota
	removeResultNotFound
	removeResultActive
)

// remove drops a terminal (done/failed/canceled) job from the jobs map and the
// order list immediately, instead of waiting for its 30-minute eviction, and
// broadcasts a `remove` event so connected queue panels drop the row. A queued
// or running job is left untouched and reported as removeResultActive (it must
// be canceled first); an unknown id reports removeResultNotFound. Removing a
// job whose eviction timer is still pending is safe — the later AfterFunc just
// finds nothing to delete.
func (jm *jobManager) remove(id string) removeResult {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	js, ok := jm.jobs[id]
	if !ok {
		return removeResultNotFound
	}
	switch js.status {
	case JobStatusQueued, JobStatusRunning:
		return removeResultActive
	}
	delete(jm.jobs, id)
	for i, oid := range jm.order {
		if oid == id {
			jm.order = append(jm.order[:i], jm.order[i+1:]...)
			break
		}
	}
	jm.broadcastEventLocked(jobEvent{ID: id, Removed: true})
	return removeResultRemoved
}

// scheduleEviction removes id from both jobs and order 30 minutes after it
// reaches a terminal state, keeping it briefly visible to status/log queries
// while bounding long-run memory growth. Shared by complete() and the
// queued path of cancel().
func (jm *jobManager) scheduleEviction(id string) {
	time.AfterFunc(30*time.Minute, func() {
		jm.mu.Lock()
		delete(jm.jobs, id)
		for i, oid := range jm.order {
			if oid == id {
				jm.order = append(jm.order[:i], jm.order[i+1:]...)
				break
			}
		}
		jm.mu.Unlock()
	})
}

// get returns the jobState for a given ID, or false if not found.
func (jm *jobManager) get(id string) (*jobState, bool) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	js, ok := jm.jobs[id]
	return js, ok
}
