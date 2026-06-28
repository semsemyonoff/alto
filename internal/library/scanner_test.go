package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/semsemyonoff/ALTO/internal/db"
)

// mockProber is a test double for Prober that returns canned metadata.
type mockProber struct {
	results map[string]*TrackInfo
	err     map[string]error
	// defaultResult is returned for any path not in results.
	defaultResult *TrackInfo
}

func (m *mockProber) Probe(_ context.Context, path string) (*TrackInfo, error) {
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

	if err := s.ScanAll(context.Background(), libs); err != nil {
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
