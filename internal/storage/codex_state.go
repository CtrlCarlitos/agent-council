package storage

// AC-009 durable state for the Codex adapter (spec §3.11): session
// bindings, turn attempts with typed baselines and crash-safe launch
// reservations, and cprot-v2 protection attestations. All transitions
// mirror the AC-008 Claude adapter: atomic single-transaction updates
// with idempotency via the journal.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── Session bindings ────────────────────────────────────────────────────

type CodexSessionBinding struct {
	SessionID         string
	NativeID          string
	Materialized      bool
	Model             string
	Workspace         string
	ProfileDigest     string
	RolloutPath       *string
	FirstPromptDigest *string
	CreatedAt         time.Time
}

// InsertCodexSessionBinding persists a new unmaterialized binding. The
// native_id is a server-generated UUIDv7 enforced by the schema CHECK;
// fail closed if the session or native identity is already bound.
func (s *Store) InsertCodexSessionBinding(ctx context.Context, b CodexSessionBinding) error {
	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return fmt.Errorf("begin binding insert: %w", err)
	}
	defer tx.Rollback()
	if err := insertCodexSessionBindingTx(ctx, tx.Tx(), b); err != nil {
		return err
	}
	return tx.Commit()
}

func insertCodexSessionBindingTx(ctx context.Context, tx *sql.Tx, b CodexSessionBinding) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO codex_session_bindings
	(session_id, native_id, materialized, model, workspace, profile_digest, created_at)
VALUES (?, ?, 0, ?, ?, ?, ?)`,
		b.SessionID, b.NativeID, b.Model, b.Workspace, b.ProfileDigest, b.CreatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("insert codex session binding: %w", err)
	}
	return nil
}

// GetCodexSessionBinding returns the binding for a logical session.
func (s *Store) GetCodexSessionBinding(ctx context.Context, sessionID string) (*CodexSessionBinding, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT session_id, native_id, materialized, model, workspace, rollout_path,
       profile_digest, first_prompt_digest, created_at
FROM codex_session_bindings WHERE session_id = ?`, sessionID)
	var b CodexSessionBinding
	var createdAt string
	var mat int
	var rolloutPath, firstPrompt sql.NullString
	if err := row.Scan(&b.SessionID, &b.NativeID, &mat, &b.Model, &b.Workspace,
		&rolloutPath, &b.ProfileDigest, &firstPrompt, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query codex session binding: %w", err)
	}
	b.Materialized = mat != 0
	b.RolloutPath = nullStr(rolloutPath)
	b.FirstPromptDigest = nullStr(firstPrompt)
	b.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &b, nil
}

// MarkCodexSessionMaterialized flips materialized after the rollout file
// is observed and its resolved path recorded. Only transitions
// false → true; already-materialized is a no-op.
func (s *Store) MarkCodexSessionMaterialized(ctx context.Context, sessionID, rolloutPath string) error {
	_, err := s.DB().ExecContext(ctx, `
UPDATE codex_session_bindings SET materialized = 1, rollout_path = ?
WHERE session_id = ? AND materialized = 0`, rolloutPath, sessionID)
	return err
}

// ── Turn attempts ───────────────────────────────────────────────────────

type CodexTurnAttempt struct {
	AttemptID              string
	SessionID              string
	TurnKey                string
	NativeTurnID           *string
	PromptDigest           string
	BaselineIdentity       string
	BaselineSize           int64
	BaselineEntries        int
	BaselineMaterialized   bool
	RolloutProtection      string // protected|advisory|unverified
	AttestationID          *string
	LaunchCount            int
	AbsenceRedispatch      bool
	AbsenceVerified        string
	Accepted               *bool
	Terminal               bool
	ResultPayload          *string
	ResultUsage            *string
	ObservedStatus         string // completed|failed|missing|uncertain
	UncertaintyDisposition *string
	TransitionVersion      int64
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// InsertCodexTurnAttempt durably records the pre-launch baseline before
// any process can start (spec §3.11 crash-safe ordering, step 1). The
// native turn id is bound in-life from turn/started and starts NULL.
func (s *Store) InsertCodexTurnAttempt(ctx context.Context, a CodexTurnAttempt) error {
	mat := 0
	if a.BaselineMaterialized {
		mat = 1
	}
	_, err := s.DB().ExecContext(ctx, `
INSERT INTO codex_turn_attempts
	(attempt_id, session_id, turn_key, prompt_digest,
	 baseline_file_identity, baseline_size, baseline_entries, materialized_baseline,
	 rollout_protection, protection_attestation_id,
	 launch_count, observed_status, transition_version, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 'uncertain', 1, ?, ?)`,
		a.AttemptID, a.SessionID, a.TurnKey, a.PromptDigest,
		a.BaselineIdentity, a.BaselineSize, a.BaselineEntries, mat,
		a.RolloutProtection, a.AttestationID,
		a.CreatedAt.UTC().Format(time.RFC3339), a.CreatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("insert codex turn attempt: %w", err)
	}
	return nil
}

// ReserveCodexLaunch atomically consumes a launch slot (launch_count
// 0→1 initial, 1→2 protected-absence redispatch) and inserts the
// RESERVED launch row — all before executor.Start. Preconditions are
// enforced in the WHERE/conditional logic; a failed precondition
// returns an error and nothing changes.
func (s *Store) ReserveCodexLaunch(ctx context.Context, attemptID, executorIdentity string, childGeneration int64) (reservationSeq int64, err error) {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var count, consumed int
	err = tx.QueryRowContext(ctx, `
SELECT launch_count, absence_redispatch_consumed
FROM codex_turn_attempts WHERE attempt_id = ?`, attemptID).Scan(&count, &consumed)
	if err != nil {
		return 0, fmt.Errorf("query attempt: %w", err)
	}

	var priorDead int
	err = tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM codex_attempt_launches
WHERE attempt_id = ? AND state IN ('started','dead')`, attemptID).Scan(&priorDead)
	if err != nil {
		return 0, err
	}

	if count == 0 {
		// Initial launch: always allowed.
	} else if count == 1 && consumed == 0 && priorDead >= 1 {
		// Protected verified-absence redispatch: requires (1) a valid
		// cprot-v2 attestation in the attestations table matching the
		// attempt's frozen attestation_id, (2) a separately recorded
		// positive-absence decision (absence_verified flag), and (3)
		// rollout_protection = 'protected'. Advisory attempts stay
		// uncertain permanently.
		var protection, absenceVerified string
		var attestationID sql.NullString
		err = tx.QueryRowContext(ctx, `
SELECT ta.rollout_protection, ta.protection_attestation_id, ta.absence_verified
FROM codex_turn_attempts ta WHERE ta.attempt_id = ?`, attemptID).Scan(&protection, &attestationID, &absenceVerified)
		if err != nil {
			return 0, err
		}
		if protection != "protected" {
			return 0, fmt.Errorf(
				"redispatch requires protected rollout evidence (protection=%q)", protection)
		}
		if !attestationID.Valid || strings.TrimSpace(attestationID.String) == "" {
			return 0, fmt.Errorf("redispatch requires a protection attestation id")
		}
		// The attestation must exist in the attestations table and match
		// the attempt's frozen identity.
		var attCount int
		err = tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM codex_protection_attestations
WHERE attestation_id = ?`, attestationID.String).Scan(&attCount)
		if err != nil {
			return 0, err
		}
		if attCount != 1 {
			return 0, fmt.Errorf(
				"redispatch requires a valid attestation %q in codex_protection_attestations", attestationID.String)
		}
		// A separately recorded positive-absence decision is required:
		// process death alone proves the child stopped, not that the
		// rollout was unaffected.
		if absenceVerified != "verified" {
			return 0, fmt.Errorf(
				"redispatch requires a separately recorded positive-absence decision (absence_verified=%q)", absenceVerified)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE codex_turn_attempts SET absence_redispatch_consumed = 1
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
SELECT MAX(reservation_seq) FROM codex_attempt_launches WHERE attempt_id = ?`, attemptID).Scan(&maxSeq)
	if err != nil {
		return 0, err
	}
	seq := int64(1)
	if maxSeq.Valid && maxSeq.Int64 > 0 {
		seq = maxSeq.Int64 + 1
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO codex_attempt_launches (attempt_id, reservation_seq, state, child_generation, executor_identity)
VALUES (?, ?, 'reserved', ?, ?)`, attemptID, seq, childGeneration, executorIdentity); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE codex_turn_attempts SET launch_count = launch_count + 1,
	transition_version = transition_version + 1, updated_at = ?
WHERE attempt_id = ?`, time.Now().UTC().Format(time.RFC3339), attemptID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(seq), nil
}

// RecordCodexLaunchState marks a reserved launch as started,
// start-failed, or dead. start_failed releases the slot (count decrement
// is NOT done — the row is retained as evidence and the cap counts
// started/dead rows, per spec §3.11).
func (s *Store) RecordCodexLaunchState(ctx context.Context, attemptID string, seq int64, state string, exitCode *int) error {
	now := time.Now().UTC().Format(time.RFC3339)
	switch state {
	case "started":
		_, err := s.DB().ExecContext(ctx, `
UPDATE codex_attempt_launches SET state = 'started', started_at = ?
WHERE attempt_id = ? AND reservation_seq = ?`, now, attemptID, seq)
		return err
	case "start_failed":
		tx, txErr := s.DB().BeginTx(ctx, nil)
		if txErr != nil {
			return txErr
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `
UPDATE codex_attempt_launches SET state = 'start_failed', start_failed_at = ?
WHERE attempt_id = ? AND reservation_seq = ?`, now, attemptID, seq); err != nil {
			return err
		}
		// No process was created: release the launch slot so a new
		// reservation is possible, and classify the attempt as
		// definitively missing — positive pre-start evidence, NOT an
		// unresolved uncertainty. The attempt never blocks the native
		// session (only 'uncertain' rows do), and the launch row is
		// retained as evidence. All three writes are one atomic
		// transaction; the status guard never downgrades a resolved
		// attempt.
		if _, err := tx.ExecContext(ctx, `
UPDATE codex_turn_attempts SET launch_count = launch_count - 1,
	observed_status = 'missing'
WHERE attempt_id = ? AND launch_count > 0 AND observed_status = 'uncertain'`, attemptID); err != nil {
			return err
		}
		return tx.Commit()
	case "dead":
		_, err := s.DB().ExecContext(ctx, `
UPDATE codex_attempt_launches SET state = 'dead', known_dead_at = ?, exit_code = ?
WHERE attempt_id = ? AND reservation_seq = ?`, now, exitCode, attemptID, seq)
		return err
	default:
		return fmt.Errorf("unknown launch state %q", state)
	}
}

// RecordCodexStdinTransmitted records the first-byte transmission
// boundary on a launch row.
func (s *Store) RecordCodexStdinTransmitted(ctx context.Context, attemptID string, seq int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	// First-write-wins: the WHERE clause requires the boundary to be
	// unset AND the launch to be in started state. A duplicate call is
	// idempotent — the original ambiguity boundary is never overwritten.
	res, err := s.DB().ExecContext(ctx, `
UPDATE codex_attempt_launches SET first_stdin_byte_at = ?
WHERE attempt_id = ? AND reservation_seq = ? AND state = 'started'
  AND first_stdin_byte_at IS NULL`,
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
			"stdin transmission requires a started launch row with no recorded boundary (attempt=%s seq=%d)",
			attemptID, seq)
	}
	return nil
}

// RecordCodexAbsenceVerified records the service-derived positive-
// absence decision on the attempt. It is NOT a controller decision —
// verified absence is protected evidence derived from the attempt's own
// frozen attestation. Fail closed: the attempt must be in protected
// mode, carry a valid attestation in the attestations table, and not
// already have absence verified. RowsAffected is checked: a no-op
// update returns an error rather than silently succeeding.
func (s *Store) RecordCodexAbsenceVerified(ctx context.Context, attemptID string) error {
	res, err := s.DB().ExecContext(ctx, `
UPDATE codex_turn_attempts SET absence_verified = 'verified',
	transition_version = transition_version + 1, updated_at = ?
WHERE attempt_id = ?
  AND rollout_protection = 'protected'
  AND protection_attestation_id IS NOT NULL
  AND protection_attestation_id != ''
  AND absence_verified = ''
  AND EXISTS (
      SELECT 1 FROM codex_protection_attestations
      WHERE attestation_id = codex_turn_attempts.protection_attestation_id
  )`, time.Now().UTC().Format(time.RFC3339), attemptID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf(
			"absence verification requires protected mode with a valid attestation (attempt %s)", attemptID)
	}
	return nil
}

// RecordCodexNativeTurnID binds the in-life native turn id on the
// attempt (spec §3.5: the turn/started notification carries the
// server-generated UUIDv7; the adapter maps it durably). The binding is
// once-only: a conflicting rebinding fails closed.
func (s *Store) RecordCodexNativeTurnID(ctx context.Context, attemptID, nativeTurnID string) error {
	res, err := s.DB().ExecContext(ctx, `
UPDATE codex_turn_attempts SET native_turn_id = ?, transition_version = transition_version + 1,
	updated_at = ? WHERE attempt_id = ? AND native_turn_id IS NULL`,
		nativeTurnID, time.Now().UTC().Format(time.RFC3339), attemptID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var existing sql.NullString
		if err := s.DB().QueryRowContext(ctx,
			`SELECT native_turn_id FROM codex_turn_attempts WHERE attempt_id = ?`, attemptID).Scan(&existing); err != nil {
			return err
		}
		if existing.Valid && existing.String == nativeTurnID {
			return nil // idempotent rebinding of the same id
		}
		return fmt.Errorf("attempt %s already carries native turn id %v; refusing %q",
			attemptID, existing.String, nativeTurnID)
	}
	return nil
}

// RecordCodexFirstAcceptance records the first-acceptance rollout
// baseline in ONE atomic transaction (spec §3.11): the attempt's
// baseline (file-identity, byte size, entry count) is written with
// materialized_baseline, and the session binding flips to materialized
// with the resolved rollout path and the attempt's prompt digest as
// first_prompt_digest. Conflicting rebinding (a different rollout path
// for an already-materialized binding) fails closed.
func (s *Store) RecordCodexFirstAcceptance(ctx context.Context, sessionID, attemptID, rolloutPath, identity string, size int64, entries int) error {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
UPDATE codex_turn_attempts SET baseline_file_identity = ?, baseline_size = ?, baseline_entries = ?,
	materialized_baseline = 1, transition_version = transition_version + 1, updated_at = ?
WHERE attempt_id = ? AND materialized_baseline = 0`, identity, size, entries, now, attemptID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		var mat int
		if err := tx.QueryRowContext(ctx,
			`SELECT materialized_baseline FROM codex_turn_attempts WHERE attempt_id = ?`, attemptID).Scan(&mat); err != nil {
			return fmt.Errorf("first acceptance for unknown attempt %s: %w", attemptID, err)
		}
		if mat == 0 {
			return fmt.Errorf("attempt %s could not accept the baseline", attemptID)
		}
		return fmt.Errorf("attempt %s already carries a materialized baseline", attemptID)
	}

	var mat int
	var existingPath sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT materialized, rollout_path FROM codex_session_bindings WHERE session_id = ?`,
		sessionID).Scan(&mat, &existingPath); err != nil {
		return err
	}
	if mat == 1 {
		if !existingPath.Valid || existingPath.String != rolloutPath {
			return fmt.Errorf("binding %s is already materialized at %v; refusing rollout path %q",
				sessionID, existingPath.String, rolloutPath)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
UPDATE codex_session_bindings SET materialized = 1, rollout_path = ?,
	first_prompt_digest = (SELECT prompt_digest FROM codex_turn_attempts WHERE attempt_id = ?)
WHERE session_id = ? AND materialized = 0`, rolloutPath, attemptID, sessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetCodexAttemptObservedStatus records the evidence-derived outcome.
func (s *Store) SetCodexAttemptObservedStatus(ctx context.Context, attemptID, status string) error {
	_, err := s.DB().ExecContext(ctx, `
UPDATE codex_turn_attempts SET observed_status = ?, transition_version = transition_version + 1,
	updated_at = ? WHERE attempt_id = ?`, status, time.Now().UTC().Format(time.RFC3339), attemptID)
	return err
}

// SetCodexAttemptTerminal records the verified terminal result payload
// with its evidence-derived observed status: 'completed' for a verified
// success result, 'failed' for a verified error result, 'interrupted'
// for a verified interrupted terminal (§3.9 — the accepted interrupt
// confirmed by the interrupted turn status). All are terminal; only the
// status differs.
func (s *Store) SetCodexAttemptTerminal(ctx context.Context, attemptID, observedStatus, resultPayload, resultUsage string) error {
	if observedStatus != "completed" && observedStatus != "failed" && observedStatus != "interrupted" {
		return fmt.Errorf("terminal observed status must be completed, failed, or interrupted, got %q", observedStatus)
	}
	// Exactly-once guard: only the first successful write wins (the
	// WHERE clause requires terminal = 0).
	res, err := s.DB().ExecContext(ctx, `
UPDATE codex_turn_attempts SET terminal = 1, result_payload = ?, result_usage = ?,
	observed_status = ?, transition_version = transition_version + 1,
	updated_at = ? WHERE attempt_id = ? AND terminal = 0`,
		resultPayload, resultUsage, observedStatus, time.Now().UTC().Format(time.RFC3339), attemptID)
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

// SetCodexAttemptAccepted records the protected-mode acceptance upgrade
// (spec §3.7): accepted is written ONLY from verified ordered rollout
// correlation of a protected attempt — advisory attempts keep accepted
// NULL. The write is once-only; a replay of the same upgrade is
// idempotent.
func (s *Store) SetCodexAttemptAccepted(ctx context.Context, attemptID string) error {
	res, err := s.DB().ExecContext(ctx, `
UPDATE codex_turn_attempts SET accepted = 1,
	transition_version = transition_version + 1, updated_at = ?
WHERE attempt_id = ? AND accepted IS NULL
  AND rollout_protection = 'protected'`,
		time.Now().UTC().Format(time.RFC3339), attemptID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var accepted sql.NullInt64
		var protection string
		if err := s.DB().QueryRowContext(ctx,
			`SELECT accepted, rollout_protection FROM codex_turn_attempts WHERE attempt_id = ?`,
			attemptID).Scan(&accepted, &protection); err != nil {
			return err
		}
		if accepted.Valid && accepted.Int64 == 1 {
			return nil // idempotent replay of the same upgrade
		}
		return fmt.Errorf(
			"acceptance upgrade requires a protected attempt without recorded acceptance (attempt %s protection=%q)",
			attemptID, protection)
	}
	return nil
}

// FindCodexProtectionAttestation returns the attestation id whose
// durable cprot-v2 row matches the frozen launch tuple (codex version,
// platform, manifest digest, profile digest) EXACTLY — all four binding
// columns, spec §3.7 — or "" when none matches. This is the adapter-side
// freeze check for the §3.7 protected-evidence upgrade; a row that
// disagrees on ANY tuple member (manifest digest included) never
// satisfies the lookup.
func (s *Store) FindCodexProtectionAttestation(ctx context.Context, codexVersion, platform, manifestDigest, profileDigest string) (string, error) {
	var id string
	err := s.DB().QueryRowContext(ctx, `
SELECT attestation_id FROM codex_protection_attestations
WHERE codex_version = ? AND platform = ? AND manifest_digest = ? AND profile_digest = ?
ORDER BY probed_at DESC LIMIT 1`,
		codexVersion, platform, manifestDigest, profileDigest).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query codex protection attestation: %w", err)
	}
	return id, nil
}

// CodexAttemptLaunchStates returns the launch reservation states for an
// attempt in reservation order (reserved|started|start_failed|dead).
func (s *Store) CodexAttemptLaunchStates(ctx context.Context, attemptID string) ([]string, error) {
	rows, err := s.DB().QueryContext(ctx, `
SELECT state FROM codex_attempt_launches
WHERE attempt_id = ? ORDER BY reservation_seq`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("query codex launch states: %w", err)
	}
	defer rows.Close()
	var states []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, rows.Err()
}

// GetCodexTurnAttempt returns the current attempt row.
func (s *Store) GetCodexTurnAttempt(ctx context.Context, attemptID string) (*CodexTurnAttempt, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT attempt_id, session_id, turn_key, prompt_digest,
       baseline_file_identity, baseline_size, baseline_entries, materialized_baseline,
       rollout_protection, protection_attestation_id, launch_count, absence_redispatch_consumed,
       absence_verified, accepted, terminal, result_payload, result_usage, observed_status,
       uncertainty_disposition, native_turn_id,
       transition_version, created_at, updated_at
FROM codex_turn_attempts WHERE attempt_id = ?`, attemptID)
	return scanCodexAttempt(row)
}

// GetLatestCodexTurnAttempt returns the latest attempt for a turn.
func (s *Store) GetLatestCodexTurnAttempt(ctx context.Context, sessionID, turnKey string) (*CodexTurnAttempt, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT attempt_id, session_id, turn_key, prompt_digest,
       baseline_file_identity, baseline_size, baseline_entries, materialized_baseline,
       rollout_protection, protection_attestation_id, launch_count, absence_redispatch_consumed,
       absence_verified, accepted, terminal, result_payload, result_usage, observed_status,
       uncertainty_disposition, native_turn_id,
       transition_version, created_at, updated_at
FROM codex_turn_attempts
WHERE session_id = ? AND turn_key = ?
ORDER BY created_at DESC LIMIT 1`, sessionID, turnKey)
	return scanCodexAttempt(row)
}

func scanCodexAttempt(row *sql.Row) (*CodexTurnAttempt, error) {
	var a CodexTurnAttempt
	if err := scanCodexAttemptInto(row.Scan, &a); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func scanCodexAttemptInto(scan func(dest ...any) error, a *CodexTurnAttempt) error {
	var createdAt, updatedAt string
	var terminal, consumed, matBaseline int
	var acceptedNull sql.NullInt64
	var nativeTurnID, attestationID, disposition sql.NullString
	if err := scan(
		&a.AttemptID, &a.SessionID, &a.TurnKey, &a.PromptDigest,
		&a.BaselineIdentity, &a.BaselineSize, &a.BaselineEntries, &matBaseline,
		&a.RolloutProtection, &attestationID, &a.LaunchCount, &consumed,
		&a.AbsenceVerified, &acceptedNull, &terminal, &a.ResultPayload, &a.ResultUsage, &a.ObservedStatus,
		&disposition, &nativeTurnID,
		&a.TransitionVersion, &createdAt, &updatedAt,
	); err != nil {
		return err
	}
	a.BaselineMaterialized = matBaseline != 0
	a.AbsenceRedispatch = consumed != 0
	a.Terminal = terminal != 0
	a.NativeTurnID = nullStr(nativeTurnID)
	a.AttestationID = nullStr(attestationID)
	if acceptedNull.Valid {
		v := acceptedNull.Int64 == 1
		a.Accepted = &v
	}
	a.UncertaintyDisposition = nullStr(disposition)
	a.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	a.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return nil
}

// HasCodexUnresolvedAttempts reports whether the native session has any
// uncertain attempt without a controller disposition — these block all
// subsequent turns on the native session (spec §3.5). Codex attempts
// correlate to the native identity through the session binding.
func (s *Store) HasCodexUnresolvedAttempts(ctx context.Context, nativeID string) (bool, error) {
	var count int
	err := s.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM codex_turn_attempts
JOIN codex_session_bindings ON codex_session_bindings.session_id = codex_turn_attempts.session_id
WHERE codex_session_bindings.native_id = ?
  AND codex_turn_attempts.observed_status = 'uncertain'
  AND codex_turn_attempts.uncertainty_disposition IS NULL`, nativeID).Scan(&count)
	return count > 0, err
}
