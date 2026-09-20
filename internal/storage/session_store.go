package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	SessionID string    `json:"session_id"`
	TurnKey   string    `json:"turn_key"`
	Prompt    string    `json:"prompt"`
	CreatedAt time.Time `json:"created_at"`
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

	// Idempotency semantics only (AC-004 §4.3 adaptation): authority is
	// classified at the operation boundary before this helper runs, and the
	// recorded caller_lease may legitimately belong to a superseded
	// controller whose committed response the current controller recovers.
	_ = callerLease

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

func (s *Store) CreateRun(ctx context.Context, opID string, runID string, briefDigest string, sourceDigest string, profileDigest string, controllerLease string) (OperationReceipt, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(briefDigest) == "" || strings.TrimSpace(sourceDigest) == "" || strings.TrimSpace(profileDigest) == "" || strings.TrimSpace(controllerLease) == "" {
		return OperationReceipt{}, errors.New("empty run parameter or lease")
	}
	fp := computeFingerprint("create_run", runID, briefDigest, sourceDigest, profileDigest, controllerLease)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, controllerLease, "create_run", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO runs (run_id, brief_digest, source_digest, profile_digest, controller_lease, lifecycle, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'active', ?, ?);`, runID, briefDigest, sourceDigest, profileDigest, controllerLease, now, now)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert run: %w", err)
	}

	// The supplied lease is recorded as generation-0 provenance only —
	// never an adopted controller grant (AC-004).
	_, err = tx.Tx().ExecContext(ctx, `INSERT INTO controller_leases
		(run_id, generation, harness, controller_ref, lease, status, granted_by_op_id, attached_at, updated_at)
		VALUES (?, 0, NULL, 'create-run-provenance', ?, 'legacy', ?, ?, ?);`,
		runID, controllerLease, opID, now, now)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert run provenance: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "create_run",
		CommittedVersion: 1,
		CreatedAt:        time.Now().UTC(),
	}

	if err := recordJournalEntry(tx.Tx(), opID, "create_run", fp, runID, "", "", "run_created", receipt, controllerLease); err != nil {
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

	// Administrator-class authority precedes idempotent replay (AC-004).
	// CreateSession resolves the run directly: the session does not exist yet.
	if _, err := classifyCredential(ctx, tx.Tx(), session.RunID, callerLease, false); err != nil {
		return OperationReceipt{}, err
	}

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "create_session", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	// Validate caller authority against runs.controller_lease
	var runLease string
	err = tx.Tx().QueryRowContext(ctx, "SELECT controller_lease FROM runs WHERE run_id = ?;", session.RunID).Scan(&runLease)
	if err != nil {
		return OperationReceipt{}, err
	}
	_ = runLease // authority classified before replay

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
	if expectedVersion <= 0 {
		return OperationReceipt{}, ErrInvalidExpectedVersion
	}
	if strings.TrimSpace(prompt.TurnKey) == "" {
		return OperationReceipt{}, errors.New("empty turn key")
	}
	if strings.TrimSpace(prompt.Prompt) == "" {
		return OperationReceipt{}, errors.New("empty prompt")
	}

	sanitized := SanitizeText(prompt.Prompt)
	fp := computeFingerprint("queue_prompt", sessionID, prompt.TurnKey, sanitized)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	// Current controller authority precedes idempotent replay (AC-004).
	if _, err := authorizeSessionController(ctx, tx.Tx(), sessionID, callerLease, true); err != nil {
		return OperationReceipt{}, err
	}

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "queue_prompt", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	// Validate session, authority, expected version, lifecycle, controller status
	var runID, runLease, lifecycle, controllerStatus string
	var activeKey sql.NullString
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.lifecycle, s.controller_status, s.active_key
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &lifecycle, &controllerStatus, &activeKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	_ = runLease // authority classified before replay
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}
	if controllerStatus == "disconnected" {
		return OperationReceipt{}, ErrControllerDisconnected
	}
	if activeKey.Valid && activeKey.String == prompt.TurnKey {
		return OperationReceipt{}, errors.New("key already active")
	}

	// Check if turn already retired in turns
	var existingTurnPrompt string
	err = tx.Tx().QueryRowContext(ctx, "SELECT prompt FROM turns WHERE session_id = ? AND turn_key = ?;", sessionID, prompt.TurnKey).Scan(&existingTurnPrompt)
	if err == nil {
		if existingTurnPrompt != sanitized {
			return OperationReceipt{}, errors.New("retired turn identifier cannot be reused with different content")
		}
		return OperationReceipt{}, ErrTurnAlreadyExists
	} else if err != sql.ErrNoRows {
		return OperationReceipt{}, fmt.Errorf("check existing turn: %w", err)
	}

	// Check if already in pending_prompts
	var existingPendingPrompt string
	err = tx.Tx().QueryRowContext(ctx, "SELECT prompt FROM pending_prompts WHERE session_id = ? AND turn_key = ?;", sessionID, prompt.TurnKey).Scan(&existingPendingPrompt)
	if err == nil {
		if existingPendingPrompt == sanitized {
			receipt := OperationReceipt{
				OpID:             opID,
				CommandType:      "queue_prompt",
				SessionID:        sessionID,
				TurnKey:          prompt.TurnKey,
				CommittedVersion: currentVer,
				CreatedAt:        time.Now().UTC(),
				Payload:          sanitized,
			}
			if err := recordJournalEntry(tx.Tx(), opID, "queue_prompt", fp, runID, sessionID, prompt.TurnKey, "prompt_identical_noop", receipt, callerLease); err != nil {
				return OperationReceipt{}, err
			}
			if err := tx.Commit(); err != nil {
				return OperationReceipt{}, err
			}
			return receipt, nil
		}
		return OperationReceipt{}, errors.New("duplicate pending key with different content")
	} else if err != sql.ErrNoRows {
		return OperationReceipt{}, fmt.Errorf("check existing pending: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Insert into pending_prompts
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO pending_prompts (session_id, turn_key, prompt, queued_at)
VALUES (?, ?, ?, ?);`, sessionID, prompt.TurnKey, sanitized, now)
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
		TurnKey:          prompt.TurnKey,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          sanitized,
	}

	if err := recordJournalEntry(tx.Tx(), opID, "queue_prompt", fp, runID, sessionID, prompt.TurnKey, "prompt_queued", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) ReplacePendingPrompt(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, prompt PendingPrompt) (OperationReceipt, error) {
	if expectedVersion <= 0 {
		return OperationReceipt{}, ErrInvalidExpectedVersion
	}
	if strings.TrimSpace(prompt.TurnKey) == "" {
		return OperationReceipt{}, errors.New("empty turn key")
	}
	if strings.TrimSpace(prompt.Prompt) == "" {
		return OperationReceipt{}, errors.New("empty prompt")
	}

	sanitized := SanitizeText(prompt.Prompt)
	fp := computeFingerprint("replace_pending_prompt", sessionID, prompt.TurnKey, sanitized)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	// Current controller authority precedes idempotent replay (AC-004).
	if _, err := authorizeSessionController(ctx, tx.Tx(), sessionID, callerLease, true); err != nil {
		return OperationReceipt{}, err
	}

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "replace_pending_prompt", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease, lifecycle, controllerStatus string
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.lifecycle, s.controller_status
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &lifecycle, &controllerStatus)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	_ = runLease // authority classified before replay
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}
	if controllerStatus == "disconnected" {
		return OperationReceipt{}, ErrControllerDisconnected
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Update ONLY the specified pending prompt row
	res, err := tx.Tx().ExecContext(ctx, `
UPDATE pending_prompts SET prompt = ?, queued_at = ? WHERE session_id = ? AND turn_key = ?;`, sanitized, now, sessionID, prompt.TurnKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("update pending prompt: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return OperationReceipt{}, ErrPromptNotQueued
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
		TurnKey:          prompt.TurnKey,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          sanitized,
	}

	if err := recordJournalEntry(tx.Tx(), opID, "replace_pending_prompt", fp, runID, sessionID, prompt.TurnKey, "pending_prompt_replaced", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) DiscardPendingPrompt(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, turnKey string) (OperationReceipt, error) {
	if expectedVersion <= 0 {
		return OperationReceipt{}, ErrInvalidExpectedVersion
	}
	if strings.TrimSpace(turnKey) == "" {
		return OperationReceipt{}, errors.New("empty turn key")
	}

	fp := computeFingerprint("discard_pending_prompt", sessionID, turnKey)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	// Current controller authority precedes idempotent replay (AC-004).
	if _, err := authorizeSessionController(ctx, tx.Tx(), sessionID, callerLease, true); err != nil {
		return OperationReceipt{}, err
	}

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "discard_pending_prompt", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease, lifecycle, controllerStatus string
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.lifecycle, s.controller_status
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &lifecycle, &controllerStatus)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	_ = runLease // authority classified before replay
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}
	if controllerStatus == "disconnected" {
		return OperationReceipt{}, ErrControllerDisconnected
	}

	// Delete ONLY the specified pending prompt row
	res, err := tx.Tx().ExecContext(ctx, `DELETE FROM pending_prompts WHERE session_id = ? AND turn_key = ?;`, sessionID, turnKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("delete pending prompt: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return OperationReceipt{}, ErrPromptNotQueued
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
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
		TurnKey:          turnKey,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
	}

	if err := recordJournalEntry(tx.Tx(), opID, "discard_pending_prompt", fp, runID, sessionID, turnKey, "pending_prompt_discarded", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
