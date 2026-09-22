# AC-008 Design — Claude persistent contributor adapter

Status: DRAFT v3 for review
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
| `claude -p/--print` | Non-interactive one-shot turn; **prompt is read from stdin when not given as an argument** (verified live: piped prompt → `result success` with the expected reply) |
| `--output-format stream-json` (+ required `--verbose` in print mode) | Newline-delimited JSON event stream on stdout |
| `--session-id <uuid>` | Caller-chosen session identity; **must be a valid UUID** (`not-a-uuid` → `Invalid session ID. Must be a valid UUID.`) |
| `--resume <session-id>` | Exact resumption, verified to continue under the SAME session UUID |
| `--resume <missing-id>` | Deterministic failure: `No conversation found with session ID: <id>` |
| `--session-id` + `--resume` without `--fork-session` | Rejected — plain `--resume` preserves identity; `--fork-session` never used by Council |
| `-c/--continue` | Most-recent conversation — **forbidden** (exact-session rule) |
| `--model <model>` | Model preset (verified `haiku` → `claude-haiku-4-5-20251001`) |
| `--max-turns <n>` | Bound on agentic turns; exceeded → `result` `subtype:"error_max_turns"`, `is_error:true` (verified) |
| `--permission-mode` | Choices: `acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan` — Council **omits the flag** (native default); `bypassPermissions`/`auto` **forbidden** |
| `--allowedTools` / `--disallowedTools` | Native allow/deny lists; verified structured denial for a denied tool (`tool_result` `is_error:true`, "Bash is disabled for this session") |
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
  channel** (three distinct verified denials, §2.4).
- `result` (terminal, stdout-only): `subtype` (`success` /
  `error_max_turns`), `is_error`, `result` text, `total_cost_usd`,
  `usage`, `duration_api_ms`, `session_id`.

### 2.3 Verified headless permission behavior (decisive probes)

| Scenario (all under `-p`, native default mode) | Verified outcome |
|---|---|
| Safe tool (Bash `echo`) | Executes: `tool_result` `is_error:false`; result `success` |
| **`manual`** mode, safe tool | **Also executes without prompting** — `manual` is not deny-by-default headless; Council does not rely on it |
| Would-prompt tool (Write outside the workspace) | **Immediate structured denial, no blocking**: `tool_result` `is_error:true`, "Claude requested permissions to write to …, but you haven't granted it yet"; run ends `error_max_turns` |
| Deny-listed tool | `tool_result` `is_error:true`, "No such tool available: Bash. Bash is disabled for this session" |
| Guardrail PreToolUse hook denial | `tool_result` `is_error:true`, "PreToolUse:Glob hook error: [… guardrail]: Guardrail denied this action …" |
| Working-directory boundary | Native denial: "may only list files in the allowed working directories for this session: '<workspace>'" |
| Model refuses without a tool call | `result` `success` with declining text — a completed turn, never a fabricated denial |

**Consequence**: in headless mode an approval requirement is surfaced as an
immediate structured denial, never as a wait. Every invocation is still
bounded by `--max-turns` and a process deadline; no invocation can wait
indefinitely.

### 2.4 Verified behaviors and hazards

- **Session store**: `<config-dir>/projects/<munged-cwd>/<session-id>.jsonl`;
  project-scoped by working directory — resumption must run with the AC-005
  workspace root pinned as cwd or the session will not be found.
- **No durable terminal record in JSONL** (verified by full-file inspection
  of a completed session): entry types are `queue-operation`, `attachment`,
  `user`, `atis-latch`, `last-prompt`, `assistant`, `cost-state`, `mode`.
  There is **no `result`-equivalent entry** — the terminal `result` event is
  stdout-only. An `assistant` entry may be a mid-work tool request. JSONL
  proves acceptance and progress, never completion (§3.7).
- **Model-level refusal vs structured denial**: a model may decline without
  any tool call (`result` `success` with declining text) — completed turn,
  honest output, never a fabricated denial.
- **Native authentication**: operator-provisioned; Council never copies or
  inspects credentials. `system/init.apiKeySource` is observable without
  reading secrets; its value under a per-session config root is recorded as
  implementation-verification evidence (fail closed if unreachable).

## 3. Architecture

### 3.1 Process-per-turn, not a server

Claude turns are **one CLI process per dispatched turn**:
`claude -p … --resume <native-id>` (first turn: `--session-id <native-id>`)
with the prompt delivered on **stdin** and stream-json on stdout.

- The prompt is NEVER an argv element (process-list exposure); it is
  written to the process's stdin by the adapter (verified transport). The
  ambiguity boundary follows the AC-007 rule adapted to stdin: the **first
  successfully transmitted stdin byte** marks transmission begun — a
  transport failure after that point is DispatchUnknown, never rejected.
- Every process launches through the AC-005 `PolicyExecutor` (never
  `os/exec`) with the session's workspace root pinned as cwd.
- Council generates the native session UUID itself (the CLI accepts
  caller-chosen UUIDs — verified). Generation uses `crypto/rand` UUIDv4
  (RFC 4122 version/variant bits), validated on reuse; generation happens
  once per logical session inside the creation reservation (§3.3).
- Subsequent turns resume with `--resume <native-id>` (never `-c`).

### 3.2 Isolation: per-session config roots (finding 1)

- **Layout**: `<service-store>/<run-id>/<session-id>/config/` is a
  **per-session config root**, created at session materialization by
  copying an **operator-provisioned template** (approved settings, hooks,
  skills, plugins manifests — never credentials). The launch environment
  sets `CLAUDE_CONFIG_DIR` to that root (relocation verified).
- **Effect**: a session's transcript lives in ITS OWN config root. A
  worker for session A has no sibling transcripts in its config tree —
  they are physically absent, not merely denied.
- **Native login**: verified to function with a relocated config root
  (keychain access is OS-user scoped, not config-dir scoped); the
  implementation must record `system/init.apiKeySource` under the
  per-session root as provisioning evidence and **fail closed** (session
  creation error) if authentication is unreachable.
- **Threat boundary, stated honestly**: workers run as the same OS user as
  the service. A worker that learns another session's root path could
  still read it — random per-session UUID path components (122 bits of
  unpredictability) plus executor workspace policy plus guardrail hooks
  are defense in depth, not a hard boundary. OS-level isolation between
  workers is an AC-005 executor concern and is explicitly OUT of this
  adapter's claims. Where a platform cannot enforce a protection this
  spec relies on, the affected decision fails closed (§3.7).
- **Template drift**: the template's digest is frozen into the run profile
  (§3.9); a copied root whose digest differs from the frozen one fails the
  toolkit evidence check.

### 3.3 Session creation, idempotency, and binding

- `CreateSession` validates the frozen config, generates the UUIDv4 native
  ID once (inside the per-logical-session creation reservation), creates
  the per-session config root from the operator template, and returns the
  binding with `materialized=false`. **The adapter does not persist; the
  service/storage layer persists the returned binding** (§3.8).
- **Unmaterialized binding**: until the first dispatched process starts,
  the native transcript does not exist. `ResumeSession` on an
  unmaterialized binding performs the same local inspection and returns
  success with the binding still unmaterialized — never classified as
  lost. The first dispatch materializes it.
- Concurrent `CreateSession`: creation reservation (one native identity,
  shared result including typed failures, mismatched callers fail closed).

### 3.4 ResumeSession — local inspection, native verification deferred (finding 2)

A `--resume` invocation REQUIRES a prompt and starts a provider turn
(verified flag contract); there is no provider-free native resume probe.

- `ResumeSession` performs **validated local inspection** only:
  1. binding present and well-formed (UUIDv4 native ID, model, workspace);
  2. config root present with frozen-template digest match;
  3. for a materialized binding, the transcript (per §3.6) exists, passes
     integrity checks, and contains the accepted `user` entry recorded in
     the attempt record — proof THIS transcript is THIS session's;
  4. for an unmaterialized binding, step 1–2 only.
- Native verification is **deferred to the next authorized dispatch**: the
  resume launch either starts cleanly or fails with the deterministic
  missing-session error, which raises `ErrNativeSessionMissing` at that
  point. Resume itself makes no native call and consumes no quota.

### 3.5 Durable dispatch correlation (finding 3)

- **Pre-launch cursor**: before starting the process, the adapter records
  in the attempt record the transcript's `(path-identity, byte size,
  entry-count)` — or `materialized=false`. This is the acceptance
  baseline.
- **Acceptance boundary**: the turn is "accepted" when the transcript
  advances past the baseline with a new `user` entry whose prompt hash
  matches the dispatched prompt.
- **No-redispatch rule**: once a process has been successfully STARTED for
  an attempt, no redispatch occurs unless reconciliation proves the
  positive absence of acceptance (baseline unchanged after the process is
  known dead). Acceptance-without-terminal ⇒ Uncertain, never automatic
  retry.

### 3.6 Transcript (JSONL) trust model (findings 2, 5)

- **Protection first**: the transcript lives in the per-session config
  root, **outside the workspace**. Worker tool access to it is denied by
  executor workspace policy and guardrail hooks (both verified denial
  channels). Only the native CLI process (trusted writer) and the adapter
  (service side) read it.
- **If that protection cannot be enforced on a platform**, the transcript
  is classified advisory: every recovery decision that would have used it
  fails closed, and a started process lacking an observed `result` remains
  **Uncertain permanently**. The platform capability is checked at launch
  and recorded.
- **Path derivation**: `<config-root>/projects/<munged-cwd>/<native-id>.jsonl`
  derived only from the binding's own workspace root and config root;
  never accepted from the native side or callers. The native ID is
  validated UUIDv4.
- **Symlinks**: the transcript path must not be a symlink; opened
  no-follow; link components rejected.
- **Ownership/permissions (POSIX)**: regular file owned by the service
  account, mode 0600 checked. **Windows**: the POSIX checks are not
  expressible; the platform capability statement is fail-closed — on
  Windows the adapter records `transcript-integrity=unverified` and every
  decision that would rely on the transcript degrades to Uncertain (the
  CI matrix keeps the portable decision logic covered by fixtures).
- **Partial trailing records**: a torn final line is ignored (bounded to
  the final line) and never treated as evidence.
- **Concurrent appends**: single-writer by construction (one process per
  native session via the slot); reads tolerate growth.
- **Schema/version**: entries matched by exact `type` fields from §2.2;
  unknown types skipped; mid-file JSON parse failure invalidates the read
  (except the tolerated trailing line).
- **Size limits**: reads scan only the tail past the recorded cursor,
  capped; deep scans are never needed because the baseline is recorded.
- **Trust ceiling**: with protection enforced, the remaining writers are
  the trusted native process and the service itself; worker forgery is
  denied by policy. The file therefore MAY authorize acceptance/absence
  decisions (redispatch safety). It still NEVER proves terminal
  completion (§3.10) — the terminal `result` is stdout-only.

### 3.7 Launch seam (finding 6)

- **`ClaudeTurnLaunchSource`** (service-owned, storage-backed deep module):
  builds the complete `LaunchRequest` from the frozen run profile and the
  AC-005 workspace allocation — command, exact arguments (below), cwd =
  workspace root, `CLAUDE_CONFIG_DIR`, `--max-turns` from the frozen
  bound, model from `HarnessProfileSpec`, tool policy from §3.9. The
  adapter never synthesizes policy inputs.
- **Argument contract (exact)**:
  `["-p", "--output-format", "stream-json", "--verbose",
  ("--session-id"|"--resume"), <native-uuid>, "--model", <model>,
  "--max-turns", <bound>, ("--allowedTools", <approved...>)?,
  ("--disallowedTools", <denied...>)?]`
  — prompt on stdin, never argv. The executor validates the list against
  this template: only the parameterized slots vary; forbidden flags
  (`--bare`, `--continue`, `--fork-session`,
  `--no-session-persistence`, both bypass-permission flags, any
  `--permission-mode` value, `--append-system-prompt*`, `--system-prompt*`)
  and reordered, duplicated, or extra arguments are rejected before launch.
- **Typed config-dir extension**: `CLAUDE_CONFIG_DIR` is carried in a
  narrow typed executor extension (`LaunchRequest.ClaudeConfigDir`,
  single string, must be inside the service store) — validated ONLY for
  the exact Claude launch shape, mirroring `GeneratedServerEnv`; it is
  never an inherited-environment allowlist entry and is redacted from
  captured output like any generated value (it is a path, not a secret,
  but is excluded from event payloads).
- **Probe launches**: a separate operator-owned `ClaudeProbeLaunchTemplate`
  produces the probe launch in an operator-allocated isolated scratch
  directory. The probe verifies the **installed argument surface**, not
  just the version: `--version` output plus `--help` contract assertions
  (required flags exist, `--permission-mode`/`--output-format` choice sets
  match the verified surface, forbidden flags are absent from the approved
  template). Provider-free.

### 3.8 Frozen tooling enforcement (finding 5)

Fail-closed translation from `HarnessProfileSpec` to native tool policy:

- The frozen profile declares the **approved native tool set** (the
  tool-class names verified from `system/init.tools[]`, e.g. Read, Glob,
  Grep-class) plus the tool classes the operator denies. Translation:
  `--allowedTools <approved…>` and `--disallowedTools <denied…>`;
  **unlisted native tools are not allowlisted**, so any tool outside the
  approved set requires a permission the headless run cannot grant and is
  immediately denied with the verified structured `tool_result`
  (evidence that an omitted tool cannot execute). Workspace command
  gating continues to apply to allowed shell-class tools.
- Guardrail PreToolUse hooks remain the second layer (verified denial
  shape) and take effect before execution.
- The approved tool list, denied list, expected hooks, skills, and plugins
  form the **frozen toolkit manifest**, recorded in the run profile and
  covered by the profile digest. Toolkit evidence per session = `system/init`
  compared against the manifest: `cwd` equals the workspace root, version
  matches the probe-verified version, model matches the frozen preset,
  permission mode matches, and every manifest element is present. The
  manifest is operator-frozen per run — the operator's current personal
  hook names are never universal requirements. **Immediate failure**:
  if `system/init` is missing required manifest elements or reports a
  wrong cwd/model/permission mode, the adapter terminates the process and
  rejects the turn's output (failed, evidence captured).

### 3.9 Stream and process failure semantics (finding 8)

| Condition | Classification |
|---|---|
| Malformed NDJSON line mid-stream (not final) | stream poisoned → Uncertain; process terminated |
| Oversized NDJSON line (> bound) | stream poisoned → Uncertain; process terminated |
| **Multiple `result` events** | poisoned → **Uncertain** (protocol drift; "first wins" is unsafe) |
| No `result` before stdout EOF | Uncertain (process dead without terminal proof) |
| stderr output | drained for process lifetime (bounded tail kept), never parsed as events |
| stdout truncation (partial final line at EOF) | line discarded; no `result` seen → Uncertain |
| Nonzero exit WITH a single observed `result` | outcome per the `result` |
| Nonzero exit WITHOUT `result` | Uncertain |
| Exit 0 WITHOUT `result` | Uncertain |
| Context cancellation (observer) | tap detach only; process continues |
| Cancel (Council) | graceful terminate → kill; no `result` ⇒ Uncertain |
| Deadline exceeded | terminate; Uncertain |
| Deterministic missing-session error on resume launch | **ErrNativeSessionMissing** (typed, distinct from post-start uncertainty: the binding's native session provably does not exist server-side) |
| Process-start success followed by immediate failure | acceptance per §3.5 baseline; outcome Uncertain |
| Executor start failure (process never started) | Rejected — DefinitivelyMissing evidence (pre-acceptance) |
| Observer detachment | taps detach; the stream reader runs for the process lifetime |
| `system/init` toolkit check failure | process terminated; turn Failed with toolkit-evidence reason |

Any started process without exactly one verified terminal `result` remains
Uncertain.

### 3.10 Reconciliation evidence rules (finding 3)

| Evidence | Verdict for the LOST attempt |
|---|---|
| `result` event observed in-life | terminal (durable outcome committed) |
| Anything else — including a later recovery probe | **Uncertain**, permanently |

- A recovery probe is a **separate authorized turn**: it carries its own
  attempt identity, its own dispatch intent, and its own cost attribution.
  Its `result` describes ITS OWN invocation and is never attributed to the
  lost attempt. It may establish that the session remains resumable
  (liveness evidence recorded on the probe attempt) and may be used to ask
  the contributor to re-produce work — as new work.
- The lost attempt's durable record ends in an uncertainty disposition;
  whether to re-run the work is a controller/operator decision made on the
  new attempt, never an automatic fabrication of the old outcome.

### 3.11 Durable state schema (finding: required durable state)

Owned by the storage layer (single-writer SQLite transactions; migration
version bump required before planning):

```
claude_session_bindings
  session_id        PK/FK (logical session)
  native_id         TEXT (UUIDv4, unique)
  materialized      BOOL          -- transcript known to exist
  model, workspace, config_root PATH
  template_digest   TEXT          -- frozen toolkit manifest digest
  first_prompt_digest TEXT        -- set at first acceptance

claude_turn_attempts
  attempt_id, session_id, turn_key
  baseline (file-identity, byte size, entry count)  -- pre-launch cursor
  launch_started_at TIMESTMP      -- NULL until a process started
  accepted          BOOL          -- acceptance boundary observed
  terminal          BOOL          -- in-life result observed
  disposition       TEXT          -- completed|failed|uncertain|missing
  updated_at
```

Updates are atomic per transition (record-baseline at pre-launch;
launch-started at start; accepted/terminal/disposition as evidence
arrives) using the existing storage transition pattern with idempotency
keys, exactly as AC-007's attempt records.

## 4. Evidence plan (fixtures vs integration)

- **Fixture layer** (CI, provider-free): a fake `claude` executable via the
  PolicyExecutor replaying recorded stream-json fixtures (init, assistant,
  denials, results, malformed/oversized/truncated streams, multi-result)
  plus a fixture per-session config store exercising the JSONL trust model
  and toolkit checks — full adapter contract exactly as AC-007 did with
  its fake server. Cross-platform except process fixtures (POSIX-scoped).
- **Integration layer** (manual, sanitized, operator-invoked, excluded
  from CI): `scripts/ac008-integration-evidence.sh` — real binary, real
  login, sanitized captures; never selects a provider/model beyond the
  operator's explicit step.

## 5. Boundaries

- Never `-c/--continue`, `--fork-session`, bypass-permission flags,
  `--bare`, `--no-session-persistence`, or non-default permission modes.
- Never read/copy native credentials; `apiKeySource` observation only.
- Every process launch through the AC-005 executor with the workspace root
  pinned and `CLAUDE_CONFIG_DIR` pointed at the session's own root.
- Guardrails and hooks remain enabled; denials are recorded honestly.
- JSONL is protected acceptance/progress evidence, never terminal proof.
- Uncertain attempts are never resolved by attributing another turn's
  result to them.

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
   pre-result: attempt stays Uncertain; no redispatch until reconciliation.
3. **Orphan process after daemon death** — restart finds the attempt
   unaccounted; the persisted cursor proves acceptance; outcome stays
   Uncertain pending an authorized probe (which is recorded as its own
   attempt).
4. **Truncated JSONL** — torn final line: acceptance evidence still parses;
   no terminal claim.
5. **Exact-session mismatch** — transcript whose recorded first-prompt
   digest mismatches the binding: ResumeSession fails closed.
6. **Approval-required behavior** — would-prompt tool under `-p`:
   ApprovalDenied classification, bounded run, no hang.
7. **Missing toolkit elements** — `system/init` missing frozen-manifest
   elements or wrong cwd/model/permission mode: process terminated, turn
   Failed with toolkit-evidence reason.
8. **Sibling-history denial** — sibling transcripts are absent from a
   session's own config root (per-session layout); any worker attempt to
   address another session's path is denied by executor/guardrail policy,
   and the adapter derives only the bound session's path. Residual
   same-user traversal risk is stated in §3.2 and is an AC-005 executor
   concern; where policy cannot deny, decisions fail closed (§3.6).
9. **Omitted tool cannot execute** — a native tool absent from the frozen
   allowlist is invoked: structured denial recorded, turn continues
   honestly.
10. **Concurrent creation failure sharing** — matching waiters receive the
    creator's typed failure (uncertain classification) with zero extra
    native sessions.
