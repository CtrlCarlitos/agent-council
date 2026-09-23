//go:build unix

package service

// Task 6 AC-008 wiring evidence: fail-closed service construction for
// the Claude adapter (every configuration piece required), successful
// wiring with a complete operator configuration, and the protection-
// attestation journal operation — operator authority, canonical
// persistence, idempotency, and attempt freezing.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/CtrlCarlitos/agent-council/internal/adapter/claude"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

func compileClaudeStubForService(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\ncase \"$1\" in\n" +
		"  --version) echo \"2.1.278 (Claude Code)\" ;;\n" +
		"  --help) echo \"Usage: claude [options]\"; " +
		"echo \"  -p, --output-format --verbose --session-id --resume --model --max-turns --allowedTools --disallowedTools\" ;;\n" +
		"esac\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write claude stub: %v", err)
	}
	return dir
}

// claudeWiringConfig builds a complete, valid Claude service
// configuration over disjoint operator-provisioned directories.
func claudeWiringConfig(t *testing.T, dir string, binDir string) ServerConfig {
	t.Helper()
	stateDir := filepath.Join(dir, "state")
	wsBase := filepath.Join(dir, "workspaces")
	configBase := filepath.Join(dir, "claude-config")
	scratchRoot := filepath.Join(dir, "claude-probe-scratch")
	templateDir := filepath.Join(dir, "claude-template")
	evidenceRoot := filepath.Join(dir, "claude-evidence")
	for _, d := range []string{stateDir, wsBase, templateDir, evidenceRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return ServerConfig{
		StateDir:               stateDir,
		InstanceID:             "inst-ac008-wire",
		AuthToken:              "tok-ac008-wire",
		WorkspaceBaseDir:       wsBase,
		ClaudeBinaryPath:       filepath.Join(binDir, "claude"),
		ClaudeConfigBaseDir:    configBase,
		ClaudeTemplateDir:      templateDir,
		ClaudeEvidenceRoot:     evidenceRoot,
		ClaudeProbeProfile:     claudeProbeProfileForService(),
		ClaudeProbeScratchRoot: scratchRoot,
	}
}

func claudeProbeProfileForService() storage.CanonicalProfile {
	return storage.CanonicalProfile{
		AlgoVersion:         "cprof-v1",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"claude", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"claude": {Model: "claude-haiku-4-5-20251001", NativeAuthMode: "inherited_host_keychain"},
		},
	}
}

// Every Claude configuration piece is required: removing any one fails
// construction before the service starts.
func TestServiceWiring_ClaudeConfigurationFailClosed(t *testing.T) {
	dir := t.TempDir()
	binDir := compileClaudeStubForService(t)
	base := claudeWiringConfig(t, dir, binDir)

	store, err := storage.Open(storage.StoreOptions{StateDir: base.StateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	lock := mustLock(t, base.StateDir)

	mutations := []struct {
		name    string
		mutate  func(*ServerConfig)
		wantErr string
	}{
		{"missing config base", func(c *ServerConfig) { c.ClaudeConfigBaseDir = "" }, "ClaudeConfigBaseDir is required"},
		{"missing scratch root", func(c *ServerConfig) { c.ClaudeProbeScratchRoot = "" }, "ClaudeProbeScratchRoot is required"},
		{"missing template dir", func(c *ServerConfig) { c.ClaudeTemplateDir = "" }, "ClaudeTemplateDir is required"},
		{"missing template on disk", func(c *ServerConfig) { c.ClaudeTemplateDir = filepath.Join(dir, "absent-template") }, "is missing"},
		{"missing evidence root", func(c *ServerConfig) { c.ClaudeEvidenceRoot = "" }, "ClaudeEvidenceRoot is required"},
		{"missing probe profile", func(c *ServerConfig) { c.ClaudeProbeProfile = storage.CanonicalProfile{} }, "non-empty canonical profile"},
	}
	for _, tc := range mutations {
		cfg := base
		tc.mutate(&cfg)
		_, err := NewServerWithAdapter(store, lock, cfg, nil)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("%s: expected failure containing %q, got %v", tc.name, tc.wantErr, err)
		}
	}

	// Configuring both persistent adapters is rejected.
	both := base
	both.OpenCodeBinaryPath = "opencode"
	if _, err := NewServerWithAdapter(store, lock, both, nil); err == nil ||
		!strings.Contains(err.Error(), "only one persistent contributor adapter") {
		t.Fatalf("both adapters configured must be rejected, got %v", err)
	}
}

// A complete operator configuration wires the production Claude adapter.
func TestServiceWiring_ClaudeConstructionSucceeds(t *testing.T) {
	dir := t.TempDir()
	binDir := compileClaudeStubForService(t)
	cfg := claudeWiringConfig(t, dir, binDir)

	store, err := storage.Open(storage.StoreOptions{StateDir: cfg.StateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	srv, err := NewServerWithAdapter(store, mustLock(t, cfg.StateDir), cfg, nil)
	if err != nil {
		t.Fatalf("construction with complete configuration: %v", err)
	}
	if srv == nil || srv.PolicyExecutor() == nil {
		t.Fatal("wired server must be usable")
	}
}

// validServiceAttestation builds a typed cprot-v1 attestation.
func validServiceAttestation(actor string) claude.ProtectionAttestation {
	return claude.ProtectionAttestation{
		ClaudeVersion:  "2.1.278",
		Platform:       "linux/amd64",
		ManifestDigest: "cprof-v2:sha256:1111111111111111111111111111111111111111111111111111111111111111",
		TemplateDigest: "ctmpl-v1:sha256:2222222222222222222222222222222222222222222222222222222222222222",
		Records: []claude.ProbeRecord{{
			ToolClass:           claude.ProbeRead,
			ToolName:            "Read",
			Denied:              true,
			EnforcingCapability: claude.CapCwdBoundary,
			DenialText:          "Claude requested permissions to read the sibling transcript, but you haven't granted it yet",
		}},
		ProbedAt: "2026-09-22T00:00:00Z",
		Actor:    actor,
	}
}

const attestationRunID = "run-attest"

// newAttestationRun creates the run provenance the attestation journal
// entry requires and returns the server wired over the same store.
func newAttestationServer(t *testing.T) (*Server, *storage.Store) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.CreateRunWithProfile(context.Background(), storage.CreateRunWithProfileRequest{
		OpID: "op-run-attest", ControllerLease: "lease-attest", RunID: attestationRunID,
		Brief: "attestation", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      claudeProbeProfileForService(),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	srv, err := NewServer(store, mustLock(t, stateDir), ServerConfig{
		StateDir: stateDir, InstanceID: "inst-attest", AuthToken: "tok-attest",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, store
}

// The attestation journal operation requires the operator actor bound
// to the request and matching the attestation, then persists the row
// with its journal entry; replay is idempotent and conflicting reuse of
// the op_id is rejected.
func TestServiceAttestation_OperatorAuthorityAndIdempotency(t *testing.T) {
	srv, store := newAttestationServer(t)
	ctx := context.Background()

	// Missing actor: refused before any write.
	if _, err := srv.RecordClaudeProbeAttestation(ctx, ClaudeProbeAttestationRequest{
		OpID: "op-att-1", RunID: attestationRunID, Attestation: validServiceAttestation("operator-test"),
	}); err == nil || !strings.Contains(err.Error(), "operator actor") {
		t.Fatalf("missing actor must be refused, got %v", err)
	}
	// Actor mismatch: refused.
	if _, err := srv.RecordClaudeProbeAttestation(ctx, ClaudeProbeAttestationRequest{
		OpID: "op-att-1", Actor: "someone-else", RunID: attestationRunID, Attestation: validServiceAttestation("operator-test"),
	}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("actor mismatch must be refused, got %v", err)
	}
	// denied=false evidence is invalid cprot-v1 and refused.
	invalid := validServiceAttestation("operator-test")
	invalid.Records[0].Denied = false
	if _, err := srv.RecordClaudeProbeAttestation(ctx, ClaudeProbeAttestationRequest{
		OpID: "op-att-1", Actor: "operator-test", RunID: attestationRunID, Attestation: invalid,
	}); err == nil {
		t.Fatal("denied=false attestation must be refused")
	}

	// Valid recording: receipt, durable row, journal entry.
	att := validServiceAttestation("operator-test")
	receipt, err := srv.RecordClaudeProbeAttestation(ctx, ClaudeProbeAttestationRequest{
		OpID: "op-att-1", Actor: "operator-test", RunID: attestationRunID, Attestation: att,
	})
	if err != nil {
		t.Fatalf("valid attestation: %v", err)
	}
	if receipt.CommandType != "record_claude_attestation" || receipt.Payload == "" {
		t.Fatalf("unexpected receipt %+v", receipt)
	}
	digest, err := att.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	var probeResults string
	if err := store.DB().QueryRow(
		`SELECT probe_results FROM claude_protection_attestations WHERE attestation_id = ?`, digest,
	).Scan(&probeResults); err != nil {
		t.Fatalf("durable attestation row: %v", err)
	}
	if len(probeResults) == 0 {
		t.Fatal("probe results must be persisted in canonical encoding")
	}
	var journalCount int
	if err := store.DB().QueryRow(
		`SELECT count(*) FROM journal_entries WHERE op_id = 'op-att-1' AND command_type = 'record_claude_attestation'`,
	).Scan(&journalCount); err != nil || journalCount != 1 {
		t.Fatalf("journal entry must exist exactly once, got %d err=%v", journalCount, err)
	}

	// Idempotent replay: same op_id and evidence returns the receipt.
	replay, err := srv.RecordClaudeProbeAttestation(ctx, ClaudeProbeAttestationRequest{
		OpID: "op-att-1", Actor: "operator-test", RunID: attestationRunID, Attestation: att,
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Payload != receipt.Payload {
		t.Fatalf("replay must return the committed receipt, got %+v vs %+v", replay, receipt)
	}

	// Same op_id with DIFFERENT evidence: idempotency conflict.
	changed := validServiceAttestation("operator-test")
	changed.ProbedAt = "2026-09-22T01:00:00Z"
	if _, err := srv.RecordClaudeProbeAttestation(ctx, ClaudeProbeAttestationRequest{
		OpID: "op-att-1", Actor: "operator-test", RunID: attestationRunID, Attestation: changed,
	}); err == nil {
		t.Fatal("op_id reuse with different evidence must conflict")
	}

	// Same evidence under a new op_id: the row already exists — rejected.
	if _, err := srv.RecordClaudeProbeAttestation(ctx, ClaudeProbeAttestationRequest{
		OpID: "op-att-2", Actor: "operator-test", RunID: attestationRunID, Attestation: att,
	}); err == nil || !strings.Contains(err.Error(), "already recorded") {
		t.Fatalf("duplicate attestation identity must be rejected, got %v", err)
	}
}

// Attempt freezing: the recorded attestation authorizes the protected
// absence-redispatch transition for an attempt that froze its identity
// at baseline; a later attestation never upgrades an attempt that
// froze a different id.
func TestServiceAttestation_AttemptFreezing(t *testing.T) {
	srv, store := newAttestationServer(t)
	ctx := context.Background()

	if _, err := srv.RecordClaudeProbeAttestation(ctx, ClaudeProbeAttestationRequest{
		OpID: "op-att-freeze", Actor: "operator-test", RunID: attestationRunID, Attestation: validServiceAttestation("operator-test"),
	}); err != nil {
		t.Fatalf("record attestation: %v", err)
	}
	att := validServiceAttestation("operator-test")
	digest, err := att.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	if err := store.InsertClaudeTurnAttempt(ctx, storage.ClaudeTurnAttempt{
		AttemptID: "att-freeze", SessionID: "sess-freeze", TurnKey: "t-freeze",
		NativeID: "nat-freeze", PromptDigest: "pd", TranscriptProtection: "protected", AttestationID: digest,
	}); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}

	seq1, err := store.ReserveClaudeLaunch(ctx, "att-freeze", "fixture")
	if err != nil {
		t.Fatalf("initial launch: %v", err)
	}
	if err := store.RecordClaudeLaunchState(ctx, "att-freeze", seq1, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	exit := 1
	if err := store.RecordClaudeLaunchState(ctx, "att-freeze", seq1, "dead", &exit); err != nil {
		t.Fatalf("dead: %v", err)
	}
	if err := store.RecordClaudeAbsenceVerified(ctx, "att-freeze"); err != nil {
		t.Fatalf("absence verification: %v", err)
	}

	// The frozen attestation authorizes the single protected redispatch.
	if _, err := store.ReserveClaudeLaunch(ctx, "att-freeze", "fixture"); err != nil {
		t.Fatalf("redispatch with the frozen attestation must be authorized: %v", err)
	}

	// A different attestation id can never upgrade the attempt: an
	// attempt frozen with an id that is not durably recorded can never
	// reach verified absence, so the redispatch authorization is
	// unobtainable.
	if err := store.InsertClaudeTurnAttempt(ctx, storage.ClaudeTurnAttempt{
		AttemptID: "att-frozen-unknown", SessionID: "sess-freeze", TurnKey: "t-unknown",
		NativeID: "nat-freeze-2", PromptDigest: "pd", TranscriptProtection: "protected",
		AttestationID: "cprot-v1:sha256:notrecorded",
	}); err != nil {
		t.Fatalf("insert unknown-attestation attempt: %v", err)
	}
	if err := store.RecordClaudeAbsenceVerified(ctx, "att-frozen-unknown"); err == nil {
		t.Fatal("an attempt frozen with an unknown attestation id must never verify absence")
	}
}
