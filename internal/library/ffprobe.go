package library

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

// TrackInfo holds metadata extracted from a single audio file via ffprobe.
type TrackInfo struct {
	Codec      string
	Bitrate    int64
	Duration   float64
	SampleRate int64
	Channels   int64
	HasCover   bool // embedded attached_pic stream present
}

// Prober is an interface for extracting audio metadata.
// It allows mocking ffprobe in tests.
type Prober interface {
	Probe(ctx context.Context, path string) (*TrackInfo, error)
}

// FFProber invokes the real ffprobe binary.
type FFProber struct {
	// Binary is the path to the ffprobe executable. Defaults to "ffprobe".
	Binary string
}

// ffprobeOutput matches the JSON output of ffprobe -show_streams -show_format.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}

type ffprobeStream struct {
	CodecName   string `json:"codec_name"`
	CodecType   string `json:"codec_type"`
	SampleRate  string `json:"sample_rate"`
	Channels    int64  `json:"channels"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}

type ffprobeFormat struct {
	Duration string `json:"duration"`
	BitRate  string `json:"bit_rate"`
}

// Probe runs ffprobe on the given file and returns its audio metadata.
func (p *FFProber) Probe(ctx context.Context, path string) (*TrackInfo, error) {
	bin := p.Binary
	if bin == "" {
		bin = "ffprobe"
	}

	cmd := exec.CommandContext(ctx, bin,
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %q: %w", path, err)
	}
	return parseFFProbeOutput(out)
}

// parseFFProbeOutput parses raw ffprobe JSON bytes into TrackInfo.
func parseFFProbeOutput(data []byte) (*TrackInfo, error) {
	var raw ffprobeOutput
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}

	info := &TrackInfo{}

	// Find the primary audio stream; also check for embedded cover art.
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "audio":
			if info.Codec == "" {
				info.Codec = s.CodecName
				info.Channels = s.Channels
				if s.SampleRate != "" {
					sr, err := strconv.ParseInt(s.SampleRate, 10, 64)
					if err == nil {
						info.SampleRate = sr
					}
				}
			}
		case "video":
			if s.Disposition.AttachedPic != 0 {
				info.HasCover = true
			}
		}
	}

	if raw.Format.Duration != "" {
		d, err := strconv.ParseFloat(raw.Format.Duration, 64)
		if err == nil {
			info.Duration = d
		}
	}
	if raw.Format.BitRate != "" {
		br, err := strconv.ParseInt(raw.Format.BitRate, 10, 64)
		if err == nil {
			info.Bitrate = br
		}
	}

	return info, nil
}
