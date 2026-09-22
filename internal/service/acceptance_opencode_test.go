//go:build unix

package service

// Task 7 acceptance story (OpenCode-only, provider-free): the full
// controller lifecycle through the service bridge with the production
// OpenCode adapter reaching a controlled stub `opencode serve` child
// through the real PolicyExecutor. Durable-outcome assertions throughout;
// the native worker path (supervisor) executes released turns.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/opencode"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

type acceptanceBridge struct {
	t      *testing.T
	client *http.Client
	token  string
}

func (b *acceptanceBridge) do(method, path, body string) (int, map[string]any) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, strings.NewReader(body))
	if err != nil {
		b.t.Fatalf("new request %s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestAcceptance_OpenCode_BridgeLifecycle drives the entire controller
// story: adopt (secret shown once) -> bridge connect -> bridge queue ->
// bridge release -> gated native execution through the production adapter
// -> bridge collect (parentID-correlated) -> bridge disconnect -> idle
// park -> bridge reconnect -> bridge queue/release follow-up -> resume
// replacement server -> bridge collect follow-up.
func TestAcceptance_OpenCode_BridgeLifecycle(t *testing.T) {
	// testStateDir keeps the state path short enough for a unix socket
	// (sun_path); the workspace base and probe root live beside it.
	dir := testStateDir(t)
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	for _, d := range []string{stateDir, wsBase} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	binDir := compileStubOpencodeBinary(t)
	t.Setenv("PATH", binDir+string(filepath.ListSeparator)+os.Getenv("PATH"))

	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Frozen run profile + contributor session (operator-supplied
	// canonical inputs).
	ctx := context.Background()
	bootstrap := "bootstrap-accept"
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-acc", ControllerLease: bootstrap, RunID: "run-acc",
		Brief: "acceptance story", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile: storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			Tooling:             []string{"opencode", "git", "go"},
			Harnesses: map[string]storage.HarnessProfileSpec{
				"opencode": {Model: "stub/model", NativeAuthMode: "managed_by_council"},
			},
		},
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-acc", bootstrap, storage.SessionRecord{
		ID: "sess-accept", RunID: "run-acc", Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Configured service construction: no adapter injected.
	probeProfile := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"opencode"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"opencode": {Model: "stub/model", NativeAuthMode: "managed_by_council"},
		},
	}
	instanceID := fmt.Sprintf("inst-acc-%d", time.Now().UnixNano())
	authToken := "tok-accept-" + instanceID
	lock, err := AcquireServiceLock(stateDir)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Release() })

	srv, err := NewServer(store, lock, ServerConfig{
		StateDir:                 stateDir,
		InstanceID:               instanceID,
		AuthToken:                authToken,
		WorkspaceBaseDir:         wsBase,
		OpenCodeBinaryPath:       "opencode",
		OpenCodeProbeScratchRoot: filepath.Join(dir, "probe-scratch"),
		OpenCodeProbeProfile:     probeProfile,
		OpenCodeIdleGrace:        200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("configured service construction: %v", err)
	}
	started := false
	t.Cleanup(func() {
		if started {
			_ = srv.Close()
		}
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	started = true
	bridge := &acceptanceBridge{t: t, client: newTestClient(srv.SocketPath()), token: authToken}

	// 1. Adopt: the controller lease secret is returned exactly once.
	code, resp := bridge.do("POST", "/v1/runs/run-acc/controller/adopt", `{
		"op_id": "op-adopt-acc", "harness": "opencode", "controller_ref": "conv-acc",
		"bootstrap_lease": "bootstrap-accept"}`)
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("adopt: %d %v", code, resp)
	}
	lease, _ := resp["lease_secret"].(string)
	if lease == "" {
		t.Fatalf("adopt must return the lease secret once, got %v", resp)
	}

	// 2. Bridge connect.
	code, resp = bridge.do("POST", "/v1/runs/run-acc/controller/connect", fmt.Sprintf(
		`{"op_id": "op-conn-acc-1", "controller_lease": %q, "expected_generation": 1, "instance_id": %q}`, lease, instanceID))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("connect: %d %v", code, resp)
	}

	// 3. Bridge queue (expected_version from durable state).
	hydrated0, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	ver0 := hydrated0.Sessions["sess-accept"].RowVersion
	code, resp = bridge.do("POST", "/v1/runs/run-acc/sessions/sess-accept/prompts/queue", fmt.Sprintf(
		`{"instance_id": %q, "op_id": "op-q-acc-1", "controller_lease": %q, "expected_version": %d,
		  "turn_key": "t-acc-1", "prompt": "acceptance prompt"}`, instanceID, lease, ver0))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("queue: %d %v", code, resp)
	}
	receipt := resp["receipt"].(map[string]any)
	verAfterQueue := int64(receipt["committed_version"].(float64))

	// 4. Bridge release: the supervisor worker executes through the
	// production adapter -> stub child.
	code, resp = bridge.do("POST", "/v1/runs/run-acc/sessions/sess-accept/turns/t-acc-1/release", fmt.Sprintf(
		`{"op_id": "op-rel-acc-1", "controller_lease": %q, "expected_version": %d}`, lease, verAfterQueue))
	if code != http.StatusAccepted {
		t.Fatalf("release: %d %v", code, resp)
	}

	// 5. Gated execution completes; the durable outcome is committed.
	srv.Coordinator().WaitWorkers()

	// 6. Bridge collect: the turn is terminal with the parentID-
	// correlated native output.
	code, resp = bridge.do("GET", "/v1/runs/run-acc/sessions/sess-accept/turns/t-acc-1", "")
	if code != http.StatusOK {
		t.Fatalf("collect: %d %v", code, resp)
	}
	if resp["status"] != string(council.TurnCompleted) {
		t.Fatalf("turn must be completed, got %v", resp["status"])
	}
	if result, _ := resp["result"].(string); !strings.Contains(result, "stub assistant response") {
		t.Fatalf("expected native correlated output, got %q", result)
	}

	// The native dispatch reached the stub in the session's workspace.
	ws, ok := srv.workspaceManager.GetPaths("run-acc", "sess-accept")
	if !ok {
		t.Fatal("workspace allocation missing")
	}
	if fails := readStubState(t, ws.Root, ".stub-authfail"); len(fails) != 0 {
		t.Fatalf("native traffic must authenticate: %v", fails)
	}

	// Native session persistence (the stub records created sessions in
	// its project directory); the acceptance flow addresses the child
	// session directly, so seed the session record the native server
	// would have persisted.
	if err := os.WriteFile(filepath.Join(ws.Root, ".stub-sessions"), []byte("sess-accept\n"), 0o600); err != nil {
		t.Fatalf("seed native session: %v", err)
	}

	// 7. Bridge disconnect.
	code, _ = bridge.do("POST", "/v1/runs/run-acc/controller/disconnect", fmt.Sprintf(
		`{"op_id": "op-disc-acc", "controller_lease": %q, "expected_generation": 1}`, lease))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("disconnect: %d", code)
	}

	// 8. Idle park: after the grace period with no in-flight work, the
	// serve child is parked.
	opAdp, ok := srv.adapter.(*opencode.OpenCodeAdapter)
	if !ok {
		t.Fatalf("service adapter must be the OpenCode adapter, got %T", srv.adapter)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if opAdp.IdleParkedSessions() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session must park after the idle grace")
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 9. Bridge reconnect.
	code, resp = bridge.do("POST", "/v1/runs/run-acc/controller/connect", fmt.Sprintf(
		`{"op_id": "op-conn-acc-2", "controller_lease": %q, "expected_generation": 1, "instance_id": %q}`, lease, instanceID))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("reconnect: %d %v", code, resp)
	}

	// 10. Bridge queue + release follow-up: the adapter resumes a
	// replacement server against the same workspace and verifies the
	// exact native session.
	hydrated, err := store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	verNow := hydrated.Sessions["sess-accept"].RowVersion
	code, resp = bridge.do("POST", "/v1/runs/run-acc/sessions/sess-accept/prompts/queue", fmt.Sprintf(
		`{"instance_id": %q, "op_id": "op-q-acc-2", "controller_lease": %q, "expected_version": %d,
		  "turn_key": "t-acc-2", "prompt": "follow-up prompt"}`, instanceID, lease, verNow))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("queue 2: %d %v", code, resp)
	}
	receipt2 := resp["receipt"].(map[string]any)
	verAfterQueue2 := int64(receipt2["committed_version"].(float64))

	code, resp = bridge.do("POST", "/v1/runs/run-acc/sessions/sess-accept/turns/t-acc-2/release", fmt.Sprintf(
		`{"op_id": "op-rel-acc-2", "controller_lease": %q, "expected_version": %d}`, lease, verAfterQueue2))
	if code != http.StatusAccepted {
		t.Fatalf("release 2: %d %v", code, resp)
	}

	// 11. Replacement execution + durable follow-up outcome.
	srv.Coordinator().WaitWorkers()

	code, resp = bridge.do("GET", "/v1/runs/run-acc/sessions/sess-accept/turns/t-acc-2", "")
	if code != http.StatusOK {
		t.Fatalf("collect 2: %d %v", code, resp)
	}
	if resp["status"] != string(council.TurnCompleted) {
		t.Fatalf("follow-up turn must be completed, got %v", resp["status"])
	}
	if result, _ := resp["result"].(string); !strings.Contains(result, "stub assistant response") {
		t.Fatalf("follow-up must reach the replacement server, got %q", result)
	}

	// 12. Durable evidence: both turns persisted with native results.
	hydrated, err = store.HydrateState(ctx)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	turns := hydrated.Sessions["sess-accept"].Turns
	if len(turns) != 2 {
		t.Fatalf("both turns must be durable, have %d", len(turns))
	}
	for _, key := range []string{"t-acc-1", "t-acc-2"} {
		turn, ok := turns[key]
		if !ok {
			t.Fatalf("turn %s missing from durable state", key)
		}
		if turn.Status != string(council.TurnCompleted) {
			t.Fatalf("turn %s must be durably completed, got %v", key, turn.Status)
		}
	}
	if !strings.Contains(bytes.NewBufferString(turns["t-acc-1"].Result).String(), "stub assistant response") {
		t.Fatalf("durable result must carry native output, got %q", turns["t-acc-1"].Result)
	}
}
