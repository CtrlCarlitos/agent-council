//go:build unix

package agy

// The `models` auth gate (AC-010 spec §3.2 gate step 2): provider-free
// of model calls and BEFORE any prompt exists. A parsed catalog carrying
// the frozen model passes; the not-signed-in text is ErrAgyAuthRequired;
// a catalog without the frozen model is ErrProfileDrift; anything else
// (network failure, timeout, non-zero exit, empty/unparseable output)
// is ErrAgyAuthInconclusive. It runs before a session's first dispatch
// and again after ResumeSession, with a bounded per-session cache.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
)

const frozenModel = "gpt-oss-120b-medium"

func TestClassifyModelsOutput(t *testing.T) {
	type want int
	const (
		pass want = iota
		authRequired
		inconclusive
		profileDrift
	)
	cases := map[string]struct {
		stdout, stderr string
		exit           int
		want           want
	}{
		"tab_separated_catalog": {"gemini-3.8-flash-low\tGemini 3.8 Flash (Low)\ngpt-oss-120b-medium\tGPT-OSS 120B (Medium)\n", "", 0, pass},
		"bare_ids":              {"claude-sonnet-4-6\ngpt-oss-120b-medium\n", "", 0, pass},
		"model_absent":          {"gemini-3.8-flash-low\tGemini\nclaude-sonnet-4-6\tSonnet\n", "", 0, profileDrift},
		"please_sign_in":        {"Please sign in to view available models.\n", "", 1, authRequired},
		"not_logged_in":         {"You are not logged into Antigravity. Run `agy` to sign in.\n", "", 0, authRequired},
		"sign_in_on_stderr":     {"", "Please sign in to view available models.\n", 1, authRequired},
		"empty":                 {"", "", 0, inconclusive},
		"network_failure":       {"", "Error: dial tcp: lookup example: network is unreachable\n", 1, inconclusive},
		"catalog_nonzero_exit":  {"gpt-oss-120b-medium\n", "", 1, inconclusive},
		"garbage_line":          {"gpt-oss-120b-medium\nError: partial catalog\n", "", 0, inconclusive},
		"prefix_not_id":         {"gpt-oss-120b-medium-preview\n", "", 0, profileDrift},
		"one_word_header":       {"Models:\n", "", 0, inconclusive},
		"one_word_error":        {"Error\n", "", 0, inconclusive},
		"one_word_status_tab":   {"Loading\tplease wait\n", "", 0, inconclusive},
		"one_other_model":       {"claude-sonnet-4-6\n", "", 0, profileDrift},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := classifyModelsOutput([]byte(tc.stdout), []byte(tc.stderr), tc.exit, frozenModel)
			var req *ErrAgyAuthRequired
			var inc *ErrAgyAuthInconclusive
			var drift *ErrProfileDrift
			switch tc.want {
			case pass:
				if err != nil {
					t.Fatalf("must pass: %v", err)
				}
			case authRequired:
				if !errors.As(err, &req) {
					t.Fatalf("want ErrAgyAuthRequired, got %T: %v", err, err)
				}
			case inconclusive:
				if !errors.As(err, &inc) {
					t.Fatalf("want ErrAgyAuthInconclusive, got %T: %v", err, err)
				}
			case profileDrift:
				if !errors.As(err, &drift) {
					t.Fatalf("want ErrProfileDrift, got %T: %v", err, err)
				}
			}
		})
	}
}

// dispatchTurn runs one accepted turn to completion.
func (h *agyHarness) dispatchTurn(turnKey string) {
	h.t.Helper()
	h.turnScenario(userInputDone, successResult("ok"))
	if out, err := h.dispatch(turnKey, "p-"+turnKey); err != nil || out.Status != adapter.DispatchAccepted {
		h.t.Fatalf("dispatch %s: %+v err=%v", turnKey, out, err)
	}
	h.waitIdle(h.ref(turnKey))
}

func (h *agyHarness) resumeBinding() adapter.SessionBinding {
	return adapter.SessionBinding{SessionID: testSessionID, Contributor: "agy", NativeSessionID: testNativeID,
		Config: adapter.SessionConfig{Model: h.model, WorkspaceRoot: h.wsRoot}}
}

func TestAuthGate_FirstDispatchThenCachedThenResume(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	clock := time.Now()
	h.adapter.now = func() time.Time { return clock }

	h.dispatchTurn("t1")
	if got := h.gateLines("models"); len(got) != 1 {
		t.Fatalf("the first dispatch of a session runs the models gate once, got %v", got)
	}
	h.dispatchTurn("t2")
	if got := h.gateLines("models"); len(got) != 1 {
		t.Fatalf("a dispatch inside the TTL reuses the cached pass, got %v", got)
	}

	clock = clock.Add(defaultAuthGateTTL + time.Second)
	h.dispatchTurn("t3")
	if got := h.gateLines("models"); len(got) != 2 {
		t.Fatalf("an expired pass re-runs the gate, got %v", got)
	}

	if err := h.adapter.ResumeSession(context.Background(), h.resumeBinding()); err != nil {
		t.Fatalf("ResumeSession: %v", err)
	}
	h.dispatchTurn("t4")
	if got := h.gateLines("models"); len(got) != 3 {
		t.Fatalf("ResumeSession after a park forces the gate before the next dispatch, got %v", got)
	}
	if n := h.exec.starts.Load(); n != 4 {
		t.Fatalf("four turn children, launches=%d", n)
	}
}

func TestAuthGate_TTLInjectable(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.adapter.authTTL = 0
	h.dispatchTurn("t1")
	h.dispatchTurn("t2")
	if got := h.gateLines("models"); len(got) != 2 {
		t.Fatalf("a zero TTL caches nothing, got %v", got)
	}
}

func requireRefusedPreTurn(t *testing.T, h *agyHarness, out adapter.DispatchOutcome) {
	t.Helper()
	if out.Status != adapter.DispatchRejected {
		t.Fatalf("an auth-gate failure is a clean pre-write rejection, got %+v", out)
	}
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("no turn process starts when the gate fails, launches=%d", n)
	}
	if input := h.fixtureFile(".agy-fixture-input"); len(input) != 0 {
		t.Fatalf("no prompt byte exists, child read %v", input)
	}
	if a, _ := h.store.GetAgyTurnAttempt(context.Background(), "att-t1"); a != nil {
		t.Fatalf("nothing durable is recorded, got %+v", a)
	}
}

func TestAuthGate_NotSignedIn(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"models_not_signed_in": true}`, userInputDone, successResult("ok"))
	out, err := h.dispatch("t1", "p")
	var req *ErrAgyAuthRequired
	if !errors.As(err, &req) {
		t.Fatalf("want ErrAgyAuthRequired, got %T: %v", err, err)
	}
	requireRefusedPreTurn(t, h, out)

	// A failure is never cached: once signed in, the next dispatch passes.
	h.dispatchTurn("t2")
	if got := h.gateLines("models"); len(got) != 2 {
		t.Fatalf("the gate re-ran after the failure, got %v", got)
	}
}

func TestAuthGate_NetworkFailureInconclusive(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"models_error": "Error: dial tcp: network is unreachable"}`)
	out, err := h.dispatch("t1", "p")
	var inc *ErrAgyAuthInconclusive
	if !errors.As(err, &inc) {
		t.Fatalf("want ErrAgyAuthInconclusive, got %T: %v", err, err)
	}
	requireRefusedPreTurn(t, h, out)
}

func TestAuthGate_TimeoutInconclusive(t *testing.T) {
	old := authGateTimeout
	authGateTimeout = 300 * time.Millisecond
	t.Cleanup(func() { authGateTimeout = old })

	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"models_hang_ms": 20000}`)
	start := time.Now()
	out, err := h.dispatch("t1", "p")
	var inc *ErrAgyAuthInconclusive
	if !errors.As(err, &inc) || !strings.Contains(inc.Error(), "bound") {
		t.Fatalf("want a bounded-wait ErrAgyAuthInconclusive, got %T: %v", err, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the models wait is bounded, took %v", time.Since(start))
	}
	requireRefusedPreTurn(t, h, out)
}

func TestAuthGate_FrozenModelAbsentIsProfileDrift(t *testing.T) {
	h := newAgyHarness(t)
	h.persist(testNativeID)
	h.turnScenario(`{"models_catalog": ["gemini-3.8-flash-low", "claude-sonnet-4-6"]}`)
	out, err := h.dispatch("t1", "p")
	var drift *ErrProfileDrift
	if !errors.As(err, &drift) {
		t.Fatalf("want ErrProfileDrift, got %T: %v", err, err)
	}
	requireRefusedPreTurn(t, h, out)
}

func TestAuthGate_CreationIsNotGated(t *testing.T) {
	h := newAgyHarness(t)
	h.scenario(`{"conversation_id": "`+testNativeID+`"}`, `{"models_not_signed_in": true}`)
	if _, err := h.adapter.CreateSession(context.Background(), h.createRequest()); err != nil {
		t.Fatalf("creation is provider-free and not auth-gated: %v", err)
	}
	if got := h.gateLines("models"); len(got) != 0 {
		t.Fatalf("creation never runs models, got %v", got)
	}
}
