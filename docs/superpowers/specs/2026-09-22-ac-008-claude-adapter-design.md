# AC-008 Design — Claude persistent contributor adapter

Status: DRAFT v4 for review
Date: 2026-09-22
Issue: #8
Depends on: AC-003 (controller grants), AC-005 (workspaces/execution policy),
AC-006 (adapter contract)

## 1. Problem and scope

Council needs Claude as a persistent contributor: an adapter that drives the
installed Claude Code CLI headlessly, preserves the operator's shared
toolkit (hooks, skills, plugins, guardrails) inside a defined isolation
boundary, maintains an exact native conversation identity across turns,
captures structured outcomes (completions, denials, approval-needs),
bounds invocations, cancels them, and reconciles uncertain completions
before any retry.

Out of scope: Codex/Agy adapters (AC-009+), live-provider acceptance runs
(operator-invoked only), any startup mode that silently omits hooks and
skills, and OS-level user isolation (owned by the AC-005 executor story).

## 2. Installed-interface research (verified Claude Code 2.1.278)

All findings probed against the installed binary (`claude --version` →
`2.1.278 (Claude Code)`).

### 2.1 Verified invocation surface

| CLI surface | Verified behavior |
|---|---|
| `claude -p/--print` | Non-interactive one-shot turn; **prompt is read from stdin when not given as an argument** (verified live: piped prompt → `result success`) |
| `--output-format stream-json` (+ required `--verbose` in print mode) | Newline-delimited JSON event stream on stdout |
| `--session-id <uuid>` | Caller-chosen session identity; **must be a valid UUID** (`not-a-uuid` → `Invalid session ID. Must be a valid UUID.`) |
| `--resume <session-id>` | Exact resumption, verified to continue under the SAME session UUID |
| `--resume <missing-id>` | Deterministic failure: `No conversation found with session ID: <id>` |
| `--session-id` + `--resume` without `--fork-session` | Rejected — plain `--resume` preserves identity; `--fork-session` never used by Council |
| `-c/--continue` | Most-recent conversation — **forbidden** (exact-session rule) |
| `--model <model>` | Model preset (verified `haiku` → `claude-haiku-4-5-20251001`) |
| `--max-turns <n>` | Bound on agentic turns; exceeded → `result` `subtype:"error_max_turns"`, `is_error:true` (verified) |
| `--permission-mode` | Choices: `acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan` — Council **omits the flag** (native default); `bypassPermissions`/`auto` **forbidden** |
| `--disallowedTools <tools...>` | **Verified denial mechanism**: a listed tool is unavailable for the session — `tool_result` `is_error:true`, "No such tool available: Bash. Bash is disabled for this session, in subagents as well as here" |
| `--allowedTools <tools...>` | Allow-hint; **exclusivity NOT verified** — native default mode executes some tools without any list (verified). Council never relies on allowlist exclusivity (§3.8) |
| `--dangerously-skip-permissions`, `--allow-dangerously-skip-permissions`, `--bare`, `--no-session-persistence` | **All forbidden** (guardrail bypass / toolkit omission / lost persistence) |
| `CLAUDE_CONFIG_DIR=<dir>` (env) | **Verified**: relocates the entire config/state root — transcript landed under `<dir>/projects/<munged-cwd>/<uuid>.jsonl` |

### 2.2 Verified stream-json event surface (live probes)

- `system/init`: `session_id`, `tools[]`, `mcp_servers[]`, `model`,
  `permissionMode`, `skills`, `plugins`, `claude_code_version`, `cwd`,
  `apiKeySource`. **Toolkit-loading evidence channel**: the operator's
  SessionStart hooks fire (`system/hook_started`/`hook_response` observed)
  and loaded skills/plugins/MCP servers are enumerated.
- `assistant`: message envelope with `content[]` (`text`/`tool_use`),
  `usage`, `session_id`.
- `user`: `content[].tool_result` with `is_error` — **structured denial
  channel** (distinct verified denial texts, §2.4).
- `result` (terminal, stdout-only): `subtype` (`success` /
  `error_max_turns`), `is_error`, `result` text, `total_cost_usd`,
  `usage`, `duration_api_ms`, `session_id`.

### 2.3 Verified headless permission behavior (decisive probes)

| Scenario (all under `-p`, native default mode) | Verified outcome |
|---|---|
| Safe tool (Bash `echo`) | Executes: `tool_result` `is_error:false`; result `success` |
| **`manual`** mode, safe tool | **Also executes without prompting** — Council does not rely on `manual` |
| Would-prompt tool (Write outside the workspace) | **Immediate structured denial, no blocking**: `tool_result` `is_error:true`, "Claude requested permissions to write to …, but you haven't granted it yet"; run ends `error_max_turns` |
| Deny-listed tool (`--disallowedTools`) | `tool_result` `is_error:true`, "No such tool available: … disabled for this session, in subagents as well as here" |
| Guardrail PreToolUse hook denial | `tool_result` `is_error:true`, "PreToolUse:Glob hook error: [… guardrail]: Guardrail denied this action …" |
| Working-directory boundary | Native denial: "may only list files in the allowed working directories for this session: '<workspace>'" |
| Model refuses without a tool call | `result` `success` with declining text — a completed turn, never a fabricated denial |

**Consequence**: in headless mode an approval requirement is surfaced as an
immediate structured denial, never as a wait. Every invocation is bounded
by `--max-turns` and a process deadline; no invocation can wait
indefinitely.

### 2.4 Verified behaviors and hazards

- **Session store**: `<config-dir>/projects/<munged-cwd>/<session-id>.jsonl`;
  project-scoped by cwd — resumption must run with the AC-005 workspace
  root pinned as cwd.
- **No durable terminal record in JSONL** (verified by full-file
  inspection): entry types are `queue-operation`, `attachment`, `user`,
  `atis-latch`, `last-prompt`, `assistant`, `cost-state`, `mode`. There is
  **no `result`-equivalent entry** — the terminal `result` event is
  stdout-only. An `assistant` entry may be a mid-work tool request.
- **Model-level refusal vs structured denial**: a model may decline without
  any tool call (`result` `success` with declining text) — completed turn,
  honest output.
- **Native authentication**: operator-provisioned; Council never copies or
  inspects credentials. `system/init.apiKeySource` is observable without
  reading secrets.

## 3. Architecture

### 3.1 Process-per-turn, stdin-only transport

Claude turns are **one CLI process per dispatched turn**:
`claude -p … --resume <native-id>` (first turn: `--session-id <native-id>`)
with the prompt on **stdin** and stream-json on stdout.

- The prompt is NEVER an argv element (process-list exposure). It is
  written to stdin by the adapter (verified transport). The ambiguity
  boundary is the AC-007 rule adapted to stdin: the **first successfully
  transmitted stdin byte** marks transmission begun — a transport failure
  after that is DispatchUnknown, never rejected.
- Every process launches through the AC-005 `PolicyExecutor` (never
  `os/exec`) with the session's workspace root pinned as cwd.
- Council generates the native session UUID (`crypto/rand` UUIDv4, RFC
  4122 bits, validated on reuse) once per logical session inside the
  creation reservation.
- Subsequent turns resume with `--resume <native-id>` (never `-c`).

### 3.2 Isolation: per-session config roots under a disjoint base (finding 1)

- **`claude_config_base_dir`**: a dedicated operator-provisioned base
  directory, **required and validated to be disjoint from `state_dir` and
  `workspace_base_dir`** (same symlink-safe resolved-path containment
  validation as the AC-007 probe scratch root, including the
  pre-creation/post-creation recheck). A Claude process that escapes its
  per-session root therefore finds service SQLite state and controller
  material no closer than any other same-user file — and never inside the
  service store.
- **Per-session roots**: `<claude_config_base_dir>/<run-id>/<session-id>/`
  created at materialization by copying the operator-provisioned template
  (approved settings, hooks, skills, plugins manifests — never
  credentials). `CLAUDE_CONFIG_DIR` points at the session root
  (relocation verified). A session's transcript lives in its own root;
  sibling transcripts are physically absent from it.
- **Native login**: verified to function with a relocated config root
  (keychain access is OS-user scoped); implementation records
  `system/init.apiKeySource` under the per-session root as provisioning
  evidence and fails closed (first-dispatch uncertainty, §3.9) if
  authentication is unreachable. CreateSession itself performs no native
  call.
- **Threat boundary, stated honestly**: workers run as the same OS user as
  the service. A worker that learns another session's root path could
  read it — per-session random UUID path components (122 bits), executor
  workspace policy, and guardrail hooks are defense in depth, not a hard
  boundary. OS-level worker isolation is an AC-005 executor concern, not
  an adapter claim.
- **Template drift**: the template digest is frozen into the run profile
  (§3.9); a copied root whose digest differs fails the toolkit evidence
  check.

### 3.3 Session creation, idempotency, and binding

- `CreateSession` validates the frozen config, generates the UUIDv4 native
  ID once (inside the per-logical-session creation reservation), creates
  the per-session config root (atomic template materialization, §3.9), and
  returns the binding with `materialized=false`. **The adapter does not
  persist; the service/storage layer persists the returned binding.**
- **Unmaterialized binding**: until the bound transcript is observed to
  exist (first accepted dispatch), the binding is `materialized=false`.
  `ResumeSession` on an unmaterialized binding inspects locally and
  returns success — never classified as lost.
- Concurrent `CreateSession`: creation reservation (one native identity,
  shared result including typed failures, mismatched callers fail closed).

### 3.4 ResumeSession — local inspection, native verification deferred

A `--resume` invocation REQUIRES a prompt and starts a provider turn
(verified); there is no provider-free native resume probe.

- `ResumeSession` performs **validated local inspection** only:
  1. binding present and well-formed (UUIDv4 native ID, model, workspace);
  2. per-session config root present with frozen-template digest match;
  3. for a materialized binding, the transcript (§3.6) exists, passes
     integrity checks, and contains the accepted `user` entry matching the
     attempt record's prompt digest — proof THIS transcript is THIS
     session's;
  4. unmaterialized bindings: steps 1–2 only.
- Native verification is **deferred to the next authorized dispatch**: a
  resume launch either starts cleanly or fails with the deterministic
  missing-session error (`ErrNativeSessionMissing`, typed and distinct).
  Resume itself makes no native call and consumes no quota.

### 3.5 Durable dispatch correlation

- **Pre-launch baseline**: before starting the process, the attempt record
  stores the transcript baseline — for a materialized session,
  `(file-identity, byte size, entry count)` as typed columns; for a first
  turn, `materialized=false`.
- **Acceptance boundary**: accepted when the transcript advances past the
  baseline with a new `user` entry whose prompt digest matches the
  attempt's stored prompt digest.
- **No-redispatch rule**: once a process has been STARTED for an attempt,
  no redispatch of that attempt occurs. If acceptance cannot be proven
  and no result was observed, the attempt remains **Uncertain until the
  controller explicitly records an uncertainty disposition** — and this
  holds across daemon restarts (the slot/blocking state is derived from
  durable attempt state, not process memory). Any replacement
  work runs as a NEW attempt on the session. Reconciliation cannot
  resolve a lost started attempt (§3.10); it can only gather evidence
  for the disposition decision.
- **Blocking semantics**: an unresolved (uncertain, undisposed) attempt
  blocks every subsequent turn on the same native session — including
  after restart — until the controller records the disposition.

### 3.6 Transcript (JSONL) trust model (findings 2, 5)

- **Location**: `<session-config-root>/projects/<munged-cwd>/<native-id>.jsonl`,
  derived only from the binding's own fields; never accepted from the
  native side or callers. The native ID is validated UUIDv4.
- **Advisory by default (fail-closed)**: the PolicyExecutor governs the
  Claude process launch but does not mediate Claude's internal tool calls.
  Denial of every tool path from the worker to the transcript is **not
  established** by current evidence: the verified denials cover the
  working-directory boundary, guardrail PreToolUse hooks, deny-lists, and
  would-prompt approvals — but no probe has demonstrated denial of
  Read/Glob/Grep, Bash-with-absolute-path, or an approved MCP/plugin tool
  against a transcript outside the workspace. Therefore, by default, the
  transcript is **advisory**: it informs diagnostics and session liveness,
  and a started attempt lacking an observed `result` remains **Uncertain
  permanently** until the controller records an uncertainty disposition.
- **Protected-evidence upgrade (operator-authorized)**: the operator may
  run the defined probe suite — attempts to read a SIBLING transcript via
  (a) Read, (b) Glob, (c) Grep, (d) Bash with an absolute path, (e) each
  approved MCP/plugin tool class — and record the denials with the
  enforcing capability named per path (cwd boundary, guardrail hook,
  deny-list). Only if EVERY enabled tool path is proven denied on the
  installed version does the transcript become **protected evidence**,
  authoritative for acceptance/absence decisions (redispatch safety).
  Any reachable path ⇒ advisory forever for that version. The probe suite
  is an operator-authorized action (it deliberately points a worker at a
  sibling history) and is re-run on every native version bump.
- **Integrity checks (both modes)**: derived path with no symlinks;
  POSIX ownership/mode 0600 checked — **Windows: fail-closed platform
  statement** — the POSIX checks are not expressible, so the transcript is
  `integrity=unverified` and every decision relying on it degrades to
  Uncertain (portable decision logic stays covered by fixtures); torn
  final lines ignored; mid-file parse failure invalidates the read;
  single-writer by construction (slot); tail-only reads past the recorded
  baseline, size-capped; entries matched by exact verified `type` fields.
- **Trust ceiling**: even in protected mode the JSONL is written by the
  native process, not by Council — it authorizes acceptance/absence only.
  It NEVER proves terminal completion: the terminal `result` event is
  stdout-only (verified).

### 3.7 Launch seam

- **`ClaudeTurnLaunchSource`** (service-owned, storage-backed deep module):
  builds the complete `LaunchRequest` from the frozen run profile and the
  AC-005 workspace allocation — command, exact arguments (below), cwd =
  workspace root, `CLAUDE_CONFIG_DIR`, `--max-turns` from the frozen
  bound, model from `HarnessProfileSpec`, tool policy from §3.8. The
  adapter never synthesizes policy inputs.
- **Argument contract (exact)**:
  `["-p", "--output-format", "stream-json", "--verbose",
  ("--session-id"|"--resume"), <native-uuid>, "--model", <model>,
  "--max-turns", <bound>, ("--allowedTools", <approved...>)?,
  ("--disallowedTools", <deny-complement...>)]`
  — prompt on stdin, never argv. The executor validates the list against
  this template: only the parameterized slots vary; forbidden flags
  (`--bare`, `--continue`, `--fork-session`, `--no-session-persistence`,
  both bypass-permission flags, any `--permission-mode` value,
  `--append-system-prompt*`, `--system-prompt*`) and reordered,
  duplicated, or extra arguments are rejected before launch.
- **Typed config-dir extension**: `LaunchRequest.ClaudeConfigDir` (single
  string, must resolve inside `claude_config_base_dir`) — validated ONLY
  for the exact Claude launch shape, mirroring `GeneratedServerEnv`; never
  an inherited-environment allowlist entry; excluded from event payloads.
- **Probe launches**: a separate operator-owned `ClaudeProbeLaunchTemplate`
  in an operator-allocated isolated scratch directory (disjoint from
  state, workspace, and config base — same validation family). The probe
  verifies the **installed argument surface**: `--version` output AND
  `--help` contract assertions (required flags exist;
  `--permission-mode`/`--output-format` choice sets match the verified
  surface; forbidden flags absent from the approved template). Provider-
  free.

### 3.8 Frozen tooling enforcement (finding 3)

- **Deny set = verified native tool universe − approved set.** The
  universe is the native tool list captured by `system/init.tools[]` at
  probe/first-session time and versioned with the probed CLI version.
  The adapter computes the complement against the frozen approved set and
  passes the **complete complement through `--disallowedTools`** — the
  verified denial mechanism — never relying on `--allowedTools`
  exclusivity.
- **Unknown native tools** (present in `system/init` but absent from the
  captured universe): the process is terminated and the turn classified
  Uncertain (§3.9 init rule) — protocol drift is never absorbed silently.
- **Live omitted-tool probe required**: before the enforcement claim is
  marked verified, an implementation-phase probe must dispatch a prompt
  requiring an omitted tool under the computed complement and record the
  structured denial. Until then the enforcement row is evidence-pending,
  and the guardrail PreToolUse hooks remain the operating second layer.
- **Manifest**: approved tools, denied complement, expected hooks, skills,
  plugins — the frozen toolkit manifest, recorded in the run profile and
  covered by the profile digest. Per-session toolkit evidence =
  `system/init` compared against the manifest (`cwd`, version, model,
  permission mode, manifest presence).

### 3.9 Stream, process, and init failure semantics (finding 4)

| Condition | Classification |
|---|---|
| Malformed NDJSON line mid-stream (not final) | stream poisoned → Uncertain; process terminated |
| Oversized NDJSON line (> bound) | stream poisoned → Uncertain; process terminated |
| Multiple `result` events | poisoned → Uncertain (protocol drift; never first-wins) |
| No `result` before stdout EOF | Uncertain |
| stderr output | drained for process lifetime (bounded tail kept), never parsed |
| stdout truncation (partial final line) | line discarded; no `result` → Uncertain |
| Nonzero exit WITH a single `result` | outcome per the `result` |
| Nonzero / zero exit WITHOUT `result` | Uncertain |
| Context cancellation (observer) | tap detach only; process continues |
| Cancel (Council) | graceful terminate → kill; no `result` ⇒ Uncertain |
| Deadline exceeded | terminate; Uncertain |
| Deterministic missing-session error on resume launch | **ErrNativeSessionMissing** — typed, distinct from generic post-start uncertainty: the bound native session provably does not exist server-side |
| Process-start success followed by immediate failure | acceptance per §3.5 baseline; outcome Uncertain |
| Executor start failure (process never started) | Rejected — DefinitivelyMissing evidence (pre-acceptance) |
| Observer detachment | taps detach; stream reader runs for the process lifetime |
| **`system/init` mismatch or init never arrives** (missing hooks/skills/plugins vs manifest, wrong cwd/model/permission mode, auth unreachable, unknown tool in init) | **Uncertain** — init arrives after process start and stdin transmission, so by the evidence rule no pre-acceptance classification is possible. The adapter terminates the process immediately, rejects the output, and records the toolkit-evidence reason on the uncertain attempt |

`system/init` can be checked before the first prompt only in the
SDK-style interactive flow, which Council does not use; therefore init
failures are post-transmission failures and are Uncertain, never Failed.
CreateSession performs no native call and cannot observe authentication
state; auth unreachability surfaces at first dispatch as Uncertain.

Any started process without exactly one verified terminal `result` remains
Uncertain.

### 3.10 Reconciliation evidence rules (finding 3)

| Evidence | Verdict for the LOST attempt |
|---|---|
| `result` event observed in-life | terminal (durable outcome committed) |
| Anything else — including a later recovery probe | **Uncertain, permanently** |

- A recovery probe is a **separate authorized turn** with its own attempt
  identity, dispatch intent, and cost attribution. Its `result` describes
  its own invocation and is NEVER attributed to the lost attempt. It may
  establish session liveness (recorded on the probe attempt) and may be
  used to re-produce work as NEW work.
- The lost attempt's durable record ends in an uncertainty disposition
  recorded explicitly by the controller; replacement work is a new
  attempt. Until that disposition, the unresolved attempt blocks all
  subsequent turns on the native session (§3.5), across restarts.

### 3.11 Durable state schema (finding 5)

Owned by the storage layer (single-writer SQLite transactions; migration
version bump required before planning):

```
claude_session_bindings
  session_id          PK/FK (logical session)
  native_id           TEXT (UUIDv4, unique)
  materialized        BOOL    -- TRUE only after the bound transcript was observed
  model, workspace    TEXT
  config_root         TEXT    -- inside claude_config_base_dir
  template_digest     TEXT    -- frozen toolkit manifest digest
  first_prompt_digest TEXT    -- set at first acceptance

claude_turn_attempts
  attempt_id, session_id, turn_key
  UNIQUE(session_id, turn_key, attempt_id)
  transition_version  INTEGER  -- monotonic per attempt row
  baseline_file_identity TEXT   -- device+inode or content identity
  baseline_size       INTEGER
  baseline_entries    INTEGER
  materialized_baseline BOOL
  started_at          TIMESTMP NULL  -- process launched
  first_stdin_byte_at TIMESTMP NULL  -- transmission begun (ambiguity boundary)
  known_dead_at       TIMESTMP NULL  -- process exit observed
  transcript_protection TEXT (protected|advisory|unverified)
  prompt_digest       TEXT
  accepted            BOOL
  terminal            BOOL
  result_payload      TEXT NULL    -- verified terminal result text
  result_usage        TEXT NULL    -- usage/cost JSON (verified terminal only)
  disposition         TEXT NULL    -- completed|failed|uncertain-disposed|missing
  updated_at
```

**Crash-safe ordering** (each step durable before the next):
1. persist attempt with baseline (materialized flag or cursor) — before
   launch;
2. mark `started_at` — after executor Start returns;
3. mark `first_stdin_byte_at` — after the prompt write begins;
4. acceptance (`accepted`, prompt digest match) — from transcript;
5. terminal (`terminal`, `result_payload`, `result_usage`) — only from an
   observed `result` event;
6. `disposition` — only by explicit controller decision.

**Template materialization (atomic)**: validated source template (regular
files only, no symlinks, per-file size bounds, total size bound); copied
into `<base>/<run>/<session>/config.tmp` with secure permissions (0700
dir / 0600 files; Windows: ACL-equivalent or fail-closed capability as
§3.6); atomically renamed into place; on any failure the temporary
directory is removed and the session is not materialized.

**Refinement — profile encoding**: the toolkit manifest is added to the
canonical profile as explicit fields (`toolkit_manifest`: expected hooks,
skills, plugins, approved tools, denied complement, probed CLI version),
bumping the canonical profile algorithm version; backfill behavior: runs
frozen before the version bump carry no manifest and pin toolkit
enforcement to "record-only" (evidence captured, mismatches logged, no
termination). Template digest formula: `ctmpl-v1:sha256:<hex>` over the
sorted (relative path, file bytes) pairs of the template tree.

## 4. Evidence plan (fixtures vs integration)

- **Fixture layer** (CI, provider-free): a fake `claude` executable via the
  PolicyExecutor replaying recorded stream-json fixtures (init, assistant,
  denials, results, malformed/oversized/truncated streams, multi-result,
  init-mismatch) plus a fixture per-session config store exercising the
  JSONL trust model, template materialization, and toolkit checks.
  Cross-platform except process fixtures (POSIX-scoped).
- **Integration layer** (manual, sanitized, operator-invoked, excluded
  from CI): `scripts/ac008-integration-evidence.sh` — real binary, real
  login, sanitized captures; never selects a provider/model beyond the
  operator's explicit step; includes the operator-authorized
  transcript-path denial probe suite (§3.6).

## 5. Boundaries

- Never `-c/--continue`, `--fork-session`, bypass-permission flags,
  `--bare`, `--no-session-persistence`, or non-default permission modes.
- Never read/copy native credentials; `apiKeySource` observation only.
- Every process launch through the AC-005 executor with the workspace root
  pinned and `CLAUDE_CONFIG_DIR` pointed at the session's own root under
  `claude_config_base_dir`.
- Guardrails and hooks remain enabled; denials are recorded honestly.
- JSONL is advisory by default; even protected, it is acceptance/progress
  evidence, never terminal proof.
- Uncertain attempts are never resolved by attributing another turn's
  result to them; they block the native session until explicitly disposed.

## 6. Acceptance mapping (issue #8)

| Acceptance criterion | Where |
|---|---|
| Probe installed arguments; capture model/version | §3.7 probe template (`--version` + `--help` contract) |
| Exact create/resume with approved config + native login | §3.3–§3.4; fixtures + manual script |
| Structured output; denials vs completions distinguished | §2.3/§3.9 four-class mapping; fixture regressions |
| Bounded invocation/cancellation; reconcile uncertain before retry | §3.9 state table; §3.10 rules |
| Toolkit-loading + recovery evidence; output fixtures | §3.8 init-vs-manifest check; fixture + manual layers |

### 6.1 Concrete acceptance scenarios

1. **Concurrent duplicate dispatch** — N callers, one TurnRef: one native
   process, shared verdict, single-flight per native session.
2. **Crash after process start** — process killed post-acceptance,
   pre-result: attempt stays Uncertain; reconciliation cannot resolve it;
   it remains uncertain until the controller explicitly disposes it, and
   any replacement work is a new attempt.
3. **Orphan process after daemon death** — restart finds the attempt
   unaccounted; the durable baseline proves acceptance; the unresolved
   attempt blocks the native session (across restarts) until disposed.
4. **Truncated JSONL** — torn final line: acceptance evidence still
   parses; no terminal claim.
5. **Exact-session mismatch** — transcript whose recorded first-prompt
   digest mismatches the binding: ResumeSession fails closed.
6. **Approval-required behavior** — would-prompt tool under `-p`:
   ApprovalDenied classification, bounded run, no hang.
7. **Missing toolkit elements / init mismatch** — process terminated,
   attempt Uncertain with the toolkit-evidence reason (post-transmission
   rule).
8. **Sibling-history denial / absence** — sibling transcripts are absent
   from a session's own config root; any worker attempt to address another
   session's path is denied by executor/guardrail policy where provable,
   and the adapter derives only the bound session's path. Because
   tool-path denial to the transcript is not fully established, the
   transcript remains advisory (§3.6) and scenario-dependent recovery
   decisions fail closed.
9. **Omitted tool cannot execute** — computed deny complement passed via
   `--disallowedTools`; live probe evidence required before the
   enforcement claim is marked verified.
10. **Concurrent creation failure sharing** — matching waiters receive the
    creator's typed failure with zero extra native sessions.
