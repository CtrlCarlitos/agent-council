package adaptertest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// TestReview264_CancelledWorkersDoNotBlockBehindExecutionGates verifies that
// cancelling turns held behind execution gates (both HoldExecutionStart and StallStream)
// terminates the worker goroutines without waiting for the gates to be released.
func TestReview264_CancelledWorkersDoNotBlockBehindExecutionGates(t *testing.T) {
	ctx := context.Background()

	// 1. Eight turns held behind HoldExecutionStart
	startGate := make(chan struct{})
	fakeStart := NewFake(ScriptedFaults{
		HoldExecutionStart: startGate,
	})
	defer fakeStart.Close()

	sessionStart := adapter.SessionID("sess-gate-start")
	_, err := fakeStart.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   sessionStart,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	var startRefs []adapter.TurnRef
	for i := 0; i < 8; i++ {
		ref := adapter.TurnRef{SessionID: sessionStart, TurnKey: fmt.Sprintf("turn-start-%d", i)}
		startRefs = append(startRefs, ref)
		outcome, err := fakeStart.Dispatch(ctx, ref, fmt.Sprintf("prompt %d", i))
		if err != nil || outcome.Status != adapter.DispatchAccepted {
			t.Fatalf("Dispatch %d failed: outcome=%+v, err=%v", i, outcome, err)
		}
	}

	// 2. Eight turns held behind StallStream
	stallGate := make(chan struct{})
	fakeStall := NewFake(ScriptedFaults{
		StallStream: stallGate,
	})
	defer fakeStall.Close()

	sessionStall := adapter.SessionID("sess-gate-stall")
	_, err = fakeStall.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   sessionStall,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	var stallRefs []adapter.TurnRef
	for i := 0; i < 8; i++ {
		ref := adapter.TurnRef{SessionID: sessionStall, TurnKey: fmt.Sprintf("turn-stall-%d", i)}
		stallRefs = append(stallRefs, ref)
		outcome, err := fakeStall.Dispatch(ctx, ref, fmt.Sprintf("prompt %d", i))
		if err != nil || outcome.Status != adapter.DispatchAccepted {
			t.Fatalf("Dispatch %d failed: outcome=%+v, err=%v", i, outcome, err)
		}
	}

	// Cancel all 16 turns across both fakes without opening either gate
	for i, ref := range startRefs {
		cancelOutcome, err := fakeStart.Cancel(ctx, ref)
		if err != nil {
			t.Fatalf("Cancel startRef[%d] error: %v", i, err)
		}
		if cancelOutcome.Disposition != adapter.CancelConfirmed {
			t.Fatalf("expected CancelConfirmed for startRef[%d], got: %s", i, cancelOutcome.Disposition)
		}
		if cancelOutcome.Ref != ref {
			t.Fatalf("expected Ref=%+v, got %+v", ref, cancelOutcome.Ref)
		}
	}

	for i, ref := range stallRefs {
		cancelOutcome, err := fakeStall.Cancel(ctx, ref)
		if err != nil {
			t.Fatalf("Cancel stallRef[%d] error: %v", i, err)
		}
		if cancelOutcome.Disposition != adapter.CancelConfirmed {
			t.Fatalf("expected CancelConfirmed for stallRef[%d], got: %s", i, cancelOutcome.Disposition)
		}
		if cancelOutcome.Ref != ref {
			t.Fatalf("expected Ref=%+v, got %+v", ref, cancelOutcome.Ref)
		}
	}

	// WaitWorkers must unblock promptly without opening the test-owned gates
	workersDone := make(chan struct{})
	go func() {
		fakeStart.WaitWorkers()
		fakeStall.WaitWorkers()
		close(workersDone)
	}()

	select {
	case <-workersDone:
		// All 16 workers exited cleanly
	case <-time.After(3 * time.Second):
		t.Fatal("workers remained blocked behind execution gates after cancellation")
	}

	// Verify that neither gate was released
	select {
	case <-startGate:
		t.Fatal("startGate was unexpectedly closed or received from")
	default:
	}
	select {
	case <-stallGate:
		t.Fatal("stallGate was unexpectedly closed or received from")
	default:
	}

	// Verify that none of the turns remain active
	for _, ref := range startRefs {
		if fakeStart.IsExecutionActive(ref) {
			t.Fatalf("execution remains active for startRef %+v", ref)
		}
	}
	for _, ref := range stallRefs {
		if fakeStall.IsExecutionActive(ref) {
			t.Fatalf("execution remains active for stallRef %+v", ref)
		}
	}
}

// TestReview264_ExplicitlyClosedSubscriptionsUnregister verifies that when subscribers
// call stream.Close() under a non-cancelled context (e.g. context.Background()),
// the fake's internal activeStreams registry is updated and the stream is unregistered.
func TestReview264_ExplicitlyClosedSubscriptionsUnregister(t *testing.T) {
	ctx := context.Background()

	startGate := make(chan struct{})
	fake := NewFake(ScriptedFaults{
		HoldExecutionStart: startGate,
	})
	defer fake.Close()

	sessionID := adapter.SessionID("sess-sub-unreg")
	_, err := fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   sessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	ref := adapter.TurnRef{SessionID: sessionID, TurnKey: "turn-unreg-1"}
	_, err = fake.Dispatch(ctx, ref, "test prompt")
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}

	// Establish 32 subscriptions under context.Background()
	var streams []adapter.Stream
	for i := 0; i < 32; i++ {
		st, err := fake.Observe(context.Background(), ref)
		if err != nil {
			t.Fatalf("Observe[%d] failed: %v", i, err)
		}
		streams = append(streams, st)
	}

	if count := fake.ActiveStreamCount(ref); count != 32 {
		t.Fatalf("expected 32 active subscriptions, got %d", count)
	}

	// Explicitly close all 32 subscriptions
	for i, s := range streams {
		if err := s.Close(); err != nil {
			t.Fatalf("stream[%d].Close failed: %v", i, err)
		}
	}

	// Allow brief watcher goroutine unregistration
	deadline := time.Now().Add(2 * time.Second)
	for fake.ActiveStreamCount(ref) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if count := fake.ActiveStreamCount(ref); count != 0 {
		t.Fatalf("expected 0 active subscriptions after explicit close, got %d", count)
	}
}
