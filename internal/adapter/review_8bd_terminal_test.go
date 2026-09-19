package adapter_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/conformance"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// genuineCancellationAdapter cancels the turn prior to observation replay and waits for worker exit.
type genuineCancellationAdapter struct {
	*adaptertest.FakeAdapter
}

func (g *genuineCancellationAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	outcome, err := g.FakeAdapter.Dispatch(ctx, ref, prompt)
	if err != nil {
		return outcome, err
	}
	// Cancel the turn while held behind completion gate
	_, err = g.FakeAdapter.Cancel(ctx, ref)
	if err != nil {
		return outcome, err
	}
	g.FakeAdapter.WaitWorkers()
	return outcome, nil
}

type genuineCancellationFixture struct {
	fake  *adaptertest.FakeAdapter
	stall chan struct{}
}

func (g *genuineCancellationFixture) Adapter() adapter.Adapter {
	return &genuineCancellationAdapter{FakeAdapter: g.fake}
}
func (g *genuineCancellationFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	st := g.fake.TurnState(ref)
	return st.Received, st.Accepted, st.Started
}
func (g *genuineCancellationFixture) IsCompletionAllowed(ref adapter.TurnRef) bool {
	return false // Completion gate remains unopened
}
func (g *genuineCancellationFixture) IsExecutionActive(ref adapter.TurnRef) bool {
	return g.fake.IsExecutionActive(ref)
}
func (g *genuineCancellationFixture) TerminalOutcome(ref adapter.TurnRef) council.TurnStatus {
	return g.fake.ExecutionStatus(ref)
}
func (g *genuineCancellationFixture) Cleanup() error {
	select {
	case <-g.stall:
	default:
		close(g.stall)
	}
	return g.fake.Close()
}

// TestReview8BD_GenuineCancellationReplayPasses verifies that genuine cancellation replay
// is accepted with zero violations even while the success completion gate remains closed.
func TestReview8BD_GenuineCancellationReplayPasses(t *testing.T) {
	stall := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StallStream: stall,
	})
	fix := &genuineCancellationFixture{fake: fake, stall: stall}
	defer fix.Cleanup()

	violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
	if len(violations) > 0 {
		t.Fatalf("expected 0 violations for genuine cancellation replay, got: %+v", violations)
	}
}

// fabricatedCollectionAdapter fabricates a terminal status at collection 1 (before close)
// or collection 2 (after close) while underlying worker execution remains active.
type fabricatedCollectionAdapter struct {
	*adaptertest.FakeAdapter
	targetStatus council.TurnStatus
	fabricateAt  string // "before" or "after"
	collectCount int
}

func (f *fabricatedCollectionAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	f.collectCount++
	res, err := f.FakeAdapter.Collect(ctx, ref)
	if err != nil {
		return res, err
	}
	if (f.fabricateAt == "before" && f.collectCount == 1) ||
		(f.fabricateAt == "after" && f.collectCount == 2) {
		res.Status = f.targetStatus
		res.ResultStatus = adapter.ResultAvailable
	}
	return res, nil
}

type fabricatedCollectionFixture struct {
	fake *adaptertest.FakeAdapter
	gate chan struct{}
	ad   *fabricatedCollectionAdapter
}

func (f *fabricatedCollectionFixture) Adapter() adapter.Adapter { return f.ad }
func (f *fabricatedCollectionFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	st := f.fake.TurnState(ref)
	return st.Received, st.Accepted, st.Started
}
func (f *fabricatedCollectionFixture) IsCompletionAllowed(ref adapter.TurnRef) bool { return false }
func (f *fabricatedCollectionFixture) IsExecutionActive(ref adapter.TurnRef) bool {
	return f.fake.IsExecutionActive(ref)
}
func (f *fabricatedCollectionFixture) TerminalOutcome(ref adapter.TurnRef) council.TurnStatus {
	return f.fake.ExecutionStatus(ref)
}
func (f *fabricatedCollectionFixture) Cleanup() error {
	select {
	case <-f.gate:
	default:
		close(f.gate)
	}
	return f.fake.Close()
}

// TestReview8BD_FabricatedTerminalCollectionDetected verifies that the conformance checker
// detects fabricated terminal collection claims across the whole terminal matrix
// (TurnCompleted, TurnCancelled, TurnFailed, TurnInterrupted) both before and after stream close.
func TestReview8BD_FabricatedTerminalCollectionDetected(t *testing.T) {
	cases := []struct {
		point  string
		status council.TurnStatus
	}{
		{point: "before", status: council.TurnCompleted},
		{point: "before", status: council.TurnCancelled},
		{point: "before", status: council.TurnFailed},
		{point: "before", status: council.TurnInterrupted},
		{point: "after", status: council.TurnCompleted},
		{point: "after", status: council.TurnCancelled},
		{point: "after", status: council.TurnFailed},
		{point: "after", status: council.TurnInterrupted},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_%s", tc.point, tc.status), func(t *testing.T) {
			gate := make(chan struct{})
			fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
				HoldExecutionStart: gate,
			})
			ad := &fabricatedCollectionAdapter{
				FakeAdapter:  fake,
				targetStatus: tc.status,
				fabricateAt:  tc.point,
			}
			fix := &fabricatedCollectionFixture{
				fake: fake,
				gate: gate,
				ad:   ad,
			}
			defer fix.Cleanup()

			violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
			if len(violations) == 0 {
				t.Fatalf("expected violation when %s close collection claims %s while worker is active, got 0", tc.point, tc.status)
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
		})
	}
}
