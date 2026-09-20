//go:build windows

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// Native Windows is an explicitly unsupported platform for service lifetime
// commands until named-pipe IPC and LockFileEx exclusivity are implemented
// and verified. These tests pin the unsupported-service contract: commands
// fail with clear errors rather than silently degrading. Cross-platform
// storage and adapter contracts are exercised by their own package tests on
// this platform.
func TestWindows_ServiceCommandsUnsupported(t *testing.T) {
	bin := buildTestBinary(t)
	dir := t.TempDir()

	cases := []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{"start", []string{"service", "start", "--state-dir", dir}, "not supported on Windows"},
		{"run", []string{"service", "run", "--state-dir", dir}, "not supported on this platform"},
		{"status", []string{"service", "status", "--state-dir", dir}, "unsupported on windows"},
		{"stop", []string{"service", "stop", "--state-dir", dir}, "unsupported on windows"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command(bin, tc.args...).CombinedOutput()
			if err == nil {
				t.Fatalf("expected %v to fail on Windows, output: %s", tc.args, string(out))
			}
			if _, ok := err.(*exec.ExitError); !ok {
				t.Fatalf("%v did not execute: %v (out: %s)", tc.args, err, string(out))
			}
			if !strings.Contains(strings.ToLower(string(out)), strings.ToLower(tc.wantMsg)) {
				t.Fatalf("expected error mentioning %q, got (err %v): %s", tc.wantMsg, err, string(out))
			}
		})
	}
}
