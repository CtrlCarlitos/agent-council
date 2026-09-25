package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ResolveAgyTurnUncertainty records administrative abandonment, not a native
// outcome. The caller must first exclude locally active execution. For a lost
// host the controller takes responsibility for retiring the old execution.
// Native evidence is retained; disposition, turn, session, intent and journal
// commit together. Replacement work requires a separate queue and release.
func (s *Store) ResolveAgyTurnUncertainty(ctx context.Context, opID, lease string, generation uint64, expectedVersion int64, ref ExecutionRef, disposition, reason string) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" || strings.TrimSpace(lease) == "" || generation == 0 {
		return OperationReceipt{}, errors.New("op_id, controller lease and generation are required")
	}
	if err := ref.validate(); err != nil {
		return OperationReceipt{}, err
	}
	if expectedVersion <= 0 {
		return OperationReceipt{}, ErrInvalidExpectedVersion
	}
	if disposition != "abandoned" || strings.TrimSpace(reason) == "" {
		return OperationReceipt{}, errors.New("disposition must be abandoned, with a reason confirming retirement of the old execution")
	}
	reason = SanitizeText(reason)
	const command = "resolve_agy_turn_uncertain"
	fp := computeFingerprint(command, ref.SessionID, ref.TurnKey, ref.AttemptID, disposition, reason, fmt.Sprint(generation))
	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()
	gen, err := authorizeSessionController(ctx, tx.Tx(), ref.SessionID, lease, true)
	if err != nil {
		return OperationReceipt{}, err
	}
	if gen != generation {
		return OperationReceipt{}, ErrGenerationMismatch
	}
	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, lease, command, fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}
	runID := runIDOfSession(ctx, tx.Tx(), ref.SessionID)
	if err := requireRunConnected(ctx, tx.Tx(), runID); err != nil {
		return OperationReceipt{}, err
	}
	if _, err := resolveExecutionRef(ctx, tx.Tx(), ref); err != nil {
		return OperationReceipt{}, err
	}
	var version int64
	var contributor, lifecycle, runLifecycle, controllerStatus string
	var active, recovery sql.NullString
	err = tx.Tx().QueryRowContext(ctx, `SELECT s.row_version, s.contributor, s.lifecycle, r.lifecycle, s.controller_status, s.active_key, s.recovery_context FROM sessions s JOIN runs r ON r.run_id=s.run_id WHERE s.session_id=?`, ref.SessionID).Scan(&version, &contributor, &lifecycle, &runLifecycle, &controllerStatus, &active, &recovery)
	if err != nil {
		return OperationReceipt{}, err
	}
	if version != expectedVersion {
		return OperationReceipt{}, ErrStaleUpdate
	}
	if lifecycle == "archived" || runLifecycle == "archived" {
		return OperationReceipt{}, ErrSessionArchived
	}
	if controllerStatus != "connected" {
		return OperationReceipt{}, ErrControllerDisconnected
	}
	if contributor != "agy" {
		return OperationReceipt{}, errors.New("session is not an agy contributor")
	}
	if (active.Valid && active.String != "" && active.String != ref.TurnKey) || (active.String != ref.TurnKey && recovery.String != ref.TurnKey) {
		return OperationReceipt{}, errors.New("attempt is not the active or recovering Council turn")
	}
	var status string
	if err := tx.Tx().QueryRowContext(ctx, `SELECT status FROM turns WHERE session_id=? AND turn_key=?`, ref.SessionID, ref.TurnKey).Scan(&status); err != nil {
		return OperationReceipt{}, err
	}
	if status != "running" && status != "cancelling" {
		return OperationReceipt{}, ErrConflictingTerminalOutcome
	}
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	res, err := tx.Tx().ExecContext(ctx, `UPDATE agy_turn_attempts SET uncertainty_disposition=?, transition_version=transition_version+1, updated_at=? WHERE attempt_id=? AND session_id=? AND turn_key=? AND terminal=0 AND observed_status='uncertain' AND uncertainty_disposition IS NULL`, disposition, stamp, ref.AttemptID, ref.SessionID, ref.TurnKey)
	if err != nil {
		return OperationReceipt{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return OperationReceipt{}, errors.New("attempt is not unresolved and uncertain")
	}
	result := "controller abandoned uncertain attempt (native outcome unknown): " + reason
	if _, err := tx.Tx().ExecContext(ctx, `UPDATE turns SET status='interrupted', result=?, completed_at=? WHERE session_id=? AND turn_key=?`, result, stamp, ref.SessionID, ref.TurnKey); err != nil {
		return OperationReceipt{}, err
	}
	if _, err := tx.Tx().ExecContext(ctx, `UPDATE dispatch_intents SET phase='resolved', updated_at=? WHERE session_id=? AND turn_key=?`, stamp, ref.SessionID, ref.TurnKey); err != nil {
		return OperationReceipt{}, err
	}
	if _, err := tx.Tx().ExecContext(ctx, `UPDATE sessions SET state='parked', active_key=NULL, visibility='reachable', recovery_context=NULL, active_recovery_gen=0, row_version=?, updated_at=? WHERE session_id=?`, version+1, stamp, ref.SessionID); err != nil {
		return OperationReceipt{}, err
	}
	payload, err := json.Marshal(struct {
		AttemptID   string `json:"attempt_id"`
		Disposition string `json:"disposition"`
		Reason      string `json:"reason"`
		Generation  uint64 `json:"controller_generation"`
	}{ref.AttemptID, disposition, reason, gen})
	if err != nil {
		return OperationReceipt{}, err
	}
	receipt := OperationReceipt{OpID: opID, CommandType: command, SessionID: ref.SessionID, TurnKey: ref.TurnKey, CommittedVersion: version + 1, CreatedAt: now, Payload: string(payload)}
	if err := recordJournalEntry(tx.Tx(), opID, command, fp, runID, ref.SessionID, ref.TurnKey, "agy_turn_uncertainty_resolved", receipt, lease); err != nil {
		return OperationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
