package agy

// The Agy adapter requires cprof-v4 with a complete frozen agy harness
// block; anything else fails closed with the typed ErrUnsupportedProfile
// (AC-010 spec §3.7 compatibility matrix). ValidateAgyHarness re-hashes
// and strictly decodes init_evidence_path/tool_coverage_path/
// plugins_evidence_path against the committed evidence root.

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// nonUncoveredTools is expected_tools ∩ the committed 1.2.9 coverage
// map EXCLUDING every tool classified uncovered — the operator has
// natively denied the rest, matching spec §3.2's stated production
// path ("the operator to deny them natively first").
var nonUncoveredTools = []string{
	"ask_custom_permission", "ask_permission", "ask_question", "call_mcp_tool",
	"command_status", "execute_browser_javascript", "find_by_name", "finish",
	"generate_image", "grep_search", "list_dir", "list_permissions",
	"multi_replace_file_content", "notebook_edit", "notebook_execution",
	"open_browser_url", "read_browser_page", "read_resource", "read_url_content",
	"replace_file_content", "run_command", "search_web", "sed_file",
	"send_command_input", "view_file", "wait", "wait_5_seconds", "write_to_file",
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// internal/adapter/agy -> repo root
	return filepath.Join(wd, "..", "..", "..")
}

func readCommittedEvidence(t *testing.T, rel string) []byte {
	t.Helper()
	full := filepath.Join(repoRoot(t), filepath.FromSlash(rel))
	raw, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read committed evidence %s: %v", rel, err)
	}
	return raw
}

func digestOf(raw []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}

func writeUnderRoot(t *testing.T, root, rel string, raw []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// baseAgySpec is a complete, shape-valid AgyHarnessSpec; the caller
// wires expected_tools/init_evidence_digest/tool_coverage_digest/
// plugins_evidence_digest per test.
func baseAgySpec() *storage.AgyHarnessSpec {
	return &storage.AgyHarnessSpec{
		CLIVersion:                  "1.2.9",
		BinaryPath:                  "/home/op/.local/bin/agy",
		BinaryDigest:                "sha256:" + strings.Repeat("c", 64),
		ExpectedHome:                "/home/op/.gemini",
		Platform:                    storage.AgyPlatformSpec{OS: "linux", Family: "unix"},
		PermissionMode:              "request-review",
		ExecutionMode:               "default",
		Sandbox:                     true,
		PrintTimeoutBackstopSeconds: 1800,
		ExpectedMCPServers:          []string{},
		ExpectedMCPTools:            []string{},
		ExpectedPluginTools:         []string{},
		DefaultRequiredTools:        []string{},
		HooksEvidence: storage.AgyHooksEvidenceSpec{
			Verified: []string{"guardrail hook present"},
		},
		ExpectedSkills:      []string{"research"},
		PluginsEvidencePath: "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json",
		HooksConfigDigest:   "sha256:" + strings.Repeat("d", 64),
		RequiredHooks:       []string{"/guardrail"},
		InitEvidencePath:    "docs/superpowers/evidence/ac010-agy-init-1.2.9.json",
		ToolCoveragePath:    "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json",
	}
}

func profileWithAgy(a *storage.AgyHarnessSpec) storage.CanonicalProfile {
	p := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v4",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"agy", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"agy": {Model: "gpt-oss-120b-medium", NativeAuthMode: "inherited_gemini_home", Agy: a},
		},
	}
	p.ToolkitManifest = &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
		ProbedCLIVersion:       "2.1.278",
		UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
		UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
		ApprovedTools:          []string{"Read"},
		ExpectedHooks:          []string{"SessionStart:startup"},
		TurnsBound:             8,
	}}
	return p
}

// acceptedProfileAndRoot builds an eligible v4 profile (operator has
// natively denied every uncovered tool) plus a temp evidence root
// staging: the REAL committed plugins/tool-coverage evidence bytes
// (already canonical, already classified) and a SYNTHETIC init capture
// (28 tools, not the unmodified-install 57) simulating that denial.
func acceptedProfileAndRoot(t *testing.T) (storage.CanonicalProfile, string) {
	t.Helper()
	root := t.TempDir()

	pluginsRaw := readCommittedEvidence(t, "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json")
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json", pluginsRaw)

	coverageRaw := readCommittedEvidence(t, "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json")
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json", coverageRaw)

	initDoc := map[string]any{
		"conversation_id": "<conversation-id>",
		"event":           "init",
		"init": map[string]any{
			"cwd":             "<workspace>",
			"permission_mode": "request-review",
			"tools":           nonUncoveredTools,
		},
	}
	initRaw, err := json.Marshal(initDoc)
	if err != nil {
		t.Fatalf("marshal synthetic init: %v", err)
	}
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-init-1.2.9.json", initRaw)

	a := baseAgySpec()
	a.ExpectedTools = append([]string(nil), nonUncoveredTools...)
	a.PluginsEvidenceDigest = digestOf(pluginsRaw)
	a.ToolCoverageDigest = digestOf(coverageRaw)
	a.InitEvidenceDigest = digestOf(initRaw)

	return profileWithAgy(a), root
}

func TestValidateAgyHarness_Accepts(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	policy, err := ValidateAgyHarness(p, root)
	if err != nil {
		t.Fatalf("a complete, evidence-backed v4 agy profile must validate: %v", err)
	}
	if policy.CLIVersion != "1.2.9" {
		t.Fatalf("policy CLIVersion, got %q", policy.CLIVersion)
	}
	if policy.PrintTimeoutBackstop.Seconds() != 1800 {
		t.Fatalf("policy PrintTimeoutBackstop, got %v", policy.PrintTimeoutBackstop)
	}
	if len(policy.Coverage.Tools) == 0 {
		t.Fatal("policy.Coverage must carry the decoded tool coverage map")
	}
	if _, err := ExpectedCoverage(policy); err != nil {
		t.Fatalf("the accepted policy must derive an expected coverage set: %v", err)
	}
}

func TestValidateAgyHarness_MissingModelRejected(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	h := p.Harnesses["agy"]
	h.Model = "  "
	p.Harnesses["agy"] = h
	_, err := ValidateAgyHarness(p, root)
	var unsupported *ErrUnsupportedProfile
	if !errors.As(err, &unsupported) || !strings.Contains(unsupported.Reason, "model") {
		t.Fatalf("a missing frozen model must fail closed, got %v", err)
	}
}

func TestValidateAgyHarness_WrongAlgoVersionRejected(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	p.AlgoVersion = "cprof-v3"
	_, err := ValidateAgyHarness(p, root)
	var unsupported *ErrUnsupportedProfile
	if err == nil || !errors.As(err, &unsupported) {
		t.Fatalf("cprof-v3 must be rejected as ErrUnsupportedProfile, got %T: %v", err, err)
	}
}

func TestValidateAgyHarness_MissingAgyBlockRejected(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	delete(p.Harnesses, "agy")
	_, err := ValidateAgyHarness(p, root)
	var unsupported *ErrUnsupportedProfile
	if err == nil || !errors.As(err, &unsupported) {
		t.Fatalf("a missing harnesses.agy block must be rejected as ErrUnsupportedProfile, got %T: %v", err, err)
	}
}

func TestValidateAgyHarness_MissingToolkitManifestRejected(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	p.ToolkitManifest = nil
	if _, err := ValidateAgyHarness(p, root); err == nil {
		t.Fatal("a profile without toolkit_manifest must be rejected")
	}
}

func TestValidateAgyHarness_PermissionModeEnumRejected(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	p.Harnesses["agy"].Agy.PermissionMode = "always-proceed"
	if _, err := ValidateAgyHarness(p, root); err == nil {
		t.Fatal("permission_mode always-proceed must be rejected (no bypass value representable)")
	}
}

func TestValidateAgyHarness_NonEmptyMCPInventoryRejected(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	p.Harnesses["agy"].Agy.ExpectedMCPServers = []string{"context7"}
	if _, err := ValidateAgyHarness(p, root); err == nil {
		t.Fatal("a non-empty expected_mcp_servers must be rejected: no committed evidence proves the inventory")
	}
}

// The unmodified 1.2.9 install capture (57 tools, including several
// classified uncovered) is not production-eligible: freeze/construction
// requires the operator to have denied the uncovered tools first
// (spec §3.2).
func TestValidateAgyHarness_UnmodifiedInstallHasUncoveredTools(t *testing.T) {
	root := t.TempDir()
	initRaw := readCommittedEvidence(t, "docs/superpowers/evidence/ac010-agy-init-1.2.9.json")
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-init-1.2.9.json", initRaw)
	coverageRaw := readCommittedEvidence(t, "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json")
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json", coverageRaw)
	pluginsRaw := readCommittedEvidence(t, "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json")
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json", pluginsRaw)

	var initDoc struct {
		Init struct {
			Tools []string `json:"tools"`
		} `json:"init"`
	}
	if err := json.Unmarshal(initRaw, &initDoc); err != nil {
		t.Fatalf("unmarshal committed init: %v", err)
	}

	a := baseAgySpec()
	a.ExpectedTools = initDoc.Init.Tools
	a.InitEvidenceDigest = digestOf(initRaw)
	a.ToolCoverageDigest = digestOf(coverageRaw)
	a.PluginsEvidenceDigest = digestOf(pluginsRaw)

	_, err := ValidateAgyHarness(profileWithAgy(a), root)
	if err == nil {
		t.Fatal("the unmodified-install 57-tool inventory carries uncovered tools and must be rejected")
	}
	if !strings.Contains(err.Error(), "uncovered") {
		t.Fatalf("rejection must name the uncovered classification, got %v", err)
	}
}

func TestValidateAgyHarness_InitEvidenceToolsMismatchRejected(t *testing.T) {
	root := t.TempDir()
	pluginsRaw := readCommittedEvidence(t, "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json")
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json", pluginsRaw)
	coverageRaw := readCommittedEvidence(t, "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json")
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json", coverageRaw)

	initDoc := map[string]any{
		"conversation_id": "<conversation-id>",
		"event":           "init",
		"init": map[string]any{
			"cwd":             "<workspace>",
			"permission_mode": "request-review",
			"tools":           []string{"view_file"}, // does not equal expected_tools
		},
	}
	initRaw, err := json.Marshal(initDoc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-init-1.2.9.json", initRaw)

	a := baseAgySpec()
	a.ExpectedTools = append([]string(nil), nonUncoveredTools...)
	a.InitEvidenceDigest = digestOf(initRaw)
	a.ToolCoverageDigest = digestOf(coverageRaw)
	a.PluginsEvidenceDigest = digestOf(pluginsRaw)

	if _, err := ValidateAgyHarness(profileWithAgy(a), root); err == nil {
		t.Fatal("init.tools not equal to expected_tools must be rejected")
	}
}

func TestValidateAgyHarness_InitEvidencePermissionModeMismatchRejected(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	p.Harnesses["agy"].Agy.PermissionMode = "strict" // init capture still says request-review
	if _, err := ValidateAgyHarness(p, root); err == nil {
		t.Fatal("init.permission_mode not equal to the frozen mode must be rejected")
	}
}

func TestValidateAgyHarness_ToolCoverageCLIVersionMismatchRejected(t *testing.T) {
	root := t.TempDir()
	pluginsRaw := readCommittedEvidence(t, "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json")
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json", pluginsRaw)

	var covDoc map[string]any
	coverageRaw := readCommittedEvidence(t, "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json")
	if err := json.Unmarshal(coverageRaw, &covDoc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	covDoc["cli_version"] = "9.9.9"
	mutatedCoverageRaw, err := json.Marshal(covDoc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json", mutatedCoverageRaw)

	initDoc := map[string]any{
		"conversation_id": "<conversation-id>", "event": "init",
		"init": map[string]any{"cwd": "<workspace>", "permission_mode": "request-review", "tools": nonUncoveredTools},
	}
	initRaw, err := json.Marshal(initDoc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-init-1.2.9.json", initRaw)

	a := baseAgySpec()
	a.ExpectedTools = append([]string(nil), nonUncoveredTools...)
	a.InitEvidenceDigest = digestOf(initRaw)
	a.ToolCoverageDigest = digestOf(mutatedCoverageRaw)
	a.PluginsEvidenceDigest = digestOf(pluginsRaw)

	if _, err := ValidateAgyHarness(profileWithAgy(a), root); err == nil {
		t.Fatal("tool coverage evidence cli_version not equal to the frozen cli_version must be rejected")
	}
}

// The plugins evidence canonical-bytes rule (spec §3.2): a committed
// capture whose bytes are not the canonical re-encoding is rejected
// even though it decodes and re-hashes fine.
func TestValidateAgyHarness_PluginsEvidenceNonCanonicalRejected(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	// Replace the plugins evidence with a semantically-equal but
	// non-canonical (whitespace-padded) encoding, re-pointing the digest
	// at the tampered bytes.
	tampered := []byte(`{"imports": [{"components":["hooks","skills"],"importedAt":"2026-09-02T19:59:12Z","name":"superpowers","source":"gemini-cli"}]}`)
	writeUnderRoot(t, root, "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json", tampered)
	p.Harnesses["agy"].Agy.PluginsEvidenceDigest = digestOf(tampered)

	if _, err := ValidateAgyHarness(p, root); err == nil {
		t.Fatal("non-canonical plugins evidence bytes must be rejected")
	}
}

func TestValidateAgyHarness_EvidenceDigestMismatchRejected(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	p.Harnesses["agy"].Agy.InitEvidenceDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err := ValidateAgyHarness(p, root); err == nil {
		t.Fatal("an init_evidence_digest mismatch must be rejected")
	}
}

// ValidateAgyHarness re-validates binary_path/expected_home with the
// EXACT SAME normalizer storage.ComputeProfileDigest uses to freeze
// them (storage.NormalizePathScalar), instead of a forked copy that
// could silently drift (e.g. omit NFC normalization). A path component
// written as decomposed Unicode ("e" + U+0301, NFD) must come out of
// both the frozen canonical profile and AgyLaunchPolicy as the SAME
// precomposed ("é", NFC) string.
func TestValidateAgyHarness_PathScalarsShareStorageNFCNormalization(t *testing.T) {
	p, root := acceptedProfileAndRoot(t)
	decomposedBinaryPath := "/home/op/café/.local/bin/agy" // "café" written as e + combining acute accent
	decomposedExpectedHome := "/home/op/.gemini_café"      // same decomposed sequence
	p.Harnesses["agy"].Agy.BinaryPath = decomposedBinaryPath
	p.Harnesses["agy"].Agy.ExpectedHome = decomposedExpectedHome

	_, canon, err := storage.ComputeProfileDigest(p)
	if err != nil {
		t.Fatalf("compute profile digest: %v", err)
	}
	var canonDoc struct {
		Harnesses map[string]struct {
			Agy struct {
				BinaryPath   string `json:"binary_path"`
				ExpectedHome string `json:"expected_home"`
			} `json:"agy"`
		} `json:"harnesses"`
	}
	if err := json.Unmarshal(canon, &canonDoc); err != nil {
		t.Fatalf("unmarshal canonical profile: %v", err)
	}
	wantBinaryPath := canonDoc.Harnesses["agy"].Agy.BinaryPath
	wantExpectedHome := canonDoc.Harnesses["agy"].Agy.ExpectedHome

	// Sanity: the canonical value is precomposed, not the raw decomposed
	// input, so the comparison below is actually exercising normalization.
	if !strings.Contains(wantBinaryPath, "é") || strings.Contains(wantBinaryPath, "é") {
		t.Fatalf("canonical profile binary_path must be NFC-normalized, got %q", wantBinaryPath)
	}
	if !strings.Contains(wantExpectedHome, "é") || strings.Contains(wantExpectedHome, "é") {
		t.Fatalf("canonical profile expected_home must be NFC-normalized, got %q", wantExpectedHome)
	}

	policy, err := ValidateAgyHarness(p, root)
	if err != nil {
		t.Fatalf("ValidateAgyHarness: %v", err)
	}
	if policy.BinaryPath != wantBinaryPath {
		t.Fatalf("AgyLaunchPolicy.BinaryPath %q does not equal the canonical profile's frozen binary_path %q", policy.BinaryPath, wantBinaryPath)
	}
	if policy.ExpectedHome != wantExpectedHome {
		t.Fatalf("AgyLaunchPolicy.ExpectedHome %q does not equal the canonical profile's frozen expected_home %q", policy.ExpectedHome, wantExpectedHome)
	}
}

func TestDecodeCoverageEvidence_StrictRejections(t *testing.T) {
	// Read the canonical coverage evidence once
	validRaw := readCommittedEvidence(t, "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json")

	tests := []struct {
		name        string
		raw         []byte
		wantErrText string
	}{
		{
			name:        "valid coverage evidence",
			raw:         validRaw,
			wantErrText: "",
		},
		{
			name:        "duplicate top-level key",
			raw:         []byte(`{"cli_version":"1.2.9","cli_version":"1.2.9","tools":{},"denial_map":[]}`),
			wantErrText: "duplicate object key",
		},
		{
			name:        "duplicate key inside tools object",
			raw:         []byte(`{"cli_version":"1.2.9","tools":{"a":["control"],"a":["control"]},"denial_map":[]}`),
			wantErrText: "duplicate object key",
		},
		{
			name:        "trailing content after closing brace",
			raw:         append(validRaw, []byte(" x")...),
			wantErrText: "trailing content",
		},
		{
			name:        "unsorted tools keys",
			raw:         []byte(`{"cli_version":"1.2.9","tools":{"z":["control"],"a":["control"]},"denial_map":[]}`),
			wantErrText: "sorted",
		},
		{
			name:        "unknown top-level field",
			raw:         []byte(`{"cli_version":"1.2.9","tools":{},"denial_map":[],"unknown_field":"value"}`),
			wantErrText: "unknown field",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeCoverageEvidence(tt.raw)
			if tt.wantErrText == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrText)
				}
				errStr := strings.ToLower(err.Error())
				wantStr := strings.ToLower(tt.wantErrText)
				if !strings.Contains(errStr, wantStr) {
					t.Fatalf("error %q does not contain %q", err, tt.wantErrText)
				}
			}
		})
	}
}
