package server

import (
	"context"
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
	JobStatusRunning JobStatus = "running"
	JobStatusDone    JobStatus = "done"
	JobStatusFailed  JobStatus = "failed"
)

// jobState holds the mutable state for a single transcoding job.
type jobState struct {
	id      string
	dirPath string // absolute source directory (used for deduplication)

	// Status and error are set once under jobManager.mu before done is closed.
	status JobStatus
	errMsg string

	// progress receives ProgressReports from the engine.
	progress chan transcode.ProgressReport

	// log captures human-readable lines for the tail API.
	log *ringBuffer

	// SSE subscriber management.
	subsMu sync.Mutex
	subs   []chan transcode.ProgressReport
	latest *transcode.ProgressReport // last report, replayed to new subscribers

	// done is closed after the engine exits and status is updated.
	done chan struct{}
}

// subscribe creates a new SSE subscriber channel.
// Returns nil if the job has already finished.
func (js *jobState) subscribe() chan transcode.ProgressReport {
	js.subsMu.Lock()
	defer js.subsMu.Unlock()
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
	js.subsMu.Lock()
	defer js.subsMu.Unlock()
	for i, sub := range js.subs {
		if sub == ch {
			js.subs = append(js.subs[:i], js.subs[i+1:]...)
			return
		}
	}
}

// broadcast sends a ProgressReport to all SSE subscribers (non-blocking drop on slow clients).
func (js *jobState) broadcast(p transcode.ProgressReport) {
	js.subsMu.Lock()
	defer js.subsMu.Unlock()
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
	js.subsMu.Lock()
	defer js.subsMu.Unlock()
	for _, ch := range js.subs {
		close(ch)
	}
	js.subs = nil
}

// jobManager tracks all active and recently completed transcoding jobs.
type jobManager struct {
	mu    sync.Mutex
	jobs  map[string]*jobState
	byDir map[string]string // source dir -> job ID
}

func newJobManager() *jobManager {
	return &jobManager{
		jobs:  make(map[string]*jobState),
		byDir: make(map[string]string),
	}
}

// start registers a new job. Returns the new jobState and true on success,
// or the conflicting jobState and false when that directory is already transcoding.
func (jm *jobManager) start(id, dirPath string) (*jobState, bool) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	if existingID, busy := jm.byDir[dirPath]; busy {
		return jm.jobs[existingID], false
	}
	js := &jobState{
		id:       id,
		dirPath:  dirPath,
		status:   JobStatusRunning,
		progress: make(chan transcode.ProgressReport, 64),
		log:      newRingBuffer(1000),
		done:     make(chan struct{}),
	}
	jm.jobs[id] = js
	jm.byDir[dirPath] = id
	return js, true
}

// complete marks the job as done or failed, frees the dir slot, and closes done.
// It must be called exactly once per job, after the engine has exited.
func (jm *jobManager) complete(id string, err error) {
	jm.mu.Lock()
	js, ok := jm.jobs[id]
	if ok {
		if err != nil {
			js.status = JobStatusFailed
			js.errMsg = err.Error()
		} else {
			js.status = JobStatusDone
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

// runJob starts the goroutines that drive a job and fan out progress events.
// parentCtx is cancelled when the server shuts down, which propagates cancellation
// to in-flight transcode jobs to prevent library corruption in replace mode.
func runJob(js *jobState, jm *jobManager, engine TranscodeEngine, job transcode.Job, parentCtx context.Context) {
	// Fanout goroutine: reads from the engine's progress channel, broadcasts to
	// SSE subscribers, and appends summary lines to the log ring buffer.
	go func() {
		for p := range js.progress {
			js.broadcast(p)
			js.log.add(fmt.Sprintf("file %d/%d: %s %.0f%%",
				p.FileIndex+1, p.TotalFiles, p.CurrentFile, p.FilePercent))
		}
		// Engine has finished; close SSE subscribers.
		js.closeSubs()
	}()

	// Engine goroutine.
	go func() {
		js.log.add(fmt.Sprintf("job %s started: %s -> %s/%s",
			js.id, job.SourceDir, job.Preset.Codec, job.Preset.Name))
		ctx, cancel := context.WithCancel(parentCtx)
		defer cancel()

		err := engine.Transcode(ctx, job, js.progress)
		close(js.progress) // unblocks fanout goroutine

		if err != nil {
			js.log.add(fmt.Sprintf("job %s failed: %v", js.id, err))
		} else {
			js.log.add(fmt.Sprintf("job %s complete", js.id))
		}
		jm.complete(js.id, err)
	}()
}
