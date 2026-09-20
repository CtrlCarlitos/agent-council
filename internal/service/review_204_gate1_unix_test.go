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

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Gate 1 review regressions at the HTTP/coordinator boundary (head
// 204b564): a historical receipt must never recreate connection authority,
// and the decision gate compares the exact durable episode.

func gate1ServerFixture(t *testing.T, dir, instanceID, authToken string) (*Server, *storage.Store, *ServiceLock) {
	t.Helper()
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	srv, err := NewServer(store, lock, ServerConfig{StateDir: dir, InstanceID: instanceID, AuthToken: authToken})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	return srv, store, lock
}

func gate1HTTPConnect(t *testing.T, srv *Server, authToken, runID, lease string, gen uint64, opID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := newTestClient(srv.SocketPath())
	body := fmt.Sprintf(`{"op_id":%q,"controller_lease":%q,"expected_generation":%d}`, opID, lease, gen)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/"+runID+"/controller/connect", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect: %d", resp.StatusCode)
	}
}

func gate1Decision(t *testing.T, srv *Server, authToken, runID, lease string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := newTestClient(srv.SocketPath())
	body := fmt.Sprintf(`{"op_id":"op-dec-gate","controller_lease":%q,"artifact_id":"a","revision":1,"decision_payload":"p"}`, lease)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/"+runID+"/decisions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("decision: %v", err)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	resp.Body.Close()
	t.Logf("decision status %d code %q", resp.StatusCode, env.Error.Code)
	return resp.StatusCode
}

// A fresh service instance receiving a replayed historical connect does not
// mark attachment: decisions stay fenced until explicit reattachment.
func TestGate1Review204_RestartReplayIsNotReattachment(t *testing.T) {
	dir := testStateDir(t)
	ctx := context.Background()

	srv1, store1, lock1 := gate1ServerFixture(t, dir, "inst-s1", "tok-s1")
	if _, err := store1.CreateRun(ctx, "op-run-s", "run-s", "b", "s", "p", "boot-A"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adoptForTest(t, store1, "run-s", "boot-A")
	// The historical connect committed on instance 1.
	gate1HTTPConnect(t, srv1, "tok-s1", "run-s", "boot-A", 1, "op-conn-hist")
	_ = srv1.Close()
	_ = store1.Close()
	_ = lock1.Release()

	srv2, store2, lock2 := gate1ServerFixture(t, dir, "inst-s2", "tok-s2")
	defer func() { _ = srv2.Close(); _ = store2.Close(); _ = lock2.Release() }()

	// Replay the same connect operation on the fresh instance: the durable
	// receipt returns, but no attachment is established.
	gate1HTTPConnect(t, srv2, "tok-s2", "run-s", "boot-A", 1, "op-conn-hist")
	if code := gate1Decision(t, srv2, "tok-s2", "run-s", "boot-A"); code != http.StatusConflict {
		t.Fatalf("historical connect replay must not authorize decisions on a fresh instance, got %d", code)
	}

	// Explicit reattachment with a fresh operation restores decisions.
	gate1HTTPConnect(t, srv2, "tok-s2", "run-s", "boot-A", 1, "op-conn-fresh")
	if code := gate1Decision(t, srv2, "tok-s2", "run-s", "boot-A"); code == http.StatusConflict {
		t.Fatalf("explicit reattachment must restore decisions, got %d", code)
	}
}

// Replaying A's successful disconnect after B connected leaves B's
// in-instance record intact.
func TestGate1Review204_DisconnectReplayAfterSuccessorConnect(t *testing.T) {
	dir := testStateDir(t)
	ctx := context.Background()
	srv, store, lockD := gate1ServerFixture(t, dir, "inst-d", "tok-d")
	defer func() { _ = srv.Close(); _ = store.Close(); _ = lockD.Release() }()
	if _, err := store.CreateRun(ctx, "op-run-d", "run-d", "b", "s", "p", "boot-A"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adoptForTest(t, store, "run-d", "boot-A")

	gate1HTTPConnect(t, srv, "tok-d", "run-d", "boot-A", 1, "op-conn-a")
	recA, err := store.GetControllerRecord(ctx, "run-d")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	episodeA := recA.AttachmentID
	// A disconnects explicitly.
	gate1HTTPDisconnect(t, srv, store, "tok-d", "run-d", "boot-A", 1, "op-disc-a", episodeA)
	// B (same generation, reconnect) establishes a new episode.
	gate1HTTPConnect(t, srv, "tok-d", "run-d", "boot-A", 1, "op-conn-b")

	// Replay A's already-successful disconnect: receipt returns, B stays.
	gate1HTTPDisconnect(t, srv, store, "tok-d", "run-d", "boot-A", 1, "op-disc-a", episodeA)
	if code := gate1Decision(t, srv, "tok-d", "run-d", "boot-A"); code == http.StatusConflict {
		t.Fatal("replayed stale disconnect must not clear the successor's attachment")
	}
}

// A delayed connect handler from an older generation cannot overwrite the
// replacement controller's in-instance record.
func TestGate1Review204_DelayedConnectAfterHandoff(t *testing.T) {
	dir := testStateDir(t)
	ctx := context.Background()
	srv, store, lockH := gate1ServerFixture(t, dir, "inst-h", "tok-h")
	defer func() { _ = srv.Close(); _ = store.Close(); _ = lockH.Release() }()
	if _, err := store.CreateRun(ctx, "op-run-h", "run-h", "b", "s", "p", "boot-A"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adoptForTest(t, store, "run-h", "boot-A")

	// Handoff to generation 2, and B attaches.
	if _, err := store.HandoffController(ctx, "op-handoff-h", "run-h", "boot-A", 1, "codex", "ref-B", "lease-B"); err != nil {
		t.Fatalf("handoff: %v", err)
	}
	gate1HTTPConnect(t, srv, "tok-h", "run-h", "lease-B", 2, "op-conn-b2")

	// A delayed generation-1 connect publication attempts to overwrite.
	srv.Coordinator().MarkControllerAttached("run-h", 1, "stale-episode", srv.InstanceID())

	// The gate consults the durable generation and exact episode: the stale
	// mark cannot authorize A's decisions.
	rec, err := store.GetControllerRecord(ctx, "run-h")
	if err != nil || rec.Generation != 2 {
		t.Fatalf("durable generation must remain 2: %+v err=%v", rec, err)
	}
	if srv.Coordinator().ControllerAttachedEpisode("run-h", 2, rec.AttachmentID) == false {
		t.Fatal("B's in-instance record must survive the stale generation-1 publication")
	}
	// And the stale generation-1 record does not satisfy the gate for gen 2.
	if srv.Coordinator().ControllerAttachedEpisode("run-h", 2, "stale-episode") {
		t.Fatal("generation mismatch must not authorize")
	}
}

// The decision gate compares the exact durable attachment identity: a stale
// local episode of the same generation does not authorize.
func TestGate1Review204_GateComparesExactEpisode(t *testing.T) {
	dir := testStateDir(t)
	ctx := context.Background()
	srv, store, lockE := gate1ServerFixture(t, dir, "inst-e", "tok-e")
	defer func() { _ = srv.Close(); _ = store.Close(); _ = lockE.Release() }()
	if _, err := store.CreateRun(ctx, "op-run-e", "run-e", "b", "s", "p", "boot-A"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adoptForTest(t, store, "run-e", "boot-A")

	// Durable: episode B (via reconnect).
	gate1HTTPConnect(t, srv, "tok-e", "run-e", "boot-A", 1, "op-conn-eb")
	// Local: overwrite with a forged stale episode of the same generation.
	srv.Coordinator().MarkControllerAttached("run-e", 1, "forged-episode", srv.InstanceID())

	if code := gate1Decision(t, srv, "tok-e", "run-e", "boot-A"); code != http.StatusConflict {
		t.Fatalf("local/durable episode mismatch must fence decisions, got %d", code)
	}
}

func gate1HTTPDisconnect(t *testing.T, srv *Server, store *storage.Store, authToken, runID, lease string, gen uint64, opID, attachmentID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := newTestClient(srv.SocketPath())
	body := fmt.Sprintf(`{"op_id":%q,"controller_lease":%q,"expected_generation":%d,"attachment_id":%q}`, opID, lease, gen, attachmentID)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/"+runID+"/controller/disconnect", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+authToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disconnect: %d", resp.StatusCode)
	}
}
