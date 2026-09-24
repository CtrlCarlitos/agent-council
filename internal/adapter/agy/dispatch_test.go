//go:build unix

package agy

// Dispatch evidence (AC-010 spec §3.1/§3.5/§3.9): init verified BEFORE
// the prompt exists on the wire, the prompt only ever on stdin, the
// first stdin byte as the only ambiguity boundary, acceptance on the
// user_input DONE step, required-tool verification with the four denial
// classes, stderr markers and exit-without-result as Uncertain, single
// flight per native conversation, and observer detach.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const (
	userInputDone = `{"step": {"state": "DONE", "step_type": "user_input"}}`
	agentText     = `{"step": {"state": "DONE", "step_type": "agent_response", "text_delta": "working"}}`
)

func toolStep(name string) string {
	return `{"step": {"state": "DONE", "step_type": "tool", "tool_name": "` + name + `"}}`
}

func successResult(response string) string {
	return `{"result": {"status": "SUCCESS", "response": "` + response + `", "num_turns": 1, "usage": {"input_tokens": 3, "output_tokens": 5, "total_tokens": 8}}}`
}

func collectEvents(t *testing.T, s adapter.Stream, timeout time.Duration) []adapter.Event {
	t.Helper()
	var out []adapter.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("stream did not close within %v; events so far: %+v", timeout, out)
		}
	}
}

func TestAgyDispatch_AcceptedOnUserInputAndCompleted(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(userInputDone, agentText, successResult("hello there"))

	const prompt = "review the design, then answer"
	out, err := h.dispatch("t1", prompt)
	if err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	att := h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "completed" {
		t.Fatalf("SUCCESS ⇒ completed, got %q", att.ObservedStatus)
	}
	if att.Accepted == nil || !*att.Accepted || att.NativeStepIndex == nil || *att.NativeStepIndex != 0 {
		t.Fatalf("user_input DONE is the acceptance evidence: accepted=%v idx=%v", att.Accepted, att.NativeStepIndex)
	}
	if att.LaunchCount != 1 {
		t.Fatalf("launch_count must be 1, got %d", att.LaunchCount)
	}
	want, _ := promptDigest(testNativeID, "t1", "att-t1", prompt)
	if att.PromptDigest != want {
		t.Fatalf("pdig-v1 digest must be durable on the attempt, got %q want %q", att.PromptDigest, want)
	}
	h.waitIdle(h.ref("t1"))
	if states := h.launchStates("att-t1"); len(states) != 1 || states[0] != "dead" {
		t.Fatalf("the process exit is recorded known-dead, got %v", states)
	}

	// The prompt was written to stdin as ONE user message and never
	// appears in argv; the exact frozen argv resumed the conversation.
	input := h.fixtureFile(".agy-fixture-input")
	if len(input) != 1 || !strings.Contains(input[0], `"event":"user"`) || !strings.Contains(input[0], prompt) {
		t.Fatalf("stdin must carry exactly the one user message, got %v", input)
	}
	args := h.fixtureFile(".agy-fixture-args")
	if len(args) != 1 || strings.Contains(args[0], prompt) {
		t.Fatalf("the prompt is never an argv element: %v", args)
	}
	if !strings.Contains(args[0], "--conversation\x1f"+testNativeID) {
		t.Fatalf("turn launches resume the bound conversation: %q", args[0])
	}

	res, err := h.adapter.Collect(context.Background(), h.ref("t1"))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.Status != council.TurnCompleted || res.ResultStatus != adapter.ResultAvailable || res.Output != "hello there" {
		t.Fatalf("collect: %+v", res)
	}
	if !res.Usage.InputTokens.Available || res.Usage.InputTokens.Value != 3 || res.Usage.OutputTokens.Value != 5 {
		t.Fatalf("usage as observed: %+v", res.Usage)
	}
	var raw struct {
		Result       map[string]any `json:"result"`
		Verification struct {
			Incomplete bool `json:"verification_incomplete"`
		} `json:"verification"`
	}
	if err := json.Unmarshal(res.RawEvidence, &raw); err != nil {
		t.Fatalf("RawEvidence must be JSON: %v (%s)", err, res.RawEvidence)
	}
	if raw.Result["event"] != "result" {
		t.Fatalf("RawEvidence carries the verbatim result line, got %v", raw.Result)
	}
	if raw.Verification.Incomplete {
		t.Fatal("no required tools, no denials: verification complete")
	}

	// Observe after terminal replays the recorded terminal.
	s, err := h.adapter.Observe(context.Background(), h.ref("t1"))
	if err != nil {
		t.Fatalf("Observe after terminal: %v", err)
	}
	evs := collectEvents(t, s, 5*time.Second)
	if len(evs) == 0 || evs[len(evs)-1].Type != adapter.EventTerminal || evs[len(evs)-1].Status != council.TurnCompleted {
		t.Fatalf("replay must end with the recorded terminal, got %+v", evs)
	}
}

func TestAgyDispatch_FallbackIDDriftNeverWritesPrompt(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	// The id is NOT known to the child: 1.2.9 silently falls back to a
	// fresh conversation (hazard 2).
	h.scenario(`{"conversation_id": "`+otherNativeID+`"}`, userInputDone, successResult("x"))

	out, err := h.dispatch("t1", "secret prompt")
	var drift *ErrConversationDrift
	if !errors.As(err, &drift) || out.Status != adapter.DispatchRejected {
		t.Fatalf("fallback id must be ErrConversationDrift + Rejected, got %+v err=%v", out, err)
	}
	if drift.Observed != otherNativeID || drift.Requested != testNativeID {
		t.Fatalf("orphan id recorded as a diagnostic: %+v", drift)
	}
	if !strings.Contains(out.Reason, otherNativeID) {
		t.Fatalf("the orphan id is carried in the rejection reason: %q", out.Reason)
	}
	if input := h.fixtureFile(".agy-fixture-input"); len(input) != 0 {
		t.Fatalf("the prompt must NEVER be written on drift, child read %v", input)
	}
	att := h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.ObservedStatus == "missing" })
	if att.Terminal || att.Accepted != nil {
		t.Fatalf("pre-write rejection is missing, not terminal/accepted: %+v", att)
	}
	if states := h.launchStates("att-t1"); len(states) != 1 || states[0] != "dead" {
		t.Fatalf("the terminated child is recorded dead, got %v", states)
	}
	blocked, _ := h.store.HasAgyUnresolvedAttempts(context.Background(), testNativeID)
	if blocked {
		t.Fatal("a pre-write rejection must not block the conversation")
	}
}

func TestAgyDispatch_ProfileAndToolDriftRejectedPreWrite(t *testing.T) {
	cases := []struct {
		name string
		line string
		want func(error) bool
	}{
		{"permission_mode", `{"permission_mode": "strict"}`, func(err error) bool {
			var d *ErrProfileDrift
			return errors.As(err, &d) && d.Field == "permission_mode"
		}},
		{"tools", `{"tools": ["view_file"]}`, func(err error) bool {
			var d *ErrToolInventoryDrift
			return errors.As(err, &d)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgyHarness(t)
			h.persist(testNativeID)
			h.turnScenario(tc.line, userInputDone, successResult("x"))
			out, err := h.dispatch("t1", "prompt")
			if !tc.want(err) || out.Status != adapter.DispatchRejected {
				t.Fatalf("drift must reject pre-write: %+v err=%v", out, err)
			}
			if input := h.fixtureFile(".agy-fixture-input"); len(input) != 0 {
				t.Fatalf("prompt written despite drift: %v", input)
			}
		})
	}
}

func TestAgyDispatch_DeniedToolEmitsToolDeniedAndIncomplete(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.required.set("t1", []string{"run_command"})
	// denied_actions shorthand: SUCCESS, exit 0 — exit 0 never clears
	// the flag.
	h.turnScenario(userInputDone, toolStep("run_command"),
		`{"denied_actions": [{"action": "command", "display_name": "RunCommand"}]}`)

	if out, err := h.dispatch("t1", "run it"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	att := h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "completed" || !att.VerificationIncomplete {
		t.Fatalf("SUCCESS with a denial is completed AND verification_incomplete: %+v", att)
	}
	if len(att.DeniedTools) != 1 || att.DeniedTools[0] != "run_command" {
		t.Fatalf("denied tools: %v", att.DeniedTools)
	}
	if len(att.MissingRequiredTools) != 1 || att.MissingRequiredTools[0] != "run_command" {
		t.Fatalf("missing required: %v", att.MissingRequiredTools)
	}
	v, ok, err := h.adapter.Verification(context.Background(), h.ref("t1"))
	if err != nil || !ok || !v.Incomplete || len(v.Denied) != 1 {
		t.Fatalf("typed accessor: %+v ok=%v err=%v", v, ok, err)
	}
	h.waitIdle(h.ref("t1"))
	s, err := h.adapter.Observe(context.Background(), h.ref("t1"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	var denied int
	for _, ev := range collectEvents(t, s, 5*time.Second) {
		if ev.Type == adapter.EventToolDenied {
			denied++
			if !strings.Contains(ev.Payload, "run_command") {
				t.Fatalf("tool_denied names the attributed tool: %q", ev.Payload)
			}
		}
	}
	if denied != 1 {
		t.Fatalf("one tool_denied event per denial entry, got %d", denied)
	}
}

func TestAgyDispatch_RequiredToolSkippedIncomplete(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.required.set("t1", []string{"run_command"})
	h.turnScenario(userInputDone, toolStep("view_file"), successResult("done"))

	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	att := h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	if !att.VerificationIncomplete || len(att.MissingRequiredTools) != 1 || att.MissingRequiredTools[0] != "run_command" {
		t.Fatalf("a silently skipped required tool flags incomplete: %+v", att)
	}
	if len(att.ExecutedTools) != 1 || att.ExecutedTools[0] != "view_file" {
		t.Fatalf("executed: %v", att.ExecutedTools)
	}
}

func TestAgyDispatch_RequiredToolsOutsideExpectedRejected(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.required.set("t1", []string{"invoke_subagent"})
	h.turnScenario(userInputDone, successResult("x"))
	out, err := h.dispatch("t1", "p")
	if err == nil || out.Status != adapter.DispatchRejected {
		t.Fatalf("a required tool outside expected_tools is rejected: %+v err=%v", out, err)
	}
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("no child for an invalid required set, launches=%d", n)
	}
}

func TestAgyDispatch_DenialClassesRecorded(t *testing.T) {
	cases := []struct {
		name  string
		steps []string
		deny  string
		check func(t *testing.T, a *storage.AgyTurnAttempt)
	}{
		{"ambiguous", []string{toolStep("run_command"), toolStep("send_command_input")},
			`{"action": "command", "display_name": "RunCommand"}`,
			func(t *testing.T, a *storage.AgyTurnAttempt) {
				if len(a.AmbiguousDenials) != 1 || len(a.DeniedTools) != 2 {
					t.Fatalf("ambiguous: %+v", a)
				}
			}},
		{"unattributed", []string{toolStep("view_file")},
			`{"action": "command", "display_name": "RunCommand"}`,
			func(t *testing.T, a *storage.AgyTurnAttempt) {
				if len(a.UnattributedDenials) != 1 || len(a.DeniedTools) != 0 {
					t.Fatalf("unattributed: %+v", a)
				}
			}},
		{"unmapped", []string{toolStep("view_file")},
			`{"action": "browse", "display_name": "OpenBrowser"}`,
			func(t *testing.T, a *storage.AgyTurnAttempt) {
				if len(a.UnmappedDenials) != 1 || len(a.ExecutedTools) != 0 {
					t.Fatalf("unmapped: %+v", a)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAgyHarness(t)
			h.persist(testNativeID)
			lines := append([]string{userInputDone}, tc.steps...)
			lines = append(lines, `{"denied_actions": [`+tc.deny+`]}`)
			h.turnScenario(lines...)
			if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
				t.Fatalf("dispatch: %+v err=%v", out, err)
			}
			a := h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
			if !a.VerificationIncomplete {
				t.Fatalf("every denial class flags incomplete: %+v", a)
			}
			tc.check(t, a)
		})
	}
}

func TestAgyDispatch_ResultErrorBeforeUserInputIsFailedTerminal(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"result": {"status": "ERROR", "error": "You are not signed in"}}`)

	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("after the first byte nothing is pre-acceptance: %+v err=%v", out, err)
	}
	att := h.waitAttempt("att-t1", func(a *storage.AgyTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "failed" || att.Accepted != nil {
		t.Fatalf("a result ERROR after the write is a verified FAILED terminal: %+v", att)
	}
	res, err := h.adapter.Collect(context.Background(), h.ref("t1"))
	if err != nil || res.Status != council.TurnFailed || res.ResultStatus != adapter.ResultFailed ||
		!strings.Contains(res.Output, "not signed in") {
		t.Fatalf("collect: %+v err=%v", res, err)
	}
}

func TestAgyDispatch_PrintTimeoutMarkerUncertain(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"print_timeout_marker": true}`)

	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	h.waitIdle(h.ref("t1"))
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att.Terminal || att.ObservedStatus != "uncertain" {
		t.Fatalf("print-timeout marker ⇒ Uncertain regardless of result.status: %+v", att)
	}
	out, _ := h.dispatch("t2", "p2")
	if out.Status != adapter.DispatchRejected || !strings.Contains(out.Reason, "unresolved") {
		t.Fatalf("an uncertain attempt blocks the conversation: %+v", out)
	}
}

func TestAgyDispatch_IgnoredInputMarkerUncertain(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(userInputDone, successResult("x"))
	// Rewrite the envelope on the wire so the child drops it with the
	// "ignoring unsupported stream input message event" warning.
	h.exec.setWrapProc(func(p execpolicy.ManagedProcess) execpolicy.ManagedProcess {
		return &wrappedProc{ManagedProcess: p, stdin: &rewriteWriter{w: p.Stdin(), from: `"event":"user"`, to: `"event":"usr"`}}
	})
	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	h.waitIdle(h.ref("t1"))
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att.Terminal || att.ObservedStatus != "uncertain" {
		t.Fatalf("a dropped input message is Uncertain: %+v", att)
	}
}

type rewriteWriter struct {
	w        io.WriteCloser
	from, to string
	buf      []byte
}

func (r *rewriteWriter) Write(p []byte) (int, error) {
	r.buf = append(r.buf, p...)
	return len(p), nil
}

func (r *rewriteWriter) Close() error {
	out := strings.Replace(string(r.buf), r.from, r.to, 1)
	_, _ = io.WriteString(r.w, out)
	return r.w.Close()
}

func TestAgyDispatch_ExitWithoutResultUncertain(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(userInputDone, `{"exit_without_result": true}`)

	if out, err := h.dispatch("t1", "p"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	h.waitIdle(h.ref("t1"))
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att.Terminal || att.ObservedStatus != "uncertain" || att.Accepted == nil || !*att.Accepted {
		t.Fatalf("accepted then exit without result ⇒ Uncertain: %+v", att)
	}
	res, err := h.adapter.Collect(context.Background(), h.ref("t1"))
	if err != nil || res.ResultStatus == adapter.ResultAvailable {
		t.Fatalf("no verified terminal is collectable: %+v err=%v", res, err)
	}
}

func TestAgyDispatch_SingleFlightPerConversation(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"interrupt_on_sigint": true}`)
	if out, err := h.dispatch("t1", "long"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	t.Cleanup(func() { _, _ = h.adapter.Cancel(context.Background(), h.ref("t1")) })

	out, err := h.dispatch("t2", "second")
	if out.Status != adapter.DispatchRejected || !strings.Contains(out.Reason, "busy") {
		t.Fatalf("a second dispatch is rejected (conversation busy), got %+v err=%v", out, err)
	}
	if n := h.exec.starts.Load(); n != 1 {
		t.Fatalf("the rejected dispatch starts no child, launches=%d", n)
	}
}

func TestAgyDispatch_ObserverDetachDoesNotCancelTurn(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"interrupt_on_sigint": true}`)
	if out, err := h.dispatch("t1", "long"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	// A witness tap that stays attached: every event the turn publishes
	// after the detach is ordered on it.
	witness, err := h.adapter.Observe(context.Background(), h.ref("t1"))
	if err != nil {
		t.Fatalf("Observe witness: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := h.adapter.Observe(ctx, h.ref("t1"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	// Wait for the ACTIVE step so the turn is provably running.
	for _, tap := range []adapter.Stream{s, witness} {
		select {
		case ev := <-tap.Events():
			if ev.Type != adapter.EventProgress || ev.Status != council.TurnRunning {
				t.Fatalf("first observed event: %+v", ev)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no progress observed")
		}
	}
	cancel()
	collectEvents(t, s, 5*time.Second) // the detached tap closes

	// Synchronized check (no sleep): Cancel publishes exactly one
	// "SIGINT sent" cancelling event, and only the FIRST interrupt does.
	// Had the detach cancelled or killed the turn, the witness would
	// see a cancelling/terminal event before ours, or Cancel would not
	// be Confirmed.
	co, _ := h.adapter.Cancel(context.Background(), h.ref("t1"))
	if co.Disposition != adapter.CancelConfirmed {
		t.Fatalf("cancel after detach: %+v", co)
	}
	evs := collectEvents(t, witness, 5*time.Second)
	if len(evs) != 2 || evs[0].Status != council.TurnCancelling || evs[0].Payload != "SIGINT sent" ||
		evs[1].Type != adapter.EventTerminal || evs[1].Status != council.TurnCancelled {
		t.Fatalf("after the detach the turn saw only the operator's cancel, then its terminal: %+v", evs)
	}
}

func TestAgyDispatch_StartFailureRejectedMissing(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.exec.setBeforeStart(func(execpolicy.LaunchRequest) error { return errors.New("executor refused") })
	out, err := h.dispatch("t1", "p")
	if err == nil || out.Status != adapter.DispatchRejected {
		t.Fatalf("start failure is Rejected: %+v err=%v", out, err)
	}
	att, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1")
	if att == nil || att.ObservedStatus != "missing" {
		t.Fatalf("start_failed classifies missing: %+v", att)
	}
	if st := h.launchStates("att-t1"); len(st) != 1 || st[0] != "start_failed" {
		t.Fatalf("launch state: %v", st)
	}
}

func TestLaunchArgv_ExactGrammar(t *testing.T) {
	policy := AgyLaunchPolicy{PermissionMode: "request-review", ExecutionMode: "plan", Sandbox: true,
		PrintTimeoutBackstop: 1800 * time.Second}
	logPath := "/ws/scratch/agy-logs/turn-1.log"
	good := buildLaunchArgv(LaunchTurn, "m1", policy, logPath, testNativeID)
	want := []string{"--print=", "--input-format", "stream-json", "--output-format", "stream-json",
		"--disable-slash-commands", "--model", "m1", "--print-timeout", "1800s", "--log-file", logPath,
		"--mode", "plan", "--sandbox", "--conversation", testNativeID}
	if strings.Join(good, " ") != strings.Join(want, " ") {
		t.Fatalf("frozen argv:\n got %v\nwant %v", good, want)
	}
	spec := argvSpec{kind: LaunchTurn, model: "m1", policy: policy, nativeID: testNativeID, logRoot: "/ws/scratch"}
	if err := validateLaunchArgv(good, spec); err != nil {
		t.Fatalf("exact argv must validate: %v", err)
	}
	mutations := map[string]func([]string) []string{
		"single_dash_forbidden": func(a []string) []string { return append(a, "-add-dir", "/x") },
		"extra_flag":            func(a []string) []string { return append(a, "--yolo") },
		"reordered":             func(a []string) []string { a[6], a[8] = a[8], a[6]; return a },
		"missing_sandbox": func(a []string) []string {
			out := append([]string(nil), a[:14]...)
			return append(out, a[15:]...)
		},
		"log_outside_allocation": func(a []string) []string { a[11] = "/home/op/.gemini/cli.log"; return a },
		"log_traversal":          func(a []string) []string { a[11] = "/ws/scratch/../../etc/x.log"; return a },
		"other_conversation":     func(a []string) []string { a[len(a)-1] = otherNativeID; return a },
		"wrong_model":            func(a []string) []string { a[7] = "m2"; return a },
	}
	for name, mut := range mutations {
		t.Run(name, func(t *testing.T) {
			bad := mut(append([]string(nil), good...))
			if err := validateLaunchArgv(bad, spec); err == nil {
				t.Fatalf("argv %v must be refused", bad)
			}
		})
	}
	create := buildLaunchArgv(LaunchCreate, "m1", AgyLaunchPolicy{PermissionMode: "request-review", ExecutionMode: "default",
		PrintTimeoutBackstop: 60 * time.Second}, logPath, "")
	for _, a := range create {
		if a == "--conversation" || a == "--mode" || a == "--sandbox" {
			t.Fatalf("create argv carries no --conversation, default mode omits --mode, sandbox=false omits --sandbox: %v", create)
		}
	}
	if len(create) != 12 {
		t.Fatalf("create argv carries no --conversation, default mode omits --mode, sandbox=false omits --sandbox: %v", create)
	}
}
