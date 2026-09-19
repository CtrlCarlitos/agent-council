package adapter_test

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/conformance"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// brokenTerminalReplayAdapter wraps FakeAdapter:
// - Underlying worker is held behind unopened StallStream gate
// - Observe returns fabricated terminal replay
// - Collect returns fabricated completion before and after Close
type brokenTerminalReplayAdapter struct {
	*adaptertest.FakeAdapter
}

func (b *brokenTerminalReplayAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	stream := adapter.NewBufferedStream(ref, 16)
	_ = stream.Send(adapter.Event{
		Ref:       ref,
		Type:      adapter.EventTerminal,
		Status:    council.TurnCompleted,
		Payload:   "fabricated output",
		Timestamp: time.Now(),
	})
	_ = stream.CloseWithErr(nil)
	return stream, nil
}

func (b *brokenTerminalReplayAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	return adapter.TurnResult{
		Ref:          ref,
		Status:       council.TurnCompleted,
		ResultStatus: adapter.ResultAvailable,
		Output:       "fabricated output",
		CompletedAt:  time.Now(),
	}, nil
}

type brokenTerminalReplayFixture struct {
	fake  *adaptertest.FakeAdapter
	stall chan struct{}
}

func (b *brokenTerminalReplayFixture) Adapter() adapter.Adapter {
	return &brokenTerminalReplayAdapter{FakeAdapter: b.fake}
}
func (b *brokenTerminalReplayFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	st := b.fake.TurnState(ref)
	return st.Received, st.Accepted, st.Started
}
func (b *brokenTerminalReplayFixture) IsCompletionAllowed(ref adapter.TurnRef) bool {
	return false
}
func (b *brokenTerminalReplayFixture) IsExecutionActive(ref adapter.TurnRef) bool {
	return b.fake.IsExecutionActive(ref)
}
func (b *brokenTerminalReplayFixture) TerminalOutcome(ref adapter.TurnRef) council.TurnStatus {
	return ""
}
func (b *brokenTerminalReplayFixture) Cleanup() error {
	select {
	case <-b.stall:
	default:
		close(b.stall)
	}
	return b.fake.Close()
}

func TestReview62_FabricatedTerminalReplayDetected(t *testing.T) {
	stall := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StallStream: stall,
	})
	fix := &brokenTerminalReplayFixture{fake: fake, stall: stall}
	defer fix.Cleanup()

	violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
	if len(violations) == 0 {
		t.Fatal("expected conformance check to detect violation for fabricated terminal replay while execution active and completion not allowed, got 0 violations")
	}

	var foundViolation bool
	for _, v := range violations {
		if v.Code == conformance.ViolationEOFAsCompletion {
			foundViolation = true
			break
		}
	}
	if !foundViolation {
		t.Fatalf("expected ViolationEOFAsCompletion, got: %+v", violations)
	}
}

func TestReview62_GenuineTerminalReplayPasses(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	fix := &preCompletingFixture{fake: fake}
	defer fix.Cleanup()

	violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
	if len(violations) > 0 {
		t.Fatalf("expected 0 violations for genuine terminal replay, got: %+v", violations)
	}
}
