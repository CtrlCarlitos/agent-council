//go:build !windows

package execpolicy_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Gate 2 review regressions (head 4534fd0 lineage): concurrent duplicate
// dispatch must launch once, launch failure must release the reservation,
// and reconciliation must report only verifiable execution state.

func gate2Fixture(t *testing.T, invocationsFile string) (*execpolicy.ManagedWorkerAdapter, *mockProfileStore) {
	t.Helper()
	stateDir := t.TempDir()
	workspaceBase := t.TempDir()
	wm, err := workspace.NewWorkspaceManager(stateDir, workspaceBase)
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}

	binDir := t.TempDir()
	// The fixture worker records its invocation and exits — unless the
	// test staged a ".hold" marker beside the invocations log, in which
	// case it stays alive until the ".release" marker appears. Tests that
	// assert on a LIVE execution use the hold so the assertion never races
	// the child's natural exit under load.
	script := "#!/bin/sh\n" +
		"echo \"$PATH_TEST_TOKEN $4\" >> " + invocationsFile + "\n" +
		"if [ -e " + invocationsFile + ".hold ]; then while [ ! -e " + invocationsFile + ".release ]; do sleep 0.02; done; fi\n" +
		"echo NATIVE_OK\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Setenv("PATH_TEST_TOKEN", "tok")
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	store := &mockProfileStore{
		profiles: map[string]storage.RunProfileRecord{
			"run-g2": {
				RunID:         "run-g2",
				WorkspaceMode: "none",
				Profile: storage.CanonicalProfile{
					AlgoVersion:         "cprof-v1",
					WorkspaceMode:       "none",
					IsolationStrictness: "permissive_dev",
					NetworkMode:         "unrestricted",
					Tooling:             []string{"claude"},
					Harnesses: map[string]storage.HarnessProfileSpec{
						"claude": {Model: "claude-3-7-sonnet", NativeAuthMode: "inherited_host_keychain"},
					},
				},
			},
		},
		sessions: map[string]storage.SessionMetadata{},
	}
	store.putSession("sess-g2")
	executor := execpolicy.New()
	adp := execpolicy.NewWorkerAdapter(wm, executor, store)
	if _, err := adp.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID: "sess-g2", Contributor: council.Claude, Config: adapter.SessionConfig{Tooling: []string{"claude"}},
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	return adp, store
}

func (m *mockProfileStore) putSession(sessionID string) {
	m.sessions[sessionID] = storage.SessionMetadata{SessionID: sessionID, RunID: "run-g2", Contributor: "claude"}
}

// Concurrent duplicate dispatches of one turn launch exactly one process;
// every caller receives the launcher's verdict.
func TestGate2Review_ConcurrentDuplicateDispatchLaunchesOnce(t *testing.T) {
	dir := t.TempDir()
	invocations := dir + "/invocations"
	adp, _ := gate2Fixture(t, invocations)

	ref := adapter.TurnRef{SessionID: "sess-g2", TurnKey: "t-race"}

	const n = 5
	firstErr := error(nil)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := adp.Dispatch(context.Background(), ref, "count me once")
			mu.Lock()
			defer mu.Unlock()
			if firstErr == nil {
				firstErr = err
			}
			if err == nil && out.Status != adapter.DispatchAccepted && firstErr == nil {
				firstErr = fmt.Errorf("unexpected status %v", out.Status)
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("concurrent dispatches must all succeed against one launch, got %v", firstErr)
	}

	var data []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		data, err = os.ReadFile(invocations)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("invocations never appeared: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 native invocation, got %d (%q)", len(lines), data)
	}
}

// A failed launch releases its reservation: a later dispatch of the same
// turn identity actually attempts execution instead of replaying the
// in-flight rejection.
func TestGate2Review_LaunchFailureReleasesReservation(t *testing.T) {
	invocations := t.TempDir() + "/invocations"
	adp, store := gate2Fixture(t, invocations)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: "sess-missing", TurnKey: "t-first"}

	// First dispatch fails: the session does not exist yet.
	out, err := adp.Dispatch(ctx, ref, "p")
	if err == nil || out.Status != adapter.DispatchRejected {
		t.Fatalf("first dispatch must be rejected for a missing session, got %+v err=%v", out, err)
	}

	// The session appears (operator completes setup).
	store.putSession("sess-missing")

	// The same identity now dispatches for real.
	out, err = adp.Dispatch(ctx, ref, "p")
	if err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("reservation must be released after launch failure, got %+v err=%v", out, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(invocations); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the retried dispatch must actually launch")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Reconciliation reports only verifiable state: live execution is
// reachable-active; a finished execution and a never-dispatched turn are
// uncertainty/absence — never a fabricated running worker.
func TestGate2Review_ReconcileReportsVerifiableStateOnly(t *testing.T) {
	invocations := t.TempDir() + "/invocations"
	adp, _ := gate2Fixture(t, invocations)
	ctx := context.Background()

	// Hold the worker alive so "live" is a fact, not a race against the
	// child's exit (the race detector and CI load made it lose).
	if err := os.WriteFile(invocations+".hold", []byte("hold\n"), 0o600); err != nil {
		t.Fatalf("stage hold: %v", err)
	}
	ref := adapter.TurnRef{SessionID: "sess-g2", TurnKey: "t-live"}
	if _, err := adp.Dispatch(ctx, ref, "live work"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	rref := adapter.RecoveryRef{TurnRef: ref, Generation: 1}
	out, err := adp.Reconcile(ctx, rref)
	if err != nil {
		t.Fatalf("reconcile live: %v", err)
	}
	if out.Status != adapter.ReconciliationReachableActive || out.Observed != council.TurnRunning {
		t.Fatalf("live execution must reconcile as reachable-active, got %+v", out)
	}
	if out.Reachability != council.VisibilityReachable {
		t.Fatalf("live execution must be reachable, got %v", out.Reachability)
	}

	// Release the worker, wait for natural completion, then reconcile:
	// the process is gone and the adapter can no longer verify live
	// execution — uncertainty, never a fabricated running worker.
	if err := os.WriteFile(invocations+".release", []byte("go\n"), 0o600); err != nil {
		t.Fatalf("stage release: %v", err)
	}
	_, err = adp.Collect(ctx, ref)
	if err != nil {
		t.Fatalf("collect after completion: %v", err)
	}
	waitDone(t, adp, ref)
	out, err = adp.Reconcile(ctx, rref)
	if err != nil {
		t.Fatalf("reconcile after completion: %v", err)
	}
	if out.Status != adapter.ReconciliationUncertain || out.Reachability != council.VisibilityHostLost {
		t.Fatalf("finished execution must reconcile as uncertain/host_lost, got %+v", out)
	}

	// A turn this process never dispatched is uncertain: the empty
	// in-memory record after a restart does not prove the orphan worker
	// died — it only proves this process cannot observe it.
	absent := adapter.RecoveryRef{TurnRef: adapter.TurnRef{SessionID: "sess-g2", TurnKey: "t-never"}, Generation: 1}
	out, err = adp.Reconcile(ctx, absent)
	if err != nil {
		t.Fatalf("reconcile absent: %v", err)
	}
	if out.Status != adapter.ReconciliationUncertain || out.Reachability != council.VisibilityHostLost {
		t.Fatalf("unrecorded execution must reconcile as uncertain/host_lost, got %+v", out)
	}
}

func waitDone(t *testing.T, adp *execpolicy.ManagedWorkerAdapter, ref adapter.TurnRef) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := adp.Collect(context.Background(), ref); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("execution did not finish in time")
}
