package main

import (
	"testing"

	"github.com/semsemyonoff/ALTO/internal/library"
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

func TestParseConfig_WorkersDefault(t *testing.T) {
	t.Setenv("ALTO_LIBRARIES", "Music:/music")
	t.Setenv("ALTO_TRANSCODE_WORKERS", "")

	cfg, err := ParseConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Workers != 1 {
		t.Errorf("default workers: got %d, want %d", cfg.Workers, 1)
	}
}

func TestParseConfig_WorkersValid(t *testing.T) {
	t.Setenv("ALTO_LIBRARIES", "Music:/music")
	t.Setenv("ALTO_TRANSCODE_WORKERS", "4")

	cfg, err := ParseConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Workers != 4 {
		t.Errorf("workers: got %d, want %d", cfg.Workers, 4)
	}
}

func TestParseConfig_WorkersClampedToMinOne(t *testing.T) {
	t.Setenv("ALTO_LIBRARIES", "Music:/music")
	t.Setenv("ALTO_TRANSCODE_WORKERS", "0")

	cfg, err := ParseConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Workers != 1 {
		t.Errorf("workers: got %d, want %d", cfg.Workers, 1)
	}

	t.Setenv("ALTO_TRANSCODE_WORKERS", "-3")
	cfg, err = ParseConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Workers != 1 {
		t.Errorf("workers: got %d, want %d", cfg.Workers, 1)
	}
}

func TestParseConfig_WorkersInvalid(t *testing.T) {
	t.Setenv("ALTO_LIBRARIES", "Music:/music")
	t.Setenv("ALTO_TRANSCODE_WORKERS", "not-a-number")

	_, err := ParseConfig()
	if err == nil {
		t.Fatal("expected error for invalid ALTO_TRANSCODE_WORKERS")
	}
	if !contains(err.Error(), "ALTO_TRANSCODE_WORKERS") {
		t.Errorf("error %q does not mention env var name", err.Error())
	}
}

func TestGetEnvIntDefault(t *testing.T) {
	t.Setenv("ALTO_TEST_INT", "")
	got, err := getEnvIntDefault("ALTO_TEST_INT", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 7 {
		t.Errorf("got %d, want %d", got, 7)
	}

	t.Setenv("ALTO_TEST_INT", "  12 ")
	got, err = getEnvIntDefault("ALTO_TEST_INT", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 12 {
		t.Errorf("got %d, want %d", got, 12)
	}

	t.Setenv("ALTO_TEST_INT", "abc")
	if _, err := getEnvIntDefault("ALTO_TEST_INT", 7); err == nil {
		t.Fatal("expected error for non-numeric value")
	}
}

func TestParseConfig_ScanOnStart(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    bool
		wantErr bool
	}{
		{name: "unset defaults to true", env: "", want: true},
		{name: "false disables", env: "false", want: false},
		{name: "zero disables", env: "0", want: false},
		{name: "true enables", env: "true", want: true},
		{name: "padded value is trimmed", env: " false ", want: false},
		{name: "invalid value errors", env: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ALTO_LIBRARIES", "Music:/music")
			t.Setenv("ALTO_SCAN_ON_START", tt.env)

			cfg, err := ParseConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error for invalid ALTO_SCAN_ON_START")
				}
				if !contains(err.Error(), "ALTO_SCAN_ON_START") {
					t.Errorf("error %q does not mention env var name", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.ScanOnStart != tt.want {
				t.Errorf("scan on start: got %v, want %v", cfg.ScanOnStart, tt.want)
			}
		})
	}
}

func TestParseConfig_ScanWorkers(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    int
		wantErr bool
	}{
		{name: "unset means scanner default", env: "", want: 0},
		{name: "valid value is kept", env: "6", want: 6},
		{name: "zero means scanner default", env: "0", want: 0},
		{name: "negative clamps to zero", env: "-3", want: 0},
		{name: "padded value is trimmed", env: " 2 ", want: 2},
		{name: "invalid value errors", env: "not-a-number", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ALTO_LIBRARIES", "Music:/music")
			t.Setenv("ALTO_SCAN_WORKERS", tt.env)

			cfg, err := ParseConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error for invalid ALTO_SCAN_WORKERS")
				}
				if !contains(err.Error(), "ALTO_SCAN_WORKERS") {
					t.Errorf("error %q does not mention env var name", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.ScanWorkers != tt.want {
				t.Errorf("scan workers: got %d, want %d", cfg.ScanWorkers, tt.want)
			}
		})
	}
}

func TestEffectiveScanWorkers(t *testing.T) {
	def := library.DefaultScanWorkers()
	tests := []struct {
		in   int
		want int
	}{
		{in: 0, want: def},
		{in: -1, want: def},
		{in: 1, want: 1},
		{in: 8, want: 8},
	}

	for _, tt := range tests {
		if got := effectiveScanWorkers(tt.in); got != tt.want {
			t.Errorf("effectiveScanWorkers(%d): got %d, want %d", tt.in, got, tt.want)
		}
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
