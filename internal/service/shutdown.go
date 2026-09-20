package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// ErrForcedTeardown reports that orderly quiescence could not be established
// within the final teardown deadline and the service exited through the
// forced-termination path. It is not orderly success: leftover tasks may
// still be mutating storage, so callers must terminate the process rather
// than run deferred storage or lock cleanup.
var ErrForcedTeardown = errors.New("forced teardown: quiescence deadline exceeded")

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
		// Fail closed on unavailable shutdown evidence: a diagnostic error
		// rejects the stop instead of falling back to a stale blocker count.
		if !s.refreshDiagnosticsBound(r.Context()) {
			writeError(w, http.StatusServiceUnavailable, "storage_error", "shutdown eligibility cannot be verified", "")
			return
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

	go s.drainUntilEligible()
}

// drainUntilEligible polls eligibility with bounded, fail-closed diagnostics
// until teardown can proceed or another path tears down first.
func (s *Server) drainUntilEligible() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.shutdown:
			return
		case <-ticker.C:
			if s.refreshEligibility() {
				_ = s.Teardown(5 * time.Second)
				return
			}
		}
	}
}

// refreshEligibility refreshes recovery blockers from a bounded diagnostic
// read and reports shutdown eligibility. A failed read is fail-closed: the
// service is not eligible without fresh evidence.
func (s *Server) refreshEligibility() bool {
	epoch := s.coordinator.BlockerEpoch()
	liveMap := s.coordinator.LiveWorkerKeys()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	counts, err := s.store.GetDiagnosticCounts(ctx, liveMap)
	if err != nil {
		return false
	}
	s.coordinator.ApplyDiagnosticBlockers(counts.RecoveryBlockers, epoch)
	return s.coordinator.IsShutdownEligible()
}

// refreshDiagnosticsBound refreshes recovery blockers from a bounded
// diagnostic read derived from the supplied context. It reports false when
// fresh evidence is unavailable (fail-closed).
func (s *Server) refreshDiagnosticsBound(ctx context.Context) bool {
	epoch := s.coordinator.BlockerEpoch()
	liveMap := s.coordinator.LiveWorkerKeys()

	diagCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	counts, err := s.store.GetDiagnosticCounts(diagCtx, liveMap)
	if err != nil {
		return false
	}
	s.coordinator.ApplyDiagnosticBlockers(counts.RecoveryBlockers, epoch)
	return true
}

// Teardown performs an orderly shutdown sequence bounded by one final
// deadline. On deadline expiry it takes the forced-termination path: workers
// are cancelled, discovery is removed, storage is NOT closed and ownership is
// NOT released while predecessor tasks may still mutate state; the recorded
// ErrForcedTeardown surfaces through WaitForShutdown so the process exits.
func (s *Server) Teardown(timeout time.Duration) error {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		<-s.shutdown
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.teardownErr
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

	// 3. HTTP Server Shutdown bounded by the single final deadline; this
	// waits for in-flight command handlers as well. A non-nil error means
	// shutdown did not complete: handlers may still be active (for example
	// one stalled before admission or performing untracked read work), and
	// a completed task WaitGroup does not prove they exited.
	s.httpServer.SetKeepAlivesEnabled(false)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := s.httpServer.Shutdown(ctx)
	if s.listener != nil {
		_ = s.listener.Close()
	}

	// 4. Join all admitted work: workers, accepted handoffs, pending
	// commits, and in-flight control operations.
	tasksDone := make(chan struct{})
	go func() {
		s.coordinator.WaitTasks()
		close(tasksDone)
	}()

	tasksJoined := false
	select {
	case <-tasksDone:
		tasksJoined = true
	case <-ctx.Done():
	}

	if !tasksJoined || err != nil {
		// Forced termination: a bounded return is not proof that all HTTP
		// handlers or tasks exited. Cancel tracked work, remove discovery,
		// and record the forced exit. Storage stays open and the lock stays
		// held until the process boundary terminates everything.
		s.coordinator.CancelAll()
		forcedErr := fmt.Errorf("%w after %v (http shutdown: %v)", ErrForcedTeardown, timeout, err)
		s.recordTeardownError(forcedErr)
		_ = s.lock.CleanupDiscovery()
		return forcedErr
	}

	// 5. Close storage store
	if s.store != nil {
		_ = s.store.Close()
	}

	// 6. Cleanup discovery files (unlink socket, token, metadata)
	_ = s.lock.CleanupDiscovery()

	// 7. Lock handle remains held until process exits (closed last)
	s.recordTeardownError(err)
	return err
}

func (s *Server) recordTeardownError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teardownErr = err
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
			if s.refreshEligibility() {
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
