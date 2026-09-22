# AC-008 Design — Claude persistent contributor adapter

Status: DRAFT v2 for review
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
| `claude -p/--print` | Non-interactive one-shot turn: prompt in, result out |
| `--output-format stream-json` (+ required `--verbose` in print mode) | Newline-delimited JSON event stream on stdout |
| `--session-id <uuid>` | Caller-chosen session identity; **must be a valid UUID** (`not-a-uuid` → `Invalid session ID. Must be a valid UUID.`) |
| `--resume <session-id>` | Exact resumption, verified to continue under the SAME session UUID |
| `--resume <missing-id>` | Deterministic failure: `No conversation found with session ID: <id>` |
| `--session-id` + `--resume` without `--fork-session` | Rejected: `--session-id can only be used with --continue or --resume if --fork-session is also specified` — plain `--resume` preserves identity; `--fork-session` is never used by Council |
| `-c/--continue` | Most-recent conversation — **forbidden** (exact-session rule) |
| `--model <model>` | Model preset (verified `haiku` → `claude-haiku-4-5-20251001`) |
| `--max-turns <n>` | Bound on agentic turns; exceeded → `result` `subtype:"error_max_turns"`, `is_error:true` (verified) |
| `--permission-mode` | Choices: `acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan` — Council **omits the flag** (native default); `bypassPermissions`/`auto` **forbidden** |
| `--disallowedTools <tools...>` | Session deny-list; verified structured denial (`tool_result` `is_error:true`, "Bash is disabled for this session") |
| `--dangerously-skip-permissions`, `--allow-dangerously-skip-permissions`, `--bare`, `--no-session-persistence` | **All forbidden** (guardrail bypass / toolkit omission / lost persistence) |
| `CLAUDE_CONFIG_DIR=<dir>` (env) | **Verified**: relocates the entire config/state root — session transcript landed under `<dir>/projects/<munged-cwd>/<uuid>.jsonl` |

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

| Scenario (all under `-p`) | Verified outcome |
|---|---|
| Tool that runs without prompting (Bash `echo`) in **default** mode | Executes: `tool_result` `is_error:false`; result `success` |
| Same in **`manual`** mode | **Also executes without prompting** — `manual` is not deny-by-default headless; Council does not rely on it |
| Would-prompt tool (Write outside the workspace) in default mode | **Immediate structured denial, no blocking**: `tool_result` `is_error:true`, "Claude requested permissions to write to …, but you haven't granted it yet"; run ends `error_max_turns` |
| Deny-listed tool (`--disallowedTools Bash`) | `tool_result` `is_error:true`, "No such tool available: Bash. Bash is disabled for this session" |
| Guardrail PreToolUse hook denial | `tool_result` `is_error:true`, "PreToolUse:Glob hook error: [… guardrail]: Guardrail denied this action …" |
| Model refuses without a tool call | `result` `success` with declining text — a completed turn, never a fabricated denial |

**Consequence**: in headless mode an approval requirement is surfaced as an
immediate structured denial, never as a wait. Council therefore maps
"approval-required" to the approval-denied outcome class (§3.4) and bounds
every invocation with `--max-turns` plus a process deadline; no headless
invocation can wait indefinitely.

### 2.4 Verified behaviors and hazards

- **Session store**: `<config-dir>/projects/<munged-cwd>/<session-id>.jsonl`;
  project-scoped by working directory — resumption must run with the AC-005
  workspace root pinned as cwd or the session will not be found.
- **No durable terminal record in JSONL** (verified by full-file inspection
  of a completed session): entry types are `queue-operation`, `attachment`,
  `user`, `atis-latch`, `last-prompt`, `assistant`, `cost-state`, `mode`.
  There is **no `result`-equivalent entry** — the terminal `result` event is
  stdout-only. An `assistant` entry may be a mid-work tool request. JSONL
  therefore proves acceptance and progress, never completion (§3.5).
- **Model-level refusal vs structured denial**: a model may decline without
  any tool call (`result` `success` with declining text) — completed turn,
  honest output, never a fabricated denial.
- **Native authentication**: operator-provisioned; Council never copies or
  inspects credentials. `system/init.apiKeySource` is observable without
  reading secrets.

## 3. Architecture

### 3.1 Process-per-turn, not a server

Claude turns are **one CLI process per dispatched turn**:
`claude -p … --resume <native-id>` (first turn: `--session-id <native-id>`)
with stream-json stdout. Consequences:

- No server manager, no generated credentials, no health checks. Every
  process launches through the AC-005 `PolicyExecutor` (never `os/exec`)
  with the session's workspace root pinned as cwd.
- Council generates the native session UUID itself (the CLI accepts
  caller-chosen UUIDs — verified). Generation uses `crypto/rand` UUIDv4
  (RFC 4122: version nibble `4`, RFC variant bits), validated on reuse;
  generation happens once per logical session inside the creation
  reservation (§3.3), so concurrent creates share one identity.
- Subsequent turns resume with `--resume <native-id>` (never `-c`).

### 3.2 Isolation and threat boundary (finding 1)

**Mechanism (verified)**: the worker's launch environment sets
`CLAUDE_CONFIG_DIR` to a **service-owned config root** (provisioned once by
the operator — see below). This relocates Claude's entire config and
session store out of the operator's `~/.claude`.

- **Config provisioning**: the operator (or a one-time operator-run step)
  provisions the service config root with the approved settings, hooks,
  skills, and plugins, and the native login for the service identity.
  Council never copies operator credentials or dotfiles; provisioning is
  operator action, recorded in configuration (root path), not code.
- **Filesystem exposure**: the worker process sees (a) the session
  workspace (AC-005 policy), (b) the service config root (approved toolkit
  + THIS service's session store), (c) nothing else by Council's intent.
  The operator's personal `~/.claude` (private histories, credentials) is
  outside the boundary because `CLAUDE_CONFIG_DIR` redirects all Claude
  state reads/writes.
- **Session-store ownership**: the service config root's `projects/` tree
  is owned by the service account; one store holds all of THIS service's
  sessions.
- **Threat boundary, stated honestly**: a worker runs as the same OS user
  as the service, so it could technically read sibling session transcripts
  in the shared store, and OS-user isolation against that is an executor/
  AC-005 concern (per-run users/namespaces), not an adapter claim. Council
  enforces what it can deterministically: it never passes sibling paths,
  the executor's workspace policy and guardrail hooks deny out-of-scope
  access (verified live), and session-store reads BY THE ADAPTER are
  restricted to the bound session's single JSONL path (§3.6). A worker
  forging adapter-side evidence is addressed in §3.6's trust model: JSONL
  is never authoritative for terminal outcomes.
- **Rejected alternatives**: sharing the operator's `~/.claude` (exposes
  private histories/credentials to workers); per-run `CLAUDE_CONFIG_DIR`
  (would break native login — the service login lives in the service root).

### 3.3 Session creation, idempotency, and binding

- `CreateSession` validates the frozen config, generates the UUIDv4 native
  ID once (inside the per-logical-session creation reservation), and
  returns the binding. **The adapter does not persist; the service/storage
  layer persists the returned binding.**
- **Unmaterialized binding**: until the first dispatched process starts,
  the native transcript does not exist. The binding is represented
  explicitly as `materialized=false`. `ResumeSession` on an unmaterialized
  binding performs the same local inspection and MUST NOT classify it as
  lost; it returns success with the binding still unmaterialized. The
  first dispatch materializes it (creates the transcript via
  `--session-id`).
- Concurrent `CreateSession`: creation reservation (one native identity,
  shared result, mismatched callers fail closed) — as shipped in AC-007.

### 3.4 ResumeSession — local inspection, native verification deferred (finding 2)

A `--resume` invocation REQUIRES a prompt and starts a provider turn
(verified flag contract); there is no provider-free native resume probe.
Therefore:

- `ResumeSession` performs **validated local binding inspection**:
  1. binding present and well-formed (UUIDv4 native ID, model, workspace);
  2. for a materialized binding, the transcript file derived per §3.6
     exists, passes integrity checks, and contains at least one `user`
     entry whose content matches the session's recorded first prompt —
     i.e., proof THIS transcript is THIS session's;
  3. for an unmaterialized binding, steps 1 only.
- Native verification is **deferred to the next authorized dispatch**: the
  resume launch either starts cleanly or fails with the deterministic
  missing-session error, which raises `ErrNativeSessionMissing` at that
  point. Resume itself makes no native call and consumes no quota.
- No Claude flag provides a cheaper true native check (verified: `--resume`
  without a prompt is not a defined no-op probe).

### 3.5 Durable dispatch correlation (finding 3)

State is persisted by the service/storage layer per attempt; the adapter
supplies the evidence:

- **Pre-launch cursor**: before starting the process, the adapter records
  in the attempt record the transcript's current `(file-identity, byte
  size, entry-count)` — or `materialized=false` for a first turn. This is
  the acceptance baseline.
- **Acceptance boundary**: the turn is "accepted" when the transcript
  advances past the baseline with a new `user` entry whose prompt matches
  the dispatched prompt (hash compare). Acceptance is checkable at any
  time, after crashes included.
- **No-redispatch rule**: once a process has been successfully STARTED for
  an attempt, no redispatch of that attempt may occur unless
  reconciliation proves the positive absence of acceptance (baseline
  unchanged after the process is known dead) — identical to the AC-007
  rule. Acceptance-without-terminal ⇒ Uncertain, never automatic retry.
- Process identity is deliberately NOT part of the durable record
  (transient across daemon restarts); the cursor + attempt ID are.

### 3.6 Transcript (JSONL) trust and consistency model (findings 4, 5)

- **Path derivation**: `<config-root>/projects/<munged-cwd>/<native-id>.jsonl`
  where `<munged-cwd>` is the verified transformation of the workspace root
  (path separators → `-`). The adapter derives the path from the binding's
  own workspace root and config root only; it never accepts a session path
  from the native side or from callers.
- **Symlinks**: the transcript path must not be a symlink; the adapter
  opens with no-follow and rejects link components (verified derivation is
  plain-file).
- **Ownership/permissions**: regular file owned by the service account,
  mode 0600-checked; anything else fails the inspection.
- **Partial trailing records**: the last line may be torn by a crash or
  concurrent append; parsers ignore a trailing malformed line (bounded to
  the final line) and never treat it as evidence.
- **Concurrent appends**: only one process per native session can be
  in-flight (single-flight slot, AC-007), so appends are single-writer by
  construction; reads tolerate growth (size re-check).
- **Schema/version**: entries are matched by exact `type` fields observed
  in §2.2; unknown types are skipped; entries failing JSON parse mid-file
  invalidate the read (except the tolerated trailing line).
- **Size limits**: reads are capped (entries beyond a bound are not
  scanned for acceptance evidence; the acceptance baseline makes deep
  scans unnecessary — only the tail past the cursor is read).
- **Trust ceiling — stated honestly**: a worker running as the same OS
  user could fabricate or truncate this file. Therefore the JSONL is
  **never authoritative for terminal outcomes** (finding 4): it proves
  acceptance and progress only. `ReachableTerminal` requires either the
  process's own observed `result` event (stdout, in-life) or a later
  operator-authorized recovery probe on the exact session whose own
  `result` event provides authoritative evidence. Otherwise recovery stays
  **Uncertain** — including when the JSONL shows assistant entries after
  the prompt (an assistant entry may be a mid-work tool request; a
  terminal stdout event is the only completion proof, and it cannot be
  reconstructed from disk).

### 3.7 Launch seam (finding 6)

- **`ClaudeTurnLaunchSource`** (service-owned, storage-backed deep module):
  builds the complete `LaunchRequest` from the frozen run profile and the
  AC-005 workspace allocation — command, exact arguments (§3.8), cwd =
  workspace root, `CLAUDE_CONFIG_DIR` env, `--max-turns` from the frozen
  bound, model from `HarnessProfileSpec`. The adapter never synthesizes
  policy inputs.
- **Executor validation**: the executor validates the exact Claude
  invocation shape — argument list must equal the approved template with
  only the parameterized slots (native ID, model, max-turns, prompt via
  stdin or argv) varying; forbidden flags (`--bare`, `--continue`,
  `--fork-session`, `--no-session-persistence`, both bypass-permission
  flags, `--permission-mode` with non-default values), reordered,
  duplicated, or extra arguments are rejected before launch.
- **Probe launches**: a separate operator-owned `ClaudeProbeLaunchTemplate`
  (mirroring AC-007) produces the version check (`claude --version`,
  provider-free) in an operator-allocated isolated scratch directory; the
  adapter appends nothing and never allocates directories itself.

### 3.8 Headless permission handling (finding 7)

- Council launches with the native **default permission mode** (no
  `--permission-mode` flag) — verified table §2.3: safe tools execute;
  would-prompt tools are immediately denied with a structured
  `tool_result`; nothing blocks.
- Outcome mapping (each verified):
  - approval-needed denial ("requested permissions … haven't granted") →
    **ApprovalDenied** event class — distinct from:
  - guardrail hook denial ("PreToolUse hook error: … Guardrail denied") →
    **GuardrailDenied**; and from:
  - deny-list denial ("No such tool available: … disabled") →
    **ToolDisabled**; and from:
  - model refusal with no tool call → ordinary `success` output.
- Every invocation carries the frozen `--max-turns` bound and a process
  deadline; timeouts classify as Uncertain (§3.9).

### 3.9 Stream and process failure semantics (finding 8)

| Condition | Classification |
|---|---|
| Malformed NDJSON line mid-stream (not the final line) | stream poisoned → turn Uncertain (native state unknown) |
| Oversized NDJSON line (> bound) | stream poisoned → Uncertain; process terminated |
| Multiple/duplicate `result` events in one invocation | first observed `result` wins; duplicates logged; outcome stands |
| No `result` before stdout EOF | Uncertain (process dead without terminal proof) |
| stderr output | drained for process lifetime (bounded tail kept for diagnostics), never parsed as events |
| stdout truncation (partial final line at EOF) | line discarded; if no `result` seen → Uncertain |
| Nonzero exit WITH observed `result` | outcome per the `result` (exit code secondary) |
| Nonzero exit WITHOUT `result` | Uncertain |
| Exit 0 WITHOUT `result` | Uncertain (no terminal proof) |
| Context cancellation (caller) | tap detach only (observation), process continues |
| Cancel (Council) | graceful terminate → kill; no `result` seen ⇒ Uncertain |
| Deadline exceeded | terminate; Uncertain |
| Process-start success followed by immediate failure | accepted/uncertain per §3.5 no-redispatch rule (baseline decides acceptance) |
| Executor start failure (process never started) | Rejected — DefinitivelyMissing evidence (pre-acceptance) |
| Observer detachment | taps detach; the stream reader runs for the process lifetime |

Any started process without a verified terminal `result` remains
Uncertain; reconciliation may later resolve it per §3.5/§3.10.

### 3.10 Reconciliation evidence rules

| Evidence | Verdict |
|---|---|
| `result` event observed in-life | terminal (durable outcome committed) |
| Reconciliation launch's own `result` event on the exact session (authorized recovery probe) | ReachableTerminal with that outcome |
| Transcript advanced past baseline (accepted) without terminal proof | Uncertain (host lost) — never fabricated as terminal |
| Baseline unchanged AND process known dead AND acceptance impossible | DefinitivelyMissing |
| Transport/process-start failures before acceptance | DefinitivelyMissing (pre-acceptance rejection recorded) |

## 4. Evidence plan (fixtures vs integration)

- **Fixture layer** (CI, provider-free): a fake `claude` executable via the
  PolicyExecutor replaying recorded stream-json fixtures (init, assistant,
  denials, results, malformed/oversized/truncated streams, multi-result)
  plus a fixture session store exercising the JSONL trust model — full
  adapter contract exactly as AC-007 did with its fake server.
  Cross-platform except process fixtures (POSIX-scoped).
- **Integration layer** (manual, sanitized, operator-invoked, excluded
  from CI): `scripts/ac008-integration-evidence.sh` — real binary, real
  login, sanitized captures; never selects a provider/model beyond the
  operator's explicit step.

## 5. Boundaries

- Never `-c/--continue`, `--fork-session`, bypass-permission flags,
  `--bare`, `--no-session-persistence`, or non-default permission modes.
- Never read/copy native credentials; `apiKeySource` observation only.
- Every process launch through the AC-005 executor with the workspace root
  pinned and `CLAUDE_CONFIG_DIR` pointed at the service-owned root.
- Guardrails and hooks remain enabled; denials are recorded honestly.
- JSONL is acceptance/progress evidence only, never terminal proof.

## 6. Acceptance mapping (issue #8)

| Acceptance criterion | Where |
|---|---|
| Probe installed arguments; capture model/version | §3.7 probe template; `--version` + contract checks |
| Exact create/resume with approved config + native login | §3.3–§3.4; fixtures + manual script |
| Structured output; denials vs completions distinguished | §2.3/§3.8 four-class mapping; fixture regressions |
| Bounded invocation/cancellation; reconcile uncertain before retry | §3.9 state table; §3.10 rules |
| Toolkit-loading + recovery evidence; output fixtures | §2.2 init capture compared against frozen expectations; fixture + manual layers |

### 6.1 Concrete acceptance scenarios (finding: additions)

1. **Concurrent duplicate dispatch** — N callers, one TurnRef: one native
   process, shared verdict, single-flight per native session.
2. **Crash after process start** — process killed post-acceptance,
   pre-result: attempt stays Uncertain; no redispatch until reconciliation.
3. **Orphan process after daemon death** — daemon killed with a live
   child: restart finds the attempt unaccounted; transcript cursor proves
   acceptance; outcome stays Uncertain pending an authorized probe.
4. **Truncated JSONL** — torn final line: acceptance evidence still parses;
   no terminal claim.
5. **Exact-session mismatch** — resume against a transcript whose recorded
   first-prompt hash mismatches the binding: ResumeSession fails closed.
6. **Approval-required behavior** — would-prompt tool under `-p`:
   ApprovalDenied classification, bounded run, no hang.
7. **Missing toolkit elements** — `system/init` missing expected
   frozen-profile elements (skills/plugins enumerated): toolkit-evidence
   check fails the session evidence (operator-visible), never silently
   accepted.
8. **Sibling-history denial** — worker attempt to read another session's
   transcript path is denied by guardrail/executor policy and, at the
   adapter layer, derived paths are validated to address ONLY the bound
   session's file.
