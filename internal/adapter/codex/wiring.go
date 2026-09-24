package codex

// Production wiring for the codex adapter (AC-009 Task 9): the
// operator-owned probe launch template (§3.8 probe surfaces — provider-
// free, scratch cwd, never a turn), the session child launch source, the
// storage-backed attempt-identity seam, and the fail-closed production
// constructor. The constructor ALWAYS wires the §3.3 eligibility lookup
// to the durable cprot-v2 rows (storage.FindCodexProtectionAttestation,
// manifest digest from storage.ComputeToolkitManifestDigest); there is
// no production path to the test-only fixture scope and no inference of
// eligibility from absent evidence.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// CodexProbeLaunchTemplate is the operator-owned builder producing the
// validated LaunchRequest values for the §3.8 probe template. The probe
// is provider-free and never sends a turn: version, daemon version, and
// login status are plain executions; the probe child is the pinned
// app-server argv used ONLY for the initialize handshake, the negative
// thread/resume contract check, model/list, and mcpServerStatus/list.
// The adapter never synthesizes Paths, Profile, or identifiers itself.
type CodexProbeLaunchTemplate interface {
	// VersionLaunch returns a validated LaunchRequest for
	// `codex --version`.
	VersionLaunch(ctx context.Context) (execpolicy.LaunchRequest, error)
	// DaemonVersionLaunch returns a validated LaunchRequest for
	// `codex app-server daemon version` (the version-skew surface).
	DaemonVersionLaunch(ctx context.Context) (execpolicy.LaunchRequest, error)
	// LoginStatusLaunch returns a validated LaunchRequest for
	// `codex login status` (auth evidence without secrets).
	LoginStatusLaunch(ctx context.Context) (execpolicy.LaunchRequest, error)
	// ProbeChildLaunch returns a validated LaunchRequest for the pinned
	// app-server probe child. Its working directory is the scratch
	// directory; the probe never touches a workspace root.
	ProbeChildLaunch(ctx context.Context) (execpolicy.LaunchRequest, error)
}

type operatorProbeLaunchTemplate struct {
	binaryPath  string
	scratchRoot string
	profile     storage.CanonicalProfile
}

func (t *operatorProbeLaunchTemplate) validate() error {
	if strings.TrimSpace(t.binaryPath) == "" {
		return errors.New("codex binary path is required for probe launches")
	}
	if strings.TrimSpace(t.scratchRoot) == "" {
		return errors.New("codex probe scratch root is required for probe launches")
	}
	if t.profile.AlgoVersion == "" || len(t.profile.Harnesses) == 0 {
		return errors.New("operator probe template requires a non-empty canonical profile")
	}
	return nil
}

func (t *operatorProbeLaunchTemplate) probeLaunch(_ context.Context, sessionID string, args []string) (execpolicy.LaunchRequest, error) {
	if err := t.validate(); err != nil {
		return execpolicy.LaunchRequest{}, err
	}
	return execpolicy.LaunchRequest{
		RunID:     "run-probe",
		SessionID: sessionID,
		Command:   t.binaryPath,
		Args:      args,
		Paths:     workspace.WorkspacePaths{Root: t.scratchRoot, Config: t.scratchRoot},
		Profile:   t.profile,
	}, nil
}

func (t *operatorProbeLaunchTemplate) VersionLaunch(ctx context.Context) (execpolicy.LaunchRequest, error) {
	return t.probeLaunch(ctx, "sess-probe-version", []string{"--version"})
}

func (t *operatorProbeLaunchTemplate) DaemonVersionLaunch(ctx context.Context) (execpolicy.LaunchRequest, error) {
	return t.probeLaunch(ctx, "sess-probe-daemon", []string{"app-server", "daemon", "version"})
}

func (t *operatorProbeLaunchTemplate) LoginStatusLaunch(ctx context.Context) (execpolicy.LaunchRequest, error) {
	return t.probeLaunch(ctx, "sess-probe-login", []string{"login", "status"})
}

func (t *operatorProbeLaunchTemplate) ProbeChildLaunch(ctx context.Context) (execpolicy.LaunchRequest, error) {
	return t.probeLaunch(ctx, "sess-probe-child", append([]string(nil), CodexAppServerArgs...))
}

// NewCodexProbeLaunchTemplate constructs the operator-owned probe launch
// template from explicit configuration, including the operator-approved
// canonical profile the PolicyExecutor requires.
func NewCodexProbeLaunchTemplate(binaryPath, scratchRoot string, profile storage.CanonicalProfile) CodexProbeLaunchTemplate {
	return &operatorProbeLaunchTemplate{binaryPath: binaryPath, scratchRoot: scratchRoot, profile: profile}
}

// ── Session child launch source ─────────────────────────────────────────

// codexSessionLaunchSource is the production ChildLaunchSource: the
// exact pinned app-server argv, the operator-provisioned neutral scratch
// directory as the child's working directory (never a workspace root),
// and the frozen run profile. CODEX_HOME is never set by the adapter.
type codexSessionLaunchSource struct {
	binaryPath  string
	scratchRoot string
	profile     storage.CanonicalProfile
	runID       string
}

func (s codexSessionLaunchSource) CodexAppServerLaunch(_ context.Context, sessionID adapter.SessionID) (execpolicy.LaunchRequest, error) {
	if strings.TrimSpace(s.binaryPath) == "" {
		return execpolicy.LaunchRequest{}, errors.New("codex binary path is required for child launches")
	}
	if strings.TrimSpace(s.scratchRoot) == "" {
		return execpolicy.LaunchRequest{}, errors.New("codex scratch root is required for child launches")
	}
	return execpolicy.LaunchRequest{
		RunID:     s.runID,
		SessionID: string(sessionID),
		Command:   s.binaryPath,
		Args:      append([]string(nil), CodexAppServerArgs...),
		Paths:     workspace.WorkspacePaths{Root: s.scratchRoot, Config: s.scratchRoot},
		Profile:   s.profile,
	}, nil
}

// NewCodexSessionLaunchSource constructs the production child launch
// source over the operator-provisioned scratch root and frozen profile.
func NewCodexSessionLaunchSource(binaryPath, scratchRoot string, profile storage.CanonicalProfile) ChildLaunchSource {
	return codexSessionLaunchSource{binaryPath: binaryPath, scratchRoot: scratchRoot, profile: profile, runID: "run-codex-children"}
}

// ── Storage-backed seams ────────────────────────────────────────────────

// storageDispatchIdentitySource resolves the attempt identity from the
// persisted dispatch intent in the storage store (same seam as
// AC-007/AC-008).
type storageDispatchIdentitySource struct {
	store *storage.Store
}

func (s *storageDispatchIdentitySource) AttemptFor(ctx context.Context, ref adapter.TurnRef) (string, bool) {
	details, err := s.store.GetTurnDetails(ctx, string(ref.SessionID), ref.TurnKey)
	if err != nil || details == nil || details.DispatchIntent == nil {
		return "", false
	}
	attempt := details.DispatchIntent.AttemptID
	if attempt == "" {
		return "", false
	}
	return attempt, true
}

// storageAttestationLookup is the production §3.3 eligibility lookup:
// the durable cprot-v2 rows are consulted on EVERY call (freeze-at-
// launch — every CreateSession/Dispatch re-checks), matching the frozen
// (codex version, platform, manifest digest, profile digest) tuple
// exactly AND requiring the row's records to cover the frozen profile
// (coverage.go). The manifest digest comes from the SAME frozen policy
// the launch freezes (ValidateCodexHarness → storage.ComputeToolkit
// ManifestDigest), so the tuple match can never disagree on manifest
// identity. A missing row, a lookup failure, any tuple disagreement, or
// an uncovered record set reports NOT eligible: there is no inference
// of eligibility from absent, drifted, or partial evidence.
func storageAttestationLookup(store *storage.Store, policy CodexLaunchPolicy, profileDigest string) AttestationLookup {
	return func() (string, bool) {
		id, err := lookupCoveredAttestation(context.Background(), store, policy, profileDigest)
		if err != nil || strings.TrimSpace(id) == "" {
			return "", false
		}
		return id, true
	}
}

// ── Probe template runner ───────────────────────────────────────────────

// LaunchProbeReport is one operator probe run's provider-free evidence:
// version surfaces, the credential-free model catalog, the MCP server
// inventory, the login-status capture, and the verbatim negative-resume
// contract result. No turn is ever sent; no secret is ever captured.
type LaunchProbeReport struct {
	// CodexVersion is the parsed `codex --version` version token.
	CodexVersion string
	// RawVersionOutput is the full trimmed `codex --version` output.
	RawVersionOutput string
	// DaemonVersionOutput is the trimmed `app-server daemon version`
	// output (version-skew surface).
	DaemonVersionOutput string
	// Models is the raw model/list catalog (credential-free inventory).
	Models string
	// MCPServerStatus is the raw mcpServerStatus/list inventory.
	MCPServerStatus string
	// LoginStatusOutput is the trimmed `codex login status` capture
	// (auth evidence without secrets).
	LoginStatusOutput string
	// NegativeResume carries the verbatim deterministic missing-thread
	// contract from the probe child's thread/resume against the
	// canonical zero UUID (code -32600, "no rollout found …").
	NegativeResume *ErrNativeSessionMissing
}

// probeZeroThreadID is the canonical zero UUID the negative thread/resume
// probe presents: no rollout can exist for it, so the deterministic
// missing-thread error is the only honest outcome.
const probeZeroThreadID = "00000000-0000-0000-0000-000000000000"

// RunLaunchProbe executes the operator-owned probe template (§3.8):
// `codex --version`, `app-server daemon version`, the probe child
// (initialize handshake, NEGATIVE thread/resume with the canonical zero
// UUID expecting the verbatim `no rollout found … (code -32600)`
// contract, model/list, mcpServerStatus/list), and `codex login status`.
// Every surface is provider-free; the probe NEVER sends a turn. The
// negative-resume contract failing anything other than the verbatim
// missing-thread error fails the probe closed.
func RunLaunchProbe(ctx context.Context, executor execpolicy.PolicyExecutor, template CodexProbeLaunchTemplate) (LaunchProbeReport, error) {
	report := LaunchProbeReport{}
	if executor == nil {
		return report, errors.New("policy executor is required for the codex probe")
	}
	if template == nil {
		return report, errors.New("probe launch template is required for the codex probe")
	}

	versionRaw, err := runProbeExec(ctx, executor, template.VersionLaunch)
	if err != nil {
		return report, fmt.Errorf("version probe: %w", err)
	}
	report.RawVersionOutput = strings.TrimSpace(versionRaw)
	version, err := parseCodexVersion(report.RawVersionOutput)
	if err != nil {
		return report, err
	}
	report.CodexVersion = version

	daemonRaw, err := runProbeExec(ctx, executor, template.DaemonVersionLaunch)
	if err != nil {
		return report, fmt.Errorf("daemon version probe: %w", err)
	}
	report.DaemonVersionOutput = strings.TrimSpace(daemonRaw)

	models, mcp, missing, err := runProbeChild(ctx, executor, template)
	if err != nil {
		return report, err
	}
	report.Models = models
	report.MCPServerStatus = mcp
	report.NegativeResume = missing

	loginRaw, err := runProbeExec(ctx, executor, template.LoginStatusLaunch)
	if err != nil {
		return report, fmt.Errorf("login status probe: %w", err)
	}
	report.LoginStatusOutput = strings.TrimSpace(loginRaw)

	return report, nil
}

// parseCodexVersion requires the native identity marker: the first
// `codex --version` field must be `codex-cli`, and a version token must
// follow. Anything else is protocol drift.
func parseCodexVersion(out string) (string, error) {
	fields := strings.Fields(out)
	if len(fields) < 2 || fields[0] != "codex-cli" || strings.TrimSpace(fields[1]) == "" {
		return "", fmt.Errorf("codex --version output %q lacks the native identity marker (codex-cli <version>)", out)
	}
	return fields[1], nil
}

// runProbeChild performs the probe-child contract: the pinned app-server
// child in the scratch cwd, initialize handshake, NEGATIVE thread/resume
// (canonical zero UUID ⇒ verbatim missing-thread contract, else the
// probe fails closed), model/list, and mcpServerStatus/list. The child
// is terminated with pipes drained before the process is reaped; no turn
// is ever sent.
func runProbeChild(
	ctx context.Context,
	executor execpolicy.PolicyExecutor,
	template CodexProbeLaunchTemplate,
) (models string, mcp string, missing *ErrNativeSessionMissing, err error) {
	req, err := template.ProbeChildLaunch(ctx)
	if err != nil {
		return "", "", nil, fmt.Errorf("probe child launch: %w", err)
	}
	proc, err := executor.Start(ctx, req)
	if err != nil {
		return "", "", nil, fmt.Errorf("probe child execution: %w", err)
	}

	// stderr is drained once for the child lifetime (bounded tail kept
	// for diagnostics, never parsed) — an undrained pipe blocks the child.
	stderrTail := newBoundedBuffer(8 << 10)
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(stderrTail, proc.Stderr())
	}()

	conn := NewConn(proc.Stdin(), proc.Stdout())
	pump := NewEventPump()
	conn.SetNotificationHandler(pump.HandleNotification)
	// The probe never creates threads; a fatal on the probe connection
	// fails the probe, nothing else.
	conn.SetFatalHandler(func(cause error) {})
	pump.SetFatalHandler(func(cause error) {})
	conn.Start()

	client := NewCodexClient(conn, pump)
	fail := func(cause error) (string, string, *ErrNativeSessionMissing, error) {
		conn.Close()
		termCtx, cancel := context.WithTimeout(context.Background(), terminateGrace)
		defer cancel()
		_ = proc.Terminate(termCtx)
		<-stderrDone
		_, _ = proc.Wait()
		return "", "", nil, cause
	}

	init, err := client.Initialize(ctx, ClientInfo{Name: CouncilClientName, Version: buildVersion()})
	if err != nil {
		return fail(fmt.Errorf("probe child initialize: %w (stderr tail: %q)", err, stderrTail.String()))
	}
	_ = init // attestation shape captured live; the frozen compare stays the session child's gate

	// NEGATIVE resume contract: the canonical zero UUID has no rollout,
	// so ONLY the verbatim deterministic missing-thread error is honest.
	// Any other outcome (success, timeout, different shape) fails the
	// probe closed.
	if _, rerr := client.ResumeProbe(probeZeroThreadID); rerr == nil {
		return fail(fmt.Errorf("negative resume probe against %s unexpectedly SUCCEEDED; the deterministic missing-thread contract does not hold", probeZeroThreadID))
	} else {
		var m *ErrNativeSessionMissing
		if !errors.As(rerr, &m) {
			return fail(fmt.Errorf("negative resume probe against %s produced %T (%v); want the verbatim missing-thread contract (%d: %s…)",
				probeZeroThreadID, rerr, rerr, missingThreadCode, missingThreadPrefix))
		}
		missing = m
	}

	modelsRaw, err := client.ModelList(ctx)
	if err != nil {
		return fail(fmt.Errorf("probe child model/list: %w", err))
	}
	mcpRaw, err := client.MCPServerStatusList(ctx)
	if err != nil {
		return fail(fmt.Errorf("probe child mcpServerStatus/list: %w", err))
	}

	conn.Close()
	termCtx, cancel := context.WithTimeout(context.Background(), terminateGrace)
	defer cancel()
	_ = proc.Terminate(termCtx)
	<-stderrDone
	if _, werr := proc.Wait(); werr != nil {
		// The contract surfaces were already captured; a non-zero exit
		// after terminate is reaping evidence, not a probe failure.
		_ = werr
	}
	return string(modelsRaw), string(mcpRaw), missing, nil
}

// runProbeExec starts one plain probe child (version / daemon version /
// login status), drains both pipes for the child lifetime, captures
// stdout, and reaps the process. A stdout read failure is a probe
// failure: truncated output never passes.
func runProbeExec(
	ctx context.Context,
	executor execpolicy.PolicyExecutor,
	launch func(ctx context.Context) (execpolicy.LaunchRequest, error),
) (string, error) {
	req, err := launch(ctx)
	if err != nil {
		return "", err
	}
	proc, err := executor.Start(ctx, req)
	if err != nil {
		return "", fmt.Errorf("probe execution: %w", err)
	}
	var out strings.Builder
	var scanErr error
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		sc := bufio.NewScanner(proc.Stdout())
		for sc.Scan() {
			out.WriteString(sc.Text() + "\n")
		}
		scanErr = sc.Err()
	}()
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(io.Discard, proc.Stderr())
	}()
	// StdoutPipe/StderrPipe readers must reach EOF before Wait reaps the
	// child and closes the descriptors.
	<-outDone
	<-stderrDone
	if _, waitErr := proc.Wait(); waitErr != nil {
		return "", fmt.Errorf("probe child failed: %w", waitErr)
	}
	if scanErr != nil {
		return "", fmt.Errorf("probe stdout read failed: %w", scanErr)
	}
	return out.String(), nil
}

// ── Production constructor ──────────────────────────────────────────────

// NewProductionCodexAdapter constructs the fully wired production
// CodexAdapter (AC-009 §3.3): storage-backed identity seam, the
// eligibility lookup bound to the durable cprot-v2 rows for the frozen
// (version, platform, manifest, profile) tuple, the frozen launch policy
// validated against the committed evidence (universe re-hash), and the
// operator-owned probe template. Fail closed at construction: nil
// dependencies, incomplete configuration, an incomplete codex block, or
// evidence drift fail the WIRING — never the first launch. The variadic
// option surface is deliberately absent: production wiring passes no
// construction options, so the test-only fixture scope is structurally
// unreachable from this constructor.
func NewProductionCodexAdapter(
	store *storage.Store,
	executor execpolicy.PolicyExecutor,
	probeTemplate CodexProbeLaunchTemplate,
	profile storage.CanonicalProfile,
	evidenceRoot string,
) (*CodexAdapter, error) {
	if store == nil {
		return nil, errors.New("storage store is required")
	}
	if executor == nil {
		return nil, errors.New("policy executor is required")
	}
	if probeTemplate == nil {
		return nil, errors.New("probe launch template is required")
	}
	if strings.TrimSpace(evidenceRoot) == "" {
		return nil, errors.New("codex evidence root is required")
	}
	if profile.AlgoVersion == "" || len(profile.Harnesses) == 0 {
		return nil, errors.New("codex production construction requires the frozen run profile")
	}

	// Frozen launch policy (§3.8): fails closed on an incomplete codex
	// block, a missing toolkit manifest, or universe-evidence drift.
	// The policy carries the manifest digest from the SINGLE canonical
	// source (storage.ComputeToolkitManifestDigest).
	policy, err := ValidateCodexHarness(profile, evidenceRoot)
	if err != nil {
		return nil, fmt.Errorf("frozen codex policy: %w", err)
	}
	digest, _, err := storage.ComputeProfileDigest(profile)
	if err != nil {
		return nil, fmt.Errorf("frozen profile digest: %w", err)
	}

	// Fail closed at construction: a probe template that cannot produce
	// its launches (missing binary path, scratch root, or an empty
	// profile) must fail the wiring, not the first probe call.
	for _, launch := range []struct {
		name string
		fn   func(ctx context.Context) (execpolicy.LaunchRequest, error)
	}{
		{"version", probeTemplate.VersionLaunch},
		{"daemon version", probeTemplate.DaemonVersionLaunch},
		{"login status", probeTemplate.LoginStatusLaunch},
		{"probe child", probeTemplate.ProbeChildLaunch},
	} {
		if _, err := launch.fn(context.Background()); err != nil {
			return nil, fmt.Errorf("probe template %s launch: %w", launch.name, err)
		}
	}

	server := NewCodexServer(executor, NewCodexSessionLaunchSource(
		probeLaunchBinary(probeTemplate), probeScratchRoot(probeTemplate), profile), policy)
	identity := &storageDispatchIdentitySource{store: store}
	attestation := storageAttestationLookup(store, policy, digest)
	return NewCodexAdapter(store, server, policy, digest, identity, attestation)
}

// probeLaunchBinary/Root recover the validated binary path and scratch
// root from the template's own launch output (the template validated
// them above): the session launch source pins the same operator-
// provisioned values the probes run with.
func probeLaunchBinary(template CodexProbeLaunchTemplate) string {
	req, err := template.VersionLaunch(context.Background())
	if err != nil {
		return ""
	}
	return req.Command
}

func probeScratchRoot(template CodexProbeLaunchTemplate) string {
	req, err := template.ProbeChildLaunch(context.Background())
	if err != nil {
		return ""
	}
	return req.Paths.Root
}
