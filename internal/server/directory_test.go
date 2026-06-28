package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
