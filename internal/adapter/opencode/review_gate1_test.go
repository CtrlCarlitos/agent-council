package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

const (
	gate1Session  = "sess-g2"
	gate1User     = "svc-council"
	gate1Password = "generated-secret"
)

// fixtureExecutor "launches" a serve child backed by the fake HTTP
// server: it registers the freshly generated credentials with the server
// (a real server holds its own generated transport credentials) and
// prints the listen address on stdout, exactly as the real binary does.
type fixtureExecutor struct {
	fake *fakeOpenCodeServer
}

func (e *fixtureExecutor) Start(ctx context.Context, req execpolicy.LaunchRequest) (execpolicy.ManagedProcess, error) {
	if req.GeneratedServerEnv == nil {
		return nil, errors.New("fixture launch requires GeneratedServerEnv")
	}
	e.fake.allowCredentials(req.GeneratedServerEnv.Username, req.GeneratedServerEnv.Password)
	pr, pw := io.Pipe()
	go func() {
		fmt.Fprintf(pw, "listening on %s\n", e.fake.endpoint)
	}()
	return &fixtureProcess{out: pr, in: pw}, nil
}

// fixtureProcess is a no-op ManagedProcess whose stdout carries the
// listen address and whose lifetime ends on Terminate.
type fixtureProcess struct {
	out  *io.PipeReader
	in   *io.PipeWriter
	done chan struct{}
}

func (p *fixtureProcess) Stdin() io.WriteCloser { return nopWriteCloser{} }
func (p *fixtureProcess) Stdout() io.Reader     { return p.out }
func (p *fixtureProcess) Stderr() io.Reader     { return strings.NewReader("") }

func (p *fixtureProcess) Wait() (int, error) { <-p.done; return 0, nil }

func (p *fixtureProcess) ExecutableIdentity() execpolicy.ExeIdentity {
	return execpolicy.ExeIdentity{}
}

func (p *fixtureProcess) Interrupt() error { return nil }

func (p *fixtureProcess) Terminate(ctx context.Context) error {
	_ = ctx
	p.in.Close()
	return nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// gate1LaunchSource produces the exact approved serve shape.
type gate1LaunchSource struct{}

func (gate1LaunchSource) OpenCodeServeLaunch(_ context.Context, sessionID adapter.SessionID) (execpolicy.LaunchRequest, error) {
	return execpolicy.LaunchRequest{
		RunID:     "run-g1",
		SessionID: string(sessionID),
		Command:   "opencode",
		Args:      []string{"serve", "--hostname", "127.0.0.1", "--port", "0"},
		Paths:     workspace.WorkspacePaths{Root: os.TempDir(), Config: os.TempDir()},
	}, nil
}

// newGate1ServerManager builds a server manager that launches children
// backed by the fake HTTP server.
func newGate1ServerManager(fake *fakeOpenCodeServer, owner *OpenCodeAdapter) *serverManager {
	m := newServerManager(&fixtureExecutor{fake: fake}, gate1LaunchSource{})
	if owner != nil {
		m.nativeFor = owner.resolveNativeID
	}
	return m
}

// gate1Fixture wires an adapter against an authenticated fake OpenCode
// server. The serverProcess retains the same generated credentials a real
// launch would preserve from GeneratedServerEnv.
func gate1Fixture(t *testing.T) (*OpenCodeAdapter, *fakeOpenCodeServer) {
	t.Helper()
	fake, endpoint := startFakeServerWithAuth(t, gate1User, gate1Password)
	fake.mu.Lock()
	fake.sessions[gate1Session] = &fakeSession{id: gate1Session}
	fake.mu.Unlock()

	identity := &fakeGate1Identity{}
	adp := NewOpenCodeAdapter(nil, nil, identity, func(a *OpenCodeAdapter) {
		a.servers = newGate1ServerManager(fake, a)
		a.servers.mu.Lock()
		a.servers.children[gate1Session] = &serverProcess{
			endpoint: endpoint,
			username: gate1User,
			password: gate1Password,
		}
		a.servers.mu.Unlock()
	})
	return adp, fake
}

// gate1FixtureWithGrace is gate1Fixture with a short idle grace for
// automatic park scheduling.
func gate1FixtureWithGrace(t *testing.T, grace time.Duration) (*OpenCodeAdapter, *fakeOpenCodeServer) {
	t.Helper()
	fake, endpoint := startFakeServerWithAuth(t, gate1User, gate1Password)
	fake.mu.Lock()
	fake.sessions[gate1Session] = &fakeSession{id: gate1Session}
	fake.mu.Unlock()

	identity := &fakeGate1Identity{}
	adp := NewOpenCodeAdapter(nil, nil, identity, WithIdleGrace(grace), func(a *OpenCodeAdapter) {
		a.servers = newGate1ServerManager(fake, a)
		a.servers.mu.Lock()
		a.servers.children[gate1Session] = &serverProcess{
			endpoint: endpoint,
			username: gate1User,
			password: gate1Password,
		}
		a.servers.mu.Unlock()
	})
	return adp, fake
}

type fakeGate1Identity struct{}

func (f *fakeGate1Identity) AttemptFor(ctx context.Context, ref adapter.TurnRef) (string, bool) {
	return "att_1_" + ref.TurnKey, true
}

// expectedGate1MessageID derives the native message ID through the same
// identity seam the adapter consults during Dispatch.
func expectedGate1MessageID(t *testing.T, ref adapter.TurnRef) string {
	t.Helper()
	attempt, ok := (&fakeGate1Identity{}).AttemptFor(context.Background(), ref)
	if !ok {
		t.Fatal("identity seam returned no attempt")
	}
	id, err := NativeMessageID(string(ref.SessionID), ref.TurnKey, attempt)
	if err != nil {
		t.Fatalf("native message id: %v", err)
	}
	return id
}

// Concurrent duplicate dispatches must produce exactly one native
// prompt_async request and every caller must receive the launcher's verdict.
func TestGate1Review_ConcurrentDuplicateDispatchSingleLaunch(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-race"}
	ctx := context.Background()

	const callers = 5
	var wg sync.WaitGroup
	outcomes := make([]adapter.DispatchOutcome, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i], errs[i] = adp.Dispatch(ctx, ref, "prompt")
		}(i)
	}
	wg.Wait()

	for i := range outcomes {
		if errs[i] != nil || outcomes[i].Status != adapter.DispatchAccepted {
			t.Fatalf("caller %d: expected accepted verdict shared from launcher, got %v (err: %v)", i, outcomes[i].Status, errs[i])
		}
	}

	if got := fake.ledger.promptAsyncCount(); got != 1 {
		t.Fatalf("expected exactly 1 native prompt_async request, got %d", got)
	}
	if id := fake.ledger.promptAsyncMessageID(0); id != expectedGate1MessageID(t, ref) {
		t.Fatalf("native message ID mismatch: server recorded %q", id)
	}

	adp.mu.Lock()
	count := len(adp.dispatches)
	adp.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected exactly 1 dispatch record, got %d", count)
	}
}

// Dispatch-to-Collect correlation must flow through the identity seam: the
// server records the message ID Dispatch derived from the persisted attempt,
// and Collect returns the assistant message whose parentID is exactly that
// ID. The test never mutates adapter internals.
func TestGate1Review_CollectParentIDCorrelation(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-collect"}
	outcome, err := adp.Dispatch(ctx, ref, "prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}

	wantID := expectedGate1MessageID(t, ref)
	if got := fake.ledger.promptAsyncMessageID(0); got != wantID {
		t.Fatalf("dispatch must persist the seam-derived message ID: got %q want %q", got, wantID)
	}

	result, err := adp.Collect(ctx, ref)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.Status != council.TurnCompleted || result.ResultStatus != adapter.ResultAvailable {
		t.Fatalf("expected completed turn, got status=%v result=%v", result.Status, result.ResultStatus)
	}
	if result.Output != "fake assistant response" {
		t.Fatalf("expected parentID-correlated response, got %q", result.Output)
	}
}

// Reconciliation reports only verifiable state: unrecorded turns are
// uncertain, never fabricated as active or failed.
func TestGate1Review_ReconcileUncertainForUnrecorded(t *testing.T) {
	adp, _ := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-unrecorded"},
		Generation: 1,
	}
	out, err := adp.Reconcile(ctx, ref)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if out.Status != adapter.ReconciliationUncertain {
		t.Fatalf("unrecorded turn must reconcile as uncertain, got %v", out.Status)
	}
	if out.Reachability != council.VisibilityHostLost {
		t.Fatalf("expected host_lost visibility, got %v", out.Reachability)
	}
}

// Cancellation goes through the authenticated abort endpoint for the
// addressed session.
func TestGate1Review_CancelSendsAbort(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-cancel"}
	if outcome, err := adp.Dispatch(ctx, ref, "prompt"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}

	outcome, err := adp.Cancel(ctx, ref)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if outcome.Disposition != adapter.CancelConfirmed {
		t.Fatalf("expected confirmed, got %v", outcome.Disposition)
	}
	if got := fake.ledger.abortCount(); got != 1 {
		t.Fatalf("expected exactly 1 native abort request, got %d", got)
	}
	if path := fake.ledger.abortPath(0); path != "/session/"+gate1Session+"/abort" {
		t.Fatalf("abort must address the native session, got %q", path)
	}
	fake.mu.Lock()
	aborted := fake.sessions[gate1Session].abortRequested
	fake.mu.Unlock()
	if !aborted {
		t.Fatal("abort must mark the native session aborted")
	}
}

// A turn with no live serve child cannot be cancelled.
func TestGate1Review_CancelUnknownForUndispatched(t *testing.T) {
	adp, _ := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: "sess-unknown", TurnKey: "t-unknown"}
	outcome, err := adp.Cancel(ctx, ref)
	if outcome.Disposition != adapter.CancelUnknown {
		t.Fatalf("undispatched cancel must return unknown, got %v (err: %v)", outcome.Disposition, err)
	}
}

// After a post-write ambiguity where the server did record the turn, retry
// must verify by GET-by-message-ID and accept without resubmitting.
func TestGate1Review_RetryAfterAmbiguitySkipsResubmission(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-retry-recorded"}
	fake.armFlakyPromptAsync("drop-after-record")

	first, err := adp.Dispatch(ctx, ref, "prompt")
	if first.Status != adapter.DispatchUnknown {
		t.Fatalf("dropped response after record must be unknown, got %v (err: %v)", first.Status, err)
	}
	if got := fake.ledger.promptAsyncCount(); got != 1 {
		t.Fatalf("expected 1 native request after ambiguous attempt, got %d", got)
	}

	second, err := adp.Dispatch(ctx, ref, "prompt")
	if err != nil || second.Status != adapter.DispatchAccepted {
		t.Fatalf("retry must accept an already-recorded message, got %v (err: %v)", second.Status, err)
	}
	if second.Reason != "message already recorded; resubmission skipped" {
		t.Fatalf("retry must skip resubmission, reason: %q", second.Reason)
	}
	if got := fake.ledger.promptAsyncCount(); got != 1 {
		t.Fatalf("recorded message must not be resubmitted, prompt_async count: %d", got)
	}
}

// After a post-write ambiguity where the server did not record the turn, a
// verified 404 authorizes exactly one resubmission.
func TestGate1Review_RetryAfterAmbiguityResubmitsAfterVerified404(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-retry-resubmit"}
	fake.armFlakyPromptAsync("drop-before-record")

	first, err := adp.Dispatch(ctx, ref, "prompt")
	if first.Status != adapter.DispatchUnknown {
		t.Fatalf("dropped request before record must be unknown, got %v (err: %v)", first.Status, err)
	}

	second, err := adp.Dispatch(ctx, ref, "prompt")
	if err != nil || second.Status != adapter.DispatchAccepted {
		t.Fatalf("retry after verified 404 must resubmit and accept, got %v (err: %v)", second.Status, err)
	}
	if got := fake.ledger.promptAsyncCount(); got != 2 {
		t.Fatalf("expected exactly 1 resubmission (2 total native requests), got %d", got)
	}
	if id := fake.ledger.promptAsyncMessageID(1); id != expectedGate1MessageID(t, ref) {
		t.Fatalf("resubmission must reuse the same deterministic message ID, got %q", id)
	}
}

// A serve child whose retained credentials do not match the server's
// generated credentials is rejected with 401 and recorded as an
// authentication failure.
func TestGate1Review_RejectsMismatchedCredentials(t *testing.T) {
	fake, endpoint := startFakeServerWithAuth(t, gate1User, gate1Password)
	fake.mu.Lock()
	fake.sessions[gate1Session] = &fakeSession{id: gate1Session}
	fake.mu.Unlock()

	adp := NewOpenCodeAdapter(nil, nil, &fakeGate1Identity{}, func(a *OpenCodeAdapter) {
		a.servers = newGate1ServerManager(fake, a)
		a.servers.mu.Lock()
		a.servers.children[gate1Session] = &serverProcess{
			endpoint: endpoint,
			username: gate1User,
			password: "wrong-password",
		}
		a.servers.mu.Unlock()
	})
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-auth"}
	outcome, err := adp.Dispatch(ctx, ref, "prompt")
	if outcome.Status != adapter.DispatchRejected {
		t.Fatalf("mismatched credentials must reject dispatch, got %v (err: %v)", outcome.Status, err)
	}
	if got := fake.ledger.authFailureCount(); got != 1 {
		t.Fatalf("expected 1 recorded authentication failure, got %d", got)
	}
	if got := fake.ledger.promptAsyncCount(); got != 0 {
		t.Fatalf("unauthenticated request must not reach prompt_async, got %d calls", got)
	}
}

// A refused connection before the request is written is a rejection, not an
// ambiguous unknown.
func TestGate1Review_PreWriteRefusalIsRejected(t *testing.T) {
	// Reserve a port and close it: connections to it are refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	unreachable := fmt.Sprintf("http://%s", ln.Addr().String())
	_ = ln.Close()

	adp := NewOpenCodeAdapter(nil, nil, &fakeGate1Identity{}, func(a *OpenCodeAdapter) {
		a.servers = newServerManager(nil, nil)
		a.servers.mu.Lock()
		a.servers.children[gate1Session] = &serverProcess{
			endpoint: unreachable,
			username: gate1User,
			password: gate1Password,
		}
		a.servers.mu.Unlock()
	})
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-refused"}
	outcome, err := adp.Dispatch(ctx, ref, "prompt")
	if outcome.Status != adapter.DispatchRejected {
		t.Fatalf("pre-write refusal must be rejected, got %v (err: %v)", outcome.Status, err)
	}
}

// After a terminal turn with no other in-flight work, the idle grace
// elapses and the session's serve child is parked automatically.
func TestGate1Review_IdleGraceParksAfterTerminalTurn(t *testing.T) {
	adp, _ := gate1FixtureWithGrace(t, 80*time.Millisecond)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-idle"}
	if outcome, err := adp.Dispatch(ctx, ref, "prompt"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}
	result, err := adp.Collect(ctx, ref)
	if err != nil || result.Status != council.TurnCompleted {
		t.Fatalf("collect: status=%v err=%v", result.Status, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		adp.mu.Lock()
		timers := len(adp.parkTimers)
		adp.mu.Unlock()
		parked := adp.servers.isParked(gate1Session)
		children := 0
		adp.servers.mu.Lock()
		children = len(adp.servers.children)
		adp.servers.mu.Unlock()
		if parked && children == 0 && timers == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session must park after idle grace: parked=%v children=%d timers=%d", parked, children, timers)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Dispatching again after a park resumes the session: a replacement child
// is started against the same workspace and the turn is accepted.
func TestGate1Review_DispatchAfterParkResumes(t *testing.T) {
	adp, fake := gate1FixtureWithGrace(t, 80*time.Millisecond)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-resume"}
	if outcome, err := adp.Dispatch(ctx, ref, "prompt"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}
	if _, err := adp.Collect(ctx, ref); err != nil {
		t.Fatalf("collect: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !adp.servers.isParked(gate1Session) {
		if time.Now().After(deadline) {
			t.Fatal("session must park after idle grace before resume")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Prompt_async 404-resume verification needs the native session to
	// exist on the replacement server: the fake still serves sess-g2.
	reDispatch := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-resume-2"}
	outcome, err := adp.Dispatch(ctx, reDispatch, "second prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch after park must resume: status=%v err=%v", outcome.Status, err)
	}
	adp.servers.mu.Lock()
	children := len(adp.servers.children)
	adp.servers.mu.Unlock()
	if children != 1 {
		t.Fatalf("resume must register exactly one replacement child, children=%d", children)
	}
	if got := fake.ledger.promptAsyncCount(); got != 2 {
		t.Fatalf("resume dispatch must reach the replacement server, prompt_async=%d", got)
	}
}

// An in-flight (non-terminal) turn prevents idle parking even when another
// turn on the same session reached a terminal outcome.
func TestGate1Review_NoIdleParkWhileTurnInFlight(t *testing.T) {
	adp, fake := gate1FixtureWithGrace(t, 80*time.Millisecond)
	ctx := context.Background()

	terminalRef := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-idle-a"}
	if outcome, err := adp.Dispatch(ctx, terminalRef, "prompt"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}
	if _, err := adp.Collect(ctx, terminalRef); err != nil {
		t.Fatalf("collect: %v", err)
	}

	// Start a second turn and do not collect it: it stays non-terminal.
	inFlight := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-idle-b"}
	if outcome, err := adp.Dispatch(ctx, inFlight, "prompt 2"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch 2: status=%v err=%v", outcome.Status, err)
	}

	time.Sleep(300 * time.Millisecond)
	adp.servers.mu.Lock()
	children := len(adp.servers.children)
	adp.servers.mu.Unlock()
	if children != 1 {
		t.Fatalf("in-flight turn must prevent idle parking, children=%d", children)
	}
	_ = fake
}

// A server that closes the connection after consuming only a prefix of
// the request body produces an ambiguous outcome: body transmission had
// begun, so the dispatch is unknown and must never be retried blindly.
func TestGate1Review_MidBodyDropIsAmbiguous(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-midbody"}
	fake.armFlakyPromptAsync("drop-mid-body")

	outcome, err := adp.Dispatch(ctx, ref, "prompt")
	if outcome.Status != adapter.DispatchUnknown {
		t.Fatalf("mid-body drop must be unknown, got %v (err: %v)", outcome.Status, err)
	}
	if got := fake.ledger.promptAsyncCount(); got != 1 {
		t.Fatalf("expected 1 recorded native request, got %d", got)
	}
}

// CreateSession validates the configured model against the native model
// inventory BEFORE creating any native resource, then records the
// server-assigned native session ID.
func TestAdapterContract_CreateSessionModelValidation(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	// Model not in the inventory: rejected, no native session created.
	_, err := adp.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   gate1Session,
		Contributor: "opencode",
		Config:      adapter.SessionConfig{Model: "missing/model"},
	})
	if err == nil || !strings.Contains(err.Error(), "model inventory") {
		t.Fatalf("unknown model must fail closed, got %v", err)
	}
	if got := fake.ledger.authFailureCount(); got != 0 {
		t.Fatalf("inventory lookup must authenticate, failures: %d", got)
	}

	// Known model: binding carries the server-assigned native session ID.
	binding, err := adp.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   gate1Session,
		Contributor: "opencode",
		Config:      adapter.SessionConfig{Model: "fake/model"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if binding.NativeSessionID == "" || !strings.HasPrefix(binding.NativeSessionID, "ses_fake_") {
		t.Fatalf("native session ID must be server-assigned, got %q", binding.NativeSessionID)
	}

	// Idempotent on matching configuration.
	again, err := adp.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   gate1Session,
		Contributor: "opencode",
		Config:      adapter.SessionConfig{Model: "fake/model"},
	})
	if err != nil || again.NativeSessionID != binding.NativeSessionID {
		t.Fatalf("duplicate create must return the existing binding, got %+v err=%v", again, err)
	}
}

// ResumeSession verifies the persisted native session on the server: a
// verified 404 raises ErrNativeSessionMissing.
func TestAdapterContract_ResumeSessionVerifiesNative(t *testing.T) {
	adp, _ := gate1Fixture(t)
	ctx := context.Background()

	if err := adp.ResumeSession(ctx, adapter.SessionBinding{
		SessionID: gate1Session, Contributor: "opencode", NativeSessionID: gate1Session,
	}); err != nil {
		t.Fatalf("resume of an existing native session must succeed: %v", err)
	}

	err := adp.ResumeSession(ctx, adapter.SessionBinding{
		SessionID: gate1Session, Contributor: "opencode", NativeSessionID: "sess-gone",
	})
	if err == nil {
		t.Fatal("resume of a missing native session must fail")
	}
	var missing *ErrNativeSessionMissing
	if !errorsAs(err, &missing) {
		t.Fatalf("expected ErrNativeSessionMissing, got %T: %v", err, err)
	}
	if missing.NativeSessionID != "sess-gone" {
		t.Fatalf("missing binding must carry the native ID, got %+v", missing)
	}
}

// Reconciliation with the submission recorded but unanswered is
// reachable-active.
func TestAdapterContract_ReconcileReachableActive(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-active"}
	fake.armSuppressAssistant()
	if outcome, err := adp.Dispatch(ctx, ref, "prompt"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}

	out, err := adp.Reconcile(ctx, adapter.RecoveryRef{TurnRef: ref, Generation: 1})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if out.Status != adapter.ReconciliationReachableActive || out.Reachability != council.VisibilityReachable {
		t.Fatalf("unanswered recorded turn must be reachable-active, got %+v", out)
	}
	if out.Observed != council.TurnRunning {
		t.Fatalf("reachable-active must observe running, got %v", out.Observed)
	}
}

// A recorded pre-acceptance rejection with the message provably absent is
// definitively missing, not uncertain.
func TestAdapterContract_ReconcileDefinitivelyMissingAfterRejection(t *testing.T) {
	// Reserve a port and close it: the dispatch is rejected pre-write.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	unreachable := fmt.Sprintf("http://%s", ln.Addr().String())
	_ = ln.Close()

	adp := NewOpenCodeAdapter(nil, nil, &fakeGate1Identity{}, func(a *OpenCodeAdapter) {
		a.servers = newServerManager(nil, nil)
		a.servers.mu.Lock()
		a.servers.children[gate1Session] = &serverProcess{
			endpoint: unreachable,
			username: gate1User,
			password: gate1Password,
		}
		a.servers.mu.Unlock()
	})
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-missing"}
	outcome, err := adp.Dispatch(ctx, ref, "prompt")
	if outcome.Status != adapter.DispatchRejected {
		t.Fatalf("pre-write refusal must be rejected, got %v (err: %v)", outcome.Status, err)
	}

	rec, err := adp.Reconcile(ctx, adapter.RecoveryRef{TurnRef: ref, Generation: 1})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Status != adapter.ReconciliationDefinitivelyMissing {
		t.Fatalf("rejected pre-acceptance dispatch must be definitively missing, got %+v", rec)
	}
	if rec.Observed != council.TurnFailed || rec.Reachability != council.VisibilityReachable {
		t.Fatalf("definitively missing requires failed+reachable, got %+v", rec)
	}
}

// A post-write ambiguity with no verified native record stays uncertain:
// 404 or transport loss proves nothing about an orphan worker's work.
func TestAdapterContract_ReconcileUncertainAfterPossibleAcceptance(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-ambiguous"}
	fake.armFlakyPromptAsync("drop-mid-body")
	if outcome, err := adp.Dispatch(ctx, ref, "prompt"); outcome.Status != adapter.DispatchUnknown {
		t.Fatalf("mid-body drop must be unknown, got %v (err: %v)", outcome.Status, err)
	}

	rec, err := adp.Reconcile(ctx, adapter.RecoveryRef{TurnRef: ref, Generation: 1})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Status != adapter.ReconciliationUncertain || rec.Reachability != council.VisibilityHostLost {
		t.Fatalf("ambiguous outcome must stay uncertain, got %+v", rec)
	}
}

// errorsAs mirrors errors.As locally to keep test imports tidy.
func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}
