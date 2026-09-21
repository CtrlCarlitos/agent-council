package opencode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// Production implementations of the three seams required by
// OpenCodeAdapter. Each is backed by the real storage store and workspace
// manager, never by test doubles or synthesized profiles.

// storageDispatchIdentitySource resolves the attempt identity from the
// persisted dispatch intent in the storage store.
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

// storageSessionLaunchSource builds a session-specific LaunchRequest from
// the persisted frozen run profile and AC-005 workspace allocation.
type storageSessionLaunchSource struct {
	store *storage.Store
	wm    *workspace.WorkspaceManager
}

func (s *storageSessionLaunchSource) OpenCodeServeLaunch(ctx context.Context, sessionID adapter.SessionID) (execpolicy.LaunchRequest, error) {
	meta, err := s.store.GetSessionMetadata(ctx, string(sessionID))
	if err != nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("session metadata lookup failed: %w", err)
	}
	profileRec, err := s.store.GetRunProfile(ctx, meta.RunID)
	if err != nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("run profile lookup failed: %w", err)
	}
	if profileRec.Profile.AlgoVersion == "" || len(profileRec.Profile.Harnesses) == 0 {
		return execpolicy.LaunchRequest{}, fmt.Errorf("run %s has no frozen canonical profile", meta.RunID)
	}
	// Verify the session's contributor is OpenCode and that the frozen
	// profile contains an OpenCode harness entry in the tooling allowlist.
	// Require the session's contributor to be opencode — only OpenCode
	// sessions are served by this adapter.
	if meta.Contributor != "opencode" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("session %s contributor is %q, not opencode; cannot launch OpenCode serve", sessionID, meta.Contributor)
	}
	// Select exactly profile.Harnesses[meta.Contributor] and verify opencode
	// is in the tooling allowlist.
	if _, hasHarness := profileRec.Profile.Harnesses[meta.Contributor]; !hasHarness {
		return execpolicy.LaunchRequest{}, fmt.Errorf("frozen profile for run %s has no %q harness entry", meta.RunID, meta.Contributor)
	}
	toolAllowed := false
	for _, tool := range profileRec.Profile.Tooling {
		if tool == "opencode" {
			toolAllowed = true
			break
		}
	}
	if !toolAllowed {
		return execpolicy.LaunchRequest{}, fmt.Errorf("opencode is not in the tooling allowlist for run %s", meta.RunID)
	}
	paths, ok := s.wm.GetPaths(meta.RunID, string(sessionID))
	if !ok {
		paths, err = s.wm.AllocateWorkspace(meta.RunID, string(sessionID), profileRec.Profile.WorkspaceMode, profileRec.SourceRepoIdentity, profileRec.SourceCommit)
		if err != nil {
			return execpolicy.LaunchRequest{}, fmt.Errorf("allocate workspace: %w", err)
		}
	}
	return execpolicy.LaunchRequest{
		RunID:     meta.RunID,
		SessionID: string(sessionID),
		Command:   "opencode",
		Args:      []string{"serve", "--hostname", "127.0.0.1", "--port", "0"},
		Paths:     paths,
		Profile:   profileRec.Profile,
	}, nil
}

// operatorProbeLaunchTemplate produces validated LaunchRequest values for
// the Probe capability check. Constructed from explicit operator
// configuration: binary path, scratch root, and a minimal canonical
// profile that the operator approves for probe purposes. The adapter does
// not synthesize this profile — it is injected at construction.
type operatorProbeLaunchTemplate struct {
	binaryPath  string
	scratchRoot string
	profile     storage.CanonicalProfile
}

func (t *operatorProbeLaunchTemplate) VersionLaunch(ctx context.Context) (execpolicy.LaunchRequest, error) {
	if strings.TrimSpace(t.binaryPath) == "" {
		return execpolicy.LaunchRequest{}, errors.New("opencode binary path is required for version check")
	}
	return execpolicy.LaunchRequest{
		Command: t.binaryPath,
		Args:    []string{"--version"},
		Profile: t.profile,
	}, nil
}

func (t *operatorProbeLaunchTemplate) ProbeServeLaunch(ctx context.Context) (execpolicy.LaunchRequest, error) {
	if strings.TrimSpace(t.binaryPath) == "" {
		return execpolicy.LaunchRequest{}, errors.New("opencode binary path is required for probe serve")
	}
	if strings.TrimSpace(t.scratchRoot) == "" {
		return execpolicy.LaunchRequest{}, errors.New("scratchRoot is required for probe serve launch")
	}
	// The operator-owned template allocates the scratch directory; the
	// adapter never creates directories itself.
	scratchDir, err := os.MkdirTemp(t.scratchRoot, "ac-opencode-probe-")
	if err != nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("allocate probe scratch dir: %w", err)
	}
	return execpolicy.LaunchRequest{
		Command: t.binaryPath,
		Args:    []string{"serve", "--hostname", "127.0.0.1", "--port", "0"},
		Paths:   workspace.WorkspacePaths{Root: scratchDir, Config: filepath.Join(scratchDir, "config")},
		Profile: t.profile,
	}, nil
}

// NewProductionOpenCodeAdapter constructs a fully wired OpenCodeAdapter
// with production store-backed seams. Fail-closed: returns an error if any
// required dependency is nil.
func NewProductionOpenCodeAdapter(
	store *storage.Store,
	wm *workspace.WorkspaceManager,
	executor execpolicy.PolicyExecutor,
	probeTemplate ProbeLaunchTemplate,
	opts ...OpenCodeAdapterOption,
) (*OpenCodeAdapter, error) {
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
	identity := &storageDispatchIdentitySource{store: store}
	launch := &storageSessionLaunchSource{store: store, wm: wm}
	template := probeTemplate

	return NewOpenCodeAdapterWithLaunch(executor, template, identity, launch, opts...), nil
}

// buildProductionOpenCodeAdapter constructs a production adapter from
// explicit dependencies. Test-friendly wrapper for the wiring path.
func buildProductionOpenCodeAdapter(
	store *storage.Store,
	wm *workspace.WorkspaceManager,
	executor execpolicy.PolicyExecutor,
	probeTemplate ProbeLaunchTemplate,
) (*OpenCodeAdapter, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	if wm == nil {
		return nil, errors.New("workspace manager is required")
	}
	if executor == nil {
		return nil, errors.New("executor is required")
	}
	if probeTemplate == nil {
		return nil, errors.New("probe template is required")
	}
	identity := &storageDispatchIdentitySource{store: store}
	launch := &storageSessionLaunchSource{store: store, wm: wm}
	return NewOpenCodeAdapterWithLaunch(executor, probeTemplate, identity, launch), nil
}

// NewOperatorProbeLaunchTemplate creates an operator-owned probe launch
// template from explicit configuration.
func NewOperatorProbeLaunchTemplate(binaryPath, scratchRoot string) *operatorProbeLaunchTemplate {
	return &operatorProbeLaunchTemplate{binaryPath: binaryPath, scratchRoot: scratchRoot}
}
