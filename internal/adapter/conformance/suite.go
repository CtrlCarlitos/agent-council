package conformance

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// Fixture provides an adapter under test and independent tracking inspection.
type Fixture interface {
	Adapter() adapter.Adapter
	TurnState(ref adapter.TurnRef) (received, accepted, started bool)
	IsCompletionAllowed(ref adapter.TurnRef) bool
	Cleanup() error
}

// FixtureFactory constructs a fresh test fixture.
type FixtureFactory func(t *testing.T) Fixture

// ViolationCode identifies specific contract violations.
type ViolationCode string

const (
	ViolationEOFAsCompletion           ViolationCode = "EOF_TREATED_AS_COMPLETION"
	ViolationGenerationSubstituted     ViolationCode = "RECOVERY_GENERATION_SUBSTITUTED"
	ViolationReferenceSubstituted      ViolationCode = "RECOVERY_REFERENCE_SUBSTITUTED"
	ViolationSessionIDCollision        ViolationCode = "SESSION_ID_COLLISION"
	ViolationUnboundedPayload          ViolationCode = "UNBOUNDED_PAYLOAD_ACCEPTED"
	ViolationPrematureDenialRetirement ViolationCode = "PREMATURE_DENIAL_RETIREMENT"
	ViolationCapabilityFabricated      ViolationCode = "CAPABILITY_FABRICATED"
	ViolationPrerequisiteFailed        ViolationCode = "PREREQUISITE_FAILED"
	ViolationUnexpectedError           ViolationCode = "UNEXPECTED_ERROR"
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
	ScenarioSessionResumption         Scenario = "session_resumption"
	ScenarioMidTurnCancellation       Scenario = "mid_turn_cancellation"
)

// Check runs a specific conformance scenario against a fixture, returning all detected violations.
func Check(ctx context.Context, fixture Fixture, scenario Scenario) []Violation {
	var violations []Violation
	ad := fixture.Adapter()

	probeReport, probeErr := ad.Probe(ctx)
	if probeErr != nil {
		violations = append(violations, Violation{
			Code:        ViolationUnexpectedError,
			Description: fmt.Sprintf("probe failed: %v", probeErr),
		})
		return violations
	}

	switch scenario {
	case ScenarioSessionIsolation:
		req1 := adapter.CreateSessionRequest{
			SessionID:   "session-iso-1",
			Contributor: council.Claude,
			Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		b1, err1 := ad.CreateSession(ctx, req1)
		req2 := adapter.CreateSessionRequest{
			SessionID:   "session-iso-2",
			Contributor: council.Claude,
			Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		b2, err2 := ad.CreateSession(ctx, req2)

		if err1 != nil || err2 != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("session creation failed: err1=%v err2=%v", err1, err2),
			})
			return violations
		}

		if b1.NativeSessionID == b2.NativeSessionID {
			violations = append(violations, Violation{
				Code:        ViolationSessionIDCollision,
				Description: "two distinct logical sessions bound to identical native session ID",
			})
		}

	case ScenarioObservationClose:
		if probeReport.Capabilities.StreamingObservation == adapter.CapabilityUnsupported {
			ref := adapter.TurnRef{SessionID: "s-obs", TurnKey: "t-obs"}
			_, err := ad.Observe(ctx, ref)
			if err == nil {
				violations = append(violations, Violation{
					Code:        ViolationCapabilityFabricated,
					Description: "adapter advertised StreamingObservation as unsupported but Observe succeeded",
				})
			} else if !errors.Is(err, adapter.ErrUnsupportedCapability) {
				violations = append(violations, Violation{
					Code:        ViolationUnexpectedError,
					Description: fmt.Sprintf("unsupported StreamingObservation returned unexpected error: %v", err),
				})
			}
			return violations
		}

		req := adapter.CreateSessionRequest{
			SessionID:   "s-obs",
			Contributor: council.Claude,
			Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		if _, err := ad.CreateSession(ctx, req); err != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("session creation failed: %v", err),
			})
			return violations
		}

		ref := adapter.TurnRef{SessionID: "s-obs", TurnKey: "t-obs"}
		outcome, err := ad.Dispatch(ctx, ref, "test observation prompt")
		if err != nil || outcome.Status != adapter.DispatchAccepted {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("dispatch failed or rejected: err=%v outcome=%+v", err, outcome),
			})
			return violations
		}

		rec, acc, _ := fixture.TurnState(ref)
		if !rec || !acc {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("fixture turn state did not record dispatch: rec=%v acc=%v", rec, acc),
			})
			return violations
		}

		stream, err := ad.Observe(ctx, ref)
		if err != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("observe failed: %v", err),
			})
			return violations
		}

		// Wait for initial progress event proving active stream
		var sawProgress bool
		select {
		case ev, ok := <-stream.Events():
			if ok && ev.Type == adapter.EventProgress && ev.Status == council.TurnRunning {
				sawProgress = true
			}
		case <-time.After(200 * time.Millisecond):
		}

		if !sawProgress {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: "observation stream did not emit active progress event",
			})
			_ = stream.Close()
			return violations
		}

		// Check status before closing stream to distinguish natural completion from fabricated completion
		resBefore, errBefore := ad.Collect(ctx, ref)
		if errBefore != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("collect before stream close failed: %v", errBefore),
			})
			_ = stream.Close()
			return violations
		}

		// Close observation stream
		_ = stream.Close()

		// Collect status immediately after closing stream
		resAfter, errAfter := ad.Collect(ctx, ref)
		if errAfter != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("collect after stream close failed: %v", errAfter),
			})
			return violations
		}

		// If the turn was actively running before Close(), but immediately jumps to Completed upon Close(),
		// verify whether completion was legitimately allowed by independent fixture gates.
		if resBefore.Status == council.TurnRunning && resAfter.Status == council.TurnCompleted {
			if !fixture.IsCompletionAllowed(ref) {
				violations = append(violations, Violation{
					Code:        ViolationEOFAsCompletion,
					Description: "closing stream fabricated terminal turn completion while completion was not allowed",
				})
			}
		}

	case ScenarioRecoveryIntegrity:
		ref := adapter.RecoveryRef{
			TurnRef:    adapter.TurnRef{SessionID: "s-rec", TurnKey: "t-rec"},
			Generation: 2,
		}
		outcome, err := ad.Reconcile(ctx, ref)
		if err != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("reconciliation failed: %v", err),
			})
			return violations
		}

		if err := outcome.Validate(); err != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("reconciliation outcome validation failed: %v", err),
			})
		}

		if outcome.Ref.Generation != ref.Generation {
			violations = append(violations, Violation{
				Code:        ViolationGenerationSubstituted,
				Description: fmt.Sprintf("reconciliation substituted generation: expected %d, got %d", ref.Generation, outcome.Ref.Generation),
			})
		}

		if outcome.Ref.SessionID != ref.SessionID || outcome.Ref.TurnKey != ref.TurnKey {
			violations = append(violations, Violation{
				Code:        ViolationReferenceSubstituted,
				Description: fmt.Sprintf("reconciliation substituted turn ref: expected %+v, got %+v", ref.TurnRef, outcome.Ref.TurnRef),
			})
		}

	case ScenarioPrematureDenialRetirement:
		if probeReport.Capabilities.ToolApprovalRouting == adapter.CapabilityUnsupported {
			return nil
		}

		req := adapter.CreateSessionRequest{
			SessionID:   "s-deny",
			Contributor: council.Agy,
			Config:      adapter.SessionConfig{Model: "agy-1", Tooling: []string{"bash"}},
		}
		if _, err := ad.CreateSession(ctx, req); err != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("session creation failed: %v", err),
			})
			return violations
		}

		ref := adapter.TurnRef{SessionID: "s-deny", TurnKey: "t-deny"}
		outcome, err := ad.Dispatch(ctx, ref, "test tool denial")
		if err != nil || outcome.Status != adapter.DispatchAccepted {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("dispatch failed or rejected: err=%v outcome=%+v", err, outcome),
			})
			return violations
		}

		stream, err := ad.Observe(ctx, ref)
		if err != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("observe failed: %v", err),
			})
			return violations
		}

		var sawDenial bool
		timeout := time.After(300 * time.Millisecond)
		for {
			select {
			case ev, ok := <-stream.Events():
				if !ok {
					goto streamDone
				}
				if ev.Type == adapter.EventToolDenied {
					sawDenial = true
					if ev.Status != council.TurnRunning {
						violations = append(violations, Violation{
							Code:        ViolationPrematureDenialRetirement,
							Description: fmt.Sprintf("EventToolDenied carried non-running status: %s", ev.Status),
						})
					}
					goto streamDone
				}
			case <-timeout:
				goto streamDone
			}
		}

	streamDone:
		_ = stream.Close()
		if !sawDenial {
			violations = append(violations, Violation{
				Code:        ViolationCapabilityFabricated,
				Description: "adapter advertised ToolApprovalRouting capability but did not emit EventToolDenied",
			})
		}

	case ScenarioSessionResumption:
		binding := adapter.SessionBinding{
			SessionID:       "s-resumption",
			Contributor:     council.Claude,
			NativeSessionID: "native-resumption-1",
			Config:          adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		if probeReport.Capabilities.SessionResumption == adapter.CapabilityUnsupported {
			err := ad.ResumeSession(ctx, binding)
			if err == nil {
				violations = append(violations, Violation{
					Code:        ViolationCapabilityFabricated,
					Description: "adapter advertised SessionResumption as unsupported but ResumeSession succeeded",
				})
			} else if !errors.Is(err, adapter.ErrUnsupportedCapability) {
				violations = append(violations, Violation{
					Code:        ViolationUnexpectedError,
					Description: fmt.Sprintf("unsupported SessionResumption returned unexpected error: %v", err),
				})
			}
			return violations
		}

		req := adapter.CreateSessionRequest{
			SessionID:   "s-resumption",
			Contributor: council.Claude,
			Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		b, err := ad.CreateSession(ctx, req)
		if err != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("session creation for resumption test failed: %v", err),
			})
			return violations
		}
		if err := ad.ResumeSession(ctx, b); err != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("supported ResumeSession failed on valid binding: %v", err),
			})
		}

	case ScenarioMidTurnCancellation:
		ref := adapter.TurnRef{SessionID: "s-cancel", TurnKey: "t-cancel"}
		if probeReport.Capabilities.MidTurnCancellation == adapter.CapabilityUnsupported {
			outcome, err := ad.Cancel(ctx, ref)
			if err != nil {
				violations = append(violations, Violation{
					Code:        ViolationUnexpectedError,
					Description: fmt.Sprintf("cancel returned unexpected error: %v", err),
				})
				return violations
			}
			if outcome.Disposition != adapter.CancelUnsupported {
				violations = append(violations, Violation{
					Code:        ViolationCapabilityFabricated,
					Description: fmt.Sprintf("adapter advertised MidTurnCancellation as unsupported but Cancel returned %s", outcome.Disposition),
				})
			}
			return violations
		}

		req := adapter.CreateSessionRequest{
			SessionID:   "s-cancel",
			Contributor: council.Claude,
			Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		if _, err := ad.CreateSession(ctx, req); err != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("session creation for cancel test failed: %v", err),
			})
			return violations
		}
		dispOutcome, err := ad.Dispatch(ctx, ref, "test cancel prompt")
		if err != nil || dispOutcome.Status != adapter.DispatchAccepted {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("dispatch failed for cancel test: err=%v outcome=%+v", err, dispOutcome),
			})
			return violations
		}
		outcome, err := ad.Cancel(ctx, ref)
		if err != nil {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("cancel failed: %v", err),
			})
			return violations
		}
		if outcome.Disposition != adapter.CancelConfirmed && outcome.Disposition != adapter.CancelAlreadyTerminal {
			violations = append(violations, Violation{
				Code:        ViolationUnexpectedError,
				Description: fmt.Sprintf("unexpected cancel disposition: %s", outcome.Disposition),
			})
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

	t.Run("SessionResumption", func(t *testing.T) {
		f := factory(t)
		defer func() { _ = f.Cleanup() }()
		violations := Check(context.Background(), f, ScenarioSessionResumption)
		if len(violations) > 0 {
			t.Fatalf("unexpected violations: %+v", violations)
		}
	})

	t.Run("MidTurnCancellation", func(t *testing.T) {
		f := factory(t)
		defer func() { _ = f.Cleanup() }()
		violations := Check(context.Background(), f, ScenarioMidTurnCancellation)
		if len(violations) > 0 {
			t.Fatalf("unexpected violations: %+v", violations)
		}
	})
}
