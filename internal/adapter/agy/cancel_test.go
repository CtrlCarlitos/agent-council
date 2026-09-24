//go:build unix

package agy

// Cancellation evidence (AC-010 spec §3.6): Cancel sends SIGINT
// (ManagedProcess.Interrupt) ⇒ CancelRequested; CancelConfirmed ONLY on
// the verified result{ERROR,"interrupted"}; grace expiry ⇒ SIGTERM →
// kill and the attempt stays Uncertain; terminal/absent turns report
// AlreadyTerminal/Unknown. The Council turn bound is enforced by the
// same SIGINT path, strictly inside the --print-timeout backstop.

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestAgyCancel_SIGINTConfirmedOnInterruptedResult(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"interrupt_on_sigint": true}`)
	if out, err := h.dispatch("t1", "long"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	s, err := h.adapter.Observe(context.Background(), h.ref("t1"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	co, err := h.adapter.Cancel(context.Background(), h.ref("t1"))
	if err != nil || co.Disposition != adapter.CancelConfirmed {
		t.Fatalf("SIGINT ⇒ interrupted result ⇒ CancelConfirmed, got %+v err=%v", co, err)
	}
	att := h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "cancelled" {
		t.Fatalf("interrupted ⇒ cancelled terminal, got %q", att.ObservedStatus)
	}
	var sawCancelling, sawTerminal bool
	for _, ev := range collectEvents(t, s, 5*time.Second) {
		if ev.Status == council.TurnCancelling {
			sawCancelling = true
		}
		if ev.Type == adapter.EventTerminal && ev.Status == council.TurnCancelled {
			sawTerminal = true
		}
	}
	if !sawCancelling || !sawTerminal {
		t.Fatalf("stream must show cancelling then the cancelled terminal (cancelling=%v terminal=%v)", sawCancelling, sawTerminal)
	}
	res, err := h.adapter.Collect(context.Background(), h.ref("t1"))
	if err != nil || res.Status != council.TurnCancelled {
		t.Fatalf("collect: %+v err=%v", res, err)
	}
	co, _ = h.adapter.Cancel(context.Background(), h.ref("t1"))
	if co.Disposition != adapter.CancelAlreadyTerminal {
		t.Fatalf("cancel on a terminal turn: %+v", co)
	}
}

// swallowStdin accepts the prompt bytes (the boundary IS crossed) but
// never delivers them or closes the pipe: the child blocks reading stdin
// and — having registered SIGINT — ignores the interrupt.
type swallowStdin struct{ real io.WriteCloser }

func (s *swallowStdin) Write(p []byte) (int, error) { return len(p), nil }
func (s *swallowStdin) Close() error                { return nil }

func TestAgyCancel_GraceExpiryTerminatesUncertain(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	withShortTimers(t, initTimeout, 300*time.Millisecond)
	h.turnScenario(userInputDone, successResult("never"))
	h.exec.setWrapProc(func(p execpolicy.ManagedProcess) execpolicy.ManagedProcess {
		return &wrappedProc{ManagedProcess: p, stdin: &swallowStdin{real: p.Stdin()}}
	})
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	co, err := h.adapter.Cancel(context.Background(), h.ref("t1"))
	if err != nil || co.Disposition != adapter.CancelUnknown {
		t.Fatalf("grace expiry without a verified terminal ⇒ CancelUnknown, got %+v err=%v", co, err)
	}
	h.waitIdle(h.ref("t1"))
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att.Terminal || att.ObservedStatus != "uncertain" {
		t.Fatalf("terminated without terminal ⇒ Uncertain: %+v", att)
	}
	if st := h.launchStates("att-t1"); len(st) != 1 || st[0] != "dead" {
		t.Fatalf("terminated child recorded dead: %v", st)
	}
}

func TestAgyCancel_AbsentTurnUnknown(t *testing.T) {
	h := newAgyHarness(t)
	co, err := h.adapter.Cancel(context.Background(), h.ref("never"))
	if err != nil || co.Disposition != adapter.CancelUnknown {
		t.Fatalf("absent turn ⇒ CancelUnknown, got %+v err=%v", co, err)
	}
}

func TestAgyCancel_TurnBoundSendsSIGINT(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	old := turnBoundFor
	turnBoundFor = func(AgyLaunchPolicy) time.Duration { return 150 * time.Millisecond }
	t.Cleanup(func() { turnBoundFor = old })
	h.turnScenario(`{"interrupt_on_sigint": true}`)
	if out, err := h.dispatch("t1", "long"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	att := h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "cancelled" {
		t.Fatalf("the Council turn bound interrupts via SIGINT: %+v", att)
	}
}

func TestAgyCancel_TurnBoundInsideBackstop(t *testing.T) {
	p := AgyLaunchPolicy{PrintTimeoutBackstop: 60 * time.Second}
	if b := turnBoundFor(p); b <= 0 || b >= p.PrintTimeoutBackstop {
		t.Fatalf("the Council bound must be strictly inside the backstop, got %v", b)
	}
}
