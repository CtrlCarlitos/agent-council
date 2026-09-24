package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// Gate 1 review regressions (head 204b564): every path — replay, delayed
// connection handling, and evidence arriving after handoff — must use
// exactly the authority and execution identity it is entitled to use.

func gate1Fixture(t *testing.T, lease string) (*Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-g1", "run-g1", "b", "s", "p", "boot-"+lease); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.AdoptController(ctx, "op-adopt-g1", "run-g1", "claude", "controller-g1", "boot-"+lease, nil, lease); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if _, err := store.ConnectRunController(ctx, "op-conn-g1-init", "run-g1", lease, 1, "test-instance"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	sess, err := store.CreateSession(ctx, "op-sess-g1", lease, SessionRecord{
		ID: "sess-g1", RunID: "run-g1", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	_ = sess
	return store, "run-g1", "sess-g1"
}

func gate1Connect(t *testing.T, store *Store, runID, lease string, gen uint64) string {
	t.Helper()
	rec, err := store.ConnectRunController(context.Background(), "op-conn-g1-"+time.Now().Format("150405.000000000"), runID, lease, gen, "test-instance")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return rec.AttachmentID
}

// A superseded or unknown credential cannot reach attachment replay, even
// with a matching operation ID and request fields.
func TestGate1Review204_AttachmentReplayRequiresCurrentAuthority(t *testing.T) {
	store, runID, _ := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	rec, err := store.ConnectRunController(ctx, "op-conn-hist", runID, "lease-A", 1, "test-instance")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Rotate away from lease-A.
	if _, err := store.HandoffController(ctx, "op-handoff-g1", runID, "lease-A", 1, "codex", "ref-B", "lease-B"); err != nil {
		t.Fatalf("handoff: %v", err)
	}

	// Replaying the connect with the superseded lease is fenced.
	if _, err := store.ConnectRunController(ctx, "op-conn-hist", runID, "lease-A", 1, "test-instance"); !errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("expected ErrLeaseSuperseded for superseded connect replay, got %v", err)
	}
	// Unknown credential likewise.
	if _, err := store.ConnectRunController(ctx, "op-conn-hist", runID, "never-valid", 1, "test-instance"); err == nil {
		t.Fatal("unknown credential must not reach connect replay")
	}
	// The current controller's own connect operation replays idempotently
	// within its live episode.
	recB, err := store.ConnectRunController(ctx, "op-conn-B", runID, "lease-B", 2, "test-instance")
	if err != nil {
		t.Fatalf("current controller connect: %v", err)
	}
	replayedB, err := store.ConnectRunController(ctx, "op-conn-B", runID, "lease-B", 2, "test-instance")
	if err != nil || replayedB.AttachmentID != recB.AttachmentID {
		t.Fatalf("same-episode retry must recover the receipt: %+v err=%v", replayedB, err)
	}
	// Replaying the superseded generation's operation with a stale expected
	// generation is a generation mismatch for the current controller.
	if _, err := store.ConnectRunController(ctx, "op-conn-hist", runID, "lease-B", 1, "test-instance"); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("stale-generation connect replay must be a generation mismatch, got %v", err)
	}

	// Superseded disconnect replay is fenced too.
	if _, err := store.DisconnectRunController(ctx, "op-disc-x", runID, "lease-A", rec.AttachmentID, 1); !errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("expected ErrLeaseSuperseded for superseded disconnect, got %v", err)
	}
}

// Replaying a previously successful disconnect after a new connection
// returns the receipt without ending the successor's episode.
func TestGate1Review204_DisconnectReplayAfterNewConnection(t *testing.T) {
	store, runID, _ := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	episodeA := gate1Connect(t, store, runID, "lease-A", 1)
	if _, err := store.DisconnectRunController(ctx, "op-disc-a", runID, "lease-A", episodeA, 1); err != nil {
		t.Fatalf("disconnect A: %v", err)
	}
	gate1Connect(t, store, runID, "lease-A", 1) // episode B

	replay, err := store.DisconnectRunController(ctx, "op-disc-a", runID, "lease-A", episodeA, 1)
	if err != nil {
		t.Fatalf("replay of A's successful disconnect: %v", err)
	}
	if !strings.HasPrefix(replay.Payload, "disconnected:attachment=") {
		t.Fatalf("replay must return the committed receipt verbatim, got %q", replay.Payload)
	}
	rec, err := store.GetControllerRecord(ctx, runID)
	if err != nil || !rec.Connected {
		t.Fatalf("episode B must remain connected after A's disconnect replay: %+v err=%v", rec, err)
	}
}

// Revoke replay is bound to its command, run, and recovery intent.
func TestGate1Review204_RevokeReplayBinding(t *testing.T) {
	store, runID, _ := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// A connect operation ID reused for revoke conflicts.
	if _, err := store.ConnectRunController(ctx, "op-multi", runID, "lease-A", 1, "test-instance"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := store.RevokeController(ctx, "op-multi", runID, "lease-A", nil); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected conflict for revoke reusing a connect operation ID, got %v", err)
	}

	// A genuine revoke replays idempotently.
	if _, err := store.RevokeController(ctx, "op-revoke-real", runID, "lease-A", nil); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	replayed, err := store.RevokeController(ctx, "op-revoke-real", runID, "", &OperatorRecovery{Reason: "retry after lost response", ExpectedGeneration: 1})
	// The retry used a different authorization mode: conflict, not a receipt.
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed recovery intent must conflict, got receipt %+v err=%v", replayed, err)
	}

	// Another run's revoke operation ID (same store) cannot serve this run.
	if _, err := store.CreateRun(ctx, "op-run-g1b", "run-g1b", "b", "s", "p", "boot-Z"); err != nil {
		t.Fatalf("create second run: %v", err)
	}
	if _, err := store.AdoptController(ctx, "op-adopt-g1b", "run-g1b", "claude", "controller-g1b", "boot-Z", nil, "lease-Z"); err != nil {
		t.Fatalf("adopt second run: %v", err)
	}
	if _, err := store.RevokeController(ctx, "op-revoke-real", "run-g1b", "lease-Z", nil); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected conflict for cross-run revoke operation reuse, got %v", err)
	}
}

// Handoff replay with a changed expected generation is a conflict — target
// preconditions are not excluded with the secret candidate.
func TestGate1Review204_HandoffReplayGenerationBinding(t *testing.T) {
	store, runID, _ := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	if _, err := store.HandoffController(ctx, "op-handoff-1", runID, "lease-A", 1, "codex", "ref-B", "lease-B"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if _, err := store.HandoffController(ctx, "op-handoff-1", runID, "lease-B", 2, "codex", "ref-B", "lease-C"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected conflict for changed expected generation on replay, got %v", err)
	}
}

// Recovery intent is validated and journaled; grant replay returns the
// original committed receipt with the evolving status outside it.
func TestGate1Review204_RecoveryReasonAndOriginalReceiptReplay(t *testing.T) {
	store, runID, _ := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// Empty recovery reason is rejected before mutation.
	if _, err := store.RevokeController(ctx, "op-revoke-noreason", runID, "", &OperatorRecovery{Reason: "  ", ExpectedGeneration: 1}); err == nil {
		t.Fatal("recovery without an explicit reason must be rejected")
	}

	reasoned, err := store.RevokeController(ctx, "op-revoke-reasoned", runID, "", &OperatorRecovery{Reason: "operator lost the lease", ExpectedGeneration: 1})
	if err != nil {
		t.Fatalf("recovery revoke: %v", err)
	}
	if !strings.Contains(reasoned.Payload, "operator lost the lease") {
		t.Fatalf("recovery reason must be journaled in the committed receipt, got %q", reasoned.Payload)
	}

	// Adoption recovery journals its authorization mode; replay returns the
	// original receipt (not a reconstruction) with issuance status outside.
	grant, err := store.AdoptController(ctx, "op-adopt-retry", runID, "agy", "ref-retry", "", &OperatorRecovery{Reason: "post-revoke adoption", ExpectedGeneration: 1}, "lease-N1")
	if err != nil {
		t.Fatalf("adopt via recovery: %v", err)
	}
	if !strings.Contains(grant.Payload, "authorized_by=recovery") {
		t.Fatalf("adoption receipt must record the recovery authorization, got %q", grant.Payload)
	}
	first := grant.OperationReceipt
	replayed, err := store.AdoptController(ctx, "op-adopt-retry", runID, "agy", "ref-retry", "", &OperatorRecovery{Reason: "post-revoke adoption", ExpectedGeneration: 1}, "unused-candidate")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.OperationReceipt.Payload != first.Payload || !replayed.OperationReceipt.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("replay must return the original committed receipt, got %+v want %+v", replayed.OperationReceipt, first)
	}
	if !replayed.ReplayedReceipt || replayed.IssuanceStatus != "active" {
		t.Fatalf("replay must be labelled with external issuance status: %+v", replayed)
	}
	if replayed.LeaseSecret != "lease-N1" {
		t.Fatalf("replay must return the original issuance, got %q", replayed.LeaseSecret)
	}
}

// Reconciliation validates the original attempt identity inside its atomic
// transition.
func TestGate1Review204_ReconcileValidatesExecutionReference(t *testing.T) {
	store, runID, sessID := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	ver, _ := store.GetSessionVersion(ctx, sessID)
	if _, err := store.QueuePrompt(ctx, "op-q-g1", "lease-A", sessID, ver, PendingPrompt{SessionID: sessID, TurnKey: "t-g1", Prompt: "p"}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	ver, _ = store.GetSessionVersion(ctx, sessID)
	if _, err := store.ReleaseTurn(ctx, "op-rel-g1", "lease-A", sessID, ver, "t-g1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	hlVer, _ := store.GetSessionVersion(ctx, sessID)
	hl, err := store.RecordHostLoss(ctx, "op-g1:host_loss", "lease-A", sessID, hlVer)
	if err != nil {
		t.Fatalf("host loss: %v", err)
	}
	var gen uint64
	if _, err := fmt.Sscanf(hl.Payload, "%d", &gen); err != nil || gen == 0 {
		t.Fatalf("invalid generation payload %q", hl.Payload)
	}
	_ = runID

	ref := store.ExecutionRefForTurn(ctx, sessID, "t-g1")
	if ref.AttemptID == "" {
		t.Fatal("expected a persisted execution reference")
	}
	wrong := ref
	wrong.AttemptID = "att_forged"

	rref := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID(sessID), TurnKey: "t-g1"},
		Generation: gen,
	}
	outcome := adapter.ReconciliationOutcome{
		Ref:          rref,
		Reachability: council.VisibilityHostLost,
		Status:       adapter.ReconciliationUncertain,
		Observed:     council.TurnRunning,
	}
	if _, err := store.ReconcileSession(ctx, "op-g1:reconcile", wrong, rref, outcome); !errors.Is(err, ErrWrongExecutionAttempt) {
		t.Fatalf("expected ErrWrongExecutionAttempt for wrong-attempt reconciliation, got %v", err)
	}
}

// Dispatch acknowledgement bumps the parent session version and reports it.
func TestGate1Review204_DispatchAcknowledgementAdvancesSessionVersion(t *testing.T) {
	store, _, sessID := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	ver, _ := store.GetSessionVersion(ctx, sessID)
	if _, err := store.QueuePrompt(ctx, "op-q-ver", "lease-A", sessID, ver, PendingPrompt{SessionID: sessID, TurnKey: "t-ver", Prompt: "p"}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	ver, _ = store.GetSessionVersion(ctx, sessID)
	rel, err := store.ReleaseTurn(ctx, "op-rel-ver", "lease-A", sessID, ver, "t-ver")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	ref := ExecutionRef{SessionID: sessID, TurnKey: "t-ver", AttemptID: rel.Receipt.AttemptID}

	before, _ := store.GetSessionVersion(ctx, sessID)
	receipt, err := store.RecordObservedDispatchAcknowledgement(ctx, "op-obs-ver", ref, "receipt_acknowledged")
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	after, _ := store.GetSessionVersion(ctx, sessID)
	if after != before+1 {
		t.Fatalf("dispatch acknowledgement must advance the session version: before=%d after=%d", before, after)
	}
	if receipt.CommittedVersion != after {
		t.Fatalf("receipt must report the advanced version: got %d want %d", receipt.CommittedVersion, after)
	}
}

// New decisions enforce the run connection at the write boundary: a
// decision whose preflight passed can still be fenced by an interleaved
// disconnect. Newly created sessions derive their projection from the run.
func TestGate1Review204_DecisionFencedByDisconnectAtBoundary(t *testing.T) {
	store, runID, sessID := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	// Disconnect commits; the lease remains current.
	gate1Connect(t, store, runID, "lease-A", 1)
	recA, err := store.GetControllerRecord(ctx, runID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := store.DisconnectRunController(ctx, "op-disc-g2", runID, "lease-A", recA.AttachmentID, 1); err != nil {
		t.Fatalf("disconnect: %v", err)
	}

	if _, err := store.RecordDecision(ctx, "op-dec-disc", "lease-A", runID, "a", 1, "p"); !errors.Is(err, ErrControllerDisconnected) {
		t.Fatalf("decision after disconnect must be fenced at the boundary, got %v", err)
	}

	// A session created while the run is disconnected derives a disconnected
	// projection and cannot become a separate permission source.
	if _, err := store.CreateSession(ctx, "op-sess-disc", "lease-A", SessionRecord{
		ID: "sess-disc", RunID: runID, Contributor: "codex", Role: "reviewer",
		IsActiveContributor: false, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session while disconnected: %v", err)
	}
	var projection string
	if err := store.readDB.QueryRow(`SELECT controller_status FROM sessions WHERE session_id = ?;`, "sess-disc").Scan(&projection); err != nil || projection != "disconnected" {
		t.Fatalf("new session projection must derive from the run: %q err=%v", projection, err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-disc", "lease-A", "sess-disc", 1, PendingPrompt{SessionID: "sess-disc", TurnKey: "t-disc", Prompt: "p"}); !errors.Is(err, ErrControllerDisconnected) {
		t.Fatalf("queue on disconnected run must be fenced at the boundary, got %v", err)
	}
	_ = sessID
}

// Release replay follows the single current-authority contract: a receipt
// recorded under a superseded controller is recoverable by the current one,
// both via the fast lookup and transactionally.
func TestGate1Review204_ReleaseReplayContractConsistent(t *testing.T) {
	store, runID, sessID := gate1Fixture(t, "lease-A")
	defer store.Close()
	ctx := context.Background()

	ver, _ := store.GetSessionVersion(ctx, sessID)
	if _, err := store.QueuePrompt(ctx, "op-q-relg1", "lease-A", sessID, ver, PendingPrompt{SessionID: sessID, TurnKey: "t-relg1", Prompt: "p"}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	ver, _ = store.GetSessionVersion(ctx, sessID)
	if _, err := store.ReleaseTurn(ctx, "op-rel-g1x", "lease-A", sessID, ver, "t-relg1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Rotate; the current controller recovers the committed release both ways.
	if _, err := store.HandoffController(ctx, "op-handoff-rel", runID, "lease-A", 1, "codex", "ref-B", "lease-B"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	if _, found, err := store.FindCommittedRelease(ctx, "op-rel-g1x", "lease-B", sessID, "t-relg1"); err != nil || !found {
		t.Fatalf("fast lookup by current controller: found=%v err=%v", found, err)
	}
	res, err := store.ReleaseTurn(ctx, "op-rel-g1x", "lease-B", sessID, 1, "t-relg1")
	if err != nil || res.Disposition != ReleaseDispositionReplayed {
		t.Fatalf("transactional replay by current controller: %+v err=%v", res, err)
	}
	// The superseded controller is fenced on both paths.
	if _, _, err := store.FindCommittedRelease(ctx, "op-rel-g1x", "lease-A", sessID, "t-relg1"); !errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("superseded fast lookup must be fenced, got %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-g1x", "lease-A", sessID, 1, "t-relg1"); !errors.Is(err, ErrLeaseSuperseded) {
		t.Fatalf("superseded transactional replay must be fenced, got %v", err)
	}
}
