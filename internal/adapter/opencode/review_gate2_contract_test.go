package opencode

// Gate 2 regressions: identifiers, payloads, project context,
// single-flight scope, and terminal delivery must hold when the native
// session ID deliberately differs from the Council session ID.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
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
		"agent":      nativeAgentPreset,
		"permission": "deny",
	} {
		if got, _ := payload[field].(string); got != want {
			t.Fatalf("create payload %s = %v, want %v (payload: %s)", field, got, want, raw)
		}
	}
	// The model must be the structured native reference
	// model:{providerID,id,variant} — not a flat string.
	model, _ := payload["model"].(map[string]any)
	if model == nil {
		t.Fatalf("create payload model must be a structured reference, got %v (payload: %s)", payload["model"], raw)
	}
	if model["providerID"] != "fake" || model["id"] != "model" {
		t.Fatalf("structured model mismatch: %v (payload: %s)", model, raw)
	}
}

// Malformed frozen model identifiers are rejected before any native
// resource is created.
func TestGate2_CreateSessionRejectsMalformedModel(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	for _, bad := range []string{"noseparator", "/leading", "trailing/", "a/b/c"} {
		_, err := adp.CreateSession(ctx, adapter.CreateSessionRequest{
			SessionID:   gate1Session,
			Contributor: "opencode",
			Config:      adapter.SessionConfig{Model: bad},
		})
		if err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("model %q must be rejected as malformed, got %v", bad, err)
		}
	}
	if got := fake.ledger.promptAsyncCount(); got != 0 {
		t.Fatalf("malformed model must never reach the native server, calls: %d", got)
	}
}

// Duplicate CreateSession with mismatched configuration fails closed for
// contributor, model, workspace, and tooling changes.
func TestGate2_CreateSessionIdempotencyRequiresMatchingConfig(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	const fresh = "sess-fresh-idem"
	fake.mu.Lock()
	fake.sessions[fresh] = &fakeSession{id: fresh}
	adp.servers.children[fresh] = &serverProcess{
		endpoint: fake.endpoint,
		username: gate1User,
		password: gate1Password,
	}
	fake.mu.Unlock()

	base := adapter.CreateSessionRequest{
		SessionID:   fresh,
		Contributor: "opencode",
		Config:      adapter.SessionConfig{Model: "fake/model", WorkspaceRoot: gate2Workspace, Tooling: []string{"opencode"}},
	}
	first, err := adp.CreateSession(ctx, base)
	if err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// Matching configuration: idempotent, same native session.
	same, err := adp.CreateSession(ctx, base)
	if err != nil || same.NativeSessionID != first.NativeSessionID {
		t.Fatalf("matching duplicate must be idempotent, got %+v err=%v", same, err)
	}

	negatives := []struct {
		name string
		mut  func(*adapter.CreateSessionRequest)
	}{
		{"contributor", func(r *adapter.CreateSessionRequest) { r.Contributor = "claude" }},
		{"model", func(r *adapter.CreateSessionRequest) { r.Config.Model = "other/model" }},
		{"workspace", func(r *adapter.CreateSessionRequest) { r.Config.WorkspaceRoot = "/elsewhere" }},
		{"tooling", func(r *adapter.CreateSessionRequest) { r.Config.Tooling = []string{"opencode", "extra"} }},
	}
	for _, tc := range negatives {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mut(&req)
			if _, err := adp.CreateSession(ctx, req); err == nil {
				t.Fatalf("mismatched %s must fail closed", tc.name)
			} else if !strings.Contains(err.Error(), "different configuration") {
				t.Fatalf("expected configuration-mismatch error, got %v", err)
			}
		})
	}

	// The binding still resolves to the original native session.
	if _, err := adp.Collect(ctx, adapter.TurnRef{SessionID: fresh, TurnKey: "t-idem"}); err == nil {
		t.Fatal("sanity: undispatched turn must not collect")
	}
	adp.mu.Lock()
	native := adp.bindings[adapter.SessionID(fresh)].nativeID
	adp.mu.Unlock()
	if native != first.NativeSessionID {
		t.Fatalf("binding must keep the original native session, got %q", native)
	}
}

// A verified terminal observed by the pump releases the native-session
// slot: a follow-up turn dispatches without any Collect call.
func TestGate2_SlotReleasedBySSETerminalWithoutCollect(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	refA := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-rel-a"}
	if outcome, err := adp.Dispatch(ctx, refA, "first"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch A: status=%v err=%v", outcome.Status, err)
	}
	msgID := adp.dispatches[refA].userMessageID

	// Terminal arrives via the pump while A is still uncollected.
	fake.mu.Lock()
	fake.sessions[gate2NativeSession].scriptedEvents = append(fake.sessions[gate2NativeSession].scriptedEvents,
		`{"type":"message.completed","parentID":"`+msgID+`"}`)
	fake.mu.Unlock()
	time.Sleep(150 * time.Millisecond)

	refB := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-rel-b"}
	bDone := make(chan adapter.DispatchOutcome, 1)
	go func() {
		outcome, _ := adp.Dispatch(ctx, refB, "second")
		bDone <- outcome
	}()
	select {
	case out := <-bDone:
		if out.Status != adapter.DispatchAccepted {
			t.Fatalf("follow-up must be accepted after the SSE terminal, got %v", out.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SSE terminal must release the native slot for the follow-up dispatch")
	}
	adp.mu.Lock()
	terminal := adp.dispatches[refA].terminal
	adp.mu.Unlock()
	if !terminal {
		t.Fatal("SSE terminal must mark the dispatch terminal")
	}
}

// A verified terminal from reconciliation releases the slot without any
// Collect call.
func TestGate2_SlotReleasedByReconcileTerminalWithoutCollect(t *testing.T) {
	adp, _ := gate2Fixture(t)
	ctx := context.Background()

	refA := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-rec-a"}
	if outcome, err := adp.Dispatch(ctx, refA, "first"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch A: status=%v err=%v", outcome.Status, err)
	}
	// The stub answered turn A; reconciliation observes the terminal.
	rec, err := adp.Reconcile(ctx, adapter.RecoveryRef{TurnRef: refA, Generation: 1})
	if err != nil || rec.Status != adapter.ReconciliationReachableTerminal {
		t.Fatalf("reconcile must verify the terminal, got %+v err=%v", rec, err)
	}

	refB := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-rec-b"}
	bDone := make(chan adapter.DispatchOutcome, 1)
	go func() {
		outcome, _ := adp.Dispatch(ctx, refB, "second")
		bDone <- outcome
	}()
	select {
	case out := <-bDone:
		if out.Status != adapter.DispatchAccepted {
			t.Fatalf("follow-up must be accepted after the reconciliation terminal, got %v", out.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reconciliation terminal must release the native slot")
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

// A stale terminal must not release a replacement turn's slot: the
// ownership check and deletion are atomic, so turn A's second terminal
// event cannot free the slot turn B legitimately holds.
func TestGate2_StaleTerminalCannotReleaseReplacementSlot(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	// Turn A: dispatch, then a terminal arrives via the pump.
	refA := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-stale-a"}
	if outcome, err := adp.Dispatch(ctx, refA, "first"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch A: status=%v err=%v", outcome.Status, err)
	}
	msgA := adp.dispatches[refA].userMessageID
	fake.mu.Lock()
	fake.sessions[gate2NativeSession].scriptedEvents = append(fake.sessions[gate2NativeSession].scriptedEvents,
		`{"type":"message.completed","parentID":"`+msgA+`"}`)
	fake.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for {
		adp.mu.Lock()
		held := len(adp.slots)
		adp.mu.Unlock()
		if held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn A's terminal must release its slot")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Turn B acquires the same native session's slot.
	refB := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-stale-b"}
	if outcome, err := adp.Dispatch(ctx, refB, "second"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch B: status=%v err=%v", outcome.Status, err)
	}

	// A stale duplicate terminal for A arrives while B owns the slot.
	fake.mu.Lock()
	fake.sessions[gate2NativeSession].scriptedEvents = append(fake.sessions[gate2NativeSession].scriptedEvents,
		`{"type":"message.completed","parentID":"`+msgA+`"}`)
	fake.mu.Unlock()
	time.Sleep(150 * time.Millisecond)

	// Turn C must still be blocked by B: the stale release was rejected.
	refC := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-stale-c"}
	cDone := make(chan adapter.DispatchOutcome, 1)
	go func() {
		outcome, _ := adp.Dispatch(ctx, refC, "third")
		cDone <- outcome
	}()
	select {
	case out := <-cDone:
		t.Fatalf("stale terminal must not release B's slot, C dispatched with %v", out.Status)
	case <-time.After(300 * time.Millisecond):
		// still blocked — correct
	}

	// B reaches a terminal outcome via Collect: the slot frees and C
	// proceeds.
	resultB, err := adp.Collect(ctx, refB)
	if err != nil || resultB.Status != council.TurnCompleted {
		t.Fatalf("collect B must be terminal, got %+v err=%v", resultB, err)
	}
	select {
	case out := <-cDone:
		if out.Status != adapter.DispatchAccepted {
			t.Fatalf("turn C must proceed after B is terminal, got %v", out.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn C must dispatch once B releases the slot")
	}
}

// ResumeSession carries the complete frozen configuration: an exactly
// matching CreateSession after resume is idempotent, not a mismatch.
func TestGate2_ResumePreservesConfigurationIdentity(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	const fresh = "sess-fresh-resume"
	fake.mu.Lock()
	fake.sessions[fresh] = &fakeSession{id: fresh, directory: gate2Workspace}
	adp.servers.children[fresh] = &serverProcess{
		endpoint: fake.endpoint,
		username: gate1User,
		password: gate1Password,
	}
	fake.mu.Unlock()

	created, err := adp.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   fresh,
		Contributor: "opencode",
		Config: adapter.SessionConfig{
			Model: "fake/model", WorkspaceRoot: gate2Workspace, Tooling: []string{"opencode"},
		},
	})
	if err != nil {
		t.Fatalf("initial create: %v", err)
	}

	// Resume with the full binding identity.
	binding := adapter.SessionBinding{
		SessionID:       fresh,
		Contributor:     "opencode",
		NativeSessionID: created.NativeSessionID,
		Config: adapter.SessionConfig{
			Model: "fake/model", WorkspaceRoot: gate2Workspace, Tooling: []string{"opencode"},
		},
	}
	if err := adp.ResumeSession(ctx, binding); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// The resumed binding must satisfy an exactly matching idempotent
	// create.
	again, err := adp.CreateSession(ctx, adapter.CreateSessionRequest{
		SessionID:   fresh,
		Contributor: "opencode",
		Config: adapter.SessionConfig{
			Model: "fake/model", WorkspaceRoot: gate2Workspace, Tooling: []string{"opencode"},
		},
	})
	if err != nil {
		t.Fatalf("matching create after resume must be idempotent, got %v", err)
	}
	if again.NativeSessionID != created.NativeSessionID {
		t.Fatalf("idempotent create must return the original native session, got %q", again.NativeSessionID)
	}
}

// Concurrent CreateSession calls create exactly one native session:
// matching callers share the result.
func TestGate2_ConcurrentCreateSessionCreatesOneNativeSession(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	const fresh = "sess-fresh-concurrent"
	fake.mu.Lock()
	fake.sessions[fresh] = &fakeSession{id: fresh}
	adp.servers.children[fresh] = &serverProcess{
		endpoint: fake.endpoint,
		username: gate1User,
		password: gate1Password,
	}
	fake.mu.Unlock()

	const callers = 6
	var wg sync.WaitGroup
	results := make([]adapter.SessionBinding, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = adp.CreateSession(ctx, adapter.CreateSessionRequest{
				SessionID:   fresh,
				Contributor: "opencode",
				Config:      adapter.SessionConfig{Model: "fake/model", WorkspaceRoot: gate2Workspace},
			})
		}()
	}
	wg.Wait()

	var nativeIDs []string
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		nativeIDs = append(nativeIDs, results[i].NativeSessionID)
	}
	unique := map[string]bool{}
	for _, id := range nativeIDs {
		unique[id] = true
	}
	if len(unique) != 1 {
		t.Fatalf("all callers must share one native session, got %v", nativeIDs)
	}

	// Exactly one native session was created on the server.
	fake.mu.Lock()
	created := 0
	for id := range fake.sessions {
		if strings.HasPrefix(id, "ses_fake_") {
			created++
		}
	}
	fake.mu.Unlock()
	if created != 1 {
		t.Fatalf("concurrent creates must produce exactly one native session, got %d", created)
	}
}

// Concurrent mismatched CreateSession calls fail closed and still create
// exactly one native session.
func TestGate2_ConcurrentMismatchedCreateSessionFailsClosed(t *testing.T) {
	adp, fake := gate2Fixture(t)
	ctx := context.Background()

	const fresh = "sess-fresh-mismatch"
	fake.mu.Lock()
	fake.sessions[fresh] = &fakeSession{id: fresh}
	adp.servers.children[fresh] = &serverProcess{
		endpoint: fake.endpoint,
		username: gate1User,
		password: gate1Password,
	}
	fake.mu.Unlock()

	workspaces := []string{gate2Workspace, "/elsewhere", gate2Workspace, "/also-elsewhere", gate2Workspace}
	var wg sync.WaitGroup
	errs := make([]error, len(workspaces))
	for i, wsRoot := range workspaces {
		wg.Add(1)
		go func(i int, wsRoot string) {
			defer wg.Done()
			_, errs[i] = adp.CreateSession(ctx, adapter.CreateSessionRequest{
				SessionID:   fresh,
				Contributor: "opencode",
				Config:      adapter.SessionConfig{Model: "fake/model", WorkspaceRoot: wsRoot},
			})
		}(i, wsRoot)
	}
	wg.Wait()

	// Matching callers share ONE result; mismatched callers fail closed.
	succeeded, failed := 0, 0
	var succeededWorkspaces []string
	for i, err := range errs {
		if err == nil {
			succeeded++
			succeededWorkspaces = append(succeededWorkspaces, workspaces[i])
			continue
		}
		failed++
		if !strings.Contains(err.Error(), "different configuration") {
			t.Fatalf("mismatched caller must fail closed, got %v", err)
		}
	}
	// All successes must agree on one workspace — a single native
	// session cannot serve two.
	uniqueWorkspaces := map[string]bool{}
	for _, wsRoot := range succeededWorkspaces {
		uniqueWorkspaces[wsRoot] = true
	}
	if len(uniqueWorkspaces) != 1 {
		t.Fatalf("successful callers must agree on one workspace, got %v", succeededWorkspaces)
	}
	if failed != len(workspaces)-succeeded || failed == 0 {
		t.Fatalf("mismatched callers must fail closed, got %d succeeded / %d failed", succeeded, failed)
	}
	fake.mu.Lock()
	created := 0
	for id := range fake.sessions {
		if strings.HasPrefix(id, "ses_fake_") {
			created++
		}
	}
	fake.mu.Unlock()
	if created != 1 {
		t.Fatalf("mismatched concurrent creates must not create extra native sessions, got %d", created)
	}
}

// Waiters on a failed concurrent creation receive the creator's typed
// error verbatim — for both pre-write refusal and post-write uncertainty
// — and no extra native session is created.
func TestGate2_ConcurrentCreateWaitersShareCreatorFailure(t *testing.T) {
	t.Run("pre-write refusal", func(t *testing.T) {
		adp, fake := gate2Fixture(t)
		ctx := context.Background()

		const fresh = "sess-fresh-prewrite"
		// The child's endpoint is dead: the inventory lookup fails
		// before the request is written.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		unreachable := fmt.Sprintf("http://%s", ln.Addr().String())
		_ = ln.Close()
		fake.mu.Lock()
		fake.sessions[fresh] = &fakeSession{id: fresh}
		adp.servers.children[fresh] = &serverProcess{
			endpoint: unreachable,
			username: gate1User,
			password: gate1Password,
		}
		fake.mu.Unlock()

		const callers = 4
		var wg sync.WaitGroup
		errs := make([]error, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = adp.CreateSession(ctx, adapter.CreateSessionRequest{
					SessionID:   fresh,
					Contributor: "opencode",
					Config:      adapter.SessionConfig{Model: "fake/model", WorkspaceRoot: gate2Workspace},
				})
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err == nil {
				t.Fatalf("caller %d: creation against a dead endpoint must fail", i)
			}
			var uncertain *adapter.ErrSessionCreationUncertain
			if !errorsAs(err, &uncertain) {
				t.Fatalf("caller %d must receive the creator's uncertain classification, got %T: %v", i, err, err)
			}
		}
		// No native session reached the fake server.
		fake.mu.Lock()
		created := 0
		for id := range fake.sessions {
			if strings.HasPrefix(id, "ses_fake_") {
				created++
			}
		}
		fake.mu.Unlock()
		if created != 0 {
			t.Fatalf("failed creation must not create native sessions, got %d", created)
		}
	})

	t.Run("post-write uncertainty", func(t *testing.T) {
		adp, fake := gate2Fixture(t)
		ctx := context.Background()

		const fresh = "sess-fresh-postwrite"
		fake.mu.Lock()
		fake.sessions[fresh] = &fakeSession{id: fresh}
		adp.servers.children[fresh] = &serverProcess{
			endpoint: fake.endpoint,
			username: gate1User,
			password: gate1Password,
		}
		fake.mu.Unlock()
		fake.armDropCreateSession()

		const callers = 4
		var wg sync.WaitGroup
		errs := make([]error, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = adp.CreateSession(ctx, adapter.CreateSessionRequest{
					SessionID:   fresh,
					Contributor: "opencode",
					Config:      adapter.SessionConfig{Model: "fake/model", WorkspaceRoot: gate2Workspace},
				})
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err == nil {
				t.Fatalf("caller %d: a dropped create response must fail", i)
			}
			var uncertain *adapter.ErrSessionCreationUncertain
			if !errorsAs(err, &uncertain) {
				t.Fatalf("caller %d must share the creator's uncertain classification, got %T: %v", i, err, err)
			}
		}
		// The dropped create persisted nothing: no native session exists.
		fake.mu.Lock()
		created := 0
		for id := range fake.sessions {
			if strings.HasPrefix(id, "ses_fake_") {
				created++
			}
		}
		fake.mu.Unlock()
		if created != 0 {
			t.Fatalf("an uncertain create must not record a native session, got %d", created)
		}
	})
}
