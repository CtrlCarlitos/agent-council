package service

// The OpenCode probe scratch root is operator-provided configuration, not
// a service-derived path: it must live outside the state directory and
// workspace base (the probe contract forbids probe artifacts under
// Council-owned trees) and carries operator-only permissions.
//
// Containment is decided on resolved paths, not lexical ones: symlinked
// path components must not let a root that physically lands inside a
// protected tree masquerade as an outside path. The root is resolved
// before creation (nearest existing ancestor) and again after creation
// (full chain), and containment is checked both times.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveOpenCodeProbeScratchRoot validates and prepares the configured
// probe scratch root. Fail closed: missing configuration, containment
// inside StateDir or WorkspaceBaseDir (lexical or through symlinks),
// and unusable directories are all rejections.
func resolveOpenCodeProbeScratchRoot(cfg ServerConfig) (string, error) {
	root := strings.TrimSpace(cfg.OpenCodeProbeScratchRoot)
	if root == "" {
		return "", fmt.Errorf("OpenCodeProbeScratchRoot is required when OpenCodeBinaryPath is configured")
	}
	root = filepath.Clean(root)

	bases := map[string]string{}
	for name, base := range map[string]string{
		"StateDir":         cfg.StateDir,
		"WorkspaceBaseDir": cfg.WorkspaceBaseDir,
	} {
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		bases[name] = filepath.Clean(base)
	}

	// Pre-creation: resolve the nearest existing ancestor so symlinked
	// components cannot hide the physical location.
	resolved, err := resolveExistingPath(root)
	if err != nil {
		return "", fmt.Errorf("resolve probe scratch root: %w", err)
	}
	if err := checkScratchContainment(bases, resolved); err != nil {
		return "", err
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create probe scratch root: %w", err)
	}
	// Tighten a pre-provisioned directory to operator-only access.
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("secure probe scratch root: %w", err)
	}

	// Post-creation: the full chain now exists; resolve it completely and
	// recheck containment against the resolved protected trees.
	resolvedFinal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve probe scratch root after creation: %w", err)
	}
	if err := checkScratchContainment(bases, resolvedFinal); err != nil {
		return "", err
	}
	return root, nil
}

// checkScratchContainment rejects candidate when it is inside — or equal
// to — any protected base, comparing resolved paths on both sides.
func checkScratchContainment(bases map[string]string, candidate string) error {
	for name, base := range bases {
		resolvedBase, err := resolveExistingPath(base)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", name, err)
		}
		if pathContains(resolvedBase, candidate) {
			return fmt.Errorf("%s is inside %s (%s): the probe scratch root must be outside both the state directory and the workspace base", candidate, name, base)
		}
	}
	return nil
}

// resolveExistingPath resolves the nearest existing ancestor of p with
// EvalSymlinks and rejoins the non-existing remainder. A path whose
// existing prefix contains an unresolvable symlink (broken link) is an
// error.
func resolveExistingPath(p string) (string, error) {
	p = filepath.Clean(p)
	suffix := ""
	dir := p
	for {
		if _, err := os.Lstat(dir); err == nil {
			resolved, err := filepath.EvalSymlinks(dir)
			if err != nil {
				return "", err
			}
			return filepath.Clean(filepath.Join(resolved, suffix)), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached the filesystem root without finding an existing
			// component; nothing to resolve.
			return p, nil
		}
		suffix = filepath.Join(filepath.Base(dir), suffix)
		dir = parent
	}
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
