package storage

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	writeDB              *sql.DB
	readDB               *sql.DB
	stateDir             string
	testHookBeforeCommit func(boundary string)
}

func isSystemRootSymlink(path string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	clean := filepath.Clean(path)
	return clean == "/var" || clean == "/tmp" || clean == "/etc"
}

// ensureNoSymlink verifies that path and all its ancestor components do not contain
// symbolic links or Windows reparse points.
func ensureNoSymlink(path string) error {
	if path == "" {
		return nil
	}
	cleanPath := filepath.Clean(path)
	absPath, err := filepath.Abs(cleanPath)
	if err == nil {
		cleanPath = absPath
	}

	// Build list of ancestor components from root down to cleanPath
	var components []string
	curr := cleanPath
	for {
		components = append(components, curr)
		parent := filepath.Dir(curr)
		if parent == curr || parent == "." || parent == "" {
			break
		}
		curr = parent
	}

	// Inspect each component from root to leaf
	for i := len(components) - 1; i >= 0; i-- {
		comp := components[i]
		if isSystemRootSymlink(comp) {
			continue
		}
		fi, err := os.Lstat(comp)
		if err != nil {
			if os.IsNotExist(err) {
				// Leaf or partial subpath does not exist yet; remaining ancestors checked
				break
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlinkForbidden, comp)
		}
		if runtime.GOOS == "windows" && (fi.Mode()&os.ModeIrregular != 0 && fi.Mode()&os.ModeDir != 0) {
			return fmt.Errorf("%w: %s", ErrSymlinkForbidden, comp)
		}
	}
	return nil
}

func buildDSN(dbPath string, txLock string) string {
	absPath, err := filepath.Abs(dbPath)
	if err == nil {
		dbPath = absPath
	}
	cleanPath := filepath.Clean(dbPath)
	slashPath := filepath.ToSlash(cleanPath)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	u := &url.URL{
		Scheme: "file",
		Path:   slashPath,
	}
	q := url.Values{}
	q.Set("_txlock", txLock)
	q.Set("_timeout", "5000")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(ON)")
	u.RawQuery = q.Encode()
	return u.String()
}

func Open(opts StoreOptions) (*Store, error) {
	if opts.StateDir == "" {
		return nil, fmt.Errorf("state directory required")
	}

	// Reject symlinked state directories and components
	if err := ensureNoSymlink(opts.StateDir); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(opts.StateDir, "state.db")
	if err := ensureNoSymlink(dbPath); err != nil {
		return nil, err
	}
	if err := ensureNoSymlink(filepath.Join(opts.StateDir, "state.db-wal")); err != nil {
		return nil, err
	}
	if err := ensureNoSymlink(filepath.Join(opts.StateDir, "state.db-shm")); err != nil {
		return nil, err
	}
	if err := ensureNoSymlink(filepath.Join(opts.StateDir, "artifacts")); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}

	// Tighten permissions on state directory before creating database or sidecars
	if err := EnsureDirectoryPermissions(opts.StateDir); err != nil {
		return nil, fmt.Errorf("ensure state directory permissions: %w", err)
	}

	// Install .gitignore with *
	gitignorePath := filepath.Join(opts.StateDir, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		_ = os.WriteFile(gitignorePath, []byte("*\n"), 0600)
	}

	writeDSN := buildDSN(dbPath, "immediate")
	writeDB, err := sql.Open("sqlite", writeDSN)
	if err != nil {
		return nil, fmt.Errorf("open sqlite write db: %w", err)
	}
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)

	readDSN := buildDSN(dbPath, "deferred")
	readDB, err := sql.Open("sqlite", readDSN)
	if err != nil {
		writeDB.Close()
		return nil, fmt.Errorf("open sqlite read db: %w", err)
	}

	s := &Store{
		writeDB:              writeDB,
		readDB:               readDB,
		stateDir:             opts.StateDir,
		testHookBeforeCommit: opts.TestHookBeforeCommit,
	}

	// Verify PRAGMAs on write connection
	if err := s.verifyConnectionPRAGMAs(writeDB); err != nil {
		s.Close()
		return nil, fmt.Errorf("verify write db pragmas: %w", err)
	}

	// Verify PRAGMAs on read connection
	if err := s.verifyConnectionPRAGMAs(readDB); err != nil {
		s.Close()
		return nil, fmt.Errorf("verify read db pragmas: %w", err)
	}

	// Run migration lifecycle
	if err := s.Migrate(); err != nil {
		s.Close()
		return nil, err
	}

	if err := s.TightenStateDirPermissions(); err != nil {
		s.Close()
		return nil, fmt.Errorf("tighten state dir permissions: %w", err)
	}

	return s, nil
}

func (s *Store) verifyConnectionPRAGMAs(db *sql.DB) error {
	var jm string
	var err error
	for attempt := 0; attempt < 25; attempt++ {
		err = db.QueryRow("PRAGMA journal_mode;").Scan(&jm)
		if err == nil && jm == "wal" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || jm != "wal" {
		return fmt.Errorf("failed to verify WAL mode (got %q, err %v)", jm, err)
	}
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys;").Scan(&fk); err != nil || fk != 1 {
		return fmt.Errorf("failed to verify foreign_keys (got %d, err %v)", fk, err)
	}
	var sync int
	if err := db.QueryRow("PRAGMA synchronous;").Scan(&sync); err != nil || sync != 2 {
		return fmt.Errorf("failed to verify synchronous FULL (got %d, err %v)", sync, err)
	}
	var busyTimeout int
	if err := db.QueryRow("PRAGMA busy_timeout;").Scan(&busyTimeout); err != nil || busyTimeout != 5000 {
		return fmt.Errorf("failed to verify busy_timeout (got %d, err %v)", busyTimeout, err)
	}
	return nil
}

func (s *Store) Close() error {
	var err1, err2 error
	if s.writeDB != nil {
		err1 = s.writeDB.Close()
	}
	if s.readDB != nil {
		err2 = s.readDB.Close()
	}
	if err1 != nil {
		return err1
	}
	return err2
}

func (s *Store) DB() *sql.DB {
	return s.writeDB
}

func (s *Store) ReadDB() *sql.DB {
	return s.readDB
}

func (s *Store) StateDir() string {
	return s.stateDir
}
