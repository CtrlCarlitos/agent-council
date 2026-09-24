//go:build unix

package codex

// Task 6 rollout trust model evidence (POSIX — real executor + fixture
// child, spec §3.7/§3.5 step 3): bounded UUID-suffix resolution with
// session_meta and path-integrity enforcement, baseline scan integrity
// (torn tails, mid-file invalidation, POSIX ownership/mode), tail-only
// size-capped reads, ordered protected entry correlation, protection
// freezing at launch (including the unverified-platform degradation),
// post-launch turn_context confirmation (protected ⇒ Uncertain on drift,
// advisory ⇒ diagnostic only), the accepted upgrade, Observe streams
// (item/usage mirroring, overflow isolating the observer, ctx-cancel
// detach), the §3.9 eligibility gate firing at EVERY launch, and the
// §3.9 collect gating.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/council"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

// ── Rollout fixtures ────────────────────────────────────────────────────

// insertHarnessAttestation records the cprot-v2 attestation row the
// harness's eligibility seam reports, bound to the harness's frozen
// (version, platform, manifest, profile) tuple and carrying a record
// frame that COVERS the frozen profile (coverage.go). Pass
// manifestDigest "" to use the harness's frozen manifest digest, or a
// divergent value to prove the tuple match fails closed.
func insertHarnessAttestation(t *testing.T, h *adapterHarness, id string, manifestDigest ...string) {
	t.Helper()
	md := h.policy.ManifestDigest
	if len(manifestDigest) > 0 && manifestDigest[0] != "" {
		md = manifestDigest[0]
	}
	cov := CoverageFor(h.policy, h.profileDigest)
	raw, err := coveringAttestation(cov).EncodeProbeRecords()
	if err != nil {
		t.Fatalf("encode covering frame: %v", err)
	}
	insertHarnessAttestationRaw(t, h, id, md, raw)
}

// insertHarnessAttestationRaw records a tuple-matching row (modulo the
// manifest digest) whose probe_results column carries exactly raw — the
// seam for proving that an undecodable or uncovered record set never
// freezes protected evidence.
func insertHarnessAttestationRaw(t *testing.T, h *adapterHarness, id, manifestDigest string, raw []byte) {
	t.Helper()
	_, err := h.store.DB().ExecContext(context.Background(), `
INSERT INTO codex_protection_attestations
	(attestation_id, codex_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, ?, ?, ?, ?, ?, '2026-09-23T00:00:00Z', 'op')`,
		id, h.policy.AppServerVersion, codexPlatformIdentity(h.policy), manifestDigest, h.profileDigest, raw)
	if err != nil {
		t.Fatalf("insert harness attestation: %v", err)
	}
}

// harnessTurnContext renders a turn_context rollout entry for the given
// drift (nil drift matches the frozen pins exactly).
func harnessTurnContext(h *adapterHarness, turnID string, drift func(map[string]any)) string {
	payload := map[string]any{
		"turn_id":            turnID,
		"cwd":                h.wsRoot,
		"model":              h.model,
		"approval_policy":    "on-request",
		"approvals_reviewer": "user",
		"sandbox_policy": map[string]any{
			"type":           "workspace-write",
			"writable_roots": []string{h.wsRoot},
			"network_access": false,
		},
		"permission_profile": map[string]any{"type": "managed"},
	}
	if drift != nil {
		drift(payload)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return `{"timestamp":"2026-09-23T00:00:05Z","type":"turn_context","payload":` + string(raw) + `}`
}

// harnessUserItem renders the user input response_item whose exact text
// is the correlation pdig input.
func harnessUserItem(turnKey, prompt string) string {
	payload := map[string]any{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{"type": "text", "text": prompt},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return `{"timestamp":"2026-09-23T00:00:06Z","type":"response_item","payload":` + string(raw) + `}`
}

func harnessTaskComplete(message string, withError bool) string {
	errField := `"error":null`
	if withError {
		errField = `"error":{"message":"sandbox denied the write"}`
	}
	msgRaw, _ := json.Marshal(message)
	return `{"timestamp":"2026-09-23T00:00:07Z","type":"event_msg","payload":{"type":"task_complete","last_agent_message":` + string(msgRaw) + `,` + errField + `}}`
}

// ── ResolveRollout (bounded scan, identity, integrity) ──────────────────

func TestResolveRollout_BoundedScanGolden(t *testing.T) {
	h := newAdapterHarness(t)
	path := seedRollout(t, h, testThreadID)
	// The resolver walks the symlink-RESOLVED sessions root and records
	// the physical path the native child experiences (macOS temp dirs
	// live behind /var → /private/var), so the golden is the physical
	// spelling of the seeded file.
	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve seeded path: %v", err)
	}

	got, err := ResolveRollout(h.policy.ExpectedCodexHome, testThreadID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != want {
		t.Fatalf("resolved %q, want the physical path %q", got, want)
	}
	// A codex home reached THROUGH a symlink (the macOS shape, reproduced
	// on every platform) resolves to the same physical rollout path, and
	// the recorded path passes the integrity check against either
	// spelling of the home.
	linkedHome := filepath.Join(h.scratch, "home-link")
	if err := os.Symlink(h.policy.ExpectedCodexHome, linkedHome); err != nil {
		t.Fatalf("symlink home: %v", err)
	}
	viaLink, err := ResolveRollout(linkedHome, testThreadID)
	if err != nil {
		t.Fatalf("resolve through the symlinked home: %v", err)
	}
	if viaLink != want {
		t.Fatalf("resolved %q through the symlinked home, want %q", viaLink, want)
	}
	if err := checkRolloutPath(got, linkedHome); err != nil {
		t.Fatalf("physical path must pass the check against the symlinked home: %v", err)
	}
	if err := checkRolloutPath(got, h.policy.ExpectedCodexHome); err != nil {
		t.Fatalf("physical path must pass the check against the home: %v", err)
	}

	// A thread with no rollout fails closed.
	if _, err := ResolveRollout(h.policy.ExpectedCodexHome, "01900000-0000-7000-8000-0000000000ff"); err == nil {
		t.Fatal("missing rollout must fail")
	}

	// A sessions root that does not exist fails closed.
	if _, err := ResolveRollout(filepath.Join(h.scratch, "absent-home"), testThreadID); err == nil {
		t.Fatal("absent sessions root must fail")
	}
}

func TestResolveRollout_AmbiguousScanFailsClosed(t *testing.T) {
	h := newAdapterHarness(t)
	seedRollout(t, h, testThreadID)
	// A second rollout file carrying the exact UUID suffix in another
	// date directory makes the scan ambiguous — never pick one.
	dir := filepath.Join(h.scratch, ".codex", "sessions", "2026", "09", "24")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	other := filepath.Join(dir, "rollout-2026-09-24T01-00-00-"+testThreadID+".jsonl")
	if err := os.WriteFile(other, []byte(`{"type":"session_meta","payload":{"session_id":"`+testThreadID+`"}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ResolveRollout(h.policy.ExpectedCodexHome, testThreadID); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous scan must fail closed, got %v", err)
	}
}

func TestResolveRollout_SymlinkRejected(t *testing.T) {
	h := newAdapterHarness(t)
	dir := filepath.Join(h.scratch, ".codex", "sessions", "2026", "09", "23")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(h.scratch, "outside.jsonl")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	link := filepath.Join(dir, "rollout-2026-09-23T00-00-00-"+testThreadID+".jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// The walk rejects the symlink inside the root…
	if _, err := ResolveRollout(h.policy.ExpectedCodexHome, testThreadID); err == nil {
		t.Fatal("symlink inside the sessions root must be rejected")
	}
	// …and a directly recorded path through checkRolloutPath as well.
	if err := checkRolloutPath(link, h.policy.ExpectedCodexHome); err == nil {
		t.Fatal("symlinked rollout path must fail the integrity check")
	}
}

// ── Baseline scan integrity ─────────────────────────────────────────────

func TestScanRolloutBaseline_TornTailToleratedMidFileInvalidates(t *testing.T) {
	h := newAdapterHarness(t)
	path := seedRollout(t, h, testThreadID)

	// A torn final line is ignored (tolerated tail).
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	fmt.Fprint(f, `{"type":"event_msg","payload":{"type":"task_sta`)
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, size, entries, err := scanRolloutBaseline(path, testThreadID)
	if err != nil {
		t.Fatalf("torn tail must be tolerated: %v", err)
	}
	if entries != 2 {
		t.Fatalf("baseline entries: %d", entries)
	}

	// A mid-file parse failure invalidates the read (content follows).
	if err := os.WriteFile(path, []byte(
		`{"type":"session_meta","payload":{"session_id":"`+testThreadID+`"}}`+"\n"+
			`{corrupt`+"\n"+
			`{"type":"turn_context","payload":{}}`+"\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, _, err := scanRolloutBaseline(path, testThreadID); err == nil {
		t.Fatal("mid-file corruption must invalidate the baseline read")
	}
	_ = size
}

func TestScanRolloutBaseline_OwnershipAndModePOSIX(t *testing.T) {
	h := newAdapterHarness(t)

	// Group-readable rollouts fail the ownership/mode check.
	path := seedRollout(t, h, testThreadID)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, _, _, err := scanRolloutBaseline(path, testThreadID); err == nil {
		t.Fatal("group-readable rollout must fail the mode check")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, _, _, err := scanRolloutBaseline(path, testThreadID); err != nil {
		t.Fatalf("0600 rollout must pass: %v", err)
	}

	// A foreign-owned rollout fails the ownership check (skipped as
	// root: root reads everything).
	if os.Geteuid() == 0 {
		t.Skip("ownership check cannot be exercised as root")
	}
	st, err := os.Stat("/etc/passwd")
	if err != nil {
		t.Skip("no /etc/passwd")
	}
	if err := verifyRolloutOwnership(st, "/etc/passwd"); err == nil {
		t.Fatal("foreign-owned file must fail ownership verification")
	}
}

// ── Tail-only reads past the baseline ───────────────────────────────────

func TestReadRolloutTail_BoundaryTornAndSizeCap(t *testing.T) {
	h := newAdapterHarness(t)
	path := seedRollout(t, h, testThreadID)

	first, err := readRolloutTail(path, 0)
	if err != nil || len(first) != 2 {
		t.Fatalf("baseline read: %d entries err=%v", len(first), err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Entries past the recorded size are returned; the boundary is
	// byte-exact.
	extra := harnessTaskComplete("done", false) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(extra); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	tail, err := readRolloutTail(path, fi.Size())
	if err != nil || len(tail) != 1 || tail[0].Type != "event_msg" {
		t.Fatalf("tail read: %+v err=%v", tail, err)
	}
	// The tail stops exactly at the baseline: nothing before it leaks.
	all, err := readRolloutTail(path, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("full read: %d entries err=%v", len(all), err)
	}

	// A partial line at the boundary (recorded torn tail) is skipped,
	// never parsed as a mid-file failure.
	fi2, _ := os.Stat(path)
	f2, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprint(f2, `{"type":"event_msg","payload":{"type":"task_`)
	f2.Close()
	tail2, err := readRolloutTail(path, fi2.Size())
	if err != nil || len(tail2) != 0 {
		t.Fatalf("torn boundary must be tolerated: %d entries err=%v", len(tail2), err)
	}

	// Growth beyond the size cap fails closed.
	defer func(old int64) { MaxRolloutTailBytes = old }(MaxRolloutTailBytes)
	MaxRolloutTailBytes = 8
	if _, err := readRolloutTail(path, 0); err == nil {
		t.Fatal("oversized tail must fail closed")
	}
}

// ── Ordered protected entry correlation (§3.7) ──────────────────────────

func harnessPins(t *testing.T, h *adapterHarness) RolloutPins {
	t.Helper()
	pins, err := RolloutPinsFor(h.policy, h.model, h.wsRoot)
	if err != nil {
		t.Fatalf("pins: %v", err)
	}
	return pins
}

func TestCorrelateRolloutTurn_OrderedMatrix(t *testing.T) {
	h := newAdapterHarness(t)
	pins := harnessPins(t, h)
	prompt := "review the rollout tail"
	digest, err := PromptDigest(testThreadID, "t-x", "att-t-x", prompt)
	if err != nil {
		t.Fatalf("pdig: %v", err)
	}

	t.Run("goldenCompleted", func(t *testing.T) {
		entries := parseEntriesForTest(t,
			harnessTurnContext(h, testTurnID, nil),
			harnessUserItem("t-x", prompt),
			harnessTaskComplete("all good", false),
		)
		ev, err := CorrelateRolloutTurn(entries, pins, testThreadID, "t-x", "att-t-x", digest)
		if err != nil {
			t.Fatalf("correlate: %v", err)
		}
		if !ev.TurnContextMatch || !ev.PromptDigestMatch {
			t.Fatalf("correlation must match: %+v", ev)
		}
		if ev.Drift != nil {
			t.Fatalf("no drift expected: %+v", ev.Drift)
		}
		if ev.TaskComplete == nil || ev.TaskComplete.Failed || ev.TaskComplete.LastAgentMessage != "all good" {
			t.Fatalf("task_complete: %+v", ev.TaskComplete)
		}
	})

	t.Run("failedCarriesError", func(t *testing.T) {
		entries := parseEntriesForTest(t,
			harnessTurnContext(h, testTurnID, nil),
			harnessUserItem("t-x", prompt),
			harnessTaskComplete("", true),
		)
		ev, err := CorrelateRolloutTurn(entries, pins, testThreadID, "t-x", "att-t-x", digest)
		if err != nil {
			t.Fatalf("correlate: %v", err)
		}
		if ev.TaskComplete == nil || !ev.TaskComplete.Failed {
			t.Fatalf("populated error must classify failed: %+v", ev.TaskComplete)
		}
	})

	t.Run("pinDriftReported", func(t *testing.T) {
		entries := parseEntriesForTest(t,
			harnessTurnContext(h, testTurnID, func(p map[string]any) { p["model"] = "gpt-6-astra" }),
			harnessUserItem("t-x", prompt),
		)
		ev, err := CorrelateRolloutTurn(entries, pins, testThreadID, "t-x", "att-t-x", digest)
		if err != nil {
			t.Fatalf("correlate: %v", err)
		}
		if ev.Drift == nil || ev.Drift.Field != "model" {
			t.Fatalf("model drift must be reported: %+v", ev.Drift)
		}
	})

	t.Run("orderViolationContextAfterItem", func(t *testing.T) {
		// The user item appearing BEFORE the turn_context never matches:
		// the ordered correlation requires context → item → terminal.
		entries := parseEntriesForTest(t,
			harnessUserItem("t-x", prompt),
			harnessTurnContext(h, testTurnID, nil),
			harnessTaskComplete("done", false),
		)
		ev, err := CorrelateRolloutTurn(entries, pins, testThreadID, "t-x", "att-t-x", digest)
		if err != nil {
			t.Fatalf("correlate: %v", err)
		}
		if ev.PromptDigestMatch || ev.TaskComplete != nil {
			t.Fatalf("out-of-order entries must not correlate: %+v", ev)
		}
	})

	t.Run("promptDigestMismatch", func(t *testing.T) {
		entries := parseEntriesForTest(t,
			harnessTurnContext(h, testTurnID, nil),
			harnessUserItem("t-x", "a different prompt entirely"),
		)
		ev, err := CorrelateRolloutTurn(entries, pins, testThreadID, "t-x", "att-t-x", digest)
		if err != nil {
			t.Fatalf("correlate: %v", err)
		}
		if ev.PromptDigestMatch {
			t.Fatal("a foreign user item must not satisfy the pdig match")
		}
	})

	t.Run("noTaskCompleteStaysIncomplete", func(t *testing.T) {
		entries := parseEntriesForTest(t,
			harnessTurnContext(h, testTurnID, nil),
			harnessUserItem("t-x", prompt),
		)
		ev, err := CorrelateRolloutTurn(entries, pins, testThreadID, "t-x", "att-t-x", digest)
		if err != nil {
			t.Fatalf("correlate: %v", err)
		}
		if ev.TaskComplete != nil {
			t.Fatal("no terminal record must stay incomplete")
		}
	})

	t.Run("unrelatedEntriesSkipped", func(t *testing.T) {
		entries := parseEntriesForTest(t,
			`{"timestamp":"…","type":"event_msg","payload":{"type":"task_started"}}`,
			harnessTurnContext(h, testTurnID, nil),
			`{"timestamp":"…","type":"event_msg","payload":{"type":"token_count","total":{}}}`,
			harnessUserItem("t-x", prompt),
			`{"timestamp":"…","type":"response_item","payload":{"type":"message","role":"assistant","content":"working"}}`,
			harnessTaskComplete("done", false),
		)
		ev, err := CorrelateRolloutTurn(entries, pins, testThreadID, "t-x", "att-t-x", digest)
		if err != nil {
			t.Fatalf("correlate: %v", err)
		}
		if !ev.TurnContextMatch || !ev.PromptDigestMatch || ev.TaskComplete == nil {
			t.Fatalf("unrelated entries must be skipped: %+v", ev)
		}
	})
}

func parseEntriesForTest(t *testing.T, lines ...string) []rolloutEntry {
	t.Helper()
	var entries []rolloutEntry
	for _, l := range lines {
		e, err := parseRolloutLine([]byte(l))
		if err != nil {
			t.Fatalf("parse %q: %v", l, err)
		}
		entries = append(entries, e)
	}
	return entries
}

// ── Protection classification (§3.7) ────────────────────────────────────

func TestRolloutProtection_UnverifiedPlatformFailsClosed(t *testing.T) {
	// Windows simulation (portable pure function): the unverified
	// platform never freezes protected evidence, regardless of what the
	// seam or durable rows report — integrity=unverified, decisions
	// degrade to Uncertain downstream.
	class, id := rolloutProtectionClass("windows", true, testAttestationID(), testAttestationID())
	if class != "unverified" || id != "" {
		t.Fatalf("windows must degrade to unverified, got %q/%q", class, id)
	}
	if rolloutTrustSupported() {
		t.Log("running on a verified platform; the windows classification is exercised through the pure function")
	}
}

func TestRolloutProtection_AdvisoryDefaultAndProtectedMatch(t *testing.T) {
	id := testAttestationID()
	if class, got := rolloutProtectionClass("linux", false, "", id); class != "advisory" || got != "" {
		t.Fatalf("no seam attestation must stay advisory, got %q/%q", class, got)
	}
	if class, got := rolloutProtectionClass("linux", true, id, ""); class != "advisory" || got != "" {
		t.Fatalf("seam id without a durable row must stay advisory, got %q/%q", class, got)
	}
	if class, got := rolloutProtectionClass("linux", true, id, "cprot-v2:sha256:"+strings.Repeat("cd", 32)); class != "advisory" || got != "" {
		t.Fatalf("row/seam disagreement must stay advisory, got %q/%q", class, got)
	}
	if class, got := rolloutProtectionClass("linux", true, id, id); class != "protected" || got != id {
		t.Fatalf("seam + matching row must freeze protected, got %q/%q", class, got)
	}
}

// ── Adapter: protection frozen at launch ────────────────────────────────

func TestCodexAdapter_ProtectionFrozenAtLaunch(t *testing.T) {
	t.Run("protected", func(t *testing.T) {
		h := newAdapterHarness(t)
		scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
		scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
		scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, true)...)
		writeScenario(t, h.scratch, scenario...)
		h.createAndPersist(t)
		seedRollout(t, h, testThreadID)
		insertHarnessAttestation(t, h, testAttestationID())

		out, err := h.dispatch(t, "t-prot", "prompt")
		if err != nil || out.Status != adapter.DispatchAccepted {
			t.Fatalf("dispatch: %+v err=%v", out, err)
		}
		att := waitAttempt(t, h, "t-prot", func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
		if att.RolloutProtection != "protected" {
			t.Fatalf("protection must freeze to protected, got %q", att.RolloutProtection)
		}
		if att.AttestationID == nil || *att.AttestationID != testAttestationID() {
			t.Fatalf("attestation id must be frozen: %v", att.AttestationID)
		}
	})

	t.Run("advisoryWithoutRow", func(t *testing.T) {
		h := newAdapterHarness(t)
		scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
		scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
		scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, true)...)
		writeScenario(t, h.scratch, scenario...)
		h.createAndPersist(t)
		seedRollout(t, h, testThreadID)
		// The seam reports an attestation, but no durable cprot-v2 row
		// matches the frozen tuple: the freeze stays advisory.

		out, err := h.dispatch(t, "t-adv", "prompt")
		if err != nil || out.Status != adapter.DispatchAccepted {
			t.Fatalf("dispatch: %+v err=%v", out, err)
		}
		att := waitAttempt(t, h, "t-adv", func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
		if att.RolloutProtection != "advisory" || att.AttestationID != nil {
			t.Fatalf("advisory freeze: %q %v", att.RolloutProtection, att.AttestationID)
		}
	})

	t.Run("uncoveredRowStaysAdvisory", func(t *testing.T) {
		h := newAdapterHarness(t)
		scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
		scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
		scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, true)...)
		writeScenario(t, h.scratch, scenario...)
		h.createAndPersist(t)
		seedRollout(t, h, testThreadID)
		// A tuple-matching row whose records do NOT cover the frozen
		// profile (one sibling_read record and one approval record —
		// the minimal shape the §3.7 encoder accepts) never freezes
		// protected evidence: coverage is enforced at lookup, not only
		// at recording.
		partial := ProtectionAttestation{
			CodexVersion: h.policy.AppServerVersion, PlatformOS: h.policy.PlatformOS, PlatformFamily: h.policy.PlatformFamily,
			ManifestDigest: h.policy.ManifestDigest, ProfileDigest: h.profileDigest,
			ProbeRecords: []ProbeRecord{{Class: RecordSiblingRead, ToolClass: ToolRead, ToolName: "Read",
				Operation: OpRead, Denied: true, EnforcingCapability: CapSandboxRestrictedFS, DenialText: "denied"}},
			ApprovalDenies: []ApprovalDenyRecord{{MethodName: "execCommandApproval", RefusalKind: RefusalNativeEnum}},
			ProbedAt:       "2026-09-23T00:00:00Z", Actor: "op",
		}
		raw, err := partial.EncodeProbeRecords()
		if err != nil {
			t.Fatalf("encode partial frame: %v", err)
		}
		insertHarnessAttestationRaw(t, h, testAttestationID(), h.policy.ManifestDigest, raw)

		out, err := h.dispatch(t, "t-uncovered", "prompt")
		if err != nil || out.Status != adapter.DispatchAccepted {
			t.Fatalf("dispatch: %+v err=%v", out, err)
		}
		att := waitAttempt(t, h, "t-uncovered", func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
		if att.RolloutProtection != "advisory" || att.AttestationID != nil {
			t.Fatalf("an uncovered row must degrade to advisory, got %q %v", att.RolloutProtection, att.AttestationID)
		}
	})

	t.Run("manifestDigestMismatchFailsClosed", func(t *testing.T) {
		h := newAdapterHarness(t)
		scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
		scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
		scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, true)...)
		writeScenario(t, h.scratch, scenario...)
		h.createAndPersist(t)
		seedRollout(t, h, testThreadID)
		// A valid otherwise-matching attestation row whose manifest
		// digest DIFFERS from the launch's frozen manifest digest must
		// never satisfy the tuple: protection degrades to advisory,
		// with no attestation id frozen.
		insertHarnessAttestation(t, h, testAttestationID(), "sha256:"+strings.Repeat("ff", 32))
		if h.policy.ManifestDigest == "sha256:"+strings.Repeat("ff", 32) {
			t.Fatal("fixture invariant broken: the divergent digest must differ from the frozen one")
		}

		out, err := h.dispatch(t, "t-mismatch", "prompt")
		if err != nil || out.Status != adapter.DispatchAccepted {
			t.Fatalf("dispatch: %+v err=%v", out, err)
		}
		att := waitAttempt(t, h, "t-mismatch", func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
		if att.RolloutProtection != "advisory" || att.AttestationID != nil {
			t.Fatalf("a manifest-digest mismatch must degrade to advisory, got %q %v",
				att.RolloutProtection, att.AttestationID)
		}
	})
}

// ── Adapter: §3.7 acceptance upgrade + §3.5 step-3 confirmation ─────────

// protectedTurnScenario stages a MATERIALIZED binding (second-turn
// shape): the rollout is seeded and materialized before dispatch, so the
// attempt's baseline is real and the natively-appended turn entries land
// in the tail past it. The attestation row makes the attempt protected.
func protectedTurnScenario(t *testing.T, turnKey, prompt string, withComplete bool, extraRolloutLines func(h *adapterHarness) []string) (*adapterHarness, adapter.TurnRef) {
	t.Helper()
	h := newAdapterHarness(t)
	lines := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	lines = append(lines, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	lines = append(lines, turnAcceptedRules(testThreadID, testTurnID, withComplete)...)
	if extraRolloutLines != nil {
		path := filepath.Join(h.scratch, ".codex", "sessions", "2026", "09", "23",
			"rollout-2026-09-23T00-00-00-"+testThreadID+".jsonl")
		raw, err := json.Marshal(map[string]any{"method": "turn/start", "path": path, "lines": extraRolloutLines(h)})
		if err != nil {
			t.Fatalf("marshal append rule: %v", err)
		}
		lines = append(lines, `{"append_on_request": `+string(raw)+`}`)
	}
	writeScenario(t, h.scratch, lines...)

	h.createAndPersist(t)
	path := seedRollout(t, h, testThreadID)
	if err := h.store.MarkCodexSessionMaterialized(context.Background(), testSessionID, path); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	insertHarnessAttestation(t, h, testAttestationID())

	out, err := h.dispatch(t, turnKey, prompt)
	if err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	return h, adapter.TurnRef{SessionID: testSessionID, TurnKey: turnKey}
}

func TestCodexAdapter_ProtectedCorrelationWritesAccepted(t *testing.T) {
	turnKey := "t-pacc"
	prompt := "review the rollout tail for acceptance"
	h, _ := protectedTurnScenario(t, turnKey, prompt, true, func(h *adapterHarness) []string {
		return []string{
			harnessTurnContext(h, testTurnID, nil),
			harnessUserItem(turnKey, prompt),
			harnessTaskComplete("durable terminal", false),
		}
	})

	att := waitAttempt(t, h, turnKey, func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "completed" {
		t.Fatalf("terminal classification: %q", att.ObservedStatus)
	}
	if att.Accepted == nil || !*att.Accepted {
		t.Fatalf("protected verified correlation must write accepted, got %v", att.Accepted)
	}
	if att.ResultPayload == nil || !strings.Contains(*att.ResultPayload, "completed") {
		t.Fatalf("result payload: %v", att.ResultPayload)
	}
}

func TestCodexAdapter_ProtectedTurnContextDriftIsUncertain(t *testing.T) {
	turnKey := "t-pdrift"
	prompt := "review the rollout tail for drift"
	h, _ := protectedTurnScenario(t, turnKey, prompt, true, func(h *adapterHarness) []string {
		return []string{
			harnessTurnContext(h, testTurnID, func(p map[string]any) { p["approval_policy"] = "never" }),
			harnessUserItem(turnKey, prompt),
			harnessTaskComplete("must not count", false),
		}
	})

	// The turn executed under unverified policy: the child is terminated
	// and NO terminal is committed.
	deadline := time.Now().Add(3 * time.Second)
	for terminatedCount(t, h.scratch) < 1 {
		att, err := h.store.GetLatestCodexTurnAttempt(context.Background(), testSessionID, turnKey)
		if err == nil && att != nil && att.Terminal {
			t.Fatal("a drifted protected turn must never reach terminal")
		}
		if time.Now().After(deadline) {
			t.Fatal("protected drift must terminate the child")
		}
		time.Sleep(10 * time.Millisecond)
	}
	att, err := h.store.GetLatestCodexTurnAttempt(context.Background(), testSessionID, turnKey)
	if err != nil || att == nil {
		t.Fatalf("attempt: %v", err)
	}
	if att.Terminal {
		t.Fatal("the drifted turn must not be terminal")
	}
	if att.Accepted != nil {
		t.Fatalf("drifted turn must not be accepted: %v", *att.Accepted)
	}

	// The unresolved attempt blocks the native session (§3.5).
	out, err := h.dispatch(t, "t-after-drift", "prompt")
	if err != nil || out.Status != adapter.DispatchRejected || !strings.Contains(out.Reason, "unresolved attempt") {
		t.Fatalf("session must block after protected drift: %+v err=%v", out, err)
	}
}

func TestCodexAdapter_AdvisoryDriftDiagnosticOnly(t *testing.T) {
	turnKey := "t-adrift"
	prompt := "review the rollout tail for advisory drift"
	// No attestation row: the attempt stays advisory.
	h := newAdapterHarness(t)
	lines := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	lines = append(lines, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	lines = append(lines, turnAcceptedRules(testThreadID, testTurnID, true)...)
	path := filepath.Join(h.scratch, ".codex", "sessions", "2026", "09", "23",
		"rollout-2026-09-23T00-00-00-"+testThreadID+".jsonl")
	raw, err := json.Marshal(map[string]any{"method": "turn/start", "path": path, "lines": []string{
		harnessTurnContext(h, testTurnID, func(p map[string]any) { p["model"] = "gpt-6-astra" }),
	}})
	if err != nil {
		t.Fatalf("marshal append rule: %v", err)
	}
	lines = append(lines, `{"append_on_request": `+string(raw)+`}`)
	writeScenario(t, h.scratch, lines...)

	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	out, err := h.dispatch(t, turnKey, prompt)
	if err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}

	att := waitAttempt(t, h, turnKey, func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "completed" {
		t.Fatalf("advisory drift is diagnostic only; the in-life terminal stands: %q", att.ObservedStatus)
	}
	if att.Accepted != nil {
		t.Fatalf("advisory attempts are never accepted-upgraded: %v", *att.Accepted)
	}
}

// ── Adapter: Observe ────────────────────────────────────────────────────

func usageNotification(threadID, turnID string, in, out int64) string {
	return `{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"threadId":"` + threadID + `","turnId":"` + turnID + `","info":{"total":{"token_usage":{"input_tokens":` + fmt.Sprint(in) + `,"output_tokens":` + fmt.Sprint(out) + `}}}}}`
}

func itemNotification(threadID, turnID, text string) string {
	return `{"jsonrpc":"2.0","method":"item/agentMessage/updated","params":{"threadId":"` + threadID + `","turnId":"` + turnID + `","item":{"type":"agentMessage","text":"` + text + `"}}}`
}

func TestCodexAdapter_ObserveStreamsItemsUsageAndTerminal(t *testing.T) {
	h := newAdapterHarness(t)
	lines := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	lines = append(lines, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	// Item, then usage snapshots out of order (an older snapshot arriving
	// late must never overwrite the newer cumulative one), then the
	// terminal — all riding the turn/start request in emission order.
	raw, err := json.Marshal(map[string]any{"method": "turn/start", "lines": []json.RawMessage{
		json.RawMessage(itemNotification(testThreadID, testTurnID, "step one")),
		json.RawMessage(usageNotification(testThreadID, testTurnID, 200, 100)),
		json.RawMessage(usageNotification(testThreadID, testTurnID, 50, 25)),
	}})
	if err != nil {
		t.Fatalf("marshal many rule: %v", err)
	}
	lines = append(lines, `{"emit_many_on_request": `+string(raw)+`}`)
	lines = append(lines, turnAcceptedRules(testThreadID, testTurnID, true)...)
	writeScenario(t, h.scratch, lines...)

	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-obs"}
	if out, err := h.dispatch(t, ref.TurnKey, "prompt"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}

	stream, err := h.adapter.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	var sawItem, sawUsage, sawTerminal bool
	deadline := time.After(5 * time.Second)
	for !(sawItem && sawUsage && sawTerminal) {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				t.Fatalf("stream closed early: %v", stream.Err())
			}
			switch {
			case ev.Type == adapter.EventProgress && strings.Contains(ev.Payload, "agentMessage"):
				sawItem = true
			case ev.Type == adapter.EventProgress && ev.Usage.InputTokens.Available:
				sawUsage = true
				if ev.Usage.InputTokens.Value != 200 || ev.Usage.OutputTokens.Value != 100 {
					t.Fatalf("usage must stay at the NEWER cumulative snapshot: %+v", ev.Usage)
				}
				if ev.Usage.TotalCostUSD.Available {
					t.Fatal("cost is never fabricated")
				}
			case ev.Type == adapter.EventTerminal:
				sawTerminal = true
				if ev.Status != council.TurnCompleted {
					t.Fatalf("terminal status: %s", ev.Status)
				}
			}
		case <-deadline:
			t.Fatalf("observation incomplete: item=%v usage=%v terminal=%v", sawItem, sawUsage, sawTerminal)
		}
	}

	// Collect carries the same verified terminal with the monotonic
	// usage snapshot; cost stays unavailable.
	res, err := h.adapter.Collect(context.Background(), ref)
	if err != nil || res.ResultStatus != adapter.ResultAvailable || res.Status != council.TurnCompleted {
		t.Fatalf("collect: %+v err=%v", res, err)
	}
	if !res.Usage.InputTokens.Available || res.Usage.InputTokens.Value != 200 ||
		!res.Usage.OutputTokens.Available || res.Usage.OutputTokens.Value != 100 {
		t.Fatalf("collect usage: %+v", res.Usage)
	}
	if res.Usage.TotalCostUSD.Available {
		t.Fatal("collected cost must never be fabricated")
	}
}

func TestCodexAdapter_ObserveOverflowTerminatesObserverOnly(t *testing.T) {
	h := newAdapterHarness(t)
	lines := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	lines = append(lines, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	manyLines := make([]json.RawMessage, 80)
	for i := range manyLines {
		manyLines[i] = json.RawMessage(itemNotification(testThreadID, testTurnID, fmt.Sprintf("event %d", i)))
	}
	raw, err := json.Marshal(map[string]any{"method": "turn/start", "lines": manyLines})
	if err != nil {
		t.Fatalf("marshal many rule: %v", err)
	}
	lines = append(lines, `{"emit_many_on_request": `+string(raw)+`}`)
	lines = append(lines, turnAcceptedRules(testThreadID, testTurnID, true)...)
	writeScenario(t, h.scratch, lines...)

	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-overflow"}
	if out, err := h.dispatch(t, ref.TurnKey, "prompt"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}
	stream, err := h.adapter.Observe(context.Background(), ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}

	// The observer never consumes: the bounded stream terminates with
	// ErrBufferOverflow…
	deadline := time.Now().Add(5 * time.Second)
	var streamErr error
	for {
		if err := stream.Err(); err != nil {
			streamErr = err
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the overflow never terminated the observer stream")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !errors.Is(streamErr, adapter.ErrBufferOverflow) {
		t.Fatalf("overflow must surface ErrBufferOverflow, got %v", streamErr)
	}

	// …while the native stream is unaffected: the turn still reaches its
	// verified terminal.
	att := waitAttempt(t, h, ref.TurnKey, func(a *storage.CodexTurnAttempt) bool { return a.Terminal })
	if att.ObservedStatus != "completed" {
		t.Fatalf("native terminal: %q", att.ObservedStatus)
	}
}

func TestCodexAdapter_ObserveCtxCancelDetachesOnly(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, false)...) // never completes
	writeScenario(t, h.scratch, scenario...)
	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	ref := adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-detach"}
	if out, err := h.dispatch(t, ref.TurnKey, "prompt"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("dispatch: %+v err=%v", out, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := h.adapter.Observe(ctx, ref)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	buffered, ok := stream.(*adapter.BufferedStream)
	if !ok {
		t.Fatalf("observe must hand out the buffered stream, got %T", stream)
	}
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case <-buffered.Done():
			goto detached
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelling the observer context must detach (close) the stream")
		}
		time.Sleep(5 * time.Millisecond)
	}
detached:
	// The turn is unaffected: the child keeps running and the slot stays
	// held by the detached turn's attempt.
	if terminatedCount(t, h.scratch) != 0 {
		t.Fatal("an observer detach must never terminate the child")
	}
	h.adapter.mu.Lock()
	slot := h.adapter.singleFlt[testThreadID]
	h.adapter.mu.Unlock()
	if slot == nil || slot.owner != "att-t-detach" {
		t.Fatalf("the detached turn must keep holding the slot, got %+v", slot)
	}
}

// ── Adapter: the §3.9 eligibility gate fires at EVERY launch ────────────

func TestCodexAdapter_EligibilityGateFiresOnEveryLaunch(t *testing.T) {
	h := newAdapterHarness(t)
	scenario := append([]string{authOKLine()}, threadStartRules(testThreadID, h.wsRoot, h.model)...)
	scenario = append(scenario, resumeRule(testThreadID, h.wsRoot, h.model, nil))
	scenario = append(scenario, turnAcceptedRules(testThreadID, testTurnID, true)...)
	writeScenario(t, h.scratch, scenario...)
	h.createAndPersist(t)
	seedRollout(t, h, testThreadID)

	// First turn accepted and terminal; the child is live.
	if out, err := h.dispatch(t, "t-gate-1", "first"); err != nil || out.Status != adapter.DispatchAccepted {
		t.Fatalf("first dispatch: %+v err=%v", out, err)
	}
	waitAttempt(t, h, "t-gate-1", func(a *storage.CodexTurnAttempt) bool { return a.Terminal })

	// Eligibility lapses while the child is STILL live: the next launch
	// must pass the gate again (TOCTOU hardening) and fail closed.
	h.eligible.Store(false)
	out, err := h.dispatch(t, "t-gate-2", "second")
	var missing *ErrProductionEligibilityMissing
	if !errors.As(err, &missing) {
		t.Fatalf("the gate must fire on every launch, got %T: %v (outcome %+v)", err, err, out)
	}
	if out.Status != adapter.DispatchRejected {
		t.Fatalf("gate refusal is a pre-acceptance rejection, got %s", out.Status)
	}
	if args := readFixtureFile(t, h.scratch, ".codex-fixture-args"); len(args) != 1 {
		t.Fatalf("no replacement child may start without eligibility, launches: %v", args)
	}
}

// ── Adapter: Collect gating ─────────────────────────────────────────────

func TestCodexAdapter_CollectMalformedAndUnattributable(t *testing.T) {
	h := newAdapterHarness(t)
	h.createAndPersist(t)
	ctx := context.Background()

	// A terminal whose stored payload is not well-formed JSON evidence
	// is ResultMalformed, never reinterpreted.
	if err := h.store.InsertCodexTurnAttempt(ctx, storage.CodexTurnAttempt{
		AttemptID: "att-malformed", SessionID: testSessionID, TurnKey: "t-malformed",
		PromptDigest: "pdig-v1:sha256:fixed", RolloutProtection: "advisory",
		BaselineMaterialized: false,
	}); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	// InsertCodexTurnAttempt leaves native_turn_id NULL (bound in-life);
	// this storage-seeded attempt IS turn-attributable.
	if _, err := h.store.DB().ExecContext(ctx,
		`UPDATE codex_turn_attempts SET native_turn_id = ? WHERE attempt_id = ?`,
		testTurnID, "att-malformed"); err != nil {
		t.Fatalf("bind native turn id: %v", err)
	}
	if err := h.store.SetCodexAttemptTerminal(ctx, "att-malformed", "completed", "not-json{", ""); err != nil {
		t.Fatalf("seed terminal: %v", err)
	}
	res, err := h.adapter.Collect(ctx, adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-malformed"})
	if err != nil {
		t.Fatalf("malformed collect: %v", err)
	}
	if res.ResultStatus != adapter.ResultMalformed {
		t.Fatalf("corrupt payload must be ResultMalformed, got %s", res.ResultStatus)
	}

	// A terminal with no bound native turn id is not THIS turn's
	// collectable evidence.
	if err := h.store.InsertCodexTurnAttempt(ctx, storage.CodexTurnAttempt{
		AttemptID: "att-unbound", SessionID: testSessionID, TurnKey: "t-unbound",
		PromptDigest: "pdig-v1:sha256:fixed", RolloutProtection: "advisory",
	}); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}
	if err := h.store.SetCodexAttemptTerminal(ctx, "att-unbound", "completed", `{"status":"completed"}`, ""); err != nil {
		t.Fatalf("seed terminal: %v", err)
	}
	res, err = h.adapter.Collect(ctx, adapter.TurnRef{SessionID: testSessionID, TurnKey: "t-unbound"})
	if err != nil {
		t.Fatalf("unattributable collect: %v", err)
	}
	if res.ResultStatus != adapter.ResultUnavailable {
		t.Fatalf("unattributable terminal must be unavailable, got %s", res.ResultStatus)
	}
}

func strPtr(s string) *string { return &s }
