package transcode

import "testing"

func TestParseFFmpegVersion(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{
			name: "release build",
			out:  "ffmpeg version 8.1.2 Copyright (c) 2000-2026 the FFmpeg developers\nbuilt with ...",
			want: "8.1.2",
		},
		{
			name: "git build with n prefix",
			out:  "ffmpeg version n7.1 Copyright (c) 2000-2024 the FFmpeg developers",
			want: "n7.1",
		},
		{
			name: "distro build with suffix",
			out:  "ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023 the FFmpeg developers",
			want: "6.1.1-3ubuntu5",
		},
		{
			name:    "unrecognized output",
			out:     "some other tool v1.2.3",
			wantErr: true,
		},
		{
			name:    "empty output",
			out:     "",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFFmpegVersion([]byte(tt.out))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseFFmpegVersion(%q) = %q, want error", tt.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFFmpegVersion(%q) unexpected error: %v", tt.out, err)
			}
			if got != tt.want {
				t.Errorf("parseFFmpegVersion(%q) = %q, want %q", tt.out, got, tt.want)
			}
		})
	}
}
