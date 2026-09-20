package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_Idempotency_InsideTx(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	// Initial QueuePrompt
	r1, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "Review this diff", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("first queue prompt: %v", err)
	}

	// Simultaneous / duplicate QueuePrompt with identical op_id and parameters -> returns original receipt
	r2, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "Review this diff", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("duplicate queue prompt: %v", err)
	}
	if r1.OpID != r2.OpID || r1.CommittedVersion != r2.CommittedVersion {
		t.Fatalf("expected identical receipt on retry: r1=%+v, r2=%+v", r1, r2)
	}

	// Verify only 1 journal record was created for op-q-1
	var journalCount int
	if err := store.DB().QueryRow("SELECT count(*) FROM journal_entries WHERE op_id = 'op-q-1';").Scan(&journalCount); err != nil {
		t.Fatalf("query journal count: %v", err)
	}
	if journalCount != 1 {
		t.Fatalf("expected exactly 1 journal entry, got %d", journalCount)
	}

	// Conflicting reuse of same op_id with different prompt text -> ErrIdempotencyConflict
	_, err = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "DIFFERENT PROMPT TEXT", CreatedAt: time.Now(),
	})
	if err != storage.ErrIdempotencyConflict {
		t.Fatalf("expected ErrIdempotencyConflict on mismatched parameters, got %v", err)
	}

	// Unauthorized caller retrying existing op_id -> ErrUnauthorizedOperation
	_, err = store.QueuePrompt(ctx, "op-q-1", "lease-attacker", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "Review this diff", CreatedAt: time.Now(),
	})
	if err != storage.ErrUnauthorizedOperation {
		t.Fatalf("expected ErrUnauthorizedOperation on unauthorized receipt lookup, got %v", err)
	}
}
