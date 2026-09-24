package execpolicy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

	// ErrAgyLaunchNotSealed is returned (wrapped in ErrInvalidLaunchRequest)
	// when an agy-shaped launch request (IsAgyLaunch) carries neither a
	// SealedImage nor the explicit FixtureLaunch marker. Production agy
	// launches must always go through the sealed-image path (AC-010);
	// only the agytest fixture harness may bypass it, and only via the
	// explicit marker.
	ErrAgyLaunchNotSealed = errors.New("agy launch requires a sealed image or the explicit fixture-launch marker")

	// ErrAgyLaunchForbiddenArg is returned (wrapped in
	// ErrInvalidLaunchRequest) when an agy-shaped launch request's argv
	// carries a flag or subcommand the frozen launch source must never
	// pass: an auth/permission bypass, identity-losing resumption,
	// interactive/remote control, or project/installation management
	// surface (AC-010 Global Constraints).
	ErrAgyLaunchForbiddenArg = errors.New("agy launch argv carries a forbidden argument")
)

// agyForbiddenArgs is the closed set of agy CLI flags/subcommands a
// Council-driven launch must never carry (AC-010 Global Constraints,
// verbatim): auth/permission bypass, identity-losing resumption,
// interactive/remote control, and project/installation/update/mic
// management surfaces.
var agyForbiddenArgs = map[string]bool{
	"--dangerously-skip-permissions": true,
	"-c":                             true,
	"--continue":                     true,
	"-i":                             true,
	"--prompt-interactive":           true,
	"--remote-control":               true,
	"--add-dir":                      true,
	"--project":                      true,
	"--new-project":                  true,
	"install":                        true,
	"update":                         true,
	"mic-serve":                      true,
}

// IsAgyLaunch reports whether req is shaped like an agy stream-json
// launch (AC-010 Global Constraints, frozen argv): the leading
// "--print=" flag together with both "--input-format stream-json" and
// "--output-format stream-json". This is sufficient for recognition;
// the full exact-argv validation belongs to the agy launch source
// (Task 5), not to this recognizer.
func IsAgyLaunch(req LaunchRequest) bool {
	args := req.Args
	if len(args) == 0 || args[0] != "--print=" {
		return false
	}
	hasInputStreamJSON := false
	hasOutputStreamJSON := false
	for i, a := range args {
		if a == "--input-format" && i+1 < len(args) && args[i+1] == "stream-json" {
			hasInputStreamJSON = true
		}
		if a == "--output-format" && i+1 < len(args) && args[i+1] == "stream-json" {
			hasOutputStreamJSON = true
		}
	}
	return hasInputStreamJSON && hasOutputStreamJSON
}

// agyForbiddenArg returns the first forbidden argv entry present, if any.
func agyForbiddenArg(args []string) (string, bool) {
	for _, a := range args {
		if agyForbiddenArgs[a] {
			return a, true
		}
	}
	return "", false
}

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
	// GeneratedServerEnv carries Council-generated transport credentials for
	// an `opencode serve` child. The executor accepts it only on that exact
	// launch shape and injects it after the scrub pass.
	GeneratedServerEnv *GeneratedServerEnv
	// ClaudeConfigDir carries the per-session Claude config root for an
	// exact `claude -p` launch. The executor accepts it only on that
	// shape, injects it as CLAUDE_CONFIG_DIR after the scrub pass, and
	// keeps it out of captured event payloads.
	ClaudeConfigDir string
	// ClaudeConfigBaseDir is the trusted base the config dir must be
	// contained in (service-validated). Required whenever
	// ClaudeConfigDir is set; verified symlink-safe before injection.
	ClaudeConfigBaseDir string
	// PromptDigest is the frozen prompt hash for durable acceptance
	// correlation (AC-008 §3.5). Recorded on the attempt, never sent to
	// the native side.
	PromptDigest string
	// UniverseTools carries the pinned native tool universe for parser
	// drift checks (AC-008 §3.8). Set by the Claude launch source.
	UniverseTools []string
	// Model is the frozen native model identity the launch pins via
	// --model (AC-008 §3.7). The stream parser validates the init
	// event's model against it. Set by the Claude launch source.
	Model string
	// ProfileDigest is the frozen run-profile digest in force for this
	// launch (AC-008 §3.6 manifest-digest binding for attestations).
	ProfileDigest string
	// SealedImage, when set, pins the exact executable Start launches:
	// a kernel-sealed memfd copy of the binary, verified at the ptrace
	// exec-stop against /proc/<pid>/exe before the child runs a single
	// instruction (Linux only; ErrSealedLaunchUnsupported elsewhere).
	// Command must equal SealedImage.ArgV0 exactly.
	SealedImage *SealedImage
	// FixtureLaunch is the explicit, construction-time marker (AC-010,
	// mirroring the codex/opencode fixture-scope discipline: never
	// inferred from absent state) that authorizes an agy-shaped launch
	// (IsAgyLaunch) to proceed WITHOUT a SealedImage. Its only reason to
	// exist: NewSealedImage is Linux-only (ErrSealedLaunchUnsupported
	// elsewhere), so the agytest fixture harness cannot build a sealed
	// image off Linux and needs an explicit, auditable bypass instead of
	// a platform-sniffed one. Only the internal/adapter/agy/agytest
	// package may set it — production packages never do (enforced by
	// TestFixtureLaunch_NeverSetOutsideAgytest in executor_agy_test.go,
	// mirroring the codex/codextest import/reference guards). On Linux,
	// agytest always builds a real SealedImage from the compiled fixture
	// binary and sets this too, so Task 3's sealed-launch path is
	// exercised by every fixture test there as well — this field is not
	// a substitute for SealedImage, only the documented escape hatch
	// where SealedImage cannot exist.
	FixtureLaunch bool
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
	return checkPlatformCapabilities(ctx, req)
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

	// 2. Validate profile algorithm version (v3/v4 are additive: manifest,
	// codex block, and agy block are carried, not acted on, by this gate)
	if req.Profile.AlgoVersion != "" && req.Profile.AlgoVersion != "cprof-v1" && req.Profile.AlgoVersion != "cprof-v2" && req.Profile.AlgoVersion != "cprof-v3" && req.Profile.AlgoVersion != "cprof-v4" {
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

	// 5b. Validate GeneratedServerEnv shape and key collisions
	if err := validateGeneratedServerEnvShape(req); err != nil {
		return nil, err
	}
	if err := validateGeneratedServerEnvCollisions(req, profileAllowedEnv); err != nil {
		return nil, err
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

	// Inject Council-generated server credentials after the scrub pass so
	// the secret filter cannot strip them (Gate-spec: these are Council-
	// generated transport credentials, not inherited secrets).
	scrubbedEnv = appendGeneratedServerEnv(scrubbedEnv, req)

	// Typed Claude config-dir extension: validated against the exact
	// Claude launch shape, then injected as CLAUDE_CONFIG_DIR after the
	// scrub pass (AC-008 §3.7).
	if req.ClaudeConfigDir != "" {
		if err := validateClaudeConfigDir(req); err != nil {
			if proxyToClose != nil {
				_ = proxyToClose.Close()
			}
			return nil, err
		}
		scrubbedEnv = append(scrubbedEnv, "CLAUDE_CONFIG_DIR="+req.ClaudeConfigDir)
	}

	cleanup := func() {
		if proxyToClose != nil {
			_ = proxyToClose.Close()
		}
	}

	// agy-shaped launches (AC-010): forbidden argv is refused
	// regardless of sealing, and production (no SealedImage, no
	// explicit FixtureLaunch marker) is refused outright — an agy
	// launch must always run the sealed, ptrace-verified binary except
	// for the agytest fixture harness, which is the only caller
	// authorized to set FixtureLaunch.
	if IsAgyLaunch(req) {
		if arg, found := agyForbiddenArg(req.Args); found {
			cleanup()
			return nil, fmt.Errorf("%w: %w: %q", ErrInvalidLaunchRequest, ErrAgyLaunchForbiddenArg, arg)
		}
		if req.SealedImage == nil && !req.FixtureLaunch {
			cleanup()
			return nil, fmt.Errorf("%w: %w", ErrInvalidLaunchRequest, ErrAgyLaunchNotSealed)
		}
	}

	if req.SealedImage != nil {
		if req.Command != req.SealedImage.ArgV0 {
			cleanup()
			return nil, fmt.Errorf("%w: command %q does not match sealed image argv0 %q", ErrInvalidLaunchRequest, req.Command, req.SealedImage.ArgV0)
		}
		proc, err := startSealed(ctx, req, scrubbedEnv, cleanup)
		if err != nil {
			cleanup()
			return nil, err
		}
		return proc, nil
	}

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	cmd.Dir = req.Paths.Root
	cmd.Env = scrubbedEnv

	if err := configureSysProcAttr(req, cmd); err != nil {
		if proxyToClose != nil {
			_ = proxyToClose.Close()
		}
		return nil, fmt.Errorf("configure isolation sysprocattr: %w", err)
	}

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
		cmd:         cmd,
		stdinPipe:   stdinPipe,
		stdout:      stdoutPipe,
		stderr:      stderrPipe,
		cleanup:     cleanup,
		exeIdentity: ExeIdentity{Path: req.Command},
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
					return fmt.Errorf("%w: git %s outside assigned branch %s is disallowed (use 'git checkout -- <path>' for files)", ErrDisallowedCommand, arg, assignedBranch)
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
