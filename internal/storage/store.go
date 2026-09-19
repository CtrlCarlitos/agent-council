package storage

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type Store struct {
	writeDB  *sql.DB
	readDB   *sql.DB
	stateDir string
}

func buildDSN(dbPath string, txLock string) string {
	cleanPath := filepath.Clean(dbPath)
	u := &url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(cleanPath),
	}
	q := url.Values{}
	q.Set("_txlock", txLock)
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
	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}

	// Install .gitignore with *
	gitignorePath := filepath.Join(opts.StateDir, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		_ = os.WriteFile(gitignorePath, []byte("*\n"), 0600)
	}

	dbPath := filepath.Join(opts.StateDir, "state.db")
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
		writeDB:  writeDB,
		readDB:   readDB,
		stateDir: opts.StateDir,
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

	return s, nil
}

func (s *Store) verifyConnectionPRAGMAs(db *sql.DB) error {
	var jm string
	if err := db.QueryRow("PRAGMA journal_mode;").Scan(&jm); err != nil || jm != "wal" {
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
