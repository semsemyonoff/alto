package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/semsemyonoff/ALTO/internal/db"
	"github.com/semsemyonoff/ALTO/internal/transcode"
)

// mockEngine implements TranscodeEngine for tests.
type mockEngine struct {
	// err is the error Transcode returns.
	err error
	// reports are sent to the progress channel before returning.
	reports []transcode.ProgressReport
	// block is an optional channel; Transcode blocks until it is closed.
	block chan struct{}
}

func (m *mockEngine) Transcode(_ context.Context, _ transcode.Job, progress chan<- transcode.ProgressReport) error {
	for _, r := range m.reports {
		progress <- r
	}
	if m.block != nil {
		<-m.block
	}
	return m.err
}

// newTestServerWithEngine builds a test server with a TranscodeEngine and inserts a directory + tracks.
// Returns the server, db, library root, and the absolute path to the seeded directory.
func newTestServerWithEngine(t *testing.T, eng TranscodeEngine) (*Server, *db.DB, string, string) {
	t.Helper()

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	libDir := t.TempDir()
	libID, err := database.UpsertLibrary("TestLib", libDir)
	if err != nil {
		t.Fatalf("UpsertLibrary: %v", err)
	}

	// Create the directory on disk (LibraryOnlyValidate uses EvalSymlinks).
	albumDir := libDir + "/album1"
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Insert a directory and two tracks.
	dirID, err := database.UpsertDirectory(libID, "album1", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	if err := database.UpsertTrack(db.Track{DirectoryID: dirID, Filename: "track1.flac", Codec: "flac", Bitrate: 1000, Duration: 10.0, SampleRate: 44100, Channels: 2, Size: 1000}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
	if err := database.UpsertTrack(db.Track{DirectoryID: dirID, Filename: "track2.flac", Codec: "flac", Bitrate: 1000, Duration: 5.0, SampleRate: 44100, Channels: 2, Size: 500}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	cfg := Config{
		Libraries: []LibraryConfig{
			{ID: libID, Name: "TestLib", Path: libDir},
		},
		OutputDir: t.TempDir(),
	}
	srv := NewWithEngine(database, &mockScanner{}, eng, cfg)
	t.Cleanup(srv.Shutdown)
	return srv, database, libDir, libDir + "/album1"
}

// --- POST /api/transcode ---

func TestHandleTranscodeStart_Success(t *testing.T) {
	block := make(chan struct{})
	eng := &mockEngine{block: block}
	srv, _, libDir, dirPath := newTestServerWithEngine(t, eng)
	defer close(block)

	body := map[string]any{
		"path":        dirPath,
		"preset":      "Balanced",
		"output_mode": "shared",
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	_ = libDir // used implicitly via dirPath
	w := httptest.NewRecorder()

	srv.handleTranscodeStart(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["job_id"] == "" {
		t.Error("expected non-empty job_id")
	}
}

func TestHandleTranscodeStart_NoEngine(t *testing.T) {
	srv, _, _, dirPath := newTestServerWithEngine(t, nil)
	// Override engine to nil.
	srv.engine = nil

	body := map[string]any{"path": dirPath, "preset": "Balanced", "output_mode": "shared"}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	w := httptest.NewRecorder()

	srv.handleTranscodeStart(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

func TestHandleTranscodeStart_OutsideLibrary(t *testing.T) {
	eng := &mockEngine{}
	srv, _, _, _ := newTestServerWithEngine(t, eng)

	body := map[string]any{"path": "/etc/passwd", "preset": "Balanced", "output_mode": "shared"}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	w := httptest.NewRecorder()

	srv.handleTranscodeStart(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestHandleTranscodeStart_AltoStarPath(t *testing.T) {
	eng := &mockEngine{}
	srv, _, libDir, _ := newTestServerWithEngine(t, eng)

	body := map[string]any{
		"path":        libDir + "/alto-out",
		"preset":      "Balanced",
		"output_mode": "shared",
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	w := httptest.NewRecorder()

	srv.handleTranscodeStart(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for app-owned path, got %d", w.Code)
	}
}

func TestHandleTranscodeStart_LossyDirectoryRejected(t *testing.T) {
	eng := &mockEngine{}
	srv, database, _, dirPath := newTestServerWithEngine(t, eng)

	dir, err := database.GetDirectoryByPath(srv.cfg.Libraries[0].ID, "album1")
	if err != nil {
		t.Fatalf("GetDirectoryByPath: %v", err)
	}
	if dir == nil {
		t.Fatal("directory should exist")
	}
	if err := database.UpsertTrack(db.Track{DirectoryID: dir.ID, Filename: "track1.flac", Codec: "mp3", Bitrate: 320000, Duration: 10.0, SampleRate: 44100, Channels: 2, Size: 1000}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
	if _, err := database.UpsertDirectory(srv.cfg.Libraries[0].ID, "album1", "MP3", false, ""); err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}

	body := map[string]any{"path": dirPath, "preset": "Balanced", "output_mode": "shared"}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	w := httptest.NewRecorder()

	srv.handleTranscodeStart(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "lossless") {
		t.Fatalf("expected lossless rejection message, got %s", w.Body.String())
	}
}

func TestHandleTranscodeStart_Deduplication(t *testing.T) {
	block := make(chan struct{})
	eng := &mockEngine{block: block}
	srv, _, _, dirPath := newTestServerWithEngine(t, eng)
	defer close(block)

	startJob := func() *httptest.ResponseRecorder {
		body := map[string]any{"path": dirPath, "preset": "Balanced", "output_mode": "shared"}
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
		w := httptest.NewRecorder()
		srv.handleTranscodeStart(w, req)
		return w
	}

	w1 := startJob()
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first job: expected 202, got %d", w1.Code)
	}

	w2 := startJob()
	if w2.Code != http.StatusConflict {
		t.Fatalf("duplicate job: expected 409, got %d", w2.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["job_id"] == "" {
		t.Error("expected conflicting job_id in response")
	}
}

func TestHandleTranscodeStart_InvalidOutputMode(t *testing.T) {
	eng := &mockEngine{}
	srv, _, _, dirPath := newTestServerWithEngine(t, eng)

	body := map[string]any{"path": dirPath, "preset": "Balanced", "output_mode": "invalid"}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	w := httptest.NewRecorder()

	srv.handleTranscodeStart(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestHandleTranscodeStart_DirectoryNotIndexed(t *testing.T) {
	eng := &mockEngine{}
	srv, _, libDir, _ := newTestServerWithEngine(t, eng)

	body := map[string]any{"path": libDir + "/nonexistent", "preset": "Balanced", "output_mode": "shared"}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	w := httptest.NewRecorder()

	srv.handleTranscodeStart(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// --- GET /api/transcode/{jobID}/progress (retired) ---

// TestTranscodeProgressRoute_Removed confirms the per-job progress SSE route
// was retired once the global queue panel (GET /api/jobs, GET /api/jobs/events)
// took over live progress display, while /log — still used for the queue
// row's lazy log expansion — keeps working through the same mux.
func TestTranscodeProgressRoute_Removed(t *testing.T) {
	eng := &mockEngine{
		reports: []transcode.ProgressReport{
			{CurrentFile: "track1.flac", FileIndex: 0, TotalFiles: 1, FilePercent: 100},
		},
	}
	srv, _, _, dirPath := newTestServerWithEngine(t, eng)

	body := map[string]any{"path": dirPath, "preset": "Balanced", "output_mode": "shared"}
	b, _ := json.Marshal(body)
	startReq := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	startW := httptest.NewRecorder()
	srv.handleTranscodeStart(startW, startReq)
	if startW.Code != http.StatusAccepted {
		t.Fatalf("start: expected 202, got %d", startW.Code)
	}
	var startResp map[string]string
	_ = json.Unmarshal(startW.Body.Bytes(), &startResp)
	jobID := startResp["job_id"]

	js, _ := srv.jobs.get(jobID)
	select {
	case <-js.done:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not complete")
	}

	progReq := httptest.NewRequest(http.MethodGet, "/api/transcode/"+jobID+"/progress", nil)
	progW := httptest.NewRecorder()
	srv.mux.ServeHTTP(progW, progReq)
	if progW.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for retired progress route, got %d", progW.Code)
	}

	logReq := httptest.NewRequest(http.MethodGet, "/api/transcode/"+jobID+"/log", nil)
	logW := httptest.NewRecorder()
	srv.mux.ServeHTTP(logW, logReq)
	if logW.Code != http.StatusOK {
		t.Fatalf("expected 200 for /log, got %d: %s", logW.Code, logW.Body.String())
	}
}

// --- GET /api/transcode/{jobID}/log ---

func TestHandleTranscodeLog_JobNotFound(t *testing.T) {
	srv, _, _, _ := newTestServerWithEngine(t, &mockEngine{})

	req := httptest.NewRequest(http.MethodGet, "/api/transcode/nope/log", nil)
	req.SetPathValue("jobID", "nope")
	w := httptest.NewRecorder()

	srv.handleTranscodeLog(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleTranscodeLog_ContainsLines(t *testing.T) {
	eng := &mockEngine{
		reports: []transcode.ProgressReport{
			{CurrentFile: "track1.flac", FileIndex: 0, TotalFiles: 1, FilePercent: 100},
		},
	}
	srv, _, _, dirPath := newTestServerWithEngine(t, eng)

	// Start job.
	body := map[string]any{"path": dirPath, "preset": "Balanced", "output_mode": "shared"}
	b, _ := json.Marshal(body)
	startReq := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	startW := httptest.NewRecorder()
	srv.handleTranscodeStart(startW, startReq)
	var startResp map[string]string
	_ = json.Unmarshal(startW.Body.Bytes(), &startResp)
	jobID := startResp["job_id"]

	// Wait for job completion.
	js, _ := srv.jobs.get(jobID)
	select {
	case <-js.done:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not complete")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/transcode/"+jobID+"/log", nil)
	req.SetPathValue("jobID", jobID)
	w := httptest.NewRecorder()
	srv.handleTranscodeLog(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	lines, _ := resp["lines"].([]any)
	if len(lines) == 0 {
		t.Error("expected log lines, got none")
	}
}

func TestHandleTranscodeLog_NParam(t *testing.T) {
	eng := &mockEngine{}
	srv, _, _, dirPath := newTestServerWithEngine(t, eng)

	// Start and complete job quickly.
	body := map[string]any{"path": dirPath, "preset": "Balanced", "output_mode": "shared"}
	b, _ := json.Marshal(body)
	startReq := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	startW := httptest.NewRecorder()
	srv.handleTranscodeStart(startW, startReq)
	var startResp map[string]string
	_ = json.Unmarshal(startW.Body.Bytes(), &startResp)
	jobID := startResp["job_id"]

	js, _ := srv.jobs.get(jobID)
	select {
	case <-js.done:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not complete")
	}

	// n=1 should return at most 1 line.
	req := httptest.NewRequest(http.MethodGet, "/api/transcode/"+jobID+"/log?n=1", nil)
	req.SetPathValue("jobID", jobID)
	w := httptest.NewRecorder()
	srv.handleTranscodeLog(w, req)

	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	lines, _ := resp["lines"].([]any)
	if len(lines) > 1 {
		t.Errorf("expected at most 1 line with n=1, got %d", len(lines))
	}
}

// --- GET /api/jobs ---

func TestHandleJobs_Empty(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	w := httptest.NewRecorder()
	srv.handleJobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp struct {
		Jobs []jobEvent `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Jobs) != 0 {
		t.Errorf("expected no jobs, got %d", len(resp.Jobs))
	}
}

func TestHandleJobs_MixedStatus(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	// job1: queued, then completed successfully — pct must show 100 even
	// though no progress report ever arrived.
	js1, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "Album One", "flac -> opus/Balanced")
	if !started {
		t.Fatalf("start job1: expected success")
	}
	jm.complete(js1.id, nil)

	// job2: queued, then failed.
	js2, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, "Album Two", "flac -> opus/Balanced")
	if !started {
		t.Fatalf("start job2: expected success")
	}
	jm.complete(js2.id, errors.New("boom"))

	// job3: canceled while still queued.
	if _, started := jm.start("job3", "/dir3", transcode.Job{ID: "job3"}, "Album Three", "flac -> opus/Balanced"); !started {
		t.Fatalf("start job3: expected success")
	}
	if result := jm.cancel("job3"); result != cancelResultCanceled {
		t.Fatalf("cancel job3: expected canceled, got %v", result)
	}

	// job4: still queued.
	if _, started := jm.start("job4", "/dir4", transcode.Job{ID: "job4"}, "Album Four", "flac -> opus/Balanced"); !started {
		t.Fatalf("start job4: expected success")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	w := httptest.NewRecorder()
	srv.handleJobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Jobs []jobEvent `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	wantOrder := []string{"job1", "job2", "job3", "job4"}
	if len(resp.Jobs) != len(wantOrder) {
		t.Fatalf("expected %d jobs, got %d: %+v", len(wantOrder), len(resp.Jobs), resp.Jobs)
	}
	for i, id := range wantOrder {
		if resp.Jobs[i].ID != id {
			t.Errorf("jobs[%d].ID = %q, want %q", i, resp.Jobs[i].ID, id)
		}
	}

	byID := make(map[string]jobEvent, len(resp.Jobs))
	for _, j := range resp.Jobs {
		byID[j.ID] = j
	}

	if got := byID["job1"]; got.Status != JobStatusDone || got.Pct != 100 || got.Title != "Album One" {
		t.Errorf("job1 = %+v, want status done, pct 100, title Album One", got)
	}
	if got := byID["job2"]; got.Status != JobStatusFailed {
		t.Errorf("job2 status = %q, want failed", got.Status)
	}
	if got := byID["job3"]; got.Status != JobStatusCanceled {
		t.Errorf("job3 status = %q, want canceled", got.Status)
	}
	if got := byID["job4"]; got.Status != JobStatusQueued {
		t.Errorf("job4 status = %q, want queued", got.Status)
	}
}

// --- GET /api/jobs/events ---

func TestHandleJobEvents_SnapshotThenLiveDelta(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	// job1 is registered before the subscription starts, so it must appear in
	// the initial snapshot burst.
	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "Album One", "flac -> opus/Balanced"); !started {
		t.Fatalf("start job1: expected success")
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleJobEvents(w, req)
	}()

	// Give the handler time to subscribe and flush the snapshot before job2 starts.
	time.Sleep(50 * time.Millisecond)

	// job2 is registered after the subscription starts, so it must arrive as a
	// live delta rather than in the snapshot.
	if _, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, "Album Two", "flac -> opus/Balanced"); !started {
		t.Fatalf("start job2: expected success")
	}

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	body := w.Body.String()
	if !strings.Contains(body, `"id":"job1"`) {
		t.Errorf("expected snapshot event for job1, got: %s", body)
	}
	if !strings.Contains(body, `"id":"job2"`) {
		t.Errorf("expected live delta event for job2, got: %s", body)
	}
	if got := strings.Count(body, "event: update"); got < 2 {
		t.Errorf("expected at least 2 update events, got %d in: %s", got, body)
	}
}

func TestHandleJobEvents_DisconnectUnsubscribes(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/events", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleJobEvents(w, req)
	}()

	var subscribed bool
	for range 100 {
		jm.mu.Lock()
		subscribed = len(jm.eventSubs) == 1
		jm.mu.Unlock()
		if subscribed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !subscribed {
		t.Fatal("handler did not register subscription in time")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after context cancel")
	}

	jm.mu.Lock()
	remaining := len(jm.eventSubs)
	jm.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected subscriber removed after disconnect, got %d remaining", remaining)
	}
}

// --- POST /api/jobs/{id}/cancel ---

func TestHandleJobCancel_Queued(t *testing.T) {
	block := make(chan struct{})
	eng := &blockingEngine{block: block}
	jm := newJobManager(eng, 1, context.Background())
	t.Cleanup(jm.Shutdown)
	srv := &Server{jobs: jm}

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "t1", "s1"); !started {
		t.Fatalf("start job1: expected success")
	}
	if _, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, "t2", "s2"); !started {
		t.Fatalf("start job2: expected success")
	}
	waitForJobStatus(t, jm, "job1", JobStatusRunning, 2*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/job2/cancel", nil)
	req.SetPathValue("id", "job2")
	w := httptest.NewRecorder()
	srv.handleJobCancel(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	if got := jobStatusFor(jm, "job2"); got != JobStatusCanceled {
		t.Fatalf("job2 status = %q, want canceled", got)
	}

	close(block)
}

func TestHandleJobCancel_Running(t *testing.T) {
	jm := newJobManager(&ctxEngine{}, 1, context.Background())
	t.Cleanup(jm.Shutdown)
	srv := &Server{jobs: jm}

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "t1", "s1"); !started {
		t.Fatalf("start: expected success")
	}
	waitForJobStatus(t, jm, "job1", JobStatusRunning, 2*time.Second)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/job1/cancel", nil)
	req.SetPathValue("id", "job1")
	w := httptest.NewRecorder()
	srv.handleJobCancel(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	waitForJobStatus(t, jm, "job1", JobStatusCanceled, 2*time.Second)
}

func TestHandleJobCancel_NotFound(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/nope/cancel", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	srv.handleJobCancel(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleJobCancel_AlreadyFinished(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	js, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, "t1", "s1")
	if !started {
		t.Fatalf("start: expected success")
	}
	jm.complete(js.id, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/job1/cancel", nil)
	req.SetPathValue("id", "job1")
	w := httptest.NewRecorder()
	srv.handleJobCancel(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestHandleJobCancel_MissingID(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs//cancel", nil)
	w := httptest.NewRecorder()
	srv.handleJobCancel(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// --- ring buffer unit tests ---

func TestRingBuffer_Order(t *testing.T) {
	rb := newRingBuffer(3)
	rb.add("a")
	rb.add("b")
	rb.add("c")
	lines := rb.lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3, got %d", len(lines))
	}
	for i, want := range []string{"a", "b", "c"} {
		if lines[i] != want {
			t.Errorf("lines[%d] = %q, want %q", i, lines[i], want)
		}
	}
}

func TestRingBuffer_Wrap(t *testing.T) {
	rb := newRingBuffer(3)
	rb.add("a")
	rb.add("b")
	rb.add("c")
	rb.add("d") // evicts "a"
	lines := rb.lines()
	if len(lines) != 3 {
		t.Fatalf("expected 3, got %d", len(lines))
	}
	for i, want := range []string{"b", "c", "d"} {
		if lines[i] != want {
			t.Errorf("lines[%d] = %q, want %q", i, lines[i], want)
		}
	}
}

func TestRingBuffer_Empty(t *testing.T) {
	rb := newRingBuffer(5)
	if lines := rb.lines(); lines != nil {
		t.Errorf("expected nil, got %v", lines)
	}
}

// --- resolvePreset ---

func TestResolvePreset_Named(t *testing.T) {
	req := transcodeRequest{Preset: "Balanced"}
	p, err := resolvePreset(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Balanced" || p.Codec != transcode.CodecFLAC {
		t.Errorf("unexpected preset: %+v", p)
	}
}

func TestResolvePreset_Custom(t *testing.T) {
	level := 3
	req := transcodeRequest{Codec: "flac", CompressionLevel: &level}
	p, err := resolvePreset(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.CompressionLevel != 3 {
		t.Errorf("expected compression_level 3, got %d", p.CompressionLevel)
	}
}

func TestResolvePreset_UnknownCodec(t *testing.T) {
	req := transcodeRequest{Codec: "mp3"}
	_, err := resolvePreset(req)
	if err == nil {
		t.Error("expected error for unknown codec")
	}
}

// --- resolveOutputMode ---

func TestResolveOutputMode(t *testing.T) {
	cases := []struct {
		in   string
		want transcode.OutputMode
		ok   bool
	}{
		{"shared", transcode.OutputShared, true},
		{"local", transcode.OutputLocal, true},
		{"replace", transcode.OutputReplace, true},
		{"", transcode.OutputShared, true},
		{"invalid", "", false},
	}
	for _, tc := range cases {
		mode, err := resolveOutputMode(tc.in)
		if tc.ok && err != nil {
			t.Errorf("%q: unexpected error: %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%q: expected error", tc.in)
		}
		if tc.ok && mode != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, mode, tc.want)
		}
	}
}

// --- calcOverallPercent ---

func TestCalcOverallPercent(t *testing.T) {
	cases := []struct {
		p    transcode.ProgressReport
		want float64
	}{
		{transcode.ProgressReport{FileIndex: 0, TotalFiles: 2, FilePercent: 50}, 25},
		{transcode.ProgressReport{FileIndex: 1, TotalFiles: 2, FilePercent: 100}, 100},
		{transcode.ProgressReport{FileIndex: 0, TotalFiles: 0, FilePercent: 50}, 0},
	}
	for _, tc := range cases {
		got := calcOverallPercent(tc.p)
		if got != tc.want {
			t.Errorf("calcOverallPercent(%+v) = %v, want %v", tc.p, got, tc.want)
		}
	}
}
