package execpolicy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNetworkAccessDenied is returned when a destination is unallowlisted or non-public.
	ErrNetworkAccessDenied = errors.New("network access denied")
)

var nonPublicCIDRs []*net.IPNet

func init() {
	cidrStrings := []string{
		"127.0.0.0/8",    // IPv4 loopback
		"10.0.0.0/8",     // IPv4 private RFC 1918
		"172.16.0.0/12",  // IPv4 private RFC 1918
		"192.168.0.0/16", // IPv4 private RFC 1918
		"169.254.0.0/16", // IPv4 link-local
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 ULA private
		"fe80::/10",      // IPv6 link-local
		"0.0.0.0/8",      // IPv4 unspecified/current network
		"::/128",         // IPv6 unspecified
	}
	for _, s := range cidrStrings {
		_, block, err := net.ParseCIDR(s)
		if err == nil {
			nonPublicCIDRs = append(nonPublicCIDRs, block)
		}
	}
}

func isNonPublicIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, block := range nonPublicCIDRs {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

type allowlistRule struct {
	host       string // lowercase
	port       string // optional
	isWildcard bool
	wildSuffix string // e.g. ".domain.com"
}

// NetworkProxy is a local forward proxy enforcing host allowlists and DNS rebinding defense.
type NetworkProxy struct {
	listener     net.Listener
	server       *http.Server
	transport    *http.Transport
	dialer       *net.Dialer
	rawAllowlist []string
	rules        []allowlistRule

	resolver    func(ctx context.Context, host string) ([]net.IP, error)
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	mu      sync.RWMutex
	denials []error
}

// ProxyOption configures a NetworkProxy.
type ProxyOption func(*NetworkProxy)

// WithDNSResolver configures a custom DNS resolver for testing or hermetic lookups.
func WithDNSResolver(fn func(ctx context.Context, host string) ([]net.IP, error)) ProxyOption {
	return func(p *NetworkProxy) {
		p.resolver = fn
	}
}

// WithDialContext configures a custom dialer for testing or routing.
func WithDialContext(fn func(ctx context.Context, network, addr string) (net.Conn, error)) ProxyOption {
	return func(p *NetworkProxy) {
		p.dialContext = fn
	}
}

func parseHostPort(raw string, defaultPort string) (host string, port string, err error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", err
		}
		host = u.Hostname()
		port = u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = defaultPort
			}
		}
		return strings.ToLower(host), port, nil
	}

	h, p, err := net.SplitHostPort(raw)
	if err == nil {
		h = strings.TrimPrefix(h, "[")
		h = strings.TrimSuffix(h, "]")
		return strings.ToLower(h), p, nil
	}

	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	return strings.ToLower(raw), defaultPort, nil
}

func (p *NetworkProxy) initAllowlist(allowlist []string) {
	for _, entry := range allowlist {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		host, port, _ := parseHostPort(entry, "")
		rule := allowlistRule{
			host: host,
			port: port,
		}
		if strings.HasPrefix(host, "*.") {
			rule.isWildcard = true
			rule.wildSuffix = host[1:] // ".domain.com"
		}
		p.rules = append(p.rules, rule)
	}
}

func (p *NetworkProxy) matchesAllowlist(destHost, destPort string) bool {
	destHost = strings.ToLower(destHost)
	for _, rule := range p.rules {
		if rule.port != "" && rule.port != destPort {
			continue
		}
		if rule.isWildcard {
			if strings.HasSuffix(destHost, rule.wildSuffix) {
				return true
			}
		} else if rule.host == destHost {
			return true
		}
	}
	return false
}

func (p *NetworkProxy) checkDestination(ctx context.Context, rawDest string, defaultPort string) error {
	destHost, destPort, err := parseHostPort(rawDest, defaultPort)
	if err != nil {
		return fmt.Errorf("%w: invalid destination %q: %v", ErrNetworkAccessDenied, rawDest, err)
	}

	// 1. Validate allowlist membership
	if !p.matchesAllowlist(destHost, destPort) {
		return fmt.Errorf("%w: destination %s:%s is not allowlisted", ErrNetworkAccessDenied, destHost, destPort)
	}

	// 2. Validate IP / DNS rebinding defense
	if directIP := net.ParseIP(destHost); directIP != nil {
		if isNonPublicIP(directIP) {
			return fmt.Errorf("%w: direct IP destination %s is non-public", ErrNetworkAccessDenied, directIP.String())
		}
		return nil
	}

	// Hostname resolution
	ips, err := p.resolver(ctx, destHost)
	if err != nil {
		return fmt.Errorf("%w: cannot resolve destination host %q: %v", ErrNetworkAccessDenied, destHost, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: no IP addresses found for destination host %q", ErrNetworkAccessDenied, destHost)
	}

	for _, ip := range ips {
		if isNonPublicIP(ip) {
			return fmt.Errorf("%w: destination host %q resolved to non-public IP %s (DNS rebinding defense)", ErrNetworkAccessDenied, destHost, ip.String())
		}
	}

	return nil
}

// CheckDestination verifies whether a destination host:port is permitted by the allowlist and public IP checks.
func (p *NetworkProxy) CheckDestination(dest string) error {
	err := p.checkDestination(context.Background(), dest, "443")
	if err != nil {
		p.recordDenial(err)
	}
	return err
}

func (p *NetworkProxy) recordDenial(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.denials = append(p.denials, err)
}

// LastDenial returns the most recent network access denial error, or nil if none occurred.
func (p *NetworkProxy) LastDenial() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.denials) == 0 {
		return nil
	}
	return p.denials[len(p.denials)-1]
}

// Denials returns a copy of all recorded network access denial errors.
func (p *NetworkProxy) Denials() []error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	res := make([]error, len(p.denials))
	copy(res, p.denials)
	return res
}

// Endpoint returns the HTTP endpoint URL of the proxy listener.
func (p *NetworkProxy) Endpoint() string {
	if p.listener == nil {
		return ""
	}
	return "http://" + p.listener.Addr().String()
}

// Close gracefully terminates the proxy server and listener.
func (p *NetworkProxy) Close() error {
	var errs []error
	if p.server != nil {
		if err := p.server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	if p.listener != nil {
		if err := p.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// StartNetworkProxy starts a local forward proxy on an ephemeral port enforcing allowlist rules.
func StartNetworkProxy(allowlist []string, opts ...ProxyOption) (*NetworkProxy, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start proxy listener: %w", err)
	}

	p := &NetworkProxy{
		listener:     listener,
		rawAllowlist: append([]string(nil), allowlist...),
		dialer:       &net.Dialer{Timeout: 10 * time.Second},
	}
	p.initAllowlist(allowlist)
	p.resolver = func(ctx context.Context, host string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", host)
	}

	for _, opt := range opts {
		opt(p)
	}

	p.transport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if p.dialContext != nil {
				return p.dialContext(ctx, network, addr)
			}
			host, port, err := parseHostPort(addr, "80")
			if err != nil {
				return nil, err
			}
			if directIP := net.ParseIP(host); directIP != nil {
				if isNonPublicIP(directIP) {
					return nil, fmt.Errorf("%w: non-public IP %s", ErrNetworkAccessDenied, directIP)
				}
				return p.dialer.DialContext(ctx, network, net.JoinHostPort(directIP.String(), port))
			}
			ips, err := p.resolver(ctx, host)
			if err != nil || len(ips) == 0 {
				return nil, fmt.Errorf("resolve %s: %w", host, err)
			}
			for _, ip := range ips {
				if isNonPublicIP(ip) {
					return nil, fmt.Errorf("%w: destination host %s resolved to non-public IP %s", ErrNetworkAccessDenied, host, ip)
				}
			}
			return p.dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
		},
	}

	p.server = &http.Server{
		Handler: p,
	}

	go func() {
		_ = p.server.Serve(listener)
	}()

	return p, nil
}

func (p *NetworkProxy) dialUpstream(ctx context.Context, rawDest string, defaultPort string) (net.Conn, error) {
	if p.dialContext != nil {
		return p.dialContext(ctx, "tcp", rawDest)
	}
	host, port, err := parseHostPort(rawDest, defaultPort)
	if err != nil {
		return nil, err
	}
	if directIP := net.ParseIP(host); directIP != nil {
		if isNonPublicIP(directIP) {
			return nil, fmt.Errorf("%w: non-public IP %s", ErrNetworkAccessDenied, directIP)
		}
		return p.dialer.DialContext(ctx, "tcp", net.JoinHostPort(directIP.String(), port))
	}
	ips, err := p.resolver(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if isNonPublicIP(ip) {
			return nil, fmt.Errorf("%w: non-public IP %s", ErrNetworkAccessDenied, ip)
		}
	}
	return p.dialer.DialContext(ctx, "tcp", net.JoinHostPort(ips[0].String(), port))
}

func (p *NetworkProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodConnect {
		target := req.RequestURI
		if target == "" {
			target = req.Host
		}
		if err := p.checkDestination(req.Context(), target, "443"); err != nil {
			p.recordDenial(err)
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}

		destConn, err := p.dialUpstream(req.Context(), target, "443")
		if err != nil {
			http.Error(w, fmt.Sprintf("proxy connect failed: %v", err), http.StatusBadGateway)
			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			destConn.Close()
			http.Error(w, "hijacking not supported", http.StatusInternalServerError)
			return
		}

		clientConn, buf, err := hijacker.Hijack()
		if err != nil {
			destConn.Close()
			return
		}

		if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
			destConn.Close()
			clientConn.Close()
			return
		}

		if buf != nil && buf.Reader.Buffered() > 0 {
			buffered, _ := buf.Reader.Peek(buf.Reader.Buffered())
			if len(buffered) > 0 {
				_, _ = destConn.Write(buffered)
			}
		}

		go func() {
			_, _ = io.Copy(destConn, clientConn)
			_ = destConn.Close()
			_ = clientConn.Close()
		}()
		_, _ = io.Copy(clientConn, destConn)
		_ = destConn.Close()
		_ = clientConn.Close()
		return
	}

	// Plain HTTP forward proxying
	targetHost := req.URL.Host
	if targetHost == "" {
		targetHost = req.Host
	}

	if err := p.checkDestination(req.Context(), targetHost, "80"); err != nil {
		p.recordDenial(err)
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}

	outReq := req.Clone(req.Context())
	outReq.RequestURI = ""
	outReq.Header.Del("Proxy-Connection")
	outReq.Header.Del("Connection")
	outReq.Header.Del("Keep-Alive")
	outReq.Header.Del("Proxy-Authenticate")
	outReq.Header.Del("Proxy-Authorization")

	resp, err := p.transport.RoundTrip(outReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("proxy forwarding error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
