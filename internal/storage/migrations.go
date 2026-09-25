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
// v2 migration body; schemaV3DDL is the immutable v3 migration body;
// claudeStateV4DDL, codexStateV5DDL, codexUncertaintyV6DDL, and
// agyStateV7DDL are the immutable v4/v5/v6/v7 bodies.
// Fresh databases apply v1 then v2 then v3 sequentially — there
// is no separate "latest schema" path that could diverge from upgrading.
const currentSchemaVersion = 7

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

	if currentVer >= 7 {
		// Fully migrated: nothing to do.
		return nil
	}

	if currentVer >= 3 {
		// Verify the v3 checksum before proceeding.
		var recordedV3Checksum string
		err := tx.Tx().QueryRow("SELECT checksum FROM schema_migrations WHERE version = 3;").Scan(&recordedV3Checksum)
		if err != nil {
			return fmt.Errorf("read recorded v3 checksum: %w", err)
		}
		if recordedV3Checksum != schemaV3Checksum() {
			return ErrMigrationChecksumMismatch
		}
		if currentVer < 4 {
			// v3 verified: apply v4 (AC-008 Claude adapter state).
			if _, err := tx.Tx().Exec(claudeStateV4DDL); err != nil {
				return fmt.Errorf("execute schema v4: %w", err)
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.Tx().Exec(`
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (4, 'claude_adapter_state', ?, ?);`, "claude-state-v4", now); err != nil {
				return fmt.Errorf("record migration v4: %w", err)
			}
		}
		if currentVer < 5 {
			// Apply v5: AC-009 Codex adapter durable state tables.
			if _, err := tx.Tx().Exec(codexStateV5DDL); err != nil {
				return fmt.Errorf("execute schema v5: %w", err)
			}
			now := time.Now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.Tx().Exec(`
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (5, 'codex_adapter_state', ?, ?);`, "codex-state-v5", now); err != nil {
				return fmt.Errorf("record migration v5: %w", err)
			}
		}
		if currentVer < 6 {
			// Apply v6: AC-009 creation-uncertainty episodes.
			if err := applyMigrationV6(tx.Tx()); err != nil {
				return err
			}
		}
		// Apply v7: AC-010 Agy adapter durable state tables and the
		// required_tools_json column on queued prompts. Reached only
		// when currentVer < 7 (the top-of-function guard returned
		// early otherwise), so this is always exactly one application.
		if err := applyMigrationV7(tx.Tx()); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration: %w", err)
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

	// Apply v4: AC-008 Claude adapter durable state tables.
	if _, err := tx.Tx().Exec(claudeStateV4DDL); err != nil {
		return fmt.Errorf("execute schema v4: %w", err)
	}
	now = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Tx().Exec(`
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (4, 'claude_adapter_state', ?, ?);`, "claude-state-v4", now); err != nil {
		return fmt.Errorf("record migration v4: %w", err)
	}

	// Apply v5: AC-009 Codex adapter durable state tables.
	if _, err := tx.Tx().Exec(codexStateV5DDL); err != nil {
		return fmt.Errorf("execute schema v5: %w", err)
	}
	now = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Tx().Exec(`
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (5, 'codex_adapter_state', ?, ?);`, "codex-state-v5", now); err != nil {
		return fmt.Errorf("record migration v5: %w", err)
	}

	// Apply v6: AC-009 creation-uncertainty episodes.
	if err := applyMigrationV6(tx.Tx()); err != nil {
		return err
	}

	// Apply v7: AC-010 Agy adapter durable state tables and the
	// required_tools_json column on queued prompts.
	if err := applyMigrationV7(tx.Tx()); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

// claudeStateV4DDL creates the AC-008 Claude adapter durable state
// tables (bindings, turn attempts, launch reservations, attestations).
const claudeStateV4DDL = `
CREATE TABLE IF NOT EXISTS claude_session_bindings (
	session_id         TEXT PRIMARY KEY,
	native_id          TEXT NOT NULL UNIQUE,
	materialized       INTEGER NOT NULL DEFAULT 0,
	model              TEXT NOT NULL,
	workspace          TEXT NOT NULL,
	config_root        TEXT NOT NULL,
	template_digest    TEXT NOT NULL,
	first_prompt_digest TEXT NOT NULL,
	created_at         TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS claude_turn_attempts (
	attempt_id         TEXT NOT NULL,
	session_id         TEXT NOT NULL,
	turn_key           TEXT NOT NULL,
	native_id          TEXT NOT NULL,
	prompt_digest      TEXT NOT NULL,
	baseline_file_identity TEXT NOT NULL DEFAULT '',
	baseline_size      INTEGER NOT NULL DEFAULT 0,
	baseline_entries   INTEGER NOT NULL DEFAULT 0,
	materialized_baseline INTEGER NOT NULL DEFAULT 0,
	transcript_protection TEXT NOT NULL DEFAULT 'advisory',
	protection_attestation_id TEXT NOT NULL DEFAULT '',
	launch_count       INTEGER NOT NULL DEFAULT 0 CHECK (launch_count BETWEEN 0 AND 2),
	absence_redispatch_consumed INTEGER NOT NULL DEFAULT 0,
	absence_verified        TEXT NOT NULL DEFAULT '',
	accepted           INTEGER,
	terminal           INTEGER NOT NULL DEFAULT 0,
	result_payload     TEXT,
	result_usage       TEXT,
	observed_status    TEXT NOT NULL DEFAULT 'uncertain',
	uncertainty_disposition TEXT,
	disposition_actor  TEXT,
	disposition_generation INTEGER,
	disposition_op_id  TEXT,
	disposition_at     TEXT,
	transition_version INTEGER NOT NULL DEFAULT 1,
	created_at         TEXT NOT NULL,
	updated_at         TEXT NOT NULL,
	UNIQUE(session_id, turn_key, attempt_id)
);
CREATE TABLE IF NOT EXISTS claude_attempt_launches (
	attempt_id         TEXT NOT NULL,
	reservation_seq    INTEGER NOT NULL,
	state              TEXT NOT NULL DEFAULT 'reserved',
	started_at         TEXT,
	start_failed_at    TEXT,
	first_stdin_byte_at TEXT,
	known_dead_at      TEXT,
	exit_code          INTEGER,
	executor_identity  TEXT NOT NULL,
	UNIQUE(attempt_id, reservation_seq)
);
CREATE TABLE IF NOT EXISTS claude_protection_attestations (
	attestation_id     TEXT PRIMARY KEY,
	claude_version     TEXT NOT NULL,
	platform           TEXT NOT NULL,
	manifest_digest    TEXT NOT NULL,
	template_digest    TEXT NOT NULL,
	probe_results      TEXT NOT NULL,
	probed_at          TEXT NOT NULL,
	actor              TEXT NOT NULL
);

`

// codexStateV5DDL creates the AC-009 Codex adapter durable state tables
// (bindings, turn attempts, launch reservations, cprot-v2 attestations).
const codexStateV5DDL = `
CREATE TABLE IF NOT EXISTS codex_session_bindings (
	session_id         TEXT PRIMARY KEY,
	native_id          TEXT NOT NULL UNIQUE
	                   CHECK (native_id GLOB '[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-7[0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'),
	materialized       INTEGER NOT NULL DEFAULT 0,
	model              TEXT NOT NULL,
	workspace          TEXT NOT NULL,
	rollout_path       TEXT,
	profile_digest     TEXT NOT NULL,
	first_prompt_digest TEXT,
	created_at         TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS codex_turn_attempts (
	attempt_id         TEXT NOT NULL,
	session_id         TEXT NOT NULL,
	turn_key           TEXT NOT NULL,
	transition_version INTEGER NOT NULL DEFAULT 1,
	native_turn_id     TEXT,
	baseline_file_identity TEXT NOT NULL DEFAULT '',
	baseline_size      INTEGER NOT NULL DEFAULT 0,
	baseline_entries   INTEGER NOT NULL DEFAULT 0,
	materialized_baseline INTEGER NOT NULL DEFAULT 0,
	rollout_protection TEXT NOT NULL DEFAULT 'advisory',
	protection_attestation_id TEXT,
	prompt_digest      TEXT NOT NULL,
	launch_count       INTEGER NOT NULL DEFAULT 0 CHECK (launch_count BETWEEN 0 AND 2),
	absence_redispatch_consumed INTEGER NOT NULL DEFAULT 0,
	absence_verified        TEXT NOT NULL DEFAULT '',
	accepted           INTEGER,
	terminal           INTEGER NOT NULL DEFAULT 0,
	result_payload     TEXT,
	result_usage       TEXT,
	observed_status    TEXT NOT NULL DEFAULT 'uncertain',
	uncertainty_disposition TEXT,
	created_at         TIMESTAMP NOT NULL,
	updated_at         TIMESTAMP NOT NULL,
	UNIQUE(session_id, turn_key, attempt_id)
);
CREATE TABLE IF NOT EXISTS codex_attempt_launches (
	attempt_id         TEXT NOT NULL,
	reservation_seq    INTEGER NOT NULL,
	state              TEXT NOT NULL DEFAULT 'reserved',
	started_at         TIMESTAMP,
	start_failed_at    TIMESTAMP,
	first_stdin_byte_at TIMESTAMP,
	known_dead_at      TIMESTAMP,
	exit_code          INTEGER,
	child_generation   INTEGER NOT NULL DEFAULT 0,
	executor_identity  TEXT NOT NULL,
	UNIQUE(attempt_id, reservation_seq)
);
CREATE TABLE IF NOT EXISTS codex_protection_attestations (
	attestation_id     TEXT PRIMARY KEY
	                   CHECK (length(attestation_id) = 80
	                          AND attestation_id LIKE 'cprot-v2:sha256:%'
	                          AND substr(attestation_id, 17) NOT GLOB '*[^0-9a-f]*'),
	codex_version      TEXT NOT NULL,
	platform           TEXT NOT NULL,
	manifest_digest    TEXT NOT NULL,
	profile_digest     TEXT NOT NULL,
	probe_results      TEXT NOT NULL,
	probed_at          TIMESTAMP NOT NULL,
	actor              TEXT NOT NULL
);
`

// codexUncertaintyV6DDL creates the AC-009 durable creation-uncertainty
// EPISODE table (spec §3.4): one row per uncertain creation outcome of a
// logical session, monotonically numbered, resolved one exact episode at
// a time by a controller-authorized journal operation. The partial
// unique index enforces at most ONE open episode per session at the
// schema level. Resolution provenance is the controller generation and
// the resolving op_id (the journal entry carries the credential).
const codexUncertaintyV6DDL = `
CREATE TABLE IF NOT EXISTS codex_creation_uncertainties (
	session_id         TEXT NOT NULL,
	episode            INTEGER NOT NULL CHECK (episode >= 1),
	run_id             TEXT NOT NULL,
	reason             TEXT NOT NULL,
	recorded_by        TEXT NOT NULL,
	record_op_id       TEXT NOT NULL,
	cause_op_id        TEXT NOT NULL,
	recorded_at        TEXT NOT NULL,
	disposition        TEXT,
	resolution_reason  TEXT,
	resolution_generation INTEGER,
	resolution_op_id   TEXT,
	resolved_at        TEXT,
	PRIMARY KEY (session_id, episode)
);
CREATE UNIQUE INDEX IF NOT EXISTS codex_creation_uncertainties_open
	ON codex_creation_uncertainties(session_id) WHERE disposition IS NULL;
`

func applyMigrationV6(tx *sql.Tx) error {
	if _, err := tx.Exec(codexUncertaintyV6DDL); err != nil {
		return fmt.Errorf("execute schema v6: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (6, 'codex_creation_uncertainty_episodes', ?, ?);`, "codex-uncertainty-v6", now); err != nil {
		return fmt.Errorf("record migration v6: %w", err)
	}
	return nil
}

// agyStateV7DDL creates the AC-010 Agy adapter durable state tables
// (session bindings, turn attempts with tool-verification columns,
// launch reservations with exe identity, cprot-v2 protection
// attestations, and creation-uncertainty episodes — mirroring the
// codex_* v5/v6 tables) and adds the required_tools_json column to the
// queued-prompt table (pending_prompts) and to dispatch_intents, so the
// dispatch-time tool allow-list survives the pending-prompt → turn
// promotion done by ReleaseTurn.
//
// Unlike the Codex/Claude adapters, Agy attempts carry NO redispatch
// branch: launch_count is capped at 1 (0→1 only) and ReserveAgyLaunch
// has no protected-absence second-launch path. The native_id CHECK
// enforces a UUIDv4 shape (the version nibble fixed at 4); the
// protection-attestation table gates production eligibility (a covering
// row is required before the adapter is constructed), not a redispatch,
// which does not exist.
const agyStateV7DDL = `
ALTER TABLE pending_prompts ADD COLUMN required_tools_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE dispatch_intents ADD COLUMN required_tools_json TEXT NOT NULL DEFAULT '[]';

CREATE TABLE IF NOT EXISTS agy_session_bindings (
	session_id         TEXT PRIMARY KEY,
	native_id          TEXT NOT NULL UNIQUE
	                   CHECK (native_id GLOB '[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-4[0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f]-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]'),
	materialized       INTEGER NOT NULL DEFAULT 0,
	model              TEXT NOT NULL,
	workspace          TEXT NOT NULL,
	profile_digest     TEXT NOT NULL,
	conversation_path  TEXT,
	file_identity      TEXT,
	first_prompt_digest TEXT,
	created_at         TIMESTAMP NOT NULL
);
CREATE TABLE IF NOT EXISTS agy_turn_attempts (
	attempt_id         TEXT NOT NULL,
	session_id         TEXT NOT NULL,
	turn_key           TEXT NOT NULL,
	transition_version INTEGER NOT NULL DEFAULT 1,
	native_step_index  INTEGER,
	prompt_digest      TEXT NOT NULL,
	launch_count       INTEGER NOT NULL DEFAULT 0 CHECK (launch_count BETWEEN 0 AND 1),
	accepted           INTEGER,
	terminal           INTEGER NOT NULL DEFAULT 0,
	result_payload     TEXT,
	result_usage       TEXT,
	required_tools_json TEXT NOT NULL DEFAULT '[]',
	executed_tools_json TEXT NOT NULL DEFAULT '[]',
	missing_required_tools_json TEXT NOT NULL DEFAULT '[]',
	denied_tools_json  TEXT NOT NULL DEFAULT '[]',
	ambiguous_denials_json TEXT NOT NULL DEFAULT '[]',
	unattributed_denials_json TEXT NOT NULL DEFAULT '[]',
	unmapped_denials_json TEXT NOT NULL DEFAULT '[]',
	verification_incomplete INTEGER NOT NULL DEFAULT 0,
	observed_status    TEXT NOT NULL DEFAULT 'uncertain'
	                   CHECK (observed_status IN ('completed','failed','cancelled','missing','uncertain')),
	uncertainty_disposition TEXT,
	orphan_conversation_id TEXT,
	created_at         TIMESTAMP NOT NULL,
	updated_at         TIMESTAMP NOT NULL,
	UNIQUE(session_id, turn_key, attempt_id)
);
CREATE TABLE IF NOT EXISTS agy_attempt_launches (
	attempt_id         TEXT NOT NULL,
	reservation_seq    INTEGER NOT NULL,
	state              TEXT NOT NULL DEFAULT 'reserved'
	                   CHECK (state IN ('reserved','started','start_failed','dead')),
	started_at         TIMESTAMP,
	first_stdin_byte_at TIMESTAMP,
	known_dead_at      TIMESTAMP,
	exit_code          INTEGER,
	executor_identity  TEXT NOT NULL,
	exe_dev_ino        TEXT,
	exe_digest         TEXT,
	UNIQUE(attempt_id, reservation_seq)
);
CREATE TABLE IF NOT EXISTS agy_protection_attestations (
	attestation_id     TEXT PRIMARY KEY
	                   CHECK (length(attestation_id) = 80
	                          AND attestation_id LIKE 'cprot-v2:sha256:%'
	                          AND substr(attestation_id, 17) NOT GLOB '*[^0-9a-f]*'),
	agy_version        TEXT NOT NULL,
	platform           TEXT NOT NULL,
	manifest_digest    TEXT NOT NULL,
	profile_digest     TEXT NOT NULL,
	probe_results      TEXT NOT NULL,
	probed_at          TIMESTAMP NOT NULL,
	actor              TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS agy_creation_uncertainties (
	session_id         TEXT NOT NULL,
	episode            INTEGER NOT NULL CHECK (episode >= 1),
	run_id             TEXT NOT NULL,
	reason             TEXT NOT NULL,
	recorded_by        TEXT NOT NULL,
	record_op_id       TEXT NOT NULL,
	cause_op_id        TEXT NOT NULL,
	orphan_native_id   TEXT,
	recorded_at        TEXT NOT NULL,
	disposition        TEXT,
	resolution_reason  TEXT,
	resolution_generation INTEGER,
	resolution_op_id   TEXT,
	resolved_at        TEXT,
	PRIMARY KEY (session_id, episode)
);
CREATE UNIQUE INDEX IF NOT EXISTS agy_creation_uncertainties_open
	ON agy_creation_uncertainties(session_id) WHERE disposition IS NULL;
`

func applyMigrationV7(tx *sql.Tx) error {
	if _, err := tx.Exec(agyStateV7DDL); err != nil {
		return fmt.Errorf("execute schema v7: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`
INSERT INTO schema_migrations (version, name, checksum, applied_at)
VALUES (7, 'agy_adapter_state', ?, ?);`, "agy-state-v7", now); err != nil {
		return fmt.Errorf("record migration v7: %w", err)
	}
	return nil
}
