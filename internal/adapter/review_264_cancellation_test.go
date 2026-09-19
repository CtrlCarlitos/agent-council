package adapter_test

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/conformance"
)

// 1. Adapter that truthfully returns CancelRequested while worker remains running
type cancelRequestedAdapter struct {
	*adaptertest.FakeAdapter
}

func (c *cancelRequestedAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	// Acknowledge the cancellation request without claiming termination
	return adapter.CancelOutcome{
		Ref:         ref,
		Disposition: adapter.CancelRequested,
		Reason:      "cancellation requested and pending termination",
	}, nil
}

type cancelRequestedFixture struct {
	fake  *adaptertest.FakeAdapter
	stall chan struct{}
}

func (c *cancelRequestedFixture) Adapter() adapter.Adapter {
	return &cancelRequestedAdapter{FakeAdapter: c.fake}
}
func (c *cancelRequestedFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	st := c.fake.TurnState(ref)
	return st.Received, st.Accepted, st.Started
}
func (c *cancelRequestedFixture) IsCompletionAllowed(ref adapter.TurnRef) bool { return false }
func (c *cancelRequestedFixture) IsExecutionActive(ref adapter.TurnRef) bool {
	return c.fake.IsExecutionActive(ref)
}
func (c *cancelRequestedFixture) Cleanup() error {
	select {
	case <-c.stall:
	default:
		close(c.stall)
	}
	return c.fake.Close()
}

func TestReview264_CancellationAcknowledgedWithoutFreeingExecution(t *testing.T) {
	stall := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StallStream: stall,
	})
	fix := &cancelRequestedFixture{fake: fake, stall: stall}
	defer fix.Cleanup()

	violations := conformance.Check(context.Background(), fix, conformance.ScenarioMidTurnCancellation)
	if len(violations) > 0 {
		t.Fatalf("expected 0 violations for truthfully acknowledged CancelRequested, got: %+v", violations)
	}
}

// 2. Adapter that falsely returns CancelConfirmed while worker remains running
type falseConfirmationAdapter struct {
	*adaptertest.FakeAdapter
}

func (f *falseConfirmationAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	// Falsely claims CancelConfirmed without cancelling the underlying worker!
	return adapter.CancelOutcome{
		Ref:         ref,
		Disposition: adapter.CancelConfirmed,
	}, nil
}

type falseConfirmationFixture struct {
	fake  *adaptertest.FakeAdapter
	stall chan struct{}
}

func (f *falseConfirmationFixture) Adapter() adapter.Adapter {
	return &falseConfirmationAdapter{FakeAdapter: f.fake}
}
func (f *falseConfirmationFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	st := f.fake.TurnState(ref)
	return st.Received, st.Accepted, st.Started
}
func (f *falseConfirmationFixture) IsCompletionAllowed(ref adapter.TurnRef) bool { return false }
func (f *falseConfirmationFixture) IsExecutionActive(ref adapter.TurnRef) bool {
	return f.fake.IsExecutionActive(ref)
}
func (f *falseConfirmationFixture) Cleanup() error {
	select {
	case <-f.stall:
	default:
		close(f.stall)
	}
	return f.fake.Close()
}

func TestReview264_CancellationCheckerDetectsFalseConfirmation(t *testing.T) {
	stall := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StallStream: stall,
	})
	fix := &falseConfirmationFixture{fake: fake, stall: stall}
	defer fix.Cleanup()

	violations := conformance.Check(context.Background(), fix, conformance.ScenarioMidTurnCancellation)
	if len(violations) == 0 {
		t.Fatal("expected violations for false CancelConfirmed claim while execution is active, got 0")
	}

	var foundViolation bool
	for _, v := range violations {
		if v.Code == conformance.ViolationUnexpectedError {
			foundViolation = true
			break
		}
	}
	if !foundViolation {
		t.Fatalf("expected ViolationUnexpectedError, got: %+v", violations)
	}
}

// 3. Adapter that falsely returns CancelAlreadyTerminal while worker remains running
type falseAlreadyTerminalAdapter struct {
	*adaptertest.FakeAdapter
}

func (f *falseAlreadyTerminalAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	// Falsely claims CancelAlreadyTerminal while worker is still running!
	return adapter.CancelOutcome{
		Ref:         ref,
		Disposition: adapter.CancelAlreadyTerminal,
	}, nil
}

type falseAlreadyTerminalFixture struct {
	fake  *adaptertest.FakeAdapter
	stall chan struct{}
}

func (f *falseAlreadyTerminalFixture) Adapter() adapter.Adapter {
	return &falseAlreadyTerminalAdapter{FakeAdapter: f.fake}
}
func (f *falseAlreadyTerminalFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	st := f.fake.TurnState(ref)
	return st.Received, st.Accepted, st.Started
}
func (f *falseAlreadyTerminalFixture) IsCompletionAllowed(ref adapter.TurnRef) bool { return false }
func (f *falseAlreadyTerminalFixture) IsExecutionActive(ref adapter.TurnRef) bool {
	return f.fake.IsExecutionActive(ref)
}
func (f *falseAlreadyTerminalFixture) Cleanup() error {
	select {
	case <-f.stall:
	default:
		close(f.stall)
	}
	return f.fake.Close()
}

func TestReview264_CancellationCheckerDetectsFalseAlreadyTerminal(t *testing.T) {
	stall := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StallStream: stall,
	})
	fix := &falseAlreadyTerminalFixture{fake: fake, stall: stall}
	defer fix.Cleanup()

	violations := conformance.Check(context.Background(), fix, conformance.ScenarioMidTurnCancellation)
	if len(violations) == 0 {
		t.Fatal("expected violations for false CancelAlreadyTerminal claim while execution is active, got 0")
	}

	var foundViolation bool
	for _, v := range violations {
		if v.Code == conformance.ViolationUnexpectedError {
			foundViolation = true
			break
		}
	}
	if !foundViolation {
		t.Fatalf("expected ViolationUnexpectedError, got: %+v", violations)
	}
}

// 4. Adapter that returns CancelOutcome with substituted/unrelated TurnRef
type substitutedTurnRefAdapter struct {
	*adaptertest.FakeAdapter
}

func (s *substitutedTurnRefAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	return adapter.CancelOutcome{
		Ref: adapter.TurnRef{
			SessionID: ref.SessionID,
			TurnKey:   "different-unrelated-turn-key",
		},
		Disposition: adapter.CancelConfirmed,
	}, nil
}

type substitutedTurnRefFixture struct {
	fake *adaptertest.FakeAdapter
}

func (s *substitutedTurnRefFixture) Adapter() adapter.Adapter {
	return &substitutedTurnRefAdapter{FakeAdapter: s.fake}
}
func (s *substitutedTurnRefFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	st := s.fake.TurnState(ref)
	return st.Received, st.Accepted, st.Started
}
func (s *substitutedTurnRefFixture) IsCompletionAllowed(ref adapter.TurnRef) bool { return false }
func (s *substitutedTurnRefFixture) IsExecutionActive(ref adapter.TurnRef) bool   { return false }
func (s *substitutedTurnRefFixture) Cleanup() error                               { return s.fake.Close() }

func TestReview264_CancellationCheckerDetectsSubstitutedTurnRef(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	fix := &substitutedTurnRefFixture{fake: fake}
	defer fix.Cleanup()

	violations := conformance.Check(context.Background(), fix, conformance.ScenarioMidTurnCancellation)
	if len(violations) == 0 {
		t.Fatal("expected ViolationReferenceSubstituted, got 0 violations")
	}

	var foundRefSub bool
	for _, v := range violations {
		if v.Code == conformance.ViolationReferenceSubstituted {
			foundRefSub = true
			break
		}
	}
	if !foundRefSub {
		t.Fatalf("expected ViolationReferenceSubstituted, got: %+v", violations)
	}
}
