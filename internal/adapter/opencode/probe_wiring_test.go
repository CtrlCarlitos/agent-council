package opencode

// Portable evidence for the exported operator template constructor: a
// template constructed without an operator-approved canonical profile
// fails closed before any process is started.

import (
	"context"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestOperatorProbeTemplate_ZeroProfileFailsClosed(t *testing.T) {
	scratchRoot := t.TempDir()

	tpl := NewOperatorProbeLaunchTemplate("opencode", scratchRoot, storage.CanonicalProfile{})
	adp := NewOpenCodeAdapterWithLaunch(execpolicy.New(), tpl, nil, nil)

	// The executor would launch anything with a valid profile; the
	// template must reject both launch kinds before that can happen.
	_, err := tpl.VersionLaunch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "canonical profile") {
		t.Fatalf("zero-profile version launch must fail closed, got %v", err)
	}
	_, err = tpl.ProbeServeLaunch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "canonical profile") {
		t.Fatalf("zero-profile serve launch must fail closed, got %v", err)
	}

	if _, err := adp.Probe(context.Background()); err == nil {
		t.Fatal("probe with a zero-profile operator template must fail")
	}
}
