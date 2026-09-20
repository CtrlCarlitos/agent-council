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
//
// Invariants:
//   - A cancellation request never cancels a worker context. Only service
//     termination (signal grace expiry, forced teardown, root cancellation)
//     cancels worker execution contexts; a cancel request is an adapter-level
//     signal and the supervisor keeps observing, collecting, and persisting.
//   - Every admitted unit of work (worker, handoff, commit, control
//     operation) is accounted in one task WaitGroup so teardown can join all
//     predecessor activity.
//   - Recovery blocker counts are reconciled against a versioned snapshot:
//     a diagnostic refresh observed before an AddRecoveryBlocker obligation
//     materialized can never erase that obligation.
type Coordinator struct {
	mu               sync.RWMutex
	state            ServiceState
	ctx              context.Context
	cancel           context.CancelFunc
	tasks            sync.WaitGroup
	liveWorkers      int
	pendingHandoffs  int
	pendingCommits   int
	inFlightControl  int
	recoveryBlockers int
	blockerEpoch     uint64
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
// The returned context is the worker's termination scope: it is cancelled only by
// service-level termination, never by a cancellation request.
// Registration changes the live set that diagnostics use to exclude live
// turns from unresolved counts, so it advances the blocker epoch.
func (c *Coordinator) RegisterWorker(ref adapter.TurnRef) (context.Context, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state == ServiceStateStopping {
		return nil, nil, ErrServiceStopping
	}

	workerCtx, workerCancel := context.WithCancel(c.ctx)
	c.activeTurns[ref] = workerCancel
	c.liveWorkers++
	c.tasks.Add(1)
	c.blockerEpoch++

	var once sync.Once
	done := func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			delete(c.activeTurns, ref)
			c.liveWorkers--
			c.tasks.Done()
			workerCancel()
			// Retirement changes the live set: a diagnostic whose snapshot
			// was taken while this worker was tracked must no longer apply,
			// because its unresolved-count exclusion relied on the old set.
			c.blockerEpoch++
		})
	}

	return workerCtx, done, nil
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
	c.pendingHandoffs++
	c.tasks.Add(1)
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.pendingHandoffs--
			c.tasks.Done()
		})
	}
}

// TrackCommit registers a pending terminal outcome database commit.
func (c *Coordinator) TrackCommit() func() {
	c.mu.Lock()
	c.pendingCommits++
	c.tasks.Add(1)
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.pendingCommits--
			c.tasks.Done()
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
	c.tasks.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.inFlightControl--
			c.tasks.Done()
		})
	}, nil
}

// AddRecoveryBlocker records a new outstanding persistence or uncertainty
// obligation and advances the blocker epoch so that any diagnostic snapshot
// taken before this obligation cannot overwrite it.
func (c *Coordinator) AddRecoveryBlocker() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recoveryBlockers++
	c.blockerEpoch++
}

// BlockerEpoch returns the current diagnostic epoch, which versions every
// input a diagnostic refresh depends on: blocker creation, worker
// registration, and worker retirement (the live set determines which
// running turns are excluded from unresolved counts). Callers must capture
// this before starting a diagnostic read and pass it to
// ApplyDiagnosticBlockers.
func (c *Coordinator) BlockerEpoch() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.blockerEpoch
}

// ApplyDiagnosticBlockers reconciles the recovery blocker count from a fresh
// diagnostic read. The read is applied only when no AddRecoveryBlocker
// obligation materialized after the snapshot was taken (epoch unchanged);
// otherwise the refresh is skipped and the next refresh reconciles.
func (c *Coordinator) ApplyDiagnosticBlockers(n int, epoch uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.blockerEpoch {
		return false
	}
	c.recoveryBlockers = n
	return true
}

// SetRecoveryBlockers installs the initial blocker count from startup
// hydration. It must not be used once any worker or command handler can run.
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

// AdmitRelease admits a new release operation, incrementing pendingHandoffs
// under lock. Accepted handoffs participate in the task WaitGroup until the
// responsibility is transferred to a worker or the admission finishes.
func (c *Coordinator) AdmitRelease() (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == ServiceStateDraining || c.state == ServiceStateStopping {
		return nil, ErrServiceStopping
	}
	c.pendingHandoffs++
	c.tasks.Add(1)
	var once sync.Once
	done := func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.pendingHandoffs--
			c.tasks.Done()
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

// WaitTasks blocks until every admitted unit of work (workers, accepted
// handoffs, pending commits, in-flight control operations) has completed.
func (c *Coordinator) WaitTasks() {
	c.tasks.Wait()
}

// WaitWorkers blocks until all active workers have completed and released accounting.
// Superseded by WaitTasks; retained for existing callers.
func (c *Coordinator) WaitWorkers() {
	c.tasks.Wait()
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

// Close cancels all active worker contexts, closes observers, and joins all tasks.
func (c *Coordinator) Close() {
	c.CancelAll()
	c.tasks.Wait()
}
