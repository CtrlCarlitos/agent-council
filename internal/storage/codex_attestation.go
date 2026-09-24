package storage

// AC-009 durable cprot-v2 protection-attestation records and the
// creation-uncertainty EPISODE journal (spec §3.7/§3.4): the attestation
// row is created only by an operator-authorized journal operation —
// idempotent by op_id with receipt replay, the attestation identity
// (cprot-v2 digest) unique, its record coverage validated by the
// service against the run's frozen profile — and an uncertain SESSION
// creation is recorded durably as a numbered episode so a service
// restart can never silently re-create a native thread for it (§3.4:
// automatic recreation blocked until a controller-authorized resolution
// of that exact episode).

import (
	"context"
	"database/sql"
	"encoding/json"
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

// ── Durable creation uncertainty (§3.4) — episodes ─────────────────────

// CodexCreationUncertainty is the durable record request for a session
// creation whose native thread MAY have been created: automatic
// recreation is blocked across service restarts until an explicit,
// controller-authorized resolution of that exact episode.
type CodexCreationUncertainty struct {
	RunID      string
	SessionID  string
	Reason     string
	RecordedBy string // the identity attributed in the journal entry (the adapter)
	CauseOpID  string // the creation operation that ended uncertain (provenance)
}

// CodexCreationUncertaintyEpisode is one durable uncertainty episode of
// a logical session. Episodes are numbered monotonically per session;
// at most one is open (unresolved) at a time — the schema enforces it.
type CodexCreationUncertaintyEpisode struct {
	SessionID            string
	Episode              int64
	RunID                string
	Reason               string
	RecordedBy           string
	RecordOpID           string
	CauseOpID            string
	RecordedAt           time.Time
	Disposition          *string
	ResolutionReason     *string
	ResolutionGeneration *uint64
	ResolutionOpID       *string
	ResolvedAt           *time.Time
}

// Dispositions a controller may record for an open episode. Both permit
// a fresh creation afterwards; they differ in what the controller
// asserts about the possibly-orphaned native thread.
const (
	// CodexUncertaintyAbandonOrphan: the native thread, if it exists,
	// is abandoned (thread/start consumed no provider quota; operator
	// purge is a separate action).
	CodexUncertaintyAbandonOrphan = "abandon_orphan"
	// CodexUncertaintyVerifiedAbsent: the controller verified out of
	// band (e.g. a diagnostic thread/list sweep) that no native thread
	// was created.
	CodexUncertaintyVerifiedAbsent = "verified_absent"
)

const (
	codexUncertainCmd  = "record_codex_creation_uncertain"
	codexUncertainKind = "codex_creation_uncertain"
	codexResolveCmd    = "resolve_codex_creation_uncertain"
	codexResolveKind   = "codex_creation_uncertainty_resolved"
)

// codexUncertaintyOpID derives the deterministic journal op id of one
// episode: the episode number, not the caller's op id, is the identity,
// so N concurrent callers sharing one uncertain creation record ONE
// episode, and a replay after a crash re-records the same fact.
func codexUncertaintyOpID(sessionID string, episode int64) string {
	return fmt.Sprintf("op-codex-creation-uncertain-%s-e%d", sessionID, episode)
}

// RecordCodexCreationUncertain durably opens an uncertainty episode for
// the session, or returns the already-open episode's receipt: while an
// episode is open, creation is blocked, so a second uncertain outcome
// cannot be a distinct event. After a resolution, a NEW uncertain
// outcome opens the next episode (monotonic), never replays the old
// one. Returns the episode number with the committed receipt.
func (s *Store) RecordCodexCreationUncertain(ctx context.Context, rec CodexCreationUncertainty) (int64, OperationReceipt, error) {
	if strings.TrimSpace(rec.SessionID) == "" || strings.TrimSpace(rec.RunID) == "" {
		return 0, OperationReceipt{}, errors.New("creation-uncertainty record requires the session and run provenance")
	}
	if strings.TrimSpace(rec.Reason) == "" {
		return 0, OperationReceipt{}, errors.New("creation-uncertainty record requires the reason")
	}
	if strings.TrimSpace(rec.RecordedBy) == "" || strings.TrimSpace(rec.CauseOpID) == "" {
		return 0, OperationReceipt{}, errors.New("creation-uncertainty record requires the recording identity and the causing operation id")
	}

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return 0, OperationReceipt{}, err
	}
	defer tx.Rollback()

	// Open episode: the fact is already durable — replay its receipt.
	var openEpisode int64
	var openOpID string
	err = tx.Tx().QueryRowContext(ctx, `
SELECT episode, record_op_id FROM codex_creation_uncertainties
WHERE session_id = ? AND disposition IS NULL`, rec.SessionID).Scan(&openEpisode, &openOpID)
	if err == nil {
		receipt, err := journalReceipt(ctx, tx.Tx(), openOpID)
		if err != nil {
			return 0, OperationReceipt{}, err
		}
		return openEpisode, receipt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, OperationReceipt{}, fmt.Errorf("query open creation uncertainty: %w", err)
	}

	var maxEpisode sql.NullInt64
	if err := tx.Tx().QueryRowContext(ctx,
		`SELECT MAX(episode) FROM codex_creation_uncertainties WHERE session_id = ?`, rec.SessionID).Scan(&maxEpisode); err != nil {
		return 0, OperationReceipt{}, fmt.Errorf("query creation uncertainty episodes: %w", err)
	}
	episode := int64(1)
	if maxEpisode.Valid {
		episode = maxEpisode.Int64 + 1
	}
	opID := codexUncertaintyOpID(rec.SessionID, episode)
	fp := computeFingerprint(codexUncertainCmd, rec.SessionID, rec.RunID, fmt.Sprintf("%d", episode), rec.Reason)
	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, rec.RecordedBy, codexUncertainCmd, fp); err != nil {
		return 0, OperationReceipt{}, err
	} else if receipt != nil {
		// A journal entry without its episode row cannot exist (same
		// transaction); a stale match here is a corrupt store.
		return 0, OperationReceipt{}, fmt.Errorf("creation-uncertainty journal entry %s exists without its episode row", opID)
	}

	now := time.Now().UTC()
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      codexUncertainCmd,
		SessionID:        rec.SessionID,
		CommittedVersion: episode,
		CreatedAt:        now,
		Payload:          rec.Reason,
	}
	if err := recordJournalEntry(tx.Tx(), opID, codexUncertainCmd, fp, rec.RunID, rec.SessionID, "",
		codexUncertainKind, receipt, rec.RecordedBy); err != nil {
		return 0, OperationReceipt{}, err
	}
	if _, err := tx.Tx().ExecContext(ctx, `
INSERT INTO codex_creation_uncertainties
	(session_id, episode, run_id, reason, recorded_by, record_op_id, cause_op_id, recorded_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.SessionID, episode, rec.RunID, rec.Reason, rec.RecordedBy, opID, rec.CauseOpID,
		now.Format(time.RFC3339Nano)); err != nil {
		return 0, OperationReceipt{}, fmt.Errorf("insert creation uncertainty episode: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, OperationReceipt{}, err
	}
	return episode, receipt, nil
}

// journalReceipt loads the committed receipt of a journal entry.
func journalReceipt(ctx context.Context, tx *sql.Tx, opID string) (OperationReceipt, error) {
	var payloadJSON string
	if err := tx.QueryRowContext(ctx,
		`SELECT payload_json FROM journal_entries WHERE op_id = ?`, opID).Scan(&payloadJSON); err != nil {
		return OperationReceipt{}, fmt.Errorf("query journal entry %s: %w", opID, err)
	}
	var jp journalPayload
	if err := json.Unmarshal([]byte(payloadJSON), &jp); err != nil {
		return OperationReceipt{}, fmt.Errorf("unmarshal journal entry %s: %w", opID, err)
	}
	return jp.Receipt, nil
}

// ResolveCodexCreationUncertainty durably resolves ONE exact open
// episode under CURRENT controller authority: the presented lease must
// be the run's active adopted controller credential at the expected
// generation, re-validated inside the write transaction (a superseded
// or revoked lease, or a stale generation, is refused). The disposition
// and reason are journaled; the operation is idempotent by op_id with
// receipt replay. An unknown or already-resolved episode is refused —
// automatic retries never reach this seam.
func (s *Store) ResolveCodexCreationUncertainty(ctx context.Context, opID, callerLease string, expectedGeneration uint64, sessionID string, episode int64, disposition, reason string) (OperationReceipt, error) {
	if strings.TrimSpace(opID) == "" || strings.TrimSpace(callerLease) == "" {
		return OperationReceipt{}, errors.New("creation-uncertainty resolution requires the operation id and the controller lease")
	}
	if strings.TrimSpace(sessionID) == "" || episode < 1 {
		return OperationReceipt{}, errors.New("creation-uncertainty resolution requires the session id and the episode")
	}
	switch disposition {
	case CodexUncertaintyAbandonOrphan, CodexUncertaintyVerifiedAbsent:
	default:
		return OperationReceipt{}, fmt.Errorf("creation-uncertainty resolution disposition %q is not one of %s|%s",
			disposition, CodexUncertaintyAbandonOrphan, CodexUncertaintyVerifiedAbsent)
	}
	if strings.TrimSpace(reason) == "" {
		return OperationReceipt{}, errors.New("creation-uncertainty resolution requires the reason")
	}

	fp := computeFingerprint(codexResolveCmd, sessionID, fmt.Sprintf("%d", episode), disposition, reason)

	tx, err := s.BeginWrite(ctx)
	if err != nil {
		return OperationReceipt{}, err
	}
	defer tx.Rollback()

	var runID string
	if err := tx.Tx().QueryRowContext(ctx,
		`SELECT run_id FROM sessions WHERE session_id = ?`, sessionID).Scan(&runID); err != nil {
		return OperationReceipt{}, fmt.Errorf("query session for uncertainty resolution: %w", err)
	}
	gen, err := classifyCredential(ctx, tx.Tx(), runID, callerLease, true)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("controller authority: %w", err)
	}
	if gen != expectedGeneration {
		return OperationReceipt{}, fmt.Errorf("controller authority: expected generation %d, active generation is %d", expectedGeneration, gen)
	}
	if receipt, err := checkOrRecordIdempotency(tx.Tx(), opID, callerLease, codexResolveCmd, fp); err != nil {
		return OperationReceipt{}, err
	} else if receipt != nil {
		return *receipt, nil
	}

	var existing sql.NullString
	err = tx.Tx().QueryRowContext(ctx, `
SELECT disposition FROM codex_creation_uncertainties
WHERE session_id = ? AND episode = ?`, sessionID, episode).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return OperationReceipt{}, fmt.Errorf("session %s has no creation-uncertainty episode %d", sessionID, episode)
	}
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("query creation uncertainty episode: %w", err)
	}
	if existing.Valid {
		return OperationReceipt{}, fmt.Errorf("session %s creation-uncertainty episode %d is already resolved (%s)",
			sessionID, episode, existing.String)
	}

	now := time.Now().UTC()
	receipt := OperationReceipt{
		OpID:             opID,
		CommandType:      codexResolveCmd,
		SessionID:        sessionID,
		CommittedVersion: episode,
		CreatedAt:        now,
		Payload:          disposition,
	}
	if err := recordJournalEntry(tx.Tx(), opID, codexResolveCmd, fp, runID, sessionID, "",
		codexResolveKind, receipt, callerLease); err != nil {
		return OperationReceipt{}, err
	}
	res, err := tx.Tx().ExecContext(ctx, `
UPDATE codex_creation_uncertainties
SET disposition = ?, resolution_reason = ?, resolution_generation = ?, resolution_op_id = ?, resolved_at = ?
WHERE session_id = ? AND episode = ? AND disposition IS NULL`,
		disposition, reason, gen, opID, now.Format(time.RFC3339Nano), sessionID, episode)
	if err != nil {
		return OperationReceipt{}, fmt.Errorf("resolve creation uncertainty episode: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return OperationReceipt{}, fmt.Errorf("resolve creation uncertainty episode: expected one open row, updated %d (%v)", n, err)
	}
	if err := tx.Commit(); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

// HasCodexCreationUncertainty reports whether the logical session has an
// OPEN creation-uncertainty episode. This is the cross-restart half of
// the §3.4 tombstone (the in-adapter map covers one process; this row
// covers every future one).
func (s *Store) HasCodexCreationUncertainty(ctx context.Context, sessionID string) (bool, error) {
	ep, err := s.OpenCodexCreationUncertainty(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return ep != nil, nil
}

// OpenCodexCreationUncertainty returns the session's open episode, or
// nil when none is open — the controller resolves exactly this one.
func (s *Store) OpenCodexCreationUncertainty(ctx context.Context, sessionID string) (*CodexCreationUncertaintyEpisode, error) {
	row := s.DB().QueryRowContext(ctx, `
SELECT session_id, episode, run_id, reason, recorded_by, record_op_id, cause_op_id, recorded_at,
       disposition, resolution_reason, resolution_generation, resolution_op_id, resolved_at
FROM codex_creation_uncertainties WHERE session_id = ? AND disposition IS NULL`, sessionID)
	return scanCodexUncertaintyEpisode(row)
}

// CodexCreationUncertaintyEpisodes returns every episode of a session in
// episode order (evidence: the resolved history is never deleted).
func (s *Store) CodexCreationUncertaintyEpisodes(ctx context.Context, sessionID string) ([]CodexCreationUncertaintyEpisode, error) {
	rows, err := s.DB().QueryContext(ctx, `
SELECT session_id, episode, run_id, reason, recorded_by, record_op_id, cause_op_id, recorded_at,
       disposition, resolution_reason, resolution_generation, resolution_op_id, resolved_at
FROM codex_creation_uncertainties WHERE session_id = ? ORDER BY episode`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query creation uncertainty episodes: %w", err)
	}
	defer rows.Close()
	var out []CodexCreationUncertaintyEpisode
	for rows.Next() {
		ep, err := scanCodexUncertaintyEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ep)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanCodexUncertaintyEpisode(row rowScanner) (*CodexCreationUncertaintyEpisode, error) {
	var ep CodexCreationUncertaintyEpisode
	var recordedAt string
	var disposition, resolutionReason, resolutionOpID, resolvedAt sql.NullString
	var resolutionGen sql.NullInt64
	if err := row.Scan(&ep.SessionID, &ep.Episode, &ep.RunID, &ep.Reason, &ep.RecordedBy, &ep.RecordOpID,
		&ep.CauseOpID, &recordedAt, &disposition, &resolutionReason, &resolutionGen, &resolutionOpID, &resolvedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan creation uncertainty episode: %w", err)
	}
	ep.RecordedAt, _ = time.Parse(time.RFC3339Nano, recordedAt)
	ep.Disposition = nullStr(disposition)
	ep.ResolutionReason = nullStr(resolutionReason)
	ep.ResolutionOpID = nullStr(resolutionOpID)
	if resolutionGen.Valid {
		g := uint64(resolutionGen.Int64)
		ep.ResolutionGeneration = &g
	}
	if resolvedAt.Valid {
		if t, err := time.Parse(time.RFC3339Nano, resolvedAt.String); err == nil {
			ep.ResolvedAt = &t
		}
	}
	return &ep, nil
}
