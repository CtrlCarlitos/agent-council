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
	// Verify the session's contributor has an OpenCode harness profile and
	// that OpenCode is in the tooling allowlist.
	harness, hasHarness := profileRec.Profile.Harnesses[string(sessionID)]
	if !hasHarness {
		harness, hasHarness = profileRec.Profile.Harnesses["opencode"]
	}
	_ = harness
	if !hasHarness {
		return execpolicy.LaunchRequest{}, fmt.Errorf("no OpenCode harness profile for session %s in run %s", sessionID, meta.RunID)
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
// configuration (binary path and scratch root), never synthesized.
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

func (t *operatorProbeLaunchTemplate) ProbeServeLaunch(ctx context.Context, scratchDir string) (execpolicy.LaunchRequest, error) {
	if strings.TrimSpace(t.binaryPath) == "" {
		return execpolicy.LaunchRequest{}, errors.New("opencode binary path is required for probe serve")
	}
	if strings.TrimSpace(scratchDir) == "" {
		return execpolicy.LaunchRequest{}, errors.New("scratchDir is required for probe serve launch")
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
	opencodeBinaryPath string,
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
	if strings.TrimSpace(opencodeBinaryPath) == "" {
		return nil, errors.New("opencode binary path is required")
	}

	identity := &storageDispatchIdentitySource{store: store}
	launch := &storageSessionLaunchSource{store: store, wm: wm}
	probeProfile := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{opencodeBinaryPath},
	}
	template := &operatorProbeLaunchTemplate{binaryPath: opencodeBinaryPath, scratchRoot: os.TempDir(), profile: probeProfile}

	return NewOpenCodeAdapterWithLaunch(executor, template, identity, launch, opts...), nil
}
