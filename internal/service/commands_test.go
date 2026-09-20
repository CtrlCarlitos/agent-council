package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestCommands_TurnReadCancelAndReconcile(t *testing.T) {
	dir := t.TempDir()
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

	// Release turn first so it is running (cancellation requires an active running turn)
	relRes, err := store.ReleaseTurn(ctx, "op-rel-1", "lease-1", "sess-1", qRec.CommittedVersion, "t-1")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	fakeAdapter := adaptertest.NewFakeAdapter("claude")
	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-test-1",
		AuthToken:  "test-token-456",
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

	// 1. Authoritative Turn Read: GET turn returns full details
	req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1", nil)
	if err != nil {
		t.Fatalf("create read req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("turn read request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on turn read, got %v", resp.StatusCode)
	}

	// 2. Cancellation Request against active running turn
	cancelBody, err := json.Marshal(CancelRequest{
		OpID:            "op-cancel-1",
		ControllerLease: "lease-1",
		ExpectedVersion: relRes.Receipt.CommittedVersion,
		Reason:          "user requested",
	})
	if err != nil {
		t.Fatalf("marshal cancel body: %v", err)
	}
	reqCancel, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/t-1/cancel", bytes.NewReader(cancelBody))
	if err != nil {
		t.Fatalf("create cancel req: %v", err)
	}
	reqCancel.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqCancel.Header.Set("Content-Type", "application/json")
	respCancel, err := client.Do(reqCancel)
	if err != nil {
		t.Fatalf("cancel request: %v", err)
	}
	defer respCancel.Body.Close()
	if respCancel.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on cancel request, got %v", respCancel.StatusCode)
	}
	var cResp CancelResponse
	if err := json.NewDecoder(respCancel.Body).Decode(&cResp); err != nil {
		t.Fatalf("decode cancel resp: %v", err)
	}
	if cResp.CancellationStatus != "requested" && cResp.CancellationStatus != "confirmed" {
		t.Fatalf("unexpected cancellation status: %s", cResp.CancellationStatus)
	}

	// 3. Controller Reattachment: session-scoped connect validates version
	connectBody, err := json.Marshal(ControllerConnectRequest{
		OpID:            "op-conn-1",
		ControllerLease: "lease-1",
		ExpectedVersion: cResp.Receipt.CommittedVersion,
	})
	if err != nil {
		t.Fatalf("marshal connect body: %v", err)
	}
	reqConn, err := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/controller/connect", bytes.NewReader(connectBody))
	if err != nil {
		t.Fatalf("create conn req: %v", err)
	}
	reqConn.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqConn.Header.Set("Content-Type", "application/json")
	respConn, err := client.Do(reqConn)
	if err != nil {
		t.Fatalf("connect req: %v", err)
	}
	defer respConn.Body.Close()
	if respConn.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on connect, got %v", respConn.StatusCode)
	}
}

func TestCommands_PromptLifecycleAndDecisions(t *testing.T) {
	dir := t.TempDir()
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
	_, err = store.CreateRun(ctx, "op-run-cmd", "run-cmd", "brief", "spec", "profile-1", "lease-cmd")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-cmd", "lease-cmd", storage.SessionRecord{
		ID:                  "sess-cmd",
		RunID:               "run-cmd",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-cmd",
		AuthToken:  "token-cmd",
	}
	srv, err := NewServer(store, lock, cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Close()

	client := newTestClient(srv.SocketPath())

	// 1. Queue prompt
	queueBody, _ := json.Marshal(QueuePromptRequest{
		OpID:            "op-q-cmd",
		ControllerLease: "lease-cmd",
		ExpectedVersion: sessRec.CommittedVersion,
		TurnKey:         "t-q",
		Prompt:          "Initial prompt text",
	})
	reqQ, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-cmd/sessions/sess-cmd/prompts/queue", bytes.NewReader(queueBody))
	reqQ.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqQ.Header.Set("Content-Type", "application/json")
	respQ, err := client.Do(reqQ)
	if err != nil {
		t.Fatalf("queue prompt req: %v", err)
	}
	defer respQ.Body.Close()
	if respQ.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on queue prompt, got %d", respQ.StatusCode)
	}
	var qResp QueuePromptResponse
	_ = json.NewDecoder(respQ.Body).Decode(&qResp)

	// 2. Replace prompt
	replaceBody, _ := json.Marshal(ReplacePromptRequest{
		OpID:            "op-r-cmd",
		ControllerLease: "lease-cmd",
		ExpectedVersion: qResp.Receipt.CommittedVersion,
		Prompt:          "Replaced prompt text",
	})
	reqR, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-cmd/sessions/sess-cmd/prompts/t-q/replace", bytes.NewReader(replaceBody))
	reqR.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqR.Header.Set("Content-Type", "application/json")
	respR, err := client.Do(reqR)
	if err != nil {
		t.Fatalf("replace prompt req: %v", err)
	}
	defer respR.Body.Close()
	if respR.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on replace prompt, got %d", respR.StatusCode)
	}
	var rResp ReplacePromptResponse
	_ = json.NewDecoder(respR.Body).Decode(&rResp)

	// 3. Discard prompt
	discardBody, _ := json.Marshal(DiscardPromptRequest{
		OpID:            "op-d-cmd",
		ControllerLease: "lease-cmd",
		ExpectedVersion: rResp.Receipt.CommittedVersion,
	})
	reqD, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-cmd/sessions/sess-cmd/prompts/t-q/discard", bytes.NewReader(discardBody))
	reqD.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqD.Header.Set("Content-Type", "application/json")
	respD, err := client.Do(reqD)
	if err != nil {
		t.Fatalf("discard prompt req: %v", err)
	}
	defer respD.Body.Close()
	if respD.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on discard prompt, got %d", respD.StatusCode)
	}

	// 4. Record decision: publish artifact revision 1 first
	_, err = store.PublishArtifact(ctx, "op-art-1", "lease-cmd", storage.ArtifactMetadata{
		ID:        "art-1",
		RunID:     "run-cmd",
		SessionID: "sess-cmd",
		TurnKey:   "t-q",
		Name:      "spec.md",
	}, []byte("artifact content"))
	if err != nil {
		t.Fatalf("publish artifact: %v", err)
	}

	decBody, _ := json.Marshal(RecordDecisionRequest{
		OpID:            "op-dec-cmd",
		ControllerLease: "lease-cmd",
		ArtifactID:      "art-1",
		Revision:        1,
		DecisionPayload: `{"approved": true}`,
	})
	reqDec, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-cmd/decisions", bytes.NewReader(decBody))
	reqDec.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqDec.Header.Set("Content-Type", "application/json")
	respDec, err := client.Do(reqDec)
	if err != nil {
		t.Fatalf("decision req: %v", err)
	}
	defer respDec.Body.Close()
	if respDec.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on decision, got %d", respDec.StatusCode)
	}
}

func TestCommands_CompositeReconciliation(t *testing.T) {
	dir := t.TempDir()
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
	_, err = store.CreateRun(ctx, "op-run-rec", "run-rec", "brief", "spec", "profile-1", "lease-rec")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-rec", "lease-rec", storage.SessionRecord{
		ID:                  "sess-rec",
		RunID:               "run-rec",
		Contributor:         "claude",
		Role:                "reviewer",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	qRec, err := store.QueuePrompt(ctx, "op-q-rec", "lease-rec", "sess-rec", sessRec.CommittedVersion, storage.PendingPrompt{
		SessionID: "sess-rec",
		TurnKey:   "t-rec",
		Prompt:    "Prompt for reconcile test",
	})
	if err != nil {
		t.Fatalf("queue prompt: %v", err)
	}

	relRes, err := store.ReleaseTurn(ctx, "op-rel-rec", "lease-rec", "sess-rec", qRec.CommittedVersion, "t-rec")
	if err != nil {
		t.Fatalf("release turn: %v", err)
	}

	fakeAdapter := adaptertest.NewFakeAdapter("claude")
	cfg := ServerConfig{
		StateDir:   dir,
		InstanceID: "inst-rec",
		AuthToken:  "token-rec",
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

	// 1. Initial reconciliation: opens episode via RecordHostLoss, calls adapter Reconcile, commits ReconcileSession
	recBody, _ := json.Marshal(ReconcileRequest{
		OpID:            "op-rec-composite",
		ControllerLease: "lease-rec",
		ExpectedVersion: relRes.Receipt.CommittedVersion,
	})
	reqRec, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-rec/sessions/sess-rec/turns/t-rec/reconcile", bytes.NewReader(recBody))
	reqRec.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqRec.Header.Set("Content-Type", "application/json")

	respRec, err := client.Do(reqRec)
	if err != nil {
		t.Fatalf("reconcile request: %v", err)
	}
	defer respRec.Body.Close()
	if respRec.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on initial reconcile, got %d", respRec.StatusCode)
	}
	var recResp ReconcileResponse
	if err := json.NewDecoder(respRec.Body).Decode(&recResp); err != nil {
		t.Fatalf("decode reconcile resp: %v", err)
	}
	if recResp.Receipt.OpID != "op-rec-composite:reconcile" {
		t.Fatalf("unexpected receipt op_id: %s", recResp.Receipt.OpID)
	}

	// 2. Retry of reconciliation with same op_id: finds completed stage immediately and returns 200 OK
	reqRetry, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-rec/sessions/sess-rec/turns/t-rec/reconcile", bytes.NewReader(recBody))
	reqRetry.Header.Set("Authorization", "Bearer "+cfg.AuthToken)
	reqRetry.Header.Set("Content-Type", "application/json")

	respRetry, err := client.Do(reqRetry)
	if err != nil {
		t.Fatalf("retry reconcile request: %v", err)
	}
	defer respRetry.Body.Close()
	if respRetry.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on retry reconcile, got %d", respRetry.StatusCode)
	}
	var retryResp ReconcileResponse
	if err := json.NewDecoder(respRetry.Body).Decode(&retryResp); err != nil {
		t.Fatalf("decode retry resp: %v", err)
	}
	if retryResp.Receipt.CommittedVersion != recResp.Receipt.CommittedVersion {
		t.Fatalf("expected identical committed version on replay: got %d vs %d", retryResp.Receipt.CommittedVersion, recResp.Receipt.CommittedVersion)
	}
}
