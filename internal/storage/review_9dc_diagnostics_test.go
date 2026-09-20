package storage

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// Regression evidence for PR #29 review round 3 (head 9dc6c5d).
//
// An unconfirmed cancellation is still an execution reservation: the turn
// survived as status 'cancelling' with reachable visibility and no live
// worker (for example after a restart). Diagnostics must count it as
// reserved, unresolved, and a recovery blocker; completing the turn must
// return the counts to zero.
func TestReview9DC_CancellingTurnCountsAsUnresolved(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	store, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.CreateRun(ctx, "op-run-9dc", "run-9dc", "brief", "spec", "profile", "lease-9dc"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-9dc", "lease-9dc", SessionRecord{
		ID: "sess-9dc", RunID: "run-9dc", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	qRec, err := store.QueuePrompt(ctx, "op-q-9dc", "lease-9dc", "sess-9dc", sessRec.CommittedVersion, PendingPrompt{
		SessionID: "sess-9dc", TurnKey: "turn-cxl", Prompt: "work",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-9dc", "lease-9dc", "sess-9dc", qRec.CommittedVersion, "turn-cxl"); err != nil {
		t.Fatalf("release turn: %v", err)
	}
	// Unconfirmed cancellation: durable intent recorded, termination never
	// confirmed.
	if _, err := store.RequestCancel(ctx, "op-cxl-9dc", "lease-9dc", "sess-9dc", qRec.CommittedVersion+1, "turn-cxl"); err != nil {
		t.Fatalf("request cancel: %v", err)
	}

	// Simulate restart: close and reopen storage before reading diagnostics.
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := Open(StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer reopened.Close()

	counts, err := reopened.GetDiagnosticCounts(ctx, nil)
	if err != nil {
		t.Fatalf("diagnostic counts: %v", err)
	}
	if counts.ReservedTurns != 1 || counts.UnresolvedTurns != 1 {
		t.Fatalf("expected cancelling turn to count as reserved and unresolved, got %+v", counts)
	}
	if counts.RecoveryBlockers < 1 {
		t.Fatalf("expected cancelling turn to be a recovery blocker, got %+v", counts)
	}

	// Control: a completed turn counts as none of the three.
	if _, err := reopened.RecordTerminalOutcome(ctx, "op-term-9dc", "lease-9dc", "sess-9dc", qRec.CommittedVersion+2, "turn-cxl", council.TurnCancelled, "done"); err != nil {
		t.Fatalf("record terminal outcome: %v", err)
	}
	counts, err = reopened.GetDiagnosticCounts(ctx, nil)
	if err != nil {
		t.Fatalf("diagnostic counts after terminal: %v", err)
	}
	if counts.ReservedTurns != 0 || counts.UnresolvedTurns != 0 || counts.RecoveryBlockers != 0 {
		t.Fatalf("expected zero counts after terminal outcome, got %+v", counts)
	}
}
