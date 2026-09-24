//go:build unix

package service

// AC-010 Task 8 acceptance story (Agy, provider-free): the controller
// lifecycle through the service bridge with the compiled agytest `agy`
// fixture launched by the REAL PolicyExecutor (on Linux through the
// Task 3 sealed, ptrace-verified image; off Linux through the explicit
// fixture marker). The lifecycle adapter is the fixture-scoped adapter
// (agy.NewFixtureScopedAdapter — the ONLY pre-attestation construction
// path, i.e. the agytest eligibility skip) wired into a configured
// service (AgyBinaryPath/AgyProfile/AgyEvidenceRoot/AgyHomeDir/
// AgyScratchRoot) sharing the service's AC-005 workspace manager. One
// test (Linux) instead goes through PRODUCTION construction
// (NewServerWithAdapter(…, nil) → agy.NewProductionAgyAdapter) unlocked
// by a cprot-v2 row recorded through the SERVICE operation
// RecordAgyProbeAttestation.
//
// Spec: docs/superpowers/specs/2026-09-24-ac-010-agy-adapter-design.md
// §6/§6.1 (scenarios 1–12, one focused test each, named
// TestAcceptance_Agy_S<NN>_<Slug>) as amended by §14 (binding deltas).
// The row ⇄ test map lives in
// docs/superpowers/evidence/ac010-evidence-matrix.md.
//
// Harness reuse: newAgyWireEnv / fixtureServerWith / fixtureServerSharing
// / stage / coveringAttestation (review_ac010_wiring_test.go), the
// acceptanceBridge HTTP client (acceptance_opencode_test.go).
//
// Determinism: no sleep-and-hope. In-flight turns are held by the
// fixture's `wait_for_file` gate (the child blocks after logging argv,
// BEFORE init, until the test creates the file). Every wait is a durable
// state poll with an explicit bound (agyAccDeadline); a bound is a
// failure ceiling, never a timing assumption.
//
// Fixture only: temp homes, temp evidence roots, temp state. The real
// agy binary and ~/.gemini are never touched.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/agy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const (
	// agyAccDeadline bounds every durable-state poll in this file. It is
	// a failure ceiling (the fixture turns finish in milliseconds), not a
	// timing assumption: nothing here waits for time to pass.
	agyAccDeadline = 20 * time.Second

	// agyAccCtlSession is the operator harness's OWN independent-proposal
	// session (AGENTS.md: "the current operator harness can control; its
	// independent proposal uses a separate session"). The controller is
	// adopted with harness "agy", so its proposal session is agy too.
	agyAccCtlSession = "sess-ac010-agy-ctl"
	agyAccCommit     = "0123456789012345678901234567890123456789"

	agyUserInputDone = `{"step": {"state": "DONE", "step_type": "user_input"}}`
	agyDeniedCommand = `{"denied_actions": [{"action": "command", "display_name": "RunCommand"}]}`
)

func agyKnown(id string) string { return `{"known_conversation": "` + id + `"}` }

func agyToolStep(name string) string {
	return `{"step": {"state": "DONE", "step_type": "tool", "tool_name": "` + name + `"}}`
}

func agySuccess(response string) string {
	return `{"result": {"status": "SUCCESS", "response": "` + response + `", "num_turns": 1, "usage": {"input_tokens": 3, "output_tokens": 5, "total_tokens": 8}}}`
}

// ── Harness ─────────────────────────────────────────────────────────────

type agyAcceptance struct {
	t      *testing.T
	e      *agyWireEnv
	store  *storage.Store
	w      *agyWireServer
	bridge *acceptanceBridge
	nConn  int
	nOps   atomic.Int64
}

// newAgyAcceptance builds the configured service over the fixture-scoped
// adapter (optional executor/required-tools overrides), starts the HTTP
// surface, and adopts+connects the controller over the bridge.
func newAgyAcceptance(t *testing.T, exec execpolicy.PolicyExecutor, required agy.RequiredToolsSource) *agyAcceptance {
	t.Helper()
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seedAdopted(t, store, e.profile)
	acc := &agyAcceptance{t: t, e: e, store: store}
	acc.w = e.fixtureServerWith(t, store, exec, required)
	acc.start()
	acc.connect()
	return acc
}

// start serves the bridge; cleanup models service termination (workers
// cancelled, then an orderly close).
func (acc *agyAcceptance) start() {
	acc.t.Helper()
	srv := acc.w.srv
	if err := srv.Start(); err != nil {
		acc.t.Fatalf("start: %v", err)
	}
	acc.t.Cleanup(func() {
		srv.Coordinator().CancelActiveWorkers()
		_ = srv.Close()
	})
	acc.bridge = &acceptanceBridge{t: acc.t, client: newTestClient(srv.SocketPath()), token: acc.e.cfg.AuthToken}
}

func (acc *agyAcceptance) op(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, acc.nOps.Add(1))
}

func (acc *agyAcceptance) connect() {
	acc.t.Helper()
	acc.nConn++
	code, resp := acc.bridge.do("POST", "/v1/runs/"+agyWireRunID+"/controller/connect", fmt.Sprintf(
		`{"op_id": "op-conn-acc-agy-%d", "controller_lease": %q, "expected_generation": 1, "instance_id": %q}`,
		acc.nConn, agyWireLease, acc.w.srv.InstanceID()))
	if code != http.StatusOK && code != http.StatusCreated {
		acc.t.Fatalf("connect: %d %v", code, resp)
	}
}

func (acc *agyAcceptance) disconnect() {
	acc.t.Helper()
	code, resp := acc.bridge.do("POST", "/v1/runs/"+agyWireRunID+"/controller/disconnect", fmt.Sprintf(
		`{"op_id": %q, "controller_lease": %q, "expected_generation": 1}`, acc.op("op-disc-acc-agy"), agyWireLease))
	if code != http.StatusOK && code != http.StatusCreated {
		acc.t.Fatalf("disconnect: %d %v", code, resp)
	}
}

// create births agyWireSession on the pinned native id (provider-free:
// empty stdin, init only).
func (acc *agyAcceptance) create() {
	acc.t.Helper()
	acc.w.stage(acc.t, `{"conversation_id": "`+agyWireNativeID+`"}`)
	b, _, err := acc.w.srv.CreateAgySession(context.Background(), acc.op("op-create-acc-agy"), agyWireLease, agyWireSession)
	if err != nil || b.NativeSessionID != agyWireNativeID {
		acc.t.Fatalf("create: %+v err=%v", b, err)
	}
}

func (acc *agyAcceptance) queue(session, turnKey, prompt string, tools []string) int64 {
	acc.t.Helper()
	ver, err := acc.store.GetSessionVersion(context.Background(), session)
	if err != nil {
		acc.t.Fatalf("version: %v", err)
	}
	body := map[string]any{"instance_id": acc.w.srv.InstanceID(), "op_id": acc.op("op-q-acc-agy"), "controller_lease": agyWireLease,
		"expected_version": ver, "turn_key": turnKey, "prompt": prompt}
	if tools != nil {
		body["required_tools"] = tools
	}
	raw, _ := json.Marshal(body)
	code, resp := acc.bridge.do("POST", "/v1/runs/"+agyWireRunID+"/sessions/"+session+"/prompts/queue", string(raw))
	if code != http.StatusOK && code != http.StatusCreated {
		acc.t.Fatalf("queue %s: %d %v", turnKey, code, resp)
	}
	receipt, _ := resp["receipt"].(map[string]any)
	committed, _ := receipt["committed_version"].(float64)
	return int64(committed)
}

func (acc *agyAcceptance) release(session, turnKey string, version int64) int {
	acc.t.Helper()
	code, _ := acc.bridge.do("POST", "/v1/runs/"+agyWireRunID+"/sessions/"+session+"/turns/"+turnKey+"/release",
		fmt.Sprintf(`{"op_id": %q, "controller_lease": %q, "expected_version": %d}`, acc.op("op-rel-acc-agy"), agyWireLease, version))
	return code
}

func (acc *agyAcceptance) queueRelease(turnKey, prompt string, tools []string) {
	acc.t.Helper()
	ver := acc.queue(agyWireSession, turnKey, prompt, tools)
	if code := acc.release(agyWireSession, turnKey, ver); code != http.StatusAccepted {
		acc.t.Fatalf("release %s: %d", turnKey, code)
	}
}

func (acc *agyAcceptance) collect(turnKey string) (int, map[string]any) {
	acc.t.Helper()
	return acc.bridge.do("GET", "/v1/runs/"+agyWireRunID+"/sessions/"+agyWireSession+"/turns/"+turnKey, "")
}

// waitTurn polls the DURABLE turn status (bounded) until pred holds.
func (acc *agyAcceptance) waitTurn(turnKey string, pred func(council.TurnStatus) bool) *storage.TurnDetails {
	acc.t.Helper()
	deadline := time.Now().Add(agyAccDeadline)
	for {
		d, err := acc.store.GetTurnDetails(context.Background(), agyWireSession, turnKey)
		if err == nil && d != nil && pred(d.Status) {
			return d
		}
		if time.Now().After(deadline) {
			acc.t.Fatalf("turn %s never reached the expected durable status within %v; last %+v err=%v", turnKey, agyAccDeadline, d, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitAttemptDead polls (bounded) until the attempt's single launch is
// recorded dead — the turn goroutine has classified the outcome.
func (acc *agyAcceptance) waitAttemptDead(turnKey string) *storage.AgyTurnAttempt {
	acc.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(agyAccDeadline)
	for {
		a, err := acc.store.GetLatestAgyTurnAttempt(ctx, agyWireSession, turnKey)
		if err == nil && a != nil {
			if states, _ := acc.store.AgyAttemptLaunchStates(ctx, a.AttemptID); len(states) == 1 && states[0] == "dead" {
				if a.Terminal || a.ObservedStatus != "uncertain" || acc.settled(a) {
					return a
				}
			}
		}
		if time.Now().After(deadline) {
			acc.t.Fatalf("attempt of %s never settled within %v; last %+v err=%v", turnKey, agyAccDeadline, a, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settled reports that the adapter retired the run (no live turn left).
func (acc *agyAcceptance) settled(a *storage.AgyTurnAttempt) bool {
	res, err := acc.w.adp.Collect(context.Background(), adapter.TurnRef{SessionID: agyWireSession, TurnKey: a.TurnKey})
	return err == nil && res.ResultStatus != adapter.ResultPending
}

func (acc *agyAcceptance) streamArgs() []string {
	return agyFixtureLines(acc.w.root, ".agy-fixture-args")
}
func (acc *agyAcceptance) inputs() []string { return agyFixtureLines(acc.w.root, ".agy-fixture-input") }

func agyFixtureLines(root, name string) []string {
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func agyArgvHasConversation(line, id string) bool {
	return strings.Contains(line, "\x1f--conversation\x1f"+id)
}

// restart models a service process death and restart over the same
// durable state: live workers stop (never recording a fabricated
// terminal), the server closes (storage closed), and a NEW server with
// a FRESH adapter (no in-memory run, tombstone, or auth cache) opens the
// same state dir and serves the bridge again.
func (acc *agyAcceptance) restart() {
	acc.t.Helper()
	acc.w.srv.Coordinator().CancelActiveWorkers()
	_ = acc.w.srv.Close()
	acc.store = acc.e.openStore(acc.t)
	acc.w = acc.e.fixtureServerSharing(acc.t, acc.store, acc.w)
	acc.start()
	acc.connect()
}

// liveChildrenIn counts live processes whose cwd is root (Linux /proc);
// -1 where /proc is unavailable.
func liveChildrenIn(root string) int {
	if runtime.GOOS != "linux" {
		return -1
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		return -1
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range entries {
		if cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd")); err == nil && cwd == want {
			if stat, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat")); err == nil && !strings.Contains(string(stat), ") Z ") {
				n++
			}
		}
	}
	return n
}

// requireInFlight asserts the gated turn child is alive: durably
// launched and not dead, not terminal, and (Linux) one live process in
// the allocation.
func (acc *agyAcceptance) requireInFlight(turnKey, why string) {
	acc.t.Helper()
	ctx := context.Background()
	a, err := acc.store.GetLatestAgyTurnAttempt(ctx, agyWireSession, turnKey)
	if err != nil || a == nil || a.Terminal {
		acc.t.Fatalf("%s: the attempt must exist and not be terminal, got %+v err=%v", why, a, err)
	}
	states, _ := acc.store.AgyAttemptLaunchStates(ctx, a.AttemptID)
	if len(states) != 1 || states[0] != "started" {
		acc.t.Fatalf("%s: the child must still be running (launch started, not dead), got %v", why, states)
	}
	if n := liveChildrenIn(acc.w.root); (runtime.GOOS == "linux" && n != 1) || (n != -1 && n != 1) {
		acc.t.Fatalf("%s: exactly one live fixture child in the allocation (/proc cwd scan), got %d", why, n)
	}
}

// sseEvents opens the turn's SSE stream; events are delivered on the
// returned channel until the stream ends or cancel is called.
type agySSEEvent struct{ name, data string }

func (acc *agyAcceptance) sse(turnKey string) (<-chan agySSEEvent, context.CancelFunc) {
	acc.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), agyAccDeadline)
	req, err := http.NewRequestWithContext(ctx, "GET",
		"http://localhost/v1/runs/"+agyWireRunID+"/sessions/"+agyWireSession+"/turns/"+turnKey+"/events", nil)
	if err != nil {
		cancel()
		acc.t.Fatalf("sse request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+acc.e.cfg.AuthToken)
	resp, err := acc.bridge.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		cancel()
		acc.t.Fatalf("sse: %v %v", resp, err)
	}
	out := make(chan agySSEEvent, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		r := bufio.NewReader(resp.Body)
		var ev agySSEEvent
		for {
			line, err := r.ReadString('\n')
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			case line == "" && ev.name != "":
				out <- ev
				ev = agySSEEvent{}
			}
			if err != nil {
				return
			}
		}
	}()
	return out, cancel
}

func nextSSE(t *testing.T, ch <-chan agySSEEvent) (agySSEEvent, bool) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		return ev, ok
	case <-time.After(agyAccDeadline):
		t.Fatalf("no SSE event within %v", agyAccDeadline)
		return agySSEEvent{}, false
	}
}

func isTerminalStatus(s council.TurnStatus) bool { return isTerminalTurnStatus(s) }

// ── The bridge lifecycle ────────────────────────────────────────────────

// TestAcceptance_Agy_Lifecycle drives the brief's chain through the
// service bridge: configure → create (provider-free init) → queue with
// required_tools → release → gated execution (auth + toolkit gates, the
// wait_for_file gate holds the child) → client disconnect while the
// child is in flight (the child survives, the turn completes) →
// tool_denied + incomplete verification → collect → reconnect →
// follow-up turn on the EXACT conversation id → restart with an open
// uncertainty episode → controller resolution → the next birth and a
// further turn proceed. Ordering note: the disconnect is taken while
// turn 1 is IN FLIGHT (the strongest form of "the process survives and
// the turn completes"); collect follows the completion.
func TestAcceptance_Agy_Lifecycle(t *testing.T) {
	acc := newAgyAcceptance(t, nil, nil)
	ctx := context.Background()
	w := acc.w

	// 1. Configure: the service is wired to the frozen cprof-v4 profile
	// (AgyBinaryPath is the pinned fixture binary, the run is frozen on
	// the same profile digest); nothing has launched yet.
	if n := len(acc.streamArgs()); n != 0 {
		t.Fatalf("configuration launches nothing, stream launches=%d", n)
	}

	// 2. Create: provider-free — one stream-json child with EMPTY stdin
	// and no --conversation; no prompt, no models gate.
	w.stage(t, `{"conversation_id": "`+agyWireNativeID+`"}`)
	binding, receipt, err := w.srv.CreateAgySession(ctx, "op-create-acc-agy-life", agyWireLease, agyWireSession)
	if err != nil || binding.NativeSessionID != agyWireNativeID || receipt.CommandType != "bind_agy_session" {
		t.Fatalf("create: %+v %+v err=%v", binding, receipt, err)
	}
	args := acc.streamArgs()
	if len(args) != 1 || strings.Contains(args[0], "--conversation") || !strings.Contains(args[0], "--print=") {
		t.Fatalf("creation is one stream-json launch without --conversation, got %q", args)
	}
	if in := acc.inputs(); len(in) != 0 {
		t.Fatalf("creation transmits nothing on stdin, got %q", in)
	}
	for _, g := range agyFixtureLines(w.root, ".agy-fixture-gate-args") {
		if strings.HasPrefix(g, "models") {
			t.Fatalf("creation is provider-free: no models (auth) gate may run, got %q", g)
		}
	}

	// 3+4. Queue with required_tools, release; the turn child is HELD at
	// the wait_for_file gate after the auth + toolkit gates passed.
	w.stage(t, agyKnown(agyWireNativeID), `{"wait_for_file": "gate"}`, agyUserInputDone,
		agyToolStep("view_file"), agyDeniedCommand)
	acc.queueRelease("t-acc-1", "review the change", []string{"write_to_file", "view_file"})
	w.waitCreationStarted(t, 2)
	var sawModels bool
	for _, g := range agyFixtureLines(w.root, ".agy-fixture-gate-args") {
		sawModels = sawModels || strings.HasPrefix(g, "models")
	}
	if !sawModels {
		t.Fatal("the provider-free models auth gate must run before the first turn child")
	}
	acc.requireInFlight("t-acc-1", "gated")

	// 5. Client disconnect while the child is in flight: never a
	// cancellation (AGENTS.md). The child keeps running.
	acc.disconnect()
	acc.requireInFlight("t-acc-1", "after the controller disconnect")
	w.openGate(t)
	acc.waitTurn("t-acc-1", isTerminalStatus)
	w.srv.Coordinator().WaitWorkers()

	// 6. tool_denied + incomplete verification; exit 0 does not verify.
	att, err := acc.store.GetLatestAgyTurnAttempt(ctx, agyWireSession, "t-acc-1")
	if err != nil || att == nil || !att.Terminal || att.ObservedStatus != "completed" || !att.VerificationIncomplete {
		t.Fatalf("SUCCESS with a native denial is completed AND verification_incomplete, got %+v err=%v", att, err)
	}
	if strings.Join(att.RequiredTools, ",") != "write_to_file,view_file" ||
		strings.Join(att.ExecutedTools, ",") != "view_file" ||
		strings.Join(att.MissingRequiredTools, ",") != "write_to_file" {
		t.Fatalf("queue-time required_tools verified against executed steps, got required=%v executed=%v missing=%v",
			att.RequiredTools, att.ExecutedTools, att.MissingRequiredTools)
	}
	if n := len(att.DeniedTools) + len(att.AmbiguousDenials) + len(att.UnattributedDenials) + len(att.UnmappedDenials); n != 1 {
		t.Fatalf("the one denied_actions entry is classified exactly once, got %+v", att)
	}
	obs, err := w.adp.Observe(ctx, adapter.TurnRef{SessionID: agyWireSession, TurnKey: "t-acc-1"})
	if err != nil {
		t.Fatalf("observer re-attach: %v", err)
	}
	var denied int
	for ev := range obs.Events() {
		if ev.Type == adapter.EventToolDenied {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("one tool_denied event for the denial, got %d", denied)
	}

	// 7. Collect through the bridge: the durable completed terminal.
	code, resp := acc.collect("t-acc-1")
	if code != http.StatusOK || resp["status"] != string(council.TurnCompleted) {
		t.Fatalf("collect: %d %v", code, resp)
	}
	if in := acc.inputs(); len(in) != 1 || !strings.Contains(in[0], "review the change") {
		t.Fatalf("exactly one prompt line was transmitted, got %q", in)
	}
	if args := acc.streamArgs(); len(args) != 2 || !agyArgvHasConversation(args[1], agyWireNativeID) {
		t.Fatalf("turn 1 addressed the exact conversation, got %q", args)
	}

	// 8. Reconnect; 9. follow-up on the EXACT conversation id.
	acc.connect()
	w.stage(t, agyKnown(agyWireNativeID), agyUserInputDone, agySuccess("follow-up done"))
	acc.queueRelease("t-acc-2", "follow up", nil)
	acc.waitTurn("t-acc-2", isTerminalStatus)
	w.srv.Coordinator().WaitWorkers()
	if code, resp := acc.collect("t-acc-2"); code != http.StatusOK || resp["status"] != string(council.TurnCompleted) ||
		!strings.Contains(fmt.Sprint(resp["result"]), "follow-up done") {
		t.Fatalf("follow-up collect: %d %v", code, resp)
	}
	args = acc.streamArgs()
	if len(args) != 3 || !agyArgvHasConversation(args[2], agyWireNativeID) {
		t.Fatalf("the follow-up is a NEW process on the exact conversation (--conversation %s), got %q", agyWireNativeID, args)
	}
	if b, _ := acc.store.GetAgySessionBinding(ctx, agyWireSession); b == nil || b.NativeID != agyWireNativeID {
		t.Fatalf("the binding never changes, got %+v", b)
	}

	// 10. An open uncertainty episode on the controller's own proposal
	// session: its creation reports a non-UUID id (Uncertain).
	if _, err := acc.store.CreateSession(ctx, "op-sess-ctl-acc-agy", agyWireLease, storage.SessionRecord{
		ID: agyAccCtlSession, RunID: agyWireRunID, Contributor: "agy", Role: "controller-proposal",
		IsActiveContributor: false, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("controller proposal session: %v", err)
	}
	ctlPaths, err := w.wm.AllocateWorkspace(agyWireRunID, agyAccCtlSession, acc.e.profile.WorkspaceMode, "example/repo", agyAccCommit)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	ctlStage := func(lines ...string) {
		t.Helper()
		tools, _ := json.Marshal(map[string]any{"tools": agyWireTools})
		all := append([]string{string(tools)}, lines...)
		if err := os.WriteFile(filepath.Join(ctlPaths.Root, ".agy-fixture-scenario.jsonl"), []byte(strings.Join(all, "\n")+"\n"), 0o600); err != nil {
			t.Fatalf("stage: %v", err)
		}
	}
	ctlStage(`{"conversation_id": "not-a-uuid"}`)
	var unc *adapter.ErrSessionCreationUncertain
	if _, _, err := w.srv.CreateAgySession(ctx, "op-create-ctl-unc", agyWireLease, agyAccCtlSession); !errors.As(err, &unc) {
		t.Fatalf("a non-UUID creation id is Uncertain, got %T: %v", err, err)
	}
	ep, err := acc.store.OpenAgyCreationUncertainty(ctx, agyAccCtlSession)
	if err != nil || ep == nil || ep.OrphanNativeID == nil || *ep.OrphanNativeID != "not-a-uuid" {
		t.Fatalf("the durable episode carries the observed id, got %+v err=%v", ep, err)
	}

	// 11. Restart with the episode open.
	acc.restart()
	w = acc.w
	for _, key := range []string{"t-acc-1", "t-acc-2"} {
		if d, err := acc.store.GetTurnDetails(ctx, agyWireSession, key); err != nil || d == nil || d.Status != council.TurnCompleted {
			t.Fatalf("turn %s stays durably completed across the restart, got %+v err=%v", key, d, err)
		}
	}
	ctlStage(`{"conversation_id": "` + agyWireOtherID + `"}`)
	if _, _, err := w.srv.CreateAgySession(ctx, "op-create-ctl-after-restart", agyWireLease, agyAccCtlSession); !errors.As(err, &unc) {
		t.Fatalf("the open episode blocks the restarted service, got %T: %v", err, err)
	}
	if n := len(agyFixtureLines(ctlPaths.Root, ".agy-fixture-args")); n != 1 {
		t.Fatalf("no creation child while the episode is open, launches=%d", n)
	}

	// 12. Resolution by the connected controller, then the birth proceeds.
	if _, err := w.srv.ResolveAgySessionCreationUncertainty(ctx, AgyCreationUncertaintyResolution{
		OpID: "op-resolve-ctl-acc-agy", ControllerLease: agyWireLease, ExpectedGeneration: 1,
		SessionID: agyAccCtlSession, Episode: ep.Episode, Disposition: storage.AgyUncertaintyAbandonOrphan,
		Reason: "non-UUID creation id observed; the orphan is abandoned (purge is a separate operator action)",
	}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if b, _, err := w.srv.CreateAgySession(ctx, "op-create-ctl-after-resolve", agyWireLease, agyAccCtlSession); err != nil ||
		b.NativeSessionID != agyWireOtherID {
		t.Fatalf("after resolution the birth proceeds, got %+v err=%v", b, err)
	}

	// 13. The restarted service (fresh adapter, auth gate re-run) serves
	// the next turn on the same exact conversation.
	w.stage(t, agyKnown(agyWireNativeID), agyUserInputDone, agySuccess("after restart"))
	acc.queueRelease("t-acc-3", "after restart", nil)
	acc.waitTurn("t-acc-3", isTerminalStatus)
	w.srv.Coordinator().WaitWorkers()
	if code, resp := acc.collect("t-acc-3"); code != http.StatusOK || resp["status"] != string(council.TurnCompleted) {
		t.Fatalf("post-restart collect: %d %v", code, resp)
	}
	if args := acc.streamArgs(); len(args) != 4 || !agyArgvHasConversation(args[3], agyWireNativeID) {
		t.Fatalf("the post-restart turn addressed the exact conversation, got %q", args)
	}
}

// ── Production construction through the service-recorded attestation ──

// newAgyProductionServer records the covering cprot-v2 attestation
// through the SERVICE operation (RecordAgyProbeAttestation on a
// configured fixture-scoped instance — see the evidence matrix note: a
// production-configured service cannot be constructed before the first
// row exists), then builds the PRODUCTION service from configuration
// alone (NewServerWithAdapter(…, nil)).
func newAgyProductionAcceptance(t *testing.T, attest bool) (*agyAcceptance, error) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("production agy construction is Linux-only (spec §3.12)")
	}
	e := newAgyWireEnv(t, nil)
	store := e.openStore(t)
	e.seedAdopted(t, store, e.profile)
	if attest {
		rec := e.fixtureServer(t, store)
		att := e.coveringAttestation(t, "operator-acc")
		id, err := att.Digest()
		if err != nil {
			t.Fatalf("digest: %v", err)
		}
		receipt, err := rec.srv.RecordAgyProbeAttestation(context.Background(), AgyProbeAttestationRequest{
			OpID: "op-att-acc-agy", OperatorToken: e.cfg.AuthToken, Actor: "operator-acc",
			RunID: agyWireRunID, AttestationID: id, Attestation: att,
		})
		if err != nil || receipt.Payload != id {
			t.Fatalf("service-recorded attestation: %+v err=%v", receipt, err)
		}
		rec.srv.lock.Release()
	}
	srv, err := NewServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, nil)
	if err != nil {
		return &agyAcceptance{t: t, e: e, store: store}, err
	}
	adp, ok := srv.adapter.(*agy.AgyAdapter)
	if !ok {
		t.Fatalf("the production agy adapter must be wired, got %T", srv.adapter)
	}
	paths, err := srv.WorkspaceManager().AllocateWorkspace(agyWireRunID, agyWireSession, e.profile.WorkspaceMode, "example/repo", agyAccCommit)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	acc := &agyAcceptance{t: t, e: e, store: store,
		w: &agyWireServer{srv: srv, adp: adp, wm: srv.WorkspaceManager(), root: paths.Root}}
	acc.start()
	acc.connect()
	return acc, nil
}

// The production path end to end on Linux: the row recorded by the
// service operation unlocks NewProductionAgyAdapter (the construction
// `plugin list` ran), and a birth + queued turn run through the
// production adapter: the sealed fixture child inherits HOME = the
// parent of expected_home (§14.2), the attempt identity comes from the
// journaled dispatch intent, and the queued required_tools verify.
func TestAcceptance_Agy_ProductionConstructionUnlockedByServiceRecordedAttestation(t *testing.T) {
	acc, err := newAgyProductionAcceptance(t, true)
	if err != nil {
		t.Fatalf("production construction after the service-recorded attestation: %v", err)
	}
	if !constructionProbeRan(acc.e.scratch) {
		t.Fatal("the construction-scoped plugin list must have run")
	}
	acc.create()
	acc.w.stage(t, agyKnown(agyWireNativeID), agyUserInputDone, agyToolStep("view_file"), agySuccess("prod ok"))
	acc.queueRelease("t-prod-1", "production turn", []string{"view_file"})
	acc.waitTurn("t-prod-1", isTerminalStatus)
	acc.w.srv.Coordinator().WaitWorkers()
	if code, resp := acc.collect("t-prod-1"); code != http.StatusOK || resp["status"] != string(council.TurnCompleted) {
		t.Fatalf("production collect: %d %v", code, resp)
	}
	att, err := acc.store.GetLatestAgyTurnAttempt(context.Background(), agyWireSession, "t-prod-1")
	if err != nil || att == nil || !att.Terminal || att.VerificationIncomplete || strings.Join(att.ExecutedTools, ",") != "view_file" {
		t.Fatalf("the production attempt verifies the queued set, got %+v err=%v", att, err)
	}
	if strings.HasPrefix(att.AttemptID, "att-") {
		t.Fatalf("production attempt identity comes from the dispatch intent, not the fixture identity, got %q", att.AttemptID)
	}
	wantHome := "HOME=" + filepath.Dir(acc.e.home)
	for _, l := range agyFixtureLines(acc.w.root, ".agy-fixture-env") {
		if l != wantHome {
			t.Fatalf("every sealed launch inherits the operator home parent %q, got %q", wantHome, l)
		}
	}
}

// ── §6.1 scenarios ──────────────────────────────────────────────────────

// §6.1 row 1 — Concurrent duplicate dispatch: one process, shared
// verdict. Four concurrent releases of one queued turn: exactly one is
// accepted, exactly one turn child runs, and every observer collects the
// same terminal. (Adapter single flight per conversation:
// TestAgyDispatch_SingleFlightPerConversation.)
func TestAcceptance_Agy_S01_ConcurrentDuplicateDispatch(t *testing.T) {
	acc := newAgyAcceptance(t, nil, nil)
	acc.create()
	acc.w.stage(t, agyKnown(agyWireNativeID), agyUserInputDone, agySuccess("shared verdict"))
	ver := acc.queue(agyWireSession, "t-dup", "duplicate", nil)
	const n = 4
	var mu sync.Mutex
	var wg sync.WaitGroup
	accepted := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code := acc.release(agyWireSession, "t-dup", ver); code == http.StatusAccepted {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("exactly one release is accepted, got %d", accepted)
	}
	acc.waitTurn("t-dup", isTerminalStatus)
	acc.w.srv.Coordinator().WaitWorkers()
	if args := acc.streamArgs(); len(args) != 2 {
		t.Fatalf("one creation + ONE turn process, got %d launches", len(args))
	}
	if in := acc.inputs(); len(in) != 1 {
		t.Fatalf("the prompt is transmitted once, got %d", len(in))
	}
	var verdicts []string
	for i := 0; i < n; i++ {
		code, resp := acc.collect("t-dup")
		if code != http.StatusOK {
			t.Fatalf("collect: %d %v", code, resp)
		}
		verdicts = append(verdicts, fmt.Sprint(resp["status"], "|", resp["result"]))
	}
	for _, v := range verdicts {
		if v != verdicts[0] || !strings.HasPrefix(v, string(council.TurnCompleted)+"|shared verdict") {
			t.Fatalf("every collector shares one verdict, got %q", verdicts)
		}
	}
}

// §6.1 row 2 — Crash after the stdin write, result lost: Uncertain; the
// block persists across restart; a controller disposition is required.
// The fixture reads the prompt and exits without any result (the result
// is lost to Council); the service then dies and restarts. No
// fabricated terminal exists before or after the restart, the restarted
// adapter reconciles Uncertain, the conversation stays durably blocked,
// and the next turn cannot be released or transmitted. The disposition
// operation itself is NOT a shipped service surface (see the evidence
// matrix "unresolved gaps"); S03 exercises what happens after one.
// Crash-gap durability per boundary: TestAgyCrashGap_* (reconcile_test.go).
func TestAcceptance_Agy_S02_CrashAfterStdinWriteResultLost(t *testing.T) {
	acc := newAgyAcceptance(t, nil, nil)
	acc.create()
	ctx := context.Background()
	acc.w.stage(t, agyKnown(agyWireNativeID), `{"exit_without_result": true}`)
	acc.queueRelease("t-lost", "prompt whose result is lost", nil)
	att := acc.waitAttemptDead("t-lost")
	if att.Terminal || att.ObservedStatus != "uncertain" {
		t.Fatalf("a lost result is Uncertain, got %+v", att)
	}
	acc.requireUncertainReason("t-lost", "process exited without a result")
	if in := acc.inputs(); len(in) != 1 {
		t.Fatalf("the prompt crossed stdin before the loss, got %q", in)
	}
	if d, _ := acc.store.GetTurnDetails(ctx, agyWireSession, "t-lost"); d == nil || isTerminalStatus(d.Status) {
		t.Fatalf("no fabricated terminal before the restart, got %+v", d)
	}

	acc.restart()
	if d, _ := acc.store.GetTurnDetails(ctx, agyWireSession, "t-lost"); d == nil || isTerminalStatus(d.Status) {
		t.Fatalf("no fabricated terminal after the restart, got %+v", d)
	}
	if blocked, err := acc.store.HasAgyUnresolvedAttempts(ctx, agyWireNativeID); err != nil || !blocked {
		t.Fatalf("the conversation stays durably blocked across the restart, got %v err=%v", blocked, err)
	}
	out, err := acc.w.adp.Reconcile(ctx, adapter.RecoveryRef{TurnRef: adapter.TurnRef{SessionID: agyWireSession, TurnKey: "t-lost"}, Generation: 1})
	if err != nil || out.Status != adapter.ReconciliationUncertain {
		t.Fatalf("the restarted adapter reconciles Uncertain (evidence never invents an outcome), got %+v err=%v", out, err)
	}
	ver := acc.queue(agyWireSession, "t-after", "must never transmit", nil)
	if code := acc.release(agyWireSession, "t-after", ver); code == http.StatusAccepted {
		t.Fatal("the next turn cannot be released while the lost turn is unresolved")
	}
	if args := acc.streamArgs(); len(args) != 2 {
		t.Fatalf("no process for the blocked turn, launches=%d", len(args))
	}
}

// agyDefaultsRequired is the RequiredToolsSource for turns dispatched
// directly on the adapter (no service dispatch intent): the frozen
// defaults apply.
type agyDefaultsRequired struct{}

func (agyDefaultsRequired) RequiredToolsFor(context.Context, adapter.TurnRef) ([]string, bool, error) {
	return nil, false, nil
}

// §6.1 row 3 — Process death mid-turn: the lost attempt is Uncertain and
// BLOCKS the conversation; only after a controller disposition does the
// next turn start, as a NEW process on the SAME conversation after init
// equality. The turn is accepted (user_input DONE) and the child dies
// without a result. Because no controller-disposition operation for
// turn attempts is shipped (gap recorded in the evidence matrix), the
// disposition is written to the attempt row directly — the ONE place in
// this file that edits storage by hand, labelled as the missing surface.
// The next turn is dispatched on the adapter over the service's store.
func TestAcceptance_Agy_S03_ProcessDeathBlocksUntilDisposition(t *testing.T) {
	acc := newAgyAcceptance(t, nil, agyDefaultsRequired{})
	acc.create()
	ctx := context.Background()
	acc.w.stage(t, agyKnown(agyWireNativeID), agyUserInputDone, `{"exit_without_result": true}`)
	acc.queueRelease("t-dead", "the child dies mid-turn", nil)
	att := acc.waitAttemptDead("t-dead")
	if att.Terminal || att.ObservedStatus != "uncertain" || att.Accepted == nil || !*att.Accepted {
		t.Fatalf("accepted then process death ⇒ Uncertain, got %+v", att)
	}
	acc.requireUncertainReason("t-dead", "process exited without a result")
	next := adapter.TurnRef{SessionID: agyWireSession, TurnKey: "t-next"}
	out, _ := acc.w.adp.Dispatch(ctx, next, "next prompt")
	if out.Status != adapter.DispatchRejected || !strings.Contains(out.Reason, "unresolved attempt") {
		t.Fatalf("the Uncertain attempt blocks the conversation, got %+v", out)
	}
	if args := acc.streamArgs(); len(args) != 2 {
		t.Fatalf("no process while blocked, launches=%d", len(args))
	}

	// Controller disposition (missing surface — see the doc comment).
	if _, err := acc.store.DB().Exec(`UPDATE agy_turn_attempts SET uncertainty_disposition = 'abandoned' WHERE attempt_id = ?`, att.AttemptID); err != nil {
		t.Fatalf("disposition: %v", err)
	}
	acc.w.stage(t, agyKnown(agyWireNativeID), agyUserInputDone, agySuccess("after disposition"))
	if out, err := acc.w.adp.Dispatch(ctx, next, "next prompt"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("after the disposition the next turn starts, got %+v err=%v", out, err)
	}
	a := acc.waitAttemptDead("t-next")
	if !a.Terminal || a.ObservedStatus != "completed" {
		t.Fatalf("the next turn completes, got %+v", a)
	}
	args := acc.streamArgs()
	if len(args) != 3 || !agyArgvHasConversation(args[2], agyWireNativeID) {
		t.Fatalf("the next turn is a NEW process on the SAME conversation, got %q", args)
	}
	if in := acc.inputs(); len(in) != 2 || !strings.Contains(in[1], "next prompt") {
		t.Fatalf("the next prompt was written only after init equality, got %q", in)
	}
}

// §6.1 row 4 — Controller disconnect: the turn continues; the observer
// re-attaches. An SSE observer attached to the in-flight (gated) turn
// goes away (client disconnect) together with the controller; the child
// keeps running; the turn completes durably; after reconnect a new
// observer gets the durable terminal. (Adapter-level observer detach:
// TestAgyDispatch_ObserverDetachDoesNotCancelTurn.)
func TestAcceptance_Agy_S04_ControllerDisconnectObserverReattaches(t *testing.T) {
	acc := newAgyAcceptance(t, nil, nil)
	acc.create()
	acc.w.stage(t, agyKnown(agyWireNativeID), `{"wait_for_file": "gate"}`, agyUserInputDone, agySuccess("survived"))
	acc.queueRelease("t-disc", "keep going", nil)
	acc.w.waitCreationStarted(t, 2)

	events, cancelObserver := acc.sse("t-disc")
	if ev, ok := nextSSE(t, events); !ok || ev.name != "snapshot" {
		t.Fatalf("the observer attaches with a snapshot, got %+v ok=%v", ev, ok)
	}
	cancelObserver() // the observer's client goes away
	acc.disconnect() // and so does the controller
	acc.requireInFlight("t-disc", "after the observer and controller disconnect")

	acc.w.openGate(t)
	acc.waitTurn("t-disc", isTerminalStatus)
	acc.w.srv.Coordinator().WaitWorkers()
	acc.connect()
	events, cancelObserver = acc.sse("t-disc")
	defer cancelObserver()
	var terminal agySSEEvent
	for {
		ev, ok := nextSSE(t, events)
		if !ok {
			break
		}
		if ev.name == "terminal" {
			terminal = ev
		}
	}
	if !strings.Contains(terminal.data, `"status":"completed"`) || !strings.Contains(terminal.data, "survived") {
		t.Fatalf("the re-attached observer gets the durable completed terminal, got %+v", terminal)
	}
}

// §6.1 row 5 — Exact identity: an absent/malformed id ⇒
// ErrConversationDrift, the prompt is never written, the orphan is
// recorded; a non-UUID is never transmitted. (a) the 1.2.9 silent
// fallback to a NEW conversation (known id absent), (b) a malformed id
// reported by init, (c) a non-UUID native id cannot even be bound (the
// schema CHECK), so no --conversation value can be non-UUID. Creation-
// side non-UUID ⇒ Uncertain episode: the Lifecycle test step 10 and
// TestServiceAgySession_UncertainCreationEpisodeAndResolution.
func TestAcceptance_Agy_S05_ExactIdentity(t *testing.T) {
	acc := newAgyAcceptance(t, nil, nil)
	acc.create()
	ctx := context.Background()
	for i, tc := range []struct{ key, reported string }{
		{"t-fallback", agyWireOtherID},
		{"t-malformed", "not-a-uuid"},
	} {
		// No known_conversation: the fixture reports `reported` instead.
		acc.w.stage(t, `{"conversation_id": "`+tc.reported+`"}`, agyUserInputDone, agySuccess("must not run"))
		acc.queueRelease(tc.key, "never written", nil)
		acc.waitTurn(tc.key, isTerminalStatus)
		acc.w.srv.Coordinator().WaitWorkers()
		code, resp := acc.collect(tc.key)
		if code != http.StatusOK || resp["status"] != string(council.TurnFailed) ||
			!strings.Contains(fmt.Sprint(resp["result"]), "agy conversation drift") {
			t.Fatalf("%s: typed conversation drift fails the turn, got %d %v", tc.key, code, resp)
		}
		if in := acc.inputs(); len(in) != 0 {
			t.Fatalf("%s: the prompt is never written, got %q", tc.key, in)
		}
		a, err := acc.store.GetLatestAgyTurnAttempt(ctx, agyWireSession, tc.key)
		if err != nil || a == nil || a.OrphanConversationID == nil || *a.OrphanConversationID != tc.reported || a.ObservedStatus != "missing" {
			t.Fatalf("%s: the orphan id is durable, the attempt positively missing, got %+v err=%v", tc.key, a, err)
		}
		if args := acc.streamArgs(); len(args) != 2+i || !agyArgvHasConversation(args[1+i], agyWireNativeID) {
			t.Fatalf("%s: only the bound UUID is ever passed as --conversation, got %q", tc.key, args)
		}
	}
	if b, _ := acc.store.GetAgySessionBinding(ctx, agyWireSession); b == nil || b.NativeID != agyWireNativeID {
		t.Fatalf("an orphan is never bound, got %+v", b)
	}
	if _, err := acc.store.CreateSession(ctx, "op-sess-nonuuid", agyWireLease, storage.SessionRecord{
		ID: "sess-ac010-nonuuid", RunID: agyWireRunID, Contributor: "agy", Role: "reviewer",
		State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := acc.store.InsertAgySessionBinding(ctx, storage.AgySessionBinding{SessionID: "sess-ac010-nonuuid",
		NativeID: "not-a-uuid", Model: agyWireModel, Workspace: acc.w.root, ProfileDigest: acc.e.digest, CreatedAt: time.Now()}); err == nil ||
		!strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("a non-UUID native id can never be bound, got %v", err)
	}
}

// §6.1 row 6 — Permission-requiring tool: auto-denied natively; a
// tool_denied event; verification_incomplete; exit 0 does not mark the
// turn verified. The observer is attached over SSE before the gated
// child runs; the denial arrives as a live progress event. (Denial
// classes: TestAgyDispatch_DenialClassesRecorded; attributed denial:
// TestAgyDispatch_DeniedToolEmitsToolDeniedAndIncomplete — the frozen
// wire profile has no command-class tool, so this denial is recorded in
// the unattributed class.)
func TestAcceptance_Agy_S06_PermissionToolAutoDeniedIncomplete(t *testing.T) {
	acc := newAgyAcceptance(t, nil, nil)
	acc.create()
	ctx := context.Background()
	acc.w.stage(t, agyKnown(agyWireNativeID), `{"wait_for_file": "gate"}`, agyUserInputDone, agyToolStep("view_file"), agyDeniedCommand)
	acc.queueRelease("t-denied", "run a command", []string{"view_file"})
	acc.w.waitCreationStarted(t, 2)
	events, cancel := acc.sse("t-denied")
	defer cancel()
	acc.w.openGate(t)
	var progress []string
	var terminal string
	for {
		ev, ok := nextSSE(t, events)
		if !ok {
			break
		}
		switch ev.name {
		case "progress":
			progress = append(progress, ev.data)
		case "terminal":
			terminal = ev.data
		}
	}
	var sawDenied bool
	for _, p := range progress {
		sawDenied = sawDenied || (strings.Contains(p, `"tool_denied"`) && strings.Contains(p, "tool denied:"))
	}
	if !sawDenied {
		t.Fatalf("the native denial reaches the observer as a tool_denied event, progress=%q", progress)
	}
	if !strings.Contains(terminal, `"status":"completed"`) {
		t.Fatalf("the turn completes (SUCCESS, exit 0), got terminal %q", terminal)
	}
	a, err := acc.store.GetLatestAgyTurnAttempt(ctx, agyWireSession, "t-denied")
	if err != nil || a == nil || !a.Terminal || !a.VerificationIncomplete || len(a.MissingRequiredTools) != 0 ||
		strings.Join(a.ExecutedTools, ",") != "view_file" || len(a.UnattributedDenials)+len(a.DeniedTools)+len(a.AmbiguousDenials) != 1 {
		t.Fatalf("every required tool ran, yet the denial alone keeps verification incomplete, got %+v err=%v", a, err)
	}
	var exit int
	if err := acc.store.DB().QueryRow(`SELECT exit_code FROM agy_attempt_launches WHERE attempt_id = ?`, a.AttemptID).Scan(&exit); err != nil || exit != 0 {
		t.Fatalf("the child exited 0 (exit codes never classify), got %d err=%v", exit, err)
	}
	res, err := acc.w.adp.Collect(ctx, adapter.TurnRef{SessionID: agyWireSession, TurnKey: "t-denied"})
	if err != nil || !strings.Contains(string(res.RawEvidence), `"verification_incomplete":true`) {
		t.Fatalf("collected raw evidence carries verification_incomplete, got %s err=%v", res.RawEvidence, err)
	}
}

// agyRequireFailedPreWrite asserts a pre-transmission failure: the turn
// failed with the typed reason, no prompt byte was written, the attempt
// is positively missing, and the conversation is not blocked.
func (acc *agyAcceptance) requireFailedPreWrite(turnKey, reason string) {
	acc.t.Helper()
	acc.waitTurn(turnKey, isTerminalStatus)
	acc.w.srv.Coordinator().WaitWorkers()
	code, resp := acc.collect(turnKey)
	if code != http.StatusOK || resp["status"] != string(council.TurnFailed) || !strings.Contains(fmt.Sprint(resp["result"]), reason) {
		acc.t.Fatalf("%s: want a failed turn naming %q, got %d %v", turnKey, reason, code, resp)
	}
	if in := acc.inputs(); len(in) != 0 {
		acc.t.Fatalf("%s: nothing is transmitted before verification, got %q", turnKey, in)
	}
	ctx := context.Background()
	if a, _ := acc.store.GetLatestAgyTurnAttempt(ctx, agyWireSession, turnKey); a == nil || a.ObservedStatus != "missing" {
		acc.t.Fatalf("%s: the pre-write rejection is positive evidence (missing), got %+v", turnKey, a)
	}
	if blocked, _ := acc.store.HasAgyUnresolvedAttempts(ctx, agyWireNativeID); blocked {
		acc.t.Fatalf("%s: a pre-write rejection never blocks the conversation", turnKey)
	}
}

// §6.1 row 7 — Config drift: an init.permission_mode/model/cwd mismatch
// fails before transmission. The fixture can drift permission_mode
// (model and cwd are echoed from argv/cwd); the per-field matrix
// including model and cwd: TestAgyDispatch_ProfileAndToolDriftRejectedPreWrite
// and TestAgyDispatch_EmptyInitFieldsAreDriftPreWrite.
func TestAcceptance_Agy_S07_ConfigDriftFailsBeforeTransmission(t *testing.T) {
	acc := newAgyAcceptance(t, nil, nil)
	acc.create()
	acc.w.stage(t, agyKnown(agyWireNativeID), `{"permission_mode": "always-proceed"}`, agyUserInputDone, agySuccess("x"))
	acc.queueRelease("t-cfg", "never written", nil)
	acc.requireFailedPreWrite("t-cfg", "agy profile drift on permission_mode")
}

// §6.1 row 8 — Tool inventory drift: init.tools ≠ frozen ⇒ failure
// before transmission.
func TestAcceptance_Agy_S08_ToolInventoryDriftFailsBeforeTransmission(t *testing.T) {
	acc := newAgyAcceptance(t, nil, nil)
	acc.create()
	acc.w.stage(t, agyKnown(agyWireNativeID), `{"tools": ["view_file", "write_to_file", "run_command"]}`, agyUserInputDone, agySuccess("x"))
	acc.queueRelease("t-tools", "never written", nil)
	acc.requireFailedPreWrite("t-tools", "agy tool inventory drift")
}

// requireUncertainReason re-attaches an observer to the finished run
// and asserts the in-process classification reason the stream closed
// with (the durable row only says "uncertain").
func (acc *agyAcceptance) requireUncertainReason(turnKey, reason string) {
	acc.t.Helper()
	obs, err := acc.w.adp.Observe(context.Background(), adapter.TurnRef{SessionID: agyWireSession, TurnKey: turnKey})
	if err != nil {
		acc.t.Fatalf("observe %s: %v", turnKey, err)
	}
	for range obs.Events() {
	}
	if obs.Err() == nil || !strings.Contains(obs.Err().Error(), reason) {
		acc.t.Fatalf("%s: the run is classified Uncertain because of %q, got %v", turnKey, reason, obs.Err())
	}
}

// §6.1 row 9 — Print-timeout marker: Uncertain even with a result
// present. The fixture emits the stderr marker AND a result; the attempt
// stays Uncertain (never terminal), the service records no terminal, and
// the conversation is blocked.
func TestAcceptance_Agy_S09_PrintTimeoutMarkerUncertain(t *testing.T) {
	acc := newAgyAcceptance(t, nil, nil)
	acc.create()
	ctx := context.Background()
	acc.w.stage(t, agyKnown(agyWireNativeID), agyUserInputDone, `{"print_timeout_marker": true}`)
	acc.queueRelease("t-timeout", "partial output", nil)
	a := acc.waitAttemptDead("t-timeout")
	if a.Terminal || a.ObservedStatus != "uncertain" {
		t.Fatalf("the print-timeout marker is Uncertain regardless of the result, got %+v", a)
	}
	acc.requireUncertainReason("t-timeout", "stderr print-timeout marker")
	if d, _ := acc.store.GetTurnDetails(ctx, agyWireSession, "t-timeout"); d == nil || isTerminalStatus(d.Status) {
		t.Fatalf("no terminal is recorded for the marked turn, got %+v", d)
	}
	if blocked, _ := acc.store.HasAgyUnresolvedAttempts(ctx, agyWireNativeID); !blocked {
		t.Fatal("the Uncertain attempt blocks the conversation")
	}
}

// §6.1 row 10 — Binary drift: a digest mismatch ⇒ no process started.
// The pinned path now holds different bytes (the native self-update
// hazard): production construction refuses at the sealed-image digest
// check, before eligibility, the construction `plugin list`, or any
// child. (Seal/exec-stop mismatch matrix: sealed_linux_test.go;
// per-launch image checks: TestAgyValidateLaunch_TiedToAllocationImageAndHome.)
func TestAcceptance_Agy_S10_BinaryDriftStartsNoProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production agy construction is Linux-only (spec §3.12)")
	}
	tampered := filepath.Join(t.TempDir(), "agy")
	raw, err := os.ReadFile(agyWireFixture(t).BinaryPath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(tampered, append(raw, 0x00), 0o700); err != nil {
		t.Fatalf("write tampered: %v", err)
	}
	e := newAgyWireEnv(t, func(p *storage.CanonicalProfile) { p.Harnesses["agy"].Agy.BinaryPath = tampered })
	e.cfg.AgyBinaryPath = tampered
	store := e.openStore(t)
	e.seedAdopted(t, store, e.profile)
	e.recordAttestationDirect(t, store)
	_, err = NewServerWithAdapter(store, mustLock(t, e.stateDir), e.cfg, nil)
	if !errors.Is(err, execpolicy.ErrSealedImageMismatch) {
		t.Fatalf("a digest mismatch is the typed sealed-image refusal, got %T: %v", err, err)
	}
	if constructionProbeRan(e.scratch) {
		t.Fatal("no child may start for a drifted binary")
	}
}

// §6.1 row 11 — Unattested authenticated dispatch:
// ErrProductionEligibilityMissing before any child starts. (a) without a
// covering row the production service is not constructed at all (typed
// ErrNotEligible wrapping ErrProductionEligibilityMissing; no child);
// (b) a row that disappears after construction (an operator purge)
// fails the NEXT launch before any child, through the bridge.
func TestAcceptance_Agy_S11_UnattestedDispatchRefusedBeforeAnyChild(t *testing.T) {
	acc, err := newAgyProductionAcceptance(t, false)
	var ne *agy.ErrNotEligible
	var missing *agy.ErrProductionEligibilityMissing
	if !errors.As(err, &ne) || !errors.As(err, &missing) {
		t.Fatalf("an unattested production service is refused typed, got %T: %v", err, err)
	}
	if constructionProbeRan(acc.e.scratch) {
		t.Fatal("eligibility fails closed before any child")
	}

	acc, err = newAgyProductionAcceptance(t, true)
	if err != nil {
		t.Fatalf("attested construction: %v", err)
	}
	acc.create()
	if _, err := acc.store.DB().Exec(`DELETE FROM agy_protection_attestations`); err != nil {
		t.Fatalf("purge: %v", err)
	}
	acc.w.stage(t, agyKnown(agyWireNativeID), agyUserInputDone, agySuccess("must not run"))
	acc.queueRelease("t-unattested", "never launched", nil)
	acc.waitTurn("t-unattested", isTerminalStatus)
	acc.w.srv.Coordinator().WaitWorkers()
	code, resp := acc.collect("t-unattested")
	if code != http.StatusOK || resp["status"] != string(council.TurnFailed) ||
		!strings.Contains(fmt.Sprint(resp["result"]), "agy production eligibility missing") {
		t.Fatalf("the unattested launch fails typed, got %d %v", code, resp)
	}
	if args := acc.streamArgs(); len(args) != 1 {
		t.Fatalf("no turn child for an unattested dispatch (only the creation), launches=%d", len(args))
	}
	if a, _ := acc.store.GetLatestAgyTurnAttempt(context.Background(), agyWireSession, "t-unattested"); a != nil {
		t.Fatalf("eligibility is checked before the attempt reservation, got %+v", a)
	}
}

// agyIgnoredInputExecutor rewrites the turn's stdin envelope on the wire
// ("event":"user" → "event":"usr") so the child drops it with the 1.2.9
// "ignoring unsupported stream input message event" warning.
type agyIgnoredInputExecutor struct{ inner execpolicy.PolicyExecutor }

func (e *agyIgnoredInputExecutor) Start(ctx context.Context, req execpolicy.LaunchRequest) (execpolicy.ManagedProcess, error) {
	p, err := e.inner.Start(ctx, req)
	if err != nil || !strings.Contains(strings.Join(req.Args, " "), "--conversation") {
		return p, err
	}
	return &agyRewrittenStdin{ManagedProcess: p, w: &agyRewriteWriter{w: p.Stdin()}}, nil
}

type agyRewrittenStdin struct {
	execpolicy.ManagedProcess
	w io.WriteCloser
}

func (p *agyRewrittenStdin) Stdin() io.WriteCloser { return p.w }

type agyRewriteWriter struct {
	w   io.WriteCloser
	buf []byte
}

func (r *agyRewriteWriter) Write(b []byte) (int, error) {
	r.buf = append(r.buf, b...)
	return len(b), nil
}
func (r *agyRewriteWriter) Close() error {
	_, _ = io.WriteString(r.w, strings.Replace(string(r.buf), `"event":"user"`, `"event":"usr"`, 1))
	return r.w.Close()
}

// §6.1 row 12 — Ignored input event: the stderr marker ⇒ Uncertain,
// never assumed sent. The envelope reaches the child in a shape it
// ignores (warning on stderr, exit 0, no result): the attempt is
// Uncertain, not terminal, and blocks the conversation.
func TestAcceptance_Agy_S12_IgnoredInputEventUncertain(t *testing.T) {
	acc := newAgyAcceptance(t, &agyIgnoredInputExecutor{inner: &agyWireExecutor{inner: execpolicy.New()}}, nil)
	acc.create()
	ctx := context.Background()
	acc.w.stage(t, agyKnown(agyWireNativeID), agyUserInputDone, agySuccess("never produced"))
	acc.queueRelease("t-ignored", "dropped", nil)
	a := acc.waitAttemptDead("t-ignored")
	if a.Terminal || a.ObservedStatus != "uncertain" {
		t.Fatalf("a dropped input message is Uncertain, got %+v", a)
	}
	acc.requireUncertainReason("t-ignored", "stderr ignored-input marker")
	if in := acc.inputs(); len(in) != 1 || !strings.Contains(in[0], `"event":"usr"`) {
		t.Fatalf("the child saw the ignored envelope, got %q", in)
	}
	if d, _ := acc.store.GetTurnDetails(ctx, agyWireSession, "t-ignored"); d == nil || isTerminalStatus(d.Status) {
		t.Fatalf("an ignored input is never assumed sent (no terminal), got %+v", d)
	}
	if blocked, _ := acc.store.HasAgyUnresolvedAttempts(ctx, agyWireNativeID); !blocked {
		t.Fatal("the Uncertain attempt blocks the conversation")
	}
}
