# AC-009 evidence: installed Codex CLI interface research (App Server vs `exec`)

Date: 2026-09-23. Binary: `/usr/bin/codex`, verified `codex-cli 0.154.0`
(`codex doctor` also reports `Codex Doctor v0.154.0 · linux-x86_64`, Ubuntu
24.4.0, npm-installed vendored musl binary; `0.156.1 available` was advertised —
not installed, out of scope).

Method statement: **every claim in this file is tagged** `live-verified
(0.154.0)` (executed against the installed binary), `binary-derived
(provider-free)` (read from the binary's own generated protocol schemas, help
text, or rollout files — no model call), or `not verified`. Official docs were
NOT consulted: egress to `developers.openai.com` is denied by the repo
guardrail (rule `P6.egress`) and native webfetch was denied
(`web-fetch-native-deny`). There are therefore **no docs-only claims**;
protocol shape comes from `codex app-server generate-json-schema`, which is
generated from the installed binary's own protocol types and is treated as
authoritative for 0.154.0.

Safety rules followed:

- Bypass/danger flags (`--dangerously-bypass-approvals-and-sandbox`,
  `--dangerously-bypass-hook-trust`, `danger-full-access`, `--full-auto`,
  `--yolo` equivalents) were **never executed**; they appear below only as
  help-text evidence for the forbidden-flags list.
- `~/.codex/auth.json` and all credential material were never read, copied, or
  symlinked. Only `codex login status` output and `~/.codex/config.toml` (plus
  directory listings and rollout JSONL produced by this research's own probes)
  were inspected.
- Live model invocations: **6 attempts total** (the cap). Of these, 2 failed on
  auth in an isolated `CODEX_HOME` without consuming tokens, 3 were trivial
  single-turn prompts, and 1 was an unintended live call caused by a
  resume-argument hazard documented in §2 (kept and reported honestly).
- Thread-creating probes ran under `CODEX_HOME=$(mktemp -d)`-style isolation
  where auth was not required. Three probes deliberately ran against the
  operator's real `CODEX_HOME` because ChatGPT auth is bound to it and copying
  credentials is forbidden; these created three new session files under
  `~/.codex/sessions/2026/09/23/` (thread ids `01a0cd13…`, `01a0cd15…`,
  `01a0cd18…`) and one failed-auth resume attempt appended to the isolated
  probe-1 rollout. Nothing under `~/.codex` was modified or deleted beyond
  these normal session creations; no logout, no config edits.

---

## §1 Surface inventory (`codex --help`, live-verified)

| Subcommand | Verified role | Adapter relevance |
|---|---|---|
| `exec` (alias `e`) | Non-interactive run; subcommands `resume`, `fork`, `review` | Candidate transport (CLI) |
| `app-server` `[experimental]` | JSON-RPC server; subcommands `daemon`, `proxy`, `generate-ts`, `generate-json-schema` | Candidate transport (protocol) |
| `app-server daemon start/stop/restart/bootstrap/version` | Shared local daemon management | Persistence shape; one daemon serves many clients |
| `app-server proxy` | "Proxy stdio bytes to the running app-server control socket" | Attach to the shared daemon over stdio |
| `resume` | TUI picker resume; `--last` continues most recent | **Forbidden for Council** (identity must be exact, not "most recent") |
| `exec resume <id>` / `exec resume --last` | Non-interactive exact-id resume | Candidate resume path (CLI) |
| `exec fork <id>`, `fork`, `queue`, `archive`, `unarchive`, `delete`, `migrate-rollouts` | Fork into new session; queue message into existing session; session lifecycle | `queue` overlaps Council's parked-prompt shape; `archive`/`delete` map to evidence/purge separation |
| `agents` | "Browse all agent sessions on the shared local app-server daemon" | Observability of daemon-hosted threads |
| `login`, `logout` | Auth management | `login status` used as auth evidence |
| `mcp` | `list/get/add/remove/login/logout` for configured MCP servers | Toolkit inventory evidence |
| `plugin`, `features list`, `doctor`, `debug` (`models`, `app-server`, `prompt-input`) | Plugin mgmt; feature flags (`codex features list`, provider-free); diagnostics; raw model catalog JSON | Provider-free model inventory (`debug models`) |
| `sandbox` | "Run commands within a Codex-provided sandbox" | Provider-free sandbox smoke test |
| `apply` (alias `a`), `cloud`, `exec-server`, `remote-control`, `review`, `completion`, `update` | Diff application; Codex Cloud; standalone exec-server; daemon remote control | Rejected: out of Council scope |
| global `-c/--config`, `--enable/--disable <FEATURE>`, `-p/--profile`, `--strict-config`, `-m`, `-s`, `-C`, `-a` | Config override surface | Launch-shape surface; see §5 |

Sandbox choice set (`-s/--sandbox`): `read-only`, `workspace-write`,
`danger-full-access` (help text, live-verified). Approval choice set
(`-a/--ask-for-approval`, present on `codex` and `codex resume` but **absent
from `codex exec`**): `on-request` ("The model decides when to ask the user for
approval"), `never` ("Never ask for user approval Execution failures are
immediately returned to the model"). `exec` instead has `--approve-for-me`
("Route approval requests through automatic review using the workspace-write
sandbox"). Forbidden variants observed in help (never executed):
`--dangerously-bypass-approvals-and-sandbox`, `--dangerously-bypass-hook-trust`.

## §2 Thread identity & exact resumption

| Question | Verified answer |
|---|---|
| Caller-chosen thread id? | **No.** `codex exec` has no session-id input; `thread/start` params have no id field (binary-derived schema: all fields nullable, `required: None`). Ids are server-generated UUIDv7 (`01a0cd13-8afd-7283-a4bf-4a36b3d39a61`). `exec --json` emits `thread.started {thread_id}` as the first event — this is the creation handle (live-verified). |
| Rollout location | `$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ISO-ts>-<thread-uuid>.jsonl`, e.g. `rollout-2026-09-23T01-03-25-01a0cd13-8afd-7283-a4bf-4a36b3d39a61.jsonl` (live-verified in both isolated and real homes). Filename UUID == `session_meta.session_id` == `thread.started.thread_id`. |
| Exact resume | `codex exec resume <UUID> [PROMPT]` (live-verified): emitted `thread.started` with the **same id**, appended to the **same rollout file** (17→29 lines, same inode path, no new file), and answered "4" to "What is 2+2?" proving context carry (usage grew 15166→30498 input tokens). |
| Resume flag restrictions | `exec resume` **rejects** `--sandbox` (verbatim: `error: unexpected argument '--sandbox' found`) and `-C` (verbatim: `error: unexpected argument '-C' found`); it re-runs the trust check (`Not inside a trusted directory and --skip-git-repo-check was not specified.`). Resumed threads re-derive effective policy from **current** config, not the recorded turn (§5). |
| Missing-id error | `codex exec resume 00000000-0000-0000-0000-000000000000` → verbatim `Error: thread/resume: thread/resume failed: no rollout found for thread id 00000000-0000-0000-0000-000000000000 (code -32600)` (live-verified; provider-free — fails before any model call). CLI resume internally calls app-server `thread/resume`. |
| Most-recent forms (forbidden for Council) | `codex resume` (TUI picker), `codex resume --last`, `codex exec resume --last`, `--include-non-interactive`, cwd-filtered picker. `exec resume --last` live-verified to resolve the newest rollout even when that turn previously failed on auth. All are identity-ambiguous → rejected. |
| **Non-UUID hazard** | `codex exec resume definitely-not-a-real-session-id "hi"` did **not** error: it silently created a **new thread** (`01a0cd15-…`) and ran a live turn. Help says ids are "UUIDs take precedence if it parses" — non-UUID input is not rejected. Council must pass canonical UUIDs only and treat any resume output whose `thread.started.thread_id` differs from the requested id as a protocol violation (fail-closed). (live-verified, consumed the one unintended model call) |
| New prompt required? | For exec: yes — resume requires a PROMPT arg (or stdin); there is no provider-free exec liveness check beyond the missing-id error above. For app-server: **yes, provider-free verification exists** — `thread/resume {threadId, excludeTurns:true}` returns thread + effective config with no turn (live-verified, §3). |

## §3 App Server protocol findings

Transport (live-verified): `codex app-server` speaks **JSON-RPC 2.0, one
newline-terminated JSON object per message, over stdio** (default
`--listen stdio://`; `unix://`, `unix://PATH`, `ws://IP:PORT`, `off` also
accepted per help). No port is opened in stdio mode. `--listen ws://` requires
auth material (`--ws-auth capability-token|signed-bearer-token`,
`--ws-token-file`, …) — out of scope, never exercised.

### 3.1 Handshake (live-verified)

```
→ {"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{"clientInfo":{"name":"ac009-research","title":"AC009 protocol probe","version":"0.0.1"}}}
← {"id":"init-1","result":{"userAgent":"ac009-research/0.154.0 (Ubuntu 24.4.0; x86_64) WindowsTerminal (ac009-research; 0.0.1)","codexHome":"/tmp/opencode/ac009-appserver-home","platformFamily":"unix","platformOs":"linux"}}
```

Missing params → verbatim `{"error":{"code":-32600,"message":"Invalid request:
missing field \`clientInfo\`"}}`. `initialize` result exposes `codexHome` and
platform — a provider-free environment attestation surface. Schema also
documents optional `capabilities` (incl. `optOutNotificationMethods`,
`experimentalApi`) (binary-derived).

### 3.2 Method existence (live-verified unless noted)

| Method | Result |
|---|---|
| `thread/start` | Works with `{}`: creates a **local thread object, no model call**. Returns full Thread: `id` == `sessionId`, `environments:[{environmentId:"local", cwd, runtimeWorkspaceRoots}]`, `status {type:"idle"}`, `model:"gpt-6-astra"`, `historyMode:"paginated"`, `path` (session file). Emits `thread/started` notification. |
| `thread/resume {threadId, excludeTurns:true}` | **Provider-free verification of an existing thread** (live-verified against the exec-created thread `01a0cd13-…` in the operator home): returned `thread.id`, `status {type:"idle"}`, `cwd`, `approvalPolicy`, `sandbox`, `approvalsReviewer`, `model`, `modelProvider`, `instructionSources`, `initialTurnsPage`, `itemsBackwardsCursor`, `turnsBackwardsCursor`. No turn sent, no model call. This is the decisive ResumeSession primitive. |
| `thread/resume` (fake id) | Verbatim `{"error":{"code":-32600,"message":"no rollout found for thread id 00000000-0000-0000-0000-000000000001"}}` — deterministic ErrNativeSessionMissing analog. |
| `thread/list` | Paginated `{data, nextCursor, backwardsCursor}`; unloaded entries carry `status {type:"notLoaded"}` and null environments; default listing did NOT include the probe's own `/tmp/opencode` thread on its first page (cwd-scoped filtering suspected — matches the resume picker's documented `--all disables cwd filtering`). |
| `thread/loaded/list` | `{data:["01a0cd13-…"]}` — per-process loaded-thread state after resume. |
| `model/list` | Full catalog without a model call (matches `codex debug models`, e.g. `gpt-6-astra`). |
| `account/read` | `{account:null, requiresOpenaiAuth:true}` under isolated home — clean unauth indicator (live-verified); operator home returns account info (not dumped). |
| `turn/start` / `turn/interrupt` (missing thread) | Verbatim `-32600 "thread not found: <id>"`. Live-turn invocation NOT exercised (probe budget). |
| `turn/interrupt` params | `{threadId, turnId}` required (binary-derived schema) — **turn-scoped** interrupt, not thread-scoped. |
| unknown method | Verbatim `-32600 "Invalid request: unknown variant …, expected one of …"` enumerating the **authoritative live method universe** (160+ names; full list committed in `ac009-native-event-universe-0.154.0.json`), including legacy `getConversationSummary`, `getAuthStatus`, `gitDiffToRemote`, plus `turn/steer`, `thread/rollback`, `thread/revert`, `thread/turns/list`, `thread/items/list`, `config/read`, `config/value/write`, `command/exec*`, `process/*`, `mcpServerStatus/list`. |

### 3.3 Server→client requests (approvals) — binary-derived, not live-exercised

`codex app-server generate-json-schema --out <DIR>` (provider-free) emits the
complete protocol schema set, including server→client REQUESTS:
`execCommandApproval` (params: `callId`, `command[]`, `conversationId`, `cwd`,
`parsedCmd[]`, `reason?`, `approvalId?`), `applyPatchApproval` (`callId`,
`conversationId`, `fileChanges`, `grantRoot?`, `reason?`), plus newer
`item/commandExecution/requestApproval`, `item/fileChange/requestApproval`,
`item/permissions/requestApproval`, `item/tool/requestUserInput`,
`mcpServer/elicitation/request`, `item/tool/call`. Correlation fields (`callId`,
`conversationId`, `approvalId`) are present — structured approval routing is
protocol-native. **No live turn was driven to an approval decision in this
research** (probe budget); deny-path behavior over app-server is therefore
**not verified**.

### 3.4 Notifications (live-observed: `thread/started`, `remoteControl/status/changed`; binary-derived: the rest)

Schema-derived notification surface relevant to Council: `turn/started`,
`turn/completed {threadId, turn{id, items[], status, durationMs?, error?}}`,
`turn/interrupted`-shaped `TurnStatus: completed|interrupted|failed|inProgress`,
`item/started`, `item/completed`, `item/agentMessage/delta`,
`item/commandExecution/outputDelta`, `item/reasoning/*Delta`,
`thread/tokenUsage/updated {threadId, turnId, tokenUsage{last,total}}` with
`TokenUsageBreakdown {inputTokens, cachedInputTokens, cacheWriteInputTokens,
outputTokens, reasoningOutputTokens, totalTokens}` — **token counts only; no
cost field exists anywhere in the schema** (binary-derived). Turn ids are
UUIDv7 (`Turn.id`: "Codex-generated turn IDs are UUIDv7").

### 3.5 Lifecycle (live-verified)

The app-server child process was terminated by SIGTERM from the driver and
died with it; stdio-only default means no port. `app-server daemon start`
additionally offers a **shared local daemon** (`codex agents` browses sessions
on it; `app-server proxy` forwards stdio to its control socket) — the daemon,
not the parent process, can own thread lifetime. `app-server daemon version`
prints "local CLI and running app-server versions as JSON" — a version-skew
probe surface.

## §4 `codex exec` JSON event surface

Flags (live-verified help): `--json` ("Print events to stdout as JSONL"),
`-o/--output-last-message <FILE>`, `--output-schema <FILE>`, `-m/--model`,
`-s/--sandbox`, `-C/--cd`, `-p/--profile`, `-c/--config key=value`,
`--skip-git-repo-check`, `--ephemeral` ("Run without persisting session files
to disk"), `--ignore-user-config`, `--ignore-rules`, `--thread-source`,
`--worktree`, `--add-dir`, `--strict-config`, `--enable/--disable`.
**`exec` has no `-a/--ask-for-approval`** — it is non-interactive; with no
explicit policy a clean home records `approval_policy:"never"`, while the
operator's config recorded `approval_policy:"on-request"` with
`approvals_reviewer:"auto_review"` (both live-verified via rollout
`turn_context`).

### 4.1 Observed event sequence, trivial successful turn (live-verified)

Command: `codex exec --json --sandbox read-only --skip-git-repo-check -C
/tmp/opencode "Reply with exactly: OK"`. Ordered stdout events (sanitized):

```
thread.started {"type":"thread.started","thread_id":"01a0cd13-8afd-7283-a4bf-4a36b3d39a61"}
turn.started   {"type":"turn.started"}
item.completed {"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"OK"}}
turn.completed {"type":"turn.completed","usage":{"input_tokens":15166,"cached_input_tokens":11136,"cache_write_input_tokens":0,"output_tokens":5,"reasoning_output_tokens":0}}
```

Failure turn (isolated home, no auth): `thread.started` → `turn.started` →
`error {message:"Reconnecting… k/5 (unexpected status 401 Unauthorized…)"}`
×5 → `item.completed {item.type:"error"}` ("Falling back from WebSockets to
HTTPS transport…") → 5 more `error` lines → `turn.failed {error{message}}`,
exit 1. Transport detail (live-verified): Codex first attempts a **WebSocket**
(`wss://api.openai.com/v1/responses`), then falls back to HTTPS.

The stdout stream is a **reduced view**: in the denial probe the stdout showed
only agent_message items, while the rollout recorded the full structured
`custom_tool_call` (name `exec`) + `custom_tool_call_output` with
`"exit_code":1` and stderr `"zsh:2: read-only file system:
/tmp/should-not-exist-ac009.txt"`. Structured permission outcomes live in the
rollout (and, per schema, in app-server `item/*` notifications), not in exec
stdout. (live-verified)

### 4.2 Denial probe (live-verified)

`--sandbox read-only` + prompt to write `/tmp/should-not-exist-ac009.txt`: the
sandbox denied the write at the FS level (`Read-only file system`), the error
was returned to the model, the model reported the failure, `turn.completed`
(with usage) — **exit code 0, file NOT created**. No approval request was
raised (read-only mode blocks; it does not ask). Matches the provider-free
smoke test `codex sandbox /bin/sh -c "touch /tmp/…"` → `touch: cannot touch
'…': Read-only file system`.

### 4.3 Rollout terminal record (live-verified — differs from Claude)

The rollout JSONL records terminal state: `session_meta` (first line:
`session_id, id, timestamp, cwd, originator:"codex_exec", cli_version,
source:"exec", thread_source, model_provider, base_instructions, history_mode,
context_window`), `event_msg` subtypes `task_started`, `item_completed`,
`token_count` (total/last usage + `rate_limits` + `model_context_window`),
`token_usage_record`, and `task_complete` (`last_agent_message`,
`started_at`, `completed_at`, `duration_ms`, `time_to_first_token_ms`, and
`error` populated when the turn failed — verified on the 401 turn). Unlike the
Claude JSONL, **a Codex rollout carries an authoritative terminal record plus
usage**.

## §5 Sandbox, approvals, config, toolkit

- Sandbox values: `read-only`, `workspace-write`, `danger-full-access` (help).
  App-server `SandboxPolicy` adds structured variants: `readOnly
  {networkAccess}`, `workspaceWrite {writableRoots[], networkAccess,
  excludeSlashTmp, excludeTmpdirEnvVar}`, `externalSandbox`, `dangerFullAccess`
  (binary-derived). Landlock/Seatbelt are NOT named anywhere observed;
  the verified Linux mechanism evidence is the observable
  `Read-only file system` denial (`platformOs:"linux"` from initialize).
  Platform naming: `not verified`.
- Approval values (app-server `AskForApproval`): `untrusted`, `on-request`,
  `never`, plus a structured `granular` object (`mcp_elicitations`, `rules`,
  `sandbox_approval`, `request_permissions`, `skill_approval`);
  `ApprovalsReviewer`: `user` | `auto_review` | `guardian_subagent`
  (binary-derived). Exec surface: no `-a`; `--approve-for-me` only.
- `-c/--config key=value` overrides exist on every subcommand (help, incl.
  dotted paths and TOML parsing); `--strict-config` errors on unknown fields.
  Not separately exercised live.
- `CODEX_HOME` relocation: verified — isolated temp homes produced sessions
  under `$CODEX_HOME/sessions/...` and isolated auth state (`account/read` →
  `requiresOpenaiAuth:true`; model calls 401). Maps to per-session config
  roots, but auth does NOT follow (§7).
- `~/.codex/config.toml` (read, config only — no secrets): `model =
  "gpt-5.6-sol"`, `model_reasoning_effort = "low"`, `personality = "pragmatic"`,
  `approvals_reviewer = "auto_review"`, `[projects."<path>"] trust_level =
  trusted|untrusted` entries (source of the "Not inside a trusted directory"
  error), 4 `[mcp_servers.*]`, `[hooks.state]` trusted-hash entries.
- Toolkit evidence surface: `~/.codex/rules/` (`default.rules`,
  `guardrail.rules` — execpolicy), `hooks.json` + trusted-hash state, `skills/`
  (empty), no `prompts/` dir; `codex mcp list` (live-verified) prints server
  inventory; rollout `turn_context.permission_profile` records the effective
  managed profile (`{type:"managed", file_system:{type:"restricted",…},
  network:"restricted"}`); app-server resume returns `instructionSources` —
  "Environment-native paths to instruction source files currently loaded for
  this thread". So Council **can** verify: loaded instruction files, effective
  sandbox/approval profile, model, MCP server list (via `mcpServerStatus/list`,
  not live-exercised). Council **cannot** directly verify from exec stdout:
  toolkit/rule loading (stdout shows only agent/error items).
- Git check: `codex exec` outside a git repo fails with `Not inside a trusted
  directory and --skip-git-repo-check was not specified.` unless
  `--skip-git-repo-check` (live-verified; note trust-level also matters).

## §6 Interruption, reconnection, duplicate execution

- App-server has cooperative, turn-scoped `turn/interrupt {threadId, turnId}`
  and `turn/steer`; `TurnStatus` includes `interrupted` (binary-derived).
  NOT live-exercised (probe budget).
- Exec has **no cooperative cancel**: no interrupt flag exists in
  `codex exec --help`; mid-turn cancellation means killing the process
  (kill-and-resume). NOT live-exercised: no kill probe was run (live
  invocation cap). Expected per rollout structure: persisted `response_item`s
  survive; no `task_complete` line is written for the killed turn (terminal
  record only appears on completion/failure — inference from observed rollout
  shapes, `not verified` for the kill case).
- Duplicate-execution hazards: app-server turns have UUIDv7 ids and a per
  process `thread/loaded/list`; a killed exec leaves no lock evidence in what
  was observed (`thread-writer-locks/` exists under `~/.codex` — name only,
  not verified). There is **no client-side idempotency token** on `exec`:
  re-running the same command re-sends the prompt. Non-UUID resume silently
  creating a new thread (§2) is a second duplication path. Council must hold
  the process handle + rollout identity and fail closed on id drift.

## §7 Honest boundaries (not verified)

- **App-server live turn**: no `turn/start` was executed over app-server; the
  approval request/response loop (§3.3), notification stream, and
  `turn/interrupt` are schema-verified only.
- **Kill-mid-turn probe**: not executed (live cap). Exec interruption
  semantics are inferred, not observed.
- **Auth mechanics**: `codex login status` = "Logged in using ChatGPT";
  token storage/refresh was not inspected (forbidden). Live probes bound to
  the operator's home because auth cannot be relocated without touching
  credentials — meaning CODEX_HOME isolation and live auth are mutually
  exclusive; an adapter using isolated homes needs its own auth story.
- **thread/list default scoping**: the probe's own thread did not appear on
  the first page; cwd-filtering is suspected but not proven.
- **Usage/cost**: token usage (input/cached/cache-write/output/reasoning/
  total) is live-verified in both stdout and rollout; **cost is absent —
  unavailable** (maps to `UsageMetric.Available=false`). `account/usage/read`
  exists (server enumeration) but was not exercised.
- **Docs cross-check**: not performed (egress denied). Everything above stands
  on the installed binary alone; version drift beyond 0.154.0 (0.156.1
  advertised) is unassessed. `app-server` remains labeled `[experimental]`.
- **Windows/Seatbelt**, `exec fork` continuation, `queue` delivery semantics,
  `thread-writer-locks` behavior: not verified.

## §8 Adapter-implication notes (evidence, not decision)

- **(a) Fresh thread + exact resume**: both transports create fresh threads;
  exec returns the id via `thread.started` and resumes exactly via
  `exec resume <UUID>` (same-rollout append, live-verified). App-server
  `thread/start` → `thread/resume {threadId}` is the same story with a
  persistent process. Exact identity is achievable either way; both paths must
  assert the returned id equals the requested id (non-UUID hazard).
- **(b) Provider-free resume verification**: **app-server only.**
  `thread/resume {threadId, excludeTurns:true}` returns thread + effective
  config with no turn and no provider cost; exec's only check is the
  missing-id error (which does prove rollout existence, provider-free, via the
  `no rollout found … (code -32600)` text) but cannot return effective config
  or liveness without sending a prompt. CLI resume internally calls
  `thread/resume` anyway — the app-server surface is the substrate.
- **(c) Structured approval routing**: app-server protocol carries typed
  approval requests (`execCommandApproval`, `applyPatchApproval`, `item/*`
  variants) with call/thread correlation; exec is headless (no approvals;
  denials surface only as sandbox failures or auto-review). If Council needs
  controller-mediated approvals, app-server is the only verified route
  (shape verified; loop not live-exercised).
- **(d) Usage evidence**: both paths expose token usage (exec
  `turn.completed.usage` + rollout `token_count`; app-server
  `thread/tokenUsage/updated`). Cost: unavailable on both. Exec stdout omits
  commandExecution items — permission-outcome evidence requires rollout
  parsing on the CLI path, or app-server `item/*` notifications.
- **(e) Interruption semantics**: app-server has protocol-level
  `turn/interrupt {threadId, turnId}` and an `interrupted` turn status;
  exec supports only process kill (uncertain-outcome kill-and-resume). The
  shared daemon (`app-server daemon` + `proxy`) can outlive individual
  clients, matching "client disconnect is not cancellation".
- **Config drift on resume**: `thread/resume` re-derives effective
  `approvalPolicy`/`model`/`approvalsReviewer` from current config, not the
  original turn's recorded values (live-verified: exec recorded
  `gpt-6-astra`/never, resume reported `gpt-5.6-sol`/on-request/auto_review).
  An adapter must pin model/policy explicitly on every resume or accept
  drift; the frozen-profile invariant maps onto per-turn overrides
  (`turn/start` accepts per-turn `model`, `sandboxPolicy`,
  `approvalPolicy`, `cwd`).
- **Lifecycle**: app-server is a long-lived child (or daemon) with a version
  probe (`daemon version`); exec is per-turn process management (the AC-008
  reconciliation model). stdio-only default keeps the network surface empty.
