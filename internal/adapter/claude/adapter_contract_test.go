//go:build unix

package claude

// Task 5 adapter contract evidence (POSIX — real executor + fixture
// child): process lifecycle through PolicyExecutor, parser-to-tap
// event delivery, terminal persistence, slot release, and durable
// attempt state across dispatch.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

type adapterHarness struct {
	store      *storage.Store
	wm         *workspace.WorkspaceManager
	adapter    *ClaudeAdapter
	wsRoot     string
	configBase string
	workspace  string
}

func newAdapterHarness(t *testing.T) *adapterHarness {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	configBase := filepath.Join(dir, "claude-config")
	for _, d := range []string{stateDir, wsBase, configBase} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	binDir := compileClaudeFixture(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	wm, err := workspace.NewWorkspaceManager(stateDir, wsBase)
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}

	// Evidence root and universe setup (before the profile so the digest is available).
	evidenceRoot := filepath.Join(dir, "evidence")
	os.MkdirAll(filepath.Join(evidenceRoot, "docs", "superpowers", "evidence"), 0o700)
	universeDoc := `{"claude_code_version":"2.1.278","tools":["Read","Glob","Grep","Bash","Write","WebSearch"]}`
	universePath := filepath.Join(evidenceRoot, "docs", "superpowers", "evidence", "ac008-native-tool-universe-2.1.278.json")
	os.WriteFile(universePath, []byte(universeDoc), 0o600)
	universeDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(universeDoc)))

	const lease = "lease-adapter"
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-adapter", ControllerLease: lease, RunID: "run-adapter",
		Brief: "adapter test", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile: func() storage.CanonicalProfile {
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
				TurnsBound:             8,
			}}
			return p
		}(),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-adapter", lease, storage.SessionRecord{
		ID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", RunID: "run-adapter", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	executor := execpolicy.New()
	launchSrc := NewClaudeTurnLaunchSource(store, wm, configBase, evidenceRoot)
	adp := NewClaudeAdapter(store, wm, executor, launchSrc, testIdentitySource{})

	ws, err := wm.AllocateWorkspace("run-adapter", "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", "none", "example/repo",
		"0123456789012345678901234567890123456789")
	if err != nil {
		t.Fatalf("allocate workspace: %v", err)
	}

	// Materialize the per-session config root (normally done by
	// CreateSession at session birth).
	tmpl := filepath.Join(dir, "template")
	os.MkdirAll(filepath.Join(tmpl, "skills"), 0o700)
	os.WriteFile(filepath.Join(tmpl, "settings.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(tmpl, "skills", "s.md"), []byte("skill"), 0o600)

	if _, _, err := MaterializeConfigRoot(tmpl, configBase, "run-adapter", "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"); err != nil {
		t.Fatalf("materialize config root: %v", err)
	}

	return &adapterHarness{
		store: store, wm: wm, adapter: adp,
		wsRoot: ws.Root, configBase: configBase, workspace: ws.Root,
	}
}

type testIdentitySource struct{}

func (testIdentitySource) AttemptFor(_ context.Context, ref adapter.TurnRef) (string, bool) {
	return "att_" + ref.TurnKey, true
}

func unusedIdentityFunc(ctx context.Context, ref adapter.TurnRef) (string, bool) {
	return "att_" + ref.TurnKey, true
}

// Happy path: dispatch → terminal → collect returns the correlated
// result text with the native session.
func TestClaudeAdapter_DispatchCollectLifecycle(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()
	ref := adapter.TurnRef{SessionID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", TurnKey: "t-1"}

	outcome, err := h.adapter.Dispatch(ctx, ref, "test prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}

	// Wait for the parser to finish (terminal close signals completion).
	stream, err := h.adapter.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	// Drain all events; the stream closes when the terminal is persisted.
	for range stream.Events() {
	}

	// Brief settle time: the runTurn goroutine persists the terminal
	// before closing the stream.
	time.Sleep(50 * time.Millisecond)

	// Debug: check the durable attempt state.
	dbAttempt, dbErr := h.store.GetLatestClaudeTurnAttempt(ctx, string(ref.SessionID), ref.TurnKey)
	if dbErr != nil {
		t.Fatalf("db query: %v", dbErr)
	}
	if dbAttempt == nil {
		t.Fatal("attempt not found in DB")
	}
	t.Logf("attempt: id=%s terminal=%v result=%v status=%q launch_count=%d",
		dbAttempt.AttemptID, dbAttempt.Terminal,
		dbAttempt.ResultPayload, dbAttempt.ObservedStatus, dbAttempt.LaunchCount)

	result, err := h.adapter.Collect(ctx, ref)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.Status != council.TurnCompleted || result.Output != "fixture response" {
		t.Fatalf("expected completed with fixture response, got %+v", result)
	}
}

// A dispatch rejected before launch leaves no terminal and the slot is
// released for a fresh dispatch.
func TestClaudeAdapter_PreLaunchRejectionReleasesSlot(t *testing.T) {
	h := newAdapterHarness(t)
	ctx := context.Background()

	// Contributor mismatch: the launch source rejects before the
	// executor starts.
	_, err := h.store.CreateSession(ctx, "op-sess-bad", "lease-adapter", storage.SessionRecord{
		ID: "sess-bad", RunID: "run-adapter", Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create bad session: %v", err)
	}

	ref := adapter.TurnRef{SessionID: "sess-bad", TurnKey: "t-bad"}
	outcome, _ := h.adapter.Dispatch(ctx, ref, "prompt")
	if outcome.Status != adapter.DispatchRejected {
		t.Fatalf("non-claude contributor must be rejected, got %v", outcome.Status)
	}

	// The slot is released: a valid dispatch still works.
	validRef := adapter.TurnRef{SessionID: "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", TurnKey: "t-valid"}
	validOutcome, err := h.adapter.Dispatch(ctx, validRef, "prompt")
	if err != nil || validOutcome.Status != adapter.DispatchAccepted {
		t.Fatalf("valid dispatch after rejection: %v %v", validOutcome.Status, err)
	}
}

func sha256Sum(data string) string {
	sum := sha256.Sum256([]byte(data))
	return fmt.Sprintf("%x", sum)
}
