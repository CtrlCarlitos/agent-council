package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func buildTestBinary(t *testing.T) string {
	t.Helper()
	binName := "council"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(t.TempDir(), binName)
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

	// runAndFail asserts the binary executed and exited non-zero, rather
	// than merely failing to start.
	runAndFail := func(t *testing.T, args ...string) {
		t.Helper()
		out, err := exec.Command(bin, args...).CombinedOutput()
		if err == nil {
			t.Fatalf("expected %v to fail, output: %s", args, string(out))
		}
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("%v did not execute: %v (out: %s)", args, err, string(out))
		}
	}

	// Status when not running fails
	runAndFail(t, "service", "status", "--state-dir", dir)

	// Stop when not running fails
	runAndFail(t, "service", "stop", "--state-dir", dir)

	// Unknown subcommand fails
	runAndFail(t, "service", "invalid-subcmd")
}
