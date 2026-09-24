# AC-009 Design — Codex persistent contributor adapter

Status: ACCEPTED — approved for implementation planning (DRAFT v4 review,
2026-09-23); editorial corrections applied. Implementation plan:
`docs/superpowers/plans/2026-09-23-ac-009-codex-adapter.md`.
Date: 2026-09-23
Issue: #9
Depends on: AC-003 (controller grants), AC-005 (workspaces/execution policy),
AC-006 (adapter contract)
Evidence basis: `docs/superpowers/evidence/ac009-codex-installed-interface-research.md`
(live-probed `codex-cli 0.154.0`, linux-x86_64) and
`docs/superpowers/evidence/ac009-native-event-universe-0.154.0.json`

## 1. Problem and scope

Council needs Codex as a persistent contributor: an adapter that drives the
installed Codex binary over a **verified native transport**, keeps thread
identity exact across turns (fresh thread for an independent proposal, exact
resumption for later turns), preserves the operator's configured sandbox,
approval, and guardrail behavior (no yolo/bypass anywhere), routes permission
requests and tool outcomes as **structured state** rather than terminal
scraping, bounds and cancels turns, prevents duplicate execution across
interruption and reconnection, and reports usage and toolkit evidence with
explicit availability boundaries.

Out of scope: Claude/Agy adapters (AC-008 done / AC-010+), the shared
`app-server daemon` + `proxy` mode (documented future option, §3.2),
`exec fork`/`queue`/`review`/`cloud`/`remote-control` surfaces, live-provider
acceptance runs in CI (manual/sanitized only, like AC-007/AC-008), and OS-level
user isolation (owned by AC-005).

## 2. Installed-interface research (verified codex-cli 0.154.0)

Full probes, verbatim errors, and safety-method statement are in the evidence
file; the decisive subset:

### 2.1 Two transports, verified capabilities

| Capability (issue #9 need) | `codex exec` (CLI) | `codex app-server` (JSON-RPC 2.0 over stdio) |
|---|---|---|
| Fresh thread | `thread.started {thread_id}` first JSONL event (native UUIDv7, no caller-chosen id) | `thread/start` → full Thread object (`id`, `model`, `status`, `path`), **no model call** (live-verified) |
| Exact resume | `exec resume <UUID>` — same id, **same rollout file appended** (17→29 lines; context carry verified); **rejects `--sandbox`/`-C`** | `thread/resume {threadId, excludeTurns:true}` → thread + effective config, **no turn, no model call** (live-verified) |
| Provider-free resume verification | Only the deterministic missing-id error (`no rollout found for thread id <id> (code -32600)`) — proves existence, nothing else | **Yes** — the decisive ResumeSession primitive; returns `approvalPolicy`, `sandbox`, `approvalsReviewer`, `model`, `modelProvider`, `instructionSources` |
| Structured approvals | **None** — headless; no `-a` flag; denials surface only as sandbox FS failures in the rollout | Protocol-native server→client requests: `execCommandApproval {callId, command[], conversationId, cwd, parsedCmd[]}`, `applyPatchApproval`, `item/*` request variants (schema-verified; loop not live-exercised) |
| Cooperative cancellation | **None** — process kill only | `turn/interrupt {threadId, turnId}`, `TurnStatus.interrupted` (schema-verified) |
| Structured tool outcomes | stdout JSONL is a **reduced view** (no `commandExecution` items); full record only in the rollout file | `item/started`, `item/completed`, `item/commandExecution/*`, deltas, `turn/completed {turn{items[], status}}` |
| Usage | `turn.completed.usage` + rollout `token_count` | `thread/tokenUsage/updated {tokenUsage{last,total}}` with full breakdown |
| Terminal durable record | Rollout carries `task_complete` (+usage, +error) — **unlike Claude's JSONL** | `turn/completed|failed` notifications + same rollout |
| Client-disconnect decoupling | Process-per-turn dies with the turn | Child (or daemon) outlives observer detach |

### 2.2 Verified hazards (all live-verified unless noted)

- **Non-UUID resume hazard**: `codex exec resume definitely-not-a-real-id "hi"`
  did NOT error — it **silently created a new thread and ran a live turn**.
  Council must pass canonical UUIDs only and fail closed on any returned id
  drift.
- **Policy drift on resume**: `exec resume` re-derives effective policy from
  **current** config, not the recorded turn (live-verified: a turn recorded
  `gpt-6-astra`/`never`; resume reported `gpt-5.6-sol`/`on-request`/
  `auto_review`). Adapter must pin model/policy explicitly per turn and verify
  effective config before prompt transmission.
- **Identity-ambiguous resume forms** — forbidden: `codex resume` (TUI
  picker), `resume --last`, `exec resume --last`, cwd-filtered pickers.
- **Forbidden variants** (verified to exist in help; never executed):
  `--dangerously-bypass-approvals-and-sandbox`,
  `--dangerously-bypass-hook-trust`, sandbox `danger-full-access`,
  `--full-auto`, `--approve-for-me`, `--ephemeral` (loses persistence),
  `--ignore-user-config`/`--ignore-rules` (toolkit omission).
- **Auth is home-bound**: `CODEX_HOME` isolation verified to relocate state,
  but auth does not follow (`account/read → requiresOpenaiAuth:true`, 401s).
  Credentials are never copied ⇒ authenticated contributor runs must use the
  operator-provisioned `CODEX_HOME` (§3.3).
- **Git/trust check**: outside a trusted directory, exec fails with
  `Not inside a trusted directory and --skip-git-repo-check was not
  specified.` The same trust model applies to threads whose cwd is the
  workspace root (operator precondition, §3.8).
- **app-server is labeled `[experimental]`**; the live turn loop
  (`turn/start`, approval request/response, `turn/interrupt`,
  notification stream during a turn) is **schema-verified, not
  live-exercised** — closing those gaps is a required integration-evidence
  task (§4), not an assumption.
- Sandbox choice set: `read-only`, `workspace-write`, `danger-full-access`.
  Approval choice set (`AskForApproval`): `untrusted`, `on-request`, `never`,
  structured `granular`. `ApprovalsReviewer`: `user | auto_review |
  guardian_subagent`. Linux sandbox denial verified observably
  (`Read-only file system`); Landlock/Seatbelt naming: not verified.
- Cost fields: **absent from the entire protocol** — `UsageMetric.Available =
  false`, never fabricated.

## 3. Architecture

Target packages: `internal/adapter/codex` (new), `internal/storage`
(migration + `codex_state.go`), wiring in `internal/service`. Implements the
AC-006 `adapter.Adapter` contract; conformance suite and council boundary
tests apply unchanged.

### 3.1 Transport selection: App Server is primary; `exec` is not used

**Decision: `codex app-server` (stdio JSON-RPC), one child per contributor
session.** Evidence-driven: app-server is the only verified surface for
provider-free resume verification (`thread/resume {threadId,
excludeTurns:true}`), structured approval routing, turn-scoped cooperative
interrupt, and full structured tool-outcome notifications. `exec resume`
itself internally calls `thread/resume` — the app-server surface is the
substrate; driving it directly removes the reduced-stdout and no-approval
liabilities. This satisfies the issue's "verified native App Server or CLI
adapter selected from installed capabilities".

**Honest caveats, carried forward as evidence obligations**: (1) the
app-server live turn loop is schema-verified only — the integration evidence
script must live-verify `turn/start`, the approval deny-path,
`turn/interrupt`, and notification flow before any "verified" claim (§4);
(2) `[experimental]` labeling means the probe template pins and records the
installed version, and the adapter fails closed on protocol drift (unknown
method/error shapes → Uncertain per §3.9).

**Rejected alternatives**: `exec` as primary (no approvals, no interrupt,
reduced stdout, resume drift with no pinning surface); shared `app-server
daemon` (multi-tenant daemon lifecycle/version-skew ownership unverified;
per-session children give AC-007-proven isolation and bounded accumulation —
the daemon remains a documented future option and is never launched by this
adapter).

### 3.2 Child lifecycle — one `codex app-server` child per contributor session

(`CodexServer`, `internal/adapter/codex/server.go`, AC-007 pattern.)

- Launched through the AC-005 `PolicyExecutor`, never `os/exec`: argv exactly
  `["app-server", "--listen", "stdio://"]` (default listen mode is stdio; the
  explicit form is pinned so the executor's template validation has one
  shape). `ws://`/`unix://` listeners and auth-token flags are forbidden in
  the template.
- Working directory: a Council-owned neutral scratch directory allocated with
  the launch (NOT the workspace root — thread cwd is set per `thread/start`
  param, §3.5; the child's own cwd is process plumbing, kept out of every
  real project directory).
- Environment: inherited through the AC-005 inherited-env allowlist.
  **`CODEX_HOME` is NOT overridden** for authenticated runs (§3.3). No
  `GeneratedServerEnv`-style injection is needed — the transport is local
  stdio with no credential surface.

**Launch network/isolation matrix (frozen profile ⇒ executor behavior).**
The app-server child needs provider-endpoint egress (verified: Codex dials
the model provider itself, WebSocket first, HTTPS fallback), while
model-created network activity is tool activity inside the Codex sandbox.
These are distinct planes governed separately: the child's egress by the
frozen `network_mode`/`isolation_strictness` (AC-005 executor), the model's
tools by the frozen `sandbox_policy.network_access` (§3.8) — the sandbox
denial is native and holds even where executor egress is degraded.

| Isolation strictness | Network mode | Launch behavior |
|---|---|---|
| `permissive_dev` | `unrestricted` | Launches. Provider egress direct. **Degraded per AC-005**: NOT OS-enforced — never presented as egress control. Model-created network is still denied natively by the frozen sandbox (`network_access:false`). |
| `permissive_dev` | `allowlist` | Launches. Provider egress through the AC-005 allowlist proxy, destination-gated (model-provider endpoints, e.g. `api.openai.com:443`, from `network_allowlist`). Model-created network: proxy + sandbox denial. |
| `permissive_dev` | `none` | Launches (stdio, no listener, no loopback requirement). Provider calls fail natively — turns surface honest transport failures (verified error shapes). Air-gapped/fixture evidence; not production contribution. |
| `strict` | `allowlist` | **Launch rejected, fail-closed**: the executor's strict network capability is unsupported for this long-lived child class (proxy reachability from a fresh namespace is unverified). The capability error is surfaced; enforcement is never weakened to fit. |
| `strict` | `none` | Launches; fresh namespace has no external egress; provider calls fail natively (honest); stdio needs no loopback. |
| `strict` | `unrestricted` | **Launch rejected, fail-closed**: external egress from a fresh strict namespace is an unverified executor capability for this long-lived child class (only `none` is established); the gap is surfaced honestly, never silently downgraded to permissive. |

**Workspace/network composition**: workspace-mode capability enforcement
(`none`/`readonly`/`isolated_branch`) and network enforcement are
independent AC-005 dimensions, evaluated separately. Either dimension
failing its capability check rejects the launch; a degraded workspace grant
never combines with a passing network grant (or vice versa) into a
"partially isolated" launch.

If the executor cannot enforce the selected policy the launch fails or is
recorded as degraded, per AC-005 reporting — the adapter never claims
enforcement that does not exist. The neutral scratch child cwd below is
process plumbing only: thread cwd is the frozen workspace root (§3.5) and
network policy comes only from the frozen profile.
- Handshake gate: `initialize` with a Council `clientInfo` (name
  `agent-council`, version from build); the result's `codexHome` and
  `platformOs`/`platformFamily` are captured as environment attestation and
  compared against the frozen profile's expected home/platform; mismatch →
  child terminated, fail closed (pre-session, typed error).
- I/O: stdout read by a single JSON-RPC framing reader (one JSON object per
  line; oversized-line bound); stderr drained once from process start
  (bounded tail kept, never parsed); requests correlated by monotonically
  increasing JSON-RPC ids.
- **Adapter-owned detached launch context** (AC-007 lifecycle fix —
  `context.WithoutCancel` at the `opencode` server start): the child launch
  and the JSON-RPC reader/pump run on an adapter-owned context bound to the
  session lifecycle, never the caller's Dispatch/Observe ctx. Dispatch's ctx
  bounds only the request write and its ack wait; a disconnected controller
  never kills the child or the in-flight turn.
- **Pump-before-first-notification ordering (required)**: the framing
  reader, JSON-RPC dispatch loop, thread/turn route table, and bounded tap
  buffers are armed BEFORE any notification-emitting request is sent —
  `thread/start` is the first such request. The turn's route entry is
  inserted before `turn/start`.
- **Creation-reservation route (first-notification gap, closed)**: the
  native thread id is unknown until `thread/start` answers, yet
  `thread/started` may arrive earlier — so BEFORE `thread/start` is sent,
  the adapter inserts a pending-creation route entry binding the JSON-RPC
  request id, the logical session, and the durable creation reservation
  (§3.4; one pending creation per child at a time). The first
  `thread/started` during a pending creation routes to that entry and is
  CONFIRMED only when the response arrives and its result `id` equals the
  notification's `thread_id`; only then is the binding published.
  Mismatch, or a `thread/started` with neither a pending reservation nor a
  bound thread id, is protocol drift ⇒ child terminated, creation uncertain
  (§3.9). A late duplicate for an already-bound thread is deduplicated, no
  state change. Notifications for a bound thread with no route park in a
  bounded per-thread buffer and replay on route installation.
- `Close()`: no native dispose method is verified — graceful terminate →
  force-kill (AC-005 terminate split), pipes drained before Wait, exit
  evidence recorded. Park semantics follow AC-007: stop after idle grace;
  `ResumeSession` after parking starts a replacement child and verifies the
  binding provider-free (§3.4).
- Death of the child is NOT thread death: threads persist in
  `$CODEX_HOME/sessions/...`; Reconcile treats "child unreachable" as host
  visibility lost, never as definitive failure (AC-004 lesson).

### 3.3 Sibling isolation and the auth-home constraint

Codex auth (ChatGPT login) is bound to `CODEX_HOME`; isolated homes are
unauthenticated (verified: `account/read → requiresOpenaiAuth:true`, 401 on
model calls), and Council never reads, copies, or relocates credentials.
Therefore authenticated contributor threads run against the
**operator-provisioned `CODEX_HOME`**, and AC-008's per-session config-root
isolation is NOT available.

**OS-level separation was assessed and rejected for this design**: relocating
`CODEX_HOME` breaks native auth; making a relocated home authenticate would
require copying or linking credential material into worker reach
(forbidden); masking sibling rollout paths inside one shared sessions tree
via namespaces/mounts is neither expressible without privileges nor a
verified AC-005 executor capability. If AC-005 later provides
credential-preserving OS separation, this section can be revisited.

**Production-eligibility gate (attested enforcing capability) — binding:**

- The frozen-generation invariant (no sibling access during generation) is a
  **production-eligibility requirement**, not an evidence-class nicety. The
  advisory/protected rollout model (§3.7) classifies what Council may
  *believe*; it does not stop a Codex tool from *reading* another
  contributor's rollout in the shared home.
- Read isolation and evidence integrity are **different properties**, proven
  by different probe classes and bound to different consequences (§3.7):
  `sibling_read` records prove a worker cannot READ another contributor's
  rollout; `self_mutation` records prove the authoring worker cannot modify
  or forge its OWN rollout. Read isolation alone never upgrades evidence —
  a worker that could rewrite its own rollout could fabricate the durable
  terminal record the protected class relies on.
- Authenticated Codex production dispatch is eligible ONLY while a valid
  **isolation attestation** exists — all `sibling_read` records denied AND
  approval-deny verification bound (§3.6/§3.7: every approval variant the
  adapter may receive has established deny semantics — schema-native
  refusal enums statically qualify; deny-EQUIVALENT shapes qualify only
  with a live-verified `approval_deny` record in the attestation) — for the
  exact (installed Codex version, platform, profile/toolkit manifest
  digest) tuple. The attestation is produced by the operator-authorized
  probe suite (§3.7): attempts to read a SIBLING rollout through every
  enabled tool path — Read-class, Glob-class, Grep-class, shell with
  absolute path, each approved MCP tool class, skill/plugin-contributed
  tools — each recorded with the enforcing capability that denied it
  (sandbox restricted-FS policy, workspace cwd boundary, guardrail
  rules/hooks). Any reachable sibling-read path, or any unverified
  deny-equivalent variant, ⇒ no valid isolation attestation ⇒
  **authenticated production dispatch fails closed**: typed
  `ErrProductionEligibilityMissing`, frozen at launch (AC-008
  attestation-freeze semantics).
- **Enforceable gate ordering (auth state needs a started child; the
  attestation does not)**:
  1. **Before any child starts**: the isolation attestation is durable
     Council-side state and is checked first. Missing or invalid ⇒
     `ErrProductionEligibilityMissing` — **no process started**.
  2. **Child starts** (only after step 1 passes): pump armed →
     `initialize` attestation → provider-free `account/read`.
     `requiresOpenaiAuth:true` ⇒ child terminated, typed auth rejection —
     **no thread or turn created**. An inconclusive auth check (error,
     timeout, unknown shape) fails closed the same way. This is the only
     gate that requires a process, and it is honest about it: nothing
     native was created.
- **Test-only construction mode (explicit, never inferred)**: fixture
  operation does not bypass the gate by ambiguity — there is NO inference
  of fixture status from absent authentication or from any runtime
  observation; fake adapters are test utilities, never production
  fallbacks. Instead:
  - A dedicated test-only construction path — `codextest` package
    (mirroring the AC-006 `adaptertest` guard) plus a manual evidence
    executable — builds an adapter with production eligibility SKIPPED but
    every protocol validation retained (pump ordering, id discipline,
    drift detection, cancel semantics, route confirmation).
  - Production construction (`ServerConfig` wiring) has no path to the
    test-only mode: the constructor used by the service ALWAYS requires the
    attestation check before child launch. Conformance/boundary tests
    assert the production construction fails closed when handed the
    test-only option, and the build guard asserts production packages do
    not import `codextest`.
  - The manual evidence executable is operator-invoked only, never
    registered as a production adapter.
- Version bump, platform change, or manifest change invalidates the
  attestation; new launches fail closed until a fresh probe suite is
  attested. A later attestation never retroactively reclassifies earlier
  attempts.
- Threat boundary under attestation, stated honestly: the attestation
  proves denial for the probed paths on the probed version and
  configuration. It is version-bound evidence, not a mathematical
  guarantee; unprobed tool classes invalidate it by construction (the suite
  enumerates every enabled class).

Rollout files accumulate in the operator home by native design; Council's
evidence trail lives in its storage layer and references rollout paths it
observed (§3.7). Purge of native session files is an operator action
(`archive`/`delete` verified to exist as native commands) — never an
adapter side effect.

### 3.4 Session creation, idempotency, and binding

- `CreateSession` resolves a free **provider-free** path end to end, in
  this enforceable order: isolation-attestation check (durable, pre-child —
  failure ⇒ no process started, §3.3) → start child (§3.2) → pump armed →
  `initialize` attestation → `account/read` auth gate (unauthenticated or
  inconclusive ⇒ child terminated, **no thread or turn created**, §3.3) →
  creation-reservation route inserted (§3.2) → `thread/start {}` (no model
  call, verified) → response/notification id equality confirmed → the
  native UUIDv7 `id` becomes `NativeSessionID`; the adapter validates
  UUIDv7 shape and returns the binding with `materialized=false` (no
  rollout observed yet). The adapter does not persist; service/storage
  persists the binding. Errata (implementation review): persistence is
  the AC-008-shaped transactional transition (`BindCodexSession`) — the
  controller lease is re-validated INSIDE the write transaction (a lease
  handed off while the native creation was in flight cannot publish the
  binding), the op_id is journaled with receipt replay, and one binding
  per session is enforced. The pre-flight authority check is an early
  refusal, not the authority.
- `thread/start` accepts the frozen thread parameters that the schema
  supports at creation (cwd = workspace root; model/sandbox/approval are
  pinned per turn at dispatch, §3.5, because resume re-derives them —
  creation-time values are recorded for the manifest, not trusted alone).
- Repeated `CreateSession` with the same logical session and identical
  frozen config returns the existing binding (AC-006 invariant); changed
  config is rejected.
- **Lost `thread/start` response** (child transport broke before the
  response arrived): `ErrSessionCreationUncertain`, automatic recreation
  blocked. Because `thread/start` consumed no provider quota, the eventual
  operator disposition is normally "abandon the orphan thread" (cheap,
  honest); `thread/list` sweeps are diagnostic only and never auto-bind.
  Errata (implementation review): the durable block is modeled as
  numbered **creation-uncertainty episodes** per logical session
  (`codex_creation_uncertainties`, migration v6): an uncertain outcome
  opens the next episode (concurrent callers sharing one outcome open
  ONE episode; the schema allows at most one open episode per session);
  the block holds while an episode is open; a resolution targets ONE
  exact episode and requires CURRENT controller authority (lease +
  expected generation re-validated in the storage transaction) with a
  disposition (`abandon_orphan` | `verified_absent`) and reason. A
  session that ends uncertain again after a resolution opens a distinct
  episode — never a replay of, or a conflict with, the resolved one.
- Concurrent duplicate `CreateSession`: creation reservation (one native
  thread identity, shared result including typed failures, mismatched
  callers fail closed) — AC-008 pattern.

### 3.5 Dispatch — turn identity, pinning, and pre-transmission verification

- **Native turn ids are server-generated UUIDv7** (schema-verified; no
  client-chosen id). In-flight correlation: the `turn/started` notification
  carries `turnId`; the adapter maps `(nativeThreadId, turnId) → TurnRef`.
  Durable correlation: pre-launch **rollout baseline** + prompt-digest match
  (§3.7), because thread ids alone do not identify turns.
- **Pre-transmission verification — uniform, FIRST TURN INCLUDED (decisive
  improvement over Claude).** JSON-RPC gives Council a provider-free check
  BEFORE any prompt is written. For EVERY turn — first and resumed:
  1. `thread/resume {threadId, excludeTurns:true}` (provider-free,
     verified) and compare the COMPLETE returned effective config (`model`,
     `modelProvider`, `approvalPolicy`, `sandbox`, `approvalsReviewer`,
     `instructionSources`, `cwd`) against the frozen profile manifest. On
     the first turn this runs immediately after `thread/start`:
     `thread/start`'s own response carries only partial configuration
     (`model`, `environments[].cwd`) and is checked but NOT trusted as
     complete proof — the resume call is the affirmative verification. Any
     mismatch ⇒ child terminated, **typed `ErrProfileDrift` rejection —
     pre-acceptance, the prompt was never transmitted** (Claude's
     `system/init` arrives only after stdin, forcing Uncertain; Codex does
     not have that problem).
  2. `turn/start` carries the frozen per-turn pins (`model`,
     `sandboxPolicy`, `approvalPolicy`, `cwd`) from the manifest — never
     values re-derived from ambient config. This mitigates the verified
     resume-drift hazard (§2.2). Pins alone do not prove the server honored
     them: step 1 establishes the pre-transmission baseline and step 3
     closes the loop.
  3. **Post-launch policy confirmation**: in protected mode, the rollout's
     `turn_context` entry (verified to record the effective permission
     profile) is compared against the frozen pins after the turn; a
     mismatch means the turn executed under unverified policy ⇒ Uncertain,
     drift recorded. Advisory mode: diagnostic only, gap recorded honestly.
- **Prompt-digest framing (`pdig-v1`)** — the durable turn-correlation
  digest, canonical framing family of `ctmpl-v1`/`cprot-v1`: fields in
  fixed order — native thread id, turn key, attempt id, prompt (UTF-8
  bytes) — each encoded `uint32-BE(len) || bytes`; digest =
  `pdig-v1:sha256:<lowercase-hex>`. NUL bytes in any field are rejected
  before encoding. Stored on the attempt at launch; matched against rollout
  entries per §3.7.
- **Acceptance semantics (JSON-RPC ack, better than stream-only)**: a
  definitive error response to `turn/start` (e.g. `thread not found: <id>`,
  invalid params) after the request was written is **DispatchRejected** with
  the error reason — the native side definitively refused the turn. A lost
  response, transport break after first byte written, or child death before
  response ⇒ **DispatchUnknown** (the turn may have started). Pre-write
  failures (child not running, write refused before any byte) ⇒
  DispatchRejected with pre-acceptance evidence. The transmission boundary
  is the first successfully written stdin byte of the request frame.
- **Single-flight per native thread**: one in-flight turn per thread
  (`in-flight map` keyed by native thread id); a second Dispatch is rejected
  (`thread busy`) rather than queued — one TurnRef ↔ at most one in-flight
  native turn.
- **Id-drift fail-closed**: any response/notification whose thread id
  differs from the binding's native id is a protocol violation — stream
  poisoned, child terminated, attempt Uncertain (this is the adapter-side
  neutralization of the non-UUID hazard; Council never passes non-canonical
  UUIDs in the first place).

### 3.6 Permission (approval) semantics

- Server→client approval requests are answered **deny** by the adapter's
  approval responder, always and without exception. The exact deny payload
  is fixed per variant from the installed binary's own protocol schema
  (`generate-json-schema`, provider-free, 0.154.0) — **not delegated to
  whatever the generator emits at implementation time**. Council never
  sends any `approved*`/`accept*`/amendment variant (all of them widen
  policy — including network-policy and execpolicy amendments), never
  `abort`/`cancel` (a denial must not interrupt the turn; bounded-turn
  discipline governs termination), and never sets `strictAutoReview`.

  | Request method | Correlation fields | Exact deny response (JSON-RPC `result`) | Evidence |
  |---|---|---|---|
  | `execCommandApproval` | `callId`, `conversationId`; optional `approvalId` | `{"decision":{"denied":{"rejection":"<council denial text>"}}}` (schema `ReviewDecision.DeniedReviewDecision`; NOT `abort`) | schema-verified; live acceptance = integration obligation (§4) |
  | `applyPatchApproval` | `callId`, `conversationId` | same `{"decision":{"denied":{"rejection":…}}}` shape | schema-verified; live acceptance = obligation |
  | `item/commandExecution/requestApproval` | `itemId`, `threadId`, `turnId` | `{"decision":"decline"}` (schema `CommandExecutionApprovalDecision`; NOT `cancel`) | schema-verified; live acceptance = obligation |
  | `item/fileChange/requestApproval` | `itemId`, `threadId`, `turnId` | `{"decision":"decline"}` (`FileChangeApprovalDecision`) | schema-verified; live acceptance = obligation |
  | `item/permissions/requestApproval` | `itemId`, `threadId`, `turnId` | **No deny enum exists** — deny-EQUIVALENT is the EMPTY grant `{"permissions":{},"scope":"turn"}` (grants no filesystem or network permission; `strictAutoReview` never set). Usable ONLY after a live-verified `approval_deny` record exists (below); until then receipt ⇒ Uncertain | shape schema-verified (required `permissions` object, no refusal variant); denial SEMANTICS live-verified into the attestation before use |
  | `item/tool/requestUserInput` | `itemId`, `threadId`, `turnId` | deny-EQUIVALENT is `{"answers":{}}` — empty answer map (EXPERIMENTAL; no refusal shape exists). Usable ONLY after a live-verified `approval_deny` record exists (below); until then receipt ⇒ Uncertain | shape schema-verified; denial SEMANTICS live-verified into the attestation before use |
  | `mcpServer/elicitation/request` | `serverName`, `threadId`; optional `turnId` | `{"action":"decline"}` (content omitted — nullable for decline) | schema-verified; live acceptance = obligation |

  Each request and its denial are mirrored into the turn's event stream as
  `tool_requested`/`tool_denied` with `ApprovalID` set (for
  `requestUserInput`/elicitation: mirrored as `tool_requested`/`tool_denied`
  with the diagnostic reason), so the transcript records the denial honestly
  (AC-007 §3.6 carried forward). The exception: a request received for an
  UNVERIFIED deny-equivalent variant (rule below) emits NO `tool_denied` —
  nothing is sent, nothing is mirrored, and the attempt is Uncertain.
- **Unverified deny-equivalents fail closed (no fabricated denials)**: for
  the two variants whose refusal semantics are NOT schema-defined
  (`permissions` empty grant, `requestUserInput` empty answers), the
  deny-equivalent payload may be sent ONLY after a live-verified
  `approval_deny` record for that variant exists in the launch's governing
  attestation (§3.7). Until then, RECEIVING such a request terminates the
  child and classifies the attempt **Uncertain** — no deny payload is sent,
  **no `tool_denied` event is emitted** (Council would be fabricating a
  denial record it cannot substantiate), and no record may describe an
  empty grant or empty answer as a denial. Variants with schema-native
  refusal enums (`denied`, `decline`) are established statically
  and are not gated on live proof; live acceptance is still recorded as
  integration evidence.
- **Duplicates and late requests**: a repeated request for an
  already-decided `callId`/`approvalId` is denied again (idempotent deny)
  but mirrored only once per ApprovalID (pump-side dedup). A request
  arriving after the turn reached terminal state is denied and recorded as a
  late-denial diagnostic on the attempt; it never resurrects, reopens, or
  retires the turn. An approval request that cannot be parsed is protocol
  drift (§3.9): child terminated, attempt Uncertain — Council never guesses
  a decision shape.
- The approval **policy** itself comes only from the frozen profile
  (validated against the allowed set: `untrusted`, `on-request`, `never`,
  `granular` objects — `danger-full-access` sandbox and any bypass variant
  are structurally impossible in the launch template). CanonicalProfile
  tooling lists are not an approval authority.
- Frozen-profile invariant: `approvalsReviewer` must be `user` (the
  Council deny-responder is the reviewer). The operator config's
  `auto_review`/`guardian_subagent` modes would answer approvals outside
  Council's visibility; the effective-config check (§3.5) fails closed on
  any other reviewer value.
- Sandbox denials (e.g. `Read-only file system`) and native failures arrive
  as structured `item/*`/`turn/completed{status:failed}` events — recorded
  as structured outcomes, never scraped from terminal text.

### 3.7 Rollout trust model (advisory by default — AC-008 §3.6 adapted)

- **Location**: `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<thread
  -uuid>.jsonl`. The path is NOT derivable from the thread id alone (date
  components); at first materialization the adapter resolves it by a
  bounded scan constrained to the exact UUID suffix, verifies
  `session_meta.session_id == native id`, and records the resolved path +
  baseline `(file-identity, byte size, entry count)` in the attempt record.
  Path validation: no symlinks, contained under the expected sessions root.
- **Advisory by default (fail-closed)**: the rollout is written by the
  native process; denial of every worker tool path to it is NOT established
  by current evidence (workspace sandbox restrictions were observed, but no
  probe suite has proven denial of every path class against an absolute
  path outside the workspace). Started attempts lacking a verified terminal
  record therefore remain **Uncertain until the controller records a
  disposition** — the same rule as AC-008.
- **Attested probe suite — one suite, three typed record classes, two
  consequences.** Read isolation and evidence integrity are different
  properties; one operator-authorized suite (AC-008 §3.6 family; canonical
  framing family unchanged, contributor-scoped) produces ONE attestation
  record containing BOTH classes, every record carrying the tool path, the
  enforcing capability that denied it, and the denied verdict:
  - **`sibling_read`** — attempts to read a SIBLING rollout through every
    enabled tool path (Read-class, Glob-class, Grep-class, shell with
    absolute path, each approved MCP tool class, skill/plugin-contributed
    tools). Binds: **production-dispatch eligibility** (§3.3 — any
    reachable path ⇒ no valid isolation attestation ⇒ authenticated
    dispatch fails closed).
  - **`self_mutation`** — attempts by the authoring worker to modify or
    forge its OWN rollout through every enabled tool path (write, append,
    truncate, rename, delete). Read denial alone never upgrades evidence:
    a worker that could rewrite its own rollout could fabricate the durable
    terminal record the protected class relies on. Binds: the
    **protected-evidence upgrade** below.
  - Consequence binding is strict: eligibility requires all `sibling_read`
    records denied AND the `approval_deny` set complete (below); protected
    evidence requires BOTH probe classes fully denied. Any reachable path
    of either probe class ⇒ evidence stays advisory; any missing
    `approval_deny` verification for a deny-equivalent variant ⇒ that
    variant fails closed on receipt (§3.6).
  - **`approval_deny` records** — per approval request method: which deny
    semantics are established and how. A record exists only for a variant
    whose semantics are established at the stated level; absence of a
    record for a deny-equivalent variant means UNVERIFIED (receipt ⇒
    child terminated, attempt Uncertain, no `tool_denied` — §3.6).
- **Encoding: `cprot-v2` (new; AC-008's `cprot-v1` preserved unchanged).**
  The added record classes and operation-specific mutation evidence cannot
  be expressed in v1's fixed field set, so the Codex attestation uses a
  new versioned canonical encoding — a tagged union:
  - Common framing family: fields encoded `uint32-BE(len) || bytes` unless
    stated as `u8`; booleans one 0/1 byte; enums one `u8`; fixed field
    order per class; UTF-8 everywhere.
  - **Record** = `u8 record_class` || class body:
    - `sibling_read` = 1. Body: `u8 tool_class` {read=1, glob=2, grep=3,
      bash_absolute=4, mcp=5, plugin=6}; `tool_name` (exact native name,
      trimmed, case-preserved); `u8 operation` (MUST be read=1 — any other
      value invalidates the attestation); `u8 enforcing_capability`
      {cwd_boundary=1, sandbox_restricted_fs=2, guardrail_hook=3,
      deny_list=4, permission_denial=5}; `u8 denied`; `denial_text_excerpt`
      (trimmed, internal whitespace collapsed, ≤256 BYTES with the v1
      code-point-boundary truncation rule).
    - `self_mutation` = 2. Body: same fields, but `u8 operation` MUST be
      ∈ {write=2, append=3, truncate=4, rename=5, delete=6}; target is
      implicit (the author's own rollout) and is NOT a field — v1's
      literal `target` field is dropped in v2.
    - `approval_deny` = 3. Body: `method_name` (exact request method);
      `u8 refusal_kind` {native_refusal_enum=1, live_verified_equivalent=2}.
  - **Sorting and duplicates**: sibling_read/self_mutation records sorted
    by (tool_class, operation, tool_name byte-wise UTF-8, case-sensitive);
    approval_deny records sorted by method_name byte-wise. Duplicate keys
    — (class, tool_class, operation, tool_name) or (method_name) — are
    REJECTED (the suite is invalid, not deduplicated). A `sibling_read`
    record with operation ≠ read, or a `self_mutation` record with
    operation = read, invalidates the whole attestation.
  - **Attestation payload** = fixed-field-order frame: `codex_version`,
    `platform` (os + family), `manifest_digest` (toolkit manifest),
    `profile_digest` (cprof-v3 — replaces v1's template_digest), framed
    records, `probed_at` (RFC3339 UTC), `actor`. Digest =
    **`cprot-v2:sha256:<lowercase-hex>`** over the framed payload.
    Identical canonical payloads produce identical digests; `probed_at`
    and `actor` are part of the payload, so a re-run at a different time
    or by a different actor intentionally produces a different digest.
    Golden
    vectors (fixed input → fixed hex, one per record class plus a
    mixed-class payload) are implementation-test deliverables.
  - Storage: the `codex_protection_attestations` table stores ONLY
    cprot-v2 records; Claude's `claude_protection_attestations` keeps
    cprot-v1 untouched. Freeze-at-launch semantics are unchanged.
  - **Coverage binding (errata, implementation review)**: "the suite
    enumerates every enabled class" is ENFORCED, not assumed. The
    expected coverage set is derived from the run's STORED frozen
    profile (the launch policy: platform, version, manifest digest,
    `expected_mcp_servers`, toolkit `expected_plugins`/`expected_skills`)
    and the pinned §3.6 approval table — never from the evidence. A
    valid attestation carries: exactly one `sibling_read` per built-in
    class (Read, Glob, Grep, shell-absolute); `sibling_read` plus all
    five `self_mutation` operations for every mutation-capable path
    (shell, each expected MCP server tool named `<server>` or
    `<server>/<tool>`, each expected plugin tool); no mutation records
    for read-only classes; a `native_refusal_enum` `approval_deny` for
    every pinned method with a schema-native refusal enum; optional
    `live_verified_equivalent` records for the two deny-equivalent
    variants (absent ⇒ fail-closed on receipt, §3.6). Unexpected
    coverage (a server, plugin, class, or method the profile does not
    enable) or duplicate coverage (a class probed through two names)
    invalidates the suite. Enforced at RECORDING (the journal operation
    resolves the run profile and refuses anything less) AND at LOOKUP
    (the eligibility seam and the launch-time protection freeze decode
    the durable frame and refuse an uncovered row): a tuple match that
    does not cover the profile unlocks nothing. The attestation is
    therefore bound to the referenced run's profile by derivation; any
    run with a byte-identical frozen profile shares it by design.
- **Protected-evidence upgrade**: only while a matching attestation —
  both record classes fully denied — is valid for the launch's frozen
  (version, platform, manifest digest) is a launch's rollout upgraded to
  acceptance/absence evidence. The trust ceiling is unchanged even then:
  the rollout is written by the native process outside the worker's
  sandbox, and the attestation proves the worker cannot reach it; it
  classifies acceptance/terminal only and is never re-attributed to a
  different attempt.
- **Entry correlation (protected mode)**: past the recorded baseline, THIS
  turn is accepted only if entries appear in order — (a) a `turn_context`
  entry whose effective-profile fields match the frozen pins, (b) the user
  input `response_item` whose `(native thread id, turn key, attempt id,
  exact input text)` framing equals the attempt's stored `pdig-v1` digest,
  then (c) for terminal claims, the matching `task_complete` (`error`
  absent for completed, populated for failed). Matching is exact-field
  equality after canonical framing; a torn final line is ignored; a
  mid-file parse failure invalidates the read. Advisory mode: baseline
  advancement is diagnostic only.
- **What the rollout adds over Claude (verified)**: `task_complete` carries
  `last_agent_message`, timestamps, `duration_ms`, usage, and `error` for
  failed turns — a durable terminal record exists, so in **protected mode**
  a terminal outcome can be reconstructed for reconciliation (§3.10). Even
  then it authorizes acceptance/terminal classification only; it is never
  re-attributed to a different attempt.
- **Integrity checks**: single-writer per thread assumed only while its
  child is alive and single-flight holds; torn final lines ignored; mid-file
  parse failure invalidates the read; tail-only reads past the recorded
  baseline, size-capped; POSIX ownership/mode checks — Windows: fail-closed
  platform statement (unverified platform, honest gap).
- **Reduced-stdout caveat honored**: exec stdout would hide
  `commandExecution` items; over app-server the `item/*` notifications carry
  them, and the rollout remains the durable structured record for both.

### 3.8 Frozen profile, toolkit evidence, and launch seam

- **Profile encoding**: `cprof-v3` is additive on `cprof-v2` and follows
  the existing implementation shape — per-harness entries live in the
  canonical `harnesses` map (`internal/storage/canonical_profile.go`,
  typed `HarnessProfileSpec`), each entry gaining an optional typed block
  (`omitempty`, so v1/v2 encodings are byte-identical to today):
  ```json
  "harnesses": {
    "codex": {
      "extra_env_allowlist": [],
      "model": "…",
      "native_auth_mode": "inherited_codex_home",
      "codex": {
        "app_server_version": "0.154.0",
        "model_provider": "…",
        "expected_codex_home": "/home/<operator>/.codex",
        "platform": {"os": "linux", "family": "unix"},
        "sandbox_policy": {"type": "workspace-write",
                            "writable_roots": ["<workspace root>"],
                            "network_access": false},
        "approval_policy": "on-request",
        "approvals_reviewer": "user",
        "expected_mcp_servers": ["…"],
        "expected_instruction_sources": ["…"],
        "rules_evidence": {
          "verified": ["…"],
          "unverifiable": ["~/.codex/rules/*.rules contents",
                            "hooks execution evidence at the Codex layer"]
        },
        "event_universe_path":
          "docs/superpowers/evidence/ac009-native-event-universe-0.154.0.json",
        "event_universe_digest": "sha256:<hex>"
      }
    }
  }
  ```
  Every value the design freezes and compares appears in the block:
  app-server version (probe/initialize pin), `model_provider` (resume
  effective-config compare), `expected_codex_home` + platform os/family
  (initialize attestation compare), sandbox policy with writable roots and
  network access (resume + `turn_context` compare), `approval_policy` and
  `approvals_reviewer` (resume effective-config + `turn_context` compare;
  §3.6 responder), and the explicit unverifiable rules/hooks list —
  recorded as a gap, never claimed.
  - **Normalization**: the existing family — BOM trim, NFC, dedupe,
    byte-wise lexicographic sort for array fields (case-sensitive; the
    lowercasing quirk of `tooling` is NOT applied here); scalars trimmed +
    NFC; `writable_roots` path-normalized (`ToSlash`/`Clean`, no trailing
    slash); `network_access` boolean. Enum validation at freeze:
    `approval_policy` ∈ {`untrusted`, `on-request`, `never`, `granular`
    object} (no bypass value exists in the native enum; anything else is a
    validation error); `approvals_reviewer` MUST be `user` — the §3.6
    invariant (any other native value, e.g. `auto_review`, would answer
    approvals outside Council's visibility) is enforced at profile freeze,
    not at runtime.
  - **Granular approval policy — tagged union, not string normalization**.
    `approval_policy` is encoded as a tagged union on the policy kind:
    - String kind: the canonical JSON string (trimmed, NFC), validated
      against the fixed enum {`untrusted`, `on-request`, `never`}.
    - `granular` kind: a canonical JSON object with EXACTLY the five keys
      observed in the installed schema — `mcp_elicitations`, `rules`,
      `sandbox_approval`, `request_permissions`, `skill_approval` —
      unknown keys rejected at parse (`DisallowUnknownFields` on the typed
      struct); each key's value is re-encoded verbatim as canonical JSON
      (sorted keys, no insignificant whitespace). Equality of two granular
      policies = byte equality of their canonical encodings; NO semantic
      interpretation or folding of sub-values.
    - Sub-value shape validation happens ONLY against the committed
      schema-derived evidence for the pinned installed version (the
      `generate-json-schema` captures, committed with digest under
      `docs/superpowers/evidence/`). If a sub-value's shape is not pinned
      by that evidence, profile freeze REJECTS the granular policy entirely
      (fail-closed honest gap — string policies remain fully usable).
    - Effective-config comparison (§3.5) compares canonical encodings:
      any byte difference between the frozen granular policy and the
      effective policy reported by the native side is drift.
    Digest prefix
    **`cprof-v3:sha256:<lowercase-hex>`** over the canonical JSON encoding
    (sorted keys, no insignificant whitespace, same encoder as v1/v2).
  - **Unknown-field behavior**: parsing keeps `DisallowUnknownFields`; the
    `codex` block decodes into the typed `CodexHarnessSpec` struct. A
    `codex` block under any algo_version other than `cprof-v3` is a
    validation error (mirrors `toolkit_manifest requires cprof-v2`);
    `cprof-v3` requires `toolkit_manifest` (inherits the v2 rule).
  - **Compatibility matrix**:
    | Adapter | cprof-v1 | cprof-v2 | cprof-v3 |
    |---|---|---|---|
    | OpenCode (AC-007) | accepted | accepted | **accepted, ignored** — executor validity gate widens v1\|v2 → v1\|v2\|v3; manifest and codex block ignored, no behavior change |
    | Claude (AC-008) | rejected for production (unchanged) | required + accepted (unchanged) | **accepted**: purely additive — `RequireToolkitManifest` widens exactly-v2 → v2\|v3, manifest still required, codex block ignored |
    | Codex (AC-009) | rejected | rejected | **required**: complete `harnesses.codex` block, typed `ErrUnsupportedProfile` otherwise |
    Rationale: ONE run profile serves all four harnesses; a run including
    Codex is frozen as v3, and Claude/OpenCode contributors on that same
    profile remain valid. No record-only mode; no backfill of old runs.
  - The event-universe file is re-hashed at validation and must match
    (committed 0.154.0 universe is the base evidence; a different installed
    version requires an operator-authorized refresh probe before freeze).
- **Pre-dispatch toolkit verification (provider-free)**: at binding
  verification (§3.5) the effective-config comparison covers model, provider,
  sandbox, approval policy, reviewer, and `instructionSources`; MCP server
  inventory is checked via `mcpServerStatus/list` (schema-verified method;
  live exercise is an integration-evidence obligation). Drift ⇒
  `ErrProfileDrift` before transmission — the prompt is never governed by a
  policy that was not verified first. What Council **cannot** verify
  natively (rules-file contents loaded, hook execution evidence at the
  Codex layer) is recorded as an explicit gap in the manifest, not papered
  over.
- **Trust precondition**: the workspace root must be a trusted directory in
  the operator's Codex config (`[projects."<path>"]`), provisioned by the
  operator as part of run approval. The deterministic
  `Not inside a trusted directory …` failure is surfaced as a typed
  pre-acceptance rejection. Council never writes Codex config
  (`config/value/write` is forbidden to the adapter).
- **Probe template** (operator-owned, provider-free, scratch cwd):
  `codex --version`; `app-server daemon version` (version-skew surface);
  `initialize` handshake on a probe child + negative `thread/resume`
  (canonical zero UUID → verbatim `no rollout found … (code -32600)`
  asserts the deterministic missing-thread contract); `model/list`
  (credential-free inventory); `codex mcp list`; `codex login status`
  (auth evidence without secrets). Errata (planning): the MCP inventory
  probe surface is `mcpServerStatus/list` via the probe child (per §3.8's
  toolkit-verification bullet); the `codex mcp list` CLI form is an
  optional operator capture, not an adapter launch.
  `generate-json-schema` captures are
  research evidence, not a run-time dependency.

### 3.9 Stream, protocol, and failure semantics

| Condition | Classification |
|---|---|
| Malformed/oversized JSON-RPC frame mid-stream | stream poisoned → Uncertain; child terminated |
| Unknown method/error shape (protocol drift) | poisoned → Uncertain (never best-effort parse) |
| Id drift (thread/turn id ≠ binding/registry) | poisoned → Uncertain; child terminated |
| Definitive error response to `turn/start` | DispatchRejected (native refusal, pre-acceptance) |
| Response lost / child death after request written | DispatchUnknown → Uncertain until resolved |
| `turn/completed{status:"failed"}` (or rollout `task_complete.error` in protected mode) | terminal, `TurnFailed` |
| `turn/interrupt` request accepted (JSON-RPC result ok), terminal NOT yet verified | `CancelRequested` — the turn may still run; the slot is NOT released |
| Verified `interrupted` terminal evidence (turn-status notification; protected-mode rollout record) | terminal, `TurnCancelled` — `CancelConfirmed` only now |
| Deadline exceeded after interrupt | graceful child terminate → kill; no terminal ⇒ Uncertain |
| Cancel while terminal/absent | `CancelAlreadyTerminal` / `CancelUnknown` per verified evidence |
| Effective turn policy post-launch (protected-mode `turn_context`) mismatches frozen pins | Uncertain; drift recorded (advisory mode: diagnostic only) |
| Production-eligibility attestation missing/invalid at launch | typed `ErrProductionEligibilityMissing` — pre-acceptance rejection, **no process started** (§3.3 ordering step 1) |
| `account/read` unauthenticated or inconclusive (post child start) | child terminated; typed auth rejection — **no thread or turn created**; unknown/inconclusive fails closed (§3.3 ordering step 2) |
| Approval request received for a deny-equivalent variant with no live-verified `approval_deny` record in the governing attestation | child terminated; attempt Uncertain; no deny payload sent, **no `tool_denied` emitted** (§3.6) |
| Observer ctx cancellation | tap detach only; notifications keep flowing into the pump |
| Child start failure (executor proves no process) | Rejected — DefinitivelyMissing evidence |
| Child dies mid-turn (in-flight turn) | Uncertain; replacement child + `thread/resume` for the next turn; rollout advisory |
| Pre-dispatch effective-config / toolkit / reviewer mismatch | `ErrProfileDrift` — typed pre-acceptance rejection |
| `initialize` attestation mismatch (codexHome/platform) | child terminated, pre-session typed failure |

Any started turn without exactly one verified terminal
(`turn/completed`/`turn/failed` in-life, or protected-mode rollout
`task_complete`) remains Uncertain. Usage snapshots
(`thread/tokenUsage/updated`) are cumulative and deduplicated by monotonic
order; cost is reported `Available=false`.

### 3.10 Reconciliation evidence rules

Correlate the exact turn (baseline + prompt digest + in-life turnId), never
the bare thread:

| Evidence | Verdict for the LOST attempt |
|---|---|
| Verified `turn/completed`/`failed` observed in-life | terminal (durable outcome committed) |
| Protected-mode rollout `task_complete` matching this attempt's baseline + prompt digest, attestation valid at launch | terminal (acceptance + outcome) |
| Protected-mode verified absence (baseline unchanged, child known dead) | authorizes exactly ONE same-attempt redispatch — nothing was accepted |
| Anything else — child unreachable, ambiguous idle, advisory-mode rollout signals, `thread/resume` liveness | **Uncertain, permanently** until the controller records a disposition |

`thread/resume {threadId}` liveness (provider-free) establishes **session**
reachability (recorded as diagnostic), never turn-level acceptance in
advisory mode. Replacement work is always a new attempt; unresolved
attempts block the native session across restarts (durable state, not
process memory).

### 3.11 Durable state schema (storage migrations v5 + v6; AC-008 §3.11 adapted)

```
codex_session_bindings
  session_id          PK/FK (logical session)
  native_id           TEXT (UUIDv7, unique, validated on reuse)
  materialized        BOOL   -- TRUE only after rollout observed + baseline taken
  model, workspace    TEXT
  rollout_path        TEXT NULL  -- observed at materialization; UUID-suffix verified
  profile_digest      TEXT       -- frozen cprof-v3 digest
  first_prompt_digest TEXT NULL  -- set at first acceptance

codex_turn_attempts
  attempt_id, session_id, turn_key
  UNIQUE(session_id, turn_key, attempt_id)
  transition_version  INTEGER
  native_turn_id      TEXT NULL  -- bound in-life from turn/started
  baseline_file_identity TEXT, baseline_size INTEGER, baseline_entries INTEGER
  materialized_baseline BOOL
  rollout_protection  TEXT (protected|advisory|unverified)
  protection_attestation_id TEXT NULL
  prompt_digest       TEXT
  launch_count        INTEGER (0..2)
  absence_redispatch_consumed BOOL
  accepted            BOOL NULL
  terminal            BOOL
  result_payload      TEXT NULL
  result_usage        TEXT NULL
  observed_status     TEXT (completed|failed|missing|uncertain)
  uncertainty_disposition TEXT NULL  -- controller-only, journal entry
  updated_at

codex_attempt_launches   -- one row per launch RESERVATION (child+turn/start)
  attempt_id FK, reservation_seq INTEGER, UNIQUE(attempt_id, reservation_seq)
  state TEXT (reserved|started|start_failed|dead)
  started_at, start_failed_at, first_stdin_byte_at, known_dead_at TIMESTAMP NULL
  exit_code INTEGER NULL
  child_generation INTEGER    -- which app-server child instance (restarts on park)
  executor_identity TEXT

codex_protection_attestations
  -- cprot-v2 encoding ONLY (tagged-union records; §3.7): fixed field
  -- order, per-class bodies, sorting, duplicate rejection, golden
  -- vectors; AC-008's claude table keeps cprot-v1 untouched. Record
  -- classes: sibling_read[] + self_mutation[] + approval_deny[];
  -- sibling_read + approval_deny bind production-dispatch eligibility
  -- (§3.3), BOTH probe classes bind protected evidence (§3.7); a later
  -- attestation never reclassifies earlier attempts; the record set
  -- must COVER the run's frozen profile (§3.7 coverage binding) — the
  -- journal operation and every lookup enforce it

codex_creation_uncertainties   -- migration v6 (errata, §3.4 episodes)
  session_id, episode INTEGER (>= 1), PRIMARY KEY (session_id, episode)
  run_id, reason, recorded_by, record_op_id, cause_op_id, recorded_at
  disposition TEXT NULL (abandon_orphan|verified_absent)  -- NULL = open
  resolution_reason, resolution_generation, resolution_op_id, resolved_at
  UNIQUE partial index on session_id WHERE disposition IS NULL
  -- at most ONE open episode per session; resolution is a controller-
  -- authorized journal operation targeting one exact episode
```

Crash-safe ordering mirrors AC-008 §3.11 exactly: attempt+baseline durable
before launch; launch reserved (`launch_count` incremented, row
`state=reserved`) in the same transaction BEFORE the request is written;
`first_stdin_byte_at` at the write boundary (first-byte-wins);
`start_failed` releases its slot and RETAINS its row as evidence; redispatch
reservation is atomic with its preconditions (attestation, verified absence,
cap of two started launches); terminal only from a verified terminal;
`uncertainty_disposition` only by explicit controller journal operation.

Template materialization is NOT required (no per-session config root,
§3.3); the config-root digest slots are replaced by `profile_digest` and the
attestation's manifest digest.

## 4. Evidence plan (fixtures vs integration)

- **Fixture layer (CI, provider-free)**: a fake `codex` binary via the
  PolicyExecutor speaking recorded JSON-RPC fixtures: initialize handshake
  (incl. attestation mismatch), `thread/start` success + response loss,
  `thread/resume` positive/negative/drift — including the first-turn
  ordering (resume verification BEFORE the first `turn/start`),
  `turn/start` reject/accept, notification flows (items, token usage,
  completed/failed), approval request → deny mirroring for every variant
  (exact payload, duplicate re-deny with single mirror, late-denial
  diagnostic, unparseable ⇒ Uncertain), `turn/interrupt`
  (requested → confirmed only on verified interrupted terminal),
  malformed/oversized/duplicate frames, child death mid-turn,
  unrouted-notification parking, rollout fixtures for the trust model
  (materialization, baselines, torn lines, `turn_context` drift,
  `task_complete`/error shapes, protected-mode acceptance and
  verified-absence), `pdig-v1` golden vectors, `cprot-v2` golden vectors
  (one per record class + mixed-class payload), creation-reservation routing
  (notification-before-response, response id mismatch, orphan
  `thread/started` ⇒ drift), the enforceable gate ordering (attestation
  rejection pre-child; auth rejection post-child with no thread or turn
  created), receipt of an unverified deny-equivalent variant ⇒ Uncertain
  with no `tool_denied` emitted, the `codextest` construction guard
  (production construction fails closed when handed the test-only option;
  production packages do not import `codextest`), and
  production-eligibility fail-closed without a valid
  attestation. Full AC-006 conformance + council-boundary suites run
  against the adapter over the fake.
- **Integration layer (manual, sanitized, operator-invoked, never CI)**:
  `scripts/ac009-integration-evidence.sh` — the obligations that close the
  schema-only gaps: live turn over app-server end-to-end; the approval
  deny-path exercised live for EVERY approval variant the installed version
  emits (deny payload accepted natively, denial recorded structurally);
  `turn/interrupt` live; child kill-and-resume without duplicate execution;
  resume-drift verification (effective-config check firing); post-launch
  `turn_context` policy confirmation; the probe suite producing the first
  attestation record with all three record classes — `sibling_read`,
  `self_mutation`, and `approval_deny` — recorded separately (§3.3/§3.7);
  live deny acceptance for EVERY approval variant per the §3.6 table,
  including the two non-obvious ones (empty-grant `permissions` denial and
  empty-answers `requestUserInput` behavior — their live verification is
  what produces the `approval_deny` records that unlock their use); rollout
  terminal-record capture. Redacted outputs recorded in
  PR evidence with exact commands. Distinct from the AC-008 real-install
  follow-up, which remains its own evidence task.

## 5. Boundaries

- Never `--yolo`, `--dangerously-bypass-approvals-and-sandbox`,
  `--dangerously-bypass-hook-trust`, `--full-auto`, `--approve-for-me`,
  `danger-full-access` sandbox, `--ephemeral`,
  `--ignore-user-config`/`--ignore-rules`, any `resume --last`/picker form,
  `exec fork`, the shared daemon, or `ws://`/`unix://` listeners.
- Never read, copy, relocate, or symlink `auth.json` or any credential;
  `codex login status` and `account/read`-shaped evidence only.
- Never write Codex config (`config/value/write` forbidden); trust
  preconditions are operator-provisioned.
- Every launch through the AC-005 executor with the frozen argv template;
  thread cwd pinned to the AC-005 workspace root; no adapter-synthesized
  policy inputs — model/sandbox/approval/reviewer come only from the frozen
  profile and are verified against effective config before transmission.
- Approvals: deny every request; denials recorded as structured events.
- Rollout is advisory by default; even protected, it classifies
  acceptance/terminal, never authorizes re-attribution.
- Cost is `unavailable`; token usage is reported only as observed.
- `[experimental]` transport + 0.154.0-pinned evidence: version drift
  requires a new probe and profile refresh before production use; unknown
  protocol shapes fail closed.

## 6. Acceptance mapping (issue #9)

| Acceptance criterion | Where |
|---|---|
| Fresh thread for independent proposal; exact thread resumption | §3.4 (`thread/start`, provider-free, native UUIDv7 binding), §3.5 (pinned `turn/start` on `thread/resume`-verified bindings); fixture + integration evidence |
| Preserve configured sandbox/guardrails; no yolo/bypass | §3.8 frozen policy pins + pre-transmission verification; §3.3 production-eligibility gate; §5 forbidden set; argv template validation |
| Permission requests and tool outcomes as structured state | §3.6 deny-responder with `ApprovalID` correlation; §2.1/§3.9 `item/*` + `turn/*` mapping; approval-path fixture + live deny-path evidence |
| Interruption and reconnection without duplicate execution | §3.5 single-flight + ack semantics; §3.9 interrupt/child-death table; §3.10 one-redispatch rule; §3.11 crash-safe ordering; scenarios 1–4 |
| Shared-toolkit verification + honest usage/coverage reporting | §3.8 effective-config + MCP inventory vs manifest; §3.3 home-bound honesty; cost `unavailable`; §4 gap closure |

### 6.1 Concrete acceptance scenarios

1. **Concurrent duplicate dispatch** — N callers, one TurnRef: one
   in-flight turn, shared verdict, single-flight per native thread.
2. **Crash after turn/start written, response lost** — DispatchUnknown;
   protected-mode verified absence may authorize one same-attempt
   redispatch; otherwise Uncertain until the controller disposes; block
   persists across restarts.
3. **Child death mid-turn** — child restarts on next authorized turn;
   `thread/resume` verifies the thread provider-free; the lost turn's
   outcome follows §3.10 (advisory ⇒ Uncertain-until-disposed; protected ⇒
   rollout terminal or one redispatch).
4. **Interactive controller disconnect** — observer taps detach; the child
   and its in-flight turn continue; bounded work finishes and waits;
   reconnection re-attaches via the event pump/Collect.
5. **Exact-identity discipline** — resume of a missing id raises typed
   `ErrNativeSessionMissing`; any id drift poisons the stream; non-UUID
   inputs are never transmitted (canonical UUIDs only).
6. **Approval-required tool** — frozen policy yields a server approval
   request; responder denies; `tool_denied` recorded; turn proceeds to a
   bounded terminal outcome; no hang.
7. **Config drift between turns** — effective-config mismatch
   (model/sandbox/approval/reviewer) fails closed as `ErrProfileDrift`
   before any prompt is transmitted.
8. **Toolkit drift** — unknown MCP server or instruction-source mismatch ⇒
   `ErrProfileDrift` pre-transmission; missing native verification
   (rules/hooks) is recorded as an explicit evidence gap.
9. **Truncated rollout** — torn final line ignored; acceptance evidence
   still parses; no terminal claim beyond evidence class.
10. **Concurrent creation failure sharing** — matching waiters receive the
    creator's typed failure; no orphan binding; the abandoned native thread
    (if any) is operator-disposed, never auto-reused.
11. **Unattested authenticated dispatch** — authenticated production
    dispatch with no valid isolation attestation (sibling_read class not
    fully denied, or invalidated by version/platform/manifest drift): typed
    `ErrProductionEligibilityMissing` before any child starts; fixtures and
    provider-free evidence paths unaffected.
12. **Approval-request edge cases** — duplicate request: idempotent re-deny,
    single mirrored event; late request after terminal: deny + diagnostic,
    no state change; unparseable request: child terminated, Uncertain;
    `permissions`/`requestUserInput` before live attestation: child
    terminated, Uncertain, **no `tool_denied` emitted**; after attestation:
    empty grant / empty answers per the §3.6 table, never
    `strictAutoReview`.
