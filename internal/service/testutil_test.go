//go:build unix

package service

import (
	"os"
	"path/filepath"
	"testing"
)

// newTestLock acquires a service lock for testing.
func newTestLock(stateDir string) (*ServiceLock, error) {
	return AcquireServiceLock(stateDir)
}

// stubOpenCodeBinary writes a shell-script `opencode` stub on PATH.
func stubOpenCodeBinary(t *testing.T, binDir string) {
	t.Helper()
	stub := filepath.Join(binDir, "opencode")
	script := "#!/bin/sh\necho 'opencode stub' >&2\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
}
