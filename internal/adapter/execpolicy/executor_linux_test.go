//go:build linux

package execpolicy_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestPolicyExecutor_LinuxStrictKernelIsolation(t *testing.T) {
	ctx := context.Background()
	executor := execpolicy.New()

	runID := "run-strict-kernel"
	sessionID := "session-strict-kernel"
	paths := setupWorkspacePaths(t, runID, sessionID)

	t.Run("strict_allowlist_fails_closed", func(t *testing.T) {
		prof := storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "isolated_branch",
			IsolationStrictness: "strict",
			NetworkMode:         "allowlist",
			NetworkAllowlist:    []string{"example.com:443"},
			Tooling:             []string{"echo"},
			Harnesses: map[string]storage.HarnessProfileSpec{
				"agy": {Model: "gemini-2.5-pro", NativeAuthMode: "inherited_host_keychain"},
			},
		}
		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: sessionID,
			TurnKey:   "t1",
			AttemptID: "a1",
			Command:   "echo",
			Args:      []string{"hi"},
			Paths:     paths,
			Profile:   prof,
		}
		_, err := executor.Start(ctx, req)
		if !errors.Is(err, execpolicy.ErrUnsupportedIsolationCapability) {
			t.Fatalf("expected ErrUnsupportedIsolationCapability for strict allowlist, got: %v", err)
		}
	})

	t.Run("strict_readonly_fails_closed", func(t *testing.T) {
		prof := storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "readonly",
			IsolationStrictness: "strict",
			NetworkMode:         "none",
			Tooling:             []string{"echo"},
			Harnesses: map[string]storage.HarnessProfileSpec{
				"agy": {Model: "gemini-2.5-pro", NativeAuthMode: "inherited_host_keychain"},
			},
		}
		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: sessionID,
			TurnKey:   "t1",
			AttemptID: "a1",
			Command:   "echo",
			Args:      []string{"hi"},
			Paths:     paths,
			Profile:   prof,
		}
		_, err := executor.Start(ctx, req)
		if !errors.Is(err, execpolicy.ErrUnsupportedIsolationCapability) {
			t.Fatalf("expected ErrUnsupportedIsolationCapability for strict readonly, got: %v", err)
		}
	})

	t.Run("strict_none_enforces_kernel_network_unreachable", func(t *testing.T) {
		// Verify python3 or nc is available for testing socket connection failure
		tool := "python3"
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("python3 not available to test direct socket connection")
		}

		prof := storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "isolated_branch",
			IsolationStrictness: "strict",
			NetworkMode:         "none",
			Tooling:             []string{"python3"},
			Harnesses: map[string]storage.HarnessProfileSpec{
				"agy": {Model: "gemini-2.5-pro", NativeAuthMode: "inherited_host_keychain"},
			},
		}

		// Try to connect to 8.8.8.8:80
		script := `import socket, sys
try:
    s = socket.socket()
    s.settimeout(1.0)
    s.connect(("8.8.8.8", 80))
    sys.exit(0) # Unexpectedly succeeded!
except OSError as e:
    print(f"KERNEL_NETWORK_DENIED: {e}")
    sys.exit(42) # Expected denial exit code
`
		req := execpolicy.LaunchRequest{
			RunID:     runID,
			SessionID: sessionID,
			TurnKey:   "t1",
			AttemptID: "a1",
			Command:   "python3",
			Args:      []string{"-c", script},
			Paths:     paths,
			Profile:   prof,
		}

		proc, err := executor.Start(ctx, req)
		if err != nil {
			t.Fatalf("expected strict none start to succeed with kernel namespaces, got: %v", err)
		}

		var stderrBuf bytes.Buffer
		var stdoutBuf bytes.Buffer
		go func() { _, _ = io.Copy(&stdoutBuf, proc.Stdout()) }()
		go func() { _, _ = io.Copy(&stderrBuf, proc.Stderr()) }()

		code, err := proc.Wait()
		if err != nil {
			t.Fatalf("proc.Wait error: %v", err)
		}
		if code != 42 {
			t.Fatalf("expected code 42 (kernel network denied), got %d; stdout: %s; stderr: %s", code, stdoutBuf.String(), stderrBuf.String())
		}
		if !bytes.Contains(stdoutBuf.Bytes(), []byte("KERNEL_NETWORK_DENIED")) {
			t.Fatalf("expected KERNEL_NETWORK_DENIED in output, got: %s", stdoutBuf.String())
		}
	})
}
