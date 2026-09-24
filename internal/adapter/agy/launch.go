package agy

// Launch assembly for the agy adapter (AC-010 spec §3.1): the
// service-owned, storage-backed launch source builds every agy
// LaunchRequest from the run's frozen profile and the AC-005 workspace
// allocation — the adapter never synthesizes policy inputs — and the
// adapter re-validates the assembled argv against the EXACT frozen
// grammar before any process starts (the closing check behind the
// executor's forbidden-flag guard, which does not see single-dash or
// otherwise unexpected forms):
//
//	--print= --input-format stream-json --output-format stream-json
//	--disable-slash-commands --model <frozen> --print-timeout <N>s
//	--log-file <launch log> [--mode <frozen>] [--sandbox]
//	[--conversation <native id>]
//
// The prompt is NEVER an argv element (it travels on stdin). The launch
// log lives under the AC-005 allocation's scratch directory, never under
// HOME. The launch matrix (isolation strictness × network mode) is
// evaluated before any process starts.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/workspace"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// LaunchKind selects the frozen argv shape of one agy launch.
type LaunchKind int

const (
	// LaunchCreate is the provider-free creation launch: frozen argv
	// without --conversation; stdin is closed without any message.
	LaunchCreate LaunchKind = iota + 1
	// LaunchTurn is one process-per-turn launch resuming the bound
	// conversation (--conversation <native id>).
	LaunchTurn
	// LaunchModels is the `agy models` auth-gate inventory launch (Task 6
	// consumes it; no model call).
	LaunchModels
	// LaunchPluginList is the provider-free `agy plugin list` capture
	// (Task 6 consumes it).
	LaunchPluginList
)

func (k LaunchKind) String() string {
	switch k {
	case LaunchCreate:
		return "create"
	case LaunchTurn:
		return "turn"
	case LaunchModels:
		return "models"
	case LaunchPluginList:
		return "plugin-list"
	default:
		return fmt.Sprintf("launch-kind(%d)", int(k))
	}
}

// launchLogDir is the per-allocation directory holding Council-owned
// per-launch agy logs (never the operator's cli.log, never under HOME).
const launchLogDir = "agy-logs"

// AgyTurnLaunchSource builds the complete LaunchRequest for one agy
// launch of a logical session. nativeID is required for LaunchTurn and
// must be empty for LaunchCreate; promptDigest is required for
// LaunchTurn (durable acceptance correlation, never sent natively).
type AgyTurnLaunchSource interface {
	AgyTurnLaunch(ctx context.Context, sessionID adapter.SessionID, nativeID, promptDigest string, kind LaunchKind) (execpolicy.LaunchRequest, error)
}

// StorageLaunchSource is the storage-backed AgyTurnLaunchSource: frozen
// profile from the run record, cwd from the AC-005 allocation, the
// adapter's held sealed image as the executable.
type StorageLaunchSource struct {
	store  *storage.Store
	wm     *workspace.WorkspaceManager
	policy AgyLaunchPolicy
	image  *execpolicy.SealedImage
}

var _ AgyTurnLaunchSource = (*StorageLaunchSource)(nil)

// NewAgyTurnLaunchSource constructs the storage-backed launch source.
// image is the sealed image of the pinned binary (nil only off Linux in
// fixture scope, where the executor's explicit fixture marker applies).
func NewAgyTurnLaunchSource(store *storage.Store, wm *workspace.WorkspaceManager, policy AgyLaunchPolicy, image *execpolicy.SealedImage) *StorageLaunchSource {
	return &StorageLaunchSource{store: store, wm: wm, policy: policy, image: image}
}

// AgyTurnLaunch implements AgyTurnLaunchSource.
func (s *StorageLaunchSource) AgyTurnLaunch(ctx context.Context, sessionID adapter.SessionID, nativeID, promptDigest string, kind LaunchKind) (execpolicy.LaunchRequest, error) {
	switch kind {
	case LaunchCreate:
		if nativeID != "" || promptDigest != "" {
			return execpolicy.LaunchRequest{}, fmt.Errorf("creation launch carries no native id or prompt digest")
		}
	case LaunchTurn:
		if !isValidUUIDv4(nativeID) {
			return execpolicy.LaunchRequest{}, fmt.Errorf("turn launch requires a UUIDv4 native id, got %q", nativeID)
		}
		if strings.TrimSpace(promptDigest) == "" {
			return execpolicy.LaunchRequest{}, fmt.Errorf("turn launch requires the prompt digest")
		}
	case LaunchModels, LaunchPluginList:
		if nativeID != "" || promptDigest != "" {
			return execpolicy.LaunchRequest{}, fmt.Errorf("%s launch carries no native id or prompt digest", kind)
		}
	default:
		return execpolicy.LaunchRequest{}, fmt.Errorf("unknown launch kind %v", kind)
	}
	if s.store == nil || s.wm == nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("agy launch source is not wired")
	}

	meta, err := s.store.GetSessionMetadata(ctx, string(sessionID))
	if err != nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("session metadata lookup: %w", err)
	}
	if meta.Contributor != "agy" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("session %s contributor is %q, not agy", sessionID, meta.Contributor)
	}
	rec, err := s.store.GetRunProfile(ctx, meta.RunID)
	if err != nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("run profile lookup: %w", err)
	}
	spec, ok := rec.Profile.Harnesses["agy"]
	if !ok || spec.Agy == nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("frozen profile for run %s has no agy harness block", meta.RunID)
	}
	model := strings.TrimSpace(spec.Model)
	if model == "" {
		return execpolicy.LaunchRequest{}, fmt.Errorf("frozen agy harness has no model")
	}

	paths, ok := s.wm.GetPaths(meta.RunID, string(sessionID))
	if !ok {
		paths, err = s.wm.AllocateWorkspace(meta.RunID, string(sessionID), rec.Profile.WorkspaceMode, rec.SourceRepoIdentity, rec.SourceCommit)
		if err != nil {
			return execpolicy.LaunchRequest{}, fmt.Errorf("allocate workspace: %w", err)
		}
	}

	command := s.policy.BinaryPath
	if s.image != nil {
		command = s.image.ArgV0
	}

	var args []string
	switch kind {
	case LaunchModels:
		args = []string{"models"}
	case LaunchPluginList:
		args = []string{"plugin", "list"}
	default:
		logPath, err := newLaunchLogPath(paths, kind)
		if err != nil {
			return execpolicy.LaunchRequest{}, err
		}
		args = buildLaunchArgv(kind, model, s.policy, logPath, nativeID)
	}

	return execpolicy.LaunchRequest{
		RunID:         meta.RunID,
		SessionID:     string(sessionID),
		Command:       command,
		Args:          args,
		Paths:         paths,
		Profile:       rec.Profile,
		PromptDigest:  promptDigest,
		Model:         model,
		ProfileDigest: rec.ProfileDigest,
		SealedImage:   s.image,
	}, nil
}

// newLaunchLogPath allocates a fresh per-launch log path under the
// allocation's scratch directory (created 0700).
func newLaunchLogPath(paths workspace.WorkspacePaths, kind LaunchKind) (string, error) {
	root := strings.TrimSpace(paths.Scratch)
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("workspace allocation has no absolute scratch directory for the launch log")
	}
	dir := filepath.Join(root, launchLogDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create launch log dir: %w", err)
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("launch log nonce: %w", err)
	}
	return filepath.Join(dir, kind.String()+"-"+hex.EncodeToString(nonce[:])+".log"), nil
}

// buildLaunchArgv assembles the exact frozen stream-json argv (spec
// §3.1) for a creation or turn launch.
func buildLaunchArgv(kind LaunchKind, model string, policy AgyLaunchPolicy, logPath, nativeID string) []string {
	args := []string{
		"--print=",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--disable-slash-commands",
		"--model", model,
		"--print-timeout", fmt.Sprintf("%ds", int64(policy.PrintTimeoutBackstop.Seconds())),
		"--log-file", logPath,
	}
	if policy.ExecutionMode != "" && policy.ExecutionMode != "default" {
		args = append(args, "--mode", policy.ExecutionMode)
	}
	if policy.Sandbox {
		args = append(args, "--sandbox")
	}
	if kind == LaunchTurn {
		args = append(args, "--conversation", nativeID)
	}
	return args
}

// argvSpec is what the adapter independently knows about a launch: the
// argv is re-derived from it and compared element for element.
type argvSpec struct {
	kind     LaunchKind
	model    string
	policy   AgyLaunchPolicy
	nativeID string
	// logRoot is the allocation scratch directory the launch log must
	// live directly under (in launchLogDir).
	logRoot string
}

// logFileIndex is the fixed position of the --log-file value.
const logFileIndex = 11

// validateLaunchArgv refuses any argv that is not exactly the frozen
// grammar for spec: every element must match the re-derived argv, and
// the one free value (the launch log path) must be a clean absolute
// path directly inside <logRoot>/agy-logs.
func validateLaunchArgv(args []string, spec argvSpec) error {
	switch spec.kind {
	case LaunchModels:
		if len(args) != 1 || args[0] != "models" {
			return fmt.Errorf("models launch argv must be exactly [models], got %q", args)
		}
		return nil
	case LaunchPluginList:
		if len(args) != 2 || args[0] != "plugin" || args[1] != "list" {
			return fmt.Errorf("plugin-list launch argv must be exactly [plugin list], got %q", args)
		}
		return nil
	case LaunchCreate, LaunchTurn:
	default:
		return fmt.Errorf("unknown launch kind %v", spec.kind)
	}
	if len(args) <= logFileIndex {
		return fmt.Errorf("agy argv is not the frozen grammar: too short (%d elements)", len(args))
	}
	logPath := args[logFileIndex]
	want := buildLaunchArgv(spec.kind, spec.model, spec.policy, logPath, spec.nativeID)
	if len(args) != len(want) {
		return fmt.Errorf("agy argv is not the frozen grammar: %d elements, want %d (%q)", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			return fmt.Errorf("agy argv element %d is %q, the frozen grammar requires %q", i, args[i], want[i])
		}
	}
	if !filepath.IsAbs(logPath) || filepath.Clean(logPath) != logPath {
		return fmt.Errorf("launch log path %q must be a clean absolute path", logPath)
	}
	root := strings.TrimSpace(spec.logRoot)
	if root == "" || filepath.Dir(logPath) != filepath.Join(root, launchLogDir) || !strings.HasSuffix(logPath, ".log") {
		return fmt.Errorf("launch log path %q must live directly under %s", logPath, filepath.Join(root, launchLogDir))
	}
	return nil
}

// ErrLaunchMatrixRefused reports a frozen (isolation strictness, network
// mode) row the spec §3.1 launch matrix refuses. No process was started.
type ErrLaunchMatrixRefused struct {
	Strictness string
	Network    string
	Reason     string
}

func (e *ErrLaunchMatrixRefused) Error() string {
	return fmt.Sprintf("agy launch refused for isolation %q / network %q: %s", e.Strictness, e.Network, e.Reason)
}

// checkLaunchMatrix applies the spec §3.1 launch matrix, fail closed:
// strict+allowlist and strict+unrestricted are always refused (unverified
// capability, never silently downgraded); network none rows are
// fixture/diagnostic only (the auth gate cannot conclude without
// egress), allowed solely under the explicit fixture construction scope;
// permissive_dev+unrestricted and permissive_dev+allowlist launch.
func checkLaunchMatrix(p storage.CanonicalProfile, fixtureScope bool) error {
	refuse := func(reason string) error {
		return &ErrLaunchMatrixRefused{Strictness: p.IsolationStrictness, Network: p.NetworkMode, Reason: reason}
	}
	switch p.IsolationStrictness {
	case "permissive_dev", "strict":
	default:
		return refuse("unknown isolation strictness")
	}
	switch p.NetworkMode {
	case "none":
		if !fixtureScope {
			return refuse("network none is fixture/diagnostic only: the models auth gate cannot conclude without provider egress")
		}
		return nil
	case "allowlist", "unrestricted":
		if p.IsolationStrictness == "strict" {
			return refuse("strict isolation with provider egress is an unverified capability for the agy turn child")
		}
		return nil
	default:
		return refuse("unknown network mode")
	}
}
