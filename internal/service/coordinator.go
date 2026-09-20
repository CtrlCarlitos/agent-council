package service

import (
	"context"
	"errors"
	"sync"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
)

// ServiceState represents the lifecycle and admission state of the service.
type ServiceState string

const (
	ServiceStateRunning  ServiceState = "running"
	ServiceStateDraining ServiceState = "draining"
	ServiceStateStopping ServiceState = "stopping"
)

var (
	ErrServiceDraining = errors.New("service is draining")
	ErrServiceStopping = errors.New("service is stopping")
)

// Coordinator manages the release admission gate, live worker accounting,
// and service-owned execution lifetimes decoupled from incoming HTTP requests.
type Coordinator struct {
	mu          sync.RWMutex
	state       ServiceState
	ctx         context.Context
	cancel      context.CancelFunc
	workers     sync.WaitGroup
	liveWorkers int
	activeTurns map[adapter.TurnRef]context.CancelFunc
}

// NewCoordinator initializes a new coordinator in the running state.
func NewCoordinator() *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	return &Coordinator{
		state:       ServiceStateRunning,
		ctx:         ctx,
		cancel:      cancel,
		activeTurns: make(map[adapter.TurnRef]context.CancelFunc),
	}
}

// SetState updates the lifecycle state of the coordinator under an admission lock.
func (c *Coordinator) SetState(state ServiceState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = state
}

// State returns the current lifecycle state.
func (c *Coordinator) State() ServiceState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// IsDrainingOrStopping reports whether new release admissions are blocked.
func (c *Coordinator) IsDrainingOrStopping() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state == ServiceStateDraining || c.state == ServiceStateStopping
}

// Context returns the root service context for detached background execution.
func (c *Coordinator) Context() context.Context {
	return c.ctx
}

// LiveWorkers returns the current count of active workers.
func (c *Coordinator) LiveWorkers() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.liveWorkers
}

// RegisterWorker admits and tracks a new execution worker under coordinator ownership.
func (c *Coordinator) RegisterWorker(ref adapter.TurnRef) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state == ServiceStateStopping {
		return nil, nil, ErrServiceStopping
	}

	workerCtx, workerCancel := context.WithCancel(c.ctx)
	c.activeTurns[ref] = workerCancel
	c.liveWorkers++
	c.workers.Add(1)

	var once sync.Once
	done := func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			delete(c.activeTurns, ref)
			c.liveWorkers--
			c.workers.Done()
			workerCancel()
		})
	}

	return workerCtx, done, nil
}

// CancelWorker cancels the execution context of a specific active turn worker.
func (c *Coordinator) CancelWorker(ref adapter.TurnRef) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cancel, ok := c.activeTurns[ref]; ok {
		cancel()
		return true
	}
	return false
}

// WaitWorkers blocks until all active workers have completed and released accounting.
func (c *Coordinator) WaitWorkers() {
	c.workers.Wait()
}

// Close cancels all active worker contexts and shuts down the coordinator.
func (c *Coordinator) Close() {
	c.cancel()
	c.mu.Lock()
	for _, cancel := range c.activeTurns {
		cancel()
	}
	c.mu.Unlock()
	c.workers.Wait()
}
