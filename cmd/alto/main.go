package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/semsemyonoff/ALTO/internal/db"
	"github.com/semsemyonoff/ALTO/internal/library"
	"github.com/semsemyonoff/ALTO/internal/server"
	"github.com/semsemyonoff/ALTO/internal/transcode"
)

// Config holds all runtime configuration parsed from environment variables.
type Config struct {
	Libraries   []Library
	Port        string
	OutputDir   string
	DBPath      string
	CacheDir    string
	Workers     int
	ScanOnStart bool
}

// Library represents a named, mounted music library.
type Library struct {
	Name string
	Path string
}

var libraryNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// ParseConfig reads and validates configuration from environment variables.
// Returns an error if required variables are missing or invalid.
func ParseConfig() (*Config, error) {
	cfg := &Config{
		Port:      getEnvDefault("ALTO_PORT", "8080"),
		OutputDir: getEnvDefault("ALTO_OUTPUT_DIR", "/out"),
		DBPath:    getEnvDefault("ALTO_DB_PATH", "./alto.db"),
		CacheDir:  getEnvDefault("ALTO_CACHE_DIR", "./cache"),
	}

	libs, err := parseLibraries(os.Getenv("ALTO_LIBRARIES"))
	if err != nil {
		return nil, err
	}
	cfg.Libraries = libs

	workers, err := getEnvIntDefault("ALTO_TRANSCODE_WORKERS", 1)
	if err != nil {
		return nil, err
	}
	if workers < 1 {
		workers = 1
	}
	cfg.Workers = workers

	scanOnStart, err := getEnvBoolDefault("ALTO_SCAN_ON_START", true)
	if err != nil {
		return nil, err
	}
	cfg.ScanOnStart = scanOnStart

	return cfg, nil
}

// parseLibraries parses the ALTO_LIBRARIES env value into Library entries.
// Format: "name:path,name2:path2"
func parseLibraries(raw string) ([]Library, error) {
	if raw == "" {
		return nil, fmt.Errorf("ALTO_LIBRARIES is required (format: name:path[,name:path...])")
	}

	entries := strings.Split(raw, ",")
	libs := make([]Library, 0, len(entries))
	seenNames := make(map[string]bool)
	seenPaths := make(map[string]bool)

	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid library entry %q: expected name:path format", entry)
		}
		name := strings.TrimSpace(parts[0])
		path := strings.TrimSpace(parts[1])

		if name == "" {
			return nil, fmt.Errorf("library name cannot be empty in entry %q", entry)
		}
		if !libraryNameRe.MatchString(name) {
			return nil, fmt.Errorf("library name %q contains invalid characters (allowed: a-z, A-Z, 0-9, _, -)", name)
		}
		if path == "" {
			return nil, fmt.Errorf("library path cannot be empty for library %q", name)
		}
		if seenNames[name] {
			return nil, fmt.Errorf("duplicate library name %q", name)
		}
		if seenPaths[path] {
			return nil, fmt.Errorf("duplicate library path %q", path)
		}
		seenNames[name] = true
		seenPaths[path] = true

		libs = append(libs, Library{Name: name, Path: path})
	}

	if len(libs) == 0 {
		return nil, fmt.Errorf("ALTO_LIBRARIES is required (format: name:path[,name:path...])")
	}

	return libs, nil
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getEnvIntDefault reads an integer env var, returning def if unset.
// Returns an error if the value is set but not a valid integer.
func getEnvIntDefault(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: must be an integer", key, v)
	}
	return n, nil
}

// getEnvBoolDefault reads a boolean env var, returning def if unset.
// Returns an error if the value is set but not a valid boolean.
func getEnvBoolDefault(key string, def bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return false, fmt.Errorf("invalid %s %q: must be a boolean (1/0, true/false)", key, v)
	}
	return b, nil
}

func main() {
	cfg, err := ParseConfig()
	if err != nil {
		slog.Error("configuration error", "err", err)
		os.Exit(1)
	}

	// Warn if ALTO_OUTPUT_DIR resolves inside a library root.
	outResolved, err := filepath.Abs(cfg.OutputDir)
	if err == nil {
		for _, lib := range cfg.Libraries {
			libResolved, lerr := filepath.Abs(lib.Path)
			if lerr != nil {
				continue
			}
			if strings.HasPrefix(outResolved, libResolved+string(filepath.Separator)) || outResolved == libResolved {
				slog.Warn("ALTO_OUTPUT_DIR is inside a library root; output directory will be excluded from scans",
					"output_dir", cfg.OutputDir,
					"library", lib.Name,
					"library_path", lib.Path,
				)
			}
		}
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		slog.Error("open database", "err", err)
		os.Exit(1)
	}
	defer func() { _ = database.Close() }()

	// Upsert all configured libraries and collect their server configs.
	libCfgs := make([]server.LibraryConfig, 0, len(cfg.Libraries))
	for _, lib := range cfg.Libraries {
		id, err := database.UpsertLibrary(lib.Name, lib.Path)
		if err != nil {
			slog.Error("upsert library", "name", lib.Name, "err", err)
			os.Exit(1)
		}
		libCfgs = append(libCfgs, server.LibraryConfig{ID: id, Name: lib.Name, Path: lib.Path})
	}

	scanner := library.NewScanner(database, nil, library.ScanConfig{
		OutputDir: cfg.OutputDir,
		CacheDir:  cfg.CacheDir,
	})
	engine := transcode.NewEngine()

	// Detect the ffmpeg version once at startup (ffmpeg is ALTO's core tool) so
	// the UI can display it. A failure here is non-fatal — the badge is simply
	// omitted.
	ffmpegVersion := ""
	if v, err := detectFFmpegVersion(); err != nil {
		slog.Warn("ffmpeg version detection failed", "err", err)
	} else {
		ffmpegVersion = v
		slog.Info("ffmpeg detected", "version", v)
	}

	srvCfg := server.Config{
		Libraries:     libCfgs,
		OutputDir:     cfg.OutputDir,
		CacheDir:      cfg.CacheDir,
		Workers:       cfg.Workers,
		FFmpegVersion: ffmpegVersion,
	}
	srv := server.NewWithEngine(database, scanner, engine, srvCfg)

	// Add a health endpoint alongside the main server.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.Handle("/", srv)

	// Kick off an initial background scan so the UI is populated on first start.
	// Disabled via ALTO_SCAN_ON_START=false for large libraries where a full
	// re-index on every restart is too expensive; the UI's re-index button and
	// POST /api/scan still work.
	if cfg.ScanOnStart {
		srv.RunInitialScan()
	} else {
		slog.Info("initial scan skipped", "reason", "ALTO_SCAN_ON_START=false")
	}

	addr := ":" + cfg.Port
	slog.Info("starting ALTO", "addr", addr, "libraries", len(cfg.Libraries))

	httpSrv := &http.Server{Addr: addr, Handler: mux}

	// Handle SIGINT/SIGTERM: stop accepting new connections, cancel in-flight jobs.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-sigCtx.Done()
		stop()
		slog.Info("shutting down")
		srv.Shutdown()
		_ = httpSrv.Shutdown(context.Background())
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

// detectFFmpegVersion runs `ffmpeg -version` under a short timeout and returns
// the reported version token. ffmpeg is ALTO's core tool; the result is shown
// in the UI header.
func detectFFmpegVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return transcode.FFmpegVersion(ctx)
}
