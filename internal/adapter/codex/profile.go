package codex

// Provider-free validation of the frozen codex harness block: the codex
// adapter requires cprof-v3 with a complete harnesses.codex block and
// re-hashes the committed event universe against its frozen digest
// (AC-009 spec §3.8). Everything fails closed before any process is
// started.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ErrUnsupportedProfile reports a profile that is not eligible for the
// codex adapter (wrong algo_version, or a v3 profile without a complete
// codex harness block).
type ErrUnsupportedProfile struct {
	AlgoVersion string
	Reason      string
}

func (e *ErrUnsupportedProfile) Error() string {
	return fmt.Sprintf("profile algorithm %q is unsupported for this adapter: %s", e.AlgoVersion, e.Reason)
}

// CodexLaunchPolicy is the verified frozen codex policy for a run, ready
// for the launch seam. ApprovalPolicyCanonical is the canonical JSON
// encoding of the frozen approval policy (string literal or granular
// object); effective-config comparison (§3.5) treats any byte
// difference against the native side as drift. ManifestDigest is the
// canonical toolkit-manifest digest (storage.ComputeToolkitManifestDigest)
// frozen at validation — the same value the §3.7 protection-attestation
// journal operation stores, so the protected-evidence tuple match
// (version, platform, manifest, profile) is exact.
type CodexLaunchPolicy struct {
	AppServerVersion           string
	ModelProvider              string
	ExpectedCodexHome          string
	PlatformOS                 string
	PlatformFamily             string
	SandboxType                string
	WritableRoots              []string
	NetworkAccess              bool
	ApprovalPolicyCanonical    string
	ApprovalsReviewer          string
	ExpectedMCPServers         []string
	ExpectedInstructionSources []string
	RulesVerified              []string
	RulesUnverifiable          []string
	EventUniversePath          string
	EventUniverseDigest        string
	ManifestDigest             string
}

// ValidateCodexHarness validates the frozen codex harness block of a run
// profile against the committed evidence rooted at evidenceRoot: the
// profile must be cprof-v3 with a complete block, approvals must be
// answered by the user, and the event universe file is re-hashed and
// must match the frozen digest. Any incompleteness fails closed with
// ErrUnsupportedProfile.
func ValidateCodexHarness(profile storage.CanonicalProfile, evidenceRoot string) (CodexLaunchPolicy, error) {
	if profile.AlgoVersion != "cprof-v3" {
		return CodexLaunchPolicy{}, &ErrUnsupportedProfile{
			AlgoVersion: profile.AlgoVersion,
			Reason:      "the Codex adapter requires cprof-v3 with a complete codex harness block",
		}
	}
	spec, ok := profile.Harnesses["codex"]
	if !ok || spec.Codex == nil {
		return CodexLaunchPolicy{}, &ErrUnsupportedProfile{
			AlgoVersion: profile.AlgoVersion,
			Reason:      "cprof-v3 profile lacks the required harnesses.codex block",
		}
	}
	c := spec.Codex

	unsupported := func(reason string) (CodexLaunchPolicy, error) {
		return CodexLaunchPolicy{}, &ErrUnsupportedProfile{AlgoVersion: profile.AlgoVersion, Reason: reason}
	}

	// cprof-v3 requires the toolkit manifest; its canonical digest is
	// part of the frozen launch tuple (§3.7 protected-evidence match).
	if profile.ToolkitManifest == nil {
		return unsupported("cprof-v3 profile requires toolkit_manifest")
	}
	manifestDigest, err := storage.ComputeToolkitManifestDigest(profile.ToolkitManifest.ToolkitManifest)
	if err != nil {
		return unsupported("toolkit manifest digest: " + err.Error())
	}

	if strings.TrimSpace(c.AppServerVersion) == "" {
		return unsupported("codex block lacks the app_server_version")
	}
	if strings.TrimSpace(c.ModelProvider) == "" {
		return unsupported("codex block lacks the model_provider")
	}
	if strings.TrimSpace(c.ExpectedCodexHome) == "" {
		return unsupported("codex block lacks the expected_codex_home")
	}
	if strings.TrimSpace(c.Platform.OS) == "" {
		return unsupported("codex block lacks the platform os")
	}
	if strings.TrimSpace(c.Platform.Family) == "" {
		return unsupported("codex block lacks the platform family")
	}
	if strings.TrimSpace(c.SandboxPolicy.Type) == "" {
		return unsupported("codex block lacks the sandbox_policy type")
	}
	if len(c.SandboxPolicy.WritableRoots) == 0 {
		return unsupported("codex block sandbox_policy requires writable_roots")
	}
	if strings.TrimSpace(c.ApprovalsReviewer) != "user" {
		return unsupported("approvals_reviewer must be \"user\": only the user answers approvals inside Council's visibility")
	}
	if c.ExpectedMCPServers == nil {
		return unsupported("codex block lacks expected_mcp_servers")
	}
	if c.ExpectedInstructionSources == nil {
		return unsupported("codex block lacks expected_instruction_sources")
	}
	if c.RulesEvidence.Verified == nil && c.RulesEvidence.Unverifiable == nil {
		return unsupported("codex block lacks rules_evidence")
	}
	if strings.TrimSpace(c.EventUniversePath) == "" {
		return unsupported("codex block lacks the event_universe_path")
	}
	if err := storage.ValidateSHA256Digest(c.EventUniverseDigest); err != nil {
		return unsupported("event_universe_digest: " + err.Error())
	}

	policyCanonical, err := canonicalApprovalPolicyEncoding(c.ApprovalPolicy)
	if err != nil {
		return unsupported(err.Error())
	}

	if err := rehashEventUniverse(evidenceRoot, c.EventUniversePath, c.EventUniverseDigest); err != nil {
		return CodexLaunchPolicy{}, err
	}

	return CodexLaunchPolicy{
		AppServerVersion:           c.AppServerVersion,
		ModelProvider:              c.ModelProvider,
		ExpectedCodexHome:          c.ExpectedCodexHome,
		PlatformOS:                 c.Platform.OS,
		PlatformFamily:             c.Platform.Family,
		SandboxType:                c.SandboxPolicy.Type,
		WritableRoots:              append([]string(nil), c.SandboxPolicy.WritableRoots...),
		NetworkAccess:              c.SandboxPolicy.NetworkAccess,
		ApprovalPolicyCanonical:    policyCanonical,
		ApprovalsReviewer:          c.ApprovalsReviewer,
		ExpectedMCPServers:         append([]string(nil), c.ExpectedMCPServers...),
		ExpectedInstructionSources: append([]string(nil), c.ExpectedInstructionSources...),
		RulesVerified:              append([]string(nil), c.RulesEvidence.Verified...),
		RulesUnverifiable:          append([]string(nil), c.RulesEvidence.Unverifiable...),
		EventUniversePath:          c.EventUniversePath,
		EventUniverseDigest:        c.EventUniverseDigest,
		ManifestDigest:             manifestDigest,
	}, nil
}

// canonicalApprovalPolicyEncoding renders the frozen approval policy as
// canonical JSON (sorted keys, no insignificant whitespace, number
// literals verbatim) for byte-exact effective-config comparison.
func canonicalApprovalPolicyEncoding(p storage.CodexApprovalPolicy) (string, error) {
	switch p.Kind {
	case storage.CodexApprovalKindString:
		b, err := json.Marshal(p.String)
		if err != nil {
			return "", fmt.Errorf("encode approval_policy: %w", err)
		}
		return string(b), nil
	case storage.CodexApprovalKindGranular:
		if p.Granular == nil {
			return "", fmt.Errorf("approval_policy granular kind carries no granular object")
		}
		raws := map[string]json.RawMessage{
			"mcp_elicitations":    p.Granular.McpElicitations,
			"request_permissions": p.Granular.RequestPermissions,
			"rules":               p.Granular.Rules,
			"sandbox_approval":    p.Granular.SandboxApproval,
			"skill_approval":      p.Granular.SkillApproval,
		}
		out := make(map[string]any, len(raws))
		for key, raw := range raws {
			if len(raw) == 0 || string(raw) == "null" {
				return "", fmt.Errorf("granular approval_policy is missing required key %q", key)
			}
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				return "", fmt.Errorf("granular approval_policy key %q: %w", key, err)
			}
			out[key] = v
		}
		b, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("encode granular approval_policy: %w", err)
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("approval_policy must be a string or a granular object")
	}
}

// rehashEventUniverse resolves the repo-relative universe path inside the
// trusted evidence root (symlink-safe containment), re-hashes the raw
// bytes, and requires an exact digest match.
func rehashEventUniverse(evidenceRoot, relPath, wantDigest string) error {
	rel := strings.TrimSpace(relPath)
	if rel == "" {
		return fmt.Errorf("event_universe_path is empty")
	}
	rel = filepath.ToSlash(filepath.Clean(rel))
	if filepath.IsAbs(rel) || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("event_universe_path %q escapes the evidence root", relPath)
	}

	resolvedRoot, err := filepath.EvalSymlinks(evidenceRoot)
	if err != nil {
		return fmt.Errorf("resolve evidence root: %w", err)
	}
	parts := strings.Split(rel, "/")
	cur := resolvedRoot
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return fmt.Errorf("evidence path component: %w", err)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return fmt.Errorf("evidence path component %q is not a real directory", part)
		}
	}
	final := filepath.Join(cur, parts[len(parts)-1])
	fi, err := os.Lstat(final)
	if err != nil {
		return fmt.Errorf("read event universe evidence: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return fmt.Errorf("event universe evidence must be a regular file")
	}
	real, err := filepath.EvalSymlinks(final)
	if err != nil {
		return fmt.Errorf("resolve event universe evidence: %w", err)
	}
	if !strings.HasPrefix(real, resolvedRoot+string(os.PathSeparator)) {
		return fmt.Errorf("event universe evidence %q resolves outside the evidence root", relPath)
	}

	raw, err := os.ReadFile(final)
	if err != nil {
		return fmt.Errorf("read event universe evidence: %w", err)
	}
	got := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	if got != wantDigest {
		return fmt.Errorf("event universe digest mismatch: got %s want %s", got, wantDigest)
	}
	return nil
}
