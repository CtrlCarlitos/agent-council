package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_OptimisticConcurrency_StaleUpdateRejected(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	adoptControllerForTest(t, store, "run-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	sessReceipt, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	initialVersion := sessReceipt.CommittedVersion
	if initialVersion != 1 {
		t.Fatalf("expected initial version 1, got %d", initialVersion)
	}

	// Queue a prompt with expectedVersion = initialVersion -> succeeds, bumps to initialVersion + 1
	r1, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", initialVersion, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "first prompt", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue prompt 1: %v", err)
	}
	if r1.CommittedVersion != initialVersion+1 {
		t.Fatalf("expected version %d, got %d", initialVersion+1, r1.CommittedVersion)
	}

	// Attempt to replace prompt with STALE expectedVersion = initialVersion -> rejected with ErrStaleUpdate
	_, err = store.ReplacePendingPrompt(ctx, "op-rep-stale", "lease-1", "sess-1", initialVersion, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "stale prompt", CreatedAt: time.Now(),
	})
	if err != storage.ErrStaleUpdate {
		t.Fatalf("expected ErrStaleUpdate on stale replacement, got %v", err)
	}

	// Replacement with correct expectedVersion = initialVersion + 1 succeeds
	r2, err := store.ReplacePendingPrompt(ctx, "op-rep-valid", "lease-1", "sess-1", initialVersion+1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "valid prompt replacement", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("valid replacement failed: %v", err)
	}
	if r2.CommittedVersion != initialVersion+2 {
		t.Fatalf("expected version %d, got %d", initialVersion+2, r2.CommittedVersion)
	}

	// Discard with correct expectedVersion succeeds
	r3, err := store.DiscardPendingPrompt(ctx, "op-disc-1", "lease-1", "sess-1", initialVersion+2, "turn-1")
	if err != nil {
		t.Fatalf("discard pending prompt: %v", err)
	}
	if r3.CommittedVersion != initialVersion+3 {
		t.Fatalf("expected version %d, got %d", initialVersion+3, r3.CommittedVersion)
	}
}

func TestStore_Authority_LeaseValidation(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-valid")
	adoptControllerForTest(t, store, "run-1", "lease-valid")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Caller with invalid lease must be rejected with ErrUnauthorizedOperation
	_, err = store.CreateSession(ctx, "op-sess-1", "lease-wrong", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != storage.ErrUnauthorizedOperation {
		t.Fatalf("expected ErrUnauthorizedOperation on wrong lease, got %v", err)
	}
}
