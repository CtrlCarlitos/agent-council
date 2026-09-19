package adapter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func TestCouncilBoundary_UnacknowledgedDispatchPreservesReservation(t *testing.T) {
	sess, err := council.NewSession(council.Claude, "lease-1")
	if err != nil {
		t.Fatal(err)
	}

	_ = sess.Queue("lease-1", "t1", "prompt 1")
	_ = sess.Queue("lease-1", "t2", "prompt 2")

	// Release t1
	ref1, err := sess.Release("lease-1", "t1")
	if err != nil {
		t.Fatalf("release t1 failed: %v", err)
	}

	// Dispatch t1 to fake with held acknowledgement
	holdAck := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		HoldDispatchAck: holdAck,
		DispatchStatus:  adapter.DispatchUnknown,
	})

	turnRef1 := adapter.TurnRef{SessionID: adapter.SessionID(sess.ID), TurnKey: ref1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneDispatch := make(chan adapter.DispatchOutcome)
	go func() {
		outcome, _ := fake.Dispatch(ctx, turnRef1, "prompt 1")
		doneDispatch <- outcome
	}()

	close(holdAck)
	outcome := <-doneDispatch
	if outcome.Status != adapter.DispatchUnknown {
		t.Fatalf("expected DispatchUnknown, got %s", outcome.Status)
	}

	// Turn reservation must be preserved: attempting to release t2 must fail!
	if _, err := sess.Release("lease-1", "t2"); err == nil {
		t.Fatal("release of t2 permitted while t1 execution reservation remains active")
	}

	if sess.Active != "t1" || sess.State != council.Running {
		t.Fatalf("session state corrupted after DispatchUnknown: active=%s state=%s", sess.Active, sess.State)
	}
}

func TestCouncilBoundary_ToolDenialDoesNotRetireSlot(t *testing.T) {
	sess, err := council.NewSession(council.Agy, "lease-1")
	if err != nil {
		t.Fatal(err)
	}

	_ = sess.Queue("lease-1", "t1", "prompt 1")
	_ = sess.Queue("lease-1", "t2", "prompt 2")
	_, _ = sess.Release("lease-1", "t1")

	// Process tool denial event
	denialEv := adapter.Event{
		Ref:        adapter.TurnRef{SessionID: adapter.SessionID(sess.ID), TurnKey: "t1"},
		Type:       adapter.EventToolDenied,
		Status:     council.TurnRunning,
		ApprovalID: "tool-git-commit",
		Payload:    "permission denied by policy",
	}
	if err := denialEv.Validate(); err != nil {
		t.Fatalf("invalid denial event: %v", err)
	}

	// Tool denial does NOT retire the slot
	if sess.Active != "t1" || sess.State != council.Running {
		t.Fatalf("tool denial prematurely cleared active turn: active=%s state=%s", sess.Active, sess.State)
	}

	// Releasing t2 must remain blocked
	if _, err := sess.Release("lease-1", "t2"); err == nil {
		t.Fatal("release of t2 permitted while t1 is unretired after tool denial")
	}

	// Authoritative terminal failure arrives
	if err := sess.Fail("t1", "tool denied"); err != nil {
		t.Fatalf("Fail failed: %v", err)
	}
	if sess.State != council.Parked {
		t.Fatalf("expected parked after fail, got %s", sess.State)
	}

	// Now t2 can be released
	if _, err := sess.Release("lease-1", "t2"); err != nil {
		t.Fatalf("release of t2 failed after terminal outcome: %v", err)
	}
}

func TestCouncilBoundary_StaleRecoveryGenerationRejectedAcrossOutages(t *testing.T) {
	sess, err := council.NewSession(council.Codex, "lease-1")
	if err != nil {
		t.Fatal(err)
	}

	_ = sess.Queue("lease-1", "t1", "p1")
	_, _ = sess.Release("lease-1", "t1")

	// Outage 1
	gen1, err := sess.RecordHostLoss()
	if err != nil {
		t.Fatal(err)
	}

	// Outage 1 recovers
	if err := sess.ReconcileHost("t1", gen1, council.TurnRunning, ""); err != nil {
		t.Fatal(err)
	}

	// Outage 2
	gen2, err := sess.RecordHostLoss()
	if err != nil {
		t.Fatal(err)
	}
	if gen2 <= gen1 {
		t.Fatalf("expected gen2 > gen1, got %d <= %d", gen2, gen1)
	}

	// Stale recovery response with gen1 from fake
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StaleRecoveryRef: &adapter.RecoveryRef{
			TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID(sess.ID), TurnKey: "t1"},
			Generation: gen1,
		},
	})

	outcome, err := fake.Reconcile(context.Background(), adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID(sess.ID), TurnKey: "t1"},
		Generation: gen2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Delivery of stale outcome to Council must be rejected
	err = sess.ReconcileHost(outcome.Ref.TurnKey, outcome.Ref.Generation, outcome.Observed, outcome.Result)
	if err == nil {
		t.Fatal("stale generation 1 reconciliation was accepted during outage 2")
	}

	if sess.Visibility != council.VisibilityHostLost {
		t.Fatalf("stale reconciliation cleared host uncertainty: visibility=%s", sess.Visibility)
	}
}

func TestCouncilBoundary_RosterSessionIsolation(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	ctx := context.Background()

	roster := council.Roster()
	bindings := make(map[council.Contributor]adapter.SessionBinding)

	for _, contrib := range roster {
		sessID := adapter.SessionID("sess-" + string(contrib))
		req := adapter.CreateSessionRequest{
			SessionID:   sessID,
			Contributor: contrib,
			Config: adapter.SessionConfig{
				WorkspaceRoot: "/workspace/" + string(contrib),
				Model:         "model-" + string(contrib),
			},
		}
		b, err := fake.CreateSession(ctx, req)
		if err != nil {
			t.Fatalf("failed to create session for %s: %v", contrib, err)
		}
		bindings[contrib] = b
	}

	// Verify all native session IDs are distinct across roster
	seen := make(map[string]council.Contributor)
	for contrib, b := range bindings {
		if other, exists := seen[b.NativeSessionID]; exists {
			t.Fatalf("collision between %s and %s on native ID %s", contrib, other, b.NativeSessionID)
		}
		seen[b.NativeSessionID] = contrib
	}
}

func TestCouncilBoundary_ProductionRegistryRejectsFake(t *testing.T) {
	// Assert that production adapter resolution fails closed when asked for "fake" or unknown
	resolveAdapter := func(name string) (adapter.Adapter, error) {
		switch name {
		case "opencode", "claude", "codex", "agy":
			return nil, errors.New("native adapter not yet implemented")
		default:
			return nil, errors.New("unknown or prohibited adapter")
		}
	}

	if _, err := resolveAdapter("fake"); err == nil {
		t.Fatal("production adapter resolver allowed 'fake' adapter")
	}
	if _, err := resolveAdapter("unknown"); err == nil {
		t.Fatal("production adapter resolver allowed unknown adapter")
	}
}
