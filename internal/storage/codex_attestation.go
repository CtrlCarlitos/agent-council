package storage

// AC-009 durable cprot-v2 protection-attestation records and the
// creation-uncertainty journal (spec §3.7/§3.4): the attestation row is
// created only by an operator-authorized journal operation — idempotent
// by op_id with receipt replay, the attestation identity (cprot-v2
// digest) unique — and an uncertain SESSION creation is recorded
// durably so a service restart can never silently re-create a native
// thread for it (§3.4: automatic recreation blocked until an explicit
// resolution).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CodexProtectionAttestationRecord is the durable form of a cprot-v2
// protection attestation. ProbeResults carries the canonical framed
// record list produced by the cprot-v2 encoder — never free-form JSON.
// The run provenance lives on the journal entry, not the row (the
// codex_protection_attestations table has no run column).
type CodexProtectionAttestationRecord struct {
	AttestationID  string // cprot-v2:sha256:<hex> over the canonical frame
	RunID          string // the run the probe was performed for (journal provenance)
	CodexVersion   string
	Platform       string // canonical os + "/" + family identity (§3.7)
	ManifestDigest string // sha256:<hex> — the ComputeToolkitManifestDigest value
	ProfileDigest  string // cprof-v3:sha256:<hex>
	ProbeResults   string // canonical cprot-v2 framed record list
	ProbedAt       string // RFC3339 UTC
	Actor          string // operator identity (journal-linked)
}

// RecordCodexProtectionAttestation persists the attestation row and its
// journal entry in one transaction. Idempotent by op_id: replaying the
// same operation returns the committed receipt; the same op_id with
// different evidence is an idempotency conflict; the same evidence under
// a new op_id is rejected (the row already exists). Later attestations
// never reclassify earlier attempts: attempts freeze their attestation
// id at launch, and the redispatch/absence gates match that frozen id —
// never "the latest row".
func (s *Store) RecordCodexProtectionAttestation(ctx context.Context, opID string, rec CodexProtectionAttestationRecord) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return OperationReceipt{}, errors.New("attestation operation requires an op_id")
	}
	if strings.TrimSpace(rec.RunID) == "" {
		return OperationReceipt{}, errors.New("attestation operation requires the run provenance")
	}
	// The durable table CHECK enforces exactly this shape; validate
	// before the transaction so a malformed id fails with a readable
	// error instead of a constraint violation.
	if len(rec.AttestationID) != 80 || !strings.HasPrefix(rec.AttestationID, "cprot-v2:sha256:") {
		return OperationReceipt{}, fmt.Errorf("attestation id %q is not a cprot-v2 digest", rec.AttestationID)
	}
	for name, value := range map[string]string{
		"codex_version":   rec.CodexVersion,
		"platform":        rec.Platform,
		"manifest_digest": rec.ManifestDigest,
		"profile_digest":  rec.ProfileDigest,
		"probe_results":   rec.ProbeResults,
		"probed_at":       rec.ProbedAt,
		"actor":           rec.Actor,
	} {
		if strings.TrimSpace(value) == "" {
			return OperationReceipt{}, fmt.Errorf("attestation %s is required", name)
		}
	}

	fp := computeFingerprint("record_codex_attestation", rec.AttestationID, rec.RunID, rec.CodexVersion,
		rec.Platform, rec.ManifestDigest, rec.ProfileDigest, rec.ProbeResults, rec.ProbedAt, rec.Actor)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, rec.Actor, "record_codex_attestation", fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	now := time.Now().UTC()
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      "record_codex_attestation",
		CommittedVersion: 1,
		CreatedAt:        now,
		Payload:          rec.AttestationID,
	}
	if err := recordJournalEntry(tx.Tx(), opID, "record_codex_attestation", fp, rec.RunID, "", "",
		"codex_attestation", receipt, rec.Actor); err != nil {
		return OperationReceipt{}, err
	}

	// Duplicate evidence identity: the same attestation under a new
	// op_id is rejected rather than silently duplicated.
	var existingID string
	if err := tx.Tx().QueryRowContext(ctx,
		`SELECT attestation_id FROM codex_protection_attestations WHERE attestation_id = ?`,
		rec.AttestationID).Scan(&existingID); err == nil {
		return OperationReceipt{}, fmt.Errorf("attestation %s is already recorded", rec.AttestationID)
	}

	res, err := tx.Tx().ExecContext(ctx, `
INSERT INTO codex_protection_attestations
	(attestation_id, codex_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.AttestationID, rec.CodexVersion, rec.Platform, rec.ManifestDigest,
		rec.ProfileDigest, rec.ProbeResults, rec.ProbedAt, rec.Actor)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("insert codex protection attestation: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return OperationReceipt{}, fmt.Errorf("attestation %s is already recorded", rec.AttestationID)
	}

	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// ── Durable creation uncertainty (§3.4) ─────────────────────────────────

// CodexCreationUncertainty is the durable record of a session creation
// whose native thread MAY have been created: automatic recreation is
// blocked across service restarts until an explicit resolution is
// recorded.
type CodexCreationUncertainty struct {
	RunID      string
	SessionID  string
	Reason     string
	RecordedBy string // the identity attributed in the journal entry
}

const (
	codexUncertainCmd  = "record_codex_creation_uncertain"
	codexUncertainKind = "codex_creation_uncertain"
	codexResolveCmd    = "resolve_codex_creation_uncertain"
	codexResolveKind   = "codex_creation_uncertainty_resolved"
)

// RecordCodexCreationUncertain durably records that a session's native
// creation ended UNCERTAIN. The record is a journal entry bound to the
// logical session; it is idempotent by op_id (the service derives a
// deterministic op id per session, so a replay after a crash cannot
// duplicate it).
func (s *Store) RecordCodexCreationUncertain(ctx context.Context, opID string, rec CodexCreationUncertainty) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return OperationReceipt{}, errors.New("creation-uncertainty record requires an op_id")
	}
	if strings.TrimSpace(rec.SessionID) == "" || strings.TrimSpace(rec.RunID) == "" {
		return OperationReceipt{}, errors.New("creation-uncertainty record requires the session and run provenance")
	}
	if strings.TrimSpace(rec.Reason) == "" {
		return OperationReceipt{}, errors.New("creation-uncertainty record requires the reason")
	}

	fp := computeFingerprint(codexUncertainCmd, rec.SessionID, rec.RunID, rec.Reason)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, rec.RecordedBy, codexUncertainCmd, fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	now := time.Now().UTC()
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      codexUncertainCmd,
		SessionID:        rec.SessionID,
		CommittedVersion: 1,
		CreatedAt:        now,
		Payload:          rec.Reason,
	}
	if err := recordJournalEntry(tx.Tx(), opID, codexUncertainCmd, fp, rec.RunID, rec.SessionID, "",
		codexUncertainKind, receipt, rec.RecordedBy); err != nil {
		return OperationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// ResolveCodexCreationUncertainty durably records the explicit,
// controller-visible resolution of a session's creation uncertainty.
// Automatic retries NEVER resolve it; only this operation does.
func (s *Store) ResolveCodexCreationUncertainty(ctx context.Context, opID, sessionID, resolvedBy string) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" {
		return OperationReceipt{}, errors.New("creation-uncertainty resolution requires an op_id")
	}
	if strings.TrimSpace(sessionID) == "" {
		return OperationReceipt{}, errors.New("creation-uncertainty resolution requires the session id")
	}

	fp := computeFingerprint(codexResolveCmd, sessionID)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, resolvedBy, codexResolveCmd, fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var runID string
	if err := tx.Tx().QueryRowContext(ctx,
		`SELECT run_id FROM sessions WHERE session_id = ?`, sessionID).Scan(&runID); err != nil {
		return OperationReceipt{}, fmt.Errorf("query session for uncertainty resolution: %w", err)
	}

	now := time.Now().UTC()
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      codexResolveCmd,
		SessionID:        sessionID,
		CommittedVersion: 1,
		CreatedAt:        now,
		Payload:          "resolved",
	}
	if err := recordJournalEntry(tx.Tx(), opID, codexResolveCmd, fp, runID, sessionID, "",
		codexResolveKind, receipt, resolvedBy); err != nil {
		return OperationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// HasCodexCreationUncertainty reports whether the logical session
// carries an unresolved durable creation uncertainty: more recorded
// uncertainty entries than recorded resolutions. This is the cross-
// restart half of the §3.4 tombstone (the in-adapter map covers one
// process; this query covers every future one).
func (s *Store) HasCodexCreationUncertainty(ctx context.Context, sessionID string) (bool, error) {
	var uncertain, resolved int
	err := s.DB().QueryRowContext(ctx, `
SELECT
	SUM(CASE WHEN command_type = '`+codexUncertainCmd+`' THEN 1 ELSE 0 END),
	SUM(CASE WHEN command_type = '`+codexResolveCmd+`' THEN 1 ELSE 0 END)
FROM journal_entries WHERE session_id = ?`, sessionID).Scan(&uncertain, &resolved)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("query codex creation uncertainty: %w", err)
	}
	return uncertain > resolved, nil
}
