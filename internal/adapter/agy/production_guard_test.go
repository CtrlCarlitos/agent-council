package agy

// Production guard for the agy test-only construction mode (AC-010,
// mirroring internal/adapter/codex/production_guard_test.go): the
// production option-decoding seam fails closed with a typed error when
// handed the fixture option, independent of any other state. Task 5's
// NewAgyAdapter must route through applyConstructionOptions for this
// guard to protect the real production constructor; until then this
// test exercises the seam directly (package-internal: applyConstruction-
// Options is unexported on purpose).

import (
	"errors"
	"testing"
)

func TestProductionGuard_ApplyConstructionOptionsRejectsFixtureOption(t *testing.T) {
	settings, err := applyConstructionOptions(FixtureOption(FixtureMode{}))
	var prohibited *ErrFixtureModeProhibited
	if !errors.As(err, &prohibited) {
		t.Fatalf("expected *ErrFixtureModeProhibited, got %T: %v", err, err)
	}
	if settings.fixtureScope {
		t.Fatalf("rejected settings must not carry fixtureScope=true, got %+v", settings)
	}
}

func TestProductionGuard_ApplyConstructionOptionsWithoutOptionsSucceeds(t *testing.T) {
	settings, err := applyConstructionOptions()
	if err != nil {
		t.Fatalf("no options must succeed, got: %v", err)
	}
	if settings.fixtureScope {
		t.Fatalf("no options must not set fixtureScope, got %+v", settings)
	}
}

func TestProductionGuard_ApplyConstructionOptionsIgnoresNilOption(t *testing.T) {
	settings, err := applyConstructionOptions(nil)
	if err != nil {
		t.Fatalf("nil option must not error, got: %v", err)
	}
	if settings.fixtureScope {
		t.Fatalf("nil option must not set fixtureScope, got %+v", settings)
	}
}
