package client

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
	"path/filepath"
	"strings"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/service"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Client interacts with an active local Council service instance.
type Client struct {
	stateDir   string
	meta       service.DiscoveryMeta
	token      string
	httpClient *http.Client
}

// New initializes a Client by reading discovery metadata and credentials from stateDir.
func New(stateDir string) (*Client, error) {
	if stateDir == "" {
		return nil, errors.New("stateDir cannot be empty")
	}

	metaPath := filepath.Join(stateDir, "service.json")
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, fmt.Errorf("read service.json: %w", err)
	}

	var meta service.DiscoveryMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("unmarshal service.json: %w", err)
	}

	tokenPath := filepath.Join(stateDir, "auth.token")
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read auth.token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))

	endpoint := meta.Endpoint
	if endpoint == "" {
		endpoint = filepath.Join(stateDir, "council.sock")
	}

	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", endpoint)
		},
		DisableKeepAlives: true,
	}

	return &Client{
		stateDir: stateDir,
		meta:     meta,
		token:    token,
		httpClient: &http.Client{
			Transport: tr,
			Timeout:   30 * time.Second,
		},
	}, nil
}

// Meta returns the discovery metadata of the connected service instance.
func (c *Client) Meta() service.DiscoveryMeta {
	return c.meta
}

// Token returns the authentication bearer token for the service.
func (c *Client) Token() string {
	return c.token
}

// HTTPClient returns the underlying http.Client configured for the service transport.
func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

func (c *Client) do(ctx context.Context, method, path string, body any, dst any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, bodyReader)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Close = true
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}

	respBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	c.httpClient.CloseIdleConnections()
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errEnv service.ErrorEnvelope
		if err := json.Unmarshal(respBytes, &errEnv); err == nil && errEnv.Error.Code != "" {
			return fmt.Errorf("[%s] %s (status %d)", errEnv.Error.Code, errEnv.Error.Message, resp.StatusCode)
		}
		return fmt.Errorf("server error: status %d, body: %s", resp.StatusCode, string(respBytes))
	}

	if dst != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, dst); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
	}
	return nil
}

// GetReadiness queries GET /v1/readiness.
func (c *Client) GetReadiness(ctx context.Context) (*service.ReadinessResponse, error) {
	var resp service.ReadinessResponse
	if err := c.do(ctx, http.MethodGet, "/v1/readiness", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetStatus queries GET /v1/status.
func (c *Client) GetStatus(ctx context.Context) (*service.StatusResponse, error) {
	var resp service.StatusResponse
	if err := c.do(ctx, http.MethodGet, "/v1/status", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StopService calls POST /v1/service/stop.
func (c *Client) StopService(ctx context.Context, instanceID string, drain bool) (*service.StopResponse, error) {
	req := service.StopRequest{
		InstanceID: instanceID,
		Drain:      drain,
	}
	var resp service.StopResponse
	if err := c.do(ctx, http.MethodPost, "/v1/service/stop", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReleaseTurn calls POST /v1/runs/run-1-style release for a session turn.
// The session and turn identify the target; runID correlates the session.
func (c *Client) ReleaseTurn(ctx context.Context, runID, sessionID, turnKey, opID, controllerLease string, expectedVersion int64) (*service.ReleaseResponse, error) {
	req := service.ReleaseRequest{
		OpID:            opID,
		ControllerLease: controllerLease,
		ExpectedVersion: expectedVersion,
	}
	var resp service.ReleaseResponse
	path := fmt.Sprintf("/v1/runs/%s/sessions/%s/turns/%s/release", runID, sessionID, turnKey)
	if err := c.do(ctx, http.MethodPost, path, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetTurnDetails retrieves the authoritative turn state.
func (c *Client) GetTurnDetails(ctx context.Context, runID, sessionID, turnKey string) (*storage.TurnDetails, error) {
	var resp storage.TurnDetails
	path := fmt.Sprintf("/v1/runs/%s/sessions/%s/turns/%s", runID, sessionID, turnKey)
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReconcileTurn requests reconciliation of an uncertain turn.
func (c *Client) ReconcileTurn(ctx context.Context, runID, sessionID, turnKey, opID, controllerLease string) (*service.ReconcileResponse, error) {
	req := service.ReconcileRequest{
		OpID:            opID,
		ControllerLease: controllerLease,
	}
	var resp service.ReconcileResponse
	path := fmt.Sprintf("/v1/runs/%s/sessions/%s/turns/%s/reconcile", runID, sessionID, turnKey)
	if err := c.do(ctx, http.MethodPost, path, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
