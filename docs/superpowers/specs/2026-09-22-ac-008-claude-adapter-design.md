# AC-008 Design — Claude persistent contributor adapter

Status: DRAFT for review
Date: 2026-09-22
Issue: #8
Depends on: AC-003 (controller grants), AC-005 (workspaces/execution policy),
AC-006 (adapter contract)

## 1. Problem and scope

Council needs Claude as a persistent contributor: an adapter that drives the
installed Claude Code CLI headlessly, preserves the operator's shared
toolkit (hooks, skills, plugins, guardrails), maintains an exact native
conversation identity across turns, captures structured outcomes
(completions, tool denials, approval needs), bounds invocations, cancels
them, and reconciles uncertain completions before any retry.

Out of scope: Codex/Agy adapters (AC-009+), live-provider acceptance runs
(operator-invoked only), and any startup mode that silently omits hooks and
skills.

## 2. Installed-interface research (verified Claude Code 2.1.278)

All findings below were probed against the installed binary
(`~/.local/bin/claude`, `claude --version` → `2.1.278 (Claude Code)`).

### 2.1 Verified invocation surface

| CLI surface | Verified behavior |
|---|---|
| `claude -p/--print` | Non-interactive one-shot turn: prompt in, result out |
| `--output-format stream-json` (+ required `--verbose` in print mode) | Realtime newline-delimited JSON event stream |
| `--input-format stream-json` | Structured stdin (multi-turn within one process) |
| `--session-id <uuid>` | Caller-chosen session identity; **must be a valid UUID** (verified: `not-a-uuid` → `Invalid session ID. Must be a valid UUID.`) |
| `--resume <session-id>` | Exact resumption; verified to continue under the SAME session UUID (init/assistant/result events all carry the original id) |
| `--resume <missing-id>` | Deterministic failure: `No conversation found with session ID: <id>` |
| `--session-id` + `--resume` without `--fork-session` | Rejected: `--session-id can only be used with --continue or --resume if --fork-session is also specified` (so plain `--resume` preserves identity; `--fork-session` is never used by Council) |
| `-c/--continue` | Continues the MOST RECENT conversation — **forbidden** for Council (exact-session rule, mirrors AC-004/§3.5) |
| `--model <model>` | Model preset for the session (verified `haiku` → `claude-haiku-4-5-20251001` in init) |
| `--max-turns <n>` | Hard bound on agentic turns; exceeded → `result` with `subtype:"error_max_turns"`, `is_error:true` (verified) |
| `--permission-mode` | Choices: `acceptEdits`, `auto`, `bypassPermissions`, `manual`, `dontAsk`, `plan` — Council uses **`manual`** only; `bypassPermissions`/`auto` are **forbidden** (guardrail bypass) |
| `--permission-prompt-tool <mcp-tool\|host\|none>` | Permission prompt routing; `none` expects a literal MCP tool and is NOT a usable deny-all (verified error) — denial handling relies on the permission mode plus structured `tool_result` outcomes, not this flag |
| `--disallowedTools <tools...>` | Session deny-list; verified structured denial: `tool_result` with `is_error:true`, `Error: No such tool available: Bash. Bash is disabled for this session…` |
| `--dangerously-skip-permissions`, `--allow-dangerously-skip-permissions` | **Forbidden** (guardrail bypass) |
| `--bare` | Skips hooks, LSP, plugins, CLAUDE.md discovery — **forbidden** (silently omits the shared toolkit) |
| `--no-session-persistence` | **Forbidden** (Council requires persisted native sessions) |

### 2.2 Verified stream-json event surface (live probe, native login)

One minimal probe per shape; captured on this installation:

- `system/init`: `session_id`, `tools[]`, `mcp_servers[]`, `model`,
  `permissionMode`, `slash_commands`, `skills`, `plugins`,
  `claude_code_version`, `agents`, `cwd`. **This is the toolkit-loading
  evidence channel**: the operator's SessionStart hooks fire
  (`system/hook_started` / `system/hook_response` events observed, including
  the operator's guardrail and superpowers hooks), and the loaded
  skills/plugins/MCP servers are enumerated.
- `assistant`: full message envelope with `content[]`
  (`text`/`tool_use` blocks), `usage`, `session_id`.
- `user`: tool results — `content[].tool_result` with
  `is_error:true/false`; **structured denial channel** (verified both a
  deny-list denial and a guardrail PreToolUse hook denial surfacing this
  way).
- `result` (terminal): `subtype` (`success` / `error_max_turns` / …),
  `is_error`, `result` text, `total_cost_usd`, `usage`,
  `duration_api_ms`, `session_id`.
- `rate_limit_event`, `system/thinking_tokens`: advisory stream events.

### 2.3 Verified behaviors and hazards

- **Session store**: `~/.claude/projects/<munged-cwd>/<session-id>.jsonl` —
  project-scoped by working directory; resumption must run with the same
  cwd (the AC-005 workspace root) or the session will not be found.
- **Native authentication**: API-key/OAuth state is the operator's; Council
  never copies or inspects credentials. `system/init.apiKeySource` is
  observable without reading secrets. An unauthenticated native side fails
  at invocation and is reported honestly.
- **Model-level refusal vs structured denial**: a model may decline a
  prompt without any tool call (observed); this is a `success` result with
  declining text — Council treats it as a completed turn with that output,
  never as a fabricated tool denial.
- **`error_max_turns`** is a bounded non-completion: `is_error:true` —
  retry policy handles it as a failed (not uncertain) turn.

## 3. Architecture

### 3.1 Process-per-turn, not a server

Unlike OpenCode's long-lived serve child, Claude turns are **one CLI
process per dispatched turn**: `claude -p --resume <native-id> …` with
stream-json on stdout. Consequences:

- No server manager, no credentials generation, no health checks. The
  AC-005 `PolicyExecutor` launches every process (never `os/exec`), with
  the session's workspace root as the pinned working directory.
- The native session is born on the first dispatched turn with
  `--session-id <council-generated uuid>`; Council generates a UUIDv4 per
  session because the CLI accepts caller-chosen UUIDs (verified) — this is
  the native identity mechanism, recorded in the persisted binding.
- Subsequent turns resume with `--resume <native-id>` (never `-c`).

### 3.2 Adapter (`internal/adapter/claude/adapter.go`)

Implements the AC-006 contract:

- `Probe`: `claude --version` through the executor (provider-free) +
  `--help` contract checks against the verified surface. Model inventory
  is not queryable provider-free — reported as unavailable honestly; model
  validation happens at CreateSession/first dispatch and fails closed on
  CLI rejection of an unknown model alias.
- `CreateSession`: validate + persist the binding (council-generated UUID
  as native ID, frozen model, workspace root). No native call yet — the
  session materializes on first dispatch; `ResumeSession` verifies via a
  zero-cost resumption check (`--resume <missing>` fails deterministically,
  so a real session's file plus a resume-ready binding is the verification
  path) and fails closed with `ErrNativeSessionMissing` on the deterministic
  missing-session error.
- `Dispatch`: single-flight per native session (AC-007 semantics carry
  over); spawn `claude -p --resume|--session-id … --model … --permission-mode
  manual --max-turns <bound>` via the executor; parse the stream.
- `Observe`: taps on the pump — here the "pump" is the turn's own stream
  reader (process lifetime), so per-turn streams are native; taps preserve
  AC-007 detach semantics where the process outlives an observer.
- `Collect`: correlates the terminal `result` event (exactly one per
  invocation) with the TurnRef; `success` → completed with output/usage;
  `error_max_turns`/`is_error` → failed.
- `Cancel`: terminate the process (graceful → kill, pipes drained). A
  turn killed mid-flight is **uncertain** — the model may have persisted
  session state before death.
- `Reconcile`: §3.5 rules driven by the session JSONL + a resume probe.

### 3.3 Turn identity and correlation

The native message ID concept does not exist — Claude assigns message UUIDs
internally. Correlation is by **(native session UUID, invocation)**: each
dispatch is exactly one CLI invocation whose `result` event carries the
same `session_id`. The deterministic digest from AC-007 is not needed;
instead the adapter records per dispatch: native session ID, process
identity, and the observed terminal event. Attempt identity comes from the
storage dispatch intent as in AC-007.

### 3.4 Structured outcomes

| Native evidence | Council classification |
|---|---|
| `result` `subtype:"success"`, `is_error:false` | TurnCompleted with `result` text + usage/cost |
| `result` `subtype:"error_max_turns"` (or any `is_error:true` result) | TurnFailed (bounded non-completion) |
| `tool_result` with `is_error:true` (deny-list or guardrail PreToolUse denial) | tool_denied event on the turn stream; the turn continues honestly |
| Guardrail/other hooks firing (`hook_started`/`hook_response`) | toolkit-loaded evidence; mirrored as progress events |

### 3.5 Reconciliation evidence rules

| Evidence | Verdict |
|---|---|
| Process exited with a `result` event observed | terminal (durable outcome already committed) |
| Process terminated by Council/timeout, no `result` observed | Uncertain (host lost): the model may have persisted partial work |
| Resume probe shows the session's JSONL advanced past the dispatched prompt with a terminal assistant entry | ReachableTerminal with the recovered outcome |
| Dispatch never accepted (pre-write rejection: executor start failure) | DefinitivelyMissing |

### 3.6 Permission semantics

`--permission-mode manual` with NO bypass flags. Denials arrive as
structured `tool_result` errors (verified for deny-list and guardrail
hook denials). Council denies nothing itself — the operator's guardrail
hooks and native permission mode decide; Council observes and records.
No `--allow/--disallow` grants beyond the frozen profile, no bypass flags,
no `--bare`.

### 3.7 Toolkit and guardrail preservation

The adapter adds no flags that filter the toolkit: no `--bare`, no
`--strict-mcp-config`, no tool allowlists. `system/init` is captured per
session and asserted in evidence to prove hooks/skills/plugins loaded
(the AC-008 "real toolkit-loading evidence" requirement).

## 4. Evidence plan (fixtures vs integration)

- Fixture layer (CI, provider-free): a fake `claude` executable via the
  PolicyExecutor emitting recorded stream-json fixtures (init/assistant/
  result/denials) — exercises the full adapter contract exactly as AC-007
  did with its fake server. Cross-platform except process fixtures
  (POSIX-scoped).
- Integration layer (manual, sanitized, operator-invoked):
  `scripts/ac008-integration-evidence.sh` — real binary, real login,
  sanitized captures; never run by CI; never selects a provider/model
  beyond the operator's explicit step.

## 5. Boundaries

- Never `-c/--continue`, never `--fork-session`, never bypass permissions,
  never `--bare`, never `--no-session-persistence`.
- Never read/copy native credentials; `apiKeySource` observation only.
- Every process launch through the AC-005 executor with the workspace root
  pinned.
- Guardrails and hooks remain enabled; denials are recorded honestly.

## 6. Acceptance mapping (issue #8)

| Acceptance criterion | Where |
|---|---|
| Probe installed arguments; capture model/version | §3.2 Probe; probe_test |
| Exact create/resume with approved config + native login | §3.1/§3.2; fixtures + manual script |
| Structured output; denials vs completions distinguished | §3.4; fixture regressions |
| Bounded invocation/cancellation; reconcile uncertain before retry | §3.2 Cancel/§3.5; fixture regressions |
| Toolkit-loading + recovery evidence; output fixtures | §3.7; init-capture fixtures; manual script |
