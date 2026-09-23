package codex

// Adapter-owned event pump for the codex app-server child (AC-009 spec
// §3.2): armed BEFORE any notification-emitting request is sent, with the
// thread/turn route tables and bounded buffers allocated up front. The
// creation-reservation route closes the first-notification gap:
// thread/started may arrive before the thread/start response, so a pending
// reservation is inserted BEFORE the request is written and the binding is
// CONFIRMED only when the response id equals the notified thread id.
// Orphan or mismatched thread/started is protocol drift ⇒ poison ⇒ child
// terminated, creation uncertain. Notifications for bound threads with no
// installed turn route park in bounded per-thread buffers and replay on
// route installation.

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	// maxParkedPerThread bounds the per-thread park buffer for
	// notifications that arrive before their turn route is installed.
	// Overflow drops the OLDEST parked notification and counts the drop:
	// bounded memory, never a silent protocol-evidence leak (the counter
	// is surfaced).
	maxParkedPerThread = 256

	// maxGlobalNotifications bounds the connection-scope capture for
	// notifications without a thread id (e.g. remoteControl/status/changed
	// at connection setup). Drop-oldest with a counter, like the park
	// buffers.
	maxGlobalNotifications = 64

	// maxQueuedServerRequests bounds the default surface for server→client
	// requests when no handler is registered. Overflow poisons the
	// connection: silently dropping an approval request would hang the
	// native turn with no evidence trail.
	maxQueuedServerRequests = 64
)

// creationConfirmGrace bounds how long CompleteCreation waits for the
// thread/started notification after the response arrived. Package-level so
// tests can shorten it.
var creationConfirmGrace = 5 * time.Second

// ErrProtocolDrift reports native protocol behavior that contradicts the
// verified 0.154.0 contract: the connection is poisoned and the child must
// be terminated (spec §3.9: protocol drift → Uncertain, never best-effort
// parse).
type ErrProtocolDrift struct {
	Method string
	Reason string
}

func (e *ErrProtocolDrift) Error() string {
	return "codex protocol drift on " + e.Method + ": " + e.Reason
}

// ErrCreationAbandoned reports that a reserved creation was abandoned
// before confirmation (the request write failed or the caller gave up).
// The eventual native thread id is unknown — creation is uncertain.
var ErrCreationAbandoned = errors.New("thread creation reservation abandoned before confirmation")

// NativeNotification is one routed server notification with the thread/
// turn correlation extracted from its params.
type NativeNotification struct {
	Method     string
	ThreadID   string
	TurnID     string
	Params     json.RawMessage
	ReceivedAt time.Time
}

// TurnTap is one installed (thread, turn) route: a bounded notification
// channel closed when the tap is removed or the pump dies.
type TurnTap struct {
	threadID string
	turnID   string
	ch       chan NativeNotification
	done     chan struct{}
	once     sync.Once
}

// C returns the notification channel.
func (t *TurnTap) C() <-chan NativeNotification { return t.ch }

// ThreadID returns the bound native thread id.
func (t *TurnTap) ThreadID() string { return t.threadID }

// TurnID returns the bound native turn id.
func (t *TurnTap) TurnID() string { return t.turnID }

// Done closes when the tap is no longer routable.
func (t *TurnTap) Done() <-chan struct{} { return t.done }

func (t *TurnTap) close() { t.once.Do(func() { close(t.done) }) }

// threadRoute is the per-bound-thread routing state.
type threadRoute struct {
	park        []NativeNotification
	parkDropped int
	turns       map[string]*TurnTap
}

// pendingCreation is the in-flight creation-reservation route entry,
// inserted BEFORE thread/start is written. An abandoned reservation stays
// as a tombstone: the native thread may still exist (creation uncertain),
// so a late thread/started routes to the tombstone and is swallowed —
// never drift, never a binding — and a new reservation stays blocked
// (spec §3.4: automatic recreation blocked after a lost creation).
type pendingCreation struct {
	requestID int64
	abandoned bool
	// notified closes when the first thread/started routed here.
	notified chan struct{}
	// done closes when the reservation reaches a terminal state
	// (confirmed, mismatched, or abandoned).
	done    chan struct{}
	once    sync.Once
	outcome error
}

func (pc *pendingCreation) finish(err error) {
	pc.outcome = err
	pc.once.Do(func() { close(pc.done) })
}

// EventPump owns the route tables for one child's notification stream.
type EventPump struct {
	mu       sync.Mutex
	creation *pendingCreation
	// creationNotifiedThreadID is the thread id of the first
	// thread/started routed to the pending creation.
	creationNotifiedThreadID string
	threads                  map[string]*threadRoute
	global                   []NativeNotification
	globalDropped            int
	poisoned                 bool
	poisonErr                error

	onFatal   func(error)
	fatalOnce sync.Once
}

// NewEventPump allocates the route tables. Arming happens before any
// notification-emitting request: the server wires the pump to the conn
// before the handshake is sent.
func NewEventPump() *EventPump {
	return &EventPump{
		threads: make(map[string]*threadRoute),
		global:  make([]NativeNotification, 0, maxGlobalNotifications),
	}
}

// SetFatalHandler registers the drift callback (child termination).
func (p *EventPump) SetFatalHandler(fn func(error)) {
	p.mu.Lock()
	p.onFatal = fn
	p.mu.Unlock()
}

// Poisoned reports whether protocol drift has poisoned routing.
func (p *EventPump) Poisoned() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.poisoned, p.poisonErr
}

// PoisonCause returns the drift cause, or nil while healthy.
func (p *EventPump) PoisonCause() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.poisonErr
}

// poison records drift exactly once and fires the fatal handler.
func (p *EventPump) poison(cause error) {
	p.mu.Lock()
	if p.poisoned {
		p.mu.Unlock()
		return
	}
	p.poisoned = true
	p.poisonErr = cause
	fn := p.onFatal
	// A pending creation reservation fails with the drift cause.
	if pc := p.creation; pc != nil {
		pc.finish(cause)
	}
	// Terminate all taps so consumers observe the failure.
	for _, route := range p.threads {
		for _, tap := range route.turns {
			tap.close()
		}
	}
	p.mu.Unlock()
	if fn != nil {
		p.fatalOnce.Do(func() { go fn(cause) })
	}
}

// ReserveCreation inserts the pending creation route for the JSON-RPC
// request id. It MUST be called before thread/start is written. One
// pending creation per child at a time.
func (p *EventPump) ReserveCreation(requestID int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.poisoned {
		return p.poisonErr
	}
	if p.creation != nil {
		return ErrCreationPending
	}
	p.creation = &pendingCreation{
		requestID: requestID,
		notified:  make(chan struct{}),
		done:      make(chan struct{}),
	}
	return nil
}

// AbandonCreation releases a reservation whose request never completed
// (write failure, caller cancellation). The reservation becomes a
// tombstone: a racing thread/started is swallowed without drift, no
// binding publishes, and the blocked slot keeps automatic recreation from
// reusing an uncertain creation (spec §3.4).
func (p *EventPump) AbandonCreation() {
	p.mu.Lock()
	pc := p.creation
	if pc != nil {
		pc.abandoned = true
	}
	p.mu.Unlock()
	if pc != nil {
		pc.finish(ErrCreationAbandoned)
	}
}

// CompleteCreation confirms the reservation once the thread/start response
// arrived: the binding publishes only when the response's result id equals
// the notified thread id. A mismatch, or no notification within the grace
// bound, is protocol drift.
func (p *EventPump) CompleteCreation(resultThreadID string) error {
	p.mu.Lock()
	pc := p.creation
	p.mu.Unlock()
	if pc == nil {
		return errors.New("no pending creation reservation to complete")
	}

	select {
	case <-pc.done:
		p.mu.Lock()
		p.creation = nil
		p.mu.Unlock()
		return pc.outcome
	case <-pc.notified:
		// The reservation may have been abandoned concurrently; a closed
		// done always wins over the notified signal.
		select {
		case <-pc.done:
			p.mu.Lock()
			p.creation = nil
			p.mu.Unlock()
			return pc.outcome
		default:
		}
	case <-time.After(creationConfirmGrace):
		p.mu.Lock()
		p.creation = nil
		p.mu.Unlock()
		pc.finish(&ErrProtocolDrift{Method: "thread/start", Reason: "no thread/started notification within the confirmation grace"})
		p.poison(&ErrProtocolDrift{Method: "thread/started", Reason: "thread/start response arrived without its thread/started notification"})
		return pc.outcome
	}

	p.mu.Lock()
	notifiedID := p.creationNotifiedThreadID
	p.mu.Unlock()

	if notifiedID != resultThreadID {
		drift := &ErrProtocolDrift{
			Method: "thread/start",
			Reason: "response id " + resultThreadID + " != notified thread id " + notifiedID,
		}
		p.mu.Lock()
		p.creation = nil
		p.mu.Unlock()
		pc.finish(drift)
		p.poison(drift)
		return drift
	}

	// Confirm: publish the binding.
	p.mu.Lock()
	p.threads[resultThreadID] = &threadRoute{turns: make(map[string]*TurnTap)}
	p.creation = nil
	p.mu.Unlock()
	pc.finish(nil)
	return nil
}

// BindThread publishes a thread binding directly (resume path: the native
// id is already known and verified by the caller).
func (p *EventPump) BindThread(threadID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.poisoned {
		return
	}
	if _, ok := p.threads[threadID]; !ok {
		p.threads[threadID] = &threadRoute{turns: make(map[string]*TurnTap)}
	}
}

// InstallTurnRoute installs the (thread, turn) route and replays parked
// notifications for that turn in arrival order. The route must be
// installed against a bound thread and before turn/start is written.
func (p *EventPump) InstallTurnRoute(threadID, turnID string) (*TurnTap, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.poisoned {
		return nil, p.poisonErr
	}
	route, ok := p.threads[threadID]
	if !ok {
		return nil, &ErrProtocolDrift{Method: "InstallTurnRoute", Reason: "thread " + threadID + " is not bound"}
	}
	if _, ok := route.turns[turnID]; ok {
		return nil, &ErrProtocolDrift{Method: "InstallTurnRoute", Reason: "turn route already installed for " + turnID}
	}
	tap := &TurnTap{
		threadID: threadID,
		turnID:   turnID,
		ch:       make(chan NativeNotification, maxParkedPerThread),
		done:     make(chan struct{}),
	}
	route.turns[turnID] = tap

	// Replay the matching parked prefix in FIFO order; leave anything
	// belonging to other turns parked.
	kept := route.park[:0]
	for _, n := range route.park {
		if n.TurnID == turnID {
			tap.ch <- n
			continue
		}
		kept = append(kept, n)
	}
	route.park = kept
	return tap, nil
}

// RemoveTurnRoute detaches a tap (consumer gone; the pump keeps draining).
func (p *EventPump) RemoveTurnRoute(threadID, turnID string) {
	p.mu.Lock()
	route, ok := p.threads[threadID]
	if ok {
		if tap, ok := route.turns[turnID]; ok {
			delete(route.turns, turnID)
			tap.close()
		}
	}
	p.mu.Unlock()
}

// ParkedCount reports the parked (unrouted) notification count for a bound
// thread.
func (p *EventPump) ParkedCount(threadID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if route, ok := p.threads[threadID]; ok {
		return len(route.park)
	}
	return 0
}

// ParkedDropped reports how many parked notifications were dropped by the
// bounded-overflow rule for a thread.
func (p *EventPump) ParkedDropped(threadID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if route, ok := p.threads[threadID]; ok {
		return route.parkDropped
	}
	return 0
}

// GlobalNotifications returns the captured thread-less notifications
// (connection-scope status events).
func (p *EventPump) GlobalNotifications() []NativeNotification {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]NativeNotification, len(p.global))
	copy(out, p.global)
	return out
}

// HandleNotification routes one server notification. It is the conn's
// notification entry point and safe for concurrent use.
func (p *EventPump) HandleNotification(method string, params json.RawMessage) {
	p.mu.Lock()
	if p.poisoned {
		p.mu.Unlock()
		return
	}
	threadID, turnID := extractCorrelation(params)

	if method == "thread/started" {
		p.handleThreadStartedLocked(threadID)
		return
	}

	if threadID == "" {
		// Connection-scope notification (no thread correlation): capture
		// bounded, never drift.
		n := NativeNotification{Method: method, Params: params, ReceivedAt: time.Now().UTC()}
		if len(p.global) >= maxGlobalNotifications {
			p.global = append(p.global[1:], n)
			p.globalDropped++
		} else {
			p.global = append(p.global, n)
		}
		p.mu.Unlock()
		return
	}

	route, ok := p.threads[threadID]
	if !ok {
		// An event for an unbound thread: id drift (spec §3.5).
		p.mu.Unlock()
		p.poison(&ErrProtocolDrift{Method: method, Reason: "notification for unbound thread id " + threadID})
		return
	}

	n := NativeNotification{Method: method, ThreadID: threadID, TurnID: turnID, Params: params, ReceivedAt: time.Now().UTC()}
	if turnID != "" {
		if tap, ok := route.turns[turnID]; ok {
			select {
			case tap.ch <- n:
				p.mu.Unlock()
				return
			default:
				// Tap full: consumer too slow. Detach the tap and park the
				// notification (bounded) so nothing is silently lost; the
				// route can be reinstalled to replay.
				tap.close()
				delete(route.turns, turnID)
			}
		}
	}
	if len(route.park) >= maxParkedPerThread {
		route.park = append(route.park[1:], n)
		route.parkDropped++
	} else {
		route.park = append(route.park, n)
	}
	p.mu.Unlock()
}

// handleThreadStartedLocked routes a thread/started notification.
// p.mu is held; the lock is released on every path.
func (p *EventPump) handleThreadStartedLocked(threadID string) {
	if pc := p.creation; pc != nil {
		if pc.abandoned {
			// The creation was abandoned (lost response / caller cancel):
			// the thread may exist natively. Swallow the notification —
			// no drift, no binding; the tombstone keeps recreation blocked.
			p.mu.Unlock()
			return
		}
		select {
		case <-pc.notified:
			// A second thread/started during the same pending creation:
			// the first already routed; duplicates are ignored until the
			// response confirms.
		default:
			p.creationNotifiedThreadID = threadID
			close(pc.notified)
		}
		p.mu.Unlock()
		return
	}
	if _, ok := p.threads[threadID]; ok {
		// Late duplicate for an already-bound thread: dedup, no state
		// change (spec §3.2).
		p.mu.Unlock()
		return
	}
	// Orphan: neither pending reservation nor bound id — protocol drift.
	p.mu.Unlock()
	p.poison(&ErrProtocolDrift{Method: "thread/started", Reason: "orphan thread/started for unbound thread id " + threadID})
}

// CreationOutcome reports the terminal outcome of the current creation
// reservation: ErrCreationPending while still in flight, the outcome error
// once terminal (nil on confirmed success), or nil when nothing is
// reserved. The adapter layer uses it to surface
// ErrSessionCreationUncertain for abandoned creations.
func (p *EventPump) CreationOutcome() error {
	p.mu.Lock()
	pc := p.creation
	p.mu.Unlock()
	if pc == nil {
		return nil
	}
	select {
	case <-pc.done:
		return pc.outcome
	default:
		return ErrCreationPending
	}
}

// extractCorrelation pulls the native thread/turn ids from notification
// params across the verified 0.154.0 param spellings: thread/started uses
// thread.id; turn/* and item/* use threadId (+ turnId or turn.id).
func extractCorrelation(params json.RawMessage) (threadID, turnID string) {
	if len(params) == 0 {
		return "", ""
	}
	var flat struct {
		ThreadID  string `json:"threadId"`
		ThreadID2 string `json:"thread_id"`
		TurnID    string `json:"turnId"`
		TurnID2   string `json:"turn_id"`
		Thread    *struct {
			ID string `json:"id"`
		} `json:"thread"`
		Turn *struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(params, &flat); err != nil {
		return "", ""
	}
	threadID = flat.ThreadID
	if threadID == "" {
		threadID = flat.ThreadID2
	}
	if threadID == "" && flat.Thread != nil {
		threadID = flat.Thread.ID
	}
	turnID = flat.TurnID
	if turnID == "" {
		turnID = flat.TurnID2
	}
	if turnID == "" && flat.Turn != nil {
		turnID = flat.Turn.ID
	}
	return threadID, turnID
}

// notificationFromFrames builds the routed representation of one recorded
// notification (fixture decoding uses the same extraction as the live
// pump).
func notificationFromFrames(method string, params json.RawMessage) NativeNotification {
	threadID, turnID := extractCorrelation(params)
	return NativeNotification{Method: method, ThreadID: threadID, TurnID: turnID, Params: params}
}
