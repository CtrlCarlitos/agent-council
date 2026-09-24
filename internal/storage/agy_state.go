package storage

// AC-010 durable state for the Agy adapter: session bindings, turn
// attempts carrying the dispatched tool allow-list and the evidence
// derived from verifying which tools actually executed, and crash-safe
// launch reservations with executor identity. All transitions mirror
// the AC-008 Claude / AC-009 Codex adapters: atomic single-transaction
// updates with idempotency via the journal.
//
// Agy differs from its siblings in one structural way: there is NO
// redispatch branch. launch_count is capped at 1 (0→1 only); a second
// reservation attempt is always an error and changes nothing — there is
// no protected-absence path that authorizes a second launch. Acceptance
// of dispatched input is folded into RecordAgyNativeStepIndex: the
// arrival of the first native step index IS the crash-safe evidence
// that the native process began consuming the queued input, so it sets
// both native_step_index and accepted in the same atomic write.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── JSON tool-list helpers ──────────────────────────────────────────────

// marshalStrings canonicalizes a possibly-nil string slice to a JSON
// array; an empty or nil slice becomes the literal "[]" (the schema
// default), never SQL NULL or an empty string.
func marshalStrings(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// unmarshalStrings is the inverse of marshalStrings: "[]", "", and
// malformed JSON all decode to nil (never an error — these columns are
// evidence, not caller input, so a decode failure must not break reads).
// The silent nil is the codebase convention; a caller must never read nil
// as proof that a set was recorded empty.
func unmarshalStrings(raw string) []string {
	if strings.TrimSpace(raw) == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// ── Session bindings ────────────────────────────────────────────────────

type AgySessionBinding struct {
	SessionID         string
	NativeID          string
	Materialized      bool
	Model             string
	Workspace         string
	ProfileDigest     string
	ConversationPath  *string
	FileIdentity      *string
	FirstPromptDigest *string
	CreatedAt         time.Time
}

// InsertAgySessionBinding persists a new unmaterialized binding. The
// native_id is a UUIDv4-shaped identity enforced by the schema CHECK;
// fail closed if the session or native identity is already bound.
// Direct use is reserved for adapter contract tests and fixtures; the
// production birth path is BindAgySession (controller authority
// re-validated inside the transaction, journaled, receipt-replayed).
func (s *Store) InsertAgySessionBinding(ctx context.Context, b AgySessionBinding) error {
	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return fmt.Errorf("begin binding insert: %w", err)
	}
	defer tx.Rollback()
	if err := insertAgySessionBindingTx(ctx, tx.Tx(), b); err != nil {
		return err
	}
	return tx.Commit()
}

func insertAgySessionBindingTx(ctx context.Context, tx *sql.Tx, b AgySessionBinding) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO agy_session_bindings
	(session_id, native_id, materialized, model, workspace, profile_digest, created_at)
VALUES (?, ?, 0, ?, ?, ?, ?)`,
		b.SessionID, b.NativeID, b.Model, b.Workspace, b.ProfileDigest, b.CreatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("insert agy session binding: %w", err)
	}
	return nil
}

// BindAgySession is the production session-birth persistence path
// (AC-008/AC-009 BindClaudeSession/BindCodexSession pattern): the
// adapter does not persist; the service records the binding under the
// run controller's authority. Authority is re-validated INSIDE the
// write transaction — a lease handed off while the native creation was
// in flight can no longer publish the binding — before the idempotency
// check; the operation is idempotent by op_id with receipt replay; a
// session that already has a binding is rejected (one native identity
// per session); the native_id shape is enforced by the schema.
//
// A session with an OPEN creation-uncertainty episode (an in-flight
// marker included) cannot be bound here: the service's birth path binds
// through BindAgySessionClosingEpisode, which closes its own marker in
// the same transaction.
func (s *Store) BindAgySession(ctx context.Context, opID, callerLease string, b AgySessionBinding) (OperationReceipt, error) {
	return s.bindAgySession(ctx, opID, callerLease, b, 0)
}

// BindAgySessionClosingEpisode is BindAgySession for the service's birth
// path: in ONE transaction it re-validates the lease, replays by op_id,
// refuses an existing binding, inserts the binding, journals the op, and
// closes the session's open in-flight creation marker `episode` with
// disposition AgyUncertaintyBound (resolution op = opID, generation =
// the validated one). A missing, closed, or non-in-flight episode is
// refused before any write. The op fingerprint is BindAgySession's, so a
// replay of the same create op through either entry point returns the
// committed receipt.
func (s *Store) BindAgySessionClosingEpisode(ctx context.Context, opID, callerLease string, b AgySessionBinding, episode int64) (OperationReceipt, error) {
	if episode < 1 {
		return OperationReceipt{}, errors.New("binding that closes a creation marker requires the episode")
	}
	return s.bindAgySession(ctx, opID, callerLease, b, episode)
}

func (s *Store) bindAgySession(ctx context.Context, opID, callerLease string, b AgySessionBinding, episode int64) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" || strings.TrimSpace(callerLease) == "" {
		return OperationReceipt{}, errors.New("binding requires the operation id and the controller lease")
	}
	if strings.TrimSpace(b.SessionID) == "" || strings.TrimSpace(b.NativeID) == "" {
		return OperationReceipt{}, errors.New("binding requires the session id and the native id")
	}
	if strings.TrimSpace(b.ProfileDigest) == "" {
		return OperationReceipt{}, errors.New("binding requires the frozen profile digest")
	}
	runID, err := s.GetSessionRunID(ctx, b.SessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("session run lookup: %w", err)
	}
	fp := computeFingerprint("bind_agy_session", b.SessionID, b.NativeID, b.Model, b.Workspace, b.ProfileDigest)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	gen, err := classifyCredential(ctx, tx.Tx(), runID, callerLease, true)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("controller authority: %w", err)
	}
	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "bind_agy_session", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var existing string
	err = tx.Tx().QueryRowContext(ctx,
		`SELECT native_id FROM agy_session_bindings WHERE session_id = ?`, b.SessionID).Scan(&existing)
	if err == nil {
		return OperationReceipt{}, fmt.Errorf("session %s is already bound to native identity %s", b.SessionID, existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return OperationReceipt{}, fmt.Errorf("query existing binding: %w", err)
	}
	var openEpisode int64
	var openReason string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT episode, reason FROM agy_creation_uncertainties
WHERE session_id = ? AND disposition IS NULL`, b.SessionID).Scan(&openEpisode, &openReason)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if episode > 0 {
			return OperationReceipt{}, fmt.Errorf("session %s has no open creation marker %d to close", b.SessionID, episode)
		}
	case err != nil:
		return OperationReceipt{}, fmt.Errorf("query open creation uncertainty: %w", err)
	case episode == 0:
		return OperationReceipt{}, &ErrAgyCreationUncertaintyOpen{SessionID: b.SessionID, Episode: openEpisode, Reason: openReason}
	case openEpisode != episode || openReason != AgyCreationInFlightReason:
		return OperationReceipt{}, fmt.Errorf("session %s open creation-uncertainty episode %d (%s) is not the unannotated in-flight marker %d; refusing to bind",
			b.SessionID, openEpisode, openReason, episode)
	}

	now := time.Now().UTC()
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "bind_agy_session",
		SessionID:        b.SessionID,
		CommittedVersion: 1,
		CreatedAt:        now,
		Payload:          b.NativeID,
	}
	if err := recordJournalEntry(tx.Tx(), opID, "bind_agy_session", fp, runID, b.SessionID, "",
		"agy_session_binding", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	if err := insertAgySessionBindingTx(ctx, tx.Tx(), b); err != nil {
		return OperationReceipt{}, err
	}
	if episode > 0 {
		res, err := tx.Tx().ExecContext(ctx, `
UPDATE agy_creation_uncertainties
SET disposition = ?, resolution_reason = ?, resolution_generation = ?, resolution_op_id = ?, resolved_at = ?
WHERE session_id = ? AND episode = ? AND disposition IS NULL`,
			AgyUncertaintyBound, "bound to native conversation "+b.NativeID, gen, opID, now.Format(time.RFC3339Nano),
			b.SessionID, episode)
		if err != nil {
			return OperationReceipt{}, fmt.Errorf("close creation marker: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return OperationReceipt{}, fmt.Errorf("close creation marker: expected one open row, updated %d (%v)", n, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// GetAgySessionBinding returns the binding for a logical session.
func (s *Store) GetAgySessionBinding(ctx context.Context, sessionID string) (*AgySessionBinding, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT session_id, native_id, materialized, model, workspace, conversation_path,
       file_identity, profile_digest, first_prompt_digest, created_at
FROM agy_session_bindings WHERE session_id = ?`, sessionID)
	var b AgySessionBinding
	var createdAt string
	var mat int
	var conversationPath, fileIdentity, firstPrompt sql.NullString
	if err := row.Scan(&b.SessionID, &b.NativeID, &mat, &b.Model, &b.Workspace,
		&conversationPath, &fileIdentity, &b.ProfileDigest, &firstPrompt, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query agy session binding: %w", err)
	}
	b.Materialized = mat != 0
	b.ConversationPath = nullStr(conversationPath)
	b.FileIdentity = nullStr(fileIdentity)
	b.FirstPromptDigest = nullStr(firstPrompt)
	b.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &b, nil
}

// MarkAgySessionMaterialized flips materialized after the native
// conversation file is observed, recording its resolved path and file
// identity together. Only transitions false → true; already-materialized
// is a no-op.
func (s *Store) MarkAgySessionMaterialized(ctx context.Context, sessionID, conversationPath, fileIdentity string) error {
	_, err := s.DB().ExecContext(ctx, `
UPDATE agy_session_bindings SET materialized = 1, conversation_path = ?, file_identity = ?
WHERE session_id = ? AND materialized = 0`, conversationPath, fileIdentity, sessionID)
	return err
}

// ── Turn attempts ───────────────────────────────────────────────────────

type AgyTurnAttempt struct {
	AttemptID              string
	SessionID              string
	TurnKey                string
	NativeStepIndex        *int
	PromptDigest           string
	LaunchCount            int
	Accepted               *bool
	Terminal               bool
	ResultPayload          *string
	ResultUsage            *string
	RequiredTools          []string
	ExecutedTools          []string
	MissingRequiredTools   []string
	DeniedTools            []string
	AmbiguousDenials       []string
	UnattributedDenials    []string
	UnmappedDenials        []string
	VerificationIncomplete bool
	ObservedStatus         string // completed|failed|cancelled|missing|uncertain
	UncertaintyDisposition *string
	// OrphanConversationID is the native conversation id a turn launch
	// reported instead of the bound one (a silent fallback to a NEW
	// conversation, hazard 2): recorded once, before the child is
	// terminated, as durable diagnostic evidence — never bound.
	OrphanConversationID *string
	TransitionVersion    int64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// AgyExeIdentity is the executable identity captured at launch start —
// device/inode pair and content digest of the process image actually
// exec'd, recorded as launch evidence (spec: sealed-image executor
// launch).
type AgyExeIdentity struct {
	DevIno string
	Digest string
}

// AgyVerification is the tool-verification evidence recorded exactly
// once at attempt termination: which tools the native process actually
// executed against the dispatched required-tools allow-list, and how
// any denial evidence classified.
type AgyVerification struct {
	Executed        []string
	MissingRequired []string
	Denied          []string
	Ambiguous       []string
	Unattributed    []string
	Unmapped        []string
	Incomplete      bool
}

// InsertAgyTurnAttempt durably records the pre-launch baseline before
// any process can start (crash-safe ordering, step 1, mirroring
// AC-009 §3.11): the dispatched required-tools allow-list is recorded
// with the attempt so verification at termination has something to
// verify against, independent of whatever the queued-prompt row still
// says by then.
func (s *Store) InsertAgyTurnAttempt(ctx context.Context, a AgyTurnAttempt) error {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := insertAgyTurnAttemptTx(ctx, tx, a); err != nil {
		return err
	}
	return tx.Commit()
}

func insertAgyTurnAttemptTx(ctx context.Context, tx *sql.Tx, a AgyTurnAttempt) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO agy_turn_attempts
	(attempt_id, session_id, turn_key, prompt_digest, required_tools_json,
	 launch_count, observed_status, transition_version, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 0, 'uncertain', 1, ?, ?)`,
		a.AttemptID, a.SessionID, a.TurnKey, a.PromptDigest, marshalStrings(a.RequiredTools),
		a.CreatedAt.UTC().Format(time.RFC3339), a.CreatedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("insert agy turn attempt: %w", err)
	}
	return nil
}

// InsertAgyTurnAttemptAndReserveLaunch durably records the attempt
// (with its prompt digest and required tools) AND consumes its single
// launch slot in ONE transaction (spec §3.5: the launch is reserved in
// the same transaction, before the executor starts). A crash leaves
// either nothing or both — never an attempt without its reservation.
func (s *Store) InsertAgyTurnAttemptAndReserveLaunch(ctx context.Context, a AgyTurnAttempt, executorIdentity string) (reservationSeq int64, err error) {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := insertAgyTurnAttemptTx(ctx, tx, a); err != nil {
		return 0, err
	}
	seq, err := reserveAgyLaunchTx(ctx, tx, a.AttemptID, executorIdentity)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// ReserveAgyLaunch atomically consumes the single launch slot
// (launch_count 0→1) and inserts the RESERVED launch row — all before
// executor.Start. There is no redispatch branch: any attempt whose
// launch_count is already 1 refuses a second reservation outright,
// regardless of the prior launch's fate. A failed precondition returns
// an error and nothing changes.
func (s *Store) ReserveAgyLaunch(ctx context.Context, attemptID, executorIdentity string) (reservationSeq int64, err error) {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	seq, err := reserveAgyLaunchTx(ctx, tx, attemptID, executorIdentity)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

func reserveAgyLaunchTx(ctx context.Context, tx *sql.Tx, attemptID, executorIdentity string) (int64, error) {
	var count int
	if err := tx.QueryRowContext(ctx, `
SELECT launch_count FROM agy_turn_attempts WHERE attempt_id = ?`, attemptID).Scan(&count); err != nil {
		return 0, fmt.Errorf("query attempt: %w", err)
	}
	if count != 0 {
		return 0, fmt.Errorf("launch reservation precondition failed for %s (count=%d): no redispatch branch", attemptID, count)
	}

	// Use MAX(reservation_seq)+1 for parity with the sibling adapters'
	// retained-evidence numbering; in practice this attempt only ever
	// reaches seq 1, since launch_count never returns to 0.
	var maxSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
SELECT MAX(reservation_seq) FROM agy_attempt_launches WHERE attempt_id = ?`, attemptID).Scan(&maxSeq); err != nil {
		return 0, err
	}
	seq := int64(1)
	if maxSeq.Valid && maxSeq.Int64 > 0 {
		seq = maxSeq.Int64 + 1
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO agy_attempt_launches (attempt_id, reservation_seq, state, executor_identity)
VALUES (?, ?, 'reserved', ?)`, attemptID, seq, executorIdentity); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE agy_turn_attempts SET launch_count = launch_count + 1,
	transition_version = transition_version + 1, updated_at = ?
WHERE attempt_id = ?`, time.Now().UTC().Format(time.RFC3339), attemptID); err != nil {
		return 0, err
	}
	return seq, nil
}

// RecordAgyLaunchState marks a reserved launch as started, start-failed,
// or dead. start_failed classifies the attempt as definitively missing
// — positive pre-start evidence, never an unresolved uncertainty — but,
// unlike codex/claude, does NOT release the launch slot: Agy has no
// redispatch branch at all, so launch_count stays at 1 forever once
// consumed and ReserveAgyLaunch never authorizes a second launch for
// this attempt, regardless of the first launch's fate. A caller that
// wants to try again dispatches a NEW attempt (a new attempt_id), not a
// second reservation on this one. exeIdentity, when non-nil, is
// recorded at 'started' (the sealed-image executor identity captured
// at launch).
func (s *Store) RecordAgyLaunchState(ctx context.Context, attemptID string, seq int64, state string, exitCode *int, exeIdentity *AgyExeIdentity) error {
	now := time.Now().UTC().Format(time.RFC3339)
	switch state {
	case "started":
		var devIno, digest any
		if exeIdentity != nil {
			devIno = exeIdentity.DevIno
			digest = exeIdentity.Digest
		}
		_, err := s.DB().ExecContext(ctx, `
UPDATE agy_attempt_launches SET state = 'started', started_at = ?, exe_dev_ino = ?, exe_digest = ?
WHERE attempt_id = ? AND reservation_seq = ?`, now, devIno, digest, attemptID, seq)
		return err
	case "start_failed":
		tx, txErr := s.DB().BeginTx(ctx, nil)
		if txErr != nil {
			return txErr
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `
UPDATE agy_attempt_launches SET state = 'start_failed'
WHERE attempt_id = ? AND reservation_seq = ?`, attemptID, seq); err != nil {
			return err
		}
		// No process was created: classify the attempt as definitively
		// missing — positive pre-start evidence, so it never blocks the
		// native session as an unresolved uncertainty. The launch slot
		// is NOT released (no redispatch branch); the status guard never
		// downgrades a resolved attempt. Both writes are one atomic
		// transaction.
		if _, err := tx.ExecContext(ctx, `
UPDATE agy_turn_attempts SET observed_status = 'missing'
WHERE attempt_id = ? AND observed_status = 'uncertain'`, attemptID); err != nil {
			return err
		}
		return tx.Commit()
	case "dead":
		_, err := s.DB().ExecContext(ctx, `
UPDATE agy_attempt_launches SET state = 'dead', known_dead_at = ?, exit_code = ?
WHERE attempt_id = ? AND reservation_seq = ?`, now, exitCode, attemptID, seq)
		return err
	default:
		return fmt.Errorf("unknown launch state %q", state)
	}
}

// RecordAgyStdinTransmitted records the first-byte transmission
// boundary on a launch row. First-write-wins: the WHERE clause requires
// the boundary to be unset AND the launch to be in started state.
func (s *Store) RecordAgyStdinTransmitted(ctx context.Context, attemptID string, seq int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.DB().ExecContext(ctx, `
UPDATE agy_attempt_launches SET first_stdin_byte_at = ?
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

// RecordAgyNativeStepIndex binds the in-life native step index on the
// attempt — the crash-safe evidence that the native process began
// consuming the dispatched input. Because that evidence IS the
// definitive proof of acceptance, this call ALSO flips accepted in the
// same atomic write. The binding is once-only: a conflicting rebinding
// fails closed; a replay of the same index is idempotent.
func (s *Store) RecordAgyNativeStepIndex(ctx context.Context, attemptID string, idx int) error {
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
UPDATE agy_turn_attempts SET native_step_index = ?, accepted = 1,
	transition_version = transition_version + 1, updated_at = ?
WHERE attempt_id = ? AND native_step_index IS NULL`,
		idx, time.Now().UTC().Format(time.RFC3339), attemptID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var existing sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT native_step_index FROM agy_turn_attempts WHERE attempt_id = ?`, attemptID).Scan(&existing); err != nil {
			return err
		}
		if existing.Valid && existing.Int64 == int64(idx) {
			return tx.Commit() // idempotent replay of the same binding
		}
		return fmt.Errorf("attempt %s already carries native step index %v; refusing %d",
			attemptID, existing, idx)
	}
	return tx.Commit()
}

// RecordAgyOrphanConversation durably records the orphan native
// conversation id a turn launch reported instead of the bound one. It
// is once-only: replaying the same id is idempotent; a different id is
// refused and changes nothing. An unknown attempt is an error.
func (s *Store) RecordAgyOrphanConversation(ctx context.Context, attemptID, id string) error {
	if strings.TrimSpace(attemptID) == "" {
		return errors.New("orphan conversation record requires the attempt id")
	}
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT orphan_conversation_id FROM agy_turn_attempts WHERE attempt_id = ?`, attemptID).Scan(&existing); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("attempt %s does not exist", attemptID)
		}
		return fmt.Errorf("query orphan conversation: %w", err)
	}
	if existing.Valid {
		if existing.String == id {
			return tx.Commit() // idempotent replay
		}
		return fmt.Errorf("attempt %s already records orphan conversation %q; refusing %q", attemptID, existing.String, id)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE agy_turn_attempts SET orphan_conversation_id = ?, transition_version = transition_version + 1,
	updated_at = ? WHERE attempt_id = ?`, id, time.Now().UTC().Format(time.RFC3339), attemptID); err != nil {
		return fmt.Errorf("record orphan conversation: %w", err)
	}
	return tx.Commit()
}

// SetAgyAttemptObservedStatus records the evidence-derived outcome
// without marking the attempt terminal (e.g. a non-terminal status
// refinement mid-flight).
func (s *Store) SetAgyAttemptObservedStatus(ctx context.Context, attemptID, status string) error {
	_, err := s.DB().ExecContext(ctx, `
UPDATE agy_turn_attempts SET observed_status = ?, transition_version = transition_version + 1,
	updated_at = ? WHERE attempt_id = ?`, status, time.Now().UTC().Format(time.RFC3339), attemptID)
	return err
}

// SetAgyAttemptTerminal records the verified terminal result payload,
// its evidence-derived observed status, and the tool-verification
// evidence in ONE atomic write. Exactly-once guard: only the first
// successful write wins (the WHERE clause requires terminal = 0).
func (s *Store) SetAgyAttemptTerminal(ctx context.Context, attemptID, observedStatus, resultPayload, resultUsage string, verification AgyVerification) error {
	if observedStatus != "completed" && observedStatus != "failed" && observedStatus != "cancelled" {
		return fmt.Errorf("terminal observed status must be completed, failed, or cancelled, got %q", observedStatus)
	}
	incomplete := 0
	if verification.Incomplete {
		incomplete = 1
	}
	res, err := s.DB().ExecContext(ctx, `
UPDATE agy_turn_attempts SET terminal = 1, result_payload = ?, result_usage = ?,
	observed_status = ?, executed_tools_json = ?, missing_required_tools_json = ?,
	denied_tools_json = ?, ambiguous_denials_json = ?, unattributed_denials_json = ?,
	unmapped_denials_json = ?, verification_incomplete = ?,
	transition_version = transition_version + 1,
	updated_at = ? WHERE attempt_id = ? AND terminal = 0`,
		resultPayload, resultUsage, observedStatus,
		marshalStrings(verification.Executed), marshalStrings(verification.MissingRequired),
		marshalStrings(verification.Denied), marshalStrings(verification.Ambiguous),
		marshalStrings(verification.Unattributed), marshalStrings(verification.Unmapped),
		incomplete, time.Now().UTC().Format(time.RFC3339), attemptID)
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

// AgyAttemptLaunchStates returns the launch reservation states for an
// attempt in reservation order (reserved|started|start_failed|dead).
func (s *Store) AgyAttemptLaunchStates(ctx context.Context, attemptID string) ([]string, error) {
	rows, err := s.DB().QueryContext(ctx, `
SELECT state FROM agy_attempt_launches
WHERE attempt_id = ? ORDER BY reservation_seq`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("query agy launch states: %w", err)
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

// GetAgyTurnAttempt returns the current attempt row.
func (s *Store) GetAgyTurnAttempt(ctx context.Context, attemptID string) (*AgyTurnAttempt, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT attempt_id, session_id, turn_key, native_step_index, prompt_digest, launch_count,
       accepted, terminal, result_payload, result_usage,
       required_tools_json, executed_tools_json, missing_required_tools_json, denied_tools_json,
       ambiguous_denials_json, unattributed_denials_json, unmapped_denials_json, verification_incomplete,
       observed_status, uncertainty_disposition, orphan_conversation_id, transition_version, created_at, updated_at
FROM agy_turn_attempts WHERE attempt_id = ?`, attemptID)
	return scanAgyAttempt(row)
}

// GetLatestAgyTurnAttempt returns the latest attempt for a turn.
func (s *Store) GetLatestAgyTurnAttempt(ctx context.Context, sessionID, turnKey string) (*AgyTurnAttempt, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT attempt_id, session_id, turn_key, native_step_index, prompt_digest, launch_count,
       accepted, terminal, result_payload, result_usage,
       required_tools_json, executed_tools_json, missing_required_tools_json, denied_tools_json,
       ambiguous_denials_json, unattributed_denials_json, unmapped_denials_json, verification_incomplete,
       observed_status, uncertainty_disposition, orphan_conversation_id, transition_version, created_at, updated_at
FROM agy_turn_attempts
WHERE session_id = ? AND turn_key = ?
ORDER BY created_at DESC LIMIT 1`, sessionID, turnKey)
	return scanAgyAttempt(row)
}

func scanAgyAttempt(row *sql.Row) (*AgyTurnAttempt, error) {
	var a AgyTurnAttempt
	if err := scanAgyAttemptInto(row.Scan, &a); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func scanAgyAttemptInto(scan func(dest ...any) error, a *AgyTurnAttempt) error {
	var createdAt, updatedAt string
	var terminal, verificationIncomplete int
	var nativeStepIndex sql.NullInt64
	var acceptedNull sql.NullInt64
	var disposition, orphan sql.NullString
	var requiredJSON, executedJSON, missingJSON, deniedJSON, ambiguousJSON, unattributedJSON, unmappedJSON string
	if err := scan(
		&a.AttemptID, &a.SessionID, &a.TurnKey, &nativeStepIndex, &a.PromptDigest, &a.LaunchCount,
		&acceptedNull, &terminal, &a.ResultPayload, &a.ResultUsage,
		&requiredJSON, &executedJSON, &missingJSON, &deniedJSON,
		&ambiguousJSON, &unattributedJSON, &unmappedJSON, &verificationIncomplete,
		&a.ObservedStatus, &disposition, &orphan, &a.TransitionVersion, &createdAt, &updatedAt,
	); err != nil {
		return err
	}
	if nativeStepIndex.Valid {
		v := int(nativeStepIndex.Int64)
		a.NativeStepIndex = &v
	}
	a.Terminal = terminal != 0
	a.VerificationIncomplete = verificationIncomplete != 0
	if acceptedNull.Valid {
		v := acceptedNull.Int64 == 1
		a.Accepted = &v
	}
	a.RequiredTools = unmarshalStrings(requiredJSON)
	a.ExecutedTools = unmarshalStrings(executedJSON)
	a.MissingRequiredTools = unmarshalStrings(missingJSON)
	a.DeniedTools = unmarshalStrings(deniedJSON)
	a.AmbiguousDenials = unmarshalStrings(ambiguousJSON)
	a.UnattributedDenials = unmarshalStrings(unattributedJSON)
	a.UnmappedDenials = unmarshalStrings(unmappedJSON)
	a.UncertaintyDisposition = nullStr(disposition)
	a.OrphanConversationID = nullStr(orphan)
	a.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	a.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	return nil
}

// HasAgyUnresolvedAttempts reports whether the native session has any
// uncertain attempt without a controller disposition — these block all
// subsequent turns on the native session. Agy attempts correlate to the
// native identity through the session binding.
func (s *Store) HasAgyUnresolvedAttempts(ctx context.Context, nativeID string) (bool, error) {
	var count int
	err := s.DB().QueryRowContext(ctx, `
SELECT COUNT(*) FROM agy_turn_attempts
JOIN agy_session_bindings ON agy_session_bindings.session_id = agy_turn_attempts.session_id
WHERE agy_session_bindings.native_id = ?
  AND agy_turn_attempts.observed_status = 'uncertain'
  AND agy_turn_attempts.uncertainty_disposition IS NULL`, nativeID).Scan(&count)
	return count > 0, err
}
