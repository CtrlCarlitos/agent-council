//go:build unix

package codex

// Task 6 reconciliation + cancellation evidence (POSIX — real executor +
// fixture child, spec §3.10/§3.9): the four-state reconciliation matrix
// (in-life terminal, protected reconstruction, protected verified absence
// with the ONE same-attempt redispatch authorization, uncertain
// block-until-disposed across reopen), pre-acceptance rejections as
// DefinitivelyMissing, and the cancel flow (requested → confirmed →
// escalation → definitive refusal → already-terminal).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// lostAttemptScenario dispatches a turn that is accepted but never
// completes, then stops the child (host-loss analog): the launch is
// recorded dead and the attempt stays uncertain.
func lostAttemptScenario(t *testing.T, turnKey, prompt string, protected bool) (*adapterHarness, adapter.TurnRef) {
	t.Helper()
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, false)...)
	writeScenario(t, h.scratch, scenario...)
	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)
	if protected {
		insertHarnessAttestation(t, h, testAttestationID())
	}
	if out, err := h.dispatch(t, turnKey, prompt); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	att := waitAttempt(t, h, turnKey, func(a *storage.CodexTurnAttempt) bool { return a.NativeTurnID != nil })
	if protected {
		if att.RolloutProtection != "protected" {
			t.Fatalf("protected fixture setup failed: %q", att.RolloutProtection)
		}
	}
	// Child death mid-turn: launch recorded dead, attempt unresolved,
	// and the run fully retired from the adapter's live map.
	h.server.stop(context.Background(), testSessionID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		states, stateErr := h.store.CodexAttemptLaunchStates(context.Background(), att.AttemptID)
		h.adapter.mu.Lock()
		live := len(h.adapter.turns) > 0
		h.adapter.mu.Unlock()
		if stateErr == nil && len(states) > 0 && allLaunchesDead(states) && !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lost attempt never fully retired (states=%v live=%v)", states, live)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h, adapter.TurnRef{SessionID: testSessionID, TurnKey: turnKey}
}

func reconcileRef(ref adapter.TurnRef) adapter.RecoveryRef {
	return adapter.RecoveryRef{TurnRef: ref, Generation: 2}
}

// §3.10 row 1: the verified in-life terminal commits the durable
// outcome; reconciliation reports ReachableTerminal with the caller's
// RecoveryRef echoed verbatim.
func TestCodexAdapter_Reconcile_InLifeTerminal(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, true)...)
	writeScenario(t, h.scratch, scenario...)
	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-rec-live"}
	if out, err := h.dispatch(t, ref.TurnKey, "prompt"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.Terminal })

	recRef := reconcileRef(ref)
	outcome, err := h.adapter.Reconcile(context.Background(), recRef)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatalf("outcome validation: %v", err)
	}
	if outcome.Status != adapter.ReconciliationReachableTerminal || outcome.Observed != council.TurnCompleted {
		t.Fatalf("in-life terminal must reconcile terminal: %+v", outcome)
	}
	if outcome.Ref != recRef {
		t.Fatalf("recovery ref must be echoed verbatim: %+v", outcome.Ref)
	}
}

// §3.10 row 2: protected-mode rollout reconstruction — baseline + pdig +
// task_complete ⇒ terminal (acceptance + outcome committed durably).
func TestCodexAdapter_Reconcile_ProtectedReconstruction(t *testing.T) {
	turnKey := "t-rec-rebuild"
	prompt := "reconstruct this lost turn from the rollout"
	h, ref := lostAttemptScenario(t, turnKey, prompt, true)

	binding, err := h.store.GetCodexSessionBinding(context.Background(), testSessionID)
	if err != nil || binding == nil || binding.RolloutPath == nil {
		t.Fatalf("binding: %+v err=%v", binding, err)
	}
	attempt, err := h.store.GetLatestCodexTurnAttempt(context.Background(), testSessionID, turnKey)
	if err != nil || attempt == nil {
		t.Fatalf("attempt: %v", err)
	}

	// The native side absorbed the turn before dying: turn_context pins,
	// the user item matching the attempt's pdig, then task_complete.
	f, err := os.OpenFile(*binding.RolloutPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open rollout: %v", err)
	}
	for _, line := range []string{
		harnessTurnContext(h, testTurnID, nil),
		harnessUserItem(turnKey, prompt),
		harnessTaskComplete("reconstructed outcome", false),
	} {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_ = attempt

	outcome, err := h.adapter.Reconcile(context.Background(), reconcileRef(ref))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatalf("outcome validation: %v", err)
	}
	if outcome.Status != adapter.ReconciliationReachableTerminal || outcome.Observed != council.TurnCompleted {
		t.Fatalf("protected reconstruction must reconcile terminal: %+v", outcome)
	}

	// The reconstruction is committed durably: terminal + accepted.
	att, err := h.store.GetLatestCodexTurnAttempt(context.Background(), testSessionID, turnKey)
	if err != nil || att == nil {
		t.Fatalf("attempt: %v", err)
	}
	if !att.Terminal || att.ObservedStatus != "completed" {
		t.Fatalf("reconstruction must commit the terminal: %+v", att)
	}
	if att.Accepted == nil || !*att.Accepted {
		t.Fatalf("reconstruction must upgrade acceptance: %v", att.Accepted)
	}
}

// §3.10 row 3: protected verified absence (baseline unchanged, child
// known dead) authorizes exactly ONE same-attempt redispatch; the
// advisory twin stays Uncertain permanently.
func TestCodexAdapter_Reconcile_VerifiedAbsenceAuthorizesOneRedispatch(t *testing.T) {
	turnKey := "t-rec-absent"
	prompt := "never absorbed by the rollout"
	h, ref := lostAttemptScenario(t, turnKey, prompt, true)
	ctx := context.Background()

	outcome, err := h.adapter.Reconcile(ctx, reconcileRef(ref))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatalf("outcome validation: %v", err)
	}
	if outcome.Status != adapter.ReconciliationDefinitivelyMissing {
		t.Fatalf("verified absence must authorize redispatch, got %+v", outcome)
	}

	att, err := h.store.GetLatestCodexTurnAttempt(ctx, testSessionID, turnKey)
	if err != nil || att == nil {
		t.Fatalf("attempt: %v", err)
	}
	if att.AbsenceVerified != "verified" {
		t.Fatalf("absence must be recorded durably: %q", att.AbsenceVerified)
	}

	// The one-redispatch authorization: exactly one further launch
	// reservation passes the durable preconditions.
	attemptID := att.AttemptID
	if _, err := h.store.ReserveCodexLaunch(ctx, attemptID, "codex-app-server", 0); err != nil {
		t.Fatalf("the same-attempt redispatch must be authorized: %v", err)
	}
	if _, err := h.store.ReserveCodexLaunch(ctx, attemptID, "codex-app-server", 0); err == nil {
		t.Fatal("a second redispatch must be refused (one-redispatch rule)")
	}

	// The advisory twin: identical shape, but rollout evidence is
	// advisory — no absence claim, permanently uncertain.
	h2, ref2 := lostAttemptScenario(t, "t-rec-absent-adv", prompt, false)
	outcome2, err := h2.adapter.Reconcile(ctx, reconcileRef(ref2))
	if err != nil {
		t.Fatalf("advisory reconcile: %v", err)
	}
	if outcome2.Status != adapter.ReconciliationUncertain {
		t.Fatalf("advisory rollout signals stay uncertain: %+v", outcome2)
	}
	adv, _ := h2.store.GetLatestCodexTurnAttempt(ctx, testSessionID, "t-rec-absent-adv")
	if adv.AbsenceVerified != "" {
		t.Fatalf("advisory attempts must never verify absence: %q", adv.AbsenceVerified)
	}
	if _, err := h2.store.ReserveCodexLaunch(ctx, adv.AttemptID, "codex-app-server", 0); err == nil {
		t.Fatal("an advisory attempt must not redispatch")
	}
}

// Forced accepted-write failure: the terminal IS committed durably, so
// Reconcile must return ReachableTerminal with the committed outcome —
// never an Uncertain verdict that contradicts durable state.
func TestCodexAdapter_Reconcile_AcceptedWriteFailureKeepsTerminalTruth(t *testing.T) {
	turnKey := "t-rec-accfail"
	prompt := "reconstruct with a failing acceptance upgrade"
	h, ref := lostAttemptScenario(t, turnKey, prompt, true)

	binding, err := h.store.GetCodexSessionBinding(context.Background(), testSessionID)
	if err != nil || binding == nil || binding.RolloutPath == nil {
		t.Fatalf("binding: %+v err=%v", binding, err)
	}
	f, err := os.OpenFile(*binding.RolloutPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open rollout: %v", err)
	}
	for _, line := range []string{
		harnessTurnContext(h, testTurnID, nil),
		harnessUserItem(turnKey, prompt),
		harnessTaskComplete("committed regardless", false),
	} {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Force the acceptance upgrade to fail: accepted set to 0 (not NULL,
	// not 1) makes SetCodexAttemptAccepted's once-only guard refuse the
	// write with an error, exactly like any other upgrade failure.
	att, err := h.store.GetLatestCodexTurnAttempt(context.Background(), testSessionID, turnKey)
	if err != nil || att == nil {
		t.Fatalf("attempt: %v", err)
	}
	if _, err := h.store.DB().ExecContext(context.Background(),
		`UPDATE codex_turn_attempts SET accepted = 0 WHERE attempt_id = ?`, att.AttemptID); err != nil {
		t.Fatalf("force accepted failure: %v", err)
	}

	outcome, err := h.adapter.Reconcile(context.Background(), reconcileRef(ref))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatalf("outcome validation: %v", err)
	}
	if outcome.Status != adapter.ReconciliationReachableTerminal || outcome.Observed != council.TurnCompleted {
		t.Fatalf("the committed terminal is the truth; must not report Uncertain: %+v", outcome)
	}

	// Durable state confirms: terminal committed, accepted untouched.
	after, err := h.store.GetCodexTurnAttempt(context.Background(), att.AttemptID)
	if err != nil || after == nil {
		t.Fatalf("attempt after reconcile: %v", err)
	}
	if !after.Terminal || after.ObservedStatus != "completed" {
		t.Fatalf("terminal must be durably committed: %+v", after)
	}
	if after.Accepted == nil || *after.Accepted {
		t.Fatalf("accepted must remain unupgraded after the forced failure: %v", after.Accepted)
	}
}

// Protected attempt, baseline unchanged, but the child death is NOT
// durably known (launch never recorded dead): no absence claim —
// Uncertain.
func TestCodexAdapter_Reconcile_AbsenceRequiresKnownDeadChild(t *testing.T) {
	h := newAdapterHarness(t)
	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)
	insertHarnessAttestation(t, h, testAttestationID())

	// Crash boundary: attempt + reservation durable, launch never
	// reached a state — the child's fate is unknown.
	ctx := context.Background()
	if err := h.store.InsertCodexTurnAttempt(ctx, storage.CodexTurnAttempt{
		AttemptID: "att-unknown-fate", SessionID: testSessionID, TurnKey: "t-fate",
		PromptDigest: "pdig-v1:sha256:fixed", RolloutProtection: "protected",
		AttestationID:        strPtr(testAttestationID()),
		BaselineIdentity:     "0000:0000",
		BaselineMaterialized: true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := h.store.ReserveCodexLaunch(ctx, "att-unknown-fate", "codex-app-server", 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	outcome, err := h.adapter.Reconcile(ctx, reconcileRef(adapter.TurnRef{
		SessionID: testSessionID, TurnKey: "t-fate",
	}))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if outcome.Status != adapter.ReconciliationUncertain {
		t.Fatalf("unknown child fate must stay uncertain: %+v", outcome)
	}
}

// Only the recorded pre-acceptance rejection is DefinitivelyMissing
// without protected evidence.
func TestCodexAdapter_Reconcile_PreAcceptanceRejectionMissing(t *testing.T) {
	h := newAdapterHarness(t)
	writeScenario(t, h.scratch,
		authOKLine(),
		`{"emit_on_request": {"method":"thread/start","line":{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}}}`,
		`{"respond": {"method":"thread/start","result":{"id":"`+testThreadID+`","status":{"type":"idle"}}}}`,
		resumeRule(testThreadID, h.wsRoot, h.model, nil),
		`{"respond_error": {"method":"turn/start","code":-32600,"message":"thread not found: `+testThreadID+`"}}`)
	h.createAndPersist(t)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-rec-missing"}
	out, err := h.dispatch(t, ref.TurnKey, "prompt")
	if out.Status != adapter.DispatchRejected {
		t.Fatalf("dispatch must reject: %+v err=%v", out, err)
	}
	waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.ObservedStatus == "missing" })

	outcome, err := h.adapter.Reconcile(context.Background(), reconcileRef(ref))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if outcome.Status != adapter.ReconciliationDefinitivelyMissing || outcome.Observed != council.TurnFailed {
		t.Fatalf("pre-acceptance rejection is positive never-accepted evidence: %+v", outcome)
	}
}

// §3.10: unresolved attempts block the native session across restarts —
// durable state, not process memory.
func TestCodexAdapter_UncertainAttemptBlocksAcrossReopen(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario,
		`{"respond": {"method":"turn/start","delay_ms":2000,"result":{"id":"`+testTurnID+`","status":{"type":"inProgress"}}}}`)
	writeScenario(t, h.scratch, scenario...)
	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)
	withTimeoutVar(t, &dispatchAckTimeout, 200*time.Millisecond)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-block"}
	if out, err := h.dispatch(t, ref.TurnKey, "prompt"); err != nil || out.Status != adapter.DispatchUnknown {
		t.Fatalf("lost response must be unknown: %+v err=%v", out, err)
	}
	waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.ObservedStatus == "uncertain" })

	// "Restart": close the store, reopen the same state directory, and
	// build a fresh adapter over the durable state.
	stateDir := filepath.Join(h.scratch, "state")
	if err := h.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	blocked, err := reopened.HasCodexUnresolvedAttempts(context.Background(), testThreadID)
	if err != nil || !blocked {
		t.Fatalf("the unresolved attempt must block after reopen: %v err=%v", blocked, err)
	}

	freshServer := NewCodexServer(execpolicy.New(), lifecycleLaunchSource{scratch: h.scratch}, h.policy)
	t.Cleanup(func() { freshServer.stopAll(context.Background()) })
	fresh, err := NewCodexAdapter(reopened, freshServer, h.policy, h.profileDigest, codexIdentity{fn: defaultIdentity},
		func() (string, bool) { return testAttestationID(), true })
	if err != nil {
		t.Fatalf("new codex adapter: %v", err)
	}
	out, err := fresh.Dispatch(context.Background(), adapter.TurnRef{
		SessionID: testSessionID, TurnKey: "t-after-restart",
	}, "prompt")
	if err != nil || out.Status != adapter.DispatchRejected {
		t.Fatalf("the restarted session must stay blocked: %+v err=%v", out, err)
	}
	if !strings.Contains(out.Reason, "unresolved attempt") {
		t.Fatalf("block reason: %q", out.Reason)
	}
}

// ── Cancel (§3.9) ───────────────────────────────────────────────────────

// Accepted interrupt + verified interrupted terminal ⇒ CancelConfirmed;
// the confirmed cancel is durable: Collect reports TurnCancelled.
func TestCodexAdapter_Cancel_RequestedThenConfirmed(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, false)...)
	scenario = append(scenario,
		`{"respond": {"method":"turn/interrupt","result":{"action":"interrupted"}}}`,
		`{"emit_after_response": {"method":"turn/interrupt","line":{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"`+testThreadID+`","turn":{"id":"`+testTurnID+`","status":"interrupted"}}}}}`)
	writeScenario(t, h.scratch, scenario...)
	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-cancel-ok"}
	if out, err := h.dispatch(t, ref.TurnKey, "prompt"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.NativeTurnID != nil })

	outcome, err := h.adapter.Cancel(context.Background(), ref)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if outcome.Disposition != adapter.CancelConfirmed {
		t.Fatalf("verified interrupted terminal must confirm, got %+v", outcome)
	}

	att := waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "interrupted" {
		t.Fatalf("the interrupted terminal must be durable: %q", att.ObservedStatus)
	}
	res, err := h.adapter.Collect(context.Background(), ref)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if res.Status != council.TurnCancelled {
		t.Fatalf("a confirmed cancel collects TurnCancelled, got %s", res.Status)
	}
	// The terminal released the slot: the session is dispatchable again.
	_ = att
}

// Terminal before interrupt ⇒ CancelAlreadyTerminal (both live and
// finished turns).
func TestCodexAdapter_Cancel_AlreadyTerminal(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, true)...)
	writeScenario(t, h.scratch, scenario...)
	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-cancel-done"}
	if out, err := h.dispatch(t, ref.TurnKey, "prompt"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.Terminal })

	outcome, err := h.adapter.Cancel(context.Background(), ref)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if outcome.Disposition != adapter.CancelAlreadyTerminal {
		t.Fatalf("terminal-before-interrupt must be CancelAlreadyTerminal, got %+v", outcome)
	}
}

// Accepted interrupt but no terminal within the bound ⇒ graceful
// terminate → kill ⇒ CancelUnknown (attempt stays Uncertain).
func TestCodexAdapter_Cancel_EscalationUncertain(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, false)...)
	scenario = append(scenario, `{"respond": {"method":"turn/interrupt","result":{"action":"interrupted"}}}`)
	writeScenario(t, h.scratch, scenario...)
	withTimeoutVar(t, &cancelTerminalGrace, 200*time.Millisecond)
	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-cancel-esc"}
	if out, err := h.dispatch(t, ref.TurnKey, "prompt"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.NativeTurnID != nil })

	outcome, err := h.adapter.Cancel(context.Background(), ref)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if outcome.Disposition != adapter.CancelUnknown {
		t.Fatalf("no terminal within the bound must stay unknown, got %+v", outcome)
	}
	if terminatedCount(t, h.scratch) < 1 {
		t.Fatal("the escalation must terminate the child")
	}

	// Without a verified terminal the attempt stays uncertain and the
	// native session blocks until disposition.
	out, err := h.dispatch(t, "t-after-esc", "prompt")
	if err != nil || out.Status != adapter.DispatchRejected || !strings.Contains(out.Reason, "unresolved attempt") {
		t.Fatalf("the escalated attempt must keep blocking: %+v err=%v", out, err)
	}
}

// A definitive native refusal of the interrupt is CancelRejected.
func TestCodexAdapter_Cancel_DefinitiveRefusalRejected(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, false)...)
	scenario = append(scenario,
		`{"respond_error": {"method":"turn/interrupt","code":-32600,"message":"Invalid request: unknown variant <turn/interrupt>"}}`)
	writeScenario(t, h.scratch, scenario...)
	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-cancel-rej"}
	if out, err := h.dispatch(t, ref.TurnKey, "prompt"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.NativeTurnID != nil })

	outcome, err := h.adapter.Cancel(context.Background(), ref)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if outcome.Disposition != adapter.CancelRejected {
		t.Fatalf("a definitive refusal must be CancelRejected, got %+v", outcome)
	}
	// A refused interrupt is not terminal evidence: the turn is still
	// running and the slot is still held.
	if _, live := h.adapter.turns[ref]; !live {
		t.Fatal("a refused interrupt must not retire the turn run")
	}
}
