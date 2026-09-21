package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// CreateRunRequest defines the body for POST /v1/runs.
type CreateRunRequest struct {
	OpID               string                   `json:"op_id"`
	ControllerLease    string                   `json:"controller_lease"`
	RunID              string                   `json:"run_id"`
	Brief              string                   `json:"brief"`
	SourceRepoIdentity string                   `json:"source_repo_identity"`
	SourceCommit       string                   `json:"source_commit"`
	SourceTree         string                   `json:"source_tree"`
	Profile            storage.CanonicalProfile `json:"profile"`
}

// CreateRunResponse defines the response returned upon successful run creation.
type CreateRunResponse struct {
	InstanceID    string                   `json:"instance_id"`
	OpID          string                   `json:"op_id"`
	Receipt       storage.OperationReceipt `json:"receipt"`
	BriefDigest   string                   `json:"brief_digest"`
	SourceDigest  string                   `json:"source_digest"`
	ProfileDigest string                   `json:"profile_digest"`
}

// GetRunResponse defines the response returned upon querying a run's profile.
type GetRunResponse struct {
	InstanceID string                   `json:"instance_id"`
	Run        storage.RunProfileRecord `json:"run"`
}

func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req CreateRunRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), "")
		return
	}

	if strings.TrimSpace(req.OpID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "empty op_id", "")
		return
	}
	if strings.TrimSpace(req.RunID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "empty run_id", req.OpID)
		return
	}
	if strings.TrimSpace(req.ControllerLease) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "empty controller_lease", req.OpID)
		return
	}
	if strings.TrimSpace(req.Brief) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "empty brief", req.OpID)
		return
	}

	res, err := s.store.CreateRunWithProfile(r.Context(), storage.CreateRunWithProfileRequest{
		OpID:               req.OpID,
		ControllerLease:    req.ControllerLease,
		RunID:              req.RunID,
		Brief:              req.Brief,
		SourceRepoIdentity: req.SourceRepoIdentity,
		SourceCommit:       req.SourceCommit,
		SourceTree:         req.SourceTree,
		Profile:            req.Profile,
	})
	if err != nil {
		if errors.Is(err, storage.ErrIdempotencyConflict) {
			writeError(w, http.StatusConflict, "idempotency_conflict", err.Error(), req.OpID)
			return
		}
		writeError(w, http.StatusBadRequest, "create_run_failed", err.Error(), req.OpID)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(CreateRunResponse{
		InstanceID:    s.cfg.InstanceID,
		OpID:          req.OpID,
		Receipt:       res.Receipt,
		BriefDigest:   res.BriefDigest,
		SourceDigest:  res.SourceDigest,
		ProfileDigest: res.ProfileDigest,
	})
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	runID := r.PathValue("run_id")
	if strings.TrimSpace(runID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "empty run_id", "")
		return
	}

	rec, err := s.store.GetRunProfile(r.Context(), runID)
	if err != nil {
		if errors.Is(err, storage.ErrRunNotFound) {
			writeError(w, http.StatusNotFound, "run_not_found", "run not found", "")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(GetRunResponse{
		InstanceID: s.cfg.InstanceID,
		Run:        rec,
	})
}
