package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/semsemyonoff/ALTO/internal/db"
)

// --- Response DTOs ---

type libraryDTO struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type directoryDTO struct {
	ID           int64  `json:"id"`
	LibraryID    int64  `json:"library_id"`
	Path         string `json:"path"`
	HasCover     bool   `json:"has_cover"`
	CodecSummary string `json:"codec_summary"`
}

type trackDTO struct {
	ID          int64   `json:"id"`
	DirectoryID int64   `json:"directory_id"`
	Filename    string  `json:"filename"`
	Codec       string  `json:"codec"`
	Bitrate     int64   `json:"bitrate"`
	Duration    float64 `json:"duration"`
	SampleRate  int64   `json:"sample_rate"`
	Channels    int64   `json:"channels"`
	Size        int64   `json:"size"`
}

// writeJSON serialises v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("json encode", "err", err)
	}
}

// --- Handlers ---

// handleLibraries returns all libraries.
// GET /api/libraries
func (s *Server) handleLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := s.db.GetLibraries()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	dtos := make([]libraryDTO, len(libs))
	for i, l := range libs {
		dtos[i] = libraryDTO{ID: l.ID, Name: l.Name, Path: l.Path}
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": dtos})
}

// handleTree returns the full directory tree for a library.
// GET /api/tree/{libraryID}
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	libraryID, err := strconv.ParseInt(r.PathValue("libraryID"), 10, 64)
	if err != nil || libraryID <= 0 {
		http.Error(w, "invalid library_id", http.StatusBadRequest)
		return
	}

	dirs, err := s.db.GetDirectoryTree(libraryID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	dtos := make([]directoryDTO, len(dirs))
	for i, d := range dirs {
		dtos[i] = directoryDTO{
			ID: d.ID, LibraryID: d.LibraryID, Path: d.Path,
			HasCover: d.HasCover, CodecSummary: d.CodecSummary,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"directories": dtos})
}

// handleTreeChildren returns direct children of a directory as an HTML partial for HTMX.
// GET /api/tree/{libraryID}/children?parent=RELATIVE_PATH
func (s *Server) handleTreeChildren(w http.ResponseWriter, r *http.Request) {
	libraryID, err := strconv.ParseInt(r.PathValue("libraryID"), 10, 64)
	if err != nil || libraryID <= 0 {
		http.Error(w, "invalid library_id", http.StatusBadRequest)
		return
	}

	parent := r.URL.Query().Get("parent")

	children, err := s.db.GetDirectoryChildren(libraryID, parent)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	libCfg, ok := findLibConfigByID(s.cfg, libraryID)
	if !ok {
		// Library config not found; fall back to a minimal response.
		libCfg = LibraryConfig{ID: libraryID}
	}

	nodes, err := s.buildTreeNodes(libCfg, children)
	if err != nil {
		slog.Error("handleTreeChildren: buildTreeNodes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	html, err := renderTreeNodes(nodes)
	if err != nil {
		slog.Error("handleTreeChildren: renderTreeNodes", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, string(html))
}

// handleDir returns directory details and tracks for the given absolute path.
// GET /api/dir?path=ABSOLUTE_PATH
func (s *Server) handleDir(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	resolved, err := LibraryOnlyValidate(path, s.libRoots())
	if err != nil {
		WritePathError(w, err)
		return
	}

	lib, rel, ok := s.findLibraryForPath(resolved)
	if !ok {
		http.Error(w, "library not found for path", http.StatusNotFound)
		return
	}

	dir, err := s.db.GetDirectoryByPath(lib.ID, rel)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if dir == nil {
		http.Error(w, "directory not found", http.StatusNotFound)
		return
	}
	if !dir.IsAudio {
		http.Error(w, "directory not found", http.StatusNotFound)
		return
	}

	tracks, err := s.db.GetDirectoryFiles(dir.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	dirDTO := directoryDTO{
		ID: dir.ID, LibraryID: dir.LibraryID, Path: dir.Path,
		HasCover: dir.HasCover, CodecSummary: dir.CodecSummary,
	}
	trackDTOs := make([]trackDTO, len(tracks))
	for i, t := range tracks {
		trackDTOs[i] = trackDTO{
			ID: t.ID, DirectoryID: t.DirectoryID, Filename: t.Filename,
			Codec: t.Codec, Bitrate: t.Bitrate, Duration: t.Duration,
			SampleRate: t.SampleRate, Channels: t.Channels, Size: t.Size,
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"directory": dirDTO,
		"tracks":    trackDTOs,
	})
}

// handleScan triggers an asynchronous library re-index.
// POST /api/scan[?library_id=N]
// Returns 202 if started, 409 if a scan is already running, 404 if library_id not found.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	// Parse optional library_id filter.
	var targetLibraryID int64
	if idStr := r.URL.Query().Get("library_id"); idStr != "" {
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "invalid library_id", http.StatusBadRequest)
			return
		}
		targetLibraryID = id
	}

	if !s.scan.start() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "scan already running"})
		return
	}

	// Collect libraries to scan.
	var libs []db.Library
	for _, l := range s.cfg.Libraries {
		if targetLibraryID == 0 || l.ID == targetLibraryID {
			libs = append(libs, db.Library{ID: l.ID, Name: l.Name, Path: l.Path})
		}
	}

	if len(libs) == 0 {
		s.scan.reset()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "library not found"})
		return
	}

	s.launchScan(libs)

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// handleScanStatus streams scan progress events via SSE.
// GET /api/scan/status
// Sends an "idle" event immediately if no scan is running.
func (s *Server) handleScanStatus(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, running := s.scan.subscribe()
	if !running {
		_, _ = fmt.Fprintf(w, "event: idle\ndata: {}\n\n")
		flusher.Flush()
		return
	}
	defer s.scan.unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-ch:
			if !ok {
				return
			}
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
			flusher.Flush()
			if event.Type == "complete" || event.Type == "error" {
				return
			}
		}
	}
}

// handleCover serves cover art for a library directory.
// GET /api/cover?path=ABSOLUTE_DIR_PATH
// Path is validated against library roots; the cover file is resolved internally via DB.
func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	resolved, err := LibraryOnlyValidate(path, s.libRoots())
	if err != nil {
		WritePathError(w, err)
		return
	}

	lib, rel, ok := s.findLibraryForPath(resolved)
	if !ok {
		http.Error(w, "library not found for path", http.StatusNotFound)
		return
	}

	dir, err := s.db.GetDirectoryByPath(lib.ID, rel)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if dir == nil || !dir.HasCover || dir.CoverPath == "" {
		http.Error(w, "no cover art", http.StatusNotFound)
		return
	}

	// Open cover file with O_NOFOLLOW to prevent TOCTOU: if cover.jpg was replaced
	// with a symlink after scan-time Lstat validation, this call fails with ELOOP
	// rather than following the symlink to an arbitrary file.
	fd, err := syscall.Open(dir.CoverPath, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		http.Error(w, "cover not found", http.StatusNotFound)
		return
	}
	f := os.NewFile(uintptr(fd), dir.CoverPath)
	if f == nil {
		http.Error(w, "cover not found", http.StatusNotFound)
		return
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Determine content type from extension.
	ct := "image/jpeg"
	if strings.HasSuffix(strings.ToLower(dir.CoverPath), ".png") {
		ct = "image/png"
	}
	w.Header().Set("Content-Type", ct)

	http.ServeContent(w, r, dir.CoverPath, fi.ModTime(), f)
}
