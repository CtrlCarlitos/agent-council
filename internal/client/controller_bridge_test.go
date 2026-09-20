//go:build unix

package client_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
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
		if json.Indent(&indented, raw, "", "  ") == nil {
			out.WriteString(indented.String())
		}
	}
	return resp.StatusCode, out.String()
}

func newBridgeTestServer(t *testing.T) *adminBridgeFixture {
	t.Helper()
	dir, dirErr := os.MkdirTemp("/tmp", "ac-br-")
	if dirErr != nil {
		t.Fatalf("state dir: %v", dirErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	lock, err := service.AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv, err := service.NewServerWithAdapter(store, lock, service.ServerConfig{StateDir: dir, InstanceID: "inst-br", AuthToken: "tok-br"}, adaptertest.NewFakeAdapter("codex"))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
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
	t.Cleanup(func() { _ = srv.Close() })
	return &adminBridgeFixture{srv: srv, store: store, dir: dir, token: "tok-br"}
}

func TestAC004_RestrictedBridgePositiveAndEscalation(t *testing.T) {
	f := newBridgeTestServer(t)
	ctx := context.Background()
	if _, err := f.store.CreateRun(ctx, "op-run-br", "run-br", "b", "s", "p", "boot-br"); err != nil {
		t.Fatalf("create run: %v", err)
	}

	code, body := f.post(t, "/v1/runs/run-br/controller/adopt",
		`{"op_id":"op-adopt-br","harness":"codex","controller_ref":"BR","bootstrap_lease":"boot-br"}`)
	if code != http.StatusOK {
		t.Fatalf("adopt: %d %s", code, body)
	}
	var grant struct {
		LeaseSecret string `json:"lease_secret"`
	}
	_ = json.Unmarshal([]byte(body), &grant)

	if _, err := f.store.CreateSession(ctx, "op-sess-br", grant.LeaseSecret, storage.SessionRecord{
		ID: "sess-br", RunID: "run-br", Contributor: "codex", Role: "reviewer",
		IsActiveContributor: false, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// The controller conversation holds ONLY its lease.
	bridge, err := client.NewControllerBridge(f.dir, "run-br", grant.LeaseSecret)
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}

	// Positive: connect, queue, replace, discard, release, events through
	// the restricted interface (plan §6 requires all of these).
	if _, err := bridge.Connect(ctx, "op-conn-br", 1); err != nil {
		t.Fatalf("bridge connect: %v", err)
	}

	// Queue t-replace and t-discard (to exercise replace/discard) and
	// t-br for the main release path.
	queueVer, _ := f.store.GetSessionVersion(ctx, "sess-br")
	if _, err := bridge.QueuePrompt(ctx, "op-q-replace", "sess-br", "t-replace", "original", queueVer); err != nil {
		t.Fatalf("bridge queue t-replace: %v", err)
	}
	replaceVer, _ := f.store.GetSessionVersion(ctx, "sess-br")
	if _, err := bridge.ReplacePrompt(ctx, "op-rep-br", "sess-br", "t-replace", "replaced text", replaceVer); err != nil {
		t.Fatalf("bridge replace: %v", err)
	}

	discardVer, _ := f.store.GetSessionVersion(ctx, "sess-br")
	if _, err := bridge.QueuePrompt(ctx, "op-q-discard", "sess-br", "t-discard", "to discard", discardVer); err != nil {
		t.Fatalf("bridge queue t-discard: %v", err)
	}
	discardVer, _ = f.store.GetSessionVersion(ctx, "sess-br")
	if _, err := bridge.DiscardPrompt(ctx, "op-dis-br", "sess-br", "t-discard", discardVer); err != nil {
		t.Fatalf("bridge discard: %v", err)
	}

	// Queue and release t-br; use events to observe the outcome.
	queueVer, _ = f.store.GetSessionVersion(ctx, "sess-br")
	if _, err := bridge.QueuePrompt(ctx, "op-q-br", "sess-br", "t-br", "bridge work", queueVer); err != nil {
		t.Fatalf("bridge queue: %v", err)
	}
	ver, _ := f.store.GetSessionVersion(ctx, "sess-br")
	if _, err := bridge.ReleaseTurn(ctx, "op-rel-br", "sess-br", "t-br", ver); err != nil {
		t.Fatalf("bridge release: %v", err)
	}
	d, err := f.store.GetTurnDetails(ctx, "sess-br", "t-br")
	if err != nil || d.Status != "running" {
		t.Fatalf("bridge-released turn must be running: %+v err=%v", d, err)
	}

	// Subscribe to events; verify we receive at least the initial snapshot.
	evResp, err := bridge.SubscribeEvents(ctx, "sess-br", "t-br")
	if err != nil {
		t.Fatalf("bridge subscribe events: %v", err)
	}
	scanner := bufio.NewScanner(evResp.Body)
	gotSnapshot := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			gotSnapshot = true
			break
		}
	}
	_ = evResp.Body.Close()
	if !gotSnapshot {
		t.Fatal("bridge events stream must deliver at least one event")
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
	rec, _ := f.store.GetControllerRecord(ctx, "run-br")
	if rec.Generation != 1 || !rec.Adopted {
		t.Fatalf("escalation attempts must not change authority: %+v", rec)
	}
}

// TestAC004_BridgeDisconnectSubmitsOwnEpisode verifies the delayed-disconnect
// invariant: bridge A connects, bridge B reconnects under the same lease and
// generation, then A calls Disconnect. A must submit its own (stale) episode
// ID, not B's current attachment. The service ignores the stale episode
// (delayed-disconnect rule), and B remains connected.
func TestAC004_BridgeDisconnectSubmitsOwnEpisode(t *testing.T) {
	f := newBridgeTestServer(t)
	ctx := context.Background()
	if _, err := f.store.CreateRun(ctx, "op-run-ep", "run-ep", "b", "s", "p", "boot-ep"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	code, body := f.post(t, "/v1/runs/run-ep/controller/adopt",
		`{"op_id":"op-adopt-ep","harness":"agy","controller_ref":"EP","bootstrap_lease":"boot-ep"}`)
	if code != http.StatusOK {
		t.Fatalf("adopt: %d %s", code, body)
	}
	var grant struct {
		LeaseSecret string `json:"lease_secret"`
	}
	_ = json.Unmarshal([]byte(body), &grant)

	// Bridge A connects and records its episode.
	bridgeA, err := client.NewControllerBridge(f.dir, "run-ep", grant.LeaseSecret)
	if err != nil {
		t.Fatalf("bridge A: %v", err)
	}
	connA, err := bridgeA.Connect(ctx, "op-conn-ep-a", 1)
	if err != nil {
		t.Fatalf("bridge A connect: %v", err)
	}
	episodeA := connA.Receipt.AttachmentID
	if episodeA == "" {
		t.Fatal("bridge A connect must return an attachment ID")
	}

	// Bridge B reconnects under the same lease, creating a new episode.
	bridgeB, err := client.NewControllerBridge(f.dir, "run-ep", grant.LeaseSecret)
	if err != nil {
		t.Fatalf("bridge B: %v", err)
	}
	connB, err := bridgeB.Connect(ctx, "op-conn-ep-b", 1)
	if err != nil {
		t.Fatalf("bridge B connect: %v", err)
	}
	episodeB := connB.Receipt.AttachmentID
	if episodeB == "" || episodeB == episodeA {
		t.Fatalf("bridge B must establish a distinct episode, got A=%q B=%q", episodeA, episodeB)
	}

	// The coordinator must see B as attached.
	if !f.srv.Coordinator().ControllerAttached("run-ep", 1) {
		t.Fatal("coordinator must show controller attached after B connects")
	}

	// Bridge A now calls Disconnect. It must submit episodeA (its own
	// retained ID), not B's current episode.
	if err := bridgeA.Disconnect(ctx, "op-disc-ep-a", 1); err != nil {
		// The service ignores a stale episode (delayed-disconnect rule);
		// a non-error response is expected.
		t.Fatalf("bridge A disconnect (stale episode) must not error: %v", err)
	}

	// B must remain attached: A disconnected its own stale episode, not B's.
	if !f.srv.Coordinator().ControllerAttached("run-ep", 1) {
		t.Fatal("B must remain attached after A disconnects its own stale episode")
	}
}
