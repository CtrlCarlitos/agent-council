//go:build unix

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// 1. Two Client Disconnect: Detached execution persists through client exit;
// follow-up prompt remains queued and unexecuted.
func TestAcceptance_TwoClientDisconnect_Deterministic(t *testing.T) {
	holdStart := make(chan struct{})
	h := newTestHarnessWithFaults(t, adaptertest.ScriptedFaults{
		HoldExecutionStart: holdStart,
	})

	ctx := context.Background()
	committedVer, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-1", "do work")

	// Also queue a follow-up prompt
	qRec2, err := h.store.QueuePrompt(ctx, "op-q-turn-2", "lease-1", "sess-1", committedVer, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "turn-2",
		Prompt:    "follow-up work",
	})
	if err != nil {
		t.Fatalf("queue prompt 2: %v", err)
	}

	// Client A: Releases turn-1 with a short request context
	ctxA, cancelA := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelA()

	relBody := fmt.Sprintf(`{"op_id":"op-rel-1","controller_lease":"lease-1","expected_version":%d}`, qRec2.CommittedVersion)
	reqA, err := http.NewRequestWithContext(ctxA, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-1/release", strings.NewReader(relBody))
	if err != nil {
		t.Fatalf("new request A: %v", err)
	}
	reqA.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	reqA.Header.Set("Content-Type", "application/json")

	respA, err := h.client.Do(reqA)
	if err != nil || respA.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on release, got %v, err: %v", respA.StatusCode, err)
	}
	respA.Body.Close()

	// Client A disconnects (cancel context) while worker is held at latch
	cancelA()

	// Release the latch: worker runs to completion under service ownership
	close(holdStart)

	// Wait for worker to finish and commit
	h.server.Coordinator().WaitWorkers()

	// Client B: Connects and queries turn-1 state
	reqB, err := http.NewRequestWithContext(context.Background(), "GET", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-1", nil)
	if err != nil {
		t.Fatalf("new request B: %v", err)
	}
	reqB.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)

	respB, err := h.client.Do(reqB)
	if err != nil || respB.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from Client B, got %v, err: %v", respB.StatusCode, err)
	}
	defer respB.Body.Close()

	var turnDetails struct {
		TurnKey string             `json:"turn_key"`
		Status  council.TurnStatus `json:"status"`
		Result  string             `json:"result"`
	}
	if err := json.NewDecoder(respB.Body).Decode(&turnDetails); err != nil {
		t.Fatalf("decode turn details: %v", err)
	}
	if turnDetails.Status != council.TurnCompleted {
		t.Fatalf("expected turn completed, got %v", turnDetails.Status)
	}

	// Verify follow-up prompt turn-2 remains queued and unexecuted
	hydrated, err := h.store.HydrateState(context.Background())
	if err != nil {
		t.Fatalf("hydrate from storage: %v", err)
	}
	sess, ok := hydrated.Sessions["sess-1"]
	if !ok {
		t.Fatalf("session sess-1 not found in hydrated state")
	}
	if _, ok := sess.PendingPrompts["turn-2"]; !ok {
		t.Fatalf("expected follow-up prompt turn-2 to remain queued in pending_prompts")
	}
	if _, ok := sess.Turns["turn-2"]; ok {
		t.Fatalf("expected follow-up prompt turn-2 not to be released into turns table")
	}
}

// 2. Crash Recovery Without Accessible Native Evidence: Turn reservation preserved as unresolved,
// no automatic redispatch.
func TestAcceptance_CrashRecovery_WithoutAccessibleNativeEvidence(t *testing.T) {
	dir := testStateDir(t)

	// 1. Initial service instance releases turn and simulates crash
	lock1, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock 1: %v", err)
	}
	store1, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store 1: %v", err)
	}

	ctx := context.Background()
	_, _ = store1.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile", "lease-1")
	sessRec, _ := store1.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID:                  "sess-1",
		RunID:               "run-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	qRec, _ := store1.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "turn-crash",
		Prompt:    "work before crash",
	})
	relResult, err := store1.ReleaseTurn(ctx, "op-rel-crash", "lease-1", "sess-1", qRec.CommittedVersion, "turn-crash")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}
	obsRec, err := store1.RecordDispatchObservation(ctx, "op-obs-crash", "lease-1", "sess-1", "turn-crash", "receipt_acknowledged")
	if err != nil {
		t.Fatalf("record dispatch observation: %v", err)
	}
	_ = obsRec

	// Simulate crash: close store and release lock without clean teardown or terminal recording
	_ = store1.Close()
	_ = lock1.Release()

	// 2. Restart service instance against same state directory
	lock2, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock 2: %v", err)
	}
	defer lock2.Release()

	store2, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store 2: %v", err)
	}
	defer store2.Close()

	fakeAdp2 := adaptertest.NewFakeAdapter("claude")
	cfg2 := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-restart-2",
		AuthToken:  "token-restart-2",
	}
	srv2, err := NewServerWithAdapter(store2, lock2, cfg2, fakeAdp2)
	if err != nil {
		t.Fatalf("new server 2: %v", err)
	}
	if err := srv2.Start(); err != nil {
		t.Fatalf("start server 2: %v", err)
	}
	defer srv2.Close()

	client2 := newTestClient(srv2.SocketPath())

	// Authoritative turn read verifies state is running with receipt_acknowledged
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-crash", nil)
	req.Header.Set("Authorization", "Bearer "+cfg2.AuthToken)
	resp, err := client2.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on turn read after restart, got %v, err: %v", resp.StatusCode, err)
	}
	defer resp.Body.Close()

	var turnRead storage.TurnDetails
	if err := json.NewDecoder(resp.Body).Decode(&turnRead); err != nil {
		t.Fatalf("decode turn read: %v", err)
	}
	if turnRead.Status != council.TurnRunning {
		t.Fatalf("expected turn running in durable storage, got %v", turnRead.Status)
	}
	if turnRead.DispatchIntent == nil || turnRead.DispatchIntent.Phase != "receipt_acknowledged" {
		t.Fatalf("expected receipt_acknowledged intent, got %+v", turnRead.DispatchIntent)
	}

	// Verify no automatic redispatch occurred
	if fakeAdp2.DispatchCount("sess-1") != 0 {
		t.Fatalf("turn must not be automatically redispatched upon restart")
	}

	// Verify /v1/status reports live_workers: 0, reserved_turns: 1, unresolved_turns: 1
	statusReq, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/status", nil)
	statusReq.Header.Set("Authorization", "Bearer "+cfg2.AuthToken)
	statusResp, err := client2.Do(statusReq)
	if err != nil || statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status request failed: %v", err)
	}
	defer statusResp.Body.Close()
	var stResp StatusResponse
	if err := json.NewDecoder(statusResp.Body).Decode(&stResp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if stResp.LiveWorkers != 0 || stResp.ReservedTurns != 1 || stResp.UnresolvedTurns != 1 {
		t.Fatalf("expected live_workers=0, reserved_turns=1, unresolved_turns=1, got %+v", stResp)
	}

	// Verify idle stop (drain=false) is rejected with 409 service_busy due to recoveryBlockers
	stopBody := fmt.Sprintf(`{"instance_id":%q,"drain":false}`, cfg2.InstanceID)
	stopReq, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/service/stop", strings.NewReader(stopBody))
	stopReq.Header.Set("Authorization", "Bearer "+cfg2.AuthToken)
	stopReq.Header.Set("Content-Type", "application/json")
	stopResp, err := client2.Do(stopReq)
	if err != nil || stopResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 Conflict on idle stop with unresolved recovery blockers, got %v", stopResp.StatusCode)
	}
	stopResp.Body.Close()

	_ = relResult
}

// 3. Restart With Independently Retained Evidence: Reconcile allocates recovery episode,
// probes independent evidence, and resolves turn.
func TestAcceptance_RestartWithIndependentlyRetainedEvidence(t *testing.T) {
	dir := testStateDir(t)

	// Initial store setup
	lock1, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock 1: %v", err)
	}
	store1, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store 1: %v", err)
	}
	ctx := context.Background()
	if _, err := store1.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile", "lease-1"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store1.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID:                  "sess-1",
		RunID:               "run-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// The saved native binding is the attach point for explicit recovery.
	bindRec, err := store1.SetNativeBinding(ctx, "op-bind-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.NativeBinding{
		LogicalSessionID: "sess-1",
		NativeSessionID:  "native-sess-1",
		Harness:          "claude",
	})
	if err != nil {
		t.Fatalf("set native binding: %v", err)
	}
	qRec, err := store1.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", bindRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "turn-evid",
		Prompt:    "work before crash",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	if _, err := store1.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", qRec.CommittedVersion, "turn-evid"); err != nil {
		t.Fatalf("release turn: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store 1: %v", err)
	}
	if err := lock1.Release(); err != nil {
		t.Fatalf("release lock 1: %v", err)
	}

	// Restart service
	lock2, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock 2: %v", err)
	}
	defer lock2.Release()
	store2, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store 2: %v", err)
	}
	defer store2.Close()

	// Adapter simulates independently retained evidence. The native session
	// matching the saved binding exists at the harness side.
	fakeAdp2 := adaptertest.NewFakeAdapter("claude")
	if _, err := fakeAdp2.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID("sess-1"),
		Contributor: "claude",
	}); err != nil {
		t.Fatalf("create adapter session: %v", err)
	}
	fakeAdp2.SetExecutionResult(adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-evid"}, adapter.TurnResult{
		Status: council.TurnCompleted,
		Output: "authoritative output from independent evidence",
	})

	cfg2 := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-restart-3",
		AuthToken:  "token-restart-3",
	}
	srv2, err := NewServerWithAdapter(store2, lock2, cfg2, fakeAdp2)
	if err != nil {
		t.Fatalf("new server 2: %v", err)
	}
	if err := srv2.Start(); err != nil {
		t.Fatalf("start server 2: %v", err)
	}
	defer srv2.Close()

	client2 := newTestClient(srv2.SocketPath())

	// Call reconcile endpoint
	recBody := `{"op_id":"op-rec-evid","controller_lease":"lease-1"}`
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-evid/reconcile", strings.NewReader(recBody))
	req.Header.Set("Authorization", "Bearer "+cfg2.AuthToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client2.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on reconcile, got %v, err: %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Verify turn is authoritatively completed with output
	details, err := store2.GetTurnDetails(ctx, "sess-1", "turn-evid")
	if err != nil {
		t.Fatalf("get turn details: %v", err)
	}
	if details.Status != council.TurnCompleted || details.Result != "authoritative output from independent evidence" {
		t.Fatalf("unexpected reconciled details: %+v", details)
	}
}

// 4. Reconcile Retry Lost Composite Response: Retrying reconcile with same op_id returns
// committed receipt idempotently without re-probing adapter.
func TestAcceptance_ReconcileRetry_LostCompositeResponse(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-retry", "prompt")

	// Explicit recovery attaches to the saved native binding.
	bindRec, err := h.store.SetNativeBinding(ctx, "op-bind-retry", "lease-1", "sess-1", ver, storage.NativeBinding{
		LogicalSessionID: "sess-1",
		NativeSessionID:  "native-sess-1",
		Harness:          "claude",
	})
	if err != nil {
		t.Fatalf("set native binding: %v", err)
	}
	if _, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID("sess-1"),
		Contributor: "claude",
	}); err != nil {
		t.Fatalf("create adapter session: %v", err)
	}

	_, err = h.store.ReleaseTurn(ctx, "op-rel-retry", "lease-1", "sess-1", bindRec.CommittedVersion, "turn-retry")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	h.adapter.SetExecutionResult(adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-retry"}, adapter.TurnResult{
		Status: council.TurnCompleted,
		Output: "reconciled output",
	})

	recBody := `{"op_id":"op-comp-retry","controller_lease":"lease-1"}`

	// First reconcile invocation
	req1, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-retry/reconcile", strings.NewReader(recBody))
	req1.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := h.client.Do(req1)
	if err != nil || resp1.StatusCode != http.StatusOK {
		t.Fatalf("first reconcile: %v, err: %v", resp1.StatusCode, err)
	}
	var res1 ReconcileResponse
	_ = json.NewDecoder(resp1.Body).Decode(&res1)
	resp1.Body.Close()

	// Client retries with same op_id (simulating lost response)
	req2, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-retry/reconcile", strings.NewReader(recBody))
	req2.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := h.client.Do(req2)
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry reconcile: %v, err: %v", resp2.StatusCode, err)
	}
	var res2 ReconcileResponse
	_ = json.NewDecoder(resp2.Body).Decode(&res2)
	resp2.Body.Close()

	if res1.Receipt.CommittedVersion != res2.Receipt.CommittedVersion {
		t.Fatalf("expected identical receipt version on retry: got %d and %d", res1.Receipt.CommittedVersion, res2.Receipt.CommittedVersion)
	}
}

// 5. Idle Stop Release Race: Under admission lock, release is either admitted and tracked
// or rejected with 503 / stopping.
func TestAcceptance_IdleStop_ReleaseRace(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-race", "prompt")

	var wg sync.WaitGroup
	wg.Add(2)

	var stopStatusCode int
	var releaseStatusCode int

	// Goroutine 1: Stop service
	go func() {
		defer wg.Done()
		stopBody := fmt.Sprintf(`{"instance_id":%q,"drain":false}`, h.cfg.InstanceID)
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/service/stop", strings.NewReader(stopBody))
		req.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.client.Do(req)
		if err == nil {
			stopStatusCode = resp.StatusCode
			resp.Body.Close()
		}
	}()

	// Goroutine 2: Release turn
	go func() {
		defer wg.Done()
		relBody := fmt.Sprintf(`{"op_id":"op-rel-race","controller_lease":"lease-1","expected_version":%d}`, ver)
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-race/release", strings.NewReader(relBody))
		req.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.client.Do(req)
		if err == nil {
			releaseStatusCode = resp.StatusCode
			resp.Body.Close()
		}
	}()

	wg.Wait()

	// Release was either accepted (202) causing stop to return 409 service_busy,
	// or stop was accepted (202) causing release to be rejected with 503 service_stopping
	if releaseStatusCode == http.StatusAccepted {
		if stopStatusCode != http.StatusConflict {
			t.Fatalf("expected stop to return 409 conflict when release was accepted, got %v", stopStatusCode)
		}
	} else if releaseStatusCode == http.StatusServiceUnavailable {
		if stopStatusCode != http.StatusAccepted {
			t.Fatalf("expected stop to return 202 accepted when release was rejected, got %v", stopStatusCode)
		}
	} else {
		t.Fatalf("unexpected release status code: %v (stop status: %v)", releaseStatusCode, stopStatusCode)
	}
}

// 6. Concurrent Startup Race: Exactly one owner succeeds; loser fails with already running.
func TestAcceptance_ConcurrentStartupRace(t *testing.T) {
	dir := testStateDir(t)

	var wg sync.WaitGroup
	wg.Add(2)

	var lock1, lock2 *ServiceLock
	var err1, err2 error

	go func() {
		defer wg.Done()
		lock1, err1 = AcquireServiceLock(dir)
	}()

	go func() {
		defer wg.Done()
		lock2, err2 = AcquireServiceLock(dir)
	}()

	wg.Wait()

	successCount := 0
	if err1 == nil && lock1 != nil {
		successCount++
		defer lock1.Release()
	}
	if err2 == nil && lock2 != nil {
		successCount++
		defer lock2.Release()
	}

	if successCount != 1 {
		t.Fatalf("expected exactly 1 winner in concurrent lock acquisition, got %d (err1: %v, err2: %v)", successCount, err1, err2)
	}
}

// 7. Release Retry During Drain: Service in draining state returns 200 OK for already-committed
// release without additional dispatch.
func TestAcceptance_ReleaseRetryDuringDrain(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-drain-rel", "prompt")

	// 1. Initial release
	relBody := fmt.Sprintf(`{"op_id":"op-rel-drain-1","controller_lease":"lease-1","expected_version":%d}`, ver)
	req1, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-drain-rel/release", strings.NewReader(relBody))
	req1.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := h.client.Do(req1)
	if err != nil || resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on release, got %v, err: %v", resp1.StatusCode, err)
	}
	resp1.Body.Close()

	// Wait for worker to finish
	h.server.Coordinator().WaitWorkers()

	// 2. Put coordinator into draining state
	h.server.Coordinator().SetState(ServiceStateDraining)

	// 3. Retry matching release
	req2, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-drain-rel/release", strings.NewReader(relBody))
	req2.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := h.client.Do(req2)
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for replayed release during drain, got %v, err: %v", resp2.StatusCode, err)
	}
	defer resp2.Body.Close()

	var relResp ReleaseResponse
	_ = json.NewDecoder(resp2.Body).Decode(&relResp)
	if !relResp.Replayed {
		t.Fatalf("expected replayed = true on duplicate release during drain")
	}

	// 4. Attempting NEW release during drain must be rejected with 503
	newBody := fmt.Sprintf(`{"op_id":"op-rel-new","controller_lease":"lease-1","expected_version":%d}`, ver+1)
	req3, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-drain-rel/release", strings.NewReader(newBody))
	req3.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req3.Header.Set("Content-Type", "application/json")
	resp3, err := h.client.Do(req3)
	if err != nil || resp3.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable for new release during drain, got %v", resp3.StatusCode)
	}
	resp3.Body.Close()
}

// 8. Signal Grace Expiry Preserves Unresolved: When signal grace expires before execution completes,
// active turn is cancelled and preserved as unresolved in SQLite without early lock release.
func TestAcceptance_SignalGraceExpiry_PreservesUnresolved(t *testing.T) {
	holdStart := make(chan struct{})
	h := newTestHarnessWithFaults(t, adaptertest.ScriptedFaults{
		HoldExecutionStart: holdStart,
	})

	ctx := context.Background()
	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-sig", "long work")

	relBody := fmt.Sprintf(`{"op_id":"op-rel-sig","controller_lease":"lease-1","expected_version":%d}`, ver)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-sig/release", strings.NewReader(relBody))
	req.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on release, got %v, err: %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Wait until fake adapter receives dispatch and blocks on holdStart
	ref := adapter.TurnRef{SessionID: adapter.SessionID("sess-1"), TurnKey: "turn-sig"}
	for i := 0; i < 100; i++ {
		if h.adapter.TurnState(ref).Received {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Service is now running worker (blocked at holdStart).
	// Trigger signal grace handling directly or via cancel active workers and teardown with short deadline
	h.server.Coordinator().SetState(ServiceStateDraining)
	h.server.Coordinator().CancelActiveWorkers()

	// Teardown with short deadline
	start := time.Now()
	_ = h.server.Teardown(100 * time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("teardown took too long on grace expiry: %v", elapsed)
	}

	// Release latch so blocked goroutine can exit
	close(holdStart)

	// Verify discovery files unlinked and lock file remains on disk
	if fileExists(filepath.Join(h.dir, "council.sock")) || fileExists(filepath.Join(h.dir, "auth.token")) {
		t.Fatal("runtime socket and token must be unlinked on forced teardown")
	}
	if !fileExists(filepath.Join(h.dir, "service.lock")) {
		t.Fatal("service.lock must remain on disk")
	}

	// Reopen store to verify turn status was preserved as running (unresolved) without fabricated completion
	storeVerify, err := storage.Open(storage.StoreOptions{StateDir: h.dir})
	if err != nil {
		t.Fatalf("open store verify: %v", err)
	}
	defer storeVerify.Close()

	details, err := storeVerify.GetTurnDetails(context.Background(), "sess-1", "turn-sig")
	if err != nil {
		t.Fatalf("get turn details: %v", err)
	}
	if details.Status != council.TurnRunning {
		t.Fatalf("expected turn to remain running/unresolved in SQLite, got status: %v (result: %q)", details.Status, details.Result)
	}
}
