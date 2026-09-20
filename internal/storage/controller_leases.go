package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// Controller lease grant transitions (AC-004). One request contract per
// operation; secrets are candidates supplied by the caller (generated once
// per committed operation — candidates are never idempotency inputs); the
// authoritative issuance is resolved transactionally via granted_by_op_id.

var (
	ErrAdoptionExists      = errors.New("an adopted controller lease is already active for this run")
	ErrAdoptionRequired    = errors.New("run has no adopted controller; explicit adoption is required")
	ErrGenerationMismatch  = errors.New("expected controller lease generation is stale")
	ErrLeaseSuperseded     = errors.New("controller lease has been superseded or revoked")
	ErrRecoveryUnavailable = errors.New("requested credential issuance is no longer recoverable")
)

// OperatorRecovery carries verified operator recovery intent. The flag is
// meaningful only on an operator-authenticated transport (enforced by the
// service layer); storage records it as journaled intent.
type OperatorRecovery struct {
	Reason             string
	ExpectedGeneration uint64
}

// ControllerGrantReceipt is returned by adopt, handoff, and credential
// recovery. LeaseSecret is present only while the issued generation is
// active; historical replays and inspections return receipts without it.
type ControllerGrantReceipt struct {
	OperationReceipt
	Generation  uint64
	LeaseSecret string
}

// ControllerRecord is the redacted outward view of run-scoped controller
// authority. It never contains a lease secret.
type ControllerRecord struct {
	Generation    uint64
	Harness       string
	ControllerRef string
	Status        string // "active", "legacy", "revoked", "superseded", "none"
	Adopted       bool
	Connected     bool
	AttachmentID  string
}

// rowQueryer abstracts QueryRowContext over transactions and read pools so
// credential classification runs identically in write transitions and
// read-only replay resolvers.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// classifyCredential resolves the authority state of a presented credential
// for runID. Empty credentials never pass; superseded/revoked credentials
// classify distinctly (ErrLeaseSuperseded); success requires an internally
// consistent grant — controller class demands an adopted active grant,
// administrator class additionally accepts the unadopted run's legacy
// provenance credential (operator maintenance handle). A bare string match
// against runs.controller_lease authorizes nothing.
func classifyCredential(ctx context.Context, q rowQueryer, runID, presentedLease string, requireAdopted bool) (uint64, error) {
	if strings.TrimSpace(presentedLease) == "" {
		return 0, ErrAdoptionRequired
	}

	var adopted int
	var runLease, lifecycle string
	err := q.QueryRowContext(ctx, `SELECT controller_adopted, controller_lease, lifecycle FROM runs WHERE run_id = ?;`, runID).
		Scan(&adopted, &runLease, &lifecycle)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("run %s not found", runID)
	}
	if err != nil {
		return 0, fmt.Errorf("query run for controller authority: %w", err)
	}
	if lifecycle == "archived" {
		return 0, ErrSessionArchived
	}

	// Current authority first: an internally consistent adopted, active
	// grant (an active match outranks any retired-history collision with
	// the same value). Never a bare string match, never the legacy
	// provenance for controller-class commands.
	var activeGen uint64
	var activeLease string
	err = q.QueryRowContext(ctx, `SELECT generation, lease FROM controller_leases
		WHERE run_id = ? AND status = 'active';`, runID).Scan(&activeGen, &activeLease)
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("query active grant: %w", err)
	}
	if err == nil && adopted == 1 && activeLease == presentedLease && runLease == presentedLease {
		return activeGen, nil
	}

	// Superseded/revoked classification: a credential matching only
	// retired history never re-authorizes.
	var gen uint64
	err = q.QueryRowContext(ctx, `SELECT generation FROM controller_leases
		WHERE run_id = ? AND lease = ? AND status IN ('superseded','revoked');`, runID, presentedLease).Scan(&gen)
	if err == nil {
		return 0, fmt.Errorf("%w (generation %d)", ErrLeaseSuperseded, gen)
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("classify retired credential: %w", err)
	}

	if adopted == 0 {
		if requireAdopted {
			return 0, ErrAdoptionRequired
		}
		// Administrator class: the unadopted run's legacy provenance
		// credential remains the operator's maintenance handle.
		var legacyLease string
		err = q.QueryRowContext(ctx, `SELECT lease FROM controller_leases
			WHERE run_id = ? AND generation = 0 AND status = 'legacy';`, runID).Scan(&legacyLease)
		if err == nil && legacyLease == presentedLease && runLease == presentedLease {
			return 0, nil
		}
		if err != nil && err != sql.ErrNoRows {
			return 0, fmt.Errorf("query legacy provenance: %w", err)
		}
		return 0, ErrUnauthorizedOperation
	}
	return 0, ErrUnauthorizedOperation
}

// classifyRunController enforces controller-class authority inside a write
// transaction. Attachment requirements join this boundary in Task 5.
func classifyRunController(ctx context.Context, tx *sql.Tx, runID, presentedLease string) (uint64, error) {
	return classifyCredential(ctx, tx, runID, presentedLease, true)
}

// authorizeSessionController resolves a session's run and classifies the
// presented credential; controller commands call this before any
// idempotency lookup.
func authorizeSessionController(ctx context.Context, q rowQueryer, sessionID, presentedLease string, requireAdopted bool) (uint64, error) {
	var runID string
	err := q.QueryRowContext(ctx, `SELECT run_id FROM sessions WHERE session_id = ?;`, sessionID).Scan(&runID)
	if err == sql.ErrNoRows {
		return 0, ErrSessionNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("resolve session run: %w", err)
	}
	return classifyCredential(ctx, q, runID, presentedLease, requireAdopted)
}

// resolveGrantReplay returns the original committed issuance for opID when
// the operation has already committed with matching command content.
// LeaseSecret is included only while that generation remains active.
func resolveGrantReplay(ctx context.Context, tx *sql.Tx, opID, commandType, fingerprint string) (*ControllerGrantReceipt, bool, error) {
	var storedCmdType, storedFingerprint string
	err := tx.QueryRowContext(ctx, `SELECT command_type, command_fingerprint FROM journal_entries WHERE op_id = ?;`, opID).
		Scan(&storedCmdType, &storedFingerprint)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("query grant operation: %w", err)
	}
	if storedCmdType != commandType || storedFingerprint != fingerprint {
		return nil, false, ErrIdempotencyConflict
	}

	var gen uint64
	var lease, status string
	var runID string
	err = tx.QueryRowContext(ctx, `SELECT run_id, generation, lease, status FROM controller_leases WHERE granted_by_op_id = ?;`, opID).
		Scan(&runID, &gen, &lease, &status)
	if err == sql.ErrNoRows {
		return nil, false, fmt.Errorf("grant operation %s has no issuance record", opID)
	}
	if err != nil {
		return nil, false, fmt.Errorf("query grant issuance: %w", err)
	}

	secret := ""
	if status == "active" {
		secret = lease
	}
	return &ControllerGrantReceipt{
		OperationReceipt: OperationReceipt{
			OpID:             opID,
			CommandType:      commandType,
			CommittedVersion: int64(gen),
			CreatedAt:        time.Now().UTC(),
			Payload:          "replay:" + status,
		},
		Generation:  gen,
		LeaseSecret: secret,
	}, true, nil
}

// AdoptController installs the next controller generation for runID.
// Authority to supersede the legacy provenance row is the matching
// bootstrap credential or verified operator recovery. Candidates are never
// idempotency inputs: replay resolves the original issuance.
func (s *Store) AdoptController(ctx context.Context, opID, runID, harness, controllerRef, bootstrapLease string, recovery *OperatorRecovery, newLeaseCandidate string) (ControllerGrantReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return ControllerGrantReceipt{}, errors.New("empty operation id")
	}
	if !council.ValidContributor(council.Contributor(harness)) {
		return ControllerGrantReceipt{}, fmt.Errorf("invalid controller harness %q", harness)
	}
	if strings.TrimSpace(controllerRef) == "" {
		return ControllerGrantReceipt{}, errors.New("controller reference is required")
	}
	if strings.TrimSpace(newLeaseCandidate) == "" {
		return ControllerGrantReceipt{}, errors.New("lease candidate is required")
	}

	fingerprint := computeFingerprint("adopt_controller", runID, harness, controllerRef)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return ControllerGrantReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, found, err := resolveGrantReplay(ctx, tx.Tx(), opID, "adopt_controller", fingerprint); err != nil {
		return ControllerGrantReceipt{}, err
	} else if found {
		return *receipt, nil
	}

	// An adopted active controller may not be silently replaced.
	var adopted int
	err = tx.Tx().QueryRowContext(ctx, `SELECT controller_adopted FROM runs WHERE run_id = ?;`, runID).Scan(&adopted)
	if err == sql.ErrNoRows {
		return ControllerGrantReceipt{}, fmt.Errorf("run %s not found", runID)
	}
	if err != nil {
		return ControllerGrantReceipt{}, fmt.Errorf("query run adoption state: %w", err)
	}
	if adopted == 1 {
		return ControllerGrantReceipt{}, ErrAdoptionExists
	}

	// Authority to supersede the legacy provenance: bootstrap credential
	// match or verified operator recovery with the expected generation.
	if recovery != nil {
		var legacyGen uint64
		err = tx.Tx().QueryRowContext(ctx, `SELECT generation FROM controller_leases
			WHERE run_id = ? AND status IN ('legacy','revoked') ORDER BY generation DESC LIMIT 1;`, runID).Scan(&legacyGen)
		if err == nil && recovery.ExpectedGeneration != legacyGen {
			return ControllerGrantReceipt{}, ErrGenerationMismatch
		}
	} else {
		var runLease string
		if err := tx.Tx().QueryRowContext(ctx, `SELECT controller_lease FROM runs WHERE run_id = ?;`, runID).Scan(&runLease); err != nil {
			return ControllerGrantReceipt{}, fmt.Errorf("query run lease: %w", err)
		}
		if bootstrapLease != runLease {
			return ControllerGrantReceipt{}, ErrUnauthorizedOperation
		}
	}

	return s.installGrant(ctx, tx.Tx(), opID, runID, harness, controllerRef, newLeaseCandidate, "adopt_controller", fingerprint, "controller_adopted")
}

// HandoffController supersedes the current active grant with the next
// generation. Requires the current lease and its expected generation;
// stale-authority classification precedes generation mismatch.
func (s *Store) HandoffController(ctx context.Context, opID, runID, currentLease string, expectedGeneration uint64, harness, controllerRef, newLeaseCandidate string) (ControllerGrantReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return ControllerGrantReceipt{}, errors.New("empty operation id")
	}
	if !council.ValidContributor(council.Contributor(harness)) {
		return ControllerGrantReceipt{}, fmt.Errorf("invalid controller harness %q", harness)
	}
	if strings.TrimSpace(controllerRef) == "" {
		return ControllerGrantReceipt{}, errors.New("controller reference is required")
	}
	if strings.TrimSpace(newLeaseCandidate) == "" {
		return ControllerGrantReceipt{}, errors.New("lease candidate is required")
	}

	fingerprint := computeFingerprint("handoff_controller", runID, harness, controllerRef)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return ControllerGrantReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, found, err := resolveGrantReplay(ctx, tx.Tx(), opID, "handoff_controller", fingerprint); err != nil {
		return ControllerGrantReceipt{}, err
	} else if found {
		return *receipt, nil
	}

	// Current authority first: an active, adopted, generation-consistent
	// grant. Superseded credentials classify here (ErrLeaseSuperseded).
	activeGen, err := classifyRunController(ctx, tx.Tx(), runID, currentLease)
	if err != nil {
		return ControllerGrantReceipt{}, err
	}
	if activeGen != expectedGeneration {
		return ControllerGrantReceipt{}, ErrGenerationMismatch
	}

	// The old generation is retired by the new installation; its
	// attachment is invalidated and session projections updated
	// transactionally (the new controller never inherits connected flags).
	if _, err := tx.Tx().ExecContext(ctx, `UPDATE controller_leases SET status = 'superseded', connected = 0, attachment_id = NULL, updated_at = ?
		WHERE run_id = ? AND status = 'active';`, time.Now().UTC().Format(time.RFC3339Nano), runID); err != nil {
		return ControllerGrantReceipt{}, fmt.Errorf("supersede previous grant: %w", err)
	}
	if err := projectSessionConnection(ctx, tx.Tx(), runID, "disconnected"); err != nil {
		return ControllerGrantReceipt{}, err
	}

	return s.installGrant(ctx, tx.Tx(), opID, runID, harness, controllerRef, newLeaseCandidate, "handoff_controller", fingerprint, "controller_handed_off")
}

// RevokeController removes controller authority. With the current lease it
// is controller self-revocation; with verified operator recovery it is a
// lost-lease recovery revocation targeting the expected generation.
func (s *Store) RevokeController(ctx context.Context, opID, runID, controllerLease string, recovery *OperatorRecovery) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return OperationReceipt{}, errors.New("empty operation id")
	}

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	// Idempotent replay of the revocation.
	var storedCmdType, storedFingerprint, payloadJSON string
	err = tx.Tx().QueryRowContext(ctx, `SELECT command_type, command_fingerprint, payload_json FROM journal_entries WHERE op_id = ?;`, opID).
		Scan(&storedCmdType, &storedFingerprint, &payloadJSON)
	if err == nil {
		var jp journalPayload
		if err := json.Unmarshal([]byte(payloadJSON), &jp); err == nil && jp.Receipt.OpID != "" {
			return jp.Receipt, nil
		}
		return OperationReceipt{}, ErrIdempotencyConflict
	}
	if err != sql.ErrNoRows {
		return OperationReceipt{}, fmt.Errorf("query revocation operation: %w", err)
	}

	var targetGen uint64
	switch {
	case recovery != nil:
		// Operator recovery: revoke the grant at the expected generation —
		// the legacy bootstrap (0) or the active adopted grant.
		var gen uint64
		var status string
		err = tx.Tx().QueryRowContext(ctx, `SELECT generation, status FROM controller_leases
			WHERE run_id = ? AND status IN ('legacy','active') ORDER BY generation DESC LIMIT 1;`, runID).Scan(&gen, &status)
		if err == sql.ErrNoRows {
			return OperationReceipt{}, fmt.Errorf("no controller grant to revoke for run %s", runID)
		}
		if err != nil {
			return OperationReceipt{}, fmt.Errorf("query revocation target: %w", err)
		}
		if recovery.ExpectedGeneration != gen {
			return OperationReceipt{}, ErrGenerationMismatch
		}
		targetGen = gen
	case strings.TrimSpace(controllerLease) != "":
		gen, err := classifyRunController(ctx, tx.Tx(), runID, controllerLease)
		if err != nil {
			return OperationReceipt{}, err
		}
		targetGen = gen
	default:
		return OperationReceipt{}, ErrAdoptionRequired
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Tx().ExecContext(ctx, `UPDATE controller_leases SET status = 'revoked', connected = 0, attachment_id = NULL, updated_at = ?
		WHERE run_id = ? AND generation = ?;`, now, runID, targetGen); err != nil {
		return OperationReceipt{}, fmt.Errorf("revoke grant: %w", err)
	}
	// The revoked controller's attachment is invalidated and session
	// projections updated transactionally.
	if err := projectSessionConnection(ctx, tx.Tx(), runID, "disconnected"); err != nil {
		return OperationReceipt{}, err
	}
	if _, err := tx.Tx().ExecContext(ctx, `UPDATE runs SET controller_lease = '', updated_at = ? WHERE run_id = ?;`, now, runID); err != nil {
		return OperationReceipt{}, fmt.Errorf("clear run lease: %w", err)
	}
	if targetGen > 0 {
		if _, err := tx.Tx().ExecContext(ctx, `UPDATE runs SET controller_adopted = 0, updated_at = ? WHERE run_id = ?;`, now, runID); err != nil {
			return OperationReceipt{}, fmt.Errorf("clear adoption flag: %w", err)
		}
	}

	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "revoke_controller",
		CommittedVersion: int64(targetGen),
		CreatedAt:        time.Now().UTC(),
		Payload:          fmt.Sprintf("revoked:generation=%d", targetGen),
	}
	if err := recordJournalEntry(tx.Tx(), opID, "revoke_controller",
		computeFingerprint("revoke_controller", runID, fmt.Sprintf("%d", targetGen)),
		runID, "", "", "controller_revoked", receipt, ""); err != nil {
		return OperationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// RecoverControllerCredential retrieves the still-active secret issued by a
// committed adoption or handoff operation. Operator-authorized (service
// transport); no old lease is required or accepted.
func (s *Store) RecoverControllerCredential(ctx context.Context, opID, runID, sourceOpID string, expectedGeneration uint64) (ControllerGrantReceipt, error) {
	if strings.TrimSpace(opID) == "" || strings.TrimSpace(sourceOpID) == "" {
		return ControllerGrantReceipt{}, errors.New("operation id and source operation id are required")
	}

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return ControllerGrantReceipt{}, err
	}
	defer tx.Rollback()

	// Idempotent replay of the recovery resolves the source issuance again:
	// disclosure follows its current status.
	var storedCmdType, storedFingerprint string
	err = tx.Tx().QueryRowContext(ctx, `SELECT command_type, command_fingerprint FROM journal_entries WHERE op_id = ?;`, opID).
		Scan(&storedCmdType, &storedFingerprint)
	if err == nil {
		if storedCmdType != "recover_controller_credential" || storedFingerprint != computeFingerprint("recover_controller_credential", runID, sourceOpID, fmt.Sprintf("%d", expectedGeneration)) {
			return ControllerGrantReceipt{}, ErrIdempotencyConflict
		}
		return s.resolveIssuanceForRecovery(ctx, tx.Tx(), runID, sourceOpID, expectedGeneration)
	}
	if err != sql.ErrNoRows {
		return ControllerGrantReceipt{}, fmt.Errorf("query recovery operation: %w", err)
	}

	grant, err := s.resolveIssuanceForRecovery(ctx, tx.Tx(), runID, sourceOpID, expectedGeneration)
	if err != nil {
		return ControllerGrantReceipt{}, err
	}

	// The recovery operation is journaled without the secret; its replay
	// resolves the source issuance's current status.
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "recover_controller_credential",
		CommittedVersion: int64(grant.Generation),
		CreatedAt:        time.Now().UTC(),
		Payload:          fmt.Sprintf("recovered:generation=%d", grant.Generation),
	}
	if err := recordJournalEntry(tx.Tx(), opID, "recover_controller_credential",
		computeFingerprint("recover_controller_credential", runID, sourceOpID, fmt.Sprintf("%d", expectedGeneration)),
		runID, "", "", "controller_credential_recovered", receipt, ""); err != nil {
		return ControllerGrantReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return ControllerGrantReceipt{}, err
	}
	return grant, nil
}

func (s *Store) resolveIssuanceForRecovery(ctx context.Context, tx *sql.Tx, runID, sourceOpID string, expectedGeneration uint64) (ControllerGrantReceipt, error) {
	var storedCmdType string
	err := tx.QueryRowContext(ctx, `SELECT command_type FROM journal_entries WHERE op_id = ?;`, sourceOpID).Scan(&storedCmdType)
	if err == sql.ErrNoRows {
		return ControllerGrantReceipt{}, fmt.Errorf("%w: source operation %s not found", ErrRecoveryUnavailable, sourceOpID)
	}
	if err != nil {
		return ControllerGrantReceipt{}, fmt.Errorf("query source operation: %w", err)
	}
	if storedCmdType != "adopt_controller" && storedCmdType != "handoff_controller" {
		return ControllerGrantReceipt{}, fmt.Errorf("%w: source operation %s is not a grant issuance", ErrRecoveryUnavailable, sourceOpID)
	}

	var gen uint64
	var lease, status, grantedRun string
	err = tx.QueryRowContext(ctx, `SELECT run_id, generation, lease, status FROM controller_leases WHERE granted_by_op_id = ?;`, sourceOpID).
		Scan(&grantedRun, &gen, &lease, &status)
	if err == sql.ErrNoRows {
		return ControllerGrantReceipt{}, fmt.Errorf("%w: no issuance for %s", ErrRecoveryUnavailable, sourceOpID)
	}
	if err != nil {
		return ControllerGrantReceipt{}, fmt.Errorf("query source issuance: %w", err)
	}
	if grantedRun != runID {
		return ControllerGrantReceipt{}, fmt.Errorf("%w: issuance belongs to run %s", ErrRecoveryUnavailable, grantedRun)
	}
	if gen != expectedGeneration {
		return ControllerGrantReceipt{}, ErrGenerationMismatch
	}
	if status != "active" {
		return ControllerGrantReceipt{}, ErrRecoveryUnavailable
	}
	return ControllerGrantReceipt{
		OperationReceipt: OperationReceipt{
			OpID:             sourceOpID,
			CommandType:      storedCmdType,
			CommittedVersion: int64(gen),
			CreatedAt:        time.Now().UTC(),
			Payload:          fmt.Sprintf("generation=%d", gen),
		},
		Generation:  gen,
		LeaseSecret: lease,
	}, nil
}

// installGrant supersedes nothing itself (callers retire the previous
// state), installs the new active generation, updates the run, and journals
// the transition. The secret is never placed in the journal payload.
func (s *Store) installGrant(ctx context.Context, tx *sql.Tx, opID, runID, harness, controllerRef, newLeaseCandidate, commandType, fingerprint, eventKind string) (ControllerGrantReceipt, error) {
	var nextGen uint64
	err := tx.QueryRowContext(ctx, `SELECT coalesce(max(generation), 0) + 1 FROM controller_leases WHERE run_id = ?;`, runID).Scan(&nextGen)
	if err != nil {
		return ControllerGrantReceipt{}, fmt.Errorf("allocate generation: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO controller_leases
		(run_id, generation, harness, controller_ref, lease, status, granted_by_op_id, attachment_id, connected, attached_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'active', ?, NULL, 0, ?, ?);`,
		runID, nextGen, harness, controllerRef, newLeaseCandidate, opID, now, now)
	if err != nil {
		return ControllerGrantReceipt{}, fmt.Errorf("install grant: %w", err)
	}

	// The legacy provenance row is retired by the first adoption.
	if _, err := tx.ExecContext(ctx, `UPDATE controller_leases SET status = 'superseded', updated_at = ?
		WHERE run_id = ? AND generation = 0 AND status = 'legacy';`, now, runID); err != nil {
		return ControllerGrantReceipt{}, fmt.Errorf("retire legacy provenance: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE runs SET controller_lease = ?, controller_adopted = 1, updated_at = ? WHERE run_id = ?;`,
		newLeaseCandidate, now, runID); err != nil {
		return ControllerGrantReceipt{}, fmt.Errorf("update run authority: %w", err)
	}

	receipt := ControllerGrantReceipt{
		OperationReceipt: OperationReceipt{
			OpID:             opID,
			CommandType:      commandType,
			CommittedVersion: int64(nextGen),
			CreatedAt:        time.Now().UTC(),
			Payload:          fmt.Sprintf("generation=%d", nextGen),
		},
		Generation:  nextGen,
		LeaseSecret: newLeaseCandidate,
	}
	if err := recordJournalEntry(tx, opID, commandType, fingerprint, runID, "", "", eventKind, receipt.OperationReceipt, ""); err != nil {
		return ControllerGrantReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return ControllerGrantReceipt{}, err
	}
	return receipt, nil
}

// ValidateRunControllerLease classifies a presented credential directly
// against a run (controller class).
func (s *Store) ValidateRunControllerLease(ctx context.Context, runID, callerLease string) error {
	_, err := classifyCredential(ctx, s.readDB, runID, callerLease, true)
	return err
}

// GetControllerRecord returns the redacted run-scoped controller record.
func (s *Store) GetControllerRecord(ctx context.Context, runID string) (ControllerRecord, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT generation, coalesce(harness,''), controller_ref, status, connected, coalesce(attachment_id,'')
		FROM controller_leases WHERE run_id = ? ORDER BY generation DESC;`, runID)
	if err != nil {
		return ControllerRecord{}, fmt.Errorf("query controller leases: %w", err)
	}
	defer rows.Close()

	var adopted int
	if err := s.readDB.QueryRowContext(ctx, `SELECT controller_adopted FROM runs WHERE run_id = ?;`, runID).Scan(&adopted); err != nil {
		return ControllerRecord{}, fmt.Errorf("query run adoption state: %w", err)
	}

	rec := ControllerRecord{Status: "none"}
	for rows.Next() {
		var gen uint64
		var harness, controllerRef, status, attachment string
		var connected int
		if err := rows.Scan(&gen, &harness, &controllerRef, &status, &connected, &attachment); err != nil {
			return ControllerRecord{}, fmt.Errorf("scan controller lease: %w", err)
		}
		if err := rows.Err(); err != nil {
			return ControllerRecord{}, err
		}
		switch status {
		case "active":
			rec = ControllerRecord{
				Generation: gen, Harness: harness, ControllerRef: controllerRef,
				Status: "active", Adopted: adopted == 1, Connected: connected == 1, AttachmentID: attachment,
			}
		case "legacy":
			rec = ControllerRecord{Generation: gen, Status: "legacy", Adopted: false}
		case "revoked", "superseded":
			if rec.Status == "none" {
				rec = ControllerRecord{Generation: gen, Harness: harness, ControllerRef: controllerRef, Status: status, Adopted: adopted == 1}
			}
		}
	}
	return rec, nil
}
