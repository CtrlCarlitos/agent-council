package codex

// Approval responder for the codex app-server child (AC-009 spec §3.6):
// every server→client approval request is answered DENY with the exact
// per-variant payload from the installed binary's own protocol schema
// (0.154.0 evidence) — never an approved*/accept*/amendment decision,
// never abort/cancel (a denial must not interrupt the turn), never
// strictAutoReview. The two deny-EQUIVALENT variants whose refusal
// semantics are not schema-defined (permissions empty grant,
// requestUserInput empty answers) may be sent ONLY after a live-verified
// approval_deny record exists for that method in the launch's governing
// attestation (the attempt's frozen §3.7 protection attestation); until
// then RECEIVING such a request terminates the child and classifies the
// attempt Uncertain — no payload, no tool_denied, no fabricated denial.
// Answered denials mirror into the turn's event stream as
// tool_requested/tool_denied exactly once per ApprovalID; duplicates are
// re-denied idempotently; requests arriving after a verified terminal are
// denied without touching state; an unparseable request is protocol drift
// (§3.9): child terminated, attempt Uncertain — Council never guesses a
// decision shape.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// Council denial texts. Carried in the schema-defined rejection fields
// and mirrored payloads; reviewed against the forbidden-token set (the
// denial must never widen policy or name an approval variant).
const (
	councilApprovalRejection     = "denied by council: contributor approvals are never granted; the turn continues"
	councilLateApprovalRejection = "denied by council after the turn reached a verified terminal state; recorded as a late denial"
)

// maxRunDiagnostics bounds the in-memory diagnostic log an attempt run
// keeps for responder events (late denials, fail-closed receipts).
const maxRunDiagnostics = 16

// approvalReplyResolutionGrace bounds the reply closure's wait for the
// child to become resolvable (a request racing the handshake reply).
var approvalReplyResolutionGrace = 2 * time.Second

// Responder routes one server→client request (spec §3.6). It is
// stateless: mirror dedup lives on the turn run, replies go through the
// child's connection (injected so tests can capture the exact payload
// bytes).
type Responder struct {
	a         *CodexAdapter
	sessionID adapter.SessionID
	reply     func(id json.RawMessage, result any) error
}

// newResponder binds the adapter, the owning session, and the reply
// transport. The production reply closure resolves the live child per
// call (the child may die between receipt and reply).
func newResponder(a *CodexAdapter, sessionID adapter.SessionID, reply func(id json.RawMessage, result any) error) *Responder {
	return &Responder{a: a, sessionID: sessionID, reply: reply}
}

// ── Per-variant deny table (spec §3.6) ──────────────────────────────────

// The deny payloads are fixed-shape structs: there is no representable
// approved/accept/amendment/abort/cancel/strictAutoReview variant, so a
// forbidden decision cannot be encoded by this file.

type deniedRejectionBody struct {
	Rejection string `json:"rejection"`
}

type deniedDecisionPayload struct {
	Decision struct {
		Denied deniedRejectionBody `json:"denied"`
	} `json:"decision"`
}

type declineDecisionPayload struct {
	Decision string `json:"decision"`
}

type emptyGrantPayload struct {
	Permissions struct{} `json:"permissions"`
	Scope       string   `json:"scope"`
}

type emptyAnswersPayload struct {
	Answers struct{} `json:"answers"`
}

type declineActionPayload struct {
	Action string `json:"action"`
}

// approvalVariant is one row of the §3.6 deny table.
type approvalVariant struct {
	method string
	// denyEquivalent marks the two variants whose refusal semantics are
	// NOT schema-defined: the empty grant / empty answers payload is a
	// live-verified deny-EQUIVALENT, gated on the governing attestation.
	denyEquivalent bool
	parse          func(params json.RawMessage) (approvalCorrelation, error)
	payload        func(rejection string) any
}

// approvalCorrelation carries the per-variant correlation fields exactly
// as the 0.154.0 param schemas define them.
type approvalCorrelation struct {
	callID     string
	approvalID string // optional when the variant allows it
	itemID     string
	threadID   string // threadId or conversationId
	turnID     string
	serverName string
}

// approvalKey returns the ApprovalID correlation identity used for
// mirroring (callId, or approvalId when present for the exec/patch
// variants; itemId for item/*; serverName for elicitation).
func (c approvalCorrelation) approvalKey() string {
	if c.approvalID != "" {
		return c.approvalID
	}
	if c.callID != "" {
		return c.callID
	}
	if c.itemID != "" {
		return c.itemID
	}
	return c.serverName
}

func parseExecApprovalParams(params json.RawMessage) (approvalCorrelation, error) {
	var p struct {
		CallID         string  `json:"callId"`
		ConversationID string  `json:"conversationId"`
		ApprovalID     *string `json:"approvalId"`
	}
	if err := strictUnmarshal(params, &p); err != nil {
		return approvalCorrelation{}, err
	}
	if p.CallID == "" || p.ConversationID == "" {
		return approvalCorrelation{}, errors.New("callId and conversationId are required")
	}
	c := approvalCorrelation{callID: p.CallID, threadID: p.ConversationID}
	if p.ApprovalID != nil {
		c.approvalID = *p.ApprovalID
	}
	return c, nil
}

func parseItemApprovalParams(params json.RawMessage) (approvalCorrelation, error) {
	var p struct {
		ItemID     string  `json:"itemId"`
		ThreadID   string  `json:"threadId"`
		TurnID     string  `json:"turnId"`
		ApprovalID *string `json:"approvalId"`
	}
	if err := strictUnmarshal(params, &p); err != nil {
		return approvalCorrelation{}, err
	}
	if p.ItemID == "" || p.ThreadID == "" || p.TurnID == "" {
		return approvalCorrelation{}, errors.New("itemId, threadId, and turnId are required")
	}
	c := approvalCorrelation{itemID: p.ItemID, threadID: p.ThreadID, turnID: p.TurnID}
	if p.ApprovalID != nil {
		c.approvalID = *p.ApprovalID
	}
	return c, nil
}

func parseElicitationParams(params json.RawMessage) (approvalCorrelation, error) {
	var p struct {
		ServerName string  `json:"serverName"`
		ThreadID   string  `json:"threadId"`
		TurnID     *string `json:"turnId"`
	}
	if err := strictUnmarshal(params, &p); err != nil {
		return approvalCorrelation{}, err
	}
	if p.ServerName == "" || p.ThreadID == "" {
		return approvalCorrelation{}, errors.New("serverName and threadId are required")
	}
	c := approvalCorrelation{serverName: p.ServerName, threadID: p.ThreadID}
	if p.TurnID != nil {
		c.turnID = *p.TurnID
	}
	return c, nil
}

// strictUnmarshal decodes params into dst, failing on JSON type
// mismatches (wrong shapes are drift). Members outside the correlation
// struct are schema-legal request detail (reason, startedAtMs, …) and are
// ignored, never treated as drift.
func strictUnmarshal(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return errors.New("params are required")
	}
	return json.Unmarshal(raw, dst)
}

// approvalVariants is the §3.6 deny table. Method names are verbatim
// JSON-RPC method spellings from the installed binary's schema.
var approvalVariants = map[string]*approvalVariant{
	"execCommandApproval": {
		method: "execCommandApproval",
		parse:  parseExecApprovalParams,
		payload: func(rejection string) any {
			var p deniedDecisionPayload
			p.Decision.Denied.Rejection = rejection
			return p
		},
	},
	"applyPatchApproval": {
		method: "applyPatchApproval",
		parse:  parseExecApprovalParams,
		payload: func(rejection string) any {
			var p deniedDecisionPayload
			p.Decision.Denied.Rejection = rejection
			return p
		},
	},
	"item/commandExecution/requestApproval": {
		method: "item/commandExecution/requestApproval",
		parse:  parseItemApprovalParams,
		payload: func(string) any {
			return declineDecisionPayload{Decision: "decline"}
		},
	},
	"item/fileChange/requestApproval": {
		method: "item/fileChange/requestApproval",
		parse:  parseItemApprovalParams,
		payload: func(string) any {
			return declineDecisionPayload{Decision: "decline"}
		},
	},
	"item/permissions/requestApproval": {
		method:         "item/permissions/requestApproval",
		denyEquivalent: true,
		parse:          parseItemApprovalParams,
		payload: func(string) any {
			// Deny-EQUIVALENT: the EMPTY grant — no filesystem or
			// network permission; scope turn; strictAutoReview never set
			// (the struct has no such field).
			var p emptyGrantPayload
			p.Scope = "turn"
			return p
		},
	},
	"item/tool/requestUserInput": {
		method:         "item/tool/requestUserInput",
		denyEquivalent: true,
		parse:          parseItemApprovalParams,
		payload: func(string) any {
			// Deny-EQUIVALENT: the empty answer map (EXPERIMENTAL).
			return emptyAnswersPayload{}
		},
	},
	"mcpServer/elicitation/request": {
		method: "mcpServer/elicitation/request",
		parse:  parseElicitationParams,
		payload: func(string) any {
			// Content omitted — nullable for decline.
			return declineActionPayload{Action: "decline"}
		},
	},
}

// ── cprot-v2 record decoding (eligibility binding) ──────────────────────

// DecodeApprovalDenies decodes the approval_deny records from a cprot-v2
// encoded record frame (ProtectionAttestation.EncodeProbeRecords output).
// Truncation, an unknown class, or trailing bytes is an error (the
// caller fails closed). The full decoder lives in coverage.go.
func DecodeApprovalDenies(raw []byte) ([]ApprovalDenyRecord, error) {
	_, denies, err := DecodeProbeRecords(raw)
	if err != nil {
		return nil, err
	}
	return denies, nil
}

// PinnedApprovalMethods is the pinned §3.6 approval surface (the
// 0.154.0 schema captures): method name → whether the variant is a
// deny-EQUIVALENT (no schema-native refusal enum). It is the approval
// half of the attestation coverage universe (coverage.go). The returned
// map is a fresh copy.
func PinnedApprovalMethods() map[string]bool {
	return pinnedApprovalMethods()
}

func pinnedApprovalMethods() map[string]bool {
	out := make(map[string]bool, len(approvalVariants))
	for name, v := range approvalVariants {
		out[name] = v.denyEquivalent
	}
	return out
}

// ── Routing ─────────────────────────────────────────────────────────────

// HandleRequest routes one server→client request through the §3.6
// deny table. Safe for concurrent use (one goroutine per request from
// the connection's routing).
func (r *Responder) HandleRequest(sr ServerRequest) {
	variant, ok := approvalVariants[sr.Method]
	if !ok {
		// A server→client request outside the verified 0.154.0 approval
		// surface cannot be answered honestly and must not hang the
		// native turn silently: protocol drift (§3.9).
		r.failDrift(sr, lenientThreadID(sr.Params), "unclassifiable server→client request: outside the verified approval surface")
		return
	}
	corr, err := variant.parse(sr.Params)
	if err != nil {
		// Unparseable params (missing correlation fields, wrong shape):
		// Council never guesses a decision shape (§3.6/§3.9). The thread
		// correlation is still extracted leniently so a live attempt on
		// that thread can be classified Uncertain.
		r.failDrift(sr, lenientThreadID(sr.Params), "approval request params failed the verified schema shape: "+err.Error())
		return
	}

	run := r.a.runForNativeThread(corr.threadID)

	// Unverified deny-equivalents fail closed (§3.6): no fabricated
	// denials. Receipt terminates the child and classifies the attempt
	// Uncertain — no payload, no tool_denied, no mirror.
	if variant.denyEquivalent && !r.denyEquivalentVerified(variant.method, run) {
		r.failUnverifiedDenyEquivalent(variant, run)
		return
	}

	late := run == nil && !r.inFlightWithoutRun(corr.threadID)
	if run != nil {
		if _, terminal := run.terminalStatus(); terminal {
			late = true
		}
	}
	rejection := councilApprovalRejection
	if late {
		rejection = councilLateApprovalRejection
	}

	payload, err := json.Marshal(variant.payload(rejection))
	if err != nil {
		// The payload constructors are fixed-shape structs; marshaling
		// cannot fail. If it ever does, refuse to answer rather than
		// guess.
		r.failDrift(sr, corr.threadID, "deny payload encoding failed: "+err.Error())
		return
	}

	var replyErr error
	if r.reply != nil {
		replyErr = r.reply(sr.ID, json.RawMessage(payload))
	}
	if run != nil {
		if replyErr != nil {
			r.recordRunDiagnostic(run, fmt.Sprintf("deny reply for %s approval %q failed: %v", variant.method, corr.approvalKey(), replyErr))
		}
		if late {
			// Late denial: no state change, no resurrection, no mirror.
			r.recordRunDiagnostic(run, "late approval denial for "+variant.method+" (approval "+corr.approvalKey()+") after a verified terminal; turn state untouched")
			return
		}
		r.mirrorDenial(run, corr.approvalKey(), variant.method, rejection, replyErr)
	}
}

// mirrorDenial records the answered denial into the turn's event stream
// as tool_requested then tool_denied with the ApprovalID set, exactly
// once per ApprovalID (duplicate re-denies do not re-mirror).
func (r *Responder) mirrorDenial(run *codexTurnRun, approvalID, method, rejection string, replyErr error) {
	if !run.markApprovalMirrored(approvalID) {
		return
	}
	deniedText := rejection
	if replyErr != nil {
		deniedText = fmt.Sprintf("%s (deny reply failed: %v)", rejection, replyErr)
	}
	_ = run.stream.SendOrOverflow(adapter.Event{
		Ref: run.ref, Type: adapter.EventToolRequested,
		Status:     council.TurnRunning,
		ApprovalID: approvalID,
		Payload:    method,
	})
	_ = run.stream.SendOrOverflow(adapter.Event{
		Ref: run.ref, Type: adapter.EventToolDenied,
		Status:     council.TurnRunning,
		ApprovalID: approvalID,
		Payload:    truncateEventPayload(deniedText),
	})
}

// denyEquivalentVerified reports whether the launch's governing
// attestation carries a live-verified approval_deny record for the
// variant method (spec §3.6/§3.7). Every failure mode — no live run, no
// frozen attestation, unreadable row, undecodable frame, wrong refusal
// kind — is unverified: the caller fails closed.
func (r *Responder) denyEquivalentVerified(method string, run *codexTurnRun) bool {
	if run == nil {
		return false
	}
	if _, terminal := run.terminalStatus(); terminal {
		return false
	}
	attempt, err := r.a.store.GetCodexTurnAttempt(context.Background(), run.attemptID)
	if err != nil || attempt == nil || attempt.AttestationID == nil {
		return false
	}
	raw, err := r.a.store.CodexProtectionProbeResults(context.Background(), *attempt.AttestationID)
	if err != nil || len(raw) == 0 {
		return false
	}
	denies, err := DecodeApprovalDenies(raw)
	if err != nil {
		return false
	}
	for _, d := range denies {
		if d.MethodName == method && d.RefusalKind == RefusalLiveVerifiedEquivalent {
			return true
		}
	}
	return false
}

// failUnverifiedDenyEquivalent applies the fail-closed receipt rule: the
// child is terminated (house terminate split), the attempt is classified
// Uncertain with a diagnostic, and NOTHING is sent or mirrored.
func (r *Responder) failUnverifiedDenyEquivalent(variant *approvalVariant, run *codexTurnRun) {
	reason := "receipt of an approval request for unverified deny-equivalent variant " + variant.method +
		" (no live-verified approval_deny record in the governing attestation); child terminated, attempt uncertain, no denial fabricated (spec §3.6)"
	r.classifyUncertain(run, reason)
	r.a.server.stop(context.Background(), r.sessionID)
}

// failDrift applies the §3.9 protocol-drift rule for an unanswerable
// approval request: no reply is ever guessed, the attempt is classified
// Uncertain, and the child is terminated.
func (r *Responder) failDrift(sr ServerRequest, threadID, reason string) {
	run := r.a.runForNativeThread(threadID)
	r.classifyUncertain(run, "protocol drift on "+sr.Method+": "+reason+"; no decision guessed, child terminated")
	r.a.server.poisonKey(r.sessionID, &ErrProtocolDrift{Method: sr.Method, Reason: reason})
}

// lenientThreadID extracts the thread correlation (threadId or
// conversationId) from params without shape enforcement, for Uncertain
// classification of unparseable requests.
func lenientThreadID(params json.RawMessage) string {
	var p struct {
		ThreadID       string `json:"threadId"`
		ConversationID string `json:"conversationId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ""
	}
	if p.ThreadID != "" {
		return p.ThreadID
	}
	return p.ConversationID
}

// classifyUncertain records the house-pattern Uncertain classification:
// an in-memory diagnostic on the run and, when the attempt has not
// already reached a verified terminal (which is never downgraded), the
// durable observed-status write. A missing run records nothing.
func (r *Responder) classifyUncertain(run *codexTurnRun, reason string) {
	if run == nil {
		return
	}
	r.recordRunDiagnostic(run, reason)
	if _, terminal := run.terminalStatus(); terminal {
		return
	}
	_ = r.a.store.SetCodexAttemptObservedStatus(context.Background(), run.attemptID, "uncertain")
	if run.stream != nil {
		_ = run.stream.SendOrOverflow(adapter.Event{
			Ref: run.ref, Type: adapter.EventProgress,
			Status:  council.TurnRunning,
			Payload: truncateEventPayload(reason),
		})
	}
}

// recordRunDiagnostic appends a bounded in-memory diagnostic to the run.
func (r *Responder) recordRunDiagnostic(run *codexTurnRun, note string) {
	run.diagMu.Lock()
	defer run.diagMu.Unlock()
	if len(run.diagnostics) >= maxRunDiagnostics {
		copy(run.diagnostics, run.diagnostics[1:])
		run.diagnostics[len(run.diagnostics)-1] = note
		return
	}
	run.diagnostics = append(run.diagnostics, note)
}

// inFlightWithoutRun reports whether a turn is in flight on the native
// thread in the pre-registration window (the dispatch slot is held but
// the run's acceptance is not yet verified). Such a request is NOT late:
// it is denied with the live rejection text and cannot be mirrored (no
// observation stream exists yet).
func (r *Responder) inFlightWithoutRun(threadID string) bool {
	return r.a.slotHeld(threadID)
}
