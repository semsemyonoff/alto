package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/semsemyonoff/ALTO/internal/db"
)

// minimalDirTemplates writes base.html, index.html, and directory.html to a
// temp directory and returns its path. The templates are minimal but structurally
// faithful (directory.html has the #dir-content wrapper that HTMX selects).
func minimalDirTemplates(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	writeTemplateFile(t, dir, "base.html",
		`{{define "base"}}<!DOCTYPE html><html><body>{{template "sidebar" .}}{{template "content" .}}</body></html>{{end}}`)
	writeTemplateFile(t, dir, "index.html",
		`{{define "sidebar"}}<nav id="tree-root">{{.TopDirsHTML}}</nav>{{end}}`+
			`{{define "content"}}<main>Select a directory</main>{{end}}`+
			`{{define "index.html"}}{{template "base" .}}{{end}}`)
	writeTemplateFile(t, dir, "directory.html",
		`{{define "directory.html"}}<!DOCTYPE html><html><body>`+
			`<div id="dir-content" class="dir-page">`+
			`<h1 class="dir-title">{{.DirName}}</h1>`+
			`<span class="dir-breadcrumb">{{.LibraryName}}</span>`+
			`{{if .HasCover}}<img class="dir-cover" src="/api/cover?path={{.PathEncoded}}" alt="Cover art">{{end}}`+
			`{{if .CodecSummary}}<span class="codec-badge {{.CodecClass}}">{{.CodecSummary}}</span>{{end}}`+
			`<span class="dir-stats">{{.TrackCount}} tracks</span>`+
			`{{range .Tracks}}`+
			`<tr><td class="track-filename">{{.Filename}}</td>`+
			`<td class="track-codec">{{.Codec}}</td>`+
			`<td class="track-bitrate">{{.Bitrate}}</td>`+
			`<td class="track-duration">{{.Duration}}</td>`+
			`<td class="track-samplerate">{{.SampleRate}}</td>`+
			`<td class="track-channels">{{.Channels}}</td>`+
			`<td class="track-size">{{.Size}}</td></tr>`+
			`{{end}}`+
			`</div></body></html>{{end}}`)

	return dir
}

// newTestServerWithDirTemplate creates a server with templates that include directory.html.
func newTestServerWithDirTemplate(t *testing.T) (*Server, *db.DB, string) {
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

	tmplDir := minimalDirTemplates(t)
	cfg := Config{
		Libraries:   []LibraryConfig{{ID: libID, Name: "TestLib", Path: libDir}},
		OutputDir:   t.TempDir(),
		TemplateDir: tmplDir,
	}
	srv := New(database, &mockScanner{}, cfg)
	return srv, database, libDir
}

// --- GET /dir ---

func TestHandleDirPage_NoPath(t *testing.T) {
	srv, _, _ := newTestServerWithDirTemplate(t)

	req := httptest.NewRequest(http.MethodGet, "/dir", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestHandleDirPage_OutsideRoot(t *testing.T) {
	srv, _, _ := newTestServerWithDirTemplate(t)

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": "/etc"}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestHandleDirPage_AltoSegmentRejected(t *testing.T) {
	srv, _, libDir := newTestServerWithDirTemplate(t)

	altoDir := filepath.Join(libDir, ".alto-out", "album")
	mkdirAll(t, altoDir)

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": altoDir}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleDirPage_NotInDB(t *testing.T) {
	srv, _, libDir := newTestServerWithDirTemplate(t)

	absPath := filepath.Join(libDir, "UnknownAlbum")
	mkdirAll(t, absPath)

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/?notice=directory_not_found" {
		t.Fatalf("want redirect to /?notice=directory_not_found, got %q", got)
	}
}

func TestHandleDirPage_NoTracks(t *testing.T) {
	srv, database, libDir := newTestServerWithDirTemplate(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "EmptyAlbum")
	mkdirAll(t, absPath)
	database.UpsertDirectory(libID, "EmptyAlbum", "FLAC", false, "") //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "dir-content") {
		t.Errorf("response should contain #dir-content; got:\n%s", body)
	}
	if !strings.Contains(body, "EmptyAlbum") {
		t.Errorf("response should contain directory name; got:\n%s", body)
	}
	if !strings.Contains(body, "0 tracks") {
		t.Errorf("response should show 0 tracks; got:\n%s", body)
	}
}

func TestHandleDirPage_NonAudioDirectoryRejected(t *testing.T) {
	srv, database, libDir := newTestServerWithDirTemplate(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Artists")
	mkdirAll(t, absPath)
	if _, err := database.UpsertDirectoryWithAudioFlag(libID, "Artists", "", false, "", false); err != nil {
		t.Fatalf("UpsertDirectoryWithAudioFlag: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Location"); got != "/?notice=directory_not_found" {
		t.Fatalf("want redirect to /?notice=directory_not_found, got %q", got)
	}
}

func TestHandleDirPage_WithTracks(t *testing.T) {
	srv, database, libDir := newTestServerWithDirTemplate(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Jazz")
	mkdirAll(t, absPath)
	dirID, err := database.UpsertDirectory(libID, "Jazz", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}

	tracks := []db.Track{
		{DirectoryID: dirID, Filename: "01_so_what.flac", Codec: "flac", Bitrate: 900000, Duration: 565.0, SampleRate: 44100, Channels: 2, Size: 63504000},
		{DirectoryID: dirID, Filename: "02_freddie_freeloader.flac", Codec: "flac", Bitrate: 950000, Duration: 590.0, SampleRate: 44100, Channels: 2, Size: 70125000},
	}
	for _, tr := range tracks {
		if err := database.UpsertTrack(tr); err != nil {
			t.Fatalf("UpsertTrack: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "dir-content") {
		t.Errorf("response should contain #dir-content wrapper; got:\n%s", body)
	}
	if !strings.Contains(body, "Jazz") {
		t.Errorf("response should contain directory name 'Jazz'; got:\n%s", body)
	}
	if !strings.Contains(body, "TestLib") {
		t.Errorf("response should contain library name 'TestLib'; got:\n%s", body)
	}
	if !strings.Contains(body, "01_so_what.flac") {
		t.Errorf("response should contain first track filename; got:\n%s", body)
	}
	if !strings.Contains(body, "02_freddie_freeloader.flac") {
		t.Errorf("response should contain second track filename; got:\n%s", body)
	}
	if !strings.Contains(body, "2 tracks") {
		t.Errorf("response should show track count; got:\n%s", body)
	}
}

func TestHandleDirPage_WithCover(t *testing.T) {
	srv, database, libDir := newTestServerWithDirTemplate(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Classical")
	mkdirAll(t, absPath)

	coverFile := filepath.Join(absPath, "cover.jpg")
	if err := os.WriteFile(coverFile, []byte("\xFF\xD8\xFF"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	database.UpsertDirectory(libID, "Classical", "FLAC", true, coverFile) //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "dir-cover") {
		t.Errorf("response should contain cover art element; got:\n%s", body)
	}
	if !strings.Contains(body, "/api/cover") {
		t.Errorf("response should reference /api/cover endpoint; got:\n%s", body)
	}
}

func TestBuildDirPageData_PathKeptRawForTemplateEncoding(t *testing.T) {
	resolvedPath := "/music/My Album"
	data := buildDirPageData(
		LibraryConfig{ID: 1, Name: "Music", Path: "/music"},
		&db.Directory{ID: 10, LibraryID: 1, Path: "My Album", HasCover: true, CodecSummary: "FLAC"},
		nil,
		resolvedPath,
	)

	if data.Path != resolvedPath {
		t.Fatalf("Path want %q, got %q", resolvedPath, data.Path)
	}
}

func TestHandleDirPage_CodecBadge(t *testing.T) {
	srv, database, libDir := newTestServerWithDirTemplate(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Lossless")
	mkdirAll(t, absPath)
	database.UpsertDirectory(libID, "Lossless", "FLAC", false, "") //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "FLAC") {
		t.Errorf("response should contain codec summary; got:\n%s", body)
	}
	if !strings.Contains(body, "codec-flac") {
		t.Errorf("response should contain codec CSS class; got:\n%s", body)
	}
}

func TestHandleDirPage_MixedCodecs(t *testing.T) {
	srv, database, libDir := newTestServerWithDirTemplate(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "MixedLibrary")
	mkdirAll(t, absPath)
	dirID, err := database.UpsertDirectory(libID, "MixedLibrary", "Mixed", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}

	_ = database.UpsertTrack(db.Track{DirectoryID: dirID, Filename: "track1.flac", Codec: "flac", Bitrate: 900000, Duration: 200.0, SampleRate: 44100, Channels: 2, Size: 22500000})
	_ = database.UpsertTrack(db.Track{DirectoryID: dirID, Filename: "track2.mp3", Codec: "mp3", Bitrate: 320000, Duration: 200.0, SampleRate: 44100, Channels: 2, Size: 8000000})

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "track1.flac") || !strings.Contains(body, "track2.mp3") {
		t.Errorf("response should contain both track filenames; got:\n%s", body)
	}
	if !strings.Contains(body, "Mixed") {
		t.Errorf("response should contain 'Mixed' codec summary; got:\n%s", body)
	}
}

// --- Task 13: directory.html unified onto the base.html shell ---

// TestHandleDirPage_RealTemplates_RendersUnifiedShell verifies that /dir,
// rendered against the project's real templates, produces a single full page
// (base's head/topbar/sidebar) rather than directory.html's old standalone
// <html><head><body> document, and that the shared sidebar partial and
// #dir-content both appear exactly once.
func TestHandleDirPage_RealTemplates_RendersUnifiedShell(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Jazz")
	mkdirAll(t, absPath)
	database.UpsertDirectory(libID, "Jazz", "FLAC", false, "") //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if got := strings.Count(body, "<!DOCTYPE html>"); got != 1 {
		t.Errorf("want exactly one <!DOCTYPE html> (unified shell, not a nested standalone document); got %d in:\n%s", got, body)
	}
	if got := strings.Count(body, `id="dir-content"`); got != 1 {
		t.Errorf("want exactly one #dir-content; got %d", got)
	}
	if !strings.Contains(body, `class="topbar"`) {
		t.Error("directory page should render the shared topbar from base.html")
	}
	if !strings.Contains(body, "scan-btn") {
		t.Error("directory page should render the shared Re-index control from base.html")
	}
	if !strings.Contains(body, `class="libwrap"`) || !strings.Contains(body, `id="tree-root"`) {
		t.Error("directory page should render the shared topbar library selector + sidebar tree partial")
	}
}

// TestHandleDirPage_RealTemplates_HTMXSelectRegionIntact verifies that the
// HTMX contract used by tree-node clicks (hx-select="#dir-content") still
// resolves against the full /dir response: the selected fragment must
// contain the directory's tracks and title without any of the surrounding
// shell markup being required.
func TestHandleDirPage_RealTemplates_HTMXSelectRegionIntact(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Jazz")
	mkdirAll(t, absPath)
	dirID, err := database.UpsertDirectory(libID, "Jazz", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	if err := database.UpsertTrack(db.Track{
		DirectoryID: dirID, Filename: "01_so_what.flac", Codec: "flac",
		Bitrate: 900_000, Duration: 565.0, SampleRate: 44100, Channels: 2, Size: 63_504_000,
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

	start := strings.Index(body, `id="dir-content"`)
	mainEnd := strings.Index(body, "</main>")
	if start == -1 || mainEnd == -1 || mainEnd < start {
		t.Fatalf("could not locate the #dir-content region within <main> in body:\n%s", body)
	}
	fragment := body[start:mainEnd]

	if !strings.Contains(fragment, "Jazz") {
		t.Error("hx-select=#dir-content fragment should contain the directory name")
	}
	if !strings.Contains(fragment, "01_so_what.flac") {
		t.Error("hx-select=#dir-content fragment should contain track rows")
	}
}

// TestHandleDirPage_RealTemplates_NoCollisionWithIndex verifies that loading
// directory.html's isolated template group for /dir doesn't clobber the
// cached index.html group, and vice versa, using the real templates.
func TestHandleDirPage_RealTemplates_NoCollisionWithIndex(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Jazz")
	mkdirAll(t, absPath)
	dirID, err := database.UpsertDirectory(libID, "Jazz", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	// Give the library indexed content so "/" renders its normal placeholder
	// rather than the standby (not-indexed) state — this test is about
	// template-group isolation, not the standby screen.
	if err := database.UpsertTrack(db.Track{DirectoryID: dirID, Filename: "01.flac", Codec: "flac"}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	// Render "/" first to warm the index.html template group cache.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for '/', got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Select a directory from the tree") {
		t.Errorf("index page should render its own placeholder content; got:\n%s", w.Body.String())
	}

	// Now render /dir, which loads (and caches) its own isolated group.
	req = httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for /dir, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `id="dir-content"`) {
		t.Errorf("directory page should render its own content block; got:\n%s", w.Body.String())
	}

	// Re-render "/" to ensure the index.html group is unaffected.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for '/' after /dir, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Select a directory from the tree") {
		t.Errorf("index page content should be unaffected by rendering /dir; got:\n%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `id="dir-content"`) {
		t.Errorf("index page must not leak directory.html's content block; got:\n%s", w.Body.String())
	}
}

// --- Task 16: stage rebuild — aggregates, codec pill, tech toggle ---

// TestHandleDirPage_RealTemplates_StageAggregatesAndAlwaysShowsTech verifies the
// rebuilt stage renders the total duration/size subline, a large codec pill, and
// the technical columns unconditionally (the show-technical toggle was removed).
func TestHandleDirPage_RealTemplates_StageAggregatesAndAlwaysShowsTech(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Jazz")
	mkdirAll(t, absPath)
	dirID, err := database.UpsertDirectory(libID, "Jazz", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	if err := database.UpsertTrack(db.Track{
		DirectoryID: dirID, Filename: "01_so_what.flac", Codec: "flac",
		Bitrate: 900_000, Duration: 565.0, SampleRate: 44100, Channels: 2, Size: 63_504_000,
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

	if !strings.Contains(body, "codec-badge-lg") {
		t.Errorf("stage header should render a large codec pill; got:\n%s", body)
	}
	if !strings.Contains(body, fmtDuration(565.0)) {
		t.Errorf("stage subline should show the total duration; got:\n%s", body)
	}
	if !strings.Contains(body, fmtSize(63_504_000)) {
		t.Errorf("stage subline should show the total size; got:\n%s", body)
	}
	// The show-technical toggle was removed: no Alpine toggle island, no
	// hide-tech binding — the technical columns are always visible.
	if strings.Contains(body, "showTech") || strings.Contains(body, "tech-toggle") || strings.Contains(body, "hide-tech") {
		t.Errorf("show-technical toggle should be gone; got:\n%s", body)
	}
	// The technical columns themselves are always rendered.
	for _, header := range []string{"Bitrate", "Sample Rate", "Ch"} {
		if !strings.Contains(body, ">"+header+"<") {
			t.Errorf("technical column %q should always be shown; got:\n%s", header, body)
		}
	}
}

func TestHandleDirPage_ContentTypeHTML(t *testing.T) {
	srv, database, libDir := newTestServerWithDirTemplate(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Album")
	mkdirAll(t, absPath)
	database.UpsertDirectory(libID, "Album", "FLAC", false, "") //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("want text/html Content-Type, got %q", ct)
	}
}

// --- Formatting helpers ---

func TestFmtBitrate(t *testing.T) {
	cases := []struct {
		bps  int64
		want string
	}{
		{0, "–"},
		{-1, "–"},
		{1000000, "1000 kbps"},
		{320000, "320 kbps"},
		{128000, "128 kbps"},
		{500, "500 bps"},
	}
	for _, tc := range cases {
		got := fmtBitrate(tc.bps)
		if got != tc.want {
			t.Errorf("fmtBitrate(%d) = %q, want %q", tc.bps, got, tc.want)
		}
	}
}

func TestFmtDuration(t *testing.T) {
	cases := []struct {
		secs float64
		want string
	}{
		{0, "–"},
		{-1, "–"},
		{60, "1:00"},
		{65, "1:05"},
		{3661, "1:01:01"},
		{337.5, "5:38"},
		{3600, "1:00:00"},
	}
	for _, tc := range cases {
		got := fmtDuration(tc.secs)
		if got != tc.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}

func TestFmtSampleRate(t *testing.T) {
	cases := []struct {
		hz   int64
		want string
	}{
		{0, "–"},
		{44100, "44.1 kHz"},
		{48000, "48 kHz"},
		{96000, "96 kHz"},
		{192000, "192 kHz"},
		{22050, "22.1 kHz"},
	}
	for _, tc := range cases {
		got := fmtSampleRate(tc.hz)
		if got != tc.want {
			t.Errorf("fmtSampleRate(%d) = %q, want %q", tc.hz, got, tc.want)
		}
	}
}

func TestFmtSize(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "–"},
		{-1, "–"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{26214400, "25.0 MB"},
		{1073741824, "1.0 GB"},
	}
	for _, tc := range cases {
		got := fmtSize(tc.bytes)
		if got != tc.want {
			t.Errorf("fmtSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

// --- buildDirPageData ---

func TestBuildDirPageData(t *testing.T) {
	lib := LibraryConfig{ID: 1, Name: "Music", Path: "/music"}
	dir := &db.Directory{
		ID:           5,
		LibraryID:    1,
		Path:         "Jazz/Kind of Blue",
		HasCover:     true,
		CodecSummary: "FLAC",
	}
	tracks := []db.Track{
		{DirectoryID: 5, Filename: "01_so_what.flac", Codec: "flac", Bitrate: 900000, Duration: 565.0, SampleRate: 44100, Channels: 2, Size: 63504000},
	}
	resolvedPath := "/music/Jazz/Kind of Blue"

	data := buildDirPageData(lib, dir, tracks, resolvedPath)

	if data.DirName != "Kind of Blue" {
		t.Errorf("DirName want 'Kind of Blue', got %q", data.DirName)
	}
	if data.LibraryName != "Music" {
		t.Errorf("LibraryName want 'Music', got %q", data.LibraryName)
	}
	if !data.HasCover {
		t.Error("HasCover should be true")
	}
	if data.CodecSummary != "FLAC" {
		t.Errorf("CodecSummary want 'FLAC', got %q", data.CodecSummary)
	}
	if data.CodecClass != "codec-flac" {
		t.Errorf("CodecClass want 'codec-flac', got %q", data.CodecClass)
	}
	if !data.CanTranscode {
		t.Error("CanTranscode should be true for lossless tracks")
	}
	if data.TrackCount != 1 {
		t.Errorf("TrackCount want 1, got %d", data.TrackCount)
	}
	if len(data.Tracks) != 1 {
		t.Fatalf("want 1 track row, got %d", len(data.Tracks))
	}
	row := data.Tracks[0]
	if row.Index != 1 {
		t.Errorf("track Index want 1, got %d", row.Index)
	}
	if row.Filename != "01_so_what.flac" {
		t.Errorf("track Filename want '01_so_what.flac', got %q", row.Filename)
	}
	if row.Bitrate != "900 kbps" {
		t.Errorf("track Bitrate want '900 kbps', got %q", row.Bitrate)
	}
	if row.SampleRate != "44.1 kHz" {
		t.Errorf("track SampleRate want '44.1 kHz', got %q", row.SampleRate)
	}
	if data.PathEncoded == "" {
		t.Error("PathEncoded should not be empty")
	}
}

func TestBuildDirPageData_Aggregates(t *testing.T) {
	lib := LibraryConfig{ID: 1, Name: "Music", Path: "/music"}
	dir := &db.Directory{ID: 5, LibraryID: 1, Path: "Jazz/Kind of Blue", CodecSummary: "FLAC"}
	tracks := []db.Track{
		{DirectoryID: 5, Filename: "01_so_what.flac", Codec: "flac", Duration: 565.0, Size: 63504000},
		{DirectoryID: 5, Filename: "02_freddie_freeloader.flac", Codec: "flac", Duration: 590.0, Size: 70125000},
	}

	data := buildDirPageData(lib, dir, tracks, "/music/Jazz/Kind of Blue")

	if want := fmtDuration(565.0 + 590.0); data.TotalDuration != want {
		t.Errorf("TotalDuration want %q, got %q", want, data.TotalDuration)
	}
	if want := fmtSize(63504000 + 70125000); data.TotalSize != want {
		t.Errorf("TotalSize want %q, got %q", want, data.TotalSize)
	}
}

func TestBuildDirPageData_NoTracksTotalsAreDash(t *testing.T) {
	lib := LibraryConfig{ID: 1, Name: "Music", Path: "/music"}
	dir := &db.Directory{ID: 5, LibraryID: 1, Path: "Empty"}

	data := buildDirPageData(lib, dir, nil, "/music/Empty")

	if data.TotalDuration != "–" {
		t.Errorf("TotalDuration want %q, got %q", "–", data.TotalDuration)
	}
	if data.TotalSize != "–" {
		t.Errorf("TotalSize want %q, got %q", "–", data.TotalSize)
	}
}

func TestBuildDirPageData_LossyTracksCannotTranscode(t *testing.T) {
	data := buildDirPageData(
		LibraryConfig{ID: 1, Name: "Music", Path: "/music"},
		&db.Directory{ID: 5, LibraryID: 1, Path: "Lossy", CodecSummary: "MP3"},
		[]db.Track{
			{DirectoryID: 5, Filename: "song.mp3", Codec: "mp3", Bitrate: 320000, Duration: 200.0, SampleRate: 44100, Channels: 2, Size: 8000000},
		},
		"/music/Lossy",
	)

	if data.CanTranscode {
		t.Error("CanTranscode should be false for lossy tracks")
	}
}

// TestBuildDirPageData_LosslessBreakdown pins the per-track lossless data the
// selection UI is built on. CanTranscode is deliberately left all-or-nothing here —
// it is loosened in the same change that teaches the dock to send skip_lossy.
func TestBuildDirPageData_LosslessBreakdown(t *testing.T) {
	lib := LibraryConfig{ID: 1, Name: "Music", Path: "/music"}
	dir := &db.Directory{ID: 5, LibraryID: 1, Path: "Album"}

	tests := []struct {
		name             string
		tracks           []db.Track
		wantLossless     []bool
		wantCount        int
		wantHasLossy     bool
		wantCanTranscode bool
	}{
		{
			name: "all lossless",
			tracks: []db.Track{
				{Filename: "01.flac", Codec: "flac"},
				{Filename: "02.ape", Codec: "APE"},
				{Filename: "03.wav", Codec: "pcm_s16le"},
			},
			wantLossless:     []bool{true, true, true},
			wantCount:        3,
			wantHasLossy:     false,
			wantCanTranscode: true,
		},
		{
			name: "mixed",
			tracks: []db.Track{
				{Filename: "01.flac", Codec: "flac"},
				{Filename: "02.mp3", Codec: "mp3"},
				{Filename: "03.flac", Codec: "flac"},
			},
			wantLossless:     []bool{true, false, true},
			wantCount:        2,
			wantHasLossy:     true,
			wantCanTranscode: false,
		},
		{
			name: "all lossy",
			tracks: []db.Track{
				{Filename: "01.mp3", Codec: "mp3"},
				{Filename: "02.opus", Codec: "opus"},
			},
			wantLossless:     []bool{false, false},
			wantCount:        0,
			wantHasLossy:     true,
			wantCanTranscode: false,
		},
		{
			name:             "no tracks",
			tracks:           nil,
			wantLossless:     nil,
			wantCount:        0,
			wantHasLossy:     false,
			wantCanTranscode: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := buildDirPageData(lib, dir, tc.tracks, "/music/Album")

			if len(data.Tracks) != len(tc.wantLossless) {
				t.Fatalf("want %d rows, got %d", len(tc.wantLossless), len(data.Tracks))
			}
			for i, want := range tc.wantLossless {
				if data.Tracks[i].Lossless != want {
					t.Errorf("row %d (%s): Lossless = %v, want %v", i, data.Tracks[i].Filename, data.Tracks[i].Lossless, want)
				}
			}
			if data.LosslessCount != tc.wantCount {
				t.Errorf("LosslessCount = %d, want %d", data.LosslessCount, tc.wantCount)
			}
			if data.HasLossy != tc.wantHasLossy {
				t.Errorf("HasLossy = %v, want %v", data.HasLossy, tc.wantHasLossy)
			}
			if data.CanTranscode != tc.wantCanTranscode {
				t.Errorf("CanTranscode = %v, want %v", data.CanTranscode, tc.wantCanTranscode)
			}
		})
	}
}

// TestBuildDirTracksJSON verifies the payload the tc-tracks-data tag carries, and
// that a filename able to close the script element is escaped away by json.Marshal.
func TestBuildDirTracksJSON(t *testing.T) {
	data := buildDirPageData(
		LibraryConfig{ID: 1, Name: "Music", Path: "/music"},
		&db.Directory{ID: 5, LibraryID: 1, Path: "Album"},
		[]db.Track{
			{Filename: "01 A.flac", Codec: "flac"},
			{Filename: "02 </script>.mp3", Codec: "mp3"},
		},
		"/music/Album",
	)

	raw := string(data.TracksJSON)
	if strings.Contains(raw, "</script>") {
		t.Errorf("tracks JSON must not contain a literal </script>: %s", raw)
	}

	var got []dirTrackDTO
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("unmarshal tracks JSON: %v (%s)", err, raw)
	}
	want := []dirTrackDTO{
		{Name: "01 A.flac", Codec: "flac", Lossless: true},
		{Name: "02 </script>.mp3", Codec: "mp3", Lossless: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tracks JSON = %+v, want %+v", got, want)
	}
}

func TestBuildDirTracksJSON_NoTracksIsEmptyArray(t *testing.T) {
	data := buildDirPageData(
		LibraryConfig{ID: 1, Name: "Music", Path: "/music"},
		&db.Directory{ID: 5, LibraryID: 1, Path: "Empty"},
		nil,
		"/music/Empty",
	)

	if string(data.TracksJSON) != "[]" {
		t.Errorf("TracksJSON want %q, got %q", "[]", string(data.TracksJSON))
	}
}

// --- Tree node HTMX update (label loads dir content) ---

func TestTreeNodeTemplate_LabelLoadsContent(t *testing.T) {
	nd := TreeNodeData{
		LibraryID:   1,
		Path:        "Jazz",
		PathEncoded: "Jazz",
		AbsPath:     "/music/Jazz",
		AbsEncoded:  "%2Fmusic%2FJazz",
		Basename:    "Jazz",
		IsAudioDir:  true,
	}

	html, err := renderTreeNodes([]TreeNodeData{nd})
	if err != nil {
		t.Fatalf("renderTreeNodes: %v", err)
	}
	body := string(html)

	// Label should have hx-get pointing to /dir
	if !strings.Contains(body, `hx-get="/dir?path=`) {
		t.Errorf("tree label should have hx-get targeting /dir; got:\n%s", body)
	}
	// Should target the content area
	if !strings.Contains(body, `hx-target="#content-area"`) {
		t.Errorf("tree label should target #content-area; got:\n%s", body)
	}
	// Should select only dir-content from the response
	if !strings.Contains(body, `hx-select="#dir-content"`) {
		t.Errorf("tree label should use hx-select=#dir-content; got:\n%s", body)
	}
	// Row should exclude label clicks from children trigger
	if !strings.Contains(body, "tree-label-link") {
		t.Errorf("tree node should contain tree-label element; got:\n%s", body)
	}
}

func TestTreeNodeTemplate_RowExcludesLabelFromExpand(t *testing.T) {
	nd := TreeNodeData{
		LibraryID:   1,
		Path:        "Rock",
		PathEncoded: "Rock",
		AbsPath:     "/music/Rock",
		AbsEncoded:  "%2Fmusic%2FRock",
		Basename:    "Rock",
		IsAudioDir:  true,
		HasChildren: true,
	}

	html, err := renderTreeNodes([]TreeNodeData{nd})
	if err != nil {
		t.Fatalf("renderTreeNodes: %v", err)
	}
	body := string(html)

	// Row trigger should exclude clicks on the audio label link only.
	if !strings.Contains(body, ".tree-label-link") {
		t.Errorf("row hx-trigger should exclude tree-label-link clicks; got:\n%s", body)
	}
}

func TestTreeNodeTemplate_RowToggleControlsChildVisibility(t *testing.T) {
	nd := TreeNodeData{
		LibraryID:   1,
		Path:        "Rock",
		AbsPath:     "/music/Rock",
		Basename:    "Rock",
		IsAudioDir:  true,
		HasChildren: true,
	}

	html, err := renderTreeNodes([]TreeNodeData{nd})
	if err != nil {
		t.Fatalf("renderTreeNodes: %v", err)
	}
	body := string(html)

	if !strings.Contains(body, `var expanded=this.classList.toggle('expanded')`) {
		t.Errorf("row toggle should update expanded state; got:\n%s", body)
	}
	if !strings.Contains(body, `children.hidden=!expanded`) {
		t.Errorf("row toggle should hide child container when collapsed; got:\n%s", body)
	}
	if !strings.Contains(body, `<div class="tree-children" hidden></div>`) {
		t.Errorf("branch nodes should render hidden child container by default; got:\n%s", body)
	}
}

func TestTreeNodeTemplate_NonAudioLabelStillExpandsBranch(t *testing.T) {
	nd := TreeNodeData{
		LibraryID:   1,
		Path:        "Artists",
		AbsPath:     "/music/Artists",
		Basename:    "Artists",
		IsAudioDir:  false,
		HasChildren: true,
	}

	html, err := renderTreeNodes([]TreeNodeData{nd})
	if err != nil {
		t.Fatalf("renderTreeNodes: %v", err)
	}
	body := string(html)

	if !strings.Contains(body, "tree-label tree-label-disabled") {
		t.Errorf("non-audio branch should render a disabled label; got:\n%s", body)
	}
	if strings.Contains(body, `<span class="tree-label tree-label-link"`) {
		t.Errorf("non-audio branch should not render a clickable label; got:\n%s", body)
	}
	if !strings.Contains(body, ".tree-label-link") {
		t.Errorf("row expand trigger should ignore only clickable audio labels; got:\n%s", body)
	}
}
