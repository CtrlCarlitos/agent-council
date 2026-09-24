# AC-010 evidence: installed Agy (Antigravity CLI) interface research (provider-free)

Date: 2026-09-24. Binary: `/home/carlitos/.local/bin/agy` (ELF x86-64, Go
`go1.28-20260721-RC03`, stripped, dynamically linked). Version at first probe:
`1.2.7`; **the binary self-updated to `1.2.9` in the background during this
research** (see §0 hazard 1). Version at last probe: `1.2.9`, sha256
`1dbb10f8295cc1ad2e558bd006c7808fe53b6c7f678a887eb557b576bb591711`,
217,424,080 bytes, mtime 2026-09-24 00:06:33 -0600. The `--help` flag and
subcommand surface is byte-identical between 1.2.7 and 1.2.9 (both captured).

Method statement: **every claim is tagged** `live-verified (1.2.9)` or
`live-verified (1.2.7)` (executed against the installed binary),
`binary-derived (provider-free)` (read from the binary's own help text,
changelog subcommand, or `strings` of the 1.2.7 binary — no model call), or
`not verified`. No official documentation was consulted (binary-derived only,
same discipline as AC-009). The `agy changelog` subcommand is the vendor's
release-notes text shipped inside the binary; claims taken from it are tagged
`binary-derived (changelog)` and are treated as vendor statements, not as
verified behavior.

Safety rules followed:

- **Zero model calls.** Every `--print` probe used `--input-format
  stream-json` with EMPTY or MALFORMED stdin, which the CLI rejects or
  ignores BEFORE any turn starts (live-verified: no `step_update`, no
  `result`, no token usage). No prompt text was ever sent.
- The operator's interactive shell aliases `agy` to
  `agy --dangerously-skip-permissions` (`whence -v agy`, source file not
  located in the standard rc files). **Every probe invoked the binary by
  absolute path**, never through the alias, and no bypass flag was ever
  passed. `--dangerously-skip-permissions`, `--remote-control`, `-c/--continue`
  appear below only as help-text evidence for the forbidden-flags list.
- Credential material was never read, copied, or moved. The credential lives
  in the OS keyring with a file fallback under `~/.gemini/antigravity-cli/`
  (file name omitted here); only its existence by name pattern was observed.
  `settings.json`, `config.json`, `hooks.json`, `mcp_config.json`, and the
  project descriptor were inspected as **key names only** (`jq keys`), never
  values — MCP server entries can carry env secrets.
- `agy models` (network, uses the CLI's own stored sign-in; no model call) was
  run twice to capture the model catalog. `agy remote-control status` was run
  once (read-only: daemon inactive). `install`, `update`, `remote-control
  start`, `mic-serve`, `plugin install/import`, `mcp add/remove` were never
  executed.
- **Side effects, recorded honestly:** each headless probe created one EMPTY
  conversation in the operator's CLI state
  (`~/.gemini/antigravity-cli/conversations/<uuid>.db`, 0 steps, mode 0600).
  Eleven were created on 2026-09-24 between 00:11 and 00:15:
  `8da339d7-1d70-4735-8ae2-ee404dae0305`, `c191a0d5-ffbb-43fc-b3c0-cd87d4a3230c`,
  `077faa9d-10f7-4ee4-a496-084f11a9d718`, `5dce8217-7414-4207-9516-7a58b561cea1`,
  `d70b0c5a-a32f-46a2-9863-75e387d72a0a`, `f0522bc0-02c8-4385-ab61-df7166cca088`,
  `1b4c09bb-ae5b-463b-8a7c-d6b08d967fbb`, `47ddfd27-23f5-43b6-bc02-39ebdfcfce5f`,
  `f2d60d08-ef3e-42ce-b5d6-dd98618c8003`, `fb583d24-3cd2-4e69-add0-0bddf53f9cc8`,
  `02fa7419-01bf-4f06-9b85-799b43927f45`. Nothing was deleted or modified
  under `~/.gemini`; removal of these empty records is an operator purge
  action (Council never deletes native state).

---

## §0 Decisive findings and hazards (summary)

1. **Background self-update changes the binary under a running deployment**
   (live-verified): the installed binary went from `1.2.7` to `1.2.9` between
   the first `--version` probe and the stream-json probes (mtime 00:06:33; an
   `updater/` directory and a `bin/` payload appeared/refreshed at 00:06–00:15;
   `strings` shows `cli/updater/auto_updater.go`, `JB_AUTO_UPDATED`,
   `AUTO_UPDATER_TEST_MODE`, `update_channel`). No documented off switch was
   found (`not verified`). Consequence for Council: the frozen profile must
   pin version AND binary digest; every launch must re-verify both and fail
   closed on drift; an update mid-run invalidates attestations by construction.
2. **`--conversation <id>` silently falls back to a NEW conversation** when
   the id is absent or malformed (live-verified 1.2.9): stderr
   `warning: conversation "<id>" not found`, exit 0, and the `init` event
   carries a DIFFERENT, freshly generated `conversation_id`. This is the same
   class of hazard AC-009 found for Codex `resume`, but worse: even a
   well-formed absent UUID is not an error. Council must treat
   `init.conversation_id != requested id` as a protocol violation (fail closed,
   never bind), and must never rely on the exit code for resumption.
3. **Conversation identity is CLI-generated**, not caller-chosen
   (live-verified): there is no session-id input flag; a fresh headless run
   emits `init.conversation_id` (UUID v4 shape). Creation happens at process
   start, before any turn, and persists (an empty `<uuid>.db` is written).
4. **Unsupported stream input is ignored with a warning, not rejected**
   (live-verified): `{"event":"bogus"}` → stderr `warning: ignoring
   unsupported stream input message event "bogus"`, exit 0. A typo'd or
   drifted message shape is silently dropped; Council must treat that stderr
   warning as a dispatch-rejected condition and never assume a turn ran.
   Missing `event` field and non-JSON lines DO fail closed (exit 1, no turn).
5. **Headless exit code does not reflect denials** (binary-derived,
   changelog): "print mode … treating benign tool execution errors and
   permission denials as fatal run failures with non-zero exit codes" was
   FIXED so that "headless exit codes reflect only cascade-level failures".
   Matches the issue's acceptance criterion: denied/skipped tools must keep
   verification incomplete regardless of exit code.
6. **`--print-timeout` expiry returns PARTIAL output with exit 0** and a
   warning on stderr (binary-derived, changelog). A timeout is therefore
   indistinguishable from success by exit code; the `result` event and the
   stderr warning are the only signals (`not verified` live).
7. **`init` is emitted even when not signed in** (live-verified with
   `--log-file`: the CLI's own log recorded `You are not logged into
   Antigravity` / `Auth mode is unspecified` while `init` was still printed
   with the full tool list). `init` therefore proves nothing about auth; the
   auth gate needs a separate provider-free signal (`not verified`; candidate:
   the `AGY_ERROR` line on the first turn, or `agy models` as sign-in
   evidence — the latter is network-bound).
8. **Headless permission behavior is not established provider-free.**
   `init.permission_mode` is `request-review` by default (live-verified).
   Mode names seen in the binary: `always-proceed`, `request-review`,
   `strict`, `proceed-in-sandbox` (binary-derived); the changelog states
   headless runs honor persisted `settings.json` policies including
   `permissions`, file access, sandbox mode, and that `--sandbox` propagates
   to headless. What a `request-review` prompt does with NO human (deny?
   wait? "The user did not respond in time, continue to make progress with
   your best judgement" exists as a prompt-template string) is `not
   verified` and is the first Stage B question.
9. **Isolation via `HOME` relocation works for config but not auth**
   (live-verified 1.2.9): `HOME=<empty dir> agy mcp list` reported "No MCP
   servers configured" and created NOTHING under the relocated home. The
   sign-in credential is keyring/file-backed under the real home, so an
   isolated home is unauthenticated — the AC-009 shared-home constraint
   applies again (never copy credentials).
10. **Durable transcript is protobuf-in-SQLite, not JSONL** (live-verified):
    each conversation is `<uuid>.db` with tables `steps(idx, step_type INT,
    status INT, has_subtrajectory, metadata BLOB, error_details BLOB,
    permissions BLOB, task_details BLOB, render_info BLOB, step_payload BLOB,
    step_format INT)`, `trajectory_meta`, `trajectory_metadata_blob`,
    `gen_metadata`, `executor_metadata`, `parent_references`,
    `battle_mode_infos`. The proto schemas are NOT pinned by any committed
    evidence, so a rollout-style entry-correlation trust model (AC-008/009)
    is not available at the payload level; only row-level facts (step count,
    `step_type`/`status` integers, file identity/size) are readable without
    a schema. Treat as advisory-only evidence until a schema is pinned.

## §1 Surface inventory (`agy --help`, live-verified 1.2.7 and 1.2.9 — identical)

Flags: `--add-dir` (repeatable workspace dir), `--agent`, `-c/--continue`
(most recent conversation — **forbidden**, identity must be exact),
`--conversation <ID>` (resume by id; see hazard 2),
`--dangerously-skip-permissions` (**forbidden**), `--disable-slash-commands`
(print mode: disables slash-command/skill expansion), `--effort
low|medium|high`, `-i/--prompt-interactive` (**not used**: interactive),
`--input-format text|stream-json` (stream-json: one NDJSON message per line,
one turn per message, requires `--output-format stream-json`),
`--json-schema` (structured final `result`), `--log-file`, `--mode
accept-edits|plan` (execution mode; changelog: `default → accept-edits →
plan`), `--model`, `--new-project`, `--output-format text|json|stream-json`,
`-p/--print`/`--prompt` (non-interactive; **valueless `--print` swallows the
next flag as its prompt** — live-verified twice; use `--print=` or put the
prompt/flag order deliberately), `--print-timeout` (0 = wait; default 0),
`--project`, `--remote-control` (**forbidden**), `--sandbox` ("terminal
restrictions enabled").

Subcommands: `agent`/`agents` (list custom agents; printed nothing here —
none configured; `--output-format json` NOT accepted on 1.2.7/1.2.9 despite a
changelog entry claiming it — discrepancy recorded), `changelog` (local
release notes, 55+ versions), `help`, `install` (**never run**; configures
PATH and shell aliases — likely origin of the operator's bypass alias, `not
verified`), `mcp add|remove|list|enable|disable`, `mic-serve` (**never run**),
`models` (network; requires sign-in), `plugin list|import|install|uninstall|
enable|disable|validate|link`, `remote-control start|status|stop` (**never
started**; status: inactive, systemd user unit `antigravity-cli-daemon`),
`update` (**never run**).

## §2 Identity, creation, resumption (live-verified 1.2.9, provider-free)

| Probe (empty stdin, stream-json) | Result |
|---|---|
| fresh run, cwd = temp dir | stdout: one `init` event; new `<uuid>.db` created; exit 0; no `result` event |
| `--conversation <existing uuid from the previous probe>` | `init.conversation_id` == requested id (exact resumption of the identity, no turn); no new file |
| `--conversation 00000000-0000-0000-0000-000000000000` | stderr `warning: conversation "…" not found`; **new** conversation id in `init`; exit 0 |
| `--conversation definitely-not-a-real-id` | same silent fallback with a new id; exit 0 |
| stdin EOF with no message | exit 0, only `init`, no `result` |
| idle with stdin held open for 6 s | process stays alive (driver can keep the session open) |
| SIGINT / SIGTERM while idle | exit 0, only `init`, no terminal event |

`init` event shape (verbatim keys): `{"event":"init","conversation_id":"<uuid>",
"init":{"cwd":"<abs>","tools":[<58 names>],"permission_mode":"request-review"}}`.
Tool names observed (1.2.9): `ask_custom_permission, ask_permission,
ask_question, browser_* (17), browser_subagent, call_mcp_tool,
capture_browser_console_logs, capture_browser_screenshot, click_browser_pixel,
command_status, define_subagent, delete_knowledge, execute_browser_javascript,
find_by_name, finish, generate_image, grep_search, invoke_subagent,
list_browser_pages, list_dir, list_permissions, list_resources, manage_inbox,
manage_subagents, manage_task, multi_replace_file_content, notebook_edit,
notebook_execution, open_browser_url, read_browser_page, read_resource,
read_url_content, replace_file_content, run_command, schedule, search_web,
sed_file, send_command_input, send_message, view_file, wait, wait_5_seconds,
write_to_file`. The changelog notes the `init` tools list once advertised
tools "not available in your build" (fixed) — treat `init.tools` as the
inventory evidence channel but verify against the frozen expected set.

## §3 stream-json protocol (partly verified)

- Output events (binary-derived, changelog): typed `init`, `step_update`,
  terminal `result`, with a closed-vocabulary `step_type` discriminator; the
  `usage` object reports token accounting including `cache_read_tokens`;
  `--json-schema` applies to the final `result`. Only `init` was observed
  live (no turn was ever run). JSON tags present in the binary
  (binary-derived): `type, subtype, session_id, conversation_id, step_type,
  status, tool_name, result, error, content, usage, input_tokens,
  output_tokens, cache_read_tokens, num_turns, duration_ms, cost, model, cwd,
  project, version, permission`.
- Input messages (live-verified): NDJSON objects that MUST carry `"event"`;
  a missing field → stderr `error: stream input message is missing the
  "event" field`, exit 1; non-JSON → `error: failed to decode stream input:
  …`, exit 1; unsupported event name → warning + ignored, exit 0 (hazard 4).
  The user message is `"event":"user"` with a required content
  (`stream input "user" message has no content`, binary-derived; symbols
  `streamInputUserMessage`, `streamInputContentBlock`, `streamInputResult`).
  The exact content shape (string vs. content blocks) is `not verified`: it
  can only be learned by sending a real user message (Stage B).
- Headless error channel (binary-derived, changelog): on agent/model API
  failure the CLI prints `AGY_ERROR: {...}` JSON on stderr (canonical
  status, HTTP/gRPC code, retryability, error id) and exits with code `3`;
  server-side request failure → non-zero exit with stderr message.
- Timeouts (binary-derived, changelog): headless default timeout is
  unlimited; `--print-timeout` expiry returns partial output and exits 0 with
  a stderr warning; background tasks are awaited up to the deadline (30-min
  cap); daemon background processes are terminated at run end (1.2.9).

## §4 Permissions, modes, sandbox, hooks (binary-derived unless noted)

- Permission modes: `always-proceed`, `request-review` (headless default,
  live-verified), `strict`, `proceed-in-sandbox`. `--dangerously-skip-
  permissions` auto-approves everything (forbidden). "Admin escalation
  permissions cannot be auto-approved and will always prompt the user."
- Denial configuration: settings carry `denied_tools` and
  `denied_tool_prefixes` (proto field names; non-empty prefix rule enforced) —
  candidate frozen-deny mechanism, `not verified` in headless.
- Execution modes: `default`, `accept-edits`, `plan` (`--mode`; changelog says
  a past bug ignored `--mode` in headless — verify live).
- Sandbox: `--sandbox` enables terminal restrictions; changelog states
  headless honors `settings.json` `permissions`, file access, sandbox mode,
  auto-execution, artifact review, and that `--sandbox` propagates to
  headless. "Bypass Sandbox Mode … requires manual user approval". `not
  verified` live.
- Hooks: `PreToolUse` / `PostToolUse` contracts; user hooks in
  `~/.gemini/config/hooks.json` (the operator's file has one top-level key,
  `guardrail`); workspace-local hooks in `<workspace>/.agents/hooks.json`
  load after the folder is trusted. Plugins may bundle hooks and MCP servers
  (namespaced `<plugin>_<server>`).
- Trust: `settings.json` (CLI) carries `trustedWorkspaces`; the changelog
  ties hook loading to trusting a folder. Headless `init` was emitted in an
  untrusted temp cwd, so trust does not gate startup (`live-verified`);
  what it gates during a turn is `not verified`.

## §5 State layout (names/keys only, live-verified)

- `~/.gemini/config/`: `config.json` (keys `plugins`, `userSettings`),
  `hooks.json` (key `guardrail`), `mcp_config.json` (servers `graft`,
  `serena`; `agy mcp list` shows both stdio, enabled), `import_manifest.json`,
  `projects/default-cli-project.json` (keys `id`, `name`, `projectResources`),
  `plugins/` (`mattpocock-skills`, `superpowers`).
- `~/.gemini/antigravity-cli/`: `conversations/<uuid>.db` (SQLite, 0600),
  `conversation_summaries.db` (table `conversation_summaries`:
  `conversation_id, title, preview, step_count, last_modified_time,
  workspace_uris, status, source, project_id, agent_name,
  parent_conversation_id, nesting_depth, battle_id, winning_conversation_id,
  not_fully_idle, killed, last_user_input_time, last_user_input_step_index,
  app_data_dir, raw_summary, group_id`), `history.jsonl` (prompt history —
  keys `display, timestamp, workspace`; NOT read further), `settings.json`
  (keys `colorScheme, model, trustedWorkspaces`), `cli.log` + `log/`
  (glog-style text lines), `skills/`, `mcp/`, `plugin_data/`, `bin/`,
  `updater/`, `installation_id`, the keyring-fallback credential file.
- `~/.gemini/antigravity/` and `~/.gemini/antigravity-ide/`: IDE-side state
  (`conversations/<uuid>.pb`) — separate products, not used by the CLI runs.
- Plugins imported: `superpowers` (source `gemini-cli`, components skills +
  hooks). Models catalog (network): 14 entries incl. `gemini-3.8-flash-*`,
  `gemini-3.1-pro-*`, `claude-sonnet-4-6`, `claude-opus-4-6-thinking`,
  `gpt-oss-120b-medium` — model choice is per-session `--model`; provider is
  the signed-in Antigravity backend (alternative `GEMINI_API_KEY` mode exists,
  `not verified`).

## §6 Open questions for the operator-authorized live stage (Stage B)

1. Exact `"event":"user"` input content shape; the `step_update` and
   `result` event shapes and the closed `step_type` vocabulary; `usage`.
2. What `request-review` does headlessly with no human: deny, wait, or
   proceed on timeout — and which stream event records the outcome.
3. Whether `denied_tools`/`denied_tool_prefixes` (settings) produce a
   structured denial step, and whether `--mode plan` blocks edits.
4. Cancellation of an IN-FLIGHT turn: SIGINT vs. closing stdin vs.
   `--print-timeout`, and the terminal event/exit code each produces.
5. Whether `init.tools` equals the frozen expected inventory (skills, MCP
   tools via `call_mcp_tool`, plugin hooks) and how MCP tool names surface.
6. Auth gate: a provider-free signal that the child is signed in before a
   prompt is transmitted.
7. Auto-update control: whether a setting or env var disables the background
   updater; otherwise the adapter's version+digest pin is the only defense.
