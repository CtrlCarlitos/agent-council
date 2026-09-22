package service

// The OpenCode probe scratch root is operator-provided configuration, not
// a service-derived path: it must live outside the state directory and
// workspace base (the probe contract forbids probe artifacts under
// Council-owned trees) and carries operator-only permissions.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveOpenCodeProbeScratchRoot validates and prepares the configured
// probe scratch root. Fail closed: missing configuration, containment
// inside StateDir or WorkspaceBaseDir, and unusable directories are all
// rejections.
func resolveOpenCodeProbeScratchRoot(cfg ServerConfig) (string, error) {
	root := strings.TrimSpace(cfg.OpenCodeProbeScratchRoot)
	if root == "" {
		return "", fmt.Errorf("OpenCodeProbeScratchRoot is required when OpenCodeBinaryPath is configured")
	}
	root = filepath.Clean(root)

	for name, base := range map[string]string{
		"StateDir":         cfg.StateDir,
		"WorkspaceBaseDir": cfg.WorkspaceBaseDir,
	} {
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		if pathContains(base, root) {
			return "", fmt.Errorf("%s is inside %s (%s): the probe scratch root must be outside both the state directory and the workspace base", root, name, base)
		}
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create probe scratch root: %w", err)
	}
	// Tighten a pre-provisioned directory to operator-only access.
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("secure probe scratch root: %w", err)
	}
	return root, nil
}

// pathContains reports whether candidate is base itself or lies beneath
// it (lexically, after absolutization).
func pathContains(base, candidate string) bool {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absBase, absCandidate)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
