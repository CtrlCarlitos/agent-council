package service

// The probe scratch root is operator configuration: required with the
// OpenCode binary path, outside the state and workspace trees (including
// through symlinks), secured to operator-only permissions. These tests
// exercise the resolver directly and are portable; the POSIX subprocess
// fixture and construction-level evidence live in
// probe_scratch_unix_test.go.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func probeScratchCfg(dir, scratchRoot string) ServerConfig {
	if scratchRoot == "" {
		scratchRoot = filepath.Join(dir, "probe-scratch")
	}
	return ServerConfig{
		StateDir:                 filepath.Join(dir, "state"),
		WorkspaceBaseDir:         filepath.Join(dir, "workspaces"),
		OpenCodeBinaryPath:       "opencode",
		OpenCodeProbeScratchRoot: scratchRoot,
	}
}

// A missing scratch root must be rejected when the OpenCode binary path
// is configured.
func TestProbeScratchRoot_RequiredWithOpenCodeBinary(t *testing.T) {
	dir := t.TempDir()
	cfg := probeScratchCfg(dir, "")
	cfg.OpenCodeProbeScratchRoot = ""

	_, err := resolveOpenCodeProbeScratchRoot(cfg)
	if err == nil {
		t.Fatal("missing probe scratch root must be rejected")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required-scratch-root error, got %v", err)
	}
}

// A scratch root inside the state directory violates the probe contract.
func TestProbeScratchRoot_RejectsInsideStateDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := probeScratchCfg(dir, filepath.Join(dir, "state", "probe-scratch"))

	_, err := resolveOpenCodeProbeScratchRoot(cfg)
	if err == nil {
		t.Fatal("scratch root inside the state directory must be rejected")
	}
	if !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("expected containment rejection, got %v", err)
	}
}

// A scratch root equal to the state directory itself is containment too.
func TestProbeScratchRoot_RejectsEqualStateDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := probeScratchCfg(dir, filepath.Join(dir, "state"))

	if _, err := resolveOpenCodeProbeScratchRoot(cfg); err == nil {
		t.Fatal("scratch root equal to the state directory must be rejected")
	}
}

// A scratch root inside the workspace base violates the probe contract.
func TestProbeScratchRoot_RejectsInsideWorkspaceBase(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := probeScratchCfg(dir, filepath.Join(dir, "workspaces", "probe"))

	_, err := resolveOpenCodeProbeScratchRoot(cfg)
	if err == nil {
		t.Fatal("scratch root inside the workspace base must be rejected")
	}
	if !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("expected containment rejection, got %v", err)
	}
}

// A valid root is created securely with operator-only permissions.
func TestProbeScratchRoot_CreatedSecure(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "probe-scratch")
	cfg := probeScratchCfg(dir, root)

	resolved, err := resolveOpenCodeProbeScratchRoot(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != root {
		t.Fatalf("resolver must return the configured root, got %q", resolved)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("scratch root must exist: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("scratch root must be a directory")
	}
	// Permission bits are a POSIX concept; Windows reports synthetic
	// modes, so the 0700 assertion is meaningful only on POSIX.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("scratch root must be 0700, got %v", perm)
		}
	}
}

// A pre-provisioned directory is tightened to operator-only permissions.
func TestProbeScratchRoot_TightensPreProvisioned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics are POSIX-only")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "probe-scratch")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := probeScratchCfg(dir, root)

	if _, err := resolveOpenCodeProbeScratchRoot(cfg); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("pre-provisioned root must be tightened to 0700, got %v", perm)
	}
}

// An external-looking symlink targeting the state directory must be
// rejected, and nothing may be created inside the state directory.
func TestProbeScratchRoot_RejectsSymlinkToStateDir(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(stateDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := probeScratchCfg(dir, link)

	_, err := resolveOpenCodeProbeScratchRoot(cfg)
	if err == nil {
		t.Fatal("symlink targeting the state directory must be rejected")
	}
	if !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("expected containment rejection, got %v", err)
	}
	assertNoProbeArtifacts(t, stateDir)
}

// A symlink targeting a child of the state directory is containment too:
// the resolved root would land inside the protected tree.
func TestProbeScratchRoot_RejectsSymlinkToStateChild(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(stateDir, "probe-scratch-target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := probeScratchCfg(dir, link)

	_, err := resolveOpenCodeProbeScratchRoot(cfg)
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
	dir := t.TempDir()
	wsBase := filepath.Join(dir, "workspaces")
	if err := os.MkdirAll(wsBase, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(wsBase, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := probeScratchCfg(dir, link)

	_, err := resolveOpenCodeProbeScratchRoot(cfg)
	if err == nil {
		t.Fatal("symlink targeting the workspace base must be rejected")
	}
	if !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("expected containment rejection, got %v", err)
	}
	assertNoProbeArtifacts(t, wsBase)
}

// A symlink whose target does not exist cannot be resolved and is
// rejected rather than created through the broken link.
func TestProbeScratchRoot_RejectsSymlinkToNowhere(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(filepath.Join(stateDir, "does-not-exist"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cfg := probeScratchCfg(dir, link)

	if _, err := resolveOpenCodeProbeScratchRoot(cfg); err == nil {
		t.Fatal("symlink to a non-existent target must be rejected")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "does-not-exist")); !os.IsNotExist(err) {
		t.Fatal("rejected root must not create the symlink target")
	}
}

func assertNoProbeArtifacts(t *testing.T, protectedDir string) {
	t.Helper()
	entries, err := os.ReadDir(protectedDir)
	if err != nil {
		t.Fatalf("read %s: %v", protectedDir, err)
	}
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		if strings.Contains(name, "probe") || strings.Contains(name, "scratch") {
			t.Fatalf("rejected root must not create probe artifacts in %s, found: %s", protectedDir, e.Name())
		}
	}
}
