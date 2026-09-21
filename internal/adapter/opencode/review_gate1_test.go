//go:build unix

package opencode

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func gate1Fixture(t *testing.T) (*OpenCodeAdapter, *fakeOpenCodeServer) {
	t.Helper()
	fake, endpoint := startFakeServer(t)
	identity := &fakeGate1Identity{}
	fake.mu.Lock()
	fake.sessions["sess-g2"] = &fakeSession{id: "sess-g2"}
	fake.mu.Unlock()
	adp := NewOpenCodeAdapter(nil, nil, identity, func(a *OpenCodeAdapter) {
		a.mu.Lock()
		a.servers = &serverManager{children: map[string]*serverProcess{
			"sess-g2": {endpoint: endpoint},
		}}
		a.mu.Unlock()
	})
	return adp, fake
}

type fakeGate1Identity struct{}

func (f *fakeGate1Identity) AttemptFor(ctx context.Context, ref adapter.TurnRef) (string, bool) {
	return "att_1_" + ref.TurnKey, true
}

// Concurrent duplicate dispatches must launch only once.
func TestGate1Review_ConcurrentDuplicateDispatchSingleLaunch(t *testing.T) {
	adp, _ := gate1Fixture(t)
	ref := adapter.TurnRef{SessionID: "sess-g2", TurnKey: "t-race"}
	ctx := context.Background()

	var wg sync.WaitGroup
	outcomes := make([]adapter.DispatchOutcome, 5)
	errs := make([]error, 5)
	var mu sync.Mutex
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := adp.Dispatch(ctx, ref, "prompt")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				outcomes[i] = out
			}
			if err != nil && errs[i] == nil {
				errs[i] = err
			}
		}()
	}
	wg.Wait()

	accepted := 0
	for i := range outcomes {
		if outcomes[i].Status == adapter.DispatchAccepted {
			accepted++
		}
	}
	if accepted != 5 {
		t.Fatalf("expected 5 accepted dispatches, got %d", accepted)
	}

	adp.mu.Lock()
	count := len(adp.dispatches)
	adp.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected exactly 1 dispatch record, got %d", count)
	}
}

// Collect finds the assistant message whose parentID equals the
// deterministic user-message ID.
func TestGate1Review_CollectParentIDCorrelation(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: "sess-g2", TurnKey: "t-collect"}
	msgID, _ := NativeMessageID("sess-g2", "t-collect", "att_1")

	adp.mu.Lock()
	adp.dispatches[ref] = &managedDispatch{userMessageID: msgID}
	adp.mu.Unlock()

	fake.mu.Lock()
	sess := &fakeSession{id: "sess-g2"}
	sess.messages = append(sess.messages, fakeMessage{
		ID: msgID, Role: "user",
		Parts: []fakePart{{Type: "text", Text: "prompt"}},
	})
	sess.messages = append(sess.messages, fakeMessage{
		ID: "msg_asst_parent", Role: "assistant", ParentID: msgID,
		Parts: []fakePart{{Type: "text", Text: "verified response"}},
	})
	fake.sessions["sess-g2"] = sess
	fake.mu.Unlock()

	result, err := adp.Collect(ctx, ref)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.Output != "verified response" {
		t.Fatalf("expected parentID-correlated response, got %q", result.Output)
	}
	if result.Status != council.TurnCompleted {
		t.Fatalf("expected completed, got %v", result.Status)
	}
}

// Reconciliation reports only verifiable state: unrecorded turns are
// uncertain, never fabricated as active or failed.
func TestGate1Review_ReconcileUncertainForUnrecorded(t *testing.T) {
	adp, _ := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "sess-g2", TurnKey: "t-unrecorded"},
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

// Cancellation sends abort to the correct session.
func TestGate1Review_CancelSendsAbort(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	msgID, _ := NativeMessageID("sess-g2", "t-cancel", "att_1")
	ref := adapter.TurnRef{SessionID: "sess-g2", TurnKey: "t-cancel"}
	adp.mu.Lock()
	adp.dispatches[ref] = &managedDispatch{userMessageID: msgID, nativeSessionID: "sess-g2"}
	adp.mu.Unlock()

	outcome, err := adp.Cancel(ctx, ref)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if outcome.Disposition != adapter.CancelConfirmed {
		t.Fatalf("expected confirmed, got %v", outcome.Disposition)
	}
	fake.mu.Lock()
	aborted := fake.sessions["sess-g2"].abortRequested
	fake.mu.Unlock()
	if !aborted {
		t.Fatal("abort endpoint must be called")
	}
}

// A never-dispatched turn cannot be cancelled.
func TestGate1Review_CancelUnknownForUndispatched(t *testing.T) {
	adp, _ := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: "sess-unknown", TurnKey: "t-unknown"}
	outcome, err := adp.Cancel(ctx, ref)
	if outcome.Disposition != adapter.CancelUnknown {
		t.Fatalf("undispatched cancel must return unknown, got %v (err: %v)", outcome.Disposition, err)
	}
	_ = strings.TrimSpace("")
}
