package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semsemyonoff/ALTO/internal/db"
)

// --- Scan UI: Re-index button and scan indicator in index page ---

// TestScanUI_ReindexButtonPresent verifies the Re-index button and scan indicator
// are rendered in the index page.
func TestScanUI_ReindexButtonPresent(t *testing.T) {
	srv, _, _ := newTestServerWithRealTemplates(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "scan-btn") {
		t.Error("index page should contain scan-btn element")
	}
	if !strings.Contains(body, "scan-indicator") {
		t.Error("index page should contain scan-indicator element")
	}
	if !strings.Contains(body, "Re-index") {
		t.Error("index page should contain Re-index label")
	}
}

// TestScanUI_ScanBtnCallsJS verifies the Re-index button uses onclick JS (not hx-post).
func TestScanUI_ScanBtnCallsJS(t *testing.T) {
	srv, _, _ := newTestServerWithRealTemplates(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "altoTriggerScan") {
		t.Error("index page scan button should invoke altoTriggerScan()")
	}
}

// TestScanUI_ScanSSEJSPresent verifies the SSE scan logic JS is embedded in the page.
func TestScanUI_ScanSSEJSPresent(t *testing.T) {
	srv, _, _ := newTestServerWithRealTemplates(t)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// SSE connection logic should be present.
	if !strings.Contains(body, "EventSource") {
		t.Error("index page should embed EventSource scan SSE logic")
	}
	if !strings.Contains(body, "/api/scan/status") {
		t.Error("index page JS should reference /api/scan/status SSE endpoint")
	}
	// Tree refresh after scan.
	if !strings.Contains(body, "refreshTree") {
		t.Error("index page JS should include refreshTree function for post-scan tree reload")
	}
	if !strings.Contains(body, `rel="icon"`) {
		t.Error("index page should include favicon link")
	}
}

// --- Directory page: library-id data attribute ---

// TestScanUI_DirectoryPageHasLibraryID verifies the directory page embeds the
// library ID in a data attribute so JS can trigger a library-scoped re-index.
func TestScanUI_DirectoryPageHasLibraryID(t *testing.T) {
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

	if !strings.Contains(body, "data-library-id=") {
		t.Error("directory page should have data-library-id attribute on #dir-content")
	}
}

// TestScanUI_ReindexOfferRendered verifies the Re-index Library button is present
// in a directory page with tracks (it is hidden initially, shown after transcode completes).
func TestScanUI_ReindexOfferRendered(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "Rock")
	mkdirAll(t, absPath)
	dirID, err := database.UpsertDirectory(libID, "Rock", "FLAC", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	database.UpsertTrack(db.Track{ //nolint:errcheck
		DirectoryID: dirID, Filename: "song.flac", Codec: "flac",
		Bitrate: 900_000, Duration: 180.0, SampleRate: 44100, Channels: 2, Size: 27_200_000,
	})

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "tc_reindex_btn") {
		t.Error("directory page should contain tc_reindex_btn element for post-transcode re-index offer")
	}
	if !strings.Contains(body, "altoPostTranscodeReindex") {
		t.Error("directory page should reference altoPostTranscodeReindex JS function")
	}
}

// TestScanUI_ReindexOfferNotRenderedWithoutTracks verifies the re-index button is
// absent when a directory has no tracks (transcode panel is not shown either).
func TestScanUI_ReindexOfferNotRenderedWithoutTracks(t *testing.T) {
	srv, database, libDir := newTestServerWithRealTemplates(t)
	libID := srv.cfg.Libraries[0].ID

	absPath := filepath.Join(libDir, "EmptyAlbum")
	mkdirAll(t, absPath)
	database.UpsertDirectoryWithAudioFlag(libID, "EmptyAlbum", "", false, "", true) //nolint:errcheck

	req := httptest.NewRequest(http.MethodGet, apiURL("/dir", map[string]string{"path": absPath}), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if strings.Contains(body, `id="tc_reindex_btn"`) {
		t.Error("tc_reindex_btn must not be rendered when directory has no tracks")
	}
}
