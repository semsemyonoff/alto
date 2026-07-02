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
