//go:build unix

package service

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// testStateDir returns a short-lived state directory with a bounded path
// length. macOS limits unix socket paths to ~104 bytes (sun_path), which
// t.TempDir() directories routinely exceed.
func testStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "acsvc-")
	if err != nil {
		t.Fatalf("create short state dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return dir
}

type testHarness struct {
	t           *testing.T
	dir         string
	lock        *ServiceLock
	store       *storage.Store
	adapter     *adaptertest.FakeAdapter
	server      *Server
	cfg         ServerConfig
	client      *http.Client
	socketPath  string
	tokenPath   string
	cleanupDone bool
}

func newTestHarnessWithFaults(t *testing.T, faults adaptertest.ScriptedFaults) *testHarness {
	t.Helper()
	dir := testStateDir(t)

	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire service lock: %v", err)
	}

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		_ = lock.Release()
		t.Fatalf("open store: %v", err)
	}

	fakeAdp := adaptertest.NewFake(faults)
	fakeAdp.SetDefaultContributor("claude")
	instanceID := fmt.Sprintf("inst-test-%d", time.Now().UnixNano())
	authToken := "test-auth-token-" + instanceID

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: instanceID,
		AuthToken:  authToken,
	}

	srv, err := NewServerWithAdapter(store, lock, cfg, fakeAdp)
	if err != nil {
		_ = store.Close()
		_ = lock.Release()
		t.Fatalf("new server: %v", err)
	}

	meta := DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      instanceID,
		PID:             12345,
		Transport:       "unix",
		Endpoint:        srv.SocketPath(),
		StateDir:        dir,
		StartedAt:       time.Now().UTC(),
	}
	if err := PublishDiscovery(dir, meta, authToken); err != nil {
		_ = store.Close()
		_ = lock.Release()
		t.Fatalf("publish discovery: %v", err)
	}

	if err := srv.Start(); err != nil {
		_ = lock.CleanupDiscovery()
		_ = store.Close()
		_ = lock.Release()
		t.Fatalf("start server: %v", err)
	}

	c := newTestClient(srv.SocketPath())

	h := &testHarness{
		t:          t,
		dir:        dir,
		lock:       lock,
		store:      store,
		adapter:    fakeAdp,
		server:     srv,
		cfg:        cfg,
		client:     c,
		socketPath: srv.SocketPath(),
		tokenPath:  srv.TokenPath(),
	}

	t.Cleanup(func() {
		h.cleanup()
	})

	return h
}

func newTestHarness(t *testing.T) *testHarness {
	return newTestHarnessWithFaults(t, adaptertest.ScriptedFaults{})
}

func (h *testHarness) cleanup() {
	if h.cleanupDone {
		return
	}
	h.cleanupDone = true
	if h.server != nil {
		_ = h.server.Close()
	}
	if h.store != nil {
		_ = h.store.Close()
	}
	if h.lock != nil {
		_ = h.lock.Release()
	}
}

func (h *testHarness) createSessionAndTurn(ctx context.Context, runID, sessionID, turnKey, prompt string) (int64, storage.PendingPrompt) {
	h.t.Helper()
	_, err := h.store.CreateRun(ctx, "op-run-"+runID, runID, "brief", "spec", "profile", "lease-1")
	if err != nil {
		h.t.Fatalf("create run: %v", err)
	}
	adoptForTest(h.t, h.store, runID, "lease-1")
	connectControllerForTest(h.t, h.server, h.store, runID, "lease-1")

	sessRec, err := h.store.CreateSession(ctx, "op-sess-"+sessionID, "lease-1", storage.SessionRecord{
		ID:                  sessionID,
		RunID:               runID,
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		h.t.Fatalf("create session: %v", err)
	}

	qRec, err := h.store.QueuePrompt(ctx, "op-q-"+turnKey, "lease-1", sessionID, sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: sessionID,
		TurnKey:   turnKey,
		Prompt:    prompt,
	})
	if err != nil {
		h.t.Fatalf("queue prompt: %v", err)
	}

	return qRec.CommittedVersion, storage.PendingPrompt{
		SessionID: sessionID,
		TurnKey:   turnKey,
		Prompt:    prompt,
	}
}
