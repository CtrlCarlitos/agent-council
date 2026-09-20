package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// Regression evidence for AC-004 Task 4: evidence persistence for accepted
// executions authorizes by the complete persisted execution identity —
// session, turn, and attempt — and never depends on the issuing
// controller's lease remaining current.

func seedAcceptedExecution(t *testing.T, store *Store, lease, turnKey string) ExecutionRef {
	t.Helper()
	ctx := context.Background()
	ver, err := store.GetSessionVersion(ctx, "sess-auth")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-"+turnKey, lease, "sess-auth", ver, PendingPrompt{SessionID: "sess-auth", TurnKey: turnKey, Prompt: "p"}); err != nil {
		t.Fatalf("queue %s: %v", turnKey, err)
	}
	ver, _ = store.GetSessionVersion(ctx, "sess-auth")
	rel, err := store.ReleaseTurn(ctx, "op-rel-"+turnKey, lease, "sess-auth", ver, turnKey)
	if err != nil {
		t.Fatalf("release %s: %v", turnKey, err)
	}
	return ExecutionRef{SessionID: "sess-auth", TurnKey: turnKey, AttemptID: rel.Receipt.AttemptID}
}

func TestAC004_ObservedOutcomeRejectsWrongAttempt(t *testing.T) {
	store, _, _ := adoptedFixture(t, "lease-A")
	defer store.Close()
	ref := seedAcceptedExecution(t, store, "lease-A", "t-wrong-att")

	wrong := ref
	wrong.AttemptID = "att_999_forged"
	if _, err := store.RecordObservedExecutionOutcome(context.Background(), "op-term-wrong", wrong, council.TurnCompleted, "r"); !errors.Is(err, ErrWrongExecutionAttempt) {
		t.Fatalf("expected ErrWrongExecutionAttempt, got %v", err)
	}

	// The turn remains running; nothing was committed.
	d, err := store.GetTurnDetails(context.Background(), "sess-auth", "t-wrong-att")
	if err != nil || d.Status != council.TurnRunning {
		t.Fatalf("turn must remain running after wrong-attempt rejection: %+v err=%v", d, err)
	}
}

func TestAC004_ObservedOutcomePersistsAfterHandoffWithIssuingGeneration(t *testing.T) {
	store, runID, _ := adoptedFixture(t, "lease-A")
	defer store.Close()
	ref := seedAcceptedExecution(t, store, "lease-A", "t-rotate")

	// Rotate authority away from the issuing controller mid-flight.
	if _, err := store.HandoffController(context.Background(), "op-handoff-mid", runID, "lease-A", 1, "codex", "ref-b", "lease-B"); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	// The supervisor's evidence write still succeeds and journals the
	// issuing generation (1), never the current controller.
	receipt, err := store.RecordObservedExecutionOutcome(context.Background(), "op-term-rotate", ref, council.TurnCompleted, "verified result")
	if err != nil {
		t.Fatalf("observed outcome after handoff: %v", err)
	}
	if receipt.Payload != "completed:issued_by_generation=1" {
		t.Fatalf("journal must record issuing generation 1, got %q", receipt.Payload)
	}
	d, _ := store.GetTurnDetails(context.Background(), "sess-auth", "t-rotate")
	if d.Status != council.TurnCompleted || d.Result != "verified result" {
		t.Fatalf("outcome not persisted: %+v", d)
	}

	// Duplicate terminal evidence is an exact-duplicate receipt; conflicting
	// evidence is rejected.
	dup, err := store.RecordObservedExecutionOutcome(context.Background(), "op-term-rotate-dup", ref, council.TurnCompleted, "verified result")
	if err != nil || dup.Payload != "completed:duplicate" {
		t.Fatalf("exact duplicate must return duplicate receipt: %+v err=%v", dup, err)
	}
	if _, err := store.RecordObservedExecutionOutcome(context.Background(), "op-term-rotate-conf", ref, council.TurnFailed, "different"); !errors.Is(err, ErrConflictingTerminalOutcome) {
		t.Fatalf("conflicting terminal evidence must be rejected, got %v", err)
	}
}

func TestAC004_DispatchAcknowledgementAfterRotation(t *testing.T) {
	store, runID, _ := adoptedFixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// Accepted execution whose intent is still 'intent_recorded'.
	ver, _ := store.GetSessionVersion(ctx, "sess-auth")
	if _, err := store.QueuePrompt(ctx, "op-q-ack", "lease-A", "sess-auth", ver, PendingPrompt{SessionID: "sess-auth", TurnKey: "t-ack", Prompt: "p"}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	ver, _ = store.GetSessionVersion(ctx, "sess-auth")
	rel, err := store.ReleaseTurn(ctx, "op-rel-ack", "lease-A", "sess-auth", ver, "t-ack")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	ref := ExecutionRef{SessionID: "sess-auth", TurnKey: "t-ack", AttemptID: rel.Receipt.AttemptID}

	if _, err := store.HandoffController(ctx, "op-handoff-ack", runID, "lease-A", 1, "agy", "ref-c", "lease-B"); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	receipt, err := store.RecordObservedDispatchAcknowledgement(ctx, "op-obs-ack", ref, "receipt_acknowledged")
	if err != nil {
		t.Fatalf("dispatch acknowledgement after rotation: %v", err)
	}
	if receipt.Payload != "receipt_acknowledged:issued_by_generation=1" {
		t.Fatalf("acknowledgement must journal the issuing generation, got %q", receipt.Payload)
	}
}

func TestAC004_ReconcileCommitSurvivesMidProbeHandoff(t *testing.T) {
	store, runID, _ := adoptedFixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// Accepted execution, then the controller-authority probe window opens
	// an episode (handler already classified authority at initiation).
	ref := seedAcceptedExecution(t, store, "lease-A", "t-mid")
	_ = ref
	hlVer, _ := store.GetSessionVersion(ctx, "sess-auth")
	hl, err := store.RecordHostLoss(ctx, "op-mid:host_loss", "lease-A", "sess-auth", hlVer)
	if err != nil {
		t.Fatalf("host loss: %v", err)
	}
	var gen uint64
	if _, err := fmt.Sscanf(hl.Payload, "%d", &gen); err != nil || gen == 0 {
		t.Fatalf("invalid generation payload %q: %v", hl.Payload, err)
	}

	// Authority rotates while the authorized probe is in flight.
	if _, err := store.HandoffController(ctx, "op-handoff-mid2", runID, "lease-A", 1, "codex", "ref-b", "lease-B"); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	// The verified observation still commits atomically: episode closes,
	// turn resolves, superseded lease not required.
	rref := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID("sess-auth"), TurnKey: "t-mid"},
		Generation: gen,
	}
	outcome := adapter.ReconciliationOutcome{
		Ref:          rref,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableTerminal,
		Observed:     council.TurnCompleted,
		Result:       "verified while rotated",
	}
	if _, err := store.ReconcileSession(ctx, "op-mid:reconcile", "lease-A", rref, outcome); err != nil {
		t.Fatalf("reconcile commit after mid-probe handoff: %v", err)
	}
	d, _ := store.GetTurnDetails(ctx, "sess-auth", "t-mid")
	if d.Status != council.TurnCompleted || d.Result != "verified while rotated" {
		t.Fatalf("reconciled outcome not persisted: %+v", d)
	}
	rs, err := store.GetSessionRecoveryState(ctx, "sess-auth")
	if err != nil || rs.Visibility != "reachable" || rs.ActiveRecoveryGen != 0 {
		t.Fatalf("recovery episode not closed atomically: %+v err=%v", rs, err)
	}

	// A stale recovery generation is still rejected: episode-scoped
	// validity did not move with the controller change.
	stale := rref
	stale.Generation = gen + 1
	outcome.Ref = stale
	if _, err := store.ReconcileSession(ctx, "op-mid:stale", "lease-B", stale, outcome); err == nil {
		t.Fatal("stale recovery generation must be rejected")
	}
}
