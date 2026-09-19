package adapter_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/conformance"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// 1. Adapter where Collect fails before and after observation close
type collectFailingAdapter struct {
	*adaptertest.FakeAdapter
}

func (c *collectFailingAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	return adapter.TurnResult{}, errors.New("collect connection failed")
}

type collectFailingFixture struct {
	ad adapter.Adapter
}

func (c *collectFailingFixture) Adapter() adapter.Adapter { return c.ad }
func (c *collectFailingFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	return true, true, true
}
func (c *collectFailingFixture) IsCompletionAllowed(ref adapter.TurnRef) bool { return false }
func (c *collectFailingFixture) Cleanup() error                               { return nil }

func TestReviewD910_ConformanceCheckerDetectsCollectFailure(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	ad := &collectFailingAdapter{FakeAdapter: fake}
	fix := &collectFailingFixture{ad: ad}

	violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
	if len(violations) == 0 {
		t.Fatal("expected violations when Collect fails, got 0 (false pass)")
	}

	var foundUnexpectedErr bool
	for _, v := range violations {
		if v.Code == conformance.ViolationUnexpectedError {
			foundUnexpectedErr = true
			break
		}
	}
	if !foundUnexpectedErr {
		t.Fatalf("expected ViolationUnexpectedError, got %+v", violations)
	}
}

// 2. Adapter where Reconcile returns contradictory outcome
type contradictoryReconcileAdapter struct {
	*adaptertest.FakeAdapter
}

func (c *contradictoryReconcileAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	return adapter.ReconciliationOutcome{
		Ref:          ref,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableActive,
		Observed:     council.TurnCompleted, // Contradictory: reachable active cannot be TurnCompleted!
		Result:       "done",
	}, nil
}

type contradictoryReconcileFixture struct {
	ad adapter.Adapter
}

func (c *contradictoryReconcileFixture) Adapter() adapter.Adapter { return c.ad }
func (c *contradictoryReconcileFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	return true, true, true
}
func (c *contradictoryReconcileFixture) IsCompletionAllowed(ref adapter.TurnRef) bool { return false }
func (c *contradictoryReconcileFixture) Cleanup() error                               { return nil }

func TestReviewD910_ConformanceCheckerDetectsContradictoryReconciliation(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	ad := &contradictoryReconcileAdapter{FakeAdapter: fake}
	fix := &contradictoryReconcileFixture{ad: ad}

	violations := conformance.Check(context.Background(), fix, conformance.ScenarioRecoveryIntegrity)
	if len(violations) == 0 {
		t.Fatal("expected violations for contradictory reconciliation matrix, got 0 (false pass)")
	}

	var foundUnexpectedErr bool
	for _, v := range violations {
		if v.Code == conformance.ViolationUnexpectedError {
			foundUnexpectedErr = true
			break
		}
	}
	if !foundUnexpectedErr {
		t.Fatalf("expected ViolationUnexpectedError, got %+v", violations)
	}
}

// 3. Fixture where worker legitimately completes
type legitimateCompletionAdapter struct {
	*adaptertest.FakeAdapter
	observedClose chan struct{}
}

func (l *legitimateCompletionAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	stream := adapter.NewBufferedStream(ref, 10)
	go func() {
		_ = stream.Send(adapter.Event{
			Ref:       ref,
			Type:      adapter.EventProgress,
			Status:    council.TurnRunning,
			Payload:   "progress",
			Timestamp: time.Now(),
		})
	}()
	return stream, nil
}

type legitimateCompletionFixture struct {
	ad      adapter.Adapter
	allowed bool
}

func (l *legitimateCompletionFixture) Adapter() adapter.Adapter { return l.ad }
func (l *legitimateCompletionFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	return true, true, true
}
func (l *legitimateCompletionFixture) IsCompletionAllowed(ref adapter.TurnRef) bool { return l.allowed }
func (l *legitimateCompletionFixture) Cleanup() error                               { return nil }

func TestReviewD910_ConformanceCheckerDoesNotFalselyFlagLegitimateCompletion(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{})
	ad := &legitimateCompletionAdapter{FakeAdapter: fake, observedClose: make(chan struct{})}
	fix := &legitimateCompletionFixture{ad: ad, allowed: true}

	violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
	for _, v := range violations {
		if v.Code == conformance.ViolationEOFAsCompletion {
			t.Fatalf("conformance checker falsely flagged legitimate completion as EOF_TREATED_AS_COMPLETION: %+v", v)
		}
	}
}

// 4. Testing unsupported capability contracts
type fabricatingUnsupportedAdapter struct {
	*adaptertest.FakeAdapter
}

func (f *fabricatingUnsupportedAdapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	return adapter.ProbeReport{
		HarnessVersion: adapter.UsageMetric[string]{Value: "1.0", Available: true},
		Capabilities: adapter.AdapterCapabilities{
			SessionResumption:   adapter.CapabilityUnsupported,
			MidTurnCancellation: adapter.CapabilityUnsupported,
		},
	}, nil
}

func (f *fabricatingUnsupportedAdapter) ResumeSession(ctx context.Context, b adapter.SessionBinding) error {
	// Illegally succeeds when advertised as unsupported!
	return nil
}

func (f *fabricatingUnsupportedAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	// Illegally returns CancelConfirmed when advertised as unsupported!
	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed}, nil
}

type unsupportedFixture struct {
	ad adapter.Adapter
}

func (u *unsupportedFixture) Adapter() adapter.Adapter { return u.ad }
func (u *unsupportedFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	return true, true, true
}
func (u *unsupportedFixture) IsCompletionAllowed(ref adapter.TurnRef) bool { return false }
func (u *unsupportedFixture) Cleanup() error                               { return nil }

func TestReviewD910_ConformanceCheckerTestsUnsupportedContract(t *testing.T) {
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		ProbeCapabilities: adapter.AdapterCapabilities{
			SessionResumption:   adapter.CapabilityUnsupported,
			MidTurnCancellation: adapter.CapabilityUnsupported,
		},
	})
	honestFix := &unsupportedFixture{ad: fake}

	// Honest unsupported adapter should produce zero violations
	vRes := conformance.Check(context.Background(), honestFix, conformance.ScenarioSessionResumption)
	if len(vRes) > 0 {
		t.Fatalf("honest unsupported SessionResumption produced violations: %+v", vRes)
	}

	vCancel := conformance.Check(context.Background(), honestFix, conformance.ScenarioMidTurnCancellation)
	if len(vCancel) > 0 {
		t.Fatalf("honest unsupported MidTurnCancellation produced violations: %+v", vCancel)
	}

	// Fabricating adapter should be flagged with ViolationCapabilityFabricated
	dishonestFix := &unsupportedFixture{ad: &fabricatingUnsupportedAdapter{FakeAdapter: fake}}

	vFabricatedRes := conformance.Check(context.Background(), dishonestFix, conformance.ScenarioSessionResumption)
	if len(vFabricatedRes) == 0 {
		t.Fatal("expected ViolationCapabilityFabricated for fabricated SessionResumption, got 0")
	}
	if vFabricatedRes[0].Code != conformance.ViolationCapabilityFabricated {
		t.Fatalf("expected ViolationCapabilityFabricated, got %s", vFabricatedRes[0].Code)
	}

	vFabricatedCancel := conformance.Check(context.Background(), dishonestFix, conformance.ScenarioMidTurnCancellation)
	if len(vFabricatedCancel) == 0 {
		t.Fatal("expected ViolationCapabilityFabricated for fabricated MidTurnCancellation, got 0")
	}
	if vFabricatedCancel[0].Code != conformance.ViolationCapabilityFabricated {
		t.Fatalf("expected ViolationCapabilityFabricated, got %s", vFabricatedCancel[0].Code)
	}
}
