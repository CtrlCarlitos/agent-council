package adapter_test

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// TestReview264_RejectedDispatchDoesNotBecomeRunningOrCancelled verifies that a rejected
// dispatch preserves Received=true/Accepted=false and never manufactures running observation,
// cancelled result, or empty TurnRef evidence.
func TestReview264_RejectedDispatchDoesNotBecomeRunningOrCancelled(t *testing.T) {
	ctx := context.Background()
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		DispatchStatus: adapter.DispatchRejected,
	})

	ref := adapter.TurnRef{SessionID: "s-rej-exec", TurnKey: "t1"}
	_, err := fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   ref.SessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Dispatch is rejected
	dispOutcome, err := fake.Dispatch(ctx, ref, "prompt")
	if err == nil || dispOutcome.Status != adapter.DispatchRejected {
		t.Fatalf("expected DispatchRejected, got outcome=%+v err=%v", dispOutcome, err)
	}

	// TurnState must report Received=true, Accepted=false, Started=false
	st := fake.TurnState(ref)
	if !st.Received || st.Accepted || st.Started {
		t.Fatalf("unexpected TurnState for rejected dispatch: %+v", st)
	}

	// 2. Observe must fail and NOT manufacture running progress
	if _, err := fake.Observe(ctx, ref); err == nil {
		t.Fatal("expected error observing rejected dispatch, but Observe succeeded")
	}

	// 3. Cancel must return CancelUnknown and NOT return CancelConfirmed
	cancelOutcome, err := fake.Cancel(ctx, ref)
	if err != nil {
		t.Fatalf("unexpected error from Cancel: %v", err)
	}
	if cancelOutcome.Disposition == adapter.CancelConfirmed {
		t.Fatal("Cancel returned CancelConfirmed for an unaccepted/rejected dispatch")
	}
	if cancelOutcome.Disposition != adapter.CancelUnknown {
		t.Fatalf("expected CancelUnknown for rejected dispatch, got: %s", cancelOutcome.Disposition)
	}
	if cancelOutcome.Ref != ref {
		t.Fatalf("expected Ref=%+v, got %+v", ref, cancelOutcome.Ref)
	}

	// 4. Collect must return ResultUnavailable and error, NOT available/cancelled with empty TurnRef
	colRes, err := fake.Collect(ctx, ref)
	if err == nil {
		t.Fatal("expected error from Collect for rejected dispatch, got nil")
	}
	if colRes.ResultStatus == adapter.ResultAvailable {
		t.Fatalf("Collect returned ResultAvailable for rejected dispatch: %+v", colRes)
	}
	if colRes.Status == council.TurnCancelled {
		t.Fatalf("Collect fabricated TurnCancelled for rejected dispatch: %+v", colRes)
	}

	// 5. Reconcile must report definitively missing, NOT reachable active
	recOutcome, err := fake.Reconcile(ctx, adapter.RecoveryRef{TurnRef: ref, Generation: 1})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if recOutcome.Status != adapter.ReconciliationDefinitivelyMissing {
		t.Fatalf("expected ReconciliationDefinitivelyMissing for rejected dispatch, got: %s", recOutcome.Status)
	}

	// 6. TurnState remains intact
	stFinal := fake.TurnState(ref)
	if !stFinal.Received || stFinal.Accepted || stFinal.Started {
		t.Fatalf("TurnState was corrupted after rejected dispatch operations: %+v", stFinal)
	}
}
