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
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

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
	// Workers caps the number of concurrent probes across all libraries.
	// Values <= 0 mean DefaultScanWorkers().
	Workers int
}

// DefaultScanWorkers returns the probe concurrency used when none is configured.
// Probing is process-spawn bound, so a small multiple of the available cores is
// enough to keep them busy without flooding the machine with ffprobe children.
func DefaultScanWorkers() int {
	return min(4, runtime.NumCPU())
}

// Scanner walks library directories, extracts metadata, and stores results in DB.
type Scanner struct {
	db     *db.DB
	prober Prober
	cfg    ScanConfig
	// sem bounds concurrent probes; it is shared by every library scanned by
	// this Scanner, so ScanAll cannot multiply the configured worker count.
	sem chan struct{}
}

// NewScanner constructs a Scanner with the given DB, prober, and config.
func NewScanner(database *db.DB, prober Prober, cfg ScanConfig) *Scanner {
	if prober == nil {
		prober = &FFProber{}
	}
	if cfg.Workers <= 0 {
		cfg.Workers = DefaultScanWorkers()
	}
	return &Scanner{
		db:     database,
		prober: prober,
		cfg:    cfg,
		sem:    make(chan struct{}, cfg.Workers),
	}
}

// ScanAll scans all provided libraries in parallel. If progress is non-nil, it is
// invoked from each library's goroutine as audio directories are discovered,
// with the running count of directories discovered so far in that library.
func (s *Scanner) ScanAll(ctx context.Context, libraries []db.Library, progress func(libraryID int64, discoveredDirs int)) error {
	var wg sync.WaitGroup
	errs := make([]error, len(libraries))

	for i, lib := range libraries {
		wg.Add(1)
		go func(idx int, l db.Library) {
			defer wg.Done()
			var libProgress func(int)
			if progress != nil {
				libProgress = func(discoveredDirs int) { progress(l.ID, discoveredDirs) }
			}
			if err := s.scan(ctx, l, libProgress); err != nil {
				errs[idx] = fmt.Errorf("library %q: %w", l.Name, err)
			}
		}(i, lib)
	}

	wg.Wait()

	return errors.Join(errs...)
}

// Scan walks a single library directory, extracts metadata, and syncs the DB.
func (s *Scanner) Scan(ctx context.Context, lib db.Library) error {
	return s.scan(ctx, lib, nil)
}

// scan is the shared implementation behind Scan and ScanAll. If progress is
// non-nil, it is called with the running count of discovered audio directories.
func (s *Scanner) scan(ctx context.Context, lib db.Library, progress func(discoveredDirs int)) error {
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

		if progress != nil {
			progress(len(audioPaths))
		}

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

		if err := s.indexParentDir(lib.ID, rel); err != nil {
			continue
		}
	}

	// Upsert each discovered audio directory.
	for _, rel := range audioPaths {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err := s.indexAudioDir(ctx, lib, rel, dirInfos[rel].absPath, dirToFiles[rel]); err != nil {
			// A cancellation aborts the whole scan; a per-directory failure has
			// already been logged and only costs that directory.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
	}

	// Remove directories no longer on disk.
	if deleteErr := s.db.DeleteStaleDirectories(lib.ID, indexedPaths); deleteErr != nil {
		slog.Warn("delete stale directories", "library", lib.Name, "err", deleteErr)
	}

	slog.Info("scan complete", "library", lib.Name, "directories", len(indexedPaths), "audio_directories", len(audioPaths))
	return nil
}

// indexParentDir writes the directory row of a structural parent — a directory
// that holds no audio itself but must exist for the tree to reach the ones that do.
func (s *Scanner) indexParentDir(libID int64, rel string) error {
	dirID, err := s.db.UpsertDirectoryWithAudioFlag(libID, rel, "", false, "", false)
	if err != nil {
		slog.Warn("upsert parent directory", "path", rel, "err", err)
		return err
	}

	if deleteErr := s.db.DeleteStaleFiles(dirID, nil); deleteErr != nil {
		slog.Warn("delete stale files", "dir", rel, "err", deleteErr)
	}
	return nil
}

// indexAudioDir indexes one directory holding audio: it probes the files
// (reusing the stored rows whose (size, mtime) still match), resolves the cover
// and writes the directory row, its tracks and its stale-file cleanup. rel is the
// slash-separated path relative to the library root, absPath its location on disk.
func (s *Scanner) indexAudioDir(ctx context.Context, lib db.Library, rel, absPath string, audioFiles []string) error {
	// Stored rows of this directory, keyed by filename: probeFiles reuses the
	// ones whose (size, mtime) still match on disk instead of re-probing.
	cached, cacheErr := s.db.GetTracksByDirPath(lib.ID, rel)
	if cacheErr != nil {
		slog.Warn("read cached tracks", "dir", rel, "err", cacheErr)
		cached = nil
	}

	// Probe before resolving the cover: resolveCover reads the embedded-art
	// flag off an already-probed track instead of probing the first file again.
	tracks, allCached := s.probeFiles(ctx, absPath, audioFiles, cached)

	// A cancellation during probing leaves tracks incomplete — writing it
	// would replace an already-indexed directory with zeroed metadata.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	coverPath, hasCover := s.resolveCover(ctx, absPath, tracks, lib.ID, rel)
	codecSummary := buildCodecSummary(tracks)

	dirID, upsertErr := s.db.UpsertDirectoryWithAudioFlag(lib.ID, rel, codecSummary, hasCover, coverPath, true)
	if upsertErr != nil {
		slog.Warn("upsert directory", "path", rel, "err", upsertErr)
		return upsertErr
	}

	// When every file was a cache hit and the on-disk name set is the stored
	// one, the rows to write are identical to the rows already there. Skipping
	// that transaction is what makes a warm rescan cheap; the directory row and
	// the stale-file cleanup still run unconditionally, being one statement each.
	if !allCached || !tracksMatchCache(tracks, cached) {
		// One transaction per directory: a failure rolls back this directory's
		// tracks, so warn once and move on rather than aborting the scan.
		if writeErr := s.db.UpsertTracks(dirID, tracks); writeErr != nil {
			slog.Warn("upsert tracks", "dir", rel, "tracks", len(tracks), "err", writeErr)
		}
	}

	if deleteErr := s.db.DeleteStaleFiles(dirID, audioFiles); deleteErr != nil {
		slog.Warn("delete stale files", "dir", rel, "err", deleteErr)
	}
	return nil
}

// ErrDirExcluded reports a directory a scan would never index: an app-owned
// .alto-* path, the configured output directory, or a path outside the library.
var ErrDirExcluded = errors.New("directory excluded from scanning")

// ScanDir indexes exactly one directory of lib, identified by its path relative
// to the library root ("" is the root itself). It applies the same exclusions,
// cache reuse and cover resolution a full scan applies to that directory, and
// creates the ancestor rows the tree needs to reach it.
//
// It deliberately never calls DeleteStaleDirectories: with a single path the
// call would delete the index of every other directory in the library.
func (s *Scanner) ScanDir(ctx context.Context, lib db.Library, relPath string) error {
	rel := normalizeRelPath(relPath)
	absPath := lib.Path
	if rel != "" {
		absPath = filepath.Join(lib.Path, filepath.FromSlash(rel))
	}

	if err := s.checkScannable(lib, rel, absPath); err != nil {
		return err
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return fmt.Errorf("read directory %q: %w", absPath, err)
	}

	var audioFiles []string
	for _, e := range entries {
		if e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if audioExtensions[strings.ToLower(filepath.Ext(e.Name()))] {
			audioFiles = append(audioFiles, e.Name())
		}
	}

	if len(audioFiles) == 0 {
		// A full scan would not index this directory at all, so do not invent a
		// row for it. An existing row means its files are gone: keep the row (the
		// tree may hang off it) but drop the tracks it no longer has.
		existing, lookupErr := s.db.GetDirectoryByPath(lib.ID, rel)
		if lookupErr != nil {
			return fmt.Errorf("lookup directory %q: %w", rel, lookupErr)
		}
		if existing == nil {
			slog.Info("scan dir: no audio files", "library", lib.Name, "path", rel)
			return nil
		}
		return s.indexParentDir(lib.ID, rel)
	}

	// Ancestors keep the directory reachable in the tree. An ancestor that is
	// already indexed is left alone: it may be an audio directory itself, and
	// the parent-only upsert would clear its codec summary and audio flag.
	for _, parent := range ancestorPaths(rel) {
		existing, lookupErr := s.db.GetDirectoryByPath(lib.ID, parent)
		if lookupErr != nil {
			return fmt.Errorf("lookup directory %q: %w", parent, lookupErr)
		}
		if existing != nil {
			continue
		}
		if err := s.indexParentDir(lib.ID, parent); err != nil {
			return fmt.Errorf("index parent %q: %w", parent, err)
		}
	}

	if err := s.indexAudioDir(ctx, lib, rel, absPath, audioFiles); err != nil {
		return fmt.Errorf("index directory %q: %w", rel, err)
	}

	slog.Info("scan dir complete", "library", lib.Name, "path", rel, "files", len(audioFiles))
	return nil
}

// checkScannable rejects paths a full scan would skip: anything outside the
// library root, an app-owned .alto-* segment, or the resolved ALTO_OUTPUT_DIR.
func (s *Scanner) checkScannable(lib db.Library, rel, absPath string) error {
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("%w: %q is outside the library", ErrDirExcluded, rel)
	}
	if containsAltoSegment(absPath, lib.Path) {
		return fmt.Errorf("%w: %q is app-owned", ErrDirExcluded, rel)
	}

	resolvedOut, outErr := filepath.EvalSymlinks(s.cfg.OutputDir)
	if outErr != nil || resolvedOut == "" {
		return nil
	}
	resolved, rerr := filepath.EvalSymlinks(absPath)
	if rerr != nil {
		return nil
	}
	if resolved == resolvedOut || strings.HasPrefix(resolved, resolvedOut+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q is the output directory", ErrDirExcluded, rel)
	}
	return nil
}

// normalizeRelPath brings a library-relative path to the form the index stores:
// slash-separated, no leading or trailing slash, with the library root as "".
func normalizeRelPath(relPath string) string {
	rel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relPath)))
	if rel == "." || rel == "/" {
		return ""
	}
	return strings.Trim(rel, "/")
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
// It checks external files first; if none, it tries embedded art extraction,
// using the already-probed tracks rather than probing again.
func (s *Scanner) resolveCover(ctx context.Context, dirPath string, tracks []db.Track, libID int64, relPath string) (string, bool) {
	// Check for external cover art files. Use Lstat to reject symlinks — following
	// symlinks here would allow a crafted cover.jpg -> /etc/passwd to be indexed
	// and later served through /api/cover.
	for _, name := range externalCoverNames {
		candidate := filepath.Join(dirPath, name)
		if fi, err := os.Lstat(candidate); err == nil && fi.Mode().IsRegular() {
			return candidate, true
		}
	}

	// Fall back to embedded art extraction. Guard on tracks, not on the walk's
	// audio file list: probeFiles drops files it cannot stat, so a directory with
	// audio files can still yield no tracks and the two do not correspond
	// index-for-index.
	if len(tracks) == 0 || !tracks[0].HasEmbeddedCover {
		return "", false
	}
	src := filepath.Join(dirPath, tracks[0].Filename)

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

// probeFiles returns a Track record per audio file, running ffprobe only on the
// files whose size and mtime differ from the cached row of the same name. Files
// are handled concurrently, bounded by the scanner-wide worker semaphore, but
// the returned tracks keep the order of audioFiles. The second return value
// reports whether every returned track came from the cache, i.e. whether no file
// was probed.
func (s *Scanner) probeFiles(ctx context.Context, dirPath string, audioFiles []string, cached map[string]db.Track) ([]db.Track, bool) {
	// Index-addressed so probe completion order cannot reorder the directory;
	// a nil entry is a file with no row to write (vanished, or scan canceled).
	results := make([]*db.Track, len(audioFiles))
	var probed atomic.Bool
	var wg sync.WaitGroup

	// The token is taken before the goroutine is started, so a directory with
	// thousands of files does not park thousands of goroutines on the semaphore.
launch:
	for i, name := range audioFiles {
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			break launch
		}

		wg.Go(func() {
			defer func() { <-s.sem }()

			t, hit, ok := s.probeFile(ctx, dirPath, name, cached)
			if !ok {
				return
			}
			if !hit {
				probed.Store(true)
			}
			results[i] = &t
		})
	}
	wg.Wait()

	tracks := make([]db.Track, 0, len(audioFiles))
	for _, t := range results {
		if t != nil {
			tracks = append(tracks, *t)
		}
	}
	return tracks, !probed.Load()
}

// probeFile builds the Track record of a single audio file, reusing the cached
// row when the file still matches it. hit reports a cache hit (no process
// spawned); ok is false when the file cannot be stat'd and therefore has no row.
func (s *Scanner) probeFile(ctx context.Context, dirPath, name string, cached map[string]db.Track) (track db.Track, hit bool, ok bool) {
	fullPath := filepath.Join(dirPath, name)
	before, err := os.Stat(fullPath)
	if err != nil {
		slog.Warn("stat audio file", "path", fullPath, "err", err)
		return db.Track{}, false, false
	}

	// MTime 0 marks a row whose metadata could not be trusted (migrated, failed
	// probe, or a file that changed mid-scan) and never counts as a hit.
	if prev, cachedOK := cached[name]; cachedOK && prev.MTime != 0 &&
		prev.Size == before.Size() && prev.MTime == before.ModTime().UnixNano() {
		return prev, true, true
	}

	t := db.Track{
		Filename: name,
		Size:     before.Size(),
		MTime:    before.ModTime().UnixNano(),
	}

	info, probeErr := s.prober.Probe(ctx, fullPath)
	if probeErr != nil {
		slog.Warn("ffprobe", "file", fullPath, "err", probeErr)
		t.MTime = 0
		return t, false, true
	}

	t.Codec = info.Codec
	t.Bitrate = info.Bitrate
	t.Duration = info.Duration
	t.SampleRate = info.SampleRate
	t.Channels = info.Channels
	t.HasEmbeddedCover = info.HasCover

	// The file may have been rewritten while ffprobe ran; pinning the new
	// (size, mtime) against the old file's metadata would cache it forever.
	after, statErr := os.Stat(fullPath)
	if statErr != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.MTime = 0
	} else {
		t.Size = after.Size()
		t.MTime = after.ModTime().UnixNano()
	}
	return t, false, true
}

// tracksMatchCache reports whether tracks is exactly the set of stored rows in
// cached, by filename. Paired with an all-cache-hit probe pass it means writing
// tracks back would store the same values that are already there.
func tracksMatchCache(tracks []db.Track, cached map[string]db.Track) bool {
	if len(tracks) != len(cached) {
		return false
	}
	for _, t := range tracks {
		if _, ok := cached[t.Filename]; !ok {
			return false
		}
	}
	return true
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
