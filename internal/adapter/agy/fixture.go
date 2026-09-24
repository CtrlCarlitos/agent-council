package agy

// Test-only construction mode (AC-010, mirroring the codex package's
// AC-009 §3.3 discipline verbatim: "explicit, never inferred"). The
// fixture scope is a construction-time marker, never a runtime inference
// from absent authentication or observations. The only package allowed
// to produce the fixture option outside this package is
// internal/adapter/agy/agytest — the test-only construction harness for
// the compiled fixture `agy` executable — never production wiring.
//
// Task 5 adds the production constructor (NewAgyAdapter); it must route
// its option decoding through applyConstructionOptions so the fixture
// option fails closed there exactly as it does in codex's NewCodexAdapter.

// ConstructionOption adjusts non-production construction. Option values
// can only be produced inside this package (constructionSettings is
// unexported), so an option handed to the production constructor is
// always the explicit fixture token FixtureOption produces.
type ConstructionOption func(*constructionSettings)

type constructionSettings struct {
	fixtureScope bool
}

// FixtureMode is the explicit test-only construction marker. It carries
// no behavior: naming it at a construction site is the auditable
// declaration that the resulting adapter runs under the test-only
// fixture scope. Production wiring never constructs it.
type FixtureMode struct{}

// ErrFixtureModeProhibited reports production construction refused
// because it was handed the test-only fixture option.
type ErrFixtureModeProhibited struct {
	Reason string
}

func (e *ErrFixtureModeProhibited) Error() string {
	return "fixture construction mode is prohibited in the production constructor: " + e.Reason
}

// FixtureOption produces THE test-only construction option. The
// production constructor rejects it with a typed
// ErrFixtureModeProhibited via applyConstructionOptions.
func FixtureOption(_ FixtureMode) ConstructionOption {
	return func(s *constructionSettings) { s.fixtureScope = true }
}

// applyConstructionOptions decodes opts for the PRODUCTION construction
// path: Task 5's NewAgyAdapter calls this (it is unexported — package-
// private on purpose, exactly like codex's inline equivalent — so only
// code inside this package, i.e. the future production constructor, can
// reach it). It fails closed with ErrFixtureModeProhibited whenever the
// fixture option is present; a fixture-scoped constructor (Task 5, or
// agytest indirectly through it) must take the FixtureMode marker
// directly instead of going through this function.
func applyConstructionOptions(opts ...ConstructionOption) (constructionSettings, error) {
	var s constructionSettings
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		opt(&s)
	}
	if s.fixtureScope {
		return constructionSettings{}, &ErrFixtureModeProhibited{
			Reason: "the production agy constructor must not receive the fixture-scope option; build fixture-scoped adapters through agytest instead",
		}
	}
	return s, nil
}
