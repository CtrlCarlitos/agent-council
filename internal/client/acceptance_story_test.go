//go:build unix

package client_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/client"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/service"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// AC-004 Task 7 primary acceptance story through the intended interfaces:
// adoption over operator HTTP, controller work through the restricted
// bridge, explicit disconnect, mid-story handoff, and successor-only
// release — verified against durable outcomes, not response codes alone.
// Controller identity rotates through all four harness identifiers across
// repetitions, and the adoption→handoff→replay transitions repeat across a
// service restart. Controlled fixtures; native-conversation adoption is
// explicitly unverified.
func TestAC004_PrimaryStoryThroughIntendedInterfaces_ControlledFixture(t *testing.T) {
	harnesses := []string{"opencode", "claude", "codex", "agy"}
	for i, harnessA := range harnesses {
		harnessB := harnesses[(i+1)%len(harnesses)]
		t.Run(harnessA+"->"+harnessB, func(t *testing.T) {
			runPrimaryStory(t, harnessA, harnessB)
		})
	}
}

func runPrimaryStory(t *testing.T, harnessA, harnessB string) {
	t.Helper()
	dir, dirErr := os.MkdirTemp("/tmp", "ac-ps-")
	if dirErr != nil {
		t.Fatalf("state dir: %v", dirErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctx := context.Background()

	// --- Instance 1 ---------------------------------------------------
	srv1, store1, lock1 := startStoryServer(t, dir, "inst-ps1", "tok-ps1")

	if _, err := store1.CreateRun(ctx, "op-run-ps", "run-ps", "b", "s", "p", "boot-ps"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store1.CreateSession(ctx, "op-sess-ps", "boot-ps", storage.SessionRecord{
		ID: "sess-ps", RunID: "run-ps", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	// The operator adopts Controller A over HTTP (secret returned once)
	// BEFORE queueing: the bootstrap credential is provenance only.
	grantA := storyAdopt(t, srv1, "tok-ps1", "run-ps", harnessA, "conversation-A", "boot-ps")

	// Controller A holds only its lease and works through the restricted
	// interface.
	bridgeA, err := client.NewControllerBridge(dir, "run-ps", grantA)
	if err != nil {
		t.Fatalf("bridge A: %v", err)
	}

	// A connects first (adoption grants no connection state), then queues
	// the work prompt and a follow-up through the restricted interface.
	if _, err := bridgeA.Connect(ctx, "op-conn-a", 1); err != nil {
		t.Fatalf("A connect: %v", err)
	}
	ver, _ := store1.GetSessionVersion(ctx, "sess-ps")
	if _, err := bridgeA.QueuePrompt(ctx, "op-q-work", "sess-ps", "t-work", "primary story work", ver); err != nil {
		t.Fatalf("queue work: %v", err)
	}
	ver, _ = store1.GetSessionVersion(ctx, "sess-ps")
	if _, err := bridgeA.QueuePrompt(ctx, "op-q-next", "sess-ps", "t-next", "follow-up", ver); err != nil {
		t.Fatalf("queue follow-up: %v", err)
	}

	// Release through the bridge.
	ver, _ = store1.GetSessionVersion(ctx, "sess-ps")
	rel, err := bridgeA.ReleaseTurn(ctx, "op-rel-work", "sess-ps", "t-work", ver)
	if err != nil {
		t.Fatalf("A release: %v", err)
	}
	if rel.Replayed {
		t.Fatal("first release must be a new dispatch")
	}

	// The turn is durably dispatched (the fixture adapter may complete it
	// faster than this check runs; the attempt identity proves A's release).
	d, err := store1.GetTurnDetails(ctx, "sess-ps", "t-work")
	if err != nil {
		t.Fatalf("work turn must exist: %v", err)
	}
	if d.Status != council.TurnRunning && !isTerminalStatus(d.Status) {
		t.Fatalf("work turn must be dispatched, got %v", d.Status)
	}
	if d.AttemptID == "" {
		t.Fatal("dispatched turn must carry its attempt identity")
	}

	// A disconnects explicitly; authorized work continues and completes.
	if err := bridgeA.Disconnect(ctx, "op-disc-a", 1); err != nil {
		t.Fatalf("A disconnect: %v", err)
	}
	waitForTurnTerminal(t, store1, "sess-ps", "t-work")
	d, _ = store1.GetTurnDetails(ctx, "sess-ps", "t-work")
	if d.Status != council.TurnCompleted {
		t.Fatalf("authorized work must complete after disconnect: %v", d.Status)
	}
	// The follow-up stays queued; A's new decisions are fenced.
	if d, err := store1.GetTurnDetails(ctx, "sess-ps", "t-next"); err == nil && d != nil {
		t.Fatal("follow-up must remain queued")
	}
	if _, err := bridgeA.QueuePrompt(ctx, "op-q-a2", "sess-ps", "t-a2", "fenced", 5); err == nil {
		t.Fatal("disconnected controller must not queue")
	}

	// --- Restart: transitions repeat on a new instance ----------------
	_ = srv1.Close()
	_ = store1.Close()
	_ = lock1.Release()
	srv2, store2, lock2 := startStoryServer(t, dir, "inst-ps2", "tok-ps2")
	defer func() { _ = srv2.Close(); _ = _lockClose(lock2) }()

	// A rebuilds its transport from the new instance's discovery (a real
	// controller re-reads discovery after a service restart) and reattaches
	// explicitly — the persisted connected flag never carries over.
	bridgeA, err = client.NewControllerBridge(dir, "run-ps", grantA)
	if err != nil {
		t.Fatalf("bridge A transport refresh: %v", err)
	}
	if _, err := bridgeA.Connect(ctx, "op-conn-a2", 1); err != nil {
		t.Fatalf("A reattach after restart: %v", err)
	}
	grantB := storyHandoff(t, srv2, "tok-ps2", "run-ps", grantA, 1, harnessB, "conversation-B")

	// B connects explicitly and reads the existing records through its bridge.
	bridgeB, err := client.NewControllerBridge(dir, "run-ps", grantB)
	if err != nil {
		t.Fatalf("bridge B: %v", err)
	}
	if _, err := bridgeB.Connect(ctx, "op-conn-b", 2); err != nil {
		t.Fatalf("B connect: %v", err)
	}
	details, err := bridgeB.GetTurnDetails(ctx, "sess-ps", "t-work")
	if err != nil || details.Status != council.TurnCompleted {
		t.Fatalf("B must see the committed outcome: %+v err=%v", details, err)
	}

	// A's superseded authority is rejected on command and replay paths.
	if _, err := bridgeA.ReleaseTurn(ctx, "op-rel-work", "sess-ps", "t-work", 3); !isLeaseSuperseded(err) {
		t.Fatalf("A's replay must be fenced with lease_superseded, got %v", err)
	}
	if _, err := bridgeA.QueuePrompt(ctx, "op-q-a3", "sess-ps", "t-a3", "fenced", 6); !isLeaseSuperseded(err) {
		t.Fatalf("A's new command must be fenced with lease_superseded, got %v", err)
	}

	// Only B's explicit release starts the follow-up.
	nextVer, _ := store2.GetSessionVersion(ctx, "sess-ps")
	if _, err := bridgeB.ReleaseTurn(ctx, "op-rel-next", "sess-ps", "t-next", nextVer); err != nil {
		t.Fatalf("B must release the follow-up: %v", err)
	}
	d, err = store2.GetTurnDetails(ctx, "sess-ps", "t-next")
	if err != nil {
		t.Fatalf("follow-up turn must exist after B's release: %v", err)
	}
	if d.Status != council.TurnRunning && !isTerminalStatus(d.Status) {
		t.Fatalf("follow-up must be dispatched under B's release, got %v", d.Status)
	}
	_ = adapter.TurnRef{}
	_ = adaptertest.NewFakeAdapter("claude")
}

func _lockClose(l *service.ServiceLock) error { return l.Release() }

func startStoryServer(t *testing.T, dir, instanceID, token string) (*service.Server, *storage.Store, *service.ServiceLock) {
	t.Helper()
	lock, err := service.AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	srv, err := service.NewServerWithAdapter(store, lock, service.ServerConfig{StateDir: dir, InstanceID: instanceID, AuthToken: token}, adaptertest.NewFakeAdapter("claude"))
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	meta := service.DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      instanceID,
		Transport:       "unix",
		Endpoint:        srv.SocketPath(),
		StateDir:        dir,
		StartedAt:       time.Now().UTC(),
	}
	if err := service.PublishDiscovery(dir, meta, token); err != nil {
		t.Fatalf("discovery: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	return srv, store, lock
}

func storyPost(t *testing.T, socketPath, token, path, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}}}
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
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
		out.Write(raw)
	}
	return resp.StatusCode, out.String()
}

func storyAdopt(t *testing.T, srv *service.Server, token, runID, harness, ref, bootstrap string) string {
	t.Helper()
	code, body := storyPost(t, srv.SocketPath(), token, "/v1/runs/"+runID+"/controller/adopt",
		fmt.Sprintf(`{"op_id":"op-adopt-ps","harness":%q,"controller_ref":%q,"bootstrap_lease":%q}`, harness, ref, bootstrap))
	if code != http.StatusOK {
		t.Fatalf("adopt: %d %s", code, body)
	}
	var grant struct {
		LeaseSecret string `json:"lease_secret"`
	}
	_ = json.Unmarshal([]byte(body), &grant)
	if grant.LeaseSecret == "" {
		t.Fatal("adoption must return the secret once")
	}
	return grant.LeaseSecret
}

func storyHandoff(t *testing.T, srv *service.Server, token, runID, currentLease string, gen uint64, harness, ref string) string {
	t.Helper()
	code, body := storyPost(t, srv.SocketPath(), token, "/v1/runs/"+runID+"/controller/handoff",
		fmt.Sprintf(`{"op_id":"op-handoff-ps","current_lease":%q,"expected_generation":%d,"harness":%q,"controller_ref":%q}`, currentLease, gen, harness, ref))
	if code != http.StatusOK {
		t.Fatalf("handoff: %d %s", code, body)
	}
	var grant struct {
		LeaseSecret string `json:"lease_secret"`
	}
	_ = json.Unmarshal([]byte(body), &grant)
	return grant.LeaseSecret
}

func waitForTurnTerminal(t *testing.T, store *storage.Store, sessionID, turnKey string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		d, err := store.GetTurnDetails(context.Background(), sessionID, turnKey)
		if err == nil && d != nil {
			switch d.Status {
			case council.TurnCompleted, council.TurnFailed, council.TurnCancelled, council.TurnInterrupted:
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("turn did not reach a terminal state")
}

func isLeaseSuperseded(err error) bool {
	return err != nil && strings.Contains(err.Error(), "lease_superseded")
}

func isTerminalStatus(s council.TurnStatus) bool {
	switch s {
	case council.TurnCompleted, council.TurnFailed, council.TurnCancelled, council.TurnInterrupted:
		return true
	}
	return false
}
