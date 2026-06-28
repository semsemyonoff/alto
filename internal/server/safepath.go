package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/semsemyonoff/ALTO/internal/transcode"
)

// Path security errors.
var (
	// ErrOutsideRoot is returned when a path is not within any allowed root.
	ErrOutsideRoot = errors.New("path outside allowed root")
	// ErrAltoSegment is returned when a path contains an app-owned directory segment (.alto-*).
	ErrAltoSegment = errors.New("path contains app-owned directory segment")
	// ErrTraversal is returned when a destination tail contains a traversal component.
	ErrTraversal = errors.New("path contains traversal component")
)

// LibraryOnlyValidate canonicalizes path via filepath.Clean + filepath.EvalSymlinks,
// then verifies the resolved path is within one of the provided library roots.
// Additionally rejects any path segment matching .alto-* (app-owned dirs).
// Returns the resolved absolute path on success.
func LibraryOnlyValidate(path string, libRoots []string) (string, error) {
	clean := filepath.Clean(path)

	// Reject .alto-* segments before symlink resolution.
	if containsAltoPathSegment(clean) {
		return "", ErrAltoSegment
	}

	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", err
	}

	// Check again after resolution (in case symlink pointed into an alto dir).
	if containsAltoPathSegment(resolved) {
		return "", ErrAltoSegment
	}

	for _, root := range libRoots {
		resolvedRoot, rerr := filepath.EvalSymlinks(root)
		if rerr != nil {
			continue
		}
		if isWithin(resolved, resolvedRoot) {
			return resolved, nil
		}
	}
	return "", ErrOutsideRoot
}

// DestinationValidate verifies a destination path (which may not fully exist yet)
// is within one of the allowed roots (libRoots or outputDir).
// It walks up to the nearest existing ancestor, resolves it, then validates
// the unresolved tail contains no .. components.
func DestinationValidate(path string, libRoots []string, outputDir string) (string, error) {
	clean := filepath.Clean(path)

	existing, tail, err := splitExistingPrefix(clean)
	if err != nil {
		return "", err
	}

	// Validate tail has no .. segments (defence-in-depth after Clean).
	if tail != "" && slices.Contains(strings.Split(tail, string(filepath.Separator)), "..") {
		return "", ErrTraversal
	}

	resolvedExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}

	var fullResolved string
	if tail != "" {
		fullResolved = filepath.Join(resolvedExisting, tail)
	} else {
		fullResolved = resolvedExisting
	}

	// Collect allowed roots.
	roots := make([]string, 0, len(libRoots)+1)
	roots = append(roots, libRoots...)
	if outputDir != "" {
		roots = append(roots, outputDir)
	}

	for _, root := range roots {
		resolvedRoot, rerr := filepath.EvalSymlinks(root)
		if rerr != nil {
			// Root may not exist yet; use cleaned path for comparison.
			resolvedRoot = filepath.Clean(root)
		}
		if isWithin(fullResolved, resolvedRoot) {
			return fullResolved, nil
		}
	}
	return "", ErrOutsideRoot
}

// isWithin returns true if path equals root or is a subdirectory of root.
func isWithin(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// containsAltoPathSegment returns true if any path segment is an app-owned dir (.alto-*).
func containsAltoPathSegment(path string) bool {
	vol := filepath.VolumeName(path)
	rest := path[len(vol):]
	for seg := range strings.SplitSeq(rest, string(filepath.Separator)) {
		if seg == transcode.LocalOutputDirName || seg == ".alto-out" || strings.HasPrefix(seg, ".alto-") {
			return true
		}
	}
	return false
}

// splitExistingPrefix walks up from path until an existing filesystem entry is found.
// Returns the existing prefix and the non-existent tail (may be empty).
func splitExistingPrefix(path string) (existing, tail string, err error) {
	current := path
	var segments []string

	for {
		if _, statErr := os.Stat(current); statErr == nil {
			// Reassemble tail segments in forward order.
			t := ""
			for i := len(segments) - 1; i >= 0; i-- {
				if t == "" {
					t = segments[i]
				} else {
					t = filepath.Join(t, segments[i])
				}
			}
			return current, t, nil
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", "", errors.New("no existing ancestor found for path: " + path)
		}
		segments = append(segments, filepath.Base(current))
		current = parent
	}
}

// WritePathError writes the appropriate HTTP error for a path validation error.
// Security errors (outside root, alto segment, traversal) -> 403.
// Non-existent path -> 404. Other errors -> 500.
func WritePathError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrOutsideRoot), errors.Is(err, ErrAltoSegment), errors.Is(err, ErrTraversal):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, os.ErrNotExist):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
