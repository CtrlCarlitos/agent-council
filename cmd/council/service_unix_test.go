//go:build unix

package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// CLI lifecycle: start launches a detached service, status reports ready
// diagnostics, stop tears down and unlinks runtime discovery while the lock
// file remains on disk.
func TestCLI_ServiceStartStatusStopLifecycle(t *testing.T) {
	dir := shortStateDir(t)
	bin := buildTestBinary(t)

	defer stopBackgroundService(t, bin, dir)

	// 1. council service start launches detached background service
	ctxStart, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStart()

	cmdStart := exec.CommandContext(ctxStart, bin, "service", "start", "--state-dir", dir)
	out, err := cmdStart.CombinedOutput()
	if err != nil {
		t.Fatalf("council service start failed: %v, out: %s", err, string(out))
	}

	// 2. council service status reports ready and decodes expected diagnostics
	ctxStatus, cancelStatus := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStatus()
	cmdStatus := exec.CommandContext(ctxStatus, bin, "service", "status", "--state-dir", dir)
	outStatus, err := cmdStatus.CombinedOutput()
	if err != nil {
		t.Fatalf("council service status failed: %v, out: %s", err, string(outStatus))
	}

	var statusDiag struct {
		Status      string `json:"status"`
		InstanceID  string `json:"instance_id"`
		StateDir    string `json:"state_dir"`
		LiveWorkers int    `json:"live_workers"`
	}
	if err := json.Unmarshal(outStatus, &statusDiag); err != nil {
		t.Fatalf("failed to decode JSON status: %v, raw: %s", err, string(outStatus))
	}
	if statusDiag.Status != "ready" || statusDiag.StateDir != dir || statusDiag.LiveWorkers != 0 {
		t.Fatalf("unexpected status output: %+v", statusDiag)
	}

	// 3. council service stop stops the background service and cleans up
	ctxStop, cancelStop := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStop()
	cmdStop := exec.CommandContext(ctxStop, bin, "service", "stop", "--state-dir", dir, "--timeout", "5s")
	outStop, err := cmdStop.CombinedOutput()
	if err != nil {
		t.Fatalf("council service stop failed: %v, out: %s", err, string(outStop))
	}

	sockPath := filepath.Join(dir, "council.sock")
	tokenPath := filepath.Join(dir, "auth.token")
	lockPath := filepath.Join(dir, "service.lock")
	if fileExists(sockPath) || fileExists(tokenPath) {
		t.Fatal("runtime socket and token must be unlinked after CLI stop")
	}
	if !fileExists(lockPath) {
		t.Fatal("service.lock must remain on disk after CLI stop")
	}
}
