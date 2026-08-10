package server

import (
	"errors"
	"sort"
	"strings"

	"github.com/semsemyonoff/ALTO/internal/db"
	"github.com/semsemyonoff/ALTO/internal/transcode"
)

// skippedDTO describes a track that was excluded from a transcode job.
type skippedDTO struct {
	Name   string `json:"name"`
	Codec  string `json:"codec"`
	Reason string `json:"reason"`
}

// Reasons reported in skippedDTO.
const (
	skipReasonLossy       = "lossy"        // dropped by skip_lossy
	skipReasonNotSelected = "not_selected" // absent from an explicit files list
)

// File-selection validation failures. They are sentinels rather than code
// strings so handlers map them with errors.Is, the way safepath.go does.
var (
	errFileSeparator = errors.New(`file name must not contain a path separator or ".."`)
	errFileDuplicate = errors.New("duplicate file name")
	errFileUnknown   = errors.New("file not found in the directory index")
	errFileLossy     = errors.New("file is not a lossless source")
)

// fileNamesError wraps a selection sentinel with the offending names, so the
// handler can put them into the error envelope's extra context.
type fileNamesError struct {
	sentinel error
	names    []string
}

func (e *fileNamesError) Error() string {
	return e.sentinel.Error() + ": " + strings.Join(e.names, ", ")
}

func (e *fileNamesError) Unwrap() error { return e.sentinel }

// offendingNames returns the names carried by a fileNamesError, or nil.
func offendingNames(err error) []string {
	var fe *fileNamesError
	if errors.As(err, &fe) {
		return fe.names
	}
	return nil
}

// partitionByLossless splits tracks into lossless and lossy, preserving order.
// isLosslessCodec stays the single source of truth for the distinction.
func partitionByLossless(tracks []db.Track) (lossless, lossy []db.Track) {
	for _, t := range tracks {
		if isLosslessCodec(t.Codec) {
			lossless = append(lossless, t)
		} else {
			lossy = append(lossy, t)
		}
	}
	return lossless, lossy
}

// validateFileNames resolves an explicit selection against the directory's
// indexed tracks. Names must be bare file names, unique, present in the index
// and lossless; each failure returns its sentinel, carrying the offending names
// where more than one can be at fault.
//
// The selected tracks come back in directory order, not request order, so the
// job's file list is stable regardless of how the client ordered its request.
func validateFileNames(names []string, tracks []db.Track) ([]db.Track, error) {
	var bad []string
	for _, n := range names {
		if strings.ContainsAny(n, `/\`) || strings.Contains(n, "..") {
			bad = append(bad, n)
		}
	}
	if len(bad) > 0 {
		return nil, &fileNamesError{sentinel: errFileSeparator, names: bad}
	}

	seen := make(map[string]struct{}, len(names))
	var dupes []string
	for _, n := range names {
		if _, ok := seen[n]; ok {
			dupes = append(dupes, n)
			continue
		}
		seen[n] = struct{}{}
	}
	if len(dupes) > 0 {
		return nil, &fileNamesError{sentinel: errFileDuplicate, names: dupes}
	}

	byName := make(map[string]db.Track, len(tracks))
	for _, t := range tracks {
		byName[t.Filename] = t
	}
	var unknown []string
	for _, n := range names {
		if _, ok := byName[n]; !ok {
			unknown = append(unknown, n)
		}
	}
	if len(unknown) > 0 {
		return nil, &fileNamesError{sentinel: errFileUnknown, names: unknown}
	}

	var lossy []string
	for _, n := range names {
		if !isLosslessCodec(byName[n].Codec) {
			lossy = append(lossy, n)
		}
	}
	if len(lossy) > 0 {
		return nil, &fileNamesError{sentinel: errFileLossy, names: lossy}
	}

	selected := make([]db.Track, 0, len(names))
	for _, t := range tracks {
		if _, ok := seen[t.Filename]; ok {
			selected = append(selected, t)
		}
	}
	return selected, nil
}

// outputConflictDTO reports several sources that would render to the same
// output file name.
type outputConflictDTO struct {
	Output  string   `json:"output"`
	Sources []string `json:"sources"`
}

// detectOutputConflicts finds selected tracks that collapse onto a single
// output name once their extension is rewritten for the target codec — e.g.
// "01 A.ape" and "01 A.flac" both rendering to "01 A.flac". ffmpeg runs with
// -y, so without this check the second source silently overwrites the first
// and the job reports done having produced one file instead of two.
//
// Conflicts come back sorted by output name, and their sources in directory
// order, so the error body is stable.
func detectOutputConflicts(selected []db.Track, codec transcode.Codec) []outputConflictDTO {
	sources := make(map[string][]string, len(selected))
	var order []string
	for _, t := range selected {
		out := transcode.OutFilename(t.Filename, codec)
		if _, seen := sources[out]; !seen {
			order = append(order, out)
		}
		sources[out] = append(sources[out], t.Filename)
	}

	var conflicts []outputConflictDTO
	for _, out := range order {
		if len(sources[out]) > 1 {
			conflicts = append(conflicts, outputConflictDTO{Output: out, Sources: sources[out]})
		}
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Output < conflicts[j].Output })
	return conflicts
}

// skippedReport lists the tracks of all that are absent from selected, in the
// order they appear in all.
func skippedReport(all, selected []db.Track, reason string) []skippedDTO {
	keep := make(map[string]struct{}, len(selected))
	for _, t := range selected {
		keep[t.Filename] = struct{}{}
	}
	var out []skippedDTO
	for _, t := range all {
		if _, ok := keep[t.Filename]; ok {
			continue
		}
		out = append(out, skippedDTO{Name: t.Filename, Codec: t.Codec, Reason: reason})
	}
	return out
}
