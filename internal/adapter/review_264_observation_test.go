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

// preCompletingAdapter finishes execution before Observe is invoked, replaying the terminal event.
type preCompletingAdapter struct {
	*adaptertest.FakeAdapter
}

func (p *preCompletingAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	outcome, err := p.FakeAdapter.Dispatch(ctx, ref, prompt)
	if err != nil {
		return outcome, err
	}
	// Wait until worker completes naturally before returning dispatch outcome
	for {
		res, err := p.FakeAdapter.Collect(ctx, ref)
		if err == nil && res.Status == council.TurnCompleted {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	return outcome, nil
}

type preCompletingFixture struct {
	fake *adaptertest.FakeAdapter
}

func (p *preCompletingFixture) Adapter() adapter.Adapter {
	return &preCompletingAdapter{FakeAdapter: p.fake}
}
func (p *preCompletingFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	st := p.fake.TurnState(ref)
	return st.Received, st.Accepted, st.Started
}
func (p *preCompletingFixture) IsCompletionAllowed(ref adapter.TurnRef) bool { return true }
func (p *preCompletingFixture) IsExecutionActive(ref adapter.TurnRef) bool {
	return p.fake.IsExecutionActive(ref)
}
func (p *preCompletingFixture) Cleanup() error { return p.fake.Close() }

func TestReview264_ObservePreCompletedTurnTerminalReplay(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	fix := &preCompletingFixture{fake: fake}
	defer fix.Cleanup()

	// When a worker genuinely completes before Observe starts, Observe replays terminal event.
	// The conformance checker must not falsely flag this as an adapter violation!
	violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
	if len(violations) > 0 {
		t.Fatalf("expected 0 violations when turn completes prior to observation, got: %+v", violations)
	}
}
