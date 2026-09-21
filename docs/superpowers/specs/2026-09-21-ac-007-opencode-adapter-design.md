# AC-007: OpenCode/GLM Persistent Contributor Adapter — Design

- **Status:** Proposed, revised after Gate-spec review (research against installed OpenCode 1.18.31)
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
| `GET /permission`, `POST /permission/{requestID}/reply`, `GET /api/session/{id}/permission`, `POST /api/session/{id}/permission/{requestID}/reply` | **Approval wait / tool denial**: permission requests surface on the event stream and permission endpoints; adapter denies every request (see §3.6) |
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
- **`GET /config/providers` returns raw provider API keys — Council must
  never request it.** Receiving the response puts provider credentials into
  Council memory even if redacted afterward, violating the native-auth
  invariant. Model listing/validation uses the credential-free
  `GET /api/model` endpoint. Native authentication status is therefore
  **unknown** to the adapter; if the native side is unauthenticated,
  session creation or dispatch fails honestly on OpenCode's own auth error.
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

### 3.1 Server lifecycle — one server per contributor session

`opencode serve` has exactly one process working directory, and the
contributor session's project scoping depends on it. Council cannot share
one server across workspaces. Therefore: **one adapter-owned
`opencode serve` child per contributor session** (`OpenCodeServer`,
`internal/adapter/opencode/server.go`).

**Launch is inside the AC-005 execution policy — never `os/exec` directly.**
`OpenCodeServer` launches through the session's `PolicyExecutor`
(`executor.Start`), with a `LaunchRequest` that carries:

- `Command: "opencode"`, `Args: ["serve", "--hostname", "127.0.0.1",
  "--port", "0"]` — authorized by the frozen profile's `opencode`
  tooling entry (the executor's command allowlist gates this like any
  managed process; the permissive_dev/test profiles used in fixtures
  scope accordingly).
- Working directory: that session's isolated AC-005 workspace root
  (project scoping is inherited from the process cwd — no per-request
  directory selector is trusted).
- **Server credentials via a new `LaunchRequest.SetEnv map[string]string`**
  (narrow AC-005 executor extension): explicit env vars set directly into
  the child environment (`OPENCODE_SERVER_USERNAME`/`OPENCODE_SERVER_PASSWORD`,
  generated ephemeral pair). `SetEnv` vars are Council-generated transport
  credentials, NOT inherited environment — the frozen inherited-env
  allowlist does not apply to them and they never appear in the frozen
  profile. The executor injects them verbatim after allowlist
  construction, and they are redacted from all captured output. Nothing
  else may use `SetEnv` in this task.
- Loopback listen plus the run's outbound network policy: the server must
  listen on `127.0.0.1`; provider egress follows the run's network mode.
- **Honest strict-isolation behavior:** under a network mode that cannot
  permit both a loopback listener and provider egress, the server still
  launches (loopback is local), but provider calls fail natively —
  dispatches surface honest native failures. If the platform/network
  enforcement cannot guarantee loopback listening at all, the launch is
  rejected fail-closed with an explicit unsupported-capability error.
- stdout/stderr drained and captured (termination-evidence pattern from
  AC-005); captured output is scanned for redaction (the server never
  receives provider keys from Council).
- Waits for `GET /api/health` → `healthy:true` within a startup deadline;
  failure ⇒ error (fail closed) and child termination with recorded
  evidence.
- `Close()`: graceful `POST /global/dispose` if available, then
  `terminateGracefully` → force-kill (AC-005 terminate split), drain pipes
  before Wait, record exit evidence.
- Records child PID + exit status for Reconcile honesty (no fabricated
  liveness).

**Parked-session lifecycle (bounded accumulation):**

- The server runs while its contributor session is attached and working.
- When the contributor session **parks** (turn terminal, no queued
  follow-up released to it, no in-flight dispatch), the adapter stops the
  server after a short idle grace (default 30s, config), freeing the
  process and port.
- On **service shutdown**, all live servers terminate with the standard
  teardown ordering (AC-005 terminate split; pipes drained before Wait).
- **ResumeSession after parking** starts a replacement server with the
  SAME workspace root as its working directory. OpenCode persists sessions
  per project directory, so the replacement server resolves the exact
  native session ID; `ResumeSession` verifies `GET /session/{nativeID}`
  (200 + project match) before reporting success — a 404 raises
  `ErrNativeSessionMissing` (binding gone, never silently re-created).
- Cost: one small headless process per active contributor session
  (bounded by concurrent sessions; terminated at park + grace). Benefit:
  hard workspace/project isolation without trusting an experimental
  per-request directory selector, and a single tenant per server.

### 3.2 Adapter (`internal/adapter/opencode/adapter.go`)

Implements `adapter.Adapter` (AC-006) over the server's HTTP API via a
minimal typed client (`httpclient.go`):

| AC-006 method | Behavior |
|---|---|
| `Probe` | `GET /api/health` + credential-free `GET /api/model` (model ids only; native auth status is **unknown** to Council). Server version. Streaming supported via SSE. |
| `CreateSession` | Spawn the session's `opencode serve` child (§3.1), then `POST /session` with `{title: "council <sessionID>", agent, model: HarnessProfileSpec.Model}`. Model presence is validated against `GET /api/model` (credential-free; fail closed on unknown model). Native auth status is unknown — an unauthenticated native side fails at first prompt, honestly. Records `NativeSessionID = ses_…`. Fresh session in the session's own project: no history copy. |
| `ResumeSession` | `GET /session/{nativeSessionID}`: 200 with matching project ⇒ verified; 404 ⇒ typed **`ErrNativeSessionMissing`** (a missing persisted binding — distinct from uncertain creation, which remains `ErrSessionCreationUncertain`'s role). Never falls back to newest-session selection. |
| `Dispatch` | See §3.3 turn-identity contract: baseline cursor, adapter-generated deterministic native message ID, single-flight per native session, explicit pre-acceptance vs unknown classification, and turn/session single-flight enforcement. |
| `Observe` | Taps the session's adapter-owned event pump (§3.4): buffered, cursor-tracked, deduplicated; caller detach never closes the native stream. |
| `Cancel` | `POST /session/{id}/abort`; verifies via status/events; maps to `CancelConfirmed` (turn ends aborted), `CancelAlreadyTerminal`, or `CancelUnknown` per response. |
| `Collect` | Selects the assistant reply for THIS turn by the recorded native message ID and baseline cursor (§3.3) — never "latest session message": text output, usage (tokens/cost) when present, `CompletedAt`. `ResultUnavailable` while pending. |
| `Reconcile` | Evidence rules per §3.5: verified active ⇒ ReachableActive; verified terminal message ⇒ ReachableTerminal; transport loss / missing session after possible acceptance / ambiguous idle ⇒ Uncertain; DefinitivelyMissing only with positive never-accepted evidence. |

### 3.3 Turn identity and dispatch idempotency

- **Native message ID is adapter-generated, deterministic per attempt, and
  a fixed-length digest.** The AC-006 `Dispatch(ctx, ref, prompt)` receives
  only `TurnRef{SessionID, TurnKey}` — no attempt identity — so the adapter
  takes a **trusted identity seam** at construction:
  `DispatchIdentitySource.AttemptFor(ctx, ref) (attempt string, ok bool)`,
  wired by the service to the persisted dispatch intent (storage
  `dispatch_intents.attempt_id`); the adapter never reads storage directly
  and never invents `"1"`. The native message ID is then
  `msg_council_` + hex(SHA-256(sessionID ∥ turnKey ∥ attempt))[:32]:
  a fixed-length, fixed-charset digest that cannot exceed or violate the
  native `^msg` ID pattern, cannot collide across sessions/turns in
  practice, and leaks no identifiers. The attempt appears in the hash
  input, so distinct attempts get distinct native messages.
- **Single flight per native session.** OpenCode sessions are single
  conversation streams. The adapter keeps an in-flight map keyed by native
  session ID; a second Dispatch on the same native session is rejected
  (`session busy`) rather than interleaved. One TurnRef ↔ at most one
  in-flight native message.
- **Pre-acceptance failure vs DispatchUnknown.** `DispatchRejected`
  requires transport evidence that the request was **never written**:
  connection refused, dial/setup failure, or write failure before the
  request body transmission began. Any timeout or disconnect **after
  transmission begins** — regardless of whether response bytes arrived — is
  `DispatchUnknown`: the native side may have fully processed the prompt.
  "No response byte received" alone is not pre-acceptance evidence.
- **Retry without double submission.** Retry resolves in this order:
  (1) `GET /session/{id}/message/{messageID}` — if the deterministic message
  exists, the dispatch was accepted; do not resubmit; collect or observe it.
  (2) If absent and the previous attempt was `DispatchRejected` with
  never-written evidence, resubmit with the same message ID (same attempt).
  (3) If the previous attempt was `DispatchUnknown`, resubmission is
  forbidden at the same attempt: the turn goes to reconciliation
  (uncertain), and only a reconcile that positively resolves absence may
  clear it for a new attempt.

### 3.3.1 Verified messageID semantics (live-server experiment, provider-free)

Experiment against the installed 1.18.31 headless server, using an
invalid/unauthenticated provider+model so **no provider call and no quota
consumption** occurs; sanitized evidence retained in this section:

| Step | Request | Observed |
|---|---|---|
| Submit | `POST /session/{id}/prompt_async` with `"messageID":"msg_council_test_attempt_1"` and an invalid provider/model | `204 No Content` — accepted for async processing |
| Address | `GET /session/{id}/message/msg_council_test_attempt_1` | `200` — the supplied ID IS the native message ID; record carries `role:"user"`, the submitted model, and the text part |
| Repeat | Same request resubmitted | `204`; message list still contains **exactly one** message with that ID — no duplicate message, no second user message |
| Part caveat | Inspect parts after repeat | The repeat **appended** its text as a second part on the same user message (`['capability probe', 'capability probe REPEAT']`) — upsert-with-part-append, NOT a pure no-op |
| Async honesty | Session status / error fields after the invalid-model dispatch | Accepted ≠ executed: the invalid provider failed natively with no fabricated error message; failure surfaces asynchronously |

Design conclusions (binding for implementation):

1. `prompt_async.messageID` is accepted and becomes the addressable native
   message identity — the §3.3 deterministic-ID contract is implementable.
2. **Same-ID resubmission is only performed when `GET` by that ID returns
   404.** If the message exists, never resubmit — collect or reconcile
   instead (the append-parts behavior makes blind resubmission observable
   in the transcript).
3. A new attempt uses a new attempt-scoped message ID (digest input
   changes); a natively failed attempt cannot double-execute a later
   attempt.
4. `DispatchAccepted` (204) means natively queued, not executed: native
   failures surface asynchronously through events/collect, which the
   adapter records honestly.
- **Baseline cursor.** Before submission the adapter records the session's
  current last-message ID (from `GET /session/{id}/message`). Collect selects
  the assistant response with message ID == the adapter-generated
  submission ID (and falls back to "first assistant message after the
  baseline" if the native side assigns its own reply IDs), never "latest
  message in session".
- **One turn per native session at a time** is enforced by the in-flight
  map above; a queued follow-up waits for the previous turn's terminal
  state (completed/failed/aborted) before dispatch.

### 3.4 Adapter-owned event pump

- One continuously drained SSE connection per active session
  (`GET /session/{id}/event`), owned by the adapter — NOT by any Observe
  caller. A caller ending Observe detaches from a buffered tap; the native
  stream stays connected and events keep flowing into per-TurnRef bounded
  buffers (AC-005 buffered-stream semantics: slow-consumer overflow
  disconnects the tap, never the native stream).
- Routing: events carry native session ID and (for message events) message
  ID. The adapter's turn registry maps (native session ID, message ID) →
  TurnRef, so progress/terminal/tool events route to the right turn.
- Deduplication by event sequence/id; reconnect resumes from the recorded
  cursor where the server supports Last-Event-ID, otherwise the pump
  resyncs from message history (the deterministic message ID makes resync
  unambiguous).
- Permission-request events route to the permission responder (§3.6) and are
  mirrored into the turn's stream as tool-requested/tool-denied events.

### 3.5 Reconciliation evidence rules

Correlate the exact native message/turn — never the bare session:

| Evidence | Verdict |
|---|---|
| Verified active native work (session exists AND the turn's submission message is the live/unanswered one, or native status shows the turn executing) | `ReachableActive` |
| Verified terminal message for this turn's message ID (assistant reply present) | `ReachableTerminal` with the durable outcome |
| Transport loss to the server; session 404 **after** a possibly-accepted dispatch (404 proves nothing about an orphan worker's work); ambiguous idle (session idle but this turn's message has no terminal reply and no positive never-accepted evidence) | `Uncertain` / host visibility lost |
| Positive evidence the specific dispatch was never accepted (e.g., the deterministic message ID provably absent before any acceptance could occur, recorded pre-acceptance failure) | `DefinitivelyMissing` |

`ResumeSession` 404 raises typed **`ErrNativeSessionMissing`** — a missing
persisted binding — distinct from `ErrSessionCreationUncertain` (uncertain
creation). Session 404 during reconciliation is NOT the same as a missing
binding: it routes through the uncertain rule above unless the turn was
never dispatched (then DefinitivelyMissing with the pre-acceptance
evidence).


### 3.6 Permission (approval/denial) semantics

- OpenCode surfaces a permission request as an event plus a pending entry on
  the permission endpoints while the turn stalls.
- **The adapter denies every permission request.** CanonicalProfile.Tooling
  and ExtraEnvAllowlist describe executable/POSIX allowlists (AC-005); they
  are NOT an OpenCode tool-approval authority, and no approval is ever
  inferred from them. Each request is answered `deny` and mirrored into the
  turn's event stream as explicit tool-requested / tool-denied events, so
  the transcript records the denial honestly.
- If a future task needs selective approval, it must add a narrow injected
  permission-policy interface whose decision binds the exact run, session,
  turn, request ID, tool name, and arguments — never a profile-wide flag.
  Out of scope here (YAGNI).
- The adapter never enables `--auto`, never edits global permission config,
  and never widens any allowlist.

### 3.7 Model/provider validation (credential-free)

- HarnessProfileSpec.Model is `provider/model`. At Probe, the adapter parses
  the credential-free `GET /api/model` listing and requires the intended
  provider+model to be present; missing ⇒ Probe reports the capability gap
  and CreateSession/Dispatch fail closed with `DispatchRejected`.
- Native authentication status is **unknown** to Council (the only
  auth-inspecting endpoint exposes credentials). An unauthenticated native
  side fails at session create or first prompt with OpenCode's own error,
  reported honestly as dispatch/session failure — never converted into a
  fake success.

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
| Server launch inside AC-005 policy; SetEnv credentials; parked-session server stop; resume replacement server | Fake-executor launch request assertions (command/cwd/SetEnv/network honesty) + park/resume lifecycle tests |
| Close/reopen/recovery with real installation, sanitized, no quota-bypass | Manual integration script + sanitized transcript in PR evidence; CI uses fixtures only |
