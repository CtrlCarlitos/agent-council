//go:build unix

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/adaptertest"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// AC-007/AC-004 primary acceptance story through the intended interfaces
// (controlled fixtures). The four harness identifiers rotate across
// subtests. Native-conversation integration is explicitly unverified here.
func TestAC005_PrimaryStoryThroughIntendedInterfaces_ControlledFixture(t *testing.T) {
	harnesses := []string{"opencode", "claude", "codex", "agy"}
	for i, hA := range harnesses {
		hB := harnesses[(i+1)%len(harnesses)]
		t.Run(hA+"->"+hB, func(t *testing.T) {
			runStory(t, hA, hB)
		})
	}
}

func runStory(t *testing.T, harnessA, harnessB string) {
	holdStart := make(chan struct{})
	h := newTestHarnessWithFaults(t, adaptertest.ScriptedFaults{
		HoldExecutionStart: holdStart,
	})
	ctx := context.Background()

	// 1. Create run with a bootstrap lease as provenance (not authority).
	if _, err := h.store.CreateRun(ctx, "op-run-story", "run-story", "b", "s", "p", "boot-lease"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := h.store.CreateSession(ctx, "op-sess-story", "boot-lease", storage.SessionRecord{
		ID: "sess-story", RunID: "run-story", Contributor: "claude", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// 2. Adopt Controller A via HTTP. This supersedes the bootstrap lease
	//    and installs the active grant.
	adoptResp := postJSON(t, h, "/v1/runs/run-story/controller/adopt",
		fmt.Sprintf(`{"op_id":"op-adopt-1","harness":%q,"controller_ref":"conv-A","bootstrap_lease":"boot-lease"}`, harnessA))
	if adoptResp.StatusCode != http.StatusOK {
		t.Fatalf("adopt: %d %s", adoptResp.StatusCode, readBodyText(adoptResp.Body))
	}
	var adoptGrant struct {
		LeaseSecret string `json:"lease_secret"`
	}
	_ = json.NewDecoder(adoptResp.Body).Decode(&adoptGrant)
	if adoptGrant.LeaseSecret == "" || adoptGrant.LeaseSecret == "boot-lease" {
		t.Fatalf("adoption must issue a fresh secret, got %q", adoptGrant.LeaseSecret)
	}
	// Queue version is fetched after connect (which advances row_version).

	// 3. Connect before queueing: adoption grants authority, not connection.
	connReceipt, connErr := h.store.ConnectRunController(ctx, "op-conn-A", "run-story", adoptGrant.LeaseSecret, 1, "test-instance")
	if connErr != nil {
		t.Fatalf("A connect: %v", connErr)
	}
	h.server.Coordinator().MarkControllerAttached("run-story", 1, connReceipt.AttachmentID, "test-instance", 0)

	// 4. Queue prompts under the adopted controller.
	queueVer, _ := h.store.GetSessionVersion(ctx, "sess-story")
	if _, err := h.store.QueuePrompt(ctx, "op-q-work", adoptGrant.LeaseSecret, "sess-story", queueVer, storage.PendingPrompt{SessionID: "sess-story", TurnKey: "t-work", Prompt: "primary work"}); err != nil {
		t.Fatalf("queue work: %v", err)
	}
	ver, _ := h.store.GetSessionVersion(ctx, "sess-story")
	if _, err := h.store.QueuePrompt(ctx, "op-q-next", adoptGrant.LeaseSecret, "sess-story", ver, storage.PendingPrompt{SessionID: "sess-story", TurnKey: "t-next", Prompt: "follow-up work"}); err != nil {
		t.Fatalf("queue follow-up: %v", err)
	}

	// 5. A releases the gated turn.
	ver, _ = h.store.GetSessionVersion(ctx, "sess-story")
	relResp := postJSON(t, h, "/v1/runs/run-story/sessions/sess-story/turns/t-work/release",
		fmt.Sprintf(`{"op_id":"op-rel-work","controller_lease":%q,"expected_version":%d}`, adoptGrant.LeaseSecret, ver))
	if relResp.StatusCode != http.StatusAccepted {
		t.Fatalf("release: %d %s", relResp.StatusCode, readBodyText(relResp.Body))
	}

	// 6. A disconnects explicitly.
	discResp := postJSON(t, h, "/v1/runs/run-story/controller/disconnect",
		fmt.Sprintf(`{"op_id":"op-disc-A","controller_lease":%q,"expected_generation":1,"attachment_id":"ep-A"}`, adoptGrant.LeaseSecret))
	_ = discResp

	// 7. Work completes under service ownership; follow-up stays queued.
	close(holdStart)
	h.server.Coordinator().WaitWorkers()

	workDetails, err := h.store.GetTurnDetails(ctx, "sess-story", "t-work")
	if err != nil {
		t.Fatalf("get work turn: %v", err)
	}
	if workDetails.Status != council.TurnRunning && workDetails.Status != council.TurnCompleted {
		t.Fatalf("work turn must be dispatched, got %v", workDetails.Status)
	}

	// 8. Handoff to Controller B via HTTP.
	ver, _ = h.store.GetSessionVersion(ctx, "sess-story")
	handoffResp := postJSON(t, h, "/v1/runs/run-story/controller/handoff",
		fmt.Sprintf(`{"op_id":"op-handoff-1","current_lease":%q,"expected_generation":1,"harness":%q,"controller_ref":"conv-B"}`, adoptGrant.LeaseSecret, harnessB))
	if handoffResp.StatusCode != http.StatusOK {
		t.Fatalf("handoff: %d %s", handoffResp.StatusCode, readBodyText(handoffResp.Body))
	}
	var handoffGrant struct {
		LeaseSecret string `json:"lease_secret"`
	}
	_ = json.NewDecoder(handoffResp.Body).Decode(&handoffGrant)

	// 9. A's replay is fenced with lease_superseded.
	oldRel := postJSON(t, h, "/v1/runs/run-story/sessions/sess-story/turns/t-work/release",
		fmt.Sprintf(`{"op_id":"op-rel-work","controller_lease":%q,"expected_version":%d}`, adoptGrant.LeaseSecret, ver))
	if oldRel.StatusCode != http.StatusForbidden {
		t.Fatalf("A's replay must be fenced with 403, got %d", oldRel.StatusCode)
	}

	// 10. B connects explicitly.
	bConn := postJSON(t, h, "/v1/runs/run-story/controller/connect",
		fmt.Sprintf(`{"op_id":"op-conn-B","controller_lease":%q,"expected_generation":2}`, handoffGrant.LeaseSecret))
	if bConn.StatusCode != http.StatusOK {
		t.Fatalf("B connect: %d %s", bConn.StatusCode, readBodyText(bConn.Body))
	}

	// 11. B reads the committed outcome.
	details, err := h.store.GetTurnDetails(ctx, "sess-story", "t-work")
	if err != nil || details.Status != council.TurnCompleted {
		t.Fatalf("B must see the committed outcome: %+v err=%v", details, err)
	}

	// 12. Only B releases the follow-up.
	ver, _ = h.store.GetSessionVersion(ctx, "sess-story")
	nextRel := postJSON(t, h, "/v1/runs/run-story/sessions/sess-story/turns/t-next/release",
		fmt.Sprintf(`{"op_id":"op-rel-next","controller_lease":%q,"expected_version":%d}`, handoffGrant.LeaseSecret, ver))
	if nextRel.StatusCode != http.StatusAccepted {
		t.Fatalf("B must release the follow-up: %d %s", nextRel.StatusCode, readBodyText(nextRel.Body))
	}
	h.server.Coordinator().WaitWorkers()
}

func postJSON(t *testing.T, h *testHarness, path, body string) *http.Response {
	t.Helper()
	ctx := context.Background()
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost"+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return resp
}

func readBodyText(body interface{ Read([]byte) (int, error) }) string {
	buf := make([]byte, 4096)
	n, _ := body.Read(buf)
	return string(buf[:n])
}
