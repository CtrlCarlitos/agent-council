package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

func validateToolingConfig(configStr string) error {
	trimmed := strings.TrimSpace(configStr)
	if trimmed == "" || trimmed == "{}" {
		return nil
	}
	if containsDisallowedCredential(trimmed) {
		return ErrDisallowedToolingConfig
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		return nil // Non-JSON string without credentials is accepted
	}
	allowedKeys := map[string]bool{
		"workspace_root":  true,
		"model":           true,
		"tooling":         true,
		"tools":           true,
		"timeout":         true,
		"env_allowlist":   true,
		"read_only":       true,
		"permission_mode": true,
		"log_level":       true,
		"max_iterations":  true,
	}
	for k, v := range m {
		if !allowedKeys[k] {
			return fmt.Errorf("%w: disallowed configuration key %q", ErrDisallowedToolingConfig, k)
		}
		if s, ok := v.(string); ok && containsDisallowedCredential(s) {
			return fmt.Errorf("%w: sensitive credential pattern detected in %q", ErrDisallowedToolingConfig, k)
		}
	}
	return nil
}

func (s *Store) SetNativeBinding(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, binding NativeBinding) (OperationReceipt, error) {
	if expectedVersion <= 0 {
		return OperationReceipt{}, ErrInvalidExpectedVersion
	}
	if err := validateToolingConfig(binding.ToolingConfig); err != nil {
		return OperationReceipt{}, err
	}

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

	var runID, runLease, lifecycle string
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.lifecycle 
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &lifecycle)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO native_bindings (session_id, native_session_id, harness, model, workspace_mode, config_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET
    native_session_id = excluded.native_session_id,
    harness = excluded.harness,
    model = excluded.model,
    workspace_mode = excluded.workspace_mode,
    config_json = excluded.config_json,
    updated_at = excluded.updated_at;`, sessionID, binding.NativeSessionID, binding.Harness, binding.Model, binding.WorkspaceMode, binding.ToolingConfig, now, now)
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
	if expectedVersion <= 0 {
		return ReleaseReceipt{}, ErrInvalidExpectedVersion
	}
	if strings.TrimSpace(turnKey) == "" {
		return ReleaseReceipt{}, errors.New("empty turn key")
	}

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

	var runID, runLease, state, lifecycle, controllerStatus, visibility string
	var activeKey sql.NullString
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.state, s.lifecycle, s.controller_status, s.visibility, s.active_key
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &state, &lifecycle, &controllerStatus, &visibility, &activeKey)
	if err != nil {
		return ReleaseReceipt{}, fmt.Errorf("query session for release: %w", err)
	}

	if runLease != callerLease {
		return ReleaseReceipt{}, ErrUnauthorizedOperation
	}
	if currentVer != expectedVersion {
		return ReleaseReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return ReleaseReceipt{}, ErrSessionArchived
	}
	if controllerStatus == "disconnected" {
		return ReleaseReceipt{}, ErrControllerDisconnected
	}
	if visibility == "host_lost" {
		return ReleaseReceipt{}, ErrHostLost
	}
	if state != "parked" || (activeKey.Valid && activeKey.String != "") {
		return ReleaseReceipt{}, fmt.Errorf("session %s is not parked or already has active turn %v", sessionID, activeKey)
	}

	// Fetch EXACT pending prompt for turnKey
	var rawPrompt string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT prompt FROM pending_prompts WHERE session_id = ? AND turn_key = ?;`, sessionID, turnKey).Scan(&rawPrompt)
	if err == sql.ErrNoRows {
		return ReleaseReceipt{}, ErrPromptNotQueued
	}
	if err != nil {
		return ReleaseReceipt{}, fmt.Errorf("query pending prompt for session %s, turn %s: %w", sessionID, turnKey, err)
	}

	// Delete ONLY this pending prompt
	_, err = tx.Tx().ExecContext(ctx, `DELETE FROM pending_prompts WHERE session_id = ? AND turn_key = ?;`, sessionID, turnKey)
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
	if err := outcome.Validate(); err != nil {
		return OperationReceipt{}, fmt.Errorf("invalid reconciliation outcome: %w", err)
	}
	if err := ref.Validate(); err != nil {
		return OperationReceipt{}, fmt.Errorf("invalid recovery ref: %w", err)
	}
	if outcome.Ref != ref {
		return OperationReceipt{}, fmt.Errorf("outcome ref %+v does not match request ref %+v", outcome.Ref, ref)
	}

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

	var runID, runLease, state, visibility, recoveryContext string
	var activeKey sql.NullString
	var activeRecGen uint64
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.state, s.visibility, s.active_key, s.active_recovery_gen, coalesce(s.recovery_context, '')
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessID).Scan(&runID, &runLease, &currentVer, &state, &visibility, &activeKey, &activeRecGen, &recoveryContext)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session for reconcile: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if visibility != "host_lost" {
		return OperationReceipt{}, errors.New("session host is not lost")
	}
	if ref.Generation == 0 || activeRecGen != ref.Generation {
		return OperationReceipt{}, fmt.Errorf("stale or invalid recovery generation: active is %d, ref is %d", activeRecGen, ref.Generation)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	newVer := currentVer + 1
	sanitizedResult := SanitizeText(outcome.Result)

	var payload string

	// Case 1: Active turn exists
	if activeKey.Valid && activeKey.String != "" {
		if activeKey.String != ref.TurnKey {
			return OperationReceipt{}, fmt.Errorf("turn key mismatch: active turn is %q, reconciliation ref requested %q", activeKey.String, ref.TurnKey)
		}

		switch outcome.Status {
		case adapter.ReconciliationReachableActive:
			// Turn remains active! Preserve reservation and running state.
			// Restore visibility to reachable, clear active recovery generation.
			_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET visibility = 'reachable', active_recovery_gen = 0, recovery_context = '', row_version = ?, updated_at = ?
WHERE session_id = ?;`, newVer, now, sessID)
			if err != nil {
				return OperationReceipt{}, fmt.Errorf("update session for reachable active: %w", err)
			}
			payload = "reachable_active"

		case adapter.ReconciliationUncertain:
			// Uncertainty preserved: visibility remains host_lost, active recovery episode remains open.
			_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET row_version = ?, updated_at = ?
WHERE session_id = ?;`, newVer, now, sessID)
			if err != nil {
				return OperationReceipt{}, fmt.Errorf("update session for uncertain reconciliation: %w", err)
			}
			payload = "uncertain"

		case adapter.ReconciliationReachableTerminal, adapter.ReconciliationDefinitivelyMissing:
			turnStatus := "failed"
			if outcome.Status == adapter.ReconciliationReachableTerminal {
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
					return OperationReceipt{}, fmt.Errorf("invalid terminal observed status: %s", outcome.Observed)
				}
			}

			// Update turn
			_, err = tx.Tx().ExecContext(ctx, `
UPDATE turns SET status = ?, result = ?, completed_at = ?
WHERE session_id = ? AND turn_key = ?;`, turnStatus, sanitizedResult, now, sessID, ref.TurnKey)
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

			// Update session: park, active_key = NULL, visibility = reachable, clear active recovery episode
			_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET active_key = NULL, state = 'parked', visibility = 'reachable', active_recovery_gen = 0, recovery_context = '', row_version = ?, updated_at = ?
WHERE session_id = ?;`, newVer, now, sessID)
			if err != nil {
				return OperationReceipt{}, fmt.Errorf("park session on terminal reconciliation: %w", err)
			}
			payload = turnStatus

		default:
			return OperationReceipt{}, fmt.Errorf("unknown reconciliation status: %s", outcome.Status)
		}
	} else {
		// Case 2: Post-terminal recovery (active turn was already cleared by terminal delivery while host was lost)
		if recoveryContext != ref.TurnKey {
			return OperationReceipt{}, fmt.Errorf("reconciliation key %q does not match recovery context %q", ref.TurnKey, recoveryContext)
		}

		switch outcome.Status {
		case adapter.ReconciliationReachableTerminal, adapter.ReconciliationDefinitivelyMissing:
			// Close recovery episode
			_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET visibility = 'reachable', active_recovery_gen = 0, recovery_context = '', row_version = ?, updated_at = ?
WHERE session_id = ?;`, newVer, now, sessID)
			if err != nil {
				return OperationReceipt{}, fmt.Errorf("close recovery episode for post-terminal: %w", err)
			}
			payload = "reconciled_post_terminal"

		case adapter.ReconciliationReachableActive:
			return OperationReceipt{}, errors.New("reconciliation status conflicts with recorded terminal outcome")

		case adapter.ReconciliationUncertain:
			// Uncertainty preserved
			_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET row_version = ?, updated_at = ?
WHERE session_id = ?;`, newVer, now, sessID)
			if err != nil {
				return OperationReceipt{}, fmt.Errorf("update session for uncertain post-terminal: %w", err)
			}
			payload = "uncertain"

		default:
			return OperationReceipt{}, fmt.Errorf("unknown reconciliation status: %s", outcome.Status)
		}
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "reconcile_session",
		SessionID:        sessID,
		TurnKey:          ref.TurnKey,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          payload,
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

func (s *Store) RequestCancel(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, turnKey string) (OperationReceipt, error) {
	if expectedVersion <= 0 {
		return OperationReceipt{}, ErrInvalidExpectedVersion
	}
	if strings.TrimSpace(turnKey) == "" {
		return OperationReceipt{}, errors.New("empty turn key")
	}

	fp := computeFingerprint("request_cancel", sessionID, turnKey)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "request_cancel", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease, lifecycle, controllerStatus, state string
	var activeKey sql.NullString
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.lifecycle, s.controller_status, s.state, s.active_key
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &lifecycle, &controllerStatus, &state, &activeKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}
	if controllerStatus == "disconnected" {
		return OperationReceipt{}, ErrControllerDisconnected
	}
	if state != "running" || !activeKey.Valid || activeKey.String != turnKey {
		return OperationReceipt{}, errors.New("no active running turn to cancel")
	}

	var turnStatus string
	err = tx.Tx().QueryRowContext(ctx, "SELECT status FROM turns WHERE session_id = ? AND turn_key = ?;", sessionID, turnKey).Scan(&turnStatus)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query turn status: %w", err)
	}
	if turnStatus != "running" {
		return OperationReceipt{}, fmt.Errorf("turn %s is in status %s, cannot cancel", turnKey, turnStatus)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Tx().ExecContext(ctx, `UPDATE turns SET status = 'cancelling' WHERE session_id = ? AND turn_key = ?;`, sessionID, turnKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("update turn cancelling: %w", err)
	}

	newVer := currentVer + 1
	_, err = tx.Tx().ExecContext(ctx, `UPDATE sessions SET row_version = ?, updated_at = ? WHERE session_id = ?;`, newVer, now, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("bump session version: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "request_cancel",
		SessionID:        sessionID,
		TurnKey:          turnKey,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          "cancelling",
	}

	if err := recordJournalEntry(tx.Tx(), opID, "request_cancel", fp, runID, sessionID, turnKey, "cancel_requested", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) RecordTerminalOutcome(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, turnKey string, terminalStatus council.TurnStatus, rawResult string) (OperationReceipt, error) {
	if expectedVersion <= 0 {
		return OperationReceipt{}, ErrInvalidExpectedVersion
	}
	if strings.TrimSpace(turnKey) == "" {
		return OperationReceipt{}, errors.New("empty turn key")
	}

	var statusStr string
	switch terminalStatus {
	case council.TurnCompleted:
		statusStr = "completed"
	case council.TurnCancelled:
		statusStr = "cancelled"
	case council.TurnFailed:
		statusStr = "failed"
	case council.TurnInterrupted:
		statusStr = "interrupted"
	default:
		return OperationReceipt{}, fmt.Errorf("invalid terminal status: %s", terminalStatus)
	}

	sanitizedResult := SanitizeText(rawResult)
	fp := computeFingerprint("record_terminal_outcome", sessionID, turnKey, statusStr, sanitizedResult)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "record_terminal_outcome", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease, lifecycle, visibility, state string
	var activeKey sql.NullString
	var recoveryContext sql.NullString
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.lifecycle, s.visibility, s.state, s.active_key, s.recovery_context
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &lifecycle, &visibility, &state, &activeKey, &recoveryContext)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}

	if visibility == "host_lost" {
		// Can match either active_key or recovery_context in host_lost state
		if (!activeKey.Valid || activeKey.String != turnKey) && (!recoveryContext.Valid || recoveryContext.String != turnKey) {
			return OperationReceipt{}, fmt.Errorf("terminal outcome does not match active turn or recovery context in lost host: %s", turnKey)
		}
	} else {
		if state != "running" || !activeKey.Valid || activeKey.String != turnKey {
			return OperationReceipt{}, fmt.Errorf("terminal outcome does not match active turn %v", activeKey)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Update turn
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE turns SET status = ?, result = ?, completed_at = ?
WHERE session_id = ? AND turn_key = ?;`, statusStr, sanitizedResult, now, sessionID, turnKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("update turn terminal: %w", err)
	}

	// Update dispatch intent to resolved
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE dispatch_intents SET phase = 'resolved', updated_at = ?
WHERE session_id = ? AND turn_key = ?;`, now, sessionID, turnKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("resolve dispatch intent: %w", err)
	}

	newVer := currentVer + 1

	if visibility == "host_lost" {
		// Park contributor but retain recovery_context and active_recovery_gen
		_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET active_key = NULL, state = 'parked', recovery_context = ?, row_version = ?, updated_at = ?
WHERE session_id = ?;`, turnKey, newVer, now, sessionID)
		if err != nil {
			return OperationReceipt{}, fmt.Errorf("park session retaining recovery context: %w", err)
		}
	} else {
		_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET active_key = NULL, state = 'parked', row_version = ?, updated_at = ?
WHERE session_id = ?;`, newVer, now, sessionID)
		if err != nil {
			return OperationReceipt{}, fmt.Errorf("park session on terminal outcome: %w", err)
		}
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "record_terminal_outcome",
		SessionID:        sessionID,
		TurnKey:          turnKey,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          statusStr,
	}

	eventKind := "turn_" + statusStr
	if err := recordJournalEntry(tx.Tx(), opID, "record_terminal_outcome", fp, runID, sessionID, turnKey, eventKind, receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) RecordHostLoss(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64) (OperationReceipt, error) {
	if expectedVersion <= 0 {
		return OperationReceipt{}, ErrInvalidExpectedVersion
	}

	fp := computeFingerprint("record_host_loss", sessionID)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "record_host_loss", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease, lifecycle, visibility, state string
	var activeKey sql.NullString
	var recGen, activeRecGen uint64
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.lifecycle, s.visibility, s.state, s.active_key, s.recovery_gen, s.active_recovery_gen
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &lifecycle, &visibility, &state, &activeKey, &recGen, &activeRecGen)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}

	// Idempotent check: if already host lost, return receipt
	if visibility == "host_lost" {
		receipt := OperationReceipt{
			OpID:             opID,
			CommandType:      "record_host_loss",
			SessionID:        sessionID,
			TurnKey:          activeKey.String,
			CommittedVersion: currentVer,
			CreatedAt:        time.Now().UTC(),
			Payload:          fmt.Sprintf("%d", activeRecGen),
		}
		return receipt, nil
	}

	if state != "running" || !activeKey.Valid || activeKey.String == "" {
		return OperationReceipt{}, errors.New("cannot record host loss on inactive session")
	}

	newGen := recGen + 1
	newVer := currentVer + 1
	now := time.Now().UTC().Format(time.RFC3339Nano)

	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET recovery_gen = ?, active_recovery_gen = ?, visibility = 'host_lost', recovery_context = ?, row_version = ?, updated_at = ?
WHERE session_id = ?;`, newGen, newGen, activeKey.String, newVer, now, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("update session for host loss: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "record_host_loss",
		SessionID:        sessionID,
		TurnKey:          activeKey.String,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          fmt.Sprintf("%d", newGen),
	}

	if err := recordJournalEntry(tx.Tx(), opID, "record_host_loss", fp, runID, sessionID, activeKey.String, "host_loss_recorded", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) SetControllerConnection(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, status council.ControllerConnection) (OperationReceipt, error) {
	if expectedVersion <= 0 {
		return OperationReceipt{}, ErrInvalidExpectedVersion
	}
	if status != council.ControllerConnected && status != council.ControllerDisconnected {
		return OperationReceipt{}, fmt.Errorf("invalid controller connection status: %s", status)
	}

	fp := computeFingerprint("set_controller_connection", sessionID, string(status))

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "set_controller_connection", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease, lifecycle string
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.lifecycle
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &lifecycle)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	newVer := currentVer + 1

	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET controller_status = ?, row_version = ?, updated_at = ?
WHERE session_id = ?;`, string(status), newVer, now, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("update controller connection: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "set_controller_connection",
		SessionID:        sessionID,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          string(status),
	}

	if err := recordJournalEntry(tx.Tx(), opID, "set_controller_connection", fp, runID, sessionID, "", "connection_updated", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) ArchiveSession(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64) (OperationReceipt, error) {
	if expectedVersion <= 0 {
		return OperationReceipt{}, ErrInvalidExpectedVersion
	}

	fp := computeFingerprint("archive_session", sessionID)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "archive_session", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease, state, visibility, lifecycle string
	var activeKey sql.NullString
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.state, s.visibility, s.lifecycle, s.active_key
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &state, &visibility, &lifecycle, &activeKey)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}

	if runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return OperationReceipt{
			OpID:             opID,
			CommandType:      "archive_session",
			SessionID:        sessionID,
			CommittedVersion: currentVer,
			CreatedAt:        time.Now().UTC(),
		}, nil
	}

	if state == "running" || (activeKey.Valid && activeKey.String != "") || visibility == "host_lost" {
		return OperationReceipt{}, errors.New("cannot archive a running turn or lost host")
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	newVer := currentVer + 1

	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET lifecycle = 'archived', state = 'archived', row_version = ?, updated_at = ?
WHERE session_id = ?;`, newVer, now, sessionID)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("archive session: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "archive_session",
		SessionID:        sessionID,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
	}

	if err := recordJournalEntry(tx.Tx(), opID, "archive_session", fp, runID, sessionID, "", "session_archived", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func (s *Store) RecordDecision(ctx context.Context, opID string, callerLease string, runID string, artifactID string, revision int64, decisionPayload string) (OperationReceipt, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(artifactID) == "" || revision <= 0 {
		return OperationReceipt{}, errors.New("invalid decision parameters")
	}

	fp := computeFingerprint("record_decision", runID, artifactID, fmt.Sprintf("%d", revision), decisionPayload)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "record_decision", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runLease string
	err = tx.Tx().QueryRowContext(ctx, "SELECT controller_lease FROM runs WHERE run_id = ?;", runID).Scan(&runLease)
	if err != nil || runLease != callerLease {
		return OperationReceipt{}, ErrUnauthorizedOperation
	}

	// Verify that referenced (artifact_id, revision) exists in artifact_revisions for this run
	var count int
	err = tx.Tx().QueryRowContext(ctx, `
SELECT count(*) FROM artifact_revisions
WHERE artifact_id = ? AND revision = ? AND run_id = ?;`, artifactID, revision, runID).Scan(&count)
	if err != nil || count == 0 {
		return OperationReceipt{}, ErrArtifactNotFound
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "record_decision",
		CommittedVersion: revision,
		CreatedAt:        time.Now().UTC(),
		Payload:          decisionPayload,
	}

	if err := recordJournalEntry(tx.Tx(), opID, "record_decision", fp, runID, "", "", "controller_decision", receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
