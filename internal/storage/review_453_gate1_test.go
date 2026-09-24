package storage

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// Gate 1 re-review regressions (head 4534fd0): one execution must not
// validate another's recovery, one revocation request must not stand in for
// another generation, and recovery intent is validated consistently.

// seedRecoveryEpisode releases a turn and opens a recovery episode on it,
// returning the recovery generation and the turn's execution reference.
func seedRecoveryEpisode(t *testing.T, store *Store, sessID, turnKey string) (uint64, ExecutionRef) {
	t.Helper()
	ctx := context.Background()
	ver, err := store.GetSessionVersion(ctx, sessID)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-"+sessID+"-"+turnKey, "lease-A", sessID, ver, PendingPrompt{SessionID: sessID, TurnKey: turnKey, Prompt: "p"}); err != nil {
		t.Fatalf("queue %s: %v", turnKey, err)
	}
	ver, _ = store.GetSessionVersion(ctx, sessID)
	rel, err := store.ReleaseTurn(ctx, "op-rel-"+sessID+"-"+turnKey, "lease-A", sessID, ver, turnKey)
	if err != nil {
		t.Fatalf("release %s: %v", turnKey, err)
	}
	ver, _ = store.GetSessionVersion(ctx, sessID)
	hl, err := store.RecordHostLoss(ctx, "op-hl-"+sessID+"-"+turnKey, "lease-A", sessID, ver)
	if err != nil {
		t.Fatalf("host loss %s: %v", turnKey, err)
	}
	var gen uint64
	if _, err := fmt.Sscanf(hl.Payload, "%d", &gen); err != nil || gen == 0 {
		t.Fatalf("invalid generation payload %q: %v", hl.Payload, err)
	}
	return gen, ExecutionRef{SessionID: sessID, TurnKey: turnKey, AttemptID: rel.Receipt.AttemptID}
}

func uncertain(ref adapter.RecoveryRef) adapter.ReconciliationOutcome {
	return adapter.ReconciliationOutcome{
		Ref:          ref,
		Reachability: council.VisibilityHostLost,
		Status:       adapter.ReconciliationUncertain,
		Observed:     council.TurnRunning,
	}
}

// A correct pair reconciles; a valid execution reference from ANOTHER
// session — or an older valid turn in the SAME session — cannot validate
// the current recovery. Rejections leave the target and episode unchanged.
func TestGate1Review453_ReconcileBindsExecutionToRecovery(t *testing.T) {
	store, runID, sessA := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// A second session in the same run with its own accepted execution
	// using the SAME turn key.
	if _, err := store.CreateSession(ctx, "op-sess-g1b", "lease-A", SessionRecord{
		ID: "sess-g1b", RunID: runID, Contributor: "codex", Role: "reviewer",
		IsActiveContributor: false, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create second session: %v", err)
	}
	_, execB := seedRecoveryEpisode(t, store, "sess-g1b", "t-pair")

	// An older accepted execution in the same session (no open episode).
	ver, err := store.GetSessionVersion(ctx, sessA)
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-old", "lease-A", sessA, ver, PendingPrompt{SessionID: sessA, TurnKey: "t-old", Prompt: "p"}); err != nil {
		t.Fatalf("queue t-old: %v", err)
	}
	ver, _ = store.GetSessionVersion(ctx, sessA)
	relOld, err := store.ReleaseTurn(ctx, "op-rel-old", "lease-A", sessA, ver, "t-old")
	if err != nil {
		t.Fatalf("release t-old: %v", err)
	}
	execOld := ExecutionRef{SessionID: sessA, TurnKey: "t-old", AttemptID: relOld.Receipt.AttemptID}
	// Terminate t-old so the session can accept the target turn.
	ver, _ = store.GetSessionVersion(ctx, sessA)
	if _, err := store.RecordTerminalOutcome(ctx, "op-term-old", "lease-A", sessA, ver, "t-old", council.TurnCompleted, "old work"); err != nil {
		t.Fatalf("terminate t-old: %v", err)
	}

	// The current recovery episode lives on sessA/t-pair.
	gen, execA := seedRecoveryEpisode(t, store, sessA, "t-pair")
	refA := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID(sessA), TurnKey: "t-pair"},
		Generation: gen,
	}

	// Correct pair succeeds.
	if _, err := store.ReconcileSession(ctx, "op-453:ok", execA, refA, uncertain(refA)); err != nil {
		t.Fatalf("correct pair must reconcile: %v", err)
	}

	// Re-open an episode on the target for the rejection cases.
	ver, _ = store.GetSessionVersion(ctx, sessA)
	hl, err := store.RecordHostLoss(ctx, "op-hl-453-2", "lease-A", sessA, ver)
	if err != nil {
		t.Fatalf("second host loss: %v", err)
	}
	var gen2 uint64
	if _, err := fmt.Sscanf(hl.Payload, "%d", &gen2); err != nil || gen2 == 0 {
		t.Fatalf("invalid second generation: %q", hl.Payload)
	}
	refA2 := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID(sessA), TurnKey: "t-pair"},
		Generation: gen2,
	}

	// A valid execution from another session (same turn key) is rejected.
	if _, err := store.ReconcileSession(ctx, "op-453:cross", execB, refA2, uncertain(refA2)); !errors.Is(err, ErrWrongExecutionAttempt) {
		t.Fatalf("cross-session execution reference must be rejected, got %v", err)
	}

	// An older valid turn in the same session cannot authorize the current
	// recovery.
	if _, err := store.ReconcileSession(ctx, "op-453:old", execOld, refA2, uncertain(refA2)); !errors.Is(err, ErrWrongExecutionAttempt) {
		t.Fatalf("older same-session execution reference must be rejected, got %v", err)
	}

	// Rejections left the target and its recovery episode unchanged.
	rs, err := store.GetSessionRecoveryState(ctx, sessA)
	if err != nil || rs.Visibility != "host_lost" || rs.ActiveRecoveryGen != gen2 || rs.ActiveKey != "t-pair" {
		t.Fatalf("rejection must leave the target and episode unchanged: %+v err=%v", rs, err)
	}
	d, err := store.GetTurnDetails(ctx, sessA, "t-pair")
	if err != nil || d.Status != council.TurnRunning {
		t.Fatalf("target turn must remain running after rejections: %+v err=%v", d, err)
	}
}

// A revocation request for a different target generation cannot replay an
// earlier generation's successful receipt.
func TestGate1Review453_RevokeReplayBindsTargetGeneration(t *testing.T) {
	store, runID, _ := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// Revoke generation 1 with operation X and reason R.
	if _, err := store.RevokeController(ctx, "op-X", runID, "", &OperatorRecovery{Reason: "R", ExpectedGeneration: 1}); err != nil {
		t.Fatalf("revoke gen 1: %v", err)
	}
	// Adopt generation 2.
	if _, err := store.AdoptController(ctx, "op-adopt-2", runID, "agy", "ref-2", "", &OperatorRecovery{Reason: "post-revoke", ExpectedGeneration: 1}, "lease-2"); err != nil {
		t.Fatalf("adopt gen 2: %v", err)
	}

	// Resubmit operation X, same run and reason, but targeting generation 2:
	// a conflict, never generation 1's receipt.
	if _, err := store.RevokeController(ctx, "op-X", runID, "", &OperatorRecovery{Reason: "R", ExpectedGeneration: 2}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different target generation must conflict on replay, got %v", err)
	}

	// Generation 2 remains untouched and adopted.
	rec, err := store.GetControllerRecord(ctx, runID)
	if err != nil || !rec.Adopted || rec.Generation != 2 || rec.Status != "active" {
		t.Fatalf("generation 2 must remain active after the rejected replay: %+v err=%v", rec, err)
	}
}

// Self-revocation replay classifies current controller authority first: a
// retired lease and a never-valid credential cannot replay a successful
// self-revocation; the current controller reusing the operation conflicts.
func TestGate1Review453_SelfRevokeReplayClassifiesAuthority(t *testing.T) {
	store, runID, _ := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// A self-revokes generation 1.
	if _, err := store.RevokeController(ctx, "op-self", runID, "lease-A", nil); err != nil {
		t.Fatalf("self revoke: %v", err)
	}
	// Generation 2 adopted by B.
	if _, err := store.AdoptController(ctx, "op-adopt-b", runID, "codex", "ref-B", "", &OperatorRecovery{Reason: "rotation", ExpectedGeneration: 1}, "lease-B"); err != nil {
		t.Fatalf("adopt gen 2: %v", err)
	}

	// The retired lease cannot replay its own successful self-revocation.
	if _, err := store.RevokeController(ctx, "op-self", runID, "lease-A", nil); !errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("retired lease replay must be fenced, got %v", err)
	}
	// A never-valid credential cannot either.
	if _, err := store.RevokeController(ctx, "op-self", runID, "never-valid", nil); err == nil {
		t.Fatal("never-valid credential must not reach self-revocation replay")
	}
	// The current controller reusing the operation with its own generation
	// gets an idempotency conflict, not the old receipt.
	if _, err := store.RevokeController(ctx, "op-self", runID, "lease-B", nil); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("current-controller reuse must conflict, got %v", err)
	}

	// Generation 2 remains active.
	rec, err := store.GetControllerRecord(ctx, runID)
	if err != nil || rec.Generation != 2 || rec.Status != "active" {
		t.Fatalf("generation 2 must remain active: %+v err=%v", rec, err)
	}
}

// Adoption validates explicit recovery intent like revocation does.
func TestGate1Review453_AdoptionRequiresRecoveryReason(t *testing.T) {
	store, runID, _ := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	if _, err := store.RevokeController(ctx, "op-rv", runID, "lease-A", nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := store.AdoptController(ctx, "op-adopt-blank", runID, "agy", "ref-x", "", &OperatorRecovery{Reason: "   ", ExpectedGeneration: 1}, "cand"); err == nil {
		t.Fatal("adoption with an empty recovery reason must be rejected")
	}
	// Nothing was installed.
	rec, err := store.GetControllerRecord(ctx, runID)
	if err != nil || rec.Adopted {
		t.Fatalf("rejected adoption must not install a grant: %+v err=%v", rec, err)
	}
}
