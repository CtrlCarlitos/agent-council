package claude

// cprof-v3 compatibility (AC-009 §3.8 matrix): Claude accepts cprof-v2|v3
// with a required toolkit manifest and ignores the additive codex block;
// the v1 rejection is unchanged. Mirrors launchsource_test.go fixtures.

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestClaudeTurnLaunch_AcceptsCprofV3Profile(t *testing.T) {
	src, wm, store, universeDigest := launchSourceFixture(t)

	// Freeze a second run with the additive v3 profile: the launch source
	// reads the profile of the session's run.
	const lease = "lease-ls-v3"
	ctx := context.Background()
	v3 := v2ClaudeProfile(universeDigest)
	v3.AlgoVersion = "cprof-v3"
	v3.Harnesses["codex"] = storage.HarnessProfileSpec{
		Model: "gpt-5.6-sol", NativeAuthMode: "inherited_codex_home",
		Codex: &storage.CodexHarnessSpec{
			AppServerVersion:  "0.154.0",
			ModelProvider:     "openai",
			ExpectedCodexHome: "/home/op/.codex",
			Platform:          storage.CodexPlatformSpec{OS: "linux", Family: "unix"},
			SandboxPolicy: storage.CodexSandboxPolicySpec{
				Type: "workspace-write", WritableRoots: []string{"/ws"}, NetworkAccess: false,
			},
			ApprovalPolicy:             storage.CodexApprovalPolicy{Kind: "string", String: "on-request"},
			ApprovalsReviewer:          "user",
			ExpectedMCPServers:         []string{},
			ExpectedMCPTools:           []string{},
			ExpectedPluginTools:        []string{},
			ExpectedInstructionSources: []string{"~/.codex/AGENTS.md"},
			RulesEvidence: storage.CodexRulesEvidenceSpec{
				Verified:     []string{"sandbox workspace-write"},
				Unverifiable: []string{"~/.codex/rules/*.rules contents"},
			},
			EventUniversePath:   "docs/superpowers/evidence/ac009-native-event-universe-0.154.0.json",
			EventUniverseDigest: universeDigest,
		},
	}
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-ls-v3", ControllerLease: lease, RunID: "run-ls-v3",
		Brief: "v3 compatibility", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      v3,
	}); err != nil {
		t.Fatalf("create v3 run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-ls-v3", lease, storage.SessionRecord{
		ID: "sess-ls-v3", RunID: "run-ls-v3", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create v3 session: %v", err)
	}
	if _, err := wm.AllocateWorkspace("run-ls-v3", "sess-ls-v3", "none", "example/repo",
		"0123456789012345678901234567890123456789"); err != nil {
		t.Fatalf("allocate v3 workspace: %v", err)
	}

	req, err := src.ClaudeTurnLaunch(context.Background(), "sess-ls-v3", testNativeID, true, "sha256:pd")
	if err != nil {
		t.Fatalf("claude launch from a cprof-v3 profile must succeed: %v", err)
	}
	if got := argValue(req.Args, "--model"); got != "claude-haiku-4-5-20251001" {
		t.Fatalf("v3 launch must keep the exact frozen claude contract, got model %q", got)
	}
	if req.Profile.AlgoVersion != "cprof-v3" || req.Profile.ToolkitManifest == nil {
		t.Fatalf("launch must carry the frozen v3 profile + manifest, got %q", req.Profile.AlgoVersion)
	}
	if h := req.Profile.Harnesses["codex"]; h.Codex == nil {
		t.Fatal("codex block must be carried untouched")
	}
}
