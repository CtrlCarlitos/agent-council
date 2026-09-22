package opencode

// cprof-v2 compatibility: the OpenCode adapter accepts the additive
// cprof-v2 profile (with toolkit_manifest) unchanged — its launch path
// is exercised with a v2 profile through the production storage-backed
// launch source. (Claude requires v2; OpenCode accepts v1 and v2.)

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestOpenCodeEligibility_AcceptsCprofV2Profile(t *testing.T) {
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

	const lease = "lease-v2"
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-v2", ControllerLease: lease, RunID: "run-v2",
		Brief: "v2 compatibility", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v2",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"opencode", "git", "go"},
			Harnesses: map[string]storage.HarnessProfileSpec{
				"opencode": {Model: "stub/model", NativeAuthMode: "managed_by_council"},
			},
			ToolkitManifest: &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
				ProbedCLIVersion:       "2.1.278",
				UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
				UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
				ApprovedTools:          []string{"Read", "Glob"},
				DeniedComplement:       []string{"Bash"},
				ExpectedHooks:          []string{"SessionStart:startup"},
			}},
		},
	}); err != nil {
		t.Fatalf("create run with v2 profile: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-v2", lease, storage.SessionRecord{
		ID: "sess-v2", RunID: "run-v2", Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	launch := &storageSessionLaunchSource{store: store, wm: wm}
	req, err := launch.OpenCodeServeLaunch(ctx, "sess-v2")
	if err != nil {
		t.Fatalf("opencode launch from a cprof-v2 profile must succeed: %v", err)
	}
	if !execpolicy.IsOpenCodeServeLaunch(req) {
		t.Fatalf("launch must keep the exact serve shape: %q %v", req.Command, req.Args)
	}
	if req.Profile.AlgoVersion != "cprof-v2" {
		t.Fatalf("launch must carry the frozen v2 profile, got %q", req.Profile.AlgoVersion)
	}
	if req.Profile.ToolkitManifest == nil {
		t.Fatal("launch must carry the frozen toolkit manifest")
	}
}
