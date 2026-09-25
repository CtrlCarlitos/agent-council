package agy

// Production-eligibility gate (AC-010 spec §3.2 gate step 1, §3.12):
// before ANY child starts — the construction-scoped `plugin list`
// capture included — authenticated production dispatch requires, in
// order: the linux/unix frozen platform on a Linux host, the held
// sealed image of the pinned binary, the frozen native inventories
// empty, and a valid isolation attestation for the exact frozen tuple
// (agy version, platform identity, toolkit manifest digest, cprof-v4
// profile digest) whose durable cprot-v2 frame covers expected_tools ∩
// the coverage map. The gate is durable lookups plus frame validation
// only: it never launches anything, so it runs at production
// construction and again before every creation and turn launch.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ErrNotEligible reports production dispatch refused by the §3.2 gate
// step 1: Reason names the failed rule; Err, when set, is the typed
// cause (an absent attestation wraps *ErrProductionEligibilityMissing).
// No process was started.
type ErrNotEligible struct {
	Reason string
	Err    error
}

func (e *ErrNotEligible) Error() string {
	if e.Err != nil {
		return "agy production dispatch is not eligible: " + e.Reason + ": " + e.Err.Error()
	}
	return "agy production dispatch is not eligible: " + e.Reason
}

func (e *ErrNotEligible) Unwrap() error { return e.Err }

// hostOS is the running platform (a package variable only so tests can
// exercise the host rule off Linux).
var hostOS = runtime.GOOS

// checkProductionEligibility applies the gate in its fixed order:
// platform → sealed image → inventories → attestation. It never
// launches a process.
func checkProductionEligibility(policy AgyLaunchPolicy, image *execpolicy.SealedImage, attestation AttestationLookup) error {
	return checkProductionEligibilityExplained(policy, image, attestation, nil)
}

// checkProductionEligibilityExplained is checkProductionEligibility with
// an optional explain seam: when the attestation lookup fails, explain
// (storageAttestationExplain in production) names the specific reason
// — a stored tuple disagreement, an uncovered frame, a lookup error —
// in the ErrProductionEligibilityMissing reason. A nil explain, or an
// explain returning nil (no row at all), keeps the generic reason.
func checkProductionEligibilityExplained(policy AgyLaunchPolicy, image *execpolicy.SealedImage, attestation AttestationLookup, explain func() error) error {
	if hostOS != "linux" || policy.PlatformOS != "linux" || policy.PlatformFamily != "unix" {
		return &ErrNotEligible{Reason: fmt.Sprintf(
			"production agy is eligible only for the linux/unix platform on a linux host (frozen platform %s/%s, host %s)",
			policy.PlatformOS, policy.PlatformFamily, hostOS)}
	}
	if image == nil {
		return &ErrNotEligible{Reason: "production launches are sealed-only: no sealed image of the pinned binary is held"}
	}
	if image.Digest != policy.BinaryDigest {
		return &ErrNotEligible{Reason: fmt.Sprintf("the held sealed image digest %s is not the frozen binary digest %s",
			image.Digest, policy.BinaryDigest)}
	}
	if len(policy.ExpectedTools) == 0 {
		return &ErrNotEligible{Reason: "the frozen built-in tool inventory (expected_tools) is empty"}
	}
	if len(policy.ExpectedMCPServers) > 0 || len(policy.ExpectedMCPTools) > 0 || len(policy.ExpectedPluginTools) > 0 {
		return &ErrNotEligible{Reason: "the frozen MCP/plugin tool inventories must be empty: no committed evidence proves a non-empty native inventory"}
	}
	if attestation == nil {
		return &ErrNotEligible{Reason: "attestation required",
			Err: &ErrProductionEligibilityMissing{Reason: "no attestation lookup is wired"}}
	}
	if _, ok := attestation(); !ok {
		reason := "no valid, covering isolation attestation for the frozen (agy version, platform, manifest, profile) tuple"
		if explain != nil {
			if err := explain(); err != nil {
				reason += ": " + err.Error()
			}
		}
		return &ErrNotEligible{Reason: "attestation required",
			Err: &ErrProductionEligibilityMissing{Reason: reason}}
	}
	return nil
}

// attestationRecordSource is the durable attestation surface the lookup
// reads (satisfied by *storage.Store).
type attestationRecordSource interface {
	FindAgyProtectionAttestation(ctx context.Context, agyVersion, platform, manifestDigest, profileDigest string) (string, error)
	GetAgyProtectionAttestation(ctx context.Context, attestationID string) (*storage.AgyProtectionAttestationRecord, error)
}

var _ attestationRecordSource = (*storage.Store)(nil)

// platformIdentity is the canonical os + "/" + family binding string
// (codex.ProtectionAttestation.PlatformIdentity's derivation).
func platformIdentity(policy AgyLaunchPolicy) string {
	return policy.PlatformOS + "/" + policy.PlatformFamily
}

// lookupCoveredAttestation finds the attestation for the exact frozen
// tuple, re-compares the STORED row's tuple against the policy in force
// (a row found by id never satisfies the gate on the query's say-so
// alone), and requires its durable frame to cover
// ExpectedCoverage(policy). ("", nil) means no row matched; any other
// failure is returned with its reason.
func lookupCoveredAttestation(ctx context.Context, src attestationRecordSource, policy AgyLaunchPolicy, manifestDigest, profileDigest string) (string, error) {
	if src == nil {
		return "", errors.New("no attestation source is wired")
	}
	platform := platformIdentity(policy)
	id, err := src.FindAgyProtectionAttestation(ctx, policy.CLIVersion, platform, manifestDigest, profileDigest)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(id) == "" {
		return "", nil
	}
	rec, err := src.GetAgyProtectionAttestation(ctx, id)
	if err != nil {
		return "", err
	}
	if rec == nil {
		return "", fmt.Errorf("attestation %s vanished between lookup and read", id)
	}
	for _, f := range []struct{ name, have, want string }{
		{"attestation id", rec.AttestationID, id},
		{"agy version", rec.AgyVersion, policy.CLIVersion},
		{"platform", rec.Platform, platform},
		{"manifest digest", rec.ManifestDigest, manifestDigest},
		{"profile digest", rec.ProfileDigest, profileDigest},
	} {
		if f.have != f.want {
			return "", fmt.Errorf("stored attestation %s %q does not match the frozen %q", f.name, f.have, f.want)
		}
	}
	cov, err := ExpectedCoverage(policy)
	if err != nil {
		return "", fmt.Errorf("expected coverage: %w", err)
	}
	if err := ValidateFrame([]byte(rec.ProbeResults), cov); err != nil {
		return "", fmt.Errorf("attestation %s coverage: %w", id, err)
	}
	return id, nil
}

// storageAttestationLookup is the production AttestationLookup: the
// durable rows are consulted on EVERY call (every CreateSession and
// Dispatch re-checks). A missing row, a lookup failure, any tuple
// disagreement, or an uncovered frame reports NOT eligible — there is
// no inference of eligibility from absent, drifted, or partial evidence.
func storageAttestationLookup(src attestationRecordSource, policy AgyLaunchPolicy, manifestDigest, profileDigest string) AttestationLookup {
	return func() (string, bool) {
		id, err := lookupCoveredAttestation(context.Background(), src, policy, manifestDigest, profileDigest)
		if err != nil || strings.TrimSpace(id) == "" {
			return "", false
		}
		return id, true
	}
}

// storageAttestationExplain is the production explain seam: the specific
// reason the durable lookup refused (nil when no row matched at all).
func storageAttestationExplain(src attestationRecordSource, policy AgyLaunchPolicy, manifestDigest, profileDigest string) func() error {
	return func() error {
		_, err := lookupCoveredAttestation(context.Background(), src, policy, manifestDigest, profileDigest)
		return err
	}
}
