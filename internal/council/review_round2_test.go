package council

import (
	"testing"
)

func TestReviewR2_StaleTurnCannotReconcileTerminalHostLoss(t *testing.T) {
	terminalOps := []struct {
		name string
		op   func(s *Session, key string) error
	}{
		{
			name: "CompleteWithResult",
			op: func(s *Session, key string) error {
				return s.CompleteWithResult(key, "completed output")
			},
		},
		{
			name: "Fail",
			op: func(s *Session, key string) error {
				return s.Fail(key, "failed reason")
			},
		},
		{
			name: "Interrupt",
			op: func(s *Session, key string) error {
				return s.Interrupt(key)
			},
		},
		{
			name: "ConfirmCancel",
			op: func(s *Session, key string) error {
				_ = s.RequestCancel("lease")
				return s.ConfirmCancel(key)
			},
		},
	}

	for _, tc := range terminalOps {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewSession(Claude, "lease")
			if err != nil {
				t.Fatal(err)
			}

			// 1. Run and finish t-old
			_ = s.Queue("lease", "t-old", "old task")
			_, _ = s.Release("lease", "t-old")
			if err := s.CompleteWithResult("t-old", "old output"); err != nil {
				t.Fatalf("finish t-old failed: %v", err)
			}

			// 2. Start t-loss with t-next queued
			_ = s.Queue("lease", "t-loss", "loss task")
			_ = s.Queue("lease", "t-next", "next task")
			_, _ = s.Release("lease", "t-loss")

			// 3. Lose contact with host
			gen, err := s.RecordHostLoss()
			if err != nil {
				t.Fatalf("record host loss failed: %v", err)
			}

			// 4. Terminal delivery arrives for t-loss
			if err := tc.op(s, "t-loss"); err != nil {
				t.Fatalf("terminal delivery for t-loss failed: %v", err)
			}

			if s.ActiveTurn != nil || s.Active != "" {
				t.Fatalf("expected no active turn, got active=%s activeTurn=%+v", s.Active, s.ActiveTurn)
			}
			if s.Visibility != VisibilityHostLost {
				t.Fatalf("expected VisibilityHostLost, got %s", s.Visibility)
			}

			// 5. Deliver a stale reconciliation reply for t-old
			err = s.ReconcileHost("t-old", gen, TurnCompleted, "t-old-stale")
			if err == nil {
				t.Fatal("stale reconciliation for t-old was accepted after t-loss terminal delivery")
			}

			// Visibility must remain host_lost
			if s.Visibility != VisibilityHostLost {
				t.Fatalf("stale reconciliation cleared host uncertainty: visibility=%s", s.Visibility)
			}

			// Releasing t-next must remain blocked
			if _, err := s.Release("lease", "t-next"); err == nil {
				t.Fatal("release of t-next permitted while host uncertainty remains")
			}
			if s.Active != "" || s.State != Parked {
				t.Fatalf("session state mutated: active=%s state=%s", s.Active, s.State)
			}

			// Positive control: valid reconciliation for t-loss succeeds and restores reachability
			if err := s.ReconcileHost("t-loss", gen, TurnCompleted, "reconcile valid"); err != nil {
				t.Fatalf("valid reconciliation for t-loss failed: %v", err)
			}
			if s.Visibility != VisibilityReachable {
				t.Fatalf("expected VisibilityReachable after valid reconciliation, got %s", s.Visibility)
			}

			// Now t-next can be released
			if _, err := s.Release("lease", "t-next"); err != nil {
				t.Fatalf("release of t-next failed after valid reconciliation: %v", err)
			}
			if s.Active != "t-next" || s.State != Running {
				t.Fatalf("expected t-next active and running: active=%s state=%s", s.Active, s.State)
			}
		})
	}
}

func TestReviewR2_MalformedOrContradictoryReconciliationRejectedWhenNoActiveTurn(t *testing.T) {
	cases := []struct {
		name       string
		key        string
		genOffset  uint64
		useZeroGen bool
		status     TurnStatus
		result     string
		wantError  string
	}{
		{
			name:      "Empty turn identity",
			key:       "",
			status:    TurnCompleted,
			result:    "ok",
			wantError: "empty turn identity",
		},
		{
			name:       "Zero generation",
			key:        "t-loss",
			useZeroGen: true,
			status:     TurnCompleted,
			result:     "ok",
			wantError:  "stale or invalid recovery generation",
		},
		{
			name:      "Stale generation",
			key:       "t-loss",
			genOffset: 99,
			status:    TurnCompleted,
			result:    "ok",
			wantError: "stale or invalid recovery generation",
		},
		{
			name:      "Empty status",
			key:       "t-loss",
			status:    TurnStatus(""),
			result:    "ok",
			wantError: "empty or unknown status",
		},
		{
			name:      "Unknown status",
			key:       "t-loss",
			status:    TurnStatus("bogus_status"),
			result:    "ok",
			wantError: "empty or unknown status",
		},
		{
			name:      "TurnPending status",
			key:       "t-loss",
			status:    TurnPending,
			result:    "pending",
			wantError: "pending status",
		},
		{
			name:      "TurnRunning for terminal turn",
			key:       "t-loss",
			status:    TurnRunning,
			result:    "",
			wantError: "conflicts with recorded outcome",
		},
		{
			name:      "TurnCancelling for terminal turn",
			key:       "t-loss",
			status:    TurnCancelling,
			result:    "",
			wantError: "conflicts with recorded outcome",
		},
		{
			name:      "Genuinely unknown turn ID",
			key:       "t-unknown",
			status:    TurnCompleted,
			result:    "ok",
			wantError: "unknown turn",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewSession(Agy, "lease")
			if err != nil {
				t.Fatal(err)
			}

			_ = s.Queue("lease", "t-loss", "loss task")
			_ = s.Queue("lease", "t-next", "next task")
			_, _ = s.Release("lease", "t-loss")
			gen, err := s.RecordHostLoss()
			if err != nil {
				t.Fatalf("RecordHostLoss failed: %v", err)
			}

			// Complete t-loss
			if err := s.CompleteWithResult("t-loss", "original-result"); err != nil {
				t.Fatalf("CompleteWithResult failed: %v", err)
			}

			origRecord := *s.Turns["t-loss"]

			targetGen := gen
			if tc.useZeroGen {
				targetGen = 0
			} else if tc.genOffset != 0 {
				targetGen = gen + tc.genOffset
			}

			// Attempt malformed or contradictory reconciliation
			err = s.ReconcileHost(tc.key, targetGen, tc.status, tc.result)
			if err == nil {
				t.Fatalf("%s: expected error, got nil (visibility=%s)", tc.name, s.Visibility)
			}

			// State must remain unchanged
			if s.Visibility != VisibilityHostLost {
				t.Fatalf("%s: visibility mutated to %s", tc.name, s.Visibility)
			}
			if s.Active != "" || s.ActiveTurn != nil || s.State != Parked {
				t.Fatalf("%s: session mutated: active=%s state=%s", tc.name, s.Active, s.State)
			}
			if *s.Turns["t-loss"] != origRecord {
				t.Fatalf("%s: recorded terminal turn mutated: before=%+v after=%+v", tc.name, origRecord, *s.Turns["t-loss"])
			}

			// Releasing t-next must still be blocked
			if _, err := s.Release("lease", "t-next"); err == nil {
				t.Fatalf("%s: release permitted after rejected reconciliation", tc.name)
			}
		})
	}
}
