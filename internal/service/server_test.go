//go:build unix

package service

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func newTestClient(socketPath string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

func TestServer_ReadinessAndStatus(t *testing.T) {
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
		AuthToken:  "test-auth-token-secret-1234567890",
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

	// 1. Unauthenticated request must return 401 with WWW-Authenticate
	resp, err := client.Get("http://localhost/v1/readiness")
	if err != nil {
		t.Fatalf("unauthenticated GET failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %v", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("missing WWW-Authenticate header")
	}

	// 2. Authenticated readiness request must return 200 ready
	req, err := http.NewRequestWithContext(context.Background(), "GET", "http://localhost/v1/readiness", nil)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	respReady, err := client.Do(req)
	if err != nil {
		t.Fatalf("readiness request failed: %v", err)
	}
	defer respReady.Body.Close()
	if respReady.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got: %v", respReady.StatusCode)
	}

	var r ReadinessResponse
	if err := json.NewDecoder(respReady.Body).Decode(&r); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if r.Status != "ready" || r.InstanceID != cfg.InstanceID {
		t.Fatalf("unexpected readiness response: %+v", r)
	}

	// 3. Authenticated status request returns diagnostics
	reqStatus, err := http.NewRequestWithContext(context.Background(), "GET", "http://localhost/v1/status", nil)
	if err != nil {
		t.Fatalf("create status req: %v", err)
	}
	reqStatus.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	respStatus, err := client.Do(reqStatus)
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer respStatus.Body.Close()
	if respStatus.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got: %v", respStatus.StatusCode)
	}

	var s StatusResponse
	if err := json.NewDecoder(respStatus.Body).Decode(&s); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if s.Status != "ready" || s.LiveWorkers != 0 {
		t.Fatalf("unexpected status response: %+v", s)
	}
}

func TestServer_SocketPermissions0600(t *testing.T) {
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

	srv, err := NewServer(store, lock, ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-sock-perm",
		AuthToken:  "token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	info, err := os.Stat(srv.SocketPath())
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("expected socket permission 0600, got %o", perm)
	}
}

func TestServer_ReadinessReturns503WhenDraining(t *testing.T) {
	dir := testStateDir(t)
	lock, _ := AcquireServiceLock(dir)
	defer lock.Release()
	store, _ := storage.Open(storage.StoreOptions{StateDir: dir})
	defer store.Close()

	srv, _ := NewServer(store, lock, ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-drain-test",
		AuthToken:  "token-drain",
	})
	_ = srv.Start()
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	// 1. Initial ready returns 200
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "http://localhost/v1/readiness", nil)
	req.Header.Set("Authorization", "Bearer token-drain")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK when ready, got %v", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. Transition to draining -> returns 503 Service Unavailable
	srv.Coordinator().SetState(ServiceStateDraining)
	req2, _ := http.NewRequestWithContext(context.Background(), "GET", "http://localhost/v1/readiness", nil)
	req2.Header.Set("Authorization", "Bearer token-drain")
	resp2, err := client.Do(req2)
	if err != nil || resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable when draining, got %v", resp2.StatusCode)
	}
	resp2.Body.Close()
}

func TestServer_StatusDiagnosticCounts(t *testing.T) {
	dir := testStateDir(t)
	lock, _ := AcquireServiceLock(dir)
	defer lock.Release()
	store, _ := storage.Open(storage.StoreOptions{StateDir: dir})
	defer store.Close()

	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-diag", "run-diag-1", "brief", "spec", "profile", "lease-1")
	sessRec, _ := store.CreateSession(ctx, "op-sess-diag", "lease-1", storage.SessionRecord{
		ID:                  "sess-diag-1",
		RunID:               "run-diag-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	qRec, _ := store.QueuePrompt(ctx, "op-q-diag", "lease-1", "sess-diag-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-diag-1",
		TurnKey:   "turn-diag-1",
		Prompt:    "work",
	})
	_, _ = store.ReleaseTurn(ctx, "op-rel-diag", "lease-1", "sess-diag-1", qRec.CommittedVersion, "turn-diag-1")

	srv, _ := NewServer(store, lock, ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-diag-test",
		AuthToken:  "token-diag",
	})
	_ = srv.Start()
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/status", nil)
	req.Header.Set("Authorization", "Bearer token-diag")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("get status: %v, err: %v", resp.StatusCode, err)
	}
	defer resp.Body.Close()

	var s StatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode status: %v", err)
	}

	if len(s.ActiveRuns) != 1 || s.ActiveRuns[0] != "run-diag-1" {
		t.Fatalf("expected active run run-diag-1, got %v", s.ActiveRuns)
	}
	if s.ReservedTurns != 1 {
		t.Fatalf("expected 1 reserved turn, got %d", s.ReservedTurns)
	}
	if s.UnresolvedTurns != 1 {
		t.Fatalf("expected 1 unresolved turn, got %d", s.UnresolvedTurns)
	}
}
