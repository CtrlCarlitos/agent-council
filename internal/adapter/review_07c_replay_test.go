package adapter_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
)

// TestReview07C_ReplayBufferBoundaries verifies that completed turn replay does not deadlock
// when retained history fills or exceeds the stream buffer capacity (64 slots), and that
// explicit overflow is reported without hanging or blocking the fake's mutex.
func TestReview07C_ReplayBufferBoundaries(t *testing.T) {
	cases := []struct {
		name               string
		emitCount          int // progress events emitted in addition to initial worker progress
		expectOverflow     bool
		expectCleanCollect bool
	}{
		{
			name:               "1 event history (initial worker progress only)",
			emitCount:          0, // 1 event in history
			expectOverflow:     false,
			expectCleanCollect: true,
		},
		{
			name:               "63 events history (initial + 62 emits)",
			emitCount:          62, // 1 + 62 = 63 events in history, terminal in slot 64
			expectOverflow:     false,
			expectCleanCollect: true,
		},
		{
			name:               "64 events history (initial + 63 emits; fills 64 slots, terminal overflows)",
			emitCount:          63, // 1 + 63 = 64 events in history, terminal causes buffer overflow
			expectOverflow:     true,
			expectCleanCollect: true,
		},
		{
			name:               "65 events history (overflow during history replay)",
			emitCount:          64, // 1 + 64 = 65 events, overflows during history replay
			expectOverflow:     true,
			expectCleanCollect: true,
		},
		{
			name:               "128 events history (capped at maxTurnEventHistory, explicit overflow)",
			emitCount:          150, // exceeds cap; bounded and overflows gracefully
			expectOverflow:     true,
			expectCleanCollect: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := adaptertest.NewFake(adaptertest.ScriptedFaults{
				EmitProgressCount: tc.emitCount,
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			ref := adapter.TurnRef{SessionID: "sess-07c", TurnKey: "turn-07c"}
			_, err := fake.CreateSession(ctx, adapter.CreateSessionRequest{
				SessionID:   ref.SessionID,
				Contributor: council.Claude,
				Config:      adapter.SessionConfig{Model: "claude-3-5-sonnet"},
			})
			if err != nil {
				t.Fatalf("CreateSession failed: %v", err)
			}

			// Dispatch turn
			_, err = fake.Dispatch(ctx, ref, "test prompt")
			if err != nil {
				t.Fatalf("Dispatch failed: %v", err)
			}

			// Wait for worker to complete by polling TurnState or Collect
			var res adapter.TurnResult
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				st := fake.TurnState(ref)
				if !fake.IsExecutionActive(ref) && st.Received {
					res, err = fake.Collect(ctx, ref)
					if err == nil && res.Status == council.TurnCompleted {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
			}
			if res.Status != council.TurnCompleted {
				t.Fatalf("turn failed to reach completion before Observe: %v", res)
			}

			// Observe after completion: MUST NOT DEADLOCK!
			observeDone := make(chan struct{})
			var stream adapter.Stream
			var obsErr error
			go func() {
				defer close(observeDone)
				stream, obsErr = fake.Observe(ctx, ref)
			}()

			select {
			case <-observeDone:
				if obsErr != nil {
					t.Fatalf("Observe returned error: %v", obsErr)
				}
			case <-time.After(1 * time.Second):
				t.Fatal("DEADLOCK DETECTED: Observe blocked and did not return within 1s")
			}

			// Consume stream to completion
			var eventCount int
			for range stream.Events() {
				eventCount++
			}

			if tc.expectOverflow {
				if !errors.Is(stream.Err(), adapter.ErrBufferOverflow) {
					t.Fatalf("expected ErrBufferOverflow, got %v (read %d events)", stream.Err(), eventCount)
				}
			} else {
				if stream.Err() != nil {
					t.Fatalf("expected clean stream completion, got err: %v", stream.Err())
				}
			}
		})
	}
}
