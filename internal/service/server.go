package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/opencode"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

type ServerConfig struct {
	StateDir         string
	InstanceID       string
	AuthToken        string
	WorkspaceBaseDir string
	// OpenCodeBinaryPath, when set, enables the OpenCode persistent
	// contributor adapter via production seams backed by the storage
	// store and AC-005 workspace manager. Empty means no OpenCode
	// adapter.
	OpenCodeBinaryPath string
}

type ReadinessResponse struct {
	Status          string `json:"status"`
	InstanceID      string `json:"instance_id"`
	ProtocolVersion int    `json:"protocol_version"`
	StateDir        string `json:"state_dir"`
	LiveWorkers     int    `json:"live_workers"`
	ReservedTurns   int    `json:"reserved_turns"`
	UnresolvedTurns int    `json:"unresolved_turns"`
}

type StatusResponse struct {
	InstanceID      string    `json:"instance_id"`
	PID             int       `json:"pid"`
	Status          string    `json:"status"`
	StateDir        string    `json:"state_dir"`
	StartedAt       time.Time `json:"started_at"`
	ActiveRuns      []string  `json:"active_runs"`
	LiveWorkers     int       `json:"live_workers"`
	ReservedTurns   int       `json:"reserved_turns"`
	UnresolvedTurns int       `json:"unresolved_turns"`
}

type Server struct {
	store            *storage.Store
	lock             *ServiceLock
	cfg              ServerConfig
	coordinator      *Coordinator
	adapter          adapter.Adapter
	workspaceManager *workspace.WorkspaceManager
	policyExecutor   execpolicy.PolicyExecutor
	listener         net.Listener
	httpServer       *http.Server
	socketPath       string
	tokenPath        string
	startedAt        time.Time

	mu           sync.Mutex
	running      bool
	shutdown     chan struct{}
	teardownOnce sync.Once
	teardownErr  error
}

func NewServer(store *storage.Store, lock *ServiceLock, cfg ServerConfig) (*Server, error) {
	return NewServerWithAdapter(store, lock, cfg, nil)
}

func NewServerWithAdapter(store *storage.Store, lock *ServiceLock, cfg ServerConfig, adp adapter.Adapter) (*Server, error) {
	// If no adapter is provided but OpenCode is configured, construct the
	// production OpenCode adapter with fail-closed seams.
	if adp == nil && strings.TrimSpace(cfg.OpenCodeBinaryPath) != "" {
		wm, wmErr := workspace.NewWorkspaceManager(cfg.StateDir, filepath.Join(cfg.StateDir, "workspaces"))
		if wmErr != nil {
			return nil, fmt.Errorf("workspace manager for OpenCode adapter: %w", wmErr)
		}
		var opErr error
		adp, opErr = opencode.NewProductionOpenCodeAdapter(store, wm, execpolicy.New(), cfg.OpenCodeBinaryPath)
		if opErr != nil {
			return nil, fmt.Errorf("OpenCode adapter construction: %w", opErr)
		}
	}
	return newServerWithAdapter(store, lock, cfg, adp)
}

func newServerWithAdapter(store *storage.Store, lock *ServiceLock, cfg ServerConfig, adp adapter.Adapter) (*Server, error) {
	if store == nil {
		return nil, errors.New("store cannot be nil")
	}
	if lock == nil || !lock.IsHeld() {
		return nil, errors.New("service lock must be held")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("stateDir cannot be empty")
	}
	if cfg.InstanceID == "" {
		return nil, errors.New("instanceID cannot be empty")
	}
	if cfg.AuthToken == "" {
		return nil, errors.New("authToken cannot be empty")
	}

	socketPath := filepath.Join(cfg.StateDir, "council.sock")
	tokenPath := filepath.Join(cfg.StateDir, "auth.token")

	var wm *workspace.WorkspaceManager
	if cfg.WorkspaceBaseDir != "" {
		var err error
		wm, err = workspace.NewWorkspaceManager(cfg.StateDir, cfg.WorkspaceBaseDir)
		if err != nil {
			return nil, fmt.Errorf("new workspace manager: %w", err)
		}
	}
	pe := execpolicy.New()

	if adp == nil && wm != nil {
		adp = execpolicy.NewWorkerAdapter(wm, pe, store)
	}

	srv := &Server{
		store:            store,
		lock:             lock,
		cfg:              cfg,
		coordinator:      NewCoordinator(),
		adapter:          adp,
		workspaceManager: wm,
		policyExecutor:   pe,
		socketPath:       socketPath,
		tokenPath:        tokenPath,
		startedAt:        time.Now().UTC(),
		shutdown:         make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/readiness", srv.handleReadiness)
	mux.HandleFunc("GET /v1/status", srv.handleStatus)
	mux.HandleFunc("POST /v1/runs", srv.handleCreateRun)
	mux.HandleFunc("GET /v1/runs/{run_id}", srv.handleGetRun)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/release", srv.handleRelease)
	mux.HandleFunc("GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}", srv.handleGetTurn)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/cancel", srv.handleCancel)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/reconcile", srv.handleReconcile)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/controller/connect", srv.handleControllerConnect)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/connect", srv.handleRunControllerConnect)
	mux.HandleFunc("GET /v1/runs/{run_id}/controller", srv.handleControllerRecordGet)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/adopt", srv.handleControllerAdopt)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/handoff", srv.handleControllerHandoff)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/revoke", srv.handleControllerRevoke)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/credential/recover", srv.handleControllerCredentialRecover)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/disconnect", srv.handleRunControllerDisconnect)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/prompts/queue", srv.handleQueuePrompt)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/replace", srv.handleReplacePrompt)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/discard", srv.handleDiscardPrompt)
	mux.HandleFunc("POST /v1/runs/{run_id}/decisions", srv.handleRecordDecision)
	mux.HandleFunc("POST /v1/runs/{run_id}/artifacts/release", srv.handleReleaseArtifacts)
	mux.HandleFunc("GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/events", srv.handleEvents)
	mux.HandleFunc("POST /v1/service/stop", srv.handleStop)

	handler := authMiddleware(cfg.AuthToken, mux)

	srv.httpServer = &http.Server{
		Handler:        handler,
		MaxHeaderBytes: 1 << 20, // 1MB
	}

	return srv, nil
}

func (s *Server) WorkspaceManager() *workspace.WorkspaceManager {
	return s.workspaceManager
}

func (s *Server) PolicyExecutor() execpolicy.PolicyExecutor {
	return s.policyExecutor
}

func (s *Server) Handler() http.Handler {
	if s.httpServer != nil {
		return s.httpServer.Handler
	}
	return nil
}

func (s *Server) InstanceID() string {
	return s.cfg.InstanceID
}

func (s *Server) SocketPath() string {
	return s.socketPath
}

func (s *Server) TokenPath() string {
	return s.tokenPath
}

func (s *Server) LockPath() string {
	return s.lock.Path()
}

func (s *Server) Coordinator() *Coordinator {
	return s.coordinator
}

func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return errors.New("server already running")
	}

	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", s.socketPath, err)
	}
	_ = os.Chmod(s.socketPath, 0600)
	s.listener = l
	s.running = true

	// Initial state hydration and validation before accepting requests
	if s.store != nil {
		hydrateCtx, hydrateCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer hydrateCancel()
		if _, err := s.store.HydrateState(hydrateCtx); err != nil {
			_ = l.Close()
			return fmt.Errorf("hydrate state failed: %w", err)
		}
		liveMap := s.coordinator.LiveWorkerKeys()
		counts, err := s.store.GetDiagnosticCounts(hydrateCtx, liveMap)
		if err != nil {
			_ = l.Close()
			return fmt.Errorf("initial diagnostic counts failed: %w", err)
		}
		s.coordinator.SetRecoveryBlockers(counts.RecoveryBlockers)
	}

	go func() {
		_ = s.httpServer.Serve(l)
		_ = s.Teardown(5 * time.Second)
	}()

	return nil
}

func (s *Server) Close() error {
	return s.Teardown(5 * time.Second)
}

// WaitForShutdown blocks until teardown has completed. It returns the
// recorded teardown outcome: nil for orderly completion and a wrapped
// ErrForcedTeardown when the final deadline expired, so a timed-out teardown
// is never reported as orderly success.
func (s *Server) WaitForShutdown(ctx context.Context) error {
	select {
	case <-s.shutdown:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.teardownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if s.coordinator.IsDrainingOrStopping() {
		code := "service_draining"
		if s.coordinator.State() == ServiceStateStopping {
			code = "service_stopping"
		}
		writeError(w, http.StatusServiceUnavailable, code, "service is not ready to accept new work", "")
		return
	}

	var reservedTurns, unresolvedTurns int
	if s.store != nil {
		epoch := s.coordinator.BlockerEpoch()
		liveMap := s.coordinator.LiveWorkerKeys()
		counts, err := s.store.GetDiagnosticCounts(r.Context(), liveMap)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "storage_error", fmt.Sprintf("diagnostics query failed: %v", err), "")
			return
		}
		s.coordinator.ApplyDiagnosticBlockers(counts.RecoveryBlockers, epoch)
		reservedTurns = counts.ReservedTurns
		unresolvedTurns = counts.UnresolvedTurns
	}

	resp := ReadinessResponse{
		Status:          "ready",
		InstanceID:      s.cfg.InstanceID,
		ProtocolVersion: 1,
		StateDir:        s.cfg.StateDir,
		LiveWorkers:     s.coordinator.LiveWorkers(),
		ReservedTurns:   reservedTurns,
		UnresolvedTurns: unresolvedTurns,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	statusStr := "ready"
	if s.coordinator.State() == ServiceStateDraining {
		statusStr = "draining"
	} else if s.coordinator.State() == ServiceStateStopping {
		statusStr = "stopping"
	}

	var reservedTurns, unresolvedTurns int
	activeRuns := []string{}
	if s.store != nil {
		epoch := s.coordinator.BlockerEpoch()
		liveMap := s.coordinator.LiveWorkerKeys()
		counts, err := s.store.GetDiagnosticCounts(r.Context(), liveMap)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "storage_error", fmt.Sprintf("diagnostics query failed: %v", err), "")
			return
		}
		s.coordinator.ApplyDiagnosticBlockers(counts.RecoveryBlockers, epoch)
		reservedTurns = counts.ReservedTurns
		unresolvedTurns = counts.UnresolvedTurns
		activeRuns = counts.ActiveRuns
	}

	resp := StatusResponse{
		InstanceID:      s.cfg.InstanceID,
		PID:             os.Getpid(),
		Status:          statusStr,
		StateDir:        s.cfg.StateDir,
		StartedAt:       s.startedAt,
		ActiveRuns:      activeRuns,
		LiveWorkers:     s.coordinator.LiveWorkers(),
		ReservedTurns:   reservedTurns,
		UnresolvedTurns: unresolvedTurns,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
