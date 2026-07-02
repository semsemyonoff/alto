package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/semsemyonoff/ALTO/internal/transcode"
)

// bitrateRe validates bitrate strings like "128k", "320k", "160000".
var bitrateRe = regexp.MustCompile(`^[0-9]+k?$`)

// transcodeRequest is the JSON body for POST /api/transcode.
type transcodeRequest struct {
	Path       string `json:"path"`
	Codec      string `json:"codec"`       // "flac" or "opus"
	Preset     string `json:"preset"`      // preset name (optional if custom params given)
	OutputMode string `json:"output_mode"` // "shared", "local", "replace"
	// Custom override fields (all optional; ignored when Preset matches a named preset).
	CompressionLevel *int   `json:"compression_level,omitempty"`
	Bitrate          string `json:"bitrate,omitempty"`
	CopyMetadata     *bool  `json:"copy_metadata,omitempty"`
	CopyCover        *bool  `json:"copy_cover,omitempty"`
}

// handleTranscodeStart handles POST /api/transcode.
// It validates the source path, looks up tracks in the index, starts a job, and returns the job ID.
func (s *Server) handleTranscodeStart(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		http.Error(w, "transcoding not available", http.StatusServiceUnavailable)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB max
	var req transcodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Path == "" {
		http.Error(w, "path required", http.StatusBadRequest)
		return
	}

	// Validate source path against library roots (library-only policy).
	resolved, err := LibraryOnlyValidate(req.Path, s.libRoots())
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
		http.Error(w, "directory not found in index", http.StatusNotFound)
		return
	}

	tracks, err := s.db.GetDirectoryFiles(dir.ID)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if len(tracks) == 0 {
		http.Error(w, "no tracks found in directory", http.StatusUnprocessableEntity)
		return
	}
	if !canTranscodeTracks(tracks) {
		http.Error(w, "transcoding is available only for directories with lossless tracks", http.StatusUnprocessableEntity)
		return
	}

	preset, err := resolvePreset(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	outputMode, err := resolveOutputMode(req.OutputMode)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	files := make([]transcode.FileInfo, len(tracks))
	for i, t := range tracks {
		files[i] = transcode.FileInfo{
			Name:     t.Filename,
			Duration: t.Duration,
			Size:     t.Size,
		}
	}

	// Resolve the library root the same way LibraryOnlyValidate resolved SourceDir,
	// so filepath.Rel(LibraryRoot, SourceDir) in the transcode engine is comparing
	// two symlink-free absolute paths rather than a raw config path vs a resolved one.
	resolvedLibRoot, err := filepath.EvalSymlinks(lib.Path)
	if err != nil {
		resolvedLibRoot = filepath.Clean(lib.Path)
	}

	// Validate the output directory before starting the job.
	if outputMode != transcode.OutputReplace {
		var outDir string
		switch outputMode {
		case transcode.OutputShared:
			if s.cfg.OutputDir == "" {
				http.Error(w, "output dir not configured for shared mode", http.StatusUnprocessableEntity)
				return
			}
			outDir = filepath.Join(s.cfg.OutputDir, lib.Name, rel)
		case transcode.OutputLocal:
			outDir = filepath.Join(resolved, transcode.LocalOutputDirName)
		}
		if _, err := DestinationValidate(outDir, s.libRoots(), s.cfg.OutputDir); err != nil {
			WritePathError(w, err)
			return
		}
	}

	id := newJobID()
	job := transcode.Job{
		ID:          id,
		LibraryName: lib.Name,
		LibraryRoot: resolvedLibRoot,
		SourceDir:   resolved,
		Files:       files,
		Preset:      preset,
		OutputMode:  outputMode,
		OutputDir:   s.cfg.OutputDir,
	}

	title := lib.Name
	if rel != "" && rel != "." {
		title = lib.Name + "/" + rel
	}
	sub := fmt.Sprintf("%s → %s/%s", tracks[0].Codec, preset.Codec, preset.Name)

	js, started := s.jobs.start(id, resolved, job, title, sub)
	if !started {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":  "a transcode job is already running for this directory",
			"job_id": js.id,
		})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": id})
}

// handleJobs returns every job currently tracked (queued, running, or
// terminal until its 30-minute eviction), in queue order.
// GET /api/jobs
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.jobs.snapshotJobs()})
}

// handleJobEvents streams the global queue-panel event feed via SSE: it
// replays a snapshot of every currently tracked job as an initial burst of
// "update" events, then streams live deltas as they occur. The snapshot and
// subscription are registered atomically (subscribeEventsWithSnapshot), so no
// update landing between the two can be missed or duplicated.
// GET /api/jobs/events
func (s *Server) handleJobEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch, snapshot := s.jobs.subscribeEventsWithSnapshot()
	defer s.jobs.unsubscribeEvents(ch)

	for _, ev := range snapshot {
		writeJobEvent(w, ev)
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			writeJobEvent(w, ev)
			flusher.Flush()
		}
	}
}

// writeJobEvent writes a single "update" SSE event for a jobEvent.
func writeJobEvent(w http.ResponseWriter, ev jobEvent) {
	data, _ := json.Marshal(ev)
	_, _ = fmt.Fprintf(w, "event: update\ndata: %s\n\n", data)
}

// handleJobCancel cancels a queued or running job.
// POST /api/jobs/{id}/cancel
func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "job ID required", http.StatusBadRequest)
		return
	}

	switch s.jobs.cancel(id) {
	case cancelResultCanceled:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "canceled"})
	case cancelResultNotFound:
		http.Error(w, "job not found", http.StatusNotFound)
	case cancelResultFinished:
		http.Error(w, "job already finished", http.StatusConflict)
	}
}

// handleTranscodeLog returns the last N lines from the job's in-memory log ring buffer.
// GET /api/transcode/{jobID}/log[?n=N]
func (s *Server) handleTranscodeLog(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("jobID")
	if jobID == "" {
		http.Error(w, "job ID required", http.StatusBadRequest)
		return
	}

	js, ok := s.jobs.get(jobID)
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}

	lines := js.log.lines()

	// Optional ?n=N to limit lines returned.
	if nStr := r.URL.Query().Get("n"); nStr != "" {
		n, err := strconv.Atoi(nStr)
		if err != nil || n <= 0 {
			http.Error(w, "invalid n", http.StatusBadRequest)
			return
		}
		if n < len(lines) {
			lines = lines[len(lines)-n:]
		}
	}

	s.jobs.mu.Lock()
	status := js.status
	errMsg := js.errMsg
	s.jobs.mu.Unlock()

	resp := map[string]any{
		"job_id": jobID,
		"status": string(status),
		"lines":  lines,
	}
	if errMsg != "" {
		resp["error"] = errMsg
	}
	writeJSON(w, http.StatusOK, resp)
}

// calcOverallPercent converts a ProgressReport to an overall job percentage.
func calcOverallPercent(p transcode.ProgressReport) float64 {
	if p.TotalFiles == 0 {
		return 0
	}
	return (float64(p.FileIndex)*100 + p.FilePercent) / float64(p.TotalFiles)
}

// resolvePreset builds a Preset from a transcodeRequest.
// If req.Preset names a built-in preset, it is returned directly.
// Otherwise, custom fields are used to construct a preset.
func resolvePreset(req transcodeRequest) (transcode.Preset, error) {
	// Try named preset first. Named presets are fixed configurations; ExtraArgs
	// is intentionally not applied so callers cannot inject arbitrary ffmpeg flags.
	for _, p := range transcode.DefaultPresets() {
		if p.Name == req.Preset {
			return p, nil
		}
	}

	// If a preset name was given but didn't match any built-in preset, reject it.
	// "custom" is the sentinel value the UI sends when the user picks custom params.
	if req.Preset != "" && req.Preset != "custom" {
		return transcode.Preset{}, fmt.Errorf("unknown preset %q", req.Preset)
	}

	// Build custom preset from codec + fields.
	codec := transcode.Codec(req.Codec)
	switch codec {
	case transcode.CodecFLAC, transcode.CodecOpus:
	default:
		return transcode.Preset{}, fmt.Errorf("unknown codec %q; must be \"flac\" or \"opus\"", req.Codec)
	}

	p := transcode.Preset{
		Name:         "custom",
		Codec:        codec,
		CopyMetadata: true,
		CopyCover:    codec == transcode.CodecFLAC,
	}
	if req.CompressionLevel != nil {
		p.CompressionLevel = *req.CompressionLevel
	} else if codec == transcode.CodecOpus {
		p.CompressionLevel = 10
	}
	if req.Bitrate != "" {
		if !bitrateRe.MatchString(req.Bitrate) {
			return transcode.Preset{}, fmt.Errorf("invalid bitrate %q; must be digits optionally followed by 'k'", req.Bitrate)
		}
		p.Bitrate = req.Bitrate
	} else if codec == transcode.CodecOpus {
		p.Bitrate = "160k"
	}
	if req.CopyMetadata != nil {
		p.CopyMetadata = *req.CopyMetadata
	}
	if req.CopyCover != nil {
		p.CopyCover = *req.CopyCover
	}
	return p, nil
}

// resolveOutputMode maps a string to a transcode.OutputMode.
func resolveOutputMode(s string) (transcode.OutputMode, error) {
	switch transcode.OutputMode(s) {
	case transcode.OutputShared, transcode.OutputLocal, transcode.OutputReplace:
		return transcode.OutputMode(s), nil
	case "":
		return transcode.OutputShared, nil // default
	default:
		return "", fmt.Errorf("unknown output_mode %q; must be \"shared\", \"local\", or \"replace\"", s)
	}
}

// newJobID returns a random 8-byte hex job identifier.
func newJobID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
