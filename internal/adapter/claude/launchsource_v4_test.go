package claude

// cprof-v4 compatibility (AC-010 §3.7 matrix): Claude accepts
// cprof-v2|v3|v4 with a required toolkit manifest and ignores the
// additive agy block; the v1 rejection is unchanged. Mirrors
// launchsource_v3_test.go.

import (
	"context"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func v4AgyHarnessFixture() *storage.AgyHarnessSpec {
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
		ExpectedTools:               []string{"ask_permission", "run_command", "view_file"},
		ExpectedMCPServers:          []string{},
		ExpectedMCPTools:            []string{},
		ExpectedPluginTools:         []string{},
		DefaultRequiredTools:        []string{},
		HooksEvidence: storage.AgyHooksEvidenceSpec{
			Verified: []string{"guardrail hook present"},
		},
		ExpectedSkills:        []string{"research"},
		PluginsEvidencePath:   "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json",
		PluginsEvidenceDigest: "sha256:" + strings.Repeat("e", 64),
		HooksConfigDigest:     "sha256:" + strings.Repeat("d", 64),
		RequiredHooks:         []string{"/guardrail"},
		InitEvidencePath:      "docs/superpowers/evidence/ac010-agy-init-1.2.9.json",
		InitEvidenceDigest:    "sha256:" + strings.Repeat("f", 64),
		ToolCoveragePath:      "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json",
		ToolCoverageDigest:    "sha256:" + strings.Repeat("a", 64),
	}
}

func TestClaudeTurnLaunch_AcceptsCprofV4Profile(t *testing.T) {
	src, wm, store, universeDigest := launchSourceFixture(t)

	const lease = "lease-ls-v4"
	ctx := context.Background()
	v4 := v2ClaudeProfile(universeDigest)
	v4.AlgoVersion = "cprof-v4"
	v4.Harnesses["agy"] = storage.HarnessProfileSpec{
		Model: "gpt-oss-120b-medium", NativeAuthMode: "inherited_gemini_home",
		Agy: v4AgyHarnessFixture(),
	}
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-ls-v4", ControllerLease: lease, RunID: "run-ls-v4",
		Brief: "v4 compatibility", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      v4,
	}); err != nil {
		t.Fatalf("create v4 run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-ls-v4", lease, storage.SessionRecord{
		ID: "sess-ls-v4", RunID: "run-ls-v4", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create v4 session: %v", err)
	}
	if _, err := wm.AllocateWorkspace("run-ls-v4", "sess-ls-v4", "none", "example/repo",
		"0123456789012345678901234567890123456789"); err != nil {
		t.Fatalf("allocate v4 workspace: %v", err)
	}

	req, err := src.ClaudeTurnLaunch(context.Background(), "sess-ls-v4", testNativeID, true, "sha256:pd")
	if err != nil {
		t.Fatalf("claude launch from a cprof-v4 profile must succeed: %v", err)
	}
	if got := argValue(req.Args, "--model"); got != "claude-haiku-4-5-20251001" {
		t.Fatalf("v4 launch must keep the exact frozen claude contract, got model %q", got)
	}
	if req.Profile.AlgoVersion != "cprof-v4" || req.Profile.ToolkitManifest == nil {
		t.Fatalf("launch must carry the frozen v4 profile + manifest, got %q", req.Profile.AlgoVersion)
	}
	if h := req.Profile.Harnesses["agy"]; h.Agy == nil {
		t.Fatal("agy block must be carried untouched")
	}
}
