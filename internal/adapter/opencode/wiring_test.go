//go:build unix

package opencode

// Production wiring evidence: the adapter is constructed through
// NewProductionOpenCodeAdapter with the real storage-backed identity and
// launch sources. The controlled stub `opencode` child is launched through
// the real PolicyExecutor via the production SessionLaunchSource — no
// server child or dispatch record is ever injected by the test.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func seedWiredRun(t *testing.T, stateDir, wsBase string) (*storage.Store, *workspace.WorkspaceManager, *OpenCodeAdapter) {
	t.Helper()
	ctx := context.Background()

	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	lease := "lease-wire"
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-wire", ControllerLease: lease, RunID: "run-wire",
		Brief: "wiring evidence", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"opencode", "git", "go"},
			Harnesses: map[string]storage.HarnessProfileSpec{
				"opencode": {Model: "stub/model", NativeAuthMode: "managed_by_council"},
			},
		},
	}); err != nil {
		t.Fatalf("create run with profile: %v", err)
	}

	if _, err := store.CreateSession(ctx, "op-sess-wire", lease, storage.SessionRecord{
		ID: "sess-wire", RunID: "run-wire", Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.AdoptController(ctx, "op-adopt-wire", "run-wire", "opencode", "controller-ref-wire", lease, nil, lease); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if _, err := store.ConnectRunController(ctx, "op-conn-wire", "run-wire", lease, 1, "test-instance"); err != nil {
		t.Fatalf("connect: %v", err)
	}

	ver, err := store.GetSessionVersion(ctx, "sess-wire")
	if err != nil {
		t.Fatalf("get version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-wire", lease, "sess-wire", ver, storage.PendingPrompt{
		SessionID: "sess-wire", TurnKey: "t-wire", Prompt: "wiring prompt",
	}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	ver, err = store.GetSessionVersion(ctx, "sess-wire")
	if err != nil {
		t.Fatalf("get version after queue: %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-wire", lease, "sess-wire", ver, "t-wire"); err != nil {
		t.Fatalf("release: %v", err)
	}

	wm, err := workspace.NewWorkspaceManager(stateDir, wsBase)
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}

	// Production construction: the test only supplies operator
	// configuration (binary path + scratch root).
	adp, err := NewProductionOpenCodeAdapter(store, wm, execpolicy.New(),
		NewOperatorProbeLaunchTemplate("opencode", t.TempDir(), lifecycleProfile()))
	if err != nil {
		t.Fatalf("production construction: %v", err)
	}
	return store, wm, adp
}

// Dispatch through production wiring reaches the controlled stub serve
// child over authenticated HTTP; Collect correlates the scripted assistant
// reply by the storage-derived attempt's deterministic message ID.
func TestWiring_ProductionDispatchCollectThroughStorageSeams(t *testing.T) {
	dir := t.TempDir()
	binDir := compileStubOpencode(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	store, wm, adp := seedWiredRun(t, filepath.Join(dir, "state"), filepath.Join(dir, "ws"))
	_ = store

	ctx := context.Background()
	ref := adapter.TurnRef{SessionID: "sess-wire", TurnKey: "t-wire"}

	// The identity seam resolves the attempt from the persisted dispatch
	// intent — never synthesized.
	attempt, ok := adp.identity.AttemptFor(ctx, ref)
	if !ok || attempt == "" {
		t.Fatal("storage identity source must resolve the released attempt")
	}

	outcome, err := adp.Dispatch(ctx, ref, "wiring prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		paths, _ := wm.GetPaths("run-wire", "sess-wire")
		if paths.Root != "" {
			if panics := readStubFile(t, paths.Root, ".stub-panic"); len(panics) > 0 {
				t.Fatalf("dispatch status=%v err=%v stub panics: %v", outcome.Status, err, panics)
			}
			if bad := readStubFile(t, paths.Root, ".stub-badjson"); len(bad) > 0 {
				t.Fatalf("dispatch status=%v err=%v stub rejected json: %v", outcome.Status, err, bad)
			}
			if hits := readStubFile(t, paths.Root, ".stub-hits"); len(hits) > 0 {
				t.Fatalf("dispatch status=%v err=%v stub hits: %v", outcome.Status, err, hits)
			}
		}
		adp.servers.mu.Lock()
		var tail string
		for _, sp := range adp.servers.children {
			tail = sp.stderrTail.String()
		}
		adp.servers.mu.Unlock()
		t.Fatalf("dispatch status=%v err=%v child stderr tail: %q", outcome.Status, err, tail)
		t.Fatalf("dispatch through production wiring: status=%v err=%v", outcome.Status, err)
	}

	result, err := adp.Collect(ctx, ref)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.Status != council.TurnCompleted || result.Output != "stub assistant response" {
		t.Fatalf("expected completed correlated turn, got status=%v output=%q", result.Status, result.Output)
	}

	// The native message ID must derive from the storage attempt.
	wantID, err := NativeMessageID(string(ref.SessionID), ref.TurnKey, attempt)
	if err != nil {
		t.Fatalf("message id: %v", err)
	}
	adp.mu.Lock()
	d := adp.dispatches[ref]
	adp.mu.Unlock()
	if d == nil || d.userMessageID != wantID {
		t.Fatalf("dispatch must record the attempt-derived message ID %q, got %+v", wantID, d)
	}

	// The child was launched into the session's allocated workspace and
	// every request authenticated.
	paths, ok := wm.GetPaths("run-wire", "sess-wire")
	if !ok {
		t.Fatal("workspace must be allocated for the wired session")
	}
	if hits := readStubFile(t, paths.Root, ".stub-authfail"); len(hits) != 0 {
		t.Fatalf("wiring traffic must authenticate, auth failures: %v", hits)
	}
	hits := readStubFile(t, paths.Root, ".stub-hits")
	joined := strings.Join(hits, "\n")
	if !strings.Contains(joined, "POST /session/sess-wire/prompt_async") {
		t.Fatalf("native dispatch must hit prompt_async, hits: %v", hits)
	}
	if !strings.Contains(joined, "GET /session/sess-wire/message") {
		t.Fatalf("collect must hit the message endpoint, hits: %v", hits)
	}
}
