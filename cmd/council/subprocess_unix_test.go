//go:build unix

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

// shortStateDir returns a state directory with a bounded path length; macOS
// limits unix socket paths to ~104 bytes (sun_path), which t.TempDir()
// directories routinely exceed.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ac-cli-")
	if err != nil {
		t.Fatalf("create short state dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(dir)
	})
	return dir
}

func stopBackgroundService(t *testing.T, bin, dir string) {
	t.Helper()
	ctxClean, cancelClean := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelClean()
	_ = exec.CommandContext(ctxClean, bin, "service", "stop", "--state-dir", dir, "--timeout", "5s").Run()
}

// Ownership smoke: two launcher processes race for the same state directory;
// exactly one wins and the survivor serves status. (Component evidence for
// exclusivity; it does not exercise worker execution.)
func TestSubprocess_DualLauncherRace(t *testing.T) {
	dir := shortStateDir(t)
	bin := buildTestBinary(t)

	defer stopBackgroundService(t, bin, dir)

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

	cmdStatus := exec.Command(bin, "service", "status", "--state-dir", dir)
	outStatus, err := cmdStatus.CombinedOutput()
	if err != nil {
		t.Fatalf("status failed after race: %v, out: %s", err, string(outStatus))
	}
}

// Crash-restart reservation smoke: a pre-seeded running reservation survives
// SIGKILL and restart as unresolved, without redispatch. (No service-owned
// executing worker is crashed here; see the acceptance subprocess tests for
// independent-execution recovery evidence.)
func TestSubprocess_ServiceCrashAndRestart(t *testing.T) {
	dir := shortStateDir(t)
	bin := buildTestBinary(t)

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile", "lease-1"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID:                  "sess-1",
		RunID:               "run-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	qRec, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-crash",
		Prompt:    "work before crash",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-crash", "lease-1", "sess-1", qRec.CommittedVersion, "t-crash"); err != nil {
		t.Fatalf("release turn: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	cmdStart := exec.Command(bin, "service", "start", "--state-dir", dir)
	outStart, err := cmdStart.CombinedOutput()
	if err != nil {
		t.Fatalf("service start: %v, out: %s", err, string(outStart))
	}

	metaPath := filepath.Join(dir, "service.json")
	metaData, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("read service.json: %v", err)
	}
	var meta service.DiscoveryMeta
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("unmarshal service.json: %v", err)
	}

	if err := syscall.Kill(meta.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill service pid %d: %v", meta.PID, err)
	}

	for i := 0; i < 100; i++ {
		if err := syscall.Kill(meta.PID, 0); err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	lockPath := filepath.Join(dir, "service.lock")
	if !fileExists(lockPath) {
		t.Fatal("service.lock must remain on disk after crash")
	}

	cmdRestart := exec.Command(bin, "service", "start", "--state-dir", dir)
	outRestart, err := cmdRestart.CombinedOutput()
	if err != nil {
		t.Fatalf("service restart: %v, out: %s", err, string(outRestart))
	}
	defer stopBackgroundService(t, bin, dir)

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

// Idle signal smoke: an empty service exits cleanly on SIGTERM with
// discovery cleanup and lock retention. (No active execution exists here, so
// grace-period expiry behavior is not exercised.)
func TestSubprocess_RealOSSignalGrace(t *testing.T) {
	dir := shortStateDir(t)
	bin := buildTestBinary(t)

	cmd := exec.Command(bin, "service", "run", "--state-dir", dir)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start service run: %v", err)
	}

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

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("send SIGTERM: %v", err)
	}

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

	tokenPath := filepath.Join(dir, "auth.token")
	lockPath := filepath.Join(dir, "service.lock")
	if fileExists(sockPath) || fileExists(tokenPath) {
		t.Fatal("socket and token must be unlinked on graceful shutdown")
	}
	if !fileExists(lockPath) {
		t.Fatal("service.lock must remain on disk after shutdown")
	}
}

// Disconnect survival smoke (pre-seeded unresolved row): closing an SSE
// response body does not stop a service holding a previously unresolved
// turn. (No worker executes and no client process is terminated here; see
// the acceptance subprocess tests for the full work-survival sequence.)
func TestSubprocess_ClientDisconnectMidTurn(t *testing.T) {
	dir := shortStateDir(t)
	bin := buildTestBinary(t)

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile", "lease-1"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID:                  "sess-1",
		RunID:               "run-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	qRec, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-disc",
		Prompt:    "disconnect test",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-disc", "lease-1", "sess-1", qRec.CommittedVersion, "t-disc"); err != nil {
		t.Fatalf("release turn: %v", err)
	}
	if _, err := store.RecordDispatchObservation(ctx, "op-obs-disc", "lease-1", "sess-1", "t-disc", "receipt_acknowledged"); err != nil {
		t.Fatalf("record dispatch observation: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	cmdStart := exec.Command(bin, "service", "start", "--state-dir", dir)
	if out, err := cmdStart.CombinedOutput(); err != nil {
		t.Fatalf("service start: %v, out: %s", err, string(out))
	}
	defer stopBackgroundService(t, bin, dir)

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
	_ = resp.Body.Close()

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
