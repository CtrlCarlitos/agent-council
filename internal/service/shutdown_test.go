//go:build unix

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestShutdown_IdleAndDrainingContracts(t *testing.T) {
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

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-shutdown",
	}

	srv, err := NewServer(store, lock, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	// 1. Instance mismatch returns 409 instance_mismatch
	badBody, err := json.Marshal(StopRequest{InstanceID: "wrong-id", Drain: false})
	if err != nil {
		t.Fatalf("marshal bad stop body: %v", err)
	}
	req, err := http.NewRequestWithContext(context.Background(), "POST", "http://localhost/v1/service/stop", bytes.NewReader(badBody))
	if err != nil {
		t.Fatalf("create req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("mismatch stop req failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 instance mismatch, got %v", resp.StatusCode)
	}

	// 2. Idle stop on empty service returns 202 Accepted and reaches quiescence
	stopBody, err := json.Marshal(StopRequest{InstanceID: cfg.InstanceID, Drain: false})
	if err != nil {
		t.Fatalf("marshal stop body: %v", err)
	}
	reqStop, err := http.NewRequestWithContext(context.Background(), "POST", "http://localhost/v1/service/stop", bytes.NewReader(stopBody))
	if err != nil {
		t.Fatalf("create reqStop: %v", err)
	}
	reqStop.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqStop.Header.Set("Content-Type", "application/json")
	respStop, err := client.Do(reqStop)
	if err != nil {
		t.Fatalf("stop request failed: %v", err)
	}
	defer respStop.Body.Close()
	if respStop.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on idle stop, got %v", respStop.StatusCode)
	}

	// Bounded wait for server shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.WaitForShutdown(shutdownCtx); err != nil {
		t.Fatalf("server did not shut down within timeout: %v", err)
	}

	// Verify discovery files unlinked and lock file remains on disk
	if fileExists(srv.SocketPath()) || fileExists(srv.TokenPath()) {
		t.Fatal("runtime socket and token must be unlinked on clean shutdown")
	}
	if !fileExists(lock.Path()) {
		t.Fatal("service.lock must remain on disk after shutdown")
	}
}

func TestShutdown_BusyRejectionWithoutDrain(t *testing.T) {
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

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-shutdown",
	}

	srv, err := NewServer(store, lock, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	// Manually simulate busy state on coordinator (e.g. 1 live worker)
	_, workerDone, err := srv.Coordinator().RegisterWorker(adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-1"})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}
	defer workerDone()

	// Stop without drain while busy must return 409 service_busy
	stopBody, _ := json.Marshal(StopRequest{InstanceID: cfg.InstanceID, Drain: false})
	reqStop, err := http.NewRequestWithContext(context.Background(), "POST", "http://localhost/v1/service/stop", bytes.NewReader(stopBody))
	if err != nil {
		t.Fatalf("create reqStop: %v", err)
	}
	reqStop.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqStop.Header.Set("Content-Type", "application/json")
	respStop, err := client.Do(reqStop)
	if err != nil {
		t.Fatalf("stop request failed: %v", err)
	}
	defer respStop.Body.Close()
	if respStop.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 service_busy, got %v", respStop.StatusCode)
	}
}

func TestShutdown_DrainingWaitsForWorkerCompletion(t *testing.T) {
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
	qRec, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-drain",
		Prompt:    "Hello drain",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	fakeAdapter := adaptertest.NewFakeAdapter("claude")
	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-789",
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

	// Release turn
	relBody := fmt.Sprintf(`{"op_id":"op-rel-drain","controller_lease":"lease-1","expected_version":%d}`, qRec.CommittedVersion)
	relReq, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-drain/release", strings.NewReader(relBody))
	if err != nil {
		t.Fatalf("create release req: %v", err)
	}
	relReq.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	relReq.Header.Set("Content-Type", "application/json")
	relResp, err := client.Do(relReq)
	if err != nil || relResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on release, got %v, err: %v", relResp.StatusCode, err)
	}
	relResp.Body.Close()

	// Request stop with drain=true
	stopBody, _ := json.Marshal(StopRequest{InstanceID: cfg.InstanceID, Drain: true})
	stopReq, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/service/stop", bytes.NewReader(stopBody))
	if err != nil {
		t.Fatalf("create stop req: %v", err)
	}
	stopReq.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	stopReq.Header.Set("Content-Type", "application/json")
	stopResp, err := client.Do(stopReq)
	if err != nil || stopResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on drain stop, got %v, err: %v", stopResp.StatusCode, err)
	}
	stopResp.Body.Close()

	// Server should shut down once worker finishes
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.WaitForShutdown(shutdownCtx); err != nil {
		t.Fatalf("server did not shut down after draining: %v", err)
	}

	// Verify turn reached terminal outcome in database by reopening durable store
	verifyStore, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer verifyStore.Close()

	details, err := verifyStore.GetTurnDetails(context.Background(), "sess-1", "t-drain")
	if err != nil {
		t.Fatalf("get turn details: %v", err)
	}
	if details == nil || details.Status != "completed" {
		t.Fatalf("expected turn completed, got %+v", details)
	}
}

func TestShutdown_ForcedTeardownOnDeadline(t *testing.T) {
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

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-shutdown",
	}

	srv, err := NewServer(store, lock, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	// Register a worker that hangs until cancelled
	workerCtx, workerDone, err := srv.Coordinator().RegisterWorker(adapter.TurnRef{SessionID: "sess-1", TurnKey: "t-hang"})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}

	workerCancelled := make(chan struct{})
	go func() {
		<-workerCtx.Done()
		workerDone()
		close(workerCancelled)
	}()

	// Execute teardown with 100ms timeout
	start := time.Now()
	err = srv.Teardown(100 * time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("teardown took too long: %v", elapsed)
	}

	// Worker should have been cancelled by forced teardown
	select {
	case <-workerCancelled:
	case <-time.After(1 * time.Second):
		t.Fatalf("worker was not cancelled on forced teardown")
	}

	// Verify discovery files unlinked and lock file remains on disk
	if fileExists(srv.SocketPath()) || fileExists(srv.TokenPath()) {
		t.Fatal("runtime socket and token must be unlinked on forced teardown")
	}
	if !fileExists(lock.Path()) {
		t.Fatal("service.lock must remain on disk after forced teardown")
	}
}

func TestShutdown_TeardownReturnsWithinDeadlineEvenIfWorkerNeverCallsDone(t *testing.T) {
	dir := testStateDir(t)
	lock, _ := AcquireServiceLock(dir)
	defer lock.Release()
	store, _ := storage.Open(storage.StoreOptions{StateDir: dir})
	defer store.Close()

	srv, _ := NewServer(store, lock, ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-hung-worker",
		AuthToken:  "token-hung",
	})
	_ = srv.Start()
	defer srv.Close()

	// Register a worker that NEVER calls workerDone (hung worker / uncooperative process)
	_, _, err := srv.Coordinator().RegisterWorker(adapter.TurnRef{SessionID: "sess-1", TurnKey: "t-hung-forever"})
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}

	start := time.Now()
	// Teardown with 50ms deadline
	err = srv.Teardown(50 * time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 1*time.Second {
		t.Fatalf("teardown blocked waiting on hung worker: elapsed %v", elapsed)
	}
	if err == nil {
		t.Fatalf("expected context deadline error, got nil")
	}
}
