package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ReleaseRequest represents the JSON request payload for releasing a turn.
type ReleaseRequest struct {
	InstanceID      string `json:"instance_id"`
	OpID            string `json:"op_id"`
	ControllerLease string `json:"controller_lease"`
	ExpectedVersion int64  `json:"expected_version"`
}

// ReleaseResponse represents the JSON response for turn release.
type ReleaseResponse struct {
	InstanceID string                 `json:"instance_id"`
	OpID       string                 `json:"op_id"`
	TurnURL    string                 `json:"turn_url"`
	Receipt    storage.ReleaseReceipt `json:"receipt"`
	Replayed   bool                   `json:"replayed"`
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	runID := strings.TrimSpace(r.PathValue("run_id"))
	sessionID := strings.TrimSpace(r.PathValue("session_id"))
	turnKey := strings.TrimSpace(r.PathValue("turn_key"))

	if runID == "" || sessionID == "" || turnKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_path", "run_id, session_id, and turn_key are required", "")
		return
	}

	var req ReleaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}

	if req.OpID == "" {
		writeError(w, http.StatusBadRequest, "missing_op_id", "op_id is required", "")
		return
	}
	if req.ControllerLease == "" {
		writeError(w, http.StatusBadRequest, "missing_controller_lease", "controller_lease is required", req.OpID)
		return
	}
	if req.ExpectedVersion <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_expected_version", "expected_version must be positive", req.OpID)
		return
	}
	if req.InstanceID != "" && req.InstanceID != s.cfg.InstanceID {
		writeError(w, http.StatusConflict, "instance_mismatch", fmt.Sprintf("instance_id %q does not match current instance %q", req.InstanceID, s.cfg.InstanceID), req.OpID)
		return
	}

	// 1. Idempotent Retry Resolution:
	// Check if op_id has already been committed as a release_turn operation.
	// If found, return 200 OK immediately without checking adapter availability or draining state.
	existingReceipt, found, err := s.store.FindCommittedRelease(r.Context(), req.OpID, req.ControllerLease, sessionID, turnKey)
	if err != nil {
		if errors.Is(err, storage.ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, "idempotency_conflict", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), req.OpID)
		return
	}
	if found {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ReleaseResponse{
			InstanceID: s.cfg.InstanceID,
			OpID:       req.OpID,
			TurnURL:    fmt.Sprintf("/v1/runs/%s/sessions/%s/turns/%s", runID, sessionID, turnKey),
			Receipt:    *existingReceipt,
			Replayed:   true,
		})
		return
	}

	// 2. Admission Coordination & Pre-flight Availability Check (New Releases Only):
	// Under admission lock, if the service is draining or stopping, reject.
	if s.coordinator.IsDrainingOrStopping() {
		writeError(w, http.StatusServiceUnavailable, "service_draining", "service is draining new work", req.OpID)
		return
	}

	// Verify required harness adapter is available.
	if s.adapter == nil {
		writeError(w, http.StatusServiceUnavailable, "harness_unavailable", "harness adapter is unavailable", req.OpID)
		return
	}

	// 3. Atomic Execution Hand-off:
	relResult, err := s.store.ReleaseTurn(r.Context(), req.OpID, req.ControllerLease, sessionID, req.ExpectedVersion, turnKey)
	if err != nil {
		if errors.Is(err, storage.ErrStaleUpdate) {
			writeError(w, http.StatusConflict, "stale_version", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrUnauthorizedOperation) {
			writeError(w, http.StatusForbidden, "unauthorized", err.Error(), req.OpID)
			return
		}
		if errors.Is(err, storage.ErrPromptNotQueued) {
			writeError(w, http.StatusNotFound, "prompt_not_queued", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusBadRequest, "release_failed", err.Error(), req.OpID)
		return
	}

	turnURL := fmt.Sprintf("/v1/runs/%s/sessions/%s/turns/%s", runID, sessionID, turnKey)

	if relResult.Disposition == storage.ReleaseDispositionReplayed {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ReleaseResponse{
			InstanceID: s.cfg.InstanceID,
			OpID:       req.OpID,
			TurnURL:    turnURL,
			Receipt:    relResult.Receipt,
			Replayed:   true,
		})
		return
	}

	// ReleaseDispositionNew: register worker under detached coordinator context
	ref := adapter.TurnRef{
		SessionID: adapter.SessionID(sessionID),
		TurnKey:   turnKey,
	}
	workerCtx, done, err := s.coordinator.RegisterWorker(ref)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_stopping", err.Error(), req.OpID)
		return
	}

	supervisor := NewExecutionSupervisor(
		s.store,
		s.adapter,
		s.coordinator,
		runID,
		sessionID,
		turnKey,
		req.ControllerLease,
		relResult.Receipt.CommittedVersion,
		relResult.Receipt,
		done,
	)
	go supervisor.Run(workerCtx)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(ReleaseResponse{
		InstanceID: s.cfg.InstanceID,
		OpID:       req.OpID,
		TurnURL:    turnURL,
		Receipt:    relResult.Receipt,
		Replayed:   false,
	})
}
