package adaptertest_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func TestFakeAdapter_DispatchGateAndTracking(t *testing.T) {
	holdAck := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		HoldDispatchAck: holdAck,
		DispatchStatus:  adapter.DispatchUnknown,
	})

	ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}
	_, err := fake.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   ref.SessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	doneDispatch := make(chan adapter.DispatchOutcome)
	go func() {
		outcome, _ := fake.Dispatch(context.Background(), ref, "prompt")
		doneDispatch <- outcome
	}()

	// Verify received immediately before gate opens
	select {
	case <-doneDispatch:
		t.Fatal("dispatch returned before gate opened")
	case <-time.After(50 * time.Millisecond):
	}

	state := fake.TurnState(ref)
	if !state.Received || !state.Started {
		t.Fatalf("expected received and started, got %+v", state)
	}

	// Release gate
	close(holdAck)

	select {
	case outcome := <-doneDispatch:
		if outcome.Status != adapter.DispatchUnknown {
			t.Fatalf("expected DispatchUnknown, got %s", outcome.Status)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("dispatch did not unblock after gate release")
	}
}

func TestFakeAdapter_ReconcileHonorsStaleRef(t *testing.T) {
	staleRef := &adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "s1", TurnKey: "t1"},
		Generation: 1,
	}

	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StaleRecoveryRef: staleRef,
	})

	queryRef := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "s1", TurnKey: "t1"},
		Generation: 2,
	}

	outcome, err := fake.Reconcile(context.Background(), queryRef)
	if err != nil {
		t.Fatalf("reconcile returned unexpected error: %v", err)
	}

	// Fake must deliver the exact scripted stale reference without auto-repairing
	if outcome.Ref.Generation != 1 {
		t.Fatalf("fake auto-repaired generation: expected 1, got %d", outcome.Ref.Generation)
	}
}

func TestFakeAdapter_SessionLifecycle(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	ctx := context.Background()

	req := adapter.CreateSessionRequest{
		SessionID:   "sess-alpha",
		Contributor: council.Claude,
		Config: adapter.SessionConfig{
			WorkspaceRoot: "/tmp/ws",
			Model:         "claude-3-5-sonnet",
		},
	}

	b1, err := fake.CreateSession(ctx, req)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if b1.NativeSessionID == "" {
		t.Fatal("expected non-empty native session ID")
	}

	// Idempotent creation with same config
	b2, err := fake.CreateSession(ctx, req)
	if err != nil {
		t.Fatalf("idempotent CreateSession failed: %v", err)
	}
	if b2.NativeSessionID != b1.NativeSessionID {
		t.Fatalf("expected same native session ID %s, got %s", b1.NativeSessionID, b2.NativeSessionID)
	}

	// Recreate with conflicting config fails
	reqConflict := req
	reqConflict.Config.Model = "other-model"
	if _, err := fake.CreateSession(ctx, reqConflict); err == nil {
		t.Fatal("expected error recreating session with conflicting config")
	}

	// Resume existing session succeeds
	if err := fake.ResumeSession(ctx, b1); err != nil {
		t.Fatalf("ResumeSession failed: %v", err)
	}

	// Resume unknown session fails
	bUnknown := b1
	bUnknown.NativeSessionID = "unknown-id"
	if err := fake.ResumeSession(ctx, bUnknown); err == nil {
		t.Fatal("expected error resuming unknown session")
	}
}

func TestFakeAdapter_DropStreamEarly(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		DropStreamEarly: true,
	})

	ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}
	_, err := fake.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   ref.SessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	_, err = fake.Dispatch(context.Background(), ref, "prompt")
	if err != nil {
		t.Fatalf("dispatch failed: %v", err)
	}

	stream, err := fake.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("observe failed: %v", err)
	}

	var events []adapter.Event
	for ev := range stream.Events() {
		events = append(events, ev)
	}

	if len(events) == 0 {
		t.Fatal("expected at least initial event before drop")
	}
	if stream.Err() == nil {
		t.Fatal("expected stream.Err() to report error on dropped stream")
	}
}

func TestFakeAdapter_ToolDenialStream(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		AutoDenyTools: map[string]bool{"bash": true},
	})

	ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}
	_, err := fake.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   ref.SessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet", Tooling: []string{"bash"}},
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	_, _ = fake.Dispatch(context.Background(), ref, "run bash")

	stream, err := fake.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("observe failed: %v", err)
	}

	var sawToolDenied bool
	for ev := range stream.Events() {
		if ev.Type == adapter.EventToolDenied {
			sawToolDenied = true
			if ev.Status != council.TurnRunning {
				t.Fatalf("EventToolDenied must carry TurnRunning, got %s", ev.Status)
			}
		}
	}

	if !sawToolDenied {
		t.Fatal("expected EventToolDenied event in stream")
	}

	// Verify turn is still completed eventually
	res, err := fake.Collect(context.Background(), ref)
	if err != nil {
		t.Fatalf("collect failed: %v", err)
	}
	if res.Status != council.TurnCompleted {
		t.Fatalf("expected turn completed, got %s", res.Status)
	}
}

func TestFakeAdapter_CollectMalformed(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		MalformedOutput: true,
	})

	ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}
	_, err := fake.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   ref.SessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	_, _ = fake.Dispatch(context.Background(), ref, "prompt")

	res, err := fake.Collect(context.Background(), ref)
	if err == nil {
		t.Fatal("expected error on collect malformed")
	}
	if res.ResultStatus != adapter.ResultMalformed {
		t.Fatalf("expected ResultMalformed, got %s", res.ResultStatus)
	}
}
