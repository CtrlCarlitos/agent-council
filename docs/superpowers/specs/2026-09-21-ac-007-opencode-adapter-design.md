# AC-007: OpenCode/GLM Persistent Contributor Adapter — Design

- **Status:** Proposed (research against installed OpenCode 1.18.31)
- **Date:** 2026-09-21
- **Issue:** [#7 (AC-007)](https://github.com/CtrlCarlitos/agent-council/issues/7)
- **Dependencies:** AC-003 (#3, closed), AC-005 (#5, closed), AC-006 (#6, closed)
- **Target Packages:** `internal/adapter/opencode` (new), wiring in `internal/service`

---

## 1. Problem and Scope

OpenCode/GLM must contribute independently even while an existing OpenCode
conversation controls the run. This task implements the AC-006
`adapter.Adapter` contract against the **installed** OpenCode interface and
records real conformance evidence — CLI flags and session behavior are taken
from the installed binary (1.18.31), never assumed.

In scope: headless-server lifecycle, fresh contributor sessions, exact-session
resume, async dispatch with event observation, abort, usage collection,
permission (approval/denial) semantics, model validation, reconcile honesty.

Out of scope: native provider execution in ordinary CI (deferred to fixture
adapters; real-provider conformance evidence is captured manually per the
issue's sanitized-evidence requirement), other harnesses (AC-008–AC-010),
quota-bypass flags of any kind.

## 2. Installed-Interface Research (verified 1.18.31)

### 2.1 Two invocation surfaces

| Surface | Form | Role in adapter |
|---|---|---|
| Headless HTTP server | `opencode serve --hostname 127.0.0.1 --port 0` (port 0 ⇒ random port; actual port printed at startup; PID discoverable via listener) | Primary: session create/resume/prompt/observe/abort over a documented OpenAPI 3.1 surface (162 paths at `GET /doc`) |
| One-shot CLI | `opencode run [message..] -s <sessionID> -m provider/model --agent <agent> --format json` | Rejected as primary: no streaming events, process-per-turn clashes with persistent-session design. `--attach` is how external clients reach the server; basic auth via `OPENCODE_SERVER_USERNAME`/`OPENCODE_SERVER_PASSWORD` |

Read-only inspection commands used (no provider calls): `--help` on
`serve/run/session/models`, `session list --format json`, `models`, live
`serve` + `GET /doc`, `GET /api/health`, `GET /session/status`,
`GET /config/providers` (see §2.4 credential warning).

### 2.2 Verified HTTP surface (relevant subset)

| Endpoint | Adapter mapping |
|---|---|
| `GET /api/health` → `{"healthy":true}` | Probe liveness; server readiness gate |
| `POST /session` — body: `{title, agent, model:{providerID,id,variant}, permission, metadata, workspaceID?}` → session record (`ses_…`) | **CreateSession**: fresh contributor session, project-scoped to the contributor's workspace directory; model preset from HarnessProfileSpec |
| `GET /session`, `GET /session/{id}` | **ResumeSession**: exact-session existence check (never "most recent") |
| `POST /session/{id}/prompt_async` — body: `{parts:[TextPartInput…], model:{providerID,modelID}, agent, noReply, tools, system, variant}` | **Dispatch**: async submission; returns immediately; completion observed via events/messages |
| `GET /session/{id}/event`, `GET /event`, `GET /global/event` | **Observe**: per-session SSE progress stream |
| `GET /session/{id}/message`, `GET /session/{id}/message/{messageID}` | **Collect**: final assistant message parts (text, usage, cost, tokens) |
| `POST /session/{id}/abort` | **Cancel**: cooperative abort; turn ends with abort semantics (not fabricated completion) |
| `GET /permission`, `POST /permission/{requestID}/reply`, `GET /api/session/{id}/permission`, `POST /api/session/{id}/permission/{requestID}/reply` | **Approval wait / tool denial**: permission requests surface on the event stream and permission endpoints; adapter replies deny by default unless Council policy pre-approves |
| `GET /api/model`, `GET /config/providers` | **Probe/CreateSession model validation**: intended provider+model must be listed and authenticated |
| `POST /session/{id}/fork`, `POST /session/{id}/summarize`, `GET /session/{id}/todo` | Not needed for AC-004-style flow; noted for future tasks |

### 2.3 Verified behaviors and hazards

- **Session scoping is directory/project-based.** Sessions belong to a
  project derived from the working directory. The contributor adapter MUST
  create sessions with the contributor's isolated workspace directory (from
  the AC-005 workspace manager) as the OpenCode project directory, so
  contributor sessions never mix with the controller conversation's project.
- **`run -c/--continue` resumes the most recent session — forbidden** as a
  resume mechanism (issue: "do not select the most recent unrelated
  session"). Only exact session IDs (`-s <ses_…>` / `GET /session/{id}`) are
  used.
- **`--auto` (auto-approve permissions) is a guardrail bypass — forbidden**
  in every invocation the adapter makes. Permission requests are answered
  explicitly: deny by default; approval only if Council policy pre-approved
  the exact tool/pattern.
- **`GET /config/providers` returns raw provider API keys.** The adapter
  treats that response as a credential: never logged, never forwarded, never
  stored. Model validation uses presence + auth status fields only.
- **`prompt_async` + `noReply`** allow fire-and-forget submission with
  completion observed via events — matches Dispatch/Observe split.
- **Server lifecycle is independent**: `opencode serve` is an OS child with
  no parent-death guarantee (AC-004 lesson applies). The adapter records the
  child PID and health-checks it; Reconcile treats "server unreachable" as
  uncertain/host-lost, never as definitive failure.
- **Basic auth** on the server (`--hostname 127.0.0.1`, optional
  username/password) is supported by both `serve` and `run --attach`; the
  adapter generates a random local credential per server instance (AC-003
  pattern: ephemeral owner-scoped secret, never reused across instances).

## 3. Architecture

### 3.1 Server lifecycle (adapter-owned)

`OpenCodeServer` (new, `internal/adapter/opencode/server.go`):

- Spawns `opencode serve --hostname 127.0.0.1 --port 0 --print-logs` as a
  child of the Council service (AC-003 ownership rule), with:
  - `OPENCODE_SERVER_USERNAME`/`OPENCODE_SERVER_PASSWORD` set to a generated
    ephemeral pair (32-byte hex), never persisted beyond the process env.
  - Working directory: the contributor workspace root (project scoping).
  - stdout/stderr drained and captured (termination-evidence pattern from
    AC-005).
- Waits for `GET /api/health` → `healthy:true`, then parses the printed
  listen address. Startup deadline → error (fail closed).
- `Close()`: graceful `POST /global/dispose` if available, then
  `terminateGracefully` → force-kill (AC-005 terminate split), drain pipes
  before Wait, record exit evidence.
- Records PID + exit status for Reconcile honesty (no fabricated liveness).

One server per Council service instance is shared by all OpenCode
contributor sessions; sessions themselves are per contributor session.

### 3.2 Adapter (`internal/adapter/opencode/adapter.go`)

Implements `adapter.Adapter` (AC-006) over the server's HTTP API via a
minimal typed client (`httpclient.go`):

| AC-006 method | Behavior |
|---|---|
| `Probe` | `GET /api/health` + `GET /config/providers` (redacted parse: provider id, model ids, auth presence) + server version. Returns ProbeReport with capability flags (streaming supported via SSE). |
| `CreateSession` | `POST /session` with `{title: "council <sessionID>", agent, model: HarnessProfileSpec.Model}`; model validated against the providers list first (fail closed: unknown/unauthenticated provider+model ⇒ error). Records `NativeSessionID = ses_…`. Fresh session: no history copy — OpenCode sessions are new by construction; the controller's OpenCode project is never referenced. |
| `ResumeSession` | `GET /session/{nativeSessionID}`: 200 ⇒ verified (id + project match asserted); 404 ⇒ `ErrSessionCreationUncertain`-style typed error (session gone). Never falls back to newest-session selection. |
| `Dispatch` | `POST /session/{id}/prompt_async` with the prompt as a text part, the preset model, and Council-configured agent. Returns `DispatchAccepted`. Non-2xx ⇒ `DispatchRejected` (fail closed). Transport timeout after request sent ⇒ `DispatchUnknown`. |
| `Observe` | `GET /session/{id}/event` (SSE) wrapped in `adapter.BufferedStream`: progress events from message updates; terminal on assistant message completion or `session.idle`; permission-request events surface as denial-or-wait per §4. Server-side abort/connection loss closes the stream without fabricating a terminal event. |
| `Cancel` | `POST /session/{id}/abort`; verifies via status/events; maps to `CancelConfirmed` (turn ends aborted), `CancelAlreadyTerminal`, or `CancelUnsupported/Unknown` per response. |
| `Collect` | Polls `GET /session/{id}/message` for the terminal assistant message: text output, usage (tokens/cost) when present, `CompletedAt` from the message. `ResultUnavailable` while pending. |
| `Reconcile` | Evidence-based only: `GET /session/{id}` 200 + status idle/busty ⇒ `ReachableActive`; server connection failure ⇒ `Uncertain`/host_lost; 404 ⇒ `DefinitivelyMissing`. The in-memory session map is NOT treated as proof (AC-005 lesson). |

### 3.3 Permission (approval/denial) semantics

- OpenCode surfaces a permission request as an event + a pending entry on the
  permission endpoints while the turn stalls.
- The adapter maintains a permission responder: on request, it consults the
  Council-approved tooling allowlist (HarnessProfileSpec.Tooling/ExtraEnvAllowlist
  and policy from AC-005): allow ⇒ `reply approve`; otherwise ⇒ `reply deny`
  and the turn observes a tool-denial event which flows to Collect as part of
  the transcript (honest denial, not hidden).
- The adapter never enables `--auto`, never edits global permission config,
  never widens the allowlist.

### 3.4 Model/provider validation

- HarnessProfileSpec.Model is `provider/model`. At Probe, the adapter parses
  `GET /config/providers` **redacted** (provider id, model ids, auth
  present-flag) and requires: provider present, model id present, auth
  configured. Missing ⇒ Probe reports the capability gap; CreateSession/Dispatch
  fail closed with `DispatchRejected`.
- Intended provider/model validation satisfies acceptance criterion 1
  ("validate intended provider/model") without any provider call.

## 4. Evidence plan (fixtures vs integration)

- **Fixture tier (CI, no provider calls):** a fake OpenCode server
  (`httptest`-based, in `internal/adapter/opencode/opencodefake_test.go`)
  implementing the verified endpoint subset with scripted behaviors: fresh
  session, exact-resume mismatch, async prompt → events → terminal message,
  abort, permission request → deny recorded, usage in message, server death
  → uncertain reconcile. Full adapter contract tests run against the fake.
- **Integration tier (manual, sanitized):** against the installed 1.18.31:
  real session create/resume/abort with a trivial prompt, redacted
  transcripts, cost shown from the local install. Explicitly labeled as real
  installation evidence; never required in ordinary CI; recorded in PR
  evidence notes with the exact commands and redacted outputs.

## 5. Boundaries

- The adapter speaks to the installed OpenCode; OpenCode's own provider auth
  (its keychain/auth.json) is the credential store — the adapter never reads,
  copies, or stores provider keys (and redacts `/config/providers`).
- Server-child lifetime is adapter-owned but lacks parent-death guarantees —
  Reconcile/health checks treat unknown as uncertain (AC-004/AC-005 lesson
  carried forward).
- Windows: OpenCode on Windows is untested; the adapter compiles
  cross-platform but conformance evidence is Linux (documented honestly).
- No quota-bypass, no `--auto`, no global config edits, no copied tokens.

## 6. Acceptance mapping (issue #7)

| Criterion | Evidence |
|---|---|
| Fresh session without controller history; validate provider/model | Fixture: POST /session assertion (new id, model preset, project = contributor workspace) + Probe model validation fail-closed test |
| Load approved dotfiles tools/skills, guardrails enabled | Agent/permission config passed at session create; permission-responder deny-by-default test; `--auto` absence asserted in invocation builder test |
| Resume exact native session | ResumeSession 200-path + 404-path tests; no newest-session selection (verified: `--continue` never used) |
| Async result, approval wait, tool denial, usage, abort semantics | Fake-server scripted tests per semantic + event stream mapping |
| Close/reopen/recovery with real installation, sanitized, no quota-bypass | Manual integration script + sanitized transcript in PR evidence; CI uses fixtures only |
