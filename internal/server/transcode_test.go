package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

// --- GET /api/transcode/{jobID}/progress ---

func TestHandleTranscodeProgress_JobNotFound(t *testing.T) {
	srv, _, _, _ := newTestServerWithEngine(t, &mockEngine{})

	req := httptest.NewRequest(http.MethodGet, "/api/transcode/nope/progress", nil)
	req.SetPathValue("jobID", "nope")
	w := httptest.NewRecorder()

	srv.handleTranscodeProgress(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestHandleTranscodeProgress_SSEStream(t *testing.T) {
	reports := []transcode.ProgressReport{
		{CurrentFile: "track1.flac", FileIndex: 0, TotalFiles: 2, FilePercent: 50},
		{CurrentFile: "track2.flac", FileIndex: 1, TotalFiles: 2, FilePercent: 100},
	}
	eng := &mockEngine{reports: reports}
	srv, _, _, dirPath := newTestServerWithEngine(t, eng)

	// Start a job.
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

	// Wait for job to complete.
	js, _ := srv.jobs.get(jobID)
	select {
	case <-js.done:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not complete in time")
	}

	// Progress SSE for a completed job should return a done event.
	progReq := httptest.NewRequest(http.MethodGet, "/api/transcode/"+jobID+"/progress", nil)
	progReq.SetPathValue("jobID", jobID)
	progW := httptest.NewRecorder()
	srv.handleTranscodeProgress(progW, progReq)

	body2 := progW.Body.String()
	if !strings.Contains(body2, "event: done") {
		t.Errorf("expected 'event: done' in SSE output, got: %s", body2)
	}
}

func TestHandleTranscodeProgress_LiveStream(t *testing.T) {
	block := make(chan struct{})
	reports := []transcode.ProgressReport{
		{CurrentFile: "track1.flac", FileIndex: 0, TotalFiles: 1, FilePercent: 75},
	}
	eng := &mockEngine{reports: reports, block: block}
	srv, _, _, dirPath := newTestServerWithEngine(t, eng)

	// Start job.
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

	// Wait for progress reports to be consumed by the fanout goroutine.
	time.Sleep(100 * time.Millisecond)

	// Connect a progress SSE client.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	progReq := httptest.NewRequest(http.MethodGet, "/api/transcode/"+jobID+"/progress", nil).WithContext(ctx)
	progReq.SetPathValue("jobID", jobID)
	progW := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleTranscodeProgress(progW, progReq)
	}()

	// Give the SSE handler time to subscribe before the job exits.
	time.Sleep(50 * time.Millisecond)

	// Unblock the engine so the job finishes and SSE stream closes.
	close(block)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SSE handler did not return after job completion")
	}

	if !strings.Contains(progW.Body.String(), "event: done") {
		t.Errorf("expected 'event: done', got: %s", progW.Body.String())
	}
	// Regression: the terminal SSE event must report status "done" once the engine
	// has returned without error. A prior race between the fanout goroutine closing
	// subscriber channels and jm.complete setting js.status caused the SSE handler
	// to emit status "running" here, which the UI surfaced as "Transcoding failed".
	if !strings.Contains(progW.Body.String(), `"status":"done"`) {
		t.Errorf("expected terminal event status=done, got: %s", progW.Body.String())
	}
	if !strings.Contains(progW.Body.String(), `"current_file_number":1`) {
		t.Errorf("expected current_file_number in progress payload, got: %s", progW.Body.String())
	}
	if !strings.Contains(progW.Body.String(), `"total_files":1`) {
		t.Errorf("expected total_files in progress payload, got: %s", progW.Body.String())
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

// --- SSE event format ---

func TestSSEEventFormat(t *testing.T) {
	eng := &mockEngine{
		reports: []transcode.ProgressReport{
			{CurrentFile: "song.flac", FileIndex: 0, TotalFiles: 1, FilePercent: 50},
		},
	}
	srv, _, _, dirPath := newTestServerWithEngine(t, eng)

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

	req := httptest.NewRequest(http.MethodGet, "/api/transcode/"+jobID+"/progress", nil)
	req.SetPathValue("jobID", jobID)
	w := httptest.NewRecorder()
	srv.handleTranscodeProgress(w, req)

	// Parse SSE events.
	scanner := bufio.NewScanner(strings.NewReader(w.Body.String()))
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, after)
		}
	}
	if len(events) == 0 {
		t.Error("expected at least one SSE event")
	}
	last := events[len(events)-1]
	if last != "done" {
		t.Errorf("expected last event 'done', got %q", last)
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
