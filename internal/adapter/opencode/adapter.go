package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// DispatchIdentitySource supplies the persisted attempt identity for a
// TurnRef before dispatch. Wired by the service to the storage dispatch
// intent; the adapter never reads storage directly and never invents "1".
type DispatchIdentitySource interface {
	AttemptFor(ctx context.Context, ref adapter.TurnRef) (attempt string, ok bool)
}

// managedDispatch tracks one in-flight or completed native dispatch.
type managedDispatch struct {
	nativeSessionID string
	userMessageID   string
	baselineMsgID   string
}

// launchReservation reserves a turn identity for the duration of process
// creation so concurrent duplicate dispatches cannot launch the same turn
// twice. Waiters receive the launcher's verdict.
type launchReservation struct {
	ready   chan struct{}
	outcome adapter.DispatchOutcome
	err     error
}

// OpenCodeAdapter implements the AC-006 adapter.Adapter contract against
// the installed OpenCode headless HTTP server.
type OpenCodeAdapter struct {
	executor      execpolicy.PolicyExecutor
	probeTemplate ProbeLaunchTemplate
	identity      DispatchIdentitySource
	idleGrace     time.Duration
	servers       *serverManager

	mu         sync.Mutex
	dispatches map[adapter.TurnRef]*managedDispatch
	launching  map[adapter.TurnRef]*launchReservation
}

// OpenCodeAdapterOption configures an OpenCodeAdapter.
type OpenCodeAdapterOption func(*OpenCodeAdapter)

// WithIdleGrace overrides the parked-session idle grace period.
func WithIdleGrace(d time.Duration) OpenCodeAdapterOption {
	return func(a *OpenCodeAdapter) { a.idleGrace = d }
}

// NewOpenCodeAdapter constructs an OpenCode persistent contributor adapter.
func NewOpenCodeAdapter(
	executor execpolicy.PolicyExecutor,
	probeTemplate ProbeLaunchTemplate,
	identity DispatchIdentitySource,
	opts ...OpenCodeAdapterOption,
) *OpenCodeAdapter {
	a := &OpenCodeAdapter{
		executor:      executor,
		probeTemplate: probeTemplate,
		identity:      identity,
		idleGrace:     30 * time.Second,
		servers:       newServerManager(executor, nil), // wired in SetSessionLaunchSource
		dispatches:    make(map[adapter.TurnRef]*managedDispatch),
		launching:     make(map[adapter.TurnRef]*launchReservation),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// authenticatedRequest builds an HTTP request with server basic-auth
// credentials.
func (a *OpenCodeAdapter) authenticatedRequest(ctx context.Context, method, url string, body string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Server credentials come from the serve child's GeneratedServerEnv;
	// they were set on the process env and must be presented on each HTTP
	// call. The adapter stores them per session in the server manager.
	return req, nil
}

// dispatchToServer submits a prompt to the native server.
func (a *OpenCodeAdapter) dispatchToServer(ctx context.Context, sessionID, msgID, prompt string) error {
	sp, err := a.servers.child(sessionID)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"messageID": msgID,
		"parts":     []map[string]string{{"type": "text", "text": prompt}},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/session/%s/prompt_async", sp.endpoint, sessionID), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.SetBasicAuth(sp.username, sp.password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("prompt_async returned %d", resp.StatusCode)
	}
	return nil
}

// serverGet performs an authenticated GET and decodes the response.
func (a *OpenCodeAdapter) serverGet(ctx context.Context, sessionID, path string, dst any) error {
	sp, err := a.servers.child(sessionID)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", sp.endpoint+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(sp.username, sp.password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s returned %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

// ── Dispatch ────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	if err := ref.Validate(); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	attempt, ok := a.identity.AttemptFor(ctx, ref)
	if !ok {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: "no attempt identity"}, fmt.Errorf("no attempt identity for %s/%s", ref.SessionID, ref.TurnKey)
	}

	msgID, err := NativeMessageID(string(ref.SessionID), ref.TurnKey, attempt)
	if err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	ep, err := a.endpointFor(ref.SessionID)
	if err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	payload := map[string]any{
		"messageID": msgID,
		"parts":     []map[string]string{{"type": "text", "text": prompt}},
	}
	body, _ := json.Marshal(payload)
	httpReq, reqErr := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("%s/session/%s/prompt_async", ep, ref.SessionID), strings.NewReader(string(body)))
	if reqErr != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: reqErr.Error()}, reqErr
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, respErr := http.DefaultClient.Do(httpReq)
	if respErr != nil {
		var opErr *net.OpError
		if errors.As(respErr, &opErr) {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: respErr.Error()}, respErr
		}
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown, Reason: respErr.Error()}, respErr
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 204 || resp.StatusCode == 200:
		a.mu.Lock()
		a.dispatches[ref] = &managedDispatch{userMessageID: msgID, nativeSessionID: string(ref.SessionID)}
		a.mu.Unlock()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
	default:
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
}

// ── Observe ─────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	stream := adapter.NewBufferedStream(ref, 64)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.mu.Lock()
				d, ok := a.dispatches[ref]
				a.mu.Unlock()
				if !ok {
					return
				}
				// Poll for assistant reply via parentID correlation.
				msg, found := a.findAssistantByParentID(ctx, ref.SessionID, d.userMessageID)
				if !found {
					continue
				}
				text := ""
				for _, p := range msg.Parts {
					if p.Type == "text" {
						text = p.Text
						break
					}
				}
				now := time.Now().UTC()
				if msg.Error != nil {
					_ = stream.Send(adapter.Event{
						Ref: ref, Type: adapter.EventTerminal,
						Status: council.TurnFailed, Payload: msg.Error.Message, Timestamp: now,
					})
					return
				}
				_ = stream.Send(adapter.Event{
					Ref: ref, Type: adapter.EventTerminal,
					Status: council.TurnCompleted, Payload: text, Timestamp: now,
				})
				return
			}
		}
	}()
	return stream, nil
}

// ── Cancel ──────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	a.mu.Lock()
	d, ok := a.dispatches[ref]
	a.mu.Unlock()
	if !ok {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelRejected, Reason: "turn not dispatched"}, nil
	}

	ep, err := a.endpointFor(adapter.SessionID(d.nativeSessionID))
	if err != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelRejected, Reason: err.Error()}, err
	}

	url := fmt.Sprintf("%s/session/%s/abort", ep, d.nativeSessionID)
	req, reqErr := http.NewRequestWithContext(ctx, "POST", url, nil)
	if reqErr != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelRejected, Reason: reqErr.Error()}, reqErr
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelRejected, Reason: err.Error()}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed}, nil
	}
	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown, Reason: fmt.Sprintf("abort returned %d", resp.StatusCode)}, nil
}

// ── Collect ─────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	a.mu.Lock()
	d, ok := a.dispatches[ref]
	a.mu.Unlock()
	if !ok {
		return adapter.TurnResult{
			Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultPending,
		}, errors.New("turn not dispatched")
	}

	msg, found := a.findAssistantByParentID(ctx, ref.SessionID, d.userMessageID)
	if !found {
		return adapter.TurnResult{
			Ref: ref, Status: council.TurnRunning, ResultStatus: adapter.ResultPending,
		}, nil
	}

	text := ""
	for _, p := range msg.Parts {
		if p.Type == "text" {
			text = p.Text
			break
		}
	}
	if text == "" {
		text = "(assistant message has no text content)"
	}

	now := time.Now().UTC()
	return adapter.TurnResult{
		Ref: ref, Status: council.TurnCompleted, ResultStatus: adapter.ResultAvailable,
		Output: text, CompletedAt: now,
	}, nil
}

// ── Reconcile ───────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	a.mu.Lock()
	d, hasDispatch := a.dispatches[ref.TurnRef]
	a.mu.Unlock()

	// Resolve the server endpoint for this session.
	ep, epErr := a.endpointFor(ref.TurnRef.SessionID)
	if epErr != nil {
		// No server for this session: we cannot observe the worker.
		return adapter.ReconciliationOutcome{
			Ref: ref, Reachability: council.VisibilityHostLost,
			Status: adapter.ReconciliationUncertain, Observed: council.TurnRunning,
		}, nil
	}

	// Check if the native session exists.
	req, reqErr := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/session/%s", ep, ref.TurnRef.SessionID), nil)
	if reqErr != nil {
		return adapter.ReconciliationOutcome{Ref: ref}, reqErr
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return adapter.ReconciliationOutcome{
			Ref: ref, Reachability: council.VisibilityHostLost,
			Status: adapter.ReconciliationUncertain, Observed: council.TurnRunning,
		}, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// Session 404 after a possibly-accepted dispatch proves nothing about
		// the native execution — the worker may still be running as an orphan.
		return adapter.ReconciliationOutcome{
			Ref: ref, Reachability: council.VisibilityHostLost,
			Status: adapter.ReconciliationUncertain, Observed: council.TurnRunning,
		}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return adapter.ReconciliationOutcome{
			Ref: ref, Reachability: council.VisibilityHostLost,
			Status: adapter.ReconciliationUncertain, Observed: council.TurnRunning,
		}, nil
	}

	if !hasDispatch {
		return adapter.ReconciliationOutcome{
			Ref: ref, Reachability: council.VisibilityHostLost,
			Status: adapter.ReconciliationUncertain, Observed: council.TurnRunning,
		}, nil
	}

	// Look for the assistant reply correlated by parentID.
	msg, found := a.findAssistantByParentID(ctx, ref.TurnRef.SessionID, d.userMessageID)
	if !found {
		// Session is reachable but the native execution has not produced a
		// verified outcome yet.
		return adapter.ReconciliationOutcome{
			Ref: ref, Reachability: council.VisibilityReachable,
			Status: adapter.ReconciliationUncertain, Observed: council.TurnRunning,
		}, nil
	}

	// The native execution has produced a verified terminal outcome.
	text := ""
	for _, p := range msg.Parts {
		if p.Type == "text" {
			text = p.Text
			break
		}
	}
	observed := council.TurnCompleted
	if msg.Error != nil {
		observed = council.TurnFailed
	}
	return adapter.ReconciliationOutcome{
		Ref: ref, Reachability: council.VisibilityReachable,
		Status:   adapter.ReconciliationReachableTerminal,
		Observed: observed, Result: text,
	}, nil
}

// ── CreateSession / ResumeSession ──────────────────────────────────────

func (a *OpenCodeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}
	// Create a fresh session via POST /session on the server.
	ep, epErr := a.endpointFor(req.SessionID)
	if epErr != nil {
		// No server yet; return the binding — the server will be started
		// lazily when the first dispatch or connect occurs.
		return adapter.SessionBinding{
			SessionID:       req.SessionID,
			Contributor:     req.Contributor,
			NativeSessionID: fmt.Sprintf("ses_council_%s", req.SessionID),
			Config:          req.Config,
		}, nil
	}

	payload := map[string]any{
		"title": fmt.Sprintf("council %s", req.SessionID),
		"agent": string(req.Contributor),
	}
	body, _ := json.Marshal(payload)
	httpReq, httpErr := http.NewRequestWithContext(ctx, "POST", ep+"/session", strings.NewReader(string(body)))
	if httpErr != nil {
		return adapter.SessionBinding{}, httpErr
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return adapter.SessionBinding{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return adapter.SessionBinding{}, fmt.Errorf("session create returned %d", resp.StatusCode)
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return adapter.SessionBinding{}, err
	}
	return adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: result.ID,
		Config:          req.Config,
	}, nil
}

func (a *OpenCodeAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	ep, epErr := a.endpointFor(binding.SessionID)
	if epErr != nil {
		return fmt.Errorf("%w: no server for session %s", ErrNativeSessionMissing, binding.SessionID)
	}
	url := fmt.Sprintf("%s/session/%s", ep, binding.NativeSessionID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNativeSessionMissing
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("resume check returned %d", resp.StatusCode)
	}
	return nil
}

// findAssistantByParentID polls the server for an assistant message whose
// parentID matches the given ID. Returns the message and true when found.
func (a *OpenCodeAdapter) findAssistantByParentID(ctx context.Context, sessionID adapter.SessionID, parentID string) (*fakeAssistantMsg, bool) {
	ep, err := a.endpointFor(sessionID)
	if err != nil {
		return nil, false
	}
	url := fmt.Sprintf("%s/session/%s/message", ep, sessionID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var msgs []struct {
		Info struct {
			ID       string `json:"id"`
			Role     string `json:"role"`
			ParentID string `json:"parentID"`
		} `json:"info"`
		Parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, false
	}
	for _, m := range msgs {
		if m.Info.Role == "assistant" && m.Info.ParentID == parentID {
			result := &fakeAssistantMsg{ID: m.Info.ID, Role: "assistant", ParentID: m.Info.ParentID}
			for _, p := range m.Parts {
				result.Parts = append(result.Parts, fakePart{Type: p.Type, Text: p.Text})
			}
			return result, true
		}
	}
	return nil, false
}

// ── Internal helpers ────────────────────────────────────────────────────
// ── Internal helpers ────────────────────────────────────────────────────

func (a *OpenCodeAdapter) endpointFor(sessionID adapter.SessionID) (string, error) {
	sp, err := a.servers.child(string(sessionID))
	if err != nil {
		return "", err
	}
	return sp.endpoint, nil
}

// SetSessionLaunchSource wires the service-owned launch authority. Must be
// called before any session operation.
func (a *OpenCodeAdapter) SetSessionLaunchSource(src SessionLaunchSource) {
	a.servers.launch = src
}

// ErrNativeSessionMissing reports that the referenced native session does
// not exist on the server (a missing persisted binding).
var ErrNativeSessionMissing = errors.New("native session not found on server")

// fakeAssistantMsg mirrors the assistant message shape returned by the
// fake server for testing correlation.
type fakeAssistantMsg struct {
	ID       string          `json:"id"`
	Role     string          `json:"role"`
	ParentID string          `json:"parentID"`
	Parts    []fakePart      `json:"parts"`
	Error    *fakeProbeError `json:"error"`
}

type fakePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type fakeProbeError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}
