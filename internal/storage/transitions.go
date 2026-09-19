package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

type NativeBinding struct {
	LogicalSessionID string `json:"logical_session_id"`
	NativeSessionID  string `json:"native_session_id"`
	Harness          string `json:"harness"`
	Model            string `json:"model"`
	WorkspaceMode    string `json:"workspace_mode"`
	ToolingConfig    string `json:"tooling_config"`
}

type releaseJournalPayload struct {
	CallerLease string         `json:"caller_lease"`
	Receipt     ReleaseReceipt `json:"receipt"`
}

func (s *Store) SetNativeBinding(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, binding NativeBinding) (OperationReceipt, error) {
	fp := computeFingerprint("set_native_binding", sessionID, binding.NativeSessionID, binding.Harness, binding.Model, binding.WorkspaceMode, binding.ToolingConfig)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "set_native_binding", fp); err != nil {
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
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO native_bindings (session_id, native_session_id, harness, model, config_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET
    native_session_id = excluded.native_session_id,
    harness = excluded.harness,
    model = excluded.model,
    config_json = excluded.config_json,
    updated_at = excluded.updated_at;`, sessionID, binding.NativeSessionID, binding.Harness, binding.Model, binding.ToolingConfig, now, now)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("upsert native binding: %w", err)
	}

	newVer := currentVer + 1
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET row_version = ?, updated_at = ? WHERE session_id = ?;`, newVer, now, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("bump session version: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "set_native_binding",
		SessionID:        sessionID,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
	}

	if err := recordJournalEntry(tx.Tx(), opID, "set_native_binding", fp, runID, sessionID, "", "native_binding_set", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) ReleaseTurn(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, turnKey string) (ReleaseReceipt, error) {
	fp := computeFingerprint("release_turn", sessionID, turnKey)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return ReleaseReceipt{}, err
	}
	defer tx.Rollback()

	// Idempotency check for release_turn
	var storedCmdType, storedFingerprint, payloadJSON string
	err = tx.Tx().QueryRowContext(ctx, "SELECT command_type, command_fingerprint, payload_json FROM journal_entries WHERE op_id = ?;", opID).Scan(&storedCmdType, &storedFingerprint, &payloadJSON)
	if err == nil {
		var jp releaseJournalPayload
		if err := json.Unmarshal([]byte(payloadJSON), &jp); err == nil && jp.Receipt.TurnKey != "" {
			if jp.CallerLease != callerLease {
				return ReleaseReceipt{}, ErrUnauthorizedOperation
			}
			if storedCmdType != "release_turn" || storedFingerprint != fp {
				return ReleaseReceipt{}, ErrIdempotencyConflict
			}
			return jp.Receipt, nil
		}
	}

	var runID, runLease, state, visibility string
	var activeKey sql.NullString
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.state, s.visibility, s.active_key
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &state, &visibility, &activeKey)
	if err != nil {
		return ReleaseReceipt{}, fmt.Errorf("query session for release: %w", err)
	}

	if runLease != callerLease {
		return ReleaseReceipt{}, ErrUnauthorizedOperation
	}
	if visibility == "host_lost" {
		return ReleaseReceipt{}, ErrHostLost
	}
	if state != "parked" || (activeKey.Valid && activeKey.String != "") {
		return ReleaseReceipt{}, fmt.Errorf("session %s is not parked or already has active turn %v", sessionID, activeKey)
	}
	if expectedVersion > 0 && currentVer != expectedVersion {
		return ReleaseReceipt{}, ErrStaleUpdate
	}

	// Fetch pending prompt
	var rawPrompt string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT prompt FROM pending_prompts WHERE session_id = ?;`, sessionID).Scan(&rawPrompt)
	if err != nil {
		return ReleaseReceipt{}, fmt.Errorf("no pending prompt to release for session %s: %w", sessionID, err)
	}

	// Delete from pending_prompts
	_, err = tx.Tx().ExecContext(ctx, `DELETE FROM pending_prompts WHERE session_id = ?;`, sessionID)
	if err != nil {
		return ReleaseReceipt{}, fmt.Errorf("delete pending prompt: %w", err)
	}

	sanitizedPrompt := SanitizeText(rawPrompt)
	newVer := currentVer + 1
	attemptID := fmt.Sprintf("att_%d_%s", newVer, turnKey)
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Insert into turns
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO turns (session_id, turn_key, prompt, status, result, attempt_id, created_at)
VALUES (?, ?, ?, 'running', '', ?, ?);`, sessionID, turnKey, sanitizedPrompt, attemptID, now)
	if err != nil {
		return ReleaseReceipt{}, fmt.Errorf("insert turn: %w", err)
	}

	// Insert into dispatch_intents
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO dispatch_intents (session_id, turn_key, attempt_id, phase, recorded_at, updated_at)
VALUES (?, ?, ?, 'intent_recorded', ?, ?);`, sessionID, turnKey, attemptID, now, now)
	if err != nil {
		return ReleaseReceipt{}, fmt.Errorf("insert dispatch intent: %w", err)
	}

	// Update session
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET active_key = ?, state = 'running', row_version = ?, updated_at = ?
WHERE session_id = ?;`, turnKey, newVer, now, sessionID)
	if err != nil {
		return ReleaseReceipt{}, fmt.Errorf("update session for release: %w", err)
	}

	receipt := ReleaseReceipt{
		OperationReceipt: OperationReceipt{
			OpID:             opID,
			CommandType:      "release_turn",
			SessionID:        sessionID,
			TurnKey:          turnKey,
			CommittedVersion: newVer,
			CreatedAt:        time.Now().UTC(),
			Payload:          sanitizedPrompt,
		},
		SanitizedPrompt: sanitizedPrompt,
		TurnKey:         turnKey,
		AttemptID:       attemptID,
	}

	jp := releaseJournalPayload{
		CallerLease: callerLease,
		Receipt:     receipt,
	}
	payloadBytes, _ := json.Marshal(jp)

	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO journal_entries (op_id, command_type, command_fingerprint, run_id, session_id, turn_key, event_kind, payload_version, payload_json, created_at)
VALUES (?, 'release_turn', ?, ?, ?, ?, 'turn_released', 1, ?, ?);`, opID, fp, runID, sessionID, turnKey, string(payloadBytes), now)
	if err != nil {
		return ReleaseReceipt{}, fmt.Errorf("record release journal entry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return ReleaseReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) ReconcileSession(ctx context.Context, opID string, callerLease string, ref adapter.RecoveryRef, outcome adapter.ReconciliationOutcome) (OperationReceipt, error) {
	sessID := string(ref.SessionID)
	fp := computeFingerprint("reconcile_session", sessID, ref.TurnKey, fmt.Sprintf("%d", ref.Generation), string(outcome.Status), string(outcome.Observed), outcome.Result)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "reconcile_session", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease, state string
	var activeKey sql.NullString
	var recGen uint64
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.state, s.active_key, s.recovery_gen
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessID).Scan(&runID, &runLease, &currentVer, &state, &activeKey, &recGen)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session for reconcile: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if !activeKey.Valid || activeKey.String != ref.TurnKey {
		return OperationReceipt{}, fmt.Errorf("turn key mismatch: active turn is %q, reconciliation ref requested %q", activeKey.String, ref.TurnKey)
	}
	if recGen != ref.Generation {
		return OperationReceipt{}, fmt.Errorf("stale recovery generation: session is at %d, reconciliation ref is %d", recGen, ref.Generation)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	newVer := currentVer + 1

	// Map council.TurnStatus to schema turn status
	turnStatus := "completed"
	switch outcome.Observed {
	case council.TurnCancelled:
		turnStatus = "cancelled"
	case council.TurnFailed:
		turnStatus = "failed"
	case council.TurnInterrupted:
		turnStatus = "interrupted"
	case council.TurnCompleted:
		turnStatus = "completed"
	default:
		turnStatus = "completed"
	}

	// Update turn
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE turns SET status = ?, result = ?, completed_at = ?
WHERE session_id = ? AND turn_key = ?;`, turnStatus, outcome.Result, now, sessID, ref.TurnKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("update turn terminal status: %w", err)
	}

	// Update dispatch intent to resolved
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE dispatch_intents SET phase = 'resolved', updated_at = ?
WHERE session_id = ? AND turn_key = ?;`, now, sessID, ref.TurnKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("resolve dispatch intent: %w", err)
	}

	// Update session: active_key = NULL, state = 'parked', visibility = 'reachable'
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET active_key = NULL, state = 'parked', visibility = 'reachable', row_version = ?, updated_at = ?
WHERE session_id = ?;`, newVer, now, sessID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("park session on reconciliation: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "reconcile_session",
		SessionID:        sessID,
		TurnKey:          ref.TurnKey,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          turnStatus,
	}

	if err := recordJournalEntry(tx.Tx(), opID, "reconcile_session", fp, runID, sessID, ref.TurnKey, "session_reconciled", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) RecordDispatchObservation(ctx context.Context, opID string, callerLease string, sessionID string, turnKey string, phase string) (OperationReceipt, error) {
	fp := computeFingerprint("record_dispatch_observation", sessionID, turnKey, phase)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "record_dispatch_observation", fp); err != nil {
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

	// Check current intent phase
	var currentPhase string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT phase FROM dispatch_intents WHERE session_id = ? AND turn_key = ?;`, sessionID, turnKey).Scan(&currentPhase)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query dispatch intent: %w", err)
	}

	// Late arrival rule: if intent is already resolved, observation is ignored as clean no-op
	if currentPhase == "resolved" {
		receipt := OperationReceipt{
			OpID:             opID,
			CommandType:      "record_dispatch_observation",
			SessionID:        sessionID,
			TurnKey:          turnKey,
			CommittedVersion: currentVer,
			CreatedAt:        time.Now().UTC(),
			Payload:          "late_observation_ignored",
		}
		return receipt, nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE dispatch_intents SET phase = ?, updated_at = ? WHERE session_id = ? AND turn_key = ?;`, phase, now, sessionID, turnKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("update dispatch intent phase: %w", err)
	}

	newVer := currentVer + 1
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET row_version = ?, updated_at = ? WHERE session_id = ?;`, newVer, now, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("bump session version: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "record_dispatch_observation",
		SessionID:        sessionID,
		TurnKey:          turnKey,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          phase,
	}

	if err := recordJournalEntry(tx.Tx(), opID, "record_dispatch_observation", fp, runID, sessionID, turnKey, "dispatch_observed", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
