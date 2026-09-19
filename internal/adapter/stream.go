package adapter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
)

var (
	ErrBufferOverflow  = errors.New("observation stream buffer overflow: consumer too slow")
	ErrPayloadTooLarge = errors.New("event payload exceeds maximum size limit")
	ErrStreamClosed    = errors.New("observation stream closed")
)

const (
	MaxEventPayloadBytes  = 64 * 1024   // 64 KiB
	MaxRawEvidenceBytes   = 1024 * 1024 // 1 MiB
	DefaultBufferCapacity = 64
)

type EventType string

const (
	EventProgress      EventType = "progress"
	EventToolRequested EventType = "tool_requested"
	EventToolApproved  EventType = "tool_approved"
	EventToolDenied    EventType = "tool_denied"
	EventTerminal      EventType = "terminal"
)

type Event struct {
	Ref        TurnRef
	Type       EventType
	Status     council.TurnStatus
	ApprovalID string
	Payload    string
	Usage      ExecutionUsage
	Timestamp  time.Time
}

func (e Event) Validate() error {
	if err := e.Ref.Validate(); err != nil {
		return err
	}
	if len(e.Payload) > MaxEventPayloadBytes {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(e.Payload), MaxEventPayloadBytes)
	}
	if err := e.Usage.Validate(); err != nil {
		return err
	}

	switch e.Type {
	case EventProgress:
		if e.Status != council.TurnRunning && e.Status != council.TurnCancelling {
			return fmt.Errorf("progress event cannot carry non-active status %s", e.Status)
		}
		if e.ApprovalID != "" {
			return errors.New("progress event must not have ApprovalID")
		}
	case EventToolRequested:
		if e.Status != council.TurnRunning {
			return fmt.Errorf("tool requested event must carry TurnRunning, got %s", e.Status)
		}
		if strings.TrimSpace(e.ApprovalID) == "" {
			return errors.New("tool requested event requires non-empty ApprovalID")
		}
	case EventToolApproved:
		if e.Status != council.TurnRunning {
			return fmt.Errorf("tool approved event must carry TurnRunning, got %s", e.Status)
		}
		if strings.TrimSpace(e.ApprovalID) == "" {
			return errors.New("tool approved event requires non-empty ApprovalID")
		}
	case EventToolDenied:
		if e.Status != council.TurnRunning {
			return fmt.Errorf("tool denied event must carry TurnRunning, got %s", e.Status)
		}
		if strings.TrimSpace(e.ApprovalID) == "" {
			return errors.New("tool denied event requires non-empty ApprovalID")
		}
	case EventTerminal:
		if e.Status != council.TurnCompleted && e.Status != council.TurnCancelled &&
			e.Status != council.TurnFailed && e.Status != council.TurnInterrupted {
			return fmt.Errorf("terminal event requires terminal status, got %s", e.Status)
		}
		if e.ApprovalID != "" {
			return errors.New("terminal event must not have ApprovalID")
		}
	default:
		return fmt.Errorf("unknown event type %s", e.Type)
	}
	return nil
}

// Stream provides a receive-only channel of ordered events and stable terminal error reporting.
type Stream interface {
	Events() <-chan Event
	Err() error
	Close() error
}

// BufferedStream implements Stream with a bounded channel, cancellation context, and producer protection.
type BufferedStream struct {
	ref    TurnRef
	events chan Event
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.RWMutex
	wg     sync.WaitGroup
	err    error
	closed bool
}

// NewBufferedStream initializes a new BufferedStream and returns the write channel.
func NewBufferedStream(ref TurnRef, capacity int) (*BufferedStream, chan<- Event) {
	if capacity <= 0 {
		capacity = DefaultBufferCapacity
	}
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan Event, capacity)
	bs := &BufferedStream{
		ref:    ref,
		events: events,
		ctx:    ctx,
		cancel: cancel,
	}
	return bs, events
}

// Events returns the receive-only channel of events.
func (s *BufferedStream) Events() <-chan Event {
	return s.events
}

// Err returns the stable terminal error after observation ends.
func (s *BufferedStream) Err() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.err
}

// Close idempotently terminates observation and sets ErrStreamClosed if not already set.
func (s *BufferedStream) Close() error {
	return s.CloseWithErr(ErrStreamClosed)
}

// CloseWithErr idempotently terminates observation with a specified terminal error.
func (s *BufferedStream) CloseWithErr(err error) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.err = err
	s.cancel()
	s.mu.Unlock()

	s.wg.Wait()
	close(s.events)
	return nil
}

// Send attempts to send an event into the buffer, unblocking and returning false if closed.
func (s *BufferedStream) Send(ev Event) bool {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return false
	}
	s.wg.Add(1)
	s.mu.RUnlock()
	defer s.wg.Done()

	select {
	case <-s.ctx.Done():
		return false
	case s.events <- ev:
		return true
	}
}

// SendOrOverflow attempts to send an event into available buffer capacity.
// If the buffer is full, it immediately closes the stream with ErrBufferOverflow and returns it.
func (s *BufferedStream) SendOrOverflow(ev Event) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return ErrStreamClosed
	}
	s.wg.Add(1)
	s.mu.RUnlock()

	select {
	case <-s.ctx.Done():
		s.wg.Done()
		return ErrStreamClosed
	case s.events <- ev:
		s.wg.Done()
		return nil
	default:
		s.wg.Done()
		_ = s.CloseWithErr(ErrBufferOverflow)
		return ErrBufferOverflow
	}
}
