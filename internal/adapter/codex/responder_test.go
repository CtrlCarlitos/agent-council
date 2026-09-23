//go:build unix

package codex

// Task 7 approval-responder evidence (spec §3.6): the per-variant deny
// table against the committed 0.154.0 response schemas (exact payload
// bytes), duplicate re-deny with single mirror per ApprovalID, late
// denials after a verified terminal (deny + diagnostic, no state change),
// unparseable/unknown requests as protocol drift (Uncertain, never a
// guessed decision), both deny-equivalent variants failing closed before
// a live-verified approval_deny record and answered after one, responder
// install ordering relative to the pump arm, and the forbidden-token
// sweep over every emitted payload.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ── Shared scenario builders ────────────────────────────────────────────

// approvalRequestLine renders one server→client approval request frame.
func approvalRequestLine(id int, method, params string) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"` + method + `","params":` + params + `}`
}

func turnStartedLine(threadID string) string {
	return `{"jsonrpc":"2.0","method":"turn/started","params":{"threadId":"` + threadID + `","turnId":"` + testTurnID + `"}}`
}

func turnStartRespondRule(threadID string) string {
	return `{"respond": {"method":"turn/start","result":{"id":"` + testTurnID + `","threadId":"` + threadID + `","status":{"type":"inProgress"}}}}`
}

func turnInterruptRules(threadID string) []string {
	return []string{
		`{"respond": {"method":"turn/interrupt","result":{"action":"interrupted"}}}`,
		`{"emit_after_response": {"method":"turn/interrupt","line":{"jsonrpc":"2.0","method":"turn/completed","params":{"threadId":"` + threadID + `","turn":{"id":"` + testTurnID + `","status":"interrupted"}}}}}`,
	}
}

func baseScenario(h *adapterHarness) []string {
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	return append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
}

func execApprovalParams(callID string) string {
	return `{"callId":"` + callID + `","conversationId":"` + testThreadID + `"}`
}

func itemApprovalParams(itemID string) string {
	return `{"itemId":"` + itemID + `","threadId":"` + testThreadID + `","turnId":"` + testTurnID + `","startedAtMs":1}`
}

func permissionsApprovalParams(itemID string) string {
	return `{"itemId":"` + itemID + `","threadId":"` + testThreadID + `","turnId":"` + testTurnID + `","startedAtMs":1,"cwd":"/ws","permissions":{}}`
}

func userInputApprovalParams(itemID string) string {
	return `{"itemId":"` + itemID + `","threadId":"` + testThreadID + `","turnId":"` + testTurnID + `","isBlocking":true,"questions":[]}`
}

// ── Event collection helpers ────────────────────────────────────────────

func nextEvent(t *testing.T, s adapter.Stream) adapter.Event {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if !ok {
			t.Fatalf("stream closed before the expected event (err=%v)", s.Err())
		}
		return ev
	case <-time.After(8 * time.Second):
		t.Fatal("expected stream event never arrived")
		return adapter.Event{}
	}
}

func assertNoFurtherEvents(t *testing.T, s adapter.Stream) {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		if ok {
			t.Fatalf("unexpected extra event: %+v", ev)
		}
		return
	case <-time.After(400 * time.Millisecond):
		return
	}
}

// awaitTerminalEvent drains progress traffic (e.g. the accepted-interrupt
// marker) until the verified terminal event.
func awaitTerminalEvent(t *testing.T, s adapter.Stream) adapter.Event {
	t.Helper()
	for i := 0; i < 16; i++ {
		ev := nextEvent(t, s)
		if ev.Type == adapter.EventTerminal {
			return ev
		}
	}
	t.Fatal("terminal event never arrived")
	return adapter.Event{}
}

// ── Unit harness: responder against a real store, no child process ──────

type responderHarness struct {
	store   *storage.Store
	server  *CodexServer
	adapter *CodexAdapter

	mu      sync.Mutex
	replies []string
}

func newResponderHarness(t *testing.T) *responderHarness {
	t.Helper()
	store, err := storage.Open(storage.StoreOptions{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := NewCodexServer(execpolicy.New(), nil, CodexLaunchPolicy{})
	h := &responderHarness{store: store, server: server}
	h.adapter, err = NewCodexAdapter(store, server, CodexLaunchPolicy{}, "cprof-v3:sha256:test", nil, nil)
	if err != nil {
		t.Fatalf("new codex adapter: %v", err)
	}
	return h
}

func (h *responderHarness) replyFunc() func(json.RawMessage, any) error {
	return func(id json.RawMessage, result any) error {
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		h.mu.Lock()
		h.replies = append(h.replies, fmt.Sprintf("id=%s result=%s", id, raw))
		h.mu.Unlock()
		return nil
	}
}

func (h *responderHarness) responder() *Responder {
	return newResponder(h.adapter, testSessionID, h.replyFunc())
}

func (h *responderHarness) replyCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.replies)
}

func (h *responderHarness) lastReply() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.replies) == 0 {
		return ""
	}
	return h.replies[len(h.replies)-1]
}

// addLiveRun registers a live turn run with a durable attempt row (and,
// when deny records are given, an attestation row carrying them) and
// returns the run (stream capacity 16).
func (h *responderHarness) addLiveRun(t *testing.T, threadID, attemptID string, attestationID *string, denies ...ApprovalDenyRecord) *codexTurnRun {
	t.Helper()
	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: attemptID}
	if err := h.store.InsertCodexTurnAttempt(context.Background(), storage.CodexTurnAttempt{
		AttemptID: attemptID, SessionID: string(testSessionID), TurnKey: attemptID,
		PromptDigest: "pdig-v1:sha256:test", RolloutProtection: "protected",
		AttestationID: attestationID,
	}); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
	if len(denies) > 0 {
		if attestationID == nil {
			t.Fatal("deny records require an attestation id on the attempt")
		}
		raw, err := encodedDenyEquivalents(denies...)
		if err != nil {
			t.Fatalf("encode deny records: %v", err)
		}
		if _, err := h.store.DB().ExecContext(context.Background(), `
INSERT INTO codex_protection_attestations
	(attestation_id, codex_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, '0.154.0', ?, 'sha256:manifest', 'cprof-v3:sha256:profile', ?, '2026-09-23T00:00:00Z', 'op')
ON CONFLICT(attestation_id) DO UPDATE SET probe_results = excluded.probe_results`,
			*attestationID, codexPlatformIdentity(), raw); err != nil {
			t.Fatalf("upsert attestation row: %v", err)
		}
	}
	run := &codexTurnRun{
		ref: ref, attemptID: attemptID, nativeThreadID: threadID,
		stream: adapter.NewBufferedStream(ref, 16), termCh: make(chan string),
	}
	h.adapter.mu.Lock()
	h.adapter.turns[ref] = run
	h.adapter.mu.Unlock()
	return run
}

// encodedDenyEquivalents builds a valid cprot-v2 record frame carrying
// exactly the given approval_deny records.
func encodedDenyEquivalents(denies ...ApprovalDenyRecord) ([]byte, error) {
	att := ProtectionAttestation{
		CodexVersion:   "0.154.0",
		PlatformOS:     runtime.GOOS,
		PlatformFamily: "unix",
		ManifestDigest: "sha256:manifest",
		ProfileDigest:  "cprof-v3:sha256:profile",
		ApprovalDenies: denies,
		ProbedAt:       "2026-09-23T00:00:00Z",
		Actor:          "op",
	}
	return att.EncodeProbeRecords()
}

// insertHarnessAttestationWithRecords inserts the harness-matching
// cprot-v2 row whose probe_results carry exactly the given deny records.
func insertHarnessAttestationWithRecords(t *testing.T, h *adapterHarness, id string, records ...ApprovalDenyRecord) {
	t.Helper()
	raw, err := encodedDenyEquivalents(records...)
	if err != nil {
		t.Fatalf("encode attestation records: %v", err)
	}
	if _, err := h.store.DB().ExecContext(context.Background(), `
INSERT INTO codex_protection_attestations
	(attestation_id, codex_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, ?, ?, ?, ?, ?, '2026-09-23T00:00:00Z', 'op')`,
		id, h.policy.AppServerVersion, codexPlatformIdentity(), h.policy.ManifestDigest, h.profileDigest, raw); err != nil {
		t.Fatalf("insert attestation with records: %v", err)
	}
}

// ── §3.6 deny table: every variant, exact payload bytes ─────────────────

func TestResponder_PerVariantDenyTable(t *testing.T) {
	verifiedID := testAttestationID()
	bothRecords := []ApprovalDenyRecord{
		{MethodName: "item/permissions/requestApproval", RefusalKind: RefusalLiveVerifiedEquivalent},
		{MethodName: "item/tool/requestUserInput", RefusalKind: RefusalLiveVerifiedEquivalent},
	}
	cases := []struct {
		name           string
		method         string
		params         string
		gated          bool // deny-equivalent: needs the live-verified record
		wantResult     string
		wantApprovalID string
		evidenceFile   string
	}{
		{
			name: "execCommandApproval denied rejection", method: "execCommandApproval",
			params:         execApprovalParams("call-1"),
			wantResult:     `{"decision":{"denied":{"rejection":"` + councilApprovalRejection + `"}}}`,
			wantApprovalID: "call-1", evidenceFile: "ExecCommandApprovalResponse",
		},
		{
			name: "execCommandApproval approvalId correlation", method: "execCommandApproval",
			params:         `{"callId":"call-1b","conversationId":"` + testThreadID + `","approvalId":"app-9"}`,
			wantResult:     `{"decision":{"denied":{"rejection":"` + councilApprovalRejection + `"}}}`,
			wantApprovalID: "app-9", evidenceFile: "ExecCommandApprovalResponse",
		},
		{
			name: "applyPatchApproval denied rejection", method: "applyPatchApproval",
			params:         `{"callId":"call-2","conversationId":"` + testThreadID + `","fileChanges":{}}`,
			wantResult:     `{"decision":{"denied":{"rejection":"` + councilApprovalRejection + `"}}}`,
			wantApprovalID: "call-2", evidenceFile: "ApplyPatchApprovalResponse",
		},
		{
			name: "item/commandExecution decline", method: "item/commandExecution/requestApproval",
			params:         itemApprovalParams("item-1"),
			wantResult:     `{"decision":"decline"}`,
			wantApprovalID: "item-1", evidenceFile: "CommandExecutionRequestApprovalResponse",
		},
		{
			name: "item/fileChange decline", method: "item/fileChange/requestApproval",
			params:         itemApprovalParams("item-2"),
			wantResult:     `{"decision":"decline"}`,
			wantApprovalID: "item-2", evidenceFile: "FileChangeRequestApprovalResponse",
		},
		{
			name: "mcpServer elicitation decline", method: "mcpServer/elicitation/request",
			params:         `{"serverName":"srv","threadId":"` + testThreadID + `"}`,
			wantResult:     `{"action":"decline"}`,
			wantApprovalID: "srv", evidenceFile: "McpServerElicitationRequestResponse",
		},
		{
			name: "permissions empty grant (verified)", method: "item/permissions/requestApproval",
			params: permissionsApprovalParams("item-3"), gated: true,
			wantResult:     `{"permissions":{},"scope":"turn"}`,
			wantApprovalID: "item-3", evidenceFile: "PermissionsRequestApprovalResponse",
		},
		{
			name: "requestUserInput empty answers (verified)", method: "item/tool/requestUserInput",
			params: userInputApprovalParams("item-4"), gated: true,
			wantResult:     `{"answers":{}}`,
			wantApprovalID: "item-4", evidenceFile: "ToolRequestUserInputResponse",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponderHarness(t)
			var attID *string
			var denies []ApprovalDenyRecord
			if tc.gated {
				id := verifiedID
				attID = &id
				denies = bothRecords
			}
			run := h.addLiveRun(t, testThreadID, "att-table-"+tc.wantApprovalID, attID, denies...)

			h.responder().HandleRequest(ServerRequest{
				ID: json.RawMessage(`7`), Method: tc.method, Params: json.RawMessage(tc.params),
			})

			if h.replyCount() != 1 {
				t.Fatalf("exactly one reply expected, got %d (%v)", h.replyCount(), h.replies)
			}
			want := "id=7 result=" + tc.wantResult
			if got := h.lastReply(); got != want {
				t.Fatalf("exact deny payload mismatch:\n got %s\nwant %s", got, want)
			}

			// Field names must sit inside the installed binary's own
			// response schema: required ⊆ emitted ⊆ declared properties.
			required, properties := evidenceResponseSchema(t, tc.evidenceFile)
			var payload map[string]any
			if err := json.Unmarshal([]byte(tc.wantResult), &payload); err != nil {
				t.Fatalf("payload decode: %v", err)
			}
			for _, req := range required {
				if _, ok := payload[req]; !ok {
					t.Fatalf("payload %s missing schema-required field %q", tc.wantResult, req)
				}
			}
			for key := range payload {
				if !containsString(properties, key) {
					t.Fatalf("payload field %q is not in the schema properties %v", key, properties)
				}
			}

			// Mirror: tool_requested then tool_denied with the
			// correlation ApprovalID, both TurnRunning.
			ev1 := nextEvent(t, run.stream)
			ev2 := nextEvent(t, run.stream)
			if ev1.Type != adapter.EventToolRequested || ev2.Type != adapter.EventToolDenied {
				t.Fatalf("mirror order must be tool_requested then tool_denied, got %s then %s", ev1.Type, ev2.Type)
			}
			for _, ev := range []adapter.Event{ev1, ev2} {
				if ev.ApprovalID != tc.wantApprovalID {
					t.Fatalf("mirror ApprovalID: got %q want %q", ev.ApprovalID, tc.wantApprovalID)
				}
				if ev.Status != council.TurnRunning {
					t.Fatalf("mirror event must carry TurnRunning, got %s", ev.Status)
				}
				if ev.Ref != run.ref {
					t.Fatalf("mirror event ref mismatch: %+v", ev.Ref)
				}
			}
			assertNoFurtherEvents(t, run.stream)
		})
	}
}

// evidenceResponseSchema reads the committed 0.154.0 response schema and
// returns its top-level required fields and declared properties.
func evidenceResponseSchema(t *testing.T, name string) (required, properties []string) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "docs", "superpowers", "evidence", "ac009-schema-0.154.0", name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read evidence schema %s: %v", name, err)
	}
	var schema struct {
		Required   []string       `json:"required"`
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode evidence schema %s: %v", name, err)
	}
	for k := range schema.Properties {
		properties = append(properties, k)
	}
	return schema.Required, properties
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ── Deny-equivalent eligibility gating (fail closed) ────────────────────

func TestResponder_DenyEquivalentsFailClosedWithoutLiveVerifiedRecord(t *testing.T) {
	cases := []struct {
		name      string
		method    string
		params    string
		attID     *string
		denies    []ApprovalDenyRecord
		rawRow    string // probe_results written directly (record-free row)
		attemptID string
	}{
		{
			name:   "permissions advisory attempt without attestation",
			method: "item/permissions/requestApproval", params: permissionsApprovalParams("item-a"),
			attemptID: "att-gate-a",
		},
		{
			name:   "permissions attestation row without any deny record",
			method: "item/permissions/requestApproval", params: permissionsApprovalParams("item-b"),
			attID: strPtr(testAttestationID()), rawRow: "[]", attemptID: "att-gate-b",
		},
		{
			name:   "permissions record kind is native enum not live-verified",
			method: "item/permissions/requestApproval", params: permissionsApprovalParams("item-c"),
			attID:     strPtr(testAttestationID()),
			denies:    []ApprovalDenyRecord{{MethodName: "item/permissions/requestApproval", RefusalKind: RefusalNativeEnum}},
			attemptID: "att-gate-c",
		},
		{
			name:   "requestUserInput advisory attempt without attestation",
			method: "item/tool/requestUserInput", params: userInputApprovalParams("item-d"),
			attemptID: "att-gate-d",
		},
		{
			name:   "requestUserInput record for the other variant only",
			method: "item/tool/requestUserInput", params: userInputApprovalParams("item-e"),
			attID:     strPtr(testAttestationID()),
			denies:    []ApprovalDenyRecord{{MethodName: "item/permissions/requestApproval", RefusalKind: RefusalLiveVerifiedEquivalent}},
			attemptID: "att-gate-e",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponderHarness(t)
			run := h.addLiveRun(t, testThreadID, tc.attemptID, tc.attID, tc.denies...)
			if tc.attID != nil && tc.denies == nil {
				// A durable row exists but its record frame is not a
				// decodable cprot-v2 frame: undecodable ⇒ unverified.
				if _, err := h.store.DB().ExecContext(context.Background(), `
INSERT INTO codex_protection_attestations
	(attestation_id, codex_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, '0.154.0', ?, 'sha256:manifest', 'cprof-v3:sha256:profile', ?, '2026-09-23T00:00:00Z', 'op')
ON CONFLICT(attestation_id) DO UPDATE SET probe_results = excluded.probe_results`,
					*tc.attID, codexPlatformIdentity(), tc.rawRow); err != nil {
					t.Fatalf("seed undecodable row: %v", err)
				}
			}
			before, err := h.store.GetCodexTurnAttempt(context.Background(), tc.attemptID)
			if err != nil || before == nil {
				t.Fatalf("attempt lookup: %v", err)
			}

			h.responder().HandleRequest(ServerRequest{
				ID: json.RawMessage(`8`), Method: tc.method, Params: json.RawMessage(tc.params),
			})

			if h.replyCount() != 0 {
				t.Fatalf("no payload may be sent for an unverified deny-equivalent, got %v", h.replies)
			}
			sawDiagnostic := false
		loop:
			for {
				select {
				case ev, ok := <-run.stream.Events():
					if !ok {
						break loop
					}
					if ev.Type == adapter.EventToolRequested || ev.Type == adapter.EventToolDenied {
						t.Fatalf("unverified receipt must not mirror %s", ev.Type)
					}
					if ev.Type == adapter.EventProgress && strings.Contains(ev.Payload, "unverified deny-equivalent") {
						sawDiagnostic = true
					}
				default:
					break loop
				}
			}
			if !sawDiagnostic {
				t.Fatal("the fail-closed receipt must record a diagnostic")
			}
			after, err := h.store.GetCodexTurnAttempt(context.Background(), tc.attemptID)
			if err != nil || after == nil {
				t.Fatalf("attempt lookup: %v", err)
			}
			if after.ObservedStatus != "uncertain" {
				t.Fatalf("unverified receipt must classify the attempt Uncertain, got %q", after.ObservedStatus)
			}
			if after.TransitionVersion <= before.TransitionVersion {
				t.Fatalf("the uncertain classification must be durably recorded (transition_version %d → %d)",
					before.TransitionVersion, after.TransitionVersion)
			}
		})
	}
}

// ── Duplicates, late, in-flight-race ────────────────────────────────────

func TestResponder_DuplicateReDenyMirrorsOnce(t *testing.T) {
	h := newResponderHarness(t)
	run := h.addLiveRun(t, testThreadID, "att-dup", nil)
	r := h.responder()
	sr := ServerRequest{Method: "execCommandApproval", Params: json.RawMessage(execApprovalParams("call-dup"))}

	sr.ID = json.RawMessage(`21`)
	r.HandleRequest(sr)
	sr.ID = json.RawMessage(`22`)
	r.HandleRequest(sr)

	if h.replyCount() != 2 {
		t.Fatalf("duplicates must each be re-denied, got %d replies", h.replyCount())
	}
	for _, reply := range h.replies {
		if !strings.Contains(reply, `"denied"`) {
			t.Fatalf("every re-deny must remain a denial: %s", reply)
		}
	}
	ev1 := nextEvent(t, run.stream)
	ev2 := nextEvent(t, run.stream)
	if ev1.Type != adapter.EventToolRequested || ev2.Type != adapter.EventToolDenied {
		t.Fatalf("first denial mirrors the pair, got %s/%s", ev1.Type, ev2.Type)
	}
	if ev1.ApprovalID != "call-dup" || ev2.ApprovalID != "call-dup" {
		t.Fatalf("mirror ApprovalID mismatch: %q/%q", ev1.ApprovalID, ev2.ApprovalID)
	}
	assertNoFurtherEvents(t, run.stream)
}

func TestResponder_LateDenialAfterVerifiedTerminal(t *testing.T) {
	h := newResponderHarness(t)
	run := h.addLiveRun(t, testThreadID, "att-late", nil)
	if err := h.store.SetCodexAttemptTerminal(context.Background(), "att-late", "completed", `{"final":true}`, ""); err != nil {
		t.Fatalf("commit terminal: %v", err)
	}
	run.notifyTerminal("completed")
	before, err := h.store.GetCodexTurnAttempt(context.Background(), "att-late")
	if err != nil || before == nil {
		t.Fatalf("attempt lookup: %v", err)
	}

	h.responder().HandleRequest(ServerRequest{
		ID: json.RawMessage(`31`), Method: "execCommandApproval",
		Params: json.RawMessage(execApprovalParams("call-late")),
	})

	if h.replyCount() != 1 {
		t.Fatalf("a late request is still denied, got %d replies", h.replyCount())
	}
	if !strings.Contains(h.lastReply(), councilLateApprovalRejection) {
		t.Fatalf("late denial must carry the late rejection text: %s", h.lastReply())
	}
	assertNoFurtherEvents(t, run.stream)
	after, err := h.store.GetCodexTurnAttempt(context.Background(), "att-late")
	if err != nil || after == nil {
		t.Fatalf("attempt lookup: %v", err)
	}
	if !after.Terminal || after.ObservedStatus != "completed" || after.TransitionVersion != before.TransitionVersion {
		t.Fatalf("late denial must not change state: before=%+v after=%+v", before, after)
	}
	if len(run.runDiagnostics()) == 0 || !strings.Contains(strings.Join(run.runDiagnostics(), " "), "late approval denial") {
		t.Fatalf("late denial must record a diagnostic on the attempt run: %v", run.runDiagnostics())
	}
}

func TestResponder_LateDenialWithoutLiveRun(t *testing.T) {
	h := newResponderHarness(t)
	h.responder().HandleRequest(ServerRequest{
		ID: json.RawMessage(`41`), Method: "mcpServer/elicitation/request",
		Params: json.RawMessage(`{"serverName":"srv","threadId":"` + testThreadID + `"}`),
	})
	if h.replyCount() != 1 || !strings.Contains(h.lastReply(), `"action":"decline"`) {
		t.Fatalf("late elicitation must still be declined: %v", h.replies)
	}
}

func TestResponder_InFlightRequestBeforeRunRegistrationIsNotLate(t *testing.T) {
	h := newResponderHarness(t)
	h.adapter.mu.Lock()
	h.adapter.singleFlt[testThreadID] = &codexSlot{released: make(chan struct{}), owner: "att-inflight"}
	h.adapter.mu.Unlock()

	h.responder().HandleRequest(ServerRequest{
		ID: json.RawMessage(`51`), Method: "execCommandApproval",
		Params: json.RawMessage(execApprovalParams("call-race")),
	})
	if h.replyCount() != 1 || !strings.Contains(h.lastReply(), councilApprovalRejection) {
		t.Fatalf("an in-flight (pre-acceptance) request is denied as live, not late: %v", h.replies)
	}
}

// ── Unparseable and unknown requests: protocol drift ────────────────────

func TestResponder_UnparseableAndUnknownRequestsFailClosed(t *testing.T) {
	cases := []struct {
		name     string
		method   string
		params   string
		threaded bool // params carry a correlatable thread id
	}{
		{"missing callId", "execCommandApproval", `{"conversationId":"` + testThreadID + `"}`, true},
		{"wrong shape callId", "execCommandApproval", `{"callId":123,"conversationId":"` + testThreadID + `"}`, true},
		{"missing turnId", "item/fileChange/requestApproval", `{"itemId":"i","threadId":"` + testThreadID + `"}`, true},
		{"params not an object", "applyPatchApproval", `[1,2,3]`, false},
		{"empty params", "item/tool/requestUserInput", ``, false},
		{"unknown approval-shaped method", "item/deepResearch/start", `{"threadId":"` + testThreadID + `"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newResponderHarness(t)
			run := h.addLiveRun(t, testThreadID, "att-drift", nil)
			before, err := h.store.GetCodexTurnAttempt(context.Background(), "att-drift")
			if err != nil || before == nil {
				t.Fatalf("attempt lookup: %v", err)
			}

			h.responder().HandleRequest(ServerRequest{
				ID: json.RawMessage(`61`), Method: tc.method, Params: json.RawMessage(tc.params),
			})

			if h.replyCount() != 0 {
				t.Fatalf("an unanswerable request is never replied to, got %v", h.replies)
			}
			after, err := h.store.GetCodexTurnAttempt(context.Background(), "att-drift")
			if err != nil || after == nil {
				t.Fatalf("attempt lookup: %v", err)
			}
			if tc.threaded && (after.ObservedStatus != "uncertain" || after.TransitionVersion <= before.TransitionVersion) {
				t.Fatalf("protocol drift must classify the correlatable attempt Uncertain: %+v", after)
			}
			if !tc.threaded && after.TransitionVersion != before.TransitionVersion {
				t.Fatalf("drift without a correlatable thread must not attribute an attempt: %+v", after)
			}
			sawDiagnostic := false
			for {
				select {
				case ev, ok := <-run.stream.Events():
					if !ok {
						continue
					}
					if ev.Type == adapter.EventToolRequested || ev.Type == adapter.EventToolDenied {
						t.Fatalf("drift must not mirror %s", ev.Type)
					}
					if ev.Type == adapter.EventProgress && strings.Contains(ev.Payload, "protocol drift") {
						sawDiagnostic = true
					}
					continue
				default:
				}
				if tc.threaded && !sawDiagnostic {
					t.Fatal("drift on a live run must record a diagnostic")
				}
				break
			}
		})
	}
}

// ── Forbidden decisions are structurally unencodable ────────────────────

func TestResponder_ForbiddenTokensAbsentFromEveryPayload(t *testing.T) {
	forbidden := []string{
		"approved", "approved_execpolicy_amendment", "approved_for_session",
		"approved_mcp_policy_amendment", "network_policy_amendment",
		"applyNetworkPolicyAmendment", "accept", "acceptForSession",
		"acceptWithExecpolicyAmendment", "abort", "cancel", "strictAutoReview",
		"timed_out",
	}
	texts := []string{councilApprovalRejection, councilLateApprovalRejection}
	if len(approvalVariants) != 7 {
		t.Fatalf("the deny table must cover exactly the 7 verified variants, got %d", len(approvalVariants))
	}
	for method, variant := range approvalVariants {
		for _, text := range texts {
			raw, err := json.Marshal(variant.payload(text))
			if err != nil {
				t.Fatalf("marshal %s payload: %v", method, err)
			}
			payload := string(raw)
			for _, token := range forbidden {
				if strings.Contains(payload, token) {
					t.Fatalf("payload for %s contains forbidden token %q: %s", method, token, payload)
				}
			}
		}
	}
	// The gate is a property of the table, not a runtime flag.
	for _, m := range []string{"item/permissions/requestApproval", "item/tool/requestUserInput"} {
		if !approvalVariants[m].denyEquivalent {
			t.Fatalf("%s must be gated as a deny-equivalent", m)
		}
	}
	for _, m := range []string{"execCommandApproval", "applyPatchApproval",
		"item/commandExecution/requestApproval", "item/fileChange/requestApproval",
		"mcpServer/elicitation/request"} {
		if approvalVariants[m].denyEquivalent {
			t.Fatalf("%s has a schema-native refusal and must not be gated", m)
		}
	}
}

// TestResponder_DecodeApprovalDenies covers the cprot-v2 record-frame
// decode used by the eligibility binding, including fail-closed parsing.
func TestResponder_DecodeApprovalDenies(t *testing.T) {
	denies := []ApprovalDenyRecord{
		{MethodName: "item/permissions/requestApproval", RefusalKind: RefusalLiveVerifiedEquivalent},
		{MethodName: "item/tool/requestUserInput", RefusalKind: RefusalNativeEnum},
	}
	raw, err := encodedDenyEquivalents(denies...)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeApprovalDenies(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 || got[0] != denies[0] || got[1] != denies[1] {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	for _, broken := range [][]byte{nil, []byte("[]"), raw[:len(raw)-1], append(append([]byte{}, raw...), 0x00)} {
		if _, err := DecodeApprovalDenies(broken); err == nil {
			t.Fatalf("broken frame % x must fail closed", broken)
		}
	}
}

// ── Full-stack: responder installed before the first request ────────────

// The approval request rides the account/read auth-gate request — after
// the handshake, before any turn route exists and before any
// notification-emitting request. It is answered (late-deny: no live
// turn), which proves the responder registration happened at child
// start/pump arm, before the framing reader could surface any request.
func TestCodexAdapter_ApprovalResponderInstalledBeforeFirstRequest(t *testing.T) {
	h := newAdapterHarness(t)
	initRule := `{"emit_many_on_request": {"method":"account/read","lines":[` +
		approvalRequestLine(901, "execCommandApproval", execApprovalParams("call-init")) + `]}}`
	scenario := append([]string{initRule, authOKLine()},
		threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	writeScenario(t, h.scratch, scenario...)

	if binding := h.create(t, testSessionID); binding.SessionID != adapter.SessionID(testSessionID) {
		t.Fatalf("unexpected binding: %+v", binding)
	}

	replies := waitForReply(t, h.scratch, "901", 5*time.Second)
	want := `{"jsonrpc":"2.0","id":901,"result":{"decision":{"denied":{"rejection":"` + councilLateApprovalRejection + `"}}}}`
	found := false
	for _, line := range replies {
		if line == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("initialize-time approval must be answered with the exact late-deny frame\nwant %s\ngot  %v", want, replies)
	}

	// The handler is registered: the default queue is out of service.
	child, err := h.server.child(testSessionID)
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	if _, err := child.conn.NextServerRequest(50 * time.Millisecond); err == nil ||
		!strings.Contains(err.Error(), "handler is registered") {
		t.Fatalf("responder must own the request seam after install, got: %v", err)
	}
}

// ── Full-stack: denial mirrors and the turn continues ───────────────────

// An execCommandApproval arriving while the turn is live is denied with
// the exact §3.6 payload, mirrored as tool_requested/tool_denied, and
// the turn is NOT retired by the denial: it continues until a real
// verified terminal (here: the accepted interrupt).
func TestCodexAdapter_ExecApprovalDenialMirrorsAndTurnContinues(t *testing.T) {
	h := newAdapterHarness(t)
	rules := []string{
		`{"emit_many_on_request": {"method":"turn/start","lines":[` +
			turnStartedLine(testThreadID) + `,` +
			approvalRequestLine(911, "execCommandApproval", execApprovalParams("call-1")) + `]}}`,
		turnStartRespondRule(testThreadID),
	}
	rules = append(rules, turnInterruptRules(testThreadID)...)
	writeScenario(t, h.scratch, append(baseScenario(h), rules...)...)

	h.createAndPersist(t)
	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-approval-mirror"}
	out, err := h.dispatch(t, ref.TurnKey, "prompt")
	if err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}

	stream, err := h.adapter.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	replies := waitForReply(t, h.scratch, "911", 5*time.Second)
	want := `{"jsonrpc":"2.0","id":911,"result":{"decision":{"denied":{"rejection":"` + councilApprovalRejection + `"}}}}`
	found := false
	for _, line := range replies {
		if line == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("exact wire payload mismatch\nwant %s\ngot  %v", want, replies)
	}

	ev1 := nextEvent(t, stream)
	ev2 := nextEvent(t, stream)
	if ev1.Type != adapter.EventToolRequested || ev2.Type != adapter.EventToolDenied {
		t.Fatalf("mirror order must be tool_requested then tool_denied, got %s/%s", ev1.Type, ev2.Type)
	}
	if ev1.ApprovalID != "call-1" || ev2.ApprovalID != "call-1" {
		t.Fatalf("ApprovalID must be the callId, got %q/%q", ev1.ApprovalID, ev2.ApprovalID)
	}
	if ev1.Status != council.TurnRunning || ev2.Status != council.TurnRunning {
		t.Fatalf("denial mirrors must not retire the turn (TurnRunning), got %s/%s", ev1.Status, ev2.Status)
	}

	// The denial did not retire the turn: a later verified terminal
	// still commits (the accepted interrupt).
	outcome, err := h.adapter.Cancel(context.Background(), ref)
	if err != nil || outcome.Disposition != adapter.CancelConfirmed {
		t.Fatalf("turn must continue after the denial; cancel: %+v err=%v", outcome, err)
	}
	ev3 := awaitTerminalEvent(t, stream)
	if ev3.Status != council.TurnCancelled {
		t.Fatalf("terminal must follow, got %+v", ev3)
	}
	waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
	h.adapter.mu.Lock()
	live := len(h.adapter.turns)
	slots := len(h.adapter.singleFlt)
	h.adapter.mu.Unlock()
	if live != 0 || slots != 0 {
		t.Fatalf("finishTurn must release the slot and retire the run (live=%d slots=%d)", live, slots)
	}
}

// ── Full-stack: unverified deny-equivalents fail closed ─────────────────

// unverifiedReceiptScenario dispatches a turn whose native side sends a
// deny-equivalent approval request during the dispatch ack window (the
// ack is delayed past the child's death, so DispatchUnknown is
// deterministic). Receiving the request must terminate the child: no
// reply, no mirror, attempt Uncertain.
func unverifiedReceiptScenario(t *testing.T, method, params string, requestID int) (*adapterHarness, adapter.TurnRef) {
	t.Helper()
	h := newAdapterHarness(t)
	rules := []string{
		`{"emit_many_on_request": {"method":"turn/start","lines":[` +
			turnStartedLine(testThreadID) + `,` +
			approvalRequestLine(requestID, method, params) + `]}}`,
		`{"respond": {"method":"turn/start","result":{"id":"` + testTurnID + `","threadId":"` + testThreadID + `","status":{"type":"inProgress"}},"delay_ms":2000}}`,
	}
	writeScenario(t, h.scratch, append(baseScenario(h), rules...)...)
	h.createAndPersist(t)
	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-unverified-" + strconv.Itoa(requestID)}
	out, err := h.dispatch(t, ref.TurnKey, "prompt")
	if err != nil {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	if out.Status != adapter.DispatchUnknown {
		t.Fatalf("the fail-closed termination races the dispatch ack: expected DispatchUnknown, got %+v", out)
	}
	return h, ref
}

func assertUnverifiedReceipt(t *testing.T, h *adapterHarness, ref adapter.TurnRef, requestID int) {
	t.Helper()
	// No reply was ever sent for the request id.
	time.Sleep(500 * time.Millisecond)
	for _, line := range readReplies(t, h.scratch) {
		if strings.Contains(line, `"id":`+strconv.Itoa(requestID)+",") {
			t.Fatalf("no payload may be sent for an unverified deny-equivalent: %s", line)
		}
	}
	// The child was terminated (house terminate split).
	if terminatedCount(t, h.scratch) == 0 {
		t.Fatal("receiving an unverified deny-equivalent must terminate the child")
	}
	// The attempt stays Uncertain, never terminal, with the durable
	// uncertain classification recorded by the fail-closed receipt.
	waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool {
		return !a.Terminal && a.ObservedStatus == "uncertain"
	})
}

func TestCodexAdapter_UnverifiedPermissionsReceiptFailsClosed(t *testing.T) {
	h, ref := unverifiedReceiptScenario(t, "item/permissions/requestApproval",
		permissionsApprovalParams("item-perm-1"), 912)
	assertUnverifiedReceipt(t, h, ref, 912)
}

func TestCodexAdapter_UnverifiedUserInputReceiptFailsClosed(t *testing.T) {
	h, ref := unverifiedReceiptScenario(t, "item/tool/requestUserInput",
		userInputApprovalParams("item-input-1"), 914)
	assertUnverifiedReceipt(t, h, ref, 914)
}

// ── Full-stack: verified deny-equivalents are answered and mirrored ─────

func verifiedDenyEquivalentScenario(t *testing.T, method, params string, requestID int, records ...ApprovalDenyRecord) (*adapterHarness, adapter.TurnRef) {
	t.Helper()
	rules := []string{
		`{"emit_many_on_request": {"method":"turn/start","lines":[` +
			turnStartedLine(testThreadID) + `,` +
			approvalRequestLine(requestID, method, params) + `]}}`,
		turnStartRespondRule(testThreadID),
	}
	rules = append(rules, turnInterruptRules(testThreadID)...)
	h := newAdapterHarness(t)
	writeScenario(t, h.scratch, append(baseScenario(h), rules...)...)
	h.createAndPersist(t)
	path := seedRollout(t, h, testThreadID)
	if err := h.store.MarkCodexSessionMaterialized(context.Background(), testSessionID, path); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	insertHarnessAttestationWithRecords(t, h, testAttestationID(), records...)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-verified-" + strconv.Itoa(requestID)}
	out, err := h.dispatch(t, ref.TurnKey, "prompt")
	if err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	att := waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.NativeTurnID != nil })
	if att.RolloutProtection != "protected" || att.AttestationID == nil || *att.AttestationID != testAttestationID() {
		t.Fatalf("the governing attestation must be frozen on the attempt: protection=%q attestation=%v",
			att.RolloutProtection, att.AttestationID)
	}
	return h, ref
}

func assertVerifiedDenyEquivalent(t *testing.T, h *adapterHarness, ref adapter.TurnRef, requestID int, wantResult, approvalID string) {
	t.Helper()
	stream, err := h.adapter.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	replies := waitForReply(t, h.scratch, strconv.Itoa(requestID), 5*time.Second)
	want := `{"jsonrpc":"2.0","id":` + strconv.Itoa(requestID) + `,"result":` + wantResult + `}`
	found := false
	for _, line := range replies {
		if line == want {
			found = true
		}
		if strings.Contains(line, "strictAutoReview") {
			t.Fatalf("strictAutoReview must never be set: %s", line)
		}
	}
	if !found {
		t.Fatalf("exact wire payload mismatch\nwant %s\ngot  %v", want, replies)
	}

	ev1 := nextEvent(t, stream)
	ev2 := nextEvent(t, stream)
	if ev1.Type != adapter.EventToolRequested || ev2.Type != adapter.EventToolDenied {
		t.Fatalf("mirror order must be tool_requested then tool_denied, got %s/%s", ev1.Type, ev2.Type)
	}
	if ev1.ApprovalID != approvalID || ev2.ApprovalID != approvalID {
		t.Fatalf("ApprovalID mismatch: %q/%q want %q", ev1.ApprovalID, ev2.ApprovalID, approvalID)
	}

	outcome, err := h.adapter.Cancel(context.Background(), ref)
	if err != nil || outcome.Disposition != adapter.CancelConfirmed {
		t.Fatalf("the denial must not retire the turn; cancel: %+v err=%v", outcome, err)
	}
	ev3 := awaitTerminalEvent(t, stream)
	if ev3.Status != council.TurnCancelled {
		t.Fatalf("terminal must follow, got %+v", ev3)
	}
	waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
}

func TestCodexAdapter_VerifiedPermissionsEmptyGrantAnswered(t *testing.T) {
	h, ref := verifiedDenyEquivalentScenario(t, "item/permissions/requestApproval",
		permissionsApprovalParams("item-perm-ok"), 913,
		ApprovalDenyRecord{MethodName: "item/permissions/requestApproval", RefusalKind: RefusalLiveVerifiedEquivalent},
		ApprovalDenyRecord{MethodName: "item/tool/requestUserInput", RefusalKind: RefusalLiveVerifiedEquivalent})
	assertVerifiedDenyEquivalent(t, h, ref, 913, `{"permissions":{},"scope":"turn"}`, "item-perm-ok")
}

func TestCodexAdapter_VerifiedUserInputEmptyAnswersAnswered(t *testing.T) {
	h, ref := verifiedDenyEquivalentScenario(t, "item/tool/requestUserInput",
		userInputApprovalParams("item-input-ok"), 915,
		ApprovalDenyRecord{MethodName: "item/permissions/requestApproval", RefusalKind: RefusalLiveVerifiedEquivalent},
		ApprovalDenyRecord{MethodName: "item/tool/requestUserInput", RefusalKind: RefusalLiveVerifiedEquivalent})
	assertVerifiedDenyEquivalent(t, h, ref, 915, `{"answers":{}}`, "item-input-ok")
}
