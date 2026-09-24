package codex

// Test-only construction mode (AC-009 spec §3.3, "explicit, never
// inferred"): the fixture scope is a construction-time marker, never a
// runtime inference from absent authentication or observations. The ONLY
// eligibility-skipping surface is NewFixtureScopedAdapter, which requires
// the explicit FixtureMode marker; it exists for the test-only codextest
// package and the operator-invoked manual evidence executable — never for
// production wiring. The production constructor (NewCodexAdapter) has no
// path to this mode: handed the fixture option it fails closed with a
// typed ErrFixtureModeProhibited, and the build guard asserts production
// packages do not import codextest at all.

import (
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ConstructionOption adjusts non-production construction. Option values
// can only be produced inside this package (the settings type is
// unexported), so the option handed to the production constructor is
// always the explicit fixture token produced by FixtureOption.
type ConstructionOption func(*constructionSettings)

type constructionSettings struct {
	fixtureScope bool
}

// FixtureMode is the explicit test-only construction marker (spec §3.3).
// It carries no behavior: naming it at a construction site is the
// auditable declaration that the resulting adapter runs under the
// test-only fixture scope. Production wiring never constructs it.
type FixtureMode struct{}

// ErrFixtureModeProhibited reports production construction refused
// because it was handed the test-only fixture option (spec §3.3).
type ErrFixtureModeProhibited struct {
	Reason string
}

func (e *ErrFixtureModeProhibited) Error() string {
	return "fixture construction mode is prohibited in the production constructor: " + e.Reason
}

// FixtureOption produces THE test-only construction option. The
// production constructor rejects it with a typed
// ErrFixtureModeProhibited; the fixture-scoped constructor requires the
// marker directly instead.
func FixtureOption(_ FixtureMode) ConstructionOption {
	return func(s *constructionSettings) { s.fixtureScope = true }
}

// NewFixtureScopedAdapter builds a CodexAdapter with production
// eligibility SKIPPED and every protocol validation retained (pump
// ordering, id discipline, drift detection, cancel semantics, route
// confirmation): the attestation lookup is left nil and
// checkProductionEligibility short-circuits on the explicit marker —
// nothing else changes. The marker argument is mandatory: there is no
// inference of fixture status. This constructor is for the test-only
// codextest package and the operator-invoked manual evidence executable;
// it is never registered as a production adapter and production wiring
// has no path to it.
func NewFixtureScopedAdapter(
	store *storage.Store,
	server *CodexServer,
	policy CodexLaunchPolicy,
	profileDigest string,
	identity DispatchIdentitySource,
	_ FixtureMode,
) *CodexAdapter {
	return buildCodexAdapter(store, server, policy, profileDigest, identity, nil, true)
}
