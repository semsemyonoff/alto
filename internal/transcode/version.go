package transcode

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
)

// ffmpegVersionRe matches the leading token of `ffmpeg -version` output, whose
// first line looks like: "ffmpeg version 8.1.2 Copyright (c) 2000-2026 ...".
var ffmpegVersionRe = regexp.MustCompile(`^ffmpeg version (\S+)`)

// FFmpegVersion returns the version token reported by `ffmpeg -version` (e.g.
// "8.1.2", "n7.1", "6.1.1-3ubuntu5"), or an error if ffmpeg is unavailable or
// its output is unrecognized. ffmpeg is ALTO's core tool, so this is surfaced in
// the UI alongside the app version.
func FFmpegVersion(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "ffmpeg", "-version").Output()
	if err != nil {
		return "", fmt.Errorf("run ffmpeg -version: %w", err)
	}
	return parseFFmpegVersion(out)
}

// parseFFmpegVersion extracts the version token from `ffmpeg -version` output.
func parseFFmpegVersion(out []byte) (string, error) {
	line, _, _ := bytes.Cut(out, []byte{'\n'})
	m := ffmpegVersionRe.FindSubmatch(bytes.TrimSpace(line))
	if m == nil {
		return "", fmt.Errorf("unrecognized ffmpeg -version output")
	}
	return string(m[1]), nil
}
