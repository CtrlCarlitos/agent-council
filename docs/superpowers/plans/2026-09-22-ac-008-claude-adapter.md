# AC-008 Claude Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native inline TDD with 1 implementation owner and 2 internal verification gates). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the AC-006 adapter contract for the installed Claude Code CLI (verified 2.1.278) as a process-per-turn adapter: stdin-only prompt transport, per-session config roots under a disjoint base, protected/advisory transcript evidence, frozen-tooling enforcement via the computed deny complement, adapter-owned per-turn stream parsing with bounded taps, durable attempt/launch state with reserved-before-Start launches and a single verified-absence redispatch, and evidence-based reconciliation that never fabricates terminal outcomes.

**Architecture:** `internal/adapter/claude` provides `ClaudeAdapter` (AC-006 contract), `ClaudeTurnLaunchSource` (service-owned storage-backed launch builder), `ClaudeProbeLaunchTemplate` (operator-owned probe), a typed NDJSON stream parser with the §3.9 state table, per-session config-root materialization, and fixture `claude` executables replaying recorded stream-json for CI. `cprof-v2` profile encoding with the additive `toolkit_manifest` is owned by `storage`/`execpolicy` (shared with AC-007 compat rules).

**Tech Stack:** Go 1.25.0, AC-005 `PolicyExecutor`/`workspace`/`execpolicy`, AC-006 `adapter` contract, AC-004 storage transitions, `crypto/rand` (UUIDv4), `crypto/sha256` (digests). No new deps.

**Spec:** [`../specs/2026-09-22-ac-008-claude-adapter-design.md`](../specs/2026-09-22-ac-008-claude-adapter-design.md) (v8, approved for planning at `1630889`)

## Global Constraints

- No provider calls in CI; the fixture `claude` executable replays recorded stream-json. Native execution is deferred to the manual, sanitized, operator-invoked evidence script.
- Every process launch goes through `PolicyExecutor.Start` — never `os/exec` directly. Argument contract is exact (§3.7); forbidden/reordered/duplicated/extra arguments are rejected before launch. The prompt is NEVER an argv element.
- Forbidden flags (never in any launch): `--bare`, `-c/--continue`, `--fork-session`, `--no-session-persistence`, `--dangerously-skip-permissions`, `--allow-dangerously-skip-permissions`, any `--permission-mode` value, `--append-system-prompt*`, `--system-prompt*`.
- `CLAUDE_CONFIG_DIR` only via the typed `LaunchRequest.ClaudeConfigDir` extension, validated ONLY for the exact Claude launch shape, value inside `claude_config_base_dir`, excluded from event payloads.
- Ambiguity boundary: the first successfully transmitted stdin byte ⇒ transport failures after it are DispatchUnknown; before it, DispatchRejected.
- Transcript JSONL is ADVISORY by default: acceptance and redispatch decisions require a valid protection attestation; advisory mode leaves `accepted` unknown and forbids same-attempt redispatch permanently. JSONL never proves terminal completion.
- Windows: transcript-integrity checks are fail-closed (`integrity=unverified` → decisions degrade to Uncertain). Portable decision logic stays covered by fixtures.
- Tool policy: deny complement = pinned native tool universe − frozen approved set, passed through `--disallowedTools` (verified mechanism). `--allowedTools` exclusivity is never relied upon. Universe from the committed evidence file (native tools only) or an operator-run live inventory probe; `mcp__*`/plugin tools REQUIRE the live inventory path; init drift ⇒ terminate + Uncertain.
- Only tests requiring POSIX processes, signals, permissions, or shell fixtures are //go:build unix; parser, decision, digest, profile-encoding, schema, and fixture-replay tests run on every platform. windows/darwin vet+compile must stay green.
- Crash-boundary rule (plan-wide): every durable transition listed in spec §3.11 gets a crash-gap test — simulate death between step N and N+1 and assert the recovered state machine classifies correctly (reserved-not-started ⇒ possibly-running ⇒ Uncertain; consumption not re-derivable past its cap).

## Verification Gates
- **Gate 1 (after Task 4):** cprof-v2/template digest + evidence-path containment (Task 1); config base + materialization + typed config-dir extension (Task 2); launch source + exact shape validation + stdin boundary (Task 3); stream parser state table + fixture executable (Task 4). Full suites + race ×3.
- **Gate 2 (after Tasks 6–7):** adapter contract + durable attempt/launch state + crash boundaries (Task 5), service wiring + probe (Task 6), acceptance story + evidence script + matrix (Task 7). Full suites + race ×3 + cross-platform CI.

## Tasks

### Task 1: cprof-v2 profile encoding, toolkit manifest, template digest, evidence containment

**Files:**
- Modify: `internal/storage` (canonical profile v2: `toolkit_manifest` fields, `cprof-v2` algorithm id + digest prefix, normalization)
- Create: `internal/adapter/claude/template_digest.go`
- Create: `internal/adapter/claude/evidence_universe.go`
- Test: `internal/storage/canonical_profile_v2_test.go`
- Test: `internal/adapter/claude/template_digest_test.go`
- Test: `internal/adapter/claude/evidence_universe_test.go`
- Evidence: `docs/superpowers/evidence/ac008-native-tool-universe-2.1.278.json` (already committed)

**Interfaces:**
- `storage.ParseCanonicalProfileV2(canonicalJSON) (CanonicalProfile, error)` — accepts v1 fields + requires `toolkit_manifest` completeness for Claude eligibility; normalization per spec §3.11 (trim/reject-empty/reject-duplicate/byte-wise sort).
- `claude.TemplateDigest(dir string) (string, error)` — `ctmpl-v1:sha256:<hex>` with the exact framing (uint32-BE count, length-prefixed UTF-8 paths + raw bytes, byte-wise path order, duplicate-normalized-path rejection, symlink/non-regular rejection).
- `claude.ResolveUniverseEvidence(profile, evidenceRoot) (Universe, error)` — resolves `universe_evidence_path` against the trusted service-owned evidence root with symlink-safe containment (no `..`, no escaping the root, no symlink components), re-hashes raw bytes against `sha256:<hex>`, parses the typed universe.

**Steps:**
- [ ] 1.1 Failing tests: cprof-v2 digest vectors (golden), normalization rejections (duplicate/empty/untrimmed entries), v1/v2 acceptance matrix (OpenCode accepts both; Claude requires v2 → typed `ErrUnsupportedProfile`), `ctmpl-v1` golden vectors (empty file, unicode paths, duplicate rejection, symlink rejection), evidence containment (escape attempts: `../`, symlinked root, symlinked file, wrong digest).
- [ ] 1.2 Implement.
- [ ] 1.3 Suite green (cross-platform); commit `feat(storage,adapter/claude): cprof-v2 toolkit manifest, template digest, evidence containment`.

### Task 2: Config base dir validation + per-session config-root materialization + typed config-dir extension

**Files:**
- Modify: `internal/service/server.go` (`ServerConfig.ClaudeConfigBaseDir`, disjoint validation wiring)
- Create: `internal/service/claude_config_base.go`
- Create: `internal/adapter/claude/configroot.go` (materialization)
- Modify: `internal/adapter/execpolicy` (`LaunchRequest.ClaudeConfigDir` typed extension + exact-claude-shape validation + redaction)
- Test: `internal/service/claude_config_base_test.go` (portable)
- Test: `internal/adapter/claude/configroot_test.go` (portable; POSIX perms assertions runtime-gated)

**Interfaces:**
- `service.resolveClaudeConfigBaseDir(cfg)` — required with `OpenCodeBinaryPath`-equivalent Claude config; disjoint (resolved, symlink-safe, pre+post creation) from `state_dir` and `workspace_base_dir`; 0700 creation/tightening; Windows fail-closed degradation mirrors the probe-scratch family.
- `claude.MaterializeConfigRoot(templateDir, base, runID, sessionID) (root string, digest string, err error)` — atomic tmp+rename, validated source (regular files, no symlinks, size bounds), 0700/0600 (Windows: ACL-equivalent or fail-closed), failure cleanup.
- `execpolicy.LaunchRequest.ClaudeConfigDir string` — accepted ONLY when the executor validates the exact Claude launch shape (§3.7); rejected on any other launch; never merged into the inherited-env allowlist.

**Steps:**
- [ ] 2.1 Failing tests: base-dir disjoint rejections (inside state, inside workspace, symlink-escape both directions), secure creation, materialization golden tree + atomic-failure cleanup, executor accepts ClaudeConfigDir only on exact Claude launches and rejects collisions/other shapes.
- [ ] 2.2 Implement.
- [ ] 2.3 Suite green; commit `feat(service,adapter/claude): per-session config roots and typed config-dir extension`.

### Task 3: ClaudeTurnLaunchSource, exact launch shape, stdin transport boundary

**Files:**
- Create: `internal/adapter/claude/launchsource.go`
- Modify: `internal/adapter/execpolicy` (exact-shape validation for the Claude template, forbidden-flag rejection)
- Create: `internal/adapter/claude/stdin.go` (typed stdin writer with first-byte transmission tracking)
- Test: `internal/adapter/claude/launchsource_test.go`
- Test: `internal/adapter/execpolicy/claude_shape_test.go` (portable)

**Interfaces:**
- `ClaudeTurnLaunchSource.OpenCodeServeLaunch`-equivalent: `ClaudeTurnLaunch(ctx, sessionID, nativeID, firstTurn bool, promptDigest string) (execpolicy.LaunchRequest, error)` — builds the exact §3.7 argument contract from the frozen profile (model, max-turns, allow/deny lists) + workspace + config dir.
- execpolicy validates: only the parameterized slots vary; forbidden/reordered/duplicated/extra args rejected; `ClaudeConfigDir` rules enforced.
- `claude.StdinWriter` — wraps the process stdin pipe; first successful write flips `TransmissionBegan` (atomically); classification helper maps writer errors to pre/post-transmission.

**Steps:**
- [ ] 3.1 Failing tests: shape acceptance for the exact contract (both `--session-id` and `--resume` variants, with/without tool lists); rejection matrix (forbidden flags, reordered, duplicated, extra, missing verbose); StdinWriter first-byte boundary (byte before failure ⇒ post-transmission; zero bytes ⇒ pre-transmission).
- [ ] 3.2 Implement.
- [ ] 3.3 Suite green; commit `feat(adapter/claude): turn launch source, exact shape validation, stdin transmission boundary`.

### Task 4: NDJSON stream parser, §3.9 state table, fixture `claude` executable

**Files:**
- Create: `internal/adapter/claude/stream.go` (parser + state machine + init-vs-manifest check)
- Create: `internal/adapter/claude/claudefake_test.go` (fixture executable + recorded stream-json fixtures)
- Test: `internal/adapter/claude/stream_test.go`
- Fixtures: `internal/adapter/claude/testdata/*.jsonl` (init/assistant/denials/result/malformed/oversized/truncated/multi-result/init-mismatch recordings sanitized from the research probes)

**Interfaces:**
- `claude.StreamParser` — consumes the stdout reader; emits typed events (Progress, ToolRequested, ToolDenied(GuardrailDenied|ToolDisabled|ApprovalDenied), Terminal(result)); enforces §3.9: line/total size bounds, torn-tail tolerance at EOF, multi-result poison, init-vs-manifest mismatch ⇒ terminate+Uncertain with reason, unknown init tool ⇒ terminate+Uncertain.
- Fixture executable mirrors the AC-007 stub pattern: replays a fixture file to stdout with real NDJSON timing; honors `--version`/`--help` contract probes.

**Steps:**
- [ ] 4.1 Failing tests: one per §3.9 table row + four-class denial mapping + init-mismatch termination + golden fixture replays (sanitized research recordings).
- [ ] 4.2 Implement parser + fixtures.
- [ ] 4.3 Suite green (cross-platform); commit `feat(adapter/claude): stream parser, failure state table, fixture executable`.

### Task 5: AC-006 adapter contract + durable attempt/launch state + crash boundaries

**Files:**
- Create: `internal/adapter/claude/adapter.go`
- Create: `internal/adapter/claude/eventpump.go` (per-turn stream → bounded taps; detach semantics)
- Modify: `internal/storage` (migration: `claude_session_bindings`, `claude_turn_attempts`, `claude_attempt_launches`, `claude_protection_attestations` + transitions with idempotency keys)
- Create: `internal/adapter/claude/transcript.go` (advisory/protected JSONL inspection; path derivation; integrity checks)
- Test: `internal/adapter/claude/adapter_test.go`
- Test: `internal/adapter/claude/transcript_test.go`
- Test: `internal/storage/claude_state_test.go`

**Interfaces:**
- Full AC-006 contract per spec §3.3–§3.10: CreateSession (UUIDv4 `crypto/rand`, creation reservation, materialize config root, `materialized=false`), ResumeSession (local inspection; typed `ErrNativeSessionMissing` deferred to dispatch; unmaterialized never "lost"), Dispatch (identity seam, per-native-session single-flight, reserved-before-Start launches, stdin transport, verdict recording), Observe (taps, detach-safe), Collect (single verified `result` correlation), Cancel (terminate; uncertain without result), Reconcile (§3.10 — advisory default ⇒ uncertain-until-disposed; probe = separate attempt).
- Storage transitions: baseline/launch-reservation/start/stdin/dead/accepted/terminal/observed_status — each atomic with `transition_version`; redispatch consumption transition (attestation-valid + verified absence + count=1 + not consumed ⇒ count=2, one-time).

**Steps:**
- [ ] 5.1 Failing contract tests (cross-platform unless process-bound):
  - create/resume/idempotency/concurrency (AC-007 carry-over + unmaterialized-binding rule);
  - dispatch accepted/rejected/unknown via fixture faults (pre-write refusal; stdin transmission begun then failure ⇒ unknown);
  - per-native-session single-flight across distinct turns;
  - **crash boundaries (plan requirement):** (a) crash after launch-reservation transaction, before Start ⇒ recovered state is `reserved, started_at NULL` ⇒ possibly-running ⇒ Uncertain, no redispatch; (b) crash after Start, before `started_at` ⇒ same classification; (c) crash after stdin-byte, before acceptance ⇒ unknown stands; (d) crash between redispatch decision and consumption ⇒ flag unset, re-derived, never a third launch; (e) crash after consumption before second Start ⇒ reserved row blocks;
  - Collect/Observe/Cancel semantics incl. terminal-close and detach;
  - Reconcile: advisory default ⇒ uncertain-until-disposed; in-life result ⇒ terminal; probe attempt = separate identity/cost, never attributed;
  - transcript trust: path derivation golden, symlink/torn-tail/ownership rejections (POSIX-gated), Windows fail-closed behavior (fixture-simulated).
- [ ] 5.2 Implement adapter + pump + storage transitions.
- [ ] 5.3 Race + full suite; commit `feat(adapter/claude): AC-006 contract with durable attempt state and crash boundaries`.

### Task 6: Service wiring + probe template + attestation

**Files:**
- Modify: `internal/service/server.go` (`ServerConfig.ClaudeBinaryPath`, `ClaudeConfigBaseDir`, probe profile, idle grace equivalent — turn deadline instead)
- Modify: `internal/adapter/claude/wiring.go` (`NewProductionClaudeAdapter`, `NewOperatorProbeLaunchTemplate`)
- Create: `internal/service/claude_probe_attestation.go` (attestation journal operation)
- Test: `internal/service/review_ac008_wiring_test.go` (POSIX — real stub child)
- Test: `internal/adapter/claude/probe_wiring_test.go` (portable rejection + POSIX e2e)

**Steps:**
- [ ] 6.1 Failing tests: construction requires the Claude config (binary path, disjoint base dir, probe profile) or fails closed; probe runs `--version` + `--help` contract assertions against the stub; attestation journal operation requires operator authority and freezes the binding fields.
- [ ] 6.2 Implement wiring.
- [ ] 6.3 Commit `feat(service): wire Claude adapter construction and probe attestation`.

### Task 7: Acceptance story + integration script + Gate 2

**Files:**
- Create: `internal/service/acceptance_claude_test.go` (POSIX)
- Create: `scripts/ac008-integration-evidence.sh` (manual, sanitized, CI-excluded; includes the operator-authorized transcript-path denial probe suite §3.6)
- Modify: PR #8-linked body (evidence matrix)

**Steps:**
- [ ] 7.1 Failing acceptance test through the bridge: configured construction → adopt (secret once) → connect → queue → release → gated execution through the production adapter to the fixture `claude` child (real executor) → collect (result-correlated output) → disconnect → idle/turn-deadline parking equivalent → reconnect → follow-up via `--resume` exact native session → durable outcomes asserted; plus spec §6.1 scenarios 1–10 as focused tests.
- [ ] 7.2 Integration script: version+`--help` contract capture, minimal operator-chosen live step, sanitized fixtures, transcript-path denial probe suite (operator-authorized section), never selects a provider/model automatically.
- [ ] 7.3 Full verification: `gofmt -l cmd internal`; `go vet ./...`; `CGO_ENABLED=0 go test ./... -count=1`; race ×3 on claude/service/execpolicy; windows/darwin vet; CI green.
- [ ] 7.4 Gate 2: PR body evidence matrix (spec §6/§6.1 rows → committed tests), request review.

## Self-Review: Invariant → Interface → Assertion

| Invariant | Interface | Assertion |
|---|---|---|
| Launch through PolicyExecutor; exact Claude shape; forbidden flags rejected | `ClaudeTurnLaunchSource` + executor validation | `claude_shape_test.go` rejection matrix; lifecycle tests use real executor |
| stdin-only prompt; first-byte ambiguity boundary | `StdinWriter` + `dispatchNative` classification | boundary tests: byte-then-fail ⇒ unknown; zero-byte ⇒ rejected |
| Per-session config roots under disjoint base | `resolveClaudeConfigBaseDir` + materialization | disjoint/symlink rejections; atomic materialization cleanup |
| Transcript advisory-by-default; protected requires attestation | `transcript.go` + attestation table | advisory ⇒ accepted unknown; protected ⇒ accepted/absence authoritative; attestation freeze |
| Single verified-absence redispatch, crash-safe | launch reservation transitions | crash-boundary tests (a)–(e) |
| Terminal only from observed `result`; stdout-only | `StreamParser` | §3.9 table row tests incl. multi-result poison |
| Frozen tooling enforced (deny complement) | launch source computation + fixture | omitted-tool denial fixture; init drift ⇒ terminate |
| Toolkit evidence vs frozen manifest | init-vs-manifest check | missing element ⇒ terminate + Uncertain |
| Reconciliation never attributes probes | `Reconcile` + attempt records | probe-attempt separation test |
| Legacy cprof-v1 rejected for Claude; OpenCode unaffected | profile parsing + adapter guards | v1/v2 acceptance matrix |
| POSIX-only tests scoped | build tags | CI: parser/decision/digest/schema tests on all platforms |
