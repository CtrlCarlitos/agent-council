//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/client"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// modeEnv returns the environment for a test-binary helper mode.
func modeEnv(mode string, dir string, extra ...string) []string {
	return append(os.Environ(), append([]string{
		"COUNCIL_TEST_MODE=" + mode,
		"COUNCIL_TEST_STATE_DIR=" + dir,
	}, extra...)...)
}

// startServiceMode launches the test binary as a real service process with
// the gated adapter and waits until it is actually ready. A crashed
// predecessor leaves stale discovery files behind, so readiness (not file
// existence) and the owning PID determine readiness.
func startServiceMode(t *testing.T, dir, instanceID string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestMainDispatch", "-test.timeout=10m")
	cmd.Env = modeEnv("service", dir, "COUNCIL_TEST_INSTANCE="+instanceID)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start service mode: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			t.Fatalf("service mode exited early: %v\n%s", cmd.ProcessState, readServiceLog(t, dir))
		}
		if c, err := client.New(dir); err == nil {
			if c.Meta().PID == cmd.Process.Pid {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				_, rerr := c.GetReadiness(ctx)
				cancel()
				if rerr == nil {
					return cmd
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	t.Fatalf("service mode did not become ready in time\n%s", readServiceLog(t, dir))
	return nil
}

func readServiceLog(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "service.log"))
	if err != nil {
		return "(no service log)"
	}
	return string(data)
}

func stopServiceMode(t *testing.T, dir string, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return
	default:
	}
	// Best-effort graceful stop over HTTP, then wait for process exit.
	stopViaHTTP(t, dir)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Logf("service mode required SIGKILL")
		<-done
	}
}

func stopViaHTTP(t *testing.T, dir string) {
	t.Helper()
	c, err := client.New(dir)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = c.StopService(ctx, c.Meta().InstanceID, false)
}

// Central work-survival acceptance: Client A is a real process that
// authorizes gated work over HTTP and exits while the execution is
// demonstrably active; the service commits the completed outcome; Client B
// (a separate process) retrieves the committed result; the follow-up prompt
// remains queued.
func TestSubprocess_WorkSurvival_ClientProcessExit(t *testing.T) {
	dir := shortStateDir(t)

	// Seed durable state: one releasable prompt and one follow-up prompt.
	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile", "lease-1"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adoptForTest(t, store, "run-1", "lease-1")
	sessRec, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-1", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	qRec, err := store.QueuePrompt(ctx, "op-q-work", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-work", Prompt: "survive client exit",
	})
	if err != nil {
		t.Fatalf("queue work prompt: %v", err)
	}
	if _, err := store.QueuePrompt(ctx, "op-q-next", "lease-1", "sess-1", qRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-next", Prompt: "follow-up work",
	}); err != nil {
		t.Fatalf("queue follow-up prompt: %v", err)
	}
	// Release must carry the current version (the follow-up queue advanced it).
	releaseVersion, err := store.GetSessionVersion(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session version: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	svc := startServiceMode(t, dir, "inst-work-survival")
	defer stopServiceMode(t, dir, svc)

	// Client A: real process, authorizes the work and exits while the
	// execution is active (the finish gate does not exist yet).
	clientA := exec.Command(os.Args[0], "-test.run=TestMainDispatch", "-test.timeout=10m")
	clientA.Env = modeEnv("client-a", dir,
		"COUNCIL_TEST_RUN=run-1",
		"COUNCIL_TEST_SESSION=sess-1",
		"COUNCIL_TEST_TURN=turn-work",
		fmt.Sprintf("COUNCIL_TEST_EXPECTED_VERSION=%d", releaseVersion),
	)
	outA, err := clientA.CombinedOutput()
	if err != nil {
		t.Fatalf("client A failed: %v, out: %s", err, string(outA))
	}

	// Client A exited while work was demonstrably active: the ledger shows
	// started but not completed.
	if !waitForLine(ledgerPath(dir), "started sess-1 turn-work", time.Second) {
		t.Fatalf("no started evidence in ledger: %v", readLines(ledgerPath(dir)))
	}
	for _, l := range readLines(ledgerPath(dir)) {
		if strings.HasPrefix(l, "completed sess-1 turn-work") {
			t.Fatalf("work completed before client A exit test window: %s", l)
		}
	}

	// Let the independent worker finish.
	if err := os.WriteFile(gateFinishPath(dir), []byte("go"), 0600); err != nil {
		t.Fatalf("create finish gate: %v", err)
	}
	if !waitForLine(ledgerPath(dir), "completed sess-1 turn-work", 10*time.Second) {
		t.Fatalf("worker did not complete: %v", readLines(ledgerPath(dir)))
	}

	// Client B: separate process retrieves the committed result.
	outFile := filepath.Join(dir, "client-b-out.json")
	runClientB := func() ([]byte, error) {
		clientB := exec.Command(os.Args[0], "-test.run=TestMainDispatch", "-test.timeout=10m")
		clientB.Env = modeEnv("client-b", dir,
			"COUNCIL_TEST_RUN=run-1",
			"COUNCIL_TEST_SESSION=sess-1",
			"COUNCIL_TEST_TURN=turn-work",
			"COUNCIL_TEST_OUT="+outFile,
		)
		return clientB.CombinedOutput()
	}
	// Client B may query before the service's supervisor commits; retry
	// until the committed result is visible.
	wantResult := "worker-result-sess-1-turn-work"
	var details storage.TurnDetails
	deadline := time.Now().Add(10 * time.Second)
	for {
		outB, err := runClientB()
		if err == nil {
			data, rerr := os.ReadFile(outFile)
			if rerr != nil {
				t.Fatalf("read client B output: %v", rerr)
			}
			if err := json.Unmarshal(data, &details); err != nil {
				t.Fatalf("decode client B output: %v", err)
			}
			if council.TurnStatus(details.Status) == council.TurnCompleted {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("client B did not observe committed result: %v, out: %s", err, string(outB))
		}
		time.Sleep(50 * time.Millisecond)
	}

	if details.Status != council.TurnCompleted {
		t.Fatalf("expected committed status completed, got %v", details.Status)
	}
	if details.Result != wantResult {
		t.Fatalf("expected committed result %q, got %q", wantResult, details.Result)
	}

	// The follow-up prompt must remain queued and unreleased.
	c, err := client.New(dir)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := c.GetTurnDetails(ctx, "run-1", "sess-1", "turn-next"); err == nil {
		t.Fatal("follow-up turn-next must not be released into a turn")
	}

	// Stop the service, then verify durable state.
	stopServiceMode(t, dir, svc)

	verify, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open verify store: %v", err)
	}
	defer verify.Close()
	hydrated, err := verify.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	sess, ok := hydrated.Sessions["sess-1"]
	if !ok {
		t.Fatal("session missing after work survival")
	}
	if _, queued := sess.PendingPrompts["turn-next"]; !queued {
		t.Fatalf("follow-up prompt must remain queued, got %+v", sess.PendingPrompts)
	}
	workTurn, ok := sess.Turns["turn-work"]
	if !ok || council.TurnStatus(workTurn.Status) != council.TurnCompleted {
		t.Fatalf("work turn must be completed, got %+v", sess.Turns)
	}
}

// Crash-restart recovery with independently retained evidence: the worker is
// an independent process whose ledger evidence survives the service crash.
// After restart, explicit reconciliation attaches to the saved native
// binding and resolves the turn from the retained evidence — no fabricated
// completion, no redispatch.
func TestSubprocess_CrashRecovery_IndependentLedger(t *testing.T) {
	dir := shortStateDir(t)

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	ctx := context.Background()
	if _, err := store.CreateRun(ctx, "op-run-2", "run-2", "brief", "spec", "profile", "lease-1"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	adoptForTest(t, store, "run-2", "lease-1")
	sessRec, err := store.CreateSession(ctx, "op-sess-2", "lease-1", storage.SessionRecord{
		ID: "sess-1", RunID: "run-2", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	qRec, err := store.QueuePrompt(ctx, "op-q-crash", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-crash", Prompt: "survive service crash",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}
	// Saved native binding for recovery attach.
	if _, err := store.SetNativeBinding(ctx, "op-bind-crash", "lease-1", "sess-1", qRec.CommittedVersion, storage.NativeBinding{
		LogicalSessionID: "sess-1", NativeSessionID: "native-sess-1", Harness: "claude",
	}); err != nil {
		t.Fatalf("set native binding: %v", err)
	}
	ver, err := store.GetSessionVersion(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session version: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	// Service 1: dispatch happens through a real client process exit path.
	svc1 := startServiceMode(t, dir, "inst-crash-1")
	clientA := exec.Command(os.Args[0], "-test.run=TestMainDispatch", "-test.timeout=10m")
	clientA.Env = modeEnv("client-a", dir,
		"COUNCIL_TEST_RUN=run-2",
		"COUNCIL_TEST_SESSION=sess-1",
		"COUNCIL_TEST_TURN=turn-crash",
		fmt.Sprintf("COUNCIL_TEST_EXPECTED_VERSION=%d", ver),
	)
	if out, err := clientA.CombinedOutput(); err != nil {
		stopServiceMode(t, dir, svc1)
		t.Fatalf("client A failed: %v, out: %s", err, string(out))
	}

	// Hard-crash the service while the independent worker is executing.
	if err := syscall.Kill(svc1.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill service: %v", err)
	}
	_ = svc1.Wait()

	// The worker process survives the service crash and finishes its work;
	// its ledger is the independent evidence.
	if err := os.WriteFile(gateFinishPath(dir), []byte("go"), 0600); err != nil {
		t.Fatalf("create finish gate: %v", err)
	}
	if !waitForLine(ledgerPath(dir), "completed sess-1 turn-crash", 10*time.Second) {
		t.Fatalf("independent worker did not complete: %v", readLines(ledgerPath(dir)))
	}

	// Service 2: restart against the crashed state directory.
	svc2 := startServiceMode(t, dir, "inst-crash-2")
	defer stopServiceMode(t, dir, svc2)

	// Before reconciliation: the reservation is preserved unresolved and no
	// redispatch occurred (exactly one started line from the original worker).
	startedCount := 0
	for _, l := range readLines(ledgerPath(dir)) {
		if strings.HasPrefix(l, "started sess-1 turn-crash") {
			startedCount++
		}
	}
	if startedCount != 1 {
		t.Fatalf("expected exactly 1 dispatch (no redispatch), got %d", startedCount)
	}

	// Explicit reconciliation attaches to the saved binding and resolves
	// from the independently retained evidence.
	c, err := client.New(dir)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	recCtx, recCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer recCancel()
	if _, err := c.ReconcileTurn(recCtx, "run-2", "sess-1", "turn-crash", "op-rec-crash", "lease-1"); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The reconciled outcome is the exact worker result from the ledger.
	details, err := c.GetTurnDetails(recCtx, "run-2", "sess-1", "turn-crash")
	if err != nil {
		t.Fatalf("get turn details: %v", err)
	}
	if details.Status != council.TurnCompleted || details.Result != "worker-result-sess-1-turn-crash" {
		t.Fatalf("unexpected reconciled outcome: %+v", details)
	}

	// Recovery attached to the exact original binding and probed with the
	// allocated generation.
	resumeLines := readLines(resumesPath(dir))
	if len(resumeLines) != 1 || resumeLines[0] != "resume sess-1 native-sess-1" {
		t.Fatalf("expected exactly one resume of the original binding, got %v", resumeLines)
	}
	probeLines := readLines(probesPath(dir))
	if len(probeLines) != 1 || !strings.HasPrefix(probeLines[0], "probe sess-1 turn-crash gen=") {
		t.Fatalf("expected exactly one probe of the original reference, got %v", probeLines)
	}

	// The recovery episode is closed in durable state.
	verify, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open verify store: %v", err)
	}
	defer verify.Close()
	recState, err := verify.GetSessionRecoveryState(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get recovery state: %v", err)
	}
	if recState.Visibility != "reachable" || recState.ActiveRecoveryGen != 0 {
		t.Fatalf("expected closed recovery episode, got %+v", recState)
	}

}
