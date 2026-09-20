package service

import (
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
)

// Regression evidence for PR #29 review round 3 (head 9dc6c5d).
//
// The coordinator's diagnostic epoch must version the complete diagnostic
// inputs, including live-set transitions. A diagnostic that captured the
// live-worker map while an unresolved worker was still tracked must not be
// applied after that worker retires: retirement changes the map that the
// diagnostic used to exclude the turn from unresolved counts.
func TestReview9DC_RetirementAfterBlockerCannotBeErased(t *testing.T) {
	c := NewCoordinator()

	ref := adapter.TurnRef{SessionID: "sess-9dc", TurnKey: "turn-9dc"}
	_, done, err := c.RegisterWorker(ref)
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}

	// The supervisor fails its terminal commit while the worker is still
	// tracked and installs a persistence obligation.
	c.AddRecoveryBlocker()

	// A diagnostic begins now: it captures the post-blocker epoch while the
	// worker is still live, so its live-worker map excludes the running turn
	// from unresolved counts (controlled diagnostic input: zero blockers).
	epoch := c.BlockerEpoch()
	liveMap := c.LiveWorkerKeys()
	if !liveMap["sess-9dc:turn-9dc"] {
		t.Fatalf("expected worker in live map, got %v", liveMap)
	}

	// The worker retires, changing the live set the diagnostic relied on.
	done()

	// The stale diagnostic finishes and attempts to apply its zero count.
	if c.ApplyDiagnosticBlockers(0, epoch) {
		t.Fatal("diagnostic captured before worker retirement must not apply after it")
	}
	if got := c.ShutdownCounters()["recovery_blockers"]; got != 1 {
		t.Fatalf("obligation erased by retirement-spanning diagnostic: blockers=%d", got)
	}
	if c.TryStopIdle() {
		t.Fatal("idle stop must not succeed while the obligation is outstanding")
	}

	// A diagnostic begun after retirement applies normally.
	fresh := c.BlockerEpoch()
	if !c.ApplyDiagnosticBlockers(1, fresh) {
		t.Fatal("fresh diagnostic refresh must apply")
	}
	if got := c.ShutdownCounters()["recovery_blockers"]; got != 1 {
		t.Fatalf("expected reconciled blocker count 1, got %d", got)
	}
}

// Worker registration also changes the live set: a diagnostic captured
// before registration must not apply after it.
func TestReview9DC_RegistrationAdvancesDiagnosticEpoch(t *testing.T) {
	c := NewCoordinator()
	epoch := c.BlockerEpoch()
	if _, done, err := c.RegisterWorker(adapter.TurnRef{SessionID: "sess-r", TurnKey: "turn-r"}); err != nil {
		t.Fatalf("register worker: %v", err)
	} else {
		defer done()
	}
	if c.ApplyDiagnosticBlockers(0, epoch) {
		t.Fatal("diagnostic captured before worker registration must not apply after it")
	}
}

// The production release-admission path must participate in the task
// WaitGroup: WaitTasks may not return while an accepted handoff remains.
func TestReview9DC_AdmitReleaseJoinsTaskWait(t *testing.T) {
	c := NewCoordinator()

	release, err := c.AdmitRelease()
	if err != nil {
		t.Fatalf("admit release: %v", err)
	}

	joined := make(chan struct{})
	go func() {
		c.WaitTasks()
		close(joined)
	}()

	select {
	case <-joined:
		t.Fatal("WaitTasks returned while an accepted release handoff remains outstanding")
	case <-time.After(50 * time.Millisecond):
	}

	if got := c.ShutdownCounters()["pending_handoffs"]; got != 1 {
		t.Fatalf("expected pending_handoffs=1, got %d", got)
	}

	release()

	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitTasks did not return after the handoff completed")
	}
}
