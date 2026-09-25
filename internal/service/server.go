package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/agy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/claude"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/opencode"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// resolveClaudeProbeScratchRoot validates and prepares the configured
// Claude contract-probe scratch root. Mirrors the OpenCode probe
// scratch family: operator-provided, disjoint from StateDir and
// WorkspaceBaseDir (resolved, pre- and post-creation), operator-only
// permissions.
func resolveClaudeProbeScratchRoot(cfg ServerConfig) (string, error) {
	root := strings.TrimSpace(cfg.ClaudeProbeScratchRoot)
	if root == "" {
		return "", fmt.Errorf("ClaudeProbeScratchRoot is required when ClaudeBinaryPath is configured")
	}
	root = filepath.Clean(root)

	bases := map[string]string{}
	for name, base := range map[string]string{
		"StateDir":         cfg.StateDir,
		"WorkspaceBaseDir": cfg.WorkspaceBaseDir,
	} {
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		bases[name] = filepath.Clean(base)
	}

	resolved, err := resolveExistingPath(root)
	if err != nil {
		return "", fmt.Errorf("resolve claude probe scratch root: %w", err)
	}
	if err := checkScratchContainment(bases, resolved); err != nil {
		return "", err
	}
	if err := checkClaudeScratchConfigBaseDisjoint(cfg, resolved); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create claude probe scratch root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("secure claude probe scratch root: %w", err)
	}
	resolvedFinal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve claude probe scratch root after creation: %w", err)
	}
	if err := checkScratchContainment(bases, resolvedFinal); err != nil {
		return "", err
	}
	if err := checkClaudeScratchConfigBaseDisjoint(cfg, resolvedFinal); err != nil {
		return "", err
	}
	return root, nil
}

// checkClaudeScratchConfigBaseDisjoint rejects the probe scratch root
// and the Claude config base overlapping in EITHER direction (§3.7):
// the scratch must not live inside the config base, and the config
// base must not live inside the scratch, on resolved paths.
func checkClaudeScratchConfigBaseDisjoint(cfg ServerConfig, resolvedScratch string) error {
	configBase := strings.TrimSpace(cfg.ClaudeConfigBaseDir)
	if configBase == "" {
		return nil
	}
	resolvedConfig, err := resolveExistingPath(filepath.Clean(configBase))
	if err != nil {
		return fmt.Errorf("resolve claude config base: %w", err)
	}
	if pathContains(resolvedConfig, resolvedScratch) || pathContains(resolvedScratch, resolvedConfig) {
		return fmt.Errorf(
			"claude probe scratch root %s overlaps the claude config base %s: the two must be disjoint in both directions",
			resolvedScratch, resolvedConfig)
	}
	return nil
}

// resolveClaudeTemplateDir validates the configured frozen config
// template: required, pre-provisioned, a real directory. The service
// never creates or mutates it — materialization reads it at session
// creation.
func resolveClaudeTemplateDir(cfg ServerConfig) (string, error) {
	dir := strings.TrimSpace(cfg.ClaudeTemplateDir)
	if dir == "" {
		return "", fmt.Errorf("ClaudeTemplateDir is required when ClaudeBinaryPath is configured")
	}
	dir = filepath.Clean(dir)
	st, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("claude config template %s is missing: %w", dir, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("claude config template %s is not a directory", dir)
	}
	return dir, nil
}

// resolveClaudeEvidenceRoot validates the configured trusted universe
// evidence root: required and a real directory.
func resolveClaudeEvidenceRoot(cfg ServerConfig) (string, error) {
	dir := strings.TrimSpace(cfg.ClaudeEvidenceRoot)
	if dir == "" {
		return "", fmt.Errorf("ClaudeEvidenceRoot is required when ClaudeBinaryPath is configured")
	}
	dir = filepath.Clean(dir)
	st, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("claude evidence root %s is missing: %w", dir, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("claude evidence root %s is not a directory", dir)
	}
	return dir, nil
}

// resolveCodexEvidenceRoot validates the configured trusted evidence
// root for the codex frozen event-universe re-hash: required and a real
// directory.
func resolveCodexEvidenceRoot(cfg ServerConfig) (string, error) {
	dir := strings.TrimSpace(cfg.CodexEvidenceRoot)
	if dir == "" {
		return "", fmt.Errorf("CodexEvidenceRoot is required when CodexBinaryPath is configured")
	}
	dir = filepath.Clean(dir)
	st, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("codex evidence root %s is missing: %w", dir, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("codex evidence root %s is not a directory", dir)
	}
	return dir, nil
}

// resolveCodexScratchRoot validates and prepares the configured codex
// neutral scratch root (probe children and session children). Mirrors
// the OpenCode probe scratch family: operator-provided, disjoint from
// StateDir and WorkspaceBaseDir (resolved, pre- and post-creation),
// operator-only permissions.
func resolveCodexScratchRoot(cfg ServerConfig) (string, error) {
	root := strings.TrimSpace(cfg.CodexScratchRoot)
	if root == "" {
		return "", fmt.Errorf("CodexScratchRoot is required when CodexBinaryPath is configured")
	}
	return resolveScratchRoot(cfg, root, "codex")
}

// resolveScratchRoot is the shared scratch-root preparation of the codex
// and agy adapters (label names the adapter in errors): the root must be
// disjoint from StateDir and WorkspaceBaseDir (resolved, pre- and
// post-creation) and is created/secured with operator-only permissions.
func resolveScratchRoot(cfg ServerConfig, root, label string) (string, error) {
	root = filepath.Clean(root)

	bases := map[string]string{}
	for name, base := range map[string]string{
		"StateDir":         cfg.StateDir,
		"WorkspaceBaseDir": cfg.WorkspaceBaseDir,
	} {
		base = strings.TrimSpace(base)
		if base == "" {
			continue
		}
		bases[name] = filepath.Clean(base)
	}

	resolved, err := resolveExistingPath(root)
	if err != nil {
		return "", fmt.Errorf("resolve %s scratch root: %w", label, err)
	}
	if err := checkScratchContainment(bases, resolved); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create %s scratch root: %w", label, err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", fmt.Errorf("secure %s scratch root: %w", label, err)
	}
	resolvedFinal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve %s scratch root after creation: %w", label, err)
	}
	if err := checkScratchContainment(bases, resolvedFinal); err != nil {
		return "", err
	}
	return root, nil
}

type ServerConfig struct {
	StateDir         string
	InstanceID       string
	AuthToken        string
	WorkspaceBaseDir string
	// OpenCodeProbeProfile is the operator-approved canonical profile the
	// OpenCode capability probe launches carry. Required when
	// OpenCodeBinaryPath is set: the probe template fails closed without
	// it.
	OpenCodeProbeProfile storage.CanonicalProfile

	// OpenCodeIdleGrace, when positive, configures how long the OpenCode
	// adapter keeps a contributor server alive after its last terminal
	// turn before parking it. Zero uses the adapter default.
	OpenCodeIdleGrace time.Duration

	// ClaudeBinaryPath, when set, enables the Claude persistent
	// contributor adapter (AC-008). Empty means no Claude adapter.
	ClaudeBinaryPath string

	// ClaudeConfigBaseDir is the operator-provisioned base directory for
	// per-session Claude config roots. Required when ClaudeBinaryPath is
	// set; validated to be disjoint from StateDir and WorkspaceBaseDir
	// and secured to operator-only permissions.
	ClaudeConfigBaseDir string

	// ClaudeTemplateDir is the operator-provisioned frozen config
	// template CreateSession materializes per-session config roots
	// from. Required when ClaudeBinaryPath is set; it must already
	// exist — the service never synthesizes or mutates the template.
	ClaudeTemplateDir string

	// ClaudeEvidenceRoot is the trusted service-owned root holding the
	// pinned native-tool-universe evidence file. Required when
	// ClaudeBinaryPath is set.
	ClaudeEvidenceRoot string

	// ClaudeProbeProfile is the operator-approved canonical profile the
	// Claude contract probes (--version, --help) launch with. Required
	// when ClaudeBinaryPath is set.
	ClaudeProbeProfile storage.CanonicalProfile

	// ClaudeProbeScratchRoot is the operator-provisioned directory for
	// Claude contract-probe children. Required when ClaudeBinaryPath is
	// set; disjoint from StateDir and WorkspaceBaseDir, operator-only
	// permissions.
	ClaudeProbeScratchRoot string

	// OpenCodeProbeScratchRoot is the operator-provisioned directory for
	// probe scratch directories. Required when OpenCodeBinaryPath is set;
	// it must lie outside both StateDir and WorkspaceBaseDir. The service
	// creates it with operator-only permissions (0700) and tightens a
	// pre-provisioned directory to the same mode.
	OpenCodeProbeScratchRoot string

	// OpenCodeBinaryPath, when set, enables the OpenCode persistent
	// contributor adapter via production seams backed by the storage
	// store and AC-005 workspace manager. Empty means no OpenCode
	// adapter.
	OpenCodeBinaryPath string

	// CodexBinaryPath, when set, enables the Codex persistent
	// contributor adapter (AC-009). Empty means no Codex adapter.
	CodexBinaryPath string

	// CodexProfile is the operator-approved frozen cprof-v3 run profile
	// with the complete harnesses.codex block. Required when
	// CodexBinaryPath is set: the frozen launch policy (including the
	// canonical toolkit-manifest digest) is validated from it at
	// construction, and the event-universe evidence is re-hashed at
	// validation. An incomplete block or evidence drift fails the
	// wiring, never the first launch.
	CodexProfile storage.CanonicalProfile

	// CodexEvidenceRoot is the trusted evidence root the frozen event-
	// universe re-hash runs against. Required when CodexBinaryPath is
	// set.
	CodexEvidenceRoot string

	// CodexScratchRoot is the operator-provisioned neutral scratch
	// directory: the working directory of every codex child (probe
	// children and session children alike — process plumbing only; the
	// thread cwd is the frozen workspace root). Required when
	// CodexBinaryPath is set; disjoint from StateDir and
	// WorkspaceBaseDir, operator-only permissions (0700).
	CodexScratchRoot string

	// AgyBinaryPath, when set, enables the Agy persistent contributor
	// adapter (AC-010). Empty means no Agy adapter. It must equal the
	// frozen harnesses.agy.binary_path: the sealed image of that pinned
	// binary is what every launch execs.
	AgyBinaryPath string

	// AgyProfile is the operator-approved frozen cprof-v4 run profile
	// with the complete harnesses.agy block. Required when AgyBinaryPath
	// is set: the frozen launch policy is validated from it (evidence
	// re-hashed under AgyEvidenceRoot) at construction.
	AgyProfile storage.CanonicalProfile

	// AgyEvidenceRoot is the trusted evidence root the frozen init/tool-
	// coverage/plugins evidence is re-hashed against. Required when
	// AgyBinaryPath is set.
	AgyEvidenceRoot string

	// AgyHomeDir is the operator HOME every agy child inherits; it must
	// be the parent of the frozen expected_home. Required when
	// AgyBinaryPath is set and NEVER defaulted from $HOME.
	AgyHomeDir string

	// AgyScratchRoot is the operator-provisioned neutral directory for
	// the construction-scoped `plugin list` capture. Required when
	// AgyBinaryPath is set; disjoint from StateDir and WorkspaceBaseDir,
	// operator-only permissions (0700).
	AgyScratchRoot string
}

type ReadinessResponse struct {
	Status          string `json:"status"`
	InstanceID      string `json:"instance_id"`
	ProtocolVersion int    `json:"protocol_version"`
	StateDir        string `json:"state_dir"`
	LiveWorkers     int    `json:"live_workers"`
	ReservedTurns   int    `json:"reserved_turns"`
	UnresolvedTurns int    `json:"unresolved_turns"`
}

type StatusResponse struct {
	InstanceID      string    `json:"instance_id"`
	PID             int       `json:"pid"`
	Status          string    `json:"status"`
	StateDir        string    `json:"state_dir"`
	StartedAt       time.Time `json:"started_at"`
	ActiveRuns      []string  `json:"active_runs"`
	LiveWorkers     int       `json:"live_workers"`
	ReservedTurns   int       `json:"reserved_turns"`
	UnresolvedTurns int       `json:"unresolved_turns"`
	// Agy is the AC-010 agy adapter wiring state (spec §14.18); omitted
	// when no agy adapter is configured.
	Agy *AgyWiringStatus `json:"agy,omitempty"`
}

// Agy wiring states (spec §14.18).
const (
	// AgyNotConfigured: no AgyBinaryPath; the server has no agy adapter.
	AgyNotConfigured = "not_configured"
	// AgyWired: the production agy adapter was constructed (eligible).
	AgyWired = "wired"
	// AgyNotWired: AgyBinaryPath is configured but the server's
	// contributor adapter is not the agy adapter (an injected adapter of
	// another kind); no agy operation can run.
	AgyNotWired = "not_wired"
	// AgyAwaitingAttestation: AgyBinaryPath is configured but production
	// construction was refused ONLY because no covering cprot-v2
	// attestation row exists for the frozen tuple. The server runs
	// without the agy adapter; birth, release, reconcile and queue-time
	// validation refuse with the typed ineligibility error, other agy
	// operations report the harness as unavailable, and no agy child is
	// started. Recording the row
	// (RecordAgyProbeAttestation) does not hot-reload: a restart
	// constructs the adapter.
	AgyAwaitingAttestation = "awaiting_attestation"
)

// AgyWiringStatus is the server's agy adapter wiring state, surfaced on
// GET /v1/status and logged once at construction.
type AgyWiringStatus struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type Server struct {
	store            *storage.Store
	lock             *ServiceLock
	cfg              ServerConfig
	coordinator      *Coordinator
	adapter          adapter.Adapter
	workspaceManager *workspace.WorkspaceManager
	policyExecutor   execpolicy.PolicyExecutor
	listener         net.Listener
	httpServer       *http.Server
	socketPath       string
	tokenPath        string
	startedAt        time.Time

	mu           sync.Mutex
	running      bool
	shutdown     chan struct{}
	teardownOnce sync.Once

	// codexBirthMu serializes the AC-005 workspace lookup-or-allocate
	// step of codex session birth: the workspace manager publishes a
	// placeholder while an allocation is in progress, so concurrent
	// duplicate creations must not observe it (they share the adapter's
	// creation reservation afterwards).
	codexBirthMu sync.Mutex
	// agyBirthMu is codexBirthMu's agy counterpart (AC-010 birth).
	agyBirthMu sync.Mutex
	// agyInFlight maps a session to the in-flight creation marker episode
	// a live CreateAgySession of THIS process owns (guarded by
	// agyBirthMu): a concurrent birth is refused as "in progress" rather
	// than as an uncertainty, which is what the same durable marker means
	// after a crash.
	agyInFlight map[string]int64
	// agyAfterNativeCreate is a TEST-ONLY crash seam: when set and it
	// returns true after the native conversation was created, the birth
	// stops before ANY further durable write, exactly as a process death
	// between the creation child and the binding commit would.
	agyAfterNativeCreate func(nativeID string) bool
	// agyStatus is the agy wiring state fixed at construction (§14.18).
	agyStatus AgyWiringStatus
	// agyAwaitingErr is the typed construction refusal (an
	// *agy.ErrNotEligible wrapping *agy.ErrProductionEligibilityMissing)
	// held while agyStatus is awaiting_attestation; birth, release,
	// reconcile and queue-time validation return it wrapped.
	agyAwaitingErr error
	teardownErr    error
}

func NewServer(store *storage.Store, lock *ServiceLock, cfg ServerConfig) (*Server, error) {
	return NewServerWithAdapter(store, lock, cfg, nil)
}

func NewServerWithAdapter(store *storage.Store, lock *ServiceLock, cfg ServerConfig, adp adapter.Adapter) (*Server, error) {
	// Create shared service dependencies once.
	var wm *workspace.WorkspaceManager
	if cfg.WorkspaceBaseDir != "" {
		var err error
		wm, err = workspace.NewWorkspaceManager(cfg.StateDir, cfg.WorkspaceBaseDir)
		if err != nil {
			return nil, fmt.Errorf("workspace manager: %w", err)
		}
	}
	pe := execpolicy.New()

	// If no adapter is provided but OpenCode is configured, construct the
	// production OpenCode adapter with fail-closed seams backed by the same
	// workspace manager and policy executor the service uses.
	if adp == nil && strings.TrimSpace(cfg.OpenCodeBinaryPath) != "" {
		if strings.TrimSpace(cfg.ClaudeBinaryPath) != "" || strings.TrimSpace(cfg.CodexBinaryPath) != "" || strings.TrimSpace(cfg.AgyBinaryPath) != "" {
			return nil, errors.New("only one persistent contributor adapter can be wired per service instance (OpenCode and Claude/Codex/Agy are both configured)")
		}
		scratchRoot, scratchErr := resolveOpenCodeProbeScratchRoot(cfg)
		if scratchErr != nil {
			return nil, fmt.Errorf("OpenCode probe scratch root: %w", scratchErr)
		}
		probeTemplate := opencode.NewOperatorProbeLaunchTemplate(cfg.OpenCodeBinaryPath, scratchRoot, cfg.OpenCodeProbeProfile)
		opts := []opencode.OpenCodeAdapterOption{}
		if cfg.OpenCodeIdleGrace > 0 {
			opts = append(opts, opencode.WithIdleGrace(cfg.OpenCodeIdleGrace))
		}
		var opErr error
		adp, opErr = opencode.NewProductionOpenCodeAdapter(store, wm, pe, probeTemplate, opts...)
		if opErr != nil {
			return nil, fmt.Errorf("OpenCode adapter construction: %w", opErr)
		}
	}

	// If no adapter is provided but Claude is configured, construct the
	// production Claude adapter (AC-008) with fail-closed configuration
	// validation: config base, template dir, evidence root, probe
	// scratch root, and probe profile are all required.
	if adp == nil && strings.TrimSpace(cfg.ClaudeBinaryPath) != "" {
		if strings.TrimSpace(cfg.CodexBinaryPath) != "" {
			return nil, errors.New("only one persistent contributor adapter can be wired per service instance (Claude and Codex are both configured)")
		}
		if strings.TrimSpace(cfg.AgyBinaryPath) != "" {
			return nil, errors.New("only one persistent contributor adapter can be wired per service instance (Claude and Agy are both configured)")
		}
		configBase, err := resolveClaudeConfigBaseDir(cfg)
		if err != nil {
			return nil, fmt.Errorf("Claude config base: %w", err)
		}
		scratchRoot, err := resolveClaudeProbeScratchRoot(cfg)
		if err != nil {
			return nil, fmt.Errorf("Claude probe scratch root: %w", err)
		}
		templateDir, err := resolveClaudeTemplateDir(cfg)
		if err != nil {
			return nil, fmt.Errorf("Claude config template: %w", err)
		}
		evidenceRoot, err := resolveClaudeEvidenceRoot(cfg)
		if err != nil {
			return nil, fmt.Errorf("Claude evidence root: %w", err)
		}
		probeTemplate := claude.NewOperatorProbeLaunchTemplate(cfg.ClaudeBinaryPath, scratchRoot, cfg.ClaudeProbeProfile)
		var clErr error
		adp, clErr = claude.NewProductionClaudeAdapter(store, wm, pe, probeTemplate, configBase, templateDir, evidenceRoot)
		if clErr != nil {
			return nil, fmt.Errorf("Claude adapter construction: %w", clErr)
		}
	}

	// If no adapter is provided but Codex is configured, construct the
	// production Codex adapter (AC-009) with fail-closed configuration
	// validation: scratch root, evidence root, and the frozen cprof-v3
	// profile are all required; the frozen launch policy (including the
	// canonical toolkit-manifest digest) and the event-universe re-hash
	// are validated at construction, and the §3.3 eligibility lookup is
	// always wired to the durable cprot-v2 rows.
	if adp == nil && strings.TrimSpace(cfg.CodexBinaryPath) != "" {
		if strings.TrimSpace(cfg.AgyBinaryPath) != "" {
			return nil, errors.New("only one persistent contributor adapter can be wired per service instance (Codex and Agy are both configured)")
		}
		scratchRoot, err := resolveCodexScratchRoot(cfg)
		if err != nil {
			return nil, fmt.Errorf("Codex scratch root: %w", err)
		}
		if _, err := resolveCodexEvidenceRoot(cfg); err != nil {
			return nil, fmt.Errorf("Codex evidence root: %w", err)
		}
		probeTemplate := codex.NewCodexProbeLaunchTemplate(cfg.CodexBinaryPath, scratchRoot, cfg.CodexProfile)
		var cxErr error
		adp, cxErr = codex.NewProductionCodexAdapter(store, pe, probeTemplate, cfg.CodexProfile, cfg.CodexEvidenceRoot)
		if cxErr != nil {
			return nil, fmt.Errorf("Codex adapter construction: %w", cxErr)
		}
	}

	// If no adapter is provided but Agy is configured, construct the
	// production Agy adapter (AC-010) with fail-closed configuration
	// validation: profile, evidence root, operator home, and scratch
	// root are all required; the frozen platform must be linux/unix on a
	// Linux host (§3.12) — refused typed BEFORE any child; the
	// production constructor then runs the §3.2 gates (eligibility from
	// the durable cprot-v2 rows before the construction `plugin list`).
	//
	// Spec §14.18 bootstrap: a refusal whose ONLY cause is a missing
	// covering attestation (agy.ErrNotEligible wrapping
	// agy.ErrProductionEligibilityMissing) does not fail the server — it
	// starts WITHOUT the agy adapter in the explicit awaiting_attestation
	// state, so the operator can record the first row through the HTTP
	// attestation route and restart. Every other construction
	// error (configuration, platform, sealed image, other ineligibility
	// rules a row cannot fix) still fails the server.
	var agyAwaitingErr error
	if adp == nil && strings.TrimSpace(cfg.AgyBinaryPath) != "" {
		agyAdapter, err := newProductionAgyAdapter(store, pe, wm, cfg)
		switch {
		case err == nil:
			adp = agyAdapter
		case isAgyAwaitingAttestation(err):
			agyAwaitingErr = err
		default:
			return nil, fmt.Errorf("Agy adapter construction: %w", err)
		}
	}

	srv, err := newServerWithAdapter(store, lock, cfg, adp, wm, pe)
	if err != nil {
		return nil, err
	}
	if agyAwaitingErr != nil {
		srv.agyAwaitingErr = agyAwaitingErr
		// The configured contributor adapter of this instance is agy:
		// no generic worker adapter stands in for it while awaiting.
		srv.adapter = nil
		srv.agyStatus = AgyWiringStatus{State: AgyAwaitingAttestation, Reason: agyAwaitingReason(agyAwaitingErr)}
		slog.Warn("agy adapter not wired: awaiting attestation", "instance_id", cfg.InstanceID, "reason", srv.agyStatus.Reason)
	}
	return srv, nil
}

// isAgyAwaitingAttestation reports a production construction refusal
// that recording a covering attestation row can clear: the typed
// ineligibility error whose cause is the missing/uncovered attestation.
func isAgyAwaitingAttestation(err error) bool {
	var ne *agy.ErrNotEligible
	var missing *agy.ErrProductionEligibilityMissing
	return errors.As(err, &ne) && errors.As(err, &missing)
}

// agyAwaitingReason names the operator bootstrap route and restart boundary.
func agyAwaitingReason(err error) string {
	return err.Error() + "; the operator must POST a covering cprot-v2 attestation to /v1/runs/{run_id}/agy/attestations and restart the service (no hot reload)"
}

// AgyStatus reports the agy adapter wiring state fixed at construction
// (spec §14.18).
func (s *Server) AgyStatus() AgyWiringStatus {
	return s.agyStatus
}

// agyAwaiting returns the typed ineligibility refusal (wrapping
// *agy.ErrNotEligible) when the server is awaiting attestation, else nil.
func (s *Server) agyAwaiting() error {
	if s.agyAwaitingErr == nil {
		return nil
	}
	return fmt.Errorf("the agy adapter is not wired (awaiting attestation; a restart after recording constructs it): %w", s.agyAwaitingErr)
}

// agyAwaitingForSession is agyAwaiting scoped to one session: it refuses
// only when the session's contributor is agy (a lookup failure refuses
// too — fail closed while awaiting).
func (s *Server) agyAwaitingForSession(ctx context.Context, sessionID string) error {
	if s.agyAwaitingErr == nil || s.store == nil {
		return nil
	}
	meta, err := s.store.GetSessionMetadata(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session lookup: %w", err)
	}
	if meta.Contributor != string(council.Agy) {
		return nil
	}
	return s.agyAwaiting()
}

// newProductionAgyAdapter validates the Agy configuration fail closed
// and builds the production adapter wired to the service's
// required-tools seam (the queue-time set journaled on the dispatch
// intent).
func newProductionAgyAdapter(store *storage.Store, pe execpolicy.PolicyExecutor, wm *workspace.WorkspaceManager, cfg ServerConfig) (*agy.AgyAdapter, error) {
	if strings.TrimSpace(cfg.AgyProfile.AlgoVersion) == "" {
		return nil, errors.New("AgyProfile is required when AgyBinaryPath is configured")
	}
	evidenceRoot, err := resolveAgyEvidenceRoot(cfg)
	if err != nil {
		return nil, err
	}
	homeDir, err := resolveAgyHomeDir(cfg)
	if err != nil {
		return nil, err
	}
	scratchRoot, err := resolveAgyScratchRoot(cfg)
	if err != nil {
		return nil, fmt.Errorf("Agy scratch root: %w", err)
	}
	if wm == nil {
		return nil, errors.New("WorkspaceBaseDir is required when AgyBinaryPath is configured (AC-005 allocations)")
	}
	policy, err := agy.ValidateAgyHarness(cfg.AgyProfile, evidenceRoot)
	if err != nil {
		return nil, fmt.Errorf("frozen agy policy: %w", err)
	}
	if filepath.Clean(strings.TrimSpace(cfg.AgyBinaryPath)) != policy.BinaryPath {
		return nil, fmt.Errorf("AgyBinaryPath %q is not the frozen binary_path %q", cfg.AgyBinaryPath, policy.BinaryPath)
	}
	if err := checkAgyPlatform(policy); err != nil {
		return nil, err
	}
	return agy.NewProductionAgyAdapter(store, pe, wm, cfg.AgyProfile, evidenceRoot, homeDir, scratchRoot,
		agy.WithRequiredToolsSource(&agyRequiredToolsSource{store: store}))
}

// checkAgyPlatform is the §3.12 production-eligibility platform rule,
// applied at construction before any child: the frozen platform must be
// linux/unix and the host must be Linux.
func checkAgyPlatform(policy agy.AgyLaunchPolicy) error {
	if policy.PlatformOS == "linux" && policy.PlatformFamily == "unix" && runtime.GOOS == "linux" {
		return nil
	}
	return &agy.ErrUnsupportedProfile{AlgoVersion: "cprof-v4",
		Reason: fmt.Sprintf("production agy is eligible only for the linux/unix platform on a linux host (frozen %s/%s, host %s)",
			policy.PlatformOS, policy.PlatformFamily, runtime.GOOS)}
}

// resolveAgyEvidenceRoot validates the configured trusted evidence
// root: required and a real directory.
func resolveAgyEvidenceRoot(cfg ServerConfig) (string, error) {
	dir := strings.TrimSpace(cfg.AgyEvidenceRoot)
	if dir == "" {
		return "", errors.New("AgyEvidenceRoot is required when AgyBinaryPath is configured")
	}
	dir = filepath.Clean(dir)
	st, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("agy evidence root %s is missing: %w", dir, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("agy evidence root %s is not a directory", dir)
	}
	return dir, nil
}

// resolveAgyHomeDir validates the configured operator HOME: required
// (never defaulted from $HOME), absolute, an existing directory. The
// production constructor then requires it to be the parent of the
// frozen expected_home.
func resolveAgyHomeDir(cfg ServerConfig) (string, error) {
	dir := strings.TrimSpace(cfg.AgyHomeDir)
	if dir == "" {
		return "", errors.New("AgyHomeDir is required when AgyBinaryPath is configured (it is never defaulted from $HOME)")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("AgyHomeDir %q must be absolute", dir)
	}
	dir = filepath.Clean(dir)
	st, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("agy home %s is missing: %w", dir, err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("agy home %s is not a directory", dir)
	}
	return dir, nil
}

// resolveAgyScratchRoot validates and prepares the configured Agy
// construction scratch root with the same rules as the codex root
// (resolveScratchRoot), which additionally requires an absolute path.
func resolveAgyScratchRoot(cfg ServerConfig) (string, error) {
	root := strings.TrimSpace(cfg.AgyScratchRoot)
	if root == "" {
		return "", errors.New("AgyScratchRoot is required when AgyBinaryPath is configured")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("AgyScratchRoot %q must be absolute", root)
	}
	return resolveScratchRoot(cfg, root, "agy")
}

func newServerWithAdapter(store *storage.Store, lock *ServiceLock, cfg ServerConfig, adp adapter.Adapter, wm *workspace.WorkspaceManager, pe execpolicy.PolicyExecutor) (*Server, error) {
	if store == nil {
		return nil, errors.New("store cannot be nil")
	}
	if lock == nil || !lock.IsHeld() {
		return nil, errors.New("service lock must be held")
	}
	if cfg.StateDir == "" {
		return nil, errors.New("stateDir cannot be empty")
	}
	if cfg.InstanceID == "" {
		return nil, errors.New("instanceID cannot be empty")
	}
	if cfg.AuthToken == "" {
		return nil, errors.New("authToken cannot be empty")
	}

	socketPath := filepath.Join(cfg.StateDir, "council.sock")
	tokenPath := filepath.Join(cfg.StateDir, "auth.token")

	if adp == nil && wm != nil {
		adp = execpolicy.NewWorkerAdapter(wm, pe, store)
	}

	srv := &Server{
		store:            store,
		lock:             lock,
		cfg:              cfg,
		coordinator:      NewCoordinator(),
		adapter:          adp,
		workspaceManager: wm,
		policyExecutor:   pe,
		socketPath:       socketPath,
		tokenPath:        tokenPath,
		startedAt:        time.Now().UTC(),
		shutdown:         make(chan struct{}),
	}
	// The status reflects the adapter actually wired, not merely the
	// configuration: AgyBinaryPath with an injected non-agy adapter is
	// not "wired".
	_, isAgy := adp.(*agy.AgyAdapter)
	switch {
	case strings.TrimSpace(cfg.AgyBinaryPath) == "":
		srv.agyStatus = AgyWiringStatus{State: AgyNotConfigured}
	case isAgy:
		srv.agyStatus = AgyWiringStatus{State: AgyWired}
	default:
		srv.agyStatus = AgyWiringStatus{State: AgyNotWired,
			Reason: fmt.Sprintf("AgyBinaryPath is configured but the wired contributor adapter is %T, not the agy adapter", adp)}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/readiness", srv.handleReadiness)
	mux.HandleFunc("GET /v1/status", srv.handleStatus)
	mux.HandleFunc("POST /v1/runs", srv.handleCreateRun)
	mux.HandleFunc("POST /v1/runs/{run_id}/agy/attestations", srv.handleAgyAttestation)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/agy-disposition", srv.handleAgyTurnDisposition)
	mux.HandleFunc("GET /v1/runs/{run_id}", srv.handleGetRun)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/release", srv.handleRelease)
	mux.HandleFunc("GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}", srv.handleGetTurn)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/cancel", srv.handleCancel)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/reconcile", srv.handleReconcile)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/controller/connect", srv.handleControllerConnect)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/connect", srv.handleRunControllerConnect)
	mux.HandleFunc("GET /v1/runs/{run_id}/controller", srv.handleControllerRecordGet)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/adopt", srv.handleControllerAdopt)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/handoff", srv.handleControllerHandoff)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/revoke", srv.handleControllerRevoke)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/credential/recover", srv.handleControllerCredentialRecover)
	mux.HandleFunc("POST /v1/runs/{run_id}/controller/disconnect", srv.handleRunControllerDisconnect)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/prompts/queue", srv.handleQueuePrompt)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/replace", srv.handleReplacePrompt)
	mux.HandleFunc("POST /v1/runs/{run_id}/sessions/{session_id}/prompts/{turn_key}/discard", srv.handleDiscardPrompt)
	mux.HandleFunc("POST /v1/runs/{run_id}/decisions", srv.handleRecordDecision)
	mux.HandleFunc("POST /v1/runs/{run_id}/artifacts/release", srv.handleReleaseArtifacts)
	mux.HandleFunc("GET /v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/events", srv.handleEvents)
	mux.HandleFunc("POST /v1/service/stop", srv.handleStop)

	handler := authMiddleware(cfg.AuthToken, mux)

	srv.httpServer = &http.Server{
		Handler:        handler,
		MaxHeaderBytes: 1 << 20, // 1MB
	}

	return srv, nil
}

func (s *Server) WorkspaceManager() *workspace.WorkspaceManager {
	return s.workspaceManager
}

func (s *Server) PolicyExecutor() execpolicy.PolicyExecutor {
	return s.policyExecutor
}

func (s *Server) Handler() http.Handler {
	if s.httpServer != nil {
		return s.httpServer.Handler
	}
	return nil
}

func (s *Server) InstanceID() string {
	return s.cfg.InstanceID
}

func (s *Server) SocketPath() string {
	return s.socketPath
}

func (s *Server) TokenPath() string {
	return s.tokenPath
}

func (s *Server) LockPath() string {
	return s.lock.Path()
}

func (s *Server) Coordinator() *Coordinator {
	return s.coordinator
}

func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return errors.New("server already running")
	}

	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", s.socketPath, err)
	}
	_ = os.Chmod(s.socketPath, 0600)
	s.listener = l
	s.running = true

	// Initial state hydration and validation before accepting requests
	if s.store != nil {
		hydrateCtx, hydrateCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer hydrateCancel()
		if _, err := s.store.HydrateState(hydrateCtx); err != nil {
			_ = l.Close()
			return fmt.Errorf("hydrate state failed: %w", err)
		}
		liveMap := s.coordinator.LiveWorkerKeys()
		counts, err := s.store.GetDiagnosticCounts(hydrateCtx, liveMap)
		if err != nil {
			_ = l.Close()
			return fmt.Errorf("initial diagnostic counts failed: %w", err)
		}
		s.coordinator.SetRecoveryBlockers(counts.RecoveryBlockers)
	}

	go func() {
		_ = s.httpServer.Serve(l)
		_ = s.Teardown(5 * time.Second)
	}()

	return nil
}

func (s *Server) Close() error {
	return s.Teardown(5 * time.Second)
}

// WaitForShutdown blocks until teardown has completed. It returns the
// recorded teardown outcome: nil for orderly completion and a wrapped
// ErrForcedTeardown when the final deadline expired, so a timed-out teardown
// is never reported as orderly success.
func (s *Server) WaitForShutdown(ctx context.Context) error {
	select {
	case <-s.shutdown:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.teardownErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if s.coordinator.IsDrainingOrStopping() {
		code := "service_draining"
		if s.coordinator.State() == ServiceStateStopping {
			code = "service_stopping"
		}
		writeError(w, http.StatusServiceUnavailable, code, "service is not ready to accept new work", "")
		return
	}

	var reservedTurns, unresolvedTurns int
	if s.store != nil {
		epoch := s.coordinator.BlockerEpoch()
		liveMap := s.coordinator.LiveWorkerKeys()
		counts, err := s.store.GetDiagnosticCounts(r.Context(), liveMap)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "storage_error", fmt.Sprintf("diagnostics query failed: %v", err), "")
			return
		}
		s.coordinator.ApplyDiagnosticBlockers(counts.RecoveryBlockers, epoch)
		reservedTurns = counts.ReservedTurns
		unresolvedTurns = counts.UnresolvedTurns
	}

	resp := ReadinessResponse{
		Status:          "ready",
		InstanceID:      s.cfg.InstanceID,
		ProtocolVersion: 1,
		StateDir:        s.cfg.StateDir,
		LiveWorkers:     s.coordinator.LiveWorkers(),
		ReservedTurns:   reservedTurns,
		UnresolvedTurns: unresolvedTurns,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	statusStr := "ready"
	if s.coordinator.State() == ServiceStateDraining {
		statusStr = "draining"
	} else if s.coordinator.State() == ServiceStateStopping {
		statusStr = "stopping"
	}

	var reservedTurns, unresolvedTurns int
	activeRuns := []string{}
	if s.store != nil {
		epoch := s.coordinator.BlockerEpoch()
		liveMap := s.coordinator.LiveWorkerKeys()
		counts, err := s.store.GetDiagnosticCounts(r.Context(), liveMap)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "storage_error", fmt.Sprintf("diagnostics query failed: %v", err), "")
			return
		}
		s.coordinator.ApplyDiagnosticBlockers(counts.RecoveryBlockers, epoch)
		reservedTurns = counts.ReservedTurns
		unresolvedTurns = counts.UnresolvedTurns
		activeRuns = counts.ActiveRuns
	}

	resp := StatusResponse{
		InstanceID:      s.cfg.InstanceID,
		PID:             os.Getpid(),
		Status:          statusStr,
		StateDir:        s.cfg.StateDir,
		StartedAt:       s.startedAt,
		ActiveRuns:      activeRuns,
		LiveWorkers:     s.coordinator.LiveWorkers(),
		ReservedTurns:   reservedTurns,
		UnresolvedTurns: unresolvedTurns,
	}
	if s.agyStatus.State != "" && s.agyStatus.State != AgyNotConfigured {
		st := s.agyStatus
		resp.Agy = &st
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
