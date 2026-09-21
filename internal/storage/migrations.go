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

// schema.sql is the frozen released v1 schema; schemaV2DDL is the immutable
// v2 migration body; schemaV3DDL is the immutable v3 migration body.
// Fresh databases apply v1 then v2 then v3 sequentially — there
// is no separate "latest schema" path that could diverge from upgrading.
const currentSchemaVersion = 3

func schemaChecksum() string {
	sum := sha256.Sum256([]byte(schemaSQL))
	return fmt.Sprintf("%x", sum)
}

func schemaV2Checksum() string {
	sum := sha256.Sum256([]byte(schemaV2DDL + backfillControllerLeaseProvenance))
	return fmt.Sprintf("%x", sum)
}

func schemaV3Checksum() string {
	sum := sha256.Sum256([]byte(schemaV3DDL + backfillRunProfilesAndArtifacts))
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

	if currentVer > currentSchemaVersion {
		return ErrUnsupportedSchemaVersion
	}

	if currentVer >= 1 {
		// Verify the frozen v1 checksum before any upgrade step.
		var recordedChecksum string
		err := tx.Tx().QueryRow("SELECT checksum FROM schema_migrations WHERE version = 1;").Scan(&recordedChecksum)
		if err != nil {
			return fmt.Errorf("read recorded checksum: %w", err)
		}
		if recordedChecksum != schemaChecksum() {
			return ErrMigrationChecksumMismatch
		}
	} else {
		// currentVer == 0: apply the frozen v1 schema sequentially.
		if _, err := tx.Tx().Exec(schemaSQL); err != nil {
			return fmt.Errorf("execute schema v1: %w", err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err = tx.Tx().Exec(`
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (1, 'initial_schema', ?, ?);`, schemaChecksum(), now)
		if err != nil {
			return fmt.Errorf("record migration v1: %w", err)
		}
	}

	if currentVer >= 2 {
		// Verify the v2 checksum before proceeding.
		var recordedV2Checksum string
		err := tx.Tx().QueryRow("SELECT checksum FROM schema_migrations WHERE version = 2;").Scan(&recordedV2Checksum)
		if err != nil {
			return fmt.Errorf("read recorded v2 checksum: %w", err)
		}
		if recordedV2Checksum != schemaV2Checksum() {
			return ErrMigrationChecksumMismatch
		}
	} else {
		// Apply v2: schema additions plus legacy provenance backfill, atomically.
		if _, err := tx.Tx().Exec(schemaV2DDL); err != nil {
			return fmt.Errorf("execute schema v2: %w", err)
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.Tx().Exec(backfillControllerLeaseProvenance, now, now); err != nil {
			return fmt.Errorf("backfill controller lease provenance: %w", err)
		}

		if s.testHookBeforeCommit != nil {
			hookErr := func() (err error) {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("injected migration failure: %v", r)
					}
				}()
				s.testHookBeforeCommit("pre_commit_migration_v2")
				return nil
			}()
			if hookErr != nil {
				return hookErr
			}
		}

		_, err = tx.Tx().Exec(`
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (2, 'controller_leases_provenance', ?, ?);`, schemaV2Checksum(), now)
		if err != nil {
			return fmt.Errorf("record migration v2: %w", err)
		}
	}

	if currentVer >= 3 {
		// Verify the v3 checksum and stop: fully migrated.
		var recordedV3Checksum string
		err := tx.Tx().QueryRow("SELECT checksum FROM schema_migrations WHERE version = 3;").Scan(&recordedV3Checksum)
		if err != nil {
			return fmt.Errorf("read recorded v3 checksum: %w", err)
		}
		if recordedV3Checksum != schemaV3Checksum() {
			return ErrMigrationChecksumMismatch
		}
		return nil
	}

	// Apply v3: run_profiles, artifact_revisions columns, proposal_sets, proposal_set_members, and backfills.
	if _, err := tx.Tx().Exec(schemaV3DDL); err != nil {
		return fmt.Errorf("execute schema v3: %w", err)
	}
	if _, err := tx.Tx().Exec(backfillRunProfilesAndArtifacts); err != nil {
		return fmt.Errorf("backfill run profiles and artifacts: %w", err)
	}

	if s.testHookBeforeCommit != nil {
		hookErr := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("injected migration failure: %v", r)
				}
			}()
			s.testHookBeforeCommit("pre_commit_migration_v3")
			return nil
		}()
		if hookErr != nil {
			return hookErr
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Tx().Exec(`
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (3, 'run_profiles_proposal_sets', ?, ?);`, schemaV3Checksum(), now)
	if err != nil {
		return fmt.Errorf("record migration v3: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}
