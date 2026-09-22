package claude

// ClaudeTurnLaunchSource is the service-owned, storage-backed deep
// module that builds the complete `claude -p` LaunchRequest from the
// frozen run profile and the AC-005 workspace allocation (spec §3.7).
// The adapter never synthesizes policy inputs.

import (
	"context"
	"fmt"
	"sort"
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
	store        *storage.Store
	wm           *workspace.WorkspaceManager
	configBase   string // trusted claude_config_base_dir
	evidenceRoot string // trusted root for the pinned universe evidence
}

// NewClaudeTurnLaunchSource constructs the launch source. configBase is
// the trusted claude_config_base_dir validated by the service;
// evidenceRoot is the trusted service-owned root holding the pinned
// native-tool-universe evidence file. The turns bound is NOT accepted
// here: it is frozen in the profile manifest.
func NewClaudeTurnLaunchSource(store *storage.Store, wm *workspace.WorkspaceManager, configBase, evidenceRoot string) *ClaudeTurnLaunchSource {
	return &ClaudeTurnLaunchSource{
		store:        store,
		wm:           wm,
		configBase:   strings.TrimSpace(configBase),
		evidenceRoot: strings.TrimSpace(evidenceRoot),
	}
}

// ClaudeTurnLaunch builds the launch request for one turn on the exact
// native session. firstTurn selects --session-id (materialization);
// otherwise the request resumes the exact native session.
func (s *ClaudeTurnLaunchSource) ClaudeTurnLaunch(ctx context.Context, sessionID adapter.SessionID, nativeID string, firstTurn bool, promptDigest string) (execpolicy.LaunchRequest, error) {
	if strings.TrimSpace(nativeID) == "" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("native session id is required")
	}
	if strings.TrimSpace(promptDigest) == "" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("prompt digest is required for durable acceptance correlation")
	}
	if s.configBase == "" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("claude config base is required")
	}
	if s.evidenceRoot == "" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("claude evidence root is required")
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

	// The frozen deny complement is VERIFIED, not trusted: re-resolve the
	// pinned universe against the evidence root, check the CLI version,
	// require approved ⊆ universe, and recompute universe − approved. Any
	// mismatch with the frozen complement rejects the launch.
	manifest := profileRec.Profile.ToolkitManifest.ToolkitManifest
	universe, err := ResolveUniverseEvidence(s.evidenceRoot, manifest.UniverseEvidencePath, manifest.UniverseEvidenceDigest)
	if err != nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("pinned tool universe verification: %w", err)
	}
	if universe.ClaudeCodeVersion != manifest.ProbedCLIVersion {
		return execpolicy.LaunchRequest{}, fmt.Errorf(
			"pinned universe is for CLI %q but the manifest froze %q",
			universe.ClaudeCodeVersion, manifest.ProbedCLIVersion)
	}
	universeSet := make(map[string]struct{}, len(universe.Tools))
	for _, tool := range universe.Tools {
		universeSet[tool] = struct{}{}
	}
	approvedSet := make(map[string]struct{}, len(manifest.ApprovedTools))
	for _, tool := range manifest.ApprovedTools {
		if _, in := universeSet[tool]; !in {
			return execpolicy.LaunchRequest{}, fmt.Errorf(
				"approved tool %q is absent from the pinned native tool universe", tool)
		}
		approvedSet[tool] = struct{}{}
	}
	recomputed := make([]string, 0, len(universe.Tools))
	for _, tool := range universe.Tools {
		if _, approved := approvedSet[tool]; !approved {
			recomputed = append(recomputed, tool)
		}
	}
	sort.Strings(recomputed)
	frozen := append([]string(nil), manifest.DeniedComplement...)
	sort.Strings(frozen)
	if strings.Join(recomputed, "\x00") != strings.Join(frozen, "\x00") {
		return execpolicy.LaunchRequest{}, fmt.Errorf(
			"frozen denied complement does not match universe−approved (frozen %v, recomputed %v)",
			frozen, recomputed)
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		identityFlag, nativeID,
		"--model", model,
		"--max-turns", fmt.Sprintf("%d", manifest.TurnsBound),
	}
	if len(manifest.ApprovedTools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, manifest.ApprovedTools...)
	}
	if len(frozen) > 0 {
		args = append(args, "--disallowedTools")
		args = append(args, frozen...)
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
		PromptDigest:        promptDigest,
	}, nil
}
