//go:build unix

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Regression evidence for PR #29 review round 2 (head 3a4e07b).
//
// These tests translate the reviewer's controlled regressions into real-store,
// real-HTTP service tests. They encode the AC-003 invariants:
//
//  1. A cancellation request must not end outcome supervision while the
//     native execution may still be running (unsupported/requested/unknown).
//  2. A diagnostic refresh taken before a persistence obligation materialized
//     must not erase that obligation (versioned blocker reconciliation).
//  3. Post-terminal host-loss recovery must be reconcilable through the
//     retained recovery context, not just through a live active key.
//  4. Mutations must be rejected once the service is in final stopping.
//  5. A timed-out teardown must surface as a forced-termination error, not
//     orderly success.

// 1a. Cancel requested/unsupported: execution continues; supervisor still
// collects and persists the verified outcome; the response preserves the
// original request acceptance receipt and reports status separately.
func TestReviewPR29_CancelRequestKeepsOutcomeSupervision(t *testing.T) {
	holdStart := make(chan struct{})
	h := newTestHarnessWithFaults(t, adaptertest.ScriptedFaults{
		HoldExecutionStart: holdStart,
		ProbeCapabilities: adapter.AdapterCapabilities{
			MidTurnCancellation: adapter.CapabilityUnsupported,
		},
	})

	ctx := context.Background()
	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-cx", "survive cancel")

	relBody := fmt.Sprintf(`{"op_id":"op-rel-cx","controller_lease":"lease-1","expected_version":%d}`, ver)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-cx/release", strings.NewReader(relBody))
	req.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("release: %v, err: %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Wait until dispatch accepted, execution held, and the dispatch
	// observation durably recorded (stable session version).
	waitFor(t, 2*time.Second, func() bool {
		d, err := h.store.GetTurnDetails(ctx, "sess-1", "turn-cx")
		return err == nil && d != nil && d.DispatchIntent != nil && d.DispatchIntent.Phase == "receipt_acknowledged"
	})

	// Operator requests cancellation; adapter reports it unsupported, so the
	// native execution continues. The request carries the current session
	// version (release and dispatch observation advanced it).
	curVer, err := h.store.GetSessionVersion(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session version: %v", err)
	}
	cxlBody := fmt.Sprintf(`{"op_id":"op-cx","controller_lease":"lease-1","expected_version":%d,"reason":"operator stop"}`, curVer)
	cxlReq, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-cx/cancel", strings.NewReader(cxlBody))
	cxlReq.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	cxlReq.Header.Set("Content-Type", "application/json")
	cxlResp, err := h.client.Do(cxlReq)
	if err != nil || cxlResp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %v, err: %v", cxlResp.StatusCode, err)
	}
	var cxlResult CancelResponse
	if err := json.NewDecoder(cxlResp.Body).Decode(&cxlResult); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	cxlResp.Body.Close()

	if cxlResult.CancellationStatus != "unsupported" {
		t.Fatalf("expected cancellation_status unsupported, got %q", cxlResult.CancellationStatus)
	}
	// The original request acceptance receipt must be preserved, not replaced
	// by a terminal receipt.
	if cxlResult.Receipt.CommandType != "request_cancel" {
		t.Fatalf("expected request_cancel receipt preserved, got %+v", cxlResult.Receipt)
	}

	// Execution finishes only now, after the cancellation request completed.
	close(holdStart)

	h.server.Coordinator().WaitWorkers()

	// Supervisor must have collected and durably recorded the real outcome.
	details, err := h.store.GetTurnDetails(ctx, "sess-1", "turn-cx")
	if err != nil {
		t.Fatalf("get turn details: %v", err)
	}
	if details.Status != council.TurnCompleted {
		t.Fatalf("expected turn completed after unsupported cancel, got %v (result %q)", details.Status, details.Result)
	}

	counters := h.server.Coordinator().ShutdownCounters()
	for k, v := range counters {
		if v != 0 {
			t.Fatalf("expected zero accounting after supervised completion, got %s=%d", k, v)
		}
	}
}

// 1b. Cancel confirmed: the command-driven terminal commit notifies existing
// SSE subscribers and preserves the request receipt in the response.
func TestReviewPR29_CancelConfirmedNotifiesSubscribersAndKeepsReceipt(t *testing.T) {
	holdStart := make(chan struct{})
	h := newTestHarnessWithFaults(t, adaptertest.ScriptedFaults{
		HoldExecutionStart: holdStart,
	})

	ctx := context.Background()
	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-cc", "cancel me")

	relBody := fmt.Sprintf(`{"op_id":"op-rel-cc","controller_lease":"lease-1","expected_version":%d}`, ver)
	relReq, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-cc/release", strings.NewReader(relBody))
	relReq.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	relReq.Header.Set("Content-Type", "application/json")
	relResp, err := h.client.Do(relReq)
	if err != nil || relResp.StatusCode != http.StatusAccepted {
		t.Fatalf("release: %v, err: %v", relResp.StatusCode, err)
	}
	relResp.Body.Close()

	// Subscribe to the SSE stream while the execution is held (turn row
	// exists from the release commit; execution cannot finish yet).
	eventsReq, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-cc/events", nil)
	eventsReq.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	eventsReq.Header.Set("Accept", "text/event-stream")
	eventsResp, err := h.client.Do(eventsReq)
	if err != nil {
		t.Fatalf("subscribe events: %v", err)
	}
	defer eventsResp.Body.Close()

	events := make(chan string, 16)
	go func() {
		defer close(events)
		buf := make([]byte, 4096)
		for {
			n, err := eventsResp.Body.Read(buf)
			if n > 0 {
				events <- string(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait until the dispatch observation is durably recorded so the
	// session version is stable for the cancel request.
	waitFor(t, 2*time.Second, func() bool {
		d, err := h.store.GetTurnDetails(ctx, "sess-1", "turn-cc")
		return err == nil && d != nil && d.DispatchIntent != nil && d.DispatchIntent.Phase == "receipt_acknowledged"
	})

	curVer, err := h.store.GetSessionVersion(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session version: %v", err)
	}
	cxlBody := fmt.Sprintf(`{"op_id":"op-cc","controller_lease":"lease-1","expected_version":%d,"reason":"operator stop"}`, curVer)
	cxlReq, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-cc/cancel", strings.NewReader(cxlBody))
	cxlReq.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	cxlReq.Header.Set("Content-Type", "application/json")
	cxlResp, err := h.client.Do(cxlReq)
	if err != nil || cxlResp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %v, err: %v", cxlResp.StatusCode, err)
	}
	var cxlResult CancelResponse
	if err := json.NewDecoder(cxlResp.Body).Decode(&cxlResult); err != nil {
		t.Fatalf("decode cancel response: %v", err)
	}
	cxlResp.Body.Close()

	if cxlResult.CancellationStatus != "confirmed" {
		t.Fatalf("expected cancellation_status confirmed, got %q", cxlResult.CancellationStatus)
	}
	if cxlResult.Receipt.CommandType != "request_cancel" {
		t.Fatalf("expected original request receipt preserved, got %+v", cxlResult.Receipt)
	}

	// The SSE stream must receive a terminal event after the authoritative
	// command-driven commit.
	deadline := time.After(3 * time.Second)
	accum := ""
	for {
		select {
		case chunk, ok := <-events:
			if !ok {
				t.Fatalf("stream closed without terminal event; received: %s", accum)
			}
			accum += chunk
			if strings.Contains(accum, "event: terminal") && strings.Contains(accum, "cancelled") {
				close(holdStart)
				h.server.Coordinator().WaitWorkers()
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for terminal SSE notification; received: %s", accum)
		}
	}
}

// 2. Versioned blocker reconciliation: a diagnostic snapshot taken before an
// AddRecoveryBlocker obligation must not overwrite the newer obligation.
func TestReviewPR29_StaleDiagnosticCannotEraseRecoveryBlocker(t *testing.T) {
	c := NewCoordinator()

	// A diagnostic read begins while everything looks clean.
	epoch := c.BlockerEpoch()

	// Meanwhile the supervisor fails a terminal commit and installs a
	// persistence obligation.
	c.AddRecoveryBlocker()

	// The older diagnostic finishes and attempts to replace the count.
	if c.ApplyDiagnosticBlockers(0, epoch) {
		t.Fatal("stale diagnostic refresh must not be applied after a newer obligation")
	}
	if got := c.ShutdownCounters()["recovery_blockers"]; got != 1 {
		t.Fatalf("newer recovery obligation erased by stale diagnostic: blockers=%d", got)
	}
	if c.TryStopIdle() {
		t.Fatal("idle stop must not succeed while a recovery obligation is outstanding")
	}

	// A fresh diagnostic reflecting durable nonterminal state may reconcile
	// the count upward.
	fresh := c.BlockerEpoch()
	if !c.ApplyDiagnosticBlockers(1, fresh) {
		t.Fatal("fresh diagnostic refresh must apply")
	}
	if got := c.ShutdownCounters()["recovery_blockers"]; got != 1 {
		t.Fatalf("expected reconciled blocker count 1, got %d", got)
	}
}

// 3. Post-terminal host-loss reconciliation: terminal outcome recorded while
// the host was lost (active key cleared, recovery context retained) must be
// reconcilable, attaching to the saved native binding.
func TestReviewPR29_ReconcilePostTerminalRetainedContext(t *testing.T) {
	dir := testStateDir(t)

	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-pt", "run-pt", "brief", "spec", "profile", "lease-pt"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-pt", "lease-pt", storage.SessionRecord{
		ID: "sess-pt", RunID: "run-pt", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	qRec, err := store.QueuePrompt(ctx, "op-q-pt", "lease-pt", "sess-pt", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-pt", TurnKey: "turn-pt", Prompt: "work",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-pt", "lease-pt", "sess-pt", qRec.CommittedVersion, "turn-pt"); err != nil {
		t.Fatalf("release turn: %v", err)
	}

	// Save a native binding that reconciliation must attach to.
	bindRec, err := store.SetNativeBinding(ctx, "op-bind-pt", "lease-pt", "sess-pt", qRec.CommittedVersion+1, storage.NativeBinding{
		LogicalSessionID: "sess-pt", NativeSessionID: "native-sess-pt", Harness: "claude",
		Model: "", WorkspaceMode: "", ToolingConfig: "",
	})
	if err != nil {
		t.Fatalf("set native binding: %v", err)
	}

	// Host loss opens a recovery episode.
	hlRec, err := store.RecordHostLoss(ctx, "op-hl-pt", "lease-pt", "sess-pt", bindRec.CommittedVersion)
	if err != nil {
		t.Fatalf("record host loss: %v", err)
	}
	var generation uint64
	if _, err := fmt.Sscanf(hlRec.Payload, "%d", &generation); err != nil || generation == 0 {
		t.Fatalf("invalid host loss receipt payload %q: %v", hlRec.Payload, err)
	}

	// The terminal outcome arrives while visibility is still host_lost:
	// active_key is cleared and recovery_context retains the episode.
	termRec, err := store.RecordTerminalOutcome(ctx, "op-term-pt", "lease-pt", "sess-pt", hlRec.CommittedVersion, "turn-pt", council.TurnCompleted, "done while host lost")
	if err != nil {
		t.Fatalf("record terminal outcome under host loss: %v", err)
	}

	fake := adaptertest.NewFakeAdapter("claude")
	// Pre-create the matching native session so resume attaches to it.
	if _, err := fake.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID: adapter.SessionID("sess-pt"), Contributor: "claude",
	}); err != nil {
		t.Fatalf("create adapter session: %v", err)
	}

	cfg := ServerConfig{StateDir: dir, InstanceID: "inst-pt", AuthToken: "token-pt"}
	srv, err := NewServerWithAdapter(store, lock, cfg, fake)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	// Reconcile the retained post-terminal episode.
	recBody := fmt.Sprintf(`{"op_id":"op-rec-pt","controller_lease":"lease-pt","expected_version":%d}`, termRec.CommittedVersion)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-pt/sessions/sess-pt/turns/turn-pt/reconcile", strings.NewReader(recBody))
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("reconcile request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var env ErrorEnvelope
		_ = json.NewDecoder(resp.Body).Decode(&env)
		t.Fatalf("expected 200 OK on post-terminal reconcile, got %d (%+v)", resp.StatusCode, env.Error)
	}

	// Immutable terminal history is preserved; only the episode closes.
	details, err := store.GetTurnDetails(ctx, "sess-pt", "turn-pt")
	if err != nil {
		t.Fatalf("get turn details: %v", err)
	}
	if details.Status != council.TurnCompleted || details.Result != "done while host lost" {
		t.Fatalf("terminal history mutated by reconcile: %+v", details)
	}
	recState, err := store.GetSessionRecoveryState(ctx, "sess-pt")
	if err != nil {
		t.Fatalf("get recovery state: %v", err)
	}
	if recState.Visibility != "reachable" || recState.ActiveRecoveryGen != 0 || recState.RecoveryContext != "" {
		t.Fatalf("expected closed recovery episode, got %+v", recState)
	}
}

// 3b. Reconciliation of a wrong turn must be rejected before any probe,
// including in the retained post-terminal state.
func TestReviewPR29_ReconcileRejectsWrongTurnBeforeProbe(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-real", "prompt")
	if _, err := h.store.ReleaseTurn(ctx, "op-rel-wt", "lease-1", "sess-1", ver, "turn-real"); err != nil {
		t.Fatalf("release turn: %v", err)
	}

	recBody := `{"op_id":"op-rec-wrong","controller_lease":"lease-1"}`
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-wrong/reconcile", strings.NewReader(recBody))
	req.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("reconcile request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for wrong turn reconcile, got %d", resp.StatusCode)
	}
	// A wrong turn must be rejected before opening an episode or probing.
	recState, err := h.store.GetSessionRecoveryState(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get recovery state: %v", err)
	}
	if recState.Visibility != "reachable" || recState.ActiveRecoveryGen != 0 {
		t.Fatalf("wrong-turn reconcile mutated recovery state: %+v", recState)
	}
}

// 4. All mutation handlers must reject admission once the service is in
// final stopping.
func TestReviewPR29_MutationsRejectedWhenStopping(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-st", "prompt")

	h.server.Coordinator().SetState(ServiceStateStopping)

	mutations := []struct {
		name string
		req  *http.Request
	}{
		{"controller_connect", func() *http.Request {
			r, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/controller/connect", strings.NewReader(`{"op_id":"op-cn","controller_lease":"lease-1","expected_version":1}`))
			return r
		}()},
		{"queue_prompt", func() *http.Request {
			r, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/prompts/queue", strings.NewReader(fmt.Sprintf(`{"op_id":"op-q2","controller_lease":"lease-1","expected_version":%d,"turn_key":"turn-st-2","prompt":"x"}`, ver)))
			return r
		}()},
		{"replace_prompt", func() *http.Request {
			r, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/prompts/turn-st/replace", strings.NewReader(fmt.Sprintf(`{"op_id":"op-rp","controller_lease":"lease-1","expected_version":%d,"prompt":"x"}`, ver)))
			return r
		}()},
		{"discard_prompt", func() *http.Request {
			r, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/prompts/turn-st/discard", strings.NewReader(fmt.Sprintf(`{"op_id":"op-dp","controller_lease":"lease-1","expected_version":%d}`, ver)))
			return r
		}()},
		{"record_decision", func() *http.Request {
			r, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/decisions", strings.NewReader(`{"op_id":"op-dec","controller_lease":"lease-1","artifact_id":"a","revision":1,"decision_payload":"x"}`))
			return r
		}()},
	}

	for _, m := range mutations {
		m.req.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
		m.req.Header.Set("Content-Type", "application/json")
		resp, err := h.client.Do(m.req)
		if err != nil {
			t.Fatalf("%s: request: %v", m.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s: expected 503 in final stopping, got %d", m.name, resp.StatusCode)
		}
	}
}

// 5. A timed-out teardown must surface as a forced termination error from
// WaitForShutdown, not as orderly success.
func TestReviewPR29_ForcedTeardownSurfacesError(t *testing.T) {
	dir := testStateDir(t)
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-ft", "run-ft", "b", "s", "p", "lease-ft"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-ft", "lease-ft", storage.SessionRecord{
		ID: "sess-ft", RunID: "run-ft", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-ft", "lease-ft", "sess-ft", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-ft", TurnKey: "turn-ft", Prompt: "hang",
	}); err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	hold := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{HoldExecutionStart: hold})
	fake.SetDefaultContributor("claude")
	cfg := ServerConfig{StateDir: dir, InstanceID: "inst-ft", AuthToken: "token-ft"}
	srv, err := NewServerWithAdapter(store, lock, cfg, fake)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	relBody := fmt.Sprintf(`{"op_id":"op-rel-ft","controller_lease":"lease-ft","expected_version":%d}`, sessRec.CommittedVersion+1)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-ft/sessions/sess-ft/turns/turn-ft/release", strings.NewReader(relBody))
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := newTestClient(srv.SocketPath()).Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("release: %v, err: %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	ref := adapter.TurnRef{SessionID: adapter.SessionID("sess-ft"), TurnKey: "turn-ft"}
	waitFor(t, 2*time.Second, func() bool {
		st := fake.TurnState(ref)
		return st.Received && st.Accepted
	})

	// Simulate an admitted task that cannot finish before the deadline: the
	// coordinator never receives its done() callback (an unjoinable worker).
	_, stuckDone, err := srv.Coordinator().RegisterWorker(adapter.TurnRef{SessionID: "sess-ft", TurnKey: "t-stuck"})
	if err != nil {
		t.Fatalf("register stuck worker: %v", err)
	}
	defer func() {
		if stuckDone != nil {
			stuckDone()
		}
	}()

	// Grace expiry with an execution that never finishes: forced teardown.
	srv.Coordinator().CancelActiveWorkers()
	_ = srv.Teardown(100 * time.Millisecond)

	err = srv.WaitForShutdown(context.Background())
	if err == nil {
		t.Fatal("timed-out teardown must not surface as orderly success")
	}
	if !strings.Contains(err.Error(), "forced") {
		t.Fatalf("expected forced-termination error, got %v", err)
	}

	close(hold)
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
