// Package db implements SQLite database operations for SLB.
// Uses modernc.org/sqlite (pure Go, no cgo) with WAL mode and FTS5.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite" // Pure Go SQLite driver
)

// DB wraps the SQLite database connection.
type DB struct {
	conn *sql.DB
	path string
	mu   sync.RWMutex
}

// OpenOptions configures database opening behavior.
type OpenOptions struct {
	// CreateIfNotExists creates the database file if it doesn't exist.
	CreateIfNotExists bool
	// InitSchema initializes the schema if the database is new.
	InitSchema bool
	// ReadOnly opens the database in read-only mode.
	ReadOnly bool
}

// DefaultOpenOptions returns sensible defaults for opening a database.
func DefaultOpenOptions() OpenOptions {
	return OpenOptions{
		CreateIfNotExists: true,
		InitSchema:        true,
		ReadOnly:          false,
	}
}

// Open opens a database connection with WAL mode enabled.
func Open(path string) (*DB, error) {
	return OpenWithOptions(path, DefaultOpenOptions())
}

// OpenWithOptions opens a database connection with the given options.
func OpenWithOptions(path string, opts OpenOptions) (*DB, error) {
	if path == "" || strings.ContainsRune(path, '\x00') {
		return nil, fmt.Errorf("database path must be nonempty and contain no NUL bytes")
	}
	inMemory := path == ":memory:"
	if inMemory && opts.ReadOnly {
		return nil, fmt.Errorf("an in-memory database cannot be opened read-only")
	}
	if !inMemory {
		// database/sql may open more connections after the caller changes CWD.
		// Every connection must continue to use the originally selected file.
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolving database path: %w", err)
		}
		path = absolute
	}

	// Only explicit writable creation may change the filesystem. SQLite will
	// subsequently use mode=rw so it cannot recreate a removed file with
	// default permissions when the connection pool opens another connection.
	if !inMemory && opts.CreateIfNotExists && !opts.ReadOnly {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("creating database directory: %w", err)
		}

		// Ensure the file exists with restrictive permissions before SQLite opens it
		// (SQLite usually respects umask, but being explicit is safer)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("creating database file: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("closing database file: %w", err)
		}
	}

	// Build connection string with pragmas
	// Note: modernc.org/sqlite uses different pragma syntax
	// Writers begin every transaction with BEGIN IMMEDIATE. A deferred
	// transaction whose first statement reads and then writes (the claim and
	// review paths lock a row with a self-assigning UPDATE) is refused with
	// SQLITE_BUSY the moment another connection committed in between, and the
	// busy handler is deliberately not consulted for that lock upgrade, so
	// concurrent claimers lost with "database is locked" instead of waiting
	// their turn. Taking the write lock up front makes contenders queue on
	// busy_timeout and serializes them, which is what the single-winner
	// contract needs. Read-only transactions opt out with
	// sql.TxOptions{ReadOnly: true}, and read-only handles cannot take the
	// lock at all, so they keep deferred transactions.
	query := url.Values{
		"_pragma": {"journal_mode(WAL)", "busy_timeout(5000)", "synchronous(NORMAL)", "foreign_keys(ON)"},
		"mode":    {"rw"},
	}
	if opts.ReadOnly {
		query.Set("mode", "ro")
	} else {
		query.Set("_txlock", "immediate")
	}
	var filename string
	if inMemory {
		filename = "file::memory:"
		query.Set("mode", "memory")
	} else {
		// The API accepts a filename, never a caller-supplied SQLite URI.
		// URL.Path escapes literal %, # and ? so neither SQLite nor the driver
		// can reinterpret the filename as another file or connection options.
		uriPath := filepath.ToSlash(path)
		if filepath.Separator == '\\' {
			if strings.HasPrefix(path, `\\?\`) {
				// SQLite's Windows VFS recognizes the native long-path prefix;
				// preserve its backslashes through URI percent encoding.
				uriPath = "/" + path
			} else if !strings.HasPrefix(uriPath, "/") {
				uriPath = "/" + uriPath
			}
		}
		uri := url.URL{Scheme: "file", Path: uriPath}
		filename = uri.String()
	}
	dsn := filename + "?" + query.Encode()

	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	// Verify connection
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	db := &DB{
		conn: conn,
		path: path,
	}

	// Initialize schema if requested
	if opts.InitSchema {
		if err := db.InitSchema(); err != nil {
			conn.Close()
			return nil, fmt.Errorf("initializing schema: %w", err)
		}
	}

	return db, nil
}

// OpenAndMigrate opens a database at the given path, initializing the schema
// and applying any pending migrations.
func OpenAndMigrate(path string) (*DB, error) {
	db, err := Open(path)
	if err != nil {
		return nil, err
	}
	if err := db.ApplyMigrations(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// ProjectDatabasePath returns the state path for a discovered project without
// creating it. An uninitialized directory may create its first database, but
// an existing marker with missing or unreadable state may have lost custom
// policy and must be repaired or explicitly initialized before ordinary use.
func ProjectDatabasePath(project string) (string, error) {
	if project == "" {
		return "", fmt.Errorf("project directory is required")
	}
	marker := filepath.Join(project, ".slb")
	path := filepath.Join(marker, "state.db")
	if _, err := os.Lstat(marker); os.IsNotExist(err) {
		return path, nil
	} else if err != nil {
		return "", fmt.Errorf("reading project state directory: %w", err)
	}
	info, err := os.Stat(marker)
	if err != nil {
		return "", fmt.Errorf("reading project state directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project state path %q is not a directory", marker)
	}
	info, err = os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("project state database %q is unavailable; restore it or initialize this project explicitly with slb init: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("project state database %q is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("reading project state database %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("closing project state database %q: %w", path, err)
	}
	return path, nil
}

// OpenProjectDB opens the project-level database at .slb/state.db.
func OpenProjectDB(projectPath string) (*DB, error) {
	dbPath := filepath.Join(projectPath, ".slb", "state.db")
	return Open(dbPath)
}

// OpenUserDB opens the user-level database at ~/.slb/history.db.
func OpenUserDB() (*DB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("getting home directory: %w", err)
	}
	dbPath := filepath.Join(home, ".slb", "history.db")
	return Open(dbPath)
}

// Close closes the database connection.
func (db *DB) Close() error {
	return db.conn.Close()
}

// Path returns the database file path.
func (db *DB) Path() string {
	return db.path
}

// InitSchema initializes the schema by applying migrations.
func (db *DB) InitSchema() error {
	return db.ApplyMigrations(context.Background())
}

// GetSchemaVersion returns the current schema version.
func (db *DB) GetSchemaVersion() (int, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()

	if err := ensureMigrationsTable(db.conn); err != nil {
		return 0, err
	}
	return currentVersion(db.conn)
}

// ValidateSchema ensures the database is at the expected schema version.
func (db *DB) ValidateSchema() error {
	version, err := db.GetSchemaVersion()
	if err != nil {
		return err
	}
	if version != SchemaVersion {
		return fmt.Errorf("schema version mismatch: have %d want %d", version, SchemaVersion)
	}
	return nil
}

// Exec executes a SQL statement.
func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.conn.Exec(query, args...)
}

// Query executes a query that returns rows.
func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.conn.Query(query, args...)
}

// QueryRow executes a query that returns a single row.
func (db *DB) QueryRow(query string, args ...any) *sql.Row {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.conn.QueryRow(query, args...)
}

// Begin starts a transaction.
func (db *DB) Begin() (*sql.Tx, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.conn.Begin()
}

// Transaction executes a function within a transaction.
// If the function returns an error, the transaction is rolled back.
func (db *DB) Transaction(fn func(*sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}

// Stats returns database statistics.
type Stats struct {
	Path          string `json:"path"`
	SchemaVersion int    `json:"schema_version"`
	SessionCount  int    `json:"session_count"`
	RequestCount  int    `json:"request_count"`
	ReviewCount   int    `json:"review_count"`
}

// GetStats returns database statistics.
func (db *DB) GetStats() (*Stats, error) {
	stats := &Stats{Path: db.path}

	version, err := db.GetSchemaVersion()
	if err != nil {
		return nil, err
	}
	stats.SchemaVersion = version

	db.mu.RLock()
	defer db.mu.RUnlock()

	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&stats.SessionCount); err != nil {
		return nil, err
	}
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&stats.RequestCount); err != nil {
		return nil, err
	}
	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM reviews`).Scan(&stats.ReviewCount); err != nil {
		return nil, err
	}

	return stats, nil
}
