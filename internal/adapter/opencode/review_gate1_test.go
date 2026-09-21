package opencode

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

const (
	gate1Session  = "sess-g2"
	gate1User     = "svc-council"
	gate1Password = "generated-secret"
)

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
		a.mu.Lock()
		a.servers = &serverManager{children: map[string]*serverProcess{
			gate1Session: {
				endpoint: endpoint,
				username: gate1User,
				password: gate1Password,
			},
		}}
		a.mu.Unlock()
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
		a.mu.Lock()
		a.servers = &serverManager{children: map[string]*serverProcess{
			gate1Session: {
				endpoint: endpoint,
				username: gate1User,
				password: "wrong-password",
			},
		}}
		a.mu.Unlock()
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
		a.mu.Lock()
		a.servers = &serverManager{children: map[string]*serverProcess{
			gate1Session: {endpoint: unreachable, username: gate1User, password: gate1Password},
		}}
		a.mu.Unlock()
	})
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-refused"}
	outcome, err := adp.Dispatch(ctx, ref, "prompt")
	if outcome.Status != adapter.DispatchRejected {
		t.Fatalf("pre-write refusal must be rejected, got %v (err: %v)", outcome.Status, err)
	}
}
