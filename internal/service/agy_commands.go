package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// AgyAttestationRequest is an operator-only transport envelope. Authority
// comes from the authenticated bearer header, never from a JSON credential.
type AgyAttestationRequest struct {
	OpID          string                      `json:"op_id"`
	Actor         string                      `json:"actor"`
	AttestationID string                      `json:"attestation_id,omitempty"`
	Attestation   codex.ProtectionAttestation `json:"attestation"`
}

type AgyOperationResponse struct {
	InstanceID string                   `json:"instance_id"`
	OpID       string                   `json:"op_id"`
	Receipt    storage.OperationReceipt `json:"receipt"`
}

func (s *Server) handleAgyAttestation(w http.ResponseWriter, r *http.Request) {
	var req AgyAttestationRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}
	done, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", err.Error(), req.OpID)
		return
	}
	defer done()
	receipt, err := s.RecordAgyProbeAttestation(r.Context(), AgyProbeAttestationRequest{
		OpID: req.OpID, OperatorToken: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), Actor: req.Actor,
		RunID: r.PathValue("run_id"), AttestationID: req.AttestationID, Attestation: req.Attestation,
	})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, storage.ErrIdempotencyConflict) {
			status = http.StatusConflict
		}
		writeError(w, status, "attestation_failed", err.Error(), req.OpID)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(AgyOperationResponse{s.cfg.InstanceID, req.OpID, receipt})
}

type AgyTurnDispositionRequest struct {
	OpID               string `json:"op_id"`
	ControllerLease    string `json:"controller_lease"`
	ExpectedGeneration uint64 `json:"expected_generation"`
	ExpectedVersion    int64  `json:"expected_version"`
	AttemptID          string `json:"attempt_id"`
	Disposition        string `json:"disposition"`
	Reason             string `json:"reason"`
}

func (s *Server) handleAgyTurnDisposition(w http.ResponseWriter, r *http.Request) {
	runID, sessionID, turnKey := r.PathValue("run_id"), r.PathValue("session_id"), r.PathValue("turn_key")
	var req AgyTurnDispositionRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}
	if err := s.store.ValidateSessionRun(r.Context(), sessionID, runID); err != nil {
		writeError(w, http.StatusNotFound, "session_not_found", err.Error(), req.OpID)
		return
	}
	done, err := s.coordinator.TrackControl()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", err.Error(), req.OpID)
		return
	}
	defer done()
	if !s.requireConnectedController(w, r, runID, sessionID, req.ControllerLease, req.OpID) {
		return
	}
	// Before init/dispatch completes, a child can exist without an adapter
	// live-run entry. A locally owned worker must have durable retirement
	// evidence for its actual attempt, never an attempt chosen by the caller.
	if s.coordinator.LiveWorkerKeys()[sessionID+":"+turnKey] {
		ref := s.store.ExecutionRefForTurn(r.Context(), sessionID, turnKey)
		states, err := s.store.AgyAttemptLaunchStates(r.Context(), ref.AttemptID)
		if err != nil || len(states) != 1 || (states[0] != "dead" && states[0] != "start_failed") {
			writeError(w, http.StatusConflict, "execution_active", "the local launch has not retired", req.OpID)
			return
		}
	}
	// An uncertain row also exists during normal execution. The supervisor
	// may still be polling a dead attempt, so worker presence alone is not
	// native liveness. Agy Collect returns Pending while its run is live.
	if s.adapter != nil {
		result, err := s.adapter.Collect(r.Context(), adapter.TurnRef{SessionID: adapter.SessionID(sessionID), TurnKey: turnKey})
		if err != nil || result.ResultStatus == adapter.ResultPending {
			writeError(w, http.StatusConflict, "execution_active", "cannot establish that the local execution has retired", req.OpID)
			return
		}
	}
	receipt, err := s.store.ResolveAgyTurnUncertainty(r.Context(), req.OpID, req.ControllerLease, req.ExpectedGeneration, req.ExpectedVersion, storage.ExecutionRef{SessionID: sessionID, TurnKey: turnKey, AttemptID: req.AttemptID}, req.Disposition, req.Reason)
	if err != nil {
		if writeControllerAuthError(w, err, req.OpID) {
			return
		}
		status := http.StatusBadRequest
		if errors.Is(err, storage.ErrStaleUpdate) || errors.Is(err, storage.ErrIdempotencyConflict) {
			status = http.StatusConflict
		}
		writeError(w, status, "disposition_failed", err.Error(), req.OpID)
		return
	}
	s.refreshDiagnosticsBound(r.Context())
	data, _ := json.Marshal(map[string]any{"session_id": sessionID, "turn_key": turnKey, "status": council.TurnInterrupted})
	s.coordinator.BroadcastEvent(adapter.TurnRef{SessionID: adapter.SessionID(sessionID), TurnKey: turnKey}, SSEEvent{Event: "terminal", Data: string(data)})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(AgyOperationResponse{s.cfg.InstanceID, req.OpID, receipt})
}
