package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLibraryOnlyValidate(t *testing.T) {
	libRoot := t.TempDir()
	otherRoot := t.TempDir()

	// Create subdirectories used in tests.
	validDir := filepath.Join(libRoot, "Jazz", "Miles Davis")
	mkdirAll(t, validDir)

	altoOutDir := filepath.Join(libRoot, ".alto-out")
	mkdirAll(t, altoOutDir)

	visibleAltoOutDir := filepath.Join(libRoot, "alto-out")
	mkdirAll(t, visibleAltoOutDir)

	altoTmpDir := filepath.Join(libRoot, ".alto-tmp-abc123")
	mkdirAll(t, altoTmpDir)

	// Legitimate "out" directory — NOT an alto dir (no .alto- prefix).
	legitimateOut := filepath.Join(libRoot, "out")
	mkdirAll(t, legitimateOut)

	// Nested .alto-* inside a normal directory.
	nestedAlto := filepath.Join(libRoot, "Rock", ".alto-backup-xyz")
	mkdirAll(t, nestedAlto)

	roots := []string{libRoot}

	tests := []struct {
		name    string
		path    string
		wantErr error
	}{
		{
			name: "valid path within library root",
			path: validDir,
		},
		{
			name: "library root itself",
			path: libRoot,
		},
		{
			name:    "path in different root",
			path:    otherRoot,
			wantErr: ErrOutsideRoot,
		},
		{
			name:    "visible alto-out segment",
			path:    visibleAltoOutDir,
			wantErr: ErrAltoSegment,
		},
		{
			name:    ".alto-out segment",
			path:    altoOutDir,
			wantErr: ErrAltoSegment,
		},
		{
			name:    ".alto-tmp segment",
			path:    altoTmpDir,
			wantErr: ErrAltoSegment,
		},
		{
			name:    ".alto-out with subdirectory",
			path:    filepath.Join(altoOutDir, "sub"),
			wantErr: ErrAltoSegment,
		},
		{
			name:    "nested .alto-backup inside normal dir",
			path:    nestedAlto,
			wantErr: ErrAltoSegment,
		},
		{
			name: "legitimate out/ directory (not .alto-*)",
			path: legitimateOut,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LibraryOnlyValidate(tc.path, roots)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("want error %v, got %v", tc.wantErr, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestLibraryOnlyValidate_MultipleRoots(t *testing.T) {
	root1 := t.TempDir()
	root2 := t.TempDir()

	dirInRoot2 := filepath.Join(root2, "Classical")
	mkdirAll(t, dirInRoot2)

	// Only root1 and root2 as valid roots.
	roots := []string{root1, root2}

	// Path in root2 should succeed even if root1 has no match.
	resolved, err := LibraryOnlyValidate(dirInRoot2, roots)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resolved == "" {
		t.Error("expected non-empty resolved path")
	}
}

func TestLibraryOnlyValidate_TraversalResolvedByClean(t *testing.T) {
	libRoot := t.TempDir()
	sub := filepath.Join(libRoot, "sub")
	mkdirAll(t, sub)

	// filepath.Clean resolves ".." so the final path is either within or outside.
	// ../sub within libRoot/../other resolves to outside if libRoot is top-level.
	outsideRoot := t.TempDir()

	// A traversal that resolves outside via Clean -> ErrOutsideRoot (not ErrAltoSegment).
	// Example: <libRoot>/sub/../../<outsideRoot>/<something>
	// After Clean this becomes <outsideRoot>/<something>.
	escapePath := filepath.Join(sub, "..", "..", outsideRoot, "secret")
	_, err := LibraryOnlyValidate(escapePath, []string{libRoot})
	// The path after Clean resolves outside; EvalSymlinks may fail (doesn't exist) or
	// succeed with ErrOutsideRoot. Either way it should not return nil.
	if err == nil {
		t.Error("expected error for traversal-resolved-outside path, got nil")
	}
}

func TestDestinationValidate(t *testing.T) {
	libRoot := t.TempDir()
	outRoot := t.TempDir()

	existingSub := filepath.Join(libRoot, "existing")
	mkdirAll(t, existingSub)

	roots := []string{libRoot}

	tests := []struct {
		name      string
		path      string
		outputDir string
		wantErr   error
	}{
		{
			name: "existing dir within library root",
			path: existingSub,
		},
		{
			name: "non-existent dir within library root",
			path: filepath.Join(libRoot, "newdir", "sub"),
		},
		{
			name:      "path within output dir",
			path:      filepath.Join(outRoot, "Music", "output"),
			outputDir: outRoot,
		},
		{
			name:    "path outside all roots",
			path:    filepath.Join(t.TempDir(), "outside"),
			wantErr: ErrOutsideRoot,
		},
		{
			name:    "path in output dir but outputDir not configured",
			path:    filepath.Join(outRoot, "something"),
			wantErr: ErrOutsideRoot,
		},
		{
			name: "deep non-existent path within library root",
			path: filepath.Join(libRoot, "a", "b", "c", "d"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DestinationValidate(tc.path, roots, tc.outputDir)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("want error %v, got %v", tc.wantErr, err)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestIsWithin(t *testing.T) {
	tests := []struct {
		path, root string
		want       bool
	}{
		{"/music/jazz", "/music", true},
		{"/music", "/music", true},
		{"/music/jazz/miles", "/music", true},
		{"/musicextra", "/music", false},
		{"/other", "/music", false},
		{"/", "/music", false},
	}
	for _, tc := range tests {
		got := isWithin(tc.path, tc.root)
		if got != tc.want {
			t.Errorf("isWithin(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
		}
	}
}

func TestContainsAltoPathSegment(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/music/alto-out", true},
		{"/music/alto-out/sub", true},
		{"/music/.alto-out", true},
		{"/music/.alto-out/sub", true},
		{"/music/.alto-tmp-abc", true},
		{"/music/.alto-backup-xyz/file", true},
		{"/music/out", false},
		{"/music/Jazz", false},
		{"/music", false},
		{"/.alto-out", true},
	}
	for _, tc := range tests {
		got := containsAltoPathSegment(tc.path)
		if got != tc.want {
			t.Errorf("containsAltoPathSegment(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestSplitExistingPrefix(t *testing.T) {
	base := t.TempDir()
	sub := filepath.Join(base, "existing")
	mkdirAll(t, sub)

	t.Run("existing path", func(t *testing.T) {
		existing, tail, err := splitExistingPrefix(sub)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if existing != sub {
			t.Errorf("existing = %q, want %q", existing, sub)
		}
		if tail != "" {
			t.Errorf("tail = %q, want empty", tail)
		}
	})

	t.Run("non-existent tail", func(t *testing.T) {
		path := filepath.Join(sub, "new", "dir")
		existing, tail, err := splitExistingPrefix(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if existing != sub {
			t.Errorf("existing = %q, want %q", existing, sub)
		}
		wantTail := filepath.Join("new", "dir")
		if tail != wantTail {
			t.Errorf("tail = %q, want %q", tail, wantTail)
		}
	})
}

// mkdirAll is a test helper that creates directories and fails the test on error.
func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdirAll(%q): %v", path, err)
	}
}
