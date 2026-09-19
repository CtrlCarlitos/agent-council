package adaptertest

import (
	"context"
	"errors"
	"fmt"
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
	HoldDispatchAck   chan struct{}
	DispatchStatus    adapter.DispatchStatus
	DispatchError     error
	AutoDenyTools     map[string]bool
	DropStreamEarly   bool
	InjectDuplicates  bool
	MalformedOutput   bool
	StallStream       chan struct{}
	StaleRecoveryRef  *adapter.RecoveryRef
	ProbeCapabilities adapter.AdapterCapabilities
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
	activeStreams map[adapter.TurnRef]*adapter.BufferedStream
}

// NewFake constructs a FakeAdapter configured with the provided scripted faults.
func NewFake(faults ScriptedFaults) *FakeAdapter {
	return &FakeAdapter{
		faults:        faults,
		sessions:      make(map[adapter.SessionID]adapter.SessionBinding),
		dispatches:    make(map[adapter.TurnRef]*DispatchState),
		prompts:       make(map[adapter.TurnRef]string),
		results:       make(map[adapter.TurnRef]adapter.TurnResult),
		activeStreams: make(map[adapter.TurnRef]*adapter.BufferedStream),
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
	caps := f.faults.ProbeCapabilities.Normalize()
	return adapter.ProbeReport{
		HarnessVersion: adapter.UsageMetric[string]{Value: "1.0.0-fake", Available: true},
		Capabilities:   caps,
		ModelInventory: adapter.UsageMetric[[]string]{Value: []string{"fake-model-1"}, Available: true},
	}, nil
}

// CreateSession establishes an isolated conversation binding for a logical session.
func (f *FakeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
	if err := req.Validate(); err != nil {
		return adapter.SessionBinding{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if existing, exists := f.sessions[req.SessionID]; exists {
		if existing.Config.Model != req.Config.Model || existing.Config.WorkspaceRoot != req.Config.WorkspaceRoot {
			return adapter.SessionBinding{}, errors.New("cannot recreate existing session with changed config")
		}
		return existing, nil
	}

	binding := adapter.SessionBinding{
		SessionID:       req.SessionID,
		Contributor:     req.Contributor,
		NativeSessionID: fmt.Sprintf("native-%s", req.SessionID),
		Config:          req.Config,
	}
	f.sessions[req.SessionID] = binding
	return binding, nil
}

// ResumeSession attaches to a previously verified native session binding.
func (f *FakeAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing, exists := f.sessions[binding.SessionID]
	if !exists || existing.NativeSessionID != binding.NativeSessionID {
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
	st := &DispatchState{Received: true, Accepted: true, Started: true}
	f.dispatches[ref] = st
	f.prompts[ref] = prompt
	f.results[ref] = adapter.TurnResult{
		Ref:          ref,
		Status:       council.TurnRunning,
		ResultStatus: adapter.ResultPending,
		CompletedAt:  time.Time{},
	}
	hold := f.faults.HoldDispatchAck
	status := f.faults.DispatchStatus
	dispErr := f.faults.DispatchError
	f.mu.Unlock()

	if status == "" {
		status = adapter.DispatchAccepted
	}

	if hold != nil {
		select {
		case <-ctx.Done():
			return adapter.DispatchOutcome{Ref: ref, Status: adapter.DispatchUnknown, Reason: ctx.Err().Error()}, ctx.Err()
		case <-hold:
		}
	}

	if dispErr != nil {
		return adapter.DispatchOutcome{Ref: ref, Status: status, Reason: dispErr.Error()}, dispErr
	}

	return adapter.DispatchOutcome{Ref: ref, Status: status}, nil
}

// Observe establishes an event subscription for a running turn.
func (f *FakeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	stream, _ := adapter.NewBufferedStream(ref, 64)
	f.activeStreams[ref] = stream

	go func() {
		// Emit initial progress event
		ev := adapter.Event{
			Ref:       ref,
			Type:      adapter.EventProgress,
			Status:    council.TurnRunning,
			Payload:   "started",
			Timestamp: time.Now(),
		}
		if !stream.Send(ev) {
			return
		}

		if f.faults.InjectDuplicates {
			if !stream.Send(ev) {
				return
			}
		}

		// Handle scripted auto-denied tools if configured
		if len(f.faults.AutoDenyTools) > 0 {
			for tool := range f.faults.AutoDenyTools {
				denialEv := adapter.Event{
					Ref:        ref,
					Type:       adapter.EventToolDenied,
					Status:     council.TurnRunning,
					ApprovalID: fmt.Sprintf("app-deny-%s", tool),
					Payload:    fmt.Sprintf("tool %s denied", tool),
					Timestamp:  time.Now(),
				}
				if !stream.Send(denialEv) {
					return
				}
			}
		}

		if f.faults.DropStreamEarly {
			_ = stream.CloseWithErr(errors.New("transport dropped early"))
			return
		}

		if f.faults.StallStream != nil {
			select {
			case <-f.faults.StallStream:
			case <-ctx.Done():
				_ = stream.CloseWithErr(ctx.Err())
				return
			}
		}

		// Emit terminal event
		term := adapter.Event{
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
		if !stream.Send(term) {
			return
		}

		// Update result in fake state upon successful terminal delivery
		f.mu.Lock()
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
		f.mu.Unlock()

		_ = stream.CloseWithErr(nil)
	}()

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
	if res.Status == council.TurnCompleted {
		return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelAlreadyTerminal}, nil
	}

	res.Status = council.TurnCancelled
	res.ResultStatus = adapter.ResultAvailable
	res.Output = "cancelled"
	res.CompletedAt = time.Now()
	f.results[ref] = res

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
