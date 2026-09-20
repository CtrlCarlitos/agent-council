package service

import (
	"context"
	"errors"
	"fmt"
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

// SSEEvent represents a server-sent event with name and serialized data payload.
type SSEEvent struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

// Coordinator manages the release admission gate, live worker accounting,
// service-owned execution lifetimes, and SSE event fanout.
type Coordinator struct {
	mu               sync.RWMutex
	state            ServiceState
	ctx              context.Context
	cancel           context.CancelFunc
	workers          sync.WaitGroup
	liveWorkers      int
	pendingHandoffs  int
	pendingCommits   int
	inFlightControl  int
	recoveryBlockers int
	activeTurns      map[adapter.TurnRef]context.CancelFunc
	subscribers      map[adapter.TurnRef][]chan SSEEvent
}

// NewCoordinator initializes a new coordinator in the running state.
func NewCoordinator() *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	return &Coordinator{
		state:       ServiceStateRunning,
		ctx:         ctx,
		cancel:      cancel,
		activeTurns: make(map[adapter.TurnRef]context.CancelFunc),
		subscribers: make(map[adapter.TurnRef][]chan SSEEvent),
	}
}

func stateRank(s ServiceState) int {
	switch s {
	case ServiceStateRunning:
		return 0
	case ServiceStateDraining:
		return 1
	case ServiceStateStopping:
		return 2
	default:
		return 0
	}
}

// SetState updates the lifecycle state monotonically under an admission lock.
func (c *Coordinator) SetState(state ServiceState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if stateRank(state) > stateRank(c.state) {
		c.state = state
	}
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

// CancelActiveWorkers cancels the execution contexts of all active turn workers.
func (c *Coordinator) CancelActiveWorkers() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.activeTurns {
		cancel()
	}
}

// TrackHandoff registers an accepted release handoff until worker registration.
func (c *Coordinator) TrackHandoff() func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingHandoffs++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.pendingHandoffs--
		})
	}
}

// TrackCommit registers a pending terminal outcome database commit.
func (c *Coordinator) TrackCommit() func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendingCommits++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.pendingCommits--
		})
	}
}

// TrackControl registers an in-flight control operation (cancellation or reconciliation).
func (c *Coordinator) TrackControl() (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == ServiceStateStopping {
		return nil, ErrServiceStopping
	}
	c.inFlightControl++
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.inFlightControl--
		})
	}, nil
}

// AddRecoveryBlocker increments the count of outstanding unresolved recovery blockers.
func (c *Coordinator) AddRecoveryBlocker() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recoveryBlockers++
}

// SetRecoveryBlockers updates the count of outstanding unresolved recovery blockers.
func (c *Coordinator) SetRecoveryBlockers(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recoveryBlockers = n
}

// ShutdownCounters returns a snapshot of the five shutdown eligibility counters.
func (c *Coordinator) ShutdownCounters() map[string]int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return map[string]int{
		"pending_handoffs":  c.pendingHandoffs,
		"live_workers":      c.liveWorkers,
		"pending_commits":   c.pendingCommits,
		"in_flight_control": c.inFlightControl,
		"recovery_blockers": c.recoveryBlockers,
	}
}

// IsShutdownEligible reports whether all five shutdown counters are zero.
func (c *Coordinator) IsShutdownEligible() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.pendingHandoffs == 0 &&
		c.liveWorkers == 0 &&
		c.pendingCommits == 0 &&
		c.inFlightControl == 0 &&
		c.recoveryBlockers == 0
}

// TryStopIdle checks if all five shutdown counters are zero under lock and transitions
// the coordinator to ServiceStateStopping atomically.
func (c *Coordinator) TryStopIdle() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != ServiceStateRunning {
		return false
	}
	if c.pendingHandoffs == 0 &&
		c.liveWorkers == 0 &&
		c.pendingCommits == 0 &&
		c.inFlightControl == 0 &&
		c.recoveryBlockers == 0 {
		c.state = ServiceStateStopping
		return true
	}
	return false
}

// AdmitRelease admits a new release operation, incrementing pendingHandoffs under lock.
func (c *Coordinator) AdmitRelease() (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == ServiceStateDraining || c.state == ServiceStateStopping {
		return nil, ErrServiceStopping
	}
	c.pendingHandoffs++
	var once sync.Once
	done := func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.pendingHandoffs--
		})
	}
	return done, nil
}

// LiveWorkerKeys returns a set of "session_id:turn_key" strings for all currently tracked live workers.
func (c *Coordinator) LiveWorkerKeys() map[string]bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make(map[string]bool, len(c.activeTurns))
	for ref := range c.activeTurns {
		keys[fmt.Sprintf("%s:%s", ref.SessionID, ref.TurnKey)] = true
	}
	return keys
}

// RegisterSubscriber registers an event channel for a specific turn ref.
func (c *Coordinator) RegisterSubscriber(ref adapter.TurnRef) (chan SSEEvent, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch := make(chan SSEEvent, 64)
	c.subscribers[ref] = append(c.subscribers[ref], ch)

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			subs := c.subscribers[ref]
			for i, sub := range subs {
				if sub == ch {
					c.subscribers[ref] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
			if len(c.subscribers[ref]) == 0 {
				delete(c.subscribers, ref)
			}
		})
	}

	return ch, unsub
}

// SubscriberCount returns the number of active observers for a specific turn ref.
func (c *Coordinator) SubscriberCount(ref adapter.TurnRef) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.subscribers[ref])
}

// TotalSubscriberCount returns the total number of active observers across all turns.
func (c *Coordinator) TotalSubscriberCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	total := 0
	for _, subs := range c.subscribers {
		total += len(subs)
	}
	return total
}

// BroadcastEvent sends an event to all subscribers registered for the turn ref.
// Slow observers that fail to consume within buffer capacity are disconnected.
func (c *Coordinator) BroadcastEvent(ref adapter.TurnRef, ev SSEEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	subs := c.subscribers[ref]
	if len(subs) == 0 {
		return
	}

	remaining := make([]chan SSEEvent, 0, len(subs))
	for _, ch := range subs {
		select {
		case ch <- ev:
			remaining = append(remaining, ch)
		default:
			// slow consumer overflow: disconnect observer
			close(ch)
		}
	}
	if len(remaining) == 0 {
		delete(c.subscribers, ref)
	} else {
		c.subscribers[ref] = remaining
	}
}

// CloseAllSubscribers closes all active subscriber channels.
func (c *Coordinator) CloseAllSubscribers() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, subs := range c.subscribers {
		for _, ch := range subs {
			close(ch)
		}
	}
	c.subscribers = make(map[adapter.TurnRef][]chan SSEEvent)
}

// WaitWorkers blocks until all active workers have completed and released accounting.
func (c *Coordinator) WaitWorkers() {
	c.workers.Wait()
}

// CancelAll cancels all active worker contexts and closes observers without joining workers.
func (c *Coordinator) CancelAll() {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cancel := range c.activeTurns {
		cancel()
	}
	for _, subs := range c.subscribers {
		for _, ch := range subs {
			close(ch)
		}
	}
	c.subscribers = make(map[adapter.TurnRef][]chan SSEEvent)
}

// Close cancels all active worker contexts, closes observers, and joins workers.
func (c *Coordinator) Close() {
	c.CancelAll()
	c.workers.Wait()
}
