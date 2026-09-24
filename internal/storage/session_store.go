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
	SessionID     string    `json:"session_id"`
	TurnKey       string    `json:"turn_key"`
	Prompt        string    `json:"prompt"`
	RequiredTools []string  `json:"required_tools,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
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

	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO run_profiles (
	run_id, profile_digest, algorithm_version, workspace_mode,
	isolation_strictness, network_mode, canonical_profile_json,
	source_repo_identity, source_commit, source_tree, brief_artifact_digest, created_at
) VALUES (?, ?, 'legacy-unverified', 'none', 'permissive_dev', 'unrestricted', '', 'legacy', '', '', '', ?)
ON CONFLICT(run_id) DO NOTHING;`, runID, profileDigest, now)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert legacy run profile: %w", err)
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

// CreateRunWithProfileRequest contains all inputs necessary to create a run with frozen canonical inputs.
type CreateRunWithProfileRequest struct {
	OpID               string           `json:"op_id"`
	ControllerLease    string           `json:"controller_lease"`
	RunID              string           `json:"run_id"`
	Brief              string           `json:"brief"`
	SourceRepoIdentity string           `json:"source_repo_identity"`
	SourceCommit       string           `json:"source_commit"`
	SourceTree         string           `json:"source_tree"`
	Profile            CanonicalProfile `json:"profile"`
}

// CreateRunWithProfileResult contains the receipt and the calculated canonical digests for the created run.
type CreateRunWithProfileResult struct {
	Receipt       OperationReceipt `json:"receipt"`
	BriefDigest   string           `json:"brief_digest"`
	SourceDigest  string           `json:"source_digest"`
	ProfileDigest string           `json:"profile_digest"`
}

// CreateRunWithProfile creates a new run with immutable canonical inputs:
// 1. Computes canonical brief_digest (cbrief-v1), source_digest (csource-v1), and profile_digest (cprof-v1).
// 2. Stores the raw brief in CAS blob storage.
// 3. Atomically inserts the run into runs, run_profiles, controller_leases (provenance), and journal_entries.
func (s *Store) CreateRunWithProfile(ctx context.Context, req CreateRunWithProfileRequest) (CreateRunWithProfileResult, error) {
	if strings.TrimSpace(req.OpID) == "" {
		return CreateRunWithProfileResult{}, errors.New("empty op_id")
	}
	if strings.TrimSpace(req.ControllerLease) == "" {
		return CreateRunWithProfileResult{}, errors.New("empty controller_lease")
	}
	if strings.TrimSpace(req.RunID) == "" {
		return CreateRunWithProfileResult{}, errors.New("empty run_id")
	}
	if strings.TrimSpace(req.Brief) == "" {
		return CreateRunWithProfileResult{}, errors.New("empty brief")
	}

	briefDigest, err := ComputeBriefDigest(req.Brief)
	if err != nil {
		return CreateRunWithProfileResult{}, fmt.Errorf("compute brief digest: %w", err)
	}

	sourceDigest, err := ComputeSourceDigest(req.Profile.WorkspaceMode, req.SourceRepoIdentity, req.SourceCommit, req.SourceTree)
	if err != nil {
		return CreateRunWithProfileResult{}, fmt.Errorf("compute source digest: %w", err)
	}

	profileDigest, canonicalProfileJSON, err := ComputeProfileDigest(req.Profile)
	if err != nil {
		return CreateRunWithProfileResult{}, fmt.Errorf("compute profile digest: %w", err)
	}

	// Persist the brief in CAS storage (hex sha256)
	briefSum := sha256.Sum256([]byte(req.Brief))
	briefRawHex := fmt.Sprintf("%x", briefSum)
	if err := s.writeCASBlob([]byte(req.Brief), briefRawHex); err != nil {
		return CreateRunWithProfileResult{}, fmt.Errorf("store brief in cas: %w", err)
	}

	fp := computeFingerprint("create_run_with_profile", req.RunID, briefDigest, sourceDigest, profileDigest, req.ControllerLease)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return CreateRunWithProfileResult{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), req.OpID, req.ControllerLease, "create_run_with_profile", fp); err != nil {
		return CreateRunWithProfileResult{}, err
	} else if receipt != nil {
		return CreateRunWithProfileResult{
			Receipt:       *receipt,
			BriefDigest:   briefDigest,
			SourceDigest:  sourceDigest,
			ProfileDigest: profileDigest,
		}, nil
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO runs (run_id, brief_digest, source_digest, profile_digest, controller_lease, lifecycle, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'active', ?, ?);`, req.RunID, briefDigest, sourceDigest, profileDigest, req.ControllerLease, now, now)
	if err != nil {
		return CreateRunWithProfileResult{}, fmt.Errorf("insert run: %w", err)
	}

	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO run_profiles (
	run_id, profile_digest, algorithm_version, workspace_mode,
	isolation_strictness, network_mode, canonical_profile_json,
	source_repo_identity, source_commit, source_tree, brief_artifact_digest, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`,
		req.RunID, profileDigest, req.Profile.AlgoVersion, req.Profile.WorkspaceMode,
		req.Profile.IsolationStrictness, req.Profile.NetworkMode, string(canonicalProfileJSON),
		req.SourceRepoIdentity, req.SourceCommit, req.SourceTree, briefDigest, now)
	if err != nil {
		return CreateRunWithProfileResult{}, fmt.Errorf("insert run profile: %w", err)
	}

	// Generation-0 provenance
	_, err = tx.Tx().ExecContext(ctx, `INSERT INTO controller_leases
		(run_id, generation, harness, controller_ref, lease, status, granted_by_op_id, attached_at, updated_at)
		VALUES (?, 0, NULL, 'create-run-provenance', ?, 'legacy', ?, ?, ?);`,
		req.RunID, req.ControllerLease, req.OpID, now, now)
	if err != nil {
		return CreateRunWithProfileResult{}, fmt.Errorf("insert run provenance: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             req.OpID,
		CommandType:      "create_run_with_profile",
		CommittedVersion: 1,
		CreatedAt:        time.Now().UTC(),
	}

	if err := recordJournalEntry(tx.Tx(), req.OpID, "create_run_with_profile", fp, req.RunID, "", "", "run_created", receipt, req.ControllerLease); err != nil {
		return CreateRunWithProfileResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return CreateRunWithProfileResult{}, err
	}

	return CreateRunWithProfileResult{
		Receipt:       receipt,
		BriefDigest:   briefDigest,
		SourceDigest:  sourceDigest,
		ProfileDigest: profileDigest,
	}, nil
}

// RunProfileRecord holds a persisted run_profiles database row.
type RunProfileRecord struct {
	RunID                string           `json:"run_id"`
	ProfileDigest        string           `json:"profile_digest"`
	AlgorithmVersion     string           `json:"algorithm_version"`
	WorkspaceMode        string           `json:"workspace_mode"`
	IsolationStrictness  string           `json:"isolation_strictness"`
	NetworkMode          string           `json:"network_mode"`
	CanonicalProfileJSON string           `json:"canonical_profile_json"`
	SourceRepoIdentity   string           `json:"source_repo_identity"`
	SourceCommit         string           `json:"source_commit"`
	SourceTree           string           `json:"source_tree"`
	BriefArtifactDigest  string           `json:"brief_artifact_digest"`
	Profile              CanonicalProfile `json:"profile"`
	CreatedAt            time.Time        `json:"created_at"`
}

// GetRunProfile retrieves the frozen profile record for a run.
func (s *Store) GetRunProfile(ctx context.Context, runID string) (RunProfileRecord, error) {
	row := s.readDB.QueryRowContext(ctx, `
SELECT run_id, profile_digest, algorithm_version, workspace_mode,
       isolation_strictness, network_mode, canonical_profile_json,
       source_repo_identity, source_commit, source_tree, brief_artifact_digest, created_at
FROM run_profiles WHERE run_id = ?;`, runID)

	var rec RunProfileRecord
	var createdAtStr string
	if err := row.Scan(
		&rec.RunID, &rec.ProfileDigest, &rec.AlgorithmVersion, &rec.WorkspaceMode,
		&rec.IsolationStrictness, &rec.NetworkMode, &rec.CanonicalProfileJSON,
		&rec.SourceRepoIdentity, &rec.SourceCommit, &rec.SourceTree, &rec.BriefArtifactDigest, &createdAtStr,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunProfileRecord{}, ErrRunNotFound
		}
		return RunProfileRecord{}, fmt.Errorf("query run profile: %w", err)
	}

	rec.CreatedAt = parseTime(createdAtStr)
	if rec.CanonicalProfileJSON != "" {
		prof, err := ParseCanonicalProfileJSON([]byte(rec.CanonicalProfileJSON))
		if err == nil {
			rec.Profile = prof
		}
	}
	return rec, nil
}

// SessionMetadata contains core identities for an existing session.
type SessionMetadata struct {
	SessionID   string
	RunID       string
	Contributor string
}

// GetSessionMetadata returns the metadata (run_id, contributor) for a session.
func (s *Store) GetSessionMetadata(ctx context.Context, sessionID string) (SessionMetadata, error) {
	var meta SessionMetadata
	meta.SessionID = sessionID
	err := s.readDB.QueryRowContext(ctx, "SELECT run_id, contributor FROM sessions WHERE session_id = ?;", sessionID).
		Scan(&meta.RunID, &meta.Contributor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionMetadata{}, ErrSessionNotFound
		}
		return SessionMetadata{}, fmt.Errorf("query session metadata: %w", err)
	}
	return meta, nil
}

// GetSessionRunID returns the run_id associated with a session.
func (s *Store) GetSessionRunID(ctx context.Context, sessionID string) (string, error) {
	var runID string
	err := s.readDB.QueryRowContext(ctx, "SELECT run_id FROM sessions WHERE session_id = ?;", sessionID).Scan(&runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrSessionNotFound
		}
		return "", fmt.Errorf("query session run_id: %w", err)
	}
	return runID, nil
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

	// The compatibility projection is derived from the run's actual
	// connection state — never defaulted to connected.
	var runConnected int
	if err := tx.Tx().QueryRowContext(ctx, `SELECT connected FROM controller_leases WHERE run_id = ? AND status = 'active';`, session.RunID).Scan(&runConnected); err != nil {
		runConnected = 0
	}
	projection := "disconnected"
	if runConnected == 1 {
		projection = "connected"
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Tx().ExecContext(ctx, `
INSERT INTO sessions (session_id, run_id, contributor, is_active_contributor, state, lifecycle, controller_status, visibility, recovery_gen, active_recovery_gen, row_version, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, 1, ?, ?);`,
		session.ID, session.RunID, session.Contributor, isActive, session.State, projection, session.Visibility, session.RecoveryGeneration, session.RecoveryGeneration, now, now)
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
	requiredToolsJSON := marshalStrings(prompt.RequiredTools)
	fp := computeFingerprint("queue_prompt", sessionID, prompt.TurnKey, sanitized, requiredToolsJSON)

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
	// New decisions enforce the run connection precondition at this write
	// boundary; idempotent replay above follows the disconnected-read policy.
	if err := requireRunConnected(ctx, tx.Tx(), runIDOfSession(ctx, tx.Tx(), sessionID)); err != nil {
		return OperationReceipt{}, err
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
	var existingPendingPrompt, existingRequiredTools string
	err = tx.Tx().QueryRowContext(ctx, "SELECT prompt, required_tools_json FROM pending_prompts WHERE session_id = ? AND turn_key = ?;", sessionID, prompt.TurnKey).Scan(&existingPendingPrompt, &existingRequiredTools)
	if err == nil {
		if existingPendingPrompt == sanitized && existingRequiredTools == requiredToolsJSON {
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
INSERT INTO pending_prompts (session_id, turn_key, prompt, queued_at, required_tools_json)
VALUES (?, ?, ?, ?, ?);`, sessionID, prompt.TurnKey, sanitized, now, requiredToolsJSON)
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
	// required_tools are validated at queue time and immutable thereafter
	// (AC-010 §3.5): a replace may restate the journaled set or omit it
	// (nil keeps it); a different set is refused, never silently ignored.
	// The set joins the fingerprint only when stated, so replays of
	// replaces recorded without one keep their fingerprint.
	fpParts := []string{"replace_pending_prompt", sessionID, prompt.TurnKey, sanitized}
	var statedTools string
	if prompt.RequiredTools != nil {
		statedTools = marshalStrings(prompt.RequiredTools)
		fpParts = append(fpParts, statedTools)
	}
	fp := computeFingerprint(fpParts...)

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
	// New decisions enforce the run connection precondition at this write
	// boundary; idempotent replay above follows the disconnected-read policy.
	if err := requireRunConnected(ctx, tx.Tx(), runIDOfSession(ctx, tx.Tx(), sessionID)); err != nil {
		return OperationReceipt{}, err
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

	if prompt.RequiredTools != nil {
		var storedTools string
		err := tx.Tx().QueryRowContext(ctx, `
SELECT required_tools_json FROM pending_prompts WHERE session_id = ? AND turn_key = ?;`, sessionID, prompt.TurnKey).Scan(&storedTools)
		if errors.Is(err, sql.ErrNoRows) {
			return OperationReceipt{}, ErrPromptNotQueued
		}
		if err != nil {
			return OperationReceipt{}, fmt.Errorf("query pending required tools: %w", err)
		}
		if storedTools != statedTools {
			return OperationReceipt{}, fmt.Errorf("%w: turn %s was queued with %s, replace names %s",
				ErrRequiredToolsImmutable, prompt.TurnKey, storedTools, statedTools)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Update ONLY the specified pending prompt row (prompt text and queue
	// time; required_tools_json is immutable and deliberately untouched)
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
	// New decisions enforce the run connection precondition at this write
	// boundary; idempotent replay above follows the disconnected-read policy.
	if err := requireRunConnected(ctx, tx.Tx(), runIDOfSession(ctx, tx.Tx(), sessionID)); err != nil {
		return OperationReceipt{}, err
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
