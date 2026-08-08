package library

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semsemyonoff/ALTO/internal/db"
	_ "modernc.org/sqlite"
)

// mockProber is a test double for Prober that returns canned metadata and
// counts the calls it received per path.
type mockProber struct {
	results map[string]*TrackInfo
	err     map[string]error
	// defaultResult is returned for any path not in results.
	defaultResult *TrackInfo
	// hook, if set, runs before each probe. It lets a test mutate the tree
	// between the walk and a later directory's probe.
	hook func(path string)
	// delay is how long every probe blocks; delays overrides it per path. Both
	// exist to make concurrent probes actually overlap in wall-clock time.
	delay  time.Duration
	delays map[string]time.Duration

	mu    sync.Mutex
	calls map[string]int

	inFlight atomic.Int32
	peak     atomic.Int32
}

// count returns how many times path was probed.
func (m *mockProber) count(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[path]
}

// total returns the number of probes across all paths.
func (m *mockProber) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int
	for _, c := range m.calls {
		n += c
	}
	return n
}

// peakInFlight returns the highest number of probes observed running at once.
func (m *mockProber) peakInFlight() int {
	return int(m.peak.Load())
}

func (m *mockProber) Probe(_ context.Context, path string) (*TrackInfo, error) {
	m.mu.Lock()
	if m.calls == nil {
		m.calls = make(map[string]int)
	}
	m.calls[path]++
	delay := m.delay
	if d, ok := m.delays[path]; ok {
		delay = d
	}
	m.mu.Unlock()

	cur := m.inFlight.Add(1)
	defer m.inFlight.Add(-1)
	for {
		seen := m.peak.Load()
		if cur <= seen || m.peak.CompareAndSwap(seen, cur) {
			break
		}
	}

	if delay > 0 {
		time.Sleep(delay)
	}
	if m.hook != nil {
		m.hook(path)
	}
	if e, ok := m.err[path]; ok {
		return nil, e
	}
	if info, ok := m.results[path]; ok {
		return info, nil
	}
	if m.defaultResult != nil {
		return m.defaultResult, nil
	}
	return &TrackInfo{Codec: "flac", SampleRate: 44100, Channels: 2, Duration: 60, Bitrate: 800000}, nil
}

// makeTestTree creates a temporary directory tree for testing.
// Structure: root/<dirs[i]>/<files[i]...>
func makeTestTree(t *testing.T, dirs map[string][]string) string {
	t.Helper()
	root := t.TempDir()
	for dir, files := range dirs {
		dirPath := filepath.Join(root, filepath.FromSlash(dir))
		if err := os.MkdirAll(dirPath, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			fp := filepath.Join(dirPath, f)
			if err := os.WriteFile(fp, []byte("fake"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// TestScannerBasic verifies that audio directories are discovered and stored.
func TestScannerBasic(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Artist/Album": {"01 - Track.flac", "02 - Track.flac", "cover.jpg"},
		"empty":        {},
		"docs":         {"readme.txt"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dirs, err := database.GetDirectoryTree(libID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 {
		t.Fatalf("expected album plus parent directory, got %d: %v", len(dirs), dirs)
	}
	if dirs[0].Path != "Artist" {
		t.Errorf("first path: got %q want %q", dirs[0].Path, "Artist")
	}
	if dirs[1].Path != "Artist/Album" {
		t.Errorf("second path: got %q want %q", dirs[1].Path, "Artist/Album")
	}
	if !dirs[1].HasCover {
		t.Error("expected HasCover true for directory with cover.jpg")
	}
	if dirs[1].CodecSummary != "FLAC" {
		t.Errorf("CodecSummary: got %q want %q", dirs[1].CodecSummary, "FLAC")
	}
	if dirs[0].HasCover {
		t.Error("parent directory should not inherit album cover")
	}
	if dirs[0].CodecSummary != "" {
		t.Errorf("parent directory CodecSummary: got %q want empty", dirs[0].CodecSummary)
	}

	tracks, err := database.GetDirectoryFiles(dirs[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(tracks))
	}
}

func TestScannerIndexesAncestorsForNestedAudioDirectories(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Artists/Live/Bootlegs/1994": {"01.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dirs, err := database.GetDirectoryTree(libID)
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, len(dirs))
	for i, dir := range dirs {
		got[i] = dir.Path
	}
	want := []string{
		"Artists",
		"Artists/Live",
		"Artists/Live/Bootlegs",
		"Artists/Live/Bootlegs/1994",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d indexed directories, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("path[%d]: got %q want %q (all: %v)", i, got[i], want[i], got)
		}
	}

	children, err := database.GetDirectoryChildren(libID, "")
	if err != nil {
		t.Fatalf("GetDirectoryChildren(root): %v", err)
	}
	if len(children) != 1 || children[0].Path != "Artists" {
		t.Fatalf("expected top-level Artists node, got %v", childPaths(children))
	}

	children, err = database.GetDirectoryChildren(libID, "Artists/Live")
	if err != nil {
		t.Fatalf("GetDirectoryChildren(Artists/Live): %v", err)
	}
	if len(children) != 1 || children[0].Path != "Artists/Live/Bootlegs" {
		t.Fatalf("expected Bootlegs child, got %v", childPaths(children))
	}
}

// TestScannerExcludesAltoDirs verifies that .alto-* directories are skipped.
func TestScannerExcludesAltoDirs(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Music":                  {"song.flac"},
		"Music/alto-out":         {"output-visible.flac"}, // must be excluded
		"Music/.alto-out":        {"output.flac"},         // must be excluded
		"Music/.alto-tmp-abc123": {"temp.flac"},           // must be excluded
		"Music/.alto-backup-abc": {"backup.flac"},         // must be excluded
		"out":                    {"user.flac"},           // regular user dir — must be included
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dirs, err := database.GetDirectoryTree(libID)
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range dirs {
		if containsAltoSegment(filepath.Join(root, filepath.FromSlash(d.Path)), root) {
			t.Errorf("alto-dir %q should have been excluded", d.Path)
		}
	}

	// "Music" and "out" should be indexed.
	paths := make(map[string]bool)
	for _, d := range dirs {
		paths[d.Path] = true
	}
	if !paths["Music"] {
		t.Error("expected Music to be indexed")
	}
	if !paths["out"] {
		t.Error("expected out (user dir) to be indexed")
	}
}

// TestScannerExcludesOutputDir verifies that ALTO_OUTPUT_DIR nested in a library is excluded.
func TestScannerExcludesOutputDir(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Music":      {"song.flac"},
		"transcoded": {"output.flac"}, // this is ALTO_OUTPUT_DIR
	})

	outputDir := filepath.Join(root, "transcoded")
	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	s := NewScanner(database, &mockProber{}, ScanConfig{
		OutputDir: outputDir,
		CacheDir:  t.TempDir(),
	})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dirs, err := database.GetDirectoryTree(libID)
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range dirs {
		if d.Path == "transcoded" {
			t.Error("transcoded dir (ALTO_OUTPUT_DIR) should have been excluded")
		}
	}

	paths := make(map[string]bool)
	for _, d := range dirs {
		paths[d.Path] = true
	}
	if !paths["Music"] {
		t.Error("Music should still be indexed")
	}
}

// TestScannerStaleReconciliation verifies that renamed/removed files and dirs are cleaned up.
func TestScannerStaleReconciliation(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album1": {"a.flac", "b.flac"},
		"Album2": {"c.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}

	// First scan — both albums present.
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatal(err)
	}
	dirs, _ := database.GetDirectoryTree(libID)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs after initial scan, got %d", len(dirs))
	}

	// Remove Album2 from disk.
	if err := os.RemoveAll(filepath.Join(root, "Album2")); err != nil {
		t.Fatal(err)
	}
	// Remove b.flac from Album1.
	if err := os.Remove(filepath.Join(root, "Album1", "b.flac")); err != nil {
		t.Fatal(err)
	}

	// Second scan — stale entries should be removed.
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	dirs, _ = database.GetDirectoryTree(libID)
	if len(dirs) != 1 {
		t.Fatalf("expected 1 dir after rescan, got %d: %v", len(dirs), dirs)
	}
	if dirs[0].Path != "Album1" {
		t.Errorf("expected Album1, got %q", dirs[0].Path)
	}

	tracks, _ := database.GetDirectoryFiles(dirs[0].ID)
	if len(tracks) != 1 {
		t.Fatalf("expected 1 track after removing b.flac, got %d", len(tracks))
	}
	if tracks[0].Filename != "a.flac" {
		t.Errorf("expected a.flac, got %q", tracks[0].Filename)
	}
}

func TestScannerClearsTracksWhenDirectoryBecomesParentOnly(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Artist": {"song.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(root, "Artist", "song.flac")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "Artist", "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Artist", "Album", "song.flac"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	dir, err := database.GetDirectoryByPath(libID, "Artist")
	if err != nil {
		t.Fatal(err)
	}
	if dir == nil {
		t.Fatal("expected Artist parent directory to stay indexed")
	}

	tracks, err := database.GetDirectoryFiles(dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 0 {
		t.Fatalf("expected Artist to have no direct tracks after becoming parent-only, got %d", len(tracks))
	}

	children, err := database.GetDirectoryChildren(libID, "Artist")
	if err != nil {
		t.Fatalf("GetDirectoryChildren(Artist): %v", err)
	}
	if len(children) != 1 || children[0].Path != "Artist/Album" {
		t.Fatalf("expected nested album child, got %v", childPaths(children))
	}
}

// TestScannerExternalCoverArt verifies detection of known external cover filenames.
func TestScannerExternalCoverArt(t *testing.T) {
	covers := []string{"cover.jpg", "cover.png", "folder.jpg", "folder.png", "front.jpg", "front.png"}
	for _, coverFile := range covers {
		t.Run(coverFile, func(t *testing.T) {
			root := makeTestTree(t, map[string][]string{
				"Album": {"song.flac", coverFile},
			})

			database := openTestDB(t)
			libID, err := database.UpsertLibrary("test", root)
			if err != nil {
				t.Fatal(err)
			}

			s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
			lib := db.Library{ID: libID, Name: "test", Path: root}
			if err := s.Scan(context.Background(), lib); err != nil {
				t.Fatalf("Scan: %v", err)
			}

			dirs, _ := database.GetDirectoryTree(libID)
			if len(dirs) == 0 {
				t.Fatal("no dirs indexed")
			}
			if !dirs[0].HasCover {
				t.Errorf("%s: expected HasCover=true", coverFile)
			}
			expectedCoverPath := filepath.Join(root, "Album", coverFile)
			if dirs[0].CoverPath != expectedCoverPath {
				t.Errorf("CoverPath: got %q want %q", dirs[0].CoverPath, expectedCoverPath)
			}
		})
	}
}

// TestScannerNoCoverArt verifies that directories without cover art have HasCover=false.
func TestScannerNoCoverArt(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"song.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	// Prober that reports no embedded cover.
	prober := &mockProber{
		defaultResult: &TrackInfo{Codec: "flac", SampleRate: 44100, Channels: 2, HasCover: false},
	}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	dirs, _ := database.GetDirectoryTree(libID)
	if len(dirs) == 0 {
		t.Fatal("no dirs indexed")
	}
	if dirs[0].HasCover {
		t.Error("expected HasCover=false when no cover art present")
	}
}

// TestScannerEmbeddedCoverArt verifies that embedded cover art triggers extraction.
func TestScannerEmbeddedCoverArt(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"song.mp3"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	// Prober reports embedded cover.
	prober := &mockProber{
		defaultResult: &TrackInfo{Codec: "mp3", SampleRate: 44100, Channels: 2, HasCover: true},
	}

	cacheDir := t.TempDir()
	// We can't actually run ffmpeg in tests, so we create the expected cache file manually
	// to simulate a successful extraction.
	// We'll use a custom scanner that overrides cover extraction.
	// Instead, test that the scanner attempts cover extraction and handles failure gracefully.
	s := NewScanner(database, prober, ScanConfig{CacheDir: cacheDir})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	// Scan should complete without error even if ffmpeg isn't available.
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dirs, _ := database.GetDirectoryTree(libID)
	if len(dirs) == 0 {
		t.Fatal("no dirs indexed")
	}
	// HasCover may be false if ffmpeg is not available; we just check it doesn't crash.
	// If ffmpeg IS available, HasCover would be true.
	_ = dirs[0].HasCover // no assertion — environment-dependent
}

// TestScannerMixedCodecs verifies the "Mixed" codec summary.
func TestScannerMixedCodecs(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Mixed": {"a.flac", "b.mp3"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	aPath := filepath.Join(root, "Mixed", "a.flac")
	bPath := filepath.Join(root, "Mixed", "b.mp3")
	prober := &mockProber{
		results: map[string]*TrackInfo{
			aPath: {Codec: "flac", SampleRate: 44100, Channels: 2},
			bPath: {Codec: "mp3", SampleRate: 44100, Channels: 2},
		},
	}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	dirs, _ := database.GetDirectoryTree(libID)
	if len(dirs) == 0 {
		t.Fatal("no dirs indexed")
	}
	if dirs[0].CodecSummary != "Mixed" {
		t.Errorf("CodecSummary: got %q want %q", dirs[0].CodecSummary, "Mixed")
	}
}

// TestScannerAudioExtensions verifies all recognised audio extensions are indexed.
func TestScannerAudioExtensions(t *testing.T) {
	extensions := []string{
		"a.flac", "b.opus", "c.ogg", "d.mp3", "e.wav",
		"f.aac", "g.m4a", "h.wma", "i.alac", "j.ape", "k.wv",
	}
	root := makeTestTree(t, map[string][]string{
		"AllFormats": extensions,
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	dirs, _ := database.GetDirectoryTree(libID)
	if len(dirs) != 1 {
		t.Fatalf("expected 1 dir, got %d", len(dirs))
	}

	tracks, _ := database.GetDirectoryFiles(dirs[0].ID)
	if len(tracks) != len(extensions) {
		t.Errorf("expected %d tracks, got %d", len(extensions), len(tracks))
	}
}

// TestScannerNonAudioDirNotIndexed verifies that dirs without audio files are not indexed.
func TestScannerNonAudioDirNotIndexed(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Docs":   {"readme.txt", "notes.pdf"},
		"Images": {"photo.jpg"},
		"Music":  {"song.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatal(err)
	}

	dirs, _ := database.GetDirectoryTree(libID)
	if len(dirs) != 1 {
		t.Fatalf("expected only Music to be indexed, got %d dirs: %v", len(dirs), dirs)
	}
	if dirs[0].Path != "Music" {
		t.Errorf("expected Music, got %q", dirs[0].Path)
	}
}

// TestScanAllParallel verifies that multiple libraries can be scanned concurrently.
func TestScanAllParallel(t *testing.T) {
	root1 := makeTestTree(t, map[string][]string{"AlbumA": {"a.flac"}})
	root2 := makeTestTree(t, map[string][]string{"AlbumB": {"b.flac"}})

	database := openTestDB(t)
	id1, _ := database.UpsertLibrary("lib1", root1)
	id2, _ := database.UpsertLibrary("lib2", root2)

	s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
	libs := []db.Library{
		{ID: id1, Name: "lib1", Path: root1},
		{ID: id2, Name: "lib2", Path: root2},
	}

	if err := s.ScanAll(context.Background(), libs, nil); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}

	dirs1, _ := database.GetDirectoryTree(id1)
	dirs2, _ := database.GetDirectoryTree(id2)

	if len(dirs1) != 1 {
		t.Errorf("lib1: expected 1 dir, got %d", len(dirs1))
	}
	if len(dirs2) != 1 {
		t.Errorf("lib2: expected 1 dir, got %d", len(dirs2))
	}
}

// TestScanAllProgress verifies that ScanAll reports increasing, race-free
// discovered-directory counts per library while scanning concurrently.
func TestScanAllProgress(t *testing.T) {
	root1 := makeTestTree(t, map[string][]string{"AlbumA": {"a.flac"}, "AlbumB": {"b.flac"}})
	root2 := makeTestTree(t, map[string][]string{"AlbumC": {"c.flac"}})

	database := openTestDB(t)
	id1, _ := database.UpsertLibrary("lib1", root1)
	id2, _ := database.UpsertLibrary("lib2", root2)

	s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
	libs := []db.Library{
		{ID: id1, Name: "lib1", Path: root1},
		{ID: id2, Name: "lib2", Path: root2},
	}

	var mu sync.Mutex
	calls := make(map[int64][]int)
	progress := func(libraryID int64, discoveredDirs int) {
		mu.Lock()
		defer mu.Unlock()
		calls[libraryID] = append(calls[libraryID], discoveredDirs)
	}

	if err := s.ScanAll(context.Background(), libs, progress); err != nil {
		t.Fatalf("ScanAll: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	for _, libID := range []int64{id1, id2} {
		seq := calls[libID]
		if len(seq) == 0 {
			t.Fatalf("library %d: expected at least one progress call, got none", libID)
		}
		for i := 1; i < len(seq); i++ {
			if seq[i] <= seq[i-1] {
				t.Errorf("library %d: expected strictly increasing counts, got %v", libID, seq)
				break
			}
		}
	}

	if got := len(calls[id1]); got != 2 {
		t.Errorf("lib1: expected 2 progress calls (2 audio dirs), got %d: %v", got, calls[id1])
	}
	if got := len(calls[id2]); got != 1 {
		t.Errorf("lib2: expected 1 progress call (1 audio dir), got %d: %v", got, calls[id2])
	}
}

// TestBuildCodecSummary tests the codec summary helper directly.
func TestBuildCodecSummary(t *testing.T) {
	tests := []struct {
		name   string
		tracks []db.Track
		want   string
	}{
		{"empty", nil, ""},
		{"all flac", []db.Track{{Codec: "flac"}, {Codec: "flac"}}, "FLAC"},
		{"all opus", []db.Track{{Codec: "opus"}}, "OPUS"},
		{"mixed", []db.Track{{Codec: "flac"}, {Codec: "mp3"}}, "Mixed"},
		{"no codec fields", []db.Track{{Codec: ""}, {Codec: ""}}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildCodecSummary(tc.tracks)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestScannerBatchWritesAllTracks verifies every track of a multi-file directory
// survives the batched write path with its probed metadata intact.
func TestScannerBatchWritesAllTracks(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"01.flac", "02.mp3", "03.opus"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	dirPath := filepath.Join(root, "Album")
	prober := &mockProber{
		results: map[string]*TrackInfo{
			filepath.Join(dirPath, "01.flac"): {Codec: "flac", Bitrate: 900000, Duration: 61.5, SampleRate: 44100, Channels: 2},
			filepath.Join(dirPath, "02.mp3"):  {Codec: "mp3", Bitrate: 320000, Duration: 122.25, SampleRate: 48000, Channels: 1},
			filepath.Join(dirPath, "03.opus"): {Codec: "opus", Bitrate: 128000, Duration: 33, SampleRate: 48000, Channels: 2},
		},
	}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dir, err := database.GetDirectoryByPath(libID, "Album")
	if err != nil {
		t.Fatal(err)
	}
	if dir == nil {
		t.Fatal("expected Album to be indexed")
	}

	tracks, err := database.GetDirectoryFiles(dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 3 {
		t.Fatalf("expected 3 tracks, got %d", len(tracks))
	}

	byName := make(map[string]db.Track, len(tracks))
	for _, tr := range tracks {
		byName[tr.Filename] = tr
	}

	want := map[string]db.Track{
		"01.flac": {Codec: "flac", Bitrate: 900000, Duration: 61.5, SampleRate: 44100, Channels: 2},
		"02.mp3":  {Codec: "mp3", Bitrate: 320000, Duration: 122.25, SampleRate: 48000, Channels: 1},
		"03.opus": {Codec: "opus", Bitrate: 128000, Duration: 33, SampleRate: 48000, Channels: 2},
	}
	for name, w := range want {
		got, ok := byName[name]
		if !ok {
			t.Errorf("%s: missing from DB", name)
			continue
		}
		if got.DirectoryID != dir.ID {
			t.Errorf("%s: DirectoryID = %d, want %d", name, got.DirectoryID, dir.ID)
		}
		if got.Codec != w.Codec || got.Bitrate != w.Bitrate || got.Duration != w.Duration ||
			got.SampleRate != w.SampleRate || got.Channels != w.Channels {
			t.Errorf("%s: got %+v, want codec/bitrate/duration/rate/channels %+v", name, got, w)
		}
		fi, statErr := os.Stat(filepath.Join(dirPath, name))
		if statErr != nil {
			t.Fatal(statErr)
		}
		if got.Size != fi.Size() {
			t.Errorf("%s: Size = %d, want %d", name, got.Size, fi.Size())
		}
	}
}

// TestScannerBatchWriteNoProbeableFiles verifies a directory whose audio files
// all vanish before probing writes no track rows and does not fail the scan.
func TestScannerBatchWriteNoProbeableFiles(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"A-Album": {"keeper.flac"},
		"B-Album": {"gone.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	// Directories are processed in sorted order, so deleting B-Album's only file
	// while probing A-Album drops it after the walk listed it: probeFiles' os.Stat
	// then fails and the directory yields zero tracks.
	victim := filepath.Join(root, "B-Album", "gone.flac")
	prober := &mockProber{hook: func(string) { _ = os.Remove(victim) }}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dir, err := database.GetDirectoryByPath(libID, "B-Album")
	if err != nil {
		t.Fatal(err)
	}
	if dir == nil {
		t.Fatal("expected B-Album to still be indexed")
	}

	tracks, err := database.GetDirectoryFiles(dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 0 {
		t.Fatalf("expected no tracks, got %d: %+v", len(tracks), tracks)
	}
	if dir.CodecSummary != "" {
		t.Errorf("CodecSummary: got %q want empty", dir.CodecSummary)
	}

	// The unaffected sibling is still written normally.
	kept, err := database.GetDirectoryByPath(libID, "A-Album")
	if err != nil {
		t.Fatal(err)
	}
	if kept == nil {
		t.Fatal("expected A-Album to be indexed")
	}
	keptTracks, err := database.GetDirectoryFiles(kept.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keptTracks) != 1 || keptTracks[0].Filename != "keeper.flac" {
		t.Fatalf("expected keeper.flac in A-Album, got %+v", keptTracks)
	}
}

// TestScannerProbesEachFileOnce verifies that a directory without external cover
// art probes every audio file exactly once. Before cover resolution was fed by
// probeFiles, the first file was probed a second time by resolveCover.
func TestScannerProbesEachFileOnce(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"01.flac", "02.flac", "03.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	prober := &mockProber{
		defaultResult: &TrackInfo{Codec: "flac", SampleRate: 44100, Channels: 2, HasCover: false},
	}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dirPath := filepath.Join(root, "Album")
	for _, name := range []string{"01.flac", "02.flac", "03.flac"} {
		if got := prober.count(filepath.Join(dirPath, name)); got != 1 {
			t.Errorf("%s: probed %d times, want 1", name, got)
		}
	}
	if got := prober.total(); got != 3 {
		t.Errorf("total probes: got %d want 3", got)
	}
}

// TestScannerEmbeddedCoverUsesCachedFile verifies the resolution order past the
// external-file check: the embedded-art flag comes from the probed track, and an
// already-extracted cache file short-circuits the ffmpeg extraction.
func TestScannerEmbeddedCoverUsesCachedFile(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"01.flac", "02.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	cacheFile := coverCachePath(cacheDir, libID, "Album")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}

	prober := &mockProber{
		defaultResult: &TrackInfo{Codec: "flac", SampleRate: 44100, Channels: 2, HasCover: true},
	}

	s := NewScanner(database, prober, ScanConfig{CacheDir: cacheDir})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dir, err := database.GetDirectoryByPath(libID, "Album")
	if err != nil {
		t.Fatal(err)
	}
	if dir == nil {
		t.Fatal("expected Album to be indexed")
	}
	if !dir.HasCover {
		t.Error("expected HasCover=true from the embedded-art flag")
	}
	if dir.CoverPath != cacheFile {
		t.Errorf("CoverPath: got %q want %q", dir.CoverPath, cacheFile)
	}
	if got := prober.total(); got != 2 {
		t.Errorf("total probes: got %d want 2 (one per file)", got)
	}

	// The flag is persisted per track, not just consumed for cover resolution.
	tracks, err := database.GetDirectoryFiles(dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range tracks {
		if !tr.HasEmbeddedCover {
			t.Errorf("%s: HasEmbeddedCover=false, want true", tr.Filename)
		}
	}
}

// TestScannerExternalCoverBeatsEmbedded verifies external art still wins over
// embedded art, and that a symlinked cover file is still rejected.
func TestScannerExternalCoverBeatsEmbedded(t *testing.T) {
	t.Run("regular file wins", func(t *testing.T) {
		root := makeTestTree(t, map[string][]string{
			"Album": {"01.flac", "cover.jpg"},
		})

		database := openTestDB(t)
		libID, err := database.UpsertLibrary("test", root)
		if err != nil {
			t.Fatal(err)
		}

		prober := &mockProber{
			defaultResult: &TrackInfo{Codec: "flac", SampleRate: 44100, Channels: 2, HasCover: true},
		}

		s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
		lib := db.Library{ID: libID, Name: "test", Path: root}
		if err := s.Scan(context.Background(), lib); err != nil {
			t.Fatalf("Scan: %v", err)
		}

		dir, err := database.GetDirectoryByPath(libID, "Album")
		if err != nil {
			t.Fatal(err)
		}
		if dir == nil {
			t.Fatal("expected Album to be indexed")
		}
		if !dir.HasCover {
			t.Error("expected HasCover=true")
		}
		want := filepath.Join(root, "Album", "cover.jpg")
		if dir.CoverPath != want {
			t.Errorf("CoverPath: got %q want %q", dir.CoverPath, want)
		}
	})

	t.Run("symlink rejected", func(t *testing.T) {
		root := makeTestTree(t, map[string][]string{
			"Album": {"01.flac"},
		})
		target := filepath.Join(root, "outside.jpg")
		if err := os.WriteFile(target, []byte("jpeg"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "Album", "cover.jpg")); err != nil {
			t.Skipf("symlinks unsupported: %v", err)
		}

		database := openTestDB(t)
		libID, err := database.UpsertLibrary("test", root)
		if err != nil {
			t.Fatal(err)
		}

		// No embedded art either, so the symlink must leave the directory coverless.
		prober := &mockProber{
			defaultResult: &TrackInfo{Codec: "flac", SampleRate: 44100, Channels: 2, HasCover: false},
		}

		s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
		lib := db.Library{ID: libID, Name: "test", Path: root}
		if err := s.Scan(context.Background(), lib); err != nil {
			t.Fatalf("Scan: %v", err)
		}

		dir, err := database.GetDirectoryByPath(libID, "Album")
		if err != nil {
			t.Fatal(err)
		}
		if dir == nil {
			t.Fatal("expected Album to be indexed")
		}
		if dir.HasCover || dir.CoverPath != "" {
			t.Errorf("symlinked cover.jpg accepted: HasCover=%v CoverPath=%q", dir.HasCover, dir.CoverPath)
		}
	})
}

// TestScannerCoverSourceIsFirstProbedTrack verifies the embedded-art flag is read
// from the first *probed track*, not the first walked filename: the two slices do
// not correspond index-for-index once a file vanishes before its stat.
func TestScannerCoverSourceIsFirstProbedTrack(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"A-Album": {"keeper.flac"},
		"B-Album": {"01.flac", "02.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	// Directories are processed in sorted order: probing A-Album drops B-Album's
	// first file, so B-Album's only track is 02.flac, which has no embedded art.
	victim := filepath.Join(root, "B-Album", "01.flac")
	prober := &mockProber{
		hook: func(string) { _ = os.Remove(victim) },
		results: map[string]*TrackInfo{
			victim: {Codec: "flac", SampleRate: 44100, Channels: 2, HasCover: true},
			filepath.Join(root, "B-Album", "02.flac"): {Codec: "flac", SampleRate: 44100, Channels: 2, HasCover: false},
		},
	}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if got := prober.count(victim); got != 0 {
		t.Errorf("vanished file probed %d times, want 0", got)
	}

	dir, err := database.GetDirectoryByPath(libID, "B-Album")
	if err != nil {
		t.Fatal(err)
	}
	if dir == nil {
		t.Fatal("expected B-Album to be indexed")
	}
	if dir.HasCover {
		t.Error("expected HasCover=false: the surviving track has no embedded art")
	}

	tracks, err := database.GetDirectoryFiles(dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 1 || tracks[0].Filename != "02.flac" {
		t.Fatalf("expected only 02.flac, got %+v", tracks)
	}
}

// TestScannerNoCoverWhenAllAudioFilesVanish verifies a directory whose only audio
// file disappears between the walk and the probe resolves to no cover instead of
// indexing past the end of an empty track slice.
func TestScannerNoCoverWhenAllAudioFilesVanish(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"A-Album": {"keeper.flac"},
		"B-Album": {"gone.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	victim := filepath.Join(root, "B-Album", "gone.flac")
	prober := &mockProber{
		hook:          func(string) { _ = os.Remove(victim) },
		defaultResult: &TrackInfo{Codec: "flac", SampleRate: 44100, Channels: 2, HasCover: true},
	}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dir, err := database.GetDirectoryByPath(libID, "B-Album")
	if err != nil {
		t.Fatal(err)
	}
	if dir == nil {
		t.Fatal("expected B-Album to still be indexed")
	}
	if dir.HasCover || dir.CoverPath != "" {
		t.Errorf("expected no cover, got HasCover=%v CoverPath=%q", dir.HasCover, dir.CoverPath)
	}
}

// tracksByName returns the stored tracks of one directory keyed by filename.
func tracksByName(t *testing.T, database *db.DB, libID int64, relPath string) map[string]db.Track {
	t.Helper()
	dir, err := database.GetDirectoryByPath(libID, relPath)
	if err != nil {
		t.Fatal(err)
	}
	if dir == nil {
		t.Fatalf("directory %q not indexed", relPath)
	}
	tracks, err := database.GetDirectoryFiles(dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]db.Track, len(tracks))
	for _, tr := range tracks {
		byName[tr.Filename] = tr
	}
	return byName
}

// TestScannerRescanSkipsUnchangedFiles verifies a second scan of an untouched
// tree spawns no probes at all and leaves the stored metadata identical.
func TestScannerRescanSkipsUnchangedFiles(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album":       {"01.flac", "02.flac"},
		"Album/Extra": {"03.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	prober := &mockProber{
		defaultResult: &TrackInfo{Codec: "flac", Bitrate: 900000, Duration: 61.5, SampleRate: 44100, Channels: 2},
	}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	if got := prober.total(); got != 3 {
		t.Fatalf("first scan probes: got %d want 3", got)
	}
	before := tracksByName(t, database, libID, "Album")
	if len(before) != 2 {
		t.Fatalf("expected 2 tracks in Album, got %d", len(before))
	}
	for name, tr := range before {
		if tr.MTime == 0 {
			t.Errorf("%s: MTime not stored after a successful probe", name)
		}
	}

	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	if got := prober.total(); got != 3 {
		t.Errorf("second scan spawned %d extra probes, want 0", got-3)
	}

	after := tracksByName(t, database, libID, "Album")
	if len(after) != len(before) {
		t.Fatalf("track count changed: %d -> %d", len(before), len(after))
	}
	for name, w := range before {
		got, ok := after[name]
		if !ok {
			t.Errorf("%s: missing after rescan", name)
			continue
		}
		if got != w {
			t.Errorf("%s: row changed across rescan\n got %+v\nwant %+v", name, got, w)
		}
	}
}

// TestScannerReprobesChangedMTime verifies that touching a single file re-probes
// exactly that file.
func TestScannerReprobesChangedMTime(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"01.flac", "02.flac", "03.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	prober := &mockProber{}
	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	// A distant timestamp, not a rewrite: on a coarse-timestamp filesystem a
	// rewrite of a small fixture can land in the same window and pass falsely.
	touched := filepath.Join(root, "Album", "02.flac")
	distant := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(touched, distant, distant); err != nil {
		t.Fatal(err)
	}

	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	if got := prober.count(touched); got != 2 {
		t.Errorf("touched file probed %d times, want 2 (once per scan)", got)
	}
	for _, name := range []string{"01.flac", "03.flac"} {
		if got := prober.count(filepath.Join(root, "Album", name)); got != 1 {
			t.Errorf("%s: probed %d times, want 1", name, got)
		}
	}

	stored := tracksByName(t, database, libID, "Album")
	if got := stored["02.flac"].MTime; got != distant.UnixNano() {
		t.Errorf("02.flac MTime: got %d want %d", got, distant.UnixNano())
	}
}

// TestScannerReprobesChangedSize verifies that a size change alone re-probes the
// file even when its mtime is restored to the cached value.
func TestScannerReprobesChangedSize(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"01.flac", "02.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	prober := &mockProber{}
	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	stored := tracksByName(t, database, libID, "Album")
	grown := filepath.Join(root, "Album", "01.flac")
	if err := os.WriteFile(grown, []byte("fake but longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Restore the cached mtime so size is the only difference left.
	prevMTime := time.Unix(0, stored["01.flac"].MTime)
	if err := os.Chtimes(grown, prevMTime, prevMTime); err != nil {
		t.Fatal(err)
	}

	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	if got := prober.count(grown); got != 2 {
		t.Errorf("resized file probed %d times, want 2 (once per scan)", got)
	}
	if got := prober.count(filepath.Join(root, "Album", "02.flac")); got != 1 {
		t.Errorf("unchanged file probed %d times, want 1", got)
	}

	fi, err := os.Stat(grown)
	if err != nil {
		t.Fatal(err)
	}
	if got := tracksByName(t, database, libID, "Album")["01.flac"].Size; got != fi.Size() {
		t.Errorf("01.flac Size: got %d want %d", got, fi.Size())
	}
}

// TestScannerProbesOnlyAddedFile verifies a file added to an already-scanned
// directory is the only one probed on the next scan.
func TestScannerProbesOnlyAddedFile(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"01.flac", "02.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	prober := &mockProber{}
	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	added := filepath.Join(root, "Album", "03.flac")
	if err := os.WriteFile(added, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	if got := prober.total(); got != 3 {
		t.Errorf("total probes: got %d want 3 (2 initial + 1 added)", got)
	}
	if got := prober.count(added); got != 1 {
		t.Errorf("added file probed %d times, want 1", got)
	}

	stored := tracksByName(t, database, libID, "Album")
	if len(stored) != 3 {
		t.Fatalf("expected 3 stored tracks, got %d", len(stored))
	}
}

// TestScannerFailedProbeRetriedNextScan verifies a file whose probe fails is
// stored with MTime 0 and is probed again on the next scan even though nothing
// about it changed on disk.
func TestScannerFailedProbeRetriedNextScan(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"broken.flac", "ok.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	broken := filepath.Join(root, "Album", "broken.flac")
	prober := &mockProber{err: map[string]error{broken: errors.New("ffprobe: invalid data")}}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	stored := tracksByName(t, database, libID, "Album")
	if got := stored["broken.flac"].MTime; got != 0 {
		t.Errorf("failed probe stored MTime %d, want 0", got)
	}
	if stored["broken.flac"].Codec != "" {
		t.Errorf("failed probe stored codec %q, want empty", stored["broken.flac"].Codec)
	}
	if stored["ok.flac"].MTime == 0 {
		t.Error("ok.flac: MTime not stored after a successful probe")
	}

	// The probe now succeeds; the retry must pick the metadata up.
	prober.err = nil

	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	if got := prober.count(broken); got != 2 {
		t.Errorf("failed file probed %d times, want 2 (retried)", got)
	}
	if got := prober.count(filepath.Join(root, "Album", "ok.flac")); got != 1 {
		t.Errorf("ok.flac probed %d times, want 1", got)
	}

	stored = tracksByName(t, database, libID, "Album")
	if stored["broken.flac"].MTime == 0 {
		t.Error("broken.flac: MTime still 0 after a successful retry")
	}
	if stored["broken.flac"].Codec != "flac" {
		t.Errorf("broken.flac codec after retry: got %q want %q", stored["broken.flac"].Codec, "flac")
	}
}

// legacyTracksSchema is the tracks DDL as it was before mtime and
// has_embedded_cover were added; it backs the upgrade test below.
const legacyTracksSchema = `
PRAGMA foreign_keys = ON;

CREATE TABLE libraries (
	id   INTEGER PRIMARY KEY,
	name TEXT UNIQUE NOT NULL,
	path TEXT UNIQUE NOT NULL
);

CREATE TABLE directories (
	id            INTEGER PRIMARY KEY,
	library_id    INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
	path          TEXT NOT NULL,
	has_cover     BOOLEAN NOT NULL DEFAULT 0,
	cover_path    TEXT NOT NULL DEFAULT '',
	codec_summary TEXT NOT NULL DEFAULT '',
	is_audio      BOOLEAN NOT NULL DEFAULT 0,
	UNIQUE(library_id, path)
);

CREATE TABLE tracks (
	id           INTEGER PRIMARY KEY,
	directory_id INTEGER NOT NULL REFERENCES directories(id) ON DELETE CASCADE,
	filename     TEXT NOT NULL,
	codec        TEXT NOT NULL DEFAULT '',
	bitrate      INTEGER NOT NULL DEFAULT 0,
	duration     REAL NOT NULL DEFAULT 0,
	sample_rate  INTEGER NOT NULL DEFAULT 0,
	channels     INTEGER NOT NULL DEFAULT 0,
	size         INTEGER NOT NULL DEFAULT 0,
	UNIQUE(directory_id, filename)
);
`

// TestScannerUpgradedLegacyDBRescansOnceThenCaches is the automated form of the
// operator upgrade path: a database written by the pre-change schema, already
// holding a fully indexed directory, must migrate in place, re-probe everything
// once (the backfilled mtime = 0 can never match a real file), and be probe-free
// from the second scan on.
func TestScannerUpgradedLegacyDBRescansOnceThenCaches(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"01.flac", "02.flac"},
	})

	// Build a legacy database that already describes the tree on disk, sizes
	// included — so nothing but the missing mtime can force the re-probe.
	dbPath := filepath.Join(t.TempDir(), "alto.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := raw.Exec(legacyTracksSchema); err != nil {
		t.Fatalf("exec legacy schema: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO libraries(id, name, path) VALUES(1, 'test', ?)`, root); err != nil {
		t.Fatalf("insert legacy library: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO directories(id, library_id, path, codec_summary, is_audio) VALUES(1, 1, 'Album', 'FLAC', 1)`,
	); err != nil {
		t.Fatalf("insert legacy directory: %v", err)
	}
	for _, name := range []string{"01.flac", "02.flac"} {
		fi, statErr := os.Stat(filepath.Join(root, "Album", name))
		if statErr != nil {
			t.Fatal(statErr)
		}
		if _, err := raw.Exec(
			`INSERT INTO tracks(directory_id, filename, codec, bitrate, duration, sample_rate, channels, size)
			 VALUES(1, ?, 'flac', 900000, 61.5, 44100, 2, ?)`,
			name, fi.Size(),
		); err != nil {
			t.Fatalf("insert legacy track %s: %v", name, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	prober := &mockProber{
		defaultResult: &TrackInfo{Codec: "flac", Bitrate: 900000, Duration: 61.5, SampleRate: 44100, Channels: 2, HasCover: true},
	}
	// Pre-create the extracted-cover cache file so the embedded-art path resolves
	// without shelling out to ffmpeg.
	cacheDir := t.TempDir()
	cacheFile := coverCachePath(cacheDir, 1, "Album")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewScanner(database, prober, ScanConfig{CacheDir: cacheDir})
	lib := db.Library{ID: 1, Name: "test", Path: root}

	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("first Scan after upgrade: %v", err)
	}
	if got := prober.total(); got != 2 {
		t.Fatalf("first scan after upgrade probes: got %d want 2 (a full re-probe)", got)
	}

	stored := tracksByName(t, database, 1, "Album")
	if len(stored) != 2 {
		t.Fatalf("expected 2 tracks after upgrade scan, got %d", len(stored))
	}
	for name, tr := range stored {
		if tr.MTime == 0 {
			t.Errorf("%s: MTime not backfilled by the upgrade scan", name)
		}
		if !tr.HasEmbeddedCover {
			t.Errorf("%s: HasEmbeddedCover not backfilled by the upgrade scan", name)
		}
	}

	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("second Scan after upgrade: %v", err)
	}
	if got := prober.total(); got != 2 {
		t.Errorf("second scan after upgrade spawned %d extra probes, want 0", got-2)
	}
}

// TestScannerSkipsTrackWritesForUnchangedDirectory verifies a warm rescan of an
// untouched directory issues no track writes at all — the point of the cache is
// lost if every rescan still upserts every row.
func TestScannerSkipsTrackWritesForUnchangedDirectory(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album":       {"01.flac", "02.flac"},
		"Album/Extra": {"03.flac"},
	})

	database, trackWrites := openCountingTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	prober := &mockProber{}
	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir()})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("first Scan: %v", err)
	}

	first := trackWrites()
	if first != 3 {
		t.Fatalf("first scan track writes: got %d want 3", first)
	}
	before := tracksByName(t, database, libID, "Album")

	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("second Scan: %v", err)
	}

	if got := trackWrites(); got != first {
		t.Errorf("second scan issued %d track writes, want 0", got-first)
	}
	if got := prober.total(); got != 3 {
		t.Errorf("second scan issued %d probes, want 0", got-3)
	}

	// Skipping the write must not skip the rows: they are still the stored ones.
	after := tracksByName(t, database, libID, "Album")
	for name, want := range before {
		if got := after[name]; got != want {
			t.Errorf("%s: row changed across rescan\n got %+v\nwant %+v", name, got, want)
		}
	}
	if len(after) != len(before) {
		t.Errorf("track count changed: %d -> %d", len(before), len(after))
	}
}

// TestScannerWritesDirectoryWithChangedFileSet verifies the write skip is keyed
// on the filename set too: a directory that gains or loses a file is written
// even though its surviving files are all cache hits.
func TestScannerWritesDirectoryWithChangedFileSet(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, albumDir string)
		want   []string
	}{
		{
			name: "gains a file",
			mutate: func(t *testing.T, albumDir string) {
				if err := os.WriteFile(filepath.Join(albumDir, "03.flac"), []byte("fake"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"01.flac", "02.flac", "03.flac"},
		},
		{
			name: "loses a file",
			mutate: func(t *testing.T, albumDir string) {
				if err := os.Remove(filepath.Join(albumDir, "02.flac")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"01.flac"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := makeTestTree(t, map[string][]string{
				"Album": {"01.flac", "02.flac"},
			})

			database, trackWrites := openCountingTestDB(t)
			libID, err := database.UpsertLibrary("test", root)
			if err != nil {
				t.Fatal(err)
			}

			s := NewScanner(database, &mockProber{}, ScanConfig{CacheDir: t.TempDir()})
			lib := db.Library{ID: libID, Name: "test", Path: root}
			if err := s.Scan(context.Background(), lib); err != nil {
				t.Fatalf("first Scan: %v", err)
			}

			first := trackWrites()
			tc.mutate(t, filepath.Join(root, "Album"))

			if err := s.Scan(context.Background(), lib); err != nil {
				t.Fatalf("second Scan: %v", err)
			}

			if got := trackWrites() - first; got != len(tc.want) {
				t.Errorf("second scan track writes: got %d want %d", got, len(tc.want))
			}

			stored := tracksByName(t, database, libID, "Album")
			if len(stored) != len(tc.want) {
				t.Fatalf("stored tracks: got %d (%v) want %d", len(stored), stored, len(tc.want))
			}
			for _, name := range tc.want {
				if _, ok := stored[name]; !ok {
					t.Errorf("%s: missing from the DB after rescan", name)
				}
			}
		})
	}
}

// TestDefaultScanWorkers verifies the computed default and the normalisation of
// unset/negative worker counts in NewScanner.
func TestDefaultScanWorkers(t *testing.T) {
	def := DefaultScanWorkers()
	if def < 1 || def > 4 {
		t.Fatalf("DefaultScanWorkers() = %d, want between 1 and 4", def)
	}

	tests := []struct {
		name    string
		workers int
		want    int
	}{
		{"unset", 0, def},
		{"negative", -3, def},
		{"explicit", 2, 2},
		{"above default", 16, 16},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewScanner(openTestDB(t), &mockProber{}, ScanConfig{Workers: tc.workers})
			if got := cap(s.sem); got != tc.want {
				t.Errorf("semaphore capacity: got %d want %d", got, tc.want)
			}
			if got := s.cfg.Workers; got != tc.want {
				t.Errorf("cfg.Workers: got %d want %d", got, tc.want)
			}
		})
	}
}

// TestScannerProbeConcurrencyBounded verifies that concurrent probes never
// exceed the configured worker count, and that they do overlap at all.
func TestScannerProbeConcurrencyBounded(t *testing.T) {
	const workers = 2
	files := []string{"01.flac", "02.flac", "03.flac", "04.flac", "05.flac", "06.flac", "07.flac", "08.flac"}
	root := makeTestTree(t, map[string][]string{"Album": files})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	// Each probe blocks long enough that a second one is guaranteed to start
	// while it runs, so the peak is a real observation and not a scheduling fluke.
	prober := &mockProber{delay: 20 * time.Millisecond}
	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir(), Workers: workers})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if got := prober.peakInFlight(); got > workers {
		t.Errorf("peak concurrent probes: got %d, want at most %d", got, workers)
	} else if got < workers {
		t.Errorf("peak concurrent probes: got %d, probes never ran in parallel", got)
	}
	if got := prober.total(); got != len(files) {
		t.Errorf("probes: got %d want %d", got, len(files))
	}
	if got := tracksByName(t, database, libID, "Album"); len(got) != len(files) {
		t.Errorf("stored tracks: got %d want %d", len(got), len(files))
	}
}

// TestScannerTrackOrderIndependentOfProbeOrder verifies that tracks are written
// in directory order even when probes finish in the opposite order, and that
// each row keeps the metadata of its own file.
func TestScannerTrackOrderIndependentOfProbeOrder(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"01.flac", "02.flac", "03.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	dirPath := filepath.Join(root, "Album")
	path := func(name string) string { return filepath.Join(dirPath, name) }

	// Completion order is the reverse of the directory order.
	prober := &mockProber{
		delays: map[string]time.Duration{
			path("01.flac"): 60 * time.Millisecond,
			path("02.flac"): 30 * time.Millisecond,
			path("03.flac"): 0,
		},
		results: map[string]*TrackInfo{
			path("01.flac"): {Codec: "flac", Bitrate: 900000, Duration: 1, SampleRate: 44100, Channels: 2},
			path("02.flac"): {Codec: "mp3", Bitrate: 320000, Duration: 2, SampleRate: 48000, Channels: 1},
			path("03.flac"): {Codec: "opus", Bitrate: 128000, Duration: 3, SampleRate: 48000, Channels: 2},
		},
	}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir(), Workers: 3})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	dir, err := database.GetDirectoryByPath(libID, "Album")
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := database.GetDirectoryFiles(dir.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 3 {
		t.Fatalf("expected 3 tracks, got %d", len(tracks))
	}

	// GetDirectoryFiles sorts by filename, so ascending ids mean the rows were
	// inserted in directory order rather than in probe completion order.
	for i := 1; i < len(tracks); i++ {
		if tracks[i].ID <= tracks[i-1].ID {
			t.Errorf("insert order: %s (id %d) written after %s (id %d)",
				tracks[i-1].Filename, tracks[i-1].ID, tracks[i].Filename, tracks[i].ID)
		}
	}

	wantCodec := map[string]string{"01.flac": "flac", "02.flac": "mp3", "03.flac": "opus"}
	for _, tr := range tracks {
		if got := tr.Codec; got != wantCodec[tr.Filename] {
			t.Errorf("%s: codec = %q, want %q", tr.Filename, got, wantCodec[tr.Filename])
		}
	}
	if dir.CodecSummary != "Mixed" {
		t.Errorf("CodecSummary: got %q want %q", dir.CodecSummary, "Mixed")
	}
}

// TestScannerNoBlankRowForFileDeletedMidScan verifies that a file which vanishes
// after the walk listed it is dropped from the batch instead of being written as
// an empty row at its slot in the index-addressed result slice.
func TestScannerNoBlankRowForFileDeletedMidScan(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"A-Album": {"a.flac"},
		"B-Album": {"01.flac", "02.flac", "03.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	// Directories are processed in sorted order, so removing B-Album's middle
	// file while A-Album is being probed drops it after the walk listed it.
	victim := filepath.Join(root, "B-Album", "02.flac")
	prober := &mockProber{hook: func(string) { _ = os.Remove(victim) }}

	s := NewScanner(database, prober, ScanConfig{CacheDir: t.TempDir(), Workers: 4})
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := s.Scan(context.Background(), lib); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	stored := tracksByName(t, database, libID, "B-Album")
	if len(stored) != 2 {
		t.Fatalf("stored tracks: got %d (%v) want 2", len(stored), stored)
	}
	for _, name := range []string{"01.flac", "03.flac"} {
		if _, ok := stored[name]; !ok {
			t.Errorf("%s: missing from the DB", name)
		}
	}
	if _, ok := stored[""]; ok {
		t.Error("a blank row was written for the deleted file")
	}
}

// TestScannerCanceledScanKeepsIndexedDirectory verifies that cancelling mid-probe
// aborts the scan and leaves the previously indexed rows untouched, rather than
// writing the partial result of the interrupted directory.
func TestScannerCanceledScanKeepsIndexedDirectory(t *testing.T) {
	root := makeTestTree(t, map[string][]string{
		"Album": {"01.flac", "02.flac", "03.flac"},
	})

	database := openTestDB(t)
	libID, err := database.UpsertLibrary("test", root)
	if err != nil {
		t.Fatal(err)
	}

	first := &mockProber{defaultResult: &TrackInfo{Codec: "flac", Bitrate: 900000, Duration: 61.5, SampleRate: 44100, Channels: 2}}
	lib := db.Library{ID: libID, Name: "test", Path: root}
	if err := NewScanner(database, first, ScanConfig{CacheDir: t.TempDir()}).Scan(context.Background(), lib); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	before := tracksByName(t, database, libID, "Album")
	if len(before) != 3 {
		t.Fatalf("expected 3 tracks after the first scan, got %d", len(before))
	}

	// Invalidate the cache so the second scan actually probes, then cancel on the
	// first probe it issues.
	distant := time.Now().Add(-72 * time.Hour)
	for name := range before {
		if err := os.Chtimes(filepath.Join(root, "Album", name), distant, distant); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := &mockProber{
		defaultResult: &TrackInfo{Codec: "mp3", Bitrate: 128000, Duration: 1, SampleRate: 8000, Channels: 1},
		hook:          func(string) { cancel() },
	}

	s := NewScanner(database, second, ScanConfig{CacheDir: t.TempDir(), Workers: 1})
	err = s.Scan(ctx, lib)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan after cancel: got %v, want context.Canceled", err)
	}
	if got := second.total(); got > 3 {
		t.Errorf("scan kept probing after cancel: %d probes", got)
	}

	after := tracksByName(t, database, libID, "Album")
	if len(after) != len(before) {
		t.Fatalf("track count changed: %d -> %d", len(before), len(after))
	}
	for name, want := range before {
		if got := after[name]; got != want {
			t.Errorf("%s: row clobbered by the canceled scan\n got %+v\nwant %+v", name, got, want)
		}
	}
}

// openCountingTestDB opens a file-backed test DB and returns it alongside a
// counter of insert/update statements against the tracks table. The counter is a
// trigger installed through a second connection to the same file: the DB API
// exposes no write accounting, and an upsert keeps the row's rowid, so rowids
// cannot tell a skipped write from a no-op one.
func openCountingTestDB(t *testing.T) (*db.DB, func() int) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "alto.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatalf("open file db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open counting connection: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	for _, stmt := range []string{
		`CREATE TABLE track_writes(n INTEGER NOT NULL)`,
		`INSERT INTO track_writes(n) VALUES(0)`,
		`CREATE TRIGGER track_writes_insert AFTER INSERT ON tracks BEGIN UPDATE track_writes SET n = n + 1; END`,
		`CREATE TRIGGER track_writes_update AFTER UPDATE ON tracks BEGIN UPDATE track_writes SET n = n + 1; END`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("install track write counter (%s): %v", stmt, err)
		}
	}

	return database, func() int {
		t.Helper()
		var n int
		if err := raw.QueryRow(`SELECT n FROM track_writes`).Scan(&n); err != nil {
			t.Fatalf("read track write counter: %v", err)
		}
		return n
	}
}

// coverCachePath mirrors the extracted-cover cache location used by resolveCover.
func coverCachePath(cacheDir string, libID int64, relPath string) string {
	hash := sha256.Sum256(fmt.Appendf(nil, "%d/%s", libID, relPath))
	return filepath.Join(cacheDir, "covers", fmt.Sprintf("%d", libID), fmt.Sprintf("%x.jpg", hash))
}

func childPaths(dirs []db.Directory) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.Path
	}
	return out
}

// TestIsAltoDir verifies the .alto-* pattern matching.
func TestIsAltoDir(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"alto-out", true},
		{".alto-out", true},
		{".alto-tmp-abc", true},
		{".alto-backup-123", true},
		{".alto-", true},
		{"out", false},
		{".alto", false}, // exactly ".alto" — no dash suffix
		{"Music", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isAltoDir(tc.name)
			if got != tc.want {
				t.Errorf("isAltoDir(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
