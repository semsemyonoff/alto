package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// --- Progress parsing tests ---

func TestParseFFmpegTime(t *testing.T) {
	tests := []struct {
		line   string
		want   float64
		wantOK bool
	}{
		{"frame=100 fps=0.0 q=-1.0 size=512kB time=00:00:04.16 bitrate=1006.3kbits/s", 4.16, true},
		{"time=01:00:00.00", 3600.0, true},
		{"time=00:01:30.50", 90.5, true},
		{"time=00:00:00.00", 0.0, true},
		{"no time here", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		got, ok := ParseFFmpegTime(tc.line)
		if ok != tc.wantOK {
			t.Errorf("ParseFFmpegTime(%q) ok=%v, want %v", tc.line, ok, tc.wantOK)
		}
		if ok && absDiff(got, tc.want) > 0.001 {
			t.Errorf("ParseFFmpegTime(%q) = %f, want %f", tc.line, got, tc.want)
		}
	}
}

func TestCalcPercent(t *testing.T) {
	tests := []struct {
		elapsed float64
		total   float64
		want    float64
	}{
		{0, 100, 0},
		{50, 100, 50},
		{100, 100, 100},
		{150, 100, 100}, // capped at 100
		{0, 0, 0},       // zero total → 0
	}
	for _, tc := range tests {
		got := CalcPercent(tc.elapsed, tc.total)
		if got != tc.want {
			t.Errorf("CalcPercent(%f, %f) = %f, want %f", tc.elapsed, tc.total, got, tc.want)
		}
	}
}

// --- Command building tests ---

func TestBuildFLACArgs(t *testing.T) {
	in, out := "/in/a.mp3", "/out/a.flac"
	tests := []struct {
		name     string
		preset   Preset
		wantArgs []string
	}{
		{
			name:   "Fast",
			preset: FLACFast,
			wantArgs: []string{
				"ffmpeg", "-i", in,
				"-c:a", "flac", "-compression_level", "0",
				"-map_metadata", "0", "-c:v", "copy",
				"-y", out,
			},
		},
		{
			name:   "Balanced",
			preset: FLACBalanced,
			wantArgs: []string{
				"ffmpeg", "-i", in,
				"-c:a", "flac", "-compression_level", "5",
				"-map_metadata", "0", "-c:v", "copy",
				"-y", out,
			},
		},
		{
			name:   "Max",
			preset: FLACMax,
			wantArgs: []string{
				"ffmpeg", "-i", in,
				"-c:a", "flac", "-compression_level", "8",
				"-map_metadata", "0", "-c:v", "copy",
				"-y", out,
			},
		},
		{
			name:   "No metadata, no cover",
			preset: Preset{Codec: CodecFLAC, CompressionLevel: 5},
			wantArgs: []string{
				"ffmpeg", "-i", in,
				"-c:a", "flac", "-compression_level", "5",
				"-y", out,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildFLACArgs("ffmpeg", in, out, tc.preset)
			if !sliceEqual(got, tc.wantArgs) {
				t.Errorf("got  %v\nwant %v", got, tc.wantArgs)
			}
		})
	}
}

func TestBuildOpusArgs(t *testing.T) {
	in, out := "/in/a.flac", "/out/a.opus"
	tests := []struct {
		name     string
		preset   Preset
		wantArgs []string
	}{
		{
			name:   "Music Balanced",
			preset: OpusMusicBalanced,
			wantArgs: []string{
				"ffmpeg", "-i", in,
				"-c:a", "libopus", "-b:a", "128k",
				"-vbr", "on", "-compression_level", "10",
				"-application", "audio",
				"-map_metadata", "0",
				"-y", out,
			},
		},
		{
			name:   "Music High",
			preset: OpusMusicHigh,
			wantArgs: []string{
				"ffmpeg", "-i", in,
				"-c:a", "libopus", "-b:a", "160k",
				"-vbr", "on", "-compression_level", "10",
				"-application", "audio",
				"-map_metadata", "0",
				"-y", out,
			},
		},
		{
			name:   "Archive Lossy",
			preset: OpusArchiveLossy,
			wantArgs: []string{
				"ffmpeg", "-i", in,
				"-c:a", "libopus", "-b:a", "192k",
				"-vbr", "on", "-compression_level", "10",
				"-application", "audio",
				"-map_metadata", "0",
				"-y", out,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildOpusArgs("ffmpeg", in, out, tc.preset)
			if !sliceEqual(got, tc.wantArgs) {
				t.Errorf("got  %v\nwant %v", got, tc.wantArgs)
			}
		})
	}
}

// --- Output filename tests ---

func TestOutFilename(t *testing.T) {
	tests := []struct {
		name  string
		codec Codec
		want  string
	}{
		{"song.mp3", CodecFLAC, "song.flac"},
		{"song.flac", CodecOpus, "song.opus"},
		{"song.wav", CodecFLAC, "song.flac"},
		{"track.aac", CodecOpus, "track.opus"},
	}
	for _, tc := range tests {
		got := outFilename(tc.name, tc.codec)
		if got != tc.want {
			t.Errorf("outFilename(%q, %s) = %q, want %q", tc.name, tc.codec, got, tc.want)
		}
	}
}

// --- Output dir calculation tests ---

func TestCalcOutputDir(t *testing.T) {
	tests := []struct {
		name string
		job  Job
		want string
	}{
		{
			name: "Shared mode",
			job: Job{
				OutputMode:  OutputShared,
				LibraryRoot: "/library/music",
				LibraryName: "music",
				SourceDir:   "/library/music/Artist/Album",
				OutputDir:   "/out",
			},
			want: "/out/music/Artist/Album",
		},
		{
			name: "Local mode",
			job: Job{
				OutputMode: OutputLocal,
				SourceDir:  "/library/music/Artist/Album",
			},
			want: "/library/music/Artist/Album/alto-out",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := calcOutputDir(tc.job)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- Integration-style tests with mock ffmpegRun ---

func TestTranscodeLocalOut(t *testing.T) {
	srcDir := t.TempDir()

	// Create source audio and non-audio files.
	for _, name := range []string{"a.mp3", "b.mp3", "cover.jpg", "info.txt"} {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("content:"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var capturedArgs [][]string
	var verifiedPaths []string
	e := &Engine{
		ffmpegBin: "ffmpeg",
		ffmpegRun: func(ctx context.Context, args []string, progressFn func(string)) error {
			capturedArgs = append(capturedArgs, args)
			// Create the output file so the job sees a result.
			return os.WriteFile(args[len(args)-1], []byte("transcoded"), 0o644)
		},
		probeFile: func(ctx context.Context, path string) error {
			verifiedPaths = append(verifiedPaths, path)
			return nil
		},
		diskAvail: func(string) (uint64, error) { return 1 << 30, nil },
	}

	job := Job{
		ID:         "local-test",
		SourceDir:  srcDir,
		OutputMode: OutputLocal,
		Preset:     FLACBalanced,
		Files: []FileInfo{
			{Name: "a.mp3", Duration: 300, Size: 10_000_000},
			{Name: "b.mp3", Duration: 200, Size: 8_000_000},
		},
	}

	if err := e.Transcode(context.Background(), job, nil); err != nil {
		t.Fatalf("Transcode: %v", err)
	}

	expectedOutDir := filepath.Join(srcDir, LocalOutputDirName)

	// ffmpeg called once per audio file.
	if len(capturedArgs) != 2 {
		t.Fatalf("expected 2 ffmpeg calls, got %d", len(capturedArgs))
	}

	// Verify output paths.
	wantPaths := []string{
		filepath.Join(expectedOutDir, "a.flac"),
		filepath.Join(expectedOutDir, "b.flac"),
	}
	for i, want := range wantPaths {
		got := capturedArgs[i][len(capturedArgs[i])-1]
		if got != want {
			t.Errorf("call %d output = %q, want %q", i, got, want)
		}
	}
	if len(verifiedPaths) != len(wantPaths) {
		t.Fatalf("expected %d verification calls, got %d", len(wantPaths), len(verifiedPaths))
	}
	for i, want := range wantPaths {
		if verifiedPaths[i] != want {
			t.Errorf("verify call %d path = %q, want %q", i, verifiedPaths[i], want)
		}
	}

	// Non-audio files copied.
	for _, name := range []string{"cover.jpg", "info.txt"} {
		if _, err := os.Stat(filepath.Join(expectedOutDir, name)); err != nil {
			t.Errorf("non-audio file %s not copied: %v", name, err)
		}
	}
	// Audio files NOT copied directly.
	for _, name := range []string{"a.mp3", "b.mp3"} {
		if _, err := os.Stat(filepath.Join(expectedOutDir, name)); err == nil {
			t.Errorf("audio file %s should not be copied directly", name)
		}
	}
}

func TestTranscodeSharedOut(t *testing.T) {
	tmpDir := t.TempDir()
	libraryRoot := filepath.Join(tmpDir, "library")
	srcDir := filepath.Join(libraryRoot, "Artist", "Album")
	outputDir := filepath.Join(tmpDir, "out")

	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "track.mp3"), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}

	var capturedOutput string
	e := &Engine{
		ffmpegBin: "ffmpeg",
		ffmpegRun: func(ctx context.Context, args []string, progressFn func(string)) error {
			capturedOutput = args[len(args)-1]
			return os.WriteFile(capturedOutput, []byte("transcoded"), 0o644)
		},
		probeFile: func(ctx context.Context, path string) error { return nil },
		diskAvail: func(string) (uint64, error) { return 1 << 30, nil },
	}

	job := Job{
		ID:          "shared-test",
		LibraryRoot: libraryRoot,
		LibraryName: "music",
		SourceDir:   srcDir,
		OutputMode:  OutputShared,
		OutputDir:   outputDir,
		Preset:      OpusMusicHigh,
		Files:       []FileInfo{{Name: "track.mp3", Duration: 60}},
	}

	if err := e.Transcode(context.Background(), job, nil); err != nil {
		t.Fatalf("Transcode: %v", err)
	}

	want := filepath.Join(outputDir, "music", "Artist", "Album", "track.opus")
	if capturedOutput != want {
		t.Errorf("output path = %q, want %q", capturedOutput, want)
	}
}

func TestTranscodeReplaceSuccess(t *testing.T) {
	srcDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "song.mp3"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &Engine{
		ffmpegBin: "ffmpeg",
		ffmpegRun: func(ctx context.Context, args []string, progressFn func(string)) error {
			return os.WriteFile(args[len(args)-1], []byte("transcoded"), 0o644)
		},
		probeFile: func(ctx context.Context, path string) error { return nil },
		diskAvail: func(string) (uint64, error) { return 1 << 30, nil },
	}

	job := Job{
		ID:         "success-test",
		SourceDir:  srcDir,
		Preset:     FLACBalanced,
		OutputMode: OutputReplace,
		Files:      []FileInfo{{Name: "song.mp3", Duration: 100}},
	}

	if err := e.Transcode(context.Background(), job, nil); err != nil {
		t.Fatalf("Transcode: %v", err)
	}

	// Original is gone.
	if _, err := os.Stat(filepath.Join(srcDir, "song.mp3")); err == nil {
		t.Error("original song.mp3 should be gone after replace")
	}

	// Transcoded file exists.
	data, err := os.ReadFile(filepath.Join(srcDir, "song.flac"))
	if err != nil {
		t.Fatalf("song.flac not found: %v", err)
	}
	if string(data) != "transcoded" {
		t.Errorf("unexpected content: %q", data)
	}

	// Backup dir removed.
	if _, err := os.Stat(filepath.Join(srcDir, ".alto-backup-success-test")); !errors.Is(err, os.ErrNotExist) {
		t.Error("backup dir should be removed after successful replace")
	}
}

func TestTranscodeReplaceRollback(t *testing.T) {
	srcDir := t.TempDir()

	// Three files; transcode fails on the third.
	files := []string{"a.mp3", "b.mp3", "c.mp3"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("original:"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	callCount := 0
	e := &Engine{
		ffmpegBin: "ffmpeg",
		ffmpegRun: func(ctx context.Context, args []string, progressFn func(string)) error {
			callCount++
			if callCount == 3 {
				return fmt.Errorf("simulated ffmpeg failure")
			}
			return os.WriteFile(args[len(args)-1], []byte("transcoded"), 0o644)
		},
		probeFile: func(ctx context.Context, path string) error { return nil },
		diskAvail: func(string) (uint64, error) { return 1 << 30, nil },
	}

	job := Job{
		ID:         "rollback-test",
		SourceDir:  srcDir,
		Preset:     FLACBalanced,
		OutputMode: OutputReplace,
		Files: []FileInfo{
			{Name: "a.mp3", Duration: 100},
			{Name: "b.mp3", Duration: 100},
			{Name: "c.mp3", Duration: 100},
		},
	}

	err := e.Transcode(context.Background(), job, nil)
	if err == nil {
		t.Fatal("expected error from failed transcode")
	}

	// All originals are restored.
	for _, name := range files {
		data, readErr := os.ReadFile(filepath.Join(srcDir, name))
		if readErr != nil {
			t.Errorf("original %s missing after rollback: %v", name, readErr)
			continue
		}
		if string(data) != "original:"+name {
			t.Errorf("%s content after rollback: got %q, want %q", name, data, "original:"+name)
		}
	}

	// Transcoded files for successfully replaced entries are gone.
	for _, name := range []string{"a.flac", "b.flac"} {
		if _, err := os.Stat(filepath.Join(srcDir, name)); err == nil {
			t.Errorf("output %s should be removed after rollback", name)
		}
	}

	// Backup dir cleaned up.
	if _, err := os.Stat(filepath.Join(srcDir, ".alto-backup-rollback-test")); !errors.Is(err, os.ErrNotExist) {
		t.Error("backup dir should be removed after rollback")
	}
}

func TestTranscodeReplaceContextCancel(t *testing.T) {
	srcDir := t.TempDir()

	for _, name := range []string{"a.mp3", "b.mp3"} {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("original:"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	callCount := 0
	e := &Engine{
		ffmpegBin: "ffmpeg",
		ffmpegRun: func(ctx context.Context, args []string, progressFn func(string)) error {
			callCount++
			if callCount == 1 {
				return os.WriteFile(args[len(args)-1], []byte("transcoded"), 0o644)
			}
			cancel()
			return ctx.Err()
		},
		probeFile: func(ctx context.Context, path string) error { return nil },
		diskAvail: func(string) (uint64, error) { return 1 << 30, nil },
	}

	job := Job{
		ID:         "cancel-test",
		SourceDir:  srcDir,
		Preset:     FLACBalanced,
		OutputMode: OutputReplace,
		Files: []FileInfo{
			{Name: "a.mp3", Duration: 100},
			{Name: "b.mp3", Duration: 100},
		},
	}

	if err := e.Transcode(ctx, job, nil); err == nil {
		t.Fatal("expected error from cancelled context")
	}

	// a.mp3 must be restored.
	data, err := os.ReadFile(filepath.Join(srcDir, "a.mp3"))
	if err != nil {
		t.Fatalf("a.mp3 missing after cancel: %v", err)
	}
	if string(data) != "original:a.mp3" {
		t.Errorf("a.mp3 content after cancel: %q", data)
	}
}

func TestTranscodeProgress(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "track.mp3"), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &Engine{
		ffmpegBin: "ffmpeg",
		ffmpegRun: func(ctx context.Context, args []string, progressFn func(string)) error {
			// Simulate two progress updates from ffmpeg stderr.
			progressFn("frame=10 size=100kB time=00:00:30.00 bitrate=128kbits/s")
			progressFn("frame=20 size=200kB time=00:01:00.00 bitrate=128kbits/s")
			return os.WriteFile(args[len(args)-1], []byte("done"), 0o644)
		},
		probeFile: func(ctx context.Context, path string) error { return nil },
		diskAvail: func(string) (uint64, error) { return 1 << 30, nil },
	}

	job := Job{
		ID:         "progress-test",
		SourceDir:  srcDir,
		OutputMode: OutputLocal,
		Preset:     FLACBalanced,
		Files:      []FileInfo{{Name: "track.mp3", Duration: 120}},
	}

	progress := make(chan ProgressReport, 10)
	if err := e.Transcode(context.Background(), job, progress); err != nil {
		t.Fatalf("Transcode: %v", err)
	}
	close(progress)

	var reports []ProgressReport
	for r := range progress {
		reports = append(reports, r)
	}

	if len(reports) != 2 {
		t.Fatalf("expected 2 progress reports, got %d", len(reports))
	}
	// 30s / 120s = 25%
	if absDiff(reports[0].FilePercent, 25.0) > 0.1 {
		t.Errorf("first report percent = %f, want ~25", reports[0].FilePercent)
	}
	// 60s / 120s = 50%
	if absDiff(reports[1].FilePercent, 50.0) > 0.1 {
		t.Errorf("second report percent = %f, want ~50", reports[1].FilePercent)
	}
	if reports[0].CurrentFile != "track.mp3" {
		t.Errorf("CurrentFile = %q, want %q", reports[0].CurrentFile, "track.mp3")
	}
}

// --- Non-audio file copying tests ---

func TestCopyNonAudioFiles(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	files := map[string]bool{
		"cover.jpg":  false, // not audio — should be copied
		"info.txt":   false, // not audio — should be copied
		"track.flac": true,  // audio — should NOT be copied
		"song.mp3":   true,  // audio — should NOT be copied
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	copyNonAudioFiles(srcDir, dstDir)

	for name, isAudio := range files {
		_, err := os.Stat(filepath.Join(dstDir, name))
		if isAudio && err == nil {
			t.Errorf("audio file %s should not be copied", name)
		}
		if !isAudio && err != nil {
			t.Errorf("non-audio file %s should be copied: %v", name, err)
		}
	}
}

// --- Disk space estimation tests ---

func TestEstimateOutputBytes(t *testing.T) {
	tests := []struct {
		name string
		job  Job
		want uint64
	}{
		{
			name: "Opus 128k 10s",
			job: Job{
				Preset: OpusMusicBalanced, // 128k
				Files:  []FileInfo{{Duration: 10}},
			},
			want: 10 * 128_000 / 8, // 160_000 bytes
		},
		{
			name: "Opus 160k 30s two files",
			job: Job{
				Preset: OpusMusicHigh, // 160k
				Files: []FileInfo{
					{Duration: 30},
					{Duration: 30},
				},
			},
			want: 2 * 30 * 160_000 / 8,
		},
		{
			name: "FLAC uses file size",
			job: Job{
				Preset: FLACBalanced,
				Files:  []FileInfo{{Size: 5_000_000}},
			},
			want: 5_000_000,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := estimateOutputBytes(tc.job)
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseOpusBitrateBps(t *testing.T) {
	tests := []struct {
		bitrate string
		want    uint64
	}{
		{"128k", 128_000},
		{"160k", 160_000},
		{"192k", 192_000},
	}
	for _, tc := range tests {
		got := parseOpusBitrateBps(tc.bitrate)
		if got != tc.want {
			t.Errorf("parseOpusBitrateBps(%q) = %d, want %d", tc.bitrate, got, tc.want)
		}
	}
}

// TestDiskSpaceWarning verifies that warnDiskSpace does not fail the job even
// when available space is less than 2× the estimate.
func TestDiskSpaceWarning(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "a.flac"), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}

	diskCallCount := 0
	e := &Engine{
		ffmpegBin: "ffmpeg",
		ffmpegRun: func(ctx context.Context, args []string, progressFn func(string)) error {
			return os.WriteFile(args[len(args)-1], []byte("transcoded"), 0o644)
		},
		probeFile: func(ctx context.Context, path string) error { return nil },
		diskAvail: func(string) (uint64, error) {
			diskCallCount++
			return 1, nil // almost no space
		},
	}

	job := Job{
		ID:         "disk-warn",
		SourceDir:  srcDir,
		Preset:     FLACBalanced,
		OutputMode: OutputReplace,
		Files:      []FileInfo{{Name: "a.flac", Size: 10_000_000, Duration: 30}},
	}

	// Should succeed despite low disk — it's a warning, not an error.
	if err := e.Transcode(context.Background(), job, nil); err != nil {
		t.Fatalf("Transcode: %v", err)
	}
	if diskCallCount == 0 {
		t.Error("disk space check not called")
	}
}

// --- Helpers ---

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func absDiff(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
