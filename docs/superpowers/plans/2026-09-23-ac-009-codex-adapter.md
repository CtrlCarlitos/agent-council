# AC-009 Codex Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native inline TDD with 1 implementation owner and 2 internal verification gates). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the AC-006 adapter contract for the installed Codex CLI (verified 0.154.0) over the App Server transport (stdio JSON-RPC, one child per contributor session): provider-free `thread/resume` verification before every prompt, frozen per-turn policy pins (cprof-v3), structured approval routing with exact per-variant deny payloads, creation-reservation routing, advisory/protected rollout evidence (cprot-v2 attestations), durable attempt/launch state with reserved-before-write launches, and a production-eligibility gate that fails closed until the operator-authorized evidence path exists.

**Architecture:** `internal/adapter/codex` provides `CodexAdapter` (AC-006 contract), `CodexServer` (child lifecycle + JSON-RPC pump + route tables), `CodexClient` (typed RPC methods), the approval responder (§3.6 table), rollout inspection (§3.7), `identity.go` (pdig-v1), `attestation.go` (cprot-v2), and the `codextest` test-only construction package. `internal/storage` owns migration v5 and the cprof-v3 canonical profile. `internal/service` owns wiring, the probe template, and the attestation journal operation.

**Tech Stack:** Go 1.25.0, AC-005 `PolicyExecutor`/`workspace`/`execpolicy`, AC-006 `adapter` contract, AC-004 storage transitions, `crypto/sha256` (pdig-v1/cprot-v2). No new deps (JSON-RPC framing is hand-rolled, one JSON object per line over the child's stdio pipes).

**Spec:** [`../specs/2026-09-23-ac-009-codex-adapter-design.md`](../specs/2026-09-23-ac-009-codex-adapter-design.md) (ACCEPTED, approved for planning at `cd4f9a4`)

## Global Constraints

- No provider calls in CI; the fixture `codex` executable replays recorded JSON-RPC fixtures. Native execution is deferred to the manual, sanitized, operator-invoked evidence script.
- **Gate 1 boundary (binding):** NO manual authenticated evidence — any `codex` invocation that can reach the provider, including the integration script's live sections — runs before Gate 1 closes. Gate 1 is fixture-layer only (Tasks 1–6).
- **Production eligibility is reserved for the operator-authorized cprot-v2 evidence path.** Until a valid cprot-v2 attestation (all `sibling_read` + required `approval_deny` records) exists via the journal operation, the production constructor fails closed with `ErrProductionEligibilityMissing`. `codextest` is the only pre-attestation construction path; production `ServerConfig` cannot select it.
- Every launch through `PolicyExecutor.Start` — never `os/exec`. Exact argv template `["app-server", "--listen", "stdio://"]`; any other flag/listener shape rejected. `CODEX_HOME` is never overridden by the adapter; `auth.json` is never read, copied, linked, or relocated.
- Native thread/turn ids are server-generated UUIDv7; Council transmits canonical UUIDs only. Any returned/reported id ≠ requested id is protocol drift ⇒ terminate child, Uncertain.
- Approvals: deny every request; exact §3.6 payloads; never `approved*`/`accept*`/amendment shapes, never `abort`/`cancel`, never `strictAutoReview`. Unverified deny-equivalent variants (`permissions` empty grant, `requestUserInput` empty answers) BEFORE their live-verified `approval_deny` record: terminate child, Uncertain, **no `tool_denied` emitted**.
- Rollout is ADVISORY by default; protected classification requires the launch-frozen attestation. Rollout never proves anything about a different attempt.
- Single-flight per native thread; one pending creation per child; pump/routes armed before any notification-emitting request.
- Forbidden (never in any launch or code path): `--yolo`, `--dangerously-bypass-approvals-and-sandbox`, `--dangerously-bypass-hook-trust`, `--full-auto`, `--approve-for-me`, `danger-full-access`, `--ephemeral`, `--ignore-user-config`, `--ignore-rules`, any `resume --last`/picker/fork/queue form, shared `app-server daemon`, `ws://`/`unix://` listeners, `config/value/write`.
- Only POSIX-process-bound tests are `//go:build unix`; framing, digest, encoding, profile, route, and fixture-replay tests run on every platform; windows/darwin vet+compile stay green.
- Crash-boundary rule (plan-wide): every durable transition in spec §3.11 gets a crash-gap test — simulate death between step N and N+1 and assert recovery classifies correctly.

## Verification Gates

- **Gate 1 (after Task 6):** migration v5 + codex state transitions + crash boundaries (Task 1); cprof-v3 + compatibility matrix (Task 2); pdig-v1 + cprot-v2 encodings (Task 3); JSON-RPC transport + child lifecycle + pump + routes + fixture executable (Task 4); adapter create/resume/dispatch with gate ordering (Task 5); rollout trust + observe/cancel/collect/reconcile (Task 6). Full suites + race ×3. **Manual authenticated evidence forbidden until Gate 1 closes.**
- **Gate 2 (after Task 10):** approval responder + eligibility binding (Task 7), codextest + production construction + probe + journal operation (Tasks 8–9), acceptance story + integration script + evidence matrix (Task 10). Full suites + race ×3 + cross-platform CI. The integration script's authenticated sections run only after Gate 2, operator-invoked.

## Tasks

### Task 1: Storage migration v5 + codex state transitions (MIGRATION FIRST)

**Files:**
- Modify: `internal/storage` (migration to schema version 5; four new tables)
- Create: `internal/storage/codex_state.go`
- Test: `internal/storage/codex_state_test.go`
- Test: `internal/storage/migration_v5_test.go`

**Interfaces (exact, mirroring `claude_state.go`):**
- Migration v5 creates `codex_session_bindings` (session_id PK/FK, native_id TEXT UNIQUE CHECK(uuidv7), materialized BOOL, model, workspace, rollout_path TEXT NULL, profile_digest, first_prompt_digest TEXT NULL), `codex_turn_attempts` (attempt_id, session_id, turn_key, UNIQUE(session_id, turn_key, attempt_id), transition_version INTEGER, native_turn_id TEXT NULL, baseline_file_identity TEXT, baseline_size INTEGER, baseline_entries INTEGER, materialized_baseline BOOL, rollout_protection TEXT, protection_attestation_id TEXT NULL, prompt_digest TEXT, launch_count INTEGER, absence_redispatch_consumed BOOL, accepted BOOL NULL, terminal BOOL, result_payload TEXT NULL, result_usage TEXT NULL, observed_status TEXT, uncertainty_disposition TEXT NULL, updated_at), `codex_attempt_launches` (attempt_id FK, reservation_seq INTEGER, UNIQUE(attempt_id, reservation_seq), state TEXT reserved|started|start_failed|dead, started_at/start_failed_at/first_stdin_byte_at/known_dead_at TIMESTAMP NULL, exit_code INTEGER NULL, child_generation INTEGER, executor_identity TEXT), `codex_protection_attestations` (attestation_id TEXT PK `cprot-v2:sha256:<hex>`, codex_version, platform, manifest_digest, profile_digest, probe_results TEXT canonically-encoded, probed_at TIMESTAMP, actor).
- `InsertCodexSessionBinding`, `GetCodexSessionBinding`, `MarkCodexSessionMaterialized(rolloutPath)`, `InsertCodexTurnAttempt`, `ReserveCodexLaunch` (preconditions JOIN: attestation table for protected gate + positive-absence check; `launch_count` increment + reserved row in ONE transaction), `RecordCodexLaunchState` (started / start_failed-releases-slot-row-retained / dead), `RecordCodexStdinTransmitted` (first-write-wins), `RecordCodexAbsenceVerified` (attestation JOIN, RowsAffected guard), `SetCodexAttemptTerminal(status)` (exactly-once guard; `error`-failed turns ⇒ observed_status failed), `SetCodexAttemptObservedStatus`, `GetCodexTurnAttempt`, `GetLatestCodexTurnAttempt`, `HasCodexUnresolvedAttempts`.

**Steps:**
- [ ] 1.1 Failing tests: exact schema-version 5 assertion (all four tables exist, prior tables intact); transition unit tests mirroring `claude_state_test.go` semantics; crash-gap regressions at all five boundaries (before reservation / after reservation before write / after write before ack / after stdin-byte before acceptance / redispatch consumption); start_failed row retained + next reservation_seq without UNIQUE violation; redispatch cap never exceeds two.
- [ ] 1.2 Implement migration + transitions.
- [ ] 1.3 Suite green; commit `feat(storage): schema v5 — codex bindings, attempts, launches, cprot-v2 attestations`.

### Task 2: cprof-v3 canonical profile + granular tagged union + compatibility matrix

**Files:**
- Modify: `internal/storage/canonical_profile.go` (`AlgoVersion` v3; `HarnessProfileSpec.Codex *CodexHarnessSpec json:"codex,omitempty"`; `CodexHarnessSpec` typed struct with `approval_policy` string-or-granular tagged union)
- Modify: `internal/storage/toolkit_manifest.go` (`RequireToolkitManifest`: Claude accepts v2|v3, still rejects v1)
- Modify: `internal/adapter/execpolicy/executor.go:148` (validity gate widens v1|v2 → v1|v2|v3, toolkit_manifest required for v2|v3)
- Create: `internal/adapter/codex/profile.go` (`ErrUnsupportedProfile`, codex-block completeness validation, universe re-hash)
- Evidence: commit the approval-schema subset `docs/superpowers/evidence/ac009-schema-0.154.0/` (the 7 approval Params/Response JSON pairs + SHA256SUMS manifest; generated provider-free via `codex app-server generate-json-schema`)
- Test: `internal/storage/canonical_profile_v3_test.go`
- Test: `internal/adapter/codex/profile_test.go`

**Interfaces:**
- `storage.ComputeProfileDigest` — `cprof-v3:sha256:<hex>`; v3 requires toolkit_manifest; a `codex` block under v1/v2 is a validation error; normalization per spec §3.8 (BOM/NFC/dedupe/byte-wise sort; scalar trim+NFC; writable_roots path-cleaned; `approvals_reviewer` MUST be `user` at freeze; `approval_policy` enum {untrusted, on-request, never, granular}).
- Granular tagged union: `CodexGranularApproval{McpElicitations, Rules, SandboxApproval, RequestPermissions, SkillApproval json.RawMessage}` — exactly the five keys, unknown keys rejected, values re-encoded verbatim as canonical JSON, equality = byte equality; sub-value shapes validated ONLY against the committed schema evidence; unpinned shape ⇒ granular rejected at freeze.
- `codex.ValidateCodexHarness(spec) (CodexLaunchPolicy, error)` — requires complete block (app_server_version, model_provider, expected_codex_home, platform, sandbox_policy with writable_roots+network_access, approval_policy, approvals_reviewer, expected_mcp_servers, expected_instruction_sources, rules_evidence, event_universe_path+digest); re-hashes the universe file.

**Steps:**
- [ ] 2.1 Failing tests: v3 golden digest vector; codex-block-under-v2 rejection; v1/v2 byte-identical encodings unchanged; compatibility matrix (OpenCode accepts v1|v2|v3 ignoring manifest+codex block — exercised through the opencode eligibility path; Claude accepts v2|v3 requiring manifest, rejects v1; Codex requires v3+complete block ⇒ typed `ErrUnsupportedProfile` otherwise); granular canonical byte-equality, unknown-key rejection, unpinned-shape rejection; `approvals_reviewer != "user"` rejection.
- [ ] 2.2 Implement.
- [ ] 2.3 Suite green (cross-platform); commit `feat(storage,execpolicy,adapter/codex): cprof-v3 additive codex harness spec with compatibility matrix`.

### Task 3: pdig-v1 framing + cprot-v2 attestation encoding

**Files:**
- Create: `internal/adapter/codex/identity.go` (pdig-v1)
- Create: `internal/adapter/codex/attestation.go` (cprot-v2 tagged union)
- Test: `internal/adapter/codex/identity_test.go`
- Test: `internal/adapter/codex/attestation_test.go`

**Interfaces:**
- `codex.PromptDigest(nativeThreadID, turnKey, attemptID, prompt string) (string, error)` — `pdig-v1:sha256:<hex>`; fields in fixed order, each `uint32-BE(len)||bytes`; NUL-byte rejection before encoding.
- `codex.Attestation` canonical encoder — cprot-v2 per spec §3.7: `u8 record_class` {sibling_read=1, self_mutation=2, approval_deny=3}; per-class bodies (tool_class enum read=1..plugin=6; operation enum read=1,write=2,append=3,truncate=4,rename=5,delete=6 with cross-class validity — sibling_read MUST be read, self_mutation MUST be write..delete, violation invalidates the payload; enforcing_capability {cwd_boundary=1, sandbox_restricted_fs=2, guardrail_hook=3, deny_list=4, permission_denial=5}; ≤256-byte code-point-safe excerpts; approval_deny body = method_name + `u8 refusal_kind` {native_refusal_enum=1, live_verified_equivalent=2}); sorting (probe classes by tool_class/operation/tool_name bytes; approval_deny by method bytes); duplicate rejection; payload frame (codex_version, platform, manifest_digest, profile_digest, framed records, RFC3339-UTC probed_at, actor) → `cprot-v2:sha256:<hex>`. Any probe-class record with `denied=false` invalidates the attestation.

**Steps:**
- [ ] 3.1 Failing tests: pdig golden vectors + NUL rejection + field-order sensitivity; cprot-v2 golden vectors (one per record class + mixed-class payload); duplicate rejection; cross-class operation violation; `denied=false` invalidation; excerpt truncation at code-point boundaries; digest stability (identical payloads ⇒ identical digest; changed probed_at/actor ⇒ different digest, intentionally).
- [ ] 3.2 Implement.
- [ ] 3.3 Suite green; commit `feat(adapter/codex): pdig-v1 prompt framing and cprot-v2 tagged-union attestation encoding`.

### Task 4: JSON-RPC transport, child lifecycle, pump + routes, fixture `codex` executable

**Files:**
- Create: `internal/adapter/codex/rpc.go` (framing reader/writer, id correlation, error mapping, poison rules)
- Create: `internal/adapter/codex/client.go` (`initialize`, `account/read`, `thread/start`, `thread/resume`, `turn/start`, `turn/interrupt`, `mcpServerStatus/list`)
- Create: `internal/adapter/codex/server.go` (`CodexServer`: executor launch, handshake gate, detached context, park/close)
- Create: `internal/adapter/codex/eventpump.go` (pump-before-first-notification, creation-reservation route, thread/turn route tables, bounded taps, park-and-replay)
- Create: `internal/adapter/codex/codexfake_test.go` (fixture executable + recorded JSON-RPC fixtures)
- Test: `internal/adapter/codex/rpc_test.go`, `server_test.go`, `eventpump_test.go`
- Fixtures: `internal/adapter/codex/testdata/*.jsonl` (handshake, thread flows, approval requests, notifications, malformed/oversized frames, sanitized from the research evidence)

**Interfaces:**
- `codexrpc.Conn` — one JSON object per line; oversized/malformed frame ⇒ poisoned (typed error, no recovery); request ids monotonic; response/error routed by id; context from the ADAPTER (never a caller ctx) per the detached-launch rule.
- `CodexServer` — launches via `PolicyExecutor.Start` with the exact argv template; `initialize` handshake gates readiness and compares `codexHome`/platform against the frozen profile (mismatch ⇒ terminate, typed error); park after idle grace; `Close()` graceful→kill, pipes drained before Wait.
- Pump — creation-reservation route: pending entry inserted BEFORE `thread/start`; first `thread/started` routes to it and confirms only on response-id equality; orphan `thread/started` ⇒ drift ⇒ terminate+Uncertain; late duplicate ⇒ dedup; turn route inserted BEFORE `turn/start`; unrouted notifications park in bounded per-thread buffers.
- `CodexClient.ResumeProbe(threadID) (EffectiveConfig, error)` — `thread/resume {threadId, excludeTurns:true}`; missing id ⇒ typed `ErrNativeSessionMissing` (verbatim `-32600 no rollout found` mapping).

**Steps:**
- [ ] 4.1 Failing tests: framing (round-trip, oversized poison, malformed poison, duplicate-id rejection), client method shapes against recorded fixtures, handshake attestation mismatch, creation-reservation routing (notification-before-response, id mismatch ⇒ drift, orphan ⇒ drift, late duplicate ⇒ dedup), park/replay, detached context (caller cancel never stops child/pump), exact argv template enforcement.
- [ ] 4.2 Implement + fixture executable.
- [ ] 4.3 Suite green (process tests POSIX-gated); commit `feat(adapter/codex): JSON-RPC transport, child lifecycle, pump routing, fixture executable`.

### Task 5: Adapter core — create/resume/dispatch with the enforceable gate ordering

**Files:**
- Create: `internal/adapter/codex/adapter.go`
- Create: `internal/adapter/codex/turnparams.go` (frozen thread/turn params from the profile; per-turn pins)
- Modify: `internal/storage` (wiring to Task 1 transitions)
- Test: `internal/adapter/codex/adapter_test.go`

**Interfaces:**
- `CodexAdapter` implements `adapter.Adapter`. `CreateSession` ordering (spec §3.4): attestation check (pre-child) → child start → handshake → `account/read` auth gate (unauthenticated/inconclusive ⇒ terminate, typed rejection, no thread or turn created) → creation-reservation route → `thread/start` → id equality → binding `materialized=false`; concurrent creation reservation shared-result; repeated identical config idempotent, changed config rejected; lost response ⇒ `ErrSessionCreationUncertain`.
- `ResumeSession`: validated local inspection only (binding well-formed; materialized ⇒ rollout exists + digest match); native verification deferred to the next dispatch — `thread/resume` liveness then `ErrNativeSessionMissing` on the deterministic error.
- `Dispatch`: `thread/resume` full effective-config compare BEFORE any prompt (model, modelProvider, approvalPolicy, sandbox, approvalsReviewer, instructionSources, cwd) — mismatch ⇒ terminate child, typed `ErrProfileDrift` (pre-acceptance); per-turn pins from the frozen profile on `turn/start`; baseline (file-identity, size, entry count) persisted pre-write; launch reserved in the SAME transaction as `launch_count` increment BEFORE the write; first-stdin-byte boundary ⇒ post-write failures DispatchUnknown, definitive error response ⇒ DispatchRejected, lost response ⇒ DispatchUnknown; single-flight per native thread.

**Steps:**
- [ ] 5.1 Failing contract tests: full ordering (each gate fires in sequence; each failure mode typed); idempotency/concurrency; dispatch accepted (turn/started → id binding)/rejected (definitive error)/unknown (no response); pre-transmission drift rejection for EVERY compared field; single-flight; baseline persistence; crash boundaries re-run through the adapter (reserved-not-started ⇒ Uncertain, no redispatch).
- [ ] 5.2 Implement.
- [ ] 5.3 Race ×3 + suite green; commit `feat(adapter/codex): AC-006 create/resume/dispatch with enforceable gate ordering`.

### Task 6: Rollout trust model + observe/cancel/collect/reconcile — closes Gate 1

**Files:**
- Create: `internal/adapter/codex/rollout.go` (path resolution by UUID-suffix bounded scan; session_meta identity check; baselines; advisory/protected entry correlation; integrity checks)
- Create: `internal/adapter/codex/reconcile.go`
- Test: `internal/adapter/codex/rollout_test.go`, `reconcile_test.go`

**Interfaces:**
- `codex.ResolveRollout(home, threadID) (path, error)` — bounded scan, UUID-suffix match, `session_meta.session_id == native id`, symlink/containment checks; recorded on the binding at materialization.
- Entry correlation (protected): ordered `turn_context` (pins match) → user `response_item` with pdig match → `task_complete` (error absent/present); torn-tail tolerance; mid-file parse failure invalidates; tail-only reads past baseline, size-capped; Windows ⇒ integrity=unverified (decisions degrade to Uncertain).
- Observe: taps over the pump (detach-safe, bounded, overflow ⇒ ErrBufferOverflow); usage snapshots monotonic (cost `Available=false`).
- Cancel: `turn/interrupt` accepted ⇒ `CancelRequested`; verified interrupted terminal ⇒ `CancelConfirmed`; terminal-before-interrupt ⇒ `CancelAlreadyTerminal`; no terminal within bound ⇒ escalate terminate/kill ⇒ Uncertain.
- Collect: terminal-gated on the turn's verified `turn/completed|failed`; ResultUnavailable while pending.
- Reconcile (§3.10): in-life terminal ⇒ terminal; protected-mode rollout reconstruction ⇒ terminal; verified absence (protected) ⇒ one same-attempt redispatch; else Uncertain-until-disposed; probes are separate attempts.

**Steps:**
- [ ] 6.1 Failing tests: rollout golden paths + scan rejections + torn/mid-file/ownership cases (POSIX-gated) + protected correlation order + advisory diagnostics-only; Observe detach/overflow; Cancel requested→confirmed→escalation; Collect gating; Reconcile four-state matrix + redispatch-once + block-until-disposed across reopen.
- [ ] 6.2 Implement.
- [ ] 6.3 Race ×3 + full suite + windows/darwin vet. **Gate 1 review.**
- [ ] 6.4 Commit `feat(adapter/codex): rollout trust model, observation, cancellation, reconciliation (Gate 1)`.

### Task 7: Approval responder + per-variant deny table + eligibility binding

**Files:**
- Create: `internal/adapter/codex/responder.go`
- Test: `internal/adapter/codex/responder_test.go`

**Interfaces:**
- `codex.Responder` — routes server→client requests; per-variant exact payloads (spec §3.6 table): `execCommandApproval`/`applyPatchApproval` ⇒ `{"decision":{"denied":{"rejection":…}}}`; `item/commandExecution|fileChange/requestApproval` ⇒ `{"decision":"decline"}`; `mcpServer/elicitation/request` ⇒ `{"action":"decline"}`; duplicates ⇒ idempotent re-deny + single mirror per ApprovalID; late (post-terminal) ⇒ deny + diagnostic, no state change; unparseable ⇒ terminate, Uncertain.
- Unverified deny-equivalents (`item/permissions/requestApproval`, `item/tool/requestUserInput`): BEFORE a live-verified `approval_deny` record in the launch-frozen attestation ⇒ terminate child, attempt Uncertain, no payload sent, **no `tool_denied` emitted**; AFTER ⇒ empty grant `{"permissions":{},"scope":"turn"}` / `{"answers":{}}`, never `strictAutoReview`.
- Mirroring into the turn stream as `tool_requested`/`tool_denied` with the unverified-variant exception.

**Steps:**
- [ ] 7.1 Failing tests: every §3.6 table row against fixtures; duplicate/late/unparseable; the two unverified variants pre-attestation (Uncertain + no event) and post-attestation (payload + mirror); responder install ordering relative to pump.
- [ ] 7.2 Implement.
- [ ] 7.3 Suite green; commit `feat(adapter/codex): approval responder with per-variant deny payloads and eligibility binding`.

### Task 8: codextest construction mode + production guard

**Files:**
- Create: `internal/adapter/codex/codextest/` (test-only constructor package; fixture harness)
- Test: `internal/adapter/codex/codextest/guard_test.go`
- Test: `internal/adapter/codex/production_guard_test.go`

**Interfaces:**
- `codextest.NewFixtureAdapter(...)` — production eligibility skipped, ALL protocol validation retained; usable only from `internal/adapter/codex` tests and the manual evidence executable.
- Production construction has no parameter surface selecting fixture mode; handing the test-only option to the production constructor fails closed (typed error).
- Build guard: production packages do not import `codextest` (AC-006 adaptertest pattern).

**Steps:**
- [ ] 8.1 Failing tests: production constructor + test-only option ⇒ typed rejection; registry/factory fails closed for the codex fake; import guard.
- [ ] 8.2 Implement.
- [ ] 8.3 Commit `feat(adapter/codex): test-only construction mode with production guard`.

### Task 9: Service wiring + probe template + attestation journal operation

**Files:**
- Modify: `internal/service/server.go` (`ServerConfig.CodexBinaryPath`, codex construction, scratch-dir rule)
- Create: `internal/adapter/codex/wiring.go` (`NewProductionCodexAdapter`, `CodexProbeLaunchTemplate`)
- Create: `internal/service/codex_probe_attestation.go` (cprot-v2 journal operation — operator authority required)
- Test: `internal/service/review_ac009_wiring_test.go` (POSIX — real stub child)
- Test: `internal/adapter/codex/probe_test.go` (portable rejection + POSIX e2e)

**Interfaces:**
- `NewProductionCodexAdapter(cfg, storage)` — requires codex config; EVERY launch re-checks the launch-frozen attestation; `ErrProductionEligibilityMissing` pre-child otherwise.
- Probe template (operator-owned, provider-free, scratch cwd): `codex --version`; `app-server daemon version`; probe child `initialize` + negative `thread/resume` (zero UUID ⇒ verbatim `no rollout found`); `model/list`; `mcpServerStatus/list` via the probe child; `codex login status` capture. `generate-json-schema` evidence refresh is an operator step, not a run-time dependency.
- Journal operation inserts cprot-v2 rows only with operator authority (actor, operation ID, AC-004 journal entry); attempt freezing verified (later attestations never reclassify).

**Steps:**
- [ ] 9.1 Failing tests: construction config requirements fail closed; probe contract assertions against the stub; journal operation authority + persistence + freeze semantics; `ErrProductionEligibilityMissing` end-to-end pre-attestation through production construction.
- [ ] 9.2 Implement.
- [ ] 9.3 Commit `feat(service): wire Codex production construction, probe, cprot-v2 journal operation`.

### Task 10: Acceptance story + integration script + Gate 2

**Files:**
- Create: `internal/service/acceptance_codex_test.go` (POSIX — fixture `codex` child through the production construction path via `codextest` eligibility skip, real executor)
- Create: `scripts/ac009-integration-evidence.sh` (manual, sanitized, CI-excluded)
- Modify: PR #9-linked body (evidence matrix)

**Steps:**
- [ ] 10.1 Failing acceptance test through the bridge: configure → create (gate ordering) → queue → release → gated execution to the fixture child → approval deny mirroring → collect → disconnect (child survives) → reconnect → follow-up on the exact thread → durable outcomes; spec §6.1 scenarios 1–12 as focused tests.
- [ ] 10.2 Integration script, ordered with the Gate 1 boundary restated at the top: **Stage A (provider-free, Gate-1-gated):** version/daemon-version/schema refresh capture, probe child handshake + negative resume, model/MCP inventories, `login status`. **Stage B (authenticated, operator-authorized ONLY, after Gate 2):** one trivial live turn; approval deny-path for EVERY §3.6 variant (produces the `approval_deny` records); `turn/interrupt` live; child kill-and-resume; resume-drift firing; post-launch `turn_context` confirmation. **Stage C (operator-authorized probe suite):** `sibling_read` + `self_mutation` probes → first cprot-v2 attestation via the journal operation. Never selects a provider/model automatically; redacted outputs; sanitized fixtures.
- [ ] 10.3 Full verification: `gofmt -l cmd internal`; `go vet ./...`; `CGO_ENABLED=0 go test ./... -count=1`; race ×3 on codex/storage/service/execpolicy; windows/darwin vet; CI green.
- [ ] 10.4 Gate 2: PR body evidence matrix (spec §6/§6.1 rows → committed tests), request review.

## Self-Review: Invariant → Interface → Assertion

| Invariant | Interface | Assertion |
|---|---|---|
| Migration v5 first; crash-safe durable transitions | `codex_state.go` | schema-version 5 test; five crash-gap regressions; retained start_failed rows |
| App Server stdio only; exact argv; executor-only launches | `CodexServer` + executor template validation | argv rejection matrix; lifecycle tests use real executor |
| Gate ordering: attestation pre-child, auth post-child-start (no thread or turn created) | `CreateSession` + `ErrProductionEligibilityMissing` + auth rejection | ordering tests; end-to-end pre-attestation rejection |
| cprof-v3 frozen pins verified BEFORE transmission, first turn included | `thread/resume` compare + `ErrProfileDrift` | per-field drift rejection matrix incl. first-turn ordering |
| Native UUIDv7 identity; id drift ⇒ Uncertain | creation-reservation route + pump | notification-before-response, orphan, mismatch tests |
| Approvals denied with exact payloads; unverified equivalents fail closed silently | `Responder` | §3.6 table fixtures; no-`tool_denied` pre-attestation test |
| Rollout advisory by default; protected needs launch-frozen cprot-v2 | `rollout.go` + attestation table | advisory ⇒ Uncertain-until-disposed; freeze-at-launch test |
| One verified-absence redispatch, crash-safe | launch reservation transitions | crash boundaries (a)–(e) through the adapter |
| Production eligibility via operator-authorized cprot-v2 only; codextest never production | production constructor + journal op + import guard | guard tests; ErrProductionEligibilityMissing e2e |
| No provider calls in CI; authenticated evidence post-Gate-1/2, operator-invoked | fixture executable + script stages | CI green without auth; script Stage A/B/C gating |
