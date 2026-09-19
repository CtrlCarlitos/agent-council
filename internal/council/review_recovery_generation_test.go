package council

import (
	"testing"
)

func TestReviewRecoveryGeneration_StaleEpisodeReplyCannotClearSubsequentOutage_Running(t *testing.T) {
	s, err := NewSession(Claude, "lease")
	if err != nil {
		t.Fatal(err)
	}

	_ = s.Queue("lease", "t1", "prompt 1")
	_ = s.Queue("lease", "t2", "prompt 2")
	if _, err := s.Release("lease", "t1"); err != nil {
		t.Fatalf("release t1 failed: %v", err)
	}

	// 1. Outage #1 occurs during t1
	gen1, err := s.RecordHostLoss()
	if err != nil {
		t.Fatalf("outage 1 RecordHostLoss failed: %v", err)
	}
	if gen1 == 0 {
		t.Fatal("expected non-zero generation for outage 1")
	}

	// Stale reply closure captures generation 1
	staleReply := func() error {
		return s.ReconcileHost("t1", gen1, TurnRunning, "")
	}

	// Outage #1 recovers
	if err := staleReply(); err != nil {
		t.Fatalf("outage 1 recovery failed: %v", err)
	}
	if s.Visibility != VisibilityReachable || s.Active != "t1" {
		t.Fatalf("expected reachable active t1: vis=%s active=%s", s.Visibility, s.Active)
	}

	// 2. Outage #2 occurs during the same t1
	gen2, err := s.RecordHostLoss()
	if err != nil {
		t.Fatalf("outage 2 RecordHostLoss failed: %v", err)
	}
	if gen2 <= gen1 {
		t.Fatalf("expected gen2 > gen1, got gen1=%d gen2=%d", gen1, gen2)
	}
	if s.Visibility != VisibilityHostLost {
		t.Fatalf("expected host lost for outage 2, got %s", s.Visibility)
	}

	// 3. Receive delayed duplicate of recovery reply from outage #1 (retaining gen1)
	err = staleReply()
	if err == nil {
		t.Fatalf("stale recovery reply from outage #1 was accepted during outage #2 (visibility=%s, recoveryContext=%s)", s.Visibility, s.RecoveryContext)
	}
	if s.Visibility != VisibilityHostLost {
		t.Fatalf("stale recovery reply cleared outage #2: visibility=%s", s.Visibility)
	}

	// 4. Delayed completion arrives for t1
	if err := s.CompleteWithResult("t1", "t1 done"); err != nil {
		t.Fatalf("CompleteWithResult failed: %v", err)
	}
	if s.Visibility != VisibilityHostLost {
		t.Fatalf("completion cleared host loss: visibility=%s", s.Visibility)
	}

	// 5. Release t2 must be blocked because outage #2 remains unresolved
	if _, err := s.Release("lease", "t2"); err == nil {
		t.Fatal("release of t2 permitted while outage #2 remains unresolved")
	}

	// 6. Positive control: genuine fresh recovery observation for outage #2 (using gen2)
	if err := s.ReconcileHost("t1", gen2, TurnCompleted, "t1 done"); err != nil {
		t.Fatalf("fresh reconciliation for outage 2 failed: %v", err)
	}
	if s.Visibility != VisibilityReachable {
		t.Fatalf("expected reachable after fresh reconciliation, got %s", s.Visibility)
	}

	// 7. Now t2 release succeeds
	if _, err := s.Release("lease", "t2"); err != nil {
		t.Fatalf("release of t2 failed after valid recovery: %v", err)
	}
	if s.Active != "t2" || s.State != Running {
		t.Fatalf("expected t2 active and running: active=%s state=%s", s.Active, s.State)
	}
}

func TestReviewRecoveryGeneration_StaleEpisodeReplyCannotClearSubsequentOutage_Cancelling(t *testing.T) {
	s, err := NewSession(Claude, "lease")
	if err != nil {
		t.Fatal(err)
	}

	_ = s.Queue("lease", "t1", "prompt 1")
	_ = s.Queue("lease", "t2", "prompt 2")
	if _, err := s.Release("lease", "t1"); err != nil {
		t.Fatalf("release t1 failed: %v", err)
	}
	if err := s.RequestCancel("lease"); err != nil {
		t.Fatalf("RequestCancel failed: %v", err)
	}

	// 1. Outage #1 occurs during t1 (cancelling)
	gen1, err := s.RecordHostLoss()
	if err != nil {
		t.Fatalf("outage 1 RecordHostLoss failed: %v", err)
	}
	staleReply := func() error {
		return s.ReconcileHost("t1", gen1, TurnCancelling, "")
	}

	// Outage #1 recovers
	if err := staleReply(); err != nil {
		t.Fatalf("outage 1 recovery failed: %v", err)
	}
	if s.Visibility != VisibilityReachable || s.Active != "t1" || s.ActiveTurn.Status != TurnCancelling {
		t.Fatalf("expected reachable active cancelling t1: vis=%s active=%s status=%s", s.Visibility, s.Active, s.ActiveTurn.Status)
	}

	// 2. Outage #2 occurs during the same t1
	gen2, err := s.RecordHostLoss()
	if err != nil {
		t.Fatalf("outage 2 RecordHostLoss failed: %v", err)
	}
	if gen2 <= gen1 {
		t.Fatalf("expected gen2 > gen1, got gen1=%d gen2=%d", gen1, gen2)
	}
	if s.Visibility != VisibilityHostLost {
		t.Fatalf("expected host lost for outage 2, got %s", s.Visibility)
	}

	// 3. Receive delayed duplicate of recovery reply from outage #1
	err = staleReply()
	if err == nil {
		t.Fatalf("stale recovery reply from outage #1 was accepted during cancelling outage #2 (visibility=%s)", s.Visibility)
	}
	if s.Visibility != VisibilityHostLost {
		t.Fatalf("stale recovery reply cleared outage #2: visibility=%s", s.Visibility)
	}

	// 4. ConfirmCancel arrives for t1
	if err := s.ConfirmCancel("t1"); err != nil {
		t.Fatalf("ConfirmCancel failed: %v", err)
	}
	if s.Visibility != VisibilityHostLost {
		t.Fatalf("ConfirmCancel cleared host loss: visibility=%s", s.Visibility)
	}

	// 5. Release t2 must be blocked
	if _, err := s.Release("lease", "t2"); err == nil {
		t.Fatal("release of t2 permitted while outage #2 remains unresolved")
	}

	// 6. Positive control: genuine fresh recovery observation for outage #2
	if err := s.ReconcileHost("t1", gen2, TurnCancelled, ""); err != nil {
		t.Fatalf("fresh reconciliation for outage 2 failed: %v", err)
	}
	if s.Visibility != VisibilityReachable {
		t.Fatalf("expected reachable after fresh reconciliation, got %s", s.Visibility)
	}

	// 7. Now t2 release succeeds
	if _, err := s.Release("lease", "t2"); err != nil {
		t.Fatalf("release of t2 failed after valid recovery: %v", err)
	}
	if s.Active != "t2" || s.State != Running {
		t.Fatalf("expected t2 active and running: active=%s state=%s", s.Active, s.State)
	}
}

func TestReviewRecoveryGeneration_DuplicateHostLossNotificationPreservesGeneration(t *testing.T) {
	s, err := NewSession(Agy, "lease")
	if err != nil {
		t.Fatal(err)
	}

	_ = s.Queue("lease", "t1", "p1")
	_, _ = s.Release("lease", "t1")

	gen1, err := s.RecordHostLoss()
	if err != nil {
		t.Fatalf("RecordHostLoss failed: %v", err)
	}

	// Duplicate notification while still open
	gen2, err := s.RecordHostLoss()
	if err != nil {
		t.Fatalf("duplicate RecordHostLoss failed: %v", err)
	}
	if gen2 != gen1 {
		t.Fatalf("duplicate host loss notification advanced generation: gen1=%d gen2=%d", gen1, gen2)
	}

	// Complete t1 while host loss is still active
	if err := s.CompleteWithResult("t1", "done"); err != nil {
		t.Fatalf("CompleteWithResult failed: %v", err)
	}

	// Duplicate notification after terminal delivery while host loss remains open
	gen3, err := s.RecordHostLoss()
	if err != nil {
		t.Fatalf("RecordHostLoss after terminal delivery failed: %v", err)
	}
	if gen3 != gen1 {
		t.Fatalf("duplicate host loss notification after terminal delivery advanced generation: gen1=%d gen3=%d", gen1, gen3)
	}

	// Reconcile with gen1 succeeds
	if err := s.ReconcileHost("t1", gen1, TurnCompleted, "done"); err != nil {
		t.Fatalf("reconciliation failed: %v", err)
	}
	if s.Visibility != VisibilityReachable {
		t.Fatalf("expected reachable, got %s", s.Visibility)
	}
}

func TestReviewRecoveryGeneration_ResolvedEpisodeCannotBeReused(t *testing.T) {
	s, err := NewSession(Codex, "lease")
	if err != nil {
		t.Fatal(err)
	}

	_ = s.Queue("lease", "t1", "p1")
	_, _ = s.Release("lease", "t1")

	gen1, err := s.RecordHostLoss()
	if err != nil {
		t.Fatal(err)
	}

	// Resolve gen1
	if err := s.ReconcileHost("t1", gen1, TurnRunning, ""); err != nil {
		t.Fatal(err)
	}

	// Attempting to reuse gen1 while reachable fails
	if err := s.ReconcileHost("t1", gen1, TurnRunning, ""); err == nil {
		t.Fatal("reconciliation accepted while host is reachable")
	}

	// Outage 2 occurs
	gen2, err := s.RecordHostLoss()
	if err != nil {
		t.Fatal(err)
	}
	if gen2 <= gen1 {
		t.Fatalf("expected gen2 > gen1, got gen1=%d gen2=%d", gen1, gen2)
	}

	// Attempting to use gen1 during outage 2 fails
	if err := s.ReconcileHost("t1", gen1, TurnRunning, ""); err == nil {
		t.Fatal("stale generation 1 accepted during outage 2")
	}

	// Positive control: gen2 succeeds
	if err := s.ReconcileHost("t1", gen2, TurnRunning, ""); err != nil {
		t.Fatalf("gen2 reconciliation failed: %v", err)
	}
}

func TestReviewRecoveryGeneration_StatePreservationOnRejectedGeneration(t *testing.T) {
	s, err := NewSession(OpenCode, "lease")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Queue("lease", "t1", "p1")
	_, _ = s.Release("lease", "t1")
	gen, err := s.RecordHostLoss()
	if err != nil {
		t.Fatal(err)
	}

	snap := snapshotSession(s)

	// Attempt reconcile with wrong generation
	err = s.ReconcileHost("t1", gen+10, TurnRunning, "")
	if err == nil {
		t.Fatal("expected error with invalid generation")
	}

	assertSnapshotEqual(t, "reconcile invalid gen", snap, snapshotSession(s))

	// Attempt reconcile with zero generation
	err = s.ReconcileHost("t1", 0, TurnRunning, "")
	if err == nil {
		t.Fatal("expected error with zero generation")
	}

	assertSnapshotEqual(t, "reconcile zero gen", snap, snapshotSession(s))
}
