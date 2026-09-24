package agy

// Production-eligibility gate (AC-010 spec §3.2 gate step 1, §3.12):
// checkProductionEligibility never launches anything and fails closed,
// in order, on the platform, the sealed image, the frozen inventories,
// and the attestation. The attestation lookup binds the FULL frozen
// tuple twice (the storage query, then a re-compare of the stored row
// against the policy in force) before the durable frame must cover
// expected_tools ∩ the coverage map.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/execpolicy"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const (
	eligManifest = "sha256:" + "abababababababababababababababababababababababababababababababab"
	eligProfile  = "cprof-v4:sha256:" + "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
)

func eligiblePolicy() AgyLaunchPolicy {
	p := smallPolicy()
	p.BinaryPath = "/home/op/.local/bin/agy"
	p.BinaryDigest = "sha256:" + strings.Repeat("c", 64)
	return p
}

func heldImage(p AgyLaunchPolicy) *execpolicy.SealedImage {
	return &execpolicy.SealedImage{ArgV0: p.BinaryPath, Digest: p.BinaryDigest}
}

func withHostOS(t *testing.T, goos string) {
	t.Helper()
	old := hostOS
	hostOS = goos
	t.Cleanup(func() { hostOS = old })
}

func okAttestation() (string, bool) { return "cprot-v2:sha256:" + strings.Repeat("ab", 32), true }

func requireNotEligible(t *testing.T, err error, reasonPart string) *ErrNotEligible {
	t.Helper()
	var ne *ErrNotEligible
	if !errors.As(err, &ne) {
		t.Fatalf("want *ErrNotEligible, got %T: %v", err, err)
	}
	if !strings.Contains(ne.Error(), reasonPart) {
		t.Fatalf("ineligibility %q does not name %q", ne.Error(), reasonPart)
	}
	return ne
}

func TestEligibility_EligiblePasses(t *testing.T) {
	withHostOS(t, "linux")
	p := eligiblePolicy()
	if err := checkProductionEligibility(p, heldImage(p), okAttestation); err != nil {
		t.Fatalf("an eligible policy must pass: %v", err)
	}
}

func TestEligibility_PlatformFirst(t *testing.T) {
	calls := 0
	lookup := func() (string, bool) { calls++; return okAttestation() }
	p := eligiblePolicy()

	withHostOS(t, "darwin")
	requireNotEligible(t, checkProductionEligibility(p, heldImage(p), lookup), "platform")

	withHostOS(t, "linux")
	p.PlatformOS = "windows"
	requireNotEligible(t, checkProductionEligibility(p, heldImage(p), lookup), "platform")
	p = eligiblePolicy()
	p.PlatformFamily = "posix"
	requireNotEligible(t, checkProductionEligibility(p, heldImage(p), lookup), "platform")
	if calls != 0 {
		t.Fatalf("an ineligible platform must be refused before the attestation lookup, lookups=%d", calls)
	}
}

func TestEligibility_SealedImageRequiredAndPinned(t *testing.T) {
	withHostOS(t, "linux")
	p := eligiblePolicy()
	requireNotEligible(t, checkProductionEligibility(p, nil, okAttestation), "sealed image")

	other := heldImage(p)
	other.Digest = "sha256:" + strings.Repeat("d", 64)
	requireNotEligible(t, checkProductionEligibility(p, other, okAttestation), "digest")
}

func TestEligibility_InventoriesRechecked(t *testing.T) {
	withHostOS(t, "linux")
	for name, mutate := range map[string]func(*AgyLaunchPolicy){
		"mcp_servers":    func(p *AgyLaunchPolicy) { p.ExpectedMCPServers = []string{"graft"} },
		"mcp_tools":      func(p *AgyLaunchPolicy) { p.ExpectedMCPTools = []string{"graft/ask"} },
		"plugin_tools":   func(p *AgyLaunchPolicy) { p.ExpectedPluginTools = []string{"superpowers_x"} },
		"expected_tools": func(p *AgyLaunchPolicy) { p.ExpectedTools = nil },
	} {
		t.Run(name, func(t *testing.T) {
			p := eligiblePolicy()
			mutate(&p)
			requireNotEligible(t, checkProductionEligibility(p, heldImage(p), okAttestation), "inventor")
		})
	}
}

func TestEligibility_AttestationRequired(t *testing.T) {
	withHostOS(t, "linux")
	p := eligiblePolicy()
	ne := requireNotEligible(t, checkProductionEligibility(p, heldImage(p), nil), "attestation")
	var missing *ErrProductionEligibilityMissing
	if !errors.As(ne, &missing) {
		t.Fatalf("a nil lookup wraps ErrProductionEligibilityMissing, got %v", ne)
	}
	ne = requireNotEligible(t, checkProductionEligibility(p, heldImage(p), func() (string, bool) { return "", false }), "attestation")
	if !errors.As(ne, &missing) {
		t.Fatalf("an absent attestation wraps ErrProductionEligibilityMissing, got %v", ne)
	}
}

// ── attestation lookup ──────────────────────────────────────────────────

type fakeAttestationSource struct {
	findID  string
	findErr error
	rec     *storage.AgyProtectionAttestationRecord
	getErr  error

	gotTuple [4]string
}

func (f *fakeAttestationSource) FindAgyProtectionAttestation(_ context.Context, v, pl, m, pd string) (string, error) {
	f.gotTuple = [4]string{v, pl, m, pd}
	return f.findID, f.findErr
}

func (f *fakeAttestationSource) GetAgyProtectionAttestation(_ context.Context, id string) (*storage.AgyProtectionAttestationRecord, error) {
	if f.rec == nil || f.rec.AttestationID != id {
		return nil, f.getErr
	}
	cp := *f.rec
	return &cp, f.getErr
}

// coveredSource builds a fake source whose row carries the exact frozen
// tuple and a frame covering the policy.
func coveredSource(t *testing.T, p AgyLaunchPolicy) *fakeAttestationSource {
	t.Helper()
	cov, err := ExpectedCoverage(p)
	if err != nil {
		t.Fatalf("ExpectedCoverage: %v", err)
	}
	frame, err := coveringAttestation(cov).EncodeProbeRecords()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	id := "cprot-v2:sha256:" + strings.Repeat("12", 32)
	return &fakeAttestationSource{findID: id, rec: &storage.AgyProtectionAttestationRecord{
		AttestationID: id, AgyVersion: p.CLIVersion, Platform: p.PlatformOS + "/" + p.PlatformFamily,
		ManifestDigest: eligManifest, ProfileDigest: eligProfile, ProbeResults: string(frame),
		ProbedAt: "2026-09-24T00:00:00Z", Actor: "operator",
	}}
}

func TestAttestationLookup_CoveredTupleEligible(t *testing.T) {
	p := eligiblePolicy()
	src := coveredSource(t, p)
	id, ok := storageAttestationLookup(src, p, eligManifest, eligProfile)()
	if !ok || id != src.findID {
		t.Fatalf("a covered exact-tuple row is eligible, got %q ok=%v", id, ok)
	}
	want := [4]string{p.CLIVersion, "linux/unix", eligManifest, eligProfile}
	if src.gotTuple != want {
		t.Fatalf("lookup tuple = %v, want %v", src.gotTuple, want)
	}
}

func TestAttestationLookup_StoredTupleRecompared(t *testing.T) {
	p := eligiblePolicy()
	for name, mutate := range map[string]func(*storage.AgyProtectionAttestationRecord){
		"version":  func(r *storage.AgyProtectionAttestationRecord) { r.AgyVersion = "1.3.0" },
		"platform": func(r *storage.AgyProtectionAttestationRecord) { r.Platform = "darwin/unix" },
		"manifest": func(r *storage.AgyProtectionAttestationRecord) {
			r.ManifestDigest = "sha256:" + strings.Repeat("0", 64)
		},
		"profile": func(r *storage.AgyProtectionAttestationRecord) {
			r.ProfileDigest = "cprof-v4:sha256:" + strings.Repeat("0", 64)
		},
		"id": func(r *storage.AgyProtectionAttestationRecord) {
			r.AttestationID = "cprot-v2:sha256:" + strings.Repeat("99", 32)
		},
	} {
		t.Run(name, func(t *testing.T) {
			src := coveredSource(t, p)
			mutate(src.rec)
			if id, ok := storageAttestationLookup(src, p, eligManifest, eligProfile)(); ok || id != "" {
				t.Fatalf("a stored row disagreeing on %s must stay ineligible, got %q", name, id)
			}
		})
	}
}

func TestAttestationLookup_UncoveredRowIneligible(t *testing.T) {
	p := eligiblePolicy()
	cov, err := ExpectedCoverage(p)
	if err != nil {
		t.Fatalf("ExpectedCoverage: %v", err)
	}
	att := coveringAttestation(cov)
	att.ProbeRecords = att.ProbeRecords[1:] // view_file's sibling_read dropped
	frame, err := att.EncodeProbeRecords()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	src := coveredSource(t, p)
	src.rec.ProbeResults = string(frame)
	if id, ok := storageAttestationLookup(src, p, eligManifest, eligProfile)(); ok || id != "" {
		t.Fatalf("an uncovered expected_tools row must stay ineligible, got %q", id)
	}
	if _, err := lookupCoveredAttestation(context.Background(), src, p, eligManifest, eligProfile); err == nil ||
		!strings.Contains(err.Error(), "coverage") {
		t.Fatalf("the lookup reports the coverage failure, got %v", err)
	}
}

func TestAttestationLookup_AbsentOrFailingIneligible(t *testing.T) {
	p := eligiblePolicy()
	for name, src := range map[string]*fakeAttestationSource{
		"absent":     {},
		"find_error": {findErr: errors.New("db down")},
		"row_gone":   {findID: "cprot-v2:sha256:" + strings.Repeat("34", 32)},
	} {
		t.Run(name, func(t *testing.T) {
			if id, ok := storageAttestationLookup(src, p, eligManifest, eligProfile)(); ok || id != "" {
				t.Fatalf("%s must be ineligible, got %q", name, id)
			}
		})
	}
	if lookup := storageAttestationLookup(nil, p, eligManifest, eligProfile); lookup != nil {
		if _, ok := lookup(); ok {
			t.Fatal("a nil source is never eligible")
		}
	}
}
