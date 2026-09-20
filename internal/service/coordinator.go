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

// SSEEvent represents a server-sent event with name and serialized data payload.
type SSEEvent struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

// Coordinator manages the release admission gate, live worker accounting,
// service-owned execution lifetimes, and SSE event fanout.
type Coordinator struct {
	mu          sync.RWMutex
	state       ServiceState
	ctx         context.Context
	cancel      context.CancelFunc
	workers     sync.WaitGroup
	liveWorkers int
	activeTurns map[adapter.TurnRef]context.CancelFunc
	subscribers map[string][]chan SSEEvent
}

// NewCoordinator initializes a new coordinator in the running state.
func NewCoordinator() *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	return &Coordinator{
		state:       ServiceStateRunning,
		ctx:         ctx,
		cancel:      cancel,
		activeTurns: make(map[adapter.TurnRef]context.CancelFunc),
		subscribers: make(map[string][]chan SSEEvent),
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

// RegisterSubscriber registers an event channel for a specific turn.
func (c *Coordinator) RegisterSubscriber(turnKey string) (chan SSEEvent, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch := make(chan SSEEvent, 64)
	c.subscribers[turnKey] = append(c.subscribers[turnKey], ch)

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			subs := c.subscribers[turnKey]
			for i, sub := range subs {
				if sub == ch {
					c.subscribers[turnKey] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
			if len(c.subscribers[turnKey]) == 0 {
				delete(c.subscribers, turnKey)
			}
		})
	}

	return ch, unsub
}

// SubscriberCount returns the number of active observers for a specific turn.
func (c *Coordinator) SubscriberCount(turnKey string) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.subscribers[turnKey])
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

// BroadcastEvent sends an event to all subscribers registered for the turn.
func (c *Coordinator) BroadcastEvent(turnKey string, ev SSEEvent) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, ch := range c.subscribers[turnKey] {
		select {
		case ch <- ev:
		default:
			// slow consumer overflow: drop event to avoid blocking coordinator/workers
		}
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
	c.subscribers = make(map[string][]chan SSEEvent)
}

// WaitWorkers blocks until all active workers have completed and released accounting.
func (c *Coordinator) WaitWorkers() {
	c.workers.Wait()
}

// Close cancels all active worker contexts, closes observers, and joins workers.
func (c *Coordinator) Close() {
	c.cancel()
	c.mu.Lock()
	for _, cancel := range c.activeTurns {
		cancel()
	}
	for _, subs := range c.subscribers {
		for _, ch := range subs {
			close(ch)
		}
	}
	c.subscribers = make(map[string][]chan SSEEvent)
	c.mu.Unlock()
	c.workers.Wait()
}
