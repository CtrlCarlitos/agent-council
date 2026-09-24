package claude

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// filepathSeparator is the OS path separator as a string.
var filepathSeparator = string(filepath.Separator)

// evidenceRootFor writes the committed universe evidence for the frozen
// manifest (approved + denied complement) into a temp root and returns
// the root path.
func evidenceRootFor(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	tools := []string{"Read", "Glob", "Grep", "Bash", "Write", "WebSearch"}
	doc := map[string]any{"claude_code_version": "2.1.278", "tools": tools}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p := filepath.Join(root, "docs", "superpowers", "evidence", "ac008-native-tool-universe-2.1.278.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatalf("write universe: %v", err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(b))
	return root, digest
}

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

func v2ClaudeProfile(universeDigest string) storage.CanonicalProfile {
	p := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v2",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"claude", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"claude": {Model: "claude-haiku-4-5-20251001", NativeAuthMode: "inherited_host_keychain"},
		},
	}
	p.ToolkitManifest = &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
		ProbedCLIVersion:       "2.1.278",
		UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
		UniverseEvidenceDigest: universeDigest,
		ApprovedTools:          []string{"Read", "Glob", "Grep"},
		DeniedComplement:       []string{"Bash", "Write", "WebSearch"},
		ExpectedHooks:          []string{"SessionStart:startup"},
		TurnsBound:             8,
	}}
	return p
}

func launchSourceFixture(t *testing.T) (*ClaudeTurnLaunchSource, *workspace.WorkspaceManager, *storage.Store, string) {
	t.Helper()
	dir := t.TempDir()
	stateDir := dir + "/state"
	wsBase := dir + "/workspaces"
	evidenceRoot, universeDigest := evidenceRootFor(t)
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
		Profile:      v2ClaudeProfile(universeDigest),
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

	src := NewClaudeTurnLaunchSource(store, wm, filepath.Join(dir, "claude-config"), evidenceRoot)
	return src, wm, store, universeDigest
}

// The first turn uses --session-id with the caller-chosen native UUID
// and carries the exact contract prefix, frozen model, bound, and tool
// policy from the frozen manifest.
func TestClaudeTurnLaunch_FirstTurnSessionIDContract(t *testing.T) {
	src, wm, _, _ := launchSourceFixture(t)

	req, err := src.ClaudeTurnLaunch(context.Background(), testSessionID, testNativeID, true, "sha256:prompt-digest")
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if !execpolicy.IsClaudeLaunch(req) {
		t.Fatalf("launch must satisfy the exact contract: %q %v", req.Command, req.Args)
	}
	if got := argValue(req.Args, "--session-id"); got != testNativeID {
		t.Fatalf("first turn must use --session-id with the native ID, got %q", got)
	}
	if got := argValue(req.Args, "--model"); got != "claude-haiku-4-5-20251001" {
		t.Fatalf("frozen native model identity expected, got %q", got)
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
	src, _, _, _ := launchSourceFixture(t)

	req, err := src.ClaudeTurnLaunch(context.Background(), testSessionID, testNativeID, false, "sha256:prompt-digest")
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
	src, _, store, _ := launchSourceFixture(t)

	// Seed an opencode-contributor session in the fixture run.
	ctx := context.Background()
	if _, err := store.CreateSession(ctx, "op-sess-oc", "lease-ls", storage.SessionRecord{
		ID: "sess-oc", RunID: testRunID, Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create opencode session: %v", err)
	}

	if _, err := src.ClaudeTurnLaunch(ctx, "sess-oc", testNativeID, true, "sha256:pd"); err == nil {
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
	v1 := v2ClaudeProfile("sha256:" + strings.Repeat("a", 64))
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

	evidenceRoot, _ := evidenceRootFor(t)
	src := NewClaudeTurnLaunchSource(store, wm, filepath.Join(dir, "claude-config"), evidenceRoot)
	_, err = src.ClaudeTurnLaunch(context.Background(), "sess-legacy", testNativeID, true, "sha256:pd")
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

// The pinned universe is verified, not trusted: version drift and
// complement mismatches reject the launch.
func TestClaudeTurnLaunch_VerifiesPinnedUniverse(t *testing.T) {
	ctx := context.Background()
	const lease = "lease-ls-uni"
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

	base := t.TempDir()
	src := NewClaudeTurnLaunchSource(store, wm,
		filepath.Join(dir, "claude-config"), base)

	writeUniverseFile := func(t *testing.T, path string, version string, tools []string) string {
		t.Helper()
		doc := map[string]any{"claude_code_version": version, "tools": tools}
		b, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		full := filepath.Join(base, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, b, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
	}
	seedSession := func(t *testing.T, runID, sessionID string, profile storage.CanonicalProfile) {
		t.Helper()
		if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
			OpID: "op-" + runID, ControllerLease: lease, RunID: runID,
			Brief: "universe", SourceRepoIdentity: "example/repo",
			SourceCommit: "0123456789012345678901234567890123456789",
			SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
			Profile:      profile,
		}); err != nil {
			t.Fatalf("create run: %v", err)
		}
		if _, err := store.CreateSession(ctx, "op-"+sessionID, lease, storage.SessionRecord{
			ID: sessionID, RunID: runID, Contributor: "claude", Role: "reviewer",
			IsActiveContributor: true, State: "parked", Visibility: "reachable",
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	t.Run("version drift", func(t *testing.T) {
		driftDigest := writeUniverseFile(t, "run-uni-drift/sess-uni-drift/config/universe.json",
			"9.9.9", []string{"Read", "Glob", "Grep", "Bash", "Write", "WebSearch"})
		profile := v2ClaudeProfile(driftDigest)
		profile.ToolkitManifest.ToolkitManifest.UniverseEvidencePath =
			"run-uni-drift/sess-uni-drift/config/universe.json"
		seedSession(t, "run-uni-drift", "sess-uni-drift", profile) // opID derived
		_, err := src.ClaudeTurnLaunch(ctx, "sess-uni-drift", testNativeID, true, "sha256:pd")
		if err == nil || !strings.Contains(err.Error(), "pinned universe is for CLI") {
			t.Fatalf("version drift must be rejected, got %v", err)
		}
	})

	t.Run("complement mismatch", func(t *testing.T) {
		// Universe carries Bash/Write/WebSearch beyond the approved set,
		// but the frozen complement only lists Bash.
		digest := writeUniverseFile(t, "run-uni-comp/sess-uni-comp/config/universe.json",
			"2.1.278", []string{"Read", "Glob", "Grep", "Bash", "Write", "WebSearch"})
		profile := v2ClaudeProfile(digest)
		profile.ToolkitManifest.ToolkitManifest.UniverseEvidencePath =
			"run-uni-comp/sess-uni-comp/config/universe.json"
		profile.ToolkitManifest.ToolkitManifest.DeniedComplement = []string{"Bash"}
		seedSession(t, "run-uni-comp", "sess-uni-comp", profile)

		_, err := src.ClaudeTurnLaunch(ctx, "sess-uni-comp", testNativeID, true, "sha256:pd")
		if err == nil || !strings.Contains(err.Error(), "does not match universe") {
			t.Fatalf("complement mismatch must reject the launch, got %v", err)
		}
	})

	t.Run("approved tool outside universe", func(t *testing.T) {
		digest := writeUniverseFile(t, "run-uni-app/sess-uni-app/config/universe.json",
			"2.1.278", []string{"Read", "Glob"})
		profile := v2ClaudeProfile(digest)
		profile.ToolkitManifest.ToolkitManifest.UniverseEvidencePath =
			"run-uni-app/sess-uni-app/config/universe.json"
		seedSession(t, "run-uni-app", "sess-uni-app", profile)

		_, err := src.ClaudeTurnLaunch(ctx, "sess-uni-app", testNativeID, true, "sha256:pd")
		if err == nil || !strings.Contains(err.Error(), "absent from the pinned") {
			t.Fatalf("approved tool outside the universe must reject, got %v", err)
		}
	})
}
