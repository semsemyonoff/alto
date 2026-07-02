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

	// SSE subscriber management (guarded by jm.mu).
	subs   []chan transcode.ProgressReport
	latest *transcode.ProgressReport // last report, replayed to new subscribers

	// done is closed after the engine exits and status is updated.
	done chan struct{}

	// fanoutDone is closed once the fanout goroutine has drained progress and
	// finished updating latest/log, so callers can wait for finalized state.
	fanoutDone chan struct{}
}

// subscribe creates a new SSE subscriber channel.
// Returns nil if the job has already finished.
func (js *jobState) subscribe() chan transcode.ProgressReport {
	js.jm.mu.Lock()
	defer js.jm.mu.Unlock()
	select {
	case <-js.done:
		return nil
	default:
	}
	ch := make(chan transcode.ProgressReport, 32)
	if js.latest != nil {
		ch <- *js.latest
	}
	js.subs = append(js.subs, ch)
	return ch
}

// unsubscribe removes the given channel from the subscriber list.
func (js *jobState) unsubscribe(ch chan transcode.ProgressReport) {
	js.jm.mu.Lock()
	defer js.jm.mu.Unlock()
	for i, sub := range js.subs {
		if sub == ch {
			js.subs = append(js.subs[:i], js.subs[i+1:]...)
			return
		}
	}
}

// broadcast sends a ProgressReport to all SSE subscribers (non-blocking drop on slow clients).
func (js *jobState) broadcast(p transcode.ProgressReport) {
	js.jm.mu.Lock()
	defer js.jm.mu.Unlock()
	js.latest = &p
	for _, ch := range js.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

// closeSubs closes and clears all SSE subscriber channels.
func (js *jobState) closeSubs() {
	js.jm.mu.Lock()
	defer js.jm.mu.Unlock()
	for _, ch := range js.subs {
		close(ch)
	}
	js.subs = nil
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
}

// newJobManager creates a jobManager and, if engine is non-nil, starts workers
// worker goroutines (minimum 1) that dispatch queued jobs to the engine.
// parentCtx is the context each worker derives its per-job cancel context
// from; canceling it (e.g. on server shutdown) cancels in-flight jobs.
func newJobManager(engine TranscodeEngine, workers int, parentCtx context.Context) *jobManager {
	jm := &jobManager{
		jobs:  make(map[string]*jobState),
		byDir: make(map[string]string),
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
	defer jm.mu.Unlock()
	if existingID, busy := jm.byDir[dirPath]; busy {
		return jm.jobs[existingID], false
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
	jm.mu.Lock()
	js.fanoutDone = fanoutDone
	jm.mu.Unlock()

	go func() {
		for p := range js.progress {
			js.broadcast(p)
			js.log.add(fmt.Sprintf("file %d/%d: %s %.0f%%",
				p.FileIndex+1, p.TotalFiles, p.CurrentFile, p.FilePercent))
		}
		// Engine has finished; close SSE subscribers.
		js.closeSubs()
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
	}
	jm.mu.Unlock()
	if ok {
		close(js.done)
		// Close any subscribers that slipped in after the fanout goroutine ran closeSubs
		// but before done was closed (TOCTOU window in subscribe()).
		js.closeSubs()
		// Evict the job from the map after 30 minutes so log queries still work briefly.
		time.AfterFunc(30*time.Minute, func() {
			jm.mu.Lock()
			delete(jm.jobs, id)
			jm.mu.Unlock()
		})
	}
}

// get returns the jobState for a given ID, or false if not found.
func (jm *jobManager) get(id string) (*jobState, bool) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	js, ok := jm.jobs[id]
	return js, ok
}
