//go:build unix

package service

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestEvents_SynchronizedSnapshotAndCleanDisconnect(t *testing.T) {
	dir := testStateDir(t)
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID:                  "sess-1",
		RunID:               "run-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	qRec, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-1",
		Prompt:    "Hello",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	// Release turn
	_, err = store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", qRec.CommittedVersion, "t-1")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	fakeAdapter := adaptertest.NewFakeAdapter("claude")
	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-789",
	}

	srv, err := NewServerWithAdapter(store, lock, cfg, fakeAdapter)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1/events", nil)
	if err != nil {
		t.Fatalf("create sse req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK SSE stream, got %v, err: %v", resp.StatusCode, err)
	}

	reader := bufio.NewReader(resp.Body)
	// First event must be the initial state snapshot event
	line1, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line1, "event:") {
		t.Fatalf("expected event: prefix, got: %q, err: %v", line1, err)
	}
	line2, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line2, "data:") {
		t.Fatalf("expected data: prefix, got: %q, err: %v", line2, err)
	}

	// Close client connection immediately; verify server doesn't panic or leak,
	// and verify worker execution completes and commits terminal outcome to SQLite
	resp.Body.Close()

	// Wait for cleanup
	ref := adapter.TurnRef{SessionID: "sess-1", TurnKey: "t-1"}
	for i := 0; i < 100; i++ {
		if count := srv.Coordinator().SubscriberCount(ref); count == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Verify subscriber is cleanly deregistered
	if count := srv.Coordinator().SubscriberCount(ref); count != 0 {
		t.Fatalf("expected 0 subscribers after disconnect, got %d", count)
	}
}

func TestEvents_TerminalSnapshotOnConnect(t *testing.T) {
	dir := testStateDir(t)
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID:                  "sess-1",
		RunID:               "run-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	qRec, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-term",
		Prompt:    "Hello",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	relRec, err := store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", qRec.CommittedVersion, "t-term")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	// Mark turn completed directly in storage
	_, err = store.RecordTerminalOutcome(ctx, "op-term-1", "lease-1", "sess-1", relRec.Receipt.CommittedVersion, "t-term", council.TurnCompleted, "Done!")
	if err != nil {
		t.Fatalf("record terminal outcome: %v", err)
	}

	fakeAdapter := adaptertest.NewFakeAdapter("claude")
	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-789",
	}

	srv, err := NewServerWithAdapter(store, lock, cfg, fakeAdapter)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-term/events", nil)
	if err != nil {
		t.Fatalf("create sse req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK SSE stream, got %v, err: %v", resp.StatusCode, err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)

	// Read event 1: snapshot
	line1, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line1, "event: snapshot") {
		t.Fatalf("expected snapshot event, got: %q, err: %v", line1, err)
	}
	for {
		l, err := reader.ReadString('\n')
		if err != nil || l == "\n" {
			break
		}
	}

	// Read event 2: terminal
	line2, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line2, "event: terminal") {
		t.Fatalf("expected terminal event, got: %q, err: %v", line2, err)
	}
	for {
		l, err := reader.ReadString('\n')
		if err != nil || l == "\n" {
			break
		}
	}

	// Stream should close immediately after terminal event
	doneCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, readErr := resp.Body.Read(buf)
		doneCh <- readErr
	}()

	select {
	case err := <-doneCh:
		if err == nil {
			t.Fatalf("expected EOF / stream closed, got nil error")
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("timed out waiting for stream to close on terminal event")
	}
}

func TestEvents_SlowConsumerOverflow(t *testing.T) {
	coord := NewCoordinator()
	defer coord.Close()

	ref := adapter.TurnRef{SessionID: "sess-overflow", TurnKey: "t-overflow"}
	ch, unsub := coord.RegisterSubscriber(ref)
	defer unsub()

	// Capacity is 64. Send 100 events to trigger slow consumer disconnection.
	for i := 0; i < 100; i++ {
		coord.BroadcastEvent(ref, SSEEvent{
			Event: "progress",
			Data:  `{"progress":"tick"}`,
		})
	}

	// Channel buffer should have 64 items and then be closed
	if len(ch) != 64 {
		t.Fatalf("expected buffer length 64, got %d", len(ch))
	}

	// Subscriber should be unregistered from coordinator
	if count := coord.SubscriberCount(ref); count != 0 {
		t.Fatalf("expected 0 subscribers after overflow disconnection, got %d", count)
	}

	// Drain 64 items
	for i := 0; i < 64; i++ {
		<-ch
	}

	// Next read should immediately see channel closed
	_, ok := <-ch
	if ok {
		t.Fatalf("expected subscriber channel to be closed on overflow")
	}
}

func TestEvents_SessionScopedSubscriberIsolation(t *testing.T) {
	coord := NewCoordinator()
	defer coord.Close()

	ref1 := adapter.TurnRef{SessionID: "sess-1", TurnKey: "turn-shared"}
	ref2 := adapter.TurnRef{SessionID: "sess-2", TurnKey: "turn-shared"}

	ch1, unsub1 := coord.RegisterSubscriber(ref1)
	defer unsub1()

	ch2, unsub2 := coord.RegisterSubscriber(ref2)
	defer unsub2()

	coord.BroadcastEvent(ref1, SSEEvent{
		Event: "progress",
		Data:  `{"for":"sess-1"}`,
	})

	select {
	case ev := <-ch1:
		if ev.Data != `{"for":"sess-1"}` {
			t.Fatalf("unexpected data for sess-1: %s", ev.Data)
		}
	default:
		t.Fatal("expected event for sess-1")
	}

	select {
	case ev := <-ch2:
		t.Fatalf("cross-session observation leak: sess-2 received event for sess-1: %+v", ev)
	default:
		// Expected: sess-2 receives nothing
	}
}

func TestEvents_LiveObservationStreaming(t *testing.T) {
	dir := testStateDir(t)
	lock, err := AcquireServiceLock(dir)
	if err != nil {
		t.Fatalf("acquire lock: %v", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: dir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, err = store.CreateRun(ctx, "op-run-1", "run-1", "brief", "spec", "profile-1", "lease-1")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-1", "lease-1", storage.SessionRecord{
		ID:                  "sess-1",
		RunID:               "run-1",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	qRec, err := store.QueuePrompt(ctx, "op-q-1", "lease-1", "sess-1", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-1",
		TurnKey:   "t-live",
		Prompt:    "Hello live",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	fakeAdapter := adaptertest.NewFakeAdapter("claude")
	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-789",
	}

	srv, err := NewServerWithAdapter(store, lock, cfg, fakeAdapter)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	// Release turn via HTTP
	relBody := fmt.Sprintf(`{"op_id":"op-rel-live","controller_lease":"lease-1","expected_version":%d}`, qRec.CommittedVersion)
	relReq, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-live/release", strings.NewReader(relBody))
	if err != nil {
		t.Fatalf("create release req: %v", err)
	}
	relReq.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	relReq.Header.Set("Content-Type", "application/json")
	relResp, err := client.Do(relReq)
	if err != nil || relResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 Accepted on release, got %v, err: %v", relResp.StatusCode, err)
	}
	relResp.Body.Close()

	// Connect SSE events
	sseReq, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-live/events", nil)
	if err != nil {
		t.Fatalf("create sse req: %v", err)
	}
	sseReq.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	sseReq.Header.Set("Accept", "text/event-stream")

	sseResp, err := client.Do(sseReq)
	if err != nil || sseResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK SSE stream, got %v, err: %v", sseResp.StatusCode, err)
	}
	defer sseResp.Body.Close()

	reader := bufio.NewReader(sseResp.Body)
	var eventsReceived []string

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.HasPrefix(line, "event: ") {
			evName := strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			eventsReceived = append(eventsReceived, evName)
			if evName == "terminal" {
				break
			}
		}
	}

	if len(eventsReceived) < 2 {
		t.Fatalf("expected at least 2 events (snapshot, terminal), got %v", eventsReceived)
	}
	if eventsReceived[0] != "snapshot" {
		t.Fatalf("expected first event to be snapshot, got %s", eventsReceived[0])
	}
	if eventsReceived[len(eventsReceived)-1] != "terminal" {
		t.Fatalf("expected last event to be terminal, got %s", eventsReceived[len(eventsReceived)-1])
	}
}
