package opencode

// Task 5 event-pump evidence (cross-platform): the pump is adapter-owned,
// survives caller detach, denies permissions by default, deduplicates by
// native event id, and resumes from the recorded cursor after a native
// stream drop.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
)

func drainStream(t *testing.T, adp *OpenCodeAdapter, fake *fakeOpenCodeServer, stream adapter.Stream, want int, timeout time.Duration) []adapter.Event {
	t.Helper()
	var got []adapter.Event
	deadline := time.After(timeout)
	for len(got) < want {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				t.Fatalf("stream closed after %d events, wanted %d", len(got), want)
			}
			got = append(got, ev)
		case <-deadline:
			adp.mu.Lock()
			var cursors []int64
			var taps int
			for _, p := range adp.pumps {
				p.mu.Lock()
				cursors = append(cursors, p.cursor)
				taps = len(p.turns)
				p.mu.Unlock()
			}
			adp.mu.Unlock()
			t.Fatalf("timeout waiting for events: got %d want %d; sse=%v cursors=%v taps=%d", len(got), want, fake.ledger.sseConnects, cursors, taps)
		}
	}
	return got
}

// Cancelling an Observe caller detaches only that tap: the native stream
// keeps draining, and later events reach a fresh Observe exactly once.
func TestEventPump_CallerDetachKeepsNativeStream(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx, cancel := context.WithCancel(context.Background())

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-detach"}
	outcome, err := adp.Dispatch(ctx, ref, "prompt")
	if err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}
	// Events route by parentID == deterministic user message ID; script
	// them after dispatch so the real ID is known.
	msgID := adp.dispatches[ref].userMessageID

	stream, err := adp.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	fake.mu.Lock()
	fake.sessions[gate1Session].scriptedEvents = append(fake.sessions[gate1Session].scriptedEvents,
		`{"type":"message.updated","part":{"text":"chunk 1"},"parentID":"`+msgID+`"}`,
	)
	fake.mu.Unlock()
	got := drainStream(t, adp, fake, stream, 1, 3*time.Second)
	if got[0].Type != adapter.EventProgress || got[0].Payload != "chunk 1" {
		t.Fatalf("unexpected first event: %+v", got[0])
	}

	// Detach: only the tap ends.
	cancel()
	time.Sleep(150 * time.Millisecond)

	stream2, err := adp.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("re-observe: %v", err)
	}

	// Script later events; the pump must still be draining natively and
	// route them to the fresh tap.
	fake.mu.Lock()
	fake.sessions[gate1Session].scriptedEvents = append(fake.sessions[gate1Session].scriptedEvents,
		`{"type":"message.updated","part":{"text":"chunk 2"},"parentID":"`+msgID+`"}`,
		`{"type":"message.completed","parentID":"`+msgID+`"}`,
	)
	fake.mu.Unlock()

	got2 := drainStream(t, adp, fake, stream2, 2, 3*time.Second)
	if got2[0].Payload != "chunk 2" || got2[1].Type != adapter.EventTerminal {
		t.Fatalf("post-detach events must flow without duplication: %+v", got2)
	}

	// The native stream was never reconnected for a detach: one
	// connection since dispatch.
	if got := fake.ledger.sseConnects; len(got) != 1 {
		t.Fatalf("caller detach must not close or reconnect the native stream, connections: %v", got)
	}
}

// Permission requests are answered with an explicit denial and mirrored
// into the turn's stream as tool_requested then tool_denied.
func TestEventPump_PermissionDeniedByDefault(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-perm"}
	fake.addPermission(gate1Session, "perm_1", "bash")

	if outcome, err := adp.Dispatch(ctx, ref, "prompt"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}
	msgID := adp.dispatches[ref].userMessageID

	stream, err := adp.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	fake.mu.Lock()
	fake.sessions[gate1Session].scriptedEvents = append(fake.sessions[gate1Session].scriptedEvents,
		`{"type":"permission.requested","permission":{"id":"perm_1","type":"bash"},"parentID":"`+msgID+`"}`,
	)
	fake.mu.Unlock()

	got := drainStream(t, adp, fake, stream, 2, 3*time.Second)
	if got[0].Type != adapter.EventToolRequested || got[0].ApprovalID != "perm_1" {
		t.Fatalf("expected tool_requested first, got %+v", got[0])
	}
	if got[1].Type != adapter.EventToolDenied || got[1].ApprovalID != "perm_1" {
		t.Fatalf("expected tool_denied second, got %+v", got[1])
	}
	if !strings.Contains(got[1].Payload, "denied") {
		t.Fatalf("denial must be explicit, payload: %q", got[1].Payload)
	}

	// The native permission endpoint received the deny reply.
	reply := fake.permissionStatus(gate1Session, "perm_1")
	replies := len(fake.ledger.permissionReplies)
	if reply != "denied" {
		t.Fatalf("permission must be denied on the native side, got %q", reply)
	}
	if replies != 1 {
		t.Fatalf("expected exactly 1 native deny reply, got %d", replies)
	}
}

// After a native stream drop, the pump reconnects with the recorded
// cursor and old events are not duplicated.
func TestEventPump_CursorResumeAfterNativeDrop(t *testing.T) {
	adp, fake := gate1Fixture(t)
	ctx := context.Background()

	ref := adapter.TurnRef{SessionID: gate1Session, TurnKey: "t-cursor"}
	if outcome, err := adp.Dispatch(ctx, ref, "prompt"); err != nil || outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: status=%v err=%v", outcome.Status, err)
	}
	msgID := adp.dispatches[ref].userMessageID

	stream, err := adp.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	fake.mu.Lock()
	fake.sessions[gate1Session].scriptedEvents = append(fake.sessions[gate1Session].scriptedEvents,
		`{"type":"message.updated","part":{"text":"one"},"parentID":"`+msgID+`"}`,
		`{"type":"message.updated","part":{"text":"two"},"parentID":"`+msgID+`"}`,
	)
	fake.mu.Unlock()
	drainStream(t, adp, fake, stream, 2, 3*time.Second)

	// Drop the native stream: the pump reconnects with Last-Event-ID and
	// the fake resumes from the cursor.
	fake.mu.Lock()
	fake.sessions[gate1Session].scriptedEvents = []string{
		`{"type":"message.updated","part":{"text":"three"},"parentID":"` + msgID + `"}`,
	}
	fake.mu.Unlock()
	fake.armFlakyEvents()

	got := drainStream(t, adp, fake, stream, 1, 3*time.Second)
	if got[0].Payload != "three" {
		t.Fatalf("resume must deliver only new events, got %+v", got[0])
	}
	if got := fake.ledger.sseConnects; len(got) != 2 {
		t.Fatalf("expected exactly one reconnect, connections: %v", got)
	}
}
