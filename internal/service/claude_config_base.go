package service

// The Claude config base directory is operator-provided configuration:
// required when the Claude binary is configured, disjoint from the state
// directory and workspace base (resolved paths, symlink-safe, rechecked
// after creation), and secured to operator-only permissions. Mirrors the
// AC-008 probe-scratch / OpenCode config-base validation family.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveClaudeConfigBaseDir validates and prepares the configured
// Claude config base. Fail closed: missing configuration, containment
// inside StateDir or WorkspaceBaseDir (lexical or through symlinks,
// checked before AND after creation), and unusable directories are all
// rejections.
func resolveClaudeConfigBaseDir(cfg ServerConfig) (string, error) {
	base := strings.TrimSpace(cfg.ClaudeConfigBaseDir)
	if base == "" {
		return "", fmt.Errorf("ClaudeConfigBaseDir is required when ClaudeBinaryPath is configured")
	}
	base = filepath.Clean(base)

	// Pre-creation: resolve the nearest existing ancestor so symlinked
	// components cannot hide the physical location.
	resolved, err := resolveExistingPath(base)
	if err != nil {
		return "", fmt.Errorf("resolve claude config base: %w", err)
	}
	if err := checkClaudeBaseContainment(cfg, resolved); err != nil {
		return "", err
	}

	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("create claude config base: %w", err)
	}
	// Tighten a pre-provisioned directory to operator-only access.
	if err := os.Chmod(base, 0o700); err != nil {
		return "", fmt.Errorf("secure claude config base: %w", err)
	}

	// Post-creation: resolve the completed chain and recheck containment.
	resolvedFinal, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("resolve claude config base after creation: %w", err)
	}
	if err := checkClaudeBaseContainment(cfg, resolvedFinal); err != nil {
		return "", err
	}
	return base, nil
}

// checkClaudeBaseContainment rejects candidate when it is inside — or
// equal to — the state directory or workspace base, comparing resolved
// paths on both sides.
func checkClaudeBaseContainment(cfg ServerConfig, candidate string) error {
	for name, base := range map[string]string{
		"StateDir":         cfg.StateDir,
		"WorkspaceBaseDir": cfg.WorkspaceBaseDir,
	} {
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		resolvedBase, err := resolveExistingPath(filepath.Clean(base))
		if err != nil {
			return fmt.Errorf("resolve %s: %w", name, err)
		}
		if pathContains(resolvedBase, candidate) {
			return fmt.Errorf("%s is inside %s (%s): the claude config base must be outside both the state directory and the workspace base", candidate, name, base)
		}
	}
	return nil
}
