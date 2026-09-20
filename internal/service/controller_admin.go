package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Operator-administered controller lease endpoints (AC-004 §8.2). All are
// behind the operator bearer transport; operation-specific authorization
// inputs (current lease, expected generation, verified recovery intent)
// follow the authorization matrix. Secret candidates are generated here —
// once per committed operation, never idempotency inputs (storage resolves
// the original issuance on replay). Lease secrets appear only in the
// adopt/handoff/recover success responses, never in records, status, or
// error envelopes.

type ControllerAdoptRequest struct {
	OpID               string `json:"op_id"`
	Harness            string `json:"harness"`
	ControllerRef      string `json:"controller_ref"`
	BootstrapLease     string `json:"bootstrap_lease"`
	OperatorRecovery   bool   `json:"operator_recovery"`
	Reason             string `json:"reason"`
	ExpectedGeneration uint64 `json:"expected_generation"`
}

type ControllerHandoffRequest struct {
	OpID               string `json:"op_id"`
	CurrentLease       string `json:"current_lease"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	Harness            string `json:"harness"`
	ControllerRef      string `json:"controller_ref"`
}

type ControllerRevokeRequest struct {
	OpID               string `json:"op_id"`
	ControllerLease    string `json:"controller_lease"`
	OperatorRecovery   bool   `json:"operator_recovery"`
	Reason             string `json:"reason"`
	ExpectedGeneration uint64 `json:"expected_generation"`
}

type ControllerCredentialRecoverRequest struct {
	OpID               string `json:"op_id"`
	SourceOpID         string `json:"source_op_id"`
	ExpectedGeneration uint64 `json:"expected_generation"`
}

type ControllerGrantResponse struct {
	InstanceID  string `json:"instance_id"`
	OpID        string `json:"op_id"`
	Generation  uint64 `json:"generation"`
	LeaseSecret string `json:"lease_secret"`
	Payload     string `json:"payload"`
	Replayed    bool   `json:"replayed"`
}

type ControllerRecordResponse struct {
	InstanceID    string `json:"instance_id"`
	Generation    uint64 `json:"generation"`
	Harness       string `json:"harness"`
	ControllerRef string `json:"controller_ref"`
	Status        string `json:"status"`
	Adopted       bool   `json:"adopted"`
	Connected     bool   `json:"connected"`
	// AttachmentID is the current controller's own connection episode —
	// never a lease secret — needed to address its own disconnect.
	AttachmentID string `json:"attachment_id,omitempty"`
}

// newLeaseCandidate generates one random candidate per request attempt.
// Storage resolves the original committed issuance on replay, so an unused
// candidate is discarded rather than installed or disclosed.
func newLeaseCandidate() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate lease candidate: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func (s *Server) handleControllerRecordGet(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id is required", "")
		return
	}
	rec, err := s.store.GetControllerRecord(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ControllerRecordResponse{
		InstanceID:    s.cfg.InstanceID,
		Generation:    rec.Generation,
		Harness:       rec.Harness,
		ControllerRef: rec.ControllerRef,
		Status:        rec.Status,
		Adopted:       rec.Adopted,
		Connected:     rec.Connected,
		AttachmentID:  rec.AttachmentID,
	})
}

func (s *Server) handleControllerAdopt(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id is required", "")
		return
	}
	var req ControllerAdoptRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}
	if req.OpID == "" || req.Harness == "" || req.ControllerRef == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "op_id, harness, and controller_ref are required", req.OpID)
		return
	}
	if req.OperatorRecovery && strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "operator recovery requires an explicit reason", req.OpID)
		return
	}
	if !req.OperatorRecovery && req.BootstrapLease == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "bootstrap_lease or operator_recovery is required", req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	var recovery *storage.OperatorRecovery
	if req.OperatorRecovery {
		recovery = &storage.OperatorRecovery{Reason: req.Reason, ExpectedGeneration: req.ExpectedGeneration}
	}
	candidate, err := newLeaseCandidate()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot generate credential", req.OpID)
		return
	}
	grant, err := s.store.AdoptController(r.Context(), req.OpID, runID, req.Harness, req.ControllerRef, req.BootstrapLease, recovery, candidate)
	if err != nil {
		s.writeGrantAdminError(w, err, req.OpID)
		return
	}
	s.writeGrantResponse(w, grant)
}

func (s *Server) handleControllerHandoff(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id is required", "")
		return
	}
	var req ControllerHandoffRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}
	if req.OpID == "" || req.CurrentLease == "" || req.ExpectedGeneration == 0 || req.Harness == "" || req.ControllerRef == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "op_id, current_lease, expected_generation (>0), harness, and controller_ref are required", req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	candidate, err := newLeaseCandidate()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "cannot generate credential", req.OpID)
		return
	}
	grant, err := s.store.HandoffController(r.Context(), req.OpID, runID, req.CurrentLease, req.ExpectedGeneration, req.Harness, req.ControllerRef, candidate)
	if err != nil {
		s.writeGrantAdminError(w, err, req.OpID)
		return
	}
	// A new grant deliberately carries no connection state; its attachment
	// record is invalidated in this instance.
	s.coordinator.InvalidateControllerAttachment(runID)
	s.writeGrantResponse(w, grant)
}

func (s *Server) handleControllerRevoke(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id is required", "")
		return
	}
	var req ControllerRevokeRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}
	if req.OpID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "op_id is required", req.OpID)
		return
	}
	if req.OperatorRecovery && strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "operator recovery requires an explicit reason", req.OpID)
		return
	}
	if !req.OperatorRecovery && req.ControllerLease == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "controller_lease or operator_recovery is required", req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	var recovery *storage.OperatorRecovery
	if req.OperatorRecovery {
		recovery = &storage.OperatorRecovery{Reason: req.Reason, ExpectedGeneration: req.ExpectedGeneration}
	}
	receipt, err := s.store.RevokeController(r.Context(), req.OpID, runID, req.ControllerLease, recovery)
	if err != nil {
		s.writeGrantAdminError(w, err, req.OpID)
		return
	}
	s.coordinator.InvalidateControllerAttachment(runID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"instance_id": s.cfg.InstanceID,
		"op_id":       req.OpID,
		"payload":     receipt.Payload,
	})
}

func (s *Server) handleControllerCredentialRecover(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id is required", "")
		return
	}
	var req ControllerCredentialRecoverRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}
	if req.OpID == "" || req.SourceOpID == "" || req.ExpectedGeneration == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "op_id, source_op_id, and expected_generation (>0) are required", req.OpID)
		return
	}

	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	grant, err := s.store.RecoverControllerCredential(r.Context(), req.OpID, runID, req.SourceOpID, req.ExpectedGeneration)
	if err != nil {
		s.writeGrantAdminError(w, err, req.OpID)
		return
	}
	s.writeGrantResponse(w, grant)
}

func (s *Server) writeGrantResponse(w http.ResponseWriter, grant storage.ControllerGrantReceipt) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ControllerGrantResponse{
		InstanceID:  s.cfg.InstanceID,
		OpID:        grant.OpID,
		Generation:  grant.Generation,
		LeaseSecret: grant.LeaseSecret,
		Payload:     grant.Payload,
		Replayed:    grant.ReplayedReceipt,
	})
}

// writeGrantAdminError maps grant-administration classification errors.
func (s *Server) writeGrantAdminError(w http.ResponseWriter, err error, opID string) {
	if writeControllerAuthError(w, err, opID) {
		return
	}
	switch {
	case errors.Is(err, storage.ErrGenerationMismatch):
		writeError(w, http.StatusConflict, "generation_mismatch", err.Error(), opID)
	case errors.Is(err, storage.ErrAdoptionExists):
		writeError(w, http.StatusConflict, "adoption_exists", err.Error(), opID)
	case errors.Is(err, storage.ErrRecoveryUnavailable):
		writeError(w, http.StatusConflict, "recovery_unavailable", err.Error(), opID)
	case errors.Is(err, storage.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error(), opID)
	default:
		writeError(w, http.StatusBadRequest, "grant_admin_failed", err.Error(), opID)
	}
}
