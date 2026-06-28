package transcode

import (
	"regexp"
	"strconv"
)

// ProgressReport describes the current state of a running transcoding job.
type ProgressReport struct {
	CurrentFile string
	FileIndex   int
	TotalFiles  int
	FilePercent float64 // 0–100
}

var timeRegexp = regexp.MustCompile(`time=(\d+):(\d+):(\d+\.?\d*)`)

// ParseFFmpegTime extracts the elapsed playback time (in seconds) from an ffmpeg
// stderr line that contains a "time=HH:MM:SS.ms" field.
// Returns (seconds, true) on success, or (0, false) if no time field is found.
func ParseFFmpegTime(line string) (float64, bool) {
	m := timeRegexp.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	h, _ := strconv.ParseFloat(m[1], 64)
	min, _ := strconv.ParseFloat(m[2], 64)
	sec, _ := strconv.ParseFloat(m[3], 64)
	return h*3600 + min*60 + sec, true
}

// CalcPercent converts elapsed/total seconds into a [0, 100] percentage.
func CalcPercent(elapsed, total float64) float64 {
	if total <= 0 {
		return 0
	}
	p := elapsed / total * 100
	if p > 100 {
		return 100
	}
	return p
}

// scanFFmpegOutput is a bufio.SplitFunc that splits on either '\n' or '\r'
// so that ffmpeg's carriage-return progress lines are captured as tokens.
func scanFFmpegOutput(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
