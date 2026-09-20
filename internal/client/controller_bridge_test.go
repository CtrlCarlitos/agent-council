//go:build unix

package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/client"
	"github.com/CtrlCarlitos/agent-council/internal/service"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Task 6 restricted-controller-bridge evidence: permitted controller
// operations succeed through the same interface that denies every
// escalation attempt. The operator credential stays inside the bridge; the
// controller conversation supplies only its run and lease.

type adminBridgeFixture struct {
	srv   *service.Server
	store *storage.Store
	dir   string
	token string
}

func (f *adminBridgeFixture) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", f.srv.SocketPath())
	}}}
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()
	var out strings.Builder
	dec := json.NewDecoder(resp.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err == nil {
		var indented bytes.Buffer
		if json.Indent(&indented, raw, "", " ") == nil {
			out.WriteString(indented.String())
		}
	}
	return resp.StatusCode, out.String()
}

func TestAC004_RestrictedBridgePositiveAndEscalation(t *testing.T) {
	dir := t.TempDir()
	if len(dir) > 80 {
		// Keep the unix socket path bounded on long-temp-dir systems.
		dir = "/tmp/ac-br-" + fmt.Sprint(time.Now().UnixNano())
	}
	lock, err := service.AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer lock.Release()
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer store.Close()
	srv, err := service.NewServerWithAdapter(store, lock, service.ServerConfig{StateDir: dir, InstanceID: "inst-br", AuthToken: "tok-br"}, adaptertest.NewFakeAdapter("codex"))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	// The bridge reads discovery + operator credential like a real client;
	// discovery is published before Start (publication cleans stale sockets).
	meta := service.DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      "inst-br",
		Transport:       "unix",
		Endpoint:        srv.SocketPath(),
		StateDir:        dir,
		StartedAt:       time.Now().UTC(),
	}
	if err := service.PublishDiscovery(dir, meta, "tok-br"); err != nil {
		t.Fatalf("publish discovery: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Close()

	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-br", "run-br", "b", "s", "p", "boot-br"); err != nil {
		t.Fatalf("create run: %v", err)
	}

	f := &adminBridgeFixture{srv: srv, store: store, dir: dir, token: "tok-br"}
	code, body := f.post(t, "/v1/runs/run-br/controller/adopt",
		`{"op_id":"op-adopt-br","harness":"codex","controller_ref":"BR","bootstrap_lease":"boot-br"}`)
	if code != http.StatusOK {
		t.Fatalf("adopt: %d %s", code, body)
	}
	var grant struct {
		LeaseSecret string `json:"lease_secret"`
	}
	_ = json.Unmarshal([]byte(body), &grant)

	if _, err := store.CreateSession(ctx, "op-sess-br", grant.LeaseSecret, storage.SessionRecord{
		ID: "sess-br", RunID: "run-br", Contributor: "codex", Role: "reviewer",
		IsActiveContributor: false, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// The controller conversation holds ONLY its lease.
	bridge, err := client.NewControllerBridge(dir, "run-br", grant.LeaseSecret)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}

	// Positive: connect, queue, release through the restricted interface.
	if _, err := bridge.Connect(ctx, "op-conn-br", 1); err != nil {
		t.Fatalf("bridge connect: %v", err)
	}
	queueVer, _ := store.GetSessionVersion(ctx, "sess-br")
	if _, err := bridge.QueuePrompt(ctx, "op-q-br", "sess-br", "t-br", "bridge work", queueVer); err != nil {
		t.Fatalf("bridge queue: %v", err)
	}
	ver, _ := store.GetSessionVersion(ctx, "sess-br")
	if _, err := bridge.ReleaseTurn(ctx, "op-rel-br", "sess-br", "t-br", ver); err != nil {
		t.Fatalf("bridge release: %v", err)
	}
	d, err := store.GetTurnDetails(ctx, "sess-br", "t-br")
	if err != nil || d.Status != "running" {
		t.Fatalf("bridge-released turn must be running: %+v err=%v", d, err)
	}

	// Escalation: every administrative attempt through the same interface is
	// denied.
	esc := func(err error, what string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "not available through the controller interface") {
			t.Fatalf("bridge %s escalation must be denied, got %v", what, err)
		}
	}
	esc(bridge.Adopt(ctx, "op-esc-1", "agy", "esc", "boot-br"), "adopt")
	esc(bridge.Handoff(ctx, "op-esc-2", 1, "agy", "esc"), "handoff")
	esc(bridge.Revoke(ctx, "op-esc-3"), "revoke")
	esc(bridge.RecoverCredential(ctx, "op-esc-4", "op-adopt-br", 1), "recovery")
	esc(bridge.RawRequest(ctx, "POST", "/v1/service/stop", nil), "raw-route")

	// The denial left authority unchanged.
	rec, _ := store.GetControllerRecord(ctx, "run-br")
	if rec.Generation != 1 || !rec.Adopted {
		t.Fatalf("escalation attempts must not change authority: %+v", rec)
	}
}
