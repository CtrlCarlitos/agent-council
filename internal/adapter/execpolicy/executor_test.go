package execpolicy_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func sampleProfile(tooling []string, extraEnv []string, strictness string) storage.CanonicalProfile {
	return storage.CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "isolated_branch",
		IsolationStrictness: strictness,
		NetworkMode:         "unrestricted",
		NetworkAllowlist:    []string{},
		CodeIndexScope:      []string{"internal"},
		Tooling:             tooling,
		Harnesses: map[string]storage.HarnessProfileSpec{
			"agy": {
				ExtraEnvAllowlist: extraEnv,
				Model:             "gemini-2.5-pro",
				NativeAuthMode:    "inherited_host_keychain",
			},
		},
	}
}

func setupWorkspacePaths(t *testing.T, runID, sessionID string) workspace.WorkspacePaths {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, runID, sessionID, "worktree")
	config := filepath.Join(tmp, runID, sessionID, "config")
	scratch := filepath.Join(tmp, runID, sessionID, "scratch")
	source := filepath.Join(tmp, runID, sessionID, "source")

	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatalf("failed to create root dir: %v", err)
	}
	if err := os.MkdirAll(config, 0700); err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatalf("failed to create scratch dir: %v", err)
	}
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatalf("failed to create source dir: %v", err)
	}

	return workspace.WorkspacePaths{
		Root:     root,
		Scratch:  scratch,
		Config:   config,
		Source:   source,
		Worktree: root,
		Mode:     "isolated_branch",
	}
}

func TestPolicyExecutor_DirectoryPinning(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	runID := "run-test-pin"
	sessionID := "session-test-pin"
	paths := setupWorkspacePaths(t, runID, sessionID)

	profile := sampleProfile([]string{"pwd", "sh"}, nil, "permissive_dev")

	req := execpolicy.LaunchRequest{
		RunID:     runID,
		SessionID: sessionID,
		TurnKey:   "turn-1",
		AttemptID: "attempt-1",
		Command:   "pwd",
		Args:      nil,
		Paths:     paths,
		Profile:   profile,
	}

	proc, err := executor.Start(ctx, req)
	if err != nil {
		t.Fatalf("expected Start to succeed, got: %v", err)
	}

	var stdoutBuf bytes.Buffer
	if _, err := io.Copy(&stdoutBuf, proc.Stdout()); err != nil {
		t.Fatalf("failed to read stdout: %v", err)
	}

	code, err := proc.Wait()
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}

	output := strings.TrimSpace(stdoutBuf.String())
	realExpected, err := filepath.EvalSymlinks(paths.Root)
	if err != nil {
		t.Fatalf("EvalSymlinks(paths.Root) failed: %v", err)
	}
	realOutput, err := filepath.EvalSymlinks(output)
	if err != nil {
		t.Fatalf("EvalSymlinks(output) failed: %v", err)
	}
	if realExpected != realOutput {
		t.Fatalf("expected process cwd %q, got %q", realExpected, realOutput)
	}

	// Invalid root directory rejection
	badPaths := paths
	badPaths.Root = filepath.Join(paths.Root, "nonexistent-directory-xyz")
	req.Paths = badPaths
	_, err = executor.Start(ctx, req)
	if err == nil {
		t.Fatalf("expected error for nonexistent root directory")
	}
	if !errors.Is(err, execpolicy.ErrInvalidDirectory) {
		t.Fatalf("expected ErrInvalidDirectory, got: %v", err)
	}
}

func TestPolicyExecutor_EnvironmentAllowlistAndSecretScrubbing(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	// Inject secrets and sensitive host environment variables
	t.Setenv("AUTH_TOKEN", "super-secret-auth-token")
	t.Setenv("COUNCIL_SOCKET", "/var/run/council.sock")
	t.Setenv("COUNCIL_LEASE", "lease-bearer-999")
	t.Setenv("TEST_SECRET_CANARY", "canary-secret-123")
	t.Setenv("PROVIDER_API_KEY", "key-value-456")
	t.Setenv("SESSION_TOKEN_CANARY", "token-value-789")
	t.Setenv("UNALLOWLISTED_HOST_VAR", "should-not-leak")
	t.Setenv("GEMINI_CLI_PROFILE", "approved-profile-val")

	runID := "run-env-test"
	sessionID := "session-env-test"
	paths := setupWorkspacePaths(t, runID, sessionID)

	profile := sampleProfile([]string{"env", "sh"}, []string{"GEMINI_CLI_PROFILE"}, "permissive_dev")

	req := execpolicy.LaunchRequest{
		RunID:             runID,
		SessionID:         sessionID,
		TurnKey:           "turn-env",
		AttemptID:         "att-1",
		Command:           "env",
		ExtraEnvAllowlist: []string{"GEMINI_CLI_PROFILE"},
		Paths:             paths,
		Profile:           profile,
	}

	proc, err := executor.Start(ctx, req)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	var stdout bytes.Buffer
	if _, err := io.Copy(&stdout, proc.Stdout()); err != nil {
		t.Fatalf("io.Copy stdout failed: %v", err)
	}

	code, err := proc.Wait()
	if err != nil {
		t.Fatalf("proc.Wait failed: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}

	outputEnv := stdout.String()
	lines := strings.Split(outputEnv, "\n")
	envMap := make(map[string]string)
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		parts := strings.SplitN(l, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// Invariant: HOME pinned to req.Paths.Config
	if envMap["HOME"] != paths.Config {
		t.Fatalf("expected HOME=%q, got %q", paths.Config, envMap["HOME"])
	}

	// Invariant: COUNCIL_* worker vars bound
	if envMap["COUNCIL_WORKSPACE_ROOT"] != paths.Root {
		t.Fatalf("expected COUNCIL_WORKSPACE_ROOT=%q, got %q", paths.Root, envMap["COUNCIL_WORKSPACE_ROOT"])
	}
	if envMap["COUNCIL_RUN_ID"] != runID {
		t.Fatalf("expected COUNCIL_RUN_ID=%q, got %q", runID, envMap["COUNCIL_RUN_ID"])
	}
	if envMap["COUNCIL_SESSION_ID"] != sessionID {
		t.Fatalf("expected COUNCIL_SESSION_ID=%q, got %q", sessionID, envMap["COUNCIL_SESSION_ID"])
	}

	// Invariant: Approved ExtraEnv passed
	if envMap["GEMINI_CLI_PROFILE"] != "approved-profile-val" {
		t.Fatalf("expected GEMINI_CLI_PROFILE=approved-profile-val, got %q", envMap["GEMINI_CLI_PROFILE"])
	}

	// Invariant: Unallowlisted host var NOT passed
	if _, has := envMap["UNALLOWLISTED_HOST_VAR"]; has {
		t.Fatalf("unallowlisted host var leaked into child environment")
	}

	// Invariant: Council secrets stripped
	for _, secretKey := range []string{"AUTH_TOKEN", "COUNCIL_SOCKET", "COUNCIL_LEASE"} {
		if _, has := envMap[secretKey]; has {
			t.Fatalf("council secret %s was not stripped from child environment", secretKey)
		}
	}

	// Invariant: Secret canary variables containing secret/key/token stripped
	for k := range envMap {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "secret") {
			t.Fatalf("environment variable %s containing 'secret' leaked into child", k)
		}
		if strings.Contains(lower, "key") {
			t.Fatalf("environment variable %s containing 'key' leaked into child", k)
		}
		if strings.Contains(lower, "token") {
			t.Fatalf("environment variable %s containing 'token' leaked into child", k)
		}
	}
}

func TestPolicyExecutor_RejectionOfParentSecretsInExtraAllowlist(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	runID := "run-rejection"
	sessionID := "session-rejection"
	paths := setupWorkspacePaths(t, runID, sessionID)

	profile := sampleProfile([]string{"sh", "env"}, []string{"AUTH_TOKEN", "MY_API_KEY", "LEAKED_SECRET"}, "permissive_dev")

	forbiddenKeys := []string{
		"AUTH_TOKEN",
		"COUNCIL_SOCKET",
		"COUNCIL_LEASE",
		"MY_API_KEY",
		"LEAKED_SECRET",
		"TEST_TOKEN",
	}

	for _, key := range forbiddenKeys {
		t.Run("reject_"+key, func(t *testing.T) {
			req := execpolicy.LaunchRequest{
				RunID:             runID,
				SessionID:         sessionID,
				TurnKey:           "turn-1",
				AttemptID:         "att-1",
				Command:           "env",
				ExtraEnvAllowlist: []string{key},
				Paths:             paths,
				Profile:           profile,
			}
			_, err := executor.Start(ctx, req)
			if err == nil {
				t.Fatalf("expected error for forbidden secret %q in ExtraEnvAllowlist", key)
			}
			if !errors.Is(err, execpolicy.ErrDisallowedEnv) {
				t.Fatalf("expected ErrDisallowedEnv, got: %v", err)
			}
		})
	}
}

func TestPolicyExecutor_ExtraEnvNotAllowlistedInProfile(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	runID := "run-extra-env"
	sessionID := "session-extra-env"
	paths := setupWorkspacePaths(t, runID, sessionID)

	profile := sampleProfile([]string{"sh", "env"}, []string{"APPROVED_VAR"}, "permissive_dev")

	req := execpolicy.LaunchRequest{
		RunID:             runID,
		SessionID:         sessionID,
		TurnKey:           "turn-1",
		AttemptID:         "att-1",
		Command:           "env",
		ExtraEnvAllowlist: []string{"SOME_UNAPPROVED_VAR"},
		Paths:             paths,
		Profile:           profile,
	}

	_, err := executor.Start(ctx, req)
	if err == nil {
		t.Fatalf("expected error for unapproved ExtraEnvAllowlist variable")
	}
	if !errors.Is(err, execpolicy.ErrDisallowedEnv) {
		t.Fatalf("expected ErrDisallowedEnv, got: %v", err)
	}
}

func TestPolicyExecutor_CommandAllowlist(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	runID := "run-cmd-allow"
	sessionID := "session-cmd-allow"
	paths := setupWorkspacePaths(t, runID, sessionID)

	profile := sampleProfile([]string{"git", "go"}, nil, "permissive_dev")

	// curl is not in Tooling
	req := execpolicy.LaunchRequest{
		RunID:     runID,
		SessionID: sessionID,
		TurnKey:   "turn-1",
		AttemptID: "att-1",
		Command:   "curl",
		Args:      []string{"https://example.com"},
		Paths:     paths,
		Profile:   profile,
	}

	_, err := executor.Start(ctx, req)
	if err == nil {
		t.Fatalf("expected error for unallowlisted command curl")
	}
	if !errors.Is(err, execpolicy.ErrDisallowedCommand) {
		t.Fatalf("expected ErrDisallowedCommand, got: %v", err)
	}
}

func TestPolicyExecutor_GitCommandGating(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	runID := "run-git"
	sessionID := "session-git"
	paths := setupWorkspacePaths(t, runID, sessionID)

	profile := sampleProfile([]string{"git"}, nil, "permissive_dev")

	disallowedCases := []struct {
		name string
		args []string
	}{
		{"checkout_new_branch_b", []string{"checkout", "-b", "feature-rogue"}},
		{"checkout_new_branch_B", []string{"checkout", "-B", "feature-rogue"}},
		{"switch_new_branch_c", []string{"switch", "-c", "feature-rogue"}},
		{"switch_new_branch_C", []string{"switch", "-C", "feature-rogue"}},
		{"branch_create", []string{"branch", "feature-rogue"}},
		{"push_origin_main", []string{"push", "origin", "main"}},
		{"push_main", []string{"push", "main"}},
		{"push_force_main", []string{"push", "--force", "origin", "main"}},
		{"checkout_main", []string{"checkout", "main"}},
		{"switch_main", []string{"switch", "main"}},
		{"checkout_master", []string{"checkout", "master"}},
	}

	for _, tc := range disallowedCases {
		t.Run(tc.name, func(t *testing.T) {
			req := execpolicy.LaunchRequest{
				RunID:     runID,
				SessionID: sessionID,
				TurnKey:   "turn-1",
				AttemptID: "att-1",
				Command:   "git",
				Args:      tc.args,
				Paths:     paths,
				Profile:   profile,
			}
			_, err := executor.Start(ctx, req)
			if err == nil {
				t.Fatalf("expected disallowed git invocation %v to fail", tc.args)
			}
			if !errors.Is(err, execpolicy.ErrDisallowedCommand) {
				t.Fatalf("expected ErrDisallowedCommand for %v, got: %v", tc.args, err)
			}
		})
	}
}

func TestPolicyExecutor_IsolationStrictness(t *testing.T) {
	ctx := context.Background()

	runID := "run-strict"
	sessionID := "session-strict"
	paths := setupWorkspacePaths(t, runID, sessionID)

	t.Run("strict_fails_closed_when_capability_unavailable", func(t *testing.T) {
		executor := execpolicy.New(execpolicy.WithCapabilityChecker(func(ctx context.Context, req execpolicy.LaunchRequest) error {
			return errors.New("missing netns / mount unshare capability")
		}))

		profile := sampleProfile([]string{"sh"}, nil, "strict")
		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: sessionID,
			TurnKey:   "turn-1",
			AttemptID: "att-1",
			Command:   "sh",
			Args:      []string{"-c", "echo hello"},
			Paths:     paths,
			Profile:   profile,
		}

		_, err := executor.Start(ctx, req)
		if err == nil {
			t.Fatalf("expected strict launch to fail closed when capability is unavailable")
		}
		if !errors.Is(err, execpolicy.ErrUnsupportedIsolationCapability) {
			t.Fatalf("expected ErrUnsupportedIsolationCapability, got: %v", err)
		}
	})

	t.Run("permissive_dev_proceeds_when_capability_unavailable", func(t *testing.T) {
		executor := execpolicy.New(execpolicy.WithCapabilityChecker(func(ctx context.Context, req execpolicy.LaunchRequest) error {
			return errors.New("missing netns capability")
		}))

		profile := sampleProfile([]string{"echo"}, nil, "permissive_dev")
		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: sessionID,
			TurnKey:   "turn-1",
			AttemptID: "att-1",
			Command:   "echo",
			Args:      []string{"hello"},
			Paths:     paths,
			Profile:   profile,
		}

		proc, err := executor.Start(ctx, req)
		if err != nil {
			t.Fatalf("permissive_dev should succeed, got: %v", err)
		}
		code, err := proc.Wait()
		if err != nil {
			t.Fatalf("proc.Wait failed: %v", err)
		}
		if code != 0 {
			t.Fatalf("expected exit code 0, got: %d", code)
		}
	})
}

func TestPolicyExecutor_ManagedProcessLifecycle(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	runID := "run-lifecycle"
	sessionID := "session-lifecycle"
	paths := setupWorkspacePaths(t, runID, sessionID)

	profile := sampleProfile([]string{"cat", "sleep"}, nil, "permissive_dev")

	t.Run("io_pipes_and_wait", func(t *testing.T) {
		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: sessionID,
			TurnKey:   "turn-io",
			AttemptID: "att-1",
			Command:   "cat",
			Paths:     paths,
			Profile:   profile,
		}

		proc, err := executor.Start(ctx, req)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		testData := "hello council managed process\n"
		if _, err := proc.Stdin().Write([]byte(testData)); err != nil {
			t.Fatalf("proc.Stdin().Write failed: %v", err)
		}

		// Close stdin so cat finishes
		closer, ok := proc.Stdin().(io.Closer)
		if !ok {
			t.Fatalf("stdin pipe does not implement io.Closer")
		}
		if err := closer.Close(); err != nil {
			t.Fatalf("closer.Close failed: %v", err)
		}

		var outBuf bytes.Buffer
		if _, err := io.Copy(&outBuf, proc.Stdout()); err != nil {
			t.Fatalf("io.Copy failed: %v", err)
		}

		code, err := proc.Wait()
		if err != nil {
			t.Fatalf("proc.Wait failed: %v", err)
		}
		if code != 0 {
			t.Fatalf("expected exit code 0, got %d", code)
		}
		if outBuf.String() != testData {
			t.Fatalf("expected output %q, got %q", testData, outBuf.String())
		}
	})

	t.Run("terminate_long_running_process", func(t *testing.T) {
		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: sessionID,
			TurnKey:   "turn-term",
			AttemptID: "att-1",
			Command:   "sleep",
			Args:      []string{"30"},
			Paths:     paths,
			Profile:   profile,
		}

		proc, err := executor.Start(ctx, req)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		termCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		if err := proc.Terminate(termCtx); err != nil {
			t.Fatalf("proc.Terminate failed: %v", err)
		}

		code, err := proc.Wait()
		if err != nil {
			t.Fatalf("proc.Wait failed: %v", err)
		}
		_ = code
	})

	t.Run("nonzero_exit_code_and_idempotent_wait", func(t *testing.T) {
		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: sessionID,
			TurnKey:   "turn-exit",
			AttemptID: "att-1",
			Command:   "sh",
			Args:      []string{"-c", "exit 42"},
			Paths:     paths,
			Profile:   sampleProfile([]string{"sh"}, nil, "permissive_dev"),
		}

		proc, err := executor.Start(ctx, req)
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		code, err := proc.Wait()
		if err != nil {
			t.Fatalf("proc.Wait failed: %v", err)
		}
		if code != 42 {
			t.Fatalf("expected exit code 42, got %d", code)
		}

		// Second call must return the same result
		code2, err2 := proc.Wait()
		if err2 != nil {
			t.Fatalf("second proc.Wait failed: %v", err2)
		}
		if code2 != 42 {
			t.Fatalf("expected second wait code 42, got %d", code2)
		}
	})
}

func TestPolicyExecutor_GitAllowedCommands(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	runID := "run-git-allow"
	sessionID := "session-git-allow"
	paths := setupWorkspacePaths(t, runID, sessionID)

	profile := sampleProfile([]string{"git"}, nil, "permissive_dev")
	assignedBranch := "council/" + runID + "/" + sessionID

	allowedCases := []struct {
		name string
		args []string
	}{
		{"status", []string{"status"}},
		{"diff", []string{"diff"}},
		{"log", []string{"log"}},
		{"checkout_path", []string{"checkout", "--", "file.txt"}},
		{"checkout_assigned_branch", []string{"checkout", assignedBranch}},
		{"switch_assigned_branch", []string{"switch", assignedBranch}},
	}

	for _, tc := range allowedCases {
		t.Run(tc.name, func(t *testing.T) {
			req := execpolicy.LaunchRequest{
				RunID:     runID,
				SessionID: sessionID,
				TurnKey:   "turn-1",
				AttemptID: "att-1",
				Command:   "git",
				Args:      tc.args,
				Paths:     paths,
				Profile:   profile,
			}
			// Command will fail execution because it's not a git repo, but PolicyExecutor.Start must NOT return ErrDisallowedCommand!
			proc, err := executor.Start(ctx, req)
			if err != nil {
				if errors.Is(err, execpolicy.ErrDisallowedCommand) {
					t.Fatalf("allowed git args %v was incorrectly rejected with ErrDisallowedCommand", tc.args)
				}
			}
			if proc != nil {
				_, _ = proc.Wait()
			}
		})
	}
}

func TestPolicyExecutor_RequestValidation(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	runID := "run-val"
	sessionID := "session-val"
	paths := setupWorkspacePaths(t, runID, sessionID)

	t.Run("invalid_algo_version", func(t *testing.T) {
		profile := sampleProfile([]string{"echo"}, nil, "permissive_dev")
		profile.AlgoVersion = "cprof-v999"

		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: sessionID,
			TurnKey:   "turn-1",
			AttemptID: "att-1",
			Command:   "echo",
			Paths:     paths,
			Profile:   profile,
		}

		_, err := executor.Start(ctx, req)
		if err == nil || !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) {
			t.Fatalf("expected ErrInvalidLaunchRequest for bad algo_version, got: %v", err)
		}
	})

	t.Run("invalid_session_id", func(t *testing.T) {
		profile := sampleProfile([]string{"echo"}, nil, "permissive_dev")

		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: "../malicious",
			TurnKey:   "turn-1",
			AttemptID: "att-1",
			Command:   "echo",
			Paths:     paths,
			Profile:   profile,
		}

		_, err := executor.Start(ctx, req)
		if err == nil || !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) {
			t.Fatalf("expected ErrInvalidLaunchRequest for bad session_id, got: %v", err)
		}
	})

	t.Run("empty_session_id", func(t *testing.T) {
		profile := sampleProfile([]string{"echo"}, nil, "permissive_dev")

		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: "",
			TurnKey:   "turn-1",
			AttemptID: "att-1",
			Command:   "echo",
			Paths:     paths,
			Profile:   profile,
		}

		_, err := executor.Start(ctx, req)
		if err == nil || !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) {
			t.Fatalf("expected ErrInvalidLaunchRequest for empty session_id, got: %v", err)
		}
	})
}
