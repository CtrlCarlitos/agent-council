package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_ReleaseGuards_HostLossBlocksRelease(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "host_lost",
	})
	_, _ = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "Review diff", CreatedAt: time.Now(),
	})

	// ReleaseTurn on session in VisibilityHostLost must be rejected with ErrHostLost
	_, err = store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", 2, "turn-1")
	if err != storage.ErrHostLost {
		t.Fatalf("expected ErrHostLost when releasing turn on host_lost session, got %v", err)
	}
}

func TestStore_NativeBindings_MultipleSessionsSameContributor(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")

	// Session 1: active contributor for claude
	_, err = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session 1: %v", err)
	}

	// Bind Session 1 to native workspace 1
	_, err = store.SetNativeBinding(ctx, "op-bind-1", "lease-1", "sess-1", 1, storage.NativeBinding{
		LogicalSessionID: "sess-1", NativeSessionID: "native-agent-101", Harness: "claude-code", Model: "claude-3-7-sonnet", WorkspaceMode: "branch", ToolingConfig: `{"tooling":["full"]}`,
	})
	if err != nil {
		t.Fatalf("bind session 1: %v", err)
	}

	// Session 2: historical contributor for claude in same run (IsActiveContributor = false)
	_, err = store.CreateSession(ctx, "op-sess-2", "lease-1", storage.SessionRecord{
		ID: "sess-2", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: false, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session 2: %v", err)
	}

	// Bind Session 2 to native workspace 2
	_, err = store.SetNativeBinding(ctx, "op-bind-2", "lease-1", "sess-2", 1, storage.NativeBinding{
		LogicalSessionID: "sess-2", NativeSessionID: "native-agent-102", Harness: "claude-code", Model: "claude-3-7-sonnet", WorkspaceMode: "share", ToolingConfig: `{"tooling":["read"]}`,
	})
	if err != nil {
		t.Fatalf("bind session 2: %v", err)
	}
}

func TestStore_ReconcileSession_GenerationAndTurnValidation(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable", RecoveryGeneration: 1,
	})
	_, _ = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "Review diff", CreatedAt: time.Now(),
	})
	relRes, err := store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", 2, "turn-1")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}
	relReceipt := relRes.Receipt
	if relReceipt.SanitizedPrompt != "Review diff" {
		t.Fatalf("expected sanitized prompt in release receipt, got %s", relReceipt.SanitizedPrompt)
	}

	// Record host loss so visibility becomes host_lost and active_recovery_gen becomes 2
	_, err = store.RecordHostLoss(ctx, "op-loss-1", "lease-1", "sess-1", 3)
	if err != nil {
		t.Fatalf("record host loss: %v", err)
	}

	// Reconcile with mismatched turn key must be rejected
	refMismatch := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-WRONG"},
		Generation: 2,
	}
	_, err = store.ReconcileSession(ctx, "op-rec-mismatch", "lease-1", refMismatch, adapter.ReconciliationOutcome{
		Ref: refMismatch, Reachability: council.VisibilityReachable, Status: adapter.ReconciliationReachableTerminal, Observed: council.TurnCompleted,
	})
	if err == nil {
		t.Fatalf("expected error reconciling mismatched turn key, got nil")
	}

	// Reconcile with stale generation must be rejected
	refStale := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"},
		Generation: 1,
	}
	_, err = store.ReconcileSession(ctx, "op-rec-stale", "lease-1", refStale, adapter.ReconciliationOutcome{
		Ref: refStale, Reachability: council.VisibilityReachable, Status: adapter.ReconciliationReachableTerminal, Observed: council.TurnCompleted,
	})
	if err == nil {
		t.Fatalf("expected error reconciling stale recovery generation, got nil")
	}

	// Valid reconciliation succeeds
	refValid := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"},
		Generation: 2,
	}
	recReceipt, err := store.ReconcileSession(ctx, "op-rec-valid", "lease-1", refValid, adapter.ReconciliationOutcome{
		Ref: refValid, Reachability: council.VisibilityReachable, Status: adapter.ReconciliationReachableTerminal, Observed: council.TurnCompleted, Result: "approved",
	})
	if err != nil {
		t.Fatalf("valid reconciliation failed: %v", err)
	}
	if recReceipt.TurnKey != "turn-1" {
		t.Fatalf("expected turnKey turn-1 in receipt, got %s", recReceipt.TurnKey)
	}
}

func TestStore_RecordDispatchObservation_LateArrivalIgnored(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable", RecoveryGeneration: 1,
	})
	_, _ = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "Review diff", CreatedAt: time.Now(),
	})
	_, _ = store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", 2, "turn-1")

	// Record host loss
	_, err = store.RecordHostLoss(ctx, "op-loss-1", "lease-1", "sess-1", 3)
	if err != nil {
		t.Fatalf("record host loss: %v", err)
	}

	// Reconcile turn as terminal -> intent resolved
	ref := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"},
		Generation: 2,
	}
	_, err = store.ReconcileSession(ctx, "op-rec-1", "lease-1", ref, adapter.ReconciliationOutcome{
		Ref: ref, Reachability: council.VisibilityReachable, Status: adapter.ReconciliationReachableTerminal, Observed: council.TurnCompleted,
	})
	if err != nil {
		t.Fatalf("reconcile turn: %v", err)
	}

	// Late dispatch observation arriving after resolution is ignored as a clean no-op
	r, err := store.RecordDispatchObservation(ctx, "op-obs-late", "lease-1", "sess-1", "turn-1", "receipt_acknowledged")
	if err != nil {
		t.Fatalf("expected late observation to be ignored without error, got %v", err)
	}
	if r.Payload != "late_observation_ignored" {
		t.Fatalf("expected payload late_observation_ignored, got %s", r.Payload)
	}
}
