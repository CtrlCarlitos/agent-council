//go:build unix

package client_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/client"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// AC-005 Task 7: Comprehensive worker isolation acceptance story through intended interfaces.
// Verifies immutable frozen inputs, symmetric workspace disjointness, policy executor
// environment sanitization and directory pinning, destination-allowlisting network proxy,
// worker artifact ingestion with ExecutionRef, atomic proposal sealing over HTTP,
// peer review reading, and explicit negative isolation denials.
// Controlled fixtures; native containerization is explicitly unverified.
func TestAC005_WorkerIsolationStory_ControlledFixture(t *testing.T) {
	// macOS bounds unix socket paths (~104 bytes); t.TempDir() paths exceed
	// it, so the fixture uses a bounded-length state directory root.
	dir, dirErr := os.MkdirTemp("/tmp", "ac-iso-")
	if dirErr != nil {
		t.Fatalf("state dir: %v", dirErr)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctx := context.Background()

	// -------------------------------------------------------------------------
	// Phase 1: Run Initialization with Immutable Frozen Inputs
	// -------------------------------------------------------------------------
	t.Log("Phase 1: Run Initialization with Immutable Frozen Inputs")

	briefText := "AC-005 Worker Isolation Acceptance Brief: Verify independent workers and frozen inputs."
	briefDigest, err := storage.ComputeBriefDigest(briefText)
	if err != nil {
		t.Fatalf("ComputeBriefDigest failed: %v", err)
	}
	if !strings.HasPrefix(briefDigest, "cbrief-v1:sha256:") {
		t.Fatalf("expected cbrief-v1:sha256: prefix, got %q", briefDigest)
	}

	repoDir, headCommit, treeHash := createIsolationGitRepo(t)

	sourceDigest, err := storage.ComputeSourceDigest("isolated_branch", repoDir, headCommit, treeHash)
	if err != nil {
		t.Fatalf("ComputeSourceDigest(isolated_branch) failed: %v", err)
	}
	if !strings.HasPrefix(sourceDigest, "csource-v1:sha256:") {
		t.Fatalf("expected csource-v1:sha256: prefix, got %q", sourceDigest)
	}

	sourceNoneDigest, err := storage.ComputeSourceDigest("none", "", "", "")
	if err != nil {
		t.Fatalf("ComputeSourceDigest(none) failed: %v", err)
	}
	if sourceNoneDigest != "csource-v1:none" {
		t.Fatalf("expected csource-v1:none, got %q", sourceNoneDigest)
	}

	canonicalProf := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "isolated_branch",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "allowlist",
		NetworkAllowlist:    []string{"api.anthropic.com:443"},
		CodeIndexScope:      []string{"."},
		Tooling:             []string{"env", "pwd", "git", "sh"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"claude": {
				ExtraEnvAllowlist: []string{"GEMINI_CLI_PROFILE"},
				Model:             "claude-3-7-sonnet",
				NativeAuthMode:    "native_tokens",
			},
			"codex": {
				ExtraEnvAllowlist: []string{},
				Model:             "o3",
				NativeAuthMode:    "native_tokens",
			},
			"opencode": {
				ExtraEnvAllowlist: []string{},
				Model:             "qwen-2.5-coder",
				NativeAuthMode:    "native_tokens",
			},
		},
	}

	profileDigest, _, err := storage.ComputeProfileDigest(canonicalProf)
	if err != nil {
		t.Fatalf("ComputeProfileDigest failed: %v", err)
	}
	if !strings.HasPrefix(profileDigest, "cprof-v1:sha256:") {
		t.Fatalf("expected cprof-v1:sha256: prefix, got %q", profileDigest)
	}

	baseDir := dir
	stateDir := filepath.Join(baseDir, "council-state")
	wsBaseDir := filepath.Join(baseDir, "council-workspaces")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("mkdir stateDir: %v", err)
	}
	if err := os.MkdirAll(wsBaseDir, 0700); err != nil {
		t.Fatalf("mkdir wsBaseDir: %v", err)
	}

	operatorToken := "tok-iso-story-1"
	srv, store, lock := startStoryServer(t, stateDir, "inst-iso-1", operatorToken)
	defer func() {
		_ = srv.Close()
		_ = store.Close()
		_ = lock.Release()
	}()

	runID := "run-iso-005"
	bootLease := "boot-iso-lease-1"
	if _, err := store.CreateRun(ctx, "op-init-run", runID, briefDigest, sourceDigest, profileDigest, bootLease); err != nil {
		t.Fatalf("CreateRun failed: %v", err)
	}

	controllerLease := storyAdopt(t, srv, operatorToken, runID, "claude", "conv-ctrl-iso", bootLease)
	if controllerLease == "" {
		t.Fatal("expected non-empty adopted controller lease")
	}

	ctrlBridge, err := client.NewControllerBridge(stateDir, runID, controllerLease)
	if err != nil {
		t.Fatalf("NewControllerBridge failed: %v", err)
	}
	if _, err := ctrlBridge.Connect(ctx, "op-ctrl-conn", 1); err != nil {
		t.Fatalf("controller connect failed: %v", err)
	}

	// -------------------------------------------------------------------------
	// Phase 2: Workspace Allocation Across All Three Modes with Symmetric Disjointness
	// -------------------------------------------------------------------------
	t.Log("Phase 2: Workspace Allocation Across All Three Modes with Symmetric Disjointness")

	// Disjointness negative verification:
	if _, err := workspace.NewWorkspaceManager(stateDir, filepath.Join(stateDir, "nested-workspaces")); !errors.Is(err, workspace.ErrWorkspaceStateOverlap) {
		t.Fatalf("expected ErrWorkspaceStateOverlap for workspaceBaseDir inside stateDir, got: %v", err)
	}
	if _, err := workspace.NewWorkspaceManager(filepath.Join(wsBaseDir, "nested-state"), wsBaseDir); !errors.Is(err, workspace.ErrWorkspaceStateOverlap) {
		t.Fatalf("expected ErrWorkspaceStateOverlap for stateDir inside workspaceBaseDir, got: %v", err)
	}
	if _, err := workspace.NewWorkspaceManager(stateDir, stateDir); !errors.Is(err, workspace.ErrWorkspaceStateOverlap) {
		t.Fatalf("expected ErrWorkspaceStateOverlap for identical state and workspace dirs, got: %v", err)
	}

	wsMgr, err := workspace.NewWorkspaceManager(stateDir, wsBaseDir)
	if err != nil {
		t.Fatalf("NewWorkspaceManager failed: %v", err)
	}

	// Mode "none"
	pathsNone, err := wsMgr.AllocateWorkspace(runID, "sess-none", "none", "", "")
	if err != nil {
		t.Fatalf("AllocateWorkspace(none) failed: %v", err)
	}
	defer func() { _ = wsMgr.CloseWorkspace(runID, "sess-none") }()
	if pathsNone.Mode != "none" || pathsNone.Root != pathsNone.Scratch || pathsNone.Source != "" || pathsNone.Worktree != "" {
		t.Fatalf("unexpected pathsNone layout: %+v", pathsNone)
	}

	// Mode "readonly"
	pathsRO, err := wsMgr.AllocateWorkspace(runID, "sess-ro", "readonly", repoDir, headCommit)
	if err != nil {
		t.Fatalf("AllocateWorkspace(readonly) failed: %v", err)
	}
	defer func() { _ = wsMgr.CloseWorkspace(runID, "sess-ro") }()
	if pathsRO.Mode != "readonly" || pathsRO.Root != pathsRO.Source || pathsRO.Source == "" {
		t.Fatalf("unexpected pathsRO layout: %+v", pathsRO)
	}
	if err := os.WriteFile(filepath.Join(pathsRO.Source, "forbidden-mutation.txt"), []byte("fail"), 0644); err == nil {
		t.Fatal("expected write to readonly workspace source to fail, but succeeded")
	}

	// Mode "isolated_branch" for worker 1 and worker 2
	pathsW1, err := wsMgr.AllocateWorkspace(runID, "sess-w1", "isolated_branch", repoDir, headCommit)
	if err != nil {
		t.Fatalf("AllocateWorkspace(isolated_branch, sess-w1) failed: %v", err)
	}
	defer func() { _ = wsMgr.CloseWorkspace(runID, "sess-w1") }()

	pathsW2, err := wsMgr.AllocateWorkspace(runID, "sess-w2", "isolated_branch", repoDir, headCommit)
	if err != nil {
		t.Fatalf("AllocateWorkspace(isolated_branch, sess-w2) failed: %v", err)
	}
	defer func() { _ = wsMgr.CloseWorkspace(runID, "sess-w2") }()

	if pathsW1.Root == pathsW2.Root {
		t.Fatalf("expected distinct workspace roots for workers, got %q", pathsW1.Root)
	}
	if pathsW1.Worktree == pathsW2.Worktree {
		t.Fatalf("expected distinct worktrees for workers, got %q", pathsW1.Worktree)
	}

	branchW1 := gitCurrentBranch(t, pathsW1.Worktree)
	expectedBranchW1 := "council/" + runID + "/sess-w1"
	if branchW1 != expectedBranchW1 {
		t.Fatalf("expected branch %q, got %q", expectedBranchW1, branchW1)
	}

	branchW2 := gitCurrentBranch(t, pathsW2.Worktree)
	expectedBranchW2 := "council/" + runID + "/sess-w2"
	if branchW2 != expectedBranchW2 {
		t.Fatalf("expected branch %q, got %q", expectedBranchW2, branchW2)
	}

	// -------------------------------------------------------------------------
	// Phase 3: PolicyExecutor Launching Simulated Workers
	// -------------------------------------------------------------------------
	t.Log("Phase 3: PolicyExecutor Launching Simulated Workers")

	t.Setenv("AUTH_TOKEN", "leak-secret-auth-token")
	t.Setenv("COUNCIL_SOCKET", "/var/run/council.sock")
	t.Setenv("COUNCIL_LEASE", "leak-controller-lease")
	t.Setenv("TEST_KEY_SECRET", "canary-secret-should-be-scrubbed")
	t.Setenv("GEMINI_CLI_PROFILE", "approved-profile-val")
	t.Setenv("UNALLOWLISTED_HOST_VAR", "unallowlisted-host-data")

	executor := execpolicy.New()

	// Verify environment allowlist and secret scrubbing
	envReq := execpolicy.LaunchRequest{
		RunID:             runID,
		SessionID:         "sess-w1",
		TurnKey:           "turn-w1-env",
		AttemptID:         "att-w1-env",
		Command:           "env",
		ExtraEnvAllowlist: []string{"GEMINI_CLI_PROFILE"},
		Paths:             pathsW1,
		Profile:           canonicalProf,
	}

	proc, err := executor.Start(ctx, envReq)
	if err != nil {
		t.Fatalf("executor.Start(env) failed: %v", err)
	}
	var envStdout bytes.Buffer
	if _, err := io.Copy(&envStdout, proc.Stdout()); err != nil {
		t.Fatalf("io.Copy stdout failed: %v", err)
	}
	code, err := proc.Wait()
	if err != nil || code != 0 {
		t.Fatalf("proc.Wait() failed code=%d, err=%v", code, err)
	}

	envMap := make(map[string]string)
	for _, line := range strings.Split(envStdout.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if envMap["HOME"] != pathsW1.Config {
		t.Fatalf("expected HOME=%q, got %q", pathsW1.Config, envMap["HOME"])
	}
	if envMap["COUNCIL_WORKSPACE_ROOT"] != pathsW1.Root {
		t.Fatalf("expected COUNCIL_WORKSPACE_ROOT=%q, got %q", pathsW1.Root, envMap["COUNCIL_WORKSPACE_ROOT"])
	}
	if envMap["COUNCIL_RUN_ID"] != runID {
		t.Fatalf("expected COUNCIL_RUN_ID=%q, got %q", runID, envMap["COUNCIL_RUN_ID"])
	}
	if envMap["COUNCIL_SESSION_ID"] != "sess-w1" {
		t.Fatalf("expected COUNCIL_SESSION_ID=sess-w1, got %q", envMap["COUNCIL_SESSION_ID"])
	}
	if envMap["GEMINI_CLI_PROFILE"] != "approved-profile-val" {
		t.Fatalf("expected approved env GEMINI_CLI_PROFILE to pass, got %q", envMap["GEMINI_CLI_PROFILE"])
	}

	for _, secretKey := range []string{"AUTH_TOKEN", "COUNCIL_SOCKET", "COUNCIL_LEASE"} {
		if _, has := envMap[secretKey]; has {
			t.Fatalf("council secret %s leaked into child environment", secretKey)
		}
	}
	if _, has := envMap["UNALLOWLISTED_HOST_VAR"]; has {
		t.Fatal("unallowlisted host var leaked into child environment")
	}
	for k := range envMap {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "secret") {
			t.Fatalf("key containing 'secret' (%s) leaked into child", k)
		}
		if strings.Contains(lower, "key") {
			t.Fatalf("key containing 'key' (%s) leaked into child", k)
		}
		if strings.Contains(lower, "token") {
			t.Fatalf("key containing 'token' (%s) leaked into child", k)
		}
	}

	// Verify child cwd pinning
	pwdReq := execpolicy.LaunchRequest{
		RunID:     runID,
		SessionID: "sess-w1",
		TurnKey:   "turn-w1-pwd",
		AttemptID: "att-w1-pwd",
		Command:   "pwd",
		Paths:     pathsW1,
		Profile:   canonicalProf,
	}
	pwdProc, err := executor.Start(ctx, pwdReq)
	if err != nil {
		t.Fatalf("executor.Start(pwd) failed: %v", err)
	}
	var pwdStdout bytes.Buffer
	_, _ = io.Copy(&pwdStdout, pwdProc.Stdout())
	pwdCode, err := pwdProc.Wait()
	if err != nil || pwdCode != 0 {
		t.Fatalf("pwd proc failed code=%d, err=%v", pwdCode, err)
	}
	evalExpectedRoot, _ := filepath.EvalSymlinks(pathsW1.Root)
	evalActualRoot, _ := filepath.EvalSymlinks(strings.TrimSpace(pwdStdout.String()))
	if evalExpectedRoot != evalActualRoot {
		t.Fatalf("expected pinned cwd %q, got %q", evalExpectedRoot, evalActualRoot)
	}

	// -------------------------------------------------------------------------
	// Phase 4: NetworkProxy Destination Allowlisting and Rebinding Defense
	// -------------------------------------------------------------------------
	t.Log("Phase 4: NetworkProxy Destination Allowlisting and Rebinding Defense")

	proxy, err := execpolicy.StartNetworkProxy(
		[]string{"api.anthropic.com:443"},
		execpolicy.WithDNSResolver(storyMockPublicResolver(map[string][]string{
			"rebind.attacker.local": {"10.0.0.5"},
			"loopback.local":        {"127.0.0.1"},
		})),
	)
	if err != nil {
		t.Fatalf("StartNetworkProxy failed: %v", err)
	}
	defer proxy.Close()

	// Allowed destination succeeds
	if err := proxy.CheckDestination("api.anthropic.com:443"); err != nil {
		t.Fatalf("expected allowed destination to succeed, got: %v", err)
	}

	// Unallowlisted destination denied
	if err := proxy.CheckDestination("evil.attacker.com:443"); !errors.Is(err, execpolicy.ErrNetworkAccessDenied) {
		t.Fatalf("expected ErrNetworkAccessDenied for unallowlisted host, got: %v", err)
	}

	// Non-public direct IP ranges denied
	nonPublicIPs := []string{"10.0.0.1:443", "127.0.0.1:8080", "192.168.1.1:443", "169.254.169.254:80"}
	for _, dest := range nonPublicIPs {
		if err := proxy.CheckDestination(dest); !errors.Is(err, execpolicy.ErrNetworkAccessDenied) {
			t.Fatalf("expected ErrNetworkAccessDenied for direct non-public IP %s, got: %v", dest, err)
		}
	}

	// DNS rebinding to private IPs denied
	if err := proxy.CheckDestination("rebind.attacker.local:443"); !errors.Is(err, execpolicy.ErrNetworkAccessDenied) {
		t.Fatalf("expected ErrNetworkAccessDenied for DNS rebinding domain, got: %v", err)
	}
	if err := proxy.CheckDestination("loopback.local:443"); !errors.Is(err, execpolicy.ErrNetworkAccessDenied) {
		t.Fatalf("expected ErrNetworkAccessDenied for loopback DNS rebinding domain, got: %v", err)
	}

	// -------------------------------------------------------------------------
	// Phase 5: Worker Artifact Ingestion with ExecutionRef
	// -------------------------------------------------------------------------
	t.Log("Phase 5: Worker Artifact Ingestion with ExecutionRef")

	// Create sessions in store
	if _, err := store.CreateSession(ctx, "op-sess-w1", controllerLease, storage.SessionRecord{
		ID: "sess-w1", RunID: runID, Contributor: "claude", Role: "worker",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create worker 1 session: %v", err)
	}

	if _, err := store.CreateSession(ctx, "op-sess-w2", controllerLease, storage.SessionRecord{
		ID: "sess-w2", RunID: runID, Contributor: "codex", Role: "worker",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create worker 2 session: %v", err)
	}

	if _, err := store.CreateSession(ctx, "op-sess-rev", controllerLease, storage.SessionRecord{
		ID: "sess-rev", RunID: runID, Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create reviewer session: %v", err)
	}

	// Queue and release turns to obtain active execution attempts
	verW1, _ := store.GetSessionVersion(ctx, "sess-w1")
	if _, err := ctrlBridge.QueuePrompt(ctx, "op-q-w1", "sess-w1", "turn-w1", "draft architecture", verW1); err != nil {
		t.Fatalf("queue prompt w1: %v", err)
	}
	verW1, _ = store.GetSessionVersion(ctx, "sess-w1")
	relW1, err := ctrlBridge.ReleaseTurn(ctx, "op-rel-w1", "sess-w1", "turn-w1", verW1)
	if err != nil {
		t.Fatalf("release turn w1: %v", err)
	}
	execRef1 := storage.ExecutionRef{
		SessionID: "sess-w1",
		TurnKey:   "turn-w1",
		AttemptID: relW1.Receipt.AttemptID,
	}

	verW2, _ := store.GetSessionVersion(ctx, "sess-w2")
	if _, err := ctrlBridge.QueuePrompt(ctx, "op-q-w2", "sess-w2", "turn-w2", "draft implementation", verW2); err != nil {
		t.Fatalf("queue prompt w2: %v", err)
	}
	verW2, _ = store.GetSessionVersion(ctx, "sess-w2")
	relW2, err := ctrlBridge.ReleaseTurn(ctx, "op-rel-w2", "sess-w2", "turn-w2", verW2)
	if err != nil {
		t.Fatalf("release turn w2: %v", err)
	}
	execRef2 := storage.ExecutionRef{
		SessionID: "sess-w2",
		TurnKey:   "turn-w2",
		AttemptID: relW2.Receipt.AttemptID,
	}

	// Worker 1 records draft proposal artifact
	art1Content := []byte("# Worker 1 Proposal: Frozen Profile Architecture\nProving uncompromised worker isolation.")
	draftMeta1, err := store.RecordObservedArtifact(ctx, execRef1, "proposal-arch.md", art1Content)
	if err != nil {
		t.Fatalf("RecordObservedArtifact failed: %v", err)
	}
	if draftMeta1.ID != "proposal-arch.md" || draftMeta1.Revision != 1 || draftMeta1.Digest == "" {
		t.Fatalf("unexpected draftMeta1: %+v", draftMeta1)
	}

	// Author worker can read draft
	authorData, authorMeta, err := store.ReadAuthorArtifact(ctx, execRef1, "proposal-arch.md")
	if err != nil {
		t.Fatalf("author ReadAuthorArtifact failed: %v", err)
	}
	if !bytes.Equal(authorData, art1Content) || authorMeta.Digest != draftMeta1.Digest {
		t.Fatalf("author read returned unexpected content or metadata")
	}

	// Sibling session receives ErrArtifactNotFound prior to release
	if _, _, err := store.ReadAuthorArtifact(ctx, execRef2, "proposal-arch.md"); !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound for sibling ReadAuthorArtifact, got: %v", err)
	}
	if _, _, err := store.ReadReleasedArtifact(ctx, "sess-w2", "propset-unreleased", "proposal-arch.md"); !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound for sibling ReadReleasedArtifact before release, got: %v", err)
	}
	if _, _, err := store.ReadReleasedArtifact(ctx, "sess-rev", "propset-unreleased", "proposal-arch.md"); !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound for reviewer ReadReleasedArtifact before release, got: %v", err)
	}

	// -------------------------------------------------------------------------
	// Phase 6: Controller Atomic Proposal Sealing via HTTP
	// -------------------------------------------------------------------------
	t.Log("Phase 6: Controller Atomic Proposal Sealing via HTTP")

	releasePath := "/v1/runs/" + runID + "/artifacts/release"
	validReleaseBody := fmt.Sprintf(`{"op_id":"op-seal-propset-1","controller_lease":%q,"members":[{"artifact_id":%q,"revision":%d,"digest":%q}]}`,
		controllerLease, draftMeta1.ID, draftMeta1.Revision, draftMeta1.Digest)

	// Auth negative checks:
	// 1. Missing Authorization header -> 401
	code, _ = storyPostWithAuth(t, srv.SocketPath(), "", releasePath, validReleaseBody)
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on missing auth, got %d", code)
	}
	// 2. Controller lease as bearer token -> 401
	code, _ = storyPostWithAuth(t, srv.SocketPath(), "Bearer "+controllerLease, releasePath, validReleaseBody)
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when controller lease is used as bearer, got %d", code)
	}
	// 3. Missing controller lease in body -> 400
	missingLeaseBody := fmt.Sprintf(`{"op_id":"op-seal-err-1","members":[{"artifact_id":%q,"revision":%d,"digest":%q}]}`,
		draftMeta1.ID, draftMeta1.Revision, draftMeta1.Digest)
	code, _ = storyPostWithAuth(t, srv.SocketPath(), "Bearer "+operatorToken, releasePath, missingLeaseBody)
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400 on missing controller_lease in body, got %d", code)
	}
	// 4. Invalid controller lease in body -> 403
	wrongLeaseBody := fmt.Sprintf(`{"op_id":"op-seal-err-2","controller_lease":"wrong-lease","members":[{"artifact_id":%q,"revision":%d,"digest":%q}]}`,
		draftMeta1.ID, draftMeta1.Revision, draftMeta1.Digest)
	code, _ = storyPostWithAuth(t, srv.SocketPath(), "Bearer "+operatorToken, releasePath, wrongLeaseBody)
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 on wrong controller_lease in body, got %d", code)
	}

	// Authorized atomic release call
	code, respBody := storyPostWithAuth(t, srv.SocketPath(), "Bearer "+operatorToken, releasePath, validReleaseBody)
	if code != http.StatusOK {
		t.Fatalf("expected 200 on release artifacts, got %d (body: %s)", code, respBody)
	}

	var releaseResp struct {
		Receipt storage.ProposalSetReceipt `json:"receipt"`
	}
	if err := json.Unmarshal([]byte(respBody), &releaseResp); err != nil {
		t.Fatalf("unmarshal ReleaseArtifactsResponse failed: %v, body: %s", err, respBody)
	}

	propsetDigest := releaseResp.Receipt.ProposalSetDigest
	if !strings.HasPrefix(propsetDigest, "propset-v1:sha256:") {
		t.Fatalf("expected propset-v1:sha256: prefix, got %q", propsetDigest)
	}
	if releaseResp.Receipt.RunID != runID {
		t.Fatalf("expected runID %q, got %q", runID, releaseResp.Receipt.RunID)
	}
	if releaseResp.Receipt.SealedAt.IsZero() {
		t.Fatal("expected non-zero SealedAt")
	}

	// Replay with identical op_id returns identical receipt
	code2, respBody2 := storyPostWithAuth(t, srv.SocketPath(), "Bearer "+operatorToken, releasePath, validReleaseBody)
	if code2 != http.StatusOK {
		t.Fatalf("expected 200 on replay, got %d", code2)
	}
	var replayResp struct {
		Receipt storage.ProposalSetReceipt `json:"receipt"`
	}
	_ = json.Unmarshal([]byte(respBody2), &replayResp)
	if replayResp.Receipt.ProposalSetDigest != propsetDigest {
		t.Fatalf("replay digest mismatch: %q vs %q", replayResp.Receipt.ProposalSetDigest, propsetDigest)
	}

	// -------------------------------------------------------------------------
	// Phase 7: Peer Reviewers Read Released Artifacts
	// -------------------------------------------------------------------------
	t.Log("Phase 7: Peer Reviewers Read Released Artifacts")

	relData, relMeta, err := store.ReadReleasedArtifact(ctx, "sess-rev", propsetDigest, "proposal-arch.md")
	if err != nil {
		t.Fatalf("reviewer ReadReleasedArtifact failed: %v", err)
	}
	if !bytes.Equal(relData, art1Content) {
		t.Fatalf("reviewer read unexpected content: %q vs %q", string(relData), string(art1Content))
	}
	if relMeta.Digest != draftMeta1.Digest || relMeta.Revision != draftMeta1.Revision {
		t.Fatalf("reviewer read metadata mismatch: %+v vs %+v", relMeta, draftMeta1)
	}

	// Reading with non-existent proposal set digest fails with ErrArtifactNotFound
	forgedDigest := "propset-v1:sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if _, _, err := store.ReadReleasedArtifact(ctx, "sess-rev", forgedDigest, "proposal-arch.md"); !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound for forged proposal set digest, got: %v", err)
	}

	// -------------------------------------------------------------------------
	// Phase 8: Negative Isolation Evidence
	// -------------------------------------------------------------------------
	t.Log("Phase 8: Negative Isolation Evidence")

	// 1. Nested git branch attempts denied by PolicyExecutor
	gitBranchTests := []struct {
		name string
		args []string
	}{
		{"checkout_b", []string{"checkout", "-b", "rogue-branch"}},
		{"checkout_B", []string{"checkout", "-B", "rogue-branch"}},
		{"switch_c", []string{"switch", "-c", "rogue-branch"}},
		{"switch_C", []string{"switch", "-C", "rogue-branch"}},
		{"branch_new", []string{"branch", "rogue-branch"}},
		{"checkout_main", []string{"checkout", "main"}},
		{"push_main", []string{"push", "origin", "main"}},
	}

	for _, tc := range gitBranchTests {
		t.Run("git_deny_"+tc.name, func(t *testing.T) {
			badGitReq := execpolicy.LaunchRequest{
				RunID:     runID,
				SessionID: "sess-w1",
				TurnKey:   "turn-w1-bad-git",
				AttemptID: "att-bad-git",
				Command:   "git",
				Args:      tc.args,
				Paths:     pathsW1,
				Profile:   canonicalProf,
			}
			_, err := executor.Start(ctx, badGitReq)
			if err == nil {
				t.Fatalf("expected disallowed git command %v to be denied", tc.args)
			}
			if !errors.Is(err, execpolicy.ErrDisallowedCommand) {
				t.Fatalf("expected ErrDisallowedCommand for %v, got: %v", tc.args, err)
			}
		})
	}

	// 2. Sibling cross-directory reads / path traversal
	if _, err := wsMgr.AllocateWorkspace(runID, "../sess-w2", "none", "", ""); !errors.Is(err, workspace.ErrInvalidIdentifier) {
		t.Fatalf("expected ErrInvalidIdentifier for sibling path traversal sessionID, got: %v", err)
	}
	if _, err := wsMgr.AllocateWorkspace("../rogue-run", "sess-w1", "none", "", ""); !errors.Is(err, workspace.ErrInvalidIdentifier) {
		t.Fatalf("expected ErrInvalidIdentifier for path traversal runID, got: %v", err)
	}

	badSessReq := execpolicy.LaunchRequest{
		RunID:     runID,
		SessionID: "../traversal-session",
		TurnKey:   "turn-bad-sess",
		AttemptID: "att-bad-sess",
		Command:   "pwd",
		Paths:     pathsW1,
		Profile:   canonicalProf,
	}
	if _, err := executor.Start(ctx, badSessReq); !errors.Is(err, execpolicy.ErrInvalidLaunchRequest) {
		t.Fatalf("expected ErrInvalidLaunchRequest for traversal sessionID in PolicyExecutor, got: %v", err)
	}

	// 3. Unsealed artifact access denied
	// Worker 1 creates unsealed draft on turn 2
	verW1b, _ := store.GetSessionVersion(ctx, "sess-w1")
	if _, err := ctrlBridge.QueuePrompt(ctx, "op-q-w1b", "sess-w1", "turn-w1b", "draft v2 unsealed", verW1b); err != nil {
		t.Fatalf("queue prompt w1b: %v", err)
	}
	verW1b, _ = store.GetSessionVersion(ctx, "sess-w1")
	relW1b, err := ctrlBridge.ReleaseTurn(ctx, "op-rel-w1b", "sess-w1", "turn-w1b", verW1b)
	if err != nil {
		t.Fatalf("release turn w1b: %v", err)
	}
	execRef1b := storage.ExecutionRef{
		SessionID: "sess-w1",
		TurnKey:   "turn-w1b",
		AttemptID: relW1b.Receipt.AttemptID,
	}

	unsealedContent := []byte("Sensitive unsealed draft proposal")
	if _, err := store.RecordObservedArtifact(ctx, execRef1b, "unsealed-spec.md", unsealedContent); err != nil {
		t.Fatalf("record unsealed artifact failed: %v", err)
	}

	// Reviewer cannot read unsealed artifact under sealed propset
	if _, _, err := store.ReadReleasedArtifact(ctx, "sess-rev", propsetDigest, "unsealed-spec.md"); !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound when reading unsealed artifact via ReadReleasedArtifact, got: %v", err)
	}

	// Sibling cannot read unsealed artifact via ReadAuthorArtifact
	if _, _, err := store.ReadAuthorArtifact(ctx, execRef2, "unsealed-spec.md"); !errors.Is(err, storage.ErrArtifactNotFound) {
		t.Fatalf("expected ErrArtifactNotFound when sibling reads unsealed artifact via ReadAuthorArtifact, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Test Helpers
// -----------------------------------------------------------------------------

func createIsolationGitRepo(t *testing.T) (string, string, string) {
	t.Helper()
	repoDir := t.TempDir()

	runCmd := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v (output: %s)", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}

	runCmd("init", "-b", "main")
	runCmd("config", "user.name", "Council Isolation Fixture")
	runCmd("config", "user.email", "council@isolation.internal")

	testFile := filepath.Join(repoDir, "README.md")
	if err := os.WriteFile(testFile, []byte("# AC-005 Isolation Fixture Repo\n"), 0644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	runCmd("add", "README.md")
	runCmd("commit", "-m", "initial commit")

	headCommit := runCmd("rev-parse", "HEAD")
	treeHash := runCmd("rev-parse", "HEAD^{tree}")
	return repoDir, headCommit, treeHash
}

func gitCurrentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse branch failed: %v (output: %s)", err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func storyMockPublicResolver(mappings map[string][]string) func(ctx context.Context, host string) ([]net.IP, error) {
	return func(ctx context.Context, host string) ([]net.IP, error) {
		if ips, ok := mappings[host]; ok {
			var parsed []net.IP
			for _, s := range ips {
				parsed = append(parsed, net.ParseIP(s))
			}
			return parsed, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
}

func storyPostWithAuth(t *testing.T, socketPath, authHeader, path, body string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}}}

	req, _ := http.NewRequestWithContext(ctx, "POST", "http://localhost"+path, strings.NewReader(body))
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(bodyBytes)
}
