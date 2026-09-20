package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

type ServerConfig struct {
	StateDir   string
	InstanceID string
	AuthToken  string
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
	StartedAt       time.Time `json:"started_at"`
	ActiveRuns      []string  `json:"active_runs"`
	LiveWorkers     int       `json:"live_workers"`
	ReservedTurns   int       `json:"reserved_turns"`
	UnresolvedTurns int       `json:"unresolved_turns"`
}

type Server struct {
	store       *storage.Store
	lock        *ServiceLock
	cfg         ServerConfig
	coordinator *Coordinator
	adapter     adapter.Adapter
	listener    net.Listener
	httpServer  *http.Server
	socketPath  string
	tokenPath   string
	startedAt   time.Time

	mu       sync.Mutex
	running  bool
	shutdown chan struct{}
}

func NewServer(store *storage.Store, lock *ServiceLock, cfg ServerConfig) (*Server, error) {
	return NewServerWithAdapter(store, lock, cfg, nil)
}

func NewServerWithAdapter(store *storage.Store, lock *ServiceLock, cfg ServerConfig, adp adapter.Adapter) (*Server, error) {
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

	srv := &Server{
		store:       store,
		lock:        lock,
		cfg:         cfg,
		coordinator: NewCoordinator(),
		adapter:     adp,
		socketPath:  socketPath,
		tokenPath:   tokenPath,
		startedAt:   time.Now().UTC(),
		shutdown:    make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/readiness", srv.handleReadiness)
	mux.HandleFunc("GET /v1/status", srv.handleStatus)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/release", srv.handleRelease)
	mux.HandleFunc("GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}", srv.handleGetTurn)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/cancel", srv.handleCancel)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/reconcile", srv.handleReconcile)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/controller/connect", srv.handleControllerConnect)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/prompts/queue", srv.handleQueuePrompt)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/replace", srv.handleReplacePrompt)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/discard", srv.handleDiscardPrompt)
	mux.HandleFunc("POST /v1/runs/{run_id}/decisions", srv.handleRecordDecision)

	handler := authMiddleware(cfg.AuthToken, mux)

	srv.httpServer = &http.Server{
		Handler:        handler,
		MaxHeaderBytes: 1 << 20, // 1MB
	}

	return srv, nil
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
	s.listener = l
	s.running = true

	go func() {
		_ = s.httpServer.Serve(l)
		close(s.shutdown)
	}()

	return nil
}

func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}
	s.running = false

	s.coordinator.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := s.httpServer.Shutdown(ctx)
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = s.lock.CleanupDiscovery()
	return err
}

func (s *Server) WaitForShutdown(ctx context.Context) error {
	select {
	case <-s.shutdown:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	resp := ReadinessResponse{
		Status:          "ready",
		InstanceID:      s.cfg.InstanceID,
		ProtocolVersion: 1,
		StateDir:        s.cfg.StateDir,
		LiveWorkers:     s.coordinator.LiveWorkers(),
		ReservedTurns:   0,
		UnresolvedTurns: 0,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := StatusResponse{
		InstanceID:      s.cfg.InstanceID,
		PID:             os.Getpid(),
		Status:          "ready",
		StartedAt:       s.startedAt,
		ActiveRuns:      []string{},
		LiveWorkers:     s.coordinator.LiveWorkers(),
		ReservedTurns:   0,
		UnresolvedTurns: 0,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
