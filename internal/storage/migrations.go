package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"fmt"
	"time"
)

//go:embed schema.sql
var schemaSQL string

func schemaChecksum() string {
	sum := sha256.Sum256([]byte(schemaSQL))
	return fmt.Sprintf("%x", sum)
}

func (s *Store) CurrentSchemaVersion() (int, error) {
	var count int
	err := s.writeDB.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations';").Scan(&count)
	if err != nil {
		return 0, err
	}
	if count == 0 {
		return 0, nil
	}

	var maxVer sql.NullInt64
	err = s.writeDB.QueryRow("SELECT max(version) FROM schema_migrations;").Scan(&maxVer)
	if err != nil {
		return 0, err
	}
	if !maxVer.Valid {
		return 0, nil
	}
	return int(maxVer.Int64), nil
}

func (s *Store) Migrate() error {
	tx, err := s.BeginWrite(context.Background())
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer tx.Rollback()

	// First check or create schema_migrations table inside transaction
	_, err = tx.Tx().Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER NOT NULL PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TEXT NOT NULL
);`)
	if err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	var maxVer sql.NullInt64
	err = tx.Tx().QueryRow("SELECT max(version) FROM schema_migrations;").Scan(&maxVer)
	if err != nil {
		return fmt.Errorf("check current schema version: %w", err)
	}
	var currentVer int
	if maxVer.Valid {
		currentVer = int(maxVer.Int64)
	}

	expectedChecksum := schemaChecksum()

	if currentVer > 1 {
		return ErrUnsupportedSchemaVersion
	}

	if currentVer == 1 {
		// Verify checksum
		var recordedChecksum string
		err := tx.Tx().QueryRow("SELECT checksum FROM schema_migrations WHERE version = 1;").Scan(&recordedChecksum)
		if err != nil {
			return fmt.Errorf("read recorded checksum: %w", err)
		}
		if recordedChecksum != expectedChecksum {
			return ErrMigrationChecksumMismatch
		}
		return nil // Clean no-op, rollback read
	}

	// currentVer == 0: apply schema v1
	if _, err := tx.Tx().Exec(schemaSQL); err != nil {
		return fmt.Errorf("execute schema v1: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Tx().Exec(`
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (1, 'initial_schema', ?, ?);`, expectedChecksum, now)
	if err != nil {
		return fmt.Errorf("record migration v1: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration v1: %w", err)
	}
	return nil
}
