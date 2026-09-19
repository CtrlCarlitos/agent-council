package adapter_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

func TestEvent_Validation(t *testing.T) {
	ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}

	// Valid progress event
	ev := adapter.Event{
		Ref:     ref,
		Type:    adapter.EventProgress,
		Status:  council.TurnRunning,
		Payload: "in progress",
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("expected valid progress event, got %v", err)
	}

	// Tool requested requires non-empty ApprovalID
	toolReq := adapter.Event{
		Ref:     ref,
		Type:    adapter.EventToolRequested,
		Status:  council.TurnRunning,
		Payload: "shell",
	}
	if err := toolReq.Validate(); err == nil {
		t.Fatal("expected error for EventToolRequested without ApprovalID")
	}

	toolReq.ApprovalID = "app-123"
	if err := toolReq.Validate(); err != nil {
		t.Fatalf("expected valid tool requested, got %v", err)
	}

	// Tool approved
	toolApp := adapter.Event{
		Ref:        ref,
		Type:       adapter.EventToolApproved,
		Status:     council.TurnRunning,
		ApprovalID: "app-123",
		Payload:    "approved",
	}
	if err := toolApp.Validate(); err != nil {
		t.Fatalf("expected valid tool approved, got %v", err)
	}

	// Tool approved with non-running status
	toolAppBad := toolApp
	toolAppBad.Status = council.TurnCompleted
	if err := toolAppBad.Validate(); err == nil {
		t.Fatal("expected error for tool approved with non-running status")
	}

	// Tool denied requires TurnRunning and non-empty ApprovalID (does not retire turn)
	toolDenied := adapter.Event{
		Ref:        ref,
		Type:       adapter.EventToolDenied,
		Status:     council.TurnRunning,
		ApprovalID: "app-123",
		Payload:    "denied by operator",
	}
	if err := toolDenied.Validate(); err != nil {
		t.Fatalf("expected valid tool denied, got %v", err)
	}

	toolDeniedTerminal := toolDenied
	toolDeniedTerminal.Status = council.TurnFailed
	if err := toolDeniedTerminal.Validate(); err == nil {
		t.Fatal("expected error for tool denied with terminal status (denial must not retire turn)")
	}

	// Progress cannot carry terminal status
	invalidProg := adapter.Event{
		Ref:     ref,
		Type:    adapter.EventProgress,
		Status:  council.TurnCompleted,
		Payload: "done",
	}
	if err := invalidProg.Validate(); err == nil {
		t.Fatal("expected error for progress event with terminal status")
	}

	// Progress cannot carry ApprovalID
	invalidProgApp := ev
	invalidProgApp.ApprovalID = "some-app"
	if err := invalidProgApp.Validate(); err == nil {
		t.Fatal("expected error for progress event with ApprovalID")
	}

	// Terminal event valid statuses
	for _, termStatus := range []council.TurnStatus{
		council.TurnCompleted,
		council.TurnCancelled,
		council.TurnFailed,
		council.TurnInterrupted,
	} {
		termEv := adapter.Event{
			Ref:     ref,
			Type:    adapter.EventTerminal,
			Status:  termStatus,
			Payload: "terminal outcome",
		}
		if err := termEv.Validate(); err != nil {
			t.Fatalf("expected valid terminal event for %s, got %v", termStatus, err)
		}
	}

	// Terminal event cannot carry TurnRunning
	termBad := adapter.Event{
		Ref:     ref,
		Type:    adapter.EventTerminal,
		Status:  council.TurnRunning,
		Payload: "not terminal",
	}
	if err := termBad.Validate(); err == nil {
		t.Fatal("expected error for terminal event with TurnRunning status")
	}

	// Terminal event cannot carry ApprovalID
	termAppBad := adapter.Event{
		Ref:        ref,
		Type:       adapter.EventTerminal,
		Status:     council.TurnCompleted,
		ApprovalID: "app-123",
	}
	if err := termAppBad.Validate(); err == nil {
		t.Fatal("expected error for terminal event with ApprovalID")
	}

	// Payload limit (64 KiB)
	oversized := adapter.Event{
		Ref:     ref,
		Type:    adapter.EventProgress,
		Status:  council.TurnRunning,
		Payload: strings.Repeat("x", adapter.MaxEventPayloadBytes+1),
	}
	if err := oversized.Validate(); err == nil {
		t.Fatal("expected error for oversized payload")
	} else if !errors.Is(err, adapter.ErrPayloadTooLarge) {
		t.Fatalf("expected ErrPayloadTooLarge, got: %v", err)
	}

	// Invalid Ref
	badRefEv := ev
	badRefEv.Ref = adapter.TurnRef{SessionID: "", TurnKey: ""}
	if err := badRefEv.Validate(); err == nil {
		t.Fatal("expected error for invalid turn ref")
	}
}

func TestBufferedStream_ProducerUnblockedOnClose(t *testing.T) {
	ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}
	stream, in := adapter.NewBufferedStream(ref, 2)

	// Send 2 events to fill buffer
	in <- adapter.Event{Ref: ref, Type: adapter.EventProgress, Status: council.TurnRunning, Payload: "1"}
	in <- adapter.Event{Ref: ref, Type: adapter.EventProgress, Status: council.TurnRunning, Payload: "2"}

	// Launch producer trying to send a 3rd event
	doneProducer := make(chan struct{})
	go func() {
		defer close(doneProducer)
		ev := adapter.Event{Ref: ref, Type: adapter.EventProgress, Status: council.TurnRunning, Payload: "3"}
		stream.Send(ev)
	}()

	// Give the goroutine a moment to enter stream.Send and block
	time.Sleep(20 * time.Millisecond)

	// Consumer closes stream without reading
	if err := stream.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Producer must unblock promptly
	select {
	case <-doneProducer:
	case <-time.After(1 * time.Second):
		t.Fatal("producer remained blocked after stream.Close()")
	}

	// Err() must return ErrStreamClosed
	if !errors.Is(stream.Err(), adapter.ErrStreamClosed) {
		t.Fatalf("expected ErrStreamClosed, got %v", stream.Err())
	}
}

func TestBufferedStream_ConcurrentCloseIsIdempotent(t *testing.T) {
	ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}
	stream, _ := adapter.NewBufferedStream(ref, 10)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = stream.Close()
		}()
	}
	wg.Wait()

	if err := stream.Close(); err != nil {
		t.Fatalf("repeated Close failed: %v", err)
	}
}

func TestBufferedStream_SlowConsumerOverflow(t *testing.T) {
	ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}
	stream, in := adapter.NewBufferedStream(ref, 2)

	// Fill buffer
	in <- adapter.Event{Ref: ref, Type: adapter.EventProgress, Status: council.TurnRunning, Payload: "1"}
	in <- adapter.Event{Ref: ref, Type: adapter.EventProgress, Status: council.TurnRunning, Payload: "2"}

	// Pushing beyond capacity with SendOrOverflow
	err := stream.SendOrOverflow(adapter.Event{Ref: ref, Type: adapter.EventProgress, Status: council.TurnRunning, Payload: "3"})
	if !errors.Is(err, adapter.ErrBufferOverflow) {
		t.Fatalf("expected ErrBufferOverflow, got %v", err)
	}
	if !errors.Is(stream.Err(), adapter.ErrBufferOverflow) {
		t.Fatalf("expected stream.Err() to be ErrBufferOverflow, got %v", stream.Err())
	}

	// Subsequent SendOrOverflow returns ErrStreamClosed
	err2 := stream.SendOrOverflow(adapter.Event{Ref: ref, Type: adapter.EventProgress, Status: council.TurnRunning, Payload: "4"})
	if !errors.Is(err2, adapter.ErrStreamClosed) {
		t.Fatalf("expected ErrStreamClosed on closed stream, got %v", err2)
	}
}

func TestBufferedStream_NormalCompletionDrainsCleanly(t *testing.T) {
	ref := adapter.TurnRef{SessionID: "s1", TurnKey: "t1"}
	stream, in := adapter.NewBufferedStream(ref, 5)

	in <- adapter.Event{Ref: ref, Type: adapter.EventProgress, Status: council.TurnRunning, Payload: "p1"}
	in <- adapter.Event{Ref: ref, Type: adapter.EventTerminal, Status: council.TurnCompleted, Payload: "done"}

	if err := stream.CloseWithErr(nil); err != nil {
		t.Fatalf("CloseWithErr(nil) failed: %v", err)
	}

	var received []adapter.Event
	for ev := range stream.Events() {
		received = append(received, ev)
	}

	if len(received) != 2 {
		t.Fatalf("expected 2 received events, got %d", len(received))
	}
	if stream.Err() != nil {
		t.Fatalf("expected nil Err() on normal completion, got %v", stream.Err())
	}
}
