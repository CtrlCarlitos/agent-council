package adapter_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/conformance"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

type test724Adapter struct {
	arrivalPath   string
	claimedStatus council.TurnStatus
	collectCount  int
}

func (a *test724Adapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	return adapter.ProbeReport{
		Capabilities: adapter.AdapterCapabilities{
			StreamingObservation: adapter.CapabilitySupported,
		},
	}, nil
}

func (a *test724Adapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	return adapter.SessionBinding{SessionID: req.SessionID}, nil
}

func (a *test724Adapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	return nil
}

func (a *test724Adapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
}

func (a *test724Adapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	stream := adapter.NewBufferedStream(ref, 10)
	if a.arrivalPath == "initial_replay" {
		_ = stream.Send(adapter.Event{
			Ref:    ref,
			Type:   adapter.EventTerminal,
			Status: a.claimedStatus,
		})
	} else {
		_ = stream.Send(adapter.Event{
			Ref:    ref,
			Type:   adapter.EventProgress,
			Status: council.TurnRunning,
		})
	}
	return stream, nil
}

func (a *test724Adapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	return adapter.CancelOutcome{Disposition: adapter.CancelConfirmed, Ref: ref}, nil
}

func (a *test724Adapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	a.collectCount++
	res := adapter.TurnResult{
		Ref:          ref,
		Status:       council.TurnRunning,
		ResultStatus: adapter.ResultAvailable,
	}
	if a.arrivalPath == "collect_before" && a.collectCount == 1 {
		res.Status = a.claimedStatus
	} else if a.arrivalPath == "collect_after" && a.collectCount >= 2 {
		res.Status = a.claimedStatus
	}
	return res, nil
}

func (a *test724Adapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	return adapter.ReconciliationOutcome{
		Ref:          ref,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableTerminal,
		Observed:     council.TurnCompleted,
	}, nil
}

type test724Fixture struct {
	ad             *test724Adapter
	active         bool
	expectedStatus council.TurnStatus
	allowComplete  bool
}

func (f *test724Fixture) Adapter() adapter.Adapter { return f.ad }
func (f *test724Fixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	return true, true, true
}
func (f *test724Fixture) IsCompletionAllowed(ref adapter.TurnRef) bool {
	return f.allowComplete
}
func (f *test724Fixture) IsExecutionActive(ref adapter.TurnRef) bool {
	return f.active
}
func (f *test724Fixture) TerminalOutcome(ref adapter.TurnRef) council.TurnStatus {
	return f.expectedStatus
}
func (f *test724Fixture) Cleanup() error { return nil }

var (
	test724ArrivalPaths = []string{"initial_replay", "collect_before", "collect_after"}
	test724Statuses     = []council.TurnStatus{
		council.TurnCompleted,
		council.TurnCancelled,
		council.TurnFailed,
		council.TurnInterrupted,
	}
)

func hasViolationCode(violations []conformance.Violation, code conformance.ViolationCode) bool {
	for _, v := range violations {
		if v.Code == code {
			return true
		}
	}
	return false
}

// TestReview724_GenuineOutcomePasses verifies 12 controls:
// When claimed terminal status matches independent fixture evidence,
// 0 violations are emitted across all 4 statuses and 3 arrival paths.
func TestReview724_GenuineOutcomePasses(t *testing.T) {
	for _, path := range test724ArrivalPaths {
		for _, st := range test724Statuses {
			t.Run(fmt.Sprintf("%s_%s", path, st), func(t *testing.T) {
				ad := &test724Adapter{arrivalPath: path, claimedStatus: st}
				allowComplete := (st == council.TurnCompleted)
				fix := &test724Fixture{
					ad:             ad,
					active:         false,
					expectedStatus: st,
					allowComplete:  allowComplete,
				}
				violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
				if len(violations) > 0 {
					t.Fatalf("expected 0 violations for genuine outcome match, got: %+v", violations)
				}
			})
		}
	}
}

// TestReview724_ActiveContradictionDetected verifies 12 controls:
// When execution remains active, any terminal claim is detected as ViolationEOFAsCompletion
// across all 4 statuses and 3 arrival paths.
func TestReview724_ActiveContradictionDetected(t *testing.T) {
	for _, path := range test724ArrivalPaths {
		for _, st := range test724Statuses {
			t.Run(fmt.Sprintf("%s_%s", path, st), func(t *testing.T) {
				ad := &test724Adapter{arrivalPath: path, claimedStatus: st}
				fix := &test724Fixture{
					ad:             ad,
					active:         true,
					expectedStatus: st,
					allowComplete:  true,
				}
				violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
				if !hasViolationCode(violations, conformance.ViolationEOFAsCompletion) {
					t.Fatalf("expected ViolationEOFAsCompletion when execution is active, got: %+v", violations)
				}
			})
		}
	}
}

// TestReview724_OutcomeMismatchDetected verifies 36 controls:
// When independent evidence records one terminal status but adapter claims another,
// a mismatch is detected as ViolationEOFAsCompletion across all 36 combinations.
func TestReview724_OutcomeMismatchDetected(t *testing.T) {
	for _, path := range test724ArrivalPaths {
		for _, claimed := range test724Statuses {
			for _, expected := range test724Statuses {
				if claimed == expected {
					continue
				}
				t.Run(fmt.Sprintf("%s_claimed_%s_expected_%s", path, claimed, expected), func(t *testing.T) {
					ad := &test724Adapter{arrivalPath: path, claimedStatus: claimed}
					fix := &test724Fixture{
						ad:             ad,
						active:         false,
						expectedStatus: expected,
						allowComplete:  true,
					}
					violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
					if !hasViolationCode(violations, conformance.ViolationEOFAsCompletion) {
						t.Fatalf("expected ViolationEOFAsCompletion on mismatch (claimed=%s expected=%s), got: %+v", claimed, expected, violations)
					}
				})
			}
		}
	}
}

// TestReview724_MissingEvidenceFails verifies the 24 missing-evidence matrix cases:
// Inactive execution with TerminalOutcome == "" must fail with ViolationPrerequisiteFailed
// (or ViolationEOFAsCompletion when TurnCompleted was claimed without permission).
func TestReview724_MissingEvidenceFails(t *testing.T) {
	for _, path := range test724ArrivalPaths {
		for _, claimed := range test724Statuses {
			for _, allowComplete := range []bool{false, true} {
				testName := fmt.Sprintf("%s_claimed_%s_allow_%v", path, claimed, allowComplete)
				t.Run(testName, func(t *testing.T) {
					ad := &test724Adapter{arrivalPath: path, claimedStatus: claimed}
					fix := &test724Fixture{
						ad:             ad,
						active:         false,
						expectedStatus: "",
						allowComplete:  allowComplete,
					}
					violations := conformance.Check(context.Background(), fix, conformance.ScenarioObservationClose)
					if len(violations) == 0 {
						t.Fatalf("claimed=%s arrival=%s active=false outcome=\"\" successAllowed=%v produced 0 violations; expected violation",
							claimed, path, allowComplete)
					}
					if claimed == council.TurnCompleted && !allowComplete {
						if !hasViolationCode(violations, conformance.ViolationEOFAsCompletion) {
							t.Fatalf("expected ViolationEOFAsCompletion when TurnCompleted claimed without permission, got: %+v", violations)
						}
					} else {
						if !hasViolationCode(violations, conformance.ViolationPrerequisiteFailed) {
							t.Fatalf("expected ViolationPrerequisiteFailed for missing terminal evidence, got: %+v", violations)
						}
					}
				})
			}
		}
	}
}
