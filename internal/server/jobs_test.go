package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

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
			jm := newJobManager()
			js, started := jm.start("job1", "/dir1")
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
	jm := newJobManager()
	js, started := jm.start("job1", "/dir1")
	if !started {
		t.Fatalf("start: expected success")
	}

	job := transcode.Job{ID: "job1", SourceDir: "/dir1"}
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	jm.mu.Lock()
	js.title = "album1"
	js.sub = "flac -> opus/Music Balanced"
	js.job = job
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

// TestJobState_LatestRaceFree exercises concurrent broadcast/subscribe/
// unsubscribe from many goroutines. Run with -race to confirm the collapse
// of subsMu into jobManager.mu removed the separate lock without introducing
// unsynchronized access to latest/subs.
func TestJobState_LatestRaceFree(t *testing.T) {
	jm := newJobManager()
	js, started := jm.start("job1", "/dir1")
	if !started {
		t.Fatalf("start: expected success")
	}

	var wg sync.WaitGroup

	// Concurrent broadcasters.
	for i := range 20 {
		wg.Go(func() {
			js.broadcast(transcode.ProgressReport{FileIndex: i, TotalFiles: 20})
		})
	}

	// Concurrent subscribe/unsubscribe.
	for range 20 {
		wg.Go(func() {
			ch := js.subscribe()
			if ch == nil {
				return
			}
			// broadcast() sends are non-blocking (buffered channel, default-drop),
			// so no drain goroutine is needed before unsubscribing.
			js.unsubscribe(ch)
		})
	}

	wg.Wait()

	jm.mu.Lock()
	_ = js.latest
	jm.mu.Unlock()
}
