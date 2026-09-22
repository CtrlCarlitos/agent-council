package claude

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// filepathSeparator is the OS path separator as a string.
var filepathSeparator = string(filepath.Separator)

// asErrUnsupportedProfile mirrors errors.As for the typed profile error.
func asErrUnsupportedProfile(err error, target **storage.ErrUnsupportedProfile) bool {
	if e, ok := err.(*storage.ErrUnsupportedProfile); ok {
		*target = e
		return true
	}
	return false
}

var _ = fs.Stat // keep fs referenced if unused paths change

const (
	testRunID     = "run-ls"
	testSessionID = "sess-ls"
	testNativeID  = "e8e4074f-8a52-4c28-b1f7-9a2e5dbf4a11"
)

func v2ClaudeProfile() storage.CanonicalProfile {
	p := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v2",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"claude", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"claude": {Model: "haiku", NativeAuthMode: "inherited_host_keychain"},
		},
	}
	p.ToolkitManifest = &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
		ProbedCLIVersion:       "2.1.278",
		UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
		UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
		ApprovedTools:          []string{"Read", "Glob", "Grep"},
		DeniedComplement:       []string{"Bash", "Write", "WebSearch"},
		ExpectedHooks:          []string{"SessionStart:startup"},
	}}
	return p
}

func launchSourceFixture(t *testing.T) (*ClaudeTurnLaunchSource, *workspace.WorkspaceManager, *storage.Store) {
	t.Helper()
	dir := t.TempDir()
	stateDir := dir + "/state"
	wsBase := dir + "/workspaces"
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const lease = "lease-ls"
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-ls", ControllerLease: lease, RunID: testRunID,
		Brief: "launch source", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      v2ClaudeProfile(),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-ls", lease, storage.SessionRecord{
		ID: testSessionID, RunID: testRunID, Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	wm, err := workspace.NewWorkspaceManager(stateDir, wsBase)
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}
	if _, err := wm.AllocateWorkspace(testRunID, testSessionID, "none", "example/repo",
		"0123456789012345678901234567890123456789"); err != nil {
		t.Fatalf("allocate workspace: %v", err)
	}

	src := NewClaudeTurnLaunchSource(store, wm, 8, filepath.Join(dir, "claude-config"))
	return src, wm, store
}

// The first turn uses --session-id with the caller-chosen native UUID
// and carries the exact contract prefix, frozen model, bound, and tool
// policy from the frozen manifest.
func TestClaudeTurnLaunch_FirstTurnSessionIDContract(t *testing.T) {
	src, wm, _ := launchSourceFixture(t)

	req, err := src.ClaudeTurnLaunch(context.Background(), testSessionID, testNativeID, true)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if !execpolicy.IsClaudeLaunch(req) {
		t.Fatalf("launch must satisfy the exact contract: %q %v", req.Command, req.Args)
	}
	if got := argValue(req.Args, "--session-id"); got != testNativeID {
		t.Fatalf("first turn must use --session-id with the native ID, got %q", got)
	}
	if got := argValue(req.Args, "--model"); got != "haiku" {
		t.Fatalf("frozen model expected, got %q", got)
	}
	if got := argValue(req.Args, "--max-turns"); got != "8" {
		t.Fatalf("frozen max-turns bound expected, got %q", got)
	}
	// Frozen tool policy from the manifest.
	allowed := listValue(req.Args, "--allowedTools")
	if !containsAll(allowed, "Read", "Glob", "Grep") {
		t.Fatalf("approved tools must be allowed, got %v", allowed)
	}
	denied := listValue(req.Args, "--disallowedTools")
	if !containsAll(denied, "Bash", "Write", "WebSearch") {
		t.Fatalf("deny complement must be passed, got %v", denied)
	}

	// Workspace pinned; config dir inside the trusted base.
	paths, ok := wm.GetPaths(testRunID, testSessionID)
	if !ok || req.Paths.Root != paths.Root {
		t.Fatalf("launch must pin the session workspace root, got %q", req.Paths.Root)
	}
	if req.ClaudeConfigDir == "" || req.ClaudeConfigBaseDir == "" {
		t.Fatal("config dir and base must be carried on the launch")
	}
	if !strings.HasPrefix(req.ClaudeConfigDir, req.ClaudeConfigBaseDir+string(filepathSeparator)) {
		t.Fatalf("config dir must live inside the base, got %q (base %q)", req.ClaudeConfigDir, req.ClaudeConfigBaseDir)
	}
}

// Follow-up turns resume the exact native session.
func TestClaudeTurnLaunch_ResumeUsesExactNativeSession(t *testing.T) {
	src, _, _ := launchSourceFixture(t)

	req, err := src.ClaudeTurnLaunch(context.Background(), testSessionID, testNativeID, false)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if got := argValue(req.Args, "--resume"); got != testNativeID {
		t.Fatalf("resume must address the exact native session, got %q", got)
	}
	for _, a := range req.Args {
		if a == "--session-id" {
			t.Fatal("resume turns must not carry --session-id")
		}
	}
}

// A session whose contributor is not claude is rejected.
func TestClaudeTurnLaunch_RejectsNonClaudeContributor(t *testing.T) {
	src, _, store := launchSourceFixture(t)

	// Seed an opencode-contributor session in the fixture run.
	ctx := context.Background()
	if _, err := store.CreateSession(ctx, "op-sess-oc", "lease-ls", storage.SessionRecord{
		ID: "sess-oc", RunID: testRunID, Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create opencode session: %v", err)
	}

	if _, err := src.ClaudeTurnLaunch(ctx, "sess-oc", testNativeID, true); err == nil {
		t.Fatal("non-claude sessions must be rejected")
	} else if !strings.Contains(err.Error(), "not claude") {
		t.Fatalf("expected contributor error, got %v", err)
	}
}

// A cprof-v1 profile is rejected with the typed unsupported-profile
// error (Claude requires v2).
var _ = fs.Stat // keep fs referenced

func TestClaudeTurnLaunch_RejectsLegacyProfile(t *testing.T) {
	dir := t.TempDir()
	stateDir := dir + "/state"
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	wm, err := workspace.NewWorkspaceManager(stateDir, dir+"/ws")
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}

	const lease = "lease-ls-legacy"
	ctx := context.Background()
	v1 := v2ClaudeProfile()
	v1.AlgoVersion = "cprof-v1"
	v1.ToolkitManifest = nil
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-legacy", ControllerLease: lease, RunID: "run-legacy",
		Brief: "legacy", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      v1,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-legacy", lease, storage.SessionRecord{
		ID: "sess-legacy", RunID: "run-legacy", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	src := NewClaudeTurnLaunchSource(store, wm, 8, filepath.Join(dir, "claude-config"))
	_, err = src.ClaudeTurnLaunch(context.Background(), "sess-legacy", testNativeID, true)
	var unsupported *storage.ErrUnsupportedProfile
	if err == nil || !asErrUnsupportedProfile(err, &unsupported) {
		t.Fatalf("legacy profile must fail with typed ErrUnsupportedProfile, got %T: %v", err, err)
	}
}

func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func listValue(args []string, flag string) []string {
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			out := []string{}
			for j := i + 1; j < len(args) && !strings.HasPrefix(args[j], "--"); j++ {
				out = append(out, args[j])
			}
			return out
		}
	}
	return nil
}

func containsAll(list []string, want ...string) bool {
	seen := map[string]bool{}
	for _, v := range list {
		seen[v] = true
	}
	for _, w := range want {
		if !seen[w] {
			return false
		}
	}
	return true
}
