package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/semsemyonoff/ALTO/internal/db"
)

// realTemplateDir returns the path to the project's web/templates directory,
// computed relative to this test file's location.
func realTemplateDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// filename is .../internal/server/transcode_ui_test.go
	// web/templates is two levels up: ../../web/templates
	return filepath.Join(filepath.Dir(filename), "..", "..", "web", "templates")
}

func realStaticDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(filename), "..", "..", "web", "static")
}

// newTestServerWithRealTemplates creates a Server backed by an in-memory DB
// and the project's actual web/templates directory.
func newTestServerWithRealTemplates(t *testing.T) (*Server, *db.DB, string) {
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

	cfg := Config{
		Libraries:   []LibraryConfig{{ID: libID, Name: "TestLib", Path: libDir}},
		OutputDir:   t.TempDir(),
		TemplateDir: realTemplateDir(t),
		StaticDir:   realStaticDir(t),
	}
	srv := New(database, &mockScanner{}, cfg)
	return srv, database, libDir
}

// --- Transcode dock rendering ---

// TestTranscodeDock_RenderedWithTracks verifies that a directory page with
// indexed lossless tracks includes the dock and its key controls.
func TestTranscodeDock_RenderedWithTracks(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Jazz")
	mkdirAll(t, absPath)

	dirID, err := database.UpsertDirectory(libID, "Jazz", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	if err := database.UpsertTrack(db.Track{
		DirectoryID: dirID,
		Filename:    "01_so_what.flac",
		Codec:       "flac",
		Bitrate:     900_000,
		Duration:    565.0,
		SampleRate:  44100,
		Channels:    2,
		Size:        63_504_000,
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, `id="tc-dock"`) {
		t.Error("expect dock element (id=tc-dock) for directory with tracks")
	}
	if !strings.Contains(body, `x-data="altoDock()"`) {
		t.Error("expect the dock to be wired up as the altoDock Alpine island")
	}
	if !strings.Contains(body, `data-can-transcode="true"`) {
		t.Error("expect data-can-transcode=true for a lossless directory")
	}
	if !strings.Contains(body, `data-track-count="1"`) {
		t.Error("expect data-track-count to reflect the track count")
	}
	if !strings.Contains(body, `id="tc_preset_btn"`) {
		t.Error("expect preset dropdown button (id=tc_preset_btn)")
	}
	if !strings.Contains(body, `id="tc_mode_shared"`) {
		t.Error("expect shared output mode control (id=tc_mode_shared)")
	}
	if !strings.Contains(body, `id="tc_mode_local"`) {
		t.Error("expect local output mode control (id=tc_mode_local)")
	}
	if !strings.Contains(body, `id="tc_mode_replace"`) {
		t.Error("expect replace output mode control (id=tc_mode_replace)")
	}
	if !strings.Contains(body, `id="tc_start_btn"`) {
		t.Error("expect tc_start_btn element")
	}
	if !strings.Contains(body, `id="tc-presets-data"`) {
		t.Error("expect the presets JSON script tag (id=tc-presets-data)")
	}
	if !strings.Contains(body, `"flac"`) || !strings.Contains(body, `"opus"`) {
		t.Error("expect the presets JSON to include both flac and opus groups")
	}
	if !strings.Contains(body, "libwrap") || !strings.Contains(body, "tree-root") {
		t.Error("expect direct /dir page to render the full app shell")
	}
}

// TestTranscodeDock_NotRenderedWithoutTracks verifies that a directory page
// without indexed tracks does NOT render the dock.
func TestTranscodeDock_NotRenderedWithoutTracks(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "EmptyDir")
	mkdirAll(t, absPath)
	database.UpsertDirectoryWithAudioFlag(libID, "EmptyDir", "", false, "", true) //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if strings.Contains(body, `id="tc-dock"`) {
		t.Error("dock must not be rendered when directory has no tracks")
	}
}

// TestTranscodeDock_DisabledForLossyTracks verifies that a directory with only
// lossy tracks still renders the dock, but flagged not-transcodable so START
// disables itself with a reason.
func TestTranscodeDock_DisabledForLossyTracks(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "LossyDir")
	mkdirAll(t, absPath)
	dirID, err := database.UpsertDirectory(libID, "LossyDir", "MP3", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	if err := database.UpsertTrack(db.Track{
		DirectoryID: dirID,
		Filename:    "track.mp3",
		Codec:       "mp3",
		Bitrate:     320_000,
		Duration:    180.0,
		SampleRate:  44100,
		Channels:    2,
		Size:        7_200_000,
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, `id="tc-dock"`) {
		t.Error("dock must still render for lossy directories, disabled via data-can-transcode")
	}
	if !strings.Contains(body, `data-can-transcode="false"`) {
		t.Error("expect data-can-transcode=false for a lossy directory")
	}
}

// TestTranscodeDock_DataPath verifies the dock keeps the raw absolute path in
// data-path so the Alpine component can send it without an extra decode step.
func TestTranscodeDock_DataPath(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "My Album")
	mkdirAll(t, absPath)

	dirID, err := database.UpsertDirectory(libID, "My Album", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	database.UpsertTrack(db.Track{ //nolint:errcheck
		DirectoryID: dirID, Filename: "track.flac", Codec: "flac",
		Bitrate: 900_000, Duration: 200.0, SampleRate: 44100, Channels: 2, Size: 22_500_000,
	})

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "data-path=") {
		t.Error("dock must have data-path attribute")
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if !strings.Contains(body, `data-path="`+resolvedPath+`"`) {
		t.Fatalf("dock should keep raw absolute path, got:\n%s", body)
	}
	if strings.Contains(body, "%252F") {
		t.Fatalf("directory page must not double-encode paths, got:\n%s", body)
	}
}

// TestTranscodeDock_OutputModeLabels verifies the output mode labels are rendered.
func TestTranscodeDock_OutputModeLabels(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Archive")
	mkdirAll(t, absPath)
	dirID, err := database.UpsertDirectory(libID, "Archive", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	database.UpsertTrack(db.Track{ //nolint:errcheck
		DirectoryID: dirID, Filename: "song.flac", Codec: "flac",
		Bitrate: 900_000, Duration: 240.0, SampleRate: 48000, Channels: 2, Size: 24_800_000,
	})

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "Shared /out") {
		t.Error("expect 'Shared /out' output mode label")
	}
	if !strings.Contains(body, "alto-out/") {
		t.Error("expect 'alto-out/' in local output mode label")
	}
	if !strings.Contains(body, "Replace") {
		t.Error("expect 'Replace' output mode label")
	}
}
