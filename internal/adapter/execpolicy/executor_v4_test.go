//go:build !windows

package execpolicy_test

// cprof-v4 compatibility (AC-010 §3.7 matrix): the executor validity
// gate widens v1|v2|v3 → v1|v2|v3|v4 — a v4 profile passes the
// algo_version gate unchanged, with no toolkit_manifest, codex-block,
// or agy-block behavior.

import (
	"context"
	"errors"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
)

func TestPolicyExecutor_AcceptsCprofV4AtValidityGate(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	runID := "run-v4"
	sessionID := "session-v4"
	paths := setupWorkspacePaths(t, runID, sessionID)

	profile := sampleProfile([]string{"echo"}, nil, "permissive_dev")
	profile.AlgoVersion = "cprof-v4"

	req := execpolicy.LaunchRequest{
		RunID:     runID,
		SessionID: sessionID,
		TurnKey:   "turn-1",
		AttemptID: "att-1",
		Command:   "echo",
		Args:      []string{"ok"},
		Paths:     paths,
		Profile:   profile,
	}

	proc, err := executor.Start(ctx, req)
	if err != nil {
		t.Fatalf("cprof-v4 must pass the validity gate, got: %v", err)
	}
	if proc == nil {
		t.Fatal("expected a managed process")
	}
	if _, err := proc.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}

	// The gate still rejects unknown versions after the widening.
	profile.AlgoVersion = "cprof-v999"
	req.Profile = profile
	if _, err := executor.Start(ctx, req); err == nil || !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) {
		t.Fatalf("unknown algo_version must still be rejected, got: %v", err)
	}
}
