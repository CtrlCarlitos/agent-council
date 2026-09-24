package codex

// Task 4 pump routing evidence (portable): the creation-reservation route
// (notification-before-response, response-id equality, drift fail-closed),
// orphan and duplicate thread/started handling, and the bounded
// park-and-replay buffers. These drive the pump directly through its
// notification entry point; the process-armed ordering evidence lives in
// server_test.go.

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureThreadA = "01900000-0000-7000-8000-000000000001"
const fixtureThreadB = "01900000-0000-7000-8000-000000000002"
const fixtureTurnA = "01900000-0000-7000-8000-0000000000aa"

func threadStartedParams(threadID string) string {
	return `{"thread":{"id":"` + threadID + `","sessionId":"` + threadID + `","status":{"type":"idle"}},"emittedAtMs":1760000000000}`
}

func withGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := creationConfirmGrace
	creationConfirmGrace = d
	t.Cleanup(func() { creationConfirmGrace = old })
}

// Notification-before-response: the first thread/started routes to the
// pending creation; the binding publishes only when the response id
// equals the notified thread id.
func TestPump_CreationReservation_NotifyBeforeResponse(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()

	if err := p.ReserveCreation(7); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Only one pending creation per child at a time.
	if err := p.ReserveCreation(8); !errors.Is(err, ErrCreationPending) {
		t.Fatalf("second reservation must be rejected, got %v", err)
	}

	// The notification arrives while thread/start is still in flight.
	p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))

	done := make(chan error, 1)
	go func() { done <- p.CompleteCreation(fixtureThreadA) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("complete: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CompleteCreation did not confirm")
	}

	// The binding is published: dedup for a late duplicate, and turn
	// routes can install against the bound thread.
	p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))
	if poisoned, err := p.Poisoned(); poisoned {
		t.Fatalf("late duplicate must dedup without drift: %v", err)
	}
	tap, err := p.InstallTurnRoute(fixtureThreadA, fixtureTurnA)
	if err != nil {
		t.Fatalf("install turn route: %v", err)
	}
	if tap == nil {
		t.Fatal("route installation must return a tap")
	}
}

// Response-before-notification (the live-observed ordering): confirmation
// waits the bounded grace for the thread/started notification and confirms
// on equality.
func TestPump_CreationReservation_ResponseBeforeNotification(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()

	if err := p.ReserveCreation(1); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- p.CompleteCreation(fixtureThreadA) }()
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("must wait for the notification before confirming: %v", err)
	default:
	}
	p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation never completed")
	}
}

// Response id ≠ notified thread id ⇒ protocol drift: poison + fatal.
func TestPump_CreationReservation_IDMismatchDrift(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()
	fatal := make(chan error, 1)
	p.SetFatalHandler(func(err error) { fatal <- err })

	if err := p.ReserveCreation(1); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))
	err := p.CompleteCreation(fixtureThreadB)
	var drift *ErrProtocolDrift
	if !errors.As(err, &drift) {
		t.Fatalf("expected ErrProtocolDrift, got %T: %v", err, err)
	}
	select {
	case <-fatal:
	case <-time.After(time.Second):
		t.Fatal("drift must fire the fatal handler (child termination)")
	}
	if poisoned, perr := p.Poisoned(); !poisoned {
		t.Fatalf("drift must poison the pump: %v", perr)
	}
}

// A thread/started with neither a pending reservation nor a bound id is an
// orphan ⇒ drift ⇒ terminate+Uncertain.
func TestPump_OrphanThreadStartedDrift(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()
	fatal := make(chan error, 1)
	p.SetFatalHandler(func(err error) { fatal <- err })

	p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))
	select {
	case err := <-fatal:
		var drift *ErrProtocolDrift
		if !errors.As(err, &drift) {
			t.Fatalf("orphan must report ErrProtocolDrift, got %T", err)
		}
	case <-time.After(time.Second):
		t.Fatal("orphan thread/started must fire the fatal handler")
	}
	if poisoned, _ := p.Poisoned(); !poisoned {
		t.Fatal("orphan thread/started must poison the pump")
	}
}

// Unrouted notifications for a bound thread park in a bounded per-thread
// buffer and replay in order on route installation; overflow is bounded
// with a counted drop, never unbounded growth.
func TestPump_ParkAndReplay(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()

	if err := p.ReserveCreation(1); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))
	if err := p.CompleteCreation(fixtureThreadA); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Turn notifications arrive before any turn route exists: parked.
	turnStarted := `{"threadId":"` + fixtureThreadA + `","turnId":"` + fixtureTurnA + `"}`
	tokenUsage := `{"threadId":"` + fixtureThreadA + `","turnId":"` + fixtureTurnA + `","tokenUsage":{"last":{"totalTokens":15},"total":{"totalTokens":15}}}`
	completed := `{"threadId":"` + fixtureThreadA + `","turn":{"id":"` + fixtureTurnA + `","items":[],"status":"completed"}}`
	p.HandleNotification("turn/started", []byte(turnStarted))
	p.HandleNotification("thread/tokenUsage/updated", []byte(tokenUsage))
	p.HandleNotification("turn/completed", []byte(completed))

	if got := p.ParkedCount(fixtureThreadA); got != 3 {
		t.Fatalf("expected 3 parked notifications, got %d", got)
	}

	tap, err := p.InstallTurnRoute(fixtureThreadA, fixtureTurnA)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	want := []string{"turn/started", "thread/tokenUsage/updated", "turn/completed"}
	for i, method := range want {
		select {
		case n := <-tap.C():
			if n.Method != method {
				t.Fatalf("replay %d: got %q want %q", i, n.Method, method)
			}
			if n.ThreadID != fixtureThreadA {
				t.Fatalf("replay %d thread id: %q", i, n.ThreadID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("replay %d (%s) never arrived", i, method)
		}
	}
	select {
	case n := <-tap.C():
		t.Fatalf("unexpected extra replay: %+v", n)
	default:
	}
	if got := p.ParkedCount(fixtureThreadA); got != 0 {
		t.Fatalf("park buffer must drain on install, got %d", got)
	}

	// Live notifications after installation stream through the tap.
	p.HandleNotification("item/started", []byte(`{"threadId":"`+fixtureThreadA+`","turnId":"`+fixtureTurnA+`","item":{"id":"i1"}}`))
	select {
	case n := <-tap.C():
		if n.Method != "item/started" {
			t.Fatalf("live route: %q", n.Method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live notification never reached the installed route")
	}

	// Park overflow is bounded: drop-oldest with a counter, never a leak.
	for i := 0; i < maxParkedPerThread+32; i++ {
		p.HandleNotification("item/completed", []byte(`{"threadId":"`+fixtureThreadA+`","turnId":"other-turn","item":{"id":"x"}}`))
	}
	if got := p.ParkedCount(fixtureThreadA); got > maxParkedPerThread {
		t.Fatalf("park buffer exceeded its bound: %d", got)
	}
	if p.ParkedDropped(fixtureThreadA) != 32 {
		t.Fatalf("expected 32 counted drops, got %d", p.ParkedDropped(fixtureThreadA))
	}
}

// Notifications without a thread id (connection-scope status noise, e.g.
// remoteControl/status/changed at connection setup) are captured without
// drift and without consuming thread routing.
func TestPump_GlobalNotificationsNoDrift(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()

	p.HandleNotification("remoteControl/status/changed", []byte(`{"status":"disabled","serverName":"codex"}`))
	if poisoned, err := p.Poisoned(); poisoned {
		t.Fatalf("thread-less notifications must not drift: %v", err)
	}
	got := p.GlobalNotifications()
	if len(got) != 1 || got[0].Method != "remoteControl/status/changed" {
		t.Fatalf("global capture: %+v", got)
	}
}

// A notification carrying an UNBOUND thread id (other than thread/started
// during a pending creation) is id drift per §3.5.
func TestPump_UnboundThreadNotificationDrift(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()
	fatal := make(chan error, 1)
	p.SetFatalHandler(func(err error) { fatal <- err })

	p.HandleNotification("turn/started", []byte(`{"threadId":"`+fixtureThreadB+`","turnId":"`+fixtureTurnA+`"}`))
	select {
	case <-fatal:
	case <-time.After(time.Second):
		t.Fatal("unbound-thread notification must fire the fatal handler")
	}
}

// The first finisher of a creation reservation owns its terminal outcome:
// a drift poison (finished synchronously under the pump lock) must not be
// overwritable by a later AbandonCreation.
func TestPump_FirstFinisherOutcomeWins(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()
	p.SetFatalHandler(func(error) {})

	if err := p.ReserveCreation(1); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Unbound-thread notification: drift; poison finishes the pending
	// creation with the drift cause synchronously.
	p.HandleNotification("turn/started", []byte(`{"threadId":"`+fixtureThreadB+`","turnId":"`+fixtureTurnA+`"}`))

	// A late abandon runs after the drift finish: the outcome must stay
	// the drift cause, never be rewritten to abandoned.
	p.AbandonCreation()

	outcome := p.CreationOutcome()
	var drift *ErrProtocolDrift
	if !errors.As(outcome, &drift) {
		t.Fatalf("first finisher's outcome must win, got %T: %v", outcome, outcome)
	}
	if errors.Is(outcome, ErrCreationAbandoned) {
		t.Fatal("late abandon must not overwrite the terminal drift outcome")
	}
}

// CompleteCreation must re-check the abandoned flag under its confirm
// lock: a creation abandoned while the confirm path is already past its
// done-checks must bail with the abandoned outcome and never publish a
// binding for an uncertain thread. The interleave is forced by spawning
// the confirm caller while the pump lock is held (it parks at its first
// lock, after the done-checks) with the flag already set, so the only
// question left to the scheduler is which contender wins the confirm lock
// — and both pre-fix answers are wrong.
func TestPump_AbandonDuringConfirmWindow(t *testing.T) {
	withGrace(t, 2*time.Second)
	for attempt := 0; attempt < 25; attempt++ {
		p := NewEventPump()
		p.SetFatalHandler(func(error) {})
		if err := p.ReserveCreation(1); err != nil {
			t.Fatalf("attempt %d: reserve: %v", attempt, err)
		}
		p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))

		p.mu.Lock()
		pc := p.creation
		if pc == nil {
			p.mu.Unlock()
			t.Fatalf("attempt %d: reservation vanished", attempt)
		}
		pc.abandoned = true // the flag AbandonCreation sets under the same lock
		retCh := make(chan error, 1)
		go func() { retCh <- p.CompleteCreation(fixtureThreadA) }()
		runtime.Gosched() // let the confirm path park at the pump lock
		p.mu.Unlock()
		// Hand the P to the confirm path so it advances past its
		// done-checks (the flag is set, done is still open) before the
		// abandonment's finish can close it.
		runtime.Gosched()
		runtime.Gosched()
		runtime.Gosched()

		// Complete the abandonment: closes done with the abandoned outcome.
		p.AbandonCreation()

		select {
		case err := <-retCh:
			if !errors.Is(err, ErrCreationAbandoned) {
				t.Fatalf("attempt %d: confirm in the abandoned window must bail with the abandoned outcome, got %v", attempt, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("attempt %d: CompleteCreation never returned", attempt)
		}
		if _, err := p.InstallTurnRoute(fixtureThreadA, fixtureTurnA); err == nil {
			t.Fatalf("attempt %d: binding published for an abandoned (uncertain) creation", attempt)
		}
	}
}

// Consistency loop over forced interleavings: the confirm caller's terminal
// view and the routing table must always agree — abandoned ⇒ no binding,
// confirmed ⇒ binding present.
func TestPump_ConfirmAbandonRaceInvariant(t *testing.T) {
	withGrace(t, 2*time.Second)
	for i := 0; i < 300; i++ {
		p := NewEventPump()
		p.SetFatalHandler(func(error) {})
		if err := p.ReserveCreation(1); err != nil {
			t.Fatalf("iteration %d: reserve: %v", i, err)
		}
		p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))

		retCh := make(chan error, 1)
		go func() { retCh <- p.CompleteCreation(fixtureThreadA) }()
		p.AbandonCreation()
		err := <-retCh

		_, installErr := p.InstallTurnRoute(fixtureThreadA, fixtureTurnA)
		switch {
		case errors.Is(err, ErrCreationAbandoned):
			if installErr == nil {
				t.Fatalf("iteration %d: binding published for an abandoned (uncertain) creation", i)
			}
		case err == nil:
			if installErr != nil {
				t.Fatalf("iteration %d: confirmed creation must publish the binding: %v", i, installErr)
			}
		default:
			t.Fatalf("iteration %d: unexpected terminal outcome %v", i, err)
		}
	}
}

// ReserveCreation after a poison fails closed.
func TestPump_ReserveAfterPoisonFailsClosed(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()
	p.SetFatalHandler(func(error) {})
	p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))
	if err := p.ReserveCreation(1); err == nil {
		t.Fatal("reservations on a poisoned pump must fail")
	}
	if !strings.Contains(p.PoisonCause().Error(), "thread/started") {
		t.Fatalf("poison cause should name the drift: %v", p.PoisonCause())
	}
}

// An abandoned creation tombstones the reservation: the racing
// thread/started is swallowed (the native thread may exist), no binding
// publishes, and automatic recreation stays blocked (spec §3.4).
func TestPump_AbandonedCreationSwallowsLateStarted(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()
	p.SetFatalHandler(func(error) {})

	if err := p.ReserveCreation(1); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	p.AbandonCreation()

	// The late notification arrives after the abandonment: no drift, no
	// binding.
	p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))
	if poisoned, err := p.Poisoned(); poisoned {
		t.Fatalf("late started after abandonment must not drift: %v", err)
	}
	if _, err := p.InstallTurnRoute(fixtureThreadA, fixtureTurnA); err == nil {
		t.Fatal("an abandoned creation must not publish a thread binding")
	}

	// Recreation is blocked until the adapter layer resolves the
	// uncertainty explicitly.
	if err := p.ReserveCreation(2); !errors.Is(err, ErrCreationPending) {
		t.Fatalf("recreation after abandonment must stay blocked, got %v", err)
	}
	// The tombstone outcome is observable for the adapter's
	// ErrSessionCreationUncertain path.
	if err := p.CreationOutcome(); !errors.Is(err, ErrCreationAbandoned) {
		t.Fatalf("tombstone outcome: %v", err)
	}
}

// Concurrent HandleNotification calls must not race on the route tables.
func TestPump_ConcurrentHandling(t *testing.T) {
	withGrace(t, 2*time.Second)
	p := NewEventPump()
	p.SetFatalHandler(func(error) {})
	if err := p.ReserveCreation(1); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.HandleNotification("remoteControl/status/changed", []byte(`{"status":"disabled"}`))
			p.HandleNotification("thread/started", []byte(threadStartedParams(fixtureThreadA)))
		}()
	}
	wg.Wait()
	if err := p.CompleteCreation(fixtureThreadA); err != nil {
		t.Fatalf("complete: %v", err)
	}
}
