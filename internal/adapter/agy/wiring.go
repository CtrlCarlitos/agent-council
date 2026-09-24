package agy

// Production wiring for the agy adapter (AC-010 spec §3.2/§3.7/§3.12).
// NewProductionAgyAdapter is the ONLY production construction path; it
// takes no ConstructionOption (only the distinct ProductionOption type,
// which carries service seams and nothing else), so the test-only
// fixture scope is structurally unreachable from it. Construction fails closed, in the
// spec §3.2 gate order (nothing is launched before step 4):
//
//  1. ValidateAgyHarness: the frozen cprof-v4 agy block and its
//     committed evidence (init, tool coverage, plugins) re-hashed.
//  2. homeDir must be the parent of the frozen expected_home (the
//     operator's real HOME in production, a temp dir in tests).
//  3. NewSealedImage(binary_path, binary_digest): the kernel-sealed
//     image every launch execs (closed again on any later failure).
//  4. checkProductionEligibility with the storage-backed attestation
//     lookup (full tuple, stored-row re-compare, coverage frame) —
//     before ANY child, the construction `plugin list` included.
//  5. Toolkit configured state: skills and hooks on disk, then the
//     construction-scoped `plugin list` child (its own scratch scope,
//     no session exists yet).
//  6. NewAgyAdapter over the storage launch source, the SAME sealed
//     image and the SAME workspace manager for launch and allocation
//     lookup.

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

// constructionProbeDir is the construction-scoped `plugin list` launch's
// directory under the operator-provisioned scratch root (also its
// executor session identity): no logical session exists at
// construction, so the capture runs in its own neutral scope.
const constructionProbeDir = "agy-toolkit-probe"

// ErrHomeDirMismatch reports a production homeDir that is not the
// parent of the frozen expected_home: the child HOME is derived from
// the frozen profile, never supplied independently.
type ErrHomeDirMismatch struct {
	HomeDir string
	Want    string
}

func (e *ErrHomeDirMismatch) Error() string {
	return fmt.Sprintf("agy homeDir %q is not the parent %q of the frozen expected_home", e.HomeDir, e.Want)
}

// ProductionOption supplies a service-owned seam to the production
// constructor. It is a distinct type from ConstructionOption: it can
// carry service seams only, never the test-only fixture scope.
type ProductionOption func(*productionSettings)

type productionSettings struct {
	required RequiredToolsSource
}

// WithRequiredToolsSource wires the service's required-tools seam (spec
// §3.5: the set validated at queue time and journaled with the dispatch
// intent). Without it every attempt verifies the frozen
// default_required_tools.
func WithRequiredToolsSource(src RequiredToolsSource) ProductionOption {
	return func(s *productionSettings) { s.required = src }
}

// NewProductionAgyAdapter builds the production agy adapter for one
// frozen run profile. wm is the AC-005 workspace manager (the launch
// source's allocations and the adapter's independent allocation lookup
// — the same instance); scratchRoot is an operator-provisioned
// directory for the construction-scoped `plugin list` capture.
func NewProductionAgyAdapter(
	store *storage.Store,
	executor execpolicy.PolicyExecutor,
	wm *workspace.WorkspaceManager,
	cfgProfile storage.CanonicalProfile,
	evidenceRoot, homeDir, scratchRoot string,
	opts ...ProductionOption,
) (*AgyAdapter, error) {
	var settings productionSettings
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	switch {
	case store == nil:
		return nil, errors.New("storage store is required")
	case executor == nil:
		return nil, errors.New("policy executor is required")
	case wm == nil:
		return nil, errors.New("AC-005 workspace manager is required")
	case strings.TrimSpace(evidenceRoot) == "":
		return nil, errors.New("agy evidence root is required")
	case strings.TrimSpace(homeDir) == "":
		return nil, errors.New("agy operator home is required")
	case strings.TrimSpace(scratchRoot) == "" || !filepath.IsAbs(scratchRoot):
		return nil, errors.New("an absolute agy construction scratch root is required")
	}

	policy, err := ValidateAgyHarness(cfgProfile, evidenceRoot)
	if err != nil {
		return nil, fmt.Errorf("frozen agy policy: %w", err)
	}
	wantHome, err := agyHomeDir(policy.ExpectedHome)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(homeDir) != wantHome {
		return nil, &ErrHomeDirMismatch{HomeDir: homeDir, Want: wantHome}
	}
	profileDigest, _, err := storage.ComputeProfileDigest(cfgProfile)
	if err != nil {
		return nil, fmt.Errorf("frozen profile digest: %w", err)
	}
	manifestDigest, err := storage.ComputeToolkitManifestDigest(cfgProfile.ToolkitManifest.ToolkitManifest)
	if err != nil {
		return nil, fmt.Errorf("toolkit manifest digest: %w", err)
	}

	image, err := execpolicy.NewSealedImage(policy.BinaryPath, policy.BinaryDigest)
	if err != nil {
		return nil, fmt.Errorf("sealed image of the pinned agy binary: %w", err)
	}
	built := false
	defer func() {
		if !built {
			_ = image.Close()
		}
	}()

	attestation := storageAttestationLookup(store, policy, manifestDigest, profileDigest)
	explain := storageAttestationExplain(store, policy, manifestDigest, profileDigest)
	if err := checkProductionEligibilityExplained(policy, image, attestation, explain); err != nil {
		return nil, err
	}

	if err := verifyToolkitFiles(policy); err != nil {
		return nil, err
	}
	probe, err := constructionPluginListLaunch(policy, image, cfgProfile, profileDigest, wantHome, scratchRoot)
	if err != nil {
		return nil, err
	}
	if err := validateConstructionLaunch(probe, policy, image, profileDigest, wantHome); err != nil {
		return nil, fmt.Errorf("construction plugin-list launch: %w", err)
	}
	if err := capturePluginList(executor, probe, policy.PluginsEvidenceDigest); err != nil {
		return nil, err
	}

	source := NewAgyTurnLaunchSource(store, wm, policy, image)
	a, err := NewAgyAdapter(store, executor, source, wm, policy, profileDigest, image,
		&storageDispatchIdentitySource{store: store}, settings.required, attestation)
	if err != nil {
		return nil, err
	}
	a.attestationWhy = explain
	built = true
	return a, nil
}

// validateConstructionLaunch applies the adapter's launch checks to the
// construction-scoped `plugin list` launch (which runs before any
// adapter exists): the production launch matrix, the held sealed image
// of the pinned binary as the command, the operator HOME, the frozen
// profile digest and model, and the exact frozen argv.
func validateConstructionLaunch(req execpolicy.LaunchRequest, policy AgyLaunchPolicy, image *execpolicy.SealedImage,
	profileDigest, home string) error {
	if err := checkLaunchMatrix(req.Profile, false); err != nil {
		return err
	}
	switch {
	case image == nil || req.SealedImage != image || image.Digest != policy.BinaryDigest:
		return errors.New("the launch does not carry the held sealed image of the pinned binary")
	case req.Command != image.ArgV0:
		return fmt.Errorf("launch command %q is not the pinned binary %q", req.Command, image.ArgV0)
	case req.HomeDir != home:
		return fmt.Errorf("launch HOME %q is not the operator home %q derived from the frozen expected_home", req.HomeDir, home)
	case req.ProfileDigest != profileDigest:
		return fmt.Errorf("launch profile digest %q is not the frozen %q", req.ProfileDigest, profileDigest)
	case req.Model != policy.Model:
		return &ErrProfileDrift{Field: "model", Want: policy.Model, Have: req.Model}
	}
	return validateLaunchArgv(req.Args, argvSpec{kind: LaunchPluginList, model: policy.Model, policy: policy})
}

// constructionPluginListLaunch assembles the construction-scoped `plugin
// list` launch: the sealed image, the operator HOME, the frozen profile,
// cwd a neutral 0700 directory under scratchRoot.
func constructionPluginListLaunch(policy AgyLaunchPolicy, image *execpolicy.SealedImage, profile storage.CanonicalProfile,
	profileDigest, home, scratchRoot string) (execpolicy.LaunchRequest, error) {
	root := filepath.Join(filepath.Clean(scratchRoot), constructionProbeDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return execpolicy.LaunchRequest{}, fmt.Errorf("construction probe scope: %w", err)
	}
	return execpolicy.LaunchRequest{
		SessionID:     constructionProbeDir,
		Command:       image.ArgV0,
		Args:          []string{"plugin", "list"},
		Paths:         workspace.WorkspacePaths{Root: root, Scratch: root, Config: root},
		Profile:       profile,
		Model:         policy.Model,
		ProfileDigest: profileDigest,
		SealedImage:   image,
		HomeDir:       home,
	}, nil
}

// storageDispatchIdentitySource resolves the attempt identity from the
// persisted dispatch intent (the AC-007/AC-008/AC-009 seam).
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
