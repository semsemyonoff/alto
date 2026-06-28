package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	_ "modernc.org/sqlite"
)

// DB wraps a SQLite database with a write mutex for serialized writes.
type DB struct {
	sql *sql.DB
	mu  sync.Mutex
}

// Library represents a named, mounted music library stored in the DB.
type Library struct {
	ID   int64
	Name string
	Path string
}

// Directory represents a scanned directory within a library.
type Directory struct {
	ID           int64
	LibraryID    int64
	Path         string
	HasCover     bool
	CoverPath    string
	CodecSummary string
	IsAudio      bool
	HasChildren  bool
}

// Track represents an audio file within a directory.
type Track struct {
	ID          int64
	DirectoryID int64
	Filename    string
	Codec       string
	Bitrate     int64
	Duration    float64
	SampleRate  int64
	Channels    int64
	Size        int64
}

const schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS libraries (
	id   INTEGER PRIMARY KEY,
	name TEXT UNIQUE NOT NULL,
	path TEXT UNIQUE NOT NULL
);

CREATE TABLE IF NOT EXISTS directories (
	id            INTEGER PRIMARY KEY,
	library_id    INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
	path          TEXT NOT NULL,
	has_cover     BOOLEAN NOT NULL DEFAULT 0,
	cover_path    TEXT NOT NULL DEFAULT '',
	codec_summary TEXT NOT NULL DEFAULT '',
	is_audio      BOOLEAN NOT NULL DEFAULT 0,
	UNIQUE(library_id, path)
);

CREATE TABLE IF NOT EXISTS tracks (
	id           INTEGER PRIMARY KEY,
	directory_id INTEGER NOT NULL REFERENCES directories(id) ON DELETE CASCADE,
	filename     TEXT NOT NULL,
	codec        TEXT NOT NULL DEFAULT '',
	bitrate      INTEGER NOT NULL DEFAULT 0,
	duration     REAL NOT NULL DEFAULT 0,
	sample_rate  INTEGER NOT NULL DEFAULT 0,
	channels     INTEGER NOT NULL DEFAULT 0,
	size         INTEGER NOT NULL DEFAULT 0,
	UNIQUE(directory_id, filename)
);
`

// Open opens (or creates) a SQLite database at the given path and runs migrations.
//
// For file-based databases, WAL mode is enabled and the connection pool is left
// unlimited so that concurrent HTTP reads can proceed while the scanner holds a
// write connection. The write mutex (DB.mu) serialises all writes.
//
// For in-memory databases (path == ":memory:") the URI is rewritten to use a
// named shared-cache database so that all pool connections see the same schema
// and data. This is used by tests.
func Open(path string) (*DB, error) {
	var dsn string
	if path == ":memory:" {
		// Named shared-cache in-memory database: multiple pool connections share
		// one logical database within the process.
		dsn = "file::memory:?mode=memory&cache=shared&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	} else {
		// File-based: WAL gives concurrent read/write; pool is unrestricted.
		dsn = "file:" + path + "?_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	}

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db := &DB{sql: sqlDB}
	if err := db.migrate(); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// Close closes the underlying database connection.
func (db *DB) Close() error {
	return db.sql.Close()
}

func (db *DB) migrate() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, err := db.sql.Exec(schema); err != nil {
		return err
	}
	if err := db.ensureColumnLocked(
		"directories",
		"is_audio",
		`ALTER TABLE directories ADD COLUMN is_audio BOOLEAN NOT NULL DEFAULT 0`,
	); err != nil {
		return err
	}
	_, err := db.sql.Exec(`
		UPDATE directories
		SET is_audio = CASE
			WHEN codec_summary <> '' OR has_cover = 1 OR EXISTS (
				SELECT 1 FROM tracks WHERE tracks.directory_id = directories.id
			) THEN 1
			ELSE 0
		END
	`)
	return err
}

func (db *DB) ensureColumnLocked(table, column, alter string) error {
	rows, err := db.sql.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			cid        int
			name       string
			colType    string
			notNull    int
			defaultV   any
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &defaultV, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.sql.Exec(alter)
	return err
}

// UpsertLibrary inserts or updates a library record and returns its ID.
func (db *DB) UpsertLibrary(name, path string) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	res, err := db.sql.Exec(
		`INSERT INTO libraries(name, path) VALUES(?, ?)
		 ON CONFLICT(name) DO UPDATE SET path=excluded.path`,
		name, path,
	)
	if err != nil {
		return 0, fmt.Errorf("upsert library: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		// ON CONFLICT DO UPDATE: LastInsertId may be 0; fetch it.
		return db.libraryIDByNameLocked(name)
	}
	if id == 0 {
		return db.libraryIDByNameLocked(name)
	}
	return id, nil
}

func (db *DB) libraryIDByNameLocked(name string) (int64, error) {
	var id int64
	err := db.sql.QueryRow(`SELECT id FROM libraries WHERE name=?`, name).Scan(&id)
	return id, err
}

// UpsertDirectory inserts or updates a directory record and returns its ID.
func (db *DB) UpsertDirectory(libraryID int64, path, codecSummary string, hasCover bool, coverPath string) (int64, error) {
	return db.UpsertDirectoryWithAudioFlag(libraryID, path, codecSummary, hasCover, coverPath, codecSummary != "" || hasCover)
}

// UpsertDirectoryWithAudioFlag inserts or updates a directory record and stores
// whether the directory is an actual audio directory or only a structural parent.
func (db *DB) UpsertDirectoryWithAudioFlag(libraryID int64, path, codecSummary string, hasCover bool, coverPath string, isAudio bool) (int64, error) {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.sql.Exec(
		`INSERT INTO directories(library_id, path, has_cover, cover_path, codec_summary, is_audio) VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(library_id, path) DO UPDATE SET
		   has_cover=excluded.has_cover,
		   cover_path=excluded.cover_path,
		   codec_summary=excluded.codec_summary,
		   is_audio=excluded.is_audio`,
		libraryID, path, hasCover, coverPath, codecSummary, isAudio,
	)
	if err != nil {
		return 0, fmt.Errorf("upsert directory: %w", err)
	}
	// Always SELECT after upsert: ON CONFLICT DO UPDATE does not reliably update
	// last_insert_rowid across all SQLite driver versions.
	return db.directoryIDLocked(libraryID, path)
}

func (db *DB) directoryIDLocked(libraryID int64, path string) (int64, error) {
	var id int64
	err := db.sql.QueryRow(`SELECT id FROM directories WHERE library_id=? AND path=?`, libraryID, path).Scan(&id)
	return id, err
}

// UpsertTrack inserts or updates a track record.
func (db *DB) UpsertTrack(t Track) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	_, err := db.sql.Exec(
		`INSERT INTO tracks(directory_id, filename, codec, bitrate, duration, sample_rate, channels, size)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(directory_id, filename) DO UPDATE SET
		   codec=excluded.codec,
		   bitrate=excluded.bitrate,
		   duration=excluded.duration,
		   sample_rate=excluded.sample_rate,
		   channels=excluded.channels,
		   size=excluded.size`,
		t.DirectoryID, t.Filename, t.Codec, t.Bitrate, t.Duration, t.SampleRate, t.Channels, t.Size,
	)
	if err != nil {
		return fmt.Errorf("upsert track: %w", err)
	}
	return nil
}

// DeleteStaleFiles removes track rows whose filenames are not in currentFilenames.
// It queries existing filenames first and deletes stale ones in batches of 500 to
// stay well under SQLite's default SQLITE_LIMIT_VARIABLE_NUMBER (999).
func (db *DB) DeleteStaleFiles(directoryID int64, currentFilenames []string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if len(currentFilenames) == 0 {
		_, err := db.sql.Exec(`DELETE FROM tracks WHERE directory_id=?`, directoryID)
		return err
	}

	stale, err := db.staleNames(
		`SELECT filename FROM tracks WHERE directory_id=?`, directoryID,
		currentFilenames,
	)
	if err != nil {
		return err
	}
	return db.deleteInBatches(
		`DELETE FROM tracks WHERE directory_id=? AND filename IN `, directoryID, stale,
	)
}

// DeleteStaleDirectories removes directory rows whose paths are not in currentPaths.
// It queries existing paths first and deletes stale ones in batches of 500.
func (db *DB) DeleteStaleDirectories(libraryID int64, currentPaths []string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if len(currentPaths) == 0 {
		_, err := db.sql.Exec(`DELETE FROM directories WHERE library_id=?`, libraryID)
		return err
	}

	stale, err := db.staleNames(
		`SELECT path FROM directories WHERE library_id=?`, libraryID,
		currentPaths,
	)
	if err != nil {
		return err
	}
	return db.deleteInBatches(
		`DELETE FROM directories WHERE library_id=? AND path IN `, libraryID, stale,
	)
}

// staleNames queries a single text column from the DB and returns values not
// present in keepSet. The caller must hold db.mu.
func (db *DB) staleNames(query string, id int64, keep []string) ([]string, error) {
	keepSet := make(map[string]struct{}, len(keep))
	for _, v := range keep {
		keepSet[v] = struct{}{}
	}

	rows, err := db.sql.Query(query, id)
	if err != nil {
		return nil, fmt.Errorf("query existing rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var stale []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		if _, ok := keepSet[name]; !ok {
			stale = append(stale, name)
		}
	}
	return stale, rows.Err()
}

// deleteInBatches executes a DELETE … IN (?) for stale values in chunks of 500.
// queryPrefix must end with " IN " — placeholders and the closing paren are appended.
// The caller must hold db.mu.
func (db *DB) deleteInBatches(queryPrefix string, id int64, stale []string) error {
	const chunkSize = 500
	for len(stale) > 0 {
		chunk := stale
		if len(chunk) > chunkSize {
			chunk = chunk[:chunkSize]
		}
		stale = stale[len(chunk):]

		args := make([]any, 0, 1+len(chunk))
		args = append(args, id)
		placeholders := make([]string, len(chunk))
		for i, v := range chunk {
			placeholders[i] = "?"
			args = append(args, v)
		}
		q := queryPrefix + "(" + strings.Join(placeholders, ",") + ")"
		if _, err := db.sql.Exec(q, args...); err != nil {
			return fmt.Errorf("delete stale rows: %w", err)
		}
	}
	return nil
}

// GetLibraries returns all libraries.
func (db *DB) GetLibraries() ([]Library, error) {
	rows, err := db.sql.Query(`SELECT id, name, path FROM libraries ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var libs []Library
	for rows.Next() {
		var l Library
		if err := rows.Scan(&l.ID, &l.Name, &l.Path); err != nil {
			return nil, err
		}
		libs = append(libs, l)
	}
	return libs, rows.Err()
}

// GetDirectoryTree returns all directories for a library.
func (db *DB) GetDirectoryTree(libraryID int64) ([]Directory, error) {
	rows, err := db.sql.Query(
		`SELECT id, library_id, path, has_cover, cover_path, codec_summary, is_audio
		 FROM directories WHERE library_id=? ORDER BY path`,
		libraryID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanDirectories(rows)
}

// GetDirectoryChildren returns direct children of parentPath within a library.
// A direct child has exactly one more path segment than the parent.
func (db *DB) GetDirectoryChildren(libraryID int64, parentPath string) ([]Directory, error) {
	// Normalize: ensure parentPath ends with "/" for prefix matching.
	prefix := parentPath
	if prefix != "" && prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}

	// Escape LIKE wildcards so directory paths containing % or _ match literally.
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
	rows, err := db.sql.Query(
		`SELECT id, library_id, path, has_cover, cover_path, codec_summary, is_audio
		 FROM directories WHERE library_id=? AND path LIKE ? ESCAPE '\' ORDER BY path`,
		libraryID, escaped+"%",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	all, err := scanDirectories(rows)
	if err != nil {
		return nil, err
	}

	// Filter to only immediate children (no "/" after the prefix).
	children := make([]Directory, 0, len(all))
	prefixLen := len(prefix)
	for _, d := range all {
		if len(d.Path) <= prefixLen {
			continue
		}
		rest := d.Path[prefixLen:]
		// No slash in the remainder means it's a direct child.
		hasSlash := false
		for _, c := range rest {
			if c == '/' {
				hasSlash = true
				break
			}
		}
		if !hasSlash {
			children = append(children, d)
		}
	}
	return children, nil
}

// GetDirectoryByPath returns a single directory by library and path.
func (db *DB) GetDirectoryByPath(libraryID int64, path string) (*Directory, error) {
	var d Directory
	err := db.sql.QueryRow(
		`SELECT id, library_id, path, has_cover, cover_path, codec_summary, is_audio
		 FROM directories WHERE library_id=? AND path=?`,
		libraryID, path,
	).Scan(&d.ID, &d.LibraryID, &d.Path, &d.HasCover, &d.CoverPath, &d.CodecSummary, &d.IsAudio)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// GetDirectoryFiles returns all tracks in a directory.
func (db *DB) GetDirectoryFiles(directoryID int64) ([]Track, error) {
	rows, err := db.sql.Query(
		`SELECT id, directory_id, filename, codec, bitrate, duration, sample_rate, channels, size
		 FROM tracks WHERE directory_id=? ORDER BY filename`,
		directoryID,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var tracks []Track
	for rows.Next() {
		var t Track
		if err := rows.Scan(&t.ID, &t.DirectoryID, &t.Filename, &t.Codec, &t.Bitrate, &t.Duration, &t.SampleRate, &t.Channels, &t.Size); err != nil {
			return nil, err
		}
		tracks = append(tracks, t)
	}
	return tracks, rows.Err()
}

// HasDirectChildDirectory reports whether parentPath has any indexed direct child
// directories within the library.
func (db *DB) HasDirectChildDirectory(libraryID int64, parentPath string) (bool, error) {
	if parentPath == "" {
		var exists bool
		err := db.sql.QueryRow(
			`SELECT EXISTS(
				SELECT 1
				FROM directories
				WHERE library_id=? AND instr(path, '/')=0
			)`,
			libraryID,
		).Scan(&exists)
		return exists, err
	}

	prefix := parentPath
	if prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)

	var exists bool
	err := db.sql.QueryRow(
		`SELECT EXISTS(
			SELECT 1
			FROM directories
			WHERE library_id=?
			  AND path LIKE ? ESCAPE '\'
			  AND instr(substr(path, ?), '/')=0
		)`,
		libraryID,
		escaped+"%",
		len(prefix)+1,
	).Scan(&exists)
	return exists, err
}

func scanDirectories(rows *sql.Rows) ([]Directory, error) {
	var dirs []Directory
	for rows.Next() {
		var d Directory
		if err := rows.Scan(&d.ID, &d.LibraryID, &d.Path, &d.HasCover, &d.CoverPath, &d.CodecSummary, &d.IsAudio); err != nil {
			return nil, err
		}
		dirs = append(dirs, d)
	}
	return dirs, rows.Err()
}
