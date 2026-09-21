//go:build unix

package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Production-construction regression: NewServerWithAdapter with
// OpenCodeBinaryPath set constructs a real adapter (not nil, not fake).
func TestGateSpecReview_ProductionConstruction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	for _, d := range []string{stateDir, wsBase} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	lock, lockErr := AcquireServiceLock(stateDir)
	if lockErr != nil {
		t.Fatalf("lock: %v", lockErr)
	}
	defer lock.Release()

	cfg := ServerConfig{
		StateDir:           stateDir,
		InstanceID:         "inst-pw",
		AuthToken:          "tok-pw",
		WorkspaceBaseDir:   wsBase,
		OpenCodeBinaryPath: "opencode",
	}

	// Construct through the production path: OpenCodeBinaryPath set means
	// the service builds a real OpenCode adapter with storage-backed seams.
	srv, srvErr := NewServerWithAdapter(store, lock, cfg, nil)
	if srvErr != nil {
		t.Fatalf("NewServerWithAdapter: %v", srvErr)
	}
	if srv.adapter == nil {
		t.Fatal("adapter must be non-nil when OpenCodeBinaryPath is configured")
	}
	if srv.workspaceManager == nil {
		t.Fatal("workspace manager must be non-nil")
	}
	if srv.policyExecutor == nil {
		t.Fatal("policy executor must be non-nil")
	}
}
