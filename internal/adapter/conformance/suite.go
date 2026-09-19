package conformance

import (
	"context"
	"fmt"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// Fixture provides an adapter under test and independent tracking inspection.
type Fixture interface {
	Adapter() adapter.Adapter
	TurnState(ref adapter.TurnRef) (received, accepted, started bool)
	Cleanup() error
}

// FixtureFactory constructs a fresh test fixture.
type FixtureFactory func(t *testing.T) Fixture

// ViolationCode identifies specific contract violations.
type ViolationCode string

const (
	ViolationEOFAsCompletion           ViolationCode = "EOF_TREATED_AS_COMPLETION"
	ViolationGenerationSubstituted     ViolationCode = "RECOVERY_GENERATION_SUBSTITUTED"
	ViolationSessionIDCollision        ViolationCode = "SESSION_ID_COLLISION"
	ViolationUnboundedPayload          ViolationCode = "UNBOUNDED_PAYLOAD_ACCEPTED"
	ViolationPrematureDenialRetirement ViolationCode = "PREMATURE_DENIAL_RETIREMENT"
	ViolationCapabilityFabricated      ViolationCode = "CAPABILITY_FABRICATED"
)

// Violation reports a structured failure of adapter contract conformance.
type Violation struct {
	Code        ViolationCode
	Description string
}

// Scenario identifies a conformance test scenario.
type Scenario string

const (
	ScenarioSessionIsolation          Scenario = "session_isolation"
	ScenarioObservationClose          Scenario = "observation_close"
	ScenarioRecoveryIntegrity         Scenario = "recovery_integrity"
	ScenarioPrematureDenialRetirement Scenario = "premature_denial_retirement"
)

// Check runs a specific conformance scenario against a fixture, returning all detected violations.
func Check(ctx context.Context, fixture Fixture, scenario Scenario) []Violation {
	var violations []Violation
	ad := fixture.Adapter()

	switch scenario {
	case ScenarioSessionIsolation:
		req1 := adapter.CreateSessionRequest{
			SessionID:   "session-1",
			Contributor: council.Claude,
			Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		b1, err1 := ad.CreateSession(ctx, req1)
		req2 := adapter.CreateSessionRequest{
			SessionID:   "session-2",
			Contributor: council.Claude,
			Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		b2, err2 := ad.CreateSession(ctx, req2)

		if err1 == nil && err2 == nil && b1.NativeSessionID == b2.NativeSessionID {
			violations = append(violations, Violation{
				Code:        ViolationSessionIDCollision,
				Description: "two distinct logical sessions bound to identical native session ID",
			})
		}

	case ScenarioObservationClose:
		ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}
		_, _ = ad.Dispatch(ctx, ref, "test prompt")
		stream, err := ad.Observe(ctx, ref)
		if err == nil {
			_ = stream.Close()
			// Stream closing must not fabricate completion in Collect
			res, err := ad.Collect(ctx, ref)
			if err == nil && res.Status == council.TurnCompleted {
				violations = append(violations, Violation{
					Code:        ViolationEOFAsCompletion,
					Description: "closing stream fabricated terminal turn completion",
				})
			}
		}

	case ScenarioRecoveryIntegrity:
		ref := adapter.RecoveryRef{
			TurnRef:    adapter.TurnRef{SessionID: "s1", TurnKey: "t1"},
			Generation: 2,
		}
		outcome, err := ad.Reconcile(ctx, ref)
		if err == nil && outcome.Ref.Generation != ref.Generation {
			violations = append(violations, Violation{
				Code:        ViolationGenerationSubstituted,
				Description: fmt.Sprintf("reconciliation substituted generation: expected %d, got %d", ref.Generation, outcome.Ref.Generation),
			})
		}

	case ScenarioPrematureDenialRetirement:
		ref := adapter.TurnRef{SessionID: "s-deny", TurnKey: "t-deny"}
		_, _ = ad.Dispatch(ctx, ref, "test tool denial")
		stream, err := ad.Observe(ctx, ref)
		if err == nil {
			for ev := range stream.Events() {
				if ev.Type == adapter.EventToolDenied {
					if ev.Status != council.TurnRunning {
						violations = append(violations, Violation{
							Code:        ViolationPrematureDenialRetirement,
							Description: fmt.Sprintf("EventToolDenied carried non-running status: %s", ev.Status),
						})
					}
				}
			}
		}
	}

	return violations
}

// Run executes the full conformance suite against an adapter fixture factory.
func Run(t *testing.T, factory FixtureFactory) {
	t.Helper()

	t.Run("SessionIsolation", func(t *testing.T) {
		f := factory(t)
		defer func() { _ = f.Cleanup() }()
		violations := Check(context.Background(), f, ScenarioSessionIsolation)
		if len(violations) > 0 {
			t.Fatalf("unexpected violations: %+v", violations)
		}
	})

	t.Run("ObservationCloseDoesNotComplete", func(t *testing.T) {
		f := factory(t)
		defer func() { _ = f.Cleanup() }()
		violations := Check(context.Background(), f, ScenarioObservationClose)
		if len(violations) > 0 {
			t.Fatalf("unexpected violations: %+v", violations)
		}
	})

	t.Run("RecoveryGenerationIntegrity", func(t *testing.T) {
		f := factory(t)
		defer func() { _ = f.Cleanup() }()
		violations := Check(context.Background(), f, ScenarioRecoveryIntegrity)
		if len(violations) > 0 {
			t.Fatalf("unexpected violations: %+v", violations)
		}
	})

	t.Run("PrematureDenialRetirement", func(t *testing.T) {
		f := factory(t)
		defer func() { _ = f.Cleanup() }()
		violations := Check(context.Background(), f, ScenarioPrematureDenialRetirement)
		if len(violations) > 0 {
			t.Fatalf("unexpected violations: %+v", violations)
		}
	})
}
