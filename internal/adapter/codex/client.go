package codex

// Typed client methods for the codex app-server child (AC-009 spec §3.2
// verified protocol facts, codex-cli 0.154.0): initialize, account/read,
// thread/start (creation-reservation discipline), thread/resume
// (provider-free resume probe), turn/start, turn/interrupt, and
// mcpServerStatus/list. Every request rides the shared Conn so id
// correlation and poison rules cannot diverge between call sites.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// resumeProbeTimeout bounds the provider-free resume probe. The probe is
// adapter-owned verification (spec §3.5): it deliberately does not take a
// caller context, so a disconnected controller cannot prevent the
// effective-config verification.
var resumeProbeTimeout = 10 * time.Second

// missingThreadCode is the verbatim deterministic error code for a
// thread/resume against a thread id with no rollout in CODEX_HOME.
const missingThreadCode = -32600

// missingThreadPrefix is the verbatim message prefix
// ("no rollout found for thread id <id>") that proves absence.
const missingThreadPrefix = "no rollout found for thread id "

// ClientInfo is the Council clientInfo carried by the initialize
// handshake (name "agent-council", version from build).
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// InitializeResult is the initialize handshake result. codexHome and the
// platform pair are the provider-free environment attestation surface the
// handshake gate compares against the frozen profile (spec §3.2).
type InitializeResult struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
}

// AccountStatus is the account/read result; requiresOpenaiAuth is the
// deterministic unauthenticated indicator under an isolated home.
type AccountStatus struct {
	Account            json.RawMessage `json:"account"`
	RequiresOpenaiAuth bool            `json:"requiresOpenaiAuth"`
}

// ThreadEnvironment is one thread execution environment.
type ThreadEnvironment struct {
	EnvironmentID         string   `json:"environmentId"`
	CWD                   string   `json:"cwd"`
	RuntimeWorkspaceRoots []string `json:"runtimeWorkspaceRoots,omitempty"`
}

// ThreadStatus is the native thread status object.
type ThreadStatus struct {
	Type string `json:"type"`
}

// Thread is the native thread object returned by thread/start.
type Thread struct {
	ID           string              `json:"id"`
	SessionID    string              `json:"sessionId"`
	Environments []ThreadEnvironment `json:"environments"`
	Status       ThreadStatus        `json:"status"`
	Model        string              `json:"model"`
	HistoryMode  string              `json:"historyMode"`
	Path         string              `json:"path"`
	CreatedAt    json.RawMessage     `json:"createdAt,omitempty"`
}

// ThreadStartParams are the thread/start parameters. The verified surface
// accepts empty params; the thread cwd is the frozen workspace root
// (spec §3.4).
type ThreadStartParams struct {
	CWD string `json:"cwd,omitempty"`
}

// EffectiveConfig is the complete effective configuration returned by
// thread/resume {threadId, excludeTurns:true} — the provider-free
// pre-transmission verification surface (spec §3.5). ApprovalPolicy and
// Sandbox stay raw JSON: comparison against the frozen profile is
// byte-exact on the canonical encodings.
type EffectiveConfig struct {
	ThreadID           string          `json:"id"`
	Status             ThreadStatus    `json:"status"`
	CWD                string          `json:"cwd"`
	ApprovalPolicy     json.RawMessage `json:"approvalPolicy"`
	Sandbox            json.RawMessage `json:"sandbox"`
	ApprovalsReviewer  string          `json:"approvalsReviewer"`
	Model              string          `json:"model"`
	ModelProvider      string          `json:"modelProvider"`
	InstructionSources []string        `json:"instructionSources"`
}

// TurnStartParams carry the frozen per-turn pins (spec §3.5). The exact
// native turn/input encoding is owned by the adapter layer's frozen
// turn-params seam; this struct pins the correlation and policy fields.
type TurnStartParams struct {
	ThreadID       string          `json:"threadId"`
	Input          json.RawMessage `json:"input,omitempty"`
	Model          string          `json:"model,omitempty"`
	SandboxPolicy  json.RawMessage `json:"sandboxPolicy,omitempty"`
	ApprovalPolicy json.RawMessage `json:"approvalPolicy,omitempty"`
	CWD            string          `json:"cwd,omitempty"`
}

// ErrNativeSessionMissing reports a thread/resume whose verbatim
// deterministic error (-32600 "no rollout found for thread id <id>")
// proves the native thread does not exist in the home.
type ErrNativeSessionMissing struct {
	ThreadID string
	Code     int
	Message  string
}

func (e *ErrNativeSessionMissing) Error() string {
	return fmt.Sprintf("native codex thread %s is missing (%d: %s)", e.ThreadID, e.Code, e.Message)
}

// missingThreadError maps a raw error to ErrNativeSessionMissing only on
// the verbatim deterministic shape; every other error passes through
// unchanged (absence is claimed only with positive evidence).
func missingThreadError(threadID string, err error) error {
	rpcErr, ok := err.(*RPCError)
	if !ok || rpcErr.Code != missingThreadCode {
		return err
	}
	if !strings.Contains(rpcErr.Message, missingThreadPrefix) {
		return err
	}
	return &ErrNativeSessionMissing{
		ThreadID: threadID,
		Code:     rpcErr.Code,
		Message:  rpcErr.Message,
	}
}

// CodexClient is the typed client for one app-server child.
type CodexClient struct {
	conn *Conn
	pump *EventPump
}

// NewCodexClient binds the typed client to a connection and its pump (the
// pump may be nil for connections that never create threads).
func NewCodexClient(conn *Conn, pump *EventPump) *CodexClient {
	return &CodexClient{conn: conn, pump: pump}
}

// Initialize performs the handshake: initialize with the Council
// clientInfo.
func (c *CodexClient) Initialize(ctx context.Context, info ClientInfo) (InitializeResult, error) {
	var out InitializeResult
	err := c.conn.Call(ctx, "initialize", struct {
		ClientInfo ClientInfo `json:"clientInfo"`
	}{info}, &out)
	return out, err
}

// AccountRead performs the provider-free auth gate check.
func (c *CodexClient) AccountRead(ctx context.Context) (AccountStatus, error) {
	var out AccountStatus
	err := c.conn.Call(ctx, "account/read", struct{}{}, &out)
	return out, err
}

// ThreadStart creates a native thread under the creation-reservation
// discipline (spec §3.2): the pending route is inserted BEFORE the request
// is written; the binding publishes only when the response id equals the
// notified thread id. A failed write abandons the reservation (creation
// uncertain upstream); a response/notification mismatch is protocol drift
// and poisons the connection.
func (c *CodexClient) ThreadStart(ctx context.Context, params ThreadStartParams) (Thread, error) {
	if c.pump == nil {
		return Thread{}, fmt.Errorf("thread/start requires a wired event pump")
	}
	id := c.conn.AllocateID()
	if err := c.pump.ReserveCreation(id); err != nil {
		return Thread{}, err
	}
	var thread Thread
	if err := c.conn.CallWithID(ctx, id, "thread/start", params, &thread); err != nil {
		c.pump.AbandonCreation()
		return Thread{}, err
	}
	if err := c.pump.CompleteCreation(thread.ID); err != nil {
		return Thread{}, err
	}
	return thread, nil
}

// ResumeProbe verifies an existing thread provider-free via
// thread/resume {threadId, excludeTurns:true} — no turn, no model call.
// The verbatim missing-thread error maps to ErrNativeSessionMissing; the
// probe is adapter-owned and does not take a caller context.
func (c *CodexClient) ResumeProbe(threadID string) (EffectiveConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), resumeProbeTimeout)
	defer cancel()
	var cfg EffectiveConfig
	err := c.conn.Call(ctx, "thread/resume", struct {
		ThreadID     string `json:"threadId"`
		ExcludeTurns bool   `json:"excludeTurns"`
	}{threadID, true}, &cfg)
	if err != nil {
		return EffectiveConfig{}, missingThreadError(threadID, err)
	}
	return cfg, nil
}

// TurnStart starts a turn with the frozen per-turn pins. The raw result is
// returned: turn semantics (identity binding, ack classification) are the
// adapter layer's responsibility.
func (c *CodexClient) TurnStart(ctx context.Context, params TurnStartParams) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.conn.Call(ctx, "turn/start", params, &out)
	return out, err
}

// TurnInterrupt requests a turn-scoped cooperative interrupt
// {threadId, turnId}. Acceptance of the request is NOT terminal evidence.
func (c *CodexClient) TurnInterrupt(ctx context.Context, threadID, turnID string) error {
	return c.conn.Call(ctx, "turn/interrupt", struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}{threadID, turnID}, nil)
}

// MCPServerStatusList retrieves the MCP server inventory (schema-verified
// method; toolkit verification is the adapter layer's compare).
func (c *CodexClient) MCPServerStatusList(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.conn.Call(ctx, "mcpServerStatus/list", struct{}{}, &out)
	return out, err
}

// ModelList retrieves the credential-free model catalog (schema-verified
// method; no model call is made). The raw catalog is returned: model
// pinning stays a frozen-profile concern, never an adapter choice.
func (c *CodexClient) ModelList(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.conn.Call(ctx, "model/list", struct{}{}, &out)
	return out, err
}
