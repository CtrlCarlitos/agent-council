package execpolicy_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

type mockProfileStore struct {
	profiles map[string]storage.RunProfileRecord
	sessions map[string]storage.SessionMetadata
}

func (m *mockProfileStore) GetRunProfile(ctx context.Context, runID string) (storage.RunProfileRecord, error) {
	if p, ok := m.profiles[runID]; ok {
		return p, nil
	}
	return storage.RunProfileRecord{}, storage.ErrRunNotFound
}

func (m *mockProfileStore) GetSessionRunID(ctx context.Context, sessionID string) (string, error) {
	if s, ok := m.sessions[sessionID]; ok {
		return s.RunID, nil
	}
	return "", storage.ErrSessionNotFound
}

func (m *mockProfileStore) GetSessionMetadata(ctx context.Context, sessionID string) (storage.SessionMetadata, error) {
	if s, ok := m.sessions[sessionID]; ok {
		return s, nil
	}
	return storage.SessionMetadata{}, storage.ErrSessionNotFound
}

func TestBuildNativeInvocation(t *testing.T) {
	tests := []struct {
		contrib     council.Contributor
		spec        storage.HarnessProfileSpec
		prompt      string
		wantCmd     string
		wantArgs    []string
		expectError bool
	}{
		{
			contrib: council.Claude,
			spec: storage.HarnessProfileSpec{
				Model:          "claude-3-7-sonnet",
				NativeAuthMode: "inherited_host_keychain",
			},
			prompt:   "summarize PR",
			wantCmd:  "claude",
			wantArgs: []string{"--model", "claude-3-7-sonnet", "-p", "summarize PR"},
		},
		{
			contrib: council.Codex,
			spec: storage.HarnessProfileSpec{
				Model:          "o3-mini",
				NativeAuthMode: "inherited_host_keychain",
			},
			prompt:   "audit code",
			wantCmd:  "codex",
			wantArgs: []string{"exec", "--model", "o3-mini", "audit code"},
		},
		{
			contrib: council.OpenCode,
			spec: storage.HarnessProfileSpec{
				Model:          "glm-4",
				NativeAuthMode: "inherited_host_keychain",
			},
			prompt:   "analyze repo",
			wantCmd:  "opencode",
			wantArgs: []string{"run", "--model", "glm-4", "analyze repo"},
		},
		{
			contrib: council.Agy,
			spec: storage.HarnessProfileSpec{
				Model:          "gemini-2.5-pro",
				NativeAuthMode: "inherited_host_keychain",
			},
			prompt:   "generate test",
			wantCmd:  "agy",
			wantArgs: []string{"--model", "gemini-2.5-pro", "-p", "generate test"},
		},
		{
			contrib: "unknown_contrib",
			spec: storage.HarnessProfileSpec{
				Model:          "some-model",
				NativeAuthMode: "inherited_host_keychain",
			},
			prompt:      "test",
			expectError: true,
		},
		{
			contrib: council.Claude,
			spec: storage.HarnessProfileSpec{
				Model:          "",
				NativeAuthMode: "inherited_host_keychain",
			},
			prompt:      "test",
			expectError: true,
		},
		{
			contrib: council.Claude,
			spec: storage.HarnessProfileSpec{
				Model:          "claude-3-7-sonnet",
				NativeAuthMode: "",
			},
			prompt:      "test",
			expectError: true,
		},
	}

	for _, tc := range tests {
		inv, err := execpolicy.BuildNativeInvocation(tc.contrib, tc.spec, tc.prompt)
		if tc.expectError {
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.contrib)
			}
			continue
		}
		if err != nil {
			t.Fatalf("BuildNativeInvocation failed for %s: %v", tc.contrib, err)
		}
		if inv.Command != tc.wantCmd {
			t.Fatalf("expected command %q, got %q", tc.wantCmd, inv.Command)
		}
		if len(inv.Args) != len(tc.wantArgs) {
			t.Fatalf("expected args %+v, got %+v", tc.wantArgs, inv.Args)
		}
		for i, arg := range inv.Args {
			if arg != tc.wantArgs[i] {
				t.Fatalf("arg[%d]: expected %q, got %q", i, tc.wantArgs[i], arg)
			}
		}
	}
}

func TestManagedWorkerAdapter_LifecycleAndArgumentPassing(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	workspaceBase := t.TempDir()

	// NewWorkspaceManager(stateDir, workspaceBaseDir)
	wm, err := workspace.NewWorkspaceManager(stateDir, workspaceBase)
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}

	// Create test script for "claude" in a temp bin dir
	binDir := t.TempDir()
	claudeScript := filepath.Join(binDir, "claude")
	scriptContent := `#!/bin/sh
if [ "$1" != "--model" ] || [ "$2" != "claude-3-7-sonnet" ] || [ "$3" != "-p" ] || [ "$4" != "verify native harness invocation" ]; then
    echo "INVALID ARGS: $@" >&2
    exit 1
fi
echo "NATIVE_CLAUDE_SUCCESS: model=$2 prompt=$4"
exit 0
`
	if err := os.WriteFile(claudeScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+oldPath)

	store := &mockProfileStore{
		profiles: map[string]storage.RunProfileRecord{
			"run-1": {
				RunID:         "run-1",
				WorkspaceMode: "none",
				Profile: storage.CanonicalProfile{
					AlgoVersion:         "cprof-v1",
					WorkspaceMode:       "none",
					IsolationStrictness: "permissive_dev",
					NetworkMode:         "unrestricted",
					Tooling:             []string{"claude"},
					Harnesses: map[string]storage.HarnessProfileSpec{
						"claude": {
							Model:             "claude-3-7-sonnet",
							NativeAuthMode:    "inherited_host_keychain",
							ExtraEnvAllowlist: []string{},
						},
					},
				},
			},
		},
		sessions: map[string]storage.SessionMetadata{
			"session-1": {
				SessionID:   "session-1",
				RunID:       "run-1",
				Contributor: "claude",
			},
		},
	}

	executor := execpolicy.New()
	adp := execpolicy.NewWorkerAdapter(wm, executor, store)

	// 1. Probe
	report, err := adp.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if report.Capabilities.StreamingObservation != adapter.CapabilitySupported {
		t.Fatalf("expected StreamingObservation supported")
	}

	// 2. CreateSession
	req := adapter.CreateSessionRequest{
		SessionID:   "session-1",
		Contributor: council.Claude,
		Config: adapter.SessionConfig{
			Tooling: []string{"claude"},
		},
	}
	binding, err := adp.CreateSession(ctx, req)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if binding.SessionID != "session-1" {
		t.Fatalf("expected session-1, got: %s", binding.SessionID)
	}
	if binding.Config.Model != "claude-3-7-sonnet" {
		t.Fatalf("expected model claude-3-7-sonnet, got: %s", binding.Config.Model)
	}

	// Verify workspace allocated under workspaceBase, NOT stateDir
	paths, ok := wm.GetPaths("run-1", "session-1")
	if !ok {
		t.Fatalf("expected workspace paths allocated for run-1/session-1")
	}
	realWorkspaceBase, _ := filepath.EvalSymlinks(workspaceBase)
	realStateDir, _ := filepath.EvalSymlinks(stateDir)
	realRoot, _ := filepath.EvalSymlinks(paths.Root)

	if !strings.HasPrefix(realRoot, realWorkspaceBase) {
		t.Fatalf("workspace root %s is not under workspace base %s", realRoot, realWorkspaceBase)
	}
	if strings.HasPrefix(realRoot, realStateDir) {
		t.Fatalf("workspace root %s must not be under state dir %s", realRoot, realStateDir)
	}

	// 3. Dispatch
	ref := adapter.TurnRef{
		SessionID: "session-1",
		TurnKey:   "turn-1",
	}
	outcome, err := adp.Dispatch(ctx, ref, "verify native harness invocation")
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("expected DispatchAccepted, got: %s", outcome.Status)
	}

	// 4. Collect
	collectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := adp.Collect(collectCtx, ref)
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if res.Status != council.TurnCompleted {
		t.Fatalf("expected TurnCompleted, got: %s", res.Status)
	}
	if res.ResultStatus != adapter.ResultAvailable {
		t.Fatalf("expected ResultAvailable, got: %s", res.ResultStatus)
	}
	if !strings.Contains(res.Output, "NATIVE_CLAUDE_SUCCESS: model=claude-3-7-sonnet prompt=verify native harness invocation") {
		t.Fatalf("output missing expected verification string: %s", res.Output)
	}

	// Cleanup
	_ = wm.CloseWorkspace("run-1", "session-1")
}

func TestManagedWorkerAdapter_ProcessFailureRecordedAsTurnFailed(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	workspaceBase := t.TempDir()

	wm, err := workspace.NewWorkspaceManager(stateDir, workspaceBase)
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}

	// Create test script that exits with non-zero code
	binDir := t.TempDir()
	failingScript := filepath.Join(binDir, "claude")
	scriptContent := `#!/bin/sh
echo "CRASH: missing auth token or provider failure" >&2
exit 42
`
	if err := os.WriteFile(failingScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	oldPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+oldPath)

	store := &mockProfileStore{
		profiles: map[string]storage.RunProfileRecord{
			"run-fail": {
				RunID:         "run-fail",
				WorkspaceMode: "none",
				Profile: storage.CanonicalProfile{
					AlgoVersion:         "cprof-v1",
					WorkspaceMode:       "none",
					IsolationStrictness: "permissive_dev",
					NetworkMode:         "unrestricted",
					Tooling:             []string{"claude"},
					Harnesses: map[string]storage.HarnessProfileSpec{
						"claude": {
							Model:             "claude-3-7-sonnet",
							NativeAuthMode:    "inherited_host_keychain",
							ExtraEnvAllowlist: []string{},
						},
					},
				},
			},
		},
		sessions: map[string]storage.SessionMetadata{
			"session-fail": {
				SessionID:   "session-fail",
				RunID:       "run-fail",
				Contributor: "claude",
			},
		},
	}

	executor := execpolicy.New()
	adp := execpolicy.NewWorkerAdapter(wm, executor, store)

	ref := adapter.TurnRef{
		SessionID: "session-fail",
		TurnKey:   "turn-fail",
	}

	outcome, err := adp.Dispatch(ctx, ref, "trigger crash")
	if err != nil {
		t.Fatalf("Dispatch failed: %v", err)
	}
	if outcome.Status != adapter.DispatchAccepted {
		t.Fatalf("expected DispatchAccepted, got: %s", outcome.Status)
	}

	// Collect must report TurnFailed and ResultFailed with non-nil error!
	collectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, collectErr := adp.Collect(collectCtx, ref)
	if collectErr == nil {
		t.Fatal("expected Collect to return an error for nonzero exit process, got nil")
	}
	if res.Status != council.TurnFailed {
		t.Fatalf("expected TurnFailed, got %q", res.Status)
	}
	if res.ResultStatus != adapter.ResultFailed {
		t.Fatalf("expected ResultFailed, got %q", res.ResultStatus)
	}
	if !strings.Contains(res.Output, "CRASH: missing auth token or provider failure") {
		t.Fatalf("expected crash details in output, got: %s", res.Output)
	}
}

func TestManagedWorkerAdapter_FailClosedOnMissingOrInvalidProfile(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	workspaceBase := t.TempDir()

	wm, err := workspace.NewWorkspaceManager(stateDir, workspaceBase)
	if err != nil {
		t.Fatalf("NewWorkspaceManager: %v", err)
	}

	store := &mockProfileStore{
		profiles: map[string]storage.RunProfileRecord{
			"run-empty-harnesses": {
				RunID: "run-empty-harnesses",
				Profile: storage.CanonicalProfile{
					AlgoVersion: "cprof-v1",
					Tooling:     []string{"claude"},
					Harnesses:   map[string]storage.HarnessProfileSpec{}, // empty!
				},
			},
			"run-missing-tooling": {
				RunID: "run-missing-tooling",
				Profile: storage.CanonicalProfile{
					AlgoVersion: "cprof-v1",
					Tooling:     []string{"other-tool"}, // claude missing from tooling!
					Harnesses: map[string]storage.HarnessProfileSpec{
						"claude": {
							Model:          "claude-3-7-sonnet",
							NativeAuthMode: "inherited_host_keychain",
						},
					},
				},
			},
		},
		sessions: map[string]storage.SessionMetadata{
			"session-unconfigured": {
				SessionID:   "session-unconfigured",
				RunID:       "run-empty-harnesses",
				Contributor: "claude",
			},
			"session-disallowed-tool": {
				SessionID:   "session-disallowed-tool",
				RunID:       "run-missing-tooling",
				Contributor: "claude",
			},
		},
	}

	executor := execpolicy.New()
	adp := execpolicy.NewWorkerAdapter(wm, executor, store)

	// 1. Missing session in store fails closed
	_, err = adp.Dispatch(ctx, adapter.TurnRef{SessionID: "non-existent-session", TurnKey: "t1"}, "prompt")
	if err == nil {
		t.Fatal("expected dispatch to fail closed on non-existent session")
	}

	// 2. Contributor not in harnesses fails closed
	_, err = adp.Dispatch(ctx, adapter.TurnRef{SessionID: "session-unconfigured", TurnKey: "t1"}, "prompt")
	if err == nil {
		t.Fatal("expected dispatch to fail closed on unconfigured harness contributor")
	}

	// 3. Harness command not in profile tooling allowlist fails closed
	_, err = adp.Dispatch(ctx, adapter.TurnRef{SessionID: "session-disallowed-tool", TurnKey: "t1"}, "prompt")
	if err == nil {
		t.Fatal("expected dispatch to fail closed when harness command not in profile tooling")
	}
}
