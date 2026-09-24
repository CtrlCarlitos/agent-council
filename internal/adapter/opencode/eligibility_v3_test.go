package opencode

// cprof-v3 compatibility (AC-009 §3.8 matrix): the OpenCode adapter
// accepts the additive cprof-v3 profile unchanged — the executor
// validity gate widens v1|v2 → v1|v2|v3, and the toolkit_manifest and
// codex block are carried but ignored. Exercised through the production
// storage-backed launch source, mirroring eligibility_v2_test.go.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestOpenCodeEligibility_AcceptsCprofV3Profile(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	wm, err := workspace.NewWorkspaceManager(stateDir, wsBase)
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}

	const lease = "lease-v3"
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-v3", ControllerLease: lease, RunID: "run-v3",
		Brief: "v3 compatibility", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v3",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"opencode", "git", "go"},
			Harnesses: map[string]storage.HarnessProfileSpec{
				"opencode": {Model: "stub/model", NativeAuthMode: "managed_by_council"},
				"codex": {
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
						ExpectedMCPServers:         []string{"context7"},
						ExpectedMCPTools:           []string{"context7/resolve-library-id"},
						ExpectedPluginTools:        []string{},
						ExpectedInstructionSources: []string{"~/.codex/AGENTS.md"},
						RulesEvidence: storage.CodexRulesEvidenceSpec{
							Verified:     []string{"sandbox workspace-write"},
							Unverifiable: []string{"~/.codex/rules/*.rules contents"},
						},
						EventUniversePath:   "docs/superpowers/evidence/ac009-native-event-universe-0.154.0.json",
						EventUniverseDigest: "sha256:" + strings.Repeat("a", 64),
					},
				},
			},
			ToolkitManifest: &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
				ProbedCLIVersion:       "2.1.278",
				UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
				UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
				ApprovedTools:          []string{"Read", "Glob"},
				DeniedComplement:       []string{"Bash"},
				ExpectedHooks:          []string{"SessionStart:startup"},
				TurnsBound:             8,
			}},
		},
	}); err != nil {
		t.Fatalf("create run with v3 profile: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-v3", lease, storage.SessionRecord{
		ID: "sess-v3", RunID: "run-v3", Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	launch := &storageSessionLaunchSource{store: store, wm: wm}
	req, err := launch.OpenCodeServeLaunch(ctx, "sess-v3")
	if err != nil {
		t.Fatalf("opencode launch from a cprof-v3 profile must succeed: %v", err)
	}
	if !execpolicy.IsOpenCodeServeLaunch(req) {
		t.Fatalf("launch must keep the exact serve shape: %q %v", req.Command, req.Args)
	}
	if req.Profile.AlgoVersion != "cprof-v3" {
		t.Fatalf("launch must carry the frozen v3 profile, got %q", req.Profile.AlgoVersion)
	}
	// The manifest and codex block are carried, not acted on.
	if req.Profile.ToolkitManifest == nil {
		t.Fatal("launch must carry the frozen toolkit manifest")
	}
	if h := req.Profile.Harnesses["codex"]; h.Codex == nil || h.Codex.AppServerVersion != "0.154.0" {
		t.Fatal("launch must carry the frozen codex block untouched")
	}
}
