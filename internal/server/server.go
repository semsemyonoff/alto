package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/semsemyonoff/ALTO/internal/db"
	"github.com/semsemyonoff/ALTO/internal/transcode"
)

// LibraryScanner is the interface for scanning libraries into the database.
// If progress is non-nil, implementations invoke it as audio directories are
// discovered, reporting the running per-library discovered-directory count.
type LibraryScanner interface {
	ScanAll(ctx context.Context, libraries []db.Library, progress func(libraryID int64, discoveredDirs int)) error
}

// TranscodeEngine is the interface for running transcoding jobs.
type TranscodeEngine interface {
	Transcode(ctx context.Context, job transcode.Job, progress chan<- transcode.ProgressReport) error
}

// LibraryConfig holds runtime configuration for a single library.
type LibraryConfig struct {
	ID   int64
	Name string
	Path string
}

// Config is the server configuration.
type Config struct {
	Libraries     []LibraryConfig
	OutputDir     string
	CacheDir      string
	TemplateDir   string // defaults to "web/templates"
	StaticDir     string // defaults to "web/static"
	Workers       int    // transcode worker pool size; defaults to 1
	FFmpegVersion string // detected at startup (transcode.FFmpegVersion); shown in the header
}

// ScanEvent represents a scan lifecycle event broadcast over SSE.
type ScanEvent struct {
	Type       string `json:"type"`              // "started", "progress", "complete", "error", "idle"
	Message    string `json:"message,omitempty"` // error message if Type == "error"
	Added      int    `json:"added,omitempty"`
	Removed    int    `json:"removed,omitempty"`
	Discovered int    `json:"discovered,omitempty"` // running total of directories discovered so far, Type == "progress"
}

// scanState manages scan lifecycle and SSE subscriptions under a single mutex.
type scanState struct {
	mu      sync.Mutex
	running bool
	subs    []chan ScanEvent
}

// start attempts to mark a scan as running. Returns false if already running.
func (ss *scanState) start() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.running {
		return false
	}
	ss.running = true
	return true
}

// subscribe adds a new subscriber and returns (channel, isRunning).
// If isRunning is false the caller should send an idle event instead of blocking.
func (ss *scanState) subscribe() (chan ScanEvent, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ch := make(chan ScanEvent, 16)
	if !ss.running {
		return ch, false
	}
	ss.subs = append(ss.subs, ch)
	return ch, true
}

// unsubscribe removes a subscriber (e.g. on client disconnect).
func (ss *scanState) unsubscribe(ch chan ScanEvent) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for i, sub := range ss.subs {
		if sub == ch {
			ss.subs = append(ss.subs[:i], ss.subs[i+1:]...)
			return
		}
	}
}

// reset clears the running flag without broadcasting any event.
// Use this when a scan never actually started (e.g. validation error).
func (ss *scanState) reset() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.running = false
}

// broadcast sends an event to all subscribers.
// On "complete" or "error", closes all subscriber channels and marks the scan done.
//
// Progress events use a non-blocking send and may be dropped for a slow
// subscriber. Terminal events ("complete"/"error") must never be dropped —
// otherwise the client sees the stream close with no completion message and
// reports a connection error — so they are delivered via sendScanTerminal,
// which evicts buffered (superseded) progress events to make room.
func (ss *scanState) broadcast(e ScanEvent) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	terminal := e.Type == "complete" || e.Type == "error"
	for _, ch := range ss.subs {
		if terminal {
			sendScanTerminal(ch, e)
			continue
		}
		select {
		case ch <- e:
		default:
		}
	}
	if terminal {
		for _, ch := range ss.subs {
			close(ch)
		}
		ss.subs = nil
		ss.running = false
	}
}

// sendScanTerminal delivers a terminal event to ch without blocking. When the
// buffer is full of superseded progress events it evicts one and retries; the
// broadcaster is the sole sender, so a slot always frees up and this returns
// after at most cap(ch)+1 iterations.
func sendScanTerminal(ch chan ScanEvent, e ScanEvent) {
	for {
		select {
		case ch <- e:
			return
		default:
		}
		// Buffer full: drop one buffered progress event to make room, then retry.
		select {
		case <-ch:
		default:
		}
	}
}

// Server is the ALTO HTTP server.
type Server struct {
	db             *db.DB
	scanner        LibraryScanner
	engine         TranscodeEngine
	cfg            Config
	mux            *http.ServeMux
	scan           scanState
	tmpl           templateEngine
	jobs           *jobManager
	staticDir      string
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

// New creates a new Server and registers all routes.
func New(database *db.DB, scanner LibraryScanner, cfg Config) *Server {
	return NewWithEngine(database, scanner, nil, cfg)
}

// NewWithEngine creates a new Server with an optional TranscodeEngine.
func NewWithEngine(database *db.DB, scanner LibraryScanner, engine TranscodeEngine, cfg Config) *Server {
	tmplDir := cfg.TemplateDir
	if tmplDir == "" {
		tmplDir = "web/templates"
	}
	staticDir := cfg.StaticDir
	if staticDir == "" {
		staticDir = "web/static"
	}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	s := &Server{
		db:             database,
		scanner:        scanner,
		engine:         engine,
		cfg:            cfg,
		mux:            http.NewServeMux(),
		tmpl:           templateEngine{dir: tmplDir, assets: newAssetResolver(staticDir)},
		jobs:           newJobManager(engine, cfg.Workers, shutdownCtx),
		staticDir:      staticDir,
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}
	s.registerRoutes()
	return s
}

// Shutdown cancels all in-flight transcode jobs and releases server resources.
// It blocks until all worker goroutines have exited.
func (s *Server) Shutdown() {
	s.shutdownCancel()
	s.jobs.Shutdown()
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// RunInitialScan starts a background scan of all configured libraries through
// the scan state machine so it is visible to concurrent scan requests.
// If a scan is already running, this call is a no-op.
func (s *Server) RunInitialScan() {
	libs := make([]db.Library, 0, len(s.cfg.Libraries))
	for _, l := range s.cfg.Libraries {
		libs = append(libs, db.Library{ID: l.ID, Name: l.Name, Path: l.Path})
	}
	if len(libs) == 0 || !s.scan.start() {
		return
	}
	s.launchScan(libs)
}

// launchScan starts the scan goroutine. The caller must have already called
// s.scan.start() successfully.
func (s *Server) launchScan(libs []db.Library) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("scan panicked", "panic", r)
				s.scan.broadcast(ScanEvent{Type: "error", Message: "internal error"})
			}
		}()

		before, err := s.snapshotDirectoryPaths(libs)
		if err != nil {
			slog.Error("scan snapshot before", "err", err)
			s.scan.broadcast(ScanEvent{Type: "error", Message: "failed to prepare re-index"})
			return
		}

		s.scan.broadcast(ScanEvent{Type: "started"})

		var progressMu sync.Mutex
		discoveredByLib := make(map[int64]int, len(libs))
		progress := func(libraryID int64, discoveredDirs int) {
			progressMu.Lock()
			discoveredByLib[libraryID] = discoveredDirs
			total := 0
			for _, n := range discoveredByLib {
				total += n
			}
			progressMu.Unlock()
			s.scan.broadcast(ScanEvent{Type: "progress", Discovered: total})
		}

		if err := s.scanner.ScanAll(s.shutdownCtx, libs, progress); err != nil {
			slog.Error("scan failed", "err", err)
			s.scan.broadcast(ScanEvent{Type: "error", Message: err.Error()})
		} else {
			after, snapshotErr := s.snapshotDirectoryPaths(libs)
			if snapshotErr != nil {
				slog.Error("scan snapshot after", "err", snapshotErr)
				s.scan.broadcast(ScanEvent{Type: "error", Message: "failed to summarize re-index"})
				return
			}

			added, removed := diffDirectorySnapshots(before, after)
			s.scan.broadcast(ScanEvent{
				Type:    "complete",
				Message: fmt.Sprintf("Re-index complete. Added: %d, removed: %d.", added, removed),
				Added:   added,
				Removed: removed,
			})
		}
	}()
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /dir", s.handleDirPage)
	s.mux.HandleFunc("GET /api/libraries", s.handleLibraries)
	s.mux.HandleFunc("GET /api/tree/{libraryID}", s.handleTree)
	s.mux.HandleFunc("GET /api/tree/{libraryID}/children", s.handleTreeChildren)
	s.mux.HandleFunc("GET /api/tree/{libraryID}/search", s.handleTreeSearch)
	s.mux.HandleFunc("GET /api/dir", s.handleDir)
	s.mux.HandleFunc("POST /api/scan", s.handleScan)
	s.mux.HandleFunc("GET /api/scan/status", s.handleScanStatus)
	s.mux.HandleFunc("GET /api/cover", s.handleCover)
	s.mux.HandleFunc("GET /api/jobs", s.handleJobs)
	s.mux.HandleFunc("GET /api/jobs/events", s.handleJobEvents)
	s.mux.HandleFunc("POST /api/jobs/{id}/cancel", s.handleJobCancel)
	s.mux.HandleFunc("POST /api/jobs/{id}/remove", s.handleJobRemove)
	s.mux.HandleFunc("GET /api/version", s.handleVersion)
	s.mux.HandleFunc("POST /api/transcode", s.handleTranscodeStart)
	s.mux.HandleFunc("GET /api/transcode/{jobID}/log", s.handleTranscodeLog)
	s.mux.HandleFunc("GET /{path...}", s.handleNotFoundPage)

	s.mux.Handle("GET /favicon.ico", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(s.staticDir, "logo.svg"))
	}))

	// Static file serving supports GET and HEAD through the GET pattern.
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(s.staticDir))))
}

func (s *Server) handleNotFoundPage(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	redirectToIndexWithNotice(w, r, noticeDirectoryNotFound)
}

func (s *Server) snapshotDirectoryPaths(libs []db.Library) (map[int64]map[string]struct{}, error) {
	snapshot := make(map[int64]map[string]struct{}, len(libs))
	for _, lib := range libs {
		dirs, err := s.db.GetDirectoryTree(lib.ID)
		if err != nil {
			return nil, err
		}

		paths := make(map[string]struct{}, len(dirs))
		for _, dir := range dirs {
			paths[dir.Path] = struct{}{}
		}
		snapshot[lib.ID] = paths
	}
	return snapshot, nil
}

func diffDirectorySnapshots(before, after map[int64]map[string]struct{}) (int, int) {
	var added, removed int

	for libID, afterPaths := range after {
		beforePaths := before[libID]
		for path := range afterPaths {
			if _, ok := beforePaths[path]; !ok {
				added++
			}
		}
	}

	for libID, beforePaths := range before {
		afterPaths := after[libID]
		for path := range beforePaths {
			if _, ok := afterPaths[path]; !ok {
				removed++
			}
		}
	}

	return added, removed
}

// libRoots returns the root paths of all configured libraries.
func (s *Server) libRoots() []string {
	roots := make([]string, len(s.cfg.Libraries))
	for i, l := range s.cfg.Libraries {
		roots[i] = l.Path
	}
	return roots
}

// findLibraryForPath returns the LibraryConfig whose resolved root contains resolvedPath,
// along with the relative path within that library (slash-separated, "" for root).
func (s *Server) findLibraryForPath(resolvedPath string) (LibraryConfig, string, bool) {
	for _, lib := range s.cfg.Libraries {
		resolvedRoot, err := filepath.EvalSymlinks(lib.Path)
		if err != nil {
			resolvedRoot = filepath.Clean(lib.Path)
		}
		if isWithin(resolvedPath, resolvedRoot) {
			rel, err := filepath.Rel(resolvedRoot, resolvedPath)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			if rel == "." {
				rel = ""
			}
			return lib, rel, true
		}
	}
	return LibraryConfig{}, "", false
}
