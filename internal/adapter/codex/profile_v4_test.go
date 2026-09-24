package codex

// cprof-v4 compatibility (AC-010 spec §3.7 matrix): the Codex adapter
// accepts cprof-v4 too, with the codex block validated exactly as
// under cprof-v3; a v4 profile without a complete codex block is
// rejected exactly as v3 is.

import (
	"errors"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func TestValidateCodexHarness_AcceptsCprofV4Profile(t *testing.T) {
	p := v3CodexProfile()
	p.AlgoVersion = "cprof-v4"
	p, root := evidenceRootForCodex(t, p)

	policy, err := ValidateCodexHarness(p, root)
	if err != nil {
		t.Fatalf("a complete codex block under cprof-v4 must validate exactly as v3: %v", err)
	}
	if policy.AppServerVersion != "0.154.0" {
		t.Fatalf("policy AppServerVersion, got %q", policy.AppServerVersion)
	}
}

func TestValidateCodexHarness_RejectsCprofV4WithoutCompleteCodexBlock(t *testing.T) {
	p := v3CodexProfile()
	p.AlgoVersion = "cprof-v4"
	delete(p.Harnesses, "codex")

	_, err := ValidateCodexHarness(p, t.TempDir())
	var unsupported *ErrUnsupportedProfile
	if err == nil || !errors.As(err, &unsupported) {
		t.Fatalf("a v4 profile without harnesses.codex must be rejected as ErrUnsupportedProfile, got %T: %v", err, err)
	}
}

// The v3 gates still apply verbatim under v4: an invalid sandbox type
// is rejected.
func TestValidateCodexHarness_CprofV4EnforcesV3Gates(t *testing.T) {
	p := v3CodexProfile()
	p.AlgoVersion = "cprof-v4"
	p.Harnesses["codex"].Codex.SandboxPolicy.Type = "danger-full-access"
	p, root := evidenceRootForCodex(t, p)

	_, err := ValidateCodexHarness(p, root)
	var unsupported *ErrUnsupportedProfile
	if err == nil || !errors.As(err, &unsupported) {
		t.Fatalf("an invalid sandbox_policy type under v4 must be rejected exactly as v3 rejects it, got %T: %v", err, err)
	}
}

// A v4 profile that ALSO carries a valid agy block still validates
// through the codex adapter: one run profile serves all four harnesses.
func TestValidateCodexHarness_CprofV4WithAgyBlockPresent(t *testing.T) {
	p := v3CodexProfile()
	p.AlgoVersion = "cprof-v4"
	p.Harnesses["agy"] = storage.HarnessProfileSpec{
		Model: "gpt-oss-120b-medium", NativeAuthMode: "inherited_gemini_home",
	}
	p, root := evidenceRootForCodex(t, p)

	if _, err := ValidateCodexHarness(p, root); err != nil {
		t.Fatalf("a v4 profile carrying both codex and (incomplete) agy harnesses must still validate for codex: %v", err)
	}
}
