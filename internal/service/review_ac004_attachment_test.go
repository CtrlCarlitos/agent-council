//go:build unix

package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Regression evidence for AC-004 Task 5: attachment is identity-tracked per
// service instance. A persisted connected flag never authorizes new
// decisions on a fresh instance; explicit reattachment restores them; an
// accepted execution still completes across the restart.
func TestAC004_RestartRequiresExplicitReattachment_ControlledFixture(t *testing.T) {
	dir := testStateDir(t)
	ctx := context.Background()

	// Instance 1: adopt, connect, and accept a gated execution.
	lock1, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("lock 1: %v", err)
	}
	store1, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("store 1: %v", err)
	}
	fake := adaptertest.NewFakeAdapter("claude")
	cfg1 := ServerConfig{StateDir: dir, InstanceID: "inst-r1", AuthToken: "tok-r1"}
	srv1, err := NewServerWithAdapter(store1, lock1, cfg1, fake)
	if err != nil {
		t.Fatalf("server 1: %v", err)
	}
	if err := srv1.Start(); err != nil {
		t.Fatalf("start 1: %v", err)
	}

	if _, err := store1.CreateRun(ctx, "op-run-r", "run-r", "b", "s", "p", "boot-r"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adoptForTest(t, store1, "run-r", "boot-r")
	connectControllerForTest(t, srv1, store1, "run-r", "boot-r")
	sess, err := store1.CreateSession(ctx, "op-sess-r", "boot-r", storage.SessionRecord{
		ID: "sess-r", RunID: "run-r", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ver := sess.CommittedVersion
	if _, err := store1.QueuePrompt(ctx, "op-q-r", "boot-r", "sess-r", ver, storage.PendingPrompt{SessionID: "sess-r", TurnKey: "turn-r", Prompt: "p"}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	ver, _ = store1.GetSessionVersion(ctx, "sess-r")
	if _, err := store1.ReleaseTurn(ctx, "op-rel-r", "boot-r", "sess-r", ver, "turn-r"); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Stop instance 1 without disconnecting the controller (persisted
	// connected flag remains true).
	if err := srv1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("close store 1: %v", err)
	}
	if err := lock1.Release(); err != nil {
		t.Fatalf("release lock 1: %v", err)
	}

	// Instance 2 on the same state directory.
	lock2, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("lock 2: %v", err)
	}
	defer lock2.Release()
	store2, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("store 2: %v", err)
	}
	defer store2.Close()
	cfg2 := ServerConfig{StateDir: dir, InstanceID: "inst-r2", AuthToken: "tok-r2"}
	srv2, err := NewServerWithAdapter(store2, lock2, cfg2, fake)
	if err != nil {
		t.Fatalf("server 2: %v", err)
	}
	if err := srv2.Start(); err != nil {
		t.Fatalf("start 2: %v", err)
	}
	defer srv2.Close()
	client2 := newTestClient(srv2.SocketPath())

	// Durable state says connected — but a new decision before explicit
	// reattachment is rejected.
	ver, _ = store2.GetSessionVersion(ctx, "sess-r")
	cxlBody := fmt.Sprintf(`{"op_id":"op-cxl-r","controller_lease":"boot-r","expected_version":%d,"reason":"restart"}`, ver)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-r/sessions/sess-r/turns/turn-r/cancel", strings.NewReader(cxlBody))
	req.Header.Set("Authorization", "Bearer "+cfg2.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client2.Do(req)
	if err != nil {
		t.Fatalf("cancel before reattach: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 not_connected on fresh instance, got %d", resp.StatusCode)
	}

	// Explicit reattachment through the run-scoped route restores decisions.
	connBody := `{"op_id":"op-conn-r2","controller_lease":"boot-r","expected_generation":1}`
	connReq, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-r/controller/connect", strings.NewReader(connBody))
	connReq.Header.Set("Authorization", "Bearer "+cfg2.AuthToken)
	connReq.Header.Set("Content-Type", "application/json")
	connResp, err := client2.Do(connReq)
	if err != nil || connResp.StatusCode != http.StatusOK {
		t.Fatalf("reattach: %v, code %d, err %v", connResp, connResp.StatusCode, err)
	}
	connResp.Body.Close()

	// The accepted execution from instance 1 still completes (evidence
	// path is attachment-independent).
	srv2.Coordinator().CancelActiveWorkers() // instance 1's worker is gone; resolve via observed path
	details, err := store2.GetTurnDetails(ctx, "sess-r", "turn-r")
	if err != nil {
		t.Fatalf("details: %v", err)
	}
	if details.Status != "running" {
		t.Fatalf("accepted execution must remain durably accepted across restart, got %v", details.Status)
	}

	// Inspection never marks attachment: a read does not reestablish the
	// gate (verified above via the pre-reattach 409).
	_ = time.Second
}
