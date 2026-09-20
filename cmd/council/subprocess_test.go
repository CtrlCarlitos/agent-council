package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/client"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/service"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// 1. Real Dual Launcher Race: Two launcher processes concurrently starting
// against the same state directory; exactly one wins, loser fails.
func TestSubprocess_DualLauncherRace(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t)

	var wg sync.WaitGroup
	wg.Add(2)

	var err1, err2 error
	var out1, out2 []byte

	go func() {
		defer wg.Done()
		cmd1 := exec.Command(bin, "service", "start", "--state-dir", dir)
		out1, err1 = cmd1.CombinedOutput()
	}()

	go func() {
		defer wg.Done()
		cmd2 := exec.Command(bin, "service", "start", "--state-dir", dir)
		out2, err2 = cmd2.CombinedOutput()
	}()

	wg.Wait()

	// Exactly one launcher must succeed
	successCount := 0
	if err1 == nil {
		successCount++
	}
	if err2 == nil {
		successCount++
	}

	if successCount != 1 {
		t.Fatalf("expected exactly 1 winner in concurrent service start race, got %d (err1: %v, out1: %s; err2: %v, out2: %s)",
			successCount, err1, string(out1), err2, string(out2))
	}

	// Always ensure cleanup
	defer func() {
		ctxClean, cancelClean := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClean()
		_ = exec.CommandContext(ctxClean, bin, "service", "stop", "--state-dir", dir, "--timeout", "3s").Run()
	}()

	// Status command confirms service is ready
	cmdStatus := exec.Command(bin, "service", "status", "--state-dir", dir)
	outStatus, err := cmdStatus.CombinedOutput()
	if err != nil {
		t.Fatalf("status failed after race: %v, out: %s", err, string(outStatus))
	}
}

// 2. Real Service Crash & Restart: Kill service process with SIGKILL;
// external ledger survives; restart recovers without redispatch.
func TestSubprocess_ServiceCrashAndRestart(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t)

	// Pre-seed storage with a run, session, and turn
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile", "lease-1")
	sessRec, _ := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID:                  "sess-1",
		RunID:               "run-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	qRec, _ := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-crash",
		Prompt:    "work before crash",
	})
	_, err = store.ReleaseTurn(ctx, "op-rel-crash", "lease-1", "sess-1", qRec.CommittedVersion, "t-crash")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}
	_ = store.Close()

	// 1. Start service subprocess
	cmdStart := exec.Command(bin, "service", "start", "--state-dir", dir)
	outStart, err := cmdStart.CombinedOutput()
	if err != nil {
		t.Fatalf("service start: %v, out: %s", err, string(outStart))
	}

	// Read discovery metadata to find PID
	metaPath := filepath.Join(dir, "service.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read service.json: %v", err)
	}
	var meta service.DiscoveryMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("unmarshal service.json: %v", err)
	}

	// 2. Kill service subprocess with SIGKILL (simulating hard ungraceful crash)
	if err := syscall.Kill(meta.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill service pid %d: %v", meta.PID, err)
	}

	// Wait for process to fully exit
	for i := 0; i < 100; i++ {
		if err := syscall.Kill(meta.PID, 0); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Invariant: service.lock must remain on disk after SIGKILL
	lockPath := filepath.Join(dir, "service.lock")
	if !fileExists(lockPath) {
		t.Fatal("service.lock must remain on disk after crash")
	}

	// 3. Restart service against same state directory
	cmdRestart := exec.Command(bin, "service", "start", "--state-dir", dir)
	outRestart, err := cmdRestart.CombinedOutput()
	if err != nil {
		t.Fatalf("service restart: %v, out: %s", err, string(outRestart))
	}
	defer func() {
		ctxClean, cancelClean := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClean()
		_ = exec.CommandContext(ctxClean, bin, "service", "stop", "--state-dir", dir, "--timeout", "3s").Run()
	}()

	// Query status after restart
	c, err := client.New(dir)
	if err != nil {
		t.Fatalf("client new: %v", err)
	}
	ctxStatus, cancelStatus := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelStatus()
	status, err := c.GetStatus(ctxStatus)
	if err != nil {
		t.Fatalf("get status after restart: %v", err)
	}
	if status.ReservedTurns != 1 || status.UnresolvedTurns != 1 {
		t.Fatalf("expected 1 reserved and 1 unresolved turn preserved after crash restart, got: %+v", status)
	}
}

// 3. Real OS Signal Grace: Send real SIGTERM to service subprocess;
// verify graceful draining, discovery cleanup, and lock retention.
func TestSubprocess_RealOSSignalGrace(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t)

	// Run foreground service subprocess
	cmd := exec.Command(bin, "service", "run", "--state-dir", dir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start service run: %v", err)
	}

	// Poll until service is ready
	sockPath := filepath.Join(dir, "council.sock")
	ready := false
	for i := 0; i < 100; i++ {
		if fileExists(sockPath) {
			c, err := client.New(dir)
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				_, err := c.GetReadiness(ctx)
				cancel()
				if err == nil {
					ready = true
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		_ = cmd.Process.Kill()
		t.Fatal("service did not become ready in time")
	}

	// Send real SIGTERM
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("send SIGTERM: %v", err)
	}

	// Wait for process to exit cleanly
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("process exited with error on SIGTERM: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("timed out waiting for service to exit cleanly on SIGTERM")
	}

	// Invariants: discovery unlinked, service.lock retained
	tokenPath := filepath.Join(dir, "auth.token")
	lockPath := filepath.Join(dir, "service.lock")
	if fileExists(sockPath) || fileExists(tokenPath) {
		t.Fatal("socket and token must be unlinked on graceful shutdown")
	}
	if !fileExists(lockPath) {
		t.Fatal("service.lock must remain on disk after shutdown")
	}
}

// 4. Real Client Disconnect: Client process terminated mid-turn;
// service survives; second process queries result.
func TestSubprocess_ClientDisconnectMidTurn(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t)

	// Pre-seed storage
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	_, _ = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile", "lease-1")
	sessRec, _ := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID:                  "sess-1",
		RunID:               "run-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	qRec, _ := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-disc",
		Prompt:    "disconnect test",
	})
	_, err = store.ReleaseTurn(ctx, "op-rel-disc", "lease-1", "sess-1", qRec.CommittedVersion, "t-disc")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}
	_, err = store.RecordDispatchObservation(ctx, "op-obs-disc", "lease-1", "sess-1", "t-disc", "receipt_acknowledged")
	if err != nil {
		t.Fatalf("record dispatch observation: %v", err)
	}
	_ = store.Close()

	// Start background service
	cmdStart := exec.Command(bin, "service", "start", "--state-dir", dir)
	if out, err := cmdStart.CombinedOutput(); err != nil {
		t.Fatalf("service start: %v, out: %s", err, string(out))
	}
	defer func() {
		ctxClean, cancelClean := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClean()
		_ = exec.CommandContext(ctxClean, bin, "service", "stop", "--state-dir", dir, "--timeout", "3s").Run()
	}()

	// Client 1: connect to event stream and disconnect mid-turn
	c1, err := client.New(dir)
	if err != nil {
		t.Fatalf("client 1 new: %v", err)
	}

	url := "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-disc/events"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+c1.Token())
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c1.HTTPClient().Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("stream connect failed: %v (code: %v)", err, resp.StatusCode)
	}
	// Abruptly terminate connection mid-stream
	_ = resp.Body.Close()

	// Verify service process survives
	c2, err := client.New(dir)
	if err != nil {
		t.Fatalf("client 2 new: %v", err)
	}
	ctxStatus, cancelStatus := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelStatus()
	status, err := c2.GetStatus(ctxStatus)
	if err != nil {
		t.Fatalf("client 2 get status failed (service may have crashed): %v", err)
	}
	if status.Status != "ready" {
		t.Fatalf("expected service status ready, got: %s", status.Status)
	}
	if status.ReservedTurns != 1 || status.UnresolvedTurns != 1 {
		t.Fatalf("expected preserved unresolved turn after disconnect: %+v", status)
	}

	// Client 2 can query turn state
	reqGet, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-disc", nil)
	if err != nil {
		t.Fatalf("new get request: %v", err)
	}
	reqGet.Header.Set("Authorization", "Bearer "+c2.Token())
	respGet, err := c2.HTTPClient().Do(reqGet)
	if err != nil || respGet.StatusCode != http.StatusOK {
		t.Fatalf("client 2 get turn failed: %v (code: %v)", err, respGet.StatusCode)
	}
	defer respGet.Body.Close()

	var details storage.TurnDetails
	if err := json.NewDecoder(respGet.Body).Decode(&details); err != nil {
		t.Fatalf("decode turn details: %v", err)
	}
	if details.TurnKey != "t-disc" {
		t.Fatalf("unexpected turn details: %+v", details)
	}
	if details.Status != council.TurnRunning {
		t.Fatalf("expected turn status running, got: %s", details.Status)
	}
}
