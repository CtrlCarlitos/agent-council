package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestRelease_IdempotentRetryAndDraining(t *testing.T) {
	dir := t.TempDir()
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
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
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

	qRec1, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-1",
		Prompt:    "Hello",
	})
	if err != nil {
		t.Fatalf("queue prompt t-1: %v", err)
	}

	// Authentically queue a sibling turn t-2 for later drain testing
	qRec2, err := store.QueuePrompt(ctx, "op-q-2", "lease-1", "sess-1", qRec1.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-2",
		Prompt:    "Followup",
	})
	if err != nil {
		t.Fatalf("queue prompt t-2: %v", err)
	}

	// Create test adapter assembly
	fakeAdapter := adaptertest.NewFakeAdapter("claude")

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-auth-token-12345",
	}

	srv, err := NewServerWithAdapter(store, lock, cfg, fakeAdapter)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())
	releaseBody := ReleaseRequest{
		InstanceID:      srv.InstanceID(),
		OpID:            "op-rel-1",
		ControllerLease: "lease-1",
		ExpectedVersion: qRec2.CommittedVersion,
	}
	bodyBytes, err := json.Marshal(releaseBody)
	if err != nil {
		t.Fatalf("marshal release req: %v", err)
	}

	// 1. Initial release succeeds with 202 Accepted
	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1/release", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("release request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %v", resp.StatusCode)
	}

	// Assert adapter dispatch count is exactly 1
	for i := 0; i < 100; i++ {
		if fakeAdapter.DispatchCount("sess-1") == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if count := fakeAdapter.DispatchCount("sess-1"); count != 1 {
		t.Fatalf("expected 1 dispatch for initial release, got %d", count)
	}

	// 2. Retry with same op_id succeeds with 200 OK and replayed: true
	req2, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1/release", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("create req2: %v", err)
	}
	req2.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("retry release request failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on replay, got %v", resp2.StatusCode)
	}
	var relResp ReleaseResponse
	if err := json.NewDecoder(resp2.Body).Decode(&relResp); err != nil {
		t.Fatalf("decode replay response: %v", err)
	}
	if !relResp.Replayed {
		t.Fatal("expected replayed == true on idempotent release retry")
	}

	// Assert adapter dispatch count remains exactly 1 (no second worker dispatched)
	if count := fakeAdapter.DispatchCount("sess-1"); count != 1 {
		t.Fatalf("expected dispatch count to remain 1 after replay, got %d", count)
	}

	// 3. Enter draining mode: new release is rejected with 503, but previous op_id retry still succeeds
	srv.Coordinator().SetState(ServiceStateDraining)

	// New release of sibling prompt t-2 rejected with 503
	newReleaseBody, err := json.Marshal(ReleaseRequest{
		InstanceID:      srv.InstanceID(),
		OpID:            "op-rel-2",
		ControllerLease: "lease-1",
		ExpectedVersion: qRec2.CommittedVersion,
	})
	if err != nil {
		t.Fatalf("marshal new release body: %v", err)
	}
	reqNew, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-2/release", bytes.NewReader(newReleaseBody))
	if err != nil {
		t.Fatalf("create reqNew: %v", err)
	}
	reqNew.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqNew.Header.Set("Content-Type", "application/json")
	respNew, err := client.Do(reqNew)
	if err != nil {
		t.Fatalf("new release during drain failed: %v", err)
	}
	defer respNew.Body.Close()
	if respNew.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while draining, got %v", respNew.StatusCode)
	}

	// Retry of op-rel-1 still returns 200 OK without checking adapter availability or drain state
	reqRetry, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1/release", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("create reqRetry: %v", err)
	}
	reqRetry.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqRetry.Header.Set("Content-Type", "application/json")
	respRetry, err := client.Do(reqRetry)
	if err != nil {
		t.Fatalf("retry during drain failed: %v", err)
	}
	defer respRetry.Body.Close()
	if respRetry.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on retry during drain, got %v", respRetry.StatusCode)
	}
}

func TestRelease_HarnessUnavailable(t *testing.T) {
	dir := t.TempDir()
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
	_, err = store.CreateRun(ctx, "op-run-harn", "run-harn", "brief", "spec", "profile-1", "lease-harn")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-harn", "lease-harn", storage.SessionRecord{
		ID:                  "sess-harn",
		RunID:               "run-harn",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	qRec, err := store.QueuePrompt(ctx, "op-q-harn", "lease-harn", "sess-harn", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-harn",
		TurnKey:   "t-harn",
		Prompt:    "Prompt for harness test",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-harn",
		AuthToken:  "auth-token-harn",
	}

	// Server without an adapter
	srv, err := NewServer(store, lock, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())
	bodyBytes, _ := json.Marshal(ReleaseRequest{
		InstanceID:      srv.InstanceID(),
		OpID:            "op-rel-harn",
		ControllerLease: "lease-harn",
		ExpectedVersion: qRec.CommittedVersion,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-harn/sessions/sess-harn/turns/t-harn/release", bytes.NewReader(bodyBytes))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do req: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for unavailable harness, got %d", resp.StatusCode)
	}
	var env ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != "harness_unavailable" {
		t.Fatalf("expected code harness_unavailable, got %s", env.Error.Code)
	}
}

func TestRelease_ClientDisconnectDoesNotCancelWorker(t *testing.T) {
	dir := t.TempDir()
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
	_, err = store.CreateRun(ctx, "op-run-disc", "run-disc", "brief", "spec", "profile-1", "lease-disc")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-disc", "lease-disc", storage.SessionRecord{
		ID:                  "sess-disc",
		RunID:               "run-disc",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	qRec, err := store.QueuePrompt(ctx, "op-q-disc", "lease-disc", "sess-disc", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-disc",
		TurnKey:   "t-disc",
		Prompt:    "Detached worker prompt",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	fakeAdapter := adaptertest.NewFakeAdapter("claude")

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-disc",
		AuthToken:  "auth-token-disc",
	}

	srv, err := NewServerWithAdapter(store, lock, cfg, fakeAdapter)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	reqCtx, reqCancel := context.WithCancel(ctx)
	releaseBody, _ := json.Marshal(ReleaseRequest{
		InstanceID:      srv.InstanceID(),
		OpID:            "op-rel-disc",
		ControllerLease: "lease-disc",
		ExpectedVersion: qRec.CommittedVersion,
	})

	req, err := http.NewRequestWithContext(reqCtx, "POST", "http://localhost/v1/runs/run-disc/sessions/sess-disc/turns/t-disc/release", bytes.NewReader(releaseBody))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("release failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted, got %d", resp.StatusCode)
	}

	// Cancel the HTTP client context immediately
	reqCancel()

	// Wait for worker to finish and verify outcome was committed to SQLite
	for i := 0; i < 100; i++ {
		var status string
		_ = store.ReadDB().QueryRowContext(ctx, "SELECT status FROM turns WHERE session_id = 'sess-disc' AND turn_key = 't-disc';").Scan(&status)
		if status == string(council.TurnCompleted) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var finalStatus string
	err = store.ReadDB().QueryRowContext(ctx, "SELECT status FROM turns WHERE session_id = 'sess-disc' AND turn_key = 't-disc';").Scan(&finalStatus)
	if err != nil {
		t.Fatalf("query turn status: %v", err)
	}
	if finalStatus != string(council.TurnCompleted) {
		t.Fatalf("expected turn completed despite client cancellation, got %s", finalStatus)
	}
}

func TestSupervisor_VersionAdvanceResilience(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-res", "run-res", "brief", "spec", "profile-1", "lease-res")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-res", "lease-res", storage.SessionRecord{
		ID:                  "sess-res",
		RunID:               "run-res",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	qRec, err := store.QueuePrompt(ctx, "op-q-res", "lease-res", "sess-res", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-res",
		TurnKey:   "t-res",
		Prompt:    "Prompt for version resilience",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	relRes, err := store.ReleaseTurn(ctx, "op-rel-res", "lease-res", "sess-res", qRec.CommittedVersion, "t-res")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	// Concurrent operation advances session row_version before supervisor records outcome
	_, err = store.QueuePrompt(ctx, "op-q-concurrent", "lease-res", "sess-res", relRes.Receipt.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-res",
		TurnKey:   "t-next",
		Prompt:    "Next prompt while t-res was running",
	})
	if err != nil {
		t.Fatalf("queue concurrent prompt: %v", err)
	}

	// Fake adapter that completes
	fakeAdapter := adaptertest.NewFakeAdapter("claude")

	doneCalled := false
	done := func() {
		doneCalled = true
	}

	sup := NewExecutionSupervisor(
		store,
		fakeAdapter,
		"run-res",
		"sess-res",
		"t-res",
		"lease-res",
		relRes.Receipt.CommittedVersion, // Stale version!
		relRes.Receipt,
		done,
	)

	// Run supervisor synchronously
	sup.Run(ctx)

	if !doneCalled {
		t.Fatal("expected done callback to be called")
	}

	// Assert that outcome was successfully committed despite the version advance
	var finalStatus, finalResult string
	err = store.ReadDB().QueryRowContext(ctx, "SELECT status, result FROM turns WHERE session_id = 'sess-res' AND turn_key = 't-res';").Scan(&finalStatus, &finalResult)
	if err != nil {
		t.Fatalf("query turn outcome: %v", err)
	}
	if finalStatus != string(council.TurnCompleted) {
		t.Fatalf("expected completed status, got %s", finalStatus)
	}
}
