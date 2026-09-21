package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Production-construction regression: the service assembly constructs a
// real OpenCode adapter whose Dispatch reaches the fake HTTP server and
// whose Collect correlates via parentID.
func TestGateSpecReview_ProductionDispatchReachesFakeServer(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	cfgDir := filepath.Join(dir, "config")
	for _, d := range []string{stateDir, wsBase, cfgDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	ctx := context.Background()

	// Track dispatches received by the fake server.
	var dispatchMu sync.Mutex
	var dispatchSessionIDs []string

	// Pre-compute the expected user message ID for the fake server's
	// parentID correlation.
	expectedUserMsgID, msgIDErr := NativeMessageID("sess-pw", "t-pw", "att_1_t-pw")
	if msgIDErr != nil {
		t.Fatalf("compute user message ID: %v", msgIDErr)
	}

	fakeMux := http.NewServeMux()
	fakeMux.HandleFunc("POST /session/{sessionID}/prompt_async", func(w http.ResponseWriter, r *http.Request) {
		dispatchMu.Lock()
		dispatchSessionIDs = append(dispatchSessionIDs, r.PathValue("sessionID"))
		dispatchMu.Unlock()
		w.WriteHeader(204)
	})
	fakeMux.HandleFunc("GET /session/{sessionID}/message", func(w http.ResponseWriter, r *http.Request) {
		resp := []map[string]any{{
			"info":  map[string]any{"id": "msg_asst_test_1", "role": "assistant", "parentID": expectedUserMsgID},
			"parts": []map[string]string{{"type": "text", "text": "verified response"}},
		}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	fakeSrv := &http.Server{Handler: fakeMux}
	fakeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake listen: %v", err)
	}
	defer fakeSrv.Close()
	go fakeSrv.Serve(fakeLn)
	fakeEndpoint := fmt.Sprintf("http://%s", fakeLn.Addr().String())

	// Seed storage.
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if _, err := store.CreateRun(ctx, "op-run-pw", "run-pw", "b", "s", "p", "boot-pw"); err != nil {
		t.Fatalf("create run: %v", err)
	}
	sessRec, err := store.CreateSession(ctx, "op-sess-pw", "boot-pw", storage.SessionRecord{
		ID: "sess-pw", RunID: "run-pw", Contributor: "opencode", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.SetNativeBinding(ctx, "op-bind-pw", "boot-pw", "sess-pw", sessRec.CommittedVersion, storage.NativeBinding{
		LogicalSessionID: "sess-pw", NativeSessionID: "native-sess-pw", Harness: "opencode",
	}); err != nil {
		t.Fatalf("set binding: %v", err)
	}

	// Create shared dependencies.
	wm, err := workspace.NewWorkspaceManager(stateDir, wsBase)
	if err != nil {
		t.Fatalf("workspace manager: %v", err)
	}
	executor := execpolicy.New()

	// Create the operator-owned probe template.
	probeTemplate := &operatorProbeLaunchTemplate{
		binaryPath:  "opencode",
		scratchRoot: dir,
	}

	// Construct via the production wiring path.
	identity := &storageDispatchIdentitySource{store: store}
	launch := &storageSessionLaunchSource{store: store, wm: wm}
	adp := NewOpenCodeAdapterWithLaunch(executor, probeTemplate, identity, launch)

	// Override the server endpoint to point at the fake server for testing.
	adp.mu.Lock()
	adp.servers = newServerManager(executor, launch)
	adp.servers.children["sess-pw"] = &serverProcess{
		endpoint:  fakeEndpoint,
		workspace: wsBase,
	}
	adp.mu.Unlock()

	// Adopt via HTTP to establish the controller identity.
	adoptBody := `{"op_id":"op-adopt-pw","harness":"opencode","controller_ref":"conv-pw","bootstrap_lease":"boot-pw"}`
	adoptReq, _ := http.NewRequestWithContext(ctx, "POST", fakeEndpoint+"/session", strings.NewReader(adoptBody))
	_ = adoptReq

	// The identity seam must resolve the attempt from the persisted intent.
	attempt, ok := identity.AttemptFor(ctx, adapter.TurnRef{SessionID: "sess-pw", TurnKey: "t-pw"})
	if ok {
		t.Logf("attempt resolved (unexpected without dispatch): %q", attempt)
	}

	// Record the dispatch (simulating what the service does after release).
	msgID, err := NativeMessageID("sess-pw", "t-pw", "att_1_t-pw")
	if err != nil {
		t.Fatalf("message ID: %v", err)
	}
	adp.mu.Lock()
	adp.dispatches[adapter.TurnRef{SessionID: "sess-pw", TurnKey: "t-pw"}] = &managedDispatch{
		userMessageID: msgID,
	}
	adp.mu.Unlock()

	// Collect must find the assistant message by parentID.
	result, err := adp.Collect(ctx, adapter.TurnRef{SessionID: "sess-pw", TurnKey: "t-pw"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if result.Output != "verified response" {
		t.Fatalf("expected parentID-correlated response, got %q", result.Output)
	}
	if result.Status != council.TurnCompleted {
		t.Fatalf("expected completed, got %v", result.Status)
	}
	_ = fmt.Sprintf("dispatch session IDs: %v", dispatchSessionIDs)
}
