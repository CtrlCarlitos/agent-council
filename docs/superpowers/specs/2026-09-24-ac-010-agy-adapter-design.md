# AC-010 Design — Agy (Antigravity CLI) persistent contributor adapter

Status: DRAFT v1 for review
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
  and lookup.
- **Approval records:** Agy has no per-variant approval wire protocol; the
  permission decision is native (`request-review` auto-deny) and surfaces
  in `result.denied_actions`. The attestation's `approval_deny` records
  therefore carry ONE method, `denied_actions`, with
  `refusal_kind=native_refusal_enum`, proven by a live denial in the suite.
- **Gate ordering:** (1) before any child starts: attestation valid, binary
  digest matches (§3.7), frozen inventories present; (2) child starts:
  `init` verified (§3.1); (3) auth: because `init` proves nothing about
  sign-in, the FIRST turn of a conversation is the auth gate — an
  `error_message`/`result` ERROR mentioning sign-in or `RESOURCE_EXHAUSTED`
  is a typed pre-acceptance failure only if it arrives BEFORE any
  `user_input` step; after that it is a failed/uncertain turn.
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
  ⇒ DispatchRejected; failure after ⇒ DispatchUnknown.
- Terminal: exactly one `result` per process. `SUCCESS` ⇒ `TurnCompleted`
  with `response` as the result payload, usage as observed; `ERROR` with
  `error=="interrupted"` ⇒ `TurnCancelled`; other `ERROR` ⇒ `TurnFailed`
  with the error text. `denied_actions` non-empty ⇒ one `tool_denied` event
  per action and the result flagged `verification_incomplete` (the turn is
  complete but every denied action is a required-tool gap; acceptance of
  the contributor's output is the controller's decision, never automatic).
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

### 3.7 Frozen profile (cprof-v4, additive) and the binary pin

`harnesses.agy` gains a typed `agy` block (additive; v1–v3 encodings stay
byte-identical; compatibility matrix extends AC-009's: OpenCode/Claude/
Codex accept and ignore v4, Agy requires it):

```json
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
  "expected_tools": ["…exact init.tools inventory…"],
  "expected_mcp_servers": ["…from `agy mcp list`…"],
  "expected_plugins": ["…from `agy plugin list`…"],
  "hooks_evidence": {"verified": ["…"], "unverifiable": ["hook execution at the Agy layer", "settings.json permissions.allow contents"]}
}
```

- `binary_digest` closes hazard 1: production construction and EVERY
  launch re-hash the binary (cached by size+mtime+inode, full sha256 on any
  change) and fail closed on drift (`ErrBinaryDrift`). The operator
  obligation to disable the auto-updater is recorded as unverified; the
  pin is the defense either way.
- `expected_tools` is **provable natively and provider-free**: the
  creation launch's `init.tools` (§3.3) must equal it exactly at freeze
  (via the operator probe) and at every launch (§3.1). Unlike AC-009, no
  inventory gap exists for the built-in tool set; MCP tool names inside
  `call_mcp_tool` remain `not verified` and are listed as such.
- `permission_mode` ∈ {`request-review`, `strict`}; `always-proceed`,
  `proceed-in-sandbox` are structurally rejected at freeze.
  `execution_mode` ∈ {`default`, `accept-edits`, `plan`}.

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
| `result` ERROR before any `user_input` step | pre-acceptance failure (auth/quota/validation), typed |
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
                          --   denied_actions_json, verification_incomplete BOOL,
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
- Windows/macOS: launch path is portable; the ownership/mode check on the
  conversation file is POSIX-only (fail-closed statement elsewhere).

## 5. Boundaries

- Never `--dangerously-skip-permissions`, `--continue`, `-i`,
  `--remote-control`, `install`, `update`, the alias, or any shell.
- Never read, copy, relocate, or symlink the keyring/credential file;
  `agy models` output is the only sign-in evidence and is network-bound.
- Never write `settings.json`, `hooks.json`, `mcp_config.json`, or
  `permissions.allow`; grants are operator-provisioned.
- Every launch through the AC-005 executor with the frozen argv; cwd pinned
  to the AC-005 workspace; model/mode/sandbox only from the frozen profile
  and verified against `init` before transmission.
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
| Verify expected skills/tools/plugins and enabled guardrails | §3.7 `expected_tools` = `init.tools` at freeze and every launch; MCP/plugin lists; `permission_mode` attestation; hooks recorded as unverifiable |
| Required skipped/denied tools keep verification incomplete regardless of exit code | §3.5 `denied_actions` ⇒ `verification_incomplete`; exit code never classifies (§2.1/§3.9) |
| Bounded cancellation, unknown outcomes, client-close recovery, no fallback harness | §3.6 SIGINT → verified `interrupted`; §3.9/§3.10 Uncertain rules; §3.1 detached context; no other adapter is ever substituted |

### 6.1 Concrete acceptance scenarios

1. Concurrent duplicate dispatch — one process, shared verdict.
2. Crash after the stdin write, `result` lost — Uncertain; block persists
   across restart; controller disposition required.
3. Process death mid-turn — Uncertain; next turn is a new process on the
   same conversation after `init` equality.
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

`--mode`/`--sandbox` effect headlessly; `denied_tools` rule denial shape;
workspace-trust gating; healthy-turn print-timeout; content-block user
messages; `AGY_ERROR` exit-3 path; MCP tool naming in `call_mcp_tool` steps;
plugin/skill tool surfacing; auto-updater off switch; `GEMINI_API_KEY`
mode; Windows/macOS behavior. Each is a Stage B or operator obligation and
none is claimed by the design.
