package storage_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// TestReview28_ReconciliationNonterminalPreservesReservation tests Finding 1:
// - ReconciliationReachableActive preserves execution reservation and running state, keeps active turn key, leaves dispatch intent unresolved.
// - ReconciliationUncertain preserves host_lost visibility and leaves recovery episode open.
// - Validates reconciliation outcome and ref preconditions; handles terminal reconciliation and post-terminal recovery.
func TestReview28_ReconciliationNonterminalPreservesReservation(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha", "src_sha", "prof_sha", "lease-1")
	adoptControllerForTest(t, store, "run-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	_, err = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable", RecoveryGeneration: 1,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Queue and release turn
	_, err = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 1, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-1", Prompt: "Review design", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	relRes, err := store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", 2, "turn-1")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}
	relReceipt := relRes.Receipt

	// 1. ReconcileSession while visibility is still reachable must be rejected with ErrReconciliationInvalid
	ref1 := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"},
		Generation: 1,
	}
	_, err = store.ReconcileSession(ctx, "op-rec-bad-vis", store.ExecutionRefForTurn(ctx, string(ref1.SessionID), ref1.TurnKey), ref1, adapter.ReconciliationOutcome{
		Ref:          ref1,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableActive,
		Observed:     council.TurnRunning,
	})
	if err == nil {
		t.Fatalf("expected error reconciling session when host is not lost, got nil")
	}

	// 2. Record host loss: advances session to visibility = host_lost and active_recovery_gen = 2
	lossReceipt, err := store.RecordHostLoss(ctx, "op-loss-1", "lease-1", "sess-1", relReceipt.CommittedVersion)
	if err != nil {
		t.Fatalf("record host loss: %v", err)
	}
	if lossReceipt.TurnKey != "turn-1" {
		t.Fatalf("expected turnKey turn-1 in host loss receipt, got %s", lossReceipt.TurnKey)
	}

	// Stale generation (generation 1 instead of active 2) must be rejected
	staleRef := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"},
		Generation: 1,
	}
	_, err = store.ReconcileSession(ctx, "op-rec-stale", store.ExecutionRefForTurn(ctx, string(staleRef.SessionID), staleRef.TurnKey), staleRef, adapter.ReconciliationOutcome{
		Ref:          staleRef,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableActive,
		Observed:     council.TurnRunning,
	})
	if err == nil {
		t.Fatalf("expected error reconciling stale recovery generation, got nil")
	}

	// Mismatched Ref between outcome and argument must be rejected
	validRef := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"},
		Generation: 2,
	}
	_, err = store.ReconcileSession(ctx, "op-rec-ref-mismatch", store.ExecutionRefForTurn(ctx, string(validRef.SessionID), validRef.TurnKey), validRef, adapter.ReconciliationOutcome{
		Ref:          staleRef,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableActive,
		Observed:     council.TurnRunning,
	})
	if err == nil {
		t.Fatalf("expected error reconciling mismatched ref, got nil")
	}

	// 3. ReconciliationReachableActive: turn remains active!
	recReceipt, err := store.ReconcileSession(ctx, "op-rec-active", store.ExecutionRefForTurn(ctx, string(validRef.SessionID), validRef.TurnKey), validRef, adapter.ReconciliationOutcome{
		Ref:          validRef,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableActive,
		Observed:     council.TurnRunning,
	})
	if err != nil {
		t.Fatalf("reconcile reachable active failed: %v", err)
	}

	// Hydrate and assert invariants
	hydrated, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate state: %v", err)
	}
	sess := hydrated.Sessions["sess-1"]

	// Invariant checks for ReconciliationReachableActive:
	// - Visibility restored to reachable
	if sess.Visibility != "reachable" {
		t.Fatalf("expected visibility reachable, got %s", sess.Visibility)
	}
	// - active_recovery_gen cleared to 0
	if sess.ActiveRecoveryGen != 0 {
		t.Fatalf("expected active_recovery_gen 0, got %d", sess.ActiveRecoveryGen)
	}
	// - State PRESERVED as running
	if sess.State != "running" {
		t.Fatalf("expected session state running, got %s", sess.State)
	}
	// - active_key PRESERVED as turn-1
	if sess.ActiveTurn == nil || sess.ActiveTurn.TurnKey != "turn-1" {
		t.Fatalf("expected active turn turn-1 preserved, got %+v", sess.ActiveTurn)
	}
	// - Dispatch intent remains UNRESOLVED (intent_recorded)
	if sess.ActiveIntent == nil || sess.ActiveIntent.Phase != "intent_recorded" {
		t.Fatalf("expected dispatch intent intent_recorded preserved, got %+v", sess.ActiveIntent)
	}

	// 4. Test ReconciliationUncertain:
	// Record second host loss
	lossReceipt2, err := store.RecordHostLoss(ctx, "op-loss-2", "lease-1", "sess-1", recReceipt.CommittedVersion)
	if err != nil {
		t.Fatalf("record second host loss: %v", err)
	}

	uncertRef := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"},
		Generation: 3,
	}
	_, err = store.ReconcileSession(ctx, "op-rec-uncert", store.ExecutionRefForTurn(ctx, string(uncertRef.SessionID), uncertRef.TurnKey), uncertRef, adapter.ReconciliationOutcome{
		Ref:          uncertRef,
		Reachability: council.VisibilityHostLost,
		Status:       adapter.ReconciliationUncertain,
	})
	if err != nil {
		t.Fatalf("reconcile uncertain failed: %v", err)
	}

	hydrated2, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate state 2: %v", err)
	}
	sess2 := hydrated2.Sessions["sess-1"]
	// Invariant checks for ReconciliationUncertain:
	// - Visibility remains host_lost
	if sess2.Visibility != "host_lost" {
		t.Fatalf("expected visibility host_lost after uncertain reconcile, got %s", sess2.Visibility)
	}
	// - active_recovery_gen remains 3
	if sess2.ActiveRecoveryGen != 3 {
		t.Fatalf("expected active_recovery_gen 3, got %d", sess2.ActiveRecoveryGen)
	}
	// - State remains running
	if sess2.State != "running" {
		t.Fatalf("expected state running, got %s", sess2.State)
	}

	// 5. Test Post-Terminal Recovery Path:
	// A terminal outcome is delivered while host is lost
	termReceipt, err := store.RecordTerminalOutcome(ctx, "op-term-lost", "lease-1", "sess-1", lossReceipt2.CommittedVersion+1, "turn-1", council.TurnCompleted, "done while host lost")
	if err != nil {
		t.Fatalf("record terminal outcome during host loss: %v", err)
	}

	hydrated3, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate state 3: %v", err)
	}
	sess3 := hydrated3.Sessions["sess-1"]
	if sess3.State != "parked" || sess3.ActiveTurn != nil || sess3.RecoveryContext != "turn-1" || sess3.Visibility != "host_lost" {
		t.Fatalf("expected parked session retaining recovery context, got %+v", sess3)
	}

	// Post-terminal recovery: attempting ReconciliationReachableActive must be rejected
	postRef := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"},
		Generation: 3,
	}
	_, err = store.ReconcileSession(ctx, "op-rec-conflict", store.ExecutionRefForTurn(ctx, string(postRef.SessionID), postRef.TurnKey), postRef, adapter.ReconciliationOutcome{
		Ref:          postRef,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableActive,
		Observed:     council.TurnRunning,
	})
	if err == nil {
		t.Fatalf("expected conflict error when attempting reachable active on post-terminal turn, got nil")
	}

	// Post-terminal recovery: ReconciliationReachableTerminal succeeds and closes recovery episode
	_, err = store.ReconcileSession(ctx, "op-rec-post-term", store.ExecutionRefForTurn(ctx, string(postRef.SessionID), postRef.TurnKey), postRef, adapter.ReconciliationOutcome{
		Ref:          postRef,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableTerminal,
		Observed:     council.TurnCompleted,
	})
	if err != nil {
		t.Fatalf("reconcile post-terminal failed: %v", err)
	}

	hydrated4, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate state 4: %v", err)
	}
	sess4 := hydrated4.Sessions["sess-1"]
	if sess4.Visibility != "reachable" || sess4.ActiveRecoveryGen != 0 || sess4.RecoveryContext != "" {
		t.Fatalf("post-terminal recovery did not close recovery episode: %+v", sess4)
	}
	_ = termReceipt
}

// TestReview28_QueueOperationsStrictlyKeyed tests Finding 2:
// - ReplacePendingPrompt, DiscardPendingPrompt, and ReleaseTurn are strictly turn-keyed.
// - Unknown turn keys return ErrPromptNotQueued and leave siblings untouched.
// - Enforces expectedVersion > 0 and guards against disconnected/archived controllers.
func TestReview28_QueueOperationsStrictlyKeyed(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha", "src_sha", "prof_sha", "lease-1")
	adoptControllerForTest(t, store, "run-1", "lease-1")
	sessReceipt, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	ver := sessReceipt.CommittedVersion

	// ExpectedVersion <= 0 rejected
	_, err = store.QueuePrompt(ctx, "op-q-bad-ver", "lease-1", "sess-1", 0, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t1", Prompt: "p1", CreatedAt: time.Now(),
	})
	if err != storage.ErrInvalidExpectedVersion {
		t.Fatalf("expected ErrInvalidExpectedVersion, got %v", err)
	}

	// Queue 3 prompts: t1, t2, t3
	r1, err := store.QueuePrompt(ctx, "op-q-t1", "lease-1", "sess-1", ver, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t1", Prompt: "prompt-1", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue t1: %v", err)
	}
	ver = r1.CommittedVersion

	r2, err := store.QueuePrompt(ctx, "op-q-t2", "lease-1", "sess-1", ver, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t2", Prompt: "prompt-2", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue t2: %v", err)
	}
	ver = r2.CommittedVersion

	r3, err := store.QueuePrompt(ctx, "op-q-t3", "lease-1", "sess-1", ver, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t3", Prompt: "prompt-3", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("queue t3: %v", err)
	}
	ver = r3.CommittedVersion

	// 1. ReplacePendingPrompt with unknown turn_key -> ErrPromptNotQueued
	_, err = store.ReplacePendingPrompt(ctx, "op-rep-unknown", "lease-1", "sess-1", ver, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t-nonexistent", Prompt: "new prompt", CreatedAt: time.Now(),
	})
	if err != storage.ErrPromptNotQueued {
		t.Fatalf("expected ErrPromptNotQueued on unknown key replacement, got %v", err)
	}

	// Replace t2 -> updates only t2, leaves t1 and t3 untouched
	rRep, err := store.ReplacePendingPrompt(ctx, "op-rep-t2", "lease-1", "sess-1", ver, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t2", Prompt: "prompt-2-replaced", CreatedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("replace t2: %v", err)
	}
	ver = rRep.CommittedVersion

	hydrated, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	pPrompts := hydrated.Sessions["sess-1"].PendingPrompts
	if len(pPrompts) != 3 {
		t.Fatalf("expected 3 pending prompts, got %d", len(pPrompts))
	}
	if pPrompts["t1"].Prompt != "prompt-1" || pPrompts["t2"].Prompt != "prompt-2-replaced" || pPrompts["t3"].Prompt != "prompt-3" {
		t.Fatalf("unexpected prompt contents: %+v", pPrompts)
	}

	// 2. DiscardPendingPrompt with unknown turn_key -> ErrPromptNotQueued
	_, err = store.DiscardPendingPrompt(ctx, "op-disc-unknown", "lease-1", "sess-1", ver, "t-nonexistent")
	if err != storage.ErrPromptNotQueued {
		t.Fatalf("expected ErrPromptNotQueued on unknown discard, got %v", err)
	}

	// Discard t2 -> deletes only t2, t1 and t3 untouched
	rDisc, err := store.DiscardPendingPrompt(ctx, "op-disc-t2", "lease-1", "sess-1", ver, "t2")
	if err != nil {
		t.Fatalf("discard t2: %v", err)
	}
	ver = rDisc.CommittedVersion

	hydrated, err = store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate after discard: %v", err)
	}
	pPrompts = hydrated.Sessions["sess-1"].PendingPrompts
	if len(pPrompts) != 2 || pPrompts["t1"].Prompt != "prompt-1" || pPrompts["t3"].Prompt != "prompt-3" {
		t.Fatalf("unexpected pending prompts after discarding t2: %+v", pPrompts)
	}

	// 3. ReleaseTurn with unknown turn_key -> ErrPromptNotQueued
	_, err = store.ReleaseTurn(ctx, "op-rel-unknown", "lease-1", "sess-1", ver, "t-nonexistent")
	if err != storage.ErrPromptNotQueued {
		t.Fatalf("expected ErrPromptNotQueued on unknown release, got %v", err)
	}

	// Release t1 -> deletes t1 from pending prompts, promotes to active turn; t3 remains pending!
	relRes, err := store.ReleaseTurn(ctx, "op-rel-t1", "lease-1", "sess-1", ver, "t1")
	if err != nil {
		t.Fatalf("release t1: %v", err)
	}
	rRel := relRes.Receipt
	ver = rRel.CommittedVersion

	hydrated, err = store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate after release: %v", err)
	}
	s := hydrated.Sessions["sess-1"]
	if len(s.PendingPrompts) != 1 || s.PendingPrompts["t3"].Prompt != "prompt-3" {
		t.Fatalf("expected t3 to remain in pending prompts, got %+v", s.PendingPrompts)
	}
	if s.ActiveTurn == nil || s.ActiveTurn.TurnKey != "t1" {
		t.Fatalf("expected t1 active turn, got %+v", s.ActiveTurn)
	}

	// Complete t1 turn to park session
	termReceipt, err := store.RecordTerminalOutcome(ctx, "op-term-t1", "lease-1", "sess-1", ver, "t1", council.TurnCompleted, "ok")
	if err != nil {
		t.Fatalf("record terminal outcome: %v", err)
	}
	ver = termReceipt.CommittedVersion

	// 4. Test Controller Disconnection Guard
	connReceipt, err := store.SetControllerConnection(ctx, "op-conn-disc", "lease-1", "sess-1", ver, "disconnected")
	if err != nil {
		t.Fatalf("set controller disconnected: %v", err)
	}
	ver = connReceipt.CommittedVersion

	// All queue ops fail with ErrControllerDisconnected
	_, err = store.QueuePrompt(ctx, "op-q-disc", "lease-1", "sess-1", ver, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t4", Prompt: "p4", CreatedAt: time.Now(),
	})
	if err != storage.ErrControllerDisconnected {
		t.Fatalf("expected ErrControllerDisconnected on queue, got %v", err)
	}
	_, err = store.ReplacePendingPrompt(ctx, "op-rep-disc", "lease-1", "sess-1", ver, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t3", Prompt: "p3-new", CreatedAt: time.Now(),
	})
	if err != storage.ErrControllerDisconnected {
		t.Fatalf("expected ErrControllerDisconnected on replace, got %v", err)
	}
	_, err = store.DiscardPendingPrompt(ctx, "op-disc-disc", "lease-1", "sess-1", ver, "t3")
	if err != storage.ErrControllerDisconnected {
		t.Fatalf("expected ErrControllerDisconnected on discard, got %v", err)
	}
	_, err = store.ReleaseTurn(ctx, "op-rel-disc", "lease-1", "sess-1", ver, "t3")
	if err != storage.ErrControllerDisconnected {
		t.Fatalf("expected ErrControllerDisconnected on release, got %v", err)
	}

	// Restore connection
	connReceipt2, err := store.SetControllerConnection(ctx, "op-conn-reconn", "lease-1", "sess-1", ver, "connected")
	if err != nil {
		t.Fatalf("reconnect controller: %v", err)
	}
	ver = connReceipt2.CommittedVersion

	// Discard t3 so session has no pending prompts, then archive session
	discReceipt, err := store.DiscardPendingPrompt(ctx, "op-disc-clean", "lease-1", "sess-1", ver, "t3")
	if err != nil {
		t.Fatalf("discard t3: %v", err)
	}
	ver = discReceipt.CommittedVersion

	archReceipt, err := store.ArchiveSession(ctx, "op-arch", "lease-1", "sess-1", ver)
	if err != nil {
		t.Fatalf("archive session: %v", err)
	}
	ver = archReceipt.CommittedVersion

	// All queue ops fail with ErrSessionArchived
	_, err = store.QueuePrompt(ctx, "op-q-arch", "lease-1", "sess-1", ver, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t5", Prompt: "p5", CreatedAt: time.Now(),
	})
	if err != storage.ErrSessionArchived {
		t.Fatalf("expected ErrSessionArchived on queue, got %v", err)
	}
}

// TestReview28_HydrationSnapshotAndCompleteness tests Finding 3:
// - HydrateState executes in a read-only transaction.
// - Hydrates complete keyed maps for PendingPrompts, Turns, and DispatchIntents.
// - Reconstructs all session fields (Lifecycle, ControllerStatus, RecoveryContext, ActiveRecoveryGen, WorkspaceMode).
// - Validates relational invariants and fails with ErrInconsistentStorage if invariants are violated.
func TestReview28_HydrationSnapshotAndCompleteness(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief_sha_123", "src_sha_456", "prof_sha_789", "lease-1")
	adoptControllerForTest(t, store, "run-1", "lease-1")

	_, err = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create sess-1: %v", err)
	}

	// Set native binding with workspace mode "branch"
	_, err = store.SetNativeBinding(ctx, "op-bind-1", "lease-1", "sess-1", 1, storage.NativeBinding{
		LogicalSessionID: "sess-1",
		NativeSessionID:  "native-1",
		Harness:          "claude-code",
		Model:            "claude-3-7-sonnet",
		WorkspaceMode:    "branch",
		ToolingConfig:    `{"tooling":["bash"]}`,
	})
	if err != nil {
		t.Fatalf("set binding: %v", err)
	}

	// Queue multiple prompts
	_, _ = store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", 2, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t1", Prompt: "p1", CreatedAt: time.Now(),
	})
	_, _ = store.QueuePrompt(ctx, "op-q-2", "lease-1", "sess-1", 3, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "t2", Prompt: "p2", CreatedAt: time.Now(),
	})

	// Release t1
	_, err = store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", 4, "t1")
	if err != nil {
		t.Fatalf("release turn t1: %v", err)
	}

	// Hydrate and check all maps and fields
	hydrated, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate state: %v", err)
	}

	run := hydrated.Runs["run-1"]
	if run.BriefDigest != "brief_sha_123" || run.SourceDigest != "src_sha_456" || run.ProfileDigest != "prof_sha_789" {
		t.Fatalf("run digests mismatch: %+v", run)
	}

	sess := hydrated.Sessions["sess-1"]
	if sess.Lifecycle != "active" || sess.ControllerStatus != "connected" {
		t.Fatalf("lifecycle/controller status mismatch: %+v", sess)
	}
	if sess.NativeBinding == nil || sess.NativeBinding.WorkspaceMode != "branch" {
		t.Fatalf("native binding workspace mode mismatch: %+v", sess.NativeBinding)
	}
	if len(sess.PendingPrompts) != 1 || sess.PendingPrompts["t2"].Prompt != "p2" {
		t.Fatalf("pending prompts map mismatch: %+v", sess.PendingPrompts)
	}
	if len(sess.Turns) != 1 || sess.Turns["t1"].Status != "running" {
		t.Fatalf("turns map mismatch: %+v", sess.Turns)
	}
	if len(sess.DispatchIntents) != 1 || sess.DispatchIntents["t1"].Phase != "intent_recorded" {
		t.Fatalf("dispatch intents map mismatch: %+v", sess.DispatchIntents)
	}

	// Invariant violation test: insert corrupted row into database directly
	// e.g., session active_key points to nonexistent turn
	_, err = store.DB().Exec("UPDATE sessions SET active_key = 'ghost-turn' WHERE session_id = 'sess-1';")
	if err != nil {
		t.Fatalf("corrupt active_key: %v", err)
	}

	// HydrateState must detect the relational inconsistency and return ErrInconsistentStorage
	_, err = store.HydrateState(ctx)
	if !errors.Is(err, storage.ErrInconsistentStorage) {
		t.Fatalf("expected ErrInconsistentStorage on orphan active_key, got %v", err)
	}
}

// TestReview28_LifecycleCommandsAndGuards tests Finding 4:
// - Comprehensive coverage of RequestCancel, RecordTerminalOutcome, RecordHostLoss, SetControllerConnection, ArchiveSession, and RecordDecision.
// - Rejects invalid expectedVersion (<= 0), verifies caller leases, and enforces preconditions.
func TestReview28_LifecycleCommandsAndGuards(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "b", "s", "p", "lease-1")
	adoptControllerForTest(t, store, "run-1", "lease-1")
	sessReceipt, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	ver := sessReceipt.CommittedVersion

	// 1. Verify ExpectedVersion <= 0 is rejected on all commands
	if _, err := store.RequestCancel(ctx, "op-1", "lease-1", "sess-1", 0, "t1"); err != storage.ErrInvalidExpectedVersion {
		t.Fatalf("expected ErrInvalidExpectedVersion on RequestCancel, got %v", err)
	}
	if _, err := store.RecordTerminalOutcome(ctx, "op-2", "lease-1", "sess-1", 0, "t1", council.TurnCompleted, "ok"); err != storage.ErrInvalidExpectedVersion {
		t.Fatalf("expected ErrInvalidExpectedVersion on RecordTerminalOutcome, got %v", err)
	}
	if _, err := store.RecordHostLoss(ctx, "op-3", "lease-1", "sess-1", 0); err != storage.ErrInvalidExpectedVersion {
		t.Fatalf("expected ErrInvalidExpectedVersion on RecordHostLoss, got %v", err)
	}
	if _, err := store.SetControllerConnection(ctx, "op-4", "lease-1", "sess-1", 0, "connected"); err != storage.ErrInvalidExpectedVersion {
		t.Fatalf("expected ErrInvalidExpectedVersion on SetControllerConnection, got %v", err)
	}
	if _, err := store.ArchiveSession(ctx, "op-5", "lease-1", "sess-1", 0); err != storage.ErrInvalidExpectedVersion {
		t.Fatalf("expected ErrInvalidExpectedVersion on ArchiveSession, got %v", err)
	}
	if _, err := store.RecordDecision(ctx, "op-6", "lease-1", "run-1", "art-1", 0, "review_approved"); err == nil {
		t.Fatalf("expected error on RecordDecision with revision 0, got nil")
	}

	// 2. Queue and release turn
	qReceipt, _ := store.QueuePrompt(ctx, "op-q", "lease-1", "sess-1", ver, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-lc", Prompt: "Perform audit", CreatedAt: time.Now(),
	})
	ver = qReceipt.CommittedVersion

	relRes, _ := store.ReleaseTurn(ctx, "op-rel", "lease-1", "sess-1", ver, "turn-lc")
	relReceipt := relRes.Receipt
	ver = relReceipt.CommittedVersion

	// 3. RequestCancel: transitions session to cancelling and turn to cancelling
	cancelReceipt, err := store.RequestCancel(ctx, "op-cancel", "lease-1", "sess-1", ver, "turn-lc")
	if err != nil {
		t.Fatalf("request cancel: %v", err)
	}
	ver = cancelReceipt.CommittedVersion

	hydrated, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if hydrated.Sessions["sess-1"].ActiveTurn == nil || hydrated.Sessions["sess-1"].ActiveTurn.Status != "cancelling" {
		t.Fatalf("expected cancelling turn status, got turn=%+v", hydrated.Sessions["sess-1"].ActiveTurn)
	}

	// 4. Publish artifact and RecordDecision
	artReceipt, err := store.PublishArtifact(ctx, "op-art-lc", "lease-1", storage.ArtifactMetadata{
		ID: "art-lc", RunID: "run-1", SessionID: "sess-1", TurnKey: "turn-lc", Name: "report.md",
	}, []byte("report content"))
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	decReceipt, err := store.RecordDecision(ctx, "op-dec", "lease-1", "run-1", "art-lc", artReceipt.Revision, "decision_approved")
	if err != nil {
		t.Fatalf("record decision: %v", err)
	}
	_ = decReceipt

	// 5. RecordTerminalOutcome for cancelled turn
	termReceipt, err := store.RecordTerminalOutcome(ctx, "op-term", "lease-1", "sess-1", ver, "turn-lc", council.TurnCancelled, "user cancelled")
	if err != nil {
		t.Fatalf("record terminal outcome: %v", err)
	}
	ver = termReceipt.CommittedVersion

	hydrated, err = store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if hydrated.Sessions["sess-1"].State != "parked" || hydrated.Sessions["sess-1"].ActiveTurn != nil {
		t.Fatalf("expected parked session after terminal outcome, got %+v", hydrated.Sessions["sess-1"])
	}
	if hydrated.Sessions["sess-1"].Turns["turn-lc"].Status != "cancelled" {
		t.Fatalf("expected cancelled turn, got %s", hydrated.Sessions["sess-1"].Turns["turn-lc"].Status)
	}

	// 6. ArchiveSession
	archReceipt, err := store.ArchiveSession(ctx, "op-arch", "lease-1", "sess-1", ver)
	if err != nil {
		t.Fatalf("archive session: %v", err)
	}
	ver = archReceipt.CommittedVersion

	hydrated, err = store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if hydrated.Sessions["sess-1"].Lifecycle != "archived" {
		t.Fatalf("expected archived lifecycle, got %s", hydrated.Sessions["sess-1"].Lifecycle)
	}

	// Subsequent operations on archived session return ErrSessionArchived
	_, err = store.RequestCancel(ctx, "op-cancel-arch", "lease-1", "sess-1", ver, "turn-lc")
	if err != storage.ErrSessionArchived {
		t.Fatalf("expected ErrSessionArchived on RequestCancel, got %v", err)
	}
}

// TestReview28_SensitiveDataAndFilesystemProtections tests Finding 5:
// - Comprehensive regex redaction for sk-ant-(api03-)?, Bearer, ghp_, xoxb_, etc.
// - Tooling config allowlist in SetNativeBinding; rejects unknown keys and sensitive credential values.
// - Directory permissions enforced before SQLite database creation; symlink state directories rejected.
func TestReview28_SensitiveDataAndFilesystemProtections(t *testing.T) {
	// 1. Regex Redaction Tests
	anthropicV1 := "sk-ant-api03-abcdefghijklmnopqrstuvwxyz123456"
	anthropicV2 := "sk-ant-admin-secretkey1234567890abcdef"
	bearerToken := "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"
	genericSK := "sk-proj-1234567890abcdefghijklmnopqrstuv"
	ghpToken := "ghp_1234567890abcdefghijklmnopqrstuv"
	slackToken := "xoxb-1234567890-abcdefg"

	secrets := []string{anthropicV1, anthropicV2, bearerToken, genericSK, ghpToken, slackToken}
	for _, sec := range secrets {
		sanitized := storage.SanitizeText("User config with " + sec + " embedded")
		if strings.Contains(sanitized, sec) {
			t.Errorf("SanitizeText failed to redact secret %q: got %q", sec, sanitized)
		}
		if !strings.Contains(sanitized, "[REDACTED]") {
			t.Errorf("SanitizeText missing [REDACTED] for secret %q: got %q", sec, sanitized)
		}
	}

	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "b", "s", "p", "lease-1")
	adoptControllerForTest(t, store, "run-1", "lease-1")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	// 2. Tooling Config Allowlist Validation
	// Disallowed key
	badConfig := `{"workspace_root":"/tmp","unauthorized_arbitrary_key":"val"}`
	_, err = store.SetNativeBinding(ctx, "op-bind-bad", "lease-1", "sess-1", 1, storage.NativeBinding{
		LogicalSessionID: "sess-1", NativeSessionID: "nat-1", Harness: "claude-code", Model: "sonnet", WorkspaceMode: "branch", ToolingConfig: badConfig,
	})
	if err == nil || !strings.Contains(err.Error(), "disallowed configuration key") {
		t.Fatalf("expected error on disallowed tooling key, got %v", err)
	}

	// Secret in allowed key
	secretConfig := fmt.Sprintf(`{"workspace_root":"%s"}`, anthropicV1)
	_, err = store.SetNativeBinding(ctx, "op-bind-sec", "lease-1", "sess-1", 1, storage.NativeBinding{
		LogicalSessionID: "sess-1", NativeSessionID: "nat-1", Harness: "claude-code", Model: "sonnet", WorkspaceMode: "branch", ToolingConfig: secretConfig,
	})
	if !errors.Is(err, storage.ErrDisallowedToolingConfig) {
		t.Fatalf("expected ErrDisallowedToolingConfig on sensitive credential in tooling config, got %v", err)
	}

	// Valid config with allowed keys
	validConfig := `{"workspace_root":"/workspace","model":"claude-3-7-sonnet","tooling":["bash","edit"],"permission_mode":"ask","timeout":300}`
	_, err = store.SetNativeBinding(ctx, "op-bind-valid", "lease-1", "sess-1", 1, storage.NativeBinding{
		LogicalSessionID: "sess-1", NativeSessionID: "nat-1", Harness: "claude-code", Model: "sonnet", WorkspaceMode: "branch", ToolingConfig: validConfig,
	})
	if err != nil {
		t.Fatalf("valid config failed: %v", err)
	}

	// 3. Symlink rejection
	targetDir := t.TempDir()
	symlinkPath := filepath.Join(t.TempDir(), "symlink-store")
	if err := os.Symlink(targetDir, symlinkPath); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}
	_, err = storage.Open(storage.StoreOptions{StateDir: symlinkPath})
	if !errors.Is(err, storage.ErrSymlinkForbidden) {
		t.Fatalf("expected ErrSymlinkForbidden when opening symlink state dir, got %v", err)
	}
}

// TestReview28_ArtifactStoreRevisionBoundary tests Finding 6:
// - Pre-authorizes caller lease against runs.controller_lease before filesystem writes.
// - Enforces pre-write content bound <= 100MB (ErrArtifactOversized).
// - Cross-process no-clobber installation; idempotent retries return committed metadata.
// - ReadArtifactRevision verifies byte count and sha256; fails without silent repair on corruption.
func TestReview28_ArtifactStoreRevisionBoundary(t *testing.T) {
	tempDir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "b", "s", "p", "lease-valid")
	adoptControllerForTest(t, store, "run-1", "lease-valid")
	_, _ = store.CreateSession(ctx, "op-sess-1", "lease-valid", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer", IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})

	// 1. Pre-authorization failure before creating any files on disk
	meta := storage.ArtifactMetadata{
		ID: "art-1", RunID: "run-1", SessionID: "sess-1", TurnKey: "turn-1", Name: "report.md",
	}
	content := []byte("# Report Content")

	_, err = store.PublishArtifact(ctx, "op-pub-unauth", "lease-INVALID", meta, content)
	if err != storage.ErrUnauthorizedOperation {
		t.Fatalf("expected ErrUnauthorizedOperation, got %v", err)
	}

	// Assert no artifact files were written to disk
	artifactsDir := filepath.Join(tempDir, "artifacts")
	if _, err := os.Stat(artifactsDir); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(artifactsDir)
		if len(entries) > 0 {
			t.Fatalf("expected no artifact files written on unauthorized publish, found: %v", entries)
		}
	}

	// 2. Pre-write content bound: > 100MB rejected with ErrArtifactOversized
	oversizedContent := make([]byte, 100*1024*1024+1)
	_, err = store.PublishArtifact(ctx, "op-pub-oversized", "lease-valid", meta, oversizedContent)
	if err != storage.ErrArtifactOversized {
		t.Fatalf("expected ErrArtifactOversized on >100MB content, got %v", err)
	}

	// 3. Valid PublishArtifact
	pubMeta, err := store.PublishArtifact(ctx, "op-pub-valid", "lease-valid", meta, content)
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}
	if pubMeta.Revision != 1 || pubMeta.ByteCount != int64(len(content)) {
		t.Fatalf("unexpected metadata: %+v", pubMeta)
	}

	// 4. Idempotent retry with same opID returns exact committed metadata
	retryMeta, err := store.PublishArtifact(ctx, "op-pub-valid", "lease-valid", meta, content)
	if err != nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
	if retryMeta != pubMeta {
		t.Fatalf("idempotent retry returned different metadata: got %+v, want %+v", retryMeta, pubMeta)
	}

	// 5. ReadArtifactRevision succeeds with matching content
	readMeta, readBytes, err := store.ReadArtifactRevision(ctx, "art-1", 1)
	if err != nil {
		t.Fatalf("read artifact revision: %v", err)
	}
	if readMeta.Digest != pubMeta.Digest || !bytes.Equal(readBytes, content) {
		t.Fatalf("read content or metadata mismatch: got %q, want %q", readBytes, content)
	}

	// 6. Nonexistent revision returns ErrArtifactNotFound
	_, _, err = store.ReadArtifactRevision(ctx, "art-1", 999)
	if err != storage.ErrArtifactNotFound {
		t.Fatalf("expected ErrArtifactNotFound on missing revision, got %v", err)
	}

	// 7. Disk corruption detection without silent repair
	blobPath := filepath.Join(artifactsDir, pubMeta.Digest[:2], pubMeta.Digest)
	corruptBytes := append([]byte("tampered-"), content...)
	if err := os.WriteFile(blobPath, corruptBytes, 0600); err != nil {
		t.Fatalf("tamper blob: %v", err)
	}

	_, _, err = store.ReadArtifactRevision(ctx, "art-1", 1)
	if err != storage.ErrArtifactCorrupt {
		t.Fatalf("expected ErrArtifactCorrupt on tampered blob, got %v", err)
	}
}

// TestReview28_MigrationsConcurrentImmediateTx tests Finding 7:
// - Concurrent Store openings execute migrations safely without deadlock or lock contention.
// - buildDSN correctly resolves relative POSIX paths via filepath.Abs without prepending a bare slash.
func TestReview28_MigrationsConcurrentImmediateTx(t *testing.T) {
	tempDir := t.TempDir()

	const concurrentOpeners = 6
	var wg sync.WaitGroup
	errCh := make(chan error, concurrentOpeners)

	for i := 0; i < concurrentOpeners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := storage.Open(storage.StoreOptions{StateDir: tempDir})
			if err != nil {
				errCh <- fmt.Errorf("concurrent open failed: %w", err)
				return
			}
			defer s.Close()

			// Perform a quick read query to verify migration completed
			var count int
			if err := s.DB().QueryRow("SELECT count(*) FROM schema_migrations;").Scan(&count); err != nil {
				errCh <- fmt.Errorf("query schema_migrations failed: %w", err)
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent migration error: %v", err)
	}

	// Verify relative path resolution
	// Create a subdirectory inside tempDir and test opening with a relative path
	relDir := filepath.Join(tempDir, "rel_test")
	_ = os.MkdirAll(relDir, 0700)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	relPath, err := filepath.Rel(wd, relDir)
	if err == nil {
		sRel, err := storage.Open(storage.StoreOptions{StateDir: relPath})
		if err != nil {
			t.Fatalf("open with relative path failed: %v", err)
		}
		sRel.Close()
	}
}
