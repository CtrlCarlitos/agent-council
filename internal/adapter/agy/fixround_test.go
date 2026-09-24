//go:build unix

package agy

// AC-010 Task 5 fix round 1 evidence: the child inherits the operator
// home derived from expected_home (never Paths.Config) and the
// conversation materializes under ExpectedHome; orphan ids are durable
// (dispatch attempt, creation episode); failed durable transitions
// change the reported outcome instead of being dropped (fault seam);
// attempt insert + launch reservation are atomic; sealed descendants die
// with the forced kill; and the creation/cancel/resume/launch-check
// hardening.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ── Important 1: HOME is inherited, materialization under ExpectedHome ─

func TestAgyLaunch_ChildInheritsOperatorHome(t *testing.T) {
	h := newAgyHarness(t)
	wantHome := "HOME=" + filepath.Dir(h.policy.ExpectedHome)

	h.scenario(`{"conversation_id": "` + testNativeID + `"}`)
	if _, err := h.adapter.CreateSession(context.Background(), h.createRequest()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	h.persist(testNativeID)
	h.turnScenario(userInputDone, successResult("ok"))
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	h.waitIdle(h.ref("t1"))

	env := h.fixtureFile(".agy-fixture-env")
	if len(env) != 2 {
		t.Fatalf("one env line per launch (create + turn), got %v", env)
	}
	for _, line := range env {
		if line != wantHome {
			t.Fatalf("every agy launch inherits the operator home %q (never Paths.Config), got %q", wantHome, line)
		}
	}
	if req := h.exec.last(); req.HomeDir != filepath.Dir(h.policy.ExpectedHome) || req.HomeDir == req.Paths.Config {
		t.Fatalf("launch HomeDir = %q, Paths.Config = %q", req.HomeDir, req.Paths.Config)
	}
}

func TestAgyLaunch_EveryKindCarriesHomeDir(t *testing.T) {
	h := newAgyHarness(t)
	want := filepath.Dir(h.policy.ExpectedHome)
	for _, kind := range []LaunchKind{LaunchCreate, LaunchModels, LaunchPluginList} {
		req, err := h.source.AgyTurnLaunch(context.Background(), testSessionID, "", "", kind)
		if err != nil || req.HomeDir != want {
			t.Fatalf("%v launch HomeDir = %q err=%v, want %q", kind, req.HomeDir, err, want)
		}
	}
	req, err := h.source.AgyTurnLaunch(context.Background(), testSessionID, testNativeID, "pdig-v1:sha256:x", LaunchTurn)
	if err != nil || req.HomeDir != want {
		t.Fatalf("turn launch HomeDir = %q err=%v", req.HomeDir, err)
	}
	if _, err := agyHomeDir("/home/op/not-gemini"); err == nil {
		t.Fatal("an expected_home that is not a .gemini directory must fail closed")
	}
}

func TestAgyDispatch_MaterializesUnderExpectedHome(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"materialize": true}`, userInputDone, successResult("ok"))
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	h.waitIdle(h.ref("t1"))

	b, err := h.store.GetAgySessionBinding(context.Background(), testSessionID)
	if err != nil || b == nil || !b.Materialized {
		t.Fatalf("the accepted turn materializes the binding: %+v err=%v", b, err)
	}
	want := conversationPath(h.policy.ExpectedHome, testNativeID)
	if b.ConversationPath == nil || *b.ConversationPath != want {
		t.Fatalf("materialized path = %v, want %s (under ExpectedHome)", b.ConversationPath, want)
	}
	if !strings.HasPrefix(want, h.home) {
		t.Fatalf("the conversation lives under the test ExpectedHome %s, got %s", h.home, want)
	}
	if err := h.adapter.ResumeSession(context.Background(), h.binding()); err != nil {
		t.Fatalf("materialized binding resumes: %v", err)
	}
}

func (h *agyHarness) binding() adapter.SessionBinding {
	return adapter.SessionBinding{
		SessionID: testSessionID, Contributor: "agy", NativeSessionID: testNativeID,
		Config: adapter.SessionConfig{WorkspaceRoot: h.wsRoot, Model: h.model},
	}
}

// ── Important 2: durable orphan ids ────────────────────────────────────

func TestAgyDispatch_OrphanConversationDurableAcrossRestart(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.scenario(`{"conversation_id": "`+otherNativeID+`"}`, userInputDone, successResult("x"))
	if out, _ := h.dispatch("t1", "p"); out.Status != adapter.DispatchRejected {
		t.Fatalf("fallback id drift is rejected: %+v", out)
	}
	h.reopen()
	att, err := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if err != nil || att == nil || att.OrphanConversationID == nil || *att.OrphanConversationID != otherNativeID {
		t.Fatalf("the orphan id is durable on the attempt after reopen: %+v err=%v", att, err)
	}
	if att.ObservedStatus != "missing" {
		t.Fatalf("pre-write drift stays missing: %+v", att)
	}
}

func TestAgyCreate_ProfileDriftRecordsOrphanOnEpisode(t *testing.T) {
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "`+testNativeID+`"}`, `{"permission_mode": "strict"}`)
	_, err := h.adapter.CreateSession(context.Background(), h.createRequest())
	var cd *ErrCreationDrift
	var pd *ErrProfileDrift
	if !errors.As(err, &cd) || !errors.As(err, &pd) || cd.NativeID != testNativeID || pd.Field != "permission_mode" {
		t.Fatalf("creation drift is ErrCreationDrift wrapping ErrProfileDrift with the id, got %v", err)
	}
	h.reopen()
	ep, err := h.store.OpenAgyCreationUncertainty(context.Background(), testSessionID)
	if err != nil || ep == nil || ep.OrphanNativeID == nil || *ep.OrphanNativeID != testNativeID {
		t.Fatalf("the uncertainty episode carries the orphan id: %+v err=%v", ep, err)
	}
	if ep.Episode != cd.Episode {
		t.Fatalf("the error names the recorded episode: %d vs %d", cd.Episode, ep.Episode)
	}
	before := h.exec.starts.Load()
	if _, err := h.adapter.CreateSession(context.Background(), h.createRequest()); err == nil {
		t.Fatal("the orphan episode blocks recreation until resolved")
	}
	if h.exec.starts.Load() != before {
		t.Fatal("a blocked creation starts no child")
	}
}

// ── Important 3: surfaced transition errors (fault seam) ───────────────

func faultOn(op string, err error) func(string) error {
	return func(o string) error {
		if o == op {
			return err
		}
		return nil
	}
}

func TestAgyFault_AcceptanceRecordFailureIsNeverTerminal(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.adapter.fault = faultOn(opNativeStepIndex, errors.New("disk full"))
	h.turnScenario(userInputDone, successResult("done"))
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	s, err := h.adapter.Observe(context.Background(), h.ref("t1"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	for _, ev := range collectEvents(t, s, 10*time.Second) {
		if ev.Type == adapter.EventTerminal {
			t.Fatalf("no terminal may be published without durable acceptance: %+v", ev)
		}
	}
	if err := s.Err(); err == nil || !strings.Contains(err.Error(), "acceptance evidence") {
		t.Fatalf("the tap closes with the acceptance failure, got %v", err)
	}
	h.waitIdle(h.ref("t1"))
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att.Terminal || att.ObservedStatus != "uncertain" || att.Accepted != nil {
		t.Fatalf("a failed acceptance record leaves the attempt uncertain (Unknown), never completed: %+v", att)
	}
	co, _ := h.adapter.Cancel(context.Background(), h.ref("t1"))
	if co.Disposition != adapter.CancelUnknown {
		t.Fatalf("the reported outcome is Unknown: %+v", co)
	}
}

func TestAgyFault_MissingRecordFailureIsUnknown(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.adapter.fault = faultOn(opAttemptMissing, errors.New("disk full"))
	h.turnScenario(`{"permission_mode": "strict"}`)
	out, err := h.dispatch("t1", "p")
	if out.Status != adapter.DispatchUnknown || err == nil {
		t.Fatalf("an unrecorded pre-write rejection is Unknown, never Rejected: %+v err=%v", out, err)
	}
	var pd *ErrProfileDrift
	if !errors.As(err, &pd) {
		t.Fatalf("the cause is preserved: %v", err)
	}
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att.ObservedStatus != "uncertain" {
		t.Fatalf("the attempt stays uncertain: %+v", att)
	}
	if blocked, _ := h.store.HasAgyUnresolvedAttempts(context.Background(), testNativeID); !blocked {
		t.Fatal("the unrecorded rejection keeps the conversation blocked")
	}
}

func TestAgyFault_DeadRecordFailureSurfacesOnTerminal(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.adapter.fault = faultOn(opLaunchDead, errors.New("disk full"))
	h.turnScenario(userInputDone, successResult("done"))
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	s, err := h.adapter.Observe(context.Background(), h.ref("t1"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	collectEvents(t, s, 10*time.Second)
	if err := s.Err(); err == nil || !strings.Contains(err.Error(), "launch dead state") {
		t.Fatalf("the unrecorded dead state is surfaced on the turn, got %v", err)
	}
}

func TestAgyFault_MaterializeFailureFailsResume(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.adapter.fault = faultOn(opMaterialize, errors.New("disk full"))
	h.turnScenario(`{"materialize": true}`, userInputDone, successResult("ok"))
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	h.waitIdle(h.ref("t1"))
	if err := h.adapter.ResumeSession(context.Background(), h.binding()); err == nil || !strings.Contains(err.Error(), "materialization") {
		t.Fatalf("a failed materialization record fails ResumeSession, got %v", err)
	}
	h.adapter.fault = nil
	if err := h.adapter.ResumeSession(context.Background(), h.binding()); err != nil {
		t.Fatalf("the retried materialization succeeds: %v", err)
	}
	if b, _ := h.store.GetAgySessionBinding(context.Background(), testSessionID); b == nil || !b.Materialized {
		t.Fatalf("the retry recorded the materialization: %+v", b)
	}
}

func TestAgyReconcile_StoreErrorIsReturned(t *testing.T) {
	h := newAgyHarness(t)
	_ = h.store.Close()
	out, err := h.adapter.Reconcile(context.Background(), recoveryRef(h, "t1"))
	if err == nil {
		t.Fatalf("a store read failure is returned, not reported as Uncertain/nil: %+v", out)
	}
}

func TestAgyCrashGap_DeadBeforeMissing(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	f := newFreeze(t)
	h.adapter.fault = func(op string) error {
		if op == opAttemptMissing {
			f.hit()
			return errors.New("crashed between dead and missing")
		}
		return nil
	}
	h.turnScenario(`{"permission_mode": "strict"}`)
	crashDispatch(t, h, f, "t1")
	f.wait(t)
	h.reopen()

	if st := h.launchStates("att-t1"); len(st) != 1 || st[0] != "dead" {
		t.Fatalf("the dead launch state is durable: %v", st)
	}
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att.ObservedStatus != "uncertain" {
		t.Fatalf("missing was never recorded: %+v", att)
	}
	// A dead child without the missing record is not positive evidence
	// that no byte crossed: Uncertain, blocked until a disposition.
	assertUncertainBlocked(t, h, "t1")
}

// ── Important 4: atomic attempt + reservation ──────────────────────────

func TestAgyCrashGap_AttemptAndReservationAtomic(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(userInputDone, successResult("x"))
	if _, err := h.store.DB().Exec(`CREATE TRIGGER agy_fail_reserve BEFORE INSERT ON agy_attempt_launches
BEGIN SELECT RAISE(ABORT, 'simulated reservation failure'); END;`); err != nil {
		t.Fatal(err)
	}
	out, err := h.dispatch("t1", "p")
	if err == nil || out.Status != adapter.DispatchRejected {
		t.Fatalf("a failed reservation rejects: %+v err=%v", out, err)
	}
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("no child without a reservation, launches=%d", n)
	}
	h.reopen()
	if att, err := h.store.GetAgyTurnAttempt(context.Background(), "att-t1"); err != nil || att != nil {
		t.Fatalf("attempt and reservation are one transaction: nothing survives, got %+v err=%v", att, err)
	}
	if st := h.launchStates("att-t1"); len(st) != 0 {
		t.Fatalf("no launch rows: %v", st)
	}
	if _, err := h.store.DB().Exec(`DROP TRIGGER agy_fail_reserve`); err != nil {
		t.Fatal(err)
	}
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("the turn is dispatchable afterwards: %+v err=%v", out, err)
	}
	att := h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	if att.LaunchCount != 1 {
		t.Fatalf("both writes landed together: %+v", att)
	}
}

// ── Descendant termination (sealed launches) ──────────────────────────

func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	// A killed, not-yet-reaped zombie is gone for our purposes.
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	fields := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

func TestAgyCancel_ForcedKillReapsSealedDescendants(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-group force kill applies to sealed launches (Linux only)")
	}
	h := newAgyHarness(t)
	h.persist(testNativeID)
	withShortTimers(t, initTimeout, 200*time.Millisecond)
	oldTerm := terminateTimeout
	terminateTimeout = 300 * time.Millisecond
	t.Cleanup(func() { terminateTimeout = oldTerm })
	h.turnScenario(`{"spawn_sleeper": true}`, userInputDone, successResult("never"))
	h.exec.setWrapProc(func(p execpolicy.ManagedProcess) execpolicy.ManagedProcess {
		return &wrappedProc{ManagedProcess: p, stdin: &swallowStdin{real: p.Stdin()}}
	})
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	pids := h.fixtureFile(".agy-fixture-sleeper-pid")
	if len(pids) != 1 {
		t.Fatalf("the fixture spawned one descendant before init: %v", pids)
	}
	pid, err := strconv.Atoi(pids[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if processGone(pid) {
		t.Fatal("the descendant is alive before cancellation")
	}
	co, err := h.adapter.Cancel(context.Background(), h.ref("t1"))
	if err != nil || co.Disposition != adapter.CancelUnknown {
		t.Fatalf("SIGINT ignored, SIGTERM ignored ⇒ forced kill ⇒ Unknown: %+v err=%v", co, err)
	}
	h.waitIdle(h.ref("t1"))
	deadline := time.Now().Add(5 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("descendant %d survived the forced termination of its sealed parent", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ── Minors ─────────────────────────────────────────────────────────────

func TestAgyCreate_WaiterHonoursCtxAndCallIsForgotten(t *testing.T) {
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "`+testNativeID+`"}`, `{"slow_init_ms": 400}`)
	first := make(chan error, 1)
	go func() {
		_, err := h.adapter.CreateSession(context.Background(), h.createRequest())
		first <- err
	}()
	// Wait until the first call owns the shared creation.
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.adapter.createMu.Lock()
		n := len(h.adapter.creations)
		h.adapter.createMu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first creation never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := h.adapter.CreateSession(ctx, h.createRequest()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a duplicate waiter honours its own ctx, got %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("the shared creation completes: %v", err)
	}
	h.adapter.createMu.Lock()
	n := len(h.adapter.creations)
	h.adapter.createMu.Unlock()
	if n != 0 {
		t.Fatalf("a completed creation is forgotten, %d entries remain", n)
	}
	// A later call re-runs the durable uncertainty check.
	if _, _, err := h.store.RecordAgyCreationUncertain(context.Background(), storage.AgyCreationUncertainty{
		RunID: testRunID, SessionID: testSessionID, Reason: "operator-recorded",
		RecordedBy: "test", CauseOpID: "op-x",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.adapter.CreateSession(context.Background(), h.createRequest()); err == nil {
		t.Fatal("a later CreateSession re-runs the durable check instead of replaying the old call")
	}
}

func TestAgyCancel_CtxExpiryIsRequested(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	withShortTimers(t, initTimeout, 1500*time.Millisecond)
	h.turnScenario(userInputDone, successResult("never"))
	h.exec.setWrapProc(func(p execpolicy.ManagedProcess) execpolicy.ManagedProcess {
		return &wrappedProc{ManagedProcess: p, stdin: &swallowStdin{real: p.Stdin()}}
	})
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	co, err := h.adapter.Cancel(ctx, h.ref("t1"))
	if err != nil || co.Disposition != adapter.CancelRequested {
		t.Fatalf("caller ctx expiry before the verified result ⇒ CancelRequested, got %+v err=%v", co, err)
	}
	h.waitIdle(h.ref("t1")) // the grace escalation still ends the child
}

func TestAgyDispatch_ExternalInterruptIsFailedNotCancelled(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(userInputDone, `{"result": {"status": "ERROR", "error": "interrupted"}}`)
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	att := h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "failed" {
		t.Fatalf("an interrupted result without a Council SIGINT is failed, got %q", att.ObservedStatus)
	}
	h.waitIdle(h.ref("t1"))
	s, err := h.adapter.Observe(context.Background(), h.ref("t1"))
	if err != nil {
		t.Fatal(err)
	}
	evs := collectEvents(t, s, 5*time.Second)
	last := evs[len(evs)-1]
	if last.Type != adapter.EventTerminal || last.Status != council.TurnFailed || !strings.Contains(last.Payload, "external") {
		t.Fatalf("the terminal notes the external interrupt: %+v", last)
	}
}

func TestAgyResume_EmptyConfigRefused(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	for _, cfg := range []adapter.SessionConfig{{}, {Model: h.model}, {WorkspaceRoot: h.wsRoot}} {
		b := h.binding()
		b.Config = cfg
		if err := h.adapter.ResumeSession(context.Background(), b); err == nil {
			t.Fatalf("an empty config (%+v) must be refused", cfg)
		}
	}
}

// tamperSource rewrites the launch request after the storage-backed
// source built it.
type tamperSource struct {
	inner  AgyTurnLaunchSource
	mutate func(*execpolicy.LaunchRequest)
}

func (s tamperSource) AgyTurnLaunch(ctx context.Context, sid adapter.SessionID, nativeID, pdig string, kind LaunchKind) (execpolicy.LaunchRequest, error) {
	req, err := s.inner.AgyTurnLaunch(ctx, sid, nativeID, pdig, kind)
	if err == nil {
		s.mutate(&req)
	}
	return req, err
}

func TestAgyValidateLaunch_TiedToAllocationImageAndHome(t *testing.T) {
	cases := map[string]func(t *testing.T, req *execpolicy.LaunchRequest){
		// The log path and scratch move together: a request-relative
		// check would accept it; the allocation check does not.
		"scratch_and_log_relocated": func(t *testing.T, req *execpolicy.LaunchRequest) {
			other := t.TempDir()
			req.Paths.Scratch = other
			req.Args[logFileIndex] = filepath.Join(other, launchLogDir, "turn-x.log")
		},
		"home_relocated": func(t *testing.T, req *execpolicy.LaunchRequest) { req.HomeDir = req.Paths.Config },
		"home_missing":   func(t *testing.T, req *execpolicy.LaunchRequest) { req.HomeDir = "" },
		"run_id":         func(t *testing.T, req *execpolicy.LaunchRequest) { req.RunID = "run-other" },
	}
	if runtime.GOOS == "linux" {
		cases["image_copy"] = func(t *testing.T, req *execpolicy.LaunchRequest) {
			cp := *req.SealedImage
			req.SealedImage = &cp
		}
		cases["image_digest"] = func(t *testing.T, req *execpolicy.LaunchRequest) {
			cp := *req.SealedImage
			cp.Digest = "sha256:" + strings.Repeat("0", 64)
			req.SealedImage = &cp
		}
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newAgyHarness(t)
			h.persist(testNativeID)
			h.turnScenario(userInputDone, successResult("x"))
			h.source2(tamperSource{inner: h.source, mutate: func(r *execpolicy.LaunchRequest) { mutate(t, r) }})
			out, err := h.dispatch("t1", "p")
			if err == nil || out.Status != adapter.DispatchRejected {
				t.Fatalf("a tampered launch is refused before start: %+v err=%v", out, err)
			}
			if n := h.exec.starts.Load(); n != 0 {
				t.Fatalf("no child for a tampered launch, launches=%d", n)
			}
		})
	}
}

// source2 rebuilds the adapter over a different launch source.
func (h *agyHarness) source2(src AgyTurnLaunchSource) {
	h.t.Helper()
	a, err := NewFixtureScopedAdapter(h.store, h.exec, src, h.wm, h.policy, h.profileDigest,
		h.fx.SealedImage, fnIdentity{fn: defaultAttempt}, h.required, FixtureMode{})
	if err != nil {
		h.t.Fatalf("NewFixtureScopedAdapter: %v", err)
	}
	h.adapter = a
}

// initRewriteProc rewrites the child's init line on the wire (stdout).
type initRewriteProc struct {
	execpolicy.ManagedProcess
	out io.Reader
}

func (p *initRewriteProc) Stdout() io.Reader { return p.out }

func rewriteInit(p execpolicy.ManagedProcess, from, to string) execpolicy.ManagedProcess {
	pr, pw := io.Pipe()
	go func() {
		br := bufio.NewReader(p.Stdout())
		first := true
		for {
			line, err := br.ReadString('\n')
			if first && line != "" {
				line = strings.Replace(line, from, to, 1)
				first = false
			}
			if line != "" {
				if _, werr := io.WriteString(pw, line); werr != nil {
					_, _ = io.Copy(io.Discard, br)
					return
				}
			}
			if err != nil {
				_ = pw.Close()
				return
			}
		}
	}()
	return &initRewriteProc{ManagedProcess: p, out: pr}
}

func TestAgyDispatch_EmptyInitFieldsAreDriftPreWrite(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		want     func(error) bool
	}{
		{"empty_conversation_id", `"conversation_id":"` + testNativeID + `"`, `"conversation_id":""`, func(err error) bool {
			var d *ErrConversationDrift
			return errors.As(err, &d) && d.Observed == ""
		}},
		{"empty_permission_mode", `"permission_mode":"request-review"`, `"permission_mode":""`, func(err error) bool {
			var d *ErrProfileDrift
			return errors.As(err, &d) && d.Field == "permission_mode"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgyHarness(t)
			h.persist(testNativeID)
			h.turnScenario(userInputDone, successResult("x"))
			h.exec.setWrapProc(func(p execpolicy.ManagedProcess) execpolicy.ManagedProcess {
				return rewriteInit(p, tc.from, tc.to)
			})
			out, err := h.dispatch("t1", "prompt")
			if !tc.want(err) || out.Status != adapter.DispatchRejected {
				t.Fatalf("an empty init field is drift, rejected pre-write: %+v err=%v", out, err)
			}
			if input := h.fixtureFile(".agy-fixture-input"); len(input) != 0 {
				t.Fatalf("the prompt must never be written on drift, child read %v", input)
			}
		})
	}
}

func TestAgyDispatch_RedispatchAfterTerminalRefused(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(userInputDone, successResult("x"))
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	h.waitIdle(h.ref("t1"))
	before := h.exec.starts.Load()
	// Same ref under a different attempt identity: still refused.
	h.adapter.identity = fnIdentity{fn: func(ref adapter.TurnRef) (string, bool) { return "att-retry-" + ref.TurnKey, true }}
	out, err := h.dispatch("t1", "p")
	if err == nil || out.Status != adapter.DispatchRejected || !strings.Contains(out.Reason, "verified terminal") {
		t.Fatalf("a terminal turn is never redispatched: %+v err=%v", out, err)
	}
	if h.exec.starts.Load() != before {
		t.Fatal("the refused redispatch starts no child")
	}
}

func TestAgyCollect_RawEvidenceClamped(t *testing.T) {
	h := newAgyHarness(t)
	resultLine := func(response string) []byte {
		line, _ := json.Marshal(map[string]any{"event": "result", "result": map[string]any{
			"conversation_id": testNativeID, "status": "SUCCESS", "response": response, "error": "",
			"duration_seconds": 0, "num_turns": 1,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "thinking_tokens": 0, "cache_read_tokens": 0, "total_tokens": 0},
		}})
		return line
	}
	// The largest result line the reader admits: with the verification
	// record appended the evidence would exceed the cap.
	line := resultLine(strings.Repeat("a", maxEventLineBytes-len(resultLine(""))))
	payload := string(line)
	att := &storage.AgyTurnAttempt{ObservedStatus: "completed", Terminal: true, ResultPayload: &payload,
		RequiredTools: []string{"view_file"}, ExecutedTools: []string{"view_file"}}
	res, err := h.adapter.resultFromAttempt(h.ref("t1"), att)
	if err != nil || res.ResultStatus != adapter.ResultAvailable {
		t.Fatalf("result: %+v err=%v", res.ResultStatus, err)
	}
	if len(res.RawEvidence) > adapter.MaxRawEvidenceBytes {
		t.Fatalf("RawEvidence %d bytes exceeds the %d cap", len(res.RawEvidence), adapter.MaxRawEvidenceBytes)
	}
	var raw struct {
		ResultOmitted *struct {
			Bytes int `json:"bytes"`
		} `json:"result_omitted"`
		Verification struct {
			Executed []string `json:"executed_tools"`
		} `json:"verification"`
	}
	if err := json.Unmarshal(res.RawEvidence, &raw); err != nil {
		t.Fatalf("clamped RawEvidence stays valid JSON: %v", err)
	}
	if raw.ResultOmitted == nil || raw.ResultOmitted.Bytes != len(payload) || len(raw.Verification.Executed) != 1 {
		t.Fatalf("the oversized result line is omitted by size+digest, verification kept: %+v", raw)
	}
}
