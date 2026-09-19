package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

type SessionRecord struct {
	ID                  string
	RunID               string
	Contributor         string
	Role                string
	IsActiveContributor bool
	State               string
	Visibility          string
	RecoveryGeneration  uint64
	ActiveTurnKey       *string
}

type PendingPrompt struct {
	SessionID string
	Prompt    string
	CreatedAt time.Time
}

type journalPayload struct {
	CallerLease string           `json:"caller_lease"`
	Receipt     OperationReceipt `json:"receipt"`
}

func computeFingerprint(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func checkOrRecordIdempotency(tx *sql.Tx, opID string, callerLease string, cmdType string, fingerprint string) (*OperationReceipt, error) {
	var storedCmdType, storedFingerprint, payloadJSON string
	err := tx.QueryRow("SELECT command_type, command_fingerprint, payload_json FROM journal_entries WHERE op_id = ?;", opID).Scan(&storedCmdType, &storedFingerprint, &payloadJSON)
	if err == sql.ErrNoRows {
		return nil, nil // not found, proceed with command
	}
	if err != nil {
		return nil, fmt.Errorf("query journal op_id: %w", err)
	}

	var jp journalPayload
	if err := json.Unmarshal([]byte(payloadJSON), &jp); err != nil {
		return nil, fmt.Errorf("unmarshal stored receipt: %w", err)
	}

	// Authority check: callerLease must match
	if jp.CallerLease != callerLease {
		return nil, ErrUnauthorizedOperation
	}

	// Parameters check
	if storedCmdType != cmdType || storedFingerprint != fingerprint {
		return nil, ErrIdempotencyConflict
	}

	return &jp.Receipt, nil
}

func recordJournalEntry(tx *sql.Tx, opID string, cmdType string, fingerprint string, runID string, sessionID string, turnKey string, eventKind string, receipt OperationReceipt, callerLease string) error {
	jp := journalPayload{
		CallerLease: callerLease,
		Receipt:     receipt,
	}
	payloadBytes, err := json.Marshal(jp)
	if err != nil {
		return fmt.Errorf("marshal journal payload: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	var sessVal any
	if sessionID != "" {
		sessVal = sessionID
	}
	var turnVal any
	if turnKey != "" {
		turnVal = turnKey
	}

	_, err = tx.Exec(`
INSERT INTO journal_entries (op_id, command_type, command_fingerprint, run_id, session_id, turn_key, event_kind, payload_version, payload_json, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?);`, opID, cmdType, fingerprint, runID, sessVal, turnVal, eventKind, string(payloadBytes), now)
	if err != nil {
		return fmt.Errorf("insert journal entry: %w", err)
	}
	return nil
}

func (s *Store) CreateRun(ctx context.Context, opID string, runID string, lease string) (OperationReceipt, error) {
	fp := computeFingerprint("create_run", runID, lease)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, lease, "create_run", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO runs (run_id, brief_digest, source_digest, profile_digest, controller_lease, lifecycle, created_at, updated_at)
VALUES (?, 'initial_brief', 'initial_src', 'initial_profile', ?, 'active', ?, ?);`, runID, lease, now, now)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert run: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "create_run",
		CommittedVersion: 1,
		CreatedAt:        time.Now().UTC(),
	}

	if err := recordJournalEntry(tx.Tx(), opID, "create_run", fp, runID, "", "", "run_created", receipt, lease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) CreateSession(ctx context.Context, opID string, callerLease string, session SessionRecord) (OperationReceipt, error) {
	fp := computeFingerprint("create_session", session.ID, session.RunID, session.Contributor, fmt.Sprintf("%v", session.IsActiveContributor), session.State, session.Visibility)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "create_session", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	// Validate caller authority against runs.controller_lease
	var runLease string
	err = tx.Tx().QueryRowContext(ctx, "SELECT controller_lease FROM runs WHERE run_id = ?;", session.RunID).Scan(&runLease)
	if err != nil || runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}

	isActive := 0
	if session.IsActiveContributor {
		isActive = 1
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO sessions (session_id, run_id, contributor, is_active_contributor, state, lifecycle, controller_status, visibility, recovery_gen, active_recovery_gen, row_version, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'active', 'connected', ?, ?, ?, 1, ?, ?);`,
		session.ID, session.RunID, session.Contributor, isActive, session.State, session.Visibility, session.RecoveryGeneration, session.RecoveryGeneration, now, now)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert session: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "create_session",
		SessionID:        session.ID,
		CommittedVersion: 1,
		CreatedAt:        time.Now().UTC(),
	}

	if err := recordJournalEntry(tx.Tx(), opID, "create_session", fp, session.RunID, session.ID, "", "session_created", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) QueuePrompt(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, prompt PendingPrompt) (OperationReceipt, error) {
	sanitized := SanitizeText(prompt.Prompt)
	fp := computeFingerprint("queue_prompt", sessionID, sanitized)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "queue_prompt", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	// Validate session, authority, and expected version
	var runID, runLease string
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version 
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if expectedVersion > 0 && currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}

	turnKey := fmt.Sprintf("turn_%d", currentVer)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Insert or replace in pending_prompts
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO pending_prompts (session_id, turn_key, prompt, queued_at)
VALUES (?, ?, ?, ?);`, sessionID, turnKey, sanitized, now)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert pending prompt: %w", err)
	}

	// Bump session row_version
	newVer := currentVer + 1
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET row_version = ?, updated_at = ? WHERE session_id = ?;`, newVer, now, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("bump session version: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "queue_prompt",
		SessionID:        sessionID,
		TurnKey:          turnKey,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          sanitized,
	}

	if err := recordJournalEntry(tx.Tx(), opID, "queue_prompt", fp, runID, sessionID, turnKey, "prompt_queued", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) ReplacePendingPrompt(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, prompt PendingPrompt) (OperationReceipt, error) {
	sanitized := SanitizeText(prompt.Prompt)
	fp := computeFingerprint("replace_pending_prompt", sessionID, sanitized)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "replace_pending_prompt", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease string
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version 
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if expectedVersion > 0 && currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Update pending prompt
	res, err := tx.Tx().ExecContext(ctx, `
UPDATE pending_prompts SET prompt = ?, queued_at = ? WHERE session_id = ?;`, sanitized, now, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("update pending prompt: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return OperationReceipt{}, fmt.Errorf("no pending prompt to replace for session %s", sessionID)
	}

	newVer := currentVer + 1
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET row_version = ?, updated_at = ? WHERE session_id = ?;`, newVer, now, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("bump session version: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "replace_pending_prompt",
		SessionID:        sessionID,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          sanitized,
	}

	if err := recordJournalEntry(tx.Tx(), opID, "replace_pending_prompt", fp, runID, sessionID, "", "pending_prompt_replaced", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) DiscardPendingPrompt(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64) (OperationReceipt, error) {
	fp := computeFingerprint("discard_pending_prompt", sessionID)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "discard_pending_prompt", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease string
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version 
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if expectedVersion > 0 && currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Delete from pending_prompts
	_, err = tx.Tx().ExecContext(ctx, `DELETE FROM pending_prompts WHERE session_id = ?;`, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("delete pending prompt: %w", err)
	}

	newVer := currentVer + 1
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET row_version = ?, updated_at = ? WHERE session_id = ?;`, newVer, now, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("bump session version: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "discard_pending_prompt",
		SessionID:        sessionID,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
	}

	if err := recordJournalEntry(tx.Tx(), opID, "discard_pending_prompt", fp, runID, sessionID, "", "pending_prompt_discarded", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
