package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

type decisionJournalPayload struct {
	CallerLease string           `json:"caller_lease"`
	Receipt     OperationReceipt `json:"receipt"`
	Decision    DecisionRecord   `json:"decision"`
}

var safeProfileIdentifierRegex = regexp.MustCompile(`^[a-zA-Z0-9_\-\.]+$`)
var envAssignmentRegex = regexp.MustCompile(`(?i)[a-z0-9_]*(key|token|secret|password|bearer|auth)[a-z0-9_]*\s*=`)
var envVarNameRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func isSensitiveKey(k string) bool {
	lower := strings.ToLower(k)
	sensitiveTerms := []string{
		"key", "token", "secret", "password", "passwd", "bearer", "auth", "credential", "api_key", "privkey",
	}
	for _, term := range sensitiveTerms {
		if strings.Contains(lower, term) {
			return true
		}
	}
	return false
}

func validateConfigValue(v any) error {
	switch val := v.(type) {
	case string:
		if containsDisallowedCredential(val) {
			return fmt.Errorf("%w: sensitive credential pattern detected", ErrDisallowedToolingConfig)
		}
		if envAssignmentRegex.MatchString(val) {
			return fmt.Errorf("%w: suspicious credential assignment detected", ErrDisallowedToolingConfig)
		}
	case []any:
		for _, elem := range val {
			if err := validateConfigValue(elem); err != nil {
				return err
			}
		}
	case map[string]any:
		for k, elem := range val {
			if isSensitiveKey(k) || containsDisallowedCredential(k) || envAssignmentRegex.MatchString(k) {
				return fmt.Errorf("%w: sensitive key pattern detected %q", ErrDisallowedToolingConfig, k)
			}
			if err := validateConfigValue(elem); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateToolingConfig(configStr string) error {
	trimmed := strings.TrimSpace(configStr)
	if trimmed == "" || trimmed == "{}" {
		return nil
	}
	if containsDisallowedCredential(trimmed) {
		return ErrDisallowedToolingConfig
	}
	if envAssignmentRegex.MatchString(trimmed) {
		return fmt.Errorf("%w: credential assignment pattern in config", ErrDisallowedToolingConfig)
	}

	var m map[string]any
	if err := json.Unmarshal([]byte(trimmed), &m); err != nil {
		if !safeProfileIdentifierRegex.MatchString(trimmed) {
			return fmt.Errorf("%w: configuration must be valid JSON object or safe identifier", ErrDisallowedToolingConfig)
		}
		return nil
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
		switch k {
		case "workspace_root", "model", "permission_mode", "log_level":
			if _, ok := v.(string); !ok {
				return fmt.Errorf("%w: configuration field %q must be a string", ErrDisallowedToolingConfig, k)
			}
		case "timeout", "max_iterations":
			if _, ok := v.(float64); !ok {
				return fmt.Errorf("%w: configuration field %q must be a number", ErrDisallowedToolingConfig, k)
			}
		case "read_only":
			if _, ok := v.(bool); !ok {
				return fmt.Errorf("%w: configuration field %q must be a boolean", ErrDisallowedToolingConfig, k)
			}
		case "env_allowlist":
			arr, ok := v.([]any)
			if !ok {
				return fmt.Errorf("%w: env_allowlist must be an array of variable names, not a map or scalar", ErrDisallowedToolingConfig)
			}
			for _, item := range arr {
				str, isStr := item.(string)
				if !isStr || !envVarNameRegex.MatchString(str) {
					return fmt.Errorf("%w: invalid environment variable name %v in env_allowlist", ErrDisallowedToolingConfig, item)
				}
				if isSensitiveKey(str) {
					return fmt.Errorf("%w: sensitive variable %q cannot be in env_allowlist", ErrDisallowedToolingConfig, str)
				}
			}
		case "tooling", "tools":
			switch tv := v.(type) {
			case string:
				if !safeProfileIdentifierRegex.MatchString(tv) {
					return fmt.Errorf("%w: tooling profile string must be safe identifier", ErrDisallowedToolingConfig)
				}
			case []any, map[string]any:
				// Valid shapes, validated recursively below
			default:
				return fmt.Errorf("%w: field %q has invalid shape", ErrDisallowedToolingConfig, k)
			}
		}

		if err := validateConfigValue(v); err != nil {
			return err
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

	// Current controller authority precedes idempotent replay (AC-004).
	if _, err := authorizeSessionController(ctx, tx.Tx(), sessionID, callerLease, false); err != nil {
		return OperationReceipt{}, err
	}

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

	_ = runLease // authority classified before replay
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

// FindCommittedRelease checks if opID has already been committed as a release_turn operation.
func (s *Store) FindCommittedRelease(ctx context.Context, opID string, callerLease string, sessionID string, turnKey string) (*ReleaseReceipt, bool, error) {
	// Current controller authority precedes receipt replay (AC-004).
	if _, err := authorizeSessionController(ctx, s.readDB, sessionID, callerLease, true); err != nil {
		return nil, false, err
	}
	fp := computeFingerprint("release_turn", sessionID, turnKey)
	var storedCmdType, storedFingerprint, payloadJSON string
	err := s.readDB.QueryRowContext(ctx, "SELECT command_type, command_fingerprint, payload_json FROM journal_entries WHERE op_id = ?;", opID).Scan(&storedCmdType, &storedFingerprint, &payloadJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var jp releaseJournalPayload
	if err := json.Unmarshal([]byte(payloadJSON), &jp); err == nil && jp.Receipt.TurnKey != "" {
		if jp.Receipt.OperationReceipt.TurnKey == "" {
			jp.Receipt.OperationReceipt.TurnKey = jp.Receipt.TurnKey
		}
		// Authority was classified at the boundary before this lookup; the
		// recorded caller_lease may belong to a superseded controller whose
		// committed response the current controller recovers.
		if storedCmdType != "release_turn" || storedFingerprint != fp {
			return nil, false, ErrIdempotencyConflict
		}
		return &jp.Receipt, true, nil
	}
	return nil, false, nil
}

// GetSessionVersion returns the current row_version of the given session.
func (s *Store) GetSessionVersion(ctx context.Context, sessionID string) (int64, error) {
	var version int64
	err := s.readDB.QueryRowContext(ctx, "SELECT row_version FROM sessions WHERE session_id = ?;", sessionID).Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, nil
}

// GetTurnPrompt returns the prompt text of the given turn.
func (s *Store) GetTurnPrompt(ctx context.Context, sessionID, turnKey string) (string, error) {
	var prompt string
	err := s.readDB.QueryRowContext(ctx, "SELECT prompt FROM turns WHERE session_id = ? AND turn_key = ?;", sessionID, turnKey).Scan(&prompt)
	if err != nil {
		return "", err
	}
	return prompt, nil
}

// FindOperationReceipt checks if opID exists in journal_entries and validates callerLease if non-empty.
func (s *Store) FindOperationReceipt(ctx context.Context, opID string, callerLease string) (*OperationReceipt, bool, error) {
	var storedCmdType, storedFingerprint, payloadJSON string
	var runID string
	err := s.readDB.QueryRowContext(ctx, "SELECT command_type, command_fingerprint, payload_json, run_id FROM journal_entries WHERE op_id = ?;", opID).Scan(&storedCmdType, &storedFingerprint, &payloadJSON, &runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	// Current controller authority precedes receipt replay (AC-004); the
	// journal row's run scopes the classification.
	if _, err := classifyCredential(ctx, s.readDB, runID, callerLease, true); err != nil {
		return nil, false, err
	}
	var jp journalPayload
	if err := json.Unmarshal([]byte(payloadJSON), &jp); err == nil && jp.Receipt.OpID != "" {
		// Authority was classified before this lookup; the recorded
		// caller_lease may belong to a superseded controller whose
		// committed response the current controller recovers.
		_ = callerLease
		return &jp.Receipt, true, nil
	}
	return nil, false, nil
}

// SessionRecoveryState holds reachability and recovery status for a session.
type SessionRecoveryState struct {
	Visibility        string
	ActiveRecoveryGen uint64
	RecoveryContext   string
	RowVersion        int64
	State             string
	ActiveKey         string
}

// GetSessionRecoveryState reads current visibility and recovery generation.
func (s *Store) GetSessionRecoveryState(ctx context.Context, sessionID string) (SessionRecoveryState, error) {
	var st SessionRecoveryState
	var activeKey sql.NullString
	err := s.readDB.QueryRowContext(ctx, "SELECT visibility, active_recovery_gen, row_version, state, active_key, coalesce(recovery_context, '') FROM sessions WHERE session_id = ?;", sessionID).Scan(&st.Visibility, &st.ActiveRecoveryGen, &st.RowVersion, &st.State, &activeKey, &st.RecoveryContext)
	if err != nil {
		return SessionRecoveryState{}, err
	}
	if activeKey.Valid {
		st.ActiveKey = activeKey.String
	}
	return st, nil
}

// GetSessionNativeBinding returns the saved native binding for a session
// together with the session's contributor identity, for explicit recovery
// attach. found is false when no binding has been saved.
func (s *Store) GetSessionNativeBinding(ctx context.Context, sessionID string) (NativeBinding, string, bool, error) {
	var b NativeBinding
	var contributor string
	err := s.readDB.QueryRowContext(ctx, `
SELECT b.session_id, b.native_session_id, b.harness, b.model, b.workspace_mode, b.config_json, s.contributor
FROM native_bindings b
JOIN sessions s ON s.session_id = b.session_id
WHERE b.session_id = ?;`, sessionID).Scan(&b.LogicalSessionID, &b.NativeSessionID, &b.Harness, &b.Model, &b.WorkspaceMode, &b.ToolingConfig, &contributor)
	if err == sql.ErrNoRows {
		return NativeBinding{}, "", false, nil
	}
	if err != nil {
		return NativeBinding{}, "", false, err
	}
	return b, contributor, true, nil
}

// TurnDetails provides full authoritative status and state for a turn.
type TurnDetails struct {
	SessionID      string                 `json:"session_id"`
	TurnKey        string                 `json:"turn_key"`
	Prompt         string                 `json:"prompt"`
	Status         council.TurnStatus     `json:"status"`
	Result         string                 `json:"result"`
	AttemptID      string                 `json:"attempt_id"`
	CreatedAt      time.Time              `json:"created_at"`
	CompletedAt    *time.Time             `json:"completed_at,omitempty"`
	DispatchIntent *DispatchIntentDetails `json:"dispatch_intent,omitempty"`
}

// DispatchIntentDetails captures intent tracking for an attempt.
type DispatchIntentDetails struct {
	SessionID  string    `json:"session_id"`
	TurnKey    string    `json:"turn_key"`
	AttemptID  string    `json:"attempt_id"`
	Phase      string    `json:"phase"`
	RecordedAt time.Time `json:"recorded_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// GetTurnDetails returns authoritative turn state including dispatch intent.
func (s *Store) GetTurnDetails(ctx context.Context, sessionID, turnKey string) (*TurnDetails, error) {
	var td TurnDetails
	var completedAt sql.NullString
	var createdAtStr string
	err := s.readDB.QueryRowContext(ctx, `
SELECT session_id, turn_key, prompt, status, result, attempt_id, created_at, completed_at
FROM turns
WHERE session_id = ? AND turn_key = ?;`, sessionID, turnKey).Scan(&td.SessionID, &td.TurnKey, &td.Prompt, &td.Status, &td.Result, &td.AttemptID, &createdAtStr, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query turn: %w", err)
	}

	if t, err := time.Parse(time.RFC3339Nano, createdAtStr); err == nil {
		td.CreatedAt = t
	} else if t, err := time.Parse(time.RFC3339, createdAtStr); err == nil {
		td.CreatedAt = t
	}

	if completedAt.Valid && completedAt.String != "" {
		if t, err := time.Parse(time.RFC3339Nano, completedAt.String); err == nil {
			td.CompletedAt = &t
		} else if t, err := time.Parse(time.RFC3339, completedAt.String); err == nil {
			td.CompletedAt = &t
		}
	}

	var di DispatchIntentDetails
	var diRecStr, diUpdStr string
	err = s.readDB.QueryRowContext(ctx, `
SELECT session_id, turn_key, attempt_id, phase, recorded_at, updated_at
FROM dispatch_intents
WHERE session_id = ? AND turn_key = ?;`, sessionID, turnKey).Scan(&di.SessionID, &di.TurnKey, &di.AttemptID, &di.Phase, &diRecStr, &diUpdStr)
	if err == nil {
		if t, err := time.Parse(time.RFC3339Nano, diRecStr); err == nil {
			di.RecordedAt = t
		}
		if t, err := time.Parse(time.RFC3339Nano, diUpdStr); err == nil {
			di.UpdatedAt = t
		}
		td.DispatchIntent = &di
	}

	return &td, nil
}

func (s *Store) ReleaseTurn(ctx context.Context, opID string, callerLease string, sessionID string, expectedVersion int64, turnKey string) (ReleaseResult, error) {
	if expectedVersion <= 0 {
		return ReleaseResult{}, ErrInvalidExpectedVersion
	}
	if strings.TrimSpace(turnKey) == "" {
		return ReleaseResult{}, errors.New("empty turn key")
	}

	fp := computeFingerprint("release_turn", sessionID, turnKey)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return ReleaseResult{}, err
	}
	defer tx.Rollback()

	// Current controller authority precedes idempotent replay (AC-004).
	issuingGeneration, err := authorizeSessionController(ctx, tx.Tx(), sessionID, callerLease, true)
	if err != nil {
		return ReleaseResult{}, err
	}
	// Idempotency check for release_turn
	var storedCmdType, storedFingerprint, payloadJSON string
	err = tx.Tx().QueryRowContext(ctx, "SELECT command_type, command_fingerprint, payload_json FROM journal_entries WHERE op_id = ?;", opID).Scan(&storedCmdType, &storedFingerprint, &payloadJSON)
	if err == nil {
		var jp releaseJournalPayload
		if err := json.Unmarshal([]byte(payloadJSON), &jp); err == nil && jp.Receipt.TurnKey != "" {
			if jp.Receipt.OperationReceipt.TurnKey == "" {
				jp.Receipt.OperationReceipt.TurnKey = jp.Receipt.TurnKey
			}
			// Authority was classified before this transaction; the recorded
			// caller_lease may belong to a superseded controller whose
			// committed release the current controller recovers.
			if storedCmdType != "release_turn" || storedFingerprint != fp {
				return ReleaseResult{}, ErrIdempotencyConflict
			}
			return ReleaseResult{
				Receipt:     jp.Receipt,
				Disposition: ReleaseDispositionReplayed,
			}, nil
		}
	}

	// New decisions enforce the run connection precondition at this write
	// boundary; the replay above follows the disconnected-read policy.
	if err := requireRunConnected(ctx, tx.Tx(), runIDOfSession(ctx, tx.Tx(), sessionID)); err != nil {
		return ReleaseResult{}, err
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
		return ReleaseResult{}, fmt.Errorf("query session for release: %w", err)
	}

	_ = runLease // authority classified before replay
	if currentVer != expectedVersion {
		return ReleaseResult{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return ReleaseResult{}, ErrSessionArchived
	}
	if controllerStatus == "disconnected" {
		return ReleaseResult{}, ErrControllerDisconnected
	}
	if visibility == "host_lost" {
		return ReleaseResult{}, ErrHostLost
	}
	if state != "parked" || (activeKey.Valid && activeKey.String != "") {
		return ReleaseResult{}, fmt.Errorf("session %s is not parked or already has active turn %v", sessionID, activeKey)
	}

	// Fetch EXACT pending prompt for turnKey
	var rawPrompt string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT prompt FROM pending_prompts WHERE session_id = ? AND turn_key = ?;`, sessionID, turnKey).Scan(&rawPrompt)
	if err == sql.ErrNoRows {
		return ReleaseResult{}, ErrPromptNotQueued
	}
	if err != nil {
		return ReleaseResult{}, fmt.Errorf("query pending prompt for session %s, turn %s: %w", sessionID, turnKey, err)
	}

	// Delete ONLY this pending prompt
	_, err = tx.Tx().ExecContext(ctx, `DELETE FROM pending_prompts WHERE session_id = ? AND turn_key = ?;`, sessionID, turnKey)
	if err != nil {
		return ReleaseResult{}, fmt.Errorf("delete pending prompt: %w", err)
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
		return ReleaseResult{}, fmt.Errorf("insert turn: %w", err)
	}

	// Insert into dispatch_intents, stamped with the issuing controller
	// generation that authorized this execution (AC-004 §6).
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO dispatch_intents (session_id, turn_key, attempt_id, phase, issuing_controller_generation, recorded_at, updated_at)
VALUES (?, ?, ?, 'intent_recorded', ?, ?, ?);`, sessionID, turnKey, attemptID, issuingGeneration, now, now)
	if err != nil {
		return ReleaseResult{}, fmt.Errorf("insert dispatch intent: %w", err)
	}

	// Update session
	_, err = tx.Tx().ExecContext(ctx, `
UPDATE sessions SET active_key = ?, state = 'running', row_version = ?, updated_at = ?
WHERE session_id = ?;`, turnKey, newVer, now, sessionID)
	if err != nil {
		return ReleaseResult{}, fmt.Errorf("update session for release: %w", err)
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
		return ReleaseResult{}, fmt.Errorf("record release journal entry: %w", err)
	}

	if s.testHookBeforeCommit != nil {
		s.testHookBeforeCommit("pre_commit_release")
	}

	if err := tx.Commit(); err != nil {
		return ReleaseResult{}, err
	}
	return ReleaseResult{
		Receipt:     receipt,
		Disposition: ReleaseDispositionNew,
	}, nil
}

func (s *Store) ReconcileSession(ctx context.Context, opID string, execRef ExecutionRef, ref adapter.RecoveryRef, outcome adapter.ReconciliationOutcome) (OperationReceipt, error) {
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

	// The original execution reference is validated against the persisted
	// accepted execution inside the atomic transition: session, turn, and
	// the exact attempt identity (AC-004 Gate 1 review finding 4).
	if _, err := resolveExecutionRef(ctx, tx.Tx(), execRef); err != nil {
		return OperationReceipt{}, err
	}

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, "", "reconcile_session", fp); err != nil {
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

	// Two-boundary form (AC-004 §6): the controller authorized this
	// recovery operation at initiation (service boundary classifies
	// authority before the probe); this commit records the verified
	// observation under the captured execution and recovery references —
	// an authorized probe stays persistable even if controller authority
	// rotates while it is in flight. The active recovery generation below
	// remains the episode-scoped validity check.
	_ = runLease
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

	if err := recordJournalEntry(tx.Tx(), opID, "reconcile_session", fp, runID, sessID, ref.TurnKey, "session_reconciled", receipt, ""); err != nil {
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

	if phase != "receipt_acknowledged" && phase != "acceptance_unknown" {
		return OperationReceipt{}, fmt.Errorf("invalid dispatch observation phase %q: only acknowledgement or uncertainty phases permitted", phase)
	}

	// Late arrival rule: if intent is already resolved, observation is ignored as clean no-op with durable receipt
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
		if err := recordJournalEntry(tx.Tx(), opID, "record_dispatch_observation", fp, runID, sessionID, turnKey, "dispatch_late_observation_ignored", receipt, callerLease); err != nil {
			return OperationReceipt{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperationReceipt{}, err
		}
		return receipt, nil
	}

	if currentPhase == phase {
		receipt := OperationReceipt{
			OpID:             opID,
			CommandType:      "record_dispatch_observation",
			SessionID:        sessionID,
			TurnKey:          turnKey,
			CommittedVersion: currentVer,
			CreatedAt:        time.Now().UTC(),
			Payload:          phase,
		}
		if err := recordJournalEntry(tx.Tx(), opID, "record_dispatch_observation", fp, runID, sessionID, turnKey, "dispatch_observation_noop", receipt, callerLease); err != nil {
			return OperationReceipt{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperationReceipt{}, err
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

	// Current controller authority precedes idempotent replay (AC-004).
	if _, err := authorizeSessionController(ctx, tx.Tx(), sessionID, callerLease, true); err != nil {
		return OperationReceipt{}, err
	}

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "request_cancel", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}
	// New decisions enforce the run connection precondition at this write
	// boundary; idempotent replay above follows the disconnected-read policy.
	if err := requireRunConnected(ctx, tx.Tx(), runIDOfSession(ctx, tx.Tx(), sessionID)); err != nil {
		return OperationReceipt{}, err
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
	if state != "running" || !activeKey.Valid || activeKey.String != turnKey {
		return OperationReceipt{}, errors.New("no active running turn to cancel")
	}

	var turnStatus string
	err = tx.Tx().QueryRowContext(ctx, "SELECT status FROM turns WHERE session_id = ? AND turn_key = ?;", sessionID, turnKey).Scan(&turnStatus)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query turn status: %w", err)
	}
	if turnStatus == "cancelling" {
		receipt := OperationReceipt{
			OpID:             opID,
			CommandType:      "request_cancel",
			SessionID:        sessionID,
			TurnKey:          turnKey,
			CommittedVersion: currentVer,
			CreatedAt:        time.Now().UTC(),
			Payload:          "cancelling",
		}
		if err := recordJournalEntry(tx.Tx(), opID, "request_cancel", fp, runID, sessionID, turnKey, "cancel_noop", receipt, callerLease); err != nil {
			return OperationReceipt{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperationReceipt{}, err
		}
		return receipt, nil
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

	// Current controller authority precedes idempotent replay (AC-004).
	if _, err := authorizeSessionController(ctx, tx.Tx(), sessionID, callerLease, true); err != nil {
		return OperationReceipt{}, err
	}

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

	_ = runLease // authority classified before replay
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}

	// First query existing turn status and result
	var existingTurnStatus, existingTurnResult string
	err = tx.Tx().QueryRowContext(ctx, "SELECT status, result FROM turns WHERE session_id = ? AND turn_key = ?;", sessionID, turnKey).Scan(&existingTurnStatus, &existingTurnResult)
	if err == sql.ErrNoRows {
		return OperationReceipt{}, fmt.Errorf("turn %s not found", turnKey)
	}
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query turn: %w", err)
	}

	isTerminal := func(s string) bool {
		return s == "completed" || s == "cancelled" || s == "failed" || s == "interrupted"
	}

	if isTerminal(existingTurnStatus) {
		if existingTurnStatus == statusStr && existingTurnResult == sanitizedResult {
			// Exact duplicate: return durable receipt without mutating history
			receipt := OperationReceipt{
				OpID:             opID,
				CommandType:      "record_terminal_outcome",
				SessionID:        sessionID,
				TurnKey:          turnKey,
				CommittedVersion: currentVer,
				CreatedAt:        time.Now().UTC(),
				Payload:          statusStr,
			}
			eventKind := "turn_" + statusStr + "_duplicate"
			if err := recordJournalEntry(tx.Tx(), opID, "record_terminal_outcome", fp, runID, sessionID, turnKey, eventKind, receipt, callerLease); err != nil {
				return OperationReceipt{}, err
			}
			if err := tx.Commit(); err != nil {
				return OperationReceipt{}, err
			}
			return receipt, nil
		}
		// Conflicting terminal outcome on already-terminal turn: reject!
		return OperationReceipt{}, fmt.Errorf("%w: turn %s is already terminal (%s), conflicting with %s", ErrConflictingTerminalOutcome, turnKey, existingTurnStatus, statusStr)
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

	// Current controller authority precedes idempotent replay (AC-004).
	if _, err := authorizeSessionController(ctx, tx.Tx(), sessionID, callerLease, true); err != nil {
		return OperationReceipt{}, err
	}

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

	_ = runLease // authority classified before replay
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
		if err := recordJournalEntry(tx.Tx(), opID, "record_host_loss", fp, runID, sessionID, activeKey.String, "host_loss_noop", receipt, callerLease); err != nil {
			return OperationReceipt{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperationReceipt{}, err
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

	// Current controller authority precedes idempotent replay (AC-004).
	if _, err := authorizeSessionController(ctx, tx.Tx(), sessionID, callerLease, true); err != nil {
		return OperationReceipt{}, err
	}

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "set_controller_connection", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, runLease, lifecycle, currentStatus string
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, r.controller_lease, s.row_version, s.lifecycle, s.controller_status
FROM sessions s
JOIN runs r ON s.run_id = r.run_id
WHERE s.session_id = ?;`, sessionID).Scan(&runID, &runLease, &currentVer, &lifecycle, &currentStatus)
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

	if currentStatus == string(status) {
		receipt := OperationReceipt{
			OpID:             opID,
			CommandType:      "set_controller_connection",
			SessionID:        sessionID,
			CommittedVersion: currentVer,
			CreatedAt:        time.Now().UTC(),
			Payload:          string(status),
		}
		if err := recordJournalEntry(tx.Tx(), opID, "set_controller_connection", fp, runID, sessionID, "", "connection_noop", receipt, callerLease); err != nil {
			return OperationReceipt{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperationReceipt{}, err
		}
		return receipt, nil
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

	// Current controller authority precedes idempotent replay (AC-004).
	if _, err := authorizeSessionController(ctx, tx.Tx(), sessionID, callerLease, false); err != nil {
		return OperationReceipt{}, err
	}

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

	_ = runLease // authority classified before replay
	if currentVer != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" {
		receipt := OperationReceipt{
			OpID:             opID,
			CommandType:      "archive_session",
			SessionID:        sessionID,
			CommittedVersion: currentVer,
			CreatedAt:        time.Now().UTC(),
		}
		if err := recordJournalEntry(tx.Tx(), opID, "archive_session", fp, runID, sessionID, "", "session_archived_noop", receipt, callerLease); err != nil {
			return OperationReceipt{}, err
		}
		if err := tx.Commit(); err != nil {
			return OperationReceipt{}, err
		}
		return receipt, nil
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

	sanitizedPayload := SanitizeText(decisionPayload)
	fp := computeFingerprint("record_decision", runID, artifactID, fmt.Sprintf("%d", revision), sanitizedPayload)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	// Current controller authority precedes idempotent replay (AC-004).
	if _, err := classifyCredential(ctx, tx.Tx(), runID, callerLease, true); err != nil {
		return OperationReceipt{}, err
	}

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, "record_decision", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	// New decisions enforce the run connection precondition at this write
	// boundary; idempotent replay above follows the disconnected-read policy.
	if err := requireRunConnected(ctx, tx.Tx(), runID); err != nil {
		return OperationReceipt{}, err
	}

	var runLease string
	err = tx.Tx().QueryRowContext(ctx, "SELECT controller_lease FROM runs WHERE run_id = ?;", runID).Scan(&runLease)
	if err != nil {
		return OperationReceipt{}, err
	}
	_ = runLease // authority classified before replay

	// Verify that referenced (artifact_id, revision) exists in artifact_revisions for this run and query digest
	var digest string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT digest FROM artifact_revisions
WHERE artifact_id = ? AND revision = ? AND run_id = ?;`, artifactID, revision, runID).Scan(&digest)
	if err == sql.ErrNoRows {
		return OperationReceipt{}, ErrArtifactNotFound
	}
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query artifact revision: %w", err)
	}

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "record_decision",
		CommittedVersion: revision,
		CreatedAt:        now,
		Payload:          sanitizedPayload,
	}

	dec := DecisionRecord{
		OpID:            opID,
		RunID:           runID,
		ArtifactID:      artifactID,
		Revision:        revision,
		Digest:          digest,
		DecisionPayload: sanitizedPayload,
		CreatedAt:       now,
	}

	jp := decisionJournalPayload{
		CallerLease: callerLease,
		Receipt:     receipt,
		Decision:    dec,
	}
	jpBytes, err := json.Marshal(jp)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("marshal decision journal payload: %w", err)
	}

	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO journal_entries (op_id, command_type, command_fingerprint, run_id, session_id, turn_key, event_kind, payload_version, payload_json, created_at)
VALUES (?, 'record_decision', ?, ?, NULL, NULL, 'controller_decision', 1, ?, ?);`, opID, fp, runID, string(jpBytes), nowStr)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert journal entry: %w", err)
	}

	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO decisions (op_id, run_id, artifact_id, revision, digest, decision_payload, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?);`, opID, runID, artifactID, revision, digest, sanitizedPayload, nowStr)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert decision: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// ValidateSessionRun checks whether the given session belongs to the specified run.
func (s *Store) ValidateSessionRun(ctx context.Context, sessionID, runID string) error {
	var actualRunID string
	err := s.readDB.QueryRowContext(ctx, "SELECT run_id FROM sessions WHERE session_id = ?;", sessionID).Scan(&actualRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSessionNotFound
	}
	if err != nil {
		return fmt.Errorf("validate session run: %w", err)
	}
	if actualRunID != runID {
		return ErrRunSessionMismatch
	}
	return nil
}

// ValidateControllerLease checks whether the caller lease matches the session's run controller lease.
func (s *Store) ValidateControllerLease(ctx context.Context, sessionID, callerLease string) error {
	_, err := authorizeSessionController(ctx, s.readDB, sessionID, callerLease, true)
	return err
}

// GetDiagnosticCounts computes active runs, reserved turns, unresolved turns, and recovery blockers.
// A turn in either nonterminal execution state ('running' or 'cancelling')
// is a reservation; an unconfirmed cancellation is not completed work.
func (s *Store) GetDiagnosticCounts(ctx context.Context, liveWorkerKeys map[string]bool) (DiagnosticCounts, error) {
	var counts DiagnosticCounts
	counts.ActiveRuns = make([]string, 0)

	// 1. Active runs
	rows, err := s.readDB.QueryContext(ctx, `SELECT run_id FROM runs WHERE lifecycle = 'active';`)
	if err != nil {
		return counts, err
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return counts, fmt.Errorf("scan active run: %w", err)
		}
		counts.ActiveRuns = append(counts.ActiveRuns, runID)
	}
	if err := rows.Err(); err != nil {
		return counts, fmt.Errorf("iterate active runs: %w", err)
	}

	// 2. Reserved and Unresolved turns
	turnRows, err := s.readDB.QueryContext(ctx, `SELECT session_id, turn_key, status FROM turns WHERE status IN ('running', 'cancelling');`)
	if err != nil {
		return counts, err
	}
	defer turnRows.Close()
	for turnRows.Next() {
		var sID, tKey, status string
		if err := turnRows.Scan(&sID, &tKey, &status); err != nil {
			return counts, fmt.Errorf("scan reserved turn: %w", err)
		}
		counts.ReservedTurns++
		mapKey := fmt.Sprintf("%s:%s", sID, tKey)
		if liveWorkerKeys == nil || !liveWorkerKeys[mapKey] {
			counts.UnresolvedTurns++
		}
	}
	if err := turnRows.Err(); err != nil {
		return counts, fmt.Errorf("iterate reserved turns: %w", err)
	}

	// 3. Recovery Blockers = Unresolved turns + sessions with visibility = 'host_lost'
	var hostLostSessions int
	if err := s.readDB.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE visibility = 'host_lost';`).Scan(&hostLostSessions); err != nil {
		return counts, fmt.Errorf("count host-lost sessions: %w", err)
	}
	counts.RecoveryBlockers = counts.UnresolvedTurns + hostLostSessions

	return counts, nil
}
