package adapter_test

import (
	"errors"
	"math"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func TestReconciliationOutcome_Validation(t *testing.T) {
	ref := adapter.RecoveryRef{
		TurnRef: adapter.TurnRef{
			SessionID: "sess-1",
			TurnKey:   "turn-1",
		},
		Generation: 1,
	}

	validCases := []adapter.ReconciliationOutcome{
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableActive,
			Observed:     council.TurnRunning,
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableActive,
			Observed:     council.TurnCancelling,
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableTerminal,
			Observed:     council.TurnCompleted,
			Result:       "ok",
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableTerminal,
			Observed:     council.TurnCancelled,
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableTerminal,
			Observed:     council.TurnFailed,
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableTerminal,
			Observed:     council.TurnInterrupted,
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationDefinitivelyMissing,
			Observed:     council.TurnFailed,
			Result:       "definitively missing",
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityHostLost,
			Status:       adapter.ReconciliationUncertain,
		},
	}

	for i, tc := range validCases {
		if err := tc.Validate(); err != nil {
			t.Fatalf("case %d: expected valid outcome, got error: %v", i, err)
		}
	}

	invalidCases := []struct {
		name    string
		outcome adapter.ReconciliationOutcome
	}{
		{
			name: "Contradictory reachable active with completed status",
			outcome: adapter.ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityReachable,
				Status:       adapter.ReconciliationReachableActive,
				Observed:     council.TurnCompleted,
			},
		},
		{
			name: "Contradictory reachable active with host lost visibility",
			outcome: adapter.ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityHostLost,
				Status:       adapter.ReconciliationReachableActive,
				Observed:     council.TurnRunning,
			},
		},
		{
			name: "Contradictory reachable terminal with running status",
			outcome: adapter.ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityReachable,
				Status:       adapter.ReconciliationReachableTerminal,
				Observed:     council.TurnRunning,
			},
		},
		{
			name: "Contradictory reachable terminal with host lost visibility",
			outcome: adapter.ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityHostLost,
				Status:       adapter.ReconciliationReachableTerminal,
				Observed:     council.TurnCompleted,
			},
		},
		{
			name: "Definitively missing without turn failed",
			outcome: adapter.ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityReachable,
				Status:       adapter.ReconciliationDefinitivelyMissing,
				Observed:     council.TurnRunning,
			},
		},
		{
			name: "Definitively missing with host lost visibility",
			outcome: adapter.ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityHostLost,
				Status:       adapter.ReconciliationDefinitivelyMissing,
				Observed:     council.TurnFailed,
			},
		},
		{
			name: "Uncertain with reachable visibility",
			outcome: adapter.ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityReachable,
				Status:       adapter.ReconciliationUncertain,
			},
		},
		{
			name: "Unknown reconciliation status",
			outcome: adapter.ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityHostLost,
				Status:       adapter.ReconciliationStatus("something_bogus"),
			},
		},
		{
			name: "Missing generation",
			outcome: adapter.ReconciliationOutcome{
				Ref: adapter.RecoveryRef{
					TurnRef:    adapter.TurnRef{SessionID: "s", TurnKey: "t"},
					Generation: 0,
				},
				Reachability: council.VisibilityReachable,
				Status:       adapter.ReconciliationReachableActive,
				Observed:     council.TurnRunning,
			},
		},
		{
			name: "Empty turn key",
			outcome: adapter.ReconciliationOutcome{
				Ref: adapter.RecoveryRef{
					TurnRef:    adapter.TurnRef{SessionID: "s", TurnKey: ""},
					Generation: 1,
				},
				Reachability: council.VisibilityReachable,
				Status:       adapter.ReconciliationReachableActive,
				Observed:     council.TurnRunning,
			},
		},
		{
			name: "Empty session ID",
			outcome: adapter.ReconciliationOutcome{
				Ref: adapter.RecoveryRef{
					TurnRef:    adapter.TurnRef{SessionID: "   ", TurnKey: "t"},
					Generation: 1,
				},
				Reachability: council.VisibilityReachable,
				Status:       adapter.ReconciliationReachableActive,
				Observed:     council.TurnRunning,
			},
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.outcome.Validate(); err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
		})
	}
}

func TestUsageMetric_Validation(t *testing.T) {
	t.Run("Negative input tokens", func(t *testing.T) {
		u := adapter.ExecutionUsage{
			InputTokens:  adapter.UsageMetric[int64]{Value: -1, Available: true},
			OutputTokens: adapter.UsageMetric[int64]{Value: 10, Available: true},
		}
		if err := u.Validate(); err == nil {
			t.Fatal("expected error for negative input tokens")
		}
	})

	t.Run("Negative output tokens", func(t *testing.T) {
		u := adapter.ExecutionUsage{
			InputTokens:  adapter.UsageMetric[int64]{Value: 10, Available: true},
			OutputTokens: adapter.UsageMetric[int64]{Value: -1, Available: true},
		}
		if err := u.Validate(); err == nil {
			t.Fatal("expected error for negative output tokens")
		}
	})

	t.Run("Negative total cost", func(t *testing.T) {
		u := adapter.ExecutionUsage{
			TotalCostUSD: adapter.UsageMetric[float64]{Value: -0.01, Available: true},
		}
		if err := u.Validate(); err == nil {
			t.Fatal("expected error for negative total cost")
		}
	})

	t.Run("NaN total cost", func(t *testing.T) {
		u := adapter.ExecutionUsage{
			TotalCostUSD: adapter.UsageMetric[float64]{Value: math.NaN(), Available: true},
		}
		if err := u.Validate(); err == nil {
			t.Fatal("expected error for NaN total cost")
		}
	})

	t.Run("Inf total cost", func(t *testing.T) {
		u := adapter.ExecutionUsage{
			TotalCostUSD: adapter.UsageMetric[float64]{Value: math.Inf(1), Available: true},
		}
		if err := u.Validate(); err == nil {
			t.Fatal("expected error for Inf total cost")
		}
	})

	t.Run("Valid metrics and unavailable zero/negative values", func(t *testing.T) {
		// When Available is false, raw values must not trigger validation errors
		valid := adapter.ExecutionUsage{
			InputTokens:  adapter.UsageMetric[int64]{Value: 100, Available: true, Estimated: false},
			OutputTokens: adapter.UsageMetric[int64]{Value: 50, Available: true, Estimated: true},
			TotalCostUSD: adapter.UsageMetric[float64]{Value: 0.05, Available: true},
		}
		if err := valid.Validate(); err != nil {
			t.Fatalf("expected valid usage, got %v", err)
		}

		unavail := adapter.ExecutionUsage{
			InputTokens:  adapter.UsageMetric[int64]{Value: -1, Available: false},
			OutputTokens: adapter.UsageMetric[int64]{Value: -5, Available: false},
			TotalCostUSD: adapter.UsageMetric[float64]{Value: -100.0, Available: false},
		}
		if err := unavail.Validate(); err != nil {
			t.Fatalf("expected unavailable metrics to pass validation, got %v", err)
		}
	})
}

func TestTurnRef_And_RecoveryRef_Validation(t *testing.T) {
	goodTurn := adapter.TurnRef{SessionID: "sess-123", TurnKey: "turn-456"}
	if err := goodTurn.Validate(); err != nil {
		t.Fatalf("expected good turn ref to be valid, got: %v", err)
	}

	badTurns := []adapter.TurnRef{
		{SessionID: "", TurnKey: "turn-1"},
		{SessionID: "   ", TurnKey: "turn-1"},
		{SessionID: "sess-1", TurnKey: ""},
		{SessionID: "sess-1", TurnKey: "   "},
	}
	for _, bt := range badTurns {
		if err := bt.Validate(); err == nil {
			t.Fatalf("expected invalid turn ref %+v to fail validation", bt)
		}
	}

	goodRec := adapter.RecoveryRef{TurnRef: goodTurn, Generation: 1}
	if err := goodRec.Validate(); err != nil {
		t.Fatalf("expected good recovery ref to be valid, got: %v", err)
	}

	badRecs := []adapter.RecoveryRef{
		{TurnRef: goodTurn, Generation: 0},
		{TurnRef: adapter.TurnRef{SessionID: "", TurnKey: "turn-1"}, Generation: 1},
	}
	for _, br := range badRecs {
		if err := br.Validate(); err == nil {
			t.Fatalf("expected invalid recovery ref %+v to fail validation", br)
		}
	}
}

func TestAdapterCapabilities_Normalize(t *testing.T) {
	caps := adapter.AdapterCapabilities{
		SessionResumption:    adapter.CapabilitySupported,
		MidTurnCancellation:  adapter.CapabilityUnsupported,
		ToolApprovalRouting:  adapter.CapabilityStatus("random_unknown"),
		StreamingObservation: adapter.CapabilityUnknown,
		StructuredOutput:     adapter.CapabilityStatus(""),
	}

	norm := caps.Normalize()
	if norm.SessionResumption != adapter.CapabilitySupported {
		t.Errorf("expected SessionResumption=supported, got %s", norm.SessionResumption)
	}
	if norm.MidTurnCancellation != adapter.CapabilityUnsupported {
		t.Errorf("expected MidTurnCancellation=unsupported, got %s", norm.MidTurnCancellation)
	}
	if norm.ToolApprovalRouting != adapter.CapabilityUnknown {
		t.Errorf("expected ToolApprovalRouting=unknown, got %s", norm.ToolApprovalRouting)
	}
	if norm.StreamingObservation != adapter.CapabilityUnknown {
		t.Errorf("expected StreamingObservation=unknown, got %s", norm.StreamingObservation)
	}
	if norm.StructuredOutput != adapter.CapabilityUnknown {
		t.Errorf("expected StructuredOutput=unknown, got %s", norm.StructuredOutput)
	}
}

func TestErrSessionCreationUncertain(t *testing.T) {
	inner := errors.New("connection reset by peer")
	err := &adapter.ErrSessionCreationUncertain{
		SessionID:       "sess-abc",
		Contributor:     council.Claude,
		PartialNativeID: "claude-subproc-999",
		Err:             inner,
	}

	if !errors.Is(err, inner) {
		t.Fatal("expected errors.Is to match wrapped error")
	}
	if err.Error() == "" {
		t.Fatal("expected non-empty error string")
	}
}

func TestCreateSessionRequest_Validation(t *testing.T) {
	req := adapter.CreateSessionRequest{
		SessionID:   "sess-123",
		Contributor: council.Claude,
		Config: adapter.SessionConfig{
			WorkspaceRoot: "/tmp/ws",
			Model:         "claude-3-5-sonnet",
		},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("expected valid session request, got: %v", err)
	}

	badReqs := []adapter.CreateSessionRequest{
		{SessionID: "", Contributor: council.Claude},
		{SessionID: "   ", Contributor: council.Claude},
		{SessionID: "sess-1", Contributor: "unsupported_contrib"},
	}
	for _, br := range badReqs {
		if err := br.Validate(); err == nil {
			t.Fatalf("expected invalid session request %+v to fail", br)
		}
	}
}
