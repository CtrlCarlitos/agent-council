# AC-010 Design — Agy (Antigravity CLI) persistent contributor adapter

Status: DRAFT v3 for review (v1: 7 findings, v2: 5 findings — all
addressed; change logs at the end)
Date: 2026-09-24
Issue: #10
Depends on: AC-003 (controller grants), AC-005 (workspaces/execution policy),
AC-006 (adapter contract); patterns reused from AC-008 (process-per-turn,
stdin transport) and AC-009 (shared-home eligibility gate, cprot-v2
attestations, uncertainty episodes).
Evidence basis: `docs/superpowers/evidence/ac010-agy-installed-interface-research.md`
(binary-derived + provider-free probes on 1.2.7→1.2.9, plus 6 operator-
authorized live turns on 1.2.9, linux-x86_64).

## 1. Problem and scope

Council needs Agy as a persistent contributor: an adapter that drives the
installed Antigravity CLI headlessly, keeps the native conversation identity
exact across turns (fresh conversation for an independent proposal, exact
resumption for follow-ups), preserves the operator's configured permission
mode, sandbox, hooks, skills, plugins, and MCP servers (no bypass anywhere),
routes tool denials and outcomes as structured state rather than exit codes
or terminal text, bounds and cancels turns, prevents duplicate execution
across interruption and reconnection, and reports usage and toolkit
evidence with explicit availability boundaries.

Out of scope: the interactive TUI, `--remote-control`/the remote-control
daemon, `mic-serve`, `install`/`update`, plugin/MCP management commands,
`GEMINI_API_KEY` provider mode (the signed-in Antigravity backend is the
only provider path this design covers; API-key mode is `not verified`),
live-provider acceptance runs in CI (operator-invoked only), and OS-level
user isolation (owned by AC-005).

## 2. Installed-interface research (verified 1.2.9)

Decisive subset; full probes and the safety statement are in the evidence
file.

### 2.1 Invocation surface

| Surface | Verified behavior |
|---|---|
| `--print=` + `--input-format stream-json --output-format stream-json` | Headless turn driven by ONE NDJSON user message on stdin: `{"event":"user","message":{"content":"<text>"}}`; the process exits after the `result` event when stdin is closed. Valueless `--print` swallows the next flag as its prompt — the `--print=` form is pinned. |
| `init` event | Emitted at process start BEFORE stdin is read: `conversation_id`, `init.cwd`, `init.tools[]`, `init.permission_mode`, `init.model` (when `--model` given). Emitted even when not signed in. |
| conversation identity | CLI-generated UUID at process start; persisted as `~/.gemini/antigravity-cli/conversations/<uuid>.db` even when no turn runs. No caller-chosen id. |
| `--conversation <id>` | Exact resumption when the id exists (same id in `init`, context carried, step indices and `num_turns` continue). **Absent or malformed id ⇒ stderr warning and a silently NEW conversation, exit 0.** |
| `-c/--continue` | Most-recent conversation — **forbidden**. |
| `--dangerously-skip-permissions`, `--remote-control`, `-i` | **Forbidden** (bypass, remote channel, interactive). The operator's shell aliases `agy` to the bypass flag: the adapter never invokes through a shell and always uses the absolute binary path. |
| `--model <id>`, `--effort low|medium|high` | Model per session; effort is rejected pre-turn for models that do not support it (`result` ERROR, empty `conversation_id`, no file). |
| `--mode accept-edits|plan`, `--sandbox` | Execution mode and terminal sandbox flags; headless honoring is vendor-stated (`not verified` live for edits). |
| `--print-timeout <d>` | Backstop: expiry returns partial output with **exit 0** and a stderr `[agy] print timeout …` line, even when `result.status` is ERROR. |
| `--log-file <path>` | Council-owned per-launch log (glog text); never the operator's `cli.log`. |
| `--disable-slash-commands` | Pinned on: prompt text is never expanded as a slash command or skill. |
| exit codes | 0 success; 0 with `denied_actions`; 0 on print-timeout; 1 on SIGINT (`error:"interrupted"`) and on input validation errors. Never a classification input. |

### 2.2 stream-json events (live-verified)

- `step_update{conversation_id, step_index, state:ACTIVE|DONE, step_type, …}`
  with `step_type ∈ {user_input, agent_response(text_delta, usage),
  system_message, tool(tool_name, tool_info{name,parameters}),
  error_message}` (closed vocabulary per vendor; other values are drift).
- `result{conversation_id, status:SUCCESS|ERROR, response, error,
  duration_seconds, num_turns (cumulative), usage{input_tokens,
  output_tokens, thinking_tokens, cache_read_tokens, total_tokens}
  (cumulative), denied_actions[{action, display_name}]?}` — exactly one per
  process, also on pre-turn rejection.
- Unsupported input `event` values are IGNORED with a stderr warning (exit
  0); a missing `event`/`message` field or non-JSON fails closed (exit 1).

### 2.3 Headless permission behavior (decisive)

Default headless `permission_mode` is `request-review`; a tool needing a
permission is **auto-denied without waiting**, the turn still ends
`SUCCESS` with exit 0, and the denial is reported ONLY in
`result.denied_actions` plus a stderr explanation. The durable step row is
`status=6` (vs. `3` done). Permission allow-rules live in the operator's
`settings.json` (`permissions.allow`), which Council never writes. Modes seen
in the binary: `always-proceed`, `request-review`, `strict`,
`proceed-in-sandbox`; only `request-review` and `strict` are launchable.

### 2.4 Hazards carried into the design

1. Background auto-update replaced the binary (1.2.7→1.2.9) mid-research;
   no off switch found. 2. Silent new-conversation fallback on
   `--conversation`. 3. Unsupported input events silently ignored.
4. Exit 0 on denial and on print-timeout. 5. `init` proves nothing about
   auth. 6. Durable transcript is protobuf-in-SQLite with no pinned schema
   and no interruption marker. 7. Auth is keyring/home-bound: an isolated
   home is unauthenticated (AC-009 shared-home constraint).

## 3. Architecture

### 3.1 Process-per-turn, stdin-only transport (AC-008 pattern)

One CLI process per dispatched turn, launched through the AC-005
`PolicyExecutor` (never `os/exec`, never a shell), argv exactly:

```
<absolute binary path> --print= --input-format stream-json
  --output-format stream-json --disable-slash-commands
  --model <frozen> --print-timeout <frozen backstop> --log-file <launch log>
  [--mode <frozen>] [--sandbox] [--conversation <native id>]
```

- The prompt is NEVER an argv element. It is written to stdin as ONE
  NDJSON user message, then stdin is closed. The **first successfully
  written stdin byte** is the transmission boundary (AC-007/AC-008 rule).
- **Pre-transmission verification, every turn (first included):** the
  adapter arms the stdout reader, waits for `init` (bounded), and requires
  (a) `init.conversation_id == binding native id` (or, on creation, a
  well-formed UUID), (b) `init.permission_mode == frozen mode`, (c)
  `init.model == frozen model`, (d) `init.cwd == the AC-005 workspace
  root`, (e) `init.tools` set == frozen `expected_tools` set. Any
  mismatch ⇒ the prompt is NOT written, the child is terminated (SIGTERM
  → kill), and the outcome is a typed pre-acceptance rejection
  (`ErrConversationDrift`, `ErrProfileDrift`, `ErrToolInventoryDrift`).
  This closes hazard 2 (a fallback conversation is left as an orphan and
  its id is recorded as a diagnostic, never bound) and gives Agy the same
  "verified before the prompt exists" property Codex has.
- Working directory: the AC-005 workspace allocation for (run, session);
  Agy treats cwd as the workspace. `--add-dir`, `--project`,
  `--new-project` are never passed.
- Environment: AC-005 inherited-env allowlist; `HOME` is NOT overridden for
  authenticated runs (§3.2). The alias never applies (no shell).
- **Launch network/isolation matrix (frozen profile ⇒ executor behavior),
  adapted from AC-009 §3.2 for a process-per-turn child.** The turn child
  needs provider egress (the signed-in Antigravity backend, gRPC/HTTPS);
  model-created network activity (`read_url_content`, `search_web`,
  browser tools) is tool activity governed separately by the frozen
  `sandbox` flag and the operator's permission rules — never by executor
  egress. Two planes, evaluated independently; either dimension failing
  its capability check rejects the launch, and a degraded grant never
  combines with a passing one into a "partially isolated" launch.

  | Isolation strictness | Network mode | Launch behavior |
  |---|---|---|
  | `permissive_dev` | `unrestricted` | Launches. Provider egress direct. **Degraded per AC-005** (not OS-enforced; never presented as egress control). Tool network stays under `--sandbox`/permission rules, whose headless enforcement is `not verified` (§7) and is therefore NOT claimed. |
  | `permissive_dev` | `allowlist` | Launches. Provider egress through the AC-005 allowlist proxy, destination-gated from `network_allowlist` (the Antigravity backend endpoints the operator lists). |
  | `permissive_dev` | `none` | **Fixture/diagnostic only, never production**: the `models` auth gate cannot reach the backend, so it is INCONCLUSIVE (§3.2: transport failure ≠ "not signed in") and no turn child starts. Council's fixture executable answers `models` deterministically, which is how CI exercises this row. |
  | `strict` | `allowlist` | **Launch rejected, fail-closed** (proxy reachability from a fresh namespace unverified for this child class). |
  | `strict` | `none` | **Fixture/diagnostic only, never production**: same as above — the auth gate is inconclusive without egress, so no prompt is ever transmitted. |
  | `strict` | `unrestricted` | **Launch rejected, fail-closed** (unverified capability; never silently downgraded). |

  Workspace mode (`none`/`readonly`/`isolated_branch`) is the AC-005
  allocation the child's cwd is pinned to (§3.1) and is enforced by the
  executor's capability check independently of the network dimension.
- Adapter-owned detached launch context: the process and its reader run on
  a context bound to the turn's lifecycle, never the caller's Dispatch or
  Observe ctx (AC-007 lifecycle fix). A disconnected controller never
  kills an in-flight turn.
- stdout: a single NDJSON reader with an oversized-line bound; stderr:
  drained from start with a bounded tail retained as evidence and scanned
  for two exact markers: `[agy] print timeout` and `warning: ignoring
  unsupported stream input message event`.

### 3.2 Isolation, the auth-home constraint, and the eligibility gate

Sign-in is keyring/home-bound (`~/.gemini/antigravity-cli/…`); a relocated
`HOME` isolates config but is unauthenticated, and copying credentials is
forbidden. Therefore contributors run against the operator's shared
`~/.gemini` home, and per-session isolation of the native state directory
is NOT available — the AC-009 §3.3 model applies verbatim, contributor-
scoped:

- **Production-eligibility gate (binding):** authenticated production
  dispatch is eligible ONLY while a valid isolation attestation exists for
  the exact frozen tuple (installed CLI version, platform, toolkit manifest
  digest, cprof-v4 profile digest — the binary digest is INSIDE the
  profile, so it is covered by the profile digest) — all `sibling_read`
  records denied AND the `approval` set complete. The attestation is produced by the operator-authorized
  probe suite: attempts to read a SIBLING conversation file
  (`~/.gemini/antigravity-cli/conversations/<other>.db`) through every
  enabled tool path — `view_file` (read class), `find_by_name`/`list_dir`
  (glob class), `grep_search` (grep class), `run_command` with an absolute
  path (shell class), each frozen MCP tool via `call_mcp_tool` (mcp
  class), each frozen plugin/skill tool (plugin class) — and
  `self_mutation` attempts against the author's OWN conversation file
  (write/append/truncate/rename/delete via `write_to_file`,
  `replace_file_content`, `sed_file`, `run_command`), each recorded with
  the enforcing capability. Encoding: **cprot-v2 reused unchanged**
  (record classes and tool-class enum map 1:1; the frame's version slot
  carries the Agy CLI version, `manifest_digest` the toolkit manifest
  digest, `profile_digest` the cprof-v4 digest), stored in a contributor-
  scoped `agy_protection_attestations` table with the same digest-bound
  columns. Coverage binding (AC-009 §3.7 errata) is enforced at recording
  and lookup. The expected coverage set is DERIVED, never asserted:
  `expected_tools` ∩ the pinned coverage map (below) yields the exact
  record set — one `sibling_read` per `sibling_read_path` tool name and
  the five `self_mutation` operations per `own_mutation_path` tool name —
  and because freeze rejects non-empty MCP/plugin inventories (§3.7) the
  set contains built-in tools only. Unlike the Codex validator, the Agy
  coverage validator allows several tool NAMES under one cprot-v2 tool
  class (e.g. `view_file` and `read_resource` both `read`), because the
  map, not the class, is the unit of coverage; duplicate coverage means
  the same name twice, and unexpected coverage means a name outside
  `expected_tools` or a class the map does not assign to it.
- **Version-pinned tool-to-capability coverage map (closes the built-in
  path gap).** Every name in `expected_tools` MUST appear in the pinned
  coverage map for the frozen `cli_version` (committed with the
  evidence, digest-bound like the `init` capture); the map assigns each
  tool exactly one class, and the attestation must cover every tool of a
  probed class that the profile enables:

  | Class | 1.2.9 tools | Attestation obligation |
  |---|---|---|
  | `sibling_read_path` | `view_file`, `read_resource`, `list_dir`, `find_by_name`, `grep_search`, `run_command`, `send_command_input`, `notebook_execution`, `open_browser_url`, `read_browser_page`, `execute_browser_javascript` (browser tools can dereference `file://`), `call_mcp_tool` (per frozen MCP tool) | one `sibling_read` record per tool name (cprot-v2 tool_class: read/glob/grep/bash_absolute/mcp; browser and notebook execution are recorded under `bash_absolute` semantics — "arbitrary code with filesystem reach") |
  | `own_mutation_path` | `write_to_file`, `replace_file_content`, `multi_replace_file_content`, `sed_file`, `notebook_edit`, `run_command`, `send_command_input`, `notebook_execution`, `execute_browser_javascript` | `self_mutation` records for write, append, truncate, rename, delete per tool name (a tool that cannot express an operation records that operation as denied-by-construction only when the probe suite proves the tool refuses it) |
  | `network_only` | `search_web`, `read_url_content` (non-`file://` only — a `file://` probe is part of `sibling_read_path` above), `generate_image` | none for isolation; governed by `sandbox`/permission rules |
  | `control` | `ask_permission`, `ask_custom_permission`, `ask_question`, `finish`, `wait`, `wait_5_seconds`, `schedule`, `send_message`, `manage_inbox`, `manage_task`, `manage_subagents`, `define_subagent`, `invoke_subagent`, `browser_subagent`, `list_permissions`, `list_resources`, `delete_knowledge`, `command_status`, `capture_browser_screenshot`, `capture_browser_console_logs`, `list_browser_pages`, and the remaining `browser_*` interaction tools (`click`, `input`, `scroll`, `drag`, `mouse_*`, `press_key`, `refresh_page`, `resize_window`, `select_option`, `get_dom`, `get_network_request`, `list_network_requests`, `scroll_dom`) | none — no independent filesystem reach beyond the paths above; subagents inherit the same tool set, so their reach is covered transitively by the tool-level probes (recorded as an assumption in the suite) |
  | `uncovered` | any tool NOT in the map for this version | **profile rejected at freeze** — the path to production is a native denial that removes the tool from `init.tools` (operator `settings.json` rule) proven by the `init` capture, or a later evidence run that extends the map |

  A tool listed in two classes carries both obligations. The map is
  evidence, not code: it lives at `docs/superpowers/evidence/
  ac010-agy-tool-coverage-<version>.json`, is digest-bound in the profile
  (`tool_coverage_path`/`tool_coverage_digest`, §3.7), and coverage
  validation derives the expected record set from `expected_tools` ∩ map.
- **Skills, plugins, and guardrail hooks — configured state verified,
  execution not claimed.** The profile freezes `expected_skills` (the
  skill directory names under `<home>/antigravity-cli/skills` and the
  enabled plugins' skill dirs), `expected_plugins` (from `agy plugin
  list`, provider-free), and `hooks_config_digest` (sha256 of the
  operator's `<home>/config/hooks.json` bytes). Construction and every
  launch re-derive all three and fail closed on drift
  (`ErrToolkitDrift`); `--disable-slash-commands` is pinned so skills
  cannot be invoked as slash commands. What this proves: the configured
  toolkit is the frozen one. What it does NOT prove (recorded as a gap,
  never claimed): that a hook executed on a given tool call, or that the
  model loaded a given skill. Issue #10's "verify expected
  skills/tools/plugins and enabled guardrails" is met at the
  configured-state level and stated as such in §6.
- **Approval records:** Agy has no per-variant approval wire protocol; the
  permission decision is native (`request-review` auto-deny) and surfaces
  in `result.denied_actions`. The attestation's `approval_deny` records
  therefore carry ONE method, `denied_actions`, with
  `refusal_kind=native_refusal_enum`, proven by a live denial in the suite.
- **Gate ordering:** (1) before any child starts: attestation valid, binary
  digest matches (§3.7), frozen inventories present, platform eligible
  (§3.12); (2) **auth gate, provider-free of model calls and BEFORE any
  prompt exists**: because `init` proves nothing about sign-in, the
  adapter runs the frozen `models` inventory launch (`<binary> models`,
  network-bound, no model call) through the executor before the FIRST
  dispatch of a logical session and again at `ResumeSession` after a
  park. Three outcomes, all decided before any prompt exists: the
  live-verified unauthenticated text (`Please sign in to view available
  models…`) ⇒ typed `ErrAgyAuthRequired`; a transport/network failure,
  timeout, non-zero exit, or empty/unparseable catalog ⇒ typed
  `ErrAgyAuthInconclusive` (fail closed — under `network_mode=none` this
  is the only possible outcome, which is why those matrix rows are
  fixture/diagnostic only, §3.1); a catalog that lacks the frozen model ⇒
  `ErrProfileDrift`. Only a parsed catalog containing the frozen model
  passes; no turn process starts otherwise;
  (3) turn child starts: `init` verified (§3.1) and only then the prompt
  is written. **After the first stdin byte nothing is pre-acceptance**:
  any `result` (ERROR included, sign-in/quota/validation text included)
  is the process's single verified terminal and classifies the attempt
  as a failed terminal; absence of a `result` is Uncertain. Neither
  authorizes an automatic retry (§3.5, §3.9).
- Test-only construction mode: `agytest` package + production guard,
  exactly the codextest pattern; production construction always requires
  the attestation lookup.

### 3.3 Session creation, idempotency, and binding

`CreateSession` is provider-free: launch a child with the frozen argv and
NO `--conversation`, keep stdin open, wait for `init`, validate the
`conversation_id` as UUID, run the §3.1 config checks, then close stdin
WITHOUT writing any message (verified: exit 0, no turn, conversation file
persisted). The id becomes `NativeSessionID`; the service persists the
binding (`BindAgySession`, transactional, AC-009 pattern) with
`materialized=false` until the first accepted turn. Creation ends
UNCERTAIN (typed, durable episode, AC-009 §3.4 errata) when `init` is not
observed before the child exits or the reader fails after launch; a lost
`init` may still have created a conversation file. Repeated identical
config returns the binding; changed config is rejected. Concurrent
duplicates share one creation reservation.

### 3.4 ResumeSession

Local inspection only: the binding is well-formed and, if materialized,
the recorded conversation file still exists at
`<home>/antigravity-cli/conversations/<id>.db` with mode 0600 and the
recorded file identity. Native verification is the next turn's `init`
equality check. No provider-free "resume without a turn" is used for
liveness beyond what §3.3 does (an empty-stdin `--conversation` launch is
verified provider-free and may be used for a diagnostic liveness check
that never binds).

### 3.5 Dispatch — turn identity, acceptance, and bounds

- Durable ordering (AC-009 §3.11): attempt + prompt digest (`pdig-v1`
  framing over native id, turn key, attempt id, prompt) durable before
  launch; launch reserved in the same transaction before the write; first
  stdin byte recorded at the write boundary.
- Acceptance: the `step_update{step_type:"user_input", state:DONE}` for
  this process is the native acknowledgement (its `step_index` is recorded
  as the attempt's native step index). Write failure before the first byte
  ⇒ DispatchRejected (nothing transmitted); failure after ⇒
  DispatchUnknown. **The first byte is the only ambiguity boundary**: a
  `result` ERROR that arrives after the write but before any `user_input`
  step (sign-in, quota, validation) is a verified FAILED terminal of this
  attempt, never a pre-acceptance rejection — the conversation may or may
  not have recorded the input, so no automatic retry is authorized; the
  controller disposes.
- Terminal: exactly one `result` per process. `SUCCESS` ⇒ `TurnCompleted`
  with `response` as the result payload, usage as observed; `ERROR` with
  `error=="interrupted"` ⇒ `TurnCancelled`; other `ERROR` ⇒ `TurnFailed`
  with the error text.
- **Required-tool verification (issue #10 criterion):** every attempt
  carries a `required_tools` set read from the durable dispatch intent
  through the attempt-identity seam (`RequiredToolsFor(ref)`). The set
  is validated at QUEUE time — each name must be in the frozen
  `expected_tools` (native tool names, e.g. `run_command`), duplicates
  rejected — journaled with the queued prompt, and immutable thereafter;
  absent ⇒ the frozen `default_required_tools` (also ⊆ `expected_tools`).
  **Identity namespace and correlation rule.** Native tool names are the
  single namespace. `result.denied_actions` speaks a different vocabulary
  (`action:"command"`, `display_name:"RunCommand"`), so the version-
  pinned denial map (part of the tool-coverage evidence file, §3.2)
  translates each `(action, display_name)` pair to the set of native
  tool names it denies (1.2.9: `command`/`RunCommand` → `run_command`,
  `send_command_input`; further pairs are added only from live
  evidence). Because a denied `tool` step still reports `state:DONE`
  in-stream (live-verified), execution is established as: `executed(T)`
  ⇔ a `tool` step with `tool_name == T` reached `DONE` for this process
  AND no denial in `denied_actions` maps to `T`. A `denied_actions` entry
  whose pair is NOT in the pinned map fails closed: every observed tool
  step of this process is treated as NOT executed and the attempt is
  flagged incomplete with `unmapped_denials` recorded. The result is
  flagged `verification_incomplete` when `required_tools − executed_tools
  ≠ ∅` (a required tool silently SKIPPED) or `denied_tools ≠ ∅`, with
  `missing_required_tools`, `denied_tools`, and `unmapped_denials`
  recorded on the attempt and one `tool_denied` event emitted per mapped
  denial. `SUCCESS` with exit 0 never clears the flag; acceptance of the
  contributor's output is the controller's decision, never automatic.
- Bound: Council's turn bound is enforced by SIGINT (§3.6), with
  `--print-timeout` set strictly LARGER as a backstop; if the stderr
  `[agy] print timeout` marker appears the attempt is **Uncertain**
  regardless of `result.status` (partial output is not a verified
  terminal).
- Single flight per native conversation; a second Dispatch is rejected
  (`conversation busy`).

### 3.6 Cancellation

`Cancel` sends SIGINT to the turn's process ⇒ `CancelRequested`;
`CancelConfirmed` ONLY on the verified `result{status:ERROR,
error:"interrupted"}` (live-verified within ~1 s). Grace expiry ⇒ SIGTERM →
kill; no terminal ⇒ Uncertain. Cancel on a terminal/absent turn ⇒
`CancelAlreadyTerminal`/`CancelUnknown`.

### 3.7 Frozen profile (cprof-v4, additive, canonically specified) and the binary pin

**Encoding.** `cprof-v4` is additive on `cprof-v3` in the existing
implementation shape: the per-harness `HarnessProfileSpec` gains an
optional typed `agy` block (`json:"agy,omitempty"`), so v1/v2/v3
encodings are byte-identical to today. The canonical JSON encoding uses
the same encoder as v1–v3 (sorted keys, no insignificant whitespace,
number literals verbatim); the digest is
**`cprof-v4:sha256:<lowercase-hex>`** over exactly those bytes.

```json
"harnesses": {
  "agy": {
    "extra_env_allowlist": [],
    "model": "gpt-oss-120b-medium",
    "native_auth_mode": "inherited_gemini_home",
    "agy": {
      "cli_version": "1.2.9",
      "binary_path": "/home/<operator>/.local/bin/agy",
      "binary_digest": "sha256:<hex>",
      "expected_home": "/home/<operator>/.gemini",
      "platform": {"os": "linux", "family": "unix"},
      "permission_mode": "request-review",
      "execution_mode": "default",
      "sandbox": true,
      "print_timeout_backstop_seconds": 1800,
      "expected_tools": ["ask_permission", "run_command", "view_file", "…"],
      "expected_mcp_servers": [],
      "expected_mcp_tools": [],
      "expected_plugin_tools": [],
      "default_required_tools": [],
      "hooks_evidence": {"verified": ["…"], "unverifiable": ["hook execution at the Agy layer", "settings.json permissions.allow contents"]},
      "expected_skills": ["…"],
      "expected_plugins": ["superpowers"],
      "hooks_config_digest": "sha256:<hex>",
      "init_evidence_path": "docs/superpowers/evidence/ac010-agy-init-1.2.9.json",
      "init_evidence_digest": "sha256:<hex>",
      "tool_coverage_path": "docs/superpowers/evidence/ac010-agy-tool-coverage-1.2.9.json",
      "tool_coverage_digest": "sha256:<hex>"
    }
  }
}
```

**Required fields and validation at freeze** (storage cprof-v4 validator,
mirrored by the adapter's `ValidateAgyHarness`; every rule fails closed
with a typed validation error):

- `cli_version`: non-empty, `MAJOR.MINOR.PATCH`. `binary_path`: absolute,
  cleaned. `binary_digest`: `sha256:` + 64 lowercase hex.
  `expected_home`: absolute, cleaned. `platform.os`/`platform.family`:
  non-empty; production eligibility limited per §3.12.
- `permission_mode` ∈ {`request-review`, `strict`} — `always-proceed`,
  `proceed-in-sandbox`, and any other value are rejected (no bypass value
  is representable). `execution_mode` ∈ {`default`, `accept-edits`,
  `plan`}. `sandbox`: boolean. `print_timeout_backstop_seconds`: integer
  ≥ 60.
- `expected_tools`: required, non-empty, the EXACT built-in tool
  inventory; `expected_mcp_servers`, `expected_mcp_tools`
  (`<server>/<tool>`, server ∈ servers), `expected_plugin_tools`,
  `default_required_tools` (⊆ `expected_tools`): required (`[]` when
  none); duplicates rejected. **Inventory-evidence gate (AC-009 §3.8
  errata, applied verbatim): no committed evidence path proves a
  non-empty MCP or plugin tool inventory** — `init.tools` names MCP
  tooling only as the generic `call_mcp_tool` and does not enumerate
  plugin/skill tools (both `not verified`) — so freeze REJECTS any
  profile with non-empty `expected_mcp_servers`, `expected_mcp_tools`,
  or `expected_plugin_tools` until such a path exists. Only empty
  inventories are launchable; the attestation coverage set is then the
  built-in classes exactly, fully derivable from the profile.
- `init_evidence_path`/`init_evidence_digest` and
  `tool_coverage_path`/`tool_coverage_digest`: required. **Evidence-root
  containment contract (AC-008/AC-009 rules, applied verbatim):** the
  path is repo-relative (no absolute path, no `..`, cleaned, forward
  slashes), resolved ONLY inside the operator-owned, service-configured
  evidence root; every path component must be a real directory (no
  symlinks) and the final entry a regular file whose symlink-resolved
  location stays under the resolved root; the raw bytes are re-hashed
  and must equal the digest exactly. Decoding is STRICT: exactly one
  JSON value followed by EOF (trailing content rejected), no unknown
  keys, no duplicate keys. The `init` capture must be a single
  `{"event":"init",…}` object whose `init.tools` equals `expected_tools`
  and whose `permission_mode` equals the frozen mode; the coverage map
  must be for the frozen `cli_version` and must classify every
  `expected_tools` entry (§3.2). Any profile that binds host data outside
  the root, or a capture that fails any rule, is rejected at freeze and
  at construction. This is the native, digest-bound proof of the built-in
  inventory that AC-009 lacked for Codex.
- `expected_skills`, `expected_plugins`: required (`[]` when none),
  duplicates rejected. `hooks_config_digest`: `sha256:` + 64 hex over the
  operator's `hooks.json` bytes (re-derived at construction and launch,
  §3.2).
- `hooks_evidence.verified`/`unverifiable`: at least one list present.
- Unknown fields: `DisallowUnknownFields` on the typed block; an `agy`
  block under any `algo_version` other than `cprof-v4` is a validation
  error; `cprof-v4` requires `toolkit_manifest` (inherits the v2/v3 rule)
  and, when a `codex` block is present, validates it exactly as v3 does.
- Normalization (existing family): scalars BOM-trimmed + NFC; array
  fields BOM-trimmed, NFC, deduplicated, byte-wise lexicographically
  sorted (case-sensitive; the `tooling` lowercasing quirk is NOT
  applied); paths `ToSlash`/`Clean`, no trailing slash; digests
  lowercased; booleans/integers verbatim.

**Compatibility matrix.**

| Adapter | cprof-v1 | cprof-v2 | cprof-v3 | cprof-v4 |
|---|---|---|---|---|
| OpenCode (AC-007) | accepted | accepted | accepted, ignored | **accepted, ignored** (validity gate widens to v1–v4) |
| Claude (AC-008) | rejected | required+accepted | accepted | **accepted** (manifest still required; agy block ignored) |
| Codex (AC-009) | rejected | rejected | required | **accepted** only with a complete `codex` block (validated as v3) |
| Agy (AC-010) | rejected | rejected | rejected | **required**: complete `agy` block, typed `ErrUnsupportedProfile` otherwise |

One run profile serves all four harnesses; a run including Agy is frozen
as v4 and every other contributor on it remains valid. No record-only
mode; no backfill.

**Binary pin (hazard 1).** Production construction and EVERY launch
(creation, auth gate, and each turn child) re-verify `binary_path` (must
be the resolved absolute path, no symlink escape, regular file) and
compute the FULL sha256 of its bytes against `binary_digest` — no
metadata cache: an in-place modification that preserves size, mtime, and
inode is inside the threat model. The cost (~1 s for a 217 MB binary) is
accepted per launch. Drift ⇒ typed `ErrBinaryDrift`, no process started,
and every attestation for the old digest is invalid by construction.
The hash is taken immediately before the executor launch; the
executor-observed executable identity is recorded on the launch row. The operator obligation to disable the auto-updater is
recorded as unverified; the pin is the defense either way.

### 3.8 Trust model of the durable conversation file

Advisory only. The per-conversation SQLite file is protobuf-in-SQLite with
no pinned schema and no interruption marker; it never classifies a turn.
Readable facts (file identity, size, `steps` count, `step_type`/`status`
integers) are recorded as diagnostics at first acceptance (baseline) and
at reconciliation. There is NO protected-evidence upgrade and NO
verified-absence redispatch in v1: every started attempt without an
in-life `result` remains Uncertain until the controller records a
disposition. This is simpler than AC-009 and honest to the evidence.

### 3.9 Stream, process, and failure semantics

| Condition | Classification |
|---|---|
| `init` not observed before exit / reader failure before `init` | creation: Uncertain (episode); dispatch: DispatchRejected if before the write, else Uncertain |
| `init.conversation_id` ≠ requested / not a UUID | typed `ErrConversationDrift`, child terminated, prompt never written; orphan id recorded |
| `init.permission_mode`/`model`/`cwd`/`tools` ≠ frozen | typed `ErrProfileDrift`/`ErrToolInventoryDrift`, pre-acceptance |
| `result` ERROR after the first stdin byte but before any `user_input` step (sign-in, quota, validation) | terminal failed (verified `result`); no automatic retry |
| `models` auth-gate launch unauthenticated / catalog lacks the frozen model | typed `ErrAgyAuthRequired` / `ErrProfileDrift` before any turn process starts |
| `user_input` DONE observed | accepted |
| `result` SUCCESS | terminal completed; `denied_actions` ⇒ tool_denied events + verification_incomplete |
| `result` ERROR `interrupted` | terminal cancelled |
| `result` ERROR other | terminal failed |
| stderr `[agy] print timeout` | Uncertain (partial), never terminal |
| stderr `ignoring unsupported stream input message event` | protocol violation ⇒ Uncertain (a message was dropped) |
| malformed/oversized NDJSON line, unknown `event`, unknown `step_type` | stream poisoned ⇒ Uncertain; child terminated |
| process exit without `result` after the write | Uncertain |
| child start failure (executor proves no process) | Rejected, definitively missing |
| Observe ctx cancelled | tap detach only |

### 3.10 Reconciliation evidence rules

Only the in-life `result` correlated to this attempt's process is terminal
evidence. Absence of a process, a present conversation file, step counts,
or a provider-free `--conversation` liveness `init` establish session
reachability at most (diagnostic), never turn acceptance or outcome. Lost
attempts stay Uncertain until a controller disposition; replacement work is
a new attempt; unresolved attempts block the native conversation across
restarts.

### 3.11 Durable state schema (storage migration v7; AC-009 v5/v6 adapted)

```
agy_session_bindings      -- session_id PK, native_id (UUID, unique), materialized,
                          --   model, workspace, profile_digest, conversation_path NULL,
                          --   file_identity NULL, first_prompt_digest NULL, created_at
agy_turn_attempts         -- attempt_id, session_id, turn_key, prompt_digest,
                          --   native_step_index NULL (from user_input), launch_count (0..1),
                          --   accepted, terminal, result_payload, result_usage,
                          --   required_tools_json, executed_tools_json,
                          --   missing_required_tools_json, denied_tools_json,
                          --   unmapped_denials_json, verification_incomplete BOOL,
                          --   observed_status (completed|failed|cancelled|missing|uncertain),
                          --   uncertainty_disposition NULL, transition_version, timestamps
agy_attempt_launches      -- attempt_id, reservation_seq, state (reserved|started|
                          --   start_failed|dead), started_at, first_stdin_byte_at,
                          --   known_dead_at, exit_code, executor_identity, binary_digest
agy_protection_attestations   -- cprot-v2 rows, contributor-scoped (§3.2)
agy_creation_uncertainties    -- episodes, exactly the codex v6 shape
```

`launch_count` is capped at 1 (no redispatch in v1). Every launch row
records the binary digest observed at that launch.

### 3.12 Platform eligibility (fail-closed)

All evidence in this design was gathered on linux-x86_64. Production
eligibility is limited accordingly:

- The frozen `platform` must be `{os: linux, family: unix}` for production
  construction in v1; any other frozen platform is `ErrUnsupportedProfile`
  at construction (no process started).
- macOS: the launch path and event protocol are expected to be identical
  but are `not verified`; production construction is refused until a
  macOS evidence run updates this section. Fixture tests may run on macOS
  (CI does) because the fixture executable is Council's own.
- Windows: refused at construction (no ownership/mode identity for the
  conversation file, no evidence); fixture tests only, and the fixture
  layer must not assume POSIX signals (SIGINT-based cancellation is
  POSIX; the Windows fixture asserts the typed refusal instead).
- The conversation-file ownership/mode check (0600, current uid) is POSIX;
  it is part of `ResumeSession` on eligible platforms only.

## 4. Evidence plan (fixtures vs integration)

- **Fixture layer (CI, provider-free):** a fake `agy` executable through
  the PolicyExecutor emitting recorded NDJSON: `init` (matching, id drift,
  mode/model/cwd/tools drift, missing), `user_input` ack, `agent_response`
  deltas, `tool` steps, `result` SUCCESS/ERROR/interrupted/denied_actions,
  stderr markers (print timeout, ignored event), malformed lines, exit
  without result, SIGINT handling (fixture emits `interrupted`), slow
  `init`, creation with empty stdin; cprot-v2 vectors reused; crash
  boundaries; concurrent duplicate creation and dispatch.
- **Integration (operator-invoked, sanitized):** Stage A provider-free
  (version, digest, `mcp list`, `plugin list`, empty-stdin `init` capture
  → `expected_tools`); Stage B authenticated (≤ 8 turns: trivial success,
  resume, denial, SIGINT, print-timeout on a healthy turn, `--mode plan`
  edit block, `denied_tools` rule); Stage C probe suite → first cprot-v2
  attestation via the journal operation.
- Platform evidence is linux/unix only; see §3.12 for the eligibility
  rule.

## 5. Boundaries

- Never `--dangerously-skip-permissions`, `--continue`, `-i`,
  `--remote-control`, `install`, `update`, the alias, or any shell.
- Never read, copy, relocate, or symlink the keyring/credential file;
  `agy models` output is the only sign-in evidence and is network-bound.
- Never write `settings.json`, `hooks.json`, `mcp_config.json`, or
  `permissions.allow`; grants are operator-provisioned.
- Every launch through the AC-005 executor with the frozen argv under the
  §3.1 capability matrix; cwd pinned to the AC-005 workspace;
  model/mode/sandbox only from the frozen profile and verified against
  `init` before transmission; the `models` auth gate before the first
  prompt of a session.
- Denials are structured events and make verification incomplete; exit
  codes never classify.
- Cost is `unavailable`; usage is reported as observed (per-step and
  cumulative are distinguished).
- Version+digest-pinned evidence: drift fails closed; unknown shapes fail
  closed.

## 6. Acceptance mapping (issue #10)

| Criterion | Where |
|---|---|
| Probe the installed CLI and supported output fields with sanitized fixtures | §2, evidence file; fixture executable (§4) |
| Start independently and resume a specified conversation | §3.3 (provider-free creation), §3.1/§3.5 (`--conversation` + `init` equality) |
| Verify expected skills/tools/plugins and enabled guardrails | §3.7 `expected_tools` = `init.tools` at freeze (digest-bound `init_evidence`) and every launch; every tool classified by the pinned coverage map (uncovered ⇒ rejected); `expected_skills`/`expected_plugins`/`hooks_config_digest` re-derived at every launch (configured state verified; execution explicitly not claimed); `permission_mode` attestation; MCP/plugin tool inventories gated to empty until natively provable |
| Required skipped/denied tools keep verification incomplete regardless of exit code | §3.5 queue-validated immutable `required_tools` (native names) vs observed executed `tool` steps under the pinned denial map ⇒ `missing_required_tools`; `denied_actions` ⇒ `denied_tools`; unmapped denials fail closed; any ⇒ `verification_incomplete`; exit code never classifies (§2.1/§3.9) |
| Bounded cancellation, unknown outcomes, client-close recovery, no fallback harness | §3.6 SIGINT → verified `interrupted`; §3.9/§3.10 Uncertain rules; §3.1 detached context; no other adapter is ever substituted |

### 6.1 Concrete acceptance scenarios

1. Concurrent duplicate dispatch — one process, shared verdict.
2. Crash after the stdin write, `result` lost — Uncertain; block persists
   across restart; controller disposition required.
3. Process death mid-turn — the lost attempt is Uncertain and BLOCKS the
   conversation (§3.10); only after the controller records a disposition
   does the next turn start, as a new process on the same conversation
   after `init` equality.
4. Controller disconnect — turn continues; observer re-attaches.
5. Exact identity — absent/malformed id ⇒ `ErrConversationDrift`, prompt
   never written, orphan recorded; non-UUID never transmitted.
6. Permission-requiring tool — auto-denied natively; `tool_denied` event;
   `verification_incomplete`; exit 0 does not mark the turn verified.
7. Config drift — `init.permission_mode`/`model`/`cwd` mismatch fails
   before transmission.
8. Tool inventory drift — `init.tools` ≠ frozen ⇒ pre-transmission failure.
9. Print-timeout marker — Uncertain even with `result` present.
10. Binary drift — digest mismatch ⇒ no process started.
11. Unattested authenticated dispatch — `ErrProductionEligibilityMissing`
    before any child starts.
12. Ignored input event — stderr marker ⇒ Uncertain, never assumed sent.

## 7. Unverified items carried forward (explicit)

`--mode`/`--sandbox` effect headlessly; `denied_tools` rule denial shape
and whether a natively denied tool disappears from `init.tools`; hook
EXECUTION and skill LOADING at the Agy layer (configured state is
verified, §3.2); the transitive-subagent coverage assumption;
workspace-trust gating; healthy-turn print-timeout; content-block user
messages; `AGY_ERROR` exit-3 path; MCP tool naming in `call_mcp_tool` steps;
plugin/skill tool surfacing; auto-updater off switch; `GEMINI_API_KEY`
mode; Windows/macOS behavior. Each is a Stage B or operator obligation and
none is claimed by the design.

## 8. v2 changes (review of v1)

1. Post-transmission classification: the first stdin byte is the only
   ambiguity boundary; a `result` ERROR after it is a verified failed
   terminal, never pre-acceptance; the auth gate moved to a provider-free
   `models` launch before the first prompt (§3.2, §3.5, §3.9).
2. Attestation coverage: MCP/plugin inventories are gated to empty at
   freeze until a native evidence path exists, so the coverage set is
   derivable from the profile; the built-in inventory is proven by the
   digest-bound `init` capture (§3.2, §3.7).
3. Required-but-skipped tools: per-attempt `required_tools`, observed
   executed tools, `missing_required_tools`, `denied_tools`, and the
   `verification_incomplete` rule (§3.5, §3.11, §6).
4. cprof-v4 canonical specification: fields, validation, normalization,
   unknown-field rule, digest formula, compatibility matrix (§3.7).
5. AC-005 launch capability matrix for the process-per-turn child (§3.1).
6. Scenario 3 now requires a controller disposition before the next turn.
7. Platform eligibility rule (§3.12): linux/unix only in v1, macOS and
   Windows refused at construction with fixture-only coverage.

## 9. v3 changes (review of v2)

1. Auth gate vs `network_mode=none`: transport failure is
   `ErrAgyAuthInconclusive` (fail closed), distinct from
   `ErrAgyAuthRequired`; the two `none` rows are fixture/diagnostic only
   and can never reach production dispatch (§3.1, §3.2).
2. Built-in path coverage: a version-pinned, digest-bound
   tool-to-capability coverage map classifies every `expected_tools`
   entry; uncovered tools reject the profile; skills, plugins, and the
   guardrail hooks file are frozen and re-derived at every launch as
   configured-state verification, with execution explicitly not claimed
   (§3.2, §3.7, §6, §7).
3. `init_evidence`/`tool_coverage` evidence-root containment and strict
   decoding contract (§3.7).
4. Native-tool-name namespace, version-pinned denial map, correlation
   rule, unmapped-denial fail-closed, queue-time validation and
   immutability of `required_tools` (§3.5, §3.11, §6).
5. Binary pin: full sha256 at every launch, no metadata cache (§3.7).
