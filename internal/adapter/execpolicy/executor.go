package execpolicy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

var (
	// ErrDisallowedCommand is returned when a command is not allowlisted or violates execution policy.
	ErrDisallowedCommand = errors.New("disallowed command")

	// ErrDisallowedEnv is returned when an environment variable is forbidden or not allowlisted.
	ErrDisallowedEnv = errors.New("disallowed environment variable")

	// ErrInvalidDirectory is returned when a requested execution directory is invalid or nonexistent.
	ErrInvalidDirectory = errors.New("invalid execution directory")

	// ErrUnsupportedIsolationCapability is returned when requested isolation strictness cannot be met.
	ErrUnsupportedIsolationCapability = errors.New("unsupported isolation capability")

	// ErrInvalidLaunchRequest is returned when the launch request parameters fail validation.
	ErrInvalidLaunchRequest = errors.New("invalid launch request")
)

// LaunchRequest encapsulates all parameters required to launch a supervised process.
type LaunchRequest struct {
	RunID             string
	SessionID         string
	TurnKey           string
	AttemptID         string
	Command           string
	Args              []string
	ExtraEnvAllowlist []string
	Paths             workspace.WorkspacePaths
	Profile           storage.CanonicalProfile
}

// CapabilityChecker verifies whether the host environment supports required isolation capabilities.
type CapabilityChecker func(ctx context.Context, req LaunchRequest) error

// PolicyExecutor enforces directory pinning, command allowlists, and environment sanitization.
type PolicyExecutor interface {
	Start(ctx context.Context, req LaunchRequest) (ManagedProcess, error)
}

// Option configures a defaultPolicyExecutor.
type Option func(*defaultPolicyExecutor)

// WithCapabilityChecker configures a custom capability checker for testing or platform customization.
func WithCapabilityChecker(fn CapabilityChecker) Option {
	return func(e *defaultPolicyExecutor) {
		e.capabilityChecker = fn
	}
}

// WithNetworkProxy configures a custom NetworkProxy for the executor.
func WithNetworkProxy(proxy *NetworkProxy) Option {
	return func(e *defaultPolicyExecutor) {
		e.proxy = proxy
	}
}

type defaultPolicyExecutor struct {
	capabilityChecker CapabilityChecker
	proxy             *NetworkProxy
}

// New creates a new PolicyExecutor with the given options.
func New(opts ...Option) PolicyExecutor {
	e := &defaultPolicyExecutor{
		capabilityChecker: defaultCapabilityChecker,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// NewPolicyExecutor creates a new PolicyExecutor with the given options.
func NewPolicyExecutor(opts ...Option) PolicyExecutor {
	return New(opts...)
}

func defaultCapabilityChecker(ctx context.Context, req LaunchRequest) error {
	if req.Profile.IsolationStrictness != "strict" {
		return nil
	}

	if runtime.GOOS != "linux" {
		return fmt.Errorf("%w: strict isolation requires Linux host capabilities", ErrUnsupportedIsolationCapability)
	}

	if req.Profile.WorkspaceMode == "readonly" {
		if _, err := os.Stat("/proc/self/ns/user"); err != nil {
			return fmt.Errorf("%w: user namespace unavailable for mount isolation", ErrUnsupportedIsolationCapability)
		}
		if _, err := os.Stat("/proc/self/ns/mnt"); err != nil {
			return fmt.Errorf("%w: mount namespace unavailable for mount isolation", ErrUnsupportedIsolationCapability)
		}
	}

	if req.Profile.NetworkMode == "allowlist" || req.Profile.NetworkMode == "none" {
		if _, err := os.Stat("/proc/self/ns/net"); err != nil {
			return fmt.Errorf("%w: network namespace unavailable", ErrUnsupportedIsolationCapability)
		}
	}

	return nil
}

func isForbiddenSecretKey(key string) bool {
	upper := strings.ToUpper(key)
	if upper == "AUTH_TOKEN" || upper == "COUNCIL_SOCKET" || upper == "COUNCIL_LEASE" {
		return true
	}
	lower := strings.ToLower(key)
	return strings.Contains(lower, "secret") || strings.Contains(lower, "key") || strings.Contains(lower, "token")
}

func isProxyEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	return upper == "HTTP_PROXY" || upper == "HTTPS_PROXY" || upper == "ALL_PROXY" || upper == "NO_PROXY"
}

func (e *defaultPolicyExecutor) Start(ctx context.Context, req LaunchRequest) (ManagedProcess, error) {
	// 1. Validate root directory
	if req.Paths.Root == "" {
		return nil, fmt.Errorf("%w: paths.Root cannot be empty", ErrInvalidDirectory)
	}
	st, err := os.Stat(req.Paths.Root)
	if err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%w: root %q does not exist or is not a directory", ErrInvalidDirectory, req.Paths.Root)
	}

	// 2. Validate profile algorithm version
	if req.Profile.AlgoVersion != "" && req.Profile.AlgoVersion != "cprof-v1" {
		return nil, fmt.Errorf("%w: invalid profile algo_version %q", ErrInvalidLaunchRequest, req.Profile.AlgoVersion)
	}

	// 3. Validate identifiers
	if req.SessionID == "" {
		return nil, fmt.Errorf("%w: session_id cannot be empty", ErrInvalidLaunchRequest)
	}
	if err := workspace.ValidateIdentifier(req.SessionID); err != nil {
		return nil, fmt.Errorf("%w: invalid session_id: %v", ErrInvalidLaunchRequest, err)
	}
	runID := req.RunID
	if runID == "" {
		parent := filepath.Dir(req.Paths.Root)
		grandparent := filepath.Dir(parent)
		if filepath.Base(parent) == req.SessionID {
			runID = filepath.Base(grandparent)
		}
	}
	if runID != "" {
		if err := workspace.ValidateIdentifier(runID); err != nil {
			return nil, fmt.Errorf("%w: invalid run_id: %v", ErrInvalidLaunchRequest, err)
		}
	}

	// 3. Validate command against profile tooling allowlist
	if req.Command == "" {
		return nil, fmt.Errorf("%w: command cannot be empty", ErrDisallowedCommand)
	}
	cmdBase := filepath.Base(req.Command)
	cmdAllowed := false
	for _, tool := range req.Profile.Tooling {
		if tool == req.Command || tool == cmdBase {
			cmdAllowed = true
			break
		}
	}
	if !cmdAllowed {
		return nil, fmt.Errorf("%w: command %q not in profile tooling allowlist", ErrDisallowedCommand, req.Command)
	}

	// 4. Git command policy gating
	if cmdBase == "git" {
		if err := validateGitCommand(req, runID); err != nil {
			return nil, err
		}
	}

	// 5. Validate extra env allowlist against profile and secret rules
	profileAllowedEnv := make(map[string]struct{})
	for _, h := range req.Profile.Harnesses {
		for _, v := range h.ExtraEnvAllowlist {
			profileAllowedEnv[v] = struct{}{}
		}
	}

	for _, extra := range req.ExtraEnvAllowlist {
		key := extra
		if idx := strings.Index(key, "="); idx != -1 {
			key = key[:idx]
		}
		if isForbiddenSecretKey(key) {
			return nil, fmt.Errorf("%w: secret key %q is forbidden", ErrDisallowedEnv, key)
		}
		if req.Profile.NetworkMode == "none" && isProxyEnvKey(key) {
			return nil, fmt.Errorf("%w: proxy key %q is forbidden when network_mode is none", ErrDisallowedEnv, key)
		}
		if _, ok := profileAllowedEnv[key]; !ok {
			return nil, fmt.Errorf("%w: environment variable %q not allowlisted in canonical profile", ErrDisallowedEnv, key)
		}
	}

	// 6. Check isolation capabilities
	if req.Profile.IsolationStrictness == "strict" {
		if err := e.capabilityChecker(ctx, req); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedIsolationCapability, err)
		}
	}

	// 7. Construct sanitized environment allowlist
	var env []string
	baseKeys := []string{"PATH", "TMPDIR", "TERM", "LANG", "LC_ALL", "USER"}
	for _, k := range baseKeys {
		if v, ok := os.LookupEnv(k); ok {
			if !isForbiddenSecretKey(k) {
				if req.Profile.NetworkMode == "none" && isProxyEnvKey(k) {
					continue
				}
				env = append(env, k+"="+v)
			}
		}
	}
	env = append(env, "HOME="+req.Paths.Config)
	env = append(env, "COUNCIL_WORKSPACE_ROOT="+req.Paths.Root)
	if runID != "" {
		env = append(env, "COUNCIL_RUN_ID="+runID)
	}
	if req.SessionID != "" {
		env = append(env, "COUNCIL_SESSION_ID="+req.SessionID)
	}

	var proxyToClose *NetworkProxy
	switch req.Profile.NetworkMode {
	case "allowlist":
		var activeProxy *NetworkProxy
		if e.proxy != nil {
			activeProxy = e.proxy
		} else {
			var err error
			activeProxy, err = StartNetworkProxy(req.Profile.NetworkAllowlist)
			if err != nil {
				return nil, fmt.Errorf("start network proxy: %w", err)
			}
			proxyToClose = activeProxy
		}
		endpoint := activeProxy.Endpoint()
		env = append(env,
			"HTTP_PROXY="+endpoint,
			"HTTPS_PROXY="+endpoint,
			"http_proxy="+endpoint,
			"https_proxy="+endpoint,
		)
	case "none":
		// Ensure no proxy is supplied
	}

	for _, extra := range req.ExtraEnvAllowlist {
		key := extra
		val := ""
		if idx := strings.Index(extra, "="); idx != -1 {
			key = extra[:idx]
			val = extra[idx+1:]
		} else {
			var ok bool
			val, ok = os.LookupEnv(key)
			if !ok {
				continue
			}
		}
		if isForbiddenSecretKey(key) {
			continue
		}
		if req.Profile.NetworkMode == "none" && isProxyEnvKey(key) {
			continue
		}
		env = append(env, key+"="+val)
	}

	// Final safeguard: filter any secrets or proxy variables when mode is none
	var scrubbedEnv []string
	for _, entry := range env {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) > 0 {
			if isForbiddenSecretKey(parts[0]) {
				continue
			}
			if req.Profile.NetworkMode == "none" && isProxyEnvKey(parts[0]) {
				continue
			}
		}
		scrubbedEnv = append(scrubbedEnv, entry)
	}

	// 8. Construct exec.Cmd
	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	cmd.Dir = req.Paths.Root
	cmd.Env = scrubbedEnv

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		if proxyToClose != nil {
			_ = proxyToClose.Close()
		}
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		if proxyToClose != nil {
			_ = proxyToClose.Close()
		}
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		if proxyToClose != nil {
			_ = proxyToClose.Close()
		}
		return nil, fmt.Errorf("create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		if proxyToClose != nil {
			_ = proxyToClose.Close()
		}
		return nil, fmt.Errorf("start process: %w", err)
	}

	return &managedProcess{
		cmd:       cmd,
		stdinPipe: stdinPipe,
		stdout:    stdoutPipe,
		stderr:    stderrPipe,
		cleanup: func() {
			if proxyToClose != nil {
				_ = proxyToClose.Close()
			}
		},
	}, nil
}

func validateGitCommand(req LaunchRequest, runID string) error {
	assignedBranch := ""
	altAssignedBranch := ""
	if runID != "" && req.SessionID != "" {
		assignedBranch = fmt.Sprintf("council/%s/%s", runID, req.SessionID)
		altAssignedBranch = fmt.Sprintf("council/%s", req.SessionID)
	}

	for i, arg := range req.Args {
		switch arg {
		case "checkout", "switch":
			for _, opt := range req.Args[i+1:] {
				if opt == "--" {
					break
				}
				if opt == "-b" || opt == "-B" || opt == "-c" || opt == "-C" {
					return fmt.Errorf("%w: git branch creation (%s) is disallowed", ErrDisallowedCommand, opt)
				}
				if opt == "main" || opt == "master" || strings.HasPrefix(opt, "origin/") {
					return fmt.Errorf("%w: git %s to %s is disallowed", ErrDisallowedCommand, arg, opt)
				}
				if assignedBranch != "" && !strings.HasPrefix(opt, "-") && opt != "." && opt != assignedBranch && opt != altAssignedBranch {
					return fmt.Errorf("%w: git %s outside assigned branch %s is disallowed", ErrDisallowedCommand, arg, assignedBranch)
				}
			}
		case "branch":
			for _, opt := range req.Args[i+1:] {
				if !strings.HasPrefix(opt, "-") && (assignedBranch == "" || (opt != assignedBranch && opt != altAssignedBranch)) {
					return fmt.Errorf("%w: git branch mutation for %s is disallowed", ErrDisallowedCommand, opt)
				}
			}
		case "push":
			for _, opt := range req.Args[i+1:] {
				if opt == "main" || opt == "master" || opt == "--force" || opt == "-f" {
					return fmt.Errorf("%w: git push to %s is disallowed", ErrDisallowedCommand, opt)
				}
			}
		}
	}
	return nil
}
