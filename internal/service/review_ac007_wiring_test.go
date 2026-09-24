//go:build unix

package service

// Task 6 wiring evidence: the service constructs the OpenCode adapter
// with production seams — the DispatchIdentitySource backed by the real
// storage dispatch_intents table and the operator-owned probe launch
// template — and a dispatch through the service-constructed adapter
// carries the storage-derived attempt to the native server.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// mustLock acquires the service lock with test cleanup.
func mustLock(t *testing.T, stateDir string) *ServiceLock {
	t.Helper()
	lock, err := AcquireServiceLock(stateDir)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release() })
	return lock
}

// readStubState reads the stub binary's state file lines from the given
// workspace directory.
func readStubState(t *testing.T, root, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", name, err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// The service-constructed adapter dispatches through the storage-backed
// identity seam: the released turn's attempt flows into the deterministic
// native message ID, verified on the controlled stub server.
func TestServiceWiring_StorageBackedIdentitySeam(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	for _, d := range []string{stateDir, wsBase} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const lease = "lease-svc-wire"
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-svc", ControllerLease: lease, RunID: "run-svc",
		Brief: "service wiring", SourceRepoIdentity: "example/repo",
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
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-svc", lease, storage.SessionRecord{
		ID: "sess-svc", RunID: "run-svc", Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.AdoptController(ctx, "op-adopt-svc", "run-svc", "opencode", "controller-ref-svc", lease, nil, lease); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if _, err := store.ConnectRunController(ctx, "op-conn-svc", "run-svc", lease, 1, "test-instance"); err != nil {
		t.Fatalf("connect: %v", err)
	}
	ver, err := store.GetSessionVersion(ctx, "sess-svc")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-svc", lease, "sess-svc", ver, storage.PendingPrompt{
		SessionID: "sess-svc", TurnKey: "t-svc", Prompt: "service prompt",
	}); err != nil {
		t.Fatalf("queue: %v", err)
	}
	ver, err = store.GetSessionVersion(ctx, "sess-svc")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if _, err := store.ReleaseTurn(ctx, "op-rel-svc", lease, "sess-svc", ver, "t-svc"); err != nil {
		t.Fatalf("release: %v", err)
	}

	binDir := compileStubOpencodeBinary(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	probeProfile := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"opencode"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"opencode": {Model: "stub/model", NativeAuthMode: "managed_by_council"},
		},
	}

	srv, err := NewServerWithAdapter(store, mustLock(t, stateDir), ServerConfig{
		StateDir:                 stateDir,
		InstanceID:               "inst-svc-wire",
		AuthToken:                "tok-svc-wire",
		WorkspaceBaseDir:         wsBase,
		OpenCodeBinaryPath:       "opencode",
		OpenCodeProbeScratchRoot: filepath.Join(dir, "probe-scratch"),
		OpenCodeProbeProfile:     probeProfile,
	}, nil)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	if srv.adapter == nil {
		t.Fatal("service must construct the OpenCode adapter")
	}

	// The identity seam is storage-backed: the adapter resolves the
	// attempt from dispatch_intents — verified by dispatching.
	ref := adapter.TurnRef{SessionID: "sess-svc", TurnKey: "t-svc"}
	outcome, err := srv.adapter.Dispatch(ctx, ref, "service prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch through service wiring: status=%v err=%v", outcome.Status, err)
	}

	result, err := srv.adapter.Collect(ctx, ref)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.Status != council.TurnCompleted || result.Output != "stub assistant response" {
		t.Fatalf("expected completed correlated turn, got %+v", result)
	}

	// The stub must have received the native dispatch for the wired
	// session, with credentials enforced.
	ws, ok := srv.workspaceManager.GetPaths("run-svc", "sess-svc")
	if !ok {
		t.Fatal("workspace must be allocated for the wired session")
	}
	hits := readStubState(t, ws.Root, ".stub-hits")
	joined := strings.Join(hits, "\n")
	if !strings.Contains(joined, "POST /session/sess-svc/prompt_async") {
		t.Fatalf("native dispatch missing from stub hits: %v", hits)
	}
	if fails := readStubState(t, ws.Root, ".stub-authfail"); len(fails) != 0 {
		t.Fatalf("wiring traffic must authenticate: %v", fails)
	}
}
