package main

import (
	"testing"
)

func TestParseLibraries(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		wantLen int
		wantMsg string
	}{
		{
			name:    "empty input",
			input:   "",
			wantErr: true,
			wantMsg: "ALTO_LIBRARIES is required",
		},
		{
			name:    "single library",
			input:   "Music:/music",
			wantLen: 1,
		},
		{
			name:    "multiple libraries",
			input:   "Music:/music,Lossless:/lossless",
			wantLen: 2,
		},
		{
			name:    "whitespace around entries",
			input:   " Music:/music , Lossless:/lossless ",
			wantLen: 2,
		},
		{
			name:    "missing colon separator",
			input:   "Musicmusic",
			wantErr: true,
		},
		{
			name:    "empty name",
			input:   ":/music",
			wantErr: true,
		},
		{
			name:    "empty path",
			input:   "Music:",
			wantErr: true,
		},
		{
			name:    "invalid name chars - space",
			input:   "My Music:/music",
			wantErr: true,
		},
		{
			name:    "invalid name chars - dot",
			input:   "my.music:/music",
			wantErr: true,
		},
		{
			name:    "valid name with underscore and hyphen",
			input:   "my_music-lib:/music",
			wantLen: 1,
		},
		{
			name:    "duplicate library names",
			input:   "Music:/music,Music:/other",
			wantErr: true,
			wantMsg: "duplicate library name",
		},
		{
			name:    "duplicate library paths",
			input:   "Music:/music,Lossless:/music",
			wantErr: true,
			wantMsg: "duplicate library path",
		},
		{
			name:    "only commas",
			input:   ",,,",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLibraries(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.wantMsg != "" && !contains(err.Error(), tt.wantMsg) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tt.wantLen {
				t.Errorf("got %d libraries, want %d", len(got), tt.wantLen)
			}
		})
	}
}

func TestParseConfig_MissingLibraries(t *testing.T) {
	t.Setenv("ALTO_LIBRARIES", "")
	t.Setenv("ALTO_PORT", "")
	t.Setenv("ALTO_OUTPUT_DIR", "")
	t.Setenv("ALTO_DB_PATH", "")
	t.Setenv("ALTO_CACHE_DIR", "")

	_, err := ParseConfig()
	if err == nil {
		t.Fatal("expected error when ALTO_LIBRARIES is missing")
	}
}

func TestParseConfig_Defaults(t *testing.T) {
	t.Setenv("ALTO_LIBRARIES", "Music:/music")
	t.Setenv("ALTO_PORT", "")
	t.Setenv("ALTO_OUTPUT_DIR", "")
	t.Setenv("ALTO_DB_PATH", "")
	t.Setenv("ALTO_CACHE_DIR", "")

	cfg, err := ParseConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("default port: got %q, want %q", cfg.Port, "8080")
	}
	if cfg.OutputDir != "/out" {
		t.Errorf("default output dir: got %q, want %q", cfg.OutputDir, "/out")
	}
	if cfg.DBPath != "./alto.db" {
		t.Errorf("default db path: got %q, want %q", cfg.DBPath, "./alto.db")
	}
	if cfg.CacheDir != "./cache" {
		t.Errorf("default cache dir: got %q, want %q", cfg.CacheDir, "./cache")
	}
}

func TestParseConfig_EnvOverrides(t *testing.T) {
	t.Setenv("ALTO_LIBRARIES", "Music:/music")
	t.Setenv("ALTO_PORT", "9090")
	t.Setenv("ALTO_OUTPUT_DIR", "/myout")
	t.Setenv("ALTO_DB_PATH", "/data/alto.db")
	t.Setenv("ALTO_CACHE_DIR", "/tmp/cache")

	cfg, err := ParseConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Port != "9090" {
		t.Errorf("port: got %q, want %q", cfg.Port, "9090")
	}
	if cfg.OutputDir != "/myout" {
		t.Errorf("output dir: got %q, want %q", cfg.OutputDir, "/myout")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
