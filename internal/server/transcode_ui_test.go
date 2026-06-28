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

// --- Transcode button / panel rendering ---

// TestTranscodeForm_RenderedWithTracks verifies that a directory page
// with indexed tracks includes the Transcode button and all key panel elements.
func TestTranscodeForm_RenderedWithTracks(t *testing.T) {
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

	// Transcode button
	if !strings.Contains(body, "transcode-btn") {
		t.Error("expect transcode-btn element for directory with tracks")
	}
	// Transcode panel
	if !strings.Contains(body, "transcode-panel") {
		t.Error("expect transcode-panel element for directory with tracks")
	}
	// Codec selectors
	if !strings.Contains(body, "tc_codec_flac") {
		t.Error("expect FLAC codec radio button (id=tc_codec_flac)")
	}
	if !strings.Contains(body, "tc_codec_opus") {
		t.Error("expect Opus codec radio button (id=tc_codec_opus)")
	}
	// Preset selector
	if !strings.Contains(body, "tc_preset") {
		t.Error("expect preset selector (id=tc_preset)")
	}
	// Output mode radios
	if !strings.Contains(body, "tc_mode_shared") {
		t.Error("expect shared output mode radio (id=tc_mode_shared)")
	}
	if !strings.Contains(body, "tc_mode_local") {
		t.Error("expect local output mode radio (id=tc_mode_local)")
	}
	if !strings.Contains(body, "tc_mode_replace") {
		t.Error("expect replace output mode radio (id=tc_mode_replace)")
	}
	// Replace warning element (initially hidden)
	if !strings.Contains(body, "tc_replace_warning") {
		t.Error("expect tc_replace_warning element")
	}
	// Start button
	if !strings.Contains(body, "tc_start_btn") {
		t.Error("expect tc_start_btn element")
	}
	// Progress area (initially hidden)
	if !strings.Contains(body, "tc_progress_area") {
		t.Error("expect tc_progress_area element")
	}
	if !strings.Contains(body, "tc_progress_fill") {
		t.Error("expect tc_progress_fill progress bar element")
	}
	// Log viewer
	if !strings.Contains(body, "tc_log_body") {
		t.Error("expect tc_log_body log element")
	}
	// Result element
	if !strings.Contains(body, "tc_result") {
		t.Error("expect tc_result element")
	}
	// Custom params section
	if !strings.Contains(body, "tc_custom_params") {
		t.Error("expect tc_custom_params element")
	}
	if !strings.Contains(body, "tc_custom_flac") {
		t.Error("expect FLAC-specific custom params section")
	}
	if !strings.Contains(body, "tc_custom_opus") {
		t.Error("expect Opus-specific custom params section")
	}
	if !strings.Contains(body, "tc_file_counter") {
		t.Error("expect transcode progress track counter element")
	}
	if !strings.Contains(body, "library-select") || !strings.Contains(body, "tree-root") {
		t.Error("expect direct /dir page to render the full app shell")
	}
}

// TestTranscodeForm_NotRenderedWithoutTracks verifies that a directory page
// without indexed tracks does NOT render the Transcode button or panel.
func TestTranscodeForm_NotRenderedWithoutTracks(t *testing.T) {
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

	// The JS block always references 'transcode-btn' as a string, so check for the
	// HTML attribute form to distinguish element presence from JS references.
	if strings.Contains(body, `id="transcode-btn"`) {
		t.Error("transcode-btn element must not be rendered when directory has no tracks")
	}
	if strings.Contains(body, `id="transcode-panel"`) {
		t.Error("transcode-panel element must not be rendered when directory has no tracks")
	}
}

func TestTranscodeForm_NotRenderedForLossyTracks(t *testing.T) {
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

	if strings.Contains(body, `id="transcode-btn"`) {
		t.Error("transcode-btn element must not be rendered for lossy directories")
	}
	if strings.Contains(body, `id="transcode-panel"`) {
		t.Error("transcode-panel element must not be rendered for lossy directories")
	}
}

// TestTranscodeForm_DataPath verifies the panel keeps the raw absolute path in
// data-path so JS can send it without an extra decode step.
func TestTranscodeForm_DataPath(t *testing.T) {
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
	// The panel should have data-path set (URL-encoded path contains the path)
	body := w.Body.String()
	if !strings.Contains(body, "data-path=") {
		t.Error("transcode-panel must have data-path attribute")
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if !strings.Contains(body, `data-path="`+resolvedPath+`"`) {
		t.Fatalf("transcode-panel should keep raw absolute path, got:\n%s", body)
	}
	if strings.Contains(body, "%252F") {
		t.Fatalf("directory page must not double-encode paths, got:\n%s", body)
	}
}

// TestTranscodeForm_CustomParams verifies the custom params section is present.
func TestTranscodeForm_CustomParams(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Classical")
	mkdirAll(t, absPath)
	dirID, err := database.UpsertDirectory(libID, "Classical", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	database.UpsertTrack(db.Track{ //nolint:errcheck
		DirectoryID: dirID, Filename: "symphony.flac", Codec: "flac",
		Bitrate: 1_000_000, Duration: 3600.0, SampleRate: 96000, Channels: 2, Size: 450_000_000,
	})

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "tc_bitrate") {
		t.Error("expect tc_bitrate input in custom params")
	}
	if !strings.Contains(body, "tc_compression") {
		t.Error("expect tc_compression input in custom params")
	}
	if !strings.Contains(body, "tc_copy_meta") {
		t.Error("expect tc_copy_meta checkbox in custom params")
	}
	if !strings.Contains(body, "tc_copy_cover") {
		t.Error("expect tc_copy_cover checkbox in custom params")
	}
}

// TestTranscodeForm_OutputModeLabels verifies the output mode option labels are rendered.
func TestTranscodeForm_OutputModeLabels(t *testing.T) {
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
