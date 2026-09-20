package storage_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Helper to bootstrap a test run and session
func setupRunAndSession(t *testing.T, store *storage.Store, runID, sessID, lease string) {
	t.Helper()
	ctx := context.Background()
	_, err := store.CreateRun(ctx, "op-run-"+runID, runID, "brief-sha", "src-sha", "prof-sha", lease)
	if err != nil {
		t.Fatalf("setup create run: %v", err)
	}
	_, err = store.CreateSession(ctx, "op-sess-"+sessID, lease, storage.SessionRecord{
		ID:                  sessID,
		RunID:               runID,
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("setup create session: %v", err)
	}
}

// Finding 1: Immutable Terminal History & Restricted Observations
func TestReview18F_TerminalDelivery_NoOverwrite_DuplicateAcknowledge(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	setupRunAndSession(t, store, "run-1", "sess-1", "lease-1")

	// Queue and release turn-1
	_, err = store.QueuePrompt(ctx, "op-q1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "Do work", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	relReceipt, err := store.ReleaseTurn(ctx, "op-rel1", "lease-1", "sess-1", 2, "turn-1")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	// 1. Complete turn-1 with "Original Success"
	termReceipt, err := store.RecordTerminalOutcome(ctx, "op-term1", "lease-1", "sess-1", relReceipt.CommittedVersion, "turn-1", council.TurnCompleted, "Original Success")
	if err != nil {
		t.Fatalf("record terminal outcome: %v", err)
	}
	if termReceipt.Payload != "completed" {
		t.Fatalf("unexpected payload: %s", termReceipt.Payload)
	}

	// 2. Exact duplicate delivery: must succeed without altering history and return durable receipt
	dupReceipt, err := store.RecordTerminalOutcome(ctx, "op-term-dup", "lease-1", "sess-1", termReceipt.CommittedVersion, "turn-1", council.TurnCompleted, "Original Success")
	if err != nil {
		t.Fatalf("duplicate terminal outcome failed: %v", err)
	}
	if dupReceipt.Payload != "completed" {
		t.Fatalf("unexpected dup payload: %s", dupReceipt.Payload)
	}

	// 3. Conflicting outcome (e.g. Failed instead of Completed) must be rejected
	_, err = store.RecordTerminalOutcome(ctx, "op-term-conflict1", "lease-1", "sess-1", termReceipt.CommittedVersion, "turn-1", council.TurnFailed, "Failed after all")
	if !errors.Is(err, storage.ErrConflictingTerminalOutcome) {
		t.Fatalf("expected ErrConflictingTerminalOutcome for status conflict, got %v", err)
	}

	// 4. Conflicting result string (Completed with different result) must be rejected
	_, err = store.RecordTerminalOutcome(ctx, "op-term-conflict2", "lease-1", "sess-1", termReceipt.CommittedVersion, "turn-1", council.TurnCompleted, "Different Success Content")
	if !errors.Is(err, storage.ErrConflictingTerminalOutcome) {
		t.Fatalf("expected ErrConflictingTerminalOutcome for result conflict, got %v", err)
	}

	// Verify history remained intact
	hydrated, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	turn := hydrated.Sessions["sess-1"].Turns["turn-1"]
	if turn.Status != "completed" || turn.Result != "Original Success" {
		t.Fatalf("turn history was modified: status=%s, result=%s", turn.Status, turn.Result)
	}
}

func TestReview18F_RecordDispatchObservation_PhaseRestriction(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	setupRunAndSession(t, store, "run-1", "sess-1", "lease-1")

	_, err = store.QueuePrompt(ctx, "op-q1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "Do work", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	_, err = store.ReleaseTurn(ctx, "op-rel1", "lease-1", "sess-1", 2, "turn-1")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	// Direct transition to "resolved" via observation MUST be forbidden
	_, err = store.RecordDispatchObservation(ctx, "op-obs-resolved", "lease-1", "sess-1", "turn-1", "resolved")
	if err == nil {
		t.Fatalf("expected error setting resolved phase directly via observation, but succeeded")
	}

	// Setting valid acknowledgement phase succeeds
	obsReceipt, err := store.RecordDispatchObservation(ctx, "op-obs-ack", "lease-1", "sess-1", "turn-1", "receipt_acknowledged")
	if err != nil {
		t.Fatalf("valid observation: %v", err)
	}
	if obsReceipt.Payload != "receipt_acknowledged" {
		t.Fatalf("unexpected payload: %s", obsReceipt.Payload)
	}

	// Complete turn authoritatively
	_, err = store.RecordTerminalOutcome(ctx, "op-term1", "lease-1", "sess-1", obsReceipt.CommittedVersion, "turn-1", council.TurnCompleted, "Done")
	if err != nil {
		t.Fatalf("terminal outcome: %v", err)
	}

	// Late observation on resolved turn produces clean durable no-op receipt
	lateReceipt, err := store.RecordDispatchObservation(ctx, "op-obs-late", "lease-1", "sess-1", "turn-1", "receipt_acknowledged")
	if err != nil {
		t.Fatalf("late observation error: %v", err)
	}
	if lateReceipt.Payload != "late_observation_ignored" {
		t.Fatalf("expected late_observation_ignored payload, got %s", lateReceipt.Payload)
	}
}

// Finding 2: Typed Controller Decisions & Sensitive Data Redaction
func TestReview18F_ControllerDecisions_TypedAndSanitized(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	setupRunAndSession(t, store, "run-1", "sess-1", "lease-1")

	// Publish artifact revision
	meta := storage.ArtifactMetadata{
		ID:        "art-eval-1",
		RunID:     "run-1",
		SessionID: "sess-1",
		TurnKey:   "turn-1",
		Name:      "evaluation.json",
	}
	pubMeta, err := store.PublishArtifact(ctx, "op-pub1", "lease-1", meta, []byte("artifact content to evaluate"))
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	// Record decision referencing the published artifact with sensitive credentials in payload
	decisionPayload := `{"decision":"approve","secret":"Authorization: Bearer my-secret-token","key":"sk-ant-api03-1234567890abcdef12345678"}`
	decReceipt, err := store.RecordDecision(ctx, "op-dec1", "lease-1", "run-1", "art-eval-1", pubMeta.Revision, decisionPayload)
	if err != nil {
		t.Fatalf("record decision: %v", err)
	}

	// Verify receipt payload was sanitized
	if decReceipt.Payload == decisionPayload {
		t.Fatalf("decision receipt payload was not sanitized")
	}

	// Verify typed decision row exists in decisions table
	var decID int64
	var opID, runID, artID, digest, payloadStr string
	var rev int64
	err = store.DB().QueryRowContext(ctx, "SELECT decision_id, op_id, run_id, artifact_id, revision, digest, decision_payload FROM decisions WHERE op_id = ?;", "op-dec1").Scan(&decID, &opID, &runID, &artID, &rev, &digest, &payloadStr)
	if err != nil {
		t.Fatalf("query decisions table: %v", err)
	}
	if opID != "op-dec1" || runID != "run-1" || artID != "art-eval-1" || rev != 1 || digest != pubMeta.Digest {
		t.Fatalf("unexpected decision record fields: opID=%s, runID=%s, artID=%s, rev=%d, digest=%s", opID, runID, artID, rev, digest)
	}
	if payloadStr != decReceipt.Payload {
		t.Fatalf("stored payload mismatch: got %s, want %s", payloadStr, decReceipt.Payload)
	}

	// Hydrate state and verify typed Decisions slice
	hydrated, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if len(hydrated.Decisions) != 1 {
		t.Fatalf("expected 1 hydrated decision, got %d", len(hydrated.Decisions))
	}
	dec := hydrated.Decisions[0]
	if dec.OpID != "op-dec1" || dec.ArtifactID != "art-eval-1" || dec.Digest != pubMeta.Digest {
		t.Fatalf("unexpected hydrated decision: %+v", dec)
	}

	// Idempotent retry returns identical receipt
	retryReceipt, err := store.RecordDecision(ctx, "op-dec1", "lease-1", "run-1", "art-eval-1", pubMeta.Revision, decisionPayload)
	if err != nil {
		t.Fatalf("idempotent decision retry failed: %v", err)
	}
	if retryReceipt.OpID != decReceipt.OpID || retryReceipt.Payload != decReceipt.Payload {
		t.Fatalf("idempotent receipt mismatch: %+v vs %+v", retryReceipt, decReceipt)
	}
}

// Finding 3: Durable Operation Receipts for Accepted No-ops
func TestReview18F_DurableReceipts_AcceptedNoOps(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	setupRunAndSession(t, store, "run-1", "sess-1", "lease-1")

	// 1. QueuePrompt identical no-op
	p := storage.PendingPrompt{SessionID: "sess-1", TurnKey: "turn-q", Prompt: "Same Prompt", CreatedAt: time.Now()}
	_, err = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, p)
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	noopReceipt, err := store.QueuePrompt(ctx, "op-q-noop", "lease-1", "sess-1", 2, p)
	if err != nil {
		t.Fatalf("queue prompt noop: %v", err)
	}
	// Verify journal has durable record for op-q-noop
	var eventKind string
	err = store.DB().QueryRowContext(ctx, "SELECT event_kind FROM journal_entries WHERE op_id = 'op-q-noop';").Scan(&eventKind)
	if err != nil || eventKind != "prompt_identical_noop" {
		t.Fatalf("expected prompt_identical_noop in journal, got err=%v, kind=%s", err, eventKind)
	}
	// Verify idempotent replay matches
	replayReceipt, err := store.QueuePrompt(ctx, "op-q-noop", "lease-1", "sess-1", 2, p)
	if err != nil || replayReceipt.OpID != noopReceipt.OpID {
		t.Fatalf("replay failed: %v, receipt=%+v", err, replayReceipt)
	}

	// Release turn
	relReceipt, err := store.ReleaseTurn(ctx, "op-rel-q", "lease-1", "sess-1", 2, "turn-q")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	// 2. RequestCancel durable no-op when already cancelling
	cancel1, err := store.RequestCancel(ctx, "op-c-1", "lease-1", "sess-1", relReceipt.CommittedVersion, "turn-q")
	if err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	cancelNoop, err := store.RequestCancel(ctx, "op-c-noop", "lease-1", "sess-1", cancel1.CommittedVersion, "turn-q")
	if err != nil {
		t.Fatalf("request cancel noop: %v", err)
	}
	err = store.DB().QueryRowContext(ctx, "SELECT event_kind FROM journal_entries WHERE op_id = 'op-c-noop';").Scan(&eventKind)
	if err != nil || eventKind != "cancel_noop" {
		t.Fatalf("expected cancel_noop in journal, got err=%v, kind=%s", err, eventKind)
	}
	if cancelNoop.Payload != "cancelling" {
		t.Fatalf("unexpected cancel payload: %s", cancelNoop.Payload)
	}

	// 3. RecordHostLoss durable no-op when already host_lost
	hlReceipt1, err := store.RecordHostLoss(ctx, "op-hl-1", "lease-1", "sess-1", cancel1.CommittedVersion)
	if err != nil {
		t.Fatalf("record host loss: %v", err)
	}
	hlNoop, err := store.RecordHostLoss(ctx, "op-hl-noop", "lease-1", "sess-1", hlReceipt1.CommittedVersion)
	if err != nil {
		t.Fatalf("record host loss noop: %v", err)
	}
	err = store.DB().QueryRowContext(ctx, "SELECT event_kind FROM journal_entries WHERE op_id = 'op-hl-noop';").Scan(&eventKind)
	if err != nil || eventKind != "host_loss_noop" {
		t.Fatalf("expected host_loss_noop in journal, got err=%v, kind=%s", err, eventKind)
	}
	if hlNoop.Payload != hlReceipt1.Payload {
		t.Fatalf("expected matching recovery gen: got %s, want %s", hlNoop.Payload, hlReceipt1.Payload)
	}

	// 4. SetControllerConnection durable no-op when identical
	connReceipt1, err := store.SetControllerConnection(ctx, "op-conn-1", "lease-1", "sess-1", hlReceipt1.CommittedVersion, council.ControllerDisconnected)
	if err != nil {
		t.Fatalf("set controller conn: %v", err)
	}
	connNoop, err := store.SetControllerConnection(ctx, "op-conn-noop", "lease-1", "sess-1", connReceipt1.CommittedVersion, council.ControllerDisconnected)
	if err != nil {
		t.Fatalf("set controller conn noop: %v", err)
	}
	if connNoop.Payload != string(council.ControllerDisconnected) {
		t.Fatalf("unexpected conn noop payload: %s", connNoop.Payload)
	}
	err = store.DB().QueryRowContext(ctx, "SELECT event_kind FROM journal_entries WHERE op_id = 'op-conn-noop';").Scan(&eventKind)
	if err != nil || eventKind != "connection_noop" {
		t.Fatalf("expected connection_noop in journal, got err=%v, kind=%s", err, eventKind)
	}

	// Complete turn to allow archive
	_, err = store.RecordTerminalOutcome(ctx, "op-term-q", "lease-1", "sess-1", connReceipt1.CommittedVersion, "turn-q", council.TurnCancelled, "cancelled")
	if err != nil {
		t.Fatalf("term outcome: %v", err)
	}

	// Close recovery episode via reconciliation so visibility returns to reachable
	outcome := adapter.ReconciliationOutcome{
		Ref:          adapter.RecoveryRef{TurnRef: adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-q"}, Generation: 1},
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableTerminal,
		Observed:     council.TurnCancelled,
		Result:       "cancelled",
	}
	recReceipt, err := store.ReconcileSession(ctx, "op-rec-q", "lease-1", outcome.Ref, outcome)
	if err != nil {
		t.Fatalf("reconcile session: %v", err)
	}

	// 5. ArchiveSession durable no-op when already archived
	archReceipt1, err := store.ArchiveSession(ctx, "op-arch-1", "lease-1", "sess-1", recReceipt.CommittedVersion)
	if err != nil {
		t.Fatalf("archive session: %v", err)
	}
	archNoop, err := store.ArchiveSession(ctx, "op-arch-noop", "lease-1", "sess-1", archReceipt1.CommittedVersion)
	if err != nil {
		t.Fatalf("archive session noop: %v", err)
	}
	if archNoop.OpID != "op-arch-noop" {
		t.Fatalf("unexpected arch noop op id: %s", archNoop.OpID)
	}
	err = store.DB().QueryRowContext(ctx, "SELECT event_kind FROM journal_entries WHERE op_id = 'op-arch-noop';").Scan(&eventKind)
	if err != nil || eventKind != "session_archived_noop" {
		t.Fatalf("expected session_archived_noop in journal, got err=%v, kind=%s", err, eventKind)
	}
}

// Finding 4: Complete Configuration Validation & Deep Filesystem Protections
func TestReview18F_ConfigurationValidation_NestedAndSafeGrammar(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	setupRunAndSession(t, store, "run-1", "sess-1", "lease-1")

	// 1. Safe non-JSON profile identifier passes
	_, err = store.SetNativeBinding(ctx, "op-bind-1", "lease-1", "sess-1", 1, storage.NativeBinding{
		LogicalSessionID: "sess-1",
		NativeSessionID:  "nat-1",
		Harness:          "claude",
		Model:            "claude-3-5-sonnet",
		WorkspaceMode:    "shared",
		ToolingConfig:    "profile-review-v1.2",
	})
	if err != nil {
		t.Fatalf("safe profile identifier rejected: %v", err)
	}

	// 2. Non-JSON string with assignment / disallowed grammar rejected
	_, err = store.SetNativeBinding(ctx, "op-bind-bad-str", "lease-1", "sess-1", 2, storage.NativeBinding{
		LogicalSessionID: "sess-1",
		NativeSessionID:  "nat-1",
		Harness:          "claude",
		Model:            "claude-3-5-sonnet",
		WorkspaceMode:    "shared",
		ToolingConfig:    "KEY=secret-token",
	})
	if !errors.Is(err, storage.ErrDisallowedToolingConfig) {
		t.Fatalf("expected ErrDisallowedToolingConfig for assignment in string, got %v", err)
	}

	// 3. Nested JSON credential pattern rejected
	nestedCredentialJSON := `{"tooling":["read","write"],"env_allowlist":{"nested_key":"Authorization: Bearer topsecret"}}`
	_, err = store.SetNativeBinding(ctx, "op-bind-nested-cred", "lease-1", "sess-1", 2, storage.NativeBinding{
		LogicalSessionID: "sess-1",
		NativeSessionID:  "nat-1",
		Harness:          "claude",
		Model:            "claude-3-5-sonnet",
		WorkspaceMode:    "shared",
		ToolingConfig:    nestedCredentialJSON,
	})
	if !errors.Is(err, storage.ErrDisallowedToolingConfig) {
		t.Fatalf("expected ErrDisallowedToolingConfig for nested credential pattern, got %v", err)
	}

	// 4. Nested key with suspicious assignment rejected
	nestedKeyAssignJSON := `{"tooling":["read"],"env_allowlist":{"api_key = secret": "val"}}`
	_, err = store.SetNativeBinding(ctx, "op-bind-nested-key", "lease-1", "sess-1", 2, storage.NativeBinding{
		LogicalSessionID: "sess-1",
		NativeSessionID:  "nat-1",
		Harness:          "claude",
		Model:            "claude-3-5-sonnet",
		WorkspaceMode:    "shared",
		ToolingConfig:    nestedKeyAssignJSON,
	})
	if !errors.Is(err, storage.ErrDisallowedToolingConfig) {
		t.Fatalf("expected ErrDisallowedToolingConfig for nested key assignment, got %v", err)
	}
}

func TestReview18F_DeepSymlinkProtection(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	setupRunAndSession(t, store, "run-1", "sess-1", "lease-1")

	// Symlink artifacts/tmp to an outside directory
	outsideDir := t.TempDir()
	artifactsDir := filepath.Join(tempDir, "artifacts")
	_ = os.MkdirAll(artifactsDir, 0700)
	tmpSymlink := filepath.Join(artifactsDir, "tmp")
	if err := os.Symlink(outsideDir, tmpSymlink); err != nil {
		t.Skipf("symlink creation not permitted: %v", err)
	}

	// PublishArtifact must detect deep symlink and fail
	meta := storage.ArtifactMetadata{
		ID:        "art-symlink",
		RunID:     "run-1",
		SessionID: "sess-1",
		TurnKey:   "turn-1",
		Name:      "test.txt",
	}
	_, err = store.PublishArtifact(ctx, "op-pub-sym", "lease-1", meta, []byte("content"))
	if !errors.Is(err, storage.ErrSymlinkForbidden) {
		t.Fatalf("expected ErrSymlinkForbidden for symlinked tmp dir, got %v", err)
	}
}

// Finding 6: Strict Hydration Invariants & Parent Reference Enforcement
func TestReview18F_HydrationInvariants_ParentReferences(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	setupRunAndSession(t, store, "run-1", "sess-1", "lease-1")

	// 1. Inject an orphan child row with foreign key checks temporarily disabled on raw DB
	// Insert an orphan session referencing nonexistent run-999
	_, err = store.DB().Exec("PRAGMA foreign_keys = OFF;")
	if err != nil {
		t.Fatalf("disable fk: %v", err)
	}
	_, err = store.DB().Exec(`
INSERT INTO sessions (session_id, run_id, contributor, is_active_contributor, state, lifecycle, controller_status, visibility, recovery_gen, active_recovery_gen, row_version, created_at, updated_at)
VALUES ('sess-orphan', 'run-nonexistent', 'claude', 0, 'parked', 'active', 'connected', 'reachable', 0, 0, 1, '2026-09-19T00:00:00Z', '2026-09-19T00:00:00Z');`)
	if err != nil {
		t.Fatalf("insert orphan session: %v", err)
	}

	_, err = store.HydrateState(ctx)
	if !errors.Is(err, storage.ErrInconsistentStorage) {
		t.Fatalf("expected ErrInconsistentStorage for orphan session, got %v", err)
	}

	// Clean up orphan session
	_, _ = store.DB().Exec("DELETE FROM sessions WHERE session_id = 'sess-orphan';")

	// 2. Inject a running session with missing active turn key
	_, err = store.DB().Exec(`
UPDATE sessions SET state = 'running', active_key = NULL WHERE session_id = 'sess-1';`)
	if err != nil {
		t.Fatalf("corrupt session state: %v", err)
	}

	_, err = store.HydrateState(ctx)
	if !errors.Is(err, storage.ErrInconsistentStorage) {
		t.Fatalf("expected ErrInconsistentStorage for running session without active key, got %v", err)
	}

	// 3. Inject running session with turn present, but missing dispatch intent
	_, _ = store.DB().Exec(`
UPDATE sessions SET state = 'running', active_key = 'turn-orphan' WHERE session_id = 'sess-1';`)
	_, _ = store.DB().Exec(`
INSERT INTO turns (session_id, turn_key, prompt, status, result, attempt_id, created_at)
VALUES ('sess-1', 'turn-orphan', 'orphan prompt', 'running', '', 'att_1', '2026-09-19T00:00:00Z');`)

	_, err = store.HydrateState(ctx)
	if !errors.Is(err, storage.ErrInconsistentStorage) {
		t.Fatalf("expected ErrInconsistentStorage for missing dispatch intent on active turn, got %v", err)
	}

	// 4. Inject active turn with resolved dispatch intent while turn is still running
	_, _ = store.DB().Exec(`
INSERT INTO dispatch_intents (session_id, turn_key, attempt_id, phase, recorded_at, updated_at)
VALUES ('sess-1', 'turn-orphan', 'att_1', 'resolved', '2026-09-19T00:00:00Z', '2026-09-19T00:00:00Z');`)

	_, err = store.HydrateState(ctx)
	if !errors.Is(err, storage.ErrInconsistentStorage) {
		t.Fatalf("expected ErrInconsistentStorage for resolved intent while turn is running, got %v", err)
	}

	// Re-enable foreign keys
	_, _ = store.DB().Exec("PRAGMA foreign_keys = ON;")
}

// Finding 5: Artifact Write & Directory Synchronization Fallback Fault Handling
func TestReview18F_ArtifactStore_InstallationFaultHandling(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	setupRunAndSession(t, store, "run-1", "sess-1", "lease-1")

	meta := storage.ArtifactMetadata{
		ID:        "art-fault-1",
		RunID:     "run-1",
		SessionID: "sess-1",
		TurnKey:   "turn-1",
		Name:      "fault_test.txt",
	}
	content := []byte("hello artifact world")

	// First publish succeeds
	pubMeta, err := store.PublishArtifact(ctx, "op-pub-1", "lease-1", meta, content)
	if err != nil {
		t.Fatalf("initial publish failed: %v", err)
	}

	// Idempotent retry returns committed metadata
	retryMeta, err := store.PublishArtifact(ctx, "op-pub-1", "lease-1", meta, content)
	if err != nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
	if retryMeta.Digest != pubMeta.Digest || retryMeta.Revision != pubMeta.Revision {
		t.Fatalf("idempotent retry mismatch: %+v vs %+v", retryMeta, pubMeta)
	}

	// Corrupt file on disk and attempt to publish identical content with new op_id
	blobPath := filepath.Join(tempDir, "artifacts", pubMeta.Digest[:2], pubMeta.Digest)
	err = os.WriteFile(blobPath, []byte("tampered artifact content"), 0600)
	if err != nil {
		t.Fatalf("write tampered file: %v", err)
	}

	// Publish with new op_id detecting corrupt existing destination file
	_, err = store.PublishArtifact(ctx, "op-pub-corrupt", "lease-1", meta, content)
	if !errors.Is(err, storage.ErrArtifactCorrupt) {
		t.Fatalf("expected ErrArtifactCorrupt when installing over corrupt file, got %v", err)
	}
}

// Finding 7: Production Crash Recovery Checkpoints via Hooks
func TestReview18F_CrashRecoveryHooks_Unit(t *testing.T) {
	tempDir := t.TempDir()
	var hookedBoundaries []string

	hook := func(boundary string) {
		hookedBoundaries = append(hookedBoundaries, boundary)
	}

	store, err := storage.Open(storage.StoreOptions{
		StateDir:             tempDir,
		TestHookBeforeCommit: hook,
	})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	setupRunAndSession(t, store, "run-1", "sess-1", "lease-1")

	// 1. ReleaseTurn fires pre_commit_release hook
	_, err = store.QueuePrompt(ctx, "op-q1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "test prompt", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	_, err = store.ReleaseTurn(ctx, "op-rel1", "lease-1", "sess-1", 2, "turn-1")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	// 2. PublishArtifact fires uncommitted_artifact_metadata hook
	meta := storage.ArtifactMetadata{
		ID:        "art-hook-1",
		RunID:     "run-1",
		SessionID: "sess-1",
		TurnKey:   "turn-1",
		Name:      "hook.txt",
	}
	_, err = store.PublishArtifact(ctx, "op-pub-hook", "lease-1", meta, []byte("artifact bytes"))
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	// Verify both hooks fired in sequence
	if len(hookedBoundaries) != 2 {
		t.Fatalf("expected 2 hooks to fire, got %v", hookedBoundaries)
	}
	if hookedBoundaries[0] != "pre_commit_release" || hookedBoundaries[1] != "uncommitted_artifact_metadata" {
		t.Fatalf("unexpected hook boundaries sequence: %v", hookedBoundaries)
	}
}
