# Design Specification: Harness Adapter Contract and Adversarial Fake Adapter (AC-006)

Status: Amended Specification  
Date: 2026-09-19  
Issue: [AC-006 (#6)](https://github.com/CtrlCarlitos/agent-council/issues/6)  
Target Branch: `feat/ac-006-adapter-contract`  

---

## 1. Problem and Architecture Context

Agent Council requires native harness contributors (Claude, Codex, Agy, OpenCode) to execute tasks under explicit, deterministic governance. As established in [ARCHITECTURE.md](../../ARCHITECTURE.md), Council is controller-led: the service/Council kernel owns scheduling, leases, authorization, voting, and lifecycle state. Harness adapters are workers, not policy owners.

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
    stream.go                      # Stream interface, Event envelope, and finite buffer implementation

    conformance/
      suite.go                     # Reusable conformance runner, Fixture interface, and check rules

    adaptertest/
      fake.go                      # Scripted adversarial fake worker and synchronization gates

    conformance_test.go            # Runs conformance suite against the fake (package adapter_test)
    council_boundary_test.go       # Exercises adversarial fake events against Council domain rules
```

* **No Production Coupling**: The fake adapter resides exclusively in `internal/adapter/adaptertest` with test-only constructors. Production registries/factories cannot import or instantiate it. Conformance tests verify that production resolver fails closed and does not register the fake.
* **External Test Package**: Test drivers use `package adapter_test` to prevent circular dependencies between `adapter` and subpackages `conformance` / `adaptertest`.

---

## 3. Identifiers, Outcomes, and Types (`internal/adapter/types.go`)

### 3.1 Session, Turn, and Recovery Identities

```go
package adapter

import (
    "errors"
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

// ErrSessionCreationUncertain indicates native conversation creation status could not be verified
// (e.g. transport broke after sending create request). Automatic recreation is blocked.
type ErrSessionCreationUncertain struct {
    SessionID       SessionID
    Contributor     council.Contributor
    PartialNativeID string
    Err             error
}

func (e *ErrSessionCreationUncertain) Error() string {
    return "session creation uncertain: " + e.Err.Error()
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
    HarnessVersion  UsageMetric[string]
    Capabilities    AdapterCapabilities
    ModelInventory  UsageMetric[[]string]
}
```

**Metric & Probe Invariants**:
* If `Available == false`, `Value` is ignored and must not be interpreted as a measured zero or known-empty list.
* Distinguish an unavailable model inventory (`ModelInventory.Available == false`) from a known-empty inventory (`ModelInventory.Available == true`, `len(Value) == 0`).
* Distinguish an unknown harness version (`HarnessVersion.Available == false`) from a verified string (`HarnessVersion.Available == true`).
* If `Available == true`, `InputTokens` and `OutputTokens` must be non-negative; `TotalCostUSD` must be non-negative and finite.
* Any unpopulated or unrecognized capability normalizes strictly to `CapabilityUnknown`.

### 3.3 Structured Outcomes (Dispatch, Cancel, Collect, Reconcile)

```go
type DispatchStatus string

const (
    DispatchAccepted DispatchStatus = "accepted" // Definitively accepted by harness for execution
    DispatchRejected DispatchStatus = "rejected" // Definitively not accepted for execution
    DispatchUnknown  DispatchStatus = "unknown"  // Unacknowledged; transport broke; preserves execution reservation
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

type ResultStatus string

const (
    ResultPending     ResultStatus = "pending"     // Turn still running, result not yet available
    ResultAvailable   ResultStatus = "available"   // Finalized result available (even if output string is empty)
    ResultUnavailable ResultStatus = "unavailable" // Harness cannot supply output
    ResultMalformed   ResultStatus = "malformed"   // Output violated schema/structured format
    ResultFailed      ResultStatus = "failed"      // Retrieval failed
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
```

#### Reconciliation Validation Matrix

| `Reachability` | `Status` | Allowed `Observed` | Valid? | Semantics |
| :--- | :--- | :--- | :--- | :--- |
| `VisibilityReachable` | `ReconciliationReachableActive` | `TurnRunning`, `TurnCancelling` | **Yes** | Host reachable; turn is currently active/running. |
| `VisibilityReachable` | `ReconciliationReachableTerminal` | `TurnCompleted`, `TurnCancelled`, `TurnFailed`, `TurnInterrupted` | **Yes** | Host reachable; turn reached terminal state. |
| `VisibilityReachable` | `ReconciliationDefinitivelyMissing` | `TurnFailed` | **Yes** | Authoritative report proves task was never scheduled/queued. |
| `VisibilityHostLost` | `ReconciliationUncertain` | (any / `""`) | **Yes** | Probe failed or lookup was inconclusive; uncertainty remains. |
| `VisibilityReachable` | `ReconciliationReachableActive` | `TurnCompleted` | **No** | Contradiction: active status cannot pair with completed turn. |
| `VisibilityReachable` | `ReconciliationReachableTerminal` | `TurnRunning` | **No** | Contradiction: terminal status cannot pair with running turn. |

* **Definitive Missing Evidence**: Only an authoritative query response from the harness explicitly confirming the task identifier does not exist and was never queued qualifies as `ReconciliationDefinitivelyMissing`. An empty search result or missing item in a list that could omit in-flight or queued work must strictly remain `ReconciliationUncertain`.

---

## 4. Event Envelope and Observation Stream Contract (`internal/adapter/stream.go`)

### 4.1 Event Envelope

```go
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
    ApprovalID string            // Stable identifier for tool approval requests/decisions
    Payload    string            // Bounded payload text/data
    Usage      ExecutionUsage    // Cumulative per-turn snapshot
    Timestamp  time.Time
}
```

### 4.2 Stream Interface

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

### 4.3 Event Validation Matrix

| `Type` | Valid `Status` | `ApprovalID` | Required Meaning |
| :--- | :--- | :--- | :--- |
| `EventProgress` | `TurnRunning`, `TurnCancelling` | `""` | Intermediate progress; cannot authorize terminal state. |
| `EventToolRequested` | `TurnRunning` | Required non-empty | A tool requires operator decision. |
| `EventToolApproved` | `TurnRunning` | Required matching ID | Harness reports tool approved. |
| `EventToolDenied` | `TurnRunning` | Required matching ID | Tool execution denied. Does NOT retire turn slot. |
| `EventTerminal` | `TurnCompleted`, `TurnFailed`, `TurnCancelled`, `TurnInterrupted` | `""` | Authoritative worker outcome. |

### 4.4 Streaming Semantics & Invariants
1. **Tool Denial Semantics**: An `EventToolDenied` event records the denial and any associated limitation; it does **not**, by itself, retire the turn or release its execution slot. Authoritative completion requires an `EventTerminal` or an authoritative `Collect`/`Reconcile` outcome.
2. **Close Semantics**: `Close()` returns after subscription-owned delivery resources have stopped. It is safe under concurrent and repeated calls and requires no channel draining by the consumer. The final observation error (`Err()`) is published and stable before the `Events()` channel closes.
3. **Native Output Continuation**: Ending a subscriber must not abandon native output handling in a way that blocks the still-running worker. The adapter continues consuming or safely discarding unread process stream output in the background.
4. **Usage Snapshot Ordering**: Event usage represents cumulative per-turn snapshots. Callers track monotonically increasing token totals; older snapshots arriving out of order must not overwrite a newer recorded total. Final usage reported by `Collect` is authoritative.
5. **Buffer & Payload Limits**: Default bounded capacity of 64 events. Maximum event payload (64 KiB) and maximum raw output (1 MiB). Exceeding limits produces an explicit diagnostic error rather than silent truncation.
6. **Channel Closure != Turn Completion**: An event channel closing simply signals that observation has ended. Turn completion requires an explicit `EventTerminal` or authoritative `Collect`/`Reconcile`.
7. **Buffer Overflow**: If consumer cannot keep up and the buffer fills, the stream sets `ErrBufferOverflow` and terminates, prompting the caller to fall back to `Collect` or `Reconcile`.

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

### 5.1 Context Lifetime Separation Rule
* `ctx` bounds **only the immediate request/call**.
* Cancelling `Observe(ctx)` terminates observation, but **never cancels the underlying worker**.
* Timing out during `Dispatch` yields `DispatchUnknown` but **does not kill a turn that the harness may have accepted**.
* Timing out during `CreateSession` or `ResumeSession` yields an error but **does not delete the native session**.
* Only an explicit call to `Cancel(ctx, ref)` can signal worker cancellation.

### 5.2 Dispatch Reservation Rule
* Once Council releases a turn for dispatch, an unknown acceptance outcome (`DispatchUnknown`) **preserves that turn's execution reservation**.
* No further turn may be released to that logical session until an authoritative observation resolves whether the original execution was accepted and what happened to it.
* Neither an error nor a missing lookup result automatically permits resubmission.
* Council obtains the recovery reference for this uncertainty by calling `Session.RecordHostLoss()`, which initiates the host-loss condition and allocates the recovery generation; Council supplies this `RecoveryRef` to `Reconcile()`. The adapter receives this reference and must never invent a generation or attach the current generation to a delayed response.

### 5.3 Session Creation & Resumption Invariants
* A repeated logical `SessionID` with identical `SessionConfig` must idempotently return the existing `SessionBinding`.
* A repeated logical `SessionID` with changed `SessionConfig` must be rejected.
* If creation acknowledgement is lost, `ErrSessionCreationUncertain` is returned, blocking automatic recreation until the binding is resolved.

---

## 6. Scripted Adversarial Fake (`internal/adapter/adaptertest/fake.go`)

The fake adapter provides scripted, deterministic fault injection:

```go
type DispatchState struct {
    Received bool
    Accepted bool
    Started  bool
}

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

* **Per-Turn Execution Tracking**: The fake maintains `map[TurnRef]*DispatchState`. Even when `DispatchUnknown` is returned, tests can inspect `DispatchState.Started` to verify whether execution actually started before the failure.
* **No Auto-Repair**: If a stale `RecoveryRef` is configured in `StaleRecoveryRef`, the fake delivers that exact stale reference without silently fixing the generation or turn key.
* **Synchronization Gates**: Faults are held or released via `chan struct{}` channels, ensuring zero timing-dependent sleep assertions in tests.

---

## 7. Conformance Suite & Boundary Tests

### 7.1 Importable Conformance Runner (`internal/adapter/conformance/suite.go`)

```go
type Fixture interface {
    Adapter() adapter.Adapter
    EventsObserved() []adapter.Event
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
```

Exposes test execution functions:
* `conformance.Check(ctx context.Context, fixture Fixture, scenario Scenario) []Violation`: Evaluates an adapter against a specific scenario and returns structured violations without failing `t` directly.
* `conformance.Run(t *testing.T, factory FixtureFactory)`: Standard suite runner that invokes `Check` across all scenarios and reports failures for unexpected violations.
* **Capability-Aware**: Adapters reporting a capability as unsupported are evaluated against the unsupported-behavior contract (e.g., reporting unsupported honestly without claiming termination). They are not failed for lacking the capability.

### 7.2 Sensitivity Tests (Non-Failing Green Proofs)
To verify the test suite is non-tautological:
* `TestConformance_DetectsEOFAsCompletion`: Passes a deliberately broken fake (which translates EOF to turn completion) into `conformance.Check`, asserting that `ViolationEOFAsCompletion` is returned.
* `TestConformance_DetectsRewrittenRecoveryGeneration`: Passes a broken fake (which substitutes current generation into recovery responses) into `conformance.Check`, asserting that `ViolationGenerationSubstituted` is returned.
* These tests assert that the specific violation code is returned, verifying test sensitivity while keeping the test suite green.

### 7.3 Council Boundary Integration Tests (`internal/adapter/council_boundary_test.go`)
Integrates the fake adapter with `council.Session`:
1. **Same Harness, Different Sessions**: Create 2 sessions for `Claude`; interleaved dispatches and turns stay completely isolated without cross-talk.
2. **Lost Dispatch Acknowledgement**: Accept dispatch, withhold ack, leave turn not yet started, and attempt to release a different turn. The second release must remain blocked; the original reservation is preserved.
3. **Cancellation Requested vs Confirmed**: `CancelRequested` does not free `council.Session` slot; contributor stays occupied until `CancelConfirmed`.
4. **Observation Disconnected While Worker Runs**: Downstream subscriber drops, worker finishes in background, subsequent `Collect` retrieves valid output.
5. **Tool Denial Without Premature Retirement**: Tool denial is recorded as diagnostic event; turn execution slot remains occupied until authoritative completion.
6. **Concurrent Repeated Close**: Concurrent `Close()` calls during active event generation do not leak goroutines, panic on closed channel, or block producer.
7. **Duplicate / Out-of-Order Usage Delivery**: Older cumulative usage snapshots arriving after newer snapshots do not overwrite the recorded total.
8. **Stale Recovery Episode Rejection**: Outage #1 reply delivered during Outage #2 is rejected with `stale or invalid recovery generation`.
9. **Concurrent Interleaved Sessions**: 4 contributor sessions receiving interleaved events; all operations serialized per session, verifying zero cross-talk.
10. **Production Registry Guard**: Test asserts production adapter factory fails closed when asked for `"fake"` or unknown adapter, and build verification confirms production packages do not import `internal/adapter/adaptertest`.

---

## 8. Definition of Done
1. `internal/adapter/` contract and stream implementation written and verified.
2. `internal/adapter/conformance/suite.go` implemented in ordinary Go files.
3. `internal/adapter/adaptertest/fake.go` implements deterministic scripted gates.
4. Conformance tests pass for conforming fake and detect violations for intentionally broken fakes.
5. Council boundary tests demonstrate compliance with the enumerated lifecycle and conformance cases (keeping fixture evidence strictly distinct from live integration evidence).
6. All tests pass with `-race`, clean formatting, and green CI.
