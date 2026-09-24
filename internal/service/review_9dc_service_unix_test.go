//go:build unix

package service

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Regression evidence for PR #29 review round 3 (head 9dc6c5d).

// An authenticated handler stalled before admission (decoding its request
// body) is not a tracked control task. When it outlives the final teardown
// deadline, http.Server.Shutdown returns an incomplete-shutdown error; the
// orderly path must not close storage in that state even though the task
// WaitGroup has finished. The forced-termination path must surface instead.
func TestReview9DC_HTTPShutdownTimeoutForcesExit(t *testing.T) {
	h := newTestHarness(t)

	// Open an authenticated request and stall before completing the body.
	conn, err := net.Dial("unix", h.socketPath)
	if err != nil {
		t.Fatalf("dial service socket: %v", err)
	}
	defer conn.Close()

	req := "POST /v1/runs/run-1/sessions/sess-1/prompts/queue HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Authorization: Bearer " + h.cfg.AuthToken + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: 100\r\n\r\n" +
		`{"op_id":"op-stall"`
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write stalled request: %v", err)
	}

	// Give the service time to accept the connection and enter the handler.
	time.Sleep(150 * time.Millisecond)

	err = h.server.Teardown(150 * time.Millisecond)
	if err == nil {
		t.Fatal("teardown with an unjoined HTTP handler must not report orderly success")
	}
	if !strings.Contains(err.Error(), "forced") {
		t.Fatalf("expected forced-termination error, got %v", err)
	}

	// The forced outcome surfaces through WaitForShutdown rather than the
	// orderly notification.
	if werr := h.server.WaitForShutdown(context.Background()); werr == nil || !strings.Contains(werr.Error(), "forced") {
		t.Fatalf("expected forced error from WaitForShutdown, got %v", werr)
	}

	// Drain the stalled connection so the harness cleanup can proceed.
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, _ = bufio.NewReader(conn).ReadString('\n')
}

// End-to-end recovery reconstruction: a binding saved with a nonempty
// object-form configuration must reach ResumeSession exactly as originally
// configured. The fake adapter rejects any resume whose configuration
// differs from the session it created, so a successful reconcile proves the
// faithful reconstruction.
func TestReview9DC_ReconcileResumesExactSavedConfiguration(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	originalConfig := adapter.SessionConfig{
		WorkspaceRoot: "/work/repo",
		Model:         "model-x",
		Tooling:       []string{"git", "go"},
	}
	// The strict native side: ResumeSession compares the full binding.
	if _, err := h.adapter.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID("sess-1"),
		Contributor: "claude",
		Config:      originalConfig,
	}); err != nil {
		t.Fatalf("create adapter session: %v", err)
	}

	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-res", "prompt")
	bindRec, err := h.store.SetNativeBinding(ctx, "op-bind-9dc", "lease-1", "sess-1", ver, storage.NativeBinding{
		LogicalSessionID: "sess-1",
		NativeSessionID:  "native-sess-1",
		Harness:          "claude",
		Model:            "model-x",
		WorkspaceMode:    "branch",
		ToolingConfig:    `{"workspace_root":"/work/repo","model":"model-x","tools":["git","go"]}`,
	})
	if err != nil {
		t.Fatalf("set native binding: %v", err)
	}
	if _, err := h.store.ReleaseTurn(ctx, "op-rel-9dc", "lease-1", "sess-1", bindRec.CommittedVersion, "turn-res"); err != nil {
		t.Fatalf("release turn: %v", err)
	}

	recBody := fmt.Sprintf(`{"op_id":"op-rec-9dc","controller_lease":"lease-1","expected_version":%d}`, bindRec.CommittedVersion+1)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-res/reconcile", strings.NewReader(recBody))
	req.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("reconcile request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on reconcile with faithful configuration, got %d", resp.StatusCode)
	}

	// The turn resolves from the fake's missing-execution evidence.
	details, err := h.store.GetTurnDetails(ctx, "sess-1", "turn-res")
	if err != nil {
		t.Fatalf("get turn details: %v", err)
	}
	if details.Status == "" {
		t.Fatal("expected a committed reconciliation outcome")
	}
}
