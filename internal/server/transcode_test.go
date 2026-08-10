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

// jobPassthroughNames returns the file names the started job will copy verbatim.
func jobPassthroughNames(t *testing.T, srv *Server, jobID string) []string {
	t.Helper()
	js, ok := srv.jobs.get(jobID)
	if !ok {
		t.Fatalf("job %q not found", jobID)
	}
	srv.jobs.mu.Lock()
	defer srv.jobs.mu.Unlock()
	names := make([]string, 0, len(js.job.Passthrough))
	for _, f := range js.job.Passthrough {
		names = append(names, f.Name)
	}
	return names
}

// startedJobID posts a transcode request that must be accepted and returns its job ID.
func startedJobID(t *testing.T, srv *Server, body map[string]any) string {
	t.Helper()
	w := postTranscode(t, srv, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
	}
	return acceptedBody(t, w).JobID
}

// TestHandleTranscodeStart_CopySkipped covers copy_skipped end to end: the
// skipped tracks become the job's pass-through list, and only when asked for.
func TestHandleTranscodeStart_CopySkipped(t *testing.T) {
	t.Run("mixed album produces a complete output directory", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, mixedTracks)
		id := startedJobID(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "output_mode": "shared",
			"skip_lossy": true, "copy_skipped": true,
		})

		if got := jobFileNames(t, srv, id); strings.Join(got, ",") != "01.flac,03.flac" {
			t.Errorf("job files = %v, want the lossless tracks", got)
		}
		if got := jobPassthroughNames(t, srv, id); strings.Join(got, ",") != "02.mp3" {
			t.Errorf("job passthrough = %v, want the lossy track", got)
		}
	})

	t.Run("without copy_skipped nothing is passed through", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, mixedTracks)
		id := startedJobID(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "skip_lossy": true,
		})
		if got := jobPassthroughNames(t, srv, id); len(got) != 0 {
			t.Errorf("job passthrough = %v, want none", got)
		}
	})

	t.Run("explicit files selection passes the rest through", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, mixedTracks)
		id := startedJobID(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced",
			"files": []string{"01.flac"}, "copy_skipped": true,
		})
		if got := jobPassthroughNames(t, srv, id); strings.Join(got, ",") != "02.mp3,03.flac" {
			t.Errorf("job passthrough = %v, want the unselected tracks", got)
		}
	})

	t.Run("no selection leaves the pass-through list empty", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, flacTracks)
		id := startedJobID(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "copy_skipped": true,
		})
		if got := jobPassthroughNames(t, srv, id); len(got) != 0 {
			t.Errorf("job passthrough = %v, want none", got)
		}
	})
}

// TestHandleTranscodeStart_CopySkippedWithReplace pins the refusal: in replace
// mode the skipped originals are already in place, so the flag is meaningless
// and silently ignoring it would hide the mistake.
func TestHandleTranscodeStart_CopySkippedWithReplace(t *testing.T) {
	t.Run("rejected with a dedicated code", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, mixedTracks)
		w := postTranscode(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "output_mode": "replace",
			"skip_lossy": true, "copy_skipped": true,
		})
		assertAPIError(t, w, http.StatusBadRequest, codeCopySkippedNotApplicable)
	})

	t.Run("replace without copy_skipped still starts", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, mixedTracks)
		w := postTranscode(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "output_mode": "replace", "skip_lossy": true,
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
		}
	})
}

// TestHandleTranscodeStart_PassthroughNameConflict extends the collision
// detector to the pass-through set: a selected "01 A.ape" renders to
// "01 A.flac", which is exactly the name a copied "01 A.flac" would claim.
func TestHandleTranscodeStart_PassthroughNameConflict(t *testing.T) {
	// "01 A.flac" is lossless, so skip_lossy would keep it; an explicit files
	// list is the only way to leave it unselected.
	tracks := []db.Track{
		{Filename: "01 A.ape", Codec: "ape"},
		{Filename: "01 A.flac", Codec: "flac"},
	}

	t.Run("selected versus pass-through", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, tracks)
		w := postTranscode(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced",
			"files": []string{"01 A.ape"}, "copy_skipped": true,
		})
		assertAPIError(t, w, http.StatusUnprocessableEntity, codeOutputNameConflict)

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
			t.Errorf("sources = %v, want the selected source then the pass-through", resp.Conflicts[0].Sources)
		}
	})

	t.Run("the same selection without copy_skipped starts", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, tracks)
		w := postTranscode(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "files": []string{"01 A.ape"},
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
		}
	})

	t.Run("selected versus selected still fails with copy_skipped on", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, tracks)
		w := postTranscode(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "copy_skipped": true,
		})
		assertAPIError(t, w, http.StatusUnprocessableEntity, codeOutputNameConflict)
	})

	t.Run("a pass-through name distinct from every output is fine", func(t *testing.T) {
		srv, dirPath := newTestServerWithDirTracks(t, &mockEngine{}, mixedTracks)
		w := postTranscode(t, srv, map[string]any{
			"path": dirPath, "preset": "Balanced", "skip_lossy": true, "copy_skipped": true,
		})
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", w.Code, w.Body.String())
		}
	})
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
	if got := errorCode(t, w); got != codeMixedDirectory {
		t.Fatalf("code = %q, want %q", got, codeMixedDirectory)
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
	if resp["code"] != codeJobAlreadyRunning {
		t.Errorf("code = %q, want %q", resp["code"], codeJobAlreadyRunning)
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

// TestHandleTranscodeStart_PathNotOnDisk covers a path that does not exist at
// all — distinct from a path that exists but carries no index row, which
// answers not_indexed (see TestHandleTranscodeStart_RejectionCodes).
func TestHandleTranscodeStart_PathNotOnDisk(t *testing.T) {
	eng := &mockEngine{}
	srv, _, libDir, _ := newTestServerWithEngine(t, eng)

	body := map[string]any{"path": libDir + "/nonexistent", "preset": "Balanced", "output_mode": "shared"}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/transcode", bytes.NewReader(b))
	w := httptest.NewRecorder()

	srv.handleTranscodeStart(w, req)

	assertAPIError(t, w, http.StatusNotFound, codePathNotFound)
}

// assertAPIError asserts a machine-readable error envelope: status, JSON
// content type, and the documented code.
func assertAPIError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	if got := errorCode(t, w); got != code {
		t.Errorf("code = %q, want %q: %s", got, code, w.Body.String())
	}
}

// TestHandleTranscodeStart_RejectionCodes walks every rejection path of
// POST /api/transcode and asserts its documented code, so clients can branch on
// `code` instead of matching message strings.
func TestHandleTranscodeStart_RejectionCodes(t *testing.T) {
	cases := []struct {
		name       string
		tracks     []db.Track
		prepare    func(*Server)
		body       func(t *testing.T, dir string) map[string]any
		wantStatus int
		wantCode   string
	}{
		{
			name:    "engine unavailable",
			tracks:  flacTracks,
			prepare: func(s *Server) { s.engine = nil },
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Balanced"}
			},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   codeEngineUnavailable,
		},
		{
			name:   "missing path",
			tracks: flacTracks,
			body: func(_ *testing.T, _ string) map[string]any {
				return map[string]any{"preset": "Balanced"}
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
		{
			name:   "path outside library",
			tracks: flacTracks,
			body: func(_ *testing.T, _ string) map[string]any {
				return map[string]any{"path": "/etc", "preset": "Balanced"}
			},
			wantStatus: http.StatusForbidden,
			wantCode:   codePathForbidden,
		},
		{
			name:   "app-owned path segment",
			tracks: flacTracks,
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": filepath.Join(dir, transcode.LocalOutputDirName), "preset": "Balanced"}
			},
			wantStatus: http.StatusForbidden,
			wantCode:   codePathForbidden,
		},
		{
			name:   "path missing on disk",
			tracks: flacTracks,
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": filepath.Join(dir, "nope"), "preset": "Balanced"}
			},
			wantStatus: http.StatusNotFound,
			wantCode:   codePathNotFound,
		},
		{
			// Exists on disk, absent from the index: the remedy is a scan, not
			// a corrected path — hence a different code from path_not_found.
			name:   "directory not indexed",
			tracks: flacTracks,
			body: func(t *testing.T, dir string) map[string]any {
				unindexed := filepath.Join(filepath.Dir(dir), "unindexed")
				if err := os.MkdirAll(unindexed, 0o755); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				return map[string]any{"path": unindexed, "preset": "Balanced"}
			},
			wantStatus: http.StatusNotFound,
			wantCode:   codeNotIndexed,
		},
		{
			name:   "directory has no tracks",
			tracks: nil,
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Balanced"}
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeNoTracks,
		},
		{
			name:   "unknown preset",
			tracks: flacTracks,
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Nope"}
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
		{
			name:   "unknown output mode",
			tracks: flacTracks,
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Balanced", "output_mode": "sideways"}
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
		{
			name:    "shared mode without output dir",
			tracks:  flacTracks,
			prepare: func(s *Server) { s.cfg.OutputDir = "" },
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Balanced", "output_mode": "shared"}
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeOutputDirNotConfigured,
		},
		{
			name:   "mixed directory without selection",
			tracks: mixedTracks,
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Balanced"}
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeMixedDirectory,
		},
		{
			name:   "skip_lossy with no lossless track",
			tracks: lossyTracks,
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Balanced", "skip_lossy": true}
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeNoLosslessTracks,
		},
		{
			name:   "skip_lossy together with files",
			tracks: mixedTracks,
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Balanced", "skip_lossy": true, "files": []string{"01.flac"}}
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
		{
			name:   "unknown file name",
			tracks: flacTracks,
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Balanced", "files": []string{"99.flac"}}
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeUnknownFile,
		},
		{
			name:   "lossy source selected",
			tracks: mixedTracks,
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Balanced", "files": []string{"02.mp3"}}
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeLossySourceSelected,
		},
		{
			name:   "output name conflict",
			tracks: []db.Track{{Filename: "01.ape", Codec: "ape"}, {Filename: "01.flac", Codec: "flac"}},
			body: func(_ *testing.T, dir string) map[string]any {
				return map[string]any{"path": dir, "preset": "Balanced"}
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeOutputNameConflict,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, dir := newTestServerWithDirTracks(t, &mockEngine{}, tc.tracks)
			if tc.prepare != nil {
				tc.prepare(srv)
			}
			w := postTranscode(t, srv, tc.body(t, dir))
			assertAPIError(t, w, tc.wantStatus, tc.wantCode)
		})
	}
}

// TestHandleTranscodeStart_MalformedBody covers the one rejection postTranscode
// cannot express, since it marshals a valid map.
func TestHandleTranscodeStart_MalformedBody(t *testing.T) {
	srv, _ := newTestServerWithDirTracks(t, &mockEngine{}, flacTracks)

	req := httptest.NewRequest(http.MethodPost, "/api/transcode", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleTranscodeStart(w, req)

	assertAPIError(t, w, http.StatusBadRequest, codeInvalidRequest)
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

	assertAPIError(t, w, http.StatusNotFound, codeJobNotFound)
}

func TestHandleTranscodeLog_InvalidN(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}
	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
		t.Fatalf("start: expected success")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/transcode/job1/log?n=0", nil)
	req.SetPathValue("jobID", "job1")
	w := httptest.NewRecorder()
	srv.handleTranscodeLog(w, req)

	assertAPIError(t, w, http.StatusBadRequest, codeInvalidRequest)
}

func TestHandleTranscodeLog_MissingID(t *testing.T) {
	srv := &Server{jobs: newJobManager(nil, 0, context.Background())}

	req := httptest.NewRequest(http.MethodGet, "/api/transcode//log", nil)
	w := httptest.NewRecorder()
	srv.handleTranscodeLog(w, req)

	assertAPIError(t, w, http.StatusBadRequest, codeInvalidRequest)
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
	js1, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "Album One", sub: "flac -> opus/Balanced"})
	if !started {
		t.Fatalf("start job1: expected success")
	}
	jm.complete(js1.id, nil)

	// job2: queued, then failed.
	js2, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, jobMeta{title: "Album Two", sub: "flac -> opus/Balanced"})
	if !started {
		t.Fatalf("start job2: expected success")
	}
	jm.complete(js2.id, errors.New("boom"))

	// job3: canceled while still queued.
	if _, started := jm.start("job3", "/dir3", transcode.Job{ID: "job3"}, jobMeta{title: "Album Three", sub: "flac -> opus/Balanced"}); !started {
		t.Fatalf("start job3: expected success")
	}
	if result := jm.cancel("job3"); result != cancelResultCanceled {
		t.Fatalf("cancel job3: expected canceled, got %v", result)
	}

	// job4: still queued.
	if _, started := jm.start("job4", "/dir4", transcode.Job{ID: "job4"}, jobMeta{title: "Album Four", sub: "flac -> opus/Balanced"}); !started {
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
	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "Album One", sub: "flac -> opus/Balanced"}); !started {
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
	if _, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, jobMeta{title: "Album Two", sub: "flac -> opus/Balanced"}); !started {
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

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
		t.Fatalf("start job1: expected success")
	}
	if _, started := jm.start("job2", "/dir2", transcode.Job{ID: "job2"}, jobMeta{title: "t2", sub: "s2"}); !started {
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

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
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

	assertAPIError(t, w, http.StatusNotFound, codeJobNotFound)
}

func TestHandleJobCancel_AlreadyFinished(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	js, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"})
	if !started {
		t.Fatalf("start: expected success")
	}
	jm.complete(js.id, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/job1/cancel", nil)
	req.SetPathValue("id", "job1")
	w := httptest.NewRecorder()
	srv.handleJobCancel(w, req)

	assertAPIError(t, w, http.StatusConflict, codeJobAlreadyFinished)
}

func TestHandleJobCancel_MissingID(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs//cancel", nil)
	w := httptest.NewRecorder()
	srv.handleJobCancel(w, req)

	assertAPIError(t, w, http.StatusBadRequest, codeInvalidRequest)
}

// --- POST /api/jobs/{id}/remove ---

func TestHandleJobRemove_Terminal(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	js, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"})
	if !started {
		t.Fatalf("start: expected success")
	}
	jm.complete(js.id, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/job1/remove", nil)
	req.SetPathValue("id", "job1")
	w := httptest.NewRecorder()
	srv.handleJobRemove(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleJobRemove_NotFound(t *testing.T) {
	srv := &Server{jobs: newJobManager(nil, 0, context.Background())}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/nope/remove", nil)
	req.SetPathValue("id", "nope")
	w := httptest.NewRecorder()
	srv.handleJobRemove(w, req)

	assertAPIError(t, w, http.StatusNotFound, codeJobNotFound)
}

func TestHandleJobRemove_StillActive(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	if _, started := jm.start("job1", "/dir1", transcode.Job{ID: "job1"}, jobMeta{title: "t1", sub: "s1"}); !started {
		t.Fatalf("start: expected success")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs/job1/remove", nil)
	req.SetPathValue("id", "job1")
	w := httptest.NewRecorder()
	srv.handleJobRemove(w, req)

	assertAPIError(t, w, http.StatusConflict, codeJobAlreadyRunning)
}

func TestHandleJobRemove_MissingID(t *testing.T) {
	srv := &Server{jobs: newJobManager(nil, 0, context.Background())}

	req := httptest.NewRequest(http.MethodPost, "/api/jobs//remove", nil)
	w := httptest.NewRecorder()
	srv.handleJobRemove(w, req)

	assertAPIError(t, w, http.StatusBadRequest, codeInvalidRequest)
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

// --- GET /api/jobs/{id} ---

// getJobDetail calls handleJob for id and returns the recorder.
func getJobDetail(t *testing.T, srv *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/jobs/"+id, nil)
	req.SetPathValue("id", id)
	w := httptest.NewRecorder()
	srv.handleJob(w, req)
	return w
}

// decodeJobDetail decodes a 200 detail body, failing the test on any other status.
func decodeJobDetail(t *testing.T, w *httptest.ResponseRecorder) jobDetailDTO {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var got jobDetailDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode detail body %q: %v", w.Body.String(), err)
	}
	return got
}

// twoFileJob is a job descriptor with two files, so total/done counts are
// distinguishable from zero.
func twoFileJob(id string) transcode.Job {
	return transcode.Job{
		ID:    id,
		Files: []transcode.FileInfo{{Name: "01.flac"}, {Name: "02.flac"}},
	}
}

// TestHandleJob_Lifecycle asserts the detail payload across every job status,
// including that a failed job carries the engine's error text — so a client
// never has to fetch the log endpoint to learn why a job failed.
func TestHandleJob_Lifecycle(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) *Server
		check func(t *testing.T, got jobDetailDTO)
	}{
		{
			name: "queued",
			setup: func(t *testing.T) *Server {
				jm := newJobManager(nil, 0, context.Background())
				if _, ok := jm.start("job1", "/dir1", twoFileJob("job1"), jobMeta{title: "Album", sub: "flac → opus/Balanced"}); !ok {
					t.Fatalf("start: expected success")
				}
				return &Server{jobs: jm}
			},
			check: func(t *testing.T, got jobDetailDTO) {
				if got.Status != JobStatusQueued || got.Pct != 0 || got.DoneFiles != 0 {
					t.Errorf("got %+v, want queued at 0%% with 0 done files", got)
				}
				if got.StartedAt != nil || got.FinishedAt != nil {
					t.Errorf("started_at = %v, finished_at = %v, want both null while queued", got.StartedAt, got.FinishedAt)
				}
			},
		},
		{
			name: "running",
			setup: func(t *testing.T) *Server {
				block := make(chan struct{})
				jm := newJobManager(&blockingEngine{block: block}, 1, context.Background())
				t.Cleanup(func() {
					close(block)
					jm.Shutdown()
				})
				if _, ok := jm.start("job1", "/dir1", twoFileJob("job1"), jobMeta{title: "Album", sub: "flac → opus/Balanced"}); !ok {
					t.Fatalf("start: expected success")
				}
				waitForJobStatus(t, jm, "job1", JobStatusRunning, 2*time.Second)
				return &Server{jobs: jm}
			},
			check: func(t *testing.T, got jobDetailDTO) {
				if got.Status != JobStatusRunning {
					t.Errorf("status = %q, want running", got.Status)
				}
				if got.StartedAt == nil {
					t.Error("started_at = null, want a timestamp once a worker picked the job up")
				}
				if got.FinishedAt != nil {
					t.Errorf("finished_at = %v, want null while running", got.FinishedAt)
				}
			},
		},
		{
			name: "done",
			setup: func(t *testing.T) *Server {
				jm := newJobManager(nil, 0, context.Background())
				if _, ok := jm.start("job1", "/dir1", twoFileJob("job1"), jobMeta{title: "Album", sub: "flac → opus/Balanced"}); !ok {
					t.Fatalf("start: expected success")
				}
				jm.complete("job1", nil)
				return &Server{jobs: jm}
			},
			check: func(t *testing.T, got jobDetailDTO) {
				if got.Status != JobStatusDone || got.Pct != 100 {
					t.Errorf("got %+v, want done at 100%%", got)
				}
				if got.DoneFiles != got.TotalFiles || got.TotalFiles != 2 {
					t.Errorf("done_files/total_files = %d/%d, want 2/2", got.DoneFiles, got.TotalFiles)
				}
				if got.Error != "" {
					t.Errorf("error = %q, want empty on a successful job", got.Error)
				}
				if got.FinishedAt == nil {
					t.Error("finished_at = null, want a timestamp on a terminal job")
				}
			},
		},
		{
			name: "failed",
			setup: func(t *testing.T) *Server {
				jm := newJobManager(nil, 0, context.Background())
				if _, ok := jm.start("job1", "/dir1", twoFileJob("job1"), jobMeta{title: "Album", sub: "flac → opus/Balanced"}); !ok {
					t.Fatalf("start: expected success")
				}
				jm.complete("job1", errors.New("ffmpeg exited 1"))
				return &Server{jobs: jm}
			},
			check: func(t *testing.T, got jobDetailDTO) {
				if got.Status != JobStatusFailed {
					t.Errorf("status = %q, want failed", got.Status)
				}
				if got.Error != "ffmpeg exited 1" {
					t.Errorf("error = %q, want the engine's failure reason", got.Error)
				}
			},
		},
		{
			name: "canceled while queued",
			setup: func(t *testing.T) *Server {
				jm := newJobManager(nil, 0, context.Background())
				if _, ok := jm.start("job1", "/dir1", twoFileJob("job1"), jobMeta{title: "Album", sub: "flac → opus/Balanced"}); !ok {
					t.Fatalf("start: expected success")
				}
				if res := jm.cancel("job1"); res != cancelResultCanceled {
					t.Fatalf("cancel = %v, want canceled", res)
				}
				return &Server{jobs: jm}
			},
			check: func(t *testing.T, got jobDetailDTO) {
				if got.Status != JobStatusCanceled {
					t.Errorf("status = %q, want canceled", got.Status)
				}
				if got.StartedAt != nil {
					t.Errorf("started_at = %v, want null for a job canceled before it ran", got.StartedAt)
				}
				if got.FinishedAt == nil {
					t.Error("finished_at = null, want a timestamp on a canceled job")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.setup(t)
			got := decodeJobDetail(t, getJobDetail(t, srv, "job1"))

			if got.ID != "job1" || got.Title != "Album" || got.Sub != "flac → opus/Balanced" || got.Dir != "/dir1" {
				t.Errorf("identity fields = %+v, want id/title/sub/dir of the started job", got)
			}
			if got.TotalFiles != 2 {
				t.Errorf("total_files = %d, want 2", got.TotalFiles)
			}
			if !reflect.DeepEqual(got.Files, []string{"01.flac", "02.flac"}) {
				t.Errorf("files = %v, want the job's file names", got.Files)
			}
			if got.Skipped == nil {
				t.Error("skipped = null, want an empty array")
			}
			if got.CreatedAt.IsZero() {
				t.Error("created_at is zero, want the registration timestamp")
			}
			if got.Evicted {
				t.Error("evicted = true, want false for a listed job")
			}
			tc.check(t, got)
		})
	}
}

func TestHandleJob_NotFound(t *testing.T) {
	srv := &Server{jobs: newJobManager(nil, 0, context.Background())}

	assertAPIError(t, getJobDetail(t, srv, "nope"), http.StatusNotFound, codeJobNotFound)
}

func TestHandleJob_MissingID(t *testing.T) {
	srv := &Server{jobs: newJobManager(nil, 0, context.Background())}

	req := httptest.NewRequest(http.MethodGet, "/api/jobs/", nil)
	w := httptest.NewRecorder()
	srv.handleJob(w, req)

	assertAPIError(t, w, http.StatusBadRequest, codeInvalidRequest)
}

// TestHandleJob_ReportsSelectionAndOutputDir drives a real skip_lossy request
// through the handler and asserts the detail endpoint reproduces the resolved
// selection and destination, so an agent can confirm what a job will produce
// without having kept the 202 body.
func TestHandleJob_ReportsSelectionAndOutputDir(t *testing.T) {
	srv, dir := newTestServerWithDirTracks(t, &mockEngine{}, mixedTracks)

	w := postTranscode(t, srv, map[string]any{
		"path": dir, "preset": "Balanced", "skip_lossy": true, "copy_skipped": true,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202: %s", w.Code, w.Body.String())
	}
	var accepted transcodeAcceptedDTO
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode 202 body: %v", err)
	}

	got := decodeJobDetail(t, getJobDetail(t, srv, accepted.JobID))

	if !reflect.DeepEqual(got.Files, []string{"01.flac", "03.flac"}) {
		t.Errorf("files = %v, want the lossless selection", got.Files)
	}
	wantSkipped := []skippedDTO{{Name: "02.mp3", Codec: "mp3", Reason: skipReasonLossy}}
	if !reflect.DeepEqual(got.Skipped, wantSkipped) {
		t.Errorf("skipped = %+v, want %+v", got.Skipped, wantSkipped)
	}
	wantOut := filepath.Join(srv.cfg.OutputDir, "TestLib", "album1")
	if got.OutputDir != wantOut {
		t.Errorf("output_dir = %q, want %q", got.OutputDir, wantOut)
	}
}

// TestHandleJob_OutputDirForReplaceMode pins the one mode whose destination is
// not under OutputDir: replace rewrites the sources in place.
func TestHandleJob_OutputDirForReplaceMode(t *testing.T) {
	srv, dir := newTestServerWithDirTracks(t, &mockEngine{}, flacTracks)

	w := postTranscode(t, srv, map[string]any{
		"path": dir, "preset": "Balanced", "output_mode": "replace",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202: %s", w.Code, w.Body.String())
	}
	var accepted transcodeAcceptedDTO
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode 202 body: %v", err)
	}

	got := decodeJobDetail(t, getJobDetail(t, srv, accepted.JobID))

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got.OutputDir != resolved {
		t.Errorf("output_dir = %q, want the source directory %q", got.OutputDir, resolved)
	}
}

// TestJobEventPayloadUnchanged pins the queue-panel wire format: the detail
// fields added for GET /api/jobs/{id} must not leak into GET /api/jobs or the
// SSE stream, which broadcast one event per ffmpeg progress line to every tab.
func TestJobEventPayloadUnchanged(t *testing.T) {
	jm := newJobManager(nil, 0, context.Background())
	srv := &Server{jobs: jm}

	meta := jobMeta{
		title:     "Album One",
		sub:       "flac → opus/Balanced",
		outputDir: "/out/TestLib/album1",
		skipped:   []skippedDTO{{Name: "02.mp3", Codec: "mp3", Reason: skipReasonLossy}},
	}
	if _, ok := jm.start("job1", "/dir1", twoFileJob("job1"), meta); !ok {
		t.Fatalf("start: expected success")
	}

	const wantEvent = `{"id":"job1","status":"queued","pct":0,"title":"Album One","sub":"flac → opus/Balanced","dir":"/dir1"}`

	req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	w := httptest.NewRecorder()
	srv.handleJobs(w, req)
	if got := strings.TrimSpace(w.Body.String()); got != `{"jobs":[`+wantEvent+`]}` {
		t.Errorf("GET /api/jobs body =\n%s\nwant\n%s", got, `{"jobs":[`+wantEvent+`]}`)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sseReq := httptest.NewRequest(http.MethodGet, "/api/jobs/events", nil).WithContext(ctx)
	sseW := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleJobEvents(sseW, sseReq)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SSE handler did not return after context cancel")
	}

	if got := sseW.Body.String(); got != "event: update\ndata: "+wantEvent+"\n\n" {
		t.Errorf("SSE snapshot =\n%q\nwant\n%q", got, "event: update\ndata: "+wantEvent+"\n\n")
	}
}

// TestJobRoutes_DetailDoesNotShadowEvents pins the routing precedence between
// the new GET /api/jobs/{id} pattern and the SSE stream at the literal
// /api/jobs/events, which must keep winning.
func TestJobRoutes_DetailDoesNotShadowEvents(t *testing.T) {
	srv, dir := newTestServerWithDirTracks(t, &mockEngine{}, flacTracks)

	w := postTranscode(t, srv, map[string]any{"path": dir, "preset": "Balanced"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("start status = %d, want 202: %s", w.Code, w.Body.String())
	}
	var accepted transcodeAcceptedDTO
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatalf("decode 202 body: %v", err)
	}

	detailW := httptest.NewRecorder()
	srv.ServeHTTP(detailW, httptest.NewRequest(http.MethodGet, "/api/jobs/"+accepted.JobID, nil))
	if got := decodeJobDetail(t, detailW).ID; got != accepted.JobID {
		t.Errorf("detail id = %q, want %q", got, accepted.JobID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	eventsW := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(eventsW, httptest.NewRequest(http.MethodGet, "/api/jobs/events", nil).WithContext(ctx))
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("SSE handler did not return after context cancel")
	}
	if got := eventsW.Body.String(); !strings.HasPrefix(got, "event: update\ndata: ") {
		t.Errorf("/api/jobs/events body = %q, want the SSE stream, not a job detail payload", got)
	}
}
