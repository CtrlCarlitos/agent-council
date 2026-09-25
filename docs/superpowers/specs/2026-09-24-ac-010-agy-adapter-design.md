# AC-010 Design — Agy (Antigravity CLI) persistent contributor adapter

Status: ACCEPTED — approved for implementation planning (DRAFT v7 review,
2026-09-24; seven review rounds, change logs at the end). Reviewer notes
carried into the plan: `MFD_ALLOW_SEALING`, descriptor close-on-exec
handling, ptrace cleanup with `PTRACE_O_EXITKILL`, and tracer-failure
tests. Implementation plan:
`docs/superpowers/plans/2026-09-24-ac-010-agy-adapter.md`.
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
  evidence, digest-bound like the `init` capture). The map assigns each
  tool a canonical NON-EMPTY SET of capabilities drawn from the closed
  enum {`sibling_read_path`, `own_mutation_path`, `network_only`,
  `control`, `uncovered`} — encoded as a sorted array in that enum order,
  duplicates rejected, `uncovered` exclusive of every other value — and
  the attestation must satisfy every obligation of every capability of
  every tool the profile enables:

  | Capability | 1.2.9 tools carrying it (a tool may carry several) | Attestation obligation |
  |---|---|---|
  | `sibling_read_path` | `view_file`, `read_resource`, `list_dir`, `find_by_name`, `grep_search`, `run_command`, `send_command_input`, `notebook_execution`, `open_browser_url`, `read_browser_page`, `execute_browser_javascript` (browser tools can dereference `file://`), `read_url_content` (its `file://` form), `call_mcp_tool` (per frozen MCP tool) | one `sibling_read` record per tool name (cprot-v2 tool_class: read/glob/grep/bash_absolute/mcp; browser, notebook execution, and `file://` URL reads are recorded under `bash_absolute` semantics — "arbitrary code or URL with filesystem reach") |
  | `own_mutation_path` | `write_to_file`, `replace_file_content`, `multi_replace_file_content`, `sed_file`, `notebook_edit`, `run_command`, `send_command_input`, `notebook_execution`, `execute_browser_javascript` | `self_mutation` records for write, append, truncate, rename, delete per tool name (a tool that cannot express an operation records that operation as denied-by-construction only when the probe suite proves the tool refuses it) |
  | `network_only` | `search_web`, `read_url_content` (its non-`file://` forms), `generate_image` | none for isolation; governed by `sandbox`/permission rules |
  | `control` | `ask_permission`, `ask_custom_permission`, `ask_question`, `finish`, `wait`, `wait_5_seconds`, `list_permissions` (reads the session's own permission state), `command_status` (status of commands this conversation started) — tools with no reach into any other conversation's state | none |
  | `uncovered` | `invoke_subagent`, `browser_subagent`, `define_subagent`, `manage_subagents` (a subagent's inherited tool set and restrictions are NOT verified — an escape path until a live probe proves the inherited boundary); `schedule` (work executed outside the turn); `send_message`, `manage_inbox`, `manage_task` (cross-conversation channels); `delete_knowledge` (shared-state mutation); **every remaining `browser_*` tool** plus `list_browser_pages`, `capture_browser_screenshot`, `capture_browser_console_logs`, `browser_get_dom`, `browser_get_network_request`, `browser_list_network_requests` (a shared browser instance may expose pages, DOM, network, and console state opened by another session — unproven until a cross-session probe); `list_resources` (may enumerate shared resources); and any tool NOT in the map for this version | **profile rejected at freeze** — the path to production is a native denial that removes the tool from `init.tools` (operator `settings.json` rule) proven by the `init` capture, or a later evidence run that extends the map with cross-session probes (e.g. a subagent-boundary probe, or a browser-state probe showing a second session cannot list or read pages opened by the first) |

  Consequence stated plainly: on an unmodified 1.2.9 install `init.tools`
  contains uncovered tools, so a production-eligible Agy profile requires
  the operator to deny them natively first; Council never claims
  coverage it did not probe. The map is evidence, not code: it lives at
  `docs/superpowers/evidence/ac010-agy-tool-coverage-<version>.json`
  (shape in §3.7), is digest-bound in the profile
  (`tool_coverage_path`/`tool_coverage_digest`), and coverage validation
  derives the expected record set from `expected_tools` ∩ map.
- **Skills, plugins, and guardrail hooks — configured state verified
  strictly, execution not claimed.** The profile freezes
  `expected_skills` = the sorted, deduplicated names of the REAL
  directories (no symlinks) directly under `<home>/antigravity-cli/
  skills` — a deterministic filesystem fact, provider-free; plugin-
  contributed skill directories are NOT enumerated because the
  `plugin_data` layout is not pinned (recorded as a gap; their plugins
  are captured below). `expected_plugins` is derived from a **strict digest-bound provider-free capture** of `agy plugin list` stdout, whose 1.2.9 shape is live-verified and COMMITTED as `docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json` (sha256 in `docs/superpowers/evidence/SHA256SUMS-ac010`; research file §5):
  `{"imports":[{"name":"…","source":"…","importedAt":"<RFC3339>",
  "components":["hooks","skills"]}]}`: the capture is decoded strictly
  (single JSON value, no unknown or duplicate keys, trailing content
  rejected, `importedAt` RFC3339, `components` ⊆ {`hooks`,`skills`,
  `mcp`,`commands`,`agents`}), re-encoded canonically (sorted keys, no insignificant whitespace, imports sorted by `name`, components sorted/deduplicated), and **the committed capture IS that canonical encoding** — raw file bytes and canonical bytes are identical by construction, so ONE digest, `plugins_evidence_digest`, serves both the evidence-root raw re-hash and the semantic comparison. Validation order at freeze and construction: (1) evidence-root containment (§3.7); (2) raw bytes re-hash == `plugins_evidence_digest`; (3) strict decode; (4) canonical re-encode must equal the raw bytes exactly (a non-canonical committed file is rejected); (5) at construction and every launch the adapter re-runs `plugin list` through the executor (provider-free, sealed-image verified like every launch), strictly decodes and canonicalizes the LIVE output, and requires its sha256 == `plugins_evidence_digest`. Any failure at any step is `ErrToolkitDrift`. If the
  live output ever fails the pinned shape, the profile is not launchable
  until the shape evidence is refreshed. Plus a
  **hooks configured-state capture**: the operator's
  `<home>/config/hooks.json` is parsed STRICTLY (one JSON value, no
  duplicate keys, no trailing content), re-encoded canonically (sorted
  keys, no insignificant whitespace), and its sha256 frozen as
  `hooks_config_digest`; the profile additionally freezes
  `required_hooks`, a non-empty list of JSON-pointer paths into that
  document (e.g. `/guardrail`) that must each resolve to a non-empty
  value whose subtree contains no `"enabled": false` / `"disabled":
  true` marker and, when the entry has a `command`/`handler` string,
  that string non-empty. Construction and every launch re-parse the
  live file, recompute the canonical digest, re-check every required
  pointer, re-list skills, and re-derive plugins; any difference is
  `ErrToolkitDrift` before any process starts. `--disable-slash-commands`
  is pinned so skills cannot be invoked as slash commands. What this
  proves: the frozen hooks (including the guardrail entry) are present,
  unchanged, and not marked disabled in the operator's configuration,
  and the configured skills/plugins are the frozen ones. What it does
  NOT prove (recorded as a gap, never claimed): that Agy actually ran a
  hook on a given tool call, or that the model loaded a given skill.
  Issue #10's "verify expected skills/tools/plugins and enabled
  guardrails" is met at the configured-state level and stated as such in
  §6.
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
  pinned `denial_map` in the coverage evidence file (§3.7 shape; 1.2.9:
  `command`/`RunCommand` → {`run_command`, `send_command_input`}; further
  pairs only from live evidence) translates each `(action,
  display_name)` pair to the set of native tools it CAN deny. Because a
  denied `tool` step still reports `state:DONE` in-stream
  (live-verified), attribution is intersected with observation: for each
  denial entry, `attributed = map(entry) ∩ observed_tools` where
  `observed_tools` are the `tool_name`s of `tool` steps of THIS process.
  `|attributed| = 1` ⇒ that tool is denied. `|attributed| > 1` ⇒ the
  denial is ambiguous: ALL attributed tools are treated as not executed
  and the entry is recorded in `ambiguous_denials` (conservative, never
  a false "executed"). `|attributed| = 0` ⇒ the denial is recorded in
  `unattributed_denials` (the native side denied something no observed
  step names; never attributed to a tool that was not attempted). An
  entry whose pair is NOT in the map ⇒ `unmapped_denials`, and every
  observed tool of the process is treated as not executed. Then
  `executed(T)` ⇔ T observed `DONE` AND T not denied/ambiguous AND no
  unmapped denial exists; `denied_tools` = the attributed and ambiguous
  tools. The result is flagged `verification_incomplete` when
  `required_tools − executed_tools ≠ ∅` (a required tool silently
  SKIPPED) or any of `denied_tools`, `ambiguous_denials`,
  `unattributed_denials`, `unmapped_denials` is non-empty; all five are
  recorded on the attempt and one `tool_denied` event is emitted per
  denial entry (naming the attributed tools, or the raw pair when
  unattributed/unmapped). `SUCCESS` with exit 0 never clears the flag; acceptance of the
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
      "plugins_evidence_path": "docs/superpowers/evidence/ac010-agy-plugins-1.2.9.json",
      "plugins_evidence_digest": "sha256:<hex>",
      "hooks_config_digest": "sha256:<hex>",
      "required_hooks": ["/guardrail"],
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
- `init_evidence_path`/`init_evidence_digest` and `tool_coverage_path`/`tool_coverage_digest`: required (a committed research capture of the 1.2.9 `init` shape with the workspace path and conversation id redacted is at `docs/superpowers/evidence/ac010-agy-init-1.2.9.json`; the operator's freeze-time capture is produced by Stage A against the frozen install). **Evidence-root
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
- `expected_skills`: required (`[]` when none), duplicates rejected,
  each a bare directory name (no separators). `plugins_evidence_path`/`plugins_evidence_digest`: required; the committed capture of `agy plugin list` (§3.2) MUST be canonical JSON (raw bytes == canonical re-encoding, checked at freeze), under the same evidence-root and strict-decoding contract as the `init` capture; the single digest is over those bytes, and the live re-derivation is compared after canonicalization so a byte-different but semantically identical live output still matches while any semantic difference fails. The same canonical-bytes rule applies to the `init` and coverage captures.
  `hooks_config_digest`:
  `sha256:` + 64 hex over the CANONICAL re-encoding of the strictly
  parsed `hooks.json` (§3.2). `required_hooks`: required, non-empty,
  RFC 6901 JSON pointers, sorted, duplicates rejected; each must resolve
  in the capture at freeze.
- **Coverage evidence file shape** (`tool_coverage_path`), strictly
  decoded (single value, no unknown or duplicate keys, trailing content
  rejected): `{"cli_version": "1.2.9", "tools": {"<tool>": ["<cap>",
  …]}, "denial_map": [{"action": "<a>", "display_name": "<d>", "tools":
  ["<tool>", …]}, …]}`. `tools` keys are unique and sorted; each value is
  a non-empty array in enum order (§3.2) with no duplicates and with
  `uncovered` exclusive. `denial_map` is sorted by (`action`,
  `display_name`), duplicate pairs rejected, each `tools` array sorted,
  deduplicated, non-empty, and a subset of the `tools` keys.
  `cli_version` must equal the frozen one.
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

**Binary pin (hazard 1) — three layers; the first removes the race, the
others are mandatory defense in depth, all Gate 1 fixture-tested.**

1. **Kernel-sealed execution image (primary; removes the race).**
   Permission bits are not immutability: the service user can `chmod`
   and replace its own files, and the copied Agy process (and its
   updater) run as that same user. So the launch source is a **sealed
   anonymous memory file**: at production construction the adapter
   reads the operator's `binary_path`, hashes the FULL bytes, requires
   equality with `binary_digest`, writes the bytes into a
   `memfd_create` file (created with `MFD_CLOEXEC | MFD_ALLOW_SEALING |
   MFD_EXEC`, retrying without `MFD_EXEC` only on `EINVAL` from a
   pre-6.3 kernel; see §14.1), applies `F_SEAL_WRITE | F_SEAL_SHRINK |
   F_SEAL_GROW | F_SEAL_SEAL` (kernel-enforced: no process, Council and
   the updater included, can alter the content or the seals for the
   descriptor's lifetime), and re-hashes THROUGH the sealed descriptor
   before accepting it. The descriptor is held for the adapter's
   lifetime; its `st_dev:st_ino` is the launch identity. Every launch
   (creation, auth gate, `plugin list`, each turn child) execs the image
   through a new AC-005 executor capability, **sealed-image launch**
   (`LaunchRequest.SealedImage{fd, digest, argv0}`): the executor DUPLICATES and owns the descriptor for the launch, verifies the seals are present, re-hashes the descriptor against the frozen digest, and execs `/proc/self/fd/<n>` with `argv[0]` set to the frozen `binary_path` for process-list readability; the frozen argv template is validated exactly as for path launches, and a path launch of Agy is refused in production. The adapter's own held descriptor is only the source it hands over; the adapter never launches anything itself. The operator-path
   self-updater can update the operator's install freely without
   touching what Council runs; a new install becomes launchable only
   after the operator refreshes `binary_digest` at profile freeze, never
   implicitly. Linux-only, consistent with §3.12.
2. **Post-exec identity through `/proc/<pid>/exe` — mandatory, and entirely INSIDE `PolicyExecutor.Start` (the unbypassable seam).** The executor, not the adapter, performs the whole sequence within `Start`: fork a Council-owned trampoline that requests `PTRACE_TRACEME` and execs `/proc/self/fd/<n>`; the kernel stops the child at the exec boundary with `PTRACE_O_EXITKILL` armed, so `/proc/<pid>/exe` is populated and the child cannot run or exit; the executor then verifies identity — for a sealed image the `readlink` target is `/memfd:<name> (deleted)` BY DESIGN, so the check is `st_dev:st_ino` of `/proc/<pid>/exe` == `fstat` of the executor-owned sealed descriptor, plus a hash of the bytes read THROUGH `/proc/<pid>/exe` against the frozen digest — and only on success detaches and returns a RUNNABLE `ManagedProcess` (with pipes armed) carrying the verified executable identity (`memfd` dev:ino, digest) for the launch row. On any failure (mismatch, unreadable link, child not stopped at the exec boundary, ptrace unavailable) `Start` kills the child and returns an error and NO process. The adapter never receives a pid, a stopped child, or ptrace controls, so no caller can skip, reorder, or mishandle the release; every output the adapter ever reads — `models`, `plugin list`, the creation `init`, the turn `init` — comes from a process the executor already verified, and the prompt byte is written only to such a process. There is no `inconclusive` outcome: fast children are observable by construction, and the check either passes or the launch is refused.
3. **Pre-launch re-hash of the sealed descriptor** (adapter, before handing the source to the executor) and the executor-verified executable identity returned by `Start`, both recorded on the launch row.

This is core production evidence, not an operator obligation: Gate 1
(§4) must prove, with Council's own fixture executable and no provider,
(a) a sealed descriptor rejects writes/truncation after sealing and a
descriptor missing any seal is refused, (b) a digest mismatch through
the sealed descriptor is refused pre-launch, (c) `Start` returns no process (child killed) when `/proc/<pid>/exe` dev:ino or hash does not match the executor-owned descriptor, and the executor test suite — not the adapter's — is where this is proven, (d) a fast-exiting fixture
child is verified at the exec boundary and released — never refused,
never trusted unverified, (e) a slow child is verified before its first
output is consumed, (f) production construction refuses to fall back to
a path launch. Because Agy's behavior when its executable is a memfd
(`os.Executable()` → `/memfd:… (deleted)`; install-dir discovery;
sidecar/updater paths) is `not verified`, Stage A (§4) must run
`--version`, `models`, `plugin list`, and the empty-stdin `init` through
the sealed launch and compare with path launches before any profile
freezes; a behavioral difference blocks production until resolved.
Every attestation for a superseded digest is invalid by construction. The operator obligation to disable the auto-updater is
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
                          --   ambiguous_denials_json, unattributed_denials_json,
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
  boundaries; concurrent duplicate creation and dispatch; **binary pin (§3.7, mandatory Gate 1 acceptance): sealed-memfd creation, seal presence and write/truncate rejection, digest re-verify through the descriptor, exec-stop launch with `/proc/<pid>/exe` dev:ino + hash verification before first output, fast-exit child verified at the exec boundary, slow-child ordering, path-launch refusal in production scope**; strict `plugin list` capture
  parsing and drift; hooks capture parsing, `required_hooks` presence and
  disabled-marker rejection; coverage-map decoding and derivation.
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
| Verify expected skills/tools/plugins and enabled guardrails | §3.7 `expected_tools` = `init.tools` at freeze (digest-bound `init_evidence`) and every launch; every tool classified by the pinned coverage map (subagent, scheduling, messaging, and shared-state tools are uncovered ⇒ rejected until natively denied or probed); top-level skill directories, the strict digest-bound `plugin list` capture, and the strictly parsed hooks capture with `required_hooks` present-and-not-disabled re-derived at every launch (configured guardrail state verified; hook EXECUTION and plugin-contributed skill loading explicitly not claimed); `permission_mode` attestation; MCP/plugin tool inventories gated to empty until natively provable |
| Required skipped/denied tools keep verification incomplete regardless of exit code | §3.5 queue-validated immutable `required_tools` (native names) vs observed executed `tool` steps under the pinned denial map intersected with observation ⇒ `missing_required_tools`; `denied_actions` ⇒ `denied_tools`/`ambiguous_denials`/`unattributed_denials`; unmapped denials fail closed; any ⇒ `verification_incomplete`; exit code never classifies (§2.1/§3.9) |
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
verified, §3.2); the subagent inherited tool boundary and the shared-
browser/resource state boundary (those tools are uncovered until probed,
§3.2); Agy's behavior when executed from a sealed memfd (install-dir discovery, sidecars, updater; Stage A compares sealed vs. path launches before any freeze, §3.7); the `plugin_data`
layout for plugin-contributed skills;
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

## 10. v4 changes (review of v3)

1. Coverage map cardinality: each tool maps to a canonical non-empty SET
   of capabilities (closed enum, sorted, deduplicated, `uncovered`
   exclusive); the file shape is specified (§3.2, §3.7).
2. Subagent, scheduling, cross-conversation messaging, and shared-state
   tools are `uncovered` until a live probe proves their boundary; on an
   unmodified install production requires the operator to deny them
   natively, stated plainly (§3.2, §7).
3. Binary pin closes the updater race: post-exec verification through
   `/proc/<pid>/exe` (path, dev:ino, full hash) before any child output is
   trusted or any prompt byte is written (§3.7).
4. Guardrail verification is a strict, canonical, digest-bound hooks
   capture with `required_hooks` pointers that must be present and not
   disabled, re-derived at every launch; execution still not claimed
   (§3.2, §3.7, §6).
5. Denial-map canonical contract and observation-intersected attribution
   with ambiguous/unattributed/unmapped classes, all fail-closed toward
   "not executed" (§3.5, §3.7, §3.11, §6).

## 11. v5 changes (review of v4)

1. Browser state-reading and interaction tools and `list_resources` are
   `uncovered` until cross-session probes establish isolation; `control`
   is reduced to tools with no reach into another conversation's state
   (§3.2).
2. Plugins: a strict, digest-bound, provider-free capture of the
   live-verified `agy plugin list` JSON shape with a canonical
   re-derivation contract at every launch; skills limited to top-level
   real skill directories; plugin-contributed skill dirs recorded as a
   gap (§3.2, §3.7, §6, §7).
3. Binary pin: Council-owned immutable `0500` launch copy removes the
   updater race; `/proc/<pid>/exe` verification and pre-launch hashing
   are mandatory Gate 1 fixture evidence (modification, read-only dir,
   replaced/unlinked image, fast-exit inconclusive, slow-child ordering),
   moved out of the carried-forward list (§3.7, §4, §7).
4. Stale duplicate header row removed from the coverage table (§3.2).

## 12. v6 changes (review of v5)

1. Binary pin: the launch source is a kernel-sealed `memfd` image
   (`F_SEAL_WRITE|SHRINK|GROW|SEAL`) exec'd through a new AC-005
   sealed-image launch capability; the `0500` copy is withdrawn as not
   immutable. Post-exec `/proc/<pid>/exe` verification is mandatory with
   the child held at the exec boundary (exec-stop) until it completes,
   so fast children are always observable and no `inconclusive` outcome
   exists; Gate 1 cases restated; Agy-under-memfd behavior is a Stage A
   precondition to freeze (§3.7, §4, §7).
2. Plugin and `init` contracts are now backed by committed, sanitized,
   provider-free captures with SHA-256 sums; the research file records
   the observed payload shape (§3.2, §3.7; evidence directory).

## 13. v7 changes (review of v6)

1. Plugin evidence: the committed capture is canonical JSON, so the raw
   evidence-root re-hash and the semantic digest are one value; the
   validation order is stated; the canonical-bytes rule applies to every
   capture (§3.2, §3.7; evidence file rewritten, sums refreshed).
2. Sealed-image verification is owned end-to-end by
   `PolicyExecutor.Start`: descriptor duplication, seal/digest checks,
   exec-stop, `/proc/<pid>/exe` verification, detach, and only then a
   runnable `ManagedProcess`; the adapter never sees a pid, a stopped
   child, or ptrace controls (§3.7, §4).
3. Research tool count corrected to 57 everywhere.

## 14. Implementation notes (deltas recorded during plan execution)

1. `memfd_create` flags (§3.7). The v7 text named `MFD_ALLOW_SEALING`
   and close-on-exec only. The implementation additionally passes
   `MFD_EXEC` (Linux 6.3+) and retries with the two-flag set only when
   the kernel returns `EINVAL`. Reason: on 6.3+ kernels a memfd created
   without `MFD_EXEC` is logged as a deprecation warning and, when the
   host sets `vm.memfd_noexec=1`, silently becomes `MFD_NOEXEC_SEAL`
   (adding `F_SEAL_EXEC`); the executor's exact-equality seal check
   would then refuse the launch. Asking for `MFD_EXEC` explicitly keeps
   the image executable where policy allows it and still fails closed
   (typed `ErrSealedImageMismatch`) where the host forbids executable
   memfds (`vm.memfd_noexec=2` returns `EACCES`, which is not retried).
   The seal set and the exact-equality check are unchanged.
2. Child `HOME` (§3.1, §3.7). The frozen `expected_home` is
   `/home/<operator>/.gemini`; the executor's default of setting `HOME`
   to the allocation's config directory would leave every Agy child
   unauthenticated and would put its conversation files where §3.4
   never looks. The launch request therefore carries `HomeDir`, which
   the executor accepts only for an agy-shaped, sealed launch (or the
   test-only fixture marker on a non-Linux host), absolute and clean, and
   emits as the child's `HOME` in place of the allocation config
   directory. The Agy launch source sets it to the PARENT of
   `expected_home` and refuses an `expected_home` that is not absolute,
   clean and named `.gemini`. Every other adapter's launch is unchanged.
3. Orphan conversation id durability (§3.3, §3.11). The v7 schema is
   unreleased, so `agy_turn_attempts.orphan_conversation_id` and the
   creation-uncertainty episode's `orphan_native_id` were added to the
   v7 DDL in place (no v8). A dispatch drift records the observed id
   before the child is terminated; a creation drift after a valid-UUID
   `init` opens a durable creation-uncertainty episode carrying the id,
   which blocks re-creation until a controller resolves it.
4. Attempt insert and launch reservation are one storage transaction
   (`InsertAgyTurnAttemptAndReserveLaunch`), as §3.5 requires; any
   failure before `Start` after that point records `missing`.
5. `DispatchAccepted` as returned by `Dispatch` means the single user
   line was fully transmitted and stdin closed (AC-008 precedent). The
   §3.5 acceptance (`user_input DONE`) is the durable native
   acknowledgement recorded on the attempt; a failure to record it
   surfaces as `DispatchUnknown`, never as a silent success.
6. Termination of a sealed launch signals the child's process group
   (the sealed launch sets `Setpgid`): SIGTERM to the group on the
   graceful path, SIGKILL to the group on the forced path, and after the
   leader exits (for any reason) a peek-then-kill sequence (`waitid` with
   `WNOWAIT`, then `kill(-pgid, SIGKILL)`, then the reap) so no same-group
   descendant outlives the attempt and the group id cannot be reused
   before the kill. A descendant that changes its own session or process
   group escapes this; the operator evidence script (Task 8) observes
   that limit.
7. §3.8 diagnostics (conversation file size and step counts at first
   acceptance and at reconcile) are not implemented by the adapter task
   and are tracked as an acceptance-pass gap.
8. Gate order at construction (§3.2). `NewProductionAgyAdapter` runs the
   durable eligibility checks (platform, sealed image digest, empty
   inventories, attestation tuple re-compare and coverage) BEFORE the
   filesystem toolkit checks and before the construction `plugin list`
   child; no child starts until eligibility passes. The `homeDir`
   argument must equal the parent of `expected_home`.
9. Auth gate at resume (§3.2). `ResumeSession` does not itself launch
   `models`; it invalidates the adapter's cached auth pass so the gate
   runs again before the next dispatch of that session. Passes are
   cached per adapter for a bounded TTL (default 5 minutes); failures
   are never cached.
10. Gate children (`models`, `plugin list`) leave no launch rows: §3.11
    keys launch rows by turn attempt, and these children carry no
    attempt. Their evidence is the typed gate outcome only.
11. Hooks configured state (§3.2): a `command`/`handler` value that is
    present but not a non-empty string is treated as drift (stricter
    than the text, which names the string case only); disabled markers
    are checked on the resolved subtree, not on its ancestors.
12. The recorded `models` catalog shape is "14 entries" without a column
    layout; the auth gate parses one id per row and treats any other
    layout as inconclusive (fails closed). Stage A of the evidence script
    records the real layout before any profile freeze.
13. Durable pre-launch creation marker (§3.3, §3.11). Because Agy
    cannot re-offer a created conversation across processes, the
    service opens a provisional creation-uncertainty episode (reason
    `creation_in_flight`, create-op provenance) BEFORE the creation
    child starts. A successful bind closes it with disposition `bound`
    inside the bind transaction; a pre-child rejection closes it
    `not_created`; every other outcome (uncertain, drift, refused bind,
    client disconnect, crash) leaves it open, annotated with the
    observed or orphan native id when known, and blocks re-creation
    until a controller resolves it. Exactly one open episode exists per
    session; the adapter's own drift record annotates the open marker
    instead of opening a second episode.
14. Client disconnect is not cancellation (AGENTS.md). Once the native
    conversation exists, every durable write that follows (bind, orphan
    annotation, marker close) runs detached from the request context.
15. Required tools seam (§3.5). `RequiredToolsFor` returns an error
    channel; a storage read error or a missing dispatch intent refuses
    the dispatch before any reservation. Only an intent with an empty
    set falls back to the frozen `default_required_tools`. Queue-time
    validation rejects duplicates (spec text) rather than deduplicating,
    and the queued set is immutable across prompt replacement (typed
    refusal on a changed set).
16. Attestation recording authority (§3.2) is the operator credential,
    matching the codex precedent; the tuple written to the row comes
    from the run's frozen profile and coverage, never from the request.
17. Turn-attempt disposition surface (§3.10, §6.1 rows 2–3), completed
    following the operator's P1 review request. The connected controller
    may POST `abandoned` with a retirement reason to
    `/v1/runs/{run_id}/sessions/{session_id}/turns/{turn_key}/agy-disposition`.
    The request includes `op_id`, `controller_lease`, `expected_generation`,
    `expected_version` and the exact `attempt_id`. Current authority and
    generation are checked before replay; new decisions also require the
    current connection, session version and unresolved execution identity.
    Locally live launches are refused, including the pre-init interval.
    After host loss the controller is responsible for confirming retirement
    of the old execution before abandonment. One transaction records the
    disposition, interrupts the Council turn, resolves its dispatch intent,
    parks the session, closes its recovery episode and journals the reason,
    attempt and controller generation. Native status remains Uncertain;
    no native terminal result or successful cancellation is invented.
    Replacement work needs a new queue/release. The acceptance test uses
    HTTP for disposition and the next release, both before and after restart.
    AC-008/AC-009 disposition surfaces remain outside this change.
18. Attestation bootstrap (§3.2, §4). Production construction requires
    a covering attestation row, and the first row must be recordable
    before the adapter exists. `RecordAgyProbeAttestation` therefore
    depends only on the store, the operator credential and the
    configured agy profile and evidence root, never on a wired adapter;
    and a server configured with `AgyBinaryPath` whose only
    ineligibility is the missing or uncovered attestation starts WITHOUT
    the agy adapter in an explicit "awaiting attestation" state
    (surfaced on the server status surface and logged; birth, dispatch,
    reconcile and queue-time validation refuse with the typed
    ineligibility error; other operations report the harness as
    unavailable). After the row is recorded, a restart constructs the adapter.
    No child runs before eligibility in either state. Following the
    operator's P1 review request, POST `/v1/runs/{run_id}/agy/attestations`
    exposes `RecordAgyProbeAttestation` using the operator bearer credential.
    Its JSON envelope contains `op_id`, `actor`, optional `attestation_id`
    and the typed `attestation`; the run and tuple cannot be overridden in
    the body. The restricted controller bridge exposes no attestation or
    arbitrary-route operation. The existing coverage validation and
    journaled receipt replay apply; there is still no hot reload.
19. Evidence helpers (§4 Stage A). The sealed-vs-path comparison and the
    freeze-time canonical digests use two `-tags evidence` Go test
    helpers instead of a new binary: `TestSealedProbe`
    (`internal/adapter/execpolicy/sealed_probe_evidence_test.go`:
    `NewSealedImage` + `PolicyExecutor.Start` with the operator's argv,
    home, and cwd) and `TestEvidenceCanonicalDigests`
    (`internal/adapter/agy/evidence_digests_test.go`:
    `canonicalPluginsBytes` and `CanonicalHooksConfigDigest`). Both are
    excluded from every build without the tag and skip without their
    operator environment. The script's path launches reproduce the
    executor's child environment exactly (PATH TMPDIR TERM LANG LC_ALL
    USER, HOME, COUNCIL_*), so the comparison isolates the exec
    mechanism.
