# AC-008 Design — Claude persistent contributor adapter

Status: DRAFT v6 for review
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

### 3.5 Durable dispatch correlation (finding 2)

- **Pre-launch baseline**: before starting the process, the attempt record
  stores the transcript baseline — for a materialized session,
  `(file-identity, byte size, entry count)` as typed columns; for a first
  turn, `materialized=false`.
- **Acceptance is capability-conditional**:
  - **Protected mode** (transcript-protection attestation valid, §3.6):
    the turn is accepted when the transcript advances past the baseline
    with a new `user` entry whose prompt digest matches the attempt's
    stored prompt digest.
  - **Advisory/unverified mode**: transcript observations are DIAGNOSTIC
    ONLY. `accepted` remains UNKNOWN after process start; nothing the
    adapter reads from the transcript classifies or authorizes anything.
- **Redispatch rule, capability-conditional**:
  - **Protected mode**: verified positive absence (baseline unchanged
    AND the process known dead) authorizes exactly ONE same-attempt
    redispatch — nothing was accepted, so re-execution cannot double-run.
    Absence after the redispatch too requires disposition.
  - **Advisory/unverified mode**: once a process has been STARTED, no
    redispatch of that attempt occurs at all. The attempt remains
    **Uncertain until the controller explicitly records an uncertainty
    disposition** — across daemon restarts (blocking state derives from
    durable attempt state, not process memory). Any replacement work runs
    as a NEW attempt.
- **Blocking semantics**: an unresolved (uncertain, undisposed) attempt
  blocks every subsequent turn on the same native session — including
  after restart — until the controller records the disposition.
  Reconciliation cannot resolve a lost started attempt (§3.10); it can
  only gather evidence for the disposition decision.

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
  authoritative for acceptance/absence decisions (including the single
  verified-absence redispatch). Any reachable path ⇒ advisory for that
  configuration. The probe suite is an operator-authorized action (it
  deliberately points a worker at a sibling history).
- **Protection attestation**: protected capability is persisted as an
  attestation record, not an enum on the attempt. The attestation binds:
  Claude CLI version, toolkit-manifest digest, template digest, platform,
  the full per-path probe results with enforcing capabilities, and the
  probe timestamp/actor. A launch's transcript is treated as protected
  ONLY while the attestation matches the CURRENT version, manifest
  digest, template digest, and platform exactly; any version bump,
  template or manifest change, or platform difference invalidates it and
  the mode reverts to advisory until a new probe suite is run and
  attested.
- **Integrity checks (both modes)**: derived path with no symlinks;
  POSIX ownership/mode 0600 checked — **Windows: fail-closed platform
  statement** — the POSIX checks are not expressible, so the transcript is
  `integrity=unverified` and every decision relying on it degrades to
  Uncertain (portable decision logic stays covered by fixtures); torn
  final lines ignored; mid-file parse failure invalidates the read;
  single-writer by construction (slot); tail-only reads past the recorded
  baseline, size-capped; entries matched by exact verified `type` fields.
- **Trust ceiling**: even in protected mode the JSONL is written by the
  native process, not by Council — it authorizes acceptance and
  verified-absence redispatch only. It NEVER proves terminal completion:
  the terminal `result` event is stdout-only (verified).

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

### 3.8 Frozen tooling enforcement (findings 3, 1-round-4)

- **Version-pinned universe, never self-discovered.** The deny complement
  must exist BEFORE the first prompt is transmitted, but `system/init`
  arrives only after that. The native tool universe therefore comes from
  one of two pre-freeze sources, never from the governed turn itself:
  1. the **committed evidence universe** for the probed CLI version —
     `docs/superpowers/evidence/ac008-native-tool-universe-<version>.json`
     (captured from `system/init.tools[]` during research; the 2.1.278
     file is committed); or
  2. an **operator-authorized live inventory probe** (a minimal native
     invocation whose `system/init` is captured and committed) run before
     profile freeze when the installed version differs.

  The committed base universe covers NATIVE tools only — it deliberately
  excludes installation-specific `mcp__*` and plugin-contributed tools.
  **Any enabled MCP/plugin tool therefore requires path 2 (the pre-freeze
  live inventory probe)**; the committed base universe alone is valid
  only when the frozen run has no enabled MCP/plugin tools AND
  `system/init` at dispatch contains no native tool names beyond the
  committed list (drift → Uncertain per the init rule).
  At dispatch time, `system/init.tools[]` is compared against the pinned
  universe: drift (unknown or missing tools) terminates the process and
  classifies Uncertain (§3.9) — the first turn is never used to discover
  the policy governing it.
- **Deny set = pinned universe − approved set.** The adapter computes the
  full complement against the frozen approved set and passes it through
  `--disallowedTools` — the verified denial mechanism ("No such tool
  available: … disabled for this session, in subagents as well as here").
  `--allowedTools` exclusivity is NOT relied upon (native default mode
  executes some tools with no list — verified).
- **Live omitted-tool probe required** (implementation-phase, before the
  enforcement claim is marked verified): dispatch a prompt requiring an
  omitted tool under the computed complement and record the structured
  denial. Until then the enforcement row is evidence-pending, and the
  guardrail PreToolUse hooks remain the operating second layer.
- **Manifest**: approved tools, denied complement, expected hooks, skills,
  plugins, probed CLI version, and the universe evidence file digest —
  the frozen toolkit manifest, recorded in the run profile and covered by
  the profile digest. Per-session toolkit evidence = `system/init`
  compared against the manifest (`cwd`, version, model, permission mode,
  manifest presence).

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
  transcript_protection TEXT (protected|advisory|unverified)
  protection_attestation_id TEXT NULL
                      -- WHICH attestation governed this attempt's
                      -- evidence classification, frozen at launch; a
                      -- newer attestation never upgrades old attempts
  prompt_digest       TEXT
  launch_count        INTEGER (0..2)
                      -- 1 = initial launch; 2 = the single protected-
                      -- verified-absence redispatch was consumed
  accepted            BOOL NULL    -- known only in protected mode
  terminal            BOOL
  result_payload      TEXT NULL    -- verified terminal result text
  result_usage        TEXT NULL    -- usage/cost JSON (verified terminal only)
  observed_status     TEXT         -- completed|failed|missing|uncertain
                      -- (service-derived from evidence; automatic)
  uncertainty_disposition
                      TEXT NULL    -- controller decision ONLY for
                      -- resolving uncertainty: nullable, carries actor,
                      -- controller generation, operation ID, timestamp
                      -- (AC-004 authority) via the disposition journal
                      -- entry, not a bare column write
  updated_at

claude_attempt_launches              -- one row per process launch
  attempt_id         FK
  launch_index       INTEGER (1|2)
  UNIQUE(attempt_id, launch_index)
  started_at         TIMESTMP
  first_stdin_byte_at TIMESTMP NULL
  known_dead_at      TIMESTMP NULL
  exit_code          INTEGER NULL
  executor_identity  TEXT            -- policy fingerprint of the launch
```

**Crash-safe ordering** (each step durable before the next):
1. persist attempt with baseline (materialized flag or cursor) — before
   launch;
2. `launch_count 0→1` + insert launch row + mark `started_at` — after
   executor Start returns;
3. mark `first_stdin_byte_at` — after the prompt write begins;
4. acceptance (`accepted`, prompt digest match) — protected mode only,
   from transcript;
5. terminal (`terminal`, `result_payload`, `result_usage`) — only from an
   observed `result` event;
6. `observed_status` — automatically from evidence (completed/failed/
   missing/uncertain);
7. **redispatch consumption** — the single protected-mode verified-
   absence redispatch is an atomic transition: precondition (protection
   attestation valid for this attempt, verified absence recorded, launch
   count = 1) sets `launch_count 1→2` and inserts the second launch row
   in ONE transaction; a crash between decision and consumption leaves
   launch_count = 1, and the transition is re-derived — it can never
   authorize a third launch;
8. `uncertainty_disposition` — ONLY by explicit controller decision
   (AC-004 authority), recorded as a journal entry with actor,
   generation, and operation ID; it resolves an uncertain attempt and
   unblocks the native session.

**Template materialization (atomic)**: validated source template (regular
files only, no symlinks, per-file size bounds, total size bound); copied
into `<base>/<run>/<session>/config.tmp` with secure permissions (0700
dir / 0600 files; Windows: ACL-equivalent or fail-closed capability as
§3.6); atomically renamed into place; on any failure the temporary
directory is removed and the session is not materialized.

**Protection attestation (durable, finding 2)**:

```
claude_protection_attestations
  attestation_id     TEXT PK     -- cprot-v1:sha256:<hex> over the
                                 -- canonical attestation payload below
  claude_version     TEXT        -- probed CLI version
  platform           TEXT        -- os/arch
  manifest_digest    TEXT        -- toolkit manifest digest in force
  template_digest    TEXT        -- ctmpl-v1 digest in force
  probe_results      TEXT (JSON) -- per-path: tool, target, denied bool,
                                 -- enforcing capability, denial text
  probed_at          TIMESTMP
  actor              TEXT        -- operator identity (journal-linked)
```

- Canonical attestation payload: the fields above framed like the
  template digest (length-prefixed UTF-8, fixed field order), digest
  prefix `cprot-v1:sha256:`; the attestation row is created only by an
  operator-authorized journal operation (AC-004 authority).
- Attempts freeze `protection_attestation_id` at launch: the governing
  attestation is whichever record was valid for (claude version,
  manifest digest, template digest, platform) AT LAUNCH. A later
  attestation never retroactively upgrades or reclassifies earlier
  attempts; a version/template/manifest/platform change simply means new
  launches have no valid attestation and run advisory until a new probe
  suite is attested.

**Profile encoding (finding 3, exact)**:

- New algorithm identifier: **`cprof-v2`**, digest prefix
  `cprof-v2:sha256:<hex>`. Canonical encoding is JSON with sorted keys,
  no insignificant whitespace, UTF-8 — identical normalization to
  `cprof-v1` — plus the additive field:
  ```json
  "toolkit_manifest": {
    "probed_cli_version": "2.1.278",
    "universe_evidence_digest": "<sha256 of the committed universe file>",
    "approved_tools": ["..."],
    "denied_complement": ["..."],
    "expected_hooks": ["..."],
    "expected_skills": ["..."],
    "expected_plugins": ["..."]
  }
  ```
- **AC-007 compatibility**: `cprof-v2` is purely additive — every
  `cprof-v1` field is present with identical semantics. The OpenCode
  adapter (AC-007) accepts BOTH `cprof-v1` and `cprof-v2` and ignores
  `toolkit_manifest`; its existing frozen configurations survive the
  bump unchanged.
- **Claude eligibility**: the Claude adapter REQUIRES `cprof-v2` with a
  populated `toolkit_manifest`. Legacy `cprof-v1` profiles remain
  readable (OpenCode runs continue) but are rejected specifically for
  Claude production sessions with a typed `ErrUnsupportedProfile` —
  CreateSession and dispatch fail closed; no record-only mode exists.
  Backfill of old runs is out of scope.

**Template digest formula (finding 5)** — `ctmpl-v1:sha256:<hex>`:
1. Enumerate the template tree: regular files only (symlinks, devices,
   directories-as-entries rejected); paths normalized to relative
   forward-slash UTF-8 form (no leading `./`, no trailing slash);
   duplicate normalized paths rejected.
2. Order entries by normalized path, byte-wise lexicographic (UTF-8
   bytes; case-sensitive — no case folding).
3. Frame: `uint32-BE(entry-count)` then, per entry,
   `uint32-BE(len(path-bytes)) || path-bytes ||
    uint32-BE(len(file-bytes)) || raw file bytes`
   (an empty file contributes a zero length prefix and no bytes; length
   prefixes are 4-byte big-endian; byte encoding is raw UTF-8 for paths
   and raw file content).
4. Digest = `sha256` over the framed byte sequence, rendered
   `ctmpl-v1:sha256:<lowercase-hex>`.

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
2. **Crash after process start** — process killed post-start:
   reconciliation cannot resolve it. Protected mode with verified absence
   authorizes one same-attempt redispatch; otherwise the attempt remains
   uncertain until the controller explicitly disposes it, and any
   replacement work is a new attempt.
3. **Orphan process after daemon death** — restart finds the attempt
   unaccounted; in protected mode the durable baseline may prove
   acceptance (diagnostic and acceptance only); the unresolved attempt
   blocks the native session (across restarts) until the controller
   records the uncertainty disposition. In advisory mode the baseline
   proves nothing and the attempt is simply uncertain-until-disposed.
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
