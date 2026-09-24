package storage_test

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStorage_ReleaseDisposition_NewReplayedConflict(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	adoptControllerForTest(t, store, "run-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID:                  "sess-1",
		RunID:               "run-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	qRec, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-1",
		Prompt:    "Hello",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	// 1. First release: returns ReleaseDispositionNew
	res1, err := store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", qRec.CommittedVersion, "t-1")
	if err != nil {
		t.Fatalf("first release failed: %v", err)
	}
	if res1.Disposition != storage.ReleaseDispositionNew {
		t.Fatalf("expected ReleaseDispositionNew, got %v", res1.Disposition)
	}
	if res1.Receipt.TurnKey != "t-1" {
		t.Fatalf("unexpected turn key in receipt: %s", res1.Receipt.TurnKey)
	}

	// 2. Idempotent replay: same op_id and parameters returns ReleaseDispositionReplayed with identical receipt
	res2, err := store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", qRec.CommittedVersion, "t-1")
	if err != nil {
		t.Fatalf("replay release failed: %v", err)
	}
	if res2.Disposition != storage.ReleaseDispositionReplayed {
		t.Fatalf("expected ReleaseDispositionReplayed, got %v", res2.Disposition)
	}
	if res2.Receipt != res1.Receipt {
		t.Fatalf("expected identical receipt, got %+v vs %+v", res2.Receipt, res1.Receipt)
	}

	// 3. Conflicting op_id: same op_id with different turn_key or parameters fails
	_, err = store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", qRec.CommittedVersion, "t-other")
	if err == nil {
		t.Fatal("expected error on conflicting release replay, got nil")
	}

	// 4. Replay after store reopen: preserve ReleaseDispositionReplayed and receipt
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	store2, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store2.Close()

	res3, err := store2.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", qRec.CommittedVersion, "t-1")
	if err != nil {
		t.Fatalf("replay after reopen failed: %v", err)
	}
	if res3.Disposition != storage.ReleaseDispositionReplayed || res3.Receipt != res1.Receipt {
		t.Fatalf("expected replayed receipt matching original after reopen, got %+v", res3)
	}
}
