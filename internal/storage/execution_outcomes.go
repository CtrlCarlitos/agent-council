package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// Service-owned evidence persistence for accepted executions (AC-004 §6).
//
// Authority to make a new decision is separate from authority to record
// evidence for an accepted execution. These operations authorize by the
// complete persisted execution identity — session, turn, and attempt —
// validated against the recorded release/dispatch-intent inside the write
// transaction. They perform no controller-lease comparison, accept no
// caller-authored internal-authority flag, and are never exposed as HTTP
// operations: the execution supervisor and authorized cancellation and
// reconciliation handlers call them internally after obtaining adapter
// evidence.

// ErrWrongExecutionAttempt reports an execution reference whose attempt
// identity does not match the persisted accepted execution.
var ErrWrongExecutionAttempt = errors.New("execution reference attempt mismatch")

// ExecutionRef identifies the original accepted execution by its complete
// persisted identity.
type ExecutionRef struct {
	SessionID string
	TurnKey   string
	AttemptID string
}

func (r ExecutionRef) validate() error {
	if strings.TrimSpace(r.SessionID) == "" || strings.TrimSpace(r.TurnKey) == "" || strings.TrimSpace(r.AttemptID) == "" {
		return errors.New("execution reference requires session, turn, and attempt identity")
	}
	return nil
}

// resolveExecutionRef loads and validates the persisted accepted execution
// for ref inside a transaction, returning its issuing controller
// generation. The attempt identity must match exactly: looking up whatever
// attempt currently occupies the keys is not validation of the original
// reference.
func resolveExecutionRef(ctx context.Context, q rowQueryer, ref ExecutionRef) (issuingGeneration uint64, err error) {
	if err := ref.validate(); err != nil {
		return 0, err
	}
	var attemptID string
	var gen uint64
	err = q.QueryRowContext(ctx, `SELECT attempt_id, issuing_controller_generation FROM dispatch_intents
		WHERE session_id = ? AND turn_key = ?;`, ref.SessionID, ref.TurnKey).Scan(&attemptID, &gen)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("no accepted execution for %s/%s", ref.SessionID, ref.TurnKey)
	}
	if err != nil {
		return 0, fmt.Errorf("query accepted execution: %w", err)
	}
	if attemptID != ref.AttemptID {
		return 0, fmt.Errorf("%w: presented %q, persisted %q", ErrWrongExecutionAttempt, ref.AttemptID, attemptID)
	}
	return gen, nil
}

// RecordObservedExecutionOutcome persists the verified terminal outcome of
// an accepted execution under its original execution identity. Issuing
// generation is read from the accepted release record — never inferred
// from the current controller. Terminal-conflict and exact-duplicate
// semantics match the controller-path operation.
func (s *Store) RecordObservedExecutionOutcome(ctx context.Context, opID string, ref ExecutionRef, terminalStatus council.TurnStatus, rawResult string) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return OperationReceipt{}, errors.New("empty operation id")
	}

	var statusStr string
	switch terminalStatus {
	case council.TurnCompleted:
		statusStr = "completed"
	case council.TurnCancelled:
		statusStr = "cancelled"
	case council.TurnFailed:
		statusStr = "failed"
	case council.TurnInterrupted:
		statusStr = "interrupted"
	default:
		return OperationReceipt{}, fmt.Errorf("invalid terminal status: %s", terminalStatus)
	}

	sanitizedResult := SanitizeText(rawResult)
	fp := computeFingerprint("record_observed_execution_outcome", ref.SessionID, ref.TurnKey, ref.AttemptID, statusStr, sanitizedResult)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	issuingGeneration, err := resolveExecutionRef(ctx, tx.Tx(), ref)
	if err != nil {
		return OperationReceipt{}, err
	}

	// Idempotency for the evidence operation itself.
	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, "", "record_observed_execution_outcome", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, lifecycle, visibility, state string
	var activeKey sql.NullString
	var recoveryContext sql.NullString
	var currentVer int64
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, s.row_version, s.lifecycle, s.visibility, s.state, s.active_key, s.recovery_context
FROM sessions s
WHERE s.session_id = ?;`, ref.SessionID).Scan(&runID, &currentVer, &lifecycle, &visibility, &state, &activeKey, &recoveryContext)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query session: %w", err)
	}
	if lifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}

	var existingTurnStatus, existingTurnResult string
	err = tx.Tx().QueryRowContext(ctx, "SELECT status, result FROM turns WHERE session_id = ? AND turn_key = ?;", ref.SessionID, ref.TurnKey).
		Scan(&existingTurnStatus, &existingTurnResult)
	if err == sql.ErrNoRows {
		return OperationReceipt{}, fmt.Errorf("turn %s not found", ref.TurnKey)
	}
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query turn: %w", err)
	}

	isTerminal := func(st string) bool {
		return st == "completed" || st == "cancelled" || st == "failed" || st == "interrupted"
	}

	if isTerminal(existingTurnStatus) {
		if existingTurnStatus == statusStr && existingTurnResult == sanitizedResult {
			receipt := OperationReceipt{
				OpID:             opID,
				CommandType:      "record_observed_execution_outcome",
				SessionID:        ref.SessionID,
				TurnKey:          ref.TurnKey,
				CommittedVersion: currentVer,
				CreatedAt:        time.Now().UTC(),
				Payload:          statusStr + ":duplicate",
			}
			if err := recordJournalEntry(tx.Tx(), opID, "record_observed_execution_outcome", fp, runID, ref.SessionID, ref.TurnKey, "turn_"+statusStr+"_duplicate", receipt, ""); err != nil {
				return OperationReceipt{}, err
			}
			if err := tx.Commit(); err != nil {
				return OperationReceipt{}, err
			}
			return receipt, nil
		}
		return OperationReceipt{}, fmt.Errorf("%w: turn %s is already terminal (%s), conflicting with %s", ErrConflictingTerminalOutcome, ref.TurnKey, existingTurnStatus, statusStr)
	}

	if visibility == "host_lost" {
		if (!activeKey.Valid || activeKey.String != ref.TurnKey) && (!recoveryContext.Valid || recoveryContext.String != ref.TurnKey) {
			return OperationReceipt{}, fmt.Errorf("observed outcome does not match active turn or recovery context in lost host: %s", ref.TurnKey)
		}
	} else {
		if state != "running" || !activeKey.Valid || activeKey.String != ref.TurnKey {
			return OperationReceipt{}, fmt.Errorf("observed outcome does not match active turn %v", activeKey)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	newVer := currentVer + 1

	if _, err := tx.Tx().ExecContext(ctx, `
UPDATE turns SET status = ?, result = ?, completed_at = ?
WHERE session_id = ? AND turn_key = ?;`, statusStr, sanitizedResult, now, ref.SessionID, ref.TurnKey); err != nil {
		return OperationReceipt{}, fmt.Errorf("update turn terminal: %w", err)
	}
	if _, err := tx.Tx().ExecContext(ctx, `
UPDATE dispatch_intents SET phase = 'resolved', updated_at = ?
WHERE session_id = ? AND turn_key = ?;`, now, ref.SessionID, ref.TurnKey); err != nil {
		return OperationReceipt{}, fmt.Errorf("resolve dispatch intent: %w", err)
	}

	if visibility == "host_lost" {
		if _, err := tx.Tx().ExecContext(ctx, `
UPDATE sessions SET active_key = NULL, state = 'parked', recovery_context = ?, row_version = ?, updated_at = ?
WHERE session_id = ?;`, ref.TurnKey, newVer, now, ref.SessionID); err != nil {
			return OperationReceipt{}, fmt.Errorf("park session retaining recovery context: %w", err)
		}
	} else {
		if _, err := tx.Tx().ExecContext(ctx, `
UPDATE sessions SET active_key = NULL, state = 'parked', row_version = ?, updated_at = ?
WHERE session_id = ?;`, newVer, now, ref.SessionID); err != nil {
			return OperationReceipt{}, fmt.Errorf("park session on observed outcome: %w", err)
		}
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "record_observed_execution_outcome",
		SessionID:        ref.SessionID,
		TurnKey:          ref.TurnKey,
		CommittedVersion: newVer,
		CreatedAt:        time.Now().UTC(),
		Payload:          fmt.Sprintf("%s:issued_by_generation=%d", statusStr, issuingGeneration),
	}
	if err := recordJournalEntry(tx.Tx(), opID, "record_observed_execution_outcome", fp, runID, ref.SessionID, ref.TurnKey, "turn_"+statusStr+"_observed", receipt, ""); err != nil {
		return OperationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// RecordObservedDispatchAcknowledgement records a dispatch observation
// (acknowledgement or uncertainty) for an accepted execution under its
// execution reference; it never depends on the issuing controller's lease.
func (s *Store) RecordObservedDispatchAcknowledgement(ctx context.Context, opID string, ref ExecutionRef, phase string) (OperationReceipt, error) {
	switch phase {
	case "receipt_acknowledged", "acceptance_unknown":
	default:
		return OperationReceipt{}, fmt.Errorf("invalid dispatch observation phase %q", phase)
	}
	if strings.TrimSpace(opID) == "" {
		return OperationReceipt{}, errors.New("empty operation id")
	}
	fp := computeFingerprint("record_dispatch_observation", ref.SessionID, ref.TurnKey, ref.AttemptID, phase)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	issuingGeneration, err := resolveExecutionRef(ctx, tx.Tx(), ref)
	if err != nil {
		return OperationReceipt{}, err
	}

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, "", "record_dispatch_observation", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID, currentPhase string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT s.run_id, di.phase
FROM sessions s
JOIN dispatch_intents di ON di.session_id = s.session_id AND di.turn_key = ?
WHERE s.session_id = ?;`, ref.TurnKey, ref.SessionID).Scan(&runID, &currentPhase)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query dispatch intent: %w", err)
	}
	if currentPhase == "resolved" {
		return OperationReceipt{}, errors.New("dispatch intent already resolved")
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Tx().ExecContext(ctx, `
UPDATE dispatch_intents SET phase = ?, updated_at = ?
WHERE session_id = ? AND turn_key = ?;`, phase, now, ref.SessionID, ref.TurnKey); err != nil {
		return OperationReceipt{}, fmt.Errorf("update dispatch intent phase: %w", err)
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "record_dispatch_observation",
		SessionID:        ref.SessionID,
		TurnKey:          ref.TurnKey,
		CommittedVersion: 0,
		CreatedAt:        time.Now().UTC(),
		Payload:          fmt.Sprintf("%s:issued_by_generation=%d", phase, issuingGeneration),
	}
	if err := recordJournalEntry(tx.Tx(), opID, "record_dispatch_observation", fp, runID, ref.SessionID, ref.TurnKey, "dispatch_observed", receipt, ""); err != nil {
		return OperationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
