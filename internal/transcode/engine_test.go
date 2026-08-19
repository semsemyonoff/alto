package transcode

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
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

// The key lists are spelled out here rather than read from the production
// variables, so widening what gets stripped has to be a deliberate edit in both
// places instead of the test asserting the implementation against itself.
var (
	wantKeysAnySource = []string{
		"major_brand", "minor_version", "compatible_brands", "handler_name", "vendor_id",
	}
	wantKeysMP4Source = []string{
		"major_brand", "minor_version", "compatible_brands", "handler_name", "vendor_id",
		"language", "creation_time",
	}
)

// wantCopyMetadataArgs builds the expected metadata block for a key list.
func wantCopyMetadataArgs(keys []string) []string {
	args := []string{"-map_metadata", "0", "-map_metadata", "0:s:a:0"}
	for _, k := range keys {
		args = append(args, "-metadata", k+"=")
	}
	for _, k := range keys {
		args = append(args, "-metadata:s", k+"=")
	}
	return args
}

// joinArgs flattens argument groups into one expected command line.
func joinArgs(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// flacHead returns the leading arguments for a FLAC encode of input.
func flacHead(input string, level string, cover bool) []string {
	args := []string{"ffmpeg", "-i", input, "-map", "0:a:0"}
	if cover {
		args = append(args, "-map", "0:v:0?")
	}
	return append(args, "-c:a", "flac", "-compression_level", level)
}

func TestBuildFLACArgs(t *testing.T) {
	out := "/out/a.flac"
	const in = "/in/a.mp3"
	tests := []struct {
		name     string
		input    string
		preset   Preset
		wantArgs []string
	}{
		{
			name:   "Fast",
			input:  in,
			preset: FLACFast,
			wantArgs: joinArgs(
				flacHead(in, "0", true),
				wantCopyMetadataArgs(wantKeysAnySource),
				[]string{"-c:v", "copy", "-y", out},
			),
		},
		{
			name:   "Balanced",
			input:  in,
			preset: FLACBalanced,
			wantArgs: joinArgs(
				flacHead(in, "5", true),
				wantCopyMetadataArgs(wantKeysAnySource),
				[]string{"-c:v", "copy", "-y", out},
			),
		},
		{
			name:   "Max",
			input:  in,
			preset: FLACMax,
			wantArgs: joinArgs(
				flacHead(in, "8", true),
				wantCopyMetadataArgs(wantKeysAnySource),
				[]string{"-c:v", "copy", "-y", out},
			),
		},
		{
			name:   "MP4 source also strips the ambiguous keys",
			input:  "/in/a.M4A",
			preset: FLACBalanced,
			wantArgs: joinArgs(
				flacHead("/in/a.M4A", "5", true),
				wantCopyMetadataArgs(wantKeysMP4Source),
				[]string{"-c:v", "copy", "-y", out},
			),
		},
		{
			name:   "No metadata, no cover",
			input:  in,
			preset: Preset{Codec: CodecFLAC, CompressionLevel: 5},
			wantArgs: joinArgs(
				flacHead(in, "5", false),
				[]string{"-map_metadata", "-1", "-map_chapters", "-1", "-y", out},
			),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildFLACArgs("ffmpeg", tc.input, out, tc.preset)
			if !sliceEqual(got, tc.wantArgs) {
				t.Errorf("got  %v\nwant %v", got, tc.wantArgs)
			}
		})
	}
}

// opusHead returns the codec arguments every Opus preset shares.
func opusHead(input, bitrate string) []string {
	return []string{
		"ffmpeg", "-i", input,
		"-map", "0:a:0",
		"-c:a", "libopus", "-b:a", bitrate,
		"-vbr", "on", "-compression_level", "10",
		"-application", "audio",
	}
}

func TestBuildOpusArgs(t *testing.T) {
	out := "/out/a.opus"
	const in = "/in/a.flac"
	tests := []struct {
		name     string
		input    string
		preset   Preset
		wantArgs []string
	}{
		{
			name:   "Music Balanced",
			input:  in,
			preset: OpusMusicBalanced,
			wantArgs: joinArgs(
				opusHead(in, "128k"),
				wantCopyMetadataArgs(wantKeysAnySource),
				[]string{"-y", out},
			),
		},
		{
			name:   "Music High",
			input:  in,
			preset: OpusMusicHigh,
			wantArgs: joinArgs(
				opusHead(in, "160k"),
				wantCopyMetadataArgs(wantKeysAnySource),
				[]string{"-y", out},
			),
		},
		{
			name:   "Archive Lossy",
			input:  in,
			preset: OpusArchiveLossy,
			wantArgs: joinArgs(
				opusHead(in, "192k"),
				wantCopyMetadataArgs(wantKeysAnySource),
				[]string{"-y", out},
			),
		},
		{
			name:   "MP4 source also strips the ambiguous keys",
			input:  "/in/a.m4a",
			preset: OpusMusicHigh,
			wantArgs: joinArgs(
				opusHead("/in/a.m4a", "160k"),
				wantCopyMetadataArgs(wantKeysMP4Source),
				[]string{"-y", out},
			),
		},
		{
			name:   "No metadata",
			input:  in,
			preset: Preset{Codec: CodecOpus, CompressionLevel: 10, Bitrate: "128k"},
			wantArgs: joinArgs(
				opusHead(in, "128k"),
				[]string{"-map_metadata", "-1", "-map_chapters", "-1", "-y", out},
			),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildOpusArgs("ffmpeg", tc.input, out, tc.preset)
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
		got := OutFilename(tc.name, tc.codec)
		if got != tc.want {
			t.Errorf("OutFilename(%q, %s) = %q, want %q", tc.name, tc.codec, got, tc.want)
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

// TestTranscodeCancelSurfacesContextCanceled verifies that when the context is
// canceled mid-transcode, Transcode reports context.Canceled even though the
// killed ffmpeg process returns an error that does NOT wrap it (as the real
// exec.CommandContext path does with "signal: killed"). The queue relies on
// this to map cancellation to a "canceled" status rather than "failed".
func TestTranscodeCancelSurfacesContextCanceled(t *testing.T) {
	for _, mode := range []string{"shared", "replace"} {
		t.Run(mode, func(t *testing.T) {
			libraryRoot := t.TempDir()
			srcDir := filepath.Join(libraryRoot, "Artist", "Album")
			if err := os.MkdirAll(srcDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(srcDir, "a.mp3"), []byte("audio"), 0o644); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			e := &Engine{
				ffmpegBin: "ffmpeg",
				ffmpegRun: func(ctx context.Context, args []string, progressFn func(string)) error {
					// Simulate the process being killed by cancellation: the
					// context is done, but the returned error does not wrap
					// context.Canceled.
					cancel()
					return errors.New("signal: killed")
				},
				probeFile: func(ctx context.Context, path string) error { return nil },
				diskAvail: func(string) (uint64, error) { return 1 << 30, nil },
			}

			job := Job{
				ID:          "cancel-test",
				LibraryRoot: libraryRoot,
				LibraryName: "music",
				SourceDir:   srcDir,
				Preset:      FLACBalanced,
				Files:       []FileInfo{{Name: "a.mp3", Duration: 100, Size: 1_000_000}},
			}
			if mode == "shared" {
				job.OutputMode = OutputShared
				job.OutputDir = t.TempDir()
			} else {
				job.OutputMode = OutputReplace
			}

			err := e.Transcode(ctx, job, nil)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Transcode error = %v, want context.Canceled", err)
			}
		})
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

	if err := copyNonAudioFiles(context.Background(), srcDir, dstDir); err != nil {
		t.Fatalf("copyNonAudioFiles: %v", err)
	}

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

// --- Pass-through copying tests ---

// fileFingerprint records the content hash and modification time of a file, so a
// test can assert a job left it byte-identical and untouched.
type fileFingerprint struct {
	sum   [32]byte
	mtime time.Time
}

func fingerprintDir(t *testing.T, dir string, names []string) map[string]fileFingerprint {
	t.Helper()
	out := make(map[string]fileFingerprint, len(names))
	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		out[name] = fileFingerprint{sum: sha256.Sum256(data), mtime: st.ModTime()}
	}
	return out
}

func assertUnchanged(t *testing.T, dir string, before map[string]fileFingerprint) {
	t.Helper()
	after := fingerprintDir(t, dir, sortedKeys(before))
	for name, want := range before {
		got := after[name]
		if got.sum != want.sum {
			t.Errorf("source %s was modified: content hash changed", name)
		}
		if !got.mtime.Equal(want.mtime) {
			t.Errorf("source %s was touched: mtime %v, want %v", name, got.mtime, want.mtime)
		}
	}
}

func sortedKeys(m map[string]fileFingerprint) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestTranscodePassthroughCopies covers the mixed-album case: the lossless
// tracks are transcoded, the lossy ones are copied verbatim, and every source
// file is byte-identical afterwards.
func TestTranscodePassthroughCopies(t *testing.T) {
	srcDir := t.TempDir()
	outputDir := t.TempDir()

	sources := map[string]string{
		"01 A.flac": "flac-content-a",
		"02 B.flac": "flac-content-b",
		"03 C.mp3":  "mp3-content-c",
		"04 D.mp3":  "mp3-content-d",
		"cover.jpg": "jpeg-bytes",
	}
	for name, content := range sources {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := fingerprintDir(t, srcDir, []string{"01 A.flac", "02 B.flac", "03 C.mp3", "04 D.mp3", "cover.jpg"})

	var reports []ProgressReport
	progress := make(chan ProgressReport, 10)
	e := &Engine{
		ffmpegBin: "ffmpeg",
		ffmpegRun: func(ctx context.Context, args []string, progressFn func(string)) error {
			progressFn("time=00:00:30.00")
			return os.WriteFile(args[len(args)-1], []byte("transcoded"), 0o644)
		},
		probeFile: func(ctx context.Context, path string) error { return nil },
		diskAvail: func(string) (uint64, error) { return 1 << 30, nil },
	}

	job := Job{
		ID:          "passthrough-test",
		LibraryRoot: srcDir,
		LibraryName: "music",
		SourceDir:   srcDir,
		OutputMode:  OutputShared,
		OutputDir:   outputDir,
		Preset:      OpusMusicHigh,
		Files: []FileInfo{
			{Name: "01 A.flac", Duration: 60},
			{Name: "02 B.flac", Duration: 60},
		},
		Passthrough: []FileInfo{
			{Name: "03 C.mp3", Duration: 60},
			{Name: "04 D.mp3", Duration: 60},
		},
	}

	if err := e.Transcode(context.Background(), job, progress); err != nil {
		t.Fatalf("Transcode: %v", err)
	}
	close(progress)
	for r := range progress {
		reports = append(reports, r)
	}

	outDir := filepath.Join(outputDir, "music")

	// Transcoded outputs exist.
	for _, name := range []string{"01 A.opus", "02 B.opus"} {
		data, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Errorf("transcoded output %s missing: %v", name, err)
			continue
		}
		if string(data) != "transcoded" {
			t.Errorf("%s content = %q, want %q", name, data, "transcoded")
		}
	}

	// Pass-through files are copied byte-for-byte under their original names.
	for _, name := range []string{"03 C.mp3", "04 D.mp3"} {
		data, err := os.ReadFile(filepath.Join(outDir, name))
		if err != nil {
			t.Errorf("pass-through %s missing from output: %v", name, err)
			continue
		}
		if string(data) != sources[name] {
			t.Errorf("pass-through %s content = %q, want %q", name, data, sources[name])
		}
	}

	// The pass-through sources were not transcoded into the output as well.
	for _, name := range []string{"03 C.opus", "04 D.opus"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err == nil {
			t.Errorf("pass-through file should not be transcoded: %s exists", name)
		}
	}

	// Non-audio copying still runs after the pass-through copies.
	if _, err := os.Stat(filepath.Join(outDir, "cover.jpg")); err != nil {
		t.Errorf("non-audio file not copied: %v", err)
	}

	// Progress counts the transcoded set only.
	if len(reports) != 2 {
		t.Fatalf("expected 2 progress reports, got %d", len(reports))
	}
	for i, r := range reports {
		if r.TotalFiles != 2 {
			t.Errorf("report %d TotalFiles = %d, want 2 (transcoded set only)", i, r.TotalFiles)
		}
	}

	assertUnchanged(t, srcDir, before)
}

// TestTranscodePassthroughCopyFailureFailsJob pins the difference from
// copyNonAudioFiles: an explicitly requested file that cannot be copied fails
// the whole job rather than being logged and skipped.
func TestTranscodePassthroughCopyFailureFailsJob(t *testing.T) {
	srcDir := t.TempDir()
	outputDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(srcDir, "a.flac"), []byte("audio"), 0o644); err != nil {
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
		ID:          "passthrough-fail",
		LibraryRoot: srcDir,
		LibraryName: "music",
		SourceDir:   srcDir,
		OutputMode:  OutputShared,
		OutputDir:   outputDir,
		Preset:      OpusMusicHigh,
		Files:       []FileInfo{{Name: "a.flac", Duration: 60}},
		// Never written to disk: the copy cannot succeed.
		Passthrough: []FileInfo{{Name: "gone.mp3", Duration: 60}},
	}

	err := e.Transcode(context.Background(), job, nil)
	if err == nil {
		t.Fatal("expected the job to fail when a pass-through copy fails")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want it to wrap os.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), "gone.mp3") {
		t.Errorf("error %v should name the offending file", err)
	}
}

// TestTranscodePassthroughRefusesSymlink pins the same guard copyNonAudioFiles
// applies by skipping symlinks: the scanner never indexes one, so a symlink
// here appeared after indexing and following it would copy a file from outside
// the library into the output.
func TestTranscodePassthroughRefusesSymlink(t *testing.T) {
	srcDir := t.TempDir()
	outputDir := t.TempDir()
	outsideDir := t.TempDir()

	outside := filepath.Join(outsideDir, "secret.mp3")
	if err := os.WriteFile(outside, []byte("outside the library"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "a.flac"), []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(srcDir, "b.mp3")); err != nil {
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
		ID:          "passthrough-symlink",
		LibraryRoot: srcDir,
		LibraryName: "music",
		SourceDir:   srcDir,
		OutputMode:  OutputShared,
		OutputDir:   outputDir,
		Preset:      OpusMusicHigh,
		Files:       []FileInfo{{Name: "a.flac", Duration: 60}},
		Passthrough: []FileInfo{{Name: "b.mp3", Duration: 60}},
	}

	err := e.Transcode(context.Background(), job, nil)
	if err == nil {
		t.Fatal("expected the job to fail on a symlinked pass-through source")
	}
	if !strings.Contains(err.Error(), "b.mp3") {
		t.Errorf("error %v should name the offending file", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "music", "b.mp3")); !os.IsNotExist(statErr) {
		t.Error("the symlink target was copied into the output directory")
	}
}

// TestTranscodePassthroughCancellation pins that a job canceled during the
// pass-through phase reports context.Canceled, so the queue marks it "canceled"
// rather than "failed".
func TestTranscodePassthroughCancellation(t *testing.T) {
	srcDir := t.TempDir()
	outputDir := t.TempDir()

	for _, name := range []string{"a.flac", "b.mp3"} {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("content:"+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{
		ffmpegBin: "ffmpeg",
		ffmpegRun: func(ctx context.Context, args []string, progressFn func(string)) error {
			return os.WriteFile(args[len(args)-1], []byte("transcoded"), 0o644)
		},
		// Cancel once the transcode phase is done, so the pass-through loop is
		// the first thing to observe it.
		probeFile: func(ctx context.Context, path string) error { cancel(); return nil },
		diskAvail: func(string) (uint64, error) { return 1 << 30, nil },
	}

	job := Job{
		ID:          "passthrough-cancel",
		LibraryRoot: srcDir,
		LibraryName: "music",
		SourceDir:   srcDir,
		OutputMode:  OutputShared,
		OutputDir:   outputDir,
		Preset:      OpusMusicHigh,
		Files:       []FileInfo{{Name: "a.flac", Duration: 60}},
		Passthrough: []FileInfo{{Name: "b.mp3", Duration: 60}},
	}

	err := e.Transcode(ctx, job, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(filepath.Join(outputDir, "music", "b.mp3")); !os.IsNotExist(statErr) {
		t.Error("a canceled job should not have copied the pass-through file")
	}
}

// TestTranscodeEmptyPassthroughUnchanged asserts the default (no pass-through)
// output is exactly what it was before the feature: audio sources are never
// copied into the output directory.
func TestTranscodeEmptyPassthroughUnchanged(t *testing.T) {
	srcDir := t.TempDir()
	outputDir := t.TempDir()

	for _, name := range []string{"a.flac", "b.mp3", "cover.jpg"} {
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte("content:"+name), 0o644); err != nil {
			t.Fatal(err)
		}
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
		ID:          "no-passthrough",
		LibraryRoot: srcDir,
		LibraryName: "music",
		SourceDir:   srcDir,
		OutputMode:  OutputShared,
		OutputDir:   outputDir,
		Preset:      OpusMusicHigh,
		Files:       []FileInfo{{Name: "a.flac", Duration: 60}},
	}

	if err := e.Transcode(context.Background(), job, nil); err != nil {
		t.Fatalf("Transcode: %v", err)
	}

	outDir := filepath.Join(outputDir, "music")
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	sort.Strings(got)
	want := []string{"a.opus", "cover.jpg"}
	if !sliceEqual(got, want) {
		t.Errorf("output dir contents = %v, want %v", got, want)
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
