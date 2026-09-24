package storage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestStore_HydrateState_ExactStateReconstruction(t *testing.T) {
	tempDir := t.TempDir()
	storeA, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open storeA: %v", err)
	}

	ctx := context.Background()
	_, err = storeA.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_1", "src_sha_1", "profile_sha_1", "lease-1")
	adoptControllerForTest(t, storeA, "run-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// 4 sessions
	for i := 1; i <= 4; i++ {
		sessID := fmt.Sprintf("sess-%d", i)
		_, err := storeA.CreateSession(ctx, fmt.Sprintf("op-sess-%d", i), "lease-1", storage.SessionRecord{
			ID: sessID, RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: (i == 1), State: "parked", Visibility: "reachable", RecoveryGeneration: uint64(i),
		})
		if err != nil {
			t.Fatalf("create session %d: %v", i, err)
		}
	}

	// Session 1: completed turn and native binding
	_, err = storeA.SetNativeBinding(ctx, "op-bind-1", "lease-1", "sess-1", 1, storage.NativeBinding{
		LogicalSessionID: "sess-1", NativeSessionID: "native-1", Harness: "claude-code", Model: "claude-3-7-sonnet", WorkspaceMode: "branch", ToolingConfig: "{\"tooling\":[\"full\"]}",
	})
	if err != nil {
		t.Fatalf("set binding 1: %v", err)
	}
	_, err = storeA.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 2, storage.PendingPrompt{SessionID: "sess-1", TurnKey: "turn-1", Prompt: "P1", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("queue prompt 1: %v", err)
	}
	_, err = storeA.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", 3, "turn-1")
	if err != nil {
		t.Fatalf("release turn 1: %v", err)
	}
	_, err = storeA.RecordTerminalOutcome(ctx, "op-term-1", "lease-1", "sess-1", 4, "turn-1", council.TurnCompleted, "finished")
	if err != nil {
		t.Fatalf("record terminal outcome 1: %v", err)
	}

	// Session 2: pending prompt
	_, err = storeA.QueuePrompt(ctx, "op-q-2", "lease-1", "sess-2", 1, storage.PendingPrompt{SessionID: "sess-2", TurnKey: "turn-2", Prompt: "Pending P2", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("queue prompt 2: %v", err)
	}

	// Session 3: uncertain turn (intent_recorded)
	_, err = storeA.QueuePrompt(ctx, "op-q-3", "lease-1", "sess-3", 1, storage.PendingPrompt{SessionID: "sess-3", TurnKey: "turn-3", Prompt: "Uncertain P3", CreatedAt: time.Now()})
	if err != nil {
		t.Fatalf("queue prompt 3: %v", err)
	}
	_, err = storeA.ReleaseTurn(ctx, "op-rel-3", "lease-1", "sess-3", 2, "turn-3")
	if err != nil {
		t.Fatalf("release turn 3: %v", err)
	}

	// Session 4: idle parked

	// Close Store A abruptly
	if err := storeA.Close(); err != nil {
		t.Fatalf("close storeA: %v", err)
	}

	// Store B opens same directory and reconstructs state
	storeB, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open storeB: %v", err)
	}
	defer storeB.Close()

	hydrated, err := storeB.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate state: %v", err)
	}

	// Verify runs
	if len(hydrated.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(hydrated.Runs))
	}
	if hydrated.Runs["run-1"].ControllerLease != "lease-1" {
		t.Fatalf("lease mismatch: %s", hydrated.Runs["run-1"].ControllerLease)
	}

	// Verify sessions
	if len(hydrated.Sessions) != 4 {
		t.Fatalf("expected 4 sessions, got %d", len(hydrated.Sessions))
	}

	// Session 1: completed turn, active_key == nil, has native binding
	s1 := hydrated.Sessions["sess-1"]
	if s1.State != "parked" {
		t.Fatalf("session 1 state expected parked, got %s", s1.State)
	}
	if s1.ActiveTurn != nil {
		t.Fatalf("session 1 active turn should be nil, got %v", s1.ActiveTurn)
	}
	if s1.Turns["turn-1"].Status != "completed" {
		t.Fatalf("session 1 turn-1 expected completed, got %s", s1.Turns["turn-1"].Status)
	}
	if s1.NativeBinding == nil || s1.NativeBinding.NativeSessionID != "native-1" {
		t.Fatalf("session 1 native binding missing or incorrect: %v", s1.NativeBinding)
	}

	// Session 2: pending prompt
	s2 := hydrated.Sessions["sess-2"]
	if len(s2.PendingPrompts) != 1 || s2.PendingPrompts["turn-2"].Prompt != "Pending P2" {
		t.Fatalf("session 2 pending prompt mismatch: %v", s2.PendingPrompts)
	}

	// Session 3: active turn with intent_recorded
	s3 := hydrated.Sessions["sess-3"]
	if s3.State != "running" {
		t.Fatalf("session 3 state expected running, got %s", s3.State)
	}
	if s3.ActiveTurn == nil || s3.ActiveTurn.TurnKey != "turn-3" {
		t.Fatalf("session 3 active turn mismatch: %v", s3.ActiveTurn)
	}
	if s3.ActiveIntent == nil || s3.ActiveIntent.Phase != "intent_recorded" {
		t.Fatalf("session 3 active intent expected intent_recorded, got %v", s3.ActiveIntent)
	}

	// Session 4: idle parked
	s4 := hydrated.Sessions["sess-4"]
	if s4.State != "parked" || len(s4.PendingPrompts) != 0 || s4.ActiveTurn != nil {
		t.Fatalf("session 4 expected clean parked, got state=%s, p=%v, t=%v", s4.State, s4.PendingPrompts, s4.ActiveTurn)
	}

	// Verify journal entries reconstructed
	if len(hydrated.Journals) == 0 {
		t.Fatalf("expected journal entries, got 0")
	}
}
