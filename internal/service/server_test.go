package service

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
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
