package server

import (
	"errors"
	"reflect"
	"testing"

	"github.com/semsemyonoff/ALTO/internal/db"
	"github.com/semsemyonoff/ALTO/internal/transcode"
)

func tracksOf(pairs ...string) []db.Track {
	if len(pairs)%2 != 0 {
		panic("tracksOf: want name/codec pairs")
	}
	out := make([]db.Track, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, db.Track{Filename: pairs[i], Codec: pairs[i+1]})
	}
	return out
}

func names(tracks []db.Track) []string {
	if tracks == nil {
		return nil
	}
	out := make([]string, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, t.Filename)
	}
	return out
}

func TestPartitionByLossless(t *testing.T) {
	tests := []struct {
		name         string
		tracks       []db.Track
		wantLossless []string
		wantLossy    []string
	}{
		{
			name:         "empty",
			tracks:       nil,
			wantLossless: nil,
			wantLossy:    nil,
		},
		{
			name:         "all lossless",
			tracks:       tracksOf("01 A.flac", "flac", "02 B.ape", "ape"),
			wantLossless: []string{"01 A.flac", "02 B.ape"},
			wantLossy:    nil,
		},
		{
			name:         "all lossy",
			tracks:       tracksOf("01 A.mp3", "mp3", "02 B.opus", "opus"),
			wantLossless: nil,
			wantLossy:    []string{"01 A.mp3", "02 B.opus"},
		},
		{
			name:         "mixed keeps order",
			tracks:       tracksOf("01 A.flac", "flac", "02 B.mp3", "mp3", "03 C.flac", "flac"),
			wantLossless: []string{"01 A.flac", "03 C.flac"},
			wantLossy:    []string{"02 B.mp3"},
		},
		{
			name:         "pcm prefix is lossless",
			tracks:       tracksOf("01 A.wav", "pcm_s16le", "02 B.wav", "pcm_f32le"),
			wantLossless: []string{"01 A.wav", "02 B.wav"},
			wantLossy:    nil,
		},
		{
			name:         "mixed casing and padding",
			tracks:       tracksOf("01 A.flac", " FLAC ", "02 B.mp3", "MP3", "03 C.wav", "PCM_S24LE"),
			wantLossless: []string{"01 A.flac", "03 C.wav"},
			wantLossy:    []string{"02 B.mp3"},
		},
		{
			name:         "empty codec is lossy",
			tracks:       tracksOf("01 A.flac", ""),
			wantLossless: nil,
			wantLossy:    []string{"01 A.flac"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lossless, lossy := partitionByLossless(tt.tracks)
			if got := names(lossless); !reflect.DeepEqual(got, tt.wantLossless) {
				t.Errorf("lossless = %v, want %v", got, tt.wantLossless)
			}
			if got := names(lossy); !reflect.DeepEqual(got, tt.wantLossy) {
				t.Errorf("lossy = %v, want %v", got, tt.wantLossy)
			}
		})
	}
}

func TestValidateFileNames(t *testing.T) {
	dir := tracksOf(
		"01 A.flac", "flac",
		"02 B.mp3", "mp3",
		"03 C.ape", "APE",
		"04 D.wav", "pcm_s16le",
		"05 Wait For It....flac", "flac",
	)

	tests := []struct {
		name         string
		input        []string
		wantSelected []string
		wantErr      error
		wantNames    []string
	}{
		{
			name:         "success in directory order",
			input:        []string{"03 C.ape", "01 A.flac"},
			wantSelected: []string{"01 A.flac", "03 C.ape"},
		},
		{
			name:         "success single pcm track",
			input:        []string{"04 D.wav"},
			wantSelected: []string{"04 D.wav"},
		},
		{
			name:      "forward slash",
			input:     []string{"sub/01 A.flac"},
			wantErr:   errFileSeparator,
			wantNames: []string{"sub/01 A.flac"},
		},
		{
			name:      "backslash",
			input:     []string{`sub\01 A.flac`},
			wantErr:   errFileSeparator,
			wantNames: []string{`sub\01 A.flac`},
		},
		{
			name:      "traversal",
			input:     []string{"..", "01 A.flac"},
			wantErr:   errFileSeparator,
			wantNames: []string{".."},
		},
		{
			name:      "current directory",
			input:     []string{"."},
			wantErr:   errFileSeparator,
			wantNames: []string{"."},
		},
		{
			name:      "empty name",
			input:     []string{""},
			wantErr:   errFileSeparator,
			wantNames: []string{""},
		},
		// A run of dots inside a name is ordinary text, not traversal: an
		// indexed "05 Wait For It....flac" must stay selectable.
		{
			name:         "double dot inside a legitimate file name",
			input:        []string{"05 Wait For It....flac"},
			wantSelected: []string{"05 Wait For It....flac"},
		},
		{
			name:      "duplicate",
			input:     []string{"01 A.flac", "01 A.flac"},
			wantErr:   errFileDuplicate,
			wantNames: []string{"01 A.flac"},
		},
		{
			name:      "unknown carries every offending name",
			input:     []string{"01 A.flac", "99 X.flac", "98 Y.flac"},
			wantErr:   errFileUnknown,
			wantNames: []string{"99 X.flac", "98 Y.flac"},
		},
		{
			name:      "case-sensitive lookup misses",
			input:     []string{"01 a.flac"},
			wantErr:   errFileUnknown,
			wantNames: []string{"01 a.flac"},
		},
		{
			name:      "lossy source selected",
			input:     []string{"01 A.flac", "02 B.mp3"},
			wantErr:   errFileLossy,
			wantNames: []string{"02 B.mp3"},
		},
		{
			name:      "unknown wins over lossy",
			input:     []string{"02 B.mp3", "99 X.flac"},
			wantErr:   errFileUnknown,
			wantNames: []string{"99 X.flac"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selected, err := validateFileNames(tt.input, dir)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got := names(selected); !reflect.DeepEqual(got, tt.wantSelected) {
					t.Errorf("selected = %v, want %v", got, tt.wantSelected)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if selected != nil {
				t.Errorf("selected = %v, want nil on error", names(selected))
			}
			if got := offendingNames(err); !reflect.DeepEqual(got, tt.wantNames) {
				t.Errorf("offendingNames = %v, want %v", got, tt.wantNames)
			}
		})
	}
}

func TestValidateFileNames_EmptyInput(t *testing.T) {
	// Presence is decided by the handler (req.Files != nil); an empty list here
	// is simply an empty selection.
	selected, err := validateFileNames(nil, tracksOf("01 A.flac", "flac"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(selected) != 0 {
		t.Errorf("selected = %v, want empty", names(selected))
	}
}

func TestOffendingNames_NonSelectionError(t *testing.T) {
	if got := offendingNames(errors.New("boom")); got != nil {
		t.Errorf("offendingNames = %v, want nil", got)
	}
}

func TestSkippedReport(t *testing.T) {
	all := tracksOf("01 A.flac", "flac", "02 B.mp3", "mp3", "03 C.flac", "flac", "04 D.m4a", "aac")

	tests := []struct {
		name     string
		selected []db.Track
		reason   string
		want     []skippedDTO
	}{
		{
			name:     "nothing skipped",
			selected: all,
			reason:   skipReasonLossy,
			want:     nil,
		},
		{
			name:     "lossy tracks skipped",
			selected: tracksOf("01 A.flac", "flac", "03 C.flac", "flac"),
			reason:   skipReasonLossy,
			want: []skippedDTO{
				{Name: "02 B.mp3", Codec: "mp3", Reason: skipReasonLossy},
				{Name: "04 D.m4a", Codec: "aac", Reason: skipReasonLossy},
			},
		},
		{
			name:     "explicit selection reports not_selected",
			selected: tracksOf("01 A.flac", "flac"),
			reason:   skipReasonNotSelected,
			want: []skippedDTO{
				{Name: "02 B.mp3", Codec: "mp3", Reason: skipReasonNotSelected},
				{Name: "03 C.flac", Codec: "flac", Reason: skipReasonNotSelected},
				{Name: "04 D.m4a", Codec: "aac", Reason: skipReasonNotSelected},
			},
		},
		{
			name:     "empty selection skips everything",
			selected: nil,
			reason:   skipReasonNotSelected,
			want: []skippedDTO{
				{Name: "01 A.flac", Codec: "flac", Reason: skipReasonNotSelected},
				{Name: "02 B.mp3", Codec: "mp3", Reason: skipReasonNotSelected},
				{Name: "03 C.flac", Codec: "flac", Reason: skipReasonNotSelected},
				{Name: "04 D.m4a", Codec: "aac", Reason: skipReasonNotSelected},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := skippedReport(unselectedTracks(all, tt.selected), tt.reason)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("skippedReport = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestDetectOutputConflicts exercises the detector directly: the handler tests
// only ever produce a single conflict, leaving the multi-conflict ordering and
// the selected-before-name-preserving source order unpinned.
func TestDetectOutputConflicts(t *testing.T) {
	tests := []struct {
		name         string
		selected     []db.Track
		namePreserve []db.Track
		codec        transcode.Codec
		want         []outputConflictDTO
	}{
		{
			name:     "no conflict",
			selected: tracksOf("01 A.ape", "ape", "02 B.wav", "pcm_s16le"),
			codec:    transcode.CodecFLAC,
		},
		{
			name:     "two selected sources collapse onto one output",
			selected: tracksOf("01 A.ape", "ape", "01 A.wav", "pcm_s16le"),
			codec:    transcode.CodecFLAC,
			want: []outputConflictDTO{
				{Output: "01 A.flac", Sources: []string{"01 A.ape", "01 A.wav"}},
			},
		},
		{
			name:         "a name-preserving file claims a transcode output",
			selected:     tracksOf("01 A.ape", "ape"),
			namePreserve: tracksOf("01 A.flac", "flac"),
			codec:        transcode.CodecFLAC,
			want: []outputConflictDTO{
				{Output: "01 A.flac", Sources: []string{"01 A.ape", "01 A.flac"}},
			},
		},
		{
			// Sorted by output name, so "01" comes before "02" even though the
			// tracks are given the other way round.
			name:         "several conflicts come back sorted by output name",
			selected:     tracksOf("02 B.ape", "ape", "01 A.ape", "ape"),
			namePreserve: tracksOf("02 B.flac", "flac", "01 A.flac", "flac"),
			codec:        transcode.CodecFLAC,
			want: []outputConflictDTO{
				{Output: "01 A.flac", Sources: []string{"01 A.ape", "01 A.flac"}},
				{Output: "02 B.flac", Sources: []string{"02 B.ape", "02 B.flac"}},
			},
		},
		{
			name:     "three sources on one output list all of them",
			selected: tracksOf("01 A.ape", "ape", "01 A.wav", "pcm_s16le", "01 A.wv", "wavpack"),
			codec:    transcode.CodecFLAC,
			want: []outputConflictDTO{
				{Output: "01 A.flac", Sources: []string{"01 A.ape", "01 A.wav", "01 A.wv"}},
			},
		},
		{
			// A same-extension transcode maps each name onto itself, so nothing
			// collapses that was not already colliding on disk.
			name:     "same extension is not a conflict with itself",
			selected: tracksOf("01 A.flac", "flac", "02 B.flac", "flac"),
			codec:    transcode.CodecFLAC,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectOutputConflicts(tt.selected, tt.namePreserve, tt.codec)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("detectOutputConflicts = %+v, want %+v", got, tt.want)
			}
		})
	}
}
