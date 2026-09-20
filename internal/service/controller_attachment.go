package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Run-scoped controller attachment routes (AC-004 §7/§8.2). Connect and
// disconnect are explicit controller commands; ordinary HTTP request
// completion is never interpreted as disconnection.

type ControllerConnectRunRequest struct {
	OpID               string `json:"op_id"`
	ControllerLease    string `json:"controller_lease"`
	ExpectedGeneration uint64 `json:"expected_generation"`
}

type ControllerDisconnectRunRequest struct {
	OpID               string `json:"op_id"`
	ControllerLease    string `json:"controller_lease"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	AttachmentID       string `json:"attachment_id"`
}

type ControllerConnectRunResponse struct {
	InstanceID string                 `json:"instance_id"`
	OpID       string                 `json:"op_id"`
	Receipt    storage.ConnectReceipt `json:"receipt"`
}

type ControllerDisconnectRunResponse struct {
	InstanceID string                   `json:"instance_id"`
	OpID       string                   `json:"op_id"`
	Receipt    storage.OperationReceipt `json:"receipt"`
}

func (s *Server) handleRunControllerConnect(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id is required", "")
		return
	}
	var req ControllerConnectRunRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}
	if req.OpID == "" || req.ControllerLease == "" || req.ExpectedGeneration == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "op_id, controller_lease, and expected_generation (>0) are required", req.OpID)
		return
	}
	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	receipt, err := s.store.ConnectRunController(r.Context(), req.OpID, runID, req.ControllerLease, req.ExpectedGeneration)
	if err != nil {
		s.writeGrantError(w, err, req.OpID)
		return
	}

	// Only a validated connect transition in this instance establishes the
	// in-instance attachment gate.
	s.coordinator.MarkControllerAttached(runID, req.ExpectedGeneration, receipt.AttachmentID, s.cfg.InstanceID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ControllerConnectRunResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		Receipt:    receipt,
	})
}

func (s *Server) handleRunControllerDisconnect(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	if runID == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id is required", "")
		return
	}
	var req ControllerDisconnectRunRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}
	if req.OpID == "" || req.ControllerLease == "" || req.ExpectedGeneration == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "op_id, controller_lease, and expected_generation (>0) are required", req.OpID)
		return
	}
	doneControl, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", "service is stopping", req.OpID)
		return
	}
	defer doneControl()

	receipt, err := s.store.DisconnectRunController(r.Context(), req.OpID, runID, req.ControllerLease, req.AttachmentID, req.ExpectedGeneration)
	if err != nil {
		s.writeGrantError(w, err, req.OpID)
		return
	}

	// The durable state is authoritative for the episode outcome; clearing
	// the in-instance record follows the same transition.
	if receipt.Payload == "disconnected" {
		s.coordinator.InvalidateControllerAttachment(runID)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ControllerDisconnectRunResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		Receipt:    receipt,
	})
}

// writeGrantError maps credential classification and generation errors from
// attachment transitions.
func (s *Server) writeGrantError(w http.ResponseWriter, err error, opID string) {
	if writeControllerAuthError(w, err, opID) {
		return
	}
	if errors.Is(err, storage.ErrGenerationMismatch) {
		writeError(w, http.StatusConflict, "generation_mismatch", err.Error(), opID)
		return
	}
	if errors.Is(err, storage.ErrSessionArchived) {
		writeError(w, http.StatusConflict, "session_archived", err.Error(), opID)
		return
	}
	writeError(w, http.StatusBadRequest, "attachment_failed", err.Error(), opID)
}

// requireConnectedController enforces the new-decision gate at the service
// mutation boundary: the presented credential is classified first (a
// superseded or unadopted caller learns that, not a connection complaint),
// then the run's controller must be durably connected and attached in this
// service instance. Read-only inspection and reconnect never pass through
// here.
func (s *Server) requireConnectedController(w http.ResponseWriter, r *http.Request, runID, sessionID, presentedLease, opID string) bool {
	var classifyErr error
	if sessionID != "" {
		classifyErr = s.store.ValidateControllerLease(r.Context(), sessionID, presentedLease)
	} else {
		classifyErr = s.store.ValidateRunControllerLease(r.Context(), runID, presentedLease)
	}
	if classifyErr != nil {
		if writeControllerAuthError(w, classifyErr, opID) {
			return false
		}
		writeError(w, http.StatusInternalServerError, "storage_error", classifyErr.Error(), opID)
		return false
	}

	rec, err := s.store.GetControllerRecord(r.Context(), runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error", err.Error(), opID)
		return false
	}
	if !rec.Adopted {
		writeError(w, http.StatusConflict, "adoption_required", "run has no adopted controller", opID)
		return false
	}
	if !rec.Connected || !s.coordinator.ControllerAttached(runID, rec.Generation) {
		writeError(w, http.StatusConflict, "not_connected", "current controller must attach to this service instance before new decisions", opID)
		return false
	}
	return true
}
