# Design Specification: Harness Adapter Contract and Adversarial Fake Adapter (AC-006)

Status: Draft for Review  
Date: 2026-09-19  
Issue: [AC-006 (#6)](https://github.com/CtrlCarlitos/agent-council/issues/6)  
Target Branch: `feat/ac-006-adapter-contract`  

---

## 1. Problem and Architecture Context

Agent Council requires native harness contributors (Claude, Codex, Agy, OpenCode) to execute tasks under explicit, deterministic governance. As established in [ARCHITECTURE.md](file:///home/carlitos/projects/CtrlCarlitos/agent-council/docs/ARCHITECTURE.md), Council is controller-led: the service/Council kernel owns scheduling, leases, authorization, voting, and lifecycle state. Harness adapters are workers, not policy owners.

Different harnesses possess disparate session paradigms, tool approval flows, cancellation semantics, and output formats. A bare subprocess exit code cannot establish success, and a network timeout cannot be presumed a safe retry.

To support live integrations without coupling or fragility, **AC-006** introduces:
1. A typed, native `Adapter` interface and closeable observation `Stream`.
2. Explicit identity separation: logical `SessionID` vs `council.Contributor`, `TurnRef` for execution vs `RecoveryRef` for host loss recovery.
3. Explicit representation of unknown capabilities, token usage, cost, and dispatch/cancellation states.
4. An importable, capability-aware conformance test suite (`internal/adapter/conformance`).
5. A deterministic, scripted adversarial fake (`internal/adapter/adaptertest`) that simulates stalls, duplicate events, tool denials, mid-turn disconnections, and malformed outputs using synchronization gates.
6. Council boundary integration tests proving domain state invariants hold under adversarial adapter behavior.

---

## 2. Package Structure

```
internal/
  council/                         # Core domain kernel (Session, TurnRecord, transitions)
  adapter/
    adapter.go                     # Core Adapter interface
    types.go                       # Identifiers, requests, outcomes, usage, capability types
    stream.go                      # Stream interface and finite buffer implementation

    conformance/
      suite.go                     # Reusable conformance runner and check rules

    adaptertest/
      fake.go                      # Scripted adversarial fake worker and synchronization gates

    conformance_test.go            # Runs conformance suite against the fake (package adapter_test)
    council_boundary_test.go       # Exercises adversarial fake events against Council domain rules
```

* **No Production Coupling**: The fake adapter resides exclusively in `internal/adapter/adaptertest` with test-only constructors. Production registries/factories cannot import or instantiate it.
* **External Test Package**: Test drivers use `package adapter_test` to prevent circular dependencies between `adapter` and subpackages `conformance` / `adaptertest`.

---

## 3. Identifiers, Outcomes, and Types (`internal/adapter/types.go`)

### 3.1 Session, Turn, and Recovery Identities

```go
package adapter

import (
    "time"
    "github.com/CtrlCarlitos/agent-council/internal/council"
)

// SessionID is a Council-allocated unique logical session identity.
// It is distinct from contributor identity ("claude", "opencode", etc.)
// because multiple sessions or clean-room reviewers may share the same contributor model.
type SessionID string

// TurnRef identifies a specific, stable execution assignment within a logical session.
type TurnRef struct {
    SessionID SessionID
    TurnKey   string
}

// RecoveryRef identifies a specific host-loss recovery episode for a turn.
// It is generated when host contact is lost and never replaces TurnRef for ordinary dispatch.
type RecoveryRef struct {
    TurnRef
    Generation uint64
}

// SessionConfig defines approved execution parameters supplied by Council.
type SessionConfig struct {
    WorkspaceRoot string
    Model         string
    Tooling       []string
}

// SessionBinding records the verified binding between Council's logical session
// and the harness's native session/thread identifier.
type SessionBinding struct {
    SessionID       SessionID
    Contributor     council.Contributor
    NativeSessionID string
    Config          SessionConfig
}
```

### 3.2 Explicit Usage and Capability Discovery

```go
// UsageMetric represents an observed metric with explicit availability.
type UsageMetric[T any] struct {
    Value     T
    Available bool
    Estimated bool
}

// ExecutionUsage represents cumulative per-turn resource consumption.
// Duplicate event deliveries must not double-count usage.
type ExecutionUsage struct {
    InputTokens  UsageMetric[int64]
    OutputTokens UsageMetric[int64]
    TotalCostUSD UsageMetric[float64]
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

type ProbeReport struct {
    HarnessVersion string
    Capabilities   AdapterCapabilities
    AvailableModels []string
}
```

**Metric Invariants**:
* If `Available == false`, `Value` is ignored and must not be interpreted as a measured zero.
* If `Available == true`, `InputTokens` and `OutputTokens` must be non-negative; `TotalCostUSD` must be non-negative and finite.
* Any unpopulated or unrecognized capability normalizes strictly to `CapabilityUnknown`.

### 3.3 Structured Outcomes (Dispatch, Cancel, Collect, Reconcile)

```go
type DispatchStatus string

const (
    DispatchAccepted DispatchStatus = "accepted" // Definitively accepted by harness
    DispatchRejected DispatchStatus = "rejected" // Definitively rejected before execution started
    DispatchUnknown  DispatchStatus = "unknown"  // Unacknowledged; transport broke; requires Reconcile
)

type DispatchOutcome struct {
    Ref    TurnRef
    Status DispatchStatus
    Reason string
}

type CancelDisposition string

const (
    CancelRequested       CancelDisposition = "requested"        // Accepted request; turn may still run
    CancelConfirmed       CancelDisposition = "confirmed"        // Authoritative confirmation of termination
    CancelAlreadyTerminal CancelDisposition = "already_terminal" // Turn finished before cancellation arrived
    CancelRejected        CancelDisposition = "rejected"         // Cancellation was rejected
    CancelUnsupported     CancelDisposition = "unsupported"      // Harness does not support mid-turn cancel
    CancelUnknown         CancelDisposition = "unknown"          // Request or ack lost
)

type CancelOutcome struct {
    Ref         TurnRef
    Disposition CancelDisposition
    Reason      string
}

type TurnResult struct {
    Ref         TurnRef
    Status      council.TurnStatus
    Output      string
    RawEvidence []byte
    Usage       ExecutionUsage
    CompletedAt time.Time
}

type ReconciliationStatus string

const (
    ReconciliationReachableActive     ReconciliationStatus = "reachable_active"
    ReconciliationReachableTerminal   ReconciliationStatus = "reachable_terminal"
    ReconciliationDefinitivelyMissing ReconciliationStatus = "definitively_missing"
    ReconciliationUncertain           ReconciliationStatus = "uncertain"
)

type ReconciliationOutcome struct {
    Ref        RecoveryRef
    Reachability council.ExecutionVisibility
    Status     ReconciliationStatus
    Observed   council.TurnStatus
    Result     string
}
```

---

## 4. Observation Stream Contract (`internal/adapter/stream.go`)

### 4.1 Interface & Lifecycle

```go
type Stream interface {
    // Events returns a receive-only channel of ordered execution events.
    Events() <-chan Event

    // Err returns the stable terminal error after the Events channel closes.
    // Returns nil for normal completion, or an explicit error (context.Canceled,
    // context.DeadlineExceeded, ErrBufferOverflow, transport error).
    Err() error

    // Close idempotently stops observation and releases resources.
    // Unblocks producer goroutines immediately without requiring the consumer to drain.
    Close() error
}
```

### 4.2 Event Validation Table

| `Type` | Valid `Status` | `ApprovalID` | Required Meaning |
| :--- | :--- | :--- | :--- |
| `EventProgress` | `TurnRunning`, `TurnCancelling` | `""` | Intermediate progress; cannot authorize terminal state. |
| `EventToolRequested` | `TurnRunning` | Required non-empty | A tool requires operator decision. |
| `EventToolApproved` | `TurnRunning` | Required matching ID | Harness reports tool approved. |
| `EventToolDenied` | `TurnRunning`, `TurnFailed` | Required matching ID | Tool execution denied. |
| `EventTerminal` | `TurnCompleted`, `TurnFailed`, `TurnCancelled`, `TurnInterrupted` | `""` | Authoritative worker outcome. |

### 4.3 Streaming Semantics & Invariants
1. **Buffer Limit**: Default bounded capacity of 64 events.
2. **Payload Size Limit**: Maximum event payload (e.g., 64 KiB) and maximum raw output (e.g., 1 MiB). Exceeding limits returns an explicit error diagnostic rather than silent loss.
3. **Channel Closure != Turn Completion**: An event channel closing simply means the observation subscription ended. Turn completion requires an explicit `EventTerminal` or authoritative `Collect`/`Reconcile`.
4. **Clean Producer Unblocking**: Calling `Close()` cancels an internal subscription context. Producers select on `subCtx.Done()` on every send, ensuring producer goroutines never leak or deadlock if a consumer stops reading.
5. **Slow Consumer Overflow**: If consumer is too slow and the buffer fills, the stream sets `ErrBufferOverflow` and terminates, prompting the caller to fall back to `Collect` or `Reconcile`.

---

## 5. Adapter Interface (`internal/adapter/adapter.go`)

```go
type CreateSessionRequest struct {
    SessionID   SessionID
    Contributor council.Contributor
    Config      SessionConfig
}

type Adapter interface {
    // Probe discovers capabilities, installed version, and inventory.
    Probe(ctx context.Context) (ProbeReport, error)

    // CreateSession initializes a fresh native conversation.
    CreateSession(ctx context.Context, req CreateSessionRequest) (SessionBinding, error)

    // ResumeSession validates and attaches an existing native conversation.
    ResumeSession(ctx context.Context, binding SessionBinding) error

    // Dispatch submits an assignment for execution.
    // If ctx expires during transmission, outcome returns DispatchUnknown with error.
    Dispatch(ctx context.Context, ref TurnRef, prompt string) (DispatchOutcome, error)

    // Observe establishes an event subscription for an active turn.
    Observe(ctx context.Context, ref TurnRef) (Stream, error)

    // Cancel requests cooperative cancellation of an active turn.
    Cancel(ctx context.Context, ref TurnRef) (CancelOutcome, error)

    // Collect retrieves execution outcomes and recorded usage.
    Collect(ctx context.Context, ref TurnRef) (TurnResult, error)

    // Reconcile checks reachability and turn state after host loss.
    Reconcile(ctx context.Context, ref RecoveryRef) (ReconciliationOutcome, error)
}
```

### Context Lifetime Separation Rule
* `ctx` bounds **only the immediate request/call**.
* Cancelling the `Observe(ctx)` context terminates observation, but **never cancels the underlying worker**.
* Timing out during `Dispatch` yields `DispatchUnknown` but **does not kill a turn that the harness may have accepted**.
* Only an explicit call to `Cancel(ctx, ref)` can signal worker cancellation.

---

## 6. Scripted Adversarial Fake (`internal/adapter/adaptertest/fake.go`)

The fake adapter provides scripted, deterministic fault injection:

```go
type ScriptedFaults struct {
    // Dispatch gates
    HoldDispatchAck   chan struct{} // blocks returning Dispatch outcome until closed
    DispatchStatus    DispatchStatus
    DispatchError     error

    // Tool approvals
    AutoDenyTools     map[string]bool // tool names to deny

    // Event faults
    DropStreamEarly   bool
    InjectDuplicates  bool
    MalformedOutput   bool
    StallStream       chan struct{} // holds stream open without events

    // Recovery faults
    StaleRecoveryRef  *RecoveryRef  // injects an old generation or turn key
}
```

* **Execution Tracking**: The fake maintains independent counters: `DispatchesReceived`, `DispatchesStarted`, `CancelsReceived`, `ReconcilesReceived`. Even when `DispatchUnknown` is returned, tests can inspect `DispatchesStarted` to verify whether execution actually started before the failure.
* **No Auto-Repair**: If a stale `RecoveryRef` is configured in `StaleRecoveryRef`, the fake delivers that exact stale reference without silently fixing the generation or turn key.

---

## 7. Conformance Suite & Boundary Tests

### 7.1 Importable Conformance Runner (`internal/adapter/conformance/suite.go`)

Exposes test execution functions:
* `conformance.Check(adapter Adapter) []Violation`: Inspects an adapter against core rules and returns structured violation descriptions without failing the test directly.
* `conformance.Run(t *testing.T, factory FixtureFactory)`: Standard suite runner that invokes `Check` and runs scenario subtests, failing `t` on unexpected violations.

**Scenarios Tested**:
1. **Capabilities**: Unknown fields not converted to true; probe runs without billing.
2. **Session Isolation**: Two sessions created for the same contributor do not collide or overwrite state.
3. **Session Resumption**: Resuming an unknown native ID fails; valid binding attaches cleanly.
4. **Dispatch Acknowledgement**: Normal acceptance, immediate rejection, and unknown status preserve `TurnRef`.
5. **Observation Lifecycle**: Normal completion, transport abort, consumer closing early (producer unblocked), buffer overflow.
6. **Cancellation Semantics**: Distinguishes `CancelRequested` from `CancelConfirmed` and `CancelUnsupported`.
7. **Collection**: Stable repeated collection, pending returns unavailable, malformed output returns structured error.
8. **Usage Hygiene**: Measured zero vs unavailable; non-negative tokens and cost.

### 7.2 Sensitivity Tests (Non-Failing Green Proofs)
To verify the test suite is non-tautological:
* `TestConformance_DetectsEOFAsCompletion`: Passes an intentionally broken fake that maps EOF to completion into `conformance.Check`, asserting that a `"EOF treated as completion"` violation is produced.
* `TestConformance_DetectsRewrittenRecoveryGeneration`: Passes a broken fake that substitutes current generation into `conformance.Check`, asserting that a `"recovery generation substituted"` violation is produced.
* These tests assert that violations are found, keeping test suite execution green while proving sensitivity.

### 7.3 Council Boundary Integration Tests (`internal/adapter/council_boundary_test.go`)
Integrates the fake adapter with `council.Session`:
1. **Same Harness, Different Sessions**: Create 2 sessions for `Claude`; interleaved dispatches and turns stay completely isolated.
2. **Lost Dispatch Acknowledgement**: `DispatchUnknown` leaves prompt in pending or triggers reconciliation before any re-submission.
3. **Cancellation Requested vs Confirmed**: `CancelRequested` does not free `council.Session` slot; contributor stays occupied until confirmed.
4. **Observation Disconnected While Worker Runs**: Downstream subscriber drops, worker finishes in background, subsequent `Collect` retrieves valid output.
5. **Stale Recovery Episode Rejection**: Outage #1 reply delivered during Outage #2 is rejected with `stale or invalid recovery generation`.
6. **Concurrent Interleaved Sessions**: 4 contributor sessions receiving interleaved events; all operations serialized per session, verifying zero cross-talk.
7. **Production Registry Guard**: Attempting to resolve `"fake"` in production adapter factory fails closed.

---

## 8. Definition of Done
1. `internal/adapter/` contract and stream implementation written and verified.
2. `internal/adapter/conformance/suite.go` implemented in ordinary Go files.
3. `internal/adapter/adaptertest/fake.go` implements deterministic scripted gates.
4. Conformance tests pass for conforming fake and detect violations for intentionally broken fakes.
5. Council boundary tests demonstrate full compliance with Council lifecycle invariants.
6. All tests pass with `-race`, clean formatting, and green CI.
