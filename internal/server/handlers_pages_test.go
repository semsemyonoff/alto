package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/semsemyonoff/ALTO/internal/db"
)

// TestHighlightMatch covers the case-insensitive highlight helper: HTML
// escaping of the surrounding name, correct match wrapping, the empty/no-match
// fallbacks, and multibyte inputs whose lowercase encoding differs in length
// from the original (which previously mis-sliced or panicked).
func TestHighlightMatch(t *testing.T) {
	tests := []struct {
		name  string
		input string
		query string
		want  string
	}{
		{"simple match", "Miles Davis", "miles", "<mark>Miles</mark> Davis"},
		{"case insensitive", "JAZZ", "jazz", "<mark>JAZZ</mark>"},
		{"no match", "Blues", "rock", "Blues"},
		{"empty query", "Blues", "", "Blues"},
		{"escapes surrounding", `A<b>&"C`, "b", `A&lt;<mark>b</mark>&gt;&amp;&#34;C`},
		{"escapes matched special chars", `x<y`, "<", `x<mark>&lt;</mark>y`},
		// Multibyte name matched by an ASCII query: offsets must track the
		// original string's byte boundaries, not a lowercased copy's.
		{"multibyte name ascii query", "İstanbul", "istanbul", "<mark>İstanbul</mark>"},
		{"cyrillic case fold", "Дом", "д", "<mark>Д</mark>ом"},
		// U+023A (Ⱥ, 2 bytes) lowercases to U+2C65 (ⱥ, 3 bytes): the query is
		// longer than the match in the original string.
		{"fold grows byte length", "Ⱥ", "ⱥ", "<mark>Ⱥ</mark>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(highlightMatch(tt.input, tt.query))
			if got != tt.want {
				t.Errorf("highlightMatch(%q, %q) = %q, want %q", tt.input, tt.query, got, tt.want)
			}
		})
	}
}

// TestTemplateEngine_PerPageIsolation verifies that two pages which each
// define a block of the same name ("content") are parsed into separate
// template groups and don't collide, since each page is parsed with its own
// ParseFiles call rather than one shared glob.
func TestTemplateEngine_PerPageIsolation(t *testing.T) {
	dir := t.TempDir()
	writeTemplateFile(t, dir, "base.html",
		`{{define "base"}}<base>{{template "content" .}}</base>{{end}}`)
	writeTemplateFile(t, dir, "index.html",
		`{{define "content"}}index-content{{end}}{{define "index.html"}}{{template "base" .}}{{end}}`)
	writeTemplateFile(t, dir, "directory.html",
		`{{define "content"}}dir-content{{end}}{{define "directory.html"}}{{template "base" .}}{{end}}`)

	te := templateEngine{dir: dir}

	w := httptest.NewRecorder()
	te.render(w, "index.html", nil)
	if body := w.Body.String(); body != "<base>index-content</base>" {
		t.Errorf("index.html render: got %q", body)
	}

	w = httptest.NewRecorder()
	te.render(w, "directory.html", nil)
	if body := w.Body.String(); body != "<base>dir-content</base>" {
		t.Errorf("directory.html render: got %q, want the directory page's own content block untouched by index.html's", body)
	}
}

// TestTemplateEngine_SharedPartialOptional verifies that sidebar.html is
// included when present but its absence doesn't break rendering (some
// template dirs, like plain utility tests, don't need the app shell).
func TestTemplateEngine_SharedPartialOptional(t *testing.T) {
	dir := t.TempDir()
	writeTemplateFile(t, dir, "hello.html", `{{define "hello.html"}}Hello, {{.}}!{{end}}`)

	te := templateEngine{dir: dir}
	w := httptest.NewRecorder()
	te.render(w, "hello.html", "World")

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); body != "Hello, World!" {
		t.Errorf("want 'Hello, World!', got %q", body)
	}
}

// TestTemplateEngine_SidebarPartialIncluded verifies that a shared
// sidebar.html file is parsed alongside a page that references it.
func TestTemplateEngine_SidebarPartialIncluded(t *testing.T) {
	dir := t.TempDir()
	writeTemplateFile(t, dir, "base.html",
		`{{define "base"}}<base>{{template "sidebar" .}}</base>{{end}}`)
	writeTemplateFile(t, dir, "sidebar.html", `{{define "sidebar"}}shared-sidebar{{end}}`)
	writeTemplateFile(t, dir, "index.html", `{{define "index.html"}}{{template "base" .}}{{end}}`)

	te := templateEngine{dir: dir}
	w := httptest.NewRecorder()
	te.render(w, "index.html", nil)

	if body := w.Body.String(); body != "<base>shared-sidebar</base>" {
		t.Errorf("want shared sidebar partial rendered, got %q", body)
	}
}

// newTestServerWithSidebarPartial builds a Server whose template dir mirrors
// the real app shell split: base.html + sidebar.html (shared) + a page file
// per screen, so both "/" and "/dir" can be rendered against the same shared
// sidebar without colliding.
func newTestServerWithSidebarPartial(t *testing.T) (*Server, *db.DB, string) {
	t.Helper()

	dir := t.TempDir()
	writeTemplateFile(t, dir, "base.html",
		`{{define "base"}}<!DOCTYPE html><html><body>{{template "sidebar" .}}{{template "content" .}}</body></html>{{end}}`)
	writeTemplateFile(t, dir, "sidebar.html",
		`{{define "sidebar"}}<nav id="tree-root">{{.TopDirsHTML}}</nav>{{end}}`)
	writeTemplateFile(t, dir, "index.html",
		`{{define "content"}}<main>Select a directory</main>{{end}}{{define "index.html"}}{{template "base" .}}{{end}}`)
	writeTemplateFile(t, dir, "directory.html",
		`{{define "directory.html"}}<!DOCTYPE html><html><body>`+
			`<nav id="tree-root">{{.TopDirsHTML}}</nav>`+
			`<div id="dir-content" class="dir-page"><h1 class="dir-title">{{.DirName}}</h1></div>`+
			`</body></html>{{end}}`)

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
		TemplateDir: dir,
	}
	srv := New(database, &mockScanner{}, cfg)
	return srv, database, libDir
}

func TestHandleIndex_RendersSharedSidebarPartial(t *testing.T) {
	srv, _, _ := newTestServerWithSidebarPartial(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `id="tree-root"`) {
		t.Errorf("index page should render the shared sidebar partial; got:\n%s", w.Body.String())
	}
}

func TestHandleDirPage_RendersWithoutTemplateCollision(t *testing.T) {
	srv, database, libDir := newTestServerWithSidebarPartial(t)
	libID := srv.cfg.Libraries[0].ID

	dirPath := filepath.Join(libDir, "Album")
	if err := os.MkdirAll(dirPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := database.UpsertDirectory(libID, "Album", "FLAC", false, ""); err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/dir?path="+dirPath, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `id="dir-content"`) {
		t.Errorf("directory page should render its own content block; got:\n%s", w.Body.String())
	}

	// Now render "/" again to ensure loading directory.html's isolated
	// template group didn't clobber index.html's cached one.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 for '/', got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Select a directory") {
		t.Errorf("index page content should be unaffected by rendering /dir; got:\n%s", w.Body.String())
	}
}

// --- Real templates: viteTags integration ---

// TestRealTemplates_IndexRendersViteTagsStubWithoutBuiltDist verifies that
// the real base.html's {{ viteTags "src/main.ts" }} call renders the
// test-stub placeholder (rather than failing loudly) when StaticDir has no
// built `dist/`, which is how most other server tests are configured.
func TestRealTemplates_IndexRendersViteTagsStubWithoutBuiltDist(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	cfg := Config{
		OutputDir:   t.TempDir(),
		TemplateDir: realTemplateDir(t),
		StaticDir:   t.TempDir(), // no dist/.vite/manifest.json here
	}
	srv := New(database, &mockScanner{}, cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "vite assets stubbed") {
		t.Errorf("real index template should render the vite test-stub placeholder; got:\n%s", w.Body.String())
	}
}

// TestSidebar_RealTemplates_RendersTreeSearchInput verifies that the shared
// sidebar partial (Task 15) renders a directory search input alongside the
// tree, on both the index and directory pages.
func TestSidebar_RealTemplates_RendersTreeSearchInput(t *testing.T) {
	srv, _, _ := newTestServerWithRealTemplates(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="tree-search-input"`) {
		t.Errorf("sidebar should render a tree search input; got:\n%s", body)
	}
	if !strings.Contains(body, `class="tree-search"`) {
		t.Errorf("sidebar should wrap the search input in a tree-search container; got:\n%s", body)
	}
}

// TestHandleIndex_RendersStandbyWhenNotIndexed verifies that the index page
// shows the standby state (gauge + "Re-index to scan" CTA) instead of the
// directory placeholder when the selected library has no indexed content.
func TestHandleIndex_RendersStandbyWhenNotIndexed(t *testing.T) {
	srv, _, _ := newTestServerWithRealTemplates(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="standby"`) {
		t.Errorf("un-indexed library should render the standby state; got:\n%s", body)
	}
	if !strings.Contains(body, "Re-index to scan") {
		t.Errorf("standby state should include a re-index CTA; got:\n%s", body)
	}
	if strings.Contains(body, "content-placeholder") {
		t.Errorf("un-indexed library should not render the directory placeholder; got:\n%s", body)
	}
}

// TestHandleIndex_RendersPlaceholderWhenIndexed verifies that once a library
// has indexed content, the index page falls back to the normal "select a
// directory" placeholder rather than the standby state.
func TestHandleIndex_RendersPlaceholderWhenIndexed(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Jazz")
	mkdirAll(t, absPath)
	dirID, err := database.UpsertDirectory(libID, "Jazz", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	if err := database.UpsertTrack(db.Track{DirectoryID: dirID, Filename: "01.flac", Codec: "flac"}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "content-placeholder") {
		t.Errorf("indexed library should render the directory placeholder; got:\n%s", body)
	}
	if strings.Contains(body, `id="standby"`) {
		t.Errorf("indexed library should not render the standby state; got:\n%s", body)
	}
}

// TestRealTemplates_IndexRendersHashedViteTagsWithBuiltDist verifies that,
// against the project's actual built web/static/dist manifest, base.html
// renders real hashed asset tags.
func TestRealTemplates_IndexRendersHashedViteTagsWithBuiltDist(t *testing.T) {
	staticDir := realStaticDir(t)
	if _, err := os.Stat(viteManifestPath(staticDir)); err != nil {
		t.Skip("web/static/dist/.vite/manifest.json not built; run `npm run build` in web/frontend first")
	}

	srv, _, _ := newTestServerWithRealTemplates(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `/static/dist/assets/`) {
		t.Errorf("real index template should render hashed vite asset tags; got:\n%s", w.Body.String())
	}
}

// TestBuildDockPresetsJSON_GroupsByCodecWithOneDefaultEach verifies the dock's preset
// JSON has an entry for each of DefaultPresets(), grouped under "flac"/"opus", with
// exactly one preset per codec flagged as the default.
func TestBuildDockPresetsJSON_GroupsByCodecWithOneDefaultEach(t *testing.T) {
	raw := buildDockPresetsJSON()

	var grouped map[string][]dockPresetDTO
	if err := json.Unmarshal([]byte(raw), &grouped); err != nil {
		t.Fatalf("unmarshal preset JSON: %v\ngot: %s", err, raw)
	}

	if len(grouped["flac"]) != 3 {
		t.Fatalf("want 3 flac presets, got %d: %+v", len(grouped["flac"]), grouped["flac"])
	}
	if len(grouped["opus"]) != 3 {
		t.Fatalf("want 3 opus presets, got %d: %+v", len(grouped["opus"]), grouped["opus"])
	}

	for codec, presets := range grouped {
		defaults := 0
		for _, p := range presets {
			if p.Default {
				defaults++
			}
			if p.Name == "" || p.Label == "" {
				t.Errorf("codec %q preset missing name/label: %+v", codec, p)
			}
		}
		if defaults != 1 {
			t.Errorf("codec %q should have exactly one default preset, got %d", codec, defaults)
		}
	}

	var balanced, musicHigh dockPresetDTO
	for _, p := range grouped["flac"] {
		if p.Name == "Balanced" {
			balanced = p
		}
	}
	for _, p := range grouped["opus"] {
		if p.Name == "Music High" {
			musicHigh = p
		}
	}
	if !balanced.Default {
		t.Error("expect FLAC \"Balanced\" preset to be the default")
	}
	if balanced.Label != "Balanced (compression 5)" {
		t.Errorf("unexpected FLAC Balanced label: %q", balanced.Label)
	}
	if !musicHigh.Default {
		t.Error("expect Opus \"Music High\" preset to be the default")
	}
	if musicHigh.Label != "Music High (160k)" {
		t.Errorf("unexpected Opus Music High label: %q", musicHigh.Label)
	}
}

// TestHandlePresets_ServesFullPresetShape verifies GET /api/presets answers JSON
// carrying every built-in preset with its full parameter set, so an API client can
// pick one without scraping the /dir page.
func TestHandlePresets_ServesFullPresetShape(t *testing.T) {
	srv, _, _ := newTestServerWithSidebarPartial(t)

	req := httptest.NewRequest(http.MethodGet, "/api/presets", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("want application/json, got %q", ct)
	}

	var got presetsDTO
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal body: %v\ngot: %s", err, w.Body.String())
	}

	if want := []string{"flac", "opus"}; !slices.Equal(got.Codecs, want) {
		t.Errorf("codecs = %v, want %v", got.Codecs, want)
	}
	if len(got.Presets) != 6 {
		t.Fatalf("want 6 presets, got %d: %+v", len(got.Presets), got.Presets)
	}

	byName := make(map[string]presetDTO, len(got.Presets))
	defaults := make(map[string]int, 2)
	for _, p := range got.Presets {
		byName[p.Name] = p
		if p.Default {
			defaults[p.Codec]++
		}
	}
	for _, codec := range got.Codecs {
		if defaults[codec] != 1 {
			t.Errorf("codec %q should have exactly one default preset, got %d", codec, defaults[codec])
		}
	}

	wantBalanced := presetDTO{
		Name: "Balanced", Label: "Balanced (compression 5)", Codec: "flac",
		CompressionLevel: 5, CopyMetadata: true, CopyCover: true, Default: true,
	}
	if got := byName["Balanced"]; got != wantBalanced {
		t.Errorf("FLAC Balanced = %+v, want %+v", got, wantBalanced)
	}
	wantMusicHigh := presetDTO{
		Name: "Music High", Label: "Music High (160k)", Codec: "opus",
		CompressionLevel: 10, Bitrate: "160k", CopyMetadata: true, CopyCover: false, Default: true,
	}
	if got := byName["Music High"]; got != wantMusicHigh {
		t.Errorf("Opus Music High = %+v, want %+v", got, wantMusicHigh)
	}
}

// TestPresets_PageAndAPIDescribeTheSameSet pins the invariant behind sourcing both
// from buildPresets(): the /dir page's inlined tc-presets-data payload and the API
// response must never drift apart in membership, codec grouping, label or default.
func TestPresets_PageAndAPIDescribeTheSameSet(t *testing.T) {
	var page map[string][]dockPresetDTO
	if err := json.Unmarshal([]byte(buildDockPresetsJSON()), &page); err != nil {
		t.Fatalf("unmarshal page presets: %v", err)
	}

	api := buildPresets()
	if len(page) != len(api.Codecs) {
		t.Errorf("page groups %d codecs, API reports %d: %v vs %v", len(page), len(api.Codecs), page, api.Codecs)
	}

	seen := 0
	for _, p := range api.Presets {
		seen++
		idx := slices.IndexFunc(page[p.Codec], func(d dockPresetDTO) bool { return d.Name == p.Name })
		if idx < 0 {
			t.Errorf("preset %q (%s) missing from the page payload", p.Name, p.Codec)
			continue
		}
		if d := page[p.Codec][idx]; d.Label != p.Label || d.Default != p.Default {
			t.Errorf("preset %q: page has %+v, API has label=%q default=%v", p.Name, d, p.Label, p.Default)
		}
	}
	total := 0
	for _, presets := range page {
		total += len(presets)
	}
	if total != seen {
		t.Errorf("page carries %d presets, API carries %d", total, seen)
	}
}
