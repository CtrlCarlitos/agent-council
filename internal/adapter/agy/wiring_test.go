//go:build unix

package agy

// NewProductionAgyAdapter end to end over the compiled fixture (Linux:
// production is sealed-only, spec §3.12): ValidateAgyHarness → sealed
// image → eligibility (a covering attestation for the exact tuple) →
// toolkit configured state (the construction-scoped `plugin list`
// child) → the adapter, whose every launch re-checks the toolkit and
// whose first dispatch runs the models gate.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
)

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("production construction is Linux-only (spec §3.12)")
	}
}

func (h *agyHarness) newProduction(scratch string) (*AgyAdapter, error) {
	a, err := NewProductionAgyAdapter(h.store, h.exec, h.wm, h.profile, h.evidenceRoot, filepath.Dir(h.home), scratch)
	if a != nil {
		// Production resolves the attempt from the journaled dispatch
		// intent (storageDispatchIdentitySource); these tests dispatch
		// without the service, so the harness identity stands in.
		a.identity = fnIdentity{fn: defaultAttempt}
	}
	return a, err
}

func TestNewProductionAgyAdapter_EndToEnd(t *testing.T) {
	requireLinux(t)
	h := newAgyHarness(t)
	recordAttestation(t, h.store, testRunID, h.policy, h.profile, h.profileDigest, nil)
	scratch := t.TempDir()

	prod, err := h.newProduction(scratch)
	if err != nil {
		t.Fatalf("NewProductionAgyAdapter: %v", err)
	}
	probeLog := readAgyFixtureFile(t, filepath.Join(scratch, constructionProbeDir), ".agy-fixture-gate-args")
	if len(probeLog) != 1 || probeLog[0] != "plugin\x1flist" {
		t.Fatalf("construction re-derives plugin list once in its own scratch scope, got %v", probeLog)
	}

	h.scenario(`{"conversation_id": "` + testNativeID + `"}`)
	binding, err := prod.CreateSession(context.Background(), h.createRequest())
	if err != nil {
		t.Fatalf("production CreateSession: %v", err)
	}
	h.persist(binding.NativeSessionID)
	h.turnScenario(userInputDone, successResult("ok"))
	out, err := prod.Dispatch(context.Background(), h.ref("t1"), "p")
	if err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("production dispatch: %+v err=%v", out, err)
	}
	prod.waitAllIdle(10e9)
	if got := h.gateLines("models"); len(got) != 1 {
		t.Fatalf("the first production dispatch runs the models gate, got %v", got)
	}
}

func TestNewProductionAgyAdapter_ToolkitDriftAtConstruction(t *testing.T) {
	requireLinux(t)
	t.Run("plugins", func(t *testing.T) {
		h := newAgyHarness(t)
		recordAttestation(t, h.store, testRunID, h.policy, h.profile, h.profileDigest, nil)
		scratch := t.TempDir()
		dir := filepath.Join(scratch, constructionProbeDir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".agy-fixture-scenario.jsonl"),
			[]byte(`{"plugin_list_drift": "plugin list drifted"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := h.newProduction(scratch)
		requireToolkitDrift(t, err, "plugins")
	})
	t.Run("hooks", func(t *testing.T) {
		h := newAgyHarness(t)
		recordAttestation(t, h.store, testRunID, h.policy, h.profile, h.profileDigest, nil)
		writeHooks(t, h.home, `{"guardrail": {"command": "", "event": "PreToolUse"}}`)
		_, err := h.newProduction(t.TempDir())
		requireToolkitDrift(t, err, "hooks")
		if n := h.exec.gateStarts.Load(); n != 0 {
			t.Fatalf("filesystem drift refuses before the plugin list child, launches=%d", n)
		}
	})
}

func TestNewProductionAgyAdapter_EligibilityReCheckedPerDispatch(t *testing.T) {
	requireLinux(t)
	h := newAgyHarness(t)
	recordAttestation(t, h.store, testRunID, h.policy, h.profile, h.profileDigest, nil)
	prod, err := h.newProduction(t.TempDir())
	if err != nil {
		t.Fatalf("NewProductionAgyAdapter: %v", err)
	}
	// The durable row disappears (e.g. purged): the next launch is
	// refused before any child, even though construction succeeded.
	if _, err := h.store.DB().Exec(`DELETE FROM agy_protection_attestations`); err != nil {
		t.Fatal(err)
	}
	h.persist(testNativeID)
	h.turnScenario(userInputDone, successResult("ok"))
	_, err = prod.Dispatch(context.Background(), h.ref("t1"), "p")
	var ne *ErrNotEligible
	if !errors.As(err, &ne) {
		t.Fatalf("want ErrNotEligible on re-check, got %T: %v", err, err)
	}
	if n := h.exec.starts.Load(); n != 0 {
		t.Fatalf("no turn child for an ineligible tuple, launches=%d", n)
	}
}

// The service's required-tools seam reaches the production adapter
// through WithRequiredToolsSource (Task 7): the journaled queue-time set
// is what the attempt verifies; without the option the frozen
// default_required_tools apply.
func TestNewProductionAgyAdapter_WithRequiredToolsSourceHonoured(t *testing.T) {
	requireLinux(t)
	h := newAgyHarness(t)
	recordAttestation(t, h.store, testRunID, h.policy, h.profile, h.profileDigest, nil)
	src := &mapRequired{m: map[string][]string{"t1": {"view_file", "run_command"}}}

	prod, err := NewProductionAgyAdapter(h.store, h.exec, h.wm, h.profile, h.evidenceRoot, filepath.Dir(h.home), t.TempDir(),
		WithRequiredToolsSource(src))
	if err != nil {
		t.Fatalf("NewProductionAgyAdapter: %v", err)
	}
	got, err := prod.requiredTools(context.Background(), h.ref("t1"))
	if err != nil || len(got) != 2 || got[0] != "view_file" || got[1] != "run_command" {
		t.Fatalf("the option's source must supply the attempt's required tools, got %v err=%v", got, err)
	}
	// Absent for this turn: the frozen default applies.
	got, err = prod.requiredTools(context.Background(), h.ref("t2"))
	if err != nil || len(got) != len(h.policy.DefaultRequiredTools) {
		t.Fatalf("an absent set falls back to default_required_tools %v, got %v err=%v", h.policy.DefaultRequiredTools, got, err)
	}

	plain, err := NewProductionAgyAdapter(h.store, h.exec, h.wm, h.profile, h.evidenceRoot, filepath.Dir(h.home), t.TempDir())
	if err != nil {
		t.Fatalf("NewProductionAgyAdapter without options: %v", err)
	}
	if plain.required != nil {
		t.Fatalf("without the option no required-tools source is wired, got %T", plain.required)
	}
}
