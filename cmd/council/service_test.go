package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
