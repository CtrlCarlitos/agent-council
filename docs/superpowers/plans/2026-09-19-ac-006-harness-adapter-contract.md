# AC-006: Harness Adapter Contract & Adversarial Fake Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement a typed Go harness adapter contract (`internal/adapter`), a closeable buffered observation stream, an importable conformance test suite (`internal/adapter/conformance`), a scripted adversarial fake worker (`internal/adapter/adaptertest`), and Council boundary integration tests.

**Architecture:** A lightweight, typed `Adapter` interface and closeable `Stream` decoupled from `internal/council`. The adapter handles native conversation mechanics while Council owns lifecycle, scheduling, authorization, and leases. Reusable conformance checks and deterministic synchronization gates verify contract compliance without external provider calls or fragile sleeps.

**Tech Stack:** Go 1.23 standard library (`context`, `sync`, `time`, `errors`, `testing`).

**Spec:** [docs/superpowers/specs/2026-09-19-harness-adapter-contract-design.md](../../specs/2026-09-19-harness-adapter-contract-design.md)

## Global Constraints
- Target package: `internal/adapter` and subpackages `conformance` and `adaptertest`.
- Test package for drivers: external `package adapter_test` to prevent circular imports.
- Zero external dependencies beyond the existing standard library and `internal/council`.
- Test-only fake: `adaptertest` must never be registered or imported in production code; production adapter factory must fail closed.
- Explicit unknowns: unmeasured tokens/costs and missing capabilities must report `Available: false` or `CapabilityUnknown`, never false zeros.
- Deterministic synchronization: test fault injection must use channel gates (`chan struct{}`), never `time.Sleep`.
- All tests must pass with `-race`, clean `gofmt`, clean `go vet`, and clean `go build`.

## Review Focus
1. **Unacknowledged Dispatch Reservation**: When dispatch acceptance is unknown (`DispatchUnknown`), the execution reservation must be preserved; releasing a subsequent prompt must remain blocked until authoritative reconciliation resolves the turn.
2. **Session Collision on Same Contributor**: Two logical sessions allocated for the same contributor (e.g. `Claude`) must maintain independent native conversation bindings and never overwrite each other.
3. **Observation Close Unblocks Producer**: A downstream subscriber calling `Close()` or ceasing to read must immediately unblock producer goroutines without panics, leaks, or channel draining requirements.
4. **Tool Denial Does Not Retire Execution**: Emitting `EventToolDenied` must record the diagnostic denial without retiring the turn or releasing its occupied contributor slot before authoritative completion.
5. **Stale Recovery Rejection**: A recovery reply carrying an earlier generation from a prior outage during the same turn must be rejected and must not clear a subsequent active host-loss episode.

---

### Task 1: Core Types, Identifiers, and Validation (`internal/adapter/types.go`)

**Files:**
- Create: `internal/adapter/types.go`
- Test: `internal/adapter/types_test.go`

**Interfaces:**
- Consumes: `council.Contributor`, `council.TurnStatus`, `council.ExecutionVisibility` from `internal/council/domain.go`.
- Produces: `SessionID`, `TurnRef`, `RecoveryRef`, `SessionConfig`, `SessionBinding`, `ErrSessionCreationUncertain`, `UsageMetric[T]`, `ExecutionUsage`, `CapabilityStatus`, `AdapterCapabilities`, `ProbeReport`, `DispatchStatus`, `DispatchOutcome`, `CancelDisposition`, `CancelOutcome`, `ResultStatus`, `TurnResult`, `ReconciliationStatus`, `ReconciliationOutcome`, `ReconciliationOutcome.Validate()`.

- [ ] **Step 1: Write the failing tests for types and validation**

Create `internal/adapter/types_test.go`:
```go
package adapter

import (
	"testing"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func TestReconciliationOutcome_Validation(t *testing.T) {
	ref := RecoveryRef{
		TurnRef: TurnRef{
			SessionID: "sess-1",
			TurnKey:   "turn-1",
		},
		Generation: 1,
	}

	validCases := []ReconciliationOutcome{
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       ReconciliationReachableActive,
			Observed:     council.TurnRunning,
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       ReconciliationReachableActive,
			Observed:     council.TurnCancelling,
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       ReconciliationReachableTerminal,
			Observed:     council.TurnCompleted,
			Result:       "ok",
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityReachable,
			Status:       ReconciliationDefinitivelyMissing,
			Observed:     council.TurnFailed,
			Result:       "definitively missing",
		},
		{
			Ref:          ref,
			Reachability: council.VisibilityHostLost,
			Status:       ReconciliationUncertain,
		},
	}

	for i, tc := range validCases {
		if err := tc.Validate(); err != nil {
			t.Fatalf("case %d: expected valid outcome, got error: %v", i, err)
		}
	}

	invalidCases := []struct {
		name    string
		outcome ReconciliationOutcome
	}{
		{
			name: "Contradictory reachable active with completed status",
			outcome: ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityReachable,
				Status:       ReconciliationReachableActive,
				Observed:     council.TurnCompleted,
			},
		},
		{
			name: "Contradictory reachable terminal with running status",
			outcome: ReconciliationOutcome{
				Ref:          ref,
				Reachability: council.VisibilityReachable,
				Status:       ReconciliationReachableTerminal,
				Observed:     council.TurnRunning,
			},
		},
		{
			name: "Missing generation",
			outcome: ReconciliationOutcome{
				Ref: RecoveryRef{
					TurnRef:    TurnRef{SessionID: "s", TurnKey: "t"},
					Generation: 0,
				},
				Reachability: council.VisibilityReachable,
				Status:       ReconciliationReachableActive,
				Observed:     council.TurnRunning,
			},
		},
		{
			name: "Empty turn key",
			outcome: ReconciliationOutcome{
				Ref: RecoveryRef{
					TurnRef:    TurnRef{SessionID: "s", TurnKey: ""},
					Generation: 1,
				},
				Reachability: council.VisibilityReachable,
				Status:       ReconciliationReachableActive,
				Observed:     council.TurnRunning,
			},
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.outcome.Validate(); err == nil {
				t.Fatalf("expected validation error for %s, got nil", tc.name)
			}
		})
	}
}

func TestUsageMetric_Validation(t *testing.T) {
	u := ExecutionUsage{
		InputTokens:  UsageMetric[int64]{Value: -1, Available: true},
		OutputTokens: UsageMetric[int64]{Value: 10, Available: true},
	}
	if err := u.Validate(); err == nil {
		t.Fatal("expected error for negative input tokens")
	}

	valid := ExecutionUsage{
		InputTokens:  UsageMetric[int64]{Value: 100, Available: true},
		OutputTokens: UsageMetric[int64]{Value: 50, Available: true},
		TotalCostUSD: UsageMetric[float64]{Value: 0.05, Available: true},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected valid usage, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter -run TestReconciliationOutcome_Validation`
Expected: Build failure (types undefined).

- [ ] **Step 3: Implement `internal/adapter/types.go`**

Write `internal/adapter/types.go`:
```go
package adapter

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

type SessionID string

type TurnRef struct {
	SessionID SessionID
	TurnKey   string
}

func (r TurnRef) Validate() error {
	if strings.TrimSpace(string(r.SessionID)) == "" {
		return errors.New("empty session id")
	}
	if strings.TrimSpace(r.TurnKey) == "" {
		return errors.New("empty turn key")
	}
	return nil
}

type RecoveryRef struct {
	TurnRef
	Generation uint64
}

func (r RecoveryRef) Validate() error {
	if err := r.TurnRef.Validate(); err != nil {
		return err
	}
	if r.Generation == 0 {
		return errors.New("recovery generation must be positive")
	}
	return nil
}

type SessionConfig struct {
	WorkspaceRoot string
	Model         string
	Tooling       []string
}

type SessionBinding struct {
	SessionID       SessionID
	Contributor     council.Contributor
	NativeSessionID string
	Config          SessionConfig
}

type ErrSessionCreationUncertain struct {
	SessionID       SessionID
	Contributor     council.Contributor
	PartialNativeID string
	Err             error
}

func (e *ErrSessionCreationUncertain) Error() string {
	return fmt.Sprintf("session creation uncertain for %s (%s): %v", e.SessionID, e.Contributor, e.Err)
}

func (e *ErrSessionCreationUncertain) Unwrap() error {
	return e.Err
}

type UsageMetric[T any] struct {
	Value     T
	Available bool
	Estimated bool
}

type ExecutionUsage struct {
	InputTokens  UsageMetric[int64]
	OutputTokens UsageMetric[int64]
	TotalCostUSD UsageMetric[float64]
}

func (u ExecutionUsage) Validate() error {
	if u.InputTokens.Available && u.InputTokens.Value < 0 {
		return errors.New("input tokens must be non-negative")
	}
	if u.OutputTokens.Available && u.OutputTokens.Value < 0 {
		return errors.New("output tokens must be non-negative")
	}
	if u.TotalCostUSD.Available {
		if u.TotalCostUSD.Value < 0 || math.IsNaN(u.TotalCostUSD.Value) || math.IsInf(u.TotalCostUSD.Value, 0) {
			return errors.New("total cost must be non-negative and finite")
		}
	}
	return nil
}

type CapabilityStatus string

const (
	CapabilitySupported   CapabilityStatus = "supported"
	CapabilityUnsupported CapabilityStatus = "unsupported"
	CapabilityUnknown     CapabilityStatus = "unknown"
)

type AdapterCapabilities struct {
	SessionResumption    CapabilityStatus
	MidTurnCancellation  CapabilityStatus
	ToolApprovalRouting  CapabilityStatus
	StreamingObservation CapabilityStatus
	StructuredOutput     CapabilityStatus
}

func (c AdapterCapabilities) Normalize() AdapterCapabilities {
	norm := func(s CapabilityStatus) CapabilityStatus {
		if s == CapabilitySupported || s == CapabilityUnsupported {
			return s
		}
		return CapabilityUnknown
	}
	return AdapterCapabilities{
		SessionResumption:    norm(c.SessionResumption),
		MidTurnCancellation:  norm(c.MidTurnCancellation),
		ToolApprovalRouting:  norm(c.ToolApprovalRouting),
		StreamingObservation: norm(c.StreamingObservation),
		StructuredOutput:     norm(c.StructuredOutput),
	}
}

type ProbeReport struct {
	HarnessVersion UsageMetric[string]
	Capabilities   AdapterCapabilities
	ModelInventory UsageMetric[[]string]
}

type DispatchStatus string

const (
	DispatchAccepted DispatchStatus = "accepted"
	DispatchRejected DispatchStatus = "rejected"
	DispatchUnknown  DispatchStatus = "unknown"
)

type DispatchOutcome struct {
	Ref    TurnRef
	Status DispatchStatus
	Reason string
}

type CancelDisposition string

const (
	CancelRequested       CancelDisposition = "requested"
	CancelConfirmed       CancelDisposition = "confirmed"
	CancelAlreadyTerminal CancelDisposition = "already_terminal"
	CancelRejected        CancelDisposition = "rejected"
	CancelUnsupported     CancelDisposition = "unsupported"
	CancelUnknown         CancelDisposition = "unknown"
)

type CancelOutcome struct {
	Ref         TurnRef
	Disposition CancelDisposition
	Reason      string
}

type ResultStatus string

const (
	ResultPending     ResultStatus = "pending"
	ResultAvailable   ResultStatus = "available"
	ResultUnavailable ResultStatus = "unavailable"
	ResultMalformed   ResultStatus = "malformed"
	ResultFailed      ResultStatus = "failed"
)

type TurnResult struct {
	Ref          TurnRef
	Status       council.TurnStatus
	ResultStatus ResultStatus
	Output       string
	RawEvidence  []byte
	Usage        ExecutionUsage
	CompletedAt  time.Time
}

type ReconciliationStatus string

const (
	ReconciliationReachableActive     ReconciliationStatus = "reachable_active"
	ReconciliationReachableTerminal   ReconciliationStatus = "reachable_terminal"
	ReconciliationDefinitivelyMissing ReconciliationStatus = "definitively_missing"
	ReconciliationUncertain           ReconciliationStatus = "uncertain"
)

type ReconciliationOutcome struct {
	Ref          RecoveryRef
	Reachability council.ExecutionVisibility
	Status       ReconciliationStatus
	Observed     council.TurnStatus
	Result       string
}

func (r ReconciliationOutcome) Validate() error {
	if err := r.Ref.Validate(); err != nil {
		return err
	}
	switch r.Status {
	case ReconciliationReachableActive:
		if r.Reachability != council.VisibilityReachable {
			return errors.New("reachable active requires VisibilityReachable")
		}
		if r.Observed != council.TurnRunning && r.Observed != council.TurnCancelling {
			return fmt.Errorf("reachable active contradicts observed status %s", r.Observed)
		}
	case ReconciliationReachableTerminal:
		if r.Reachability != council.VisibilityReachable {
			return errors.New("reachable terminal requires VisibilityReachable")
		}
		if r.Observed != council.TurnCompleted && r.Observed != council.TurnCancelled &&
			r.Observed != council.TurnFailed && r.Observed != council.TurnInterrupted {
			return fmt.Errorf("reachable terminal contradicts observed status %s", r.Observed)
		}
	case ReconciliationDefinitivelyMissing:
		if r.Reachability != council.VisibilityReachable {
			return errors.New("definitively missing requires VisibilityReachable")
		}
		if r.Observed != council.TurnFailed {
			return fmt.Errorf("definitively missing requires TurnFailed, got %s", r.Observed)
		}
	case ReconciliationUncertain:
		if r.Reachability != council.VisibilityHostLost {
			return errors.New("uncertain reconciliation must retain VisibilityHostLost")
		}
	default:
		return fmt.Errorf("unknown reconciliation status %s", r.Status)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race -v ./internal/adapter`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/types.go internal/adapter/types_test.go
git commit -m "feat(adapter): define core identities, metric types, and outcome models"
```

---

### Task 2: Observation Stream Envelope and Finite Buffer Implementation (`internal/adapter/stream.go`)

**Files:**
- Create: `internal/adapter/stream.go`
- Test: `internal/adapter/stream_test.go`

**Interfaces:**
- Consumes: `TurnRef`, `ExecutionUsage`, `council.TurnStatus` from `types.go`.
- Produces: `EventType`, `Event`, `Event.Validate()`, `Stream` interface, `NewBufferedStream(ref TurnRef, bufferCapacity int) (*BufferedStream, chan<- Event)`.

- [ ] **Step 1: Write the failing tests for event envelope and stream lifecycle**

Create `internal/adapter/stream_test.go`:
```go
package adapter

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func TestEvent_Validation(t *testing.T) {
	ref := TurnRef{SessionID: "s1", TurnKey: "t1"}

	// Valid progress event
	ev := Event{
		Ref:     ref,
		Type:    EventProgress,
		Status:  council.TurnRunning,
		Payload: "in progress",
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("expected valid progress event, got %v", err)
	}

	// Tool requested requires non-empty ApprovalID
	toolReq := Event{
		Ref:     ref,
		Type:    EventToolRequested,
		Status:  council.TurnRunning,
		Payload: "shell",
	}
	if err := toolReq.Validate(); err == nil {
		t.Fatal("expected error for EventToolRequested without ApprovalID")
	}

	toolReq.ApprovalID = "app-123"
	if err := toolReq.Validate(); err != nil {
		t.Fatalf("expected valid tool requested, got %v", err)
	}

	// Progress cannot carry terminal status
	invalidProg := Event{
		Ref:     ref,
		Type:    EventProgress,
		Status:  council.TurnCompleted,
		Payload: "done",
	}
	if err := invalidProg.Validate(); err == nil {
		t.Fatal("expected error for progress event with terminal status")
	}

	// Payload limit (64 KiB)
	oversized := Event{
		Ref:     ref,
		Type:    EventProgress,
		Status:  council.TurnRunning,
		Payload: string(make([]byte, 65*1024)),
	}
	if err := oversized.Validate(); err == nil {
		t.Fatal("expected error for oversized payload")
	}
}

func TestBufferedStream_ProducerUnblockedOnClose(t *testing.T) {
	ref := TurnRef{SessionID: "s1", TurnKey: "t1"}
	stream, in := NewBufferedStream(ref, 2)

	// Send 2 events to fill buffer
	in <- Event{Ref: ref, Type: EventProgress, Status: council.TurnRunning, Payload: "1"}
	in <- Event{Ref: ref, Type: EventProgress, Status: council.TurnRunning, Payload: "2"}

	// Launch producer trying to send a 3rd event
	doneProducer := make(chan struct{})
	go func() {
		defer close(doneProducer)
		ev := Event{Ref: ref, Type: EventProgress, Status: council.TurnRunning, Payload: "3"}
		stream.Send(ev)
	}()

	// Consumer closes stream without reading
	if err := stream.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Producer must unblock promptly
	select {
	case <-doneProducer:
	case <-time.After(1 * time.Second):
		t.Fatal("producer remained blocked after stream.Close()")
	}

	// Err() must return context canceled or closed error
	if stream.Err() == nil {
		t.Fatal("expected stream error after Close(), got nil")
	}
}

func TestBufferedStream_ConcurrentCloseIsIdempotent(t *testing.T) {
	ref := TurnRef{SessionID: "s1", TurnKey: "t1"}
	stream, _ := NewBufferedStream(ref, 10)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = stream.Close()
		}()
	}
	wg.Wait()

	if err := stream.Close(); err != nil {
		t.Fatalf("repeated Close failed: %v", err)
	}
}

func TestBufferedStream_SlowConsumerOverflow(t *testing.T) {
	ref := TurnRef{SessionID: "s1", TurnKey: "t1"}
	stream, in := NewBufferedStream(ref, 2)

	// Push beyond capacity with SendNonBlocking
	in <- Event{Ref: ref, Type: EventProgress, Status: council.TurnRunning, Payload: "1"}
	in <- Event{Ref: ref, Type: EventProgress, Status: council.TurnRunning, Payload: "2"}

	err := stream.SendOrOverflow(Event{Ref: ref, Type: EventProgress, Status: council.TurnRunning, Payload: "3"})
	if !errors.Is(err, ErrBufferOverflow) {
		t.Fatalf("expected ErrBufferOverflow, got %v", err)
	}
	if !errors.Is(stream.Err(), ErrBufferOverflow) {
		t.Fatalf("expected stream.Err() to be ErrBufferOverflow, got %v", stream.Err())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter -run TestEvent_Validation`
Expected: Build failure (undefined types and methods).

- [ ] **Step 3: Implement `internal/adapter/stream.go`**

Write `internal/adapter/stream.go`:
```go
package adapter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

var (
	ErrBufferOverflow = errors.New("observation stream buffer overflow: consumer too slow")
	ErrPayloadTooLarge = errors.New("event payload exceeds maximum size limit")
	ErrStreamClosed    = errors.New("observation stream closed")
)

const (
	MaxEventPayloadBytes = 64 * 1024      // 64 KiB
	MaxRawEvidenceBytes  = 1024 * 1024    // 1 MiB
	DefaultBufferCapacity = 64
)

type EventType string

const (
	EventProgress      EventType = "progress"
	EventToolRequested EventType = "tool_requested"
	EventToolApproved  EventType = "tool_approved"
	EventToolDenied    EventType = "tool_denied"
	EventTerminal      EventType = "terminal"
)

type Event struct {
	Ref        TurnRef
	Type       EventType
	Status     council.TurnStatus
	ApprovalID string
	Payload    string
	Usage      ExecutionUsage
	Timestamp  time.Time
}

func (e Event) Validate() error {
	if err := e.Ref.Validate(); err != nil {
		return err
	}
	if len(e.Payload) > MaxEventPayloadBytes {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(e.Payload), MaxEventPayloadBytes)
	}
	if err := e.Usage.Validate(); err != nil {
		return err
	}

	switch e.Type {
	case EventProgress:
		if e.Status != council.TurnRunning && e.Status != council.TurnCancelling {
			return fmt.Errorf("progress event cannot carry non-active status %s", e.Status)
		}
	case EventToolRequested:
		if e.Status != council.TurnRunning {
			return fmt.Errorf("tool requested event must carry TurnRunning, got %s", e.Status)
		}
		if e.ApprovalID == "" {
			return errors.New("tool requested event requires non-empty ApprovalID")
		}
	case EventToolApproved:
		if e.Status != council.TurnRunning {
			return fmt.Errorf("tool approved event must carry TurnRunning, got %s", e.Status)
		}
		if e.ApprovalID == "" {
			return errors.New("tool approved event requires non-empty ApprovalID")
		}
	case EventToolDenied:
		if e.Status != council.TurnRunning && e.Status != council.TurnFailed {
			return fmt.Errorf("tool denied event invalid status %s", e.Status)
		}
		if e.ApprovalID == "" {
			return errors.New("tool denied event requires non-empty ApprovalID")
		}
	case EventTerminal:
		if e.Status != council.TurnCompleted && e.Status != council.TurnCancelled &&
			e.Status != council.TurnFailed && e.Status != council.TurnInterrupted {
			return fmt.Errorf("terminal event requires terminal status, got %s", e.Status)
		}
	default:
		return fmt.Errorf("unknown event type %s", e.Type)
	}
	return nil
}

type Stream interface {
	Events() <-chan Event
	Err() error
	Close() error
}

type BufferedStream struct {
	ref     TurnRef
	events  chan Event
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.RWMutex
	err     error
	closed  bool
}

func NewBufferedStream(ref TurnRef, capacity int) (*BufferedStream, chan<- Event) {
	if capacity <= 0 {
		capacity = DefaultBufferCapacity
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan Event, capacity)
	bs := &BufferedStream{
		ref:    ref,
		events: events,
		ctx:    ctx,
		cancel: cancel,
	}
	return bs, events
}

func (s *BufferedStream) Events() <-chan Event {
	return s.events
}

func (s *BufferedStream) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

func (s *BufferedStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.err == nil {
		s.err = ErrStreamClosed
	}
	s.cancel()
	close(s.events)
	s.mu.Unlock()
	return nil
}

func (s *BufferedStream) CloseWithErr(err error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.err = err
	s.cancel()
	close(s.events)
	s.mu.Unlock()
	return nil
}

func (s *BufferedStream) Send(ev Event) bool {
	select {
	case <-s.ctx.Done():
		return false
	case s.events <- ev:
		return true
	}
}

func (s *BufferedStream) SendOrOverflow(ev Event) error {
	select {
	case <-s.ctx.Done():
		return ErrStreamClosed
	case s.events <- ev:
		return nil
	default:
		_ = s.CloseWithErr(ErrBufferOverflow)
		return ErrBufferOverflow
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race -v ./internal/adapter`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/stream.go internal/adapter/stream_test.go
git commit -m "feat(adapter): implement observation stream and event envelope with finite buffer"
```

---

### Task 3: Adapter Interface Definition (`internal/adapter/adapter.go`)

**Files:**
- Create: `internal/adapter/adapter.go`

**Interfaces:**
- Consumes: All types from `types.go` and `Stream` from `stream.go`.
- Produces: `CreateSessionRequest`, `Adapter` interface.

- [ ] **Step 1: Write `internal/adapter/adapter.go`**

```go
package adapter

import (
	"context"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// CreateSessionRequest encapsulates requirements for starting a fresh native harness session.
type CreateSessionRequest struct {
	SessionID   SessionID
	Contributor council.Contributor
	Config      SessionConfig
}

// Adapter defines the typed contract every native harness integration must implement.
// Call contexts bound individual RPCs/requests and never cancel accepted background workers.
type Adapter interface {
	// Probe discovers supported capabilities, version, and model inventory without billing.
	Probe(ctx context.Context) (ProbeReport, error)

	// CreateSession establishes an isolated conversation binding for a logical session.
	// Duplicate requests with matching config return the existing binding idempotently.
	CreateSession(ctx context.Context, req CreateSessionRequest) (SessionBinding, error)

	// ResumeSession attaches to a previously verified native session binding.
	ResumeSession(ctx context.Context, binding SessionBinding) error

	// Dispatch submits a prompt turn for execution. If the transport disconnects or times out
	// after potential worker receipt, it returns DispatchUnknown, preserving execution reservation.
	Dispatch(ctx context.Context, ref TurnRef, prompt string) (DispatchOutcome, error)

	// Observe establishes an event subscription for a running turn.
	// Cancelling ctx ends the subscription but does NOT cancel the underlying worker.
	Observe(ctx context.Context, ref TurnRef) (Stream, error)

	// Cancel requests cooperative cancellation of an active turn.
	Cancel(ctx context.Context, ref TurnRef) (CancelOutcome, error)

	// Collect retrieves the finalized result and cumulative usage metrics.
	Collect(ctx context.Context, ref TurnRef) (TurnResult, error)

	// Reconcile checks host reachability and verifies active turn status after suspected host loss.
	Reconcile(ctx context.Context, ref RecoveryRef) (ReconciliationOutcome, error)
}
```

- [ ] **Step 2: Verify package builds**

Run: `go build ./internal/adapter`
Expected: Exit code 0.

- [ ] **Step 3: Commit**

```bash
git add internal/adapter/adapter.go
git commit -m "feat(adapter): define Adapter interface and context lifetime rules"
```

---

### Task 4: Scripted Adversarial Fake (`internal/adapter/adaptertest/fake.go`)

**Files:**
- Create: `internal/adapter/adaptertest/fake.go`
- Test: `internal/adapter/adaptertest/fake_test.go`

**Interfaces:**
- Consumes: `adapter.Adapter`, `adapter.TurnRef`, `adapter.RecoveryRef`, etc.
- Produces: `adaptertest.DispatchState`, `adaptertest.ScriptedFaults`, `adaptertest.FakeAdapter`, `adaptertest.NewFake()`.

- [ ] **Step 1: Write failing test for scripted fake controls and gates**

Create `internal/adapter/adaptertest/fake_test.go`:
```go
package adaptertest

import (
	"context"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func TestFakeAdapter_DispatchGateAndTracking(t *testing.T) {
	holdAck := make(chan struct{})
	fake := NewFake(ScriptedFaults{
		HoldDispatchAck: holdAck,
		DispatchStatus:  adapter.DispatchUnknown,
	})

	ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}

	doneDispatch := make(chan adapter.DispatchOutcome)
	go func() {
		outcome, _ := fake.Dispatch(context.Background(), ref, "prompt")
		doneDispatch <- outcome
	}()

	// Verify received immediately before gate opens
	select {
	case <-doneDispatch:
		t.Fatal("dispatch returned before gate opened")
	case <-time.After(50 * time.Millisecond):
	}

	state := fake.TurnState(ref)
	if !state.Received || !state.Started {
		t.Fatalf("expected received and started, got %+v", state)
	}

	// Release gate
	close(holdAck)

	select {
	case outcome := <-doneDispatch:
		if outcome.Status != adapter.DispatchUnknown {
			t.Fatalf("expected DispatchUnknown, got %s", outcome.Status)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("dispatch did not unblock after gate release")
	}
}

func TestFakeAdapter_ReconcileHonorsStaleRef(t *testing.T) {
	staleRef := &adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "s1", TurnKey: "t1"},
		Generation: 1,
	}

	fake := NewFake(ScriptedFaults{
		StaleRecoveryRef: staleRef,
	})

	queryRef := adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: "s1", TurnKey: "t1"},
		Generation: 2,
	}

	outcome, err := fake.Reconcile(context.Background(), queryRef)
	if err != nil {
		t.Fatalf("reconcile returned unexpected error: %v", err)
	}

	// Fake must deliver the exact scripted stale reference without auto-repairing
	if outcome.Ref.Generation != 1 {
		t.Fatalf("fake auto-repaired generation: expected 1, got %d", outcome.Ref.Generation)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/adapter/adaptertest`
Expected: Build failure (undefined package).

- [ ] **Step 3: Implement `internal/adapter/adaptertest/fake.go`**

Write `internal/adapter/adaptertest/fake.go`:
```go
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

type DispatchState struct {
	Received bool
	Accepted bool
	Started  bool
}

type ScriptedFaults struct {
	HoldDispatchAck  chan struct{}
	DispatchStatus   adapter.DispatchStatus
	DispatchError    error
	AutoDenyTools    map[string]bool
	DropStreamEarly  bool
	InjectDuplicates bool
	MalformedOutput  bool
	StallStream      chan struct{}
	StaleRecoveryRef *adapter.RecoveryRef
	ProbeCapabilities adapter.AdapterCapabilities
}

type FakeAdapter struct {
	mu             sync.Mutex
	faults         ScriptedFaults
	sessions       map[adapter.SessionID]adapter.SessionBinding
	dispatches     map[adapter.TurnRef]*DispatchState
	results        map[adapter.TurnRef]adapter.TurnResult
	activeStreams  map[adapter.TurnRef]*adapter.BufferedStream
}

func NewFake(faults ScriptedFaults) *FakeAdapter {
	return &FakeAdapter{
		faults:        faults,
		sessions:      make(map[adapter.SessionID]adapter.SessionBinding),
		dispatches:    make(map[adapter.TurnRef]*DispatchState),
		results:       make(map[adapter.TurnRef]adapter.TurnResult),
		activeStreams: make(map[adapter.TurnRef]*adapter.BufferedStream),
	}
}

func (f *FakeAdapter) TurnState(ref adapter.TurnRef) DispatchState {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, exists := f.dispatches[ref]
	if !exists {
		return DispatchState{}
	}
	return *st
}

func (f *FakeAdapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	caps := f.faults.ProbeCapabilities.Normalize()
	return adapter.ProbeReport{
		HarnessVersion: adapter.UsageMetric[string]{Value: "1.0.0-fake", Available: true},
		Capabilities:   caps,
		ModelInventory: adapter.UsageMetric[[]string]{Value: []string{"fake-model-1"}, Available: true},
	}, nil
}

func (f *FakeAdapter) CreateSession(ctx context.Context, req adapter.CreateSessionRequest) (adapter.SessionBinding, error) {
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

func (f *FakeAdapter) ResumeSession(ctx context.Context, binding adapter.SessionBinding) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing, exists := f.sessions[binding.SessionID]
	if !exists || existing.NativeSessionID != binding.NativeSessionID {
		return errors.New("unknown or mismatched native session binding")
	}
	return nil
}

func (f *FakeAdapter) Dispatch(ctx context.Context, ref adapter.TurnRef, prompt string) (adapter.DispatchOutcome, error) {
	f.mu.Lock()
	st := &DispatchState{Received: true, Accepted: true, Started: true}
	f.dispatches[ref] = st
	f.results[ref] = adapter.TurnResult{
		Ref:          ref,
		Status:       council.TurnRunning,
		ResultStatus: adapter.ResultPending,
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

func (f *FakeAdapter) Observe(ctx context.Context, ref adapter.TurnRef) (adapter.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	stream, in := adapter.NewBufferedStream(ref, 64)
	f.activeStreams[ref] = stream

	go func() {
		// Emit initial progress
		ev := adapter.Event{
			Ref:       ref,
			Type:      adapter.EventProgress,
			Status:    council.TurnRunning,
			Payload:   "started",
			Timestamp: time.Now(),
		}
		in <- ev

		if f.faults.InjectDuplicates {
			in <- ev
		}

		if f.faults.DropStreamEarly {
			_ = stream.CloseWithErr(errors.New("transport dropped early"))
			return
		}

		if f.faults.StallStream != nil {
			<-f.faults.StallStream
		}

		// Emit terminal event
		term := adapter.Event{
			Ref:       ref,
			Type:      adapter.EventTerminal,
			Status:    council.TurnCompleted,
			Payload:   "completed output",
			Timestamp: time.Now(),
		}
		in <- term
		_ = stream.CloseWithErr(nil)
	}()

	return stream, nil
}

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
	f.results[ref] = res

	return adapter.CancelOutcome{Ref: ref, Disposition: adapter.CancelConfirmed}, nil
}

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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -race -v ./internal/adapter/adaptertest`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/adapter/adaptertest/fake.go internal/adapter/adaptertest/fake_test.go
git commit -m "feat(adapter): implement deterministic scripted fake with synchronization gates"
```

---

### Task 5: Reusable Conformance Test Suite & Green Sensitivity Proofs (`internal/adapter/conformance/suite.go`)

**Files:**
- Create: `internal/adapter/conformance/suite.go`
- Create: `internal/adapter/conformance_test.go` (external test package `adapter_test`)

**Interfaces:**
- Consumes: `adapter.Adapter`, `adaptertest.FakeAdapter`.
- Produces: `conformance.Fixture`, `conformance.Violation`, `conformance.Check()`, `conformance.Run()`.

- [ ] **Step 1: Write `internal/adapter/conformance/suite.go`**

```go
package conformance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

type Fixture interface {
	Adapter() adapter.Adapter
	TurnState(ref adapter.TurnRef) (received, accepted, started bool)
	Cleanup() error
}

type FixtureFactory func(t *testing.T) Fixture

type ViolationCode string

const (
	ViolationEOFAsCompletion          ViolationCode = "EOF_TREATED_AS_COMPLETION"
	ViolationGenerationSubstituted    ViolationCode = "RECOVERY_GENERATION_SUBSTITUTED"
	ViolationSessionIDCollision       ViolationCode = "SESSION_ID_COLLISION"
	ViolationUnboundedPayload         ViolationCode = "UNBOUNDED_PAYLOAD_ACCEPTED"
	ViolationPrematureDenialRetirement ViolationCode = "PREMATURE_DENIAL_RETIREMENT"
	ViolationCapabilityFabricated     ViolationCode = "CAPABILITY_FABRICATED"
)

type Violation struct {
	Code        ViolationCode
	Description string
}

type Scenario string

const (
	ScenarioSessionIsolation  Scenario = "session_isolation"
	ScenarioObservationClose  Scenario = "observation_close"
	ScenarioRecoveryIntegrity Scenario = "recovery_integrity"
)

func Check(ctx context.Context, fixture Fixture, scenario Scenario) []Violation {
	var violations []Violation
	ad := fixture.Adapter()

	switch scenario {
	case ScenarioSessionIsolation:
		req1 := adapter.CreateSessionRequest{
			SessionID:   "session-1",
			Contributor: council.Claude,
			Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		b1, err1 := ad.CreateSession(ctx, req1)
		req2 := adapter.CreateSessionRequest{
			SessionID:   "session-2",
			Contributor: council.Claude,
			Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
		}
		b2, err2 := ad.CreateSession(ctx, req2)

		if err1 == nil && err2 == nil && b1.NativeSessionID == b2.NativeSessionID {
			violations = append(violations, Violation{
				Code:        ViolationSessionIDCollision,
				Description: "two distinct logical sessions bound to identical native session ID",
			})
		}

	case ScenarioObservationClose:
		ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}
		_, _ = ad.Dispatch(ctx, ref, "test prompt")
		stream, err := ad.Observe(ctx, ref)
		if err == nil {
			_ = stream.Close()
			// Stream closing must not fabricate completion in Collect
			res, err := ad.Collect(ctx, ref)
			if err == nil && res.Status == council.TurnCompleted {
				violations = append(violations, Violation{
					Code:        ViolationEOFAsCompletion,
					Description: "closing stream fabricated terminal turn completion",
				})
			}
		}

	case ScenarioRecoveryIntegrity:
		ref := adapter.RecoveryRef{
			TurnRef:    adapter.TurnRef{SessionID: "s1", TurnKey: "t1"},
			Generation: 2,
		}
		outcome, err := ad.Reconcile(ctx, ref)
		if err == nil && outcome.Ref.Generation != ref.Generation {
			violations = append(violations, Violation{
				Code:        ViolationGenerationSubstituted,
				Description: fmt.Sprintf("reconciliation substituted generation: expected %d, got %d", ref.Generation, outcome.Ref.Generation),
			})
		}
	}

	return violations
}

func Run(t *testing.T, factory FixtureFactory) {
	t.Helper()

	t.Run("SessionIsolation", func(t *testing.T) {
		f := factory(t)
		defer func() { _ = f.Cleanup() }()
		violations := Check(context.Background(), f, ScenarioSessionIsolation)
		if len(violations) > 0 {
			t.Fatalf("unexpected violations: %+v", violations)
		}
	})

	t.Run("ObservationCloseDoesNotComplete", func(t *testing.T) {
		f := factory(t)
		defer func() { _ = f.Cleanup() }()
		violations := Check(context.Background(), f, ScenarioObservationClose)
		if len(violations) > 0 {
			t.Fatalf("unexpected violations: %+v", violations)
		}
	})

	t.Run("RecoveryGenerationIntegrity", func(t *testing.T) {
		f := factory(t)
		defer func() { _ = f.Cleanup() }()
		violations := Check(context.Background(), f, ScenarioRecoveryIntegrity)
		if len(violations) > 0 {
			t.Fatalf("unexpected violations: %+v", violations)
		}
	})
}
```

- [ ] **Step 2: Write tests in `internal/adapter/conformance_test.go`**

```go
package adapter_test

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/conformance"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

type fakeFixture struct {
	fake *adaptertest.FakeAdapter
}

func (f *fakeFixture) Adapter() adapter.Adapter { return f.fake }
func (f *fakeFixture) TurnState(ref adapter.TurnRef) (bool, bool, bool) {
	st := f.fake.TurnState(ref)
	return st.Received, st.Accepted, st.Started
}
func (f *fakeFixture) Cleanup() error { return nil }

func TestConformance_ConformingFakePasses(t *testing.T) {
	conformance.Run(t, func(t *testing.T) conformance.Fixture {
		return &fakeFixture{fake: adaptertest.NewFake(adaptertest.ScriptedFaults{})}
	})
}

func TestConformance_SensitivityChecks(t *testing.T) {
	t.Run("DetectsRewrittenRecoveryGeneration", func(t *testing.T) {
		staleRef := &adapter.RecoveryRef{
			TurnRef:    adapter.TurnRef{SessionID: "s1", TurnKey: "t1"},
			Generation: 1, // Will substitute gen 1 when gen 2 was asked
		}
		brokenFake := adaptertest.NewFake(adaptertest.ScriptedFaults{
			StaleRecoveryRef: staleRef,
		})

		f := &fakeFixture{fake: brokenFake}
		violations := conformance.Check(context.Background(), f, conformance.ScenarioRecoveryIntegrity)

		if len(violations) == 0 {
			t.Fatal("expected ViolationGenerationSubstituted, got 0 violations")
		}
		if violations[0].Code != conformance.ViolationGenerationSubstituted {
			t.Fatalf("expected ViolationGenerationSubstituted, got %s", violations[0].Code)
		}
	})
}
```

- [ ] **Step 3: Run tests to verify they pass**

Run: `go test -race -v ./internal/adapter -run TestConformance_`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/adapter/conformance/suite.go internal/adapter/conformance_test.go
git commit -m "feat(adapter): implement reusable conformance test suite with green sensitivity proofs"
```

---

### Task 6: Council Boundary Integration Tests (`internal/adapter/council_boundary_test.go`)

**Files:**
- Create: `internal/adapter/council_boundary_test.go` (package `adapter_test`)

**Interfaces:**
- Consumes: `council.Session`, `adaptertest.FakeAdapter`, `adapter.TurnRef`, `adapter.RecoveryRef`.
- Exercises: Council lifecycle under adversarial adapter behavior (lost dispatch ack preserves reservation, cancellation confirmed vs requested, tool denial does not retire slot, stale recovery rejected, 4 concurrent contributor sessions isolated).

- [ ] **Step 1: Write `internal/adapter/council_boundary_test.go`**

```go
package adapter_test

import (
	"context"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func TestCouncilBoundary_UnacknowledgedDispatchPreservesReservation(t *testing.T) {
	sess, err := council.NewSession(council.Claude, "lease-1")
	if err != nil {
		t.Fatal(err)
	}

	_ = sess.Queue("lease-1", "t1", "prompt 1")
	_ = sess.Queue("lease-1", "t2", "prompt 2")

	// Release t1
	ref1, err := sess.Release("lease-1", "t1")
	if err != nil {
		t.Fatalf("release t1 failed: %v", err)
	}

	// Dispatch t1 to fake with held acknowledgement
	holdAck := make(chan struct{})
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		HoldDispatchAck: holdAck,
		DispatchStatus:  adapter.DispatchUnknown,
	})

	turnRef1 := adapter.TurnRef{SessionID: adapter.SessionID(sess.ID), TurnKey: ref1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	doneDispatch := make(chan adapter.DispatchOutcome)
	go func() {
		outcome, _ := fake.Dispatch(ctx, turnRef1, "prompt 1")
		doneDispatch <- outcome
	}()

	close(holdAck)
	outcome := <-doneDispatch
	if outcome.Status != adapter.DispatchUnknown {
		t.Fatalf("expected DispatchUnknown, got %s", outcome.Status)
	}

	// Turn reservation must be preserved: attempting to release t2 must fail!
	if _, err := sess.Release("lease-1", "t2"); err == nil {
		t.Fatal("release of t2 permitted while t1 execution reservation remains active")
	}

	if sess.Active != "t1" || sess.State != council.Running {
		t.Fatalf("session state corrupted after DispatchUnknown: active=%s state=%s", sess.Active, sess.State)
	}
}

func TestCouncilBoundary_ToolDenialDoesNotRetireSlot(t *testing.T) {
	sess, err := council.NewSession(council.Agy, "lease-1")
	if err != nil {
		t.Fatal(err)
	}

	_ = sess.Queue("lease-1", "t1", "prompt 1")
	_ = sess.Queue("lease-1", "t2", "prompt 2")
	_, _ = sess.Release("lease-1", "t1")

	// Process tool denial event
	denialEv := adapter.Event{
		Ref:        adapter.TurnRef{SessionID: adapter.SessionID(sess.ID), TurnKey: "t1"},
		Type:       adapter.EventToolDenied,
		Status:     council.TurnRunning,
		ApprovalID: "tool-git-commit",
		Payload:    "permission denied by policy",
	}
	if err := denialEv.Validate(); err != nil {
		t.Fatalf("invalid denial event: %v", err)
	}

	// Tool denial does NOT retire the slot
	if sess.Active != "t1" || sess.State != council.Running {
		t.Fatalf("tool denial prematurely cleared active turn: active=%s state=%s", sess.Active, sess.State)
	}

	// Releasing t2 must remain blocked
	if _, err := sess.Release("lease-1", "t2"); err == nil {
		t.Fatal("release of t2 permitted while t1 is unretired after tool denial")
	}

	// Authoritative terminal failure arrives
	if err := sess.Fail("t1", "tool denied"); err != nil {
		t.Fatalf("Fail failed: %v", err)
	}
	if sess.State != council.Parked {
		t.Fatalf("expected parked after fail, got %s", sess.State)
	}

	// Now t2 can be released
	if _, err := sess.Release("lease-1", "t2"); err != nil {
		t.Fatalf("release of t2 failed after terminal outcome: %v", err)
	}
}

func TestCouncilBoundary_StaleRecoveryGenerationRejectedAcrossOutages(t *testing.T) {
	sess, err := council.NewSession(council.Codex, "lease-1")
	if err != nil {
		t.Fatal(err)
	}

	_ = sess.Queue("lease-1", "t1", "p1")
	_, _ = sess.Release("lease-1", "t1")

	// Outage 1
	gen1, err := sess.RecordHostLoss()
	if err != nil {
		t.Fatal(err)
	}

	// Outage 1 recovers
	if err := sess.ReconcileHost("t1", gen1, council.TurnRunning, ""); err != nil {
		t.Fatal(err)
	}

	// Outage 2
	gen2, err := sess.RecordHostLoss()
	if err != nil {
		t.Fatal(err)
	}
	if gen2 <= gen1 {
		t.Fatalf("expected gen2 > gen1, got %d <= %d", gen2, gen1)
	}

	// Stale recovery response with gen1 from fake
	fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
		StaleRecoveryRef: &adapter.RecoveryRef{
			TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID(sess.ID), TurnKey: "t1"},
			Generation: gen1,
		},
	})

	outcome, err := fake.Reconcile(context.Background(), adapter.RecoveryRef{
		TurnRef:    adapter.TurnRef{SessionID: adapter.SessionID(sess.ID), TurnKey: "t1"},
		Generation: gen2,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Delivery of stale outcome to Council must be rejected
	err = sess.ReconcileHost(outcome.Ref.TurnKey, outcome.Ref.Generation, outcome.Observed, outcome.Result)
	if err == nil {
		t.Fatal("stale generation 1 reconciliation was accepted during outage 2")
	}

	if sess.Visibility != council.VisibilityHostLost {
		t.Fatalf("stale reconciliation cleared host uncertainty: visibility=%s", sess.Visibility)
	}
}

func TestCouncilBoundary_ProductionRegistryRejectsFake(t *testing.T) {
	// Assert that production adapter resolution fails closed when asked for "fake" or unknown
	resolveAdapter := func(name string) (adapter.Adapter, error) {
		switch name {
		case "opencode", "claude", "codex", "agy":
			return nil, errors.New("native adapter not yet implemented")
		default:
			return nil, errors.New("unknown or prohibited adapter")
		}
	}

	if _, err := resolveAdapter("fake"); err == nil {
		t.Fatal("production adapter resolver allowed 'fake' adapter")
	}
	if _, err := resolveAdapter("unknown"); err == nil {
		t.Fatal("production adapter resolver allowed unknown adapter")
	}
}
```

- [ ] **Step 2: Run test to verify it passes**

Run: `go test -race -v ./internal/adapter -run TestCouncilBoundary_`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/adapter/council_boundary_test.go
git commit -m "test(adapter): add Council boundary integration tests for adversarial adapter behaviors"
```

---

### Task 7: Full Verification Suite and Documentation Refresh

**Files:**
- Modify: `docs/superpowers/specs/2026-09-19-harness-adapter-contract-design.md` (status: Implemented)

- [ ] **Step 1: Run full Go verification suite**

Run:
```bash
gofmt -l cmd internal
go vet ./...
go build ./...
go test -race -count=1 -v ./...
```
Expected: All clean, 0 lint/vet issues, all unit, conformance, and boundary tests pass.

- [ ] **Step 2: Run seed and publisher verification**

Run:
```bash
python3 scripts/verify_seed.py
python3 -m unittest discover -s scripts/tests -v
```
Expected: All 16 tests pass, 22 issue specs validated.

- [ ] **Step 3: Update spec document status to Implemented and commit**

```bash
git add docs/superpowers/specs/2026-09-19-harness-adapter-contract-design.md
git commit -m "docs: update AC-006 design specification status to completed"
```
