package codex_test

// Production guard for the codex test-only construction mode (AC-009
// spec §3.3): the production constructor fails closed with a typed error
// when handed the fixture option, the production registry fails closed
// for fixture-mode selection, and the production construction path
// always requires the attestation check — the exact contrast with the
// fixture scope, where eligibility is skipped by explicit construction.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
)

const guardSessionID = "b2c3d4e5-f6a7-4b8c-9d0e-1f2a3b4c5d6f"

func guardAttestation() (string, bool) {
	return "cprot-v2:sha256:" + strings.Repeat("ab", 32), true
}

// Handing the test-only option to the production constructor fails
// closed with the typed ErrFixtureModeProhibited: production wiring has
// no path to fixture mode, and no adapter is constructed.
func TestProductionGuard_ConstructorRejectsFixtureOption(t *testing.T) {
	ad, err := codex.NewCodexAdapter(nil, nil, codex.CodexLaunchPolicy{}, "", nil, nil,
		codex.FixtureOption(codex.FixtureMode{}))
	var prohibited *codex.ErrFixtureModeProhibited
	if ad != nil || !errors.As(err, &prohibited) {
		t.Fatalf("production construction handed the test-only option must fail closed typed, got adapter=%v err=%T: %v", ad, err, err)
	}
}

// The rejection is independent of eligibility state: even a working
// attestation lookup cannot buy the fixture scope through the production
// constructor.
func TestProductionGuard_ConstructorRejectsFixtureOptionEvenWhenEligible(t *testing.T) {
	ad, err := codex.NewCodexAdapter(nil, nil, codex.CodexLaunchPolicy{}, "", nil, guardAttestation,
		codex.FixtureOption(codex.FixtureMode{}))
	var prohibited *codex.ErrFixtureModeProhibited
	if ad != nil || !errors.As(err, &prohibited) {
		t.Fatalf("eligibility must not trade for fixture scope, got adapter=%v err=%T: %v", ad, err, err)
	}
}

// Production construction without options still works — the service
// wiring passes none.
func TestProductionGuard_ConstructorWithoutOptionsConstructs(t *testing.T) {
	ad, err := codex.NewCodexAdapter(nil, nil, codex.CodexLaunchPolicy{}, "", nil, guardAttestation)
	if err != nil || ad == nil {
		t.Fatalf("production construction without options must succeed, got adapter=%v err=%v", ad, err)
	}
}

// The production registry fails closed for fixture-mode selection,
// mirroring the AC-006 fake guard.
func TestProductionGuard_RegistryRejectsFixtureMode(t *testing.T) {
	reg := adapter.NewRegistry()
	for _, name := range []string{"codex-fixture", "fixture", "fixture-codex", "CODEX-FIXTURE"} {
		if _, err := reg.ResolveByName(name); !errors.Is(err, adapter.ErrFixtureAdapterProhibited) {
			t.Fatalf("ResolveByName(%q) must fail with ErrFixtureAdapterProhibited, got %v", name, err)
		}
	}
	// The AC-006 fake guard still holds, and unknown names fail closed.
	if _, err := reg.ResolveByName("fake"); !errors.Is(err, adapter.ErrFakeAdapterProhibited) {
		t.Fatalf("fake guard must hold, got %v", err)
	}
	if _, err := reg.ResolveByName("codex"); !errors.Is(err, adapter.ErrUnknownAdapter) {
		t.Fatalf("unregistered contributor must fail with ErrUnknownAdapter, got %v", err)
	}
	if _, err := reg.ResolveByName("unknown"); !errors.Is(err, adapter.ErrUnknownAdapter) {
		t.Fatalf("unknown name must fail with ErrUnknownAdapter, got %v", err)
	}
}

// The production construction path ALWAYS requires the eligibility
// check: with a nil attestation lookup, CreateSession fails with the
// typed ErrProductionEligibilityMissing before anything can start. This
// is the exact contrast with the fixture scope, where the same nil
// lookup succeeds because the marker skipped the gate explicitly.
func TestProductionGuard_NilAttestationFailsClosedBeforeAnyChild(t *testing.T) {
	ad, err := codex.NewCodexAdapter(nil, nil, codex.CodexLaunchPolicy{}, "", nil, nil)
	if err != nil || ad == nil {
		t.Fatalf("production construction must succeed, got adapter=%v err=%v", ad, err)
	}
	_, err = ad.CreateSession(context.Background(), adapter.CreateSessionRequest{
		SessionID:   guardSessionID,
		Contributor: "codex",
		Config:      adapter.SessionConfig{WorkspaceRoot: "/ws", Model: "gpt-5.6-sol"},
	})
	var missing *codex.ErrProductionEligibilityMissing
	if !errors.As(err, &missing) {
		t.Fatalf("nil lookup must fail closed with ErrProductionEligibilityMissing, got %T: %v", err, err)
	}
}
