package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/service"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestService_WorkerSeamEndToEnd(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	workspaceBaseDir := t.TempDir()

	// Install a controlled "claude" stub in a temp bin dir so the
	// native invocation path exercises the real PolicyExecutor while
	// remaining hermetic. The stub validates that the adapter selected
	// the correct contributor CLI and supplied the prompt.
	binDir := t.TempDir()
	claudeStub := filepath.Join(binDir, "claude")
	const stubScript = `#!/bin/sh
# Controlled fixture: validate model and prompt are passed correctly.
if [ "$1" != "--model" ] || [ "$2" != "claude-3-7-sonnet" ] || [ "$3" != "-p" ]; then
    echo "STUB_ARG_ERROR: $@" >&2
    exit 1
fi
echo "SEAM_FIXTURE_OK: model=$2 prompt=$4"
exit 0
`
	if err := os.WriteFile(claudeStub, []byte(stubScript), 0755); err != nil {
		t.Fatalf("write claude stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatalf("mkdir stateDir: %v", err)
	}

	lock, err := service.AcquireServiceLock(stateDir)
	if err != nil {
		t.Fatalf("AcquireServiceLock: %v", err)
	}
	defer lock.Release()

	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	defer store.Close()

	authToken := "test-token-worker-seam"
	cfg := service.ServerConfig{
		StateDir:         stateDir,
		InstanceID:       "inst-seam-1",
		AuthToken:        authToken,
		WorkspaceBaseDir: workspaceBaseDir,
	}

	// Create server with adp == nil; Server auto-wires ManagedWorkerAdapter
	// using its initialized WorkspaceManager and PolicyExecutor.
	srv, err := service.NewServer(store, lock, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// Verify production seam components are wired.
	if srv.WorkspaceManager() == nil {
		t.Fatal("expected srv.WorkspaceManager to be non-nil")
	}
	if srv.PolicyExecutor() == nil {
		t.Fatal("expected srv.PolicyExecutor to be non-nil")
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	// 1. Create run via POST /v1/runs with canonical profile including
	//    the claude harness spec. The harness spec drives native invocation.
	runID := "run-seam-1"
	controllerLease := "lease-seam-1"
	prof := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		NetworkAllowlist:    []string{},
		CodeIndexScope:      []string{},
		Tooling:             []string{"claude"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"claude": {
				Model:             "claude-3-7-sonnet",
				NativeAuthMode:    "inherited_host_keychain",
				ExtraEnvAllowlist: []string{},
			},
		},
	}

	createRunBody, _ := json.Marshal(service.CreateRunRequest{
		OpID:               "op-create-seam-run",
		ControllerLease:    controllerLease,
		RunID:              runID,
		Brief:              "Prove production daemon worker execution seam",
		SourceRepoIdentity: "none",
		SourceCommit:       "",
		SourceTree:         "",
		Profile:            prof,
	})

	runReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/runs", bytes.NewReader(createRunBody))
	runReq.Header.Set("Authorization", "Bearer "+authToken)
	runReq.Header.Set("Content-Type", "application/json")

	runResp, err := http.DefaultClient.Do(runReq)
	if err != nil {
		t.Fatalf("create run request failed: %v", err)
	}
	defer runResp.Body.Close()
	if runResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(runResp.Body)
		t.Fatalf("expected 201 Created, got %d: %s", runResp.StatusCode, string(b))
	}

	// 2. Adopt controller lease
	adoptBody, _ := json.Marshal(service.ControllerAdoptRequest{
		OpID:           "op-adopt-1",
		Harness:        "agy",
		ControllerRef:  "c1",
		BootstrapLease: controllerLease,
	})
	adoptReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v1/runs/%s/controller/adopt", server.URL, runID), bytes.NewReader(adoptBody))
	adoptReq.Header.Set("Authorization", "Bearer "+authToken)
	adoptReq.Header.Set("Content-Type", "application/json")

	adoptResp, err := http.DefaultClient.Do(adoptReq)
	if err != nil {
		t.Fatalf("adopt request failed: %v", err)
	}
	defer adoptResp.Body.Close()
	if adoptResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(adoptResp.Body)
		t.Fatalf("expected 200 OK on adopt, got %d: %s", adoptResp.StatusCode, string(b))
	}

	var grantResp service.ControllerGrantResponse
	if err := json.NewDecoder(adoptResp.Body).Decode(&grantResp); err != nil {
		t.Fatalf("decode adopt response: %v", err)
	}
	activeLease := grantResp.LeaseSecret

	// 2b. Connect adopted controller to service instance
	connBody := fmt.Sprintf(`{"op_id":"op-conn-1","controller_lease":%q,"instance_id":%q,"expected_generation":%d}`, activeLease, srv.InstanceID(), grantResp.Generation)
	connReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v1/runs/%s/controller/connect", server.URL, runID), strings.NewReader(connBody))
	connReq.Header.Set("Authorization", "Bearer "+authToken)
	connReq.Header.Set("Content-Type", "application/json")

	connResp, err := http.DefaultClient.Do(connReq)
	if err != nil {
		t.Fatalf("connect request failed: %v", err)
	}
	defer connResp.Body.Close()
	if connResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(connResp.Body)
		t.Fatalf("expected 200 OK on connect, got %d: %s", connResp.StatusCode, string(b))
	}

	// 3. Create session in store with contributor matching the harness profile
	sessionID := "sess-seam-1"
	sessRec, err := store.CreateSession(ctx, "op-create-sess", activeLease, storage.SessionRecord{
		ID:                  sessionID,
		RunID:               runID,
		Contributor:         string(council.Claude),
		Role:                "coder",
		IsActiveContributor: true,
		State:               "parked",
		Visibility:          "reachable",
	})
	if err != nil {
		t.Fatalf("store.CreateSession failed: %v", err)
	}

	// 4. Queue prompt
	turnKey := "turn-seam-1"
	queueBody := fmt.Sprintf(`{"op_id":"op-queue-1","controller_lease":%q,"expected_version":%d,"turn_key":%q,"prompt":"hello from seam test"}`,
		activeLease, sessRec.CommittedVersion, turnKey)
	queueReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v1/runs/%s/sessions/%s/prompts/queue", server.URL, runID, sessionID), strings.NewReader(queueBody))
	queueReq.Header.Set("Authorization", "Bearer "+authToken)
	queueReq.Header.Set("Content-Type", "application/json")

	queueResp, err := http.DefaultClient.Do(queueReq)
	if err != nil {
		t.Fatalf("queue prompt failed: %v", err)
	}
	defer queueResp.Body.Close()
	if queueResp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(queueResp.Body)
		t.Fatalf("expected 200 OK on queue prompt, got %d: %s", queueResp.StatusCode, string(b))
	}

	var queueResult service.QueuePromptResponse
	_ = json.NewDecoder(queueResp.Body).Decode(&queueResult)

	// 5. Release turn -> ExecutionSupervisor dispatches through ManagedWorkerAdapter
	releaseBody := fmt.Sprintf(`{"op_id":"op-rel-1","controller_lease":%q,"expected_version":%d}`,
		activeLease, queueResult.Receipt.CommittedVersion)
	relReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v1/runs/%s/sessions/%s/turns/%s/release", server.URL, runID, sessionID, turnKey), strings.NewReader(releaseBody))
	relReq.Header.Set("Authorization", "Bearer "+authToken)
	relReq.Header.Set("Content-Type", "application/json")

	relResp, err := http.DefaultClient.Do(relReq)
	if err != nil {
		t.Fatalf("release turn failed: %v", err)
	}
	defer relResp.Body.Close()
	if relResp.StatusCode != http.StatusAccepted {
		b, _ := io.ReadAll(relResp.Body)
		t.Fatalf("expected 202 Accepted on release, got %d: %s", relResp.StatusCode, string(b))
	}

	// 6. Wait for turn to reach a terminal status through the production seam
	var finalTurn *storage.TurnDetails
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		td, err := store.GetTurnDetails(ctx, sessionID, turnKey)
		if err == nil && td != nil {
			switch td.Status {
			case council.TurnCompleted, council.TurnFailed:
				finalTurn = td
			}
		}
		if finalTurn != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if finalTurn == nil {
		t.Fatalf("expected turn to reach a terminal status within deadline, but it did not")
	}
	if finalTurn.Status != council.TurnCompleted {
		t.Fatalf("expected TurnCompleted (controlled claude stub succeeds), got: %s", finalTurn.Status)
	}

	// 7. Verify workspace was allocated under workspaceBaseDir, NOT stateDir.
	// This is the primary disjoint storage boundary invariant.
	paths, ok := srv.WorkspaceManager().GetPaths(runID, sessionID)
	if !ok {
		t.Fatalf("expected workspace paths allocated for %s/%s", runID, sessionID)
	}
	if paths.Mode != "none" {
		t.Fatalf("expected mode none, got %s", paths.Mode)
	}
	if _, err := os.Stat(paths.Root); err != nil {
		t.Fatalf("expected workspace root directory to exist: %v", err)
	}

	realWorkspaceBase, _ := filepath.EvalSymlinks(workspaceBaseDir)
	realStateDir, _ := filepath.EvalSymlinks(stateDir)
	realRoot, _ := filepath.EvalSymlinks(paths.Root)

	if !strings.HasPrefix(realRoot, realWorkspaceBase) {
		t.Fatalf("workspace root %s is not under workspaceBaseDir %s — disjointness boundary violated", realRoot, realWorkspaceBase)
	}
	if strings.HasPrefix(realRoot, realStateDir) {
		t.Fatalf("workspace root %s must not be under stateDir %s — disjointness boundary violated", realRoot, realStateDir)
	}
}
