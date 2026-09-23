package service

// AC-008 protection-attestation journal operation (spec §3.6): the
// durable cprot-v1 attestation row is created ONLY through this
// operator-authorized operation. The operator actor is bound into the
// request and must match the attestation's recorded actor; the evidence
// itself is validated and canonically encoded by the cprot-v1 encoder
// before persistence.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/claude"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ClaudeProbeAttestationRequest carries one operator-authorized
// attestation recording: the idempotency op_id, the acting operator
// identity, and the typed cprot-v1 attestation.
type ClaudeProbeAttestationRequest struct {
	OpID string
	// OperatorToken is the operator credential (the service auth
	// token). Authority is validated against the configured service
	// token before any write; a claimed actor alone authorizes nothing.
	OperatorToken string
	// Actor is the operator identity attributed on the record and its
	// journal entry. It must match the attestation's recorded actor.
	Actor       string
	RunID       string // the run the probe was performed for (journal provenance)
	Attestation claude.ProtectionAttestation
}

// RecordClaudeProbeAttestation validates the attestation, canonically
// encodes its probe records, and persists the durable row with its
// journal entry. Fail closed: a missing op_id, a missing or mismatched
// operator actor, or any attestation invariant violation (including a
// denied=false probe record) rejects the operation before any write.
func (s *Server) RecordClaudeProbeAttestation(ctx context.Context, req ClaudeProbeAttestationRequest) (storage.OperationReceipt, error) {
	if s.store == nil {
		return storage.OperationReceipt{}, errors.New("storage store is required")
	}
	if strings.TrimSpace(req.OpID) == "" {
		return storage.OperationReceipt{}, errors.New("attestation operation requires an op_id")
	}
	// Operator authority (AC-004): the caller must present the
	// operator credential — the configured service auth token —
	// compared in constant time. A claimed actor alone authorizes
	// nothing.
	if req.OperatorToken == "" {
		return storage.OperationReceipt{}, errors.New("attestation operation requires the operator credential")
	}
	if subtle.ConstantTimeCompare([]byte(req.OperatorToken), []byte(s.cfg.AuthToken)) != 1 {
		return storage.OperationReceipt{}, errors.New("operator credential is invalid; refusing to record the attestation")
	}
	// The acting operator identity is mandatory and must match the
	// attestation's recorded actor (attribution, journal-linked).
	if strings.TrimSpace(req.Actor) == "" {
		return storage.OperationReceipt{}, errors.New("attestation operation requires the operator actor")
	}
	if strings.TrimSpace(req.RunID) == "" {
		return storage.OperationReceipt{}, errors.New("attestation operation requires the run provenance")
	}
	if strings.TrimSpace(req.Attestation.Actor) != strings.TrimSpace(req.Actor) {
		return storage.OperationReceipt{}, errors.New(
			"attestation actor does not match the requesting operator; refusing to record")
	}

	records, err := req.Attestation.EncodeProbeRecords()
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("canonical probe-record encoding: %w", err)
	}
	digest, err := req.Attestation.Digest()
	if err != nil {
		return storage.OperationReceipt{}, fmt.Errorf("canonical attestation digest: %w", err)
	}

	return s.store.RecordClaudeProtectionAttestation(ctx, req.OpID, storage.ClaudeProtectionAttestationRecord{
		AttestationID:  digest,
		RunID:          req.RunID,
		ClaudeVersion:  req.Attestation.ClaudeVersion,
		Platform:       req.Attestation.Platform,
		ManifestDigest: req.Attestation.ManifestDigest,
		TemplateDigest: req.Attestation.TemplateDigest,
		ProbeResults:   string(records),
		ProbedAt:       req.Attestation.ProbedAt,
		Actor:          req.Attestation.Actor,
	})
}
