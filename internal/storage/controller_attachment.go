package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Run-scoped controller attachment (AC-004 §7): connection episodes are
// identity-tracked per generation; disconnect never rotates the lease and
// reconnect never creates a second controller. Session controller_status
// columns are compatibility projections updated transactionally.

// ConnectReceipt returns the attachment episode established by a connect.
type ConnectReceipt struct {
	OperationReceipt
	AttachmentID string
	// AttachmentRev is the authoritative generation-scoped attachment
	// revision: episodes are ordered by it within one controller
	// generation, so a delayed publication of an older episode cannot
	// overwrite a newer one. It does not order across generations.
	AttachmentRev uint64
}

func newAttachmentID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate attachment id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// projectSessionConnection updates every session's controller_status
// compatibility projection with row_version bumps, inside the caller's
// transaction.
func projectSessionConnection(ctx context.Context, tx *sql.Tx, runID, status string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := tx.ExecContext(ctx, `
UPDATE sessions SET controller_status = ?, row_version = row_version + 1, updated_at = ?
WHERE run_id = ? AND controller_status != ?;`, status, now, runID, status)
	if err != nil {
		return fmt.Errorf("project session connection %s: %w", status, err)
	}
	return nil
}

// ConnectRunController establishes a new attachment episode for the current
// adopted controller. Requires the current lease and its expected
// generation; never a prior attachment; a matching identity is implied by
// classification of the current grant (no replacement identity can appear).
// Idempotent replay within the same episode recovers the existing receipt
// without creating a second episode.
func (s *Store) ConnectRunController(ctx context.Context, opID, runID, lease string, expectedGeneration uint64, instanceID string) (ConnectReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return ConnectReceipt{}, errors.New("empty operation id")
	}
	fingerprint := computeFingerprint("connect_run_controller", runID, fmt.Sprintf("%d", expectedGeneration))

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return ConnectReceipt{}, err
	}
	defer tx.Rollback()

	// Current controller authority and generation precede attachment
	// replay: a superseded or unknown credential never reaches a
	// successful replay (AC-004 Gate 1 review finding 1).
	gen, err := classifyRunController(ctx, tx.Tx(), runID, lease)
	if err != nil {
		return ConnectReceipt{}, err
	}
	if gen != expectedGeneration {
		return ConnectReceipt{}, ErrGenerationMismatch
	}

	// Idempotent replay: recover the existing episode receipt. The caller
	// distinguishes a historical episode from the current one by comparing
	// the returned identity with the durable attachment state.
	var storedFingerprint, payloadJSON string
	err = tx.Tx().QueryRowContext(ctx, `SELECT command_fingerprint, payload_json FROM journal_entries WHERE op_id = ?;`, opID).
		Scan(&storedFingerprint, &payloadJSON)
	if err == nil {
		if storedFingerprint != fingerprint {
			return ConnectReceipt{}, ErrIdempotencyConflict
		}
		var jp journalPayload
		if err := json.Unmarshal([]byte(payloadJSON), &jp); err == nil && jp.Receipt.OpID != "" {
			attachment, rev := parseAttachmentPayload(jp.Receipt.Payload)
			return ConnectReceipt{OperationReceipt: jp.Receipt, AttachmentID: attachment, AttachmentRev: rev}, nil
		}
		return ConnectReceipt{}, ErrIdempotencyConflict
	}
	if err != sql.ErrNoRows {
		return ConnectReceipt{}, fmt.Errorf("query connect operation: %w", err)
	}

	attachmentID, err := newAttachmentID()
	if err != nil {
		return ConnectReceipt{}, err
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Tx().ExecContext(ctx, `UPDATE controller_leases
		SET connected = 1, attachment_id = ?, instance_id = ?, attachment_rev = attachment_rev + 1, updated_at = ?
		WHERE run_id = ? AND generation = ? AND status = 'active';`, attachmentID, instanceID, now, runID, gen); err != nil {
		return ConnectReceipt{}, fmt.Errorf("establish attachment: %w", err)
	}
	var rev uint64
	if err := tx.Tx().QueryRowContext(ctx, `SELECT attachment_rev FROM controller_leases
		WHERE run_id = ? AND generation = ? AND status = 'active';`, runID, gen).Scan(&rev); err != nil {
		return ConnectReceipt{}, fmt.Errorf("read attachment revision: %w", err)
	}
	if err := projectSessionConnection(ctx, tx.Tx(), runID, "connected"); err != nil {
		return ConnectReceipt{}, err
	}

	receipt := ConnectReceipt{
		OperationReceipt: OperationReceipt{
			OpID:             opID,
			CommandType:      "connect_run_controller",
			CommittedVersion: int64(gen),
			CreatedAt:        time.Now().UTC(),
			Payload:          fmt.Sprintf("attachment=%s:rev=%d", attachmentID, rev),
		},
		AttachmentID:  attachmentID,
		AttachmentRev: rev,
	}
	if err := recordJournalEntry(tx.Tx(), opID, "connect_run_controller", fingerprint, runID, "", "", "controller_connected", receipt.OperationReceipt, ""); err != nil {
		return ConnectReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return ConnectReceipt{}, err
	}
	return receipt, nil
}

func parseAttachmentPayload(payload string) (string, uint64) {
	attachment, rest, _ := strings.Cut(strings.TrimPrefix(payload, "attachment="), ":rev=")
	rev := uint64(0)
	if rest != "" {
		fmt.Sscanf(rest, "%d", &rev)
	}
	return attachment, rev
}

// DisconnectRunController ends the current attachment episode. A delayed
// disconnect from a superseded episode (attachment A after B connected) is
// ignored: B remains connected.
func (s *Store) DisconnectRunController(ctx context.Context, opID, runID, lease, attachmentID string, expectedGeneration uint64) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return OperationReceipt{}, errors.New("empty operation id")
	}
	fingerprint := computeFingerprint("disconnect_run_controller", runID, attachmentID, fmt.Sprintf("%d", expectedGeneration))

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	// Current controller authority and generation precede replay: an old
	// disconnect operation replayed by its superseded controller is fenced
	// here, and a replayed receipt for an ended episode never disconnects
	// a successor (AC-004 Gate 1 review finding 1).
	gen, err := classifyRunController(ctx, tx.Tx(), runID, lease)
	if err != nil {
		return OperationReceipt{}, err
	}
	if gen != expectedGeneration {
		return OperationReceipt{}, ErrGenerationMismatch
	}

	// Idempotent replay.
	var storedFingerprint, payloadJSON string
	err = tx.Tx().QueryRowContext(ctx, `SELECT command_fingerprint, payload_json FROM journal_entries WHERE op_id = ?;`, opID).
		Scan(&storedFingerprint, &payloadJSON)
	if err == nil {
		if storedFingerprint != fingerprint {
			return OperationReceipt{}, ErrIdempotencyConflict
		}
		var jp journalPayload
		if err := json.Unmarshal([]byte(payloadJSON), &jp); err == nil && jp.Receipt.OpID != "" {
			return jp.Receipt, nil
		}
		return OperationReceipt{}, ErrIdempotencyConflict
	}
	if err != sql.ErrNoRows {
		return OperationReceipt{}, fmt.Errorf("query disconnect operation: %w", err)
	}

	var currentAttachment string
	var connected int
	err = tx.Tx().QueryRowContext(ctx, `SELECT coalesce(attachment_id,''), connected FROM controller_leases
		WHERE run_id = ? AND generation = ? AND status = 'active';`, runID, gen).Scan(&currentAttachment, &connected)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query current attachment: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload := "stale_attachment_ignored"
	if connected == 1 && currentAttachment == attachmentID {
		if _, err := tx.Tx().ExecContext(ctx, `UPDATE controller_leases
			SET connected = 0, attachment_id = NULL, updated_at = ?
			WHERE run_id = ? AND generation = ?;`, now, runID, gen); err != nil {
			return OperationReceipt{}, fmt.Errorf("end attachment: %w", err)
		}
		if err := projectSessionConnection(ctx, tx.Tx(), runID, "disconnected"); err != nil {
			return OperationReceipt{}, err
		}
		payload = "disconnected:attachment=" + attachmentID
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "disconnect_run_controller",
		CommittedVersion: int64(gen),
		CreatedAt:        time.Now().UTC(),
		Payload:          payload,
	}
	if err := recordJournalEntry(tx.Tx(), opID, "disconnect_run_controller", fingerprint, runID, "", "", "controller_disconnected", receipt, ""); err != nil {
		return OperationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}
