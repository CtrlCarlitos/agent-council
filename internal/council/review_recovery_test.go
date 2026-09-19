package council

import "testing"

func TestReviewStaleReconciliationMustNotFinishAnotherTurn(t *testing.T) {
	s, err := NewSession(OpenCode, "lease")
	if err != nil {
		t.Fatal(err)
	}
	// 1. Start t1 -> lose host contact -> reconcile t1 as completed
	_ = s.Queue("lease", "t1", "task 1")
	_, _ = s.Release("lease", "t1")
	_ = s.RecordHostLoss()
	if err := s.ReconcileHost("t1", TurnCompleted, "t1-result-from-original-probe"); err != nil {
		t.Fatalf("reconcile t1 failed: %v", err)
	}
	if s.State != Parked || s.Active != "" {
		t.Fatalf("expected parked after t1 completed, got state=%s active=%s", s.State, s.Active)
	}

	// 2. Start t2 -> lose host contact
	_ = s.Queue("lease", "t2", "task 2")
	_, _ = s.Release("lease", "t2")
	_ = s.RecordHostLoss()

	// 3. Deliver a duplicate of the old t1 reconciliation reply
	err = s.ReconcileHost("t1", TurnCompleted, "t1-stale-duplicate")
	if err == nil {
		t.Fatal("stale t1 reconciliation was accepted while t2 is active")
	}

	// Verify t2 and its state remain completely unchanged
	if s.Active != "t2" || s.State != Running || s.Visibility != VisibilityHostLost {
		t.Fatalf("stale t1 reconciliation mutated t2: state=%s, active=%s, visibility=%s", s.State, s.Active, s.Visibility)
	}
	if s.Turns["t2"].Status != TurnRunning {
		t.Fatalf("t2 status mutated: %s", s.Turns["t2"].Status)
	}
	if s.Turns["t1"].Result != "t1-result-from-original-probe" {
		t.Fatalf("t1 result overwritten by stale reconciliation: %q", s.Turns["t1"].Result)
	}
}

func TestReviewTerminalDeliveryAfterHostLossRemainsRecoverable(t *testing.T) {
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
			_ = s.Queue("lease", "t1", "prompt 1")
			_ = s.Queue("lease", "t2", "prompt 2")
			_, _ = s.Release("lease", "t1")

			// Host loss occurs while t1 is active
			_ = s.RecordHostLoss()

			// Terminal delivery arrives
			err = tc.op(s, "t1")
			if err != nil {
				// Policy: terminal delivery is rejected while host visibility is uncertain.
				if s.Active != "t1" || s.State != Running {
					t.Fatalf("rejected terminal delivery mutated state: active=%s, state=%s", s.Active, s.State)
				}
				// ReconcileHost can resolve the turn and restore reachability
				if err := s.ReconcileHost("t1", TurnCompleted, "reconciled"); err != nil {
					t.Fatalf("ReconcileHost failed after rejected terminal delivery: %v", err)
				}
			} else {
				// Policy: terminal delivery was accepted.
				// If VisibilityHostLost remains, there MUST be a valid recovery path to restore reachability!
				if s.Visibility == VisibilityHostLost {
					if err := s.ReconcileHost("t1", TurnCompleted, "reconcile reachability"); err != nil {
						t.Fatalf("session left permanently stuck after %s during host loss: %v", tc.name, err)
					}
				}
			}

			// Now session must be recoverable: releasing t2 must succeed!
			if s.Visibility != VisibilityReachable {
				t.Fatalf("host visibility not reachable: %s", s.Visibility)
			}
			if s.State != Parked {
				t.Fatalf("contributor not parked: %s", s.State)
			}
			if _, err := s.Release("lease", "t2"); err != nil {
				t.Fatalf("unable to release t2: %v", err)
			}
			if s.Active != "t2" || s.State != Running {
				t.Fatalf("t2 not active: active=%s state=%s", s.Active, s.State)
			}
		})
	}
}

func TestReviewReconcileReachableStillActiveTurn(t *testing.T) {
	t.Run("running turn restored to reachable", func(t *testing.T) {
		s, err := NewSession(Codex, "lease")
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Queue("lease", "t1", "task 1")
		_ = s.Queue("lease", "t2", "task 2")
		_, _ = s.Release("lease", "t1")

		_ = s.RecordHostLoss()
		if s.Visibility != VisibilityHostLost {
			t.Fatal("expected VisibilityHostLost")
		}

		// Reconcile host observing that t1 is still running
		if err := s.ReconcileHost("t1", TurnRunning, ""); err != nil {
			t.Fatalf("reconciliation of running turn rejected: %v", err)
		}

		// Visibility is restored, but turn-1 remains running and occupied!
		if s.Visibility != VisibilityReachable {
			t.Fatalf("expected VisibilityReachable, got %s", s.Visibility)
		}
		if s.Active != "t1" || s.State != Running || s.ActiveTurn == nil || s.ActiveTurn.Status != TurnRunning {
			t.Fatalf("turn-1 was terminated or freed: active=%s, state=%s", s.Active, s.State)
		}
		if s.Pending["t2"] != "task 2" {
			t.Fatal("queued prompt lost")
		}

		// Attempting to release t2 while t1 is still running must fail
		if _, err := s.Release("lease", "t2"); err == nil {
			t.Fatal("release of t2 permitted while t1 is still running")
		}

		// Complete t1 normally
		if err := s.CompleteWithResult("t1", "output 1"); err != nil {
			t.Fatalf("complete failed: %v", err)
		}
		if s.State != Parked {
			t.Fatalf("expected parked, got %s", s.State)
		}

		// Now t2 can be released
		if _, err := s.Release("lease", "t2"); err != nil {
			t.Fatalf("release of t2 failed: %v", err)
		}
	})

	t.Run("cancelling turn restored to reachable with cancellation request preserved", func(t *testing.T) {
		s, err := NewSession(Agy, "lease")
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Queue("lease", "t1", "task 1")
		_ = s.Queue("lease", "t2", "task 2")
		_, _ = s.Release("lease", "t1")

		_ = s.RequestCancel("lease")
		if s.ActiveTurn.Status != TurnCancelling {
			t.Fatalf("expected TurnCancelling, got %s", s.ActiveTurn.Status)
		}

		_ = s.RecordHostLoss()

		// Reconcile host contact restored; t1 is still active
		if err := s.ReconcileHost("t1", TurnCancelling, ""); err != nil {
			t.Fatalf("reconciliation of cancelling turn rejected: %v", err)
		}

		// Visibility restored, cancellation request PRESERVED
		if s.Visibility != VisibilityReachable {
			t.Fatalf("expected VisibilityReachable, got %s", s.Visibility)
		}
		if s.Active != "t1" || s.ActiveTurn.Status != TurnCancelling || s.State != Running {
			t.Fatalf("cancelling turn state corrupted: active=%s, status=%s, state=%s",
				s.Active, s.ActiveTurn.Status, s.State)
		}

		// Release of t2 still blocked
		if _, err := s.Release("lease", "t2"); err == nil {
			t.Fatal("release of t2 permitted while cancellation unresolved")
		}

		// Confirm cancel
		if err := s.ConfirmCancel("t1"); err != nil {
			t.Fatalf("confirm cancel failed: %v", err)
		}

		// Now t2 can be released
		if _, err := s.Release("lease", "t2"); err != nil {
			t.Fatalf("release of t2 failed after cancel confirmed: %v", err)
		}
	})
}
