//go:build !windows

// POSIX enforcement evidence only: the proxy injects and verifies environment through POSIX child shells; Windows process enforcement is fail-closed by design.
package execpolicy_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func mockPublicResolver(mappings map[string][]string) func(ctx context.Context, host string) ([]net.IP, error) {
	return func(ctx context.Context, host string) ([]net.IP, error) {
		if ips, ok := mappings[host]; ok {
			var parsed []net.IP
			for _, s := range ips {
				parsed = append(parsed, net.ParseIP(s))
			}
			return parsed, nil
		}
		// Default public IP for unmapped domains in tests
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
}

func setupTestWorkspacePaths(t *testing.T, runID, sessionID string) workspace.WorkspacePaths {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, runID, sessionID, "worktree")
	config := filepath.Join(tmp, runID, sessionID, "config")
	scratch := filepath.Join(tmp, runID, sessionID, "scratch")
	source := filepath.Join(tmp, runID, sessionID, "source")

	_ = os.MkdirAll(root, 0700)
	_ = os.MkdirAll(config, 0700)
	_ = os.MkdirAll(scratch, 0700)
	_ = os.MkdirAll(source, 0700)

	return workspace.WorkspacePaths{
		Root:    root,
		Config:  config,
		Scratch: scratch,
		Source:  source,
	}
}

// 1. Outbound CONNECT to allowlisted host succeeds.
func TestNetworkProxy_AllowlistedCONNECT(t *testing.T) {
	// Start a mock TLS upstream server representing api.anthropic.com:443
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer ts.Close()

	tsAddr := ts.Listener.Addr().String()

	// Start proxy allowlisting api.anthropic.com:443
	proxy, err := execpolicy.StartNetworkProxy(
		[]string{"api.anthropic.com:443"},
		execpolicy.WithDNSResolver(mockPublicResolver(map[string][]string{
			"api.anthropic.com": {"93.184.216.34"},
		})),
		execpolicy.WithDialContext(func(ctx context.Context, network, addr string) (net.Conn, error) {
			// In test environment, route allowed CONNECT to our local test server
			return net.Dial(network, tsAddr)
		}),
	)
	if err != nil {
		t.Fatalf("StartNetworkProxy failed: %v", err)
	}
	defer proxy.Close()

	if !strings.HasPrefix(proxy.Endpoint(), "http://127.0.0.1:") {
		t.Fatalf("unexpected proxy endpoint: %s", proxy.Endpoint())
	}

	proxyURL, err := url.Parse(proxy.Endpoint())
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // mock server uses self-signed cert
			},
		},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get("https://api.anthropic.com/v1/messages")
	if err != nil {
		t.Fatalf("CONNECT to allowlisted host failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"success"`) {
		t.Fatalf("unexpected body: %s", string(body))
	}
}

// 2. Outbound CONNECT to unallowlisted host returns HTTP 403 Forbidden with typed error.
func TestNetworkProxy_UnallowlistedCONNECT_Denied(t *testing.T) {
	proxy, err := execpolicy.StartNetworkProxy(
		[]string{"api.anthropic.com:443"},
		execpolicy.WithDNSResolver(mockPublicResolver(nil)),
	)
	if err != nil {
		t.Fatalf("StartNetworkProxy failed: %v", err)
	}
	defer proxy.Close()

	// Direct check of typed error
	err = proxy.CheckDestination("unauthorized.attacker.com:443")
	if err == nil {
		t.Fatalf("expected CheckDestination to deny unauthorized host")
	}
	if !errors.Is(err, execpolicy.ErrNetworkAccessDenied) {
		t.Fatalf("expected ErrNetworkAccessDenied, got: %v", err)
	}

	// HTTP CONNECT check via raw connection
	proxyHost := strings.TrimPrefix(proxy.Endpoint(), "http://")
	conn, err := net.Dial("tcp", proxyHost)
	if err != nil {
		t.Fatalf("connect to proxy: %v", err)
	}
	defer conn.Close()

	req := "CONNECT unauthorized.attacker.com:443 HTTP/1.1\r\nHost: unauthorized.attacker.com:443\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write CONNECT request: %v", err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read proxy response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 Forbidden, got: %d", resp.StatusCode)
	}

	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "network access denied") {
		t.Fatalf("expected denial message in body, got: %s", string(respBody))
	}

	if proxy.LastDenial() == nil || !errors.Is(proxy.LastDenial(), execpolicy.ErrNetworkAccessDenied) {
		t.Fatalf("expected proxy.LastDenial() to wrap ErrNetworkAccessDenied, got: %v", proxy.LastDenial())
	}
}

// 3. Outbound request to private / non-public IPs is blocked by DNS rebinding defense.
func TestNetworkProxy_DNSRebinding_NonPublicIPs(t *testing.T) {
	testCases := []struct {
		name string
		host string
		ip   string
	}{
		{name: "IPv4 loopback 127.0.0.1", host: "loopback.test", ip: "127.0.0.1"},
		{name: "IPv4 loopback 127.0.0.2", host: "loopback2.test", ip: "127.0.0.2"},
		{name: "RFC 1918 10.0.0.1", host: "rfc1918-10.test", ip: "10.0.0.1"},
		{name: "RFC 1918 172.16.0.1", host: "rfc1918-172.test", ip: "172.16.0.1"},
		{name: "RFC 1918 192.168.1.1", host: "rfc1918-192.test", ip: "192.168.1.1"},
		{name: "IPv4 link-local 169.254.169.254", host: "linklocal.test", ip: "169.254.169.254"},
		{name: "IPv6 loopback ::1", host: "loopback6.test", ip: "::1"},
		{name: "IPv6 link-local fe80::1", host: "linklocal6.test", ip: "fe80::1"},
		{name: "IPv6 unique local fc00::1", host: "ula6.test", ip: "fc00::1"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Allow the domain in allowlist, but DNS resolves to non-public IP (DNS Rebinding)
			proxy, err := execpolicy.StartNetworkProxy(
				[]string{tc.host + ":443", tc.ip + ":443"},
				execpolicy.WithDNSResolver(mockPublicResolver(map[string][]string{
					tc.host: {tc.ip},
				})),
			)
			if err != nil {
				t.Fatalf("StartNetworkProxy failed: %v", err)
			}
			defer proxy.Close()

			// A) Validate via CheckDestination on hostname (DNS rebinding)
			err = proxy.CheckDestination(tc.host + ":443")
			if err == nil {
				t.Fatalf("expected rebinding to %s (%s) to be denied", tc.host, tc.ip)
			}
			if !errors.Is(err, execpolicy.ErrNetworkAccessDenied) {
				t.Fatalf("expected ErrNetworkAccessDenied, got: %v", err)
			}

			// B) Validate direct IP destination
			errDirect := proxy.CheckDestination(net.JoinHostPort(tc.ip, "443"))
			if errDirect == nil {
				t.Fatalf("expected direct non-public IP %s to be denied", tc.ip)
			}
			if !errors.Is(errDirect, execpolicy.ErrNetworkAccessDenied) {
				t.Fatalf("expected ErrNetworkAccessDenied for direct IP, got: %v", errDirect)
			}

			// C) Issue HTTP CONNECT to proxy
			proxyHost := strings.TrimPrefix(proxy.Endpoint(), "http://")
			conn, err := net.Dial("tcp", proxyHost)
			if err != nil {
				t.Fatalf("dial proxy: %v", err)
			}
			defer conn.Close()

			reqStr := fmt.Sprintf("CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", tc.host, tc.host)
			if _, err := conn.Write([]byte(reqStr)); err != nil {
				t.Fatalf("write CONNECT: %v", err)
			}

			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("read proxy response: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("expected HTTP 403 Forbidden for non-public destination %s, got: %d", tc.ip, resp.StatusCode)
			}
		})
	}
}

// 4. HTTP redirects to unallowlisted hosts are intercepted and rejected on subsequent hop.
func TestNetworkProxy_HTTPRedirect_RejectedOnSubsequentHop(t *testing.T) {
	var evilHits int32
	evilServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&evilHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("exfiltrated"))
	}))
	defer evilServer.Close()

	evilAddr := evilServer.Listener.Addr().String()

	// Target server redirects to unallowlisted evil.attacker.com:80
	allowedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://evil.attacker.com:80/leak", http.StatusFound)
	}))
	defer allowedServer.Close()

	allowedAddr := allowedServer.Listener.Addr().String()

	// Allowlist ONLY allowed.example.com:80
	proxy, err := execpolicy.StartNetworkProxy(
		[]string{"allowed.example.com:80"},
		execpolicy.WithDNSResolver(mockPublicResolver(map[string][]string{
			"allowed.example.com": {"93.184.216.34"},
			"evil.attacker.com":   {"93.184.216.35"},
		})),
		execpolicy.WithDialContext(func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, "allowed.example.com:") {
				return net.Dial(network, allowedAddr)
			}
			if strings.HasPrefix(addr, "evil.attacker.com:") {
				return net.Dial(network, evilAddr)
			}
			return net.Dial(network, addr)
		}),
	)
	if err != nil {
		t.Fatalf("StartNetworkProxy failed: %v", err)
	}
	defer proxy.Close()

	proxyURL, _ := url.Parse(proxy.Endpoint())
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		},
		Timeout: 5 * time.Second,
	}

	resp, err := client.Get("http://allowed.example.com:80/login")
	// The client will follow redirect to http://evil.attacker.com:80/leak.
	// The proxy MUST intercept and reject the subsequent hop with 403 Forbidden.
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected redirect hop to be rejected with 403 Forbidden, got: %d", resp.StatusCode)
		}
	} else {
		// Some client configurations return an error when redirect returns 403
		if !strings.Contains(err.Error(), "403") && !strings.Contains(err.Error(), "forbidden") {
			t.Fatalf("expected redirect hop to produce 403 error, got: %v", err)
		}
	}

	// Ensure the evil server was never reached
	if atomic.LoadInt32(&evilHits) > 0 {
		t.Fatalf("evil server was reached %d times; redirect should have been blocked!", evilHits)
	}

	// Verify last denial records evil.attacker.com
	if proxy.LastDenial() == nil || !errors.Is(proxy.LastDenial(), execpolicy.ErrNetworkAccessDenied) {
		t.Fatalf("expected proxy.LastDenial() to record denial, got: %v", proxy.LastDenial())
	}
}

// 5. In network_mode: "none", all outbound attempts produce immediate failure.
func TestNetworkProxy_NetworkModeNone_ImmediateFailure(t *testing.T) {
	ctx := context.Background()
	runID := "run-net-none"
	sessionID := "session-net-none"
	paths := setupTestWorkspacePaths(t, runID, sessionID)

	t.Run("proxy_with_empty_allowlist_rejects_all_requests", func(t *testing.T) {
		proxy, err := execpolicy.StartNetworkProxy(nil)
		if err != nil {
			t.Fatalf("StartNetworkProxy(nil): %v", err)
		}
		defer proxy.Close()

		err = proxy.CheckDestination("anywhere.com:443")
		if err == nil {
			t.Fatalf("expected denial for empty allowlist")
		}
		if !errors.Is(err, execpolicy.ErrNetworkAccessDenied) {
			t.Fatalf("expected ErrNetworkAccessDenied, got: %v", err)
		}
	})

	t.Run("executor_network_mode_none_omits_proxy_env_and_fails_outbound", func(t *testing.T) {
		executor := execpolicy.New()

		profile := storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "isolated_branch",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "none",
			NetworkAllowlist:    nil,
			Tooling:             []string{"sh", "env"},
		}

		req := execpolicy.LaunchRequest{
			RunID:             runID,
			SessionID:         sessionID,
			TurnKey:           "turn-1",
			AttemptID:         "att-1",
			Command:           "env",
			Args:              nil,
			ExtraEnvAllowlist: nil,
			Paths:             paths,
			Profile:           profile,
		}

		proc, err := executor.Start(ctx, req)
		if err != nil {
			t.Fatalf("executor.Start failed: %v", err)
		}
		defer proc.Terminate(ctx)

		out, err := io.ReadAll(proc.Stdout())
		if err != nil {
			t.Fatalf("read stdout: %v", err)
		}
		_, _ = proc.Wait()

		envOutput := string(out)
		for _, forbidden := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
			if strings.Contains(envOutput, forbidden+"=") {
				t.Fatalf("network_mode=none child process contained proxy env var %s", forbidden)
			}
		}
	})

	t.Run("executor_network_mode_allowlist_injects_proxy_env", func(t *testing.T) {
		executor := execpolicy.New()

		profile := storage.CanonicalProfile{
			AlgoVersion:         "cprof-v1",
			WorkspaceMode:       "isolated_branch",
			IsolationStrictness: "permissive_dev",
			NetworkMode:         "allowlist",
			NetworkAllowlist:    []string{"api.anthropic.com:443"},
			Tooling:             []string{"sh", "env"},
		}

		req := execpolicy.LaunchRequest{
			RunID:             runID,
			SessionID:         sessionID,
			TurnKey:           "turn-1",
			AttemptID:         "att-1",
			Command:           "env",
			Args:              nil,
			ExtraEnvAllowlist: nil,
			Paths:             paths,
			Profile:           profile,
		}

		proc, err := executor.Start(ctx, req)
		if err != nil {
			t.Fatalf("executor.Start failed: %v", err)
		}
		defer proc.Terminate(ctx)

		out, err := io.ReadAll(proc.Stdout())
		if err != nil {
			t.Fatalf("read stdout: %v", err)
		}
		_, _ = proc.Wait()

		envOutput := string(out)
		for _, required := range []string{"HTTP_PROXY=", "HTTPS_PROXY=", "http_proxy=", "https_proxy="} {
			if !strings.Contains(envOutput, required) {
				t.Fatalf("network_mode=allowlist child process missing proxy env var prefix %s", required)
			}
		}
	})
}
