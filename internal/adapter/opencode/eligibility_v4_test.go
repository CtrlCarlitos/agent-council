package opencode

// cprof-v4 compatibility (AC-010 §3.7 matrix): the OpenCode adapter
// accepts the additive cprof-v4 profile unchanged — the executor
// validity gate widens v1|v2|v3 → v1|v2|v3|v4, and the toolkit_manifest
// and agy block are carried but ignored. Mirrors eligibility_v3_test.go.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestOpenCodeEligibility_AcceptsCprofV4Profile(t *testing.T) {
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

	const lease = "lease-v4"
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-v4", ControllerLease: lease, RunID: "run-v4",
		Brief: "v4 compatibility", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v4",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"opencode", "git", "go"},
			Harnesses: map[string]storage.HarnessProfileSpec{
				"opencode": {Model: "stub/model", NativeAuthMode: "managed_by_council"},
				"agy": {
					Model: "gpt-oss-120b-medium", NativeAuthMode: "inherited_gemini_home",
					Agy: &storage.AgyHarnessSpec{
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
						HooksEvidence:               storage.AgyHooksEvidenceSpec{Verified: []string{"guardrail hook present"}},
						ExpectedSkills:              []string{"research"},
						PluginsEvidencePath:         "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json",
						PluginsEvidenceDigest:       "sha256:" + strings.Repeat("e", 64),
						HooksConfigDigest:           "sha256:" + strings.Repeat("d", 64),
						RequiredHooks:               []string{"/guardrail"},
						InitEvidencePath:            "docs/superpowers/evidence/ac010-agy-init-1.2.9.json",
						InitEvidenceDigest:          "sha256:" + strings.Repeat("f", 64),
						ToolCoveragePath:            "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json",
						ToolCoverageDigest:          "sha256:" + strings.Repeat("a", 64),
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
		t.Fatalf("create run with v4 profile: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-v4", lease, storage.SessionRecord{
		ID: "sess-v4", RunID: "run-v4", Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	launch := &storageSessionLaunchSource{store: store, wm: wm}
	req, err := launch.OpenCodeServeLaunch(ctx, "sess-v4")
	if err != nil {
		t.Fatalf("opencode launch from a cprof-v4 profile must succeed: %v", err)
	}
	if !execpolicy.IsOpenCodeServeLaunch(req) {
		t.Fatalf("launch must keep the exact serve shape: %q %v", req.Command, req.Args)
	}
	if req.Profile.AlgoVersion != "cprof-v4" {
		t.Fatalf("launch must carry the frozen v4 profile, got %q", req.Profile.AlgoVersion)
	}
	if req.Profile.ToolkitManifest == nil {
		t.Fatal("launch must carry the frozen toolkit manifest")
	}
	if h := req.Profile.Harnesses["agy"]; h.Agy == nil || h.Agy.CLIVersion != "1.2.9" {
		t.Fatal("launch must carry the frozen agy block untouched")
	}
}
