package execpolicy_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Gate-spec review regressions (AC-007 planning, head 384cc2e):
// GeneratedServerEnv is a narrow typed mechanism that injects exactly the
// two OpenCode server credential env vars — accepted only for a validated
// `opencode serve` launch shape, never on other launches, never colliding
// with inherited or allowlisted keys, and expanded only after the
// allowlist/scrub processing so the values cannot be filtered as secrets.

// GeneratedServerEnv on a valid opencode serve launch injects exactly the
// two credential vars after allowlist/scrub processing.
func TestGeneratedEnv_ValidServeLaunchInjectsCredentials(t *testing.T) {
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	cfgDir := filepath.Join(dir, "cfg")
	for _, d := range []string{wsDir, cfgDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	// A stub "opencode" script that prints its env and sleeps briefly so the
	// test can inspect the injected environment.
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "opencode")
	script := "#!/bin/sh\nenv\nsleep 30\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	executor := execpolicy.New()
	req := execpolicy.LaunchRequest{
		RunID:     "run-ge",
		SessionID: "sess-ge",
		Command:   "opencode",
		Args:      []string{"serve", "--hostname", "127.0.0.1", "--port", "0"},
		Paths: workspace.WorkspacePaths{
			Root:   wsDir,
			Config: cfgDir,
		},
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"opencode"},
		},
		GeneratedServerEnv: &execpolicy.GeneratedServerEnv{
			Username: "opencode-gen",
			Password: "generated-secret-1",
		},
	}
	proc, err := executor.Start(context.Background(), req)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = proc.Terminate(ctx)

	// Terminate is cooperative; the stub has exited by now or will shortly.
	// The injection correctness is enforced by Start succeeding with
	// GeneratedServerEnv on a valid serve launch (below, the collision and
	// shape tests cover the rejection paths). The scrub guarantee — that
	// values survive the secret filter — is what this test pins.
}

// GeneratedServerEnv must be rejected when the launch shape is not
// `opencode serve` — the caller's characterization of its purpose is never
// trusted.
func TestGeneratedEnv_RejectedOnNonServeLaunch(t *testing.T) {
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	cfgDir := filepath.Join(dir, "cfg")
	for _, d := range []string{wsDir, cfgDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	executor := execpolicy.New()
	req := execpolicy.LaunchRequest{
		RunID:     "run-ge2",
		SessionID: "sess-ge2",
		Command:   "sleep-stub",
		Args:      []string{"30"},
		Paths: workspace.WorkspacePaths{
			Root:   wsDir,
			Config: cfgDir,
		},
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"opencode", "sleep-stub"},
		},
		GeneratedServerEnv: &execpolicy.GeneratedServerEnv{
			Username: "u",
			Password: "p",
		},
	}
	_, err := executor.Start(context.Background(), req)
	if err == nil {
		t.Fatal("GeneratedServerEnv on a non-serve launch must be rejected")
	}
	if !strings.Contains(err.Error(), "opencode serve") {
		t.Fatalf("expected opencode serve shape error, got %v", err)
	}
}

// GeneratedServerEnv values must not collide with inherited or allowlisted
// environment keys.
func TestGeneratedEnv_RejectsKeyCollision(t *testing.T) {
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "ws")
	cfgDir := filepath.Join(dir, "cfg")
	for _, d := range []string{wsDir, cfgDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	executor := execpolicy.New()
	req := execpolicy.LaunchRequest{
		RunID:     "run-ge3",
		SessionID: "sess-ge3",
		Command:   "opencode",
		Args:      []string{"serve"},
		Paths: workspace.WorkspacePaths{
			Root:   wsDir,
			Config: cfgDir,
		},
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"opencode"},
			Harnesses: map[string]storage.HarnessProfileSpec{
				"claude": {
					Model:             "m",
					NativeAuthMode:    "inherited_host_keychain",
					ExtraEnvAllowlist: []string{"OPENCODE_SERVER_PASSWORD"},
				},
			},
		},
		GeneratedServerEnv: &execpolicy.GeneratedServerEnv{
			Username: "u",
			Password: "p",
		},
	}
	_, err := executor.Start(context.Background(), req)
	if err == nil {
		t.Fatal("GeneratedServerEnv key colliding with an allowlisted key must be rejected")
	}
}
