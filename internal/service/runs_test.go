//go:build !windows

// POSIX enforcement evidence only: service lifetime management (flock + unix domain sockets) is deliberately unsupported on native Windows.
package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/service"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestService_RunsEndpoint(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	workspaceBaseDir := t.TempDir()

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

	authToken := "test-auth-token-runs"
	cfg := service.ServerConfig{
		StateDir:         stateDir,
		InstanceID:       "inst-runs-1",
		AuthToken:        authToken,
		WorkspaceBaseDir: workspaceBaseDir,
	}

	srv, err := service.NewServer(store, lock, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	if srv.WorkspaceManager() == nil {
		t.Fatal("expected WorkspaceManager to be initialized")
	}
	if srv.PolicyExecutor() == nil {
		t.Fatal("expected PolicyExecutor to be initialized")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		srv.Handler().ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /v1/runs/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		srv.Handler().ServeHTTP(w, r)
	})

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	t.Run("unauthenticated_rejected", func(t *testing.T) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/runs", strings.NewReader(`{}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
	})

	t.Run("disallow_unknown_fields", func(t *testing.T) {
		body := `{"op_id":"op-1","unknown":"bad"}`
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/runs", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+authToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for unknown fields, got %d", resp.StatusCode)
		}
	})

	t.Run("create_run_success_and_get", func(t *testing.T) {
		prof := storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "none",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "unrestricted",
			NetworkAllowlist:    []string{},
			CodeIndexScope:      []string{},
			Tooling:             []string{"echo"},
			Harnesses:           map[string]storage.HarnessProfileSpec{},
		}

		createReq := service.CreateRunRequest{
			OpID:               "op-run-create-1",
			ControllerLease:    "lease-run-1",
			RunID:              "run-prod-1",
			Brief:              "Demonstrate production run creation with frozen profiles",
			SourceRepoIdentity: "none",
			SourceCommit:       "",
			SourceTree:         "",
			Profile:            prof,
		}

		bodyBytes, _ := json.Marshal(createReq)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/runs", bytes.NewReader(bodyBytes))
		req.Header.Set("Authorization", "Bearer "+authToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 Created, got %d", resp.StatusCode)
		}

		var createResp service.CreateRunResponse
		if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
			t.Fatalf("decode create response: %v", err)
		}

		if !strings.HasPrefix(createResp.BriefDigest, "cbrief-v1:sha256:") {
			t.Fatalf("expected cbrief-v1 prefix, got %s", createResp.BriefDigest)
		}
		if createResp.SourceDigest != "csource-v1:none" {
			t.Fatalf("expected csource-v1:none, got %s", createResp.SourceDigest)
		}
		if !strings.HasPrefix(createResp.ProfileDigest, "cprof-v1:sha256:") {
			t.Fatalf("expected cprof-v1 prefix, got %s", createResp.ProfileDigest)
		}

		// Query via GET /v1/runs/{run_id}
		getReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/runs/run-prod-1", nil)
		getReq.Header.Set("Authorization", "Bearer "+authToken)
		getResp, err := http.DefaultClient.Do(getReq)
		if err != nil {
			t.Fatalf("get request failed: %v", err)
		}
		defer getResp.Body.Close()

		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d", getResp.StatusCode)
		}

		var getRunResp service.GetRunResponse
		if err := json.NewDecoder(getResp.Body).Decode(&getRunResp); err != nil {
			t.Fatalf("decode get run response: %v", err)
		}

		if getRunResp.Run.RunID != "run-prod-1" {
			t.Fatalf("expected run-prod-1, got %s", getRunResp.Run.RunID)
		}
		if getRunResp.Run.ProfileDigest != createResp.ProfileDigest {
			t.Fatalf("expected profile digest %s, got %s", createResp.ProfileDigest, getRunResp.Run.ProfileDigest)
		}
		if getRunResp.Run.WorkspaceMode != "none" {
			t.Fatalf("expected workspace mode none, got %s", getRunResp.Run.WorkspaceMode)
		}
	})

	t.Run("get_nonexistent_run_returns_404", func(t *testing.T) {
		getReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/runs/run-does-not-exist", nil)
		getReq.Header.Set("Authorization", "Bearer "+authToken)
		getResp, err := http.DefaultClient.Do(getReq)
		if err != nil {
			t.Fatalf("get request failed: %v", err)
		}
		defer getResp.Body.Close()

		if getResp.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 Not Found, got %d", getResp.StatusCode)
		}
	})
}
