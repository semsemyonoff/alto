package transcode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// audioExtensions is the set of file extensions recognised as audio tracks.
var audioExtensions = map[string]bool{
	".flac": true, ".opus": true, ".ogg": true, ".mp3": true,
	".wav": true, ".aac": true, ".m4a": true, ".wma": true,
	".alac": true, ".ape": true, ".wv": true,
}

// Job describes a single transcoding request.
type Job struct {
	// ID is a unique identifier used for naming temp and backup directories.
	ID string
	// LibraryName is the library slug (filesystem-safe) used in shared output paths.
	LibraryName string
	// LibraryRoot is the absolute path of the library root.
	LibraryRoot string
	// SourceDir is the absolute path of the audio directory to transcode.
	SourceDir string
	// Files lists the audio files within SourceDir to transcode.
	Files []FileInfo
	// Preset holds codec and quality parameters.
	Preset Preset
	// OutputMode determines where output files are placed.
	OutputMode OutputMode
	// OutputDir is the shared output directory (used by OutputShared only).
	OutputDir string
}

// FileInfo describes a single audio file in a Job.
type FileInfo struct {
	Name     string
	Duration float64 // seconds, from ffprobe — used for progress calculation
	Size     int64   // bytes — used for FLAC disk space estimation
}

// Engine executes ffmpeg transcoding jobs.
type Engine struct {
	ffmpegBin  string
	ffprobeBin string
	// ffmpegRun is replaceable in tests.
	ffmpegRun func(ctx context.Context, args []string, progressFn func(string)) error
	// probeFile verifies a file is a valid audio file; replaceable in tests.
	probeFile func(ctx context.Context, path string) error
	// diskAvail returns available bytes at path; replaceable in tests.
	diskAvail func(path string) (uint64, error)
}

// NewEngine creates an Engine that uses the system ffmpeg binary.
func NewEngine() *Engine {
	e := &Engine{ffmpegBin: "ffmpeg", ffprobeBin: "ffprobe"}
	e.ffmpegRun = e.execFfmpeg
	e.probeFile = e.execFfprobe
	e.diskAvail = defaultDiskAvail
	return e
}

// execFfprobe runs ffprobe on path to verify it is a valid audio file.
func (e *Engine) execFfprobe(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, e.ffprobeBin, "-v", "quiet", "-i", path)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffprobe verification failed for %q: %w", path, err)
	}
	return nil
}

func defaultDiskAvail(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	//nolint:gosec // Bavail and Bsize are non-negative on all supported platforms.
	return st.Bavail * uint64(st.Bsize), nil
}

// Transcode runs the job and sends ProgressReports to progress (may be nil).
// It returns when all files have been transcoded or on the first error.
func (e *Engine) Transcode(ctx context.Context, job Job, progress chan<- ProgressReport) error {
	if job.OutputMode == OutputReplace {
		e.warnDiskSpace(job)
		return e.transcodeReplace(ctx, job, progress)
	}
	outDir, err := calcOutputDir(job)
	if err != nil {
		return fmt.Errorf("calculate output dir: %w", err)
	}
	return e.transcodeToDir(ctx, job, outDir, progress)
}

// calcOutputDir returns the destination directory for Shared and Local modes.
func calcOutputDir(job Job) (string, error) {
	switch job.OutputMode {
	case OutputShared:
		if job.OutputDir == "" {
			return "", fmt.Errorf("output dir not configured for shared mode")
		}
		rel, err := filepath.Rel(job.LibraryRoot, job.SourceDir)
		if err != nil {
			return "", fmt.Errorf("rel path from library root: %w", err)
		}
		out := filepath.Join(job.OutputDir, job.LibraryName, rel)
		// Safety: ensure the computed path stays within job.OutputDir.
		cleanBase := filepath.Clean(job.OutputDir)
		cleanOut := filepath.Clean(out)
		if cleanOut != cleanBase && !strings.HasPrefix(cleanOut, cleanBase+string(filepath.Separator)) {
			return "", fmt.Errorf("computed output path escapes output dir")
		}
		return out, nil
	case OutputLocal:
		return filepath.Join(job.SourceDir, LocalOutputDirName), nil
	default:
		return "", fmt.Errorf("unsupported output mode for calcOutputDir: %s", job.OutputMode)
	}
}

// outFilename returns the output filename for a given input filename, with the
// file extension changed to match the target codec.
func outFilename(name string, codec Codec) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	switch codec {
	case CodecFLAC:
		return base + ".flac"
	case CodecOpus:
		return base + ".opus"
	default:
		return name
	}
}

// transcodeToDir transcodes all files in the job to outDir, then copies non-audio files.
func (e *Engine) transcodeToDir(ctx context.Context, job Job, outDir string, progress chan<- ProgressReport) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create output dir %s: %w", outDir, err)
	}
	for i, fi := range job.Files {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		inPath := filepath.Join(job.SourceDir, fi.Name)
		outPath := filepath.Join(outDir, outFilename(fi.Name, job.Preset.Codec))
		args := buildArgs(e.ffmpegBin, inPath, outPath, job.Preset)
		slog.Info("transcoding file", "file", fi.Name, "output", outPath)
		if err := e.ffmpegRun(ctx, args, makeProgressFn(fi, i, len(job.Files), progress)); err != nil {
			return fmt.Errorf("transcode %s: %w", fi.Name, err)
		}
		if err := e.probeFile(ctx, outPath); err != nil {
			return fmt.Errorf("output verification failed for %s: %w", fi.Name, err)
		}
	}
	copyNonAudioFiles(job.SourceDir, outDir)
	return nil
}

// replacedEntry records a completed per-file replacement for rollback purposes.
type replacedEntry struct {
	origName string // original filename (e.g. "song.mp3")
	outName  string // transcoded filename (e.g. "song.flac")
}

// transcodeReplace performs atomic per-file replacement with rollback on any failure.
//
// For each file:
//  1. Transcode to a temp dir (.alto-tmp-<ID>/)
//  2. Move original to backup dir (.alto-backup-<ID>/)
//  3. Move transcoded file to the source directory
//
// On failure: all previously replaced originals are restored from backup.
// On success: backup dir is removed.
func (e *Engine) transcodeReplace(ctx context.Context, job Job, progress chan<- ProgressReport) error {
	sourceDir := job.SourceDir
	tmpDir := filepath.Join(sourceDir, ".alto-tmp-"+job.ID)
	backupDir := filepath.Join(sourceDir, ".alto-backup-"+job.ID)

	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}

	var replaced []replacedEntry

	rollback := func() {
		for _, rf := range replaced {
			backupPath := filepath.Join(backupDir, rf.origName)
			origPath := filepath.Join(sourceDir, rf.origName)
			outPath := filepath.Join(sourceDir, rf.outName)
			if err := os.Rename(backupPath, origPath); err != nil {
				slog.Error("rollback: failed to restore original", "file", rf.origName, "err", err)
			} else if outPath != origPath {
				// Only remove the transcoded output when it has a different name than the
				// original; when origName == outName (same-codec transcode, e.g. FLAC→FLAC)
				// the two paths are identical and removing outPath would delete the just-
				// restored original.
				_ = os.Remove(outPath)
			}
		}
		_ = os.RemoveAll(backupDir)
		slog.Warn("replace mode: rolled back replaced files", "count", len(replaced))
	}

	for i, fi := range job.Files {
		select {
		case <-ctx.Done():
			rollback()
			return ctx.Err()
		default:
		}

		inPath := filepath.Join(sourceDir, fi.Name)
		outName := outFilename(fi.Name, job.Preset.Codec)
		tmpPath := filepath.Join(tmpDir, outName)

		args := buildArgs(e.ffmpegBin, inPath, tmpPath, job.Preset)
		slog.Info("transcoding file (replace)", "file", fi.Name)
		if err := e.ffmpegRun(ctx, args, makeProgressFn(fi, i, len(job.Files), progress)); err != nil {
			rollback()
			return fmt.Errorf("transcode %s: %w", fi.Name, err)
		}

		// Verify the transcoded output is a valid audio file before replacing the original.
		if err := e.probeFile(ctx, tmpPath); err != nil {
			rollback()
			return fmt.Errorf("output verification failed for %s: %w", fi.Name, err)
		}

		// Backup original, then place transcoded file.
		backupPath := filepath.Join(backupDir, fi.Name)
		if err := os.Rename(inPath, backupPath); err != nil {
			rollback()
			return fmt.Errorf("backup %s: %w", fi.Name, err)
		}

		finalPath := filepath.Join(sourceDir, outName)
		if err := os.Rename(tmpPath, finalPath); err != nil {
			// Restore this file before rolling back the rest.
			if rerr := os.Rename(backupPath, inPath); rerr != nil {
				slog.Error("replace: failed to restore backup after rename failure", "file", fi.Name, "err", rerr)
				// Add to replaced so rollback() also tries to restore it from backupDir,
				// preventing os.RemoveAll(backupDir) from deleting the original.
				replaced = append(replaced, replacedEntry{origName: fi.Name, outName: outName})
			}
			rollback()
			return fmt.Errorf("replace %s: %w", fi.Name, err)
		}

		replaced = append(replaced, replacedEntry{origName: fi.Name, outName: outName})
	}

	if err := os.RemoveAll(backupDir); err != nil {
		slog.Warn("replace mode: failed to remove backup dir", "err", err)
	}
	slog.Info("replace mode: completed", "files_replaced", len(replaced))
	return nil
}

// warnDiskSpace estimates the required output size and logs a warning when
// available space is less than 2× the estimate.
func (e *Engine) warnDiskSpace(job Job) {
	estimated := estimateOutputBytes(job)
	avail, err := e.diskAvail(job.SourceDir)
	if err != nil {
		slog.Warn("replace mode: disk space check failed", "err", err)
		return
	}
	if avail < estimated*2 {
		slog.Warn("replace mode: available disk space may be insufficient",
			"available_bytes", avail,
			"estimated_output_bytes", estimated,
			"recommended_minimum_bytes", estimated*2,
		)
	}
}

// estimateOutputBytes estimates the total output size for the job.
func estimateOutputBytes(job Job) uint64 {
	var total uint64
	for _, fi := range job.Files {
		switch job.Preset.Codec {
		case CodecOpus:
			bps := parseOpusBitrateBps(job.Preset.Bitrate)
			total += uint64(fi.Duration * float64(bps) / 8)
		case CodecFLAC:
			total += uint64(fi.Size) //nolint:gosec // Size is a positive file size.
		}
	}
	return total
}

// parseOpusBitrateBps converts a bitrate string like "128k" or "128000" to bits per second.
func parseOpusBitrateBps(bitrate string) uint64 {
	if trimmed, ok := strings.CutSuffix(bitrate, "k"); ok {
		n, _ := strconv.ParseUint(trimmed, 10, 64)
		return n * 1000
	}
	n, _ := strconv.ParseUint(bitrate, 10, 64)
	return n
}

// makeProgressFn returns a function that parses ffmpeg stderr lines and sends
// ProgressReports to the channel. Returns a no-op function when progress is nil.
func makeProgressFn(fi FileInfo, fileIdx, totalFiles int, progress chan<- ProgressReport) func(string) {
	if progress == nil {
		return func(string) {}
	}
	return func(line string) {
		t, ok := ParseFFmpegTime(line)
		if !ok {
			return
		}
		progress <- ProgressReport{
			CurrentFile: fi.Name,
			FileIndex:   fileIdx,
			TotalFiles:  totalFiles,
			FilePercent: CalcPercent(t, fi.Duration),
		}
	}
}

// BuildFLACArgs returns the ffmpeg arguments for FLAC transcoding.
// Output verification is performed separately via ffprobe after encoding.
func BuildFLACArgs(ffmpegBin, input, output string, preset Preset) []string {
	args := []string{
		ffmpegBin, "-i", input,
		"-c:a", "flac",
		"-compression_level", strconv.Itoa(preset.CompressionLevel),
	}
	if preset.CopyMetadata {
		args = append(args, "-map_metadata", "0")
	}
	if preset.CopyCover {
		args = append(args, "-c:v", "copy")
	}
	args = append(args, preset.ExtraArgs...)
	args = append(args, "-y", output)
	return args
}

// BuildOpusArgs returns the ffmpeg arguments for Opus transcoding.
func BuildOpusArgs(ffmpegBin, input, output string, preset Preset) []string {
	args := []string{
		ffmpegBin, "-i", input,
		"-c:a", "libopus",
		"-b:a", preset.Bitrate,
		"-vbr", "on",
		"-compression_level", strconv.Itoa(preset.CompressionLevel),
		"-application", "audio",
	}
	if preset.CopyMetadata {
		args = append(args, "-map_metadata", "0")
	}
	// Opus (Ogg) containers do not support embedded video streams; cover art
	// cannot be copied via -c:v copy for this codec.
	args = append(args, preset.ExtraArgs...)
	args = append(args, "-y", output)
	return args
}

// buildArgs dispatches to the appropriate codec-specific builder.
func buildArgs(ffmpegBin, input, output string, preset Preset) []string {
	switch preset.Codec {
	case CodecFLAC:
		return BuildFLACArgs(ffmpegBin, input, output, preset)
	case CodecOpus:
		return BuildOpusArgs(ffmpegBin, input, output, preset)
	default:
		return []string{ffmpegBin, "-i", input, "-c", "copy", "-y", output}
	}
}

// copyNonAudioFiles copies all non-audio files from srcDir to dstDir.
// Errors per file are logged but do not halt the copy.
func copyNonAudioFiles(srcDir, dstDir string) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		slog.Warn("copyNonAudioFiles: ReadDir failed", "dir", srcDir, "err", err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Skip symlinks: following them could expose arbitrary files outside the library.
		if entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		name := entry.Name()
		if audioExtensions[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		src := filepath.Join(srcDir, name)
		dst := filepath.Join(dstDir, name)
		if err := copyFile(src, dst); err != nil {
			slog.Warn("copyNonAudioFiles: copy failed", "file", name, "err", err)
		}
	}
}

// copyFile copies the file at src to dst.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}

// execFfmpeg runs ffmpeg with the given args, reading stderr and calling progressFn
// for each line. All stderr output is also forwarded to slog at Debug level.
func (e *Engine) execFfmpeg(ctx context.Context, args []string, progressFn func(string)) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	scanner := bufio.NewScanner(stderr)
	scanner.Split(scanFFmpegOutput)
	for scanner.Scan() {
		line := scanner.Text()
		slog.Debug("ffmpeg", "stderr", line)
		progressFn(line)
	}

	return cmd.Wait()
}
