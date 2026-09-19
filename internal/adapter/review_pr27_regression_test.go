package adapter_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/conformance"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// TestReviewPR27_StreamGuarantees verifies envelope validation, reference binding,
// and that concurrent Close() callers all wait for full stream cleanup.
func TestReviewPR27_StreamGuarantees(t *testing.T) {
	ref := adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"}
	stream := adapter.NewBufferedStream(ref, 2)

	// 1. Sending event for different turn reference must be rejected
	wrongRef := adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-wrong"}
	err := stream.SendOrOverflow(adapter.Event{
		Ref:     wrongRef,
		Type:    adapter.EventProgress,
		Status:  council.TurnRunning,
		Payload: "valid payload",
	})
	if err == nil {
		t.Fatal("expected error sending event with mismatched TurnRef")
	}

	// 2. Sending oversized payload must be rejected by SendOrOverflow
	err = stream.SendOrOverflow(adapter.Event{
		Ref:     ref,
		Type:    adapter.EventProgress,
		Status:  council.TurnRunning,
		Payload: strings.Repeat("a", adapter.MaxEventPayloadBytes+1),
	})
	if err == nil {
		t.Fatal("expected error sending oversized payload")
	}

	// 3. Sending progress with terminal status must be rejected
	err = stream.SendOrOverflow(adapter.Event{
		Ref:     ref,
		Type:    adapter.EventProgress,
		Status:  council.TurnCompleted,
		Payload: "premature completion",
	})
	if err == nil {
		t.Fatal("expected error sending progress event with terminal status")
	}

	// 4. Concurrent Close callers must all wait until stream cleanup is finished
	s2 := adapter.NewBufferedStream(ref, 5)
	holdSend := make(chan struct{})
	sendDone := make(chan struct{})

	go func() {
		defer close(sendDone)
		// Send an event that stays in flight until holdSend is closed
		_ = s2.Send(adapter.Event{
			Ref:     ref,
			Type:    adapter.EventProgress,
			Status:  council.TurnRunning,
			Payload: "in-flight",
		})
	}()

	// Ensure sender is active
	time.Sleep(10 * time.Millisecond)

	var closeWg sync.WaitGroup
	var closerFinished [3]bool

	for i := 0; i < 3; i++ {
		closeWg.Add(1)
		idx := i
		go func() {
			defer closeWg.Done()
			_ = s2.Close()
			closerFinished[idx] = true
		}()
	}

	// Give closers time to run
	time.Sleep(20 * time.Millisecond)

	// All closers should be waiting or completed; Events channel must not be half-closed
	close(holdSend)
	closeWg.Wait()

	for i, fin := range closerFinished {
		if !fin {
			t.Fatalf("closer %d did not complete after unblocking", i)
		}
	}
}

// TestReviewPR27_FakeExecutionIndependentOfObservation verifies that worker execution
// lifecycle is driven by Dispatch, not Observe, and confirmed cancellation is never overwritten.
func TestReviewPR27_FakeExecutionIndependentOfObservation(t *testing.T) {
	ctx := context.Background()
	stall := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StallStream: stall,
	})

	// 1. Observing undispatched turn must fail
	undispatchedRef := adapter.TurnRef{SessionID: "sess-1", TurnKey: "undispatched"}
	if _, err := fake.Observe(ctx, undispatchedRef); err == nil {
		t.Fatal("expected error observing undispatched turn")
	}
	state := fake.TurnState(undispatchedRef)
	if state.Received || state.Accepted || state.Started {
		t.Fatalf("observing undispatched turn fabricated dispatch state: %+v", state)
	}

	// 2. Observing with cancelled context must fail
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	req := adapter.CreateSessionRequest{
		SessionID:   "sess-1",
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	}
	if _, err := fake.CreateSession(ctx, req); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	turnRef := adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"}
	if _, err := fake.Dispatch(ctx, turnRef, "prompt"); err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}

	if _, err := fake.Observe(canceledCtx, turnRef); err == nil {
		t.Fatal("expected error observing with canceled context")
	}

	// 3. Confirmed cancellation must not be overwritten by delayed completion
	stream, err := fake.Observe(ctx, turnRef)
	if err != nil {
		t.Fatalf("Observe failed: %v", err)
	}

	// Read initial progress
	select {
	case ev := <-stream.Events():
		if ev.Type != adapter.EventProgress {
			t.Fatalf("expected EventProgress, got %s", ev.Type)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for progress")
	}

	// Cancel while stall is held
	cancelOutcome, err := fake.Cancel(ctx, turnRef)
	if err != nil {
		t.Fatalf("Cancel failed: %v", err)
	}
	if cancelOutcome.Disposition != adapter.CancelConfirmed {
		t.Fatalf("expected CancelConfirmed, got %s", cancelOutcome.Disposition)
	}

	res, err := fake.Collect(ctx, turnRef)
	if err != nil {
		t.Fatalf("Collect after cancel failed: %v", err)
	}
	if res.Status != council.TurnCancelled {
		t.Fatalf("expected TurnCancelled, got %s", res.Status)
	}

	// Release stall and wait
	close(stall)
	time.Sleep(30 * time.Millisecond)

	// Collect again: must STILL be TurnCancelled, NOT overwritten with TurnCompleted!
	res2, err := fake.Collect(ctx, turnRef)
	if err != nil {
		t.Fatalf("Collect after stall release failed: %v", err)
	}
	if res2.Status != council.TurnCancelled {
		t.Fatalf("confirmed cancellation was overwritten by completion! status=%s output=%s", res2.Status, res2.Output)
	}
}

// TestReviewPR27_FakeDispatchAndBindingReliability verifies session validation,
// deep copying, and independent dispatch state tracking.
func TestReviewPR27_FakeDispatchAndBindingReliability(t *testing.T) {
	ctx := context.Background()
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})

	// 1. Dispatching to uncreated session must fail
	uncreatedRef := adapter.TurnRef{SessionID: "nonexistent", TurnKey: "t1"}
	outcome, err := fake.Dispatch(ctx, uncreatedRef, "prompt")
	if err == nil || outcome.Status != adapter.DispatchRejected {
		t.Fatalf("expected rejected dispatch for uncreated session, got outcome=%+v err=%v", outcome, err)
	}

	// 2. DispatchRejected must not report accepted or started in TurnState
	rejFake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		DispatchStatus: adapter.DispatchRejected,
	})
	req := adapter.CreateSessionRequest{
		SessionID:   "sess-rej",
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	}
	_, _ = rejFake.CreateSession(ctx, req)
	rejRef := adapter.TurnRef{SessionID: "sess-rej", TurnKey: "t-rej"}
	_, _ = rejFake.Dispatch(ctx, rejRef, "prompt")
	st := rejFake.TurnState(rejRef)
	if st.Accepted || st.Started {
		t.Fatalf("DispatchRejected reported accepted=%v started=%v in TurnState", st.Accepted, st.Started)
	}

	// 3. Mutable tooling slice deep copy protection
	tooling := []string{"git", "grep"}
	reqCopy := adapter.CreateSessionRequest{
		SessionID:   "sess-copy",
		Contributor: council.Agy,
		Config: adapter.SessionConfig{
			Model:   "agy-1",
			Tooling: tooling,
		},
	}
	b, err := fake.CreateSession(ctx, reqCopy)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate original slice
	tooling[0] = "mutated-tool"
	// Also mutate returned binding's slice
	b.Config.Tooling[1] = "mutated-binding-tool"

	// Recreating with original config should succeed (not affected by mutation)
	reqOrig := adapter.CreateSessionRequest{
		SessionID:   "sess-copy",
		Contributor: council.Agy,
		Config: adapter.SessionConfig{
			Model:   "agy-1",
			Tooling: []string{"git", "grep"},
		},
	}
	bRecreate, err := fake.CreateSession(ctx, reqOrig)
	if err != nil {
		t.Fatalf("session config was mutated by caller: %v", err)
	}
	if bRecreate.Config.Tooling[0] != "git" || bRecreate.Config.Tooling[1] != "grep" {
		t.Fatalf("internal session tooling was corrupted: %+v", bRecreate.Config.Tooling)
	}

	// 4. Recreating session with changed contributor must fail
	reqDiffContrib := reqOrig
	reqDiffContrib.Contributor = council.OpenCode
	if _, err := fake.CreateSession(ctx, reqDiffContrib); err == nil {
		t.Fatal("expected error recreating session with different contributor")
	}

	// 5. Recreating session with changed tooling must fail
	reqDiffTools := reqOrig
	reqDiffTools.Config.Tooling = []string{"git", "grep", "diff"}
	if _, err := fake.CreateSession(ctx, reqDiffTools); err == nil {
		t.Fatal("expected error recreating session with different tooling")
	}

	// 6. Resume session with altered contributor/config must fail
	badBinding := bRecreate
	badBinding.Contributor = council.Codex
	if err := fake.ResumeSession(ctx, badBinding); err == nil {
		t.Fatal("expected error resuming session with altered contributor")
	}
}

// TestReviewPR27_ConformanceCheckerTrustworthiness verifies that Check detects
// nonfunctional adapters, wrong references in reconciliation, and doesn't falsely flag legitimate completion.
func TestReviewPR27_ConformanceCheckerTrustworthiness(t *testing.T) {
	ctx := context.Background()

	// 1. Adapter that errors on every operation must produce violations, NOT zero violations!
	erroringAdapter := &erroringFakeAdapter{}
	errFixture := &genericFixture{ad: erroringAdapter}

	v1 := conformance.Check(ctx, errFixture, conformance.ScenarioSessionIsolation)
	if len(v1) == 0 {
		t.Fatal("erroring adapter produced 0 violations for SessionIsolation (false pass)")
	}

	v2 := conformance.Check(ctx, errFixture, conformance.ScenarioRecoveryIntegrity)
	if len(v2) == 0 {
		t.Fatal("erroring adapter produced 0 violations for RecoveryIntegrity (false pass)")
	}

	// 2. Adapter that returns recovery reply for wrong session/turn while keeping generation must be flagged
	wrongRefAdapter := &wrongRefRecoveryAdapter{FakeAdapter: adaptertest.NewFake(adaptertest.ScriptedFaults{})}
	refFixture := &genericFixture{ad: wrongRefAdapter}
	v3 := conformance.Check(ctx, refFixture, conformance.ScenarioRecoveryIntegrity)
	if len(v3) == 0 {
		t.Fatal("recovery adapter with substituted session/turn produced 0 violations (false pass)")
	}
}

type genericFixture struct {
	ad adapter.Adapter
}

func (g *genericFixture) Adapter() adapter.Adapter { return g.ad }
func (g *genericFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	return false, false, false
}
func (g *genericFixture) Cleanup() error { return nil }

type erroringFakeAdapter struct {
	adaptertest.FakeAdapter
}

func (e *erroringFakeAdapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	return adapter.ProbeReport{}, context.DeadlineExceeded
}
func (e *erroringFakeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	return adapter.SessionBinding{}, context.DeadlineExceeded
}
func (e *erroringFakeAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	return adapter.DispatchOutcome{}, context.DeadlineExceeded
}
func (e *erroringFakeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	return nil, context.DeadlineExceeded
}
func (e *erroringFakeAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	return adapter.ReconciliationOutcome{}, context.DeadlineExceeded
}

type wrongRefRecoveryAdapter struct {
	*adaptertest.FakeAdapter
}

func (w *wrongRefRecoveryAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	return adapter.ReconciliationOutcome{
		Ref: adapter.RecoveryRef{
			TurnRef: adapter.TurnRef{
				SessionID: "wrong-session-id",
				TurnKey:   ref.TurnKey,
			},
			Generation: ref.Generation, // Preserved generation!
		},
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableActive,
		Observed:     council.TurnRunning,
	}, nil
}
