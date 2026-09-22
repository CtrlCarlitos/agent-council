package service

// The probe scratch root is operator configuration: required with the
// OpenCode binary path, outside the state and workspace trees (including
// through symlinks), secured to operator-only permissions. These
// validation tests are portable; the POSIX subprocess fixture lives in
// probe_scratch_unix_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func probeScratchConfigFixture(t *testing.T, scratchRoot string) (ServerConfig, *storage.Store, *ServiceLock, string) {
	t.Helper()
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
	t.Cleanup(func() { _ = store.Close() })

	lock, lockErr := AcquireServiceLock(stateDir)
	if lockErr != nil {
		t.Fatalf("lock: %v", lockErr)
	}
	t.Cleanup(func() { _ = lock.Release() })

	if scratchRoot == "" {
		scratchRoot = filepath.Join(dir, "probe-scratch")
	}
	cfg := ServerConfig{
		StateDir:                 stateDir,
		InstanceID:               "inst-probe-scratch",
		AuthToken:                "tok-probe-scratch",
		WorkspaceBaseDir:         wsBase,
		OpenCodeBinaryPath:       "opencode",
		OpenCodeProbeScratchRoot: scratchRoot,
	}
	return cfg, store, lock, dir
}

// A missing scratch root must be rejected when the OpenCode binary path
// is configured.
func TestProbeScratchRoot_RequiredWithOpenCodeBinary(t *testing.T) {
	cfg, store, lock, _ := probeScratchConfigFixture(t, "")
	cfg.OpenCodeProbeScratchRoot = ""

	_, err := NewServerWithAdapter(store, lock, cfg, nil)
	if err == nil {
		t.Fatal("construction must fail without a configured probe scratch root")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required-scratch-root error, got %v", err)
	}
}

// A scratch root inside the state directory violates the probe contract.
func TestProbeScratchRoot_RejectsInsideStateDir(t *testing.T) {
	cfg, store, lock, dir := probeScratchConfigFixture(t, "")
	cfg.OpenCodeProbeScratchRoot = filepath.Join(cfg.StateDir, "probe-scratch")
	if !strings.HasPrefix(cfg.OpenCodeProbeScratchRoot, dir) {
		t.Fatal("fixture sanity: scratch root must be inside state dir")
	}

	_, err := NewServerWithAdapter(store, lock, cfg, nil)
	if err == nil {
		t.Fatal("scratch root inside the state directory must be rejected")
	}
	if !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("expected containment rejection, got %v", err)
	}
}

// A scratch root equal to the state directory itself is containment too.
func TestProbeScratchRoot_RejectsEqualStateDir(t *testing.T) {
	cfg, store, lock, _ := probeScratchConfigFixture(t, "")
	cfg.OpenCodeProbeScratchRoot = cfg.StateDir

	_, err := NewServerWithAdapter(store, lock, cfg, nil)
	if err == nil {
		t.Fatal("scratch root equal to the state directory must be rejected")
	}
}

// A scratch root inside the workspace base violates the probe contract.
func TestProbeScratchRoot_RejectsInsideWorkspaceBase(t *testing.T) {
	cfg, store, lock, _ := probeScratchConfigFixture(t, "")
	cfg.OpenCodeProbeScratchRoot = filepath.Join(cfg.WorkspaceBaseDir, "probe")

	_, err := NewServerWithAdapter(store, lock, cfg, nil)
	if err == nil {
		t.Fatal("scratch root inside the workspace base must be rejected")
	}
	if !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("expected containment rejection, got %v", err)
	}
}

// A valid root is created securely with operator-only permissions and
// survives construction.
func TestProbeScratchRoot_CreatedSecure(t *testing.T) {
	cfg, store, lock, dir := probeScratchConfigFixture(t, "")
	root := filepath.Join(dir, "probe-scratch")
	cfg.OpenCodeProbeScratchRoot = root

	srv, err := NewServerWithAdapter(store, lock, cfg, nil)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	if srv.adapter == nil {
		t.Fatal("adapter must be constructed")
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("scratch root must exist after construction: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("scratch root must be a directory")
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("scratch root must be 0700, got %v", perm)
	}
}

// An external-looking symlink targeting the state directory must be
// rejected, and nothing may be created inside the state directory.
func TestProbeScratchRoot_RejectsSymlinkToStateDir(t *testing.T) {
	cfg, store, lock, dir := probeScratchConfigFixture(t, "")
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(cfg.StateDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg.OpenCodeProbeScratchRoot = link

	_, err := NewServerWithAdapter(store, lock, cfg, nil)
	if err == nil {
		t.Fatal("symlink targeting the state directory must be rejected")
	}
	if !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("expected containment rejection, got %v", err)
	}
	// The storage database lives in the state dir; assert that no probe
	// artifact was created there.
	entries, err := os.ReadDir(cfg.StateDir)
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name()), "probe") || strings.Contains(strings.ToLower(e.Name()), "scratch") {
			t.Fatalf("rejected root must not create probe artifacts in the state directory, found: %s", e.Name())
		}
	}
}

// A symlink targeting a child of the state directory is containment too:
// the resolved root would land inside the protected tree.
func TestProbeScratchRoot_RejectsSymlinkToStateChild(t *testing.T) {
	cfg, store, lock, dir := probeScratchConfigFixture(t, "")
	target := filepath.Join(cfg.StateDir, "probe-scratch-target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg.OpenCodeProbeScratchRoot = link

	_, err := NewServerWithAdapter(store, lock, cfg, nil)
	if err == nil {
		t.Fatal("symlink targeting a child of the state directory must be rejected")
	}
	if !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("expected containment rejection, got %v", err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if len(entries) != 0 {
		t.Fatal("rejected root must not create anything in the targeted state child")
	}
}

// A symlink targeting the workspace base is rejected the same way.
func TestProbeScratchRoot_RejectsSymlinkToWorkspaceBase(t *testing.T) {
	cfg, store, lock, dir := probeScratchConfigFixture(t, "")
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(cfg.WorkspaceBaseDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg.OpenCodeProbeScratchRoot = link

	_, err := NewServerWithAdapter(store, lock, cfg, nil)
	if err == nil {
		t.Fatal("symlink targeting the workspace base must be rejected")
	}
	if !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("expected containment rejection, got %v", err)
	}
	entries, err := os.ReadDir(cfg.WorkspaceBaseDir)
	if err != nil {
		t.Fatalf("read workspace base: %v", err)
	}
	if len(entries) != 0 {
		t.Fatal("rejected root must not create anything in the workspace base")
	}
}

// A symlink whose target does not exist cannot be resolved and is
// rejected rather than created through the broken link.
func TestProbeScratchRoot_RejectsSymlinkToNowhere(t *testing.T) {
	cfg, store, lock, dir := probeScratchConfigFixture(t, "")
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(filepath.Join(cfg.StateDir, "does-not-exist"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg.OpenCodeProbeScratchRoot = link

	if _, err := NewServerWithAdapter(store, lock, cfg, nil); err == nil {
		t.Fatal("symlink to a non-existent target must be rejected")
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "does-not-exist")); !os.IsNotExist(err) {
		t.Fatal("rejected root must not create the symlink target")
	}
}
