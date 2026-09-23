package codex

// Task 4 portable transport evidence: framing round-trip, poison rules,
// id correlation and duplicate-id rejection, server→client request seam,
// and client method shapes decoded against the committed recorded
// fixtures in testdata/. Process-bound lifecycle evidence lives in
// server_test.go under the unix build tag.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── Scripted JSON-RPC peer ──────────────────────────────────────────────

// scriptedPeer is the child side of a Conn under test: it reads request
// frames from the conn and lets tests replay recorded responses,
// notifications, server→client requests, or raw poisoned bytes.
type scriptedPeer struct {
	in  *bufio.Reader  // persistent reader over the conn's request frames
	src io.ReadCloser  // the pipe the reader drains (closed on cleanup)
	out io.WriteCloser // peer's frames the conn will read

	t *testing.T
}

func newScriptedPeer(t *testing.T) (*scriptedPeer, *Conn) {
	t.Helper()
	peerIn, connStdin := io.Pipe()   // conn's request frames → peer reads
	connStdout, peerOut := io.Pipe() // peer's frames → conn reads
	conn := NewConn(connStdin, connStdout)
	conn.Start()
	t.Cleanup(func() {
		conn.Close()
		_ = peerIn.Close()
		_ = peerOut.Close()
	})
	return &scriptedPeer{in: bufio.NewReaderSize(peerIn, 64<<10), src: peerIn, out: peerOut, t: t}, conn
}

type incomingRequest struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

// nextLine reads the next raw frame the conn wrote. Only one goroutine may
// read from the peer at a time.
func (p *scriptedPeer) nextLine(timeout time.Duration) string {
	p.t.Helper()
	type res struct {
		line string
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := p.in.ReadString('\n')
		ch <- res{line: line, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			p.t.Fatalf("peer nextLine: %v", r.err)
		}
		return r.line
	case <-time.After(timeout):
		p.t.Fatal("peer timed out waiting for a frame")
		return ""
	}
}

// nextRequest reads the next request frame the conn wrote.
func (p *scriptedPeer) nextRequest(timeout time.Duration) incomingRequest {
	p.t.Helper()
	line := p.nextLine(timeout)
	var head struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &head); err != nil {
		p.t.Fatalf("peer read non-object frame %q: %v", line, err)
	}
	return incomingRequest{ID: head.ID, Method: head.Method, Params: head.Params}
}

// replyFrame reads the next frame and asserts it is a successful response
// with the given id (server→client request replies).
func (p *scriptedPeer) replyFrame(timeout time.Duration, wantID string) string {
	p.t.Helper()
	line := p.nextLine(timeout)
	var head struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &head); err != nil {
		p.t.Fatalf("peer reply frame %q: %v", line, err)
	}
	if string(head.ID) != wantID {
		p.t.Fatalf("reply id: got %s want %s", string(head.ID), wantID)
	}
	if head.Error != nil {
		p.t.Fatalf("reply must be a result, got error %+v", head.Error)
	}
	return string(head.Result)
}

func (p *scriptedPeer) writeLine(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.out, line+"\n"); err != nil {
		p.t.Fatalf("peer write: %v", err)
	}
}

func (p *scriptedPeer) reply(id json.RawMessage, result string) {
	p.t.Helper()
	p.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, string(id), result))
}

func (p *scriptedPeer) replyError(id json.RawMessage, code int, msg string) {
	p.t.Helper()
	p.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%q}}`, string(id), code, msg))
}

func (p *scriptedPeer) notify(method, params string) {
	p.t.Helper()
	p.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s}`, method, params))
}

func (p *scriptedPeer) serverRequest(id, method, params string) {
	p.t.Helper()
	p.writeLine(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s}`, id, method, params))
}

// ── Framing and poison rules ────────────────────────────────────────────

func TestConn_FrameRoundTripAndMonotonicIDs(t *testing.T) {
	peer, conn := newScriptedPeer(t)

	type echoResult struct {
		Echo string `json:"echo"`
	}
	var ids []int64
	var mu sync.Mutex
	go func() {
		for i := 0; i < 2; i++ {
			req := peer.nextRequest(3 * time.Second)
			var id int64
			if err := json.Unmarshal(req.ID, &id); err == nil {
				mu.Lock()
				ids = append(ids, id)
				mu.Unlock()
			}
			peer.reply(req.ID, fmt.Sprintf(`{"echo":"r%d"}`, i))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var got, got2 echoResult
	if err := conn.Call(ctx, "thread/start", map[string]string{"echo": "one"}, &got); err != nil {
		t.Fatalf("call one: %v", err)
	}
	if got.Echo != "r0" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if err := conn.Call(ctx, "thread/start", map[string]string{"echo": "two"}, &got2); err != nil {
		t.Fatalf("call two: %v", err)
	}
	if got2.Echo != "r1" {
		t.Fatalf("round-trip mismatch two: %+v", got2)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 || ids[1] <= ids[0] {
		t.Fatalf("wire ids must be monotonic, got %v", ids)
	}
}

func TestConn_OversizedFramePoisonsConnection(t *testing.T) {
	peer, conn := newScriptedPeer(t)

	go func() {
		req := peer.nextRequest(3 * time.Second)
		// One frame larger than the documented bound.
		huge := strings.Repeat("x", MaxFrameBytes+16)
		_ = req
		_, _ = fmt.Fprintf(peer.out, "{\"jsonrpc\":\"2.0\",\"id\":0,\"result\":{\"blob\":\"%s\"}}\n", huge)
	}()

	var out map[string]any
	err := conn.Call(context.Background(), "initialize", map[string]any{}, &out)
	var poisoned *ErrPoisonedFrame
	if !errors.As(err, &poisoned) {
		t.Fatalf("expected ErrPoisonedFrame, got %T: %v", err, err)
	}
	if !strings.Contains(poisoned.Reason, "oversized") {
		t.Fatalf("poison reason must name the oversized frame, got %q", poisoned.Reason)
	}
	select {
	case <-conn.Done():
	default:
		t.Fatal("poisoned connection must terminate its read loop")
	}
	if conn.Fatal() == nil {
		t.Fatal("Fatal() must retain the poison cause")
	}
	// No recovery: further calls fail, they are never re-armed.
	if err := conn.Call(context.Background(), "initialize", map[string]any{}, &out); err == nil {
		t.Fatal("calls on a poisoned connection must fail")
	}
}

func TestConn_MalformedFramePoisonsConnection(t *testing.T) {
	peer, conn := newScriptedPeer(t)

	go func() {
		req := peer.nextRequest(3 * time.Second)
		_ = req
		peer.writeLine(`{"jsonrpc":"2.0", this is not valid json`)
	}()

	var out map[string]any
	err := conn.Call(context.Background(), "initialize", map[string]any{}, &out)
	var poisoned *ErrPoisonedFrame
	if !errors.As(err, &poisoned) {
		t.Fatalf("expected ErrPoisonedFrame, got %T: %v", err, err)
	}
	if !strings.Contains(poisoned.Reason, "malformed") {
		t.Fatalf("poison reason must name the malformed frame, got %q", poisoned.Reason)
	}
}

func TestConn_UnterminatedFinalFrameIsPoison(t *testing.T) {
	peer, conn := newScriptedPeer(t)
	// A frame with no terminating newline, then stream end: framing
	// corruption, not a clean EOF.
	if _, err := io.WriteString(peer.out, `{"jsonrpc":"2.0","id":1,"method":"thread/started","params":{}`); err != nil {
		t.Fatalf("write torn frame: %v", err)
	}
	_ = peer.out.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case <-conn.Done():
			if conn.Fatal() == nil {
				t.Fatal("torn final frame must be retained as a poison cause")
			}
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("torn final frame must poison the connection, read loop still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConn_DuplicatePendingIDRejected(t *testing.T) {
	_, conn := newScriptedPeer(t)
	id := conn.AllocateID()
	if _, err := conn.registerPending(id); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	_, err := conn.registerPending(id)
	var dup *ErrDuplicateRequestID
	if !errors.As(err, &dup) {
		t.Fatalf("duplicate registration must be rejected, got %T: %v", err, err)
	}
}

func TestConn_LateResponseForUnknownIDDropped(t *testing.T) {
	peer, conn := newScriptedPeer(t)

	slow := make(chan struct{})
	go func() {
		req := peer.nextRequest(3 * time.Second)
		<-slow
		// The caller already timed out and unregistered; this late reply
		// targets a known id shape but an unregistered slot.
		peer.reply(req.ID, `{"ok":true}`)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := conn.Call(ctx, "initialize", map[string]any{}, &map[string]any{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
	close(slow)
	time.Sleep(100 * time.Millisecond)

	// The late response must have been dropped without poisoning; the
	// connection keeps serving new calls.
	go func() {
		req := peer.nextRequest(3 * time.Second)
		peer.reply(req.ID, `{"ok":true}`)
	}()
	var out struct {
		OK bool `json:"ok"`
	}
	if err := conn.Call(context.Background(), "initialize", map[string]any{}, &out); err != nil {
		t.Fatalf("call after late response: %v", err)
	}
	if !out.OK {
		t.Fatal("result decode mismatch")
	}
	if conn.Fatal() != nil {
		t.Fatalf("late response must not poison: %v", conn.Fatal())
	}
}

// ── Server→client request seam ──────────────────────────────────────────

func TestConn_ServerRequestSurfacedAndAnswered(t *testing.T) {
	peer, conn := newScriptedPeer(t)

	// Default surface: bounded queue for unregistered handlers.
	peer.serverRequest(`"approval-1"`, "execCommandApproval", `{"callId":"call-1","conversationId":"thread-1"}`)

	sr, err := conn.NextServerRequest(3 * time.Second)
	if err != nil {
		t.Fatalf("next server request: %v", err)
	}
	if sr.Method != "execCommandApproval" {
		t.Fatalf("method: %q", sr.Method)
	}
	var params struct {
		CallID string `json:"callId"`
	}
	if err := json.Unmarshal(sr.Params, &params); err != nil {
		t.Fatalf("params: %v", err)
	}
	if params.CallID != "call-1" {
		t.Fatalf("callId: %q", params.CallID)
	}

	// The transport answers the JSON-RPC layer with the handler's decision.
	// io.Pipe is synchronous: start reading the reply before responding.
	type replyShape struct {
		Decision struct {
			Denied struct {
				Rejection string `json:"rejection"`
			} `json:"denied"`
		} `json:"decision"`
	}
	replyCh := make(chan string, 1)
	go func() { replyCh <- peer.replyFrame(3*time.Second, `"approval-1"`) }()
	if err := conn.Respond(sr.ID, map[string]any{"decision": map[string]any{"denied": map[string]string{"rejection": "denied by council"}}}); err != nil {
		t.Fatalf("respond: %v", err)
	}
	var reply replyShape
	if err := json.Unmarshal([]byte(<-replyCh), &reply); err != nil {
		t.Fatalf("reply decode: %v", err)
	}
	if reply.Decision.Denied.Rejection == "" {
		t.Fatal("deny decision must be preserved on the wire")
	}

	// Registered handler replaces the queue.
	seen := make(chan ServerRequest, 1)
	conn.SetRequestHandler(func(sr ServerRequest) { seen <- sr })
	peer.serverRequest(`"approval-2"`, "applyPatchApproval", `{"callId":"call-2"}`)
	select {
	case got := <-seen:
		if got.Method != "applyPatchApproval" {
			t.Fatalf("handler method: %q", got.Method)
		}
		errCh := make(chan error, 1)
		go func() {
			errCh <- conn.RespondError(got.ID, &RPCError{Code: -32600, Message: "declined"})
		}()
		line := peer.nextLine(3 * time.Second)
		if err := <-errCh; err != nil {
			t.Fatalf("respond error: %v", err)
		}
		if !strings.Contains(line, `"declined"`) {
			t.Fatalf("error reply must preserve the decision: %s", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("registered handler never saw the server request")
	}
}

// ── Client method shapes against the recorded fixtures ──────────────────

// fixtureDirectives parses a committed testdata scenario file.
func fixtureDirectives(t *testing.T, name string) []struct {
	Respond *struct {
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
	} `json:"respond"`
	RespondError *struct {
		Method  string `json:"method"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"respond_error"`
	Emit json.RawMessage `json:"emit"`
} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name+".jsonl"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	var out []struct {
		Respond *struct {
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
		} `json:"respond"`
		RespondError *struct {
			Method  string `json:"method"`
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"respond_error"`
		Emit json.RawMessage `json:"emit"`
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var d struct {
			Respond *struct {
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
			} `json:"respond"`
			RespondError *struct {
				Method  string `json:"method"`
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"respond_error"`
			Emit json.RawMessage `json:"emit"`
		}
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("fixture %s line %q: %v", name, line, err)
		}
		out = append(out, d)
	}
	return out
}

func findRespond(t *testing.T, name, method string) json.RawMessage {
	t.Helper()
	for _, d := range fixtureDirectives(t, name) {
		if d.Respond != nil && d.Respond.Method == method {
			return d.Respond.Result
		}
	}
	t.Fatalf("fixture %s has no respond rule for %s", name, method)
	return nil
}

func TestClient_InitializeShapeFromFixture(t *testing.T) {
	result := findRespond(t, "handshake", "initialize")
	var init InitializeResult
	if err := json.Unmarshal(result, &init); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if init.CodexHome == "" || init.PlatformOS == "" || init.PlatformFamily == "" {
		t.Fatalf("initialize result must carry the environment attestation fields: %+v", init)
	}
	if !strings.Contains(init.UserAgent, "0.154.0") {
		t.Fatalf("userAgent must carry the pinned version: %q", init.UserAgent)
	}

	// The same recorded shape must round-trip through a live Conn.
	peer, conn := newScriptedPeer(t)
	go func() {
		req := peer.nextRequest(3 * time.Second)
		var params struct {
			ClientInfo struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil || params.ClientInfo.Name == "" {
			t.Errorf("initialize must carry clientInfo: err=%v params=%s", err, string(req.Params))
			return
		}
		peer.reply(req.ID, string(result))
	}()
	client := NewCodexClient(conn, nil)
	got, err := client.Initialize(context.Background(), ClientInfo{Name: "agent-council", Version: "test"})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if got.CodexHome != init.CodexHome || got.PlatformOS != init.PlatformOS {
		t.Fatalf("initialize mismatch: %+v vs %+v", got, init)
	}
}

func TestClient_ThreadStartShapeFromFixture(t *testing.T) {
	result := findRespond(t, "thread-start", "thread/start")
	var thread Thread
	if err := json.Unmarshal(result, &thread); err != nil {
		t.Fatalf("decode thread/start result: %v", err)
	}
	if thread.ID == "" || thread.ID != thread.SessionID {
		t.Fatalf("thread id must equal sessionId: %+v", thread)
	}
	if thread.Status.Type != "idle" {
		t.Fatalf("fresh thread status: %+v", thread.Status)
	}
	if len(thread.Environments) == 0 || thread.Environments[0].CWD == "" {
		t.Fatalf("thread environments must carry cwd: %+v", thread.Environments)
	}
}

func TestClient_ResumeProbeShapeAndMissingMapping(t *testing.T) {
	result := findRespond(t, "resume-positive", "thread/resume")
	var cfg EffectiveConfig
	if err := json.Unmarshal(result, &cfg); err != nil {
		t.Fatalf("decode resume result: %v", err)
	}
	if cfg.ThreadID == "" || cfg.Model == "" || cfg.ModelProvider == "" ||
		cfg.ApprovalsReviewer == "" || cfg.CWD == "" || cfg.ApprovalPolicy == nil || cfg.Sandbox == nil {
		t.Fatalf("effective config incomplete: %+v", cfg)
	}
	if len(cfg.InstructionSources) == 0 {
		t.Fatalf("instructionSources must be present: %+v", cfg)
	}

	// Verbatim -32600 mapping through a live Conn.
	peer, conn := newScriptedPeer(t)
	missing := fixtureDirectives(t, "resume-missing")
	var missingRule *struct {
		Method  string `json:"method"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	for _, d := range missing {
		if d.RespondError != nil && d.RespondError.Method == "thread/resume" {
			missingRule = d.RespondError
		}
	}
	if missingRule == nil {
		t.Fatal("resume-missing fixture lacks the thread/resume error rule")
	}
	go func() {
		req := peer.nextRequest(3 * time.Second)
		var params struct {
			ThreadID     string `json:"threadId"`
			ExcludeTurns bool   `json:"excludeTurns"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			t.Errorf("resume params: %v", err)
			return
		}
		if !params.ExcludeTurns {
			t.Errorf("resume probe must send excludeTurns:true, got %s", string(req.Params))
			return
		}
		peer.replyError(req.ID, missingRule.Code, missingRule.Message)
	}()
	client := NewCodexClient(conn, nil)
	_, err := client.ResumeProbe("00000000-0000-0000-0000-000000000001")
	var missingErr *ErrNativeSessionMissing
	if !errors.As(err, &missingErr) {
		t.Fatalf("expected ErrNativeSessionMissing, got %T: %v", err, err)
	}
	if missingErr.ThreadID != "00000000-0000-0000-0000-000000000001" {
		t.Fatalf("missing error must carry the thread id: %+v", missingErr)
	}
}

func TestClient_TurnFlowFixtureNotifications(t *testing.T) {
	// The recorded turn flow must route through the pump: turn/started,
	// item/*, token usage, and the turn/completed terminal all carry the
	// thread/turn correlation the route tables need.
	var events []NativeNotification
	for _, d := range fixtureDirectives(t, "turn-flow") {
		if d.Emit == nil {
			continue
		}
		var n struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(d.Emit, &n); err != nil {
			t.Fatalf("emit directive: %v", err)
		}
		if n.Method == "" {
			t.Fatalf("emit directive without method: %s", string(d.Emit))
		}
		nn := notificationFromFrames(n.Method, n.Params)
		events = append(events, nn)
	}
	wantOrder := []string{"turn/started", "item/started", "item/completed", "thread/tokenUsage/updated", "turn/completed"}
	if len(events) != len(wantOrder) {
		t.Fatalf("expected %d routed notifications, got %d", len(wantOrder), len(events))
	}
	for i, method := range wantOrder {
		if events[i].Method != method {
			t.Fatalf("event %d: got %q want %q", i, events[i].Method, method)
		}
		if events[i].ThreadID == "" {
			t.Fatalf("%s must extract the thread id", method)
		}
	}
	if events[0].TurnID == "" {
		t.Fatal("turn/started must extract the turn id")
	}

	// Approval requests are recorded as server→client requests with their
	// correlation fields intact; the transport never answers them itself.
	for _, d := range fixtureDirectives(t, "approval-requests") {
		if d.Emit == nil {
			continue
		}
		var sr struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(d.Emit, &sr); err != nil {
			t.Fatalf("approval emit: %v", err)
		}
		if sr.ID == nil || sr.Method == "" {
			t.Fatalf("approval request must carry id+method: %s", string(d.Emit))
		}
		switch sr.Method {
		case "execCommandApproval":
			var p struct {
				CallID         string   `json:"callId"`
				ConversationID string   `json:"conversationId"`
				Command        []string `json:"command"`
				ParsedCmd      []string `json:"parsedCmd"`
				CWD            string   `json:"cwd"`
			}
			if err := json.Unmarshal(sr.Params, &p); err != nil || p.CallID == "" || p.ConversationID == "" || len(p.Command) == 0 || len(p.ParsedCmd) == 0 || p.CWD == "" {
				t.Fatalf("execCommandApproval correlation fields: %v %s", err, string(sr.Params))
			}
		case "applyPatchApproval":
			var p struct {
				CallID         string `json:"callId"`
				ConversationID string `json:"conversationId"`
			}
			if err := json.Unmarshal(sr.Params, &p); err != nil || p.CallID == "" || p.ConversationID == "" {
				t.Fatalf("applyPatchApproval correlation fields: %v %s", err, string(sr.Params))
			}
		}
	}
}

func TestRPCError_MissingThreadMappingVerbatim(t *testing.T) {
	err := missingThreadError("00000000-0000-0000-0000-000000000009", &RPCError{
		Code:    -32600,
		Message: "no rollout found for thread id 00000000-0000-0000-0000-000000000009",
	})
	var missing *ErrNativeSessionMissing
	if !errors.As(err, &missing) {
		t.Fatalf("expected ErrNativeSessionMissing, got %T", err)
	}
	// "thread not found" is a turn-level refusal, never a session-missing proof.
	keep := missingThreadError("t1", &RPCError{Code: -32600, Message: "thread not found: t1"})
	if errors.As(keep, &missing) {
		t.Fatal("turn-level thread-not-found must not map to ErrNativeSessionMissing")
	}
	other := missingThreadError("t1", &RPCError{Code: -32000, Message: "no rollout found for thread id t1"})
	if errors.As(other, &missing) {
		t.Fatal("only the verbatim -32600 code maps to ErrNativeSessionMissing")
	}
}

// ── Poison rule documentation bound ─────────────────────────────────────

func TestFramingBoundDocumented(t *testing.T) {
	if MaxFrameBytes != 1<<20 {
		t.Fatalf("frame bound must stay at the documented 1 MiB, got %d", MaxFrameBytes)
	}
}
