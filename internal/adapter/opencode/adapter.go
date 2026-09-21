package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
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
}

// launchReservation reserves a turn identity during process creation.
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
	scratchRoot   string

	mu         sync.Mutex
	servers    *serverManager
	dispatches map[adapter.TurnRef]*managedDispatch
	launching  map[adapter.TurnRef]*launchReservation
	unknown    map[adapter.TurnRef]string // ref → messageID of an ambiguous attempt
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
		scratchRoot:   os.TempDir(),
		dispatches:    make(map[adapter.TurnRef]*managedDispatch),
		launching:     make(map[adapter.TurnRef]*launchReservation),
		unknown:       make(map[adapter.TurnRef]string),
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// NewOpenCodeAdapterWithLaunch is like NewOpenCodeAdapter but accepts an
// explicit SessionLaunchSource (the production storage-backed
// implementation).
func NewOpenCodeAdapterWithLaunch(
	executor execpolicy.PolicyExecutor,
	probeTemplate ProbeLaunchTemplate,
	identity DispatchIdentitySource,
	launch SessionLaunchSource,
	opts ...OpenCodeAdapterOption,
) *OpenCodeAdapter {
	a := NewOpenCodeAdapter(executor, probeTemplate, identity, opts...)
	a.servers = newServerManager(executor, launch)
	return a
}

// ── Probe ───────────────────────────────────────────────────────────────

// ── CreateSession ───────────────────────────────────────────────────────

func (a *OpenCodeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}
	return adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: fmt.Sprintf("ses_council_%s", req.SessionID),
		Config:          req.Config,
	}, nil
}

// ── ResumeSession ───────────────────────────────────────────────────────

func (a *OpenCodeAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	return nil
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

	// Reservation: concurrent Dispatch calls for the same ref share one
	// native attempt and the launcher's verdict.
	a.mu.Lock()
	if _, ok := a.dispatches[ref]; ok {
		a.mu.Unlock()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
	}
	if res, ok := a.launching[ref]; ok {
		a.mu.Unlock()
		<-res.ready
		return res.outcome, res.err
	}
	res := &launchReservation{ready: make(chan struct{})}
	a.launching[ref] = res
	a.mu.Unlock()

	outcome, err := a.dispatchNative(ctx, ref, ep, msgID, prompt)

	a.mu.Lock()
	delete(a.launching, ref)
	switch outcome.Status {
	case adapter.DispatchAccepted:
		delete(a.unknown, ref)
		a.dispatches[ref] = &managedDispatch{userMessageID: msgID, nativeSessionID: string(ref.SessionID)}
	case adapter.DispatchUnknown:
		a.unknown[ref] = msgID
	}
	a.mu.Unlock()

	res.outcome, res.err = outcome, err
	close(res.ready)
	return outcome, err
}

// dispatchNative performs the authenticated POST for one turn attempt. For a
// ref with a prior ambiguous attempt it first verifies by GET-by-message-ID:
// only a verified 404 authorizes resubmission; an already-recorded message is
// accepted without a second prompt_async.
func (a *OpenCodeAdapter) dispatchNative(ctx context.Context, ref adapter.TurnRef, ep serverEndpoint, msgID, prompt string) (adapter.DispatchOutcome, error) {
	a.mu.Lock()
	_, priorUnknown := a.unknown[ref]
	a.mu.Unlock()

	if priorUnknown {
		recorded, verifyErr := a.userMessageRecorded(ctx, ep, string(ref.SessionID), msgID)
		if verifyErr != nil {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown, Reason: "retry verification failed: " + verifyErr.Error()}, verifyErr
		}
		if recorded {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted, Reason: "message already recorded; resubmission skipped"}, nil
		}
	}

	payload := map[string]any{
		"messageID": msgID,
		"parts":     []map[string]string{{"type": "text", "text": prompt}},
	}
	body, _ := json.Marshal(payload)
	resp, err := a.doAuth(ctx, ep, "POST",
		fmt.Sprintf("%s/session/%s/prompt_async", ep.url, ref.SessionID), "application/json", bytes.NewReader(body))
	if err != nil {
		var pw *errPostWrite
		if errors.As(err, &pw) {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown, Reason: "post-write transport failure: " + err.Error()}, err
		}
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == 204 || resp.StatusCode == 200:
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchAccepted}, nil
	default:
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
}

// userMessageRecorded queries the server for an exact user message ID.
// Returns true on 200, false on a verified 404, and an error otherwise.
func (a *OpenCodeAdapter) userMessageRecorded(ctx context.Context, ep serverEndpoint, sessionID, msgID string) (bool, error) {
	resp, err := a.doAuth(ctx, ep, "GET",
		fmt.Sprintf("%s/session/%s/message/%s", ep.url, sessionID, msgID), "", nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return true, nil
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("message lookup returned HTTP %d", resp.StatusCode)
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
				_, ok := a.dispatches[ref]
				a.mu.Unlock()
				if !ok {
					return
				}
			}
		}
	}()
	return stream, nil
}

// ── Cancel ──────────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	ep, err := a.endpointFor(ref.SessionID)
	if err != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown, Reason: err.Error()}, err
	}
	resp, err := a.doAuth(ctx, ep, "POST", fmt.Sprintf("%s/session/%s/abort", ep.url, ref.SessionID), "", nil)
	if err != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown, Reason: err.Error()}, err
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
	msg, found := a.findAssistantByParentID(ctx, string(ref.SessionID), d.userMessageID)
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
	now := time.Now().UTC()
	if msg.Error != nil {
		return adapter.TurnResult{
			Ref: ref, Status: council.TurnFailed, ResultStatus: adapter.ResultFailed,
			Output: text, CompletedAt: now,
		}, nil
	}
	return adapter.TurnResult{
		Ref: ref, Status: council.TurnCompleted, ResultStatus: adapter.ResultAvailable,
		Output: text, CompletedAt: now,
	}, nil
}

// ── Reconcile ───────────────────────────────────────────────────────────

func (a *OpenCodeAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	msg, found := a.findAssistantByParentID(ctx, string(ref.TurnRef.SessionID), "")
	if !found {
		// No assistant message yet: the native execution may still be running
		// or the process may have died. Uncertain.
		return adapter.ReconciliationOutcome{
			Ref: ref, Reachability: council.VisibilityHostLost,
			Status: adapter.ReconciliationUncertain, Observed: council.TurnRunning,
		}, nil
	}
	observed := council.TurnCompleted
	if msg.Error != nil {
		observed = council.TurnFailed
	}
	return adapter.ReconciliationOutcome{
		Ref: ref, Reachability: council.VisibilityReachable,
		Status:   adapter.ReconciliationReachableTerminal,
		Observed: observed,
	}, nil
}

// findAssistantByParentID polls the server for an assistant message whose
// parentID matches the given ID. Returns the message and true when found.
func (a *OpenCodeAdapter) findAssistantByParentID(ctx context.Context, sessionID string, parentID string) (*fakeAssistantMsg, bool) {
	ep, err := a.endpointFor(adapter.SessionID(sessionID))
	if err != nil {
		return nil, false
	}
	resp, err := a.doAuth(ctx, ep, "GET", fmt.Sprintf("%s/session/%s/message", ep.url, sessionID), "", nil)
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

// serverEndpoint bundles the HTTP endpoint of a launched serve child with
// the generated Basic credentials retained from its GeneratedServerEnv.
type serverEndpoint struct {
	url      string
	username string
	password string
}

func (a *OpenCodeAdapter) endpointFor(sessionID adapter.SessionID) (serverEndpoint, error) {
	if a.servers == nil {
		return serverEndpoint{}, errors.New("no server manager wired")
	}
	sp, err := a.servers.child(string(sessionID))
	if err != nil {
		return serverEndpoint{}, err
	}
	return serverEndpoint{url: sp.endpoint, username: sp.username, password: sp.password}, nil
}

// errPostWrite marks a transport failure that occurred after the request
// body was fully written: the server may have processed the turn, so the
// outcome is ambiguous (unknown), never rejected.
type errPostWrite struct{ cause error }

func (e *errPostWrite) Error() string { return e.cause.Error() }
func (e *errPostWrite) Unwrap() error { return e.cause }

// doAuth performs an authenticated request against the endpoint and
// classifies transport failures: anything before the request body was fully
// written (dial refused, connect reset) is a pre-write rejection; a failure
// after the write completes is post-write ambiguity.
func (a *OpenCodeAdapter) doAuth(ctx context.Context, ep serverEndpoint, method, url string, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.SetBasicAuth(ep.username, ep.password)

	wrote := false
	trace := &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wrote = true },
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if !wrote {
			return nil, err
		}
		return nil, &errPostWrite{cause: err}
	}
	return resp, nil
}

// fakeAssistantMsg mirrors the assistant message shape returned by the
// fake server for testing parentID correlation.
type fakeAssistantMsg struct {
	ID       string     `json:"id"`
	Role     string     `json:"role"`
	ParentID string     `json:"parentID"`
	Parts    []fakePart `json:"parts"`
	Error    *struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"error"`
}

type fakePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type fakeProbeError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}
