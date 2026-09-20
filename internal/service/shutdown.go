package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// StopRequest defines the payload for POST /v1/service/stop.
type StopRequest struct {
	InstanceID string `json:"instance_id"`
	Drain      bool   `json:"drain"`
}

// StopResponse defines the response payload for POST /v1/service/stop.
type StopResponse struct {
	InstanceID string         `json:"instance_id"`
	Status     string         `json:"status"` // "stopping" or "draining"
	Counters   map[string]int `json:"counters,omitempty"`
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var req StopRequest
	if err := decodeStrictJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), "")
		return
	}

	if strings.TrimSpace(req.InstanceID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "instance_id is required", "")
		return
	}

	if req.InstanceID != s.cfg.InstanceID {
		writeError(w, http.StatusConflict, "instance_mismatch",
			fmt.Sprintf("instance_id %q does not match current instance %q", req.InstanceID, s.cfg.InstanceID), "")
		return
	}

	if !req.Drain {
		liveMap := s.coordinator.LiveWorkerKeys()
		if counts, err := s.store.GetDiagnosticCounts(r.Context(), liveMap); err == nil {
			s.coordinator.SetRecoveryBlockers(counts.RecoveryBlockers)
		}

		if !s.coordinator.TryStopIdle() {
			writeError(w, http.StatusConflict, "service_busy", "service has active executions or pending operations", "")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(StopResponse{
			InstanceID: s.cfg.InstanceID,
			Status:     "stopping",
			Counters:   s.coordinator.ShutdownCounters(),
		})

		go func() {
			time.Sleep(20 * time.Millisecond)
			_ = s.Teardown(5 * time.Second)
		}()
		return
	}

	// Drain == true: close release admission gate and drain active executions.
	// Operator drain has no cancellation cutoff—only an OS signal imposes execution termination.
	s.coordinator.SetState(ServiceStateDraining)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(StopResponse{
		InstanceID: s.cfg.InstanceID,
		Status:     "draining",
		Counters:   s.coordinator.ShutdownCounters(),
	})

	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-s.shutdown:
				return
			case <-ticker.C:
				liveMap := s.coordinator.LiveWorkerKeys()
				if counts, err := s.store.GetDiagnosticCounts(context.Background(), liveMap); err == nil {
					s.coordinator.SetRecoveryBlockers(counts.RecoveryBlockers)
				}
				if s.coordinator.IsShutdownEligible() {
					_ = s.Teardown(5 * time.Second)
					return
				}
			}
		}
	}()
}

// Teardown performs an orderly shutdown sequence bounded by the provided timeout.
func (s *Server) Teardown(timeout time.Duration) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		<-s.shutdown
		return nil
	}
	s.running = false
	s.mu.Unlock()

	defer s.teardownOnce.Do(func() {
		close(s.shutdown)
	})

	// 1. Close command admission gate
	s.coordinator.SetState(ServiceStateStopping)

	// 2. Quiesce or terminate SSE streams
	s.coordinator.CloseAllSubscribers()

	// 3. HTTP Server Shutdown bounded by timeout
	s.httpServer.SetKeepAlivesEnabled(false)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := s.httpServer.Shutdown(ctx)
	if s.listener != nil {
		_ = s.listener.Close()
	}

	// 4. Wait for worker tasks under bounded timeout
	workerDone := make(chan struct{})
	go func() {
		s.coordinator.WaitWorkers()
		close(workerDone)
	}()

	select {
	case <-workerDone:
	case <-ctx.Done():
		s.coordinator.CancelAll()
		_ = s.lock.CleanupDiscovery()
		return ctx.Err()
	}

	// 5. Close storage store
	if s.store != nil {
		_ = s.store.Close()
	}

	// 6. Cleanup discovery files (unlink socket, token, metadata)
	_ = s.lock.CleanupDiscovery()

	// 7. Lock handle remains held until process exits (closed last)
	return err
}

// StartSignalHandler installs OS signal listeners for SIGINT and SIGTERM.
func (s *Server) StartSignalHandler() {
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer signal.Stop(sigCh)
		select {
		case <-s.shutdown:
			return
		case <-sigCh:
			s.handleSignalGrace(sigCh)
		}
	}()
}

func (s *Server) handleSignalGrace(sigCh <-chan os.Signal) {
	s.coordinator.SetState(ServiceStateDraining)

	graceTimer := time.NewTimer(15 * time.Second)
	defer graceTimer.Stop()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			// Repeated signals retain original deadline; ignore
			continue
		case <-ticker.C:
			liveMap := s.coordinator.LiveWorkerKeys()
			if counts, err := s.store.GetDiagnosticCounts(context.Background(), liveMap); err == nil {
				s.coordinator.SetRecoveryBlockers(counts.RecoveryBlockers)
			}
			if s.coordinator.IsShutdownEligible() {
				_ = s.Teardown(5 * time.Second)
				return
			}
		case <-graceTimer.C:
			s.coordinator.CancelActiveWorkers()
			_ = s.Teardown(5 * time.Second)
			return
		case <-s.shutdown:
			return
		}
	}
}
