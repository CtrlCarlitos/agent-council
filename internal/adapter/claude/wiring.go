package claude

// Production wiring for the Claude adapter (AC-008 Task 6): storage-
// backed identity seam, operator-owned probe launch template, fail-
// closed construction, and the Probe capability check (contract probes
// only — never a billing turn, never a native session).

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// storageDispatchIdentitySource resolves the attempt identity from the
// persisted dispatch intent in the storage store (same seam as AC-007).
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

// ClaudeProbeLaunchTemplate is the operator-owned builder producing
// validated LaunchRequest values for the Probe contract checks. The
// adapter never synthesizes Paths, Profile, or identifiers internally.
type ClaudeProbeLaunchTemplate interface {
	// VersionLaunch returns a validated LaunchRequest for
	// `claude --version`.
	VersionLaunch(ctx context.Context) (execpolicy.LaunchRequest, error)
	// HelpLaunch returns a validated LaunchRequest for `claude --help`.
	HelpLaunch(ctx context.Context) (execpolicy.LaunchRequest, error)
}

// requiredHelpFlags is the frozen launch-contract surface: a --help
// output missing any of these flags is protocol drift and fails the
// probe.
var requiredHelpFlags = []string{
	"-p", "--output-format", "--verbose", "--session-id", "--resume",
	"--model", "--max-turns", "--allowedTools", "--disallowedTools",
}

type operatorProbeLaunchTemplate struct {
	binaryPath  string
	scratchRoot string
	profile     storage.CanonicalProfile
}

func (t *operatorProbeLaunchTemplate) requireProfile() error {
	if t.profile.AlgoVersion == "" || len(t.profile.Harnesses) == 0 {
		return errors.New("operator probe template requires a non-empty canonical profile")
	}
	return nil
}

func (t *operatorProbeLaunchTemplate) validate() error {
	if strings.TrimSpace(t.binaryPath) == "" {
		return errors.New("claude binary path is required for contract probes")
	}
	if strings.TrimSpace(t.scratchRoot) == "" {
		return errors.New("claude probe scratch root is required for contract probes")
	}
	return t.requireProfile()
}

func (t *operatorProbeLaunchTemplate) contractLaunch(ctx context.Context, sessionID string, args []string) (execpolicy.LaunchRequest, error) {
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
	return t.contractLaunch(ctx, "sess-probe-version", []string{"--version"})
}

func (t *operatorProbeLaunchTemplate) HelpLaunch(ctx context.Context) (execpolicy.LaunchRequest, error) {
	return t.contractLaunch(ctx, "sess-probe-help", []string{"--help"})
}

// NewOperatorProbeLaunchTemplate constructs the operator-owned probe
// launch template from explicit configuration, including the operator-
// approved canonical probe profile the PolicyExecutor requires.
func NewOperatorProbeLaunchTemplate(binaryPath, scratchRoot string, profile storage.CanonicalProfile) ClaudeProbeLaunchTemplate {
	return &operatorProbeLaunchTemplate{binaryPath: binaryPath, scratchRoot: scratchRoot, profile: profile}
}

// NewProductionClaudeAdapter constructs a fully wired ClaudeAdapter
// with production store-backed seams. Fail-closed: nil dependencies,
// empty configuration, or a missing probe template are construction
// errors, never runtime surprises.
func NewProductionClaudeAdapter(
	store *storage.Store,
	wm *workspace.WorkspaceManager,
	executor execpolicy.PolicyExecutor,
	probeTemplate ClaudeProbeLaunchTemplate,
	configBase string,
	templateDir string,
	evidenceRoot string,
) (*ClaudeAdapter, error) {
	if store == nil {
		return nil, errors.New("storage store is required")
	}
	if wm == nil {
		return nil, errors.New("workspace manager is required")
	}
	if executor == nil {
		return nil, errors.New("policy executor is required")
	}
	if probeTemplate == nil {
		return nil, errors.New("probe launch template is required")
	}
	if strings.TrimSpace(configBase) == "" {
		return nil, errors.New("claude config base directory is required")
	}
	if strings.TrimSpace(templateDir) == "" {
		return nil, errors.New("claude config template directory is required")
	}
	if strings.TrimSpace(evidenceRoot) == "" {
		return nil, errors.New("claude evidence root is required")
	}
	identity := &storageDispatchIdentitySource{store: store}
	launchSource := NewClaudeTurnLaunchSource(store, wm, configBase, evidenceRoot)
	adp := NewClaudeAdapter(store, wm, executor, launchSource, identity, configBase, templateDir)
	adp.probeTemplate = probeTemplate
	// Fail closed at construction: a probe template that cannot produce
	// its contract launches (missing binary path, scratch root, or an
	// empty profile) must fail wiring, not the first probe call.
	if _, err := probeTemplate.VersionLaunch(context.Background()); err != nil {
		return nil, fmt.Errorf("probe template: %w", err)
	}
	return adp, nil
}

// Probe performs provider-free executable contract checks: `claude
// --version` (native identity marker + version capture) and `claude
// --help` (frozen launch-contract surface). Both children run through
// the PolicyExecutor in the template-owned scratch directory, both
// output pipes are drained for the child lifetime, and no native
// session is created or inspected. Native authentication status is
// reported unknown: Council never reads credentials.
func (a *ClaudeAdapter) Probe(ctx context.Context) (adapter.ProbeReport, error) {
	report := adapter.ProbeReport{
		Capabilities: adapter.AdapterCapabilities{
			StreamingObservation: adapter.CapabilitySupported,
			SessionResumption:    adapter.CapabilitySupported,
			MidTurnCancellation:  adapter.CapabilitySupported,
			ToolApprovalRouting:  adapter.CapabilityUnsupported,
			StructuredOutput:     adapter.CapabilitySupported,
		},
	}
	if a.probeTemplate == nil {
		return report, errors.New("probe launch template is required")
	}

	versionOut, err := runContractProbe(ctx, a.executor, a.probeTemplate.VersionLaunch)
	if err != nil {
		return report, fmt.Errorf("version probe: %w", err)
	}
	version := strings.TrimSpace(versionOut)
	if !strings.Contains(version, "(Claude Code)") {
		return report, fmt.Errorf("claude --version output %q lacks the native identity marker", version)
	}
	report.HarnessVersion = adapter.UsageMetric[string]{Value: version, Available: true}

	helpOut, err := runContractProbe(ctx, a.executor, a.probeTemplate.HelpLaunch)
	if err != nil {
		return report, fmt.Errorf("help probe: %w", err)
	}
	for _, flag := range requiredHelpFlags {
		if !strings.Contains(helpOut, flag) {
			return report, fmt.Errorf("claude --help is missing required launch flag %q (protocol drift)", flag)
		}
	}
	return report, nil
}

// runContractProbe starts one probe child, drains stderr for the child
// lifetime, captures stdout, and reaps the process.
func runContractProbe(
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
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		sc := bufio.NewScanner(proc.Stdout())
		for sc.Scan() {
			out.WriteString(sc.Text() + "\n")
		}
	}()
	go drainReader(proc.Stderr())
	_, waitErr := proc.Wait()
	<-outDone
	if waitErr != nil {
		return "", fmt.Errorf("probe child failed: %w", waitErr)
	}
	return out.String(), nil
}
