package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/semsemyonoff/ALTO/internal/db"
)

// --- Response DTOs ---

type libraryDTO struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Indexed    bool   `json:"indexed"`
	TrackCount int    `json:"track_count"`
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

// --- Machine-readable API errors ---

// API error codes. These are the stable contract for JSON endpoints: the
// human-readable message may change, the code may not.
const (
	codeInvalidRequest           = "invalid_request"
	codePathForbidden            = "path_forbidden"
	codePathNotFound             = "path_not_found"
	codeLibraryNotFound          = "library_not_found"
	codeNotIndexed               = "not_indexed"
	codeNoTracks                 = "no_tracks"
	codeMixedDirectory           = "mixed_directory"
	codeNoLosslessTracks         = "no_lossless_tracks"
	codeUnknownFile              = "unknown_file"
	codeLossySourceSelected      = "lossy_source_selected"
	codeOutputDirNotConfigured   = "output_dir_not_configured"
	codeOutputNameConflict       = "output_name_conflict"
	codeCopySkippedNotApplicable = "copy_skipped_not_applicable"
	codeEngineUnavailable        = "engine_unavailable"
	codeJobAlreadyRunning        = "job_already_running"
	codeJobNotFound              = "job_not_found"
	codeScanRunning              = "scan_running"
	codeInternalError            = "internal_error"
)

// apiErrorDTO is the error envelope every JSON endpoint answers with.
// Extra carries optional per-code context (offending file names, a job id, …)
// and is flattened into the same object by MarshalJSON.
type apiErrorDTO struct {
	Error string         `json:"error"`
	Code  string         `json:"code"`
	Extra map[string]any `json:"-"`
}

// MarshalJSON flattens Extra alongside error/code. Keys named "error" or "code"
// in Extra never win — the contract fields are written last.
func (e apiErrorDTO) MarshalJSON() ([]byte, error) {
	obj := make(map[string]any, len(e.Extra)+2)
	maps.Copy(obj, e.Extra)
	obj["error"] = e.Error
	obj["code"] = e.Code
	return json.Marshal(obj)
}

// writeAPIError writes a machine-readable error body for JSON endpoints.
// HTML partials and page handlers keep using http.Error.
func writeAPIError(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	writeJSON(w, status, apiErrorDTO{Error: msg, Code: code, Extra: extra})
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
	counts, err := s.db.GetLibraryTrackCounts()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	dtos := make([]libraryDTO, len(libs))
	for i, l := range libs {
		count := counts[l.ID]
		dtos[i] = libraryDTO{ID: l.ID, Name: l.Name, Path: l.Path, TrackCount: count, Indexed: count > 0}
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

// handleTreeSearch returns directories in a library whose path contains q
// (case-insensitive) as a flat HTML partial for HTMX. An empty q re-renders
// the library's root tree so clearing the search box restores the normal view.
// GET /api/tree/{libraryID}/search?q=
func (s *Server) handleTreeSearch(w http.ResponseWriter, r *http.Request) {
	libraryID, err := strconv.ParseInt(r.PathValue("libraryID"), 10, 64)
	if err != nil || libraryID <= 0 {
		http.Error(w, "invalid library_id", http.StatusBadRequest)
		return
	}

	libCfg, ok := findLibConfigByID(s.cfg, libraryID)
	if !ok {
		libCfg = LibraryConfig{ID: libraryID}
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		children, err := s.db.GetDirectoryChildren(libraryID, "")
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		nodes, err := s.buildTreeNodes(libCfg, children)
		if err != nil {
			slog.Error("handleTreeSearch: buildTreeNodes", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		html, err := renderTreeNodes(nodes)
		if err != nil {
			slog.Error("handleTreeSearch: renderTreeNodes", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, string(html))
		return
	}

	dirs, err := s.db.GetDirectorySearch(libraryID, q)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	nodes := make([]SearchResultData, len(dirs))
	for i, d := range dirs {
		nodes[i] = buildSearchResultData(libCfg, d, q)
	}

	html, err := renderSearchResults(nodes, q)
	if err != nil {
		slog.Error("handleTreeSearch: renderSearchResults", "err", err)
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
