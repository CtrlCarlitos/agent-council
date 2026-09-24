//go:build unix

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func setupProposalAdminFixture(t *testing.T) (*adminFixture, string, storage.ArtifactMetadata) {
	t.Helper()
	f := newAdminFixture(t)
	ctx := context.Background()

	runID := "run-adm"
	lease := "ctrl-lease-1"

	// Adopt controller
	_, err := f.store.AdoptController(ctx, "op-adopt-adm", runID, "claude", "ctrl-ref", "boot-adm", nil, lease)
	if err != nil {
		f.close(t)
		t.Fatalf("adopt controller: %v", err)
	}
	_, err = f.store.ConnectRunController(ctx, "op-conn-adm", runID, lease, 1, "inst-adm")
	if err != nil {
		f.close(t)
		t.Fatalf("connect controller: %v", err)
	}

	// Create worker session
	_, err = f.store.CreateSession(ctx, "op-sess-w1", lease, storage.SessionRecord{
		ID: "sess-w1", RunID: runID, Contributor: "claude", Role: "worker",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		f.close(t)
		t.Fatalf("create worker session: %v", err)
	}

	// Queue and release a turn
	ver, err := f.store.GetSessionVersion(ctx, "sess-w1")
	if err != nil {
		f.close(t)
		t.Fatalf("get session version: %v", err)
	}
	_, err = f.store.QueuePrompt(ctx, "op-q-w1", lease, "sess-w1", ver, storage.PendingPrompt{
		SessionID: "sess-w1", TurnKey: "turn-w1", Prompt: "generate proposal",
	})
	if err != nil {
		f.close(t)
		t.Fatalf("queue prompt: %v", err)
	}
	ver, _ = f.store.GetSessionVersion(ctx, "sess-w1")
	rel, err := f.store.ReleaseTurn(ctx, "op-rel-w1", lease, "sess-w1", ver, "turn-w1")
	if err != nil {
		f.close(t)
		t.Fatalf("release turn: %v", err)
	}

	execRef := storage.ExecutionRef{
		SessionID: "sess-w1",
		TurnKey:   "turn-w1",
		AttemptID: rel.Receipt.AttemptID,
	}

	meta, err := f.store.RecordObservedArtifact(ctx, execRef, "worker-proposal.md", []byte("# Worker Proposal Content"))
	if err != nil {
		f.close(t)
		t.Fatalf("record observed artifact: %v", err)
	}

	return f, lease, meta
}

func TestAdminReleaseArtifacts_Auth(t *testing.T) {
	f, lease, meta := setupProposalAdminFixture(t)
	defer f.close(t)

	path := "/v1/runs/run-adm/artifacts/release"
	validBody := fmt.Sprintf(`{"op_id":"op-rel-auth-1","controller_lease":%q,"members":[{"artifact_id":%q,"revision":%d,"digest":%q}]}`,
		lease, meta.ID, meta.Revision, meta.Digest)

	// 1. Missing Authorization header -> 401 Unauthorized
	client := newTestClient(f.srv.SocketPath())
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "http://localhost"+path, strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on missing auth header, got %d", resp.StatusCode)
	}

	// 2. Controller lease passed as bearer token -> 401 Unauthorized
	code, _ := f.post(t, path, lease, validBody)
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when controller lease is used as bearer token, got %d", code)
	}

	// 3. Operator token used, but missing controller_lease in body -> 400 Bad Request
	noLeaseBody := fmt.Sprintf(`{"op_id":"op-rel-auth-2","members":[{"artifact_id":%q,"revision":%d,"digest":%q}]}`,
		meta.ID, meta.Revision, meta.Digest)
	code, body := f.post(t, path, f.token, noLeaseBody)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 when controller_lease is omitted in body, got %d (body: %s)", code, body)
	}

	// 4. Operator token used, but invalid controller_lease in body -> 403 Forbidden
	wrongLeaseBody := fmt.Sprintf(`{"op_id":"op-rel-auth-3","controller_lease":"wrong-lease","members":[{"artifact_id":%q,"revision":%d,"digest":%q}]}`,
		meta.ID, meta.Revision, meta.Digest)
	code, body = f.post(t, path, f.token, wrongLeaseBody)
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 when wrong controller_lease in body, got %d (body: %s)", code, body)
	}
}

func TestAdminReleaseArtifacts_SuccessAndReplay(t *testing.T) {
	f, lease, meta := setupProposalAdminFixture(t)
	defer f.close(t)

	path := "/v1/runs/run-adm/artifacts/release"
	reqBody := fmt.Sprintf(`{"op_id":"op-http-seal-1","controller_lease":%q,"members":[{"artifact_id":%q,"revision":%d,"digest":%q}]}`,
		lease, meta.ID, meta.Revision, meta.Digest)

	// 1. First invocation seals proposal set
	code, body := f.post(t, path, f.token, reqBody)
	if code != http.StatusOK {
		t.Fatalf("expected 200 OK on release artifacts, got %d (body: %s)", code, body)
	}

	var resp ReleaseArtifactsResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal ReleaseArtifactsResponse: %v, raw: %s", err, body)
	}

	if !strings.HasPrefix(resp.Receipt.ProposalSetDigest, "propset-v1:sha256:") {
		t.Fatalf("expected propset-v1 prefix, got %q", resp.Receipt.ProposalSetDigest)
	}
	if resp.Receipt.RunID != "run-adm" {
		t.Fatalf("expected runID run-adm, got %q", resp.Receipt.RunID)
	}
	if resp.Receipt.SealedAt.IsZero() {
		t.Fatal("expected non-zero SealedAt")
	}

	// 2. Replay with identical op_id returns identical receipt
	code2, body2 := f.post(t, path, f.token, reqBody)
	if code2 != http.StatusOK {
		t.Fatalf("expected 200 OK on replay, got %d (body: %s)", code2, body2)
	}
	var resp2 ReleaseArtifactsResponse
	if err := json.Unmarshal([]byte(body2), &resp2); err != nil {
		t.Fatalf("unmarshal replayed response: %v", err)
	}
	if resp2.Receipt.ProposalSetDigest != resp.Receipt.ProposalSetDigest {
		t.Fatalf("expected matching digest on replay, got %q vs %q", resp.Receipt.ProposalSetDigest, resp2.Receipt.ProposalSetDigest)
	}
}

func TestAdminReleaseArtifacts_ValidationErrors(t *testing.T) {
	f, lease, meta := setupProposalAdminFixture(t)
	defer f.close(t)

	path := "/v1/runs/run-adm/artifacts/release"

	// 1. Empty members list -> 400 Bad Request
	emptyMembersBody := fmt.Sprintf(`{"op_id":"op-err-empty","controller_lease":%q,"members":[]}`, lease)
	code, _ := f.post(t, path, f.token, emptyMembersBody)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 on empty members, got %d", code)
	}

	// 2. Non-existent artifact revision -> 404 Not Found
	missingArtBody := fmt.Sprintf(`{"op_id":"op-err-missing","controller_lease":%q,"members":[{"artifact_id":"ghost.md","revision":1,"digest":%q}]}`,
		lease, meta.Digest)
	code, _ = f.post(t, path, f.token, missingArtBody)
	if code != http.StatusNotFound {
		t.Fatalf("expected 404 on missing artifact, got %d", code)
	}

	// 3. Digest mismatch -> 400 Bad Request
	mismatchBody := fmt.Sprintf(`{"op_id":"op-err-mismatch","controller_lease":%q,"members":[{"artifact_id":%q,"revision":%d,"digest":"bad-digest"}]}`,
		lease, meta.ID, meta.Revision)
	code, _ = f.post(t, path, f.token, mismatchBody)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 on digest mismatch, got %d", code)
	}
}
