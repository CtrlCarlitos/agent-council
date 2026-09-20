package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func buildTestBinary(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "council")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build test binary: %v, out: %s", err, string(out))
	}
	return binPath
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestCLI_ServiceStartStatusStopLifecycle(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t)

	// 1. council service start launches detached background service
	ctxStart, cancelStart := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStart()

	cmdStart := exec.CommandContext(ctxStart, bin, "service", "start", "--state-dir", dir)
	out, err := cmdStart.CombinedOutput()
	if err != nil {
		t.Fatalf("council service start failed: %v, out: %s", err, string(out))
	}

	// Always ensure cleanup on test exit
	defer func() {
		ctxClean, cancelClean := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelClean()
		_ = exec.CommandContext(ctxClean, bin, "service", "stop", "--state-dir", dir, "--timeout", "3s").Run()
	}()

	// 2. council service status reports ready and decodes expected diagnostics
	ctxStatus, cancelStatus := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelStatus()
	cmdStatus := exec.CommandContext(ctxStatus, bin, "service", "status", "--state-dir", dir)
	outStatus, err := cmdStatus.CombinedOutput()
	if err != nil {
		t.Fatalf("council service status failed: %v, out: %s", err, string(outStatus))
	}

	// Verify status output contains required diagnostic fields
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

	// Verify runtime discovery and socket are deleted, while service.lock remains on disk
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

func TestCLI_ServiceErrorsAndEdgeCases(t *testing.T) {
	dir := t.TempDir()
	bin := buildTestBinary(t)

	// Status when not running fails
	cmdStatus := exec.Command(bin, "service", "status", "--state-dir", dir)
	if err := cmdStatus.Run(); err == nil {
		t.Fatal("expected status to fail when service not running")
	}

	// Stop when not running fails
	cmdStop := exec.Command(bin, "service", "stop", "--state-dir", dir)
	if err := cmdStop.Run(); err == nil {
		t.Fatal("expected stop to fail when service not running")
	}

	// Unknown subcommand fails
	cmdUnknown := exec.Command(bin, "service", "invalid-subcmd")
	if err := cmdUnknown.Run(); err == nil {
		t.Fatal("expected invalid subcommand to fail")
	}
}
