//go:build unix

package service

// Task 9 AC-009 wiring evidence: fail-closed service construction for
// the Codex adapter (every configuration piece required, frozen-evidence
// drift fails the wiring), the §3.8 probe contract against a real stub
// child, the cprot-v2 attestation journal operation (operator authority,
// canonical persistence, idempotency, attempt freezing, manifest-drift
// fail-closed), and ErrProductionEligibilityMissing end-to-end through
// PRODUCTION construction — pre-attestation, before any child starts —
// plus the durable §3.4 creation-uncertainty block across a simulated
// service restart. The codextest fixture harness is NOT involved: the
// stub is a scripted shell `codex` built here.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/CtrlCarlitos/agent-council/internal/adapter"
	"github.com/CtrlCarlitos/agent-council/internal/adapter/codex"
	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

const (
	codexWiringRunID     = "run-ac009-wire"
	codexWiringSessionID = "sess-ac009-wire"
	codexWiringLease     = "lease-ac009-wire"
)

// compileCodexStubForService writes the scripted shell `codex` stub and
// returns its directory (to prepend to PATH). The stub answers the
// version surfaces, serves the app-server JSON-RPC contract (initialize,
// authenticated account/read, a POISONED thread/start whose id is not a
// canonical UUIDv7, the verbatim negative thread/resume, model/list,
// mcpServerStatus/list), and logs every request method to
// .codex-stub-requests in its working directory.
func compileCodexStubForService(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	body := `#!/bin/sh
case "$1" in
  --version) echo "codex-cli 0.154.0" ;;
  app-server)
    if [ "$2" = "daemon" ]; then echo "app-server daemon 0.154.0"; exit 0; fi
    while IFS= read -r line; do
      method=$(printf '%s' "$line" | grep -o '"method":"[^"]*"' | head -1 | cut -d'"' -f4)
      [ -n "$method" ] && printf '%s\n' "$method" >> .codex-stub-requests
      id=$(printf '%s' "$line" | grep -o '"id":[0-9]*' | head -1 | grep -o '[0-9]*')
      case "$line" in
        *'"initialize"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"userAgent":"stub/0.154.0","codexHome":"%s/.codex","platformFamily":"unix","platformOs":"%s"}}\n' "$id" "${HOME:-/tmp}" "$(uname -s | tr 'A-Z' 'a-z')" ;;
        *'"account/read"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"account":{"accountId":"acc"},"requiresOpenaiAuth":false}}\n' "$id" ;;
        *'"thread/start"'*)
          printf '{"jsonrpc":"2.0","method":"thread/started","params":{"thread":{"id":"definitely-not-a-real-id","status":{"type":"idle"}}}}\n'
          printf '{"jsonrpc":"2.0","id":%s,"result":{"id":"definitely-not-a-real-id","sessionId":"definitely-not-a-real-id","status":{"type":"idle"},"model":"gpt-5.6-sol"}}\n' "$id" ;;
        *'"thread/resume"'*) printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"no rollout found for thread id 00000000-0000-0000-0000-000000000000"}}\n' "$id" ;;
        *'"model/list"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"models":[{"id":"gpt-5.6-sol"}]}}\n' "$id" ;;
        *'"mcpServerStatus/list"'*) printf '{"jsonrpc":"2.0","id":%s,"result":{"servers":[]}}\n' "$id" ;;
      esac
    done
    ;;
  login) echo "Not logged in" ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write codex stub: %v", err)
	}
	return dir
}

// codexServiceProfile builds the frozen cprof-v3 profile pinned to the
// scratch codex home and a workspace root, staging the event-universe
// evidence under the evidence root and pinning its digest.
func codexServiceProfile(t *testing.T, scratch, evidenceRoot, wsRoot string) (storage.CanonicalProfile, string) {
	t.Helper()
	universe := []string{"initialize", "thread/start"}
	raw, err := json.Marshal(map[string]any{"codex_cli_version": "0.154.0", "methods": universe})
	if err != nil {
		t.Fatalf("marshal universe: %v", err)
	}
	rel := "docs/superpowers/evidence/ac009-native-event-universe-0.154.0.json"
	full := filepath.Join(evidenceRoot, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatalf("evidence dir: %v", err)
	}
	if err := os.WriteFile(full, raw, 0o600); err != nil {
		t.Fatalf("write universe: %v", err)
	}
	digest := "sha256:" + sha256SumService(string(raw))

	profile := storage.CanonicalProfile{
		AlgoVersion:         "cprof-v3",
		WorkspaceMode:       "none",
		IsolationStrictness: "permissive_dev",
		NetworkMode:         "unrestricted",
		Tooling:             []string{"codex", "git", "go"},
		Harnesses: map[string]storage.HarnessProfileSpec{
			"codex": {
				Model:          "gpt-5.6-sol",
				NativeAuthMode: "inherited_codex_home",
				Codex: &storage.CodexHarnessSpec{
					AppServerVersion:  "0.154.0",
					ModelProvider:     "openai",
					ExpectedCodexHome: filepath.Join(scratch, ".codex"),
					Platform:          storage.CodexPlatformSpec{OS: runtime.GOOS, Family: "unix"},
					SandboxPolicy: storage.CodexSandboxPolicySpec{
						Type:          "workspace-write",
						WritableRoots: []string{wsRoot},
						NetworkAccess: false,
					},
					ApprovalPolicy:             storage.CodexApprovalPolicy{Kind: "string", String: "on-request"},
					ApprovalsReviewer:          "user",
					ExpectedMCPServers:         []string{"context7"},
					ExpectedInstructionSources: []string{"~/.codex/AGENTS.md"},
					RulesEvidence: storage.CodexRulesEvidenceSpec{
						Verified:     []string{"sandbox workspace-write"},
						Unverifiable: []string{"~/.codex/rules/*.rules contents"},
					},
					EventUniversePath:   rel,
					EventUniverseDigest: digest,
				},
			},
		},
		ToolkitManifest: &storage.ToolkitManifestSpec{ToolkitManifest: storage.ToolkitManifest{
			ProbedCLIVersion:       "2.1.278",
			UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
			UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
			ApprovedTools:          []string{"Read", "Glob"},
			DeniedComplement:       []string{"Bash"},
			ExpectedHooks:          []string{"SessionStart:startup"},
			TurnsBound:             8,
		}},
	}
	return profile, digest
}

// codexProfileDigest is the frozen cprof-v3 digest the production
// constructor freezes (the SAME computation, storage.ComputeProfileDigest).
func codexProfileDigest(t *testing.T, profile storage.CanonicalProfile) string {
	t.Helper()
	digest, _, err := storage.ComputeProfileDigest(profile)
	if err != nil {
		t.Fatalf("profile digest: %v", err)
	}
	return digest
}

// codexWiringDirs carries the per-test layout.
type codexWiringDirs struct {
	root, stateDir, wsBase, scratch, evidenceRoot string
}

// codexWiringConfig builds a complete, valid Codex service configuration
// over disjoint operator-provisioned directories.
func codexWiringConfig(t *testing.T, dir, binDir string) (ServerConfig, codexWiringDirs, storage.CanonicalProfile, string) {
	t.Helper()
	d := codexWiringDirs{
		root:         dir,
		stateDir:     filepath.Join(dir, "state"),
		wsBase:       filepath.Join(dir, "workspaces"),
		scratch:      filepath.Join(dir, "codex-scratch"),
		evidenceRoot: filepath.Join(dir, "codex-evidence"),
	}
	for _, p := range []string{d.stateDir, d.wsBase, d.scratch, d.evidenceRoot} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
	}
	profile, digest := codexServiceProfile(t, d.scratch, d.evidenceRoot, d.wsBase)
	cfg := ServerConfig{
		StateDir:          d.stateDir,
		InstanceID:        "inst-ac009-wire",
		AuthToken:         "tok-ac009-wire",
		WorkspaceBaseDir:  d.wsBase,
		CodexBinaryPath:   filepath.Join(binDir, "codex"),
		CodexProfile:      profile,
		CodexEvidenceRoot: d.evidenceRoot,
		CodexScratchRoot:  d.scratch,
	}
	return cfg, d, profile, digest
}

// seedCodexRunAndSession creates the run + session + adopted controller
// grant the production birth path requires.
func seedCodexRunAndSession(t *testing.T, store *storage.Store, profile storage.CanonicalProfile) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.CreateRunWithProfile(ctx, storage.CreateRunWithProfileRequest{
		OpID: "op-run-ac009", ControllerLease: codexWiringLease, RunID: codexWiringRunID,
		Brief: "codex service wiring", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile:      profile,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := store.CreateSession(ctx, "op-sess-ac009", codexWiringLease, storage.SessionRecord{
		ID: codexWiringSessionID, RunID: codexWiringRunID, Contributor: "codex", Role: "reviewer",
		IsActiveContributor: true, State: "parked", Visibility: "reachable",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.AdoptController(ctx, "op-adopt-ac009", codexWiringRunID, "codex", "controller-ref-ac009", codexWiringLease, nil, codexWiringLease); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if _, err := store.ConnectRunController(ctx, "op-conn-ac009", codexWiringRunID, codexWiringLease, 1, "inst-ac009-wire"); err != nil {
		t.Fatalf("connect: %v", err)
	}
}

func openCodexWiringStore(t *testing.T, stateDir string) *storage.Store {
	t.Helper()
	store, err := storage.Open(storage.StoreOptions{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func codexCreateRequest() adapter.CreateSessionRequest {
	return adapter.CreateSessionRequest{
		SessionID:   adapter.SessionID(codexWiringSessionID),
		Contributor: "codex",
		Config: adapter.SessionConfig{
			WorkspaceRoot: filepath.Join("/tmp", "ac009-ws"),
			Model:         "gpt-5.6-sol",
		},
	}
}

// Every Codex configuration piece is required: removing any one — or
// drifting the frozen evidence — fails construction before the service
// starts.
func TestServiceWiring_CodexConfigurationFailClosed(t *testing.T) {
	dir := t.TempDir()
	binDir := compileCodexStubForService(t)
	base, d, profile, _ := codexWiringConfig(t, dir, binDir)
	universeRel := profile.Harnesses["codex"].Codex.EventUniversePath
	store := openCodexWiringStore(t, base.StateDir)
	lock := mustLock(t, base.StateDir)

	mutations := []struct {
		name    string
		mutate  func(*ServerConfig)
		wantErr string
	}{
		{"missing scratch root", func(c *ServerConfig) { c.CodexScratchRoot = "" }, "CodexScratchRoot is required"},
		{"missing evidence root", func(c *ServerConfig) { c.CodexEvidenceRoot = "" }, "CodexEvidenceRoot is required"},
		{"evidence root absent on disk", func(c *ServerConfig) { c.CodexEvidenceRoot = filepath.Join(dir, "absent-evidence") }, "is missing"},
		{"missing profile", func(c *ServerConfig) { c.CodexProfile = storage.CanonicalProfile{} }, "requires the frozen run profile"},
		{"v2 profile", func(c *ServerConfig) {
			p, _ := codexServiceProfile(t, d.scratch, d.evidenceRoot, d.wsBase)
			p.AlgoVersion = "cprof-v2"
			c.CodexProfile = p
		}, "requires cprof-v3"},
		{"missing codex block", func(c *ServerConfig) {
			p, _ := codexServiceProfile(t, d.scratch, d.evidenceRoot, d.wsBase)
			spec := p.Harnesses["codex"]
			spec.Codex = nil
			p.Harnesses["codex"] = spec
			c.CodexProfile = p
		}, "lacks the required harnesses.codex block"},
		{"universe evidence drift", func(c *ServerConfig) {
			full := filepath.Join(d.evidenceRoot, filepath.FromSlash(universeRel))
			if err := os.WriteFile(full, []byte(`{"tampered":true}`), 0o600); err != nil {
				t.Fatalf("tamper universe: %v", err)
			}
		}, "digest mismatch"},
		{"scratch inside state dir", func(c *ServerConfig) {
			c.CodexScratchRoot = filepath.Join(base.StateDir, "scratch")
		}, "must be outside"},
		{"scratch inside workspace base", func(c *ServerConfig) {
			c.CodexScratchRoot = filepath.Join(base.WorkspaceBaseDir, "scratch")
		}, "must be outside"},
		{"codex and claude both configured", func(c *ServerConfig) {
			c.ClaudeBinaryPath = "claude"
		}, "only one persistent contributor adapter"},
		{"codex and opencode both configured", func(c *ServerConfig) {
			c.OpenCodeBinaryPath = "opencode"
		}, "only one persistent contributor adapter"},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			_, err := NewServerWithAdapter(store, lock, cfg, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected failure containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// A complete operator configuration wires the production Codex adapter;
// a server without codex configuration refuses the codex birth path.
func TestServiceWiring_CodexConstructionSucceeds(t *testing.T) {
	dir := t.TempDir()
	binDir := compileCodexStubForService(t)
	cfg, _, _, _ := codexWiringConfig(t, dir, binDir)
	store := openCodexWiringStore(t, cfg.StateDir)

	srv, err := NewServerWithAdapter(store, mustLock(t, cfg.StateDir), cfg, nil)
	if err != nil {
		t.Fatalf("construction with complete configuration: %v", err)
	}
	if srv == nil || srv.PolicyExecutor() == nil {
		t.Fatal("wired server must be usable")
	}

	// No codex configuration means no codex adapter: the birth path
	// refuses instead of silently using another (or no) adapter.
	plainDir := filepath.Join(dir, "plain")
	plainState := filepath.Join(plainDir, "state")
	if err := os.MkdirAll(plainState, 0o700); err != nil {
		t.Fatalf("mkdir plain state: %v", err)
	}
	plainStore := openCodexWiringStore(t, plainState)
	plain, err := NewServer(plainStore, mustLock(t, plainState), ServerConfig{
		StateDir: plainState, InstanceID: "inst-plain", AuthToken: "tok-plain",
	})
	if err != nil {
		t.Fatalf("plain server: %v", err)
	}
	if _, _, err := plain.CreateCodexSession(context.Background(), "op-x", codexWiringLease, codexCreateRequest()); err == nil ||
		!strings.Contains(err.Error(), "no contributor adapter is wired") {
		t.Fatalf("birth path without codex wiring must refuse, got %v", err)
	}
}

// §3.3 ordering step 1, end-to-end through PRODUCTION construction:
// pre-attestation, CreateCodexSession fails with the typed
// ErrProductionEligibilityMissing and NO child process starts.
func TestServiceCodexSession_EligibilityFailsClosedPreChild(t *testing.T) {
	dir := t.TempDir()
	binDir := compileCodexStubForService(t)
	cfg, d, profile, _ := codexWiringConfig(t, dir, binDir)
	store := openCodexWiringStore(t, cfg.StateDir)
	seedCodexRunAndSession(t, store, profile)

	srv, err := NewServerWithAdapter(store, mustLock(t, cfg.StateDir), cfg, nil)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}

	binding, _, err := srv.CreateCodexSession(context.Background(), "op-create-pre", codexWiringLease, codexCreateRequest())
	if binding.NativeSessionID != "" {
		t.Fatalf("no binding may publish pre-attestation, got %+v", binding)
	}
	var missing *codex.ErrProductionEligibilityMissing
	if !errors.As(err, &missing) {
		t.Fatalf("pre-attestation creation must fail typed ErrProductionEligibilityMissing, got %T: %v", err, err)
	}
	if b, _ := store.GetCodexSessionBinding(context.Background(), codexWiringSessionID); b != nil {
		t.Fatal("no binding may persist after the eligibility refusal")
	}
	// No process was started: the stub request log never appeared.
	if _, err := os.Stat(filepath.Join(d.scratch, ".codex-stub-requests")); !os.IsNotExist(err) {
		t.Fatal("the eligibility gate must fire before any child starts")
	}
}

// A durable attestation row that disagrees on ANY tuple member — here
// the manifest digest — never satisfies the production eligibility
// lookup: the creation still fails closed pre-child (spec §3.7).
func TestServiceCodexSession_ManifestDriftRowNeverSatisfiesLookup(t *testing.T) {
	dir := t.TempDir()
	binDir := compileCodexStubForService(t)
	cfg, d, profile, _ := codexWiringConfig(t, dir, binDir)
	store := openCodexWiringStore(t, cfg.StateDir)
	seedCodexRunAndSession(t, store, profile)
	policy, err := codex.ValidateCodexHarness(profile, cfg.CodexEvidenceRoot)
	if err != nil {
		t.Fatalf("validate harness: %v", err)
	}

	// A row matching everything EXCEPT the manifest digest.
	driftID := "cprot-v2:sha256:" + strings.Repeat("cd", 32)
	_, err = store.DB().ExecContext(context.Background(), `
INSERT INTO codex_protection_attestations
	(attestation_id, codex_version, platform, manifest_digest, profile_digest,
	 probe_results, probed_at, actor)
VALUES (?, ?, ?, ?, ?, '[]', ?, 'operator-test')`,
		driftID, policy.AppServerVersion,
		policy.PlatformOS+"/"+policy.PlatformFamily,
		"sha256:"+strings.Repeat("ef", 32), // divergent manifest digest
		codexProfileDigest(t, profile), time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert drift row: %v", err)
	}

	srv, err := NewServerWithAdapter(store, mustLock(t, cfg.StateDir), cfg, nil)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	_, _, err = srv.CreateCodexSession(context.Background(), "op-create-drift", codexWiringLease, codexCreateRequest())
	var missing *codex.ErrProductionEligibilityMissing
	if !errors.As(err, &missing) {
		t.Fatalf("manifest drift must NOT satisfy the lookup; want ErrProductionEligibilityMissing, got %T: %v", err, err)
	}
	if _, err := os.Stat(filepath.Join(d.scratch, ".codex-stub-requests")); !os.IsNotExist(err) {
		t.Fatal("no child may start on a drifted tuple")
	}
}

// ── cprot-v2 attestation journal operation ──────────────────────────────

// newCodexAttestationServer creates the run provenance the attestation
// journal entry requires and returns a plain server wired over the same
// store, plus the frozen harness profile pieces.
func newCodexAttestationServer(t *testing.T) (*Server, *storage.Store, storage.ToolkitManifest) {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := openCodexWiringStore(t, stateDir)
	if _, err := store.CreateRunWithProfile(context.Background(), storage.CreateRunWithProfileRequest{
		OpID: "op-run-attest-cx", ControllerLease: "lease-attest-cx", RunID: "run-attest-cx",
		Brief: "attestation", SourceRepoIdentity: "example/repo",
		SourceCommit: "0123456789012345678901234567890123456789",
		SourceTree:   "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		Profile: storage.CanonicalProfile{
			AlgoVersion: "cprof-v1", WorkspaceMode: "none",
			IsolationStrictness: "permissive_dev", NetworkMode: "unrestricted",
			Tooling: []string{"codex"},
		},
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	srv, err := NewServer(store, mustLock(t, stateDir), ServerConfig{
		StateDir: stateDir, InstanceID: "inst-attest-cx", AuthToken: "tok-attest-cx",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, store, storage.ToolkitManifest{
		ProbedCLIVersion:       "2.1.278",
		UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
		UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
		ApprovedTools:          []string{"Read", "Glob"},
		DeniedComplement:       []string{"Bash"},
		ExpectedHooks:          []string{"SessionStart:startup"},
		TurnsBound:             8,
	}
}

// validCodexServiceAttestation builds a valid typed cprot-v2 attestation
// bound to the canonical manifest digest (the single derivation source).
func validCodexServiceAttestation(t *testing.T, manifest storage.ToolkitManifest, profileDigest, actor string) codex.ProtectionAttestation {
	t.Helper()
	manifestDigest, err := storage.ComputeToolkitManifestDigest(manifest)
	if err != nil {
		t.Fatalf("manifest digest: %v", err)
	}
	att := codex.ProtectionAttestation{
		CodexVersion:   "0.154.0",
		PlatformOS:     runtime.GOOS,
		PlatformFamily: "unix",
		ManifestDigest: manifestDigest,
		ProfileDigest:  profileDigest,
		ProbeRecords: []codex.ProbeRecord{
			{Class: codex.RecordSiblingRead, ToolClass: codex.ToolRead, ToolName: "view",
				Operation: codex.OpRead, Denied: true,
				EnforcingCapability: codex.CapSandboxRestrictedFS, DenialText: "denied: path outside the workspace"},
			{Class: codex.RecordSelfMutation, ToolClass: codex.ToolBashAbsolute, ToolName: "shell",
				Operation: codex.OpWrite, Denied: true,
				EnforcingCapability: codex.CapGuardrailHook, DenialText: "write to own rollout denied"},
		},
		ApprovalDenies: []codex.ApprovalDenyRecord{
			{MethodName: "execCommandApproval", RefusalKind: codex.RefusalNativeEnum},
		},
		ProbedAt: "2026-09-23T00:00:00Z",
		Actor:    actor,
	}
	if err := att.Validate(); err != nil {
		t.Fatalf("fixture attestation must be valid: %v", err)
	}
	return att
}

const codexAttestationRunID = "run-attest-cx"

// The attestation journal operation requires the operator credential and
// the matching actor, re-derives the manifest digest from the single
// canonical source, and persists the row with its journal entry; replay
// is idempotent and conflicting reuse is rejected.
func TestServiceAttestation_CodexOperatorAuthorityAndIdempotency(t *testing.T) {
	srv, store, manifest := newCodexAttestationServer(t)
	ctx := context.Background()
	att := validCodexServiceAttestation(t, manifest, "cprof-v3:sha256:"+strings.Repeat("7", 62), "operator-test")

	req := func(mut func(*CodexProbeAttestationRequest)) CodexProbeAttestationRequest {
		r := CodexProbeAttestationRequest{
			OpID:            "op-catt-1",
			Actor:           "operator-test",
			RunID:           codexAttestationRunID,
			ToolkitManifest: manifest,
			Attestation:     att,
		}
		if mut != nil {
			mut(&r)
		}
		return r
	}

	// Missing operator credential: refused before any write.
	if _, err := srv.RecordCodexProbeAttestation(ctx, req(func(r *CodexProbeAttestationRequest) { r.OperatorToken = "" })); err == nil ||
		!strings.Contains(err.Error(), "operator credential") {
		t.Fatalf("missing operator credential must be refused, got %v", err)
	}
	// Wrong credential: refused.
	if _, err := srv.RecordCodexProbeAttestation(ctx, req(func(r *CodexProbeAttestationRequest) { r.OperatorToken = "wrong" })); err == nil ||
		!strings.Contains(err.Error(), "operator credential is invalid") {
		t.Fatalf("invalid operator credential must be refused, got %v", err)
	}
	// Credential without the actor attribution: refused.
	if _, err := srv.RecordCodexProbeAttestation(ctx, req(func(r *CodexProbeAttestationRequest) {
		r.OperatorToken = "tok-attest-cx"
		r.Actor = ""
	})); err == nil || !strings.Contains(err.Error(), "operator actor") {
		t.Fatalf("missing actor must be refused, got %v", err)
	}
	// Actor mismatch: refused.
	if _, err := srv.RecordCodexProbeAttestation(ctx, req(func(r *CodexProbeAttestationRequest) {
		r.OperatorToken = "tok-attest-cx"
		r.Actor = "someone-else"
	})); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("actor mismatch must be refused, got %v", err)
	}
	// A manifest digest that disagrees with the canonical derivation is
	// refused: such evidence could never satisfy the launch tuple.
	if _, err := srv.RecordCodexProbeAttestation(ctx, req(func(r *CodexProbeAttestationRequest) {
		r.OperatorToken = "tok-attest-cx"
		bad := r.Attestation
		bad.ManifestDigest = "sha256:" + strings.Repeat("0", 64)
		r.Attestation = bad
	})); err == nil || !strings.Contains(err.Error(), "does not match the canonical toolkit manifest digest") {
		t.Fatalf("manifest digest disagreement must be refused, got %v", err)
	}
	// denied=false evidence is invalid cprot-v2 and refused.
	if _, err := srv.RecordCodexProbeAttestation(ctx, req(func(r *CodexProbeAttestationRequest) {
		r.OperatorToken = "tok-attest-cx"
		invalid := r.Attestation
		invalid.ProbeRecords = append([]codex.ProbeRecord(nil), invalid.ProbeRecords...)
		invalid.ProbeRecords[0].Denied = false
		r.Attestation = invalid
	})); err == nil {
		t.Fatal("denied=false attestation must be refused")
	}

	// Valid recording: receipt, durable row, journal entry.
	receipt, err := srv.RecordCodexProbeAttestation(ctx, req(func(r *CodexProbeAttestationRequest) { r.OperatorToken = "tok-attest-cx" }))
	if err != nil {
		t.Fatalf("valid attestation: %v", err)
	}
	if receipt.CommandType != "record_codex_attestation" || receipt.Payload == "" {
		t.Fatalf("unexpected receipt %+v", receipt)
	}
	attDigest, err := att.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	manifestDigest, _ := storage.ComputeToolkitManifestDigest(manifest)
	var platform, storedManifest string
	if err := store.DB().QueryRow(
		`SELECT platform, manifest_digest FROM codex_protection_attestations WHERE attestation_id = ?`, attDigest,
	).Scan(&platform, &storedManifest); err != nil {
		t.Fatalf("durable attestation row: %v", err)
	}
	if platform != att.PlatformIdentity() {
		t.Fatalf("platform column %q must be the canonical identity %q", platform, att.PlatformIdentity())
	}
	if storedManifest != manifestDigest {
		t.Fatalf("manifest_digest column %q must come from ComputeToolkitManifestDigest %q", storedManifest, manifestDigest)
	}
	var journalCount int
	if err := store.DB().QueryRow(
		`SELECT count(*) FROM journal_entries WHERE op_id = 'op-catt-1' AND command_type = 'record_codex_attestation'`,
	).Scan(&journalCount); err != nil || journalCount != 1 {
		t.Fatalf("journal entry must exist exactly once, got %d err=%v", journalCount, err)
	}

	// Idempotent replay: same op_id and evidence returns the receipt.
	replay, err := srv.RecordCodexProbeAttestation(ctx, req(func(r *CodexProbeAttestationRequest) { r.OperatorToken = "tok-attest-cx" }))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Payload != receipt.Payload {
		t.Fatalf("replay must return the committed receipt, got %+v vs %+v", replay, receipt)
	}

	// Same op_id with DIFFERENT evidence: idempotency conflict.
	if _, err := srv.RecordCodexProbeAttestation(ctx, req(func(r *CodexProbeAttestationRequest) {
		r.OperatorToken = "tok-attest-cx"
		changed := r.Attestation
		changed.ProbedAt = "2026-09-23T01:00:00Z"
		r.Attestation = changed
	})); err == nil {
		t.Fatal("op_id reuse with different evidence must conflict")
	}

	// Same evidence under a new op_id: the row already exists — rejected.
	if _, err := srv.RecordCodexProbeAttestation(ctx, req(func(r *CodexProbeAttestationRequest) {
		r.OperatorToken = "tok-attest-cx"
		r.OpID = "op-catt-2"
	})); err == nil || !strings.Contains(err.Error(), "already recorded") {
		t.Fatalf("duplicate attestation identity must be rejected, got %v", err)
	}
}

// Attempt freezing (§3.7): the recorded attestation authorizes the
// protected absence-redispatch transition for an attempt that froze its
// identity at launch; an attempt frozen with an id that is not durably
// recorded can never upgrade — a later attestation never reclassifies.
func TestServiceAttestation_CodexAttemptFreezing(t *testing.T) {
	srv, store, manifest := newCodexAttestationServer(t)
	ctx := context.Background()
	profileDigest := "cprof-v3:sha256:" + strings.Repeat("7", 62)
	att := validCodexServiceAttestation(t, manifest, profileDigest, "operator-test")
	if _, err := srv.RecordCodexProbeAttestation(ctx, CodexProbeAttestationRequest{
		OpID: "op-catt-freeze", OperatorToken: "tok-attest-cx", Actor: "operator-test",
		RunID: codexAttestationRunID, ToolkitManifest: manifest, Attestation: att,
	}); err != nil {
		t.Fatalf("record attestation: %v", err)
	}
	digest, err := att.Digest()
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	if err := store.InsertCodexSessionBinding(ctx, storage.CodexSessionBinding{
		SessionID: "sess-freeze-cx", NativeID: "01934f7a-1b2c-7def-9abc-def012345678",
		Model: "gpt-5.6-sol", Workspace: "/tmp/ws", ProfileDigest: profileDigest,
	}); err != nil {
		t.Fatalf("insert binding: %v", err)
	}
	if err := store.InsertCodexTurnAttempt(ctx, storage.CodexTurnAttempt{
		AttemptID: "att-freeze-cx", SessionID: "sess-freeze-cx", TurnKey: "t-freeze",
		PromptDigest: "pd", BaselineIdentity: "0000:00",
		RolloutProtection: "protected", AttestationID: &digest,
	}); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}

	seq, err := store.ReserveCodexLaunch(ctx, "att-freeze-cx", "fixture", 0)
	if err != nil {
		t.Fatalf("initial launch: %v", err)
	}
	if err := store.RecordCodexLaunchState(ctx, "att-freeze-cx", seq, "started", nil); err != nil {
		t.Fatalf("started: %v", err)
	}
	exit := 1
	if err := store.RecordCodexLaunchState(ctx, "att-freeze-cx", seq, "dead", &exit); err != nil {
		t.Fatalf("dead: %v", err)
	}
	if err := store.RecordCodexAbsenceVerified(ctx, "att-freeze-cx"); err != nil {
		t.Fatalf("absence verification with the frozen attestation: %v", err)
	}
	if _, err := store.ReserveCodexLaunch(ctx, "att-freeze-cx", "fixture", 0); err != nil {
		t.Fatalf("redispatch with the frozen attestation must be authorized: %v", err)
	}

	// A later attestation (different id, same tuple) never upgrades an
	// attempt frozen with an unknown id.
	unknown := "cprot-v2:sha256:" + strings.Repeat("ff", 32)
	if err := store.InsertCodexTurnAttempt(ctx, storage.CodexTurnAttempt{
		AttemptID: "att-frozen-unknown-cx", SessionID: "sess-freeze-cx", TurnKey: "t-unknown",
		PromptDigest: "pd", BaselineIdentity: "0000:00",
		RolloutProtection: "protected", AttestationID: &unknown,
	}); err != nil {
		t.Fatalf("insert unknown-attestation attempt: %v", err)
	}
	if err := store.RecordCodexAbsenceVerified(ctx, "att-frozen-unknown-cx"); err == nil {
		t.Fatal("an attempt frozen with an unknown attestation id must never verify absence")
	}
}

// ── Durable §3.4 creation uncertainty across a restart ──────────────────

// A creation that ends UNCERTAIN through the production path (the stub's
// thread/start poisons the id) is recorded durably; a NEW server
// instance over the same store — a restart — refuses to re-create the
// thread with no child started, until the uncertainty is explicitly
// resolved.
func TestServiceCodexSession_CreationUncertainBlocksAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	binDir := compileCodexStubForService(t)
	cfg, d, profile, _ := codexWiringConfig(t, dir, binDir)
	store := openCodexWiringStore(t, cfg.StateDir)
	seedCodexRunAndSession(t, store, profile)
	policy, err := codex.ValidateCodexHarness(profile, cfg.CodexEvidenceRoot)
	if err != nil {
		t.Fatalf("validate harness: %v", err)
	}

	lock1, err := AcquireServiceLock(cfg.StateDir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	srv1, err := NewServerWithAdapter(store, lock1, cfg, nil)
	if err != nil {
		t.Fatalf("construction: %v", err)
	}
	// A valid matching attestation authorizes the child start; the stub's
	// poisoned thread/start then drives the creation UNCERTAIN.
	manifest := storage.ToolkitManifest{
		ProbedCLIVersion:       "2.1.278",
		UniverseEvidencePath:   "docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json",
		UniverseEvidenceDigest: "sha256:" + strings.Repeat("a", 64),
		ApprovedTools:          []string{"Read", "Glob"},
		DeniedComplement:       []string{"Bash"},
		ExpectedHooks:          []string{"SessionStart:startup"},
		TurnsBound:             8,
	}
	manifestDigest, err := storage.ComputeToolkitManifestDigest(manifest)
	if err != nil {
		t.Fatalf("manifest digest: %v", err)
	}
	att := codex.ProtectionAttestation{
		CodexVersion:   policy.AppServerVersion,
		PlatformOS:     policy.PlatformOS,
		PlatformFamily: policy.PlatformFamily,
		ManifestDigest: manifestDigest,
		ProfileDigest:  codexProfileDigest(t, profile),
		ProbeRecords: []codex.ProbeRecord{{
			Class: codex.RecordSiblingRead, ToolClass: codex.ToolRead, ToolName: "view",
			Operation: codex.OpRead, Denied: true,
			EnforcingCapability: codex.CapSandboxRestrictedFS, DenialText: "denied",
		}},
		ApprovalDenies: []codex.ApprovalDenyRecord{
			{MethodName: "execCommandApproval", RefusalKind: codex.RefusalNativeEnum},
		},
		ProbedAt: time.Now().UTC().Format(time.RFC3339),
		Actor:    "operator-test",
	}
	if _, err := srv1.RecordCodexProbeAttestation(context.Background(), CodexProbeAttestationRequest{
		OpID: "op-catt-restart", OperatorToken: cfg.AuthToken, Actor: "operator-test",
		RunID: codexWiringRunID, ToolkitManifest: manifest, Attestation: att,
	}); err != nil {
		t.Fatalf("record attestation: %v", err)
	}

	req := codexCreateRequest()
	req.Config.WorkspaceRoot = d.wsBase
	if _, _, err := srv1.CreateCodexSession(context.Background(), "op-create-1", codexWiringLease, req); err == nil {
		t.Fatal("the poisoned creation must fail")
	} else {
		var unc *adapter.ErrSessionCreationUncertain
		if !errors.As(err, &unc) {
			t.Fatalf("creation must be typed UNCERTAIN, got %T: %v", err, err)
		}
	}
	if blocked, err := store.HasCodexCreationUncertainty(context.Background(), codexWiringSessionID); err != nil || !blocked {
		t.Fatalf("the uncertainty must be recorded durably, blocked=%v err=%v", blocked, err)
	}
	stubRequests := func() string {
		raw, err := os.ReadFile(filepath.Join(d.scratch, ".codex-stub-requests"))
		if err != nil {
			return ""
		}
		return string(raw)
	}
	requestsBefore := stubRequests()
	if requestsBefore == "" {
		t.Fatal("the first creation must have started a child")
	}

	// Simulated restart: same store, fresh server instance and a freshly
	// acquired lock (the first lock is released — the old process is
	// gone).
	if err := lock1.Release(); err != nil {
		t.Fatalf("release first lock: %v", err)
	}
	srv2, err := NewServerWithAdapter(store, mustLock(t, cfg.StateDir), cfg, nil)
	if err != nil {
		t.Fatalf("restart construction: %v", err)
	}
	if _, _, err := srv2.CreateCodexSession(context.Background(), "op-create-2", codexWiringLease, req); err == nil {
		t.Fatal("a restart must not re-create the thread for an uncertain session")
	} else {
		var unc *adapter.ErrSessionCreationUncertain
		if !errors.As(err, &unc) || !strings.Contains(err.Error(), "durable creation uncertainty") {
			t.Fatalf("the restart refusal must be the durable §3.4 block, got %T: %v", err, err)
		}
	}
	if stubRequests() != requestsBefore {
		t.Fatal("the refused re-creation must not start any child")
	}

	// Explicit resolution clears the durable block.
	if _, err := srv2.ResolveCodexSessionCreationUncertainty(context.Background(),
		"op-resolve-1", codexWiringSessionID, "controller-x"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if blocked, err := store.HasCodexCreationUncertainty(context.Background(), codexWiringSessionID); err != nil || blocked {
		t.Fatalf("the durable block must be cleared by the explicit resolution, blocked=%v err=%v", blocked, err)
	}
}
