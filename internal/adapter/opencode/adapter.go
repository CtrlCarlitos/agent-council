package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
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
		if errors.As(respErr, &opErr) && opErr.Op == "dial" {
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
	url := fmt.Sprintf("%s/session/%s/abort", ep, ref.SessionID)
	req, reqErr := http.NewRequestWithContext(ctx, "POST", url, nil)
	if reqErr != nil {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelRejected, Reason: reqErr.Error()}, reqErr
	}
	resp, err := http.DefaultClient.Do(req)
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

func (a *OpenCodeAdapter) endpointFor(sessionID adapter.SessionID) (string, error) {
	if a.servers == nil {
		return "", errors.New("no server manager wired")
	}
	sp, err := a.servers.child(string(sessionID))
	if err != nil {
		return "", err
	}
	return sp.endpoint, nil
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
