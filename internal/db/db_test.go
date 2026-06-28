package db

import (
	"fmt"
	"sync"
	"testing"
)

// openMem returns an in-memory SQLite DB for testing.
func openMem(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestMigration verifies schema creation on a fresh DB and idempotent re-run.
func TestMigration(t *testing.T) {
	db := openMem(t)

	// Run migration a second time — should be idempotent (CREATE TABLE IF NOT EXISTS).
	if err := db.migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	// Basic sanity: insert and retrieve a library.
	id, err := db.UpsertLibrary("test", "/music")
	if err != nil {
		t.Fatalf("UpsertLibrary: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero library ID")
	}
}

// TestUpsertLibrary covers insert and update semantics.
func TestUpsertLibrary(t *testing.T) {
	db := openMem(t)

	id1, err := db.UpsertLibrary("jazz", "/mnt/jazz")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Update path.
	id2, err := db.UpsertLibrary("jazz", "/mnt/jazz2")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("id mismatch after update: %d vs %d", id1, id2)
	}

	libs, err := db.GetLibraries()
	if err != nil {
		t.Fatalf("GetLibraries: %v", err)
	}
	if len(libs) != 1 {
		t.Fatalf("expected 1 library, got %d", len(libs))
	}
	if libs[0].Path != "/mnt/jazz2" {
		t.Fatalf("expected updated path, got %q", libs[0].Path)
	}
}

// TestUpsertDirectory covers insert, update, and lookup by path.
func TestUpsertDirectory(t *testing.T) {
	db := openMem(t)

	libID, _ := db.UpsertLibrary("rock", "/mnt/rock")

	dirID, err := db.UpsertDirectory(libID, "Beatles/Abbey Road", "FLAC", true, "/mnt/rock/Beatles/Abbey Road/cover.jpg")
	if err != nil {
		t.Fatalf("UpsertDirectory: %v", err)
	}
	if dirID == 0 {
		t.Fatal("expected non-zero dir ID")
	}

	// Update codec summary.
	dirID2, err := db.UpsertDirectory(libID, "Beatles/Abbey Road", "Mixed", false, "")
	if err != nil {
		t.Fatalf("UpsertDirectory update: %v", err)
	}
	if dirID != dirID2 {
		t.Fatalf("id mismatch: %d vs %d", dirID, dirID2)
	}

	d, err := db.GetDirectoryByPath(libID, "Beatles/Abbey Road")
	if err != nil {
		t.Fatalf("GetDirectoryByPath: %v", err)
	}
	if d == nil {
		t.Fatal("expected directory, got nil")
		return
	}
	if d.CodecSummary != "Mixed" {
		t.Fatalf("expected Mixed, got %q", d.CodecSummary)
	}
	if d.HasCover {
		t.Fatal("expected HasCover=false after update")
	}
	if !d.IsAudio {
		t.Fatal("expected IsAudio=true for audio directory")
	}
}

// TestGetDirectoryByPath_NotFound returns nil for missing paths.
func TestGetDirectoryByPath_NotFound(t *testing.T) {
	db := openMem(t)
	libID, _ := db.UpsertLibrary("lib", "/lib")

	d, err := db.GetDirectoryByPath(libID, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d != nil {
		t.Fatal("expected nil for missing path")
	}
}

// TestUpsertTrack covers insert and update of track metadata.
func TestUpsertTrack(t *testing.T) {
	db := openMem(t)

	libID, _ := db.UpsertLibrary("lib", "/lib")
	dirID, _ := db.UpsertDirectory(libID, "dir", "FLAC", false, "")

	track := Track{
		DirectoryID: dirID,
		Filename:    "01.flac",
		Codec:       "flac",
		Bitrate:     1000,
		Duration:    240.5,
		SampleRate:  44100,
		Channels:    2,
		Size:        30000000,
	}
	if err := db.UpsertTrack(track); err != nil {
		t.Fatalf("UpsertTrack insert: %v", err)
	}

	// Update bitrate.
	track.Bitrate = 2000
	if err := db.UpsertTrack(track); err != nil {
		t.Fatalf("UpsertTrack update: %v", err)
	}

	tracks, err := db.GetDirectoryFiles(dirID)
	if err != nil {
		t.Fatalf("GetDirectoryFiles: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("expected 1 track, got %d", len(tracks))
	}
	if tracks[0].Bitrate != 2000 {
		t.Fatalf("expected updated bitrate 2000, got %d", tracks[0].Bitrate)
	}
}

// TestDeleteStaleFiles verifies files no longer on disk are removed.
func TestDeleteStaleFiles(t *testing.T) {
	db := openMem(t)

	libID, _ := db.UpsertLibrary("lib", "/lib")
	dirID, _ := db.UpsertDirectory(libID, "albums/X", "FLAC", false, "")

	for _, f := range []string{"01.flac", "02.flac", "03.flac"} {
		_ = db.UpsertTrack(Track{DirectoryID: dirID, Filename: f})
	}

	// Keep only 01.flac and 03.flac.
	if err := db.DeleteStaleFiles(dirID, []string{"01.flac", "03.flac"}); err != nil {
		t.Fatalf("DeleteStaleFiles: %v", err)
	}

	tracks, _ := db.GetDirectoryFiles(dirID)
	if len(tracks) != 2 {
		t.Fatalf("expected 2 tracks, got %d", len(tracks))
	}
	for _, tr := range tracks {
		if tr.Filename == "02.flac" {
			t.Fatal("stale file 02.flac was not deleted")
		}
	}
}

// TestDeleteStaleFiles_AllGone verifies all tracks deleted when list is empty.
func TestDeleteStaleFiles_AllGone(t *testing.T) {
	db := openMem(t)

	libID, _ := db.UpsertLibrary("lib", "/lib")
	dirID, _ := db.UpsertDirectory(libID, "dir", "FLAC", false, "")
	_ = db.UpsertTrack(Track{DirectoryID: dirID, Filename: "a.flac"})

	if err := db.DeleteStaleFiles(dirID, nil); err != nil {
		t.Fatalf("DeleteStaleFiles(nil): %v", err)
	}

	tracks, _ := db.GetDirectoryFiles(dirID)
	if len(tracks) != 0 {
		t.Fatalf("expected 0 tracks, got %d", len(tracks))
	}
}

// TestDeleteStaleDirectories verifies removed directories are purged (including cascade).
func TestDeleteStaleDirectories(t *testing.T) {
	db := openMem(t)

	libID, _ := db.UpsertLibrary("lib", "/lib")

	paths := []string{"A", "B", "C"}
	dirIDs := make(map[string]int64)
	for _, p := range paths {
		id, _ := db.UpsertDirectory(libID, p, "FLAC", false, "")
		dirIDs[p] = id
		_ = db.UpsertTrack(Track{DirectoryID: id, Filename: "track.flac"})
	}

	// Keep A and C.
	if err := db.DeleteStaleDirectories(libID, []string{"A", "C"}); err != nil {
		t.Fatalf("DeleteStaleDirectories: %v", err)
	}

	dirs, _ := db.GetDirectoryTree(libID)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d", len(dirs))
	}

	// Cascaded tracks for B should be gone.
	tracks, _ := db.GetDirectoryFiles(dirIDs["B"])
	if len(tracks) != 0 {
		t.Fatalf("expected cascade delete of tracks for B, got %d", len(tracks))
	}
}

// TestDeleteStaleDirectories_AllGone purges all dirs when currentPaths is empty.
func TestDeleteStaleDirectories_AllGone(t *testing.T) {
	db := openMem(t)

	libID, _ := db.UpsertLibrary("lib", "/lib")
	for _, p := range []string{"X", "Y"} {
		id, _ := db.UpsertDirectory(libID, p, "FLAC", false, "")
		_ = db.UpsertTrack(Track{DirectoryID: id, Filename: "t.flac"})
	}

	if err := db.DeleteStaleDirectories(libID, nil); err != nil {
		t.Fatalf("DeleteStaleDirectories(nil): %v", err)
	}

	dirs, _ := db.GetDirectoryTree(libID)
	if len(dirs) != 0 {
		t.Fatalf("expected 0 dirs, got %d", len(dirs))
	}
}

// TestGetDirectoryTree returns ordered directories for a library.
func TestGetDirectoryTree(t *testing.T) {
	db := openMem(t)

	libID, _ := db.UpsertLibrary("lib", "/lib")
	other, _ := db.UpsertLibrary("other", "/other")

	for _, p := range []string{"Z", "A", "M"} {
		_, _ = db.UpsertDirectory(libID, p, "FLAC", false, "")
	}
	_, _ = db.UpsertDirectory(other, "shouldnotappear", "FLAC", false, "")

	dirs, err := db.GetDirectoryTree(libID)
	if err != nil {
		t.Fatalf("GetDirectoryTree: %v", err)
	}
	if len(dirs) != 3 {
		t.Fatalf("expected 3 dirs, got %d", len(dirs))
	}
	// Should be alphabetically ordered.
	if dirs[0].Path != "A" || dirs[1].Path != "M" || dirs[2].Path != "Z" {
		t.Fatalf("unexpected order: %v", []string{dirs[0].Path, dirs[1].Path, dirs[2].Path})
	}
}

// TestGetDirectoryChildren returns only immediate children.
func TestGetDirectoryChildren(t *testing.T) {
	db := openMem(t)

	libID, _ := db.UpsertLibrary("lib", "/lib")

	all := []string{
		"Artists",
		"Artists/Beatles",
		"Artists/Beatles/Abbey Road",
		"Artists/Rolling Stones",
		"Artists/Rolling Stones/Exile",
		"Compilations",
	}
	for _, p := range all {
		_, _ = db.UpsertDirectory(libID, p, "FLAC", false, "")
	}

	children, err := db.GetDirectoryChildren(libID, "Artists")
	if err != nil {
		t.Fatalf("GetDirectoryChildren: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("expected 2 children of Artists, got %d: %v", len(children), childPaths(children))
	}

	// Root children (parentPath = "").
	rootChildren, err := db.GetDirectoryChildren(libID, "")
	if err != nil {
		t.Fatalf("root children: %v", err)
	}
	// "Artists" and "Compilations" are root-level (no "/" in path).
	if len(rootChildren) != 2 {
		t.Fatalf("expected 2 root children, got %d: %v", len(rootChildren), childPaths(rootChildren))
	}
}

func TestHasDirectChildDirectory(t *testing.T) {
	db := openMem(t)
	libID, _ := db.UpsertLibrary("lib", "/lib")

	if _, err := db.UpsertDirectoryWithAudioFlag(libID, "Artists", "", false, "", false); err != nil {
		t.Fatalf("upsert parent-only dir: %v", err)
	}
	if _, err := db.UpsertDirectoryWithAudioFlag(libID, "Artists/Album", "FLAC", false, "", true); err != nil {
		t.Fatalf("upsert child dir: %v", err)
	}
	if _, err := db.UpsertDirectoryWithAudioFlag(libID, "Leaf", "FLAC", false, "", true); err != nil {
		t.Fatalf("upsert leaf dir: %v", err)
	}

	hasChildren, err := db.HasDirectChildDirectory(libID, "Artists")
	if err != nil {
		t.Fatalf("HasDirectChildDirectory(Artists): %v", err)
	}
	if !hasChildren {
		t.Fatal("expected Artists to have direct children")
	}

	hasChildren, err = db.HasDirectChildDirectory(libID, "Leaf")
	if err != nil {
		t.Fatalf("HasDirectChildDirectory(Leaf): %v", err)
	}
	if hasChildren {
		t.Fatal("expected Leaf to have no direct children")
	}
}

func childPaths(dirs []Directory) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.Path
	}
	return out
}

// TestGetLibraries verifies multiple libraries are returned.
func TestGetLibraries(t *testing.T) {
	db := openMem(t)

	for i := range 5 {
		_, err := db.UpsertLibrary(fmt.Sprintf("lib%d", i), fmt.Sprintf("/mnt/%d", i))
		if err != nil {
			t.Fatalf("UpsertLibrary %d: %v", i, err)
		}
	}

	libs, err := db.GetLibraries()
	if err != nil {
		t.Fatalf("GetLibraries: %v", err)
	}
	if len(libs) != 5 {
		t.Fatalf("expected 5 libs, got %d", len(libs))
	}
}

// TestConcurrentReadWrite exercises concurrent goroutine access for goroutine safety.
func TestConcurrentReadWrite(t *testing.T) {
	db := openMem(t)

	libID, _ := db.UpsertLibrary("concurrent", "/mnt/concurrent")

	var wg sync.WaitGroup
	const n = 20

	// Writers: upsert directories.
	for i := range n {
		wg.Go(func() {
			path := fmt.Sprintf("dir%d", i)
			id, err := db.UpsertDirectory(libID, path, "FLAC", false, "")
			if err != nil {
				t.Errorf("UpsertDirectory %d: %v", i, err)
				return
			}
			_ = db.UpsertTrack(Track{DirectoryID: id, Filename: "track.flac"})
		})
	}

	// Readers: concurrent reads (may return partial results — should not crash).
	for range n {
		wg.Go(func() {
			_, _ = db.GetLibraries()
			_, _ = db.GetDirectoryTree(libID)
		})
	}

	wg.Wait()

	dirs, err := db.GetDirectoryTree(libID)
	if err != nil {
		t.Fatalf("final GetDirectoryTree: %v", err)
	}
	if len(dirs) != n {
		t.Fatalf("expected %d dirs after concurrent writes, got %d", n, len(dirs))
	}
}

// TestForeignKeyConstraint verifies ON DELETE CASCADE and FK enforcement.
func TestForeignKeyConstraint(t *testing.T) {
	db := openMem(t)

	libID, _ := db.UpsertLibrary("fk", "/fk")
	dirID, _ := db.UpsertDirectory(libID, "d", "FLAC", false, "")
	_ = db.UpsertTrack(Track{DirectoryID: dirID, Filename: "t.flac"})

	// Delete the library — directories and tracks should cascade.
	db.mu.Lock()
	_, err := db.sql.Exec(`DELETE FROM libraries WHERE id=?`, libID)
	db.mu.Unlock()
	if err != nil {
		t.Fatalf("delete library: %v", err)
	}

	dirs, _ := db.GetDirectoryTree(libID)
	if len(dirs) != 0 {
		t.Fatalf("expected 0 dirs after library delete (cascade), got %d", len(dirs))
	}

	tracks, _ := db.GetDirectoryFiles(dirID)
	if len(tracks) != 0 {
		t.Fatalf("expected 0 tracks after cascade delete, got %d", len(tracks))
	}
}
