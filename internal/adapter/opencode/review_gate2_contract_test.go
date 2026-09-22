package opencode

// Gate 2 regressions: identifiers, payloads, project context,
// single-flight scope, and terminal delivery must hold when the native
// session ID deliberately differs from the Council session ID.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

const (
	gate2NativeSession = "ses_native_9f2c"
	gate2Workspace     = "/ws/gate2"
)

// gate2Fixture is gate1Fixture with a persisted binding whose
// server-assigned native session deliberately differs from the logical
// session, plus the native session seeded server-side for resume.
func gate2Fixture(t *testing.T) (*OpenCodeAdapter, *fakeOpenCodeServer) {
	t.Helper()
	fake, endpoint := startFakeServerWithAuth(t, gate1User, gate1Password)
	fake.mu.Lock()
	fake.sessions[gate1Session] = &fakeSession{id: gate1Session}
	fake.sessions[gate2NativeSession] = &fakeSession{id: gate2NativeSession, directory: gate2Workspace}
	fake.mu.Unlock()

	identity := &fakeGate1Identity{}
	adp := NewOpenCodeAdapter(nil, nil, identity, WithIdleGrace(80*time.Millisecond), func(a *OpenCodeAdapter) {
		a.servers = newGate1ServerManager(fake, a)
		a.servers.mu.Lock()
		a.servers.children[gate1Session] = &serverProcess{
			endpoint: endpoint,
			username: gate1User,
			password: gate1Password,
		}
		a.servers.mu.Unlock()
	})
	// The persisted binding: logical -> server-assigned native.
	adp.mu.Lock()
	adp.bindings[gate1Session] = &sessionBinding{
		nativeID:  gate2NativeSession,
		model:     "fake/model",
		directory: gate2Workspace,
	}
	adp.mu.Unlock()
	return adp, fake
}

// Every native request for a bound session must address the NATIVE
// session: prompt_async, message listing (Collect), and abort (Cancel).
func TestGate2_NativeSessionAddressing(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-native"}
	outcome, err := adp.Dispatch(ctx, ref, "prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}
	if got := fake.ledger.promptAsyncSessions[0]; got != gate2NativeSession {
		t.Fatalf("dispatch must address the native session, got %q", got)
	}
	if id := fake.ledger.promptAsyncMessageID(0); id == "" || !strings.HasPrefix(id, "msg_council_") {
		t.Fatalf("deterministic message ID expected, got %q", id)
	}

	result, err := adp.Collect(ctx, ref)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.Status != council.TurnCompleted {
		t.Fatalf("collect must correlate via the native session, got %v", result.Status)
	}

	outcomeC, err := adp.Cancel(ctx, ref)
	if err != nil || outcomeC.Disposition != adapter.CancelConfirmed {
		t.Fatalf("cancel: %v err=%v", outcomeC.Disposition, err)
	}
	if path := fake.ledger.abortPath(0); path != "/session/"+gate2NativeSession+"/abort" {
		t.Fatalf("abort must address the native session, got %q", path)
	}
}

// Park/resume with distinct identifiers: the pump stop keys on the
// native session, the parked child is keyed by the logical session, and
// the replacement start verifies the exact native session.
func TestGate2_ParkResumeWithNativeIdentifiers(t *testing.T) {
	adp, _ := gate2Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-park-native"}
	if outcome, err := adp.Dispatch(ctx, ref, "prompt"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}
	if _, err := adp.Collect(ctx, ref); err != nil {
		t.Fatalf("collect: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for adp.IdleParkedSessions() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("session must park after the idle grace")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The native pump must be stopped (keyed by native ID).
	adp.mu.Lock()
	pumps := len(adp.pumps)
	adp.mu.Unlock()
	if pumps != 0 {
		t.Fatalf("parked session must not keep a pump running, pumps=%d", pumps)
	}

	// Follow-up dispatch resumes a replacement and verifies the exact
	// native session.
	ref2 := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-park-native-2"}
	outcome, err := adp.Dispatch(ctx, ref2, "second prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("resume dispatch: status=%v err=%v", outcome.Status, err)
	}
	if got := adp.dispatches[ref2].nativeSessionID; got != gate2NativeSession {
		t.Fatalf("resumed turn must use the persisted native session, got %q", got)
	}
}

// CreateSession must send the complete frozen native payload: title,
// directory context, selected model, council agent preset, and the
// deny-by-default permission preset.
func TestGate2_CreateSessionPayloadIsComplete(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	// A logical session without a pre-installed binding: CreateSession
	// must actually reach the native server.
	const fresh = "sess-fresh"
	fake.mu.Lock()
	fake.sessions[fresh] = &fakeSession{id: fresh}
	adp.servers.children[fresh] = &serverProcess{
		endpoint: fake.endpoint,
		username: gate1User,
		password: gate1Password,
	}
	fake.mu.Unlock()

	binding, err := adp.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   fresh,
		Contributor: "opencode",
		Config:      adapter.SessionConfig{Model: "fake/model", WorkspaceRoot: gate2Workspace},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if binding.NativeSessionID == "" || !strings.HasPrefix(binding.NativeSessionID, "ses_fake_") {
		t.Fatalf("native session ID must be server-assigned, got %q", binding.NativeSessionID)
	}

	raw := fake.lastCreateSessionPayload()
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("create payload must be JSON: %v", err)
	}
	for field, want := range map[string]any{
		"title":      "council " + fresh,
		"directory":  gate2Workspace,
		"model":      "fake/model",
		"agent":      nativeAgentPreset,
		"permission": "deny",
	} {
		if got, _ := payload[field].(string); got != want {
			t.Fatalf("create payload %s = %v, want %v (payload: %s)", field, got, want, raw)
		}
	}
}

// ResumeSession fails closed when the native session belongs to a
// different project directory than the binding expects.
func TestGate2_ResumeFailsClosedOnDirectoryMismatch(t *testing.T) {
	adp, _ := gate2Fixture(t)
	ctx := context.Background()

	err := adp.ResumeSession(ctx, adapter.SessionBinding{
		SessionID:       gate1Session,
		Contributor:     "opencode",
		NativeSessionID: gate2NativeSession,
		Config:          adapter.SessionConfig{WorkspaceRoot: "/somewhere/else"},
	})
	if err == nil || !strings.Contains(err.Error(), "fails closed") {
		t.Fatalf("directory mismatch must fail closed, got %v", err)
	}

	// Matching directory resumes.
	if err := adp.ResumeSession(ctx, adapter.SessionBinding{
		SessionID:       gate1Session,
		Contributor:     "opencode",
		NativeSessionID: gate2NativeSession,
		Config:          adapter.SessionConfig{WorkspaceRoot: gate2Workspace},
	}); err != nil {
		t.Fatalf("matching resume must succeed: %v", err)
	}
}

// Single-flight is per native session: a second turn bound to the same
// native session cannot dispatch while the first is active, and proceeds
// once the first reaches a terminal outcome.
func TestGate2_SingleFlightIsPerNativeSession(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	refA := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-sf-a"}
	if outcome, err := adp.Dispatch(ctx, refA, "first"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch A: status=%v err=%v", outcome.Status, err)
	}

	// Turn B: same logical session, same native session. It must wait.
	refB := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-sf-b"}
	bDone := make(chan adapter.DispatchOutcome, 1)
	go func() {
		outcome, _ := adp.Dispatch(ctx, refB, "second")
		bDone <- outcome
	}()
	select {
	case out := <-bDone:
		t.Fatalf("second turn must not dispatch while the first is active, got %v", out.Status)
	case <-time.After(300 * time.Millisecond):
		// still waiting — correct
	}

	// Turn A reaches a terminal outcome (the stub answers every turn):
	// Collect marks it terminal and releases the slot; B proceeds.
	resultA, err := adp.Collect(ctx, refA)
	if err != nil || resultA.Status != council.TurnCompleted {
		t.Fatalf("collect A must be terminal, got %+v err=%v", resultA, err)
	}
	select {
	case out := <-bDone:
		if out.Status != adapter.DispatchAccepted {
			t.Fatalf("second turn must be accepted after the slot frees, got %v", out.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second turn must dispatch once the first turn is terminal")
	}
	if got := fake.ledger.promptAsyncSessions; len(got) != 2 || got[1] != gate2NativeSession {
		t.Fatalf("second turn must reach the same native session, ledger: %v", got)
	}
}

// A terminal event observed by the pump before any Observe caller
// attaches is retained and delivered on attach; reattachment replays it.
func TestGate2_TerminalRetainedBeforeObserveAndReattach(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-retain"}
	if outcome, err := adp.Dispatch(ctx, ref, "prompt"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}
	msgID := adp.dispatches[ref].userMessageID

	// The terminal event arrives on the NATIVE session while nobody
	// observes.
	fake.mu.Lock()
	fake.sessions[gate2NativeSession].scriptedEvents = append(fake.sessions[gate2NativeSession].scriptedEvents,
		`{"type":"message.completed","parentID":"`+msgID+`"}`)
	fake.mu.Unlock()
	time.Sleep(150 * time.Millisecond)

	// First observation: the retained terminal is delivered.
	stream, err := adp.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	got := drainStream(t, adp, fake, stream, 1, 3*time.Second)
	if got[0].Type != adapter.EventTerminal || got[0].Status != council.TurnCompleted {
		t.Fatalf("retained terminal must be delivered, got %+v", got[0])
	}

	// Reattachment replays the retained terminal.
	stream2, err := adp.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("re-observe: %v", err)
	}
	got2 := drainStream(t, adp, fake, stream2, 1, 3*time.Second)
	if got2[0].Type != adapter.EventTerminal {
		t.Fatalf("reattachment must replay the terminal, got %+v", got2[0])
	}
}

// A transport failure while listing messages cannot establish
// reachability: reconciliation stays uncertain with host visibility lost.
func TestGate2_ReconcileUncertainOnMessageTransportLoss(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-loss"}
	if outcome, err := adp.Dispatch(ctx, ref, "prompt"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}
	fake.armDropMessageList()

	rec, err := adp.Reconcile(ctx, adapter.RecoveryRef{TurnRef: ref, Generation: 1})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.Status != adapter.ReconciliationUncertain || rec.Reachability != council.VisibilityHostLost {
		t.Fatalf("transport loss must reconcile uncertain, got %+v", rec)
	}
}
