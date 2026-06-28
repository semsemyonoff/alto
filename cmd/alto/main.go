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
	"strings"
	"syscall"

	"github.com/semsemyonoff/ALTO/internal/db"
	"github.com/semsemyonoff/ALTO/internal/library"
	"github.com/semsemyonoff/ALTO/internal/server"
	"github.com/semsemyonoff/ALTO/internal/transcode"
)

// Config holds all runtime configuration parsed from environment variables.
type Config struct {
	Libraries  []Library
	Port       string
	OutputDir  string
	DBPath     string
	CacheDir   string
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

	srvCfg := server.Config{
		Libraries: libCfgs,
		OutputDir: cfg.OutputDir,
		CacheDir:  cfg.CacheDir,
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
	srv.RunInitialScan()

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
