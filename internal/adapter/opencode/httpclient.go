package opencode

// Centralized typed HTTP client for the OpenCode headless server. Every
// request the adapter or server manager makes to a launched serve child —
// health, model inventory, session lifecycle, messages, prompt dispatch,
// abort, per-session SSE events, and permission handling — goes through
// this client so authentication and transport classification cannot
// diverge between call sites.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ErrPostWrite marks a transport failure that occurred after request body
// transmission began: the server may have received part or all of the
// turn, so the outcome is ambiguous (unknown), never rejected.
type ErrPostWrite struct{ Cause error }

func (e *ErrPostWrite) Error() string { return e.Cause.Error() }
func (e *ErrPostWrite) Unwrap() error { return e.Cause }

// IsPostWriteError reports whether err is a classified post-write
// transport failure.
func IsPostWriteError(err error) bool {
	var pw *ErrPostWrite
	return errors.As(err, &pw)
}

// transmissionBody wraps a request body and records whether any byte of
// it was handed to the transport. The first successful read marks
// transmission as begun: from that point a transport failure is
// ambiguous, even if only a prefix of the body reached the server.
type transmissionBody struct {
	reader io.Reader
	began  *atomic.Bool
}

func newTransmissionBody(r io.Reader) *transmissionBody {
	b := &atomic.Bool{}
	return &transmissionBody{reader: r, began: b}
}

func (t *transmissionBody) Read(p []byte) (int, error) {
	n, err := t.reader.Read(p)
	if n > 0 {
		t.began.Store(true)
	}
	return n, err
}

// NativeClient is an authenticated typed client for one serve child.
// Credentials are the generated transport credentials retained from the
// child's GeneratedServerEnv; they are applied to every request.
type NativeClient struct {
	endpoint string
	username string
	password string
	hc       *http.Client
}

func newNativeClient(endpoint, username, password string) *NativeClient {
	return &NativeClient{
		endpoint: endpoint,
		username: username,
		password: password,
		hc:       &http.Client{Timeout: 30 * time.Second},
	}
}

// do performs an authenticated request and classifies transport failures:
// any failure before the first request-body byte is transmitted (dial
// refused, connect reset) is returned as a plain error (rejection); a
// failure after body transmission began — including a partial body write
// — is returned as *ErrPostWrite (ambiguous).
func (c *NativeClient) do(ctx context.Context, method, path, contentType string, payload []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.SetBasicAuth(c.username, c.password)

	var tb *transmissionBody
	if payload != nil {
		tb = newTransmissionBody(bytes.NewReader(payload))
		req.Body = io.NopCloser(tb)
		req.ContentLength = int64(len(payload))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(payload)), nil
		}
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		if tb != nil && tb.began.Load() {
			return nil, &ErrPostWrite{Cause: err}
		}
		return nil, err
	}
	return resp, nil
}

func decodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// ── Native payload types ────────────────────────────────────────────────

// NativeMessagePart is one content part of a native message.
type NativeMessagePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// NativeMessageError is a provider-level error reported on a message.
type NativeMessageError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// NativeMessage mirrors the native message envelope returned by the
// message endpoints.
type NativeMessage struct {
	ID       string              `json:"id"`
	Role     string              `json:"role"`
	ParentID string              `json:"parentID,omitempty"`
	Parts    []NativeMessagePart `json:"parts"`
	Error    *NativeMessageError `json:"error,omitempty"`
}

// NativePermission is one pending permission request on a session.
type NativePermission struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Title   string `json:"title,omitempty"`
	Status  string `json:"status"`
	Pattern string `json:"pattern,omitempty"`
}

// NativePermissionReply answers a permission request. The council denies
// permission requests by default; it never auto-approves.
type NativePermissionReply struct {
	Response string `json:"response"`
}

// ── Typed endpoints ─────────────────────────────────────────────────────

// Health verifies the child is serving (authenticated).
func (c *NativeClient) Health(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/api/health", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health returned %d", resp.StatusCode)
	}
	return nil
}

// Models lists the provider/model inventory reported by the child.
func (c *NativeClient) Models(ctx context.Context) ([]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/api/model", "", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
		} `json:"data"`
	}
	if err := decodeJSON(resp, &payload); err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	models := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		models = append(models, m.ProviderID+"/"+m.ID)
	}
	return models, nil
}

// CreateSession creates a native session; the returned ID is assigned by
// the server, never synthesized by the adapter. The directory context is
// sent verbatim: a server serving a different working directory rejects
// the request.
func (c *NativeClient) CreateSession(ctx context.Context, title, directory string) (string, error) {
	payload, err := json.Marshal(map[string]any{"title": title, "directory": directory})
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, http.MethodPost, "/session", "application/json", payload)
	if err != nil {
		return "", err
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(resp, &created); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	if created.ID == "" {
		return "", errors.New("create session returned no native session ID")
	}
	return created.ID, nil
}

// GetSession verifies an exact native session exists. A verified 404
// returns false without error; any other status is an error.
func (c *NativeClient) GetSession(ctx context.Context, sessionID string) (bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/session/"+sessionID, "", nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("session lookup returned %d", resp.StatusCode)
	}
}

// PromptAsync submits one turn with the deterministic native message ID.
func (c *NativeClient) PromptAsync(ctx context.Context, sessionID, messageID, text string) error {
	payload, err := json.Marshal(map[string]any{
		"messageID": messageID,
		"parts":     []map[string]string{{"type": "text", "text": text}},
	})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost,
		"/session/"+sessionID+"/prompt_async", "application/json", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("prompt_async returned %d", resp.StatusCode)
	}
	return nil
}

// Abort requests cancellation of the session's active turn.
func (c *NativeClient) Abort(ctx context.Context, sessionID string) error {
	resp, err := c.do(ctx, http.MethodPost, "/session/"+sessionID+"/abort", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("abort returned %d", resp.StatusCode)
	}
	return nil
}

// nativeMessageEnvelope is the wire shape of a message entry: metadata in
// `info`, content parts beside it.
type nativeMessageEnvelope struct {
	Info  NativeMessage       `json:"info"`
	Parts []NativeMessagePart `json:"parts"`
}

func (e nativeMessageEnvelope) flatten() NativeMessage {
	m := e.Info
	m.Parts = e.Parts
	return m
}

// ListMessages returns all native messages for a session.
func (c *NativeClient) ListMessages(ctx context.Context, sessionID string) ([]NativeMessage, error) {
	resp, err := c.do(ctx, http.MethodGet, "/session/"+sessionID+"/message", "", nil)
	if err != nil {
		return nil, err
	}
	var envelopes []nativeMessageEnvelope
	if err := decodeJSON(resp, &envelopes); err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}
	msgs := make([]NativeMessage, 0, len(envelopes))
	for _, e := range envelopes {
		msgs = append(msgs, e.flatten())
	}
	return msgs, nil
}

// GetMessage checks one exact message ID: the message on 200, nil on a
// verified 404, error otherwise.
func (c *NativeClient) GetMessage(ctx context.Context, sessionID, messageID string) (*NativeMessage, bool, error) {
	resp, err := c.do(ctx, http.MethodGet,
		"/session/"+sessionID+"/message/"+messageID, "", nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var e nativeMessageEnvelope
		if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
			return nil, false, fmt.Errorf("decode message: %w", err)
		}
		m := e.flatten()
		return &m, true, nil
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("message lookup returned %d", resp.StatusCode)
	}
}

// Events opens the native per-session SSE stream at the approved
// endpoint GET /session/{id}/event. The caller must close the response
// body. Connection-refused is a pre-write error; a failure after body
// transmission classifies as post-write.
func (c *NativeClient) Events(ctx context.Context, sessionID string) (*bufio.Scanner, *http.Response, error) {
	resp, err := c.do(ctx, http.MethodGet, "/session/"+sessionID+"/event", "", nil)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("events returned %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	return sc, resp, nil
}

// ScanNativeEvent returns the next data payload from an SSE scanner.
func ScanNativeEvent(sc *bufio.Scanner) (string, bool) {
	for sc.Scan() {
		line := sc.Text()
		if payload, ok := strings.CutPrefix(line, "data: "); ok {
			return payload, true
		}
	}
	return "", false
}

// PermissionRequests lists pending permission requests for a session.
func (c *NativeClient) PermissionRequests(ctx context.Context, sessionID string) ([]NativePermission, error) {
	resp, err := c.do(ctx, http.MethodGet, "/session/"+sessionID+"/permission", "", nil)
	if err != nil {
		return nil, err
	}
	var perms []NativePermission
	if err := decodeJSON(resp, &perms); err != nil {
		return nil, fmt.Errorf("permission requests: %w", err)
	}
	return perms, nil
}

// PermissionReply answers one permission request. Council denies by
// default; approval is an explicit operator-controlled decision.
func (c *NativeClient) PermissionReply(ctx context.Context, sessionID, permissionID string, reply NativePermissionReply) error {
	payload, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPost,
		"/session/"+sessionID+"/permission/"+permissionID+"/reply",
		"application/json", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("permission reply returned %d", resp.StatusCode)
	}
	return nil
}
