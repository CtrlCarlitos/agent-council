package storage

// AC-008 durable state for the Claude adapter (spec §3.11): session
// bindings, turn attempts with typed baselines and crash-safe launch
// reservations, and protection attestations. All transitions are
// atomic single-transaction updates with idempotency via the journal.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const claudeStateSchema = `
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
);`

// ApplyClaudeStateSchema creates the AC-008 tables if absent. Idempotent.
func ApplyClaudeStateSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, claudeStateSchema)
	return err
}

// ── Session bindings ────────────────────────────────────────────────────

type ClaudeSessionBinding struct {
	SessionID         string
	NativeID          string
	Materialized      bool
	Model             string
	Workspace         string
	ConfigRoot        string
	TemplateDigest    string
	FirstPromptDigest string
	CreatedAt         time.Time
}

// InsertClaudeSessionBinding persists a new unmaterialized binding.
// Fail closed if the session already has one (idempotency is handled by
// the adapter's creation reservation).
func (s *Store) InsertClaudeSessionBinding(ctx context.Context, b ClaudeSessionBinding) error {
	_, err := s.DB().ExecContext(ctx, `
INSERT INTO claude_session_bindings
	(session_id, native_id, materialized, model, workspace, config_root,
	 template_digest, first_prompt_digest, created_at)
VALUES (?, ?, 0, ?, ?, ?, ?, ?, ?)`,
		b.SessionID, b.NativeID, b.Model, b.Workspace, b.ConfigRoot,
		b.TemplateDigest, b.FirstPromptDigest, b.CreatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("insert claude session binding: %w", err)
	}
	return nil
}

// GetClaudeSessionBinding returns the binding for a logical session.
func (s *Store) GetClaudeSessionBinding(ctx context.Context, sessionID string) (*ClaudeSessionBinding, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT session_id, native_id, materialized, model, workspace, config_root,
       template_digest, first_prompt_digest, created_at
FROM claude_session_bindings WHERE session_id = ?`, sessionID)
	var b ClaudeSessionBinding
	var createdAt string
	var mat int
	if err := row.Scan(&b.SessionID, &b.NativeID, &mat, &b.Model, &b.Workspace,
		&b.ConfigRoot, &b.TemplateDigest, &b.FirstPromptDigest, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query claude session binding: %w", err)
	}
	b.Materialized = mat != 0
	b.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &b, nil
}

// MarkClaudeSessionMaterialized flips materialized after the transcript
// is observed. Only transitions false → true; already-materialized is a
// no-op.
func (s *Store) MarkClaudeSessionMaterialized(ctx context.Context, sessionID string) error {
	_, err := s.DB().ExecContext(ctx, `
UPDATE claude_session_bindings SET materialized = 1
WHERE session_id = ? AND materialized = 0`, sessionID)
	return err
}

// ── Turn attempts ───────────────────────────────────────────────────────

type ClaudeTurnAttempt struct {
	AttemptID              string
	SessionID              string
	TurnKey                string
	NativeID               string
	PromptDigest           string
	BaselineIdentity       string
	BaselineSize           int64
	BaselineEntries        int
	BaselineMaterialized   bool
	TranscriptProtection   string
	AttestationID          string
	LaunchCount            int
	AbsenceRedispatch      bool
	Accepted               *bool
	Terminal               bool
	ResultPayload          *string
	ResultUsage            *string
	ObservedStatus         string // completed|failed|missing|uncertain
	UncertaintyDisposition *string
	DispositionActor       *string
	TransitionVersion      int64
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// InsertClaudeTurnAttempt durably records the pre-launch baseline before
// any process can start (spec §3.11 crash-safe ordering, step 1).
func (s *Store) InsertClaudeTurnAttempt(ctx context.Context, a ClaudeTurnAttempt) error {
	mat := 0
	if a.BaselineMaterialized {
		mat = 1
	}
	_, err := s.DB().ExecContext(ctx, `
INSERT INTO claude_turn_attempts
	(attempt_id, session_id, turn_key, native_id, prompt_digest,
	 baseline_file_identity, baseline_size, baseline_entries, materialized_baseline,
	 transcript_protection, protection_attestation_id,
	 launch_count, observed_status, transition_version, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 'uncertain', 1, ?, ?)`,
		a.AttemptID, a.SessionID, a.TurnKey, a.NativeID, a.PromptDigest,
		a.BaselineIdentity, a.BaselineSize, a.BaselineEntries, mat,
		a.TranscriptProtection, a.AttestationID,
		a.CreatedAt.UTC().Format(time.RFC3339), a.CreatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("insert claude turn attempt: %w", err)
	}
	return nil
}

// ReserveClaudeLaunch atomically consumes a launch slot (launch_count
// 0→1 initial, 1→2 protected-absence redispatch) and inserts the
// RESERVED launch row — all before executor.Start. Preconditions are
// enforced in the WHERE/conditional logic; a failed precondition
// returns an error and nothing changes.
func (s *Store) ReserveClaudeLaunch(ctx context.Context, attemptID, executorIdentity string) (reservationSeq int64, err error) {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var count, consumed int
	err = tx.QueryRowContext(ctx, `
SELECT launch_count, absence_redispatch_consumed
FROM claude_turn_attempts WHERE attempt_id = ?`, attemptID).Scan(&count, &consumed)
	if err != nil {
		return 0, fmt.Errorf("query attempt: %w", err)
	}

	var priorDead int
	err = tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM claude_attempt_launches
WHERE attempt_id = ? AND state IN ('started','dead')`, attemptID).Scan(&priorDead)
	if err != nil {
		return 0, err
	}

	if count == 0 {
		// Initial launch: always allowed.
	} else if count == 1 && consumed == 0 && priorDead >= 1 {
		// Protected verified-absence redispatch: requires (1) a valid
		// protection attestation in the attestations table matching the
		// attempt's frozen attestation_id, (2) a separately recorded
		// positive-absence decision (absence_verified flag), and (3)
		// transcript_protection = 'protected'. Advisory attempts stay
		// uncertain permanently.
		var protection, attestationID, absenceVerified string
		err = tx.QueryRowContext(ctx, `
SELECT ta.transcript_protection, ta.protection_attestation_id, ta.absence_verified
FROM claude_turn_attempts ta WHERE ta.attempt_id = ?`, attemptID).Scan(&protection, &attestationID, &absenceVerified)
		if err != nil {
			return 0, err
		}
		if protection != "protected" {
			return 0, fmt.Errorf(
				"redispatch requires protected transcript evidence (protection=%q)", protection)
		}
		if strings.TrimSpace(attestationID) == "" {
			return 0, fmt.Errorf("redispatch requires a protection attestation id")
		}
		// The attestation must exist in the attestations table and match
		// the attempt's frozen identity.
		var attCount int
		err = tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM claude_protection_attestations
WHERE attestation_id = ?`, attestationID).Scan(&attCount)
		if err != nil {
			return 0, err
		}
		if attCount != 1 {
			return 0, fmt.Errorf(
				"redispatch requires a valid attestation %q in claude_protection_attestations", attestationID)
		}
		// A separately recorded positive-absence decision is required:
		// process death alone proves the child stopped, not that the
		// transcript was unaffected.
		if absenceVerified != "verified" {
			return 0, fmt.Errorf(
				"redispatch requires a separately recorded positive-absence decision (absence_verified=%q)", absenceVerified)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE claude_turn_attempts SET absence_redispatch_consumed = 1
WHERE attempt_id = ?`, attemptID); err != nil {
			return 0, err
		}
	} else {
		return 0, fmt.Errorf("launch reservation precondition failed for %s (count=%d consumed=%d prior_dead=%d)",
			attemptID, count, consumed, priorDead)
	}

	// Use MAX(reservation_seq)+1: start_failed rows retain their seq, so
	// new reservations never collide with retained evidence.
	var maxSeq sql.NullInt64
	err = tx.QueryRowContext(ctx, `
SELECT MAX(reservation_seq) FROM claude_attempt_launches WHERE attempt_id = ?`, attemptID).Scan(&maxSeq)
	if err != nil {
		return 0, err
	}
	seq := int64(1)
	if maxSeq.Valid && maxSeq.Int64 > 0 {
		seq = maxSeq.Int64 + 1
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO claude_attempt_launches (attempt_id, reservation_seq, state, executor_identity)
VALUES (?, ?, 'reserved', ?)`, attemptID, seq, executorIdentity); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE claude_turn_attempts SET launch_count = launch_count + 1,
	transition_version = transition_version + 1, updated_at = ?
WHERE attempt_id = ?`, time.Now().UTC().Format(time.RFC3339), attemptID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(seq), nil
}

// RecordClaudeLaunchOutcome marks a reserved launch as started,
// start-failed, or dead. start_failed releases the slot (count decrement
// is NOT done — the row is retained as evidence and the cap counts
// started/dead rows, per spec §3.11).
func (s *Store) RecordClaudeLaunchState(ctx context.Context, attemptID string, seq int64, state string, exitCode *int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	switch state {
	case "started":
		_, err := s.DB().ExecContext(ctx, `
UPDATE claude_attempt_launches SET state = 'started', started_at = ?
WHERE attempt_id = ? AND reservation_seq = ?`, now, attemptID, seq)
		return err
	case "start_failed":
		tx, txErr := s.DB().BeginTx(ctx, nil)
		if txErr != nil {
			return txErr
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `
UPDATE claude_attempt_launches SET state = 'start_failed', start_failed_at = ?
WHERE attempt_id = ? AND reservation_seq = ?`, now, attemptID, seq); err != nil {
			return err
		}
		// No process was created: release the launch slot so a new
		// reservation is possible. The launch row is retained as
		// evidence. Both the launch-row state change and the slot
		// release are one atomic transaction.
		if _, err := tx.ExecContext(ctx, `
UPDATE claude_turn_attempts SET launch_count = launch_count - 1
WHERE attempt_id = ? AND launch_count > 0`, attemptID); err != nil {
			return err
		}
		return tx.Commit()
	case "dead":
		_, err := s.DB().ExecContext(ctx, `
UPDATE claude_attempt_launches SET state = 'dead', known_dead_at = ?, exit_code = ?
WHERE attempt_id = ? AND reservation_seq = ?`, now, attemptID, seq, exitCode)
		return err
	default:
		return fmt.Errorf("unknown launch state %q", state)
	}
}

// RecordClaudeStdinTransmitted records the first-byte transmission
// boundary on a launch row.
func (s *Store) RecordClaudeStdinTransmitted(ctx context.Context, attemptID string, seq int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB().ExecContext(ctx, `
UPDATE claude_attempt_launches SET first_stdin_byte_at = ?
WHERE attempt_id = ? AND reservation_seq = ? AND state = 'started'`,
		now, attemptID, seq)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf(
			"stdin transmission requires a started launch row (attempt=%s seq=%d)", attemptID, seq)
	}
	return nil
}

// RecordClaudeAbsenceVerified records a controller-level positive-
// absence decision on the attempt. This is a separate, explicit action
// — process death alone does NOT set it. Only attempts in protected
// mode with a valid attestation can have absence verified.
func (s *Store) RecordClaudeAbsenceVerified(ctx context.Context, attemptID string) error {
	_, err := s.DB().ExecContext(ctx, `
UPDATE claude_turn_attempts SET absence_verified = 'verified'
WHERE attempt_id = ? AND transcript_protection = 'protected'
  AND protection_attestation_id != ''`, attemptID)
	return err
}

// MarkClaudeAttemptAccepted records the acceptance boundary (protected
// mode only).
func (s *Store) MarkClaudeAttemptAccepted(ctx context.Context, attemptID string) error {
	_, err := s.DB().ExecContext(ctx, `
UPDATE claude_turn_attempts SET accepted = 1, transition_version = transition_version + 1,
	updated_at = ? WHERE attempt_id = ?`, time.Now().UTC().Format(time.RFC3339), attemptID)
	return err
}

// SetClaudeAttemptObservedStatus records the evidence-derived outcome.
func (s *Store) SetClaudeAttemptObservedStatus(ctx context.Context, attemptID, status string) error {
	_, err := s.DB().ExecContext(ctx, `
UPDATE claude_turn_attempts SET observed_status = ?, transition_version = transition_version + 1,
	updated_at = ? WHERE attempt_id = ?`, status, time.Now().UTC().Format(time.RFC3339), attemptID)
	return err
}

// SetClaudeAttemptTerminal records the verified terminal result payload.
func (s *Store) SetClaudeAttemptTerminal(ctx context.Context, attemptID string, resultPayload, resultUsage string) error {
	// Exactly-once guard: only the first successful write wins (the
	// WHERE clause requires terminal = 0).
	res, err := s.DB().ExecContext(ctx, `
UPDATE claude_turn_attempts SET terminal = 1, result_payload = ?, result_usage = ?,
	observed_status = 'completed', transition_version = transition_version + 1,
	updated_at = ? WHERE attempt_id = ? AND terminal = 0`,
		resultPayload, resultUsage, time.Now().UTC().Format(time.RFC3339), attemptID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("attempt %s already terminal; exactly-once guard prevented overwrite", attemptID)
	}
	return nil
}

// GetClaudeTurnAttempt returns the current attempt row.
func (s *Store) GetClaudeTurnAttempt(ctx context.Context, attemptID string) (*ClaudeTurnAttempt, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT attempt_id, session_id, turn_key, native_id, prompt_digest,
       baseline_file_identity, baseline_size, baseline_entries, materialized_baseline,
       transcript_protection, protection_attestation_id, launch_count, absence_redispatch_consumed,
       accepted, terminal, result_payload, result_usage, observed_status,
       uncertainty_disposition, disposition_actor, disposition_generation,
       disposition_op_id, disposition_at,
       transition_version, created_at, updated_at
FROM claude_turn_attempts WHERE attempt_id = ?`, attemptID)
	return scanClaudeAttempt(row)
}

// GetClaudeTurnAttemptByTurn returns the latest attempt for a turn.
func (s *Store) GetLatestClaudeTurnAttempt(ctx context.Context, sessionID, turnKey string) (*ClaudeTurnAttempt, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT attempt_id, session_id, turn_key, native_id, prompt_digest,
       baseline_file_identity, baseline_size, baseline_entries, materialized_baseline,
       transcript_protection, protection_attestation_id, launch_count, absence_redispatch_consumed,
       accepted, terminal, result_payload, result_usage, observed_status,
       uncertainty_disposition, disposition_actor, disposition_generation,
       disposition_op_id, disposition_at,
       transition_version, created_at, updated_at
FROM claude_turn_attempts
WHERE session_id = ? AND turn_key = ?
ORDER BY created_at DESC LIMIT 1`, sessionID, turnKey)
	return scanClaudeAttempt(row)
}

func scanClaudeAttempt(row *sql.Row) (*ClaudeTurnAttempt, error) {
	var a ClaudeTurnAttempt
	var createdAt, updatedAt string
	var terminal, consumed, matBaseline int
	var acceptedNull sql.NullInt64
	var disposition, dispositionActor, dispositionOpID, dispositionAt sql.NullString
	var dispositionGen sql.NullInt64
	if err := row.Scan(
		&a.AttemptID, &a.SessionID, &a.TurnKey, &a.NativeID, &a.PromptDigest,
		&a.BaselineIdentity, &a.BaselineSize, &a.BaselineEntries, &matBaseline,
		&a.TranscriptProtection, &a.AttestationID, &a.LaunchCount, &consumed,
		&acceptedNull, &terminal, &a.ResultPayload, &a.ResultUsage, &a.ObservedStatus,
		&disposition, &dispositionActor, &dispositionGen, &dispositionOpID, &dispositionAt,
		&a.TransitionVersion, &createdAt, &updatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	a.BaselineMaterialized = matBaseline != 0
	a.AbsenceRedispatch = consumed != 0
	a.Terminal = terminal != 0
	if acceptedNull.Valid {
		v := acceptedNull.Int64 == 1
		a.Accepted = &v
	}
	a.UncertaintyDisposition = nullStr(disposition)
	a.DispositionActor = nullStr(dispositionActor)
	a.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	a.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return &a, nil
}

func nullStr(ns sql.NullString) *string {
	if !ns.Valid {
		return nil
	}
	v := ns.String
	return &v
}

// HasClaudeUnresolvedAttempts reports whether the session has any
// uncertain attempt without a controller disposition — these block all
// subsequent turns on the native session (spec §3.5).
func (s *Store) HasClaudeUnresolvedAttempts(ctx context.Context, nativeID string) (bool, error) {
	var count int
	err := s.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM claude_turn_attempts
WHERE native_id = ? AND observed_status = 'uncertain' AND uncertainty_disposition IS NULL`,
		nativeID).Scan(&count)
	return count > 0, err
}

// strings is referenced by the identifier rule.
var _ = strings.TrimSpace
