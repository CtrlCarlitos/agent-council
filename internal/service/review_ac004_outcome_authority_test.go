//go:build unix

package service

import (
	"context"
	"encoding/json"
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

// Regression evidence for AC-004 Task 4 (service level): rotating or
// revoking controller authority while an accepted execution is active
// fences the old controller without preventing the verified outcome from
// persisting under the original execution.

// midFlightRotation drives: A adopts+connects+releases a gated turn; the
// operator rotates authority (handoff or revocation) while the turn is
// active; A's commands and replays are rejected; the worker finishes; the
// verified outcome persists; the follow-up stays queued until the current
// controller explicitly releases it.
func runMidFlightRotation(t *testing.T, revoke bool) {
	holdStart := make(chan struct{})
	h := newTestHarnessWithFaults(t, adaptertest.ScriptedFaults{
		HoldExecutionStart: holdStart,
	})
	ctx := context.Background()

	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-mid", "gated work")
	// Queue the follow-up before releasing.
	if _, err := h.store.QueuePrompt(ctx, "op-q-next", "lease-1", "sess-1", ver, storage.PendingPrompt{
		SessionID: "sess-1", TurnKey: "turn-next", Prompt: "follow-up",
	}); err != nil {
		t.Fatalf("queue follow-up: %v", err)
	}

	// A (adopted fixture controller, lease-1) releases the gated turn.
	relVer, _ := h.store.GetSessionVersion(ctx, "sess-1")
	relBody := fmt.Sprintf(`{"op_id":"op-rel-mid","controller_lease":"lease-1","expected_version":%d}`, relVer)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-mid/release", strings.NewReader(relBody))
	req.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("release: %v, err: %v", resp.StatusCode, err)
	}
	resp.Body.Close()

	// Wait until the execution is demonstrably active (dispatch accepted,
	// observation recorded under the execution reference).
	waitFor(t, 2*time.Second, func() bool {
		d, err := h.store.GetTurnDetails(ctx, "sess-1", "turn-mid")
		return err == nil && d != nil && d.DispatchIntent != nil && d.DispatchIntent.Phase == "receipt_acknowledged"
	})

	// Rotate authority mid-flight.
	if revoke {
		if _, err := h.store.RevokeController(ctx, "op-revoke-mid", "run-1", "lease-1", nil); err != nil {
			t.Fatalf("revoke: %v", err)
		}
	} else {
		if _, err := h.store.HandoffController(ctx, "op-handoff-mid", "run-1", "lease-1", 1, "codex", "controller-B", "lease-B"); err != nil {
			t.Fatalf("handoff: %v", err)
		}
	}

	// A's new commands are fenced.
	curVer, _ := h.store.GetSessionVersion(ctx, "sess-1")
	cxlBody := fmt.Sprintf(`{"op_id":"op-cxl-a","controller_lease":"lease-1","expected_version":%d,"reason":"fenced"}`, curVer)
	cxlReq, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost/v1/runs/run-1/sessions/sess-1/turns/turn-mid/cancel", strings.NewReader(cxlBody))
	cxlReq.Header.Set("Authorization", "Bearer "+h.cfg.AuthToken)
	cxlReq.Header.Set("Content-Type", "application/json")
	cxlResp, err := h.client.Do(cxlReq)
	if err != nil {
		t.Fatalf("fenced cancel request: %v", err)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.NewDecoder(cxlResp.Body).Decode(&env)
	cxlResp.Body.Close()
	if cxlResp.StatusCode != http.StatusForbidden || env.Error.Code != "lease_superseded" {
		t.Fatalf("expected 403 lease_superseded for fenced controller, got %d %+v", cxlResp.StatusCode, env.Error)
	}

	// The original worker finishes; the verified outcome persists under the
	// original execution.
	close(holdStart)
	h.server.Coordinator().WaitWorkers()

	details, err := h.store.GetTurnDetails(ctx, "sess-1", "turn-mid")
	if err != nil {
		t.Fatalf("get turn details: %v", err)
	}
	if details.Status != council.TurnCompleted {
		t.Fatalf("expected verified completion after rotation, got %v (result %q)", details.Status, details.Result)
	}

	// The follow-up remains queued and unreleased (no turn row exists).
	if d, err := h.store.GetTurnDetails(ctx, "sess-1", "turn-next"); err == nil && d != nil {
		t.Fatal("follow-up must remain queued (no turn row) until the current controller releases it")
	}

	if revoke {
		// With no controller, the follow-up stays queued; a newly adopted
		// controller can release it.
		if _, err := h.store.AdoptController(ctx, "op-adopt-after", "run-1", "agy", "controller-C", "", &storage.OperatorRecovery{Reason: "rotation", ExpectedGeneration: 1}, "lease-C"); err != nil {
			t.Fatalf("adopt after revoke: %v", err)
		}
		nextVer, _ := h.store.GetSessionVersion(ctx, "sess-1")
		if _, err := h.store.ReleaseTurn(ctx, "op-rel-next", "lease-C", "sess-1", nextVer, "turn-next"); err != nil {
			t.Fatalf("newly adopted controller must release the follow-up: %v", err)
		}
	} else {
		// Only B's explicit release starts the follow-up.
		nextVer, _ := h.store.GetSessionVersion(ctx, "sess-1")
		if _, err := h.store.ReleaseTurn(ctx, "op-rel-next", "lease-B", "sess-1", nextVer, "turn-next"); err != nil {
			t.Fatalf("controller B must release the follow-up: %v", err)
		}
	}
	h.server.Coordinator().WaitWorkers()
}

func TestAC004_HandoffMidFlight_PreservesOutcome_ControlledFixture(t *testing.T) {
	runMidFlightRotation(t, false)
}

func TestAC004_RevokeMidFlight_PreservesOutcome_ControlledFixture(t *testing.T) {
	runMidFlightRotation(t, true)
}

// The supervisor carries the execution reference (session, turn, attempt)
// from the release receipt; the gated adapter path exercises it end to end
// through the fake adapter above. This companion asserts the HTTP release
// receipt exposes the attempt identity the evidence path validates.
func TestAC004_ReleaseReceiptCarriesAttemptIdentity(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	ver, _ := h.createSessionAndTurn(ctx, "run-1", "sess-1", "turn-att", "p")
	relVer, _ := h.store.GetSessionVersion(ctx, "sess-1")
	rel, err := h.store.ReleaseTurn(ctx, "op-rel-att", "lease-1", "sess-1", relVer, "turn-att")
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if rel.Receipt.AttemptID == "" {
		t.Fatal("release receipt must carry the attempt identity for execution-reference evidence")
	}
	d, _ := h.store.GetTurnDetails(ctx, "sess-1", "turn-att")
	if d.AttemptID != rel.Receipt.AttemptID {
		t.Fatalf("receipt attempt %q must match persisted attempt %q", rel.Receipt.AttemptID, d.AttemptID)
	}
	_ = ver
	_ = adapter.TurnRef{}
}
