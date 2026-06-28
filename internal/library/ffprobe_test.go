package library

import (
	"testing"
)

func TestParseFFProbeOutput(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    TrackInfo
		wantErr bool
	}{
		{
			name: "FLAC audio stream",
			json: `{
				"streams": [
					{
						"codec_name": "flac",
						"codec_type": "audio",
						"sample_rate": "44100",
						"channels": 2,
						"disposition": {"attached_pic": 0}
					}
				],
				"format": {
					"duration": "183.456",
					"bit_rate": "892000"
				}
			}`,
			want: TrackInfo{
				Codec:      "flac",
				Bitrate:    892000,
				Duration:   183.456,
				SampleRate: 44100,
				Channels:   2,
				HasCover:   false,
			},
		},
		{
			name: "Opus audio stream",
			json: `{
				"streams": [
					{
						"codec_name": "opus",
						"codec_type": "audio",
						"sample_rate": "48000",
						"channels": 2,
						"disposition": {"attached_pic": 0}
					}
				],
				"format": {
					"duration": "300.0",
					"bit_rate": "160000"
				}
			}`,
			want: TrackInfo{
				Codec:      "opus",
				Bitrate:    160000,
				Duration:   300.0,
				SampleRate: 48000,
				Channels:   2,
				HasCover:   false,
			},
		},
		{
			name: "MP3 with embedded cover art",
			json: `{
				"streams": [
					{
						"codec_name": "mp3",
						"codec_type": "audio",
						"sample_rate": "44100",
						"channels": 2,
						"disposition": {"attached_pic": 0}
					},
					{
						"codec_name": "mjpeg",
						"codec_type": "video",
						"disposition": {"attached_pic": 1}
					}
				],
				"format": {
					"duration": "210.5",
					"bit_rate": "320000"
				}
			}`,
			want: TrackInfo{
				Codec:      "mp3",
				Bitrate:    320000,
				Duration:   210.5,
				SampleRate: 44100,
				Channels:   2,
				HasCover:   true,
			},
		},
		{
			name: "missing duration and bitrate",
			json: `{
				"streams": [
					{
						"codec_name": "wav",
						"codec_type": "audio",
						"sample_rate": "96000",
						"channels": 6,
						"disposition": {"attached_pic": 0}
					}
				],
				"format": {}
			}`,
			want: TrackInfo{
				Codec:      "wav",
				Bitrate:    0,
				Duration:   0,
				SampleRate: 96000,
				Channels:   6,
				HasCover:   false,
			},
		},
		{
			name: "no audio stream",
			json: `{
				"streams": [],
				"format": {"duration": "10.0", "bit_rate": "1000"}
			}`,
			want: TrackInfo{
				Codec:      "",
				Bitrate:    1000,
				Duration:   10.0,
				SampleRate: 0,
				Channels:   0,
				HasCover:   false,
			},
		},
		{
			name:    "invalid JSON",
			json:    `not json`,
			wantErr: true,
		},
		{
			name: "multiple audio streams picks first",
			json: `{
				"streams": [
					{
						"codec_name": "flac",
						"codec_type": "audio",
						"sample_rate": "44100",
						"channels": 2,
						"disposition": {"attached_pic": 0}
					},
					{
						"codec_name": "mp3",
						"codec_type": "audio",
						"sample_rate": "44100",
						"channels": 2,
						"disposition": {"attached_pic": 0}
					}
				],
				"format": {"duration": "60.0", "bit_rate": "500000"}
			}`,
			want: TrackInfo{
				Codec:      "flac",
				Bitrate:    500000,
				Duration:   60.0,
				SampleRate: 44100,
				Channels:   2,
				HasCover:   false,
			},
		},
		{
			name: "invalid sample_rate string ignored",
			json: `{
				"streams": [
					{
						"codec_name": "aac",
						"codec_type": "audio",
						"sample_rate": "invalid",
						"channels": 1,
						"disposition": {"attached_pic": 0}
					}
				],
				"format": {"duration": "30.0", "bit_rate": "128000"}
			}`,
			want: TrackInfo{
				Codec:      "aac",
				Bitrate:    128000,
				Duration:   30.0,
				SampleRate: 0,
				Channels:   1,
				HasCover:   false,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFFProbeOutput([]byte(tc.json))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Codec != tc.want.Codec {
				t.Errorf("Codec: got %q want %q", got.Codec, tc.want.Codec)
			}
			if got.Bitrate != tc.want.Bitrate {
				t.Errorf("Bitrate: got %d want %d", got.Bitrate, tc.want.Bitrate)
			}
			if got.Duration != tc.want.Duration {
				t.Errorf("Duration: got %f want %f", got.Duration, tc.want.Duration)
			}
			if got.SampleRate != tc.want.SampleRate {
				t.Errorf("SampleRate: got %d want %d", got.SampleRate, tc.want.SampleRate)
			}
			if got.Channels != tc.want.Channels {
				t.Errorf("Channels: got %d want %d", got.Channels, tc.want.Channels)
			}
			if got.HasCover != tc.want.HasCover {
				t.Errorf("HasCover: got %v want %v", got.HasCover, tc.want.HasCover)
			}
		})
	}
}
