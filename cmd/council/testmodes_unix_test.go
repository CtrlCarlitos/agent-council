//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/client"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/service"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// TestMain dispatches test-binary helper modes used by the subprocess
// acceptance tests. Each mode is an independent OS process boundary:
//
//   - COUNCIL_TEST_MODE=service:  production service assembly with a
//     test-only gated adapter whose worker executions run in separate
//     worker processes and record evidence in an external ledger.
//   - COUNCIL_TEST_MODE=worker:   independent native-execution stand-in;
//     survives client and service process death by design.
//   - COUNCIL_TEST_MODE=client-a: authorizes work over HTTP and exits while
//     the execution is demonstrably active.
//   - COUNCIL_TEST_MODE=client-b: retrieves the committed turn outcome over
//     HTTP and writes it to a file.
//
// No fake adapter is registered in production code and no provider calls
// are made.
func TestMain(m *testing.M) {
	switch os.Getenv("COUNCIL_TEST_MODE") {
	case "":
		os.Exit(m.Run())
	case "service":
		runServiceMode()
	case "worker":
		runWorkerMode()
	case "client-a":
		runClientAMode()
	case "client-b":
		runClientBMode()
	default:
		fmt.Fprintf(os.Stderr, "unknown COUNCIL_TEST_MODE %q\n", os.Getenv("COUNCIL_TEST_MODE"))
		os.Exit(2)
	}
}

// Ledger and gate file conventions shared by the gated adapter and modes.
// The ledger is owned by the worker processes, outside service memory and
// outside Council's database: it is the independent execution evidence.
func ledgerPath(stateDir string) string     { return filepath.Join(stateDir, "ledger.jsonl") }
func gateFinishPath(stateDir string) string { return filepath.Join(stateDir, "gate-finish") }
func resumesPath(stateDir string) string    { return filepath.Join(stateDir, "resumes.jsonl") }
func probesPath(stateDir string) string     { return filepath.Join(stateDir, "probes.jsonl") }

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\n", line)
	return err
}

func readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// waitForLine polls a line-prefix file until it appears or the deadline expires.
func waitForLine(path, prefix string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, l := range readLines(path) {
			if strings.HasPrefix(l, prefix) {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// execWorker re-executes the test binary in worker mode for one turn.
func execWorker(testBinary, stateDir, session, turn, prompt string) *exec.Cmd {
	cmd := exec.Command(testBinary, "-test.run=TestMainDispatch", "-test.timeout=10m")
	cmd.Env = append(os.Environ(),
		"COUNCIL_TEST_MODE=worker",
		"COUNCIL_TEST_STATE_DIR="+stateDir,
		"COUNCIL_TEST_SESSION="+session,
		"COUNCIL_TEST_TURN="+turn,
		"COUNCIL_TEST_PROMPT="+prompt,
	)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd
}

// runWorkerMode is the independent native execution. It records "started",
// waits for the finish gate, computes a deterministic result, records
// "completed", and exits. Its lifetime is independent of the service.
func runWorkerMode() {
	stateDir := os.Getenv("COUNCIL_TEST_STATE_DIR")
	session := os.Getenv("COUNCIL_TEST_SESSION")
	turn := os.Getenv("COUNCIL_TEST_TURN")
	prompt := os.Getenv("COUNCIL_TEST_PROMPT")
	if stateDir == "" || session == "" || turn == "" {
		fmt.Fprintln(os.Stderr, "worker mode: missing env")
		os.Exit(2)
	}

	if err := appendLine(ledgerPath(stateDir), fmt.Sprintf("started %s %s prompt=%s", session, turn, prompt)); err != nil {
		fmt.Fprintf(os.Stderr, "worker mode: ledger append: %v\n", err)
		os.Exit(2)
	}

	gate := gateFinishPath(stateDir)
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "worker mode: gate timeout")
			os.Exit(3)
		}
		time.Sleep(10 * time.Millisecond)
	}

	result := fmt.Sprintf("worker-result-%s-%s", session, turn)
	if err := appendLine(ledgerPath(stateDir), fmt.Sprintf("completed %s %s %s", session, turn, result)); err != nil {
		fmt.Fprintf(os.Stderr, "worker mode: ledger append: %v\n", err)
		os.Exit(2)
	}
	os.Exit(0)
}

// runServiceMode assembles the production service with the gated adapter.
func runServiceMode() {
	stateDir := os.Getenv("COUNCIL_TEST_STATE_DIR")
	instanceID := os.Getenv("COUNCIL_TEST_INSTANCE")
	if stateDir == "" || instanceID == "" {
		fmt.Fprintln(os.Stderr, "service mode: missing env")
		os.Exit(2)
	}

	if err := os.MkdirAll(stateDir, 0700); err != nil {
		fmt.Fprintf(os.Stderr, "service mode: mkdir: %v\n", err)
		os.Exit(2)
	}
	_ = os.Chmod(stateDir, 0700)

	lock, err := service.AcquireServiceLock(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service mode: lock: %v\n", err)
		os.Exit(2)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		fmt.Fprintf(os.Stderr, "service mode: storage: %v\n", err)
		os.Exit(2)
	}

	authToken, err := service.GenerateAuthToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "service mode: token: %v\n", err)
		os.Exit(2)
	}

	cfg := service.ServerConfig{StateDir: stateDir, InstanceID: instanceID, AuthToken: authToken}
	srv, err := service.NewServerWithAdapter(store, lock, cfg, &gatedAdapter{stateDir: stateDir, ledgerTestBinary: os.Args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "service mode: server: %v\n", err)
		os.Exit(2)
	}

	meta := service.DiscoveryMeta{
		ProtocolVersion: 1,
		InstanceID:      instanceID,
		PID:             os.Getpid(),
		Transport:       "unix",
		Endpoint:        srv.SocketPath(),
		StateDir:        stateDir,
		StartedAt:       time.Now().UTC(),
	}
	if err := service.PublishDiscovery(stateDir, meta, authToken); err != nil {
		fmt.Fprintf(os.Stderr, "service mode: discovery: %v\n", err)
		os.Exit(2)
	}

	if err := srv.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "service mode: start: %v\n", err)
		os.Exit(2)
	}

	srv.StartSignalHandler()
	if err := srv.WaitForShutdown(context.Background()); err != nil {
		// Forced termination: process boundary exit without deferred cleanup.
		fmt.Fprintf(os.Stderr, "service mode: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runClientAMode authorizes gated work over HTTP and exits while the
// execution is demonstrably active (ledger shows started, not completed).
func runClientAMode() {
	stateDir := os.Getenv("COUNCIL_TEST_STATE_DIR")
	runID := os.Getenv("COUNCIL_TEST_RUN")
	session := os.Getenv("COUNCIL_TEST_SESSION")
	turn := os.Getenv("COUNCIL_TEST_TURN")
	expectedVersion := os.Getenv("COUNCIL_TEST_EXPECTED_VERSION")
	if stateDir == "" || runID == "" || session == "" || turn == "" || expectedVersion == "" {
		fmt.Fprintln(os.Stderr, "client-a mode: missing env")
		os.Exit(2)
	}

	c, err := client.New(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "client-a mode: client: %v\n", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Attach the adopted controller to this service instance before
	// authorizing work (AC-004: adoption grants no connection state).
	lease := os.Getenv("COUNCIL_TEST_LEASE")
	if lease == "" {
		lease = "lease-1"
	}
	gen := uint64(1)
	if g := os.Getenv("COUNCIL_TEST_EXPECTED_GENERATION"); g != "" {
		if parsed, err := strconv.ParseUint(g, 10, 64); err == nil {
			gen = parsed
		}
	}
	if _, err := c.ConnectRunController(ctx, runID, "op-conn-client-a", lease, gen); err != nil {
		fmt.Fprintf(os.Stderr, "client-a mode: connect: %v\n", err)
		os.Exit(2)
	}

	// Authorize the work through the release command.
	ver, _ := strconv.ParseInt(expectedVersion, 10, 64)
	if _, err := c.ReleaseTurn(ctx, runID, session, turn, "op-rel-client-a", lease, ver); err != nil {
		fmt.Fprintf(os.Stderr, "client-a mode: release: %v\n", err)
		os.Exit(2)
	}

	// Exit only once the execution is demonstrably active.
	if !waitForLine(ledgerPath(stateDir), "started ", 10*time.Second) {
		fmt.Fprintln(os.Stderr, "client-a mode: execution never started")
		os.Exit(3)
	}
	// Abrupt client exit while work is still active.
	os.Exit(0)
}

// runClientBMode retrieves the committed outcome over HTTP and writes it to
// the file named by COUNCIL_TEST_OUT.
func runClientBMode() {
	stateDir := os.Getenv("COUNCIL_TEST_STATE_DIR")
	runID := os.Getenv("COUNCIL_TEST_RUN")
	session := os.Getenv("COUNCIL_TEST_SESSION")
	turn := os.Getenv("COUNCIL_TEST_TURN")
	out := os.Getenv("COUNCIL_TEST_OUT")
	if stateDir == "" || runID == "" || session == "" || turn == "" || out == "" {
		fmt.Fprintln(os.Stderr, "client-b mode: missing env")
		os.Exit(2)
	}

	c, err := client.New(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "client-b mode: client: %v\n", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	details, err := c.GetTurnDetails(ctx, runID, session, turn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "client-b mode: get turn: %v\n", err)
		os.Exit(2)
	}
	data, _ := json.Marshal(details)
	if err := os.WriteFile(out, data, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "client-b mode: write out: %v\n", err)
		os.Exit(2)
	}
	os.Exit(0)
}

// gatedAdapter is a test-only adapter whose dispatches spawn independent
// worker processes and whose recovery reads the external ledger. It is
// never registered in production.
type gatedAdapter struct {
	stateDir string
	// ledgerTestBinary is this test binary, re-executed in worker mode.
	ledgerTestBinary string

	mu          sync.Mutex
	sessions    map[adapter.SessionID]adapter.SessionBinding
	dispatchCnt int
}

func (g *gatedAdapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	return adapter.ProbeReport{}, nil
}

func (g *gatedAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions == nil {
		g.sessions = make(map[adapter.SessionID]adapter.SessionBinding)
	}
	if existing, ok := g.sessions[req.SessionID]; ok {
		return existing, nil
	}
	binding := adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: "native-" + string(req.SessionID),
	}
	g.sessions[req.SessionID] = binding
	return binding, nil
}

func (g *gatedAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	// Attach must use the exact saved native binding.
	if err := appendLine(resumesPath(g.stateDir), fmt.Sprintf("resume %s %s", binding.SessionID, binding.NativeSessionID)); err != nil {
		return err
	}
	return nil
}

func (g *gatedAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	if err := ref.Validate(); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	g.mu.Lock()
	g.dispatchCnt++
	g.mu.Unlock()

	// The native execution runs in an independent worker process whose
	// lifetime is not tied to this service.
	cmd := execWorker(g.ledgerTestBinary, g.stateDir, string(ref.SessionID), ref.TurnKey, prompt)
	if err := cmd.Start(); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}
	// Reap asynchronously; the worker's evidence is the external ledger.
	go func() { _ = cmd.Wait() }()

	return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
}

func (g *gatedAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stream := adapter.NewBufferedStream(ref, 64)
	_ = stream.SendOrOverflow(adapter.Event{
		Ref:       ref,
		Type:      adapter.EventProgress,
		Status:    council.TurnRunning,
		Payload:   "gated execution observing",
		Timestamp: time.Now(),
	})
	// Close the stream when the ledger records completion or the context ends.
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = stream.CloseWithErr(nil)
				return
			case <-ticker.C:
				if ledgerHasCompletion(g.stateDir, string(ref.SessionID), ref.TurnKey) {
					_ = stream.CloseWithErr(nil)
					return
				}
			}
		}
	}()
	return stream, nil
}

func (g *gatedAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	// The gated harness has no native mid-turn cancellation.
	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnsupported, Reason: "gated adapter has no native cancellation"}, nil
}

func (g *gatedAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	for _, l := range readLines(ledgerPath(g.stateDir)) {
		if res, ok := parseCompletion(l, string(ref.SessionID), ref.TurnKey); ok {
			return adapter.TurnResult{
				Ref:          ref,
				Status:       council.TurnCompleted,
				ResultStatus: adapter.ResultAvailable,
				Output:       res,
				CompletedAt:  time.Now().UTC(),
			}, nil
		}
	}
	return adapter.TurnResult{
		Ref:          ref,
		Status:       council.TurnRunning,
		ResultStatus: adapter.ResultPending,
	}, nil
}

func (g *gatedAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	_ = appendLine(probesPath(g.stateDir), fmt.Sprintf("probe %s %s gen=%d", ref.SessionID, ref.TurnKey, ref.Generation))
	for _, l := range readLines(ledgerPath(g.stateDir)) {
		if res, ok := parseCompletion(l, string(ref.SessionID), ref.TurnKey); ok {
			return adapter.ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityReachable,
				Status:       adapter.ReconciliationReachableTerminal,
				Observed:     council.TurnCompleted,
				Result:       res,
			}, nil
		}
	}
	for _, l := range readLines(ledgerPath(g.stateDir)) {
		if strings.HasPrefix(l, fmt.Sprintf("started %s %s", ref.SessionID, ref.TurnKey)) {
			return adapter.ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityHostLost,
				Status:       adapter.ReconciliationUncertain,
				Observed:     council.TurnRunning,
			}, nil
		}
	}
	return adapter.ReconciliationOutcome{
		Ref:          ref,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationDefinitivelyMissing,
		Observed:     council.TurnFailed,
		Result:       "no ledger evidence for turn",
	}, nil
}

func ledgerHasCompletion(stateDir, session, turn string) bool {
	for _, l := range readLines(ledgerPath(stateDir)) {
		if _, ok := parseCompletion(l, session, turn); ok {
			return true
		}
	}
	return false
}

func parseCompletion(line, session, turn string) (string, bool) {
	parts := strings.Fields(line)
	if len(parts) == 4 && parts[0] == "completed" && parts[1] == session && parts[2] == turn {
		return parts[3], true
	}
	return "", false
}
