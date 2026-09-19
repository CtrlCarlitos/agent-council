package adaptertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// DispatchState records independent tracking of dispatch lifecycle within the fake.
type DispatchState struct {
	Received bool
	Accepted bool
	Started  bool
}

// ScriptedFaults specifies adversarial behaviors and deterministic synchronization gates.
type ScriptedFaults struct {
	HoldDispatchAck    chan struct{}
	HoldExecutionStart chan struct{}
	DispatchStatus     adapter.DispatchStatus
	DispatchError      error
	AutoDenyTools      map[string]bool
	DropStreamEarly    bool
	InjectDuplicates   bool
	MalformedOutput    bool
	StallStream        chan struct{}
	StaleRecoveryRef   *adapter.RecoveryRef
	ProbeCapabilities  adapter.AdapterCapabilities
}

// FakeAdapter implements adapter.Adapter with deterministic script controls for testing.
// It must never be registered or imported in production code.
type FakeAdapter struct {
	mu            sync.Mutex
	faults        ScriptedFaults
	sessions      map[adapter.SessionID]adapter.SessionBinding
	dispatches    map[adapter.TurnRef]*DispatchState
	prompts       map[adapter.TurnRef]string
	results       map[adapter.TurnRef]adapter.TurnResult
	retiredTurns  map[adapter.TurnRef]bool
	activeStreams map[adapter.TurnRef][]*adapter.BufferedStream
}

// NewFake constructs a FakeAdapter configured with the provided scripted faults.
func NewFake(faults ScriptedFaults) *FakeAdapter {
	return &FakeAdapter{
		faults:        faults,
		sessions:      make(map[adapter.SessionID]adapter.SessionBinding),
		dispatches:    make(map[adapter.TurnRef]*DispatchState),
		prompts:       make(map[adapter.TurnRef]string),
		results:       make(map[adapter.TurnRef]adapter.TurnResult),
		retiredTurns:  make(map[adapter.TurnRef]bool),
		activeStreams: make(map[adapter.TurnRef][]*adapter.BufferedStream),
	}
}

// TurnState returns an independent snapshot of what the fake received, accepted, and started.
func (f *FakeAdapter) TurnState(ref adapter.TurnRef) DispatchState {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, exists := f.dispatches[ref]
	if !exists {
		return DispatchState{}
	}
	return *st
}

// Probe returns the configured capabilities and model inventory without billing.
func (f *FakeAdapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	caps := f.faults.ProbeCapabilities
	if caps.SessionResumption == "" && caps.MidTurnCancellation == "" &&
		caps.ToolApprovalRouting == "" && caps.StreamingObservation == "" && caps.StructuredOutput == "" {
		caps = adapter.AdapterCapabilities{
			SessionResumption:    adapter.CapabilitySupported,
			MidTurnCancellation:  adapter.CapabilitySupported,
			ToolApprovalRouting:  adapter.CapabilitySupported,
			StreamingObservation: adapter.CapabilitySupported,
			StructuredOutput:     adapter.CapabilitySupported,
		}
	} else {
		caps = caps.Normalize()
	}
	return adapter.ProbeReport{
		HarnessVersion: adapter.UsageMetric[string]{Value: "1.0.0-fake", Available: true},
		Capabilities:   caps,
		ModelInventory: adapter.UsageMetric[[]string]{Value: []string{"fake-model-1"}, Available: true},
	}, nil
}

func deepCopyTools(tools []string) []string {
	if tools == nil {
		return nil
	}
	return append([]string(nil), tools...)
}

func toolsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CreateSession establishes an isolated conversation binding for a logical session.
func (f *FakeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	toolsCopy := deepCopyTools(req.Config.Tooling)

	if existing, exists := f.sessions[req.SessionID]; exists {
		if existing.Contributor != req.Contributor ||
			existing.Config.Model != req.Config.Model ||
			existing.Config.WorkspaceRoot != req.Config.WorkspaceRoot ||
			!toolsEqual(existing.Config.Tooling, req.Config.Tooling) {
			return adapter.SessionBinding{}, errors.New("cannot recreate existing session with changed config")
		}
		// Return existing binding with protected tooling slice
		ret := existing
		ret.Config.Tooling = deepCopyTools(existing.Config.Tooling)
		return ret, nil
	}

	binding := adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: fmt.Sprintf("native-%s", req.SessionID),
		Config: adapter.SessionConfig{
			WorkspaceRoot: req.Config.WorkspaceRoot,
			Model:         req.Config.Model,
			Tooling:       toolsCopy,
		},
	}
	f.sessions[req.SessionID] = binding

	ret := binding
	ret.Config.Tooling = deepCopyTools(binding.Config.Tooling)
	return ret, nil
}

// ResumeSession attaches to a previously verified native session binding.
func (f *FakeAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing, exists := f.sessions[binding.SessionID]
	if !exists ||
		existing.NativeSessionID != binding.NativeSessionID ||
		existing.Contributor != binding.Contributor ||
		existing.Config.Model != binding.Config.Model ||
		existing.Config.WorkspaceRoot != binding.Config.WorkspaceRoot ||
		!toolsEqual(existing.Config.Tooling, binding.Config.Tooling) {
		return errors.New("unknown or mismatched native session binding")
	}
	return nil
}

// Dispatch submits a prompt turn for execution.
func (f *FakeAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	if err := ref.Validate(); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

	f.mu.Lock()
	// Verify session exists
	if _, exists := f.sessions[ref.SessionID]; !exists {
		f.dispatches[ref] = &DispatchState{Received: true, Accepted: false, Started: false}
		f.mu.Unlock()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: "session not found"}, errors.New("session not found")
	}

	// Verify turn identifier has not been retired
	if f.retiredTurns[ref] {
		f.mu.Unlock()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: "turn already retired"}, errors.New("turn identifier already retired")
	}

	status := f.faults.DispatchStatus
	dispErr := f.faults.DispatchError
	holdAck := f.faults.HoldDispatchAck
	holdStart := f.faults.HoldExecutionStart

	if status == adapter.DispatchRejected {
		f.dispatches[ref] = &DispatchState{Received: true, Accepted: false, Started: false}
		f.mu.Unlock()
		if dispErr != nil {
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: dispErr.Error()}, dispErr
		}
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: "dispatch rejected by script"}, errors.New("dispatch rejected by script")
	}

	if status == "" {
		status = adapter.DispatchAccepted
	}

	started := (holdStart == nil)
	f.dispatches[ref] = &DispatchState{Received: true, Accepted: true, Started: started}
	f.prompts[ref] = prompt
	promptVal := prompt
	f.results[ref] = adapter.TurnResult{
		Ref:          ref,
		Status:       council.TurnRunning,
		ResultStatus: adapter.ResultPending,
	}

	// Launch background worker driving this turn's execution independently of observation
	go f.runWorker(ref, promptVal)

	f.mu.Unlock()

	if holdAck != nil {
		select {
		case <-ctx.Done():
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown, Reason: ctx.Err().Error()}, ctx.Err()
		case <-holdAck:
		}
	}

	if dispErr != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: status, Reason: dispErr.Error()}, dispErr
	}

	return adapter.DispatchOutcome{Ref: ref, Status: status}, nil
}

func (f *FakeAdapter) runWorker(ref adapter.TurnRef, prompt string) {
	if f.faults.HoldExecutionStart != nil {
		<-f.faults.HoldExecutionStart
		f.mu.Lock()
		if st := f.dispatches[ref]; st != nil {
			st.Started = true
		}
		f.mu.Unlock()
	}

	// Broadcast initial progress
	progEv := adapter.Event{
		Ref:       ref,
		Type:      adapter.EventProgress,
		Status:    council.TurnRunning,
		Payload:   "started",
		Timestamp: time.Now(),
	}
	f.broadcastEvent(ref, progEv)

	if f.faults.InjectDuplicates {
		f.broadcastEvent(ref, progEv)
	}

	if len(f.faults.AutoDenyTools) > 0 || strings.Contains(f.prompts[ref], "test tool denial") {
		tools := f.faults.AutoDenyTools
		if len(tools) == 0 {
			tools = map[string]bool{"bash": true}
		}
		for tool := range tools {
			denialEv := adapter.Event{
				Ref:        ref,
				Type:       adapter.EventToolDenied,
				Status:     council.TurnRunning,
				ApprovalID: fmt.Sprintf("app-deny-%s", tool),
				Payload:    fmt.Sprintf("tool %s denied", tool),
				Timestamp:  time.Now(),
			}
			f.broadcastEvent(ref, denialEv)
		}
	}

	if f.faults.StallStream != nil {
		<-f.faults.StallStream
	}

	f.mu.Lock()
	// If the turn was already cancelled, do not overwrite with completion!
	if res, ok := f.results[ref]; ok && (res.Status == council.TurnCancelled || res.Status == council.TurnCompleted) {
		f.mu.Unlock()
		return
	}

	f.results[ref] = adapter.TurnResult{
		Ref:          ref,
		Status:       council.TurnCompleted,
		ResultStatus: adapter.ResultAvailable,
		Output:       "completed output",
		CompletedAt:  time.Now(),
		Usage: adapter.ExecutionUsage{
			InputTokens:  adapter.UsageMetric[int64]{Value: 10, Available: true},
			OutputTokens: adapter.UsageMetric[int64]{Value: 20, Available: true},
			TotalCostUSD: adapter.UsageMetric[float64]{Value: 0.001, Available: true},
		},
	}
	f.retiredTurns[ref] = true
	streams := append([]*adapter.BufferedStream(nil), f.activeStreams[ref]...)
	f.activeStreams[ref] = nil
	f.mu.Unlock()

	termEv := adapter.Event{
		Ref:       ref,
		Type:      adapter.EventTerminal,
		Status:    council.TurnCompleted,
		Payload:   "completed output",
		Timestamp: time.Now(),
		Usage: adapter.ExecutionUsage{
			InputTokens:  adapter.UsageMetric[int64]{Value: 10, Available: true},
			OutputTokens: adapter.UsageMetric[int64]{Value: 20, Available: true},
			TotalCostUSD: adapter.UsageMetric[float64]{Value: 0.001, Available: true},
		},
	}
	for _, s := range streams {
		_ = s.Send(termEv)
		_ = s.CloseWithErr(nil)
	}
}

func (f *FakeAdapter) broadcastEvent(ref adapter.TurnRef, ev adapter.Event) {
	f.mu.Lock()
	streams := append([]*adapter.BufferedStream(nil), f.activeStreams[ref]...)
	f.mu.Unlock()

	for _, s := range streams {
		_ = s.Send(ev)
	}
}

// Observe establishes an event subscription for a running turn.
func (f *FakeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	st, exists := f.dispatches[ref]
	if !exists || !st.Received {
		return nil, errors.New("turn not found or not dispatched")
	}

	stream := adapter.NewBufferedStream(ref, 64)

	res, resExists := f.results[ref]
	if resExists && (res.Status == council.TurnCompleted || res.Status == council.TurnCancelled) {
		_ = stream.Send(adapter.Event{
			Ref:       ref,
			Type:      adapter.EventTerminal,
			Status:    res.Status,
			Payload:   res.Output,
			Timestamp: time.Now(),
			Usage:     res.Usage,
		})
		_ = stream.CloseWithErr(nil)
		return stream, nil
	}

	// Send initial progress synchronously into the stream buffer before subscription
	_ = stream.Send(adapter.Event{
		Ref:       ref,
		Type:      adapter.EventProgress,
		Status:    council.TurnRunning,
		Payload:   "observing",
		Timestamp: time.Now(),
	})

	if f.faults.DropStreamEarly {
		go func() {
			time.Sleep(5 * time.Millisecond)
			_ = stream.CloseWithErr(errors.New("transport dropped early"))
		}()
		return stream, nil
	}

	f.activeStreams[ref] = append(f.activeStreams[ref], stream)
	return stream, nil
}

// Cancel requests cooperative cancellation of an active turn.
func (f *FakeAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	st, exists := f.dispatches[ref]
	if !exists || !st.Received {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown, Reason: "turn not found"}, nil
	}

	res := f.results[ref]
	if res.Status == council.TurnCompleted || res.Status == council.TurnCancelled {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelAlreadyTerminal}, nil
	}

	res.Status = council.TurnCancelled
	res.ResultStatus = adapter.ResultAvailable
	res.Output = "cancelled"
	res.CompletedAt = time.Now()
	f.results[ref] = res
	f.retiredTurns[ref] = true

	streams := append([]*adapter.BufferedStream(nil), f.activeStreams[ref]...)
	f.activeStreams[ref] = nil

	go func() {
		cancelEv := adapter.Event{
			Ref:       ref,
			Type:      adapter.EventTerminal,
			Status:    council.TurnCancelled,
			Payload:   "cancelled",
			Timestamp: time.Now(),
		}
		for _, s := range streams {
			_ = s.Send(cancelEv)
			_ = s.CloseWithErr(nil)
		}
	}()

	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed}, nil
}

// Collect retrieves the finalized result and cumulative usage metrics.
func (f *FakeAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	res, exists := f.results[ref]
	if !exists {
		return adapter.TurnResult{
			Ref:          ref,
			ResultStatus: adapter.ResultUnavailable,
		}, errors.New("result unavailable")
	}

	if f.faults.MalformedOutput {
		res.ResultStatus = adapter.ResultMalformed
		res.Output = "{bad-json"
		return res, errors.New("malformed structured output")
	}

	return res, nil
}

// Reconcile checks reachability and active turn status after suspected host loss.
func (f *FakeAdapter) Reconcile(ctx context.Context, ref adapter.RecoveryRef) (adapter.ReconciliationOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	targetRef := ref
	if f.faults.StaleRecoveryRef != nil {
		targetRef = *f.faults.StaleRecoveryRef
	}

	res, exists := f.results[targetRef.TurnRef]
	if !exists {
		return adapter.ReconciliationOutcome{
			Ref:          targetRef,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationDefinitivelyMissing,
			Observed:     council.TurnFailed,
			Result:       "turn not found",
		}, nil
	}

	if res.Status == council.TurnCompleted || res.Status == council.TurnCancelled {
		return adapter.ReconciliationOutcome{
			Ref:          targetRef,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationReachableTerminal,
			Observed:     res.Status,
			Result:       res.Output,
		}, nil
	}

	return adapter.ReconciliationOutcome{
		Ref:          targetRef,
		Reachability: council.VisibilityReachable,
		Status:       adapter.ReconciliationReachableActive,
		Observed:     council.TurnRunning,
	}, nil
}

// Ensure FakeAdapter strictly adheres to adapter.Adapter interface at compile time.
var _ adapter.Adapter = (*FakeAdapter)(nil)
