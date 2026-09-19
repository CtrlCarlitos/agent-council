package adapter

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// SessionID represents a logical session identity allocated by Council.
// It is distinct from council.Contributor (multiple sessions may bind to the same contributor).
type SessionID string

// TurnRef uniquely identifies a turn within a logical session.
type TurnRef struct {
	SessionID SessionID
	TurnKey   string
}

// Validate ensures TurnRef contains non-empty, non-whitespace identifiers.
func (r TurnRef) Validate() error {
	if strings.TrimSpace(string(r.SessionID)) == "" {
		return errors.New("empty session id")
	}
	if strings.TrimSpace(r.TurnKey) == "" {
		return errors.New("empty turn key")
	}
	return nil
}

// RecoveryRef identifies a specific host-loss recovery episode for a turn.
// Generation distinguishes separate outages occurring during the same turn.
type RecoveryRef struct {
	TurnRef
	Generation uint64
}

// Validate ensures TurnRef is valid and Generation is positive.
func (r RecoveryRef) Validate() error {
	if err := r.TurnRef.Validate(); err != nil {
		return err
	}
	if r.Generation == 0 {
		return errors.New("recovery generation must be positive")
	}
	return nil
}

// SessionConfig provides parameters for initializing a native harness conversation.
type SessionConfig struct {
	WorkspaceRoot string
	Model         string
	Tooling       []string
}

// SessionBinding links a Council logical session to a native harness runtime session.
type SessionBinding struct {
	SessionID       SessionID
	Contributor     council.Contributor
	NativeSessionID string
	Config          SessionConfig
}

// ErrSessionCreationUncertain is returned when CreateSession timed out or encountered an
// uncertain transport error where native resources may have been partially allocated.
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

// UsageMetric reports a measured or estimated numeric/string quantity with explicit availability.
type UsageMetric[T any] struct {
	Value     T
	Available bool
	Estimated bool
}

// ExecutionUsage captures token counts and financial cost for a turn execution.
type ExecutionUsage struct {
	InputTokens  UsageMetric[int64]
	OutputTokens UsageMetric[int64]
	TotalCostUSD UsageMetric[float64]
}

// Validate ensures available usage metrics are non-negative and finite.
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

// ErrUnsupportedCapability is returned when an operation requires a capability that the adapter reports unsupported.
var ErrUnsupportedCapability = errors.New("unsupported capability")

// CapabilityStatus describes the level of support for an adapter capability.
type CapabilityStatus string

const (
	CapabilitySupported   CapabilityStatus = "supported"
	CapabilityUnsupported CapabilityStatus = "unsupported"
	CapabilityUnknown     CapabilityStatus = "unknown"
)

// AdapterCapabilities defines feature support exposed by an adapter.
type AdapterCapabilities struct {
	SessionResumption    CapabilityStatus
	MidTurnCancellation  CapabilityStatus
	ToolApprovalRouting  CapabilityStatus
	StreamingObservation CapabilityStatus
	StructuredOutput     CapabilityStatus
}

// Normalize ensures capability statuses conform to known constants, mapping unrecognized values to CapabilityUnknown.
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

// ProbeReport details harness version, capabilities, and discovered models.
type ProbeReport struct {
	HarnessVersion UsageMetric[string]
	Capabilities   AdapterCapabilities
	ModelInventory UsageMetric[[]string]
}

// DispatchStatus describes whether a dispatch request was accepted into execution.
type DispatchStatus string

const (
	DispatchAccepted DispatchStatus = "accepted"
	DispatchRejected DispatchStatus = "rejected"
	DispatchUnknown  DispatchStatus = "unknown"
)

// DispatchOutcome captures the result of dispatching a turn to an adapter.
type DispatchOutcome struct {
	Ref    TurnRef
	Status DispatchStatus
	Reason string
}

// CancelDisposition describes how an adapter handled a cancellation request.
type CancelDisposition string

const (
	CancelRequested       CancelDisposition = "requested"
	CancelConfirmed       CancelDisposition = "confirmed"
	CancelAlreadyTerminal CancelDisposition = "already_terminal"
	CancelRejected        CancelDisposition = "rejected"
	CancelUnsupported     CancelDisposition = "unsupported"
	CancelUnknown         CancelDisposition = "unknown"
)

// CancelOutcome captures the result of requesting mid-turn cancellation.
type CancelOutcome struct {
	Ref         TurnRef
	Disposition CancelDisposition
	Reason      string
}

// ResultStatus describes the readiness and integrity of a turn result.
type ResultStatus string

const (
	ResultPending     ResultStatus = "pending"
	ResultAvailable   ResultStatus = "available"
	ResultUnavailable ResultStatus = "unavailable"
	ResultMalformed   ResultStatus = "malformed"
	ResultFailed      ResultStatus = "failed"
)

// TurnResult records the final outcome and artifacts of a turn execution.
type TurnResult struct {
	Ref          TurnRef
	Status       council.TurnStatus
	ResultStatus ResultStatus
	Output       string
	RawEvidence  []byte
	Usage        ExecutionUsage
	CompletedAt  time.Time
}

// ReconciliationStatus indicates the recovery outcome after a host loss event.
type ReconciliationStatus string

const (
	ReconciliationReachableActive     ReconciliationStatus = "reachable_active"
	ReconciliationReachableTerminal   ReconciliationStatus = "reachable_terminal"
	ReconciliationDefinitivelyMissing ReconciliationStatus = "definitively_missing"
	ReconciliationUncertain           ReconciliationStatus = "uncertain"
)

// ReconciliationOutcome reports findings from probing a disconnected/recovered turn.
type ReconciliationOutcome struct {
	Ref          RecoveryRef
	Reachability council.ExecutionVisibility
	Status       ReconciliationStatus
	Observed     council.TurnStatus
	Result       string
}

// Validate verifies consistency between reachability, status, and observed turn status.
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
