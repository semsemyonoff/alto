package library

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/semsemyonoff/ALTO/internal/db"
	"github.com/semsemyonoff/ALTO/internal/transcode"
)

// audioExtensions is the set of file extensions recognised as audio tracks.
var audioExtensions = map[string]bool{
	".flac": true,
	".opus": true,
	".ogg":  true,
	".mp3":  true,
	".wav":  true,
	".aac":  true,
	".m4a":  true,
	".wma":  true,
	".alac": true,
	".ape":  true,
	".wv":   true,
}

// externalCoverNames is the ordered list of cover art filenames to look for.
var externalCoverNames = []string{
	"cover.jpg", "cover.png",
	"folder.jpg", "folder.png",
	"front.jpg", "front.png",
}

// ScanConfig provides runtime options for the Scanner.
type ScanConfig struct {
	// OutputDir is the resolved absolute path of ALTO_OUTPUT_DIR.
	// If it falls under any library root it will be excluded from scans.
	OutputDir string
	// CacheDir is the directory for app-managed files (extracted cover art).
	CacheDir string
}

// Scanner walks library directories, extracts metadata, and stores results in DB.
type Scanner struct {
	db     *db.DB
	prober Prober
	cfg    ScanConfig
}

// NewScanner constructs a Scanner with the given DB, prober, and config.
func NewScanner(database *db.DB, prober Prober, cfg ScanConfig) *Scanner {
	if prober == nil {
		prober = &FFProber{}
	}
	return &Scanner{db: database, prober: prober, cfg: cfg}
}

// ScanAll scans all provided libraries in parallel.
func (s *Scanner) ScanAll(ctx context.Context, libraries []db.Library) error {
	var wg sync.WaitGroup
	errs := make([]error, len(libraries))

	for i, lib := range libraries {
		wg.Add(1)
		go func(idx int, l db.Library) {
			defer wg.Done()
			if err := s.Scan(ctx, l); err != nil {
				errs[idx] = fmt.Errorf("library %q: %w", l.Name, err)
			}
		}(i, lib)
	}

	wg.Wait()

	return errors.Join(errs...)
}

// Scan walks a single library directory, extracts metadata, and syncs the DB.
func (s *Scanner) Scan(ctx context.Context, lib db.Library) error {
	slog.Info("scan started", "library", lib.Name, "path", lib.Path)

	resolvedOut, _ := filepath.EvalSymlinks(s.cfg.OutputDir)

	var audioPaths []string
	dirToFiles := make(map[string][]string)
	dirInfos := make(map[string]*dirScanResult)

	err := filepath.WalkDir(lib.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("walk error", "path", path, "err", err)
			return nil
		}
		if !d.IsDir() {
			return nil
		}

		// Skip app-owned directories.
		base := d.Name()
		if isAltoDir(base) {
			return filepath.SkipDir
		}

		// Skip if this directory is the resolved ALTO_OUTPUT_DIR or any subdirectory of it.
		if resolvedOut != "" {
			resolved, rerr := filepath.EvalSymlinks(path)
			if rerr == nil && (resolved == resolvedOut || strings.HasPrefix(resolved, resolvedOut+string(filepath.Separator))) {
				return filepath.SkipDir
			}
		}

		// Also reject any path segment containing .alto-* to avoid descending
		// into nested app dirs.
		if containsAltoSegment(path, lib.Path) {
			return filepath.SkipDir
		}

		// List audio files in this directory.
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			slog.Warn("readdir error", "path", path, "err", readErr)
			return nil
		}

		var audioFiles []string
		for _, e := range entries {
			if e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if audioExtensions[ext] {
				audioFiles = append(audioFiles, e.Name())
			}
		}

		if len(audioFiles) == 0 {
			return nil
		}

		// Compute relative path from library root.
		rel, relErr := filepath.Rel(lib.Path, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// Normalize "." (library root itself) to "" so it matches the convention
		// used by findLibraryForPath in the server, which also normalizes to "".
		if rel == "." {
			rel = ""
		}

		audioPaths = append(audioPaths, rel)
		dirToFiles[rel] = audioFiles
		dirInfos[rel] = &dirScanResult{absPath: path, entries: entries}

		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %q: %w", lib.Path, err)
	}

	indexedSet := make(map[string]struct{}, len(audioPaths))
	for _, rel := range audioPaths {
		indexedSet[rel] = struct{}{}
		for _, parent := range ancestorPaths(rel) {
			indexedSet[parent] = struct{}{}
		}
	}

	indexedPaths := make([]string, 0, len(indexedSet))
	parentOnlyPaths := make([]string, 0, len(indexedSet))
	for rel := range indexedSet {
		indexedPaths = append(indexedPaths, rel)
		if _, ok := dirInfos[rel]; !ok {
			parentOnlyPaths = append(parentOnlyPaths, rel)
		}
	}
	sort.Strings(indexedPaths)
	sort.Strings(parentOnlyPaths)
	sort.Strings(audioPaths)

	// Upsert parent directories that only exist to keep nested audio branches visible.
	for _, rel := range parentOnlyPaths {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		dirID, upsertErr := s.db.UpsertDirectoryWithAudioFlag(lib.ID, rel, "", false, "", false)
		if upsertErr != nil {
			slog.Warn("upsert parent directory", "path", rel, "err", upsertErr)
			continue
		}

		if deleteErr := s.db.DeleteStaleFiles(dirID, nil); deleteErr != nil {
			slog.Warn("delete stale files", "dir", rel, "err", deleteErr)
		}
	}

	// Upsert each discovered audio directory.
	for _, rel := range audioPaths {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		info := dirInfos[rel]
		audioFiles := dirToFiles[rel]

		coverPath, hasCover := s.resolveCover(ctx, info.absPath, audioFiles, lib.ID, rel)
		tracks := s.probeFiles(ctx, info.absPath, audioFiles)
		codecSummary := buildCodecSummary(tracks)

		dirID, upsertErr := s.db.UpsertDirectoryWithAudioFlag(lib.ID, rel, codecSummary, hasCover, coverPath, true)
		if upsertErr != nil {
			slog.Warn("upsert directory", "path", rel, "err", upsertErr)
			continue
		}

		for _, t := range tracks {
			t.DirectoryID = dirID
			if upsertErr := s.db.UpsertTrack(t); upsertErr != nil {
				slog.Warn("upsert track", "file", t.Filename, "err", upsertErr)
			}
		}

		if deleteErr := s.db.DeleteStaleFiles(dirID, audioFiles); deleteErr != nil {
			slog.Warn("delete stale files", "dir", rel, "err", deleteErr)
		}
	}

	// Remove directories no longer on disk.
	if deleteErr := s.db.DeleteStaleDirectories(lib.ID, indexedPaths); deleteErr != nil {
		slog.Warn("delete stale directories", "library", lib.Name, "err", deleteErr)
	}

	slog.Info("scan complete", "library", lib.Name, "directories", len(indexedPaths), "audio_directories", len(audioPaths))
	return nil
}

// dirScanResult holds pre-read info about a directory.
type dirScanResult struct {
	absPath string
	entries []fs.DirEntry
}

// ancestorPaths returns the slash-normalized ancestors of rel, excluding rel itself.
func ancestorPaths(rel string) []string {
	if rel == "" {
		return nil
	}

	parts := strings.Split(rel, "/")
	if len(parts) <= 1 {
		return nil
	}

	ancestors := make([]string, 0, len(parts)-1)
	for i := 1; i < len(parts); i++ {
		ancestors = append(ancestors, strings.Join(parts[:i], "/"))
	}
	return ancestors
}

// isAltoDir returns true if the directory name is an app-owned dir.
func isAltoDir(name string) bool {
	return name == transcode.LocalOutputDirName || name == ".alto-out" || strings.HasPrefix(name, ".alto-")
}

// containsAltoSegment returns true if any path segment (below libRoot) is app-owned.
func containsAltoSegment(path, libRoot string) bool {
	rel, err := filepath.Rel(libRoot, path)
	if err != nil {
		return false
	}
	for seg := range strings.SplitSeq(rel, string(filepath.Separator)) {
		if isAltoDir(seg) {
			return true
		}
	}
	return false
}

// resolveCover returns the cover art path and whether cover art was found.
// It checks external files first; if none, it tries embedded art extraction.
func (s *Scanner) resolveCover(ctx context.Context, dirPath string, audioFiles []string, libID int64, relPath string) (string, bool) {
	// Check for external cover art files. Use Lstat to reject symlinks — following
	// symlinks here would allow a crafted cover.jpg -> /etc/passwd to be indexed
	// and later served through /api/cover.
	for _, name := range externalCoverNames {
		candidate := filepath.Join(dirPath, name)
		if fi, err := os.Lstat(candidate); err == nil && fi.Mode().IsRegular() {
			return candidate, true
		}
	}

	// Fall back to embedded art extraction.
	if len(audioFiles) == 0 {
		return "", false
	}
	src := filepath.Join(dirPath, audioFiles[0])
	info, err := s.prober.Probe(ctx, src)
	if err != nil || !info.HasCover {
		return "", false
	}

	// Extract embedded cover art to cache.
	cacheDir := s.cfg.CacheDir
	if cacheDir == "" {
		cacheDir = "./cache"
	}
	hash := sha256.Sum256(fmt.Appendf(nil, "%d/%s", libID, relPath))
	cacheFile := filepath.Join(cacheDir, "covers", fmt.Sprintf("%d", libID), fmt.Sprintf("%x.jpg", hash))

	// If already cached, return immediately.
	if _, err := os.Stat(cacheFile); err == nil {
		return cacheFile, true
	}

	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		slog.Warn("create cover cache dir", "err", err)
		return "", false
	}

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-i", src,
		"-an",
		"-vcodec", "mjpeg",
		"-frames:v", "1",
		"-y",
		cacheFile,
	)
	if err := cmd.Run(); err != nil {
		slog.Warn("extract embedded cover", "src", src, "err", err)
		return "", false
	}
	return cacheFile, true
}

// probeFiles runs ffprobe on each audio file and returns Track records.
func (s *Scanner) probeFiles(ctx context.Context, dirPath string, audioFiles []string) []db.Track {
	tracks := make([]db.Track, 0, len(audioFiles))
	for _, name := range audioFiles {
		fullPath := filepath.Join(dirPath, name)
		fi, err := os.Stat(fullPath)
		if err != nil {
			slog.Warn("stat audio file", "path", fullPath, "err", err)
			continue
		}

		t := db.Track{
			Filename: name,
			Size:     fi.Size(),
		}

		info, probeErr := s.prober.Probe(ctx, fullPath)
		if probeErr != nil {
			slog.Warn("ffprobe", "file", fullPath, "err", probeErr)
		} else {
			t.Codec = info.Codec
			t.Bitrate = info.Bitrate
			t.Duration = info.Duration
			t.SampleRate = info.SampleRate
			t.Channels = info.Channels
		}
		tracks = append(tracks, t)
	}
	return tracks
}

// buildCodecSummary returns a human-readable codec summary for a directory.
// "FLAC" if all tracks are FLAC, "Opus" if all Opus, etc., or "Mixed" if multiple codecs.
func buildCodecSummary(tracks []db.Track) string {
	if len(tracks) == 0 {
		return ""
	}
	codecs := make(map[string]bool)
	for _, t := range tracks {
		if t.Codec != "" {
			codecs[strings.ToUpper(t.Codec)] = true
		}
	}
	if len(codecs) == 0 {
		return ""
	}
	if len(codecs) == 1 {
		for c := range codecs {
			return c
		}
	}
	return "Mixed"
}
