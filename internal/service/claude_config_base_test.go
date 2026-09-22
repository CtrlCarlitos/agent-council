package service

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func claudeBaseCfg(dir, base string) ServerConfig {
	return ServerConfig{
		StateDir:            filepath.Join(dir, "state"),
		WorkspaceBaseDir:    filepath.Join(dir, "workspaces"),
		ClaudeBinaryPath:    "claude",
		ClaudeConfigBaseDir: base,
	}
}

// Required when the Claude binary is configured.
func TestClaudeConfigBase_RequiredWithClaudeBinary(t *testing.T) {
	dir := t.TempDir()
	cfg := claudeBaseCfg(dir, "")
	cfg.ClaudeConfigBaseDir = ""

	_, err := resolveClaudeConfigBaseDir(cfg)
	if err == nil {
		t.Fatal("missing claude config base must be rejected")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required error, got %v", err)
	}
}

// Disjoint from the state directory.
func TestClaudeConfigBase_RejectsInsideStateDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "state"), 0o700)
	cfg := claudeBaseCfg(dir, filepath.Join(dir, "state", "claude"))

	_, err := resolveClaudeConfigBaseDir(cfg)
	if err == nil || !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("inside-state base must be rejected, got %v", err)
	}
}

func TestClaudeConfigBase_RejectsEqualStateDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "state"), 0o700)
	cfg := claudeBaseCfg(dir, filepath.Join(dir, "state"))

	if _, err := resolveClaudeConfigBaseDir(cfg); err == nil {
		t.Fatal("base equal to the state directory must be rejected")
	}
}

// Disjoint from the workspace base.
func TestClaudeConfigBase_RejectsInsideWorkspaceBase(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "workspaces"), 0o700)
	cfg := claudeBaseCfg(dir, filepath.Join(dir, "workspaces", "claude"))

	_, err := resolveClaudeConfigBaseDir(cfg)
	if err == nil || !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("inside-workspace base must be rejected, got %v", err)
	}
}

// Symlink escapes are resolved before containment is judged.
func TestClaudeConfigBase_RejectsSymlinkIntoStateDir(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	os.MkdirAll(stateDir, 0o700)
	link := filepath.Join(dir, "outside-link")
	if err := os.Symlink(stateDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := claudeBaseCfg(dir, link)

	if _, err := resolveClaudeConfigBaseDir(cfg); err == nil {
		t.Fatal("symlink targeting the state directory must be rejected")
	}
}

// A valid base is created securely.
func TestClaudeConfigBase_CreatedSecure(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "claude-config")
	cfg := claudeBaseCfg(dir, base)

	resolved, err := resolveClaudeConfigBaseDir(cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved != base {
		t.Fatalf("resolver must return the configured base, got %q", resolved)
	}
	info, err := os.Stat(base)
	if err != nil || !info.IsDir() {
		t.Fatalf("base must exist as a directory: %v", err)
	}
	// Permission bits are a POSIX concept; Windows reports synthetic modes.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("base must be 0700, got %v", perm)
		}
	}
}

// A pre-provisioned base is tightened to 0700 (POSIX).
func TestClaudeConfigBase_TightensPreProvisioned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission semantics are POSIX-only")
	}
	dir := t.TempDir()
	base := filepath.Join(dir, "claude-config")
	os.MkdirAll(base, 0o755)
	cfg := claudeBaseCfg(dir, base)

	if _, err := resolveClaudeConfigBaseDir(cfg); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	info, err := os.Stat(base)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("pre-provisioned base must be tightened to 0700, got %v", perm)
	}
}
