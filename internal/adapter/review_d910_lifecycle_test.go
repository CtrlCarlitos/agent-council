package adapter_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// TestReviewD910_ObservationContextCancellationEndsSubscription verifies that cancelling
// an observation context ends that subscription without cancelling the underlying worker execution.
func TestReviewD910_ObservationContextCancellationEndsSubscription(t *testing.T) {
	ctx := context.Background()
	holdStart := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		HoldExecutionStart: holdStart,
	})

	ref := adapter.TurnRef{SessionID: "s-obs-cancel", TurnKey: "t1"}
	_, err := fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   ref.SessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := fake.Dispatch(ctx, ref, "test observation context cancel")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch failed: err=%v outcome=%+v", err, outcome)
	}

	obsCtx, cancelObs := context.WithCancel(context.Background())
	stream, err := fake.Observe(obsCtx, ref)
	if err != nil {
		t.Fatalf("observe failed: %v", err)
	}

	// Read initial progress event
	select {
	case ev, ok := <-stream.Events():
		if !ok || ev.Type != adapter.EventProgress {
			t.Fatalf("expected initial progress event, got ev=%+v ok=%v", ev, ok)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for initial progress event")
	}

	// Cancel observation context
	cancelObs()

	// Subscription should terminate cleanly
	select {
	case _, ok := <-stream.Events():
		if ok {
			// May read any remaining buffered events, then channel must close
			for range stream.Events() {
			}
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for observation stream to close after context cancellation")
	}

	if !errors.Is(stream.Err(), context.Canceled) {
		t.Fatalf("expected stream.Err() to be context.Canceled, got %v", stream.Err())
	}

	// Crucial check: worker was NOT cancelled!
	res, err := fake.Collect(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == council.TurnCancelled {
		t.Fatal("observation context cancellation prematurely cancelled the turn worker")
	}

	// Release start gate so worker finishes naturally
	close(holdStart)
	time.Sleep(30 * time.Millisecond)

	resAfter, err := fake.Collect(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if resAfter.Status != council.TurnCompleted {
		t.Fatalf("expected TurnCompleted after start release, got %s", resAfter.Status)
	}
}

// TestReviewD910_SlowObserverOverflowDoesNotBlockWorker verifies that a slow subscriber
// that does not read is detached with ErrBufferOverflow without blocking worker execution.
func TestReviewD910_SlowObserverOverflowDoesNotBlockWorker(t *testing.T) {
	ctx := context.Background()
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		EmitProgressCount: 70, // Exceeds default buffer capacity of 64
	})

	ref := adapter.TurnRef{SessionID: "s-overflow", TurnKey: "t1"}
	_, err := fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   ref.SessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := fake.Dispatch(ctx, ref, "test overflow")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch failed: err=%v outcome=%+v", err, outcome)
	}

	// Subscribe but do NOT read from events channel
	slowStream, err := fake.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe failed: %v", err)
	}

	// Worker should complete without deadlock or blocking on slow subscriber
	done := make(chan struct{})
	go func() {
		for {
			res, err := fake.Collect(ctx, ref)
			if err == nil && res.Status == council.TurnCompleted {
				close(done)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	select {
	case <-done:
		// Success: worker completed
	case <-time.After(2 * time.Second):
		t.Fatal("worker deadlocked or was blocked by slow subscriber buffer overflow")
	}

	// Slow stream should report overflow
	if !errors.Is(slowStream.Err(), adapter.ErrBufferOverflow) {
		t.Fatalf("expected slowStream.Err() to be ErrBufferOverflow, got %v", slowStream.Err())
	}
}

// TestReviewD910_ActiveTurnReplayRejected verifies that dispatching under an already active
// TurnRef is rejected and does not overwrite per-turn data or launch another worker.
func TestReviewD910_ActiveTurnReplayRejected(t *testing.T) {
	ctx := context.Background()
	stall := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StallStream: stall,
	})

	ref := adapter.TurnRef{SessionID: "s-replay", TurnKey: "t1"}
	_, err := fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   ref.SessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatal(err)
	}

	outcome1, err := fake.Dispatch(ctx, ref, "prompt 1")
	if err != nil || outcome1.Status != adapter.DispatchAccepted {
		t.Fatalf("initial dispatch failed: err=%v outcome=%+v", err, outcome1)
	}

	// Attempting to dispatch again under same active TurnRef must be rejected
	outcome2, err := fake.Dispatch(ctx, ref, "prompt 2 (overwrite attempt)")
	if err == nil {
		t.Fatal("expected error on re-dispatching active turn")
	}
	if outcome2.Status != adapter.DispatchRejected {
		t.Fatalf("expected DispatchRejected, got %s", outcome2.Status)
	}

	close(stall)
}

// TestReviewD910_QueuedCancellationDoesNotChangeStartedToTrue verifies that confirming
// cancellation for a queued turn held behind HoldExecutionStart prevents Started from becoming true.
func TestReviewD910_QueuedCancellationDoesNotChangeStartedToTrue(t *testing.T) {
	ctx := context.Background()
	holdStart := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		HoldExecutionStart: holdStart,
	})

	ref := adapter.TurnRef{SessionID: "s-queued-cancel", TurnKey: "t1"}
	_, err := fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   ref.SessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := fake.Dispatch(ctx, ref, "prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch failed: err=%v outcome=%+v", err, outcome)
	}

	stBefore := fake.TurnState(ref)
	if !stBefore.Received || !stBefore.Accepted || stBefore.Started {
		t.Fatalf("expected received=true accepted=true started=false before start, got %+v", stBefore)
	}

	// Cancel while queued
	cancelOutcome, err := fake.Cancel(ctx, ref)
	if err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if cancelOutcome.Disposition != adapter.CancelConfirmed {
		t.Fatalf("expected CancelConfirmed, got %s", cancelOutcome.Disposition)
	}

	// Release start gate
	close(holdStart)
	time.Sleep(20 * time.Millisecond)

	// TurnState MUST NOT show Started: true
	stAfter := fake.TurnState(ref)
	if stAfter.Started {
		t.Fatal("turn transitioned Started to true after cancellation had already been confirmed")
	}

	res, err := fake.Collect(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != council.TurnCancelled {
		t.Fatalf("expected TurnCancelled, got %s", res.Status)
	}
}

// TestReviewD910_PreCancelledDispatchContextRejected verifies that calling Dispatch with
// an already-cancelled context is rejected before any side effects or state allocations.
func TestReviewD910_PreCancelledDispatchContextRejected(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	ref := adapter.TurnRef{SessionID: "s-precancel", TurnKey: "t1"}

	outcome, err := fake.Dispatch(canceledCtx, ref, "prompt")
	if err == nil {
		t.Fatal("expected error on pre-cancelled dispatch context")
	}
	if outcome.Status != adapter.DispatchRejected {
		t.Fatalf("expected DispatchRejected, got %s", outcome.Status)
	}

	// Zero side effects: no turn state recorded
	st := fake.TurnState(ref)
	if st.Received || st.Accepted || st.Started {
		t.Fatalf("pre-cancelled dispatch created state: %+v", st)
	}
}

// TestReviewD910_UnknownDispatchNonterminalReconciliationPreservesReservation verifies
// that nonterminal host recovery keeps the execution reservation occupied until authoritative completion.
func TestReviewD910_UnknownDispatchNonterminalReconciliationPreservesReservation(t *testing.T) {
	ctx := context.Background()
	sess, err := council.NewSession(council.Claude, "lease-1")
	if err != nil {
		t.Fatal(err)
	}

	_ = sess.Queue("lease-1", "t1", "prompt 1")
	_ = sess.Queue("lease-1", "t2", "prompt 2")

	prompt1, err := sess.Release("lease-1", "t1")
	if err != nil {
		t.Fatal(err)
	}

	stall := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		DispatchStatus: adapter.DispatchUnknown,
		StallStream:    stall,
	})

	turnRef1 := adapter.TurnRef{SessionID: adapter.SessionID(fmt.Sprintf("sess-%s", sess.ID)), TurnKey: "t1"}
	_, err = fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   turnRef1.SessionID,
		Contributor: sess.ID,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatal(err)
	}

	outcome, _ := fake.Dispatch(ctx, turnRef1, prompt1)
	if outcome.Status != adapter.DispatchUnknown {
		t.Fatalf("expected DispatchUnknown, got %s", outcome.Status)
	}

	// Open domain recovery episode
	gen, err := sess.RecordHostLoss()
	if err != nil {
		t.Fatal(err)
	}

	// Reconcile nonterminal outcome
	recOutcome, err := fake.Reconcile(ctx, adapter.RecoveryRef{TurnRef: turnRef1, Generation: gen})
	if err != nil {
		t.Fatal(err)
	}
	if recOutcome.Status != adapter.ReconciliationReachableActive {
		t.Fatalf("expected ReconciliationReachableActive, got %s", recOutcome.Status)
	}

	// Deliver nonterminal outcome to Council
	if err := sess.ReconcileHost(recOutcome.Ref.TurnKey, recOutcome.Ref.Generation, recOutcome.Observed, recOutcome.Result); err != nil {
		t.Fatalf("ReconcileHost failed: %v", err)
	}

	// Reservation must remain occupied: releasing t2 must fail
	if _, err := sess.Release("lease-1", "t2"); err == nil {
		t.Fatal("release of t2 permitted while t1 is still running after nonterminal reconciliation")
	}

	// Unblock completion
	close(stall)
	time.Sleep(20 * time.Millisecond)

	termResult, err := fake.Collect(ctx, turnRef1)
	if err != nil {
		t.Fatal(err)
	}
	if termResult.Status != council.TurnCompleted {
		t.Fatalf("expected TurnCompleted, got %s", termResult.Status)
	}

	if err := sess.CompleteWithResult(turnRef1.TurnKey, termResult.Output); err != nil {
		t.Fatal(err)
	}

	// Now t2 release succeeds
	if _, err := sess.Release("lease-1", "t2"); err != nil {
		t.Fatalf("release of t2 failed after terminal completion: %v", err)
	}
}
