package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestWriteAPIError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		msg    string
		extra  map[string]any
		want   map[string]any
	}{
		{
			name:   "nil extra",
			status: http.StatusUnprocessableEntity,
			code:   codeNoLosslessTracks,
			msg:    "directory has no lossless tracks",
			extra:  nil,
			want: map[string]any{
				"error": "directory has no lossless tracks",
				"code":  "no_lossless_tracks",
			},
		},
		{
			name:   "empty extra",
			status: http.StatusBadRequest,
			code:   codeInvalidRequest,
			msg:    "invalid request",
			extra:  map[string]any{},
			want: map[string]any{
				"error": "invalid request",
				"code":  "invalid_request",
			},
		},
		{
			name:   "extra merged alongside error and code",
			status: http.StatusUnprocessableEntity,
			code:   codeUnknownFile,
			msg:    "unknown file names",
			extra:  map[string]any{"unknown": []any{"04 D.flac"}},
			want: map[string]any{
				"error":   "unknown file names",
				"code":    "unknown_file",
				"unknown": []any{"04 D.flac"},
			},
		},
		{
			name:   "extra cannot clobber error or code",
			status: http.StatusConflict,
			code:   codeJobAlreadyRunning,
			msg:    "job already running",
			extra:  map[string]any{"error": "hijacked", "code": "hijacked", "job_id": "a1b2c3d4"},
			want: map[string]any{
				"error":  "job already running",
				"code":   "job_already_running",
				"job_id": "a1b2c3d4",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeAPIError(rec, tt.status, tt.code, tt.msg, tt.extra)

			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if len(got) != len(tt.want) {
				t.Errorf("body has %d keys (%v), want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for k, want := range tt.want {
				if fmt.Sprint(got[k]) != fmt.Sprint(want) {
					t.Errorf("body[%q] = %v, want %v", k, got[k], want)
				}
			}
		})
	}
}

// TestWriteAPIError_ExtraNotMutated guards against the merge writing the
// contract fields back into the caller's map.
func TestWriteAPIError_ExtraNotMutated(t *testing.T) {
	extra := map[string]any{"job_id": "a1b2c3d4"}
	writeAPIError(httptest.NewRecorder(), http.StatusConflict, codeJobAlreadyRunning, "job already running", extra)

	if len(extra) != 1 || extra["job_id"] != "a1b2c3d4" {
		t.Errorf("extra was mutated: %v", extra)
	}
}

func TestWritePathErrorJSON(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"outside root", ErrOutsideRoot, http.StatusForbidden, codePathForbidden},
		{"alto segment", ErrAltoSegment, http.StatusForbidden, codePathForbidden},
		{"traversal", ErrTraversal, http.StatusForbidden, codePathForbidden},
		{"not exist", os.ErrNotExist, http.StatusNotFound, codePathNotFound},
		{
			name:       "wrapped not exist",
			err:        &os.PathError{Op: "lstat", Path: "/libraries/nope", Err: os.ErrNotExist},
			wantStatus: http.StatusNotFound,
			wantCode:   codePathNotFound,
		},
		{"unknown", errors.New("boom"), http.StatusInternalServerError, codeInternalError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WritePathErrorJSON(rec, tt.err)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			var got apiErrorDTO
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if got.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", got.Code, tt.wantCode)
			}
			if got.Error == "" {
				t.Error("error message is empty")
			}
		})
	}
}

// TestAPIErrorCodes pins the documented code table: every constant carries its
// documented wire value and no value is used twice.
func TestAPIErrorCodes(t *testing.T) {
	codes := []string{
		codeInvalidRequest,
		codePathForbidden,
		codePathNotFound,
		codeLibraryNotFound,
		codeNotIndexed,
		codeNoTracks,
		codeMixedDirectory,
		codeNoLosslessTracks,
		codeUnknownFile,
		codeLossySourceSelected,
		codeOutputDirNotConfigured,
		codeOutputNameConflict,
		codeCopySkippedNotApplicable,
		codeEngineUnavailable,
		codeJobAlreadyRunning,
		codeJobNotFound,
		codeScanRunning,
		codeInternalError,
	}
	want := []string{
		"invalid_request",
		"path_forbidden",
		"path_not_found",
		"library_not_found",
		"not_indexed",
		"no_tracks",
		"mixed_directory",
		"no_lossless_tracks",
		"unknown_file",
		"lossy_source_selected",
		"output_dir_not_configured",
		"output_name_conflict",
		"copy_skipped_not_applicable",
		"engine_unavailable",
		"job_already_running",
		"job_not_found",
		"scan_running",
		"internal_error",
	}

	if len(codes) != len(want) {
		t.Fatalf("code table has %d entries, want %d", len(codes), len(want))
	}
	seen := make(map[string]bool, len(codes))
	for i, code := range codes {
		if code != want[i] {
			t.Errorf("code[%d] = %q, want %q", i, code, want[i])
		}
		if seen[code] {
			t.Errorf("duplicate code %q", code)
		}
		seen[code] = true
	}
}
