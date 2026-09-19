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
	EmitProgressCount  int
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
	workerCancels map[adapter.TurnRef]chan struct{}
	workersWg     sync.WaitGroup
	watchersWg    sync.WaitGroup
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
		workerCancels: make(map[adapter.TurnRef]chan struct{}),
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

// IsExecutionActive reports whether a turn execution is currently accepted and unretired.
func (f *FakeAdapter) IsExecutionActive(ref adapter.TurnRef) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, exists := f.dispatches[ref]
	if !exists || !st.Accepted {
		return false
	}
	return !f.retiredTurns[ref]
}

// ExecutionStatus returns the authoritative turn status recorded within the fake.
func (f *FakeAdapter) ExecutionStatus(ref adapter.TurnRef) council.TurnStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	if res, ok := f.results[ref]; ok {
		return res.Status
	}
	return ""
}

// ActiveStreamCount returns the number of active subscriptions registered for a turn.
func (f *FakeAdapter) ActiveStreamCount(ref adapter.TurnRef) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.activeStreams[ref])
}

// WaitWorkers waits for all background worker goroutines to exit.
func (f *FakeAdapter) WaitWorkers() {
	f.workersWg.Wait()
}

// Close cancels all active workers, closes all active observation streams,
// unregisters all subscriptions, and joins all worker and watcher goroutines.
func (f *FakeAdapter) Close() error {
	f.mu.Lock()
	for _, ch := range f.workerCancels {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	for ref := range f.dispatches {
		f.retiredTurns[ref] = true
	}
	var allStreams []*adapter.BufferedStream
	for _, streams := range f.activeStreams {
		allStreams = append(allStreams, streams...)
	}
	f.mu.Unlock()

	for _, s := range allStreams {
		_ = s.Close()
	}

	f.workersWg.Wait()
	f.watchersWg.Wait()

	f.mu.Lock()
	f.activeStreams = make(map[adapter.TurnRef][]*adapter.BufferedStream)
	f.mu.Unlock()

	return nil
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
	if f.faults.ProbeCapabilities.SessionResumption == adapter.CapabilityUnsupported {
		return adapter.ErrUnsupportedCapability
	}

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
	if err := ctx.Err(); err != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: err.Error()}, err
	}

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

	// Verify turn identifier is not currently active
	if _, exists := f.dispatches[ref]; exists && !f.retiredTurns[ref] {
		f.mu.Unlock()
		return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchRejected, Reason: "turn is already active"}, errors.New("turn is already active")
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

	cancelCh := make(chan struct{})
	f.workerCancels[ref] = cancelCh
	f.workersWg.Add(1)

	// Launch background worker driving this turn's execution independently of observation
	go f.runWorker(ref, promptVal, cancelCh)

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

func (f *FakeAdapter) runWorker(ref adapter.TurnRef, prompt string, cancelCh <-chan struct{}) {
	defer f.workersWg.Done()

	if f.faults.HoldExecutionStart != nil {
		select {
		case <-f.faults.HoldExecutionStart:
		case <-cancelCh:
			return
		}
		f.mu.Lock()
		if res, ok := f.results[ref]; ok && res.Status == council.TurnCancelled {
			f.mu.Unlock()
			return
		}
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

	for i := 0; i < f.faults.EmitProgressCount; i++ {
		ev := adapter.Event{
			Ref:       ref,
			Type:      adapter.EventProgress,
			Status:    council.TurnRunning,
			Payload:   fmt.Sprintf("progress-%d", i),
			Timestamp: time.Now(),
		}
		f.broadcastEvent(ref, ev)
	}

	if len(f.faults.AutoDenyTools) > 0 || strings.Contains(prompt, "test tool denial") {
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
		select {
		case <-f.faults.StallStream:
		case <-cancelCh:
			return
		}
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
		if err := s.SendOrOverflow(termEv); err == nil {
			_ = s.CloseWithErr(nil)
		}
	}
}

func (f *FakeAdapter) broadcastEvent(ref adapter.TurnRef, ev adapter.Event) {
	f.mu.Lock()
	streams := append([]*adapter.BufferedStream(nil), f.activeStreams[ref]...)
	f.mu.Unlock()

	for _, s := range streams {
		if err := s.SendOrOverflow(ev); err != nil {
			f.mu.Lock()
			f.removeActiveStream(ref, s)
			f.mu.Unlock()
		}
	}
}

func (f *FakeAdapter) removeActiveStream(ref adapter.TurnRef, stream *adapter.BufferedStream) {
	list := f.activeStreams[ref]
	var remaining []*adapter.BufferedStream
	for _, cur := range list {
		if cur != stream {
			remaining = append(remaining, cur)
		}
	}
	if len(remaining) == 0 {
		delete(f.activeStreams, ref)
	} else {
		f.activeStreams[ref] = remaining
	}
}

// Observe establishes an event subscription for a running turn.
func (f *FakeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	if f.faults.ProbeCapabilities.StreamingObservation == adapter.CapabilityUnsupported {
		return nil, adapter.ErrUnsupportedCapability
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	st, exists := f.dispatches[ref]
	if !exists || !st.Received {
		return nil, errors.New("turn not found or not dispatched")
	}
	if !st.Accepted {
		return nil, errors.New("turn was rejected: no execution to observe")
	}

	stream := adapter.NewBufferedStream(ref, 64)

	if f.faults.DropStreamEarly {
		_ = stream.Send(adapter.Event{
			Ref:       ref,
			Type:      adapter.EventProgress,
			Status:    council.TurnRunning,
			Payload:   "observing",
			Timestamp: time.Now(),
		})
		go func() {
			time.Sleep(5 * time.Millisecond)
			_ = stream.CloseWithErr(errors.New("transport dropped early"))
		}()
		return stream, nil
	}

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

	f.activeStreams[ref] = append(f.activeStreams[ref], stream)

	// Watch observation context cancellation or explicit close to unregister stream
	f.watchersWg.Add(1)
	go func() {
		defer f.watchersWg.Done()
		select {
		case <-ctx.Done():
			f.mu.Lock()
			f.removeActiveStream(ref, stream)
			f.mu.Unlock()
			_ = stream.CloseWithErr(ctx.Err())
		case <-stream.Done():
			f.mu.Lock()
			f.removeActiveStream(ref, stream)
			f.mu.Unlock()
		}
	}()

	return stream, nil
}

// Cancel requests cooperative cancellation of an active turn.
func (f *FakeAdapter) Cancel(ctx context.Context, ref adapter.TurnRef) (adapter.CancelOutcome, error) {
	if f.faults.ProbeCapabilities.MidTurnCancellation == adapter.CapabilityUnsupported {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnsupported, Reason: "cancellation unsupported"}, nil
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	st, exists := f.dispatches[ref]
	if !exists || !st.Received {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown, Reason: "turn not found"}, nil
	}
	if !st.Accepted {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelUnknown, Reason: "turn was rejected: no execution to cancel"}, nil
	}

	// Unblock any worker waiting behind execution gates
	if ch, ok := f.workerCancels[ref]; ok {
		select {
		case <-ch:
		default:
			close(ch)
		}
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
			if err := s.SendOrOverflow(cancelEv); err == nil {
				_ = s.CloseWithErr(nil)
			}
		}
	}()

	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed}, nil
}

// Collect retrieves the finalized result and cumulative usage metrics.
func (f *FakeAdapter) Collect(ctx context.Context, ref adapter.TurnRef) (adapter.TurnResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	st, existsDisp := f.dispatches[ref]
	if existsDisp && !st.Accepted {
		return adapter.TurnResult{
			Ref:          ref,
			ResultStatus: adapter.ResultUnavailable,
		}, errors.New("result unavailable: turn was rejected")
	}

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

	st, existsDisp := f.dispatches[targetRef.TurnRef]
	if existsDisp && !st.Accepted {
		return adapter.ReconciliationOutcome{
			Ref:          targetRef,
			Reachability: council.VisibilityReachable,
			Status:       adapter.ReconciliationDefinitivelyMissing,
			Observed:     council.TurnFailed,
			Result:       "turn was rejected: no execution to reconcile",
		}, nil
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
