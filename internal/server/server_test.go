package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semsemyonoff/ALTO/internal/db"
)

// mockScanner implements LibraryScanner for tests.
type mockScanner struct {
	// block is an optional channel; ScanAll blocks until it is closed.
	block chan struct{}
	// err is the error ScanAll returns after unblocking.
	err error
}

type scannerFunc func(context.Context, []db.Library) error

func (m *mockScanner) ScanAll(_ context.Context, _ []db.Library) error {
	if m.block != nil {
		<-m.block
	}
	return m.err
}

func (f scannerFunc) ScanAll(ctx context.Context, libraries []db.Library) error {
	return f(ctx, libraries)
}

// newTestServer creates a Server backed by an in-memory SQLite DB and a mock scanner.
// It registers a single library rooted at a temp directory and returns the server,
// the database, and the library root path.
func newTestServer(t *testing.T) (*Server, *db.DB, string) {
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
		Libraries: []LibraryConfig{
			{ID: libID, Name: "TestLib", Path: libDir},
		},
		OutputDir: t.TempDir(),
		StaticDir: realStaticDir(t),
	}
	srv := New(database, &mockScanner{}, cfg)
	return srv, database, libDir
}

// apiURL builds a URL with query parameters.
func apiURL(base string, params map[string]string) string {
	v := url.Values{}
	for k, val := range params {
		v.Set(k, val)
	}
	if len(params) == 0 {
		return base
	}
	return base + "?" + v.Encode()
}

// --- GET /api/libraries ---

func TestHandleLibraries_Empty(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	srv := New(database, &mockScanner{}, Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/libraries", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	libs := resp["libraries"].([]any)
	if len(libs) != 0 {
		t.Errorf("want 0 libraries, got %d", len(libs))
	}
}

func TestHandleLibraries_WithData(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/libraries", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	libs := resp["libraries"].([]any)
	if len(libs) != 1 {
		t.Fatalf("want 1 library, got %d", len(libs))
	}
	lib := libs[0].(map[string]any)
	if lib["name"] != "TestLib" {
		t.Errorf("want name TestLib, got %v", lib["name"])
	}
}

// --- GET /api/tree/{libraryID} ---

func TestHandleTree(t *testing.T) {
	srv, database, _ := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	_, err := database.UpsertDirectory(libID, "Jazz", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tree/"+itoa(libID), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	dirs := resp["directories"].([]any)
	if len(dirs) != 1 {
		t.Fatalf("want 1 directory, got %d", len(dirs))
	}
}

func TestHandleTree_EmptyLibrary(t *testing.T) {
	srv, _, _ := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	req := httptest.NewRequest(http.MethodGet, "/api/tree/"+itoa(libID), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	dirs := resp["directories"].([]any)
	if len(dirs) != 0 {
		t.Errorf("want 0 directories, got %d", len(dirs))
	}
}

func TestHandleTree_InvalidID(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/tree/notanid", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

// --- GET /api/tree/{libraryID}/children ---

func TestHandleTreeChildren(t *testing.T) {
	srv, database, _ := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	database.UpsertDirectory(libID, "Jazz", "FLAC", false, "")             //nolint:errcheck
	database.UpsertDirectory(libID, "Jazz/Miles Davis", "FLAC", false, "") //nolint:errcheck
	database.UpsertDirectory(libID, "Jazz/Coltrane", "FLAC", false, "")    //nolint:errcheck
	database.UpsertDirectory(libID, "Rock", "MP3", false, "")              //nolint:errcheck

	reqURL := apiURL("/api/tree/"+itoa(libID)+"/children", map[string]string{"parent": "Jazz"})
	req := httptest.NewRequest(http.MethodGet, reqURL, nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("want text/html content-type, got %q", ct)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Miles Davis") {
		t.Errorf("response should contain 'Miles Davis', got: %s", body)
	}
	if !strings.Contains(body, "Coltrane") {
		t.Errorf("response should contain 'Coltrane', got: %s", body)
	}
	// Rock is not under Jazz, should not appear.
	if strings.Contains(body, ">Rock<") {
		t.Errorf("response should not contain top-level Rock, got: %s", body)
	}
}

func TestHandleTreeChildren_NoParent(t *testing.T) {
	srv, database, _ := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	database.UpsertDirectory(libID, "Jazz", "FLAC", false, "") //nolint:errcheck
	database.UpsertDirectory(libID, "Rock", "MP3", false, "")  //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, "/api/tree/"+itoa(libID)+"/children", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Jazz") || !strings.Contains(body, "Rock") {
		t.Errorf("expected top-level directories in response, got: %s", body)
	}
}

// --- GET /api/dir ---

func TestHandleDir(t *testing.T) {
	srv, database, libDir := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Jazz")
	mkdirAll(t, absPath)

	dirID, err := database.UpsertDirectory(libID, "Jazz", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	if err := database.UpsertTrack(db.Track{
		DirectoryID: dirID,
		Filename:    "blue_in_green.flac",
		Codec:       "flac",
		Duration:    337.5,
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp["directory"] == nil {
		t.Error("missing 'directory' in response")
	}
	tracks := resp["tracks"].([]any)
	if len(tracks) != 1 {
		t.Errorf("want 1 track, got %d", len(tracks))
	}
}

func TestHandleDir_MissingPath(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/dir", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestHandleDir_OutsideRoot(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/dir", map[string]string{"path": "/etc"}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestHandleDir_AltoSegmentRejected(t *testing.T) {
	srv, _, libDir := newTestServer(t)

	altoDir := filepath.Join(libDir, ".alto-out", "something")
	mkdirAll(t, altoDir)

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/dir", map[string]string{"path": altoDir}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestHandleDir_AltoTmpRejected(t *testing.T) {
	srv, _, libDir := newTestServer(t)

	altoDir := filepath.Join(libDir, ".alto-tmp-999", "something")
	mkdirAll(t, altoDir)

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/dir", map[string]string{"path": altoDir}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestHandleDir_LegitimateOutDir(t *testing.T) {
	srv, database, libDir := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	// A directory named "out" (not .alto-*) should be accessible.
	outDir := filepath.Join(libDir, "out")
	mkdirAll(t, outDir)
	database.UpsertDirectory(libID, "out", "FLAC", false, "") //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/dir", map[string]string{"path": outDir}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleDir_NotInDB(t *testing.T) {
	srv, _, libDir := newTestServer(t)

	// Dir exists on disk but not in DB.
	absPath := filepath.Join(libDir, "Unknown")
	mkdirAll(t, absPath)

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleDir_NonAudioDirectoryRejected(t *testing.T) {
	srv, database, libDir := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Artists")
	mkdirAll(t, absPath)
	if _, err := database.UpsertDirectoryWithAudioFlag(libID, "Artists", "", false, "", false); err != nil {
		t.Fatalf("UpsertDirectoryWithAudioFlag: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
	}
}

// --- POST /api/scan ---

func TestHandleScan_Success(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp["status"] != "started" {
		t.Errorf("want status=started, got %v", resp)
	}

	// Wait for the goroutine to finish.
	time.Sleep(50 * time.Millisecond)
}

func TestHandleScan_Duplicate_Returns409(t *testing.T) {
	block := make(chan struct{})
	scanner := &mockScanner{block: block}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	libDir := t.TempDir()
	libID, _ := database.UpsertLibrary("Lib", libDir)
	cfg := Config{Libraries: []LibraryConfig{{ID: libID, Name: "Lib", Path: libDir}}}
	srv := New(database, scanner, cfg)

	// First scan: starts and blocks.
	req1 := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	w1 := httptest.NewRecorder()
	srv.ServeHTTP(w1, req1)
	if w1.Code != http.StatusAccepted {
		t.Fatalf("first scan: want 202, got %d", w1.Code)
	}

	// Allow goroutine to reach the block.
	time.Sleep(20 * time.Millisecond)

	// Second scan: should be rejected.
	req2 := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, req2)
	if w2.Code != http.StatusConflict {
		t.Fatalf("second scan: want 409, got %d", w2.Code)
	}

	var resp map[string]string
	json.NewDecoder(w2.Body).Decode(&resp) //nolint:errcheck
	if !strings.Contains(resp["error"], "already running") {
		t.Errorf("want error about already running, got %v", resp)
	}

	// Unblock and let scan finish.
	close(block)
	time.Sleep(20 * time.Millisecond)
}

func TestHandleScan_LibraryIDNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/scan?library_id=9999", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleScan_InvalidLibraryID(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/scan?library_id=notanumber", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestHandleScan_SpecificLibrary(t *testing.T) {
	srv, _, _ := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	req := httptest.NewRequest(http.MethodPost, apiURL("/api/scan", map[string]string{
		"library_id": itoa(libID),
	}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", w.Code, w.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
}

// --- GET /api/scan/status ---

func TestHandleScanStatus_Idle(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/scan/status", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("want text/event-stream, got %q", ct)
	}
	if !strings.Contains(w.Body.String(), "event: idle") {
		t.Errorf("want idle SSE event, got: %s", w.Body.String())
	}
}

func TestHandleScanStatus_ReceivesEvents(t *testing.T) {
	block := make(chan struct{})
	scanner := &mockScanner{block: block}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	libDir := t.TempDir()
	libID, _ := database.UpsertLibrary("Lib", libDir)
	cfg := Config{Libraries: []LibraryConfig{{ID: libID, Name: "Lib", Path: libDir}}}
	srv := New(database, scanner, cfg)

	// Start a scan that blocks.
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(20 * time.Millisecond)

	// Subscribe to SSE. Use a cancelable request to avoid blocking forever.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/scan/status", nil).WithContext(ctx)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(w, req)
		close(done)
	}()

	// Allow subscription to be set up.
	time.Sleep(20 * time.Millisecond)

	// Unblock scan → completion event.
	close(block)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not finish in time")
	}

	body := w.Body.String()
	if !strings.Contains(body, "event: started") && !strings.Contains(body, "event: complete") {
		t.Errorf("expected scan events, got: %s", body)
	}
}

func TestHandleScanStatus_CompleteIncludesSummary(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	libDir := t.TempDir()
	libID, err := database.UpsertLibrary("Lib", libDir)
	if err != nil {
		t.Fatalf("UpsertLibrary: %v", err)
	}
	if _, err := database.UpsertDirectory(libID, "Old Album", "MP3", false, ""); err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}

	block := make(chan struct{})
	scanner := scannerFunc(func(_ context.Context, _ []db.Library) error {
		<-block
		if _, err := database.UpsertDirectory(libID, "New Album", "FLAC", false, ""); err != nil {
			return err
		}
		return database.DeleteStaleDirectories(libID, []string{"New Album"})
	})

	cfg := Config{
		Libraries: []LibraryConfig{{ID: libID, Name: "Lib", Path: libDir}},
		StaticDir: realStaticDir(t),
	}
	srv := New(database, scanner, cfg)

	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/api/scan/status", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	close(block)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not finish in time")
	}

	body := w.Body.String()
	if !strings.Contains(body, `"message":"Re-index complete. Added: 1, removed: 1."`) {
		t.Fatalf("expected completion message with summary, got: %s", body)
	}
	if !strings.Contains(body, `"added":1`) {
		t.Fatalf("expected added count in SSE payload, got: %s", body)
	}
	if !strings.Contains(body, `"removed":1`) {
		t.Fatalf("expected removed count in SSE payload, got: %s", body)
	}
}

// --- GET /api/cover ---

func TestHandleCover_MissingPath(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/cover", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestHandleCover_OutsideRoot(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/cover", map[string]string{"path": "/etc"}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestHandleCover_AltoSegmentRejected(t *testing.T) {
	srv, _, libDir := newTestServer(t)

	altoDir := filepath.Join(libDir, ".alto-backup-123")
	mkdirAll(t, altoDir)

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/cover", map[string]string{"path": altoDir}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
}

func TestHandleCover_NoCover(t *testing.T) {
	srv, database, libDir := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Rock")
	mkdirAll(t, absPath)
	database.UpsertDirectory(libID, "Rock", "MP3", false, "") //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/cover", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleCover_WithJPEG(t *testing.T) {
	srv, database, libDir := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Classical")
	mkdirAll(t, absPath)

	coverFile := filepath.Join(absPath, "cover.jpg")
	if err := os.WriteFile(coverFile, []byte("\xFF\xD8\xFF" /* minimal JPEG marker */), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	database.UpsertDirectory(libID, "Classical", "FLAC", true, coverFile) //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/cover", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("want image/jpeg, got %q", ct)
	}
}

func TestHandleCover_WithPNG(t *testing.T) {
	srv, database, libDir := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Electronic")
	mkdirAll(t, absPath)

	coverFile := filepath.Join(absPath, "cover.png")
	if err := os.WriteFile(coverFile, []byte("\x89PNG\r\n" /* minimal PNG header */), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	database.UpsertDirectory(libID, "Electronic", "Opus", true, coverFile) //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/api/cover", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("want image/png, got %q", ct)
	}
}

func TestHandleFavicon_UsesLogo(t *testing.T) {
	srv, _, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Fatalf("want svg favicon content type, got %q", ct)
	}
}

// --- Destination path validation (unit tests for DestinationValidate) ---

func TestDestinationValidate_EndpointUsage(t *testing.T) {
	// Verify DestinationValidate works correctly for the transcode output
	// path scenarios that Task 7 will use.
	libRoot := t.TempDir()
	outRoot := t.TempDir()

	roots := []string{libRoot}

	// Resolve roots to handle OS-level symlinks (e.g. /var -> /private/var on macOS).
	resolvedLibRoot, _ := filepath.EvalSymlinks(libRoot)
	resolvedOutRoot, _ := filepath.EvalSymlinks(outRoot)

	// Non-existent nested dir under library root.
	target := filepath.Join(libRoot, "transcoded", "album")
	resolved, err := DestinationValidate(target, roots, outRoot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isWithin(resolved, resolvedLibRoot) && !isWithin(resolved, resolvedOutRoot) {
		t.Errorf("resolved path %q should be within a valid root (lib=%q, out=%q)", resolved, resolvedLibRoot, resolvedOutRoot)
	}

	// Path within output dir.
	outTarget := filepath.Join(outRoot, "Music", "output")
	resolved, err = DestinationValidate(outTarget, roots, outRoot)
	if err != nil {
		t.Fatalf("unexpected error for output dir path: %v", err)
	}
	if !isWithin(resolved, resolvedOutRoot) {
		t.Errorf("resolved %q should be within outRoot %q", resolved, resolvedOutRoot)
	}
}

// --- templateEngine ---

func TestTemplateEngine_MissingDir(t *testing.T) {
	te := templateEngine{dir: "/nonexistent/path/to/templates"}
	w := httptest.NewRecorder()
	te.render(w, "index.html", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("want 500 for missing templates dir, got %d", w.Code)
	}
}

func TestTemplateEngine_LoadAndRender(t *testing.T) {
	dir := t.TempDir()
	// Write a minimal template file.
	if err := os.WriteFile(filepath.Join(dir, "hello.html"), []byte(`{{define "hello.html"}}Hello, {{.}}!{{end}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

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

// --- Tree node template ---

func TestTreeNodeTemplate_BasicRender(t *testing.T) {
	nd := TreeNodeData{
		LibraryID:    1,
		Path:         "Jazz/Miles Davis",
		PathEncoded:  "Jazz%2FMiles+Davis",
		AbsPath:      "/music/Jazz/Miles Davis",
		AbsEncoded:   "%2Fmusic%2FJazz%2FMiles+Davis",
		Basename:     "Miles Davis",
		IsAudioDir:   true,
		HasChildren:  true,
		HasCover:     false,
		CodecSummary: "FLAC",
		CodecClass:   "codec-flac",
	}

	html, err := renderTreeNodes([]TreeNodeData{nd})
	if err != nil {
		t.Fatalf("renderTreeNodes: %v", err)
	}
	body := string(html)

	if !strings.Contains(body, "Miles Davis") {
		t.Errorf("rendered HTML should contain basename 'Miles Davis'; got:\n%s", body)
	}
	if !strings.Contains(body, "hx-get=") {
		t.Errorf("rendered HTML should contain HTMX hx-get attribute; got:\n%s", body)
	}
	if !strings.Contains(body, "tree-children") {
		t.Errorf("rendered HTML should contain .tree-children div; got:\n%s", body)
	}
	if !strings.Contains(body, "FLAC") {
		t.Errorf("rendered HTML should contain codec badge 'FLAC'; got:\n%s", body)
	}
	if !strings.Contains(body, "codec-flac") {
		t.Errorf("rendered HTML should contain CSS class 'codec-flac'; got:\n%s", body)
	}
	if strings.Contains(body, "%252Fmusic") {
		t.Errorf("rendered tree links must not double-encode absolute paths; got:\n%s", body)
	}
}

func TestTreeNodeTemplate_WithCover(t *testing.T) {
	nd := TreeNodeData{
		LibraryID:    2,
		Path:         "Classical",
		PathEncoded:  "Classical",
		AbsPath:      "/music/Classical",
		AbsEncoded:   "%2Fmusic%2FClassical",
		Basename:     "Classical",
		IsAudioDir:   true,
		HasCover:     true,
		CodecSummary: "FLAC",
		CodecClass:   "codec-flac",
	}

	html, err := renderTreeNodes([]TreeNodeData{nd})
	if err != nil {
		t.Fatalf("renderTreeNodes: %v", err)
	}
	body := string(html)

	// With cover, should show music icon rather than folder.
	if !strings.Contains(body, "🎵") {
		t.Errorf("rendered HTML with cover should contain 🎵 icon; got:\n%s", body)
	}
}

func TestTreeNodeTemplate_NoCoverNoBadge(t *testing.T) {
	nd := TreeNodeData{
		LibraryID:  1,
		Path:       "Rock",
		Basename:   "Rock",
		HasCover:   false,
		IsAudioDir: true,
	}

	html, err := renderTreeNodes([]TreeNodeData{nd})
	if err != nil {
		t.Fatalf("renderTreeNodes: %v", err)
	}
	body := string(html)

	// No cover → folder icon.
	if !strings.Contains(body, "📁") {
		t.Errorf("rendered HTML without cover should contain 📁 icon; got:\n%s", body)
	}
	// No codec summary → no badge element.
	if strings.Contains(body, "codec-badge") {
		t.Errorf("rendered HTML with no codec should not contain codec-badge; got:\n%s", body)
	}
}

func TestTreeNodeTemplate_MultipleNodes(t *testing.T) {
	nodes := []TreeNodeData{
		{LibraryID: 1, Path: "Jazz", Basename: "Jazz", IsAudioDir: true, CodecSummary: "FLAC", CodecClass: "codec-flac"},
		{LibraryID: 1, Path: "Rock", Basename: "Rock", IsAudioDir: true, CodecSummary: "MP3", CodecClass: "codec-mp3"},
		{LibraryID: 1, Path: "Electronic", Basename: "Electronic", IsAudioDir: true, CodecSummary: "Opus", CodecClass: "codec-opus"},
	}

	html, err := renderTreeNodes(nodes)
	if err != nil {
		t.Fatalf("renderTreeNodes: %v", err)
	}
	body := string(html)

	for _, want := range []string{"Jazz", "Rock", "Electronic", "FLAC", "MP3", "Opus"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered HTML should contain %q; got:\n%s", want, body)
		}
	}
}

func TestCodecClass(t *testing.T) {
	cases := []struct {
		summary string
		want    string
	}{
		{"FLAC", "codec-flac"},
		{"flac", "codec-flac"},
		{"Opus", "codec-opus"},
		{"MP3", "codec-mp3"},
		{"Mixed", "codec-mixed"},
		{"MIXED", "codec-mixed"},
		{"WAV", "codec-other"},
		{"", ""},
	}
	for _, tc := range cases {
		got := codecClass(tc.summary)
		if got != tc.want {
			t.Errorf("codecClass(%q) = %q, want %q", tc.summary, got, tc.want)
		}
	}
}

func TestBuildTreeNodeData(t *testing.T) {
	lib := LibraryConfig{ID: 1, Name: "Music", Path: "/music"}
	dir := db.Directory{
		ID:           10,
		LibraryID:    1,
		Path:         "Jazz/Miles Davis",
		HasCover:     true,
		CodecSummary: "FLAC",
		IsAudio:      true,
		HasChildren:  true,
	}

	nd := buildTreeNodeData(lib, dir)

	if nd.LibraryID != 1 {
		t.Errorf("LibraryID want 1, got %d", nd.LibraryID)
	}
	if nd.Path != "Jazz/Miles Davis" {
		t.Errorf("Path want 'Jazz/Miles Davis', got %q", nd.Path)
	}
	if nd.Basename != "Miles Davis" {
		t.Errorf("Basename want 'Miles Davis', got %q", nd.Basename)
	}
	if nd.PathEncoded == "" {
		t.Error("PathEncoded should not be empty")
	}
	if nd.HasCover != true {
		t.Error("HasCover should be true")
	}
	if !nd.IsAudioDir {
		t.Error("IsAudioDir should be true")
	}
	if !nd.HasChildren {
		t.Error("HasChildren should be true")
	}
	if nd.CodecClass != "codec-flac" {
		t.Errorf("CodecClass want 'codec-flac', got %q", nd.CodecClass)
	}
}

func TestTreeNodeTemplate_LeafAudioNodeHasNoExpandArrow(t *testing.T) {
	nd := TreeNodeData{
		LibraryID:   1,
		Path:        "Jazz/Kind of Blue",
		AbsPath:     "/music/Jazz/Kind of Blue",
		Basename:    "Kind of Blue",
		IsAudioDir:  true,
		HasChildren: false,
	}

	html, err := renderTreeNodes([]TreeNodeData{nd})
	if err != nil {
		t.Fatalf("renderTreeNodes: %v", err)
	}
	body := string(html)

	if strings.Contains(body, `<span class="tree-toggle">▶</span>`) {
		t.Errorf("leaf audio node should not render expand arrow; got:\n%s", body)
	}
	if !strings.Contains(body, "tree-toggle-placeholder") {
		t.Errorf("leaf audio node should render toggle placeholder; got:\n%s", body)
	}
	if strings.Contains(body, `hx-get="/api/tree/1/children?parent=`) {
		t.Errorf("leaf audio node should not request children; got:\n%s", body)
	}
}

func TestTreeNodeTemplate_NonAudioNodeHasNoOpenLink(t *testing.T) {
	nd := TreeNodeData{
		LibraryID:   1,
		Path:        "Jazz",
		AbsPath:     "/music/Jazz",
		Basename:    "Jazz",
		IsAudioDir:  false,
		HasChildren: true,
	}

	html, err := renderTreeNodes([]TreeNodeData{nd})
	if err != nil {
		t.Fatalf("renderTreeNodes: %v", err)
	}
	body := string(html)

	if strings.Contains(body, `hx-get="/dir?path=`) {
		t.Errorf("non-audio node should not open a directory page; got:\n%s", body)
	}
	if strings.Contains(body, "tree-open-link") {
		t.Errorf("non-audio node should not render open link; got:\n%s", body)
	}
	if !strings.Contains(body, `<span class="tree-toggle">▶</span>`) {
		t.Errorf("non-audio branch should still render expand arrow; got:\n%s", body)
	}
}

// --- Index page ---

// writeTemplateFile writes content to a file in dir, fataling on error.
func writeTemplateFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

// minimalIndexTemplates creates a minimal set of templates for testing the index page.
func minimalIndexTemplates(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTemplateFile(t, dir, "base.html", `{{define "base"}}<!DOCTYPE html><html><body>{{if .Notice}}<div class="app-notice">{{.Notice}}</div>{{end}}{{template "sidebar" .}}{{template "content" .}}</body></html>{{end}}`)
	writeTemplateFile(t, dir, "index.html", `{{define "sidebar"}}<nav id="tree-root">{{.TopDirsHTML}}</nav>{{end}}{{define "content"}}<main>Select a directory</main>{{end}}{{define "index.html"}}{{template "base" .}}{{end}}`)
	return dir
}

func TestHandleIndex_NoLibraries(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	tmplDir := minimalIndexTemplates(t)
	srv := New(database, &mockScanner{}, Config{TemplateDir: tmplDir})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "tree-root") {
		t.Errorf("response should contain tree-root element; got:\n%s", body)
	}
}

func TestHandleIndex_WithNotice(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	tmplDir := minimalIndexTemplates(t)
	srv := New(database, &mockScanner{}, Config{TemplateDir: tmplDir})

	req := httptest.NewRequest(http.MethodGet, "/?notice=directory_not_found", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Directory not found.") {
		t.Fatalf("expected notice in response, got:\n%s", w.Body.String())
	}
}

func TestHandleUnknownPage_RedirectsToIndexWithNotice(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	tmplDir := minimalIndexTemplates(t)
	srv := New(database, &mockScanner{}, Config{TemplateDir: tmplDir})

	req := httptest.NewRequest(http.MethodGet, "/missing-page", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/?notice=directory_not_found" {
		t.Fatalf("want redirect to /?notice=directory_not_found, got %q", got)
	}
}

func TestHandleUnknownAPIPath_Stays404(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	tmplDir := minimalIndexTemplates(t)
	srv := New(database, &mockScanner{}, Config{TemplateDir: tmplDir})

	req := httptest.NewRequest(http.MethodGet, "/api/missing", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", w.Code)
	}
}

func TestHandleIndex_WithLibraryAndDirectories(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	libDir := t.TempDir()
	libID, err := database.UpsertLibrary("Music", libDir)
	if err != nil {
		t.Fatalf("UpsertLibrary: %v", err)
	}
	database.UpsertDirectory(libID, "Jazz", "FLAC", false, "")          //nolint:errcheck
	database.UpsertDirectory(libID, "Rock", "MP3", false, "")           //nolint:errcheck
	database.UpsertDirectory(libID, "Jazz/Coltrane", "FLAC", false, "") //nolint:errcheck

	tmplDir := minimalIndexTemplates(t)
	cfg := Config{
		Libraries:   []LibraryConfig{{ID: libID, Name: "Music", Path: libDir}},
		TemplateDir: tmplDir,
	}
	srv := New(database, &mockScanner{}, cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Top-level dirs should appear; nested dirs should not (GetDirectoryChildren with "" only returns top-level).
	if !strings.Contains(body, "Jazz") {
		t.Errorf("index page should contain 'Jazz' directory; got:\n%s", body)
	}
	if !strings.Contains(body, "Rock") {
		t.Errorf("index page should contain 'Rock' directory; got:\n%s", body)
	}
}

func TestHandleIndex_CodecBadgesPresent(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	libDir := t.TempDir()
	libID, err := database.UpsertLibrary("Music", libDir)
	if err != nil {
		t.Fatalf("UpsertLibrary: %v", err)
	}
	database.UpsertDirectory(libID, "Lossless", "FLAC", false, "") //nolint:errcheck
	database.UpsertDirectory(libID, "Lossy", "Opus", false, "")    //nolint:errcheck

	tmplDir := minimalIndexTemplates(t)
	cfg := Config{
		Libraries:   []LibraryConfig{{ID: libID, Name: "Music", Path: libDir}},
		TemplateDir: tmplDir,
	}
	srv := New(database, &mockScanner{}, cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "codec-flac") {
		t.Errorf("index page should contain codec-flac badge; got:\n%s", body)
	}
	if !strings.Contains(body, "codec-opus") {
		t.Errorf("index page should contain codec-opus badge; got:\n%s", body)
	}
}

func TestHandleIndex_OpenInNewTabLinks(t *testing.T) {
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer func() { _ = database.Close() }()

	libDir := t.TempDir()
	libID, err := database.UpsertLibrary("Music", libDir)
	if err != nil {
		t.Fatalf("UpsertLibrary: %v", err)
	}
	database.UpsertDirectory(libID, "Jazz", "FLAC", false, "") //nolint:errcheck

	tmplDir := minimalIndexTemplates(t)
	cfg := Config{
		Libraries:   []LibraryConfig{{ID: libID, Name: "Music", Path: libDir}},
		TemplateDir: tmplDir,
	}
	srv := New(database, &mockScanner{}, cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// "Open in new tab" links should be present with target=_blank.
	if !strings.Contains(body, `target="_blank"`) {
		t.Errorf("index page should contain open-in-new-tab links; got:\n%s", body)
	}
}

// TestHandleTreeChildren_HTMXAttributes verifies the new tree children partial includes HTMX attributes.
func TestHandleTreeChildren_HTMXAttributes(t *testing.T) {
	srv, database, _ := newTestServer(t)
	libID := srv.cfg.Libraries[0].ID

	database.UpsertDirectory(libID, "Jazz", "FLAC", false, "") //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, "/api/tree/"+itoa(libID)+"/children", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "hx-get=") {
		t.Errorf("tree children should include hx-get attribute; got:\n%s", body)
	}
	if !strings.Contains(body, "tree-children") {
		t.Errorf("tree children should include .tree-children container; got:\n%s", body)
	}
	if !strings.Contains(body, "Jazz") {
		t.Errorf("tree children should include directory name 'Jazz'; got:\n%s", body)
	}
}

// itoa converts int64 to string, for building URL paths in tests.
func itoa(n int64) string {
	return strings.TrimRight(strings.TrimRight(
		// Use Sprintf since strconv needs import otherwise.
		// This avoids adding an import just for itoa.
		func() string {
			buf := make([]byte, 0, 20)
			if n < 0 {
				buf = append(buf, '-')
				n = -n
			}
			if n == 0 {
				return "0"
			}
			tmp := make([]byte, 0, 20)
			for n > 0 {
				tmp = append(tmp, byte('0'+n%10))
				n /= 10
			}
			for i := len(tmp) - 1; i >= 0; i-- {
				buf = append(buf, tmp[i])
			}
			return string(buf)
		}(),
		""), "")
}
