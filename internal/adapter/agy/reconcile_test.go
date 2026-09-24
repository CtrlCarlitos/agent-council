//go:build unix

package agy

// Reconciliation and crash-gap evidence (AC-010 spec §3.10/§3.11): every
// durable transition of the dispatch ladder is interrupted by a
// simulated crash (the adapter freezes at the phase through a test-only
// executor/stdin wrapper, the child is killed, the store is CLOSED and
// REOPENED, a fresh adapter is built) and the recorded state is
// observed. Only the in-life result is terminal evidence; lost attempts
// stay Uncertain and block the conversation across the restart.
//
//   §3.11 transition                         test
//   attempt+pdig durable (pre-reservation)   TestAgyCrashGap_AttemptBeforeReservation
//   launch reserved (launch_count 0→1)       TestAgyCrashGap_ReservedBeforeStart
//   started + exe identity                   TestAgyCrashGap_StartedBeforeFirstByte
//   first stdin byte                         TestAgyCrashGap_FirstByteBeforeUserInput
//   native step index (accepted)             TestAgyCrashGap_AcceptedBeforeTerminal
//   terminal + verification                  TestAgyCrashGap_TerminalSurvivesRestart
//   start_failed ⇒ missing                   TestAgyCrashGap_StartFailedMissing
//   dead + missing (pre-write rejection)     TestAgyCrashGap_PreWriteRejectionMissing

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func recoveryRef(h *agyHarness, turnKey string) adapter.RecoveryRef {
	return adapter.RecoveryRef{TurnRef: h.ref(turnKey), Generation: 1}
}

// freeze is a crash point: the adapter goroutine blocks at the phase
// until the test releases it (after the store is already reopened, so
// the frozen goroutine can only fail against the closed store).
type freeze struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newFreeze(t *testing.T) *freeze {
	f := &freeze{reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { f.unfreeze() })
	return f
}

func (f *freeze) hit() {
	select {
	case <-f.reached:
	default:
		close(f.reached)
	}
	<-f.release
}

func (f *freeze) unfreeze() { f.once.Do(func() { close(f.release) }) }

func (f *freeze) wait(t *testing.T) {
	t.Helper()
	select {
	case <-f.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("crash point never reached")
	}
}

// crashDispatch runs Dispatch in the background (it will freeze) and
// registers a cleanup that waits for it to return once released.
func crashDispatch(t *testing.T, h *agyHarness, f *freeze, turnKey string) {
	t.Helper()
	a := h.adapter
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Dispatch(context.Background(), h.ref(turnKey), "prompt "+turnKey)
	}()
	t.Cleanup(func() {
		f.unfreeze()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("frozen dispatch never returned after release")
		}
		a.waitAllIdle(10 * time.Second)
	})
}

func assertUncertainBlocked(t *testing.T, h *agyHarness, turnKey string) {
	t.Helper()
	out, err := h.adapter.Reconcile(context.Background(), recoveryRef(h, turnKey))
	if err != nil || out.Status != adapter.ReconciliationUncertain || out.Reachability != council.VisibilityHostLost {
		t.Fatalf("lost attempt stays Uncertain: %+v err=%v", out, err)
	}
	if err := out.Validate(); err != nil {
		t.Fatalf("outcome invalid: %v", err)
	}
	blocked, err := h.store.HasAgyUnresolvedAttempts(context.Background(), testNativeID)
	if err != nil || !blocked {
		t.Fatalf("the unresolved attempt blocks the conversation across restart: %v %v", blocked, err)
	}
	h.turnScenario(userInputDone, successResult("x"))
	next, _ := h.dispatch("t-next", "p")
	if next.Status != adapter.DispatchRejected {
		t.Fatalf("dispatch after restart must be blocked: %+v", next)
	}
}

func TestAgyCrashGap_AttemptBeforeReservation(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	if err := h.store.InsertAgyTurnAttempt(context.Background(), storage.AgyTurnAttempt{
		AttemptID: "att-t1", SessionID: testSessionID, TurnKey: "t1",
		PromptDigest: "pdig-v1:sha256:seeded", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	h.reopen()
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att == nil || att.LaunchCount != 0 || len(h.launchStates("att-t1")) != 0 {
		t.Fatalf("no launch was ever reserved: %+v", att)
	}
	// No reservation ⇒ no process could have started: positive pre-start
	// evidence ⇒ DefinitivelyMissing, and the conversation is released.
	out, err := h.adapter.Reconcile(context.Background(), recoveryRef(h, "t1"))
	if err != nil || out.Status != adapter.ReconciliationDefinitivelyMissing {
		t.Fatalf("unreserved attempt ⇒ DefinitivelyMissing: %+v err=%v", out, err)
	}
	if blocked, _ := h.store.HasAgyUnresolvedAttempts(context.Background(), testNativeID); blocked {
		t.Fatal("a never-reserved attempt must not keep blocking once reconciled")
	}
}

func TestAgyCrashGap_ReservedBeforeStart(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(userInputDone, successResult("x"))
	f := newFreeze(t)
	h.exec.setBeforeStart(func(execpolicy.LaunchRequest) error {
		f.hit()
		return errors.New("crashed")
	})
	crashDispatch(t, h, f, "t1")
	f.wait(t)
	h.exec.setBeforeStart(nil)
	h.reopen()

	if st := h.launchStates("att-t1"); len(st) != 1 || st[0] != "reserved" {
		t.Fatalf("reserved-not-started survives restart: %v", st)
	}
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att.LaunchCount != 1 {
		t.Fatalf("launch_count consumed: %+v", att)
	}
	assertUncertainBlocked(t, h, "t1")
}

// frozenStdin blocks at a chosen stdin phase.
type frozenStdin struct {
	real       io.WriteCloser
	f          *freeze
	onWrite    bool
	killOnHold func()
}

func (s *frozenStdin) Write(p []byte) (int, error) {
	if s.onWrite {
		s.killOnHold()
		s.f.hit()
		return 0, errors.New("crashed before the first byte")
	}
	return s.real.Write(p)
}

func (s *frozenStdin) Close() error {
	if !s.onWrite {
		s.killOnHold()
		s.f.hit()
		return errors.New("crashed after the write")
	}
	return s.real.Close()
}

func TestAgyCrashGap_StartedBeforeFirstByte(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(userInputDone, successResult("x"))
	f := newFreeze(t)
	h.exec.setWrapProc(func(p execpolicy.ManagedProcess) execpolicy.ManagedProcess {
		kill := func() { _ = p.Terminate(context.Background()) }
		return &wrappedProc{ManagedProcess: p, stdin: &frozenStdin{real: p.Stdin(), f: f, onWrite: true, killOnHold: kill}}
	})
	crashDispatch(t, h, f, "t1")
	f.wait(t)
	h.exec.setWrapProc(nil)
	h.reopen()

	if st := h.launchStates("att-t1"); len(st) != 1 || st[0] != "started" {
		t.Fatalf("started survives restart: %v", st)
	}
	var firstByte sql.NullString
	var exeDigest, exeDevIno sql.NullString
	if err := h.store.DB().QueryRow(`SELECT first_stdin_byte_at, exe_digest, exe_dev_ino FROM agy_attempt_launches WHERE attempt_id = ?`,
		"att-t1").Scan(&firstByte, &exeDigest, &exeDevIno); err != nil {
		t.Fatal(err)
	}
	if firstByte.Valid {
		t.Fatal("no stdin byte crossed the boundary")
	}
	if runtime.GOOS == "linux" && (exeDigest.String != h.fx.Digest || exeDevIno.String == "") {
		t.Fatalf("the sealed executable identity is recorded at start: digest=%q devino=%q", exeDigest.String, exeDevIno.String)
	}
	assertUncertainBlocked(t, h, "t1")
}

func TestAgyCrashGap_FirstByteBeforeUserInput(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(userInputDone, successResult("x"))
	f := newFreeze(t)
	h.exec.setWrapProc(func(p execpolicy.ManagedProcess) execpolicy.ManagedProcess {
		kill := func() { _ = p.Terminate(context.Background()) }
		return &wrappedProc{ManagedProcess: p, stdin: &frozenStdin{real: p.Stdin(), f: f, onWrite: false, killOnHold: kill}}
	})
	crashDispatch(t, h, f, "t1")
	f.wait(t)
	h.exec.setWrapProc(nil)
	h.reopen()

	var firstByte sql.NullString
	if err := h.store.DB().QueryRow(`SELECT first_stdin_byte_at FROM agy_attempt_launches WHERE attempt_id = ?`,
		"att-t1").Scan(&firstByte); err != nil {
		t.Fatal(err)
	}
	if !firstByte.Valid {
		t.Fatal("the first stdin byte boundary is durable")
	}
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att.Accepted != nil || att.Terminal {
		t.Fatalf("no acceptance evidence was consumed before the crash: %+v", att)
	}
	assertUncertainBlocked(t, h, "t1")
}

func TestAgyCrashGap_AcceptedBeforeTerminal(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	// The child dies after acknowledging the input, before any result.
	h.turnScenario(userInputDone, `{"exit_without_result": true}`)
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	h.waitIdle(h.ref("t1"))
	h.reopen()

	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att.Accepted == nil || !*att.Accepted || att.NativeStepIndex == nil || att.Terminal {
		t.Fatalf("accepted, not terminal, survives restart: %+v", att)
	}
	assertUncertainBlocked(t, h, "t1")
}

func TestAgyCrashGap_TerminalSurvivesRestart(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.required.set("t1", []string{"view_file"})
	h.turnScenario(userInputDone, toolStep("view_file"), successResult("final"))
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	h.waitIdle(h.ref("t1"))
	h.reopen()

	out, err := h.adapter.Reconcile(context.Background(), recoveryRef(h, "t1"))
	if err != nil || out.Status != adapter.ReconciliationReachableTerminal || out.Observed != council.TurnCompleted {
		t.Fatalf("the committed terminal stands after restart: %+v err=%v", out, err)
	}
	res, err := h.adapter.Collect(context.Background(), h.ref("t1"))
	if err != nil || res.Output != "final" {
		t.Fatalf("collect after restart: %+v err=%v", res, err)
	}
	v, ok, err := h.adapter.Verification(context.Background(), h.ref("t1"))
	if err != nil || !ok || v.Incomplete || len(v.Executed) != 1 {
		t.Fatalf("verification after restart: %+v ok=%v err=%v", v, ok, err)
	}
	if blocked, _ := h.store.HasAgyUnresolvedAttempts(context.Background(), testNativeID); blocked {
		t.Fatal("a completed attempt does not block")
	}
}

func TestAgyCrashGap_StartFailedMissing(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.exec.setBeforeStart(func(execpolicy.LaunchRequest) error { return errors.New("no process") })
	_, _ = h.dispatch("t1", "p")
	h.exec.setBeforeStart(nil)
	h.reopen()
	out, err := h.adapter.Reconcile(context.Background(), recoveryRef(h, "t1"))
	if err != nil || out.Status != adapter.ReconciliationDefinitivelyMissing {
		t.Fatalf("start_failed ⇒ DefinitivelyMissing: %+v err=%v", out, err)
	}
	h.turnScenario(userInputDone, successResult("ok"))
	if next, err := h.dispatch("t2", "p"); err != nil || next.Status != adapter.DispatchAccepted {
		t.Fatalf("a definitively missing attempt does not block: %+v err=%v", next, err)
	}
}

func TestAgyCrashGap_PreWriteRejectionMissing(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"permission_mode": "strict"}`)
	_, _ = h.dispatch("t1", "p")
	h.reopen()
	if st := h.launchStates("att-t1"); len(st) != 1 || st[0] != "dead" {
		t.Fatalf("pre-write rejection records the child dead: %v", st)
	}
	out, err := h.adapter.Reconcile(context.Background(), recoveryRef(h, "t1"))
	if err != nil || out.Status != adapter.ReconciliationDefinitivelyMissing {
		t.Fatalf("pre-write rejection ⇒ DefinitivelyMissing: %+v err=%v", out, err)
	}
}

func TestAgyReconcile_Matrix(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)

	// Unknown turn: no evidence ⇒ Uncertain.
	out, err := h.adapter.Reconcile(context.Background(), recoveryRef(h, "absent"))
	if err != nil || out.Status != adapter.ReconciliationUncertain {
		t.Fatalf("absent: %+v err=%v", out, err)
	}

	// Live turn ⇒ ReachableActive.
	h.turnScenario(`{"interrupt_on_sigint": true}`)
	if o, err := h.dispatch("t1", "p"); err != nil || o.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", o, err)
	}
	out, err = h.adapter.Reconcile(context.Background(), recoveryRef(h, "t1"))
	if err != nil || out.Status != adapter.ReconciliationReachableActive {
		t.Fatalf("live: %+v err=%v", out, err)
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
	// Cancelled terminal ⇒ ReachableTerminal/TurnCancelled.
	if co, _ := h.adapter.Cancel(context.Background(), h.ref("t1")); co.Disposition != adapter.CancelConfirmed {
		t.Fatalf("cancel: %+v", co)
	}
	h.waitIdle(h.ref("t1"))
	out, err = h.adapter.Reconcile(context.Background(), recoveryRef(h, "t1"))
	if err != nil || out.Status != adapter.ReconciliationReachableTerminal || out.Observed != council.TurnCancelled {
		t.Fatalf("cancelled: %+v err=%v", out, err)
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}

	// Failed terminal ⇒ ReachableTerminal/TurnFailed.
	h.turnScenario(`{"result": {"status": "ERROR", "error": "quota"}}`)
	if o, err := h.dispatch("t2", "p"); err != nil || o.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", o, err)
	}
	h.waitAttempt("att-t2", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	h.waitIdle(h.ref("t2"))
	out, _ = h.adapter.Reconcile(context.Background(), recoveryRef(h, "t2"))
	if out.Status != adapter.ReconciliationReachableTerminal || out.Observed != council.TurnFailed {
		t.Fatalf("failed: %+v", out)
	}

	// A present conversation file never upgrades a lost attempt.
	h.turnScenario(userInputDone, `{"exit_without_result": true}`)
	if o, err := h.dispatch("t3", "p"); err != nil || o.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", o, err)
	}
	h.waitIdle(h.ref("t3"))
	out, _ = h.adapter.Reconcile(context.Background(), recoveryRef(h, "t3"))
	if out.Status != adapter.ReconciliationUncertain {
		t.Fatalf("exit without result stays Uncertain: %+v", out)
	}
}
