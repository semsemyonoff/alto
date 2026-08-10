package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// newTestServerWithDirTracks builds a test server whose seeded "album1"
// directory holds exactly the given tracks, so a test can describe any
// lossless/lossy mix it needs. Returns the server and the absolute directory path.
func newTestServerWithDirTracks(t *testing.T, eng TranscodeEngine, tracks []db.Track) (*Server, string) {
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

	albumDir := filepath.Join(libDir, "album1")
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	dirID, err := database.UpsertDirectory(libID, "album1", "MIXED", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	for _, tr := range tracks {
		tr.DirectoryID = dirID
		if tr.Duration == 0 {
			tr.Duration = 10
		}
		if tr.Size == 0 {
			tr.Size = 1000
		}
		if err := database.UpsertTrack(tr); err != nil {
			t.Fatalf("UpsertTrack %q: %v", tr.Filename, err)
		}
	}

	cfg := Config{
		Libraries: []LibraryConfig{{ID: libID, Name: "TestLib", Path: libDir}},
		OutputDir: t.TempDir(),
	}
	srv := NewWithEngine(database, &mockScanner{}, eng, cfg)
	t.Cleanup(srv.Shutdown)
	return srv, albumDir
}

// postTranscode posts body to handleTranscodeStart and returns the recorder.
func postTranscode(t *testing.T, srv *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleTranscodeStart(w, req)
	return w
}

// errorCode reads the "code" field of an API error envelope.
func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var resp struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return resp.Code
}

// jobFileNames returns the file names the started job was built with.
func jobFileNames(t *testing.T, srv *Server, jobID string) []string {
	t.Helper()
	js, ok := srv.jobs.get(jobID)
	if !ok {
		t.Fatalf("job %q not found", jobID)
	}
	srv.jobs.mu.Lock()
	defer srv.jobs.mu.Unlock()
	names := make([]string, 0, len(js.job.Files))
	for _, f := range js.job.Files {
		names = append(names, f.Name)
	}
	return names
}

var (
	flacTracks  = []db.Track{{Filename: "01.flac", Codec: "flac"}, {Filename: "02.flac", Codec: "flac"}}
	mixedTracks = []db.Track{
		{Filename: "01.flac", Codec: "flac"},
		{Filename: "02.mp3", Codec: "mp3"},
		{Filename: "03.flac", Codec: "flac"},
	}
	lossyTracks = []db.Track{{Filename: "01.mp3", Codec: "mp3"}, {Filename: "02.mp3", Codec: "mp3"}}
)

// TestHandleTranscodeStart_SelectionMatrix walks every row of the plan's
// selection matrix, asserting the machine-readable code rather than the message.
func TestHandleTranscodeStart_SelectionMatrix(t *testing.T) {
	cases := []struct {
		name       string
		tracks     []db.Track
		extra      map[string]any
		wantStatus int
		wantCode   string
		wantFiles  []string
	}{
		{
			name:       "no selection, all lossless",
			tracks:     flacTracks,
			wantStatus: http.StatusAccepted,
			wantFiles:  []string{"01.flac", "02.flac"},
		},
		{
			name:       "no selection, mixed",
			tracks:     mixedTracks,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeMixedDirectory,
		},
		{
			name:       "skip_lossy, all lossless",
			tracks:     flacTracks,
			extra:      map[string]any{"skip_lossy": true},
			wantStatus: http.StatusAccepted,
			wantFiles:  []string{"01.flac", "02.flac"},
		},
		{
			name:       "skip_lossy, mixed",
			tracks:     mixedTracks,
			extra:      map[string]any{"skip_lossy": true},
			wantStatus: http.StatusAccepted,
			wantFiles:  []string{"01.flac", "03.flac"},
		},
		{
			name:       "skip_lossy, all lossy",
			tracks:     lossyTracks,
			extra:      map[string]any{"skip_lossy": true},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeNoLosslessTracks,
		},
		{
			name:       "files narrows a mixed directory",
			tracks:     mixedTracks,
			extra:      map[string]any{"files": []string{"03.flac"}},
			wantStatus: http.StatusAccepted,
			wantFiles:  []string{"03.flac"},
		},
		{
			name:       "both selections present",
			tracks:     mixedTracks,
			extra:      map[string]any{"skip_lossy": true, "files": []string{"01.flac"}},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
		{
			name:       "empty files list",
			tracks:     flacTracks,
			extra:      map[string]any{"files": []string{}},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
		{
			name:       "files naming an unindexed track",
			tracks:     flacTracks,
			extra:      map[string]any{"files": []string{"99.flac"}},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeUnknownFile,
		},
		{
			name:       "files naming a lossy track",
			tracks:     mixedTracks,
			extra:      map[string]any{"files": []string{"02.mp3"}},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeLossySourceSelected,
		},
		{
			name:       "files carrying a path separator",
			tracks:     flacTracks,
			extra:      map[string]any{"files": []string{"../01.flac"}},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
		{
			name:       "files carrying a duplicate",
			tracks:     flacTracks,
			extra:      map[string]any{"files": []string{"01.flac", "01.flac"}},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, tc.tracks)

			body := map[string]any{"path": dirPath, "preset": "Balanced", "output_mode": "shared"}
			maps.Copy(body, tc.extra)
			w := postTranscode(t, srv, body)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantCode != "" {
				if got := errorCode(t, w); got != tc.wantCode {
					t.Fatalf("code = %q, want %q: %s", got, tc.wantCode, w.Body.String())
				}
				return
			}

			var resp map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			jobID, _ := resp["job_id"].(string)
			if jobID == "" {
				t.Fatalf("expected job_id, got %s", w.Body.String())
			}
			got := jobFileNames(t, srv, jobID)
			if strings.Join(got, ",") != strings.Join(tc.wantFiles, ",") {
				t.Errorf("job files = %v, want %v", got, tc.wantFiles)
			}
		})
	}
}

// TestHandleTranscodeStart_FilesNilVsEmpty pins the nil-vs-empty distinction:
// an absent "files" key falls through to the all-or-nothing gate, while a
// present-but-empty one is a bad request — testing length instead of presence
// would collapse the two.
func TestHandleTranscodeStart_FilesNilVsEmpty(t *testing.T) {
	t.Run("absent files on a mixed directory hits the gate", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, mixedTracks)
		w := postTranscode(t, srv, map[string]any{"path": dirPath, "preset": "Balanced"})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
		}
		if got := errorCode(t, w); got != codeMixedDirectory {
			t.Errorf("code = %q, want %q", got, codeMixedDirectory)
		}
	})

	t.Run("empty files is rejected, not treated as absent", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, flacTracks)
		w := postTranscode(t, srv, map[string]any{"path": dirPath, "preset": "Balanced", "files": []string{}})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		if got := errorCode(t, w); got != codeInvalidRequest {
			t.Errorf("code = %q, want %q", got, codeInvalidRequest)
		}
	})

	t.Run("empty files together with skip_lossy is still rejected", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, mixedTracks)
		w := postTranscode(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "skip_lossy": true, "files": []string{},
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		if got := errorCode(t, w); got != codeInvalidRequest {
			t.Errorf("code = %q, want %q", got, codeInvalidRequest)
		}
	})
}

// TestHandleTranscodeStart_OutputNameConflict covers the pre-existing defect:
// an all-lossless directory holding "01 A.ape" and "01 A.flac" passes the
// all-or-nothing gate untouched, and both sources render to "01 A.flac" — with
// ffmpeg's -y the second silently overwrote the first.
func TestHandleTranscodeStart_OutputNameConflict(t *testing.T) {
	tracks := []db.Track{
		{Filename: "01 A.ape", Codec: "ape"},
		{Filename: "01 A.flac", Codec: "flac"},
		{Filename: "02 B.flac", Codec: "flac"},
	}

	t.Run("no selection at all", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, tracks)
		w := postTranscode(t, srv, map[string]any{"path": dirPath, "preset": "Balanced"})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
		}
		if got := errorCode(t, w); got != codeOutputNameConflict {
			t.Fatalf("code = %q, want %q", got, codeOutputNameConflict)
		}

		var resp struct {
			Conflicts []outputConflictDTO `json:"conflicts"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Conflicts) != 1 || resp.Conflicts[0].Output != "01 A.flac" {
			t.Fatalf("conflicts = %+v, want one on %q", resp.Conflicts, "01 A.flac")
		}
		if strings.Join(resp.Conflicts[0].Sources, ",") != "01 A.ape,01 A.flac" {
			t.Errorf("sources = %v", resp.Conflicts[0].Sources)
		}
	})

	t.Run("selection that avoids the collision starts", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, tracks)
		w := postTranscode(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "files": []string{"01 A.flac", "02 B.flac"},
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
		}
	})

	t.Run("no conflict when the target codec keeps the names distinct", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, []db.Track{
			{Filename: "01 A.flac", Codec: "flac"},
			{Filename: "02 B.flac", Codec: "flac"},
		})
		w := postTranscode(t, srv, map[string]any{"path": dirPath, "preset": "Balanced"})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
		}
	})
}

// TestHandleTranscodeStart_NoSelectionUnchanged pins the default path: an
// all-lossless directory with no selection still transcodes every track.
func TestHandleTranscodeStart_NoSelectionUnchanged(t *testing.T) {
	srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, flacTracks)

	w := postTranscode(t, srv, map[string]any{"path": dirPath, "preset": "Balanced", "output_mode": "shared"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	jobID, _ := resp["job_id"].(string)
	if got := jobFileNames(t, srv, jobID); strings.Join(got, ",") != "01.flac,02.flac" {
		t.Errorf("job files = %v, want both tracks", got)
	}
}

// acceptedBody decodes a 202 transcode response.
func acceptedBody(t *testing.T, w *httptest.ResponseRecorder) transcodeAcceptedDTO {
	t.Helper()
	var resp transcodeAcceptedDTO
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 202 body %q: %v", w.Body.String(), err)
	}
	if resp.JobID == "" {
		t.Fatalf("expected job_id in %s", w.Body.String())
	}
	return resp
}

// jobSub returns the queue-panel sub line the started job was created with.
func jobSub(t *testing.T, srv *Server, jobID string) string {
	t.Helper()
	js, ok := srv.jobs.get(jobID)
	if !ok {
		t.Fatalf("job %q not found", jobID)
	}
	srv.jobs.mu.Lock()
	defer srv.jobs.mu.Unlock()
	return js.sub
}

// TestHandleTranscodeStart_AcceptedBody asserts the 202 names both halves of
// the resolved selection, with the reason matching how it was narrowed.
func TestHandleTranscodeStart_AcceptedBody(t *testing.T) {
	tests := []struct {
		name        string
		tracks      []db.Track
		extra       map[string]any
		wantFiles   []string
		wantSkipped []skippedDTO
	}{
		{
			name:      "skip_lossy reports the lossy tracks",
			tracks:    mixedTracks,
			extra:     map[string]any{"skip_lossy": true},
			wantFiles: []string{"01.flac", "03.flac"},
			wantSkipped: []skippedDTO{
				{Name: "02.mp3", Codec: "mp3", Reason: skipReasonLossy},
			},
		},
		{
			name:      "files reports the unselected tracks",
			tracks:    mixedTracks,
			extra:     map[string]any{"files": []string{"03.flac"}},
			wantFiles: []string{"03.flac"},
			wantSkipped: []skippedDTO{
				{Name: "01.flac", Codec: "flac", Reason: skipReasonNotSelected},
				{Name: "02.mp3", Codec: "mp3", Reason: skipReasonNotSelected},
			},
		},
		{
			name:        "skip_lossy on an all-lossless directory skips nothing",
			tracks:      flacTracks,
			extra:       map[string]any{"skip_lossy": true},
			wantFiles:   []string{"01.flac", "02.flac"},
			wantSkipped: []skippedDTO{},
		},
		{
			name:        "no selection reports every file and an empty skipped list",
			tracks:      flacTracks,
			wantFiles:   []string{"01.flac", "02.flac"},
			wantSkipped: []skippedDTO{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, tt.tracks)

			body := map[string]any{"path": dirPath, "preset": "Balanced", "output_mode": "shared"}
			maps.Copy(body, tt.extra)
			w := postTranscode(t, srv, body)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
			}

			resp := acceptedBody(t, w)
			if !reflect.DeepEqual(resp.Files, tt.wantFiles) {
				t.Errorf("files = %v, want %v", resp.Files, tt.wantFiles)
			}
			if !reflect.DeepEqual(resp.Skipped, tt.wantSkipped) {
				t.Errorf("skipped = %+v, want %+v", resp.Skipped, tt.wantSkipped)
			}
			// The body must describe the job that was actually started.
			if got := jobFileNames(t, srv, resp.JobID); !reflect.DeepEqual(got, resp.Files) {
				t.Errorf("job files = %v, but body reported %v", got, resp.Files)
			}
		})
	}
}

// TestHandleTranscodeStart_AcceptedBodyEmitsArrays pins the wire shape: both
// lists are JSON arrays even when empty, so clients never see null.
func TestHandleTranscodeStart_AcceptedBodyEmitsArrays(t *testing.T) {
	srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, flacTracks)

	w := postTranscode(t, srv, map[string]any{"path": dirPath, "preset": "Balanced"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	var raw struct {
		Files   json.RawMessage `json:"files"`
		Skipped json.RawMessage `json:"skipped"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw.Skipped) != "[]" {
		t.Errorf("skipped = %s, want []", raw.Skipped)
	}
	if !strings.HasPrefix(string(raw.Files), "[") {
		t.Errorf("files = %s, want an array", raw.Files)
	}
}

// TestHandleTranscodeStart_SubUsesSelectedCodec covers a mixed directory whose
// first track is lossy: the sub line must describe the codec being transcoded,
// not the one being skipped.
func TestHandleTranscodeStart_SubUsesSelectedCodec(t *testing.T) {
	tracks := []db.Track{
		{Filename: "01.mp3", Codec: "mp3"},
		{Filename: "02.flac", Codec: "flac"},
	}

	t.Run("skip_lossy", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, tracks)
		w := postTranscode(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "skip_lossy": true,
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
		}
		if got := jobSub(t, srv, acceptedBody(t, w).JobID); !strings.HasPrefix(got, "flac → ") {
			t.Errorf("sub = %q, want it to start from the selected flac source", got)
		}
	})

	t.Run("explicit files", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, tracks)
		w := postTranscode(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "files": []string{"02.flac"},
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
		}
		if got := jobSub(t, srv, acceptedBody(t, w).JobID); !strings.HasPrefix(got, "flac → ") {
			t.Errorf("sub = %q, want it to start from the selected flac source", got)
		}
	})
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
	if got := acceptedBody(t, w).Files; !reflect.DeepEqual(got, []string{"track1.flac", "track2.flac"}) {
		t.Errorf("files = %v, want both seeded tracks", got)
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
