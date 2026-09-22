package claude

// Provider-free native tool universe resolution: the committed evidence
// file is resolved against the trusted service-owned evidence root with
// symlink-safe containment, re-hashed against the profile-recorded
// digest, and parsed into a typed universe. Never trusted from the
// native side or callers (spec §3.8).

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Universe is the pinned native tool list for one probed CLI version.
type Universe struct {
	ClaudeCodeVersion string   `json:"claude_code_version"`
	Tools             []string `json:"tools"`
}

// ResolveUniverseEvidence resolves universeEvidencePath (repo-relative,
// forward-slash) inside the trusted evidenceRoot with symlink-safe
// containment, re-hashes the raw bytes against wantDigest
// ("sha256:<lowercase-hex>"), and parses the typed universe.
func ResolveUniverseEvidence(evidenceRoot, universeEvidencePath, wantDigest string) (*Universe, error) {
	rel := strings.TrimSpace(universeEvidencePath)
	if rel == "" {
		return nil, fmt.Errorf("universe_evidence_path is empty")
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if filepath.IsAbs(rel) || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return nil, fmt.Errorf("universe_evidence_path %q escapes the evidence root", universeEvidencePath)
	}

	resolvedRoot, err := filepath.EvalSymlinks(evidenceRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve evidence root: %w", err)
	}
	// Reject symlink components within the relative path: every
	// intermediate component must exist and not be a symlink.
	parts := strings.Split(rel, "/")
	cur := resolvedRoot
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil, fmt.Errorf("evidence path component: %w", err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return nil, fmt.Errorf("evidence path component %q is not a real directory", part)
		}
	}
	final := filepath.Join(cur, parts[len(parts)-1])
	fi, err := os.Lstat(final)
	if err != nil {
		return nil, fmt.Errorf("evidence file: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("evidence file is a symlink")
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("evidence file is not a regular file")
	}
	// Containment re-check after resolution: the real path must remain
	// inside the resolved root.
	real, err := filepath.EvalSymlinks(final)
	if err != nil {
		return nil, fmt.Errorf("resolve evidence file: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(resolvedRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve evidence root: %w", err)
	}
	if !strings.HasPrefix(real, realRoot+string(os.PathSeparator)) {
		return nil, fmt.Errorf("evidence file %q resolves outside the evidence root", universeEvidencePath)
	}

	raw, err := os.ReadFile(final)
	if err != nil {
		return nil, fmt.Errorf("read evidence file: %w", err)
	}
	got := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	if got != strings.ToLower(wantDigest) {
		return nil, fmt.Errorf("universe evidence digest mismatch: got %s want %s", got, wantDigest)
	}

	var u Universe
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, fmt.Errorf("parse universe evidence: %w", err)
	}
	if u.ClaudeCodeVersion == "" || len(u.Tools) == 0 {
		return nil, fmt.Errorf("universe evidence must carry a version and a non-empty tool list")
	}
	return &u, nil
}
