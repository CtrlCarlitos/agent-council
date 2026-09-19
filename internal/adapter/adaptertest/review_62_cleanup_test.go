package adaptertest

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func runFixtureCloseReleasesSubscriptionsTest(t *testing.T, holdExecutionStart bool) {
	ctx := context.Background()

	var gate chan struct{} = make(chan struct{})
	var faults ScriptedFaults
	if holdExecutionStart {
		faults.HoldExecutionStart = gate
	} else {
		faults.StallStream = gate
	}

	fake := NewFake(faults)

	ref := adapter.TurnRef{SessionID: "s-close-test", TurnKey: "t1"}
	_, err := fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   ref.SessionID,
		Contributor: council.Claude,
		Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
	})
	if err != nil {
		t.Fatal(err)
	}

	outcome, err := fake.Dispatch(ctx, ref, "test prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch failed: outcome=%+v err=%v", outcome, err)
	}

	// Open four observation subscriptions using context.Background()
	var streams []adapter.Stream
	for i := 0; i < 4; i++ {
		st, err := fake.Observe(context.Background(), ref)
		if err != nil {
			t.Fatalf("Observe[%d] failed: %v", i, err)
		}
		streams = append(streams, st)
	}

	if count := fake.ActiveStreamCount(ref); count != 4 {
		t.Fatalf("expected 4 registered active streams, got %d", count)
	}

	// Call fake.Close() without manually closing the subscriptions
	if err := fake.Close(); err != nil {
		t.Fatalf("fake.Close error: %v", err)
	}

	// 1. All streams must be closed
	for i, s := range streams {
		bs, ok := s.(*adapter.BufferedStream)
		if !ok {
			t.Fatalf("stream[%d] is not *adapter.BufferedStream", i)
		}
		select {
		case <-bs.Done():
			// stream is closed
		default:
			t.Fatalf("stream[%d] remained open after fake.Close()", i)
		}
	}

	// 2. All streams must be unregistered
	if count := fake.ActiveStreamCount(ref); count != 0 {
		t.Fatalf("expected 0 active streams registered after fake.Close(), got %d", count)
	}

	// 3. Execution must no longer be active
	if fake.IsExecutionActive(ref) {
		t.Fatalf("expected IsExecutionActive to be false after fake.Close(), but was true")
	}

	// 4. The test-owned gate must not have been released or read from
	select {
	case <-gate:
		t.Fatal("gate was unexpectedly closed or received from")
	default:
	}
}

func TestReview62_FixtureCloseReleasesSubscriptions_HoldExecutionStart(t *testing.T) {
	runFixtureCloseReleasesSubscriptionsTest(t, true)
}

func TestReview62_FixtureCloseReleasesSubscriptions_StallStream(t *testing.T) {
	runFixtureCloseReleasesSubscriptionsTest(t, false)
}
