package claude

// ClaudeTurnLaunchSource is the service-owned, storage-backed deep
// module that builds the complete `claude -p` LaunchRequest from the
// frozen run profile and the AC-005 workspace allocation (spec §3.7).
// The adapter never synthesizes policy inputs.

import (
	"context"
	"fmt"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const claudeCommand = "claude"

// ClaudeTurnLaunchSource builds validated turn LaunchRequests for the
// Claude adapter from the frozen profile, workspace allocation, and
// operator-provisioned per-session config roots.
type ClaudeTurnLaunchSource struct {
	store      *storage.Store
	wm         *workspace.WorkspaceManager
	turnBound  int    // --max-turns bound (operator-configured)
	configBase string // trusted claude_config_base_dir
}

// NewClaudeTurnLaunchSource constructs the launch source. turnBound must
// be positive: every invocation is bounded. configBase is the trusted
// claude_config_base_dir validated by the service.
func NewClaudeTurnLaunchSource(store *storage.Store, wm *workspace.WorkspaceManager, turnBound int, configBase string) *ClaudeTurnLaunchSource {
	return &ClaudeTurnLaunchSource{store: store, wm: wm, turnBound: turnBound, configBase: strings.TrimSpace(configBase)}
}

// ClaudeTurnLaunch builds the launch request for one turn on the exact
// native session. firstTurn selects --session-id (materialization);
// otherwise the request resumes the exact native session.
func (s *ClaudeTurnLaunchSource) ClaudeTurnLaunch(ctx context.Context, sessionID adapter.SessionID, nativeID string, firstTurn bool) (execpolicy.LaunchRequest, error) {
	if strings.TrimSpace(nativeID) == "" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("native session id is required")
	}
	if s.turnBound <= 0 {
		return execpolicy.LaunchRequest{}, fmt.Errorf("turn bound must be positive, got %d", s.turnBound)
	}
	if s.configBase == "" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("claude config base is required")
	}

	meta, err := s.store.GetSessionMetadata(ctx, string(sessionID))
	if err != nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("session metadata lookup failed: %w", err)
	}
	if meta.Contributor != "claude" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("session %s contributor is %q, not claude; cannot launch Claude turns", sessionID, meta.Contributor)
	}
	profileRec, err := s.store.GetRunProfile(ctx, meta.RunID)
	if err != nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("run profile lookup failed: %w", err)
	}
	// Claude requires the frozen toolkit manifest: cprof-v1 profiles are
	// ineligible (fail closed, typed error).
	if err := profileRec.Profile.ValidateForClaude(); err != nil {
		return execpolicy.LaunchRequest{}, err
	}
	spec, ok := profileRec.Profile.Harnesses["claude"]
	if !ok {
		return execpolicy.LaunchRequest{}, fmt.Errorf("frozen profile for run %s has no %q harness entry", meta.RunID, "claude")
	}
	model := strings.TrimSpace(spec.Model)
	if model == "" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("frozen claude harness has no model")
	}

	paths, ok := s.wm.GetPaths(meta.RunID, string(sessionID))
	if !ok {
		paths, err = s.wm.AllocateWorkspace(meta.RunID, string(sessionID), profileRec.Profile.WorkspaceMode, profileRec.SourceRepoIdentity, profileRec.SourceCommit)
		if err != nil {
			return execpolicy.LaunchRequest{}, fmt.Errorf("allocate workspace: %w", err)
		}
	}

	configDir := ConfigRootPath(s.configBase, meta.RunID, string(sessionID))

	identityFlag := "--resume"
	if firstTurn {
		identityFlag = "--session-id"
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		identityFlag, nativeID,
		"--model", model,
		"--max-turns", fmt.Sprintf("%d", s.turnBound),
	}
	manifest := profileRec.Profile.ToolkitManifest.ToolkitManifest
	if len(manifest.ApprovedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, manifest.ApprovedTools...)
	}
	if len(manifest.DeniedComplement) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, manifest.DeniedComplement...)
	}

	return execpolicy.LaunchRequest{
		RunID:               meta.RunID,
		SessionID:           string(sessionID),
		Command:             claudeCommand,
		Args:                args,
		Paths:               paths,
		Profile:             profileRec.Profile,
		ClaudeConfigDir:     configDir,
		ClaudeConfigBaseDir: s.configBase,
	}, nil
}
