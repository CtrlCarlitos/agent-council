# AC-007 OpenCode Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native inline TDD with 1 implementation owner and 2 internal verification gates). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the AC-006 adapter contract against the installed OpenCode headless HTTP server (1.18.31), with one adapter-owned `opencode serve` child per contributor session, verified exact-session resume, adapter-owned event pumping, deny-by-default permissions, deterministic message-ID dispatch with reservation/idempotency, and evidence-based reconciliation.

**Architecture:** `internal/adapter/opencode` provides `OpenCodeServer` (PolicyExecutor-launched per-session child), `OpenCodeAdapter` (AC-006 contract over a minimal typed HTTP client), an adapter-owned per-session SSE event pump, a `DispatchIdentitySource` seam, a `GeneratedServerEnv` mechanism in the executor, and a probe launch template. The fake server (`opencodefake_test.go`) implements the verified endpoint subset for CI.

**Tech Stack:** Go 1.25.0, `net/http`, SSE via `bufio`/`net/http` (no new deps), AC-005 `PolicyExecutor`/`workspace`/`execpolicy` packages, AC-006 `adapter` contract.

**Spec:** [`../specs/2026-09-21-ac-007-opencode-adapter-design.md`](../specs/2026-09-21-ac-007-opencode-adapter-design.md)

## Global Constraints

- No provider calls in CI (fake server only); native provider execution deferred to AC-007 integration evidence (manual, sanitized) and AC-007–AC-010.
- Adapter never calls `GET /config/providers` (raw provider keys); model validation uses credential-free `GET /api/model`.
- Adapter never enables `--auto`, never edits global permission config, never widens any allowlist. Permission requests: deny every request, mirror tool-requested/tool-denied events.
- `opencode serve` launch goes through `PolicyExecutor.Start` — never `os/exec` directly. Probe has no run profile: its executor request uses an **injected, operator-owned probe launch template** (opencode `--version` + the exact serve shape); the adapter never synthesizes a permissive profile internally.
- Digest: `msg_council_` + hex(SHA-256(length-prefixed sessionID, turnKey, attempt))[:32]. NUL bytes rejected. Collision-resistant (128-bit truncated).
- `DispatchRejected` requires never-written transport evidence; post-transmission failures are `DispatchUnknown`.
- Reconcile claims only verifiable state: live ⇒ ReachableActive; verified terminal (parentID == user message ID) ⇒ ReachableTerminal; transport loss / 404-after-possible-acceptance / ambiguous idle / unrecorded turn ⇒ Uncertain/host_lost. DefinitivelyMissing only with positive never-accepted evidence.
- Same-ID resubmission only after GET-by-ID returns 404. New attempts get new attempt-scoped IDs.
- Single flight per native session (in-flight map keyed by native session ID).
- Server credentials via `LaunchRequest.GeneratedServerEnv` only (Username/Password; injected after allowlist/scrub; redacted from captured output; accepted only when the executor validates the exact `opencode serve` launch shape).
- Event pump: adapter-owned per-session SSE; Observe detaches from bounded taps; routing by native session/message ID → TurnRef; cursor-based dedup/resync.
- Only tests requiring POSIX processes, signals, permissions, or shell fixtures are //go:build !windows; digest, HTTP client, fake-server, dispatch reservation, parentID correlation, reconciliation, and event-pump tests run on every platform. windows/darwin vet+compile must stay green.

## Verification Gates
- **Gate 1 (after Task 4):** executor GeneratedServerEnv + probe template validation (Tasks 1–2); server lifecycle via PolicyExecutor including park/resume replacement (Task 2); typed HTTP client + deterministic digest + identity seam (Task 3); fake OpenCode server (Task 4). Covers executor, lifecycle, client, digest, and fake-server evidence. Full suites + race ×3.
- **Gate 2 (after Tasks 6–7):** adapter contract + event pump (Task 5), service wiring (Task 6), acceptance story + matrix (Task 7). Full suites + race ×3 + cross-platform CI.

---

### Task 1: Executor `GeneratedServerEnv` + Probe Launch Template

**Files:**
- Modify: `internal/adapter/execpolicy/executor.go` (add `GeneratedServerEnv *GeneratedServerEnv` to `LaunchRequest`; add `ProbeLaunchTemplate` type)
- Create: `internal/adapter/execpolicy/generated_env.go`
- Test: `internal/adapter/execpolicy/generated_env_test.go`

**Interfaces:**
- `type GeneratedServerEnv struct { Username, Password string }` — expanded verbatim to `OPENCODE_SERVER_USERNAME=<Username>` / `OPENCODE_SERVER_PASSWORD=<Password>` after allowlist/scrub; rejected if either key collides with an inherited/allowlisted key; rejected on non-`opencode serve` launches.
- `type ProbeLaunchTemplate interface { VersionLaunch(ctx) (LaunchRequest, error); ProbeServeLaunch(ctx, scratchDir string) (LaunchRequest, error) }` — operator-owned builder producing **fully validated `LaunchRequest` values** (command, args, Paths, Profile, RunID, SessionID) for the version check and the probe-server launch. The adapter only appends `GeneratedServerEnv` to the probe-server request; it never synthesizes Paths, Profile, or identifiers internally.

**Steps:**

- [ ] **1.1 Failing tests:** GeneratedServerEnv expands to exactly the two keys; rejected when the launch command is not `opencode serve` shape; rejected when either key collides with an inherited/allowlisted key; accepted and injected after allowlist/scrub for a valid `opencode serve` launch.
- [ ] **1.2 Implement** `GeneratedServerEnv` handling in `executor.Start` env construction (step 7: inject after allowlist construction, before proxy env).
- [ ] **1.3 Define** `ProbeLaunchTemplate` type (no behavior yet; consumed in Task 2).
- [ ] **1.4 Suite + race; commit** `feat(execpolicy): GeneratedServerEnv and probe launch template`.

### Task 2: OpenCodeServer Lifecycle

**Files:**
- Create: `internal/adapter/opencode/server.go`
- Create: `internal/adapter/opencode/probe.go`
- Test: `internal/adapter/opencode/server_test.go`

**Interfaces:**
- `type SessionLaunchSource interface { OpenCodeServeLaunch(ctx context.Context, sessionID adapter.SessionID) (execpolicy.LaunchRequest, error) }` — injected seam; the service implementation builds the complete session-specific LaunchRequest from the persisted frozen run profile and the AC-005 workspace allocation. The adapter verifies the exact `opencode serve` shape and appends `GeneratedServerEnv` but never constructs policy inputs.
- `type ServerConfig struct { WorkspaceRoot string; Executor execpolicy.PolicyExecutor; SessionLaunch SessionLaunchSource; ProbeTemplate ProbeLaunchTemplate; Identity DispatchIdentitySource; IdleGrace time.Duration }` — no StateDir: the adapter must not gain access to Council state storage for process lifecycle management.
- `type OpenCodeServer struct { … }` — one per contributor session.
  - `Start(ctx) (endpoint string, err error)` — PolicyExecutor.Start with the verified launch shape (command `opencode`, args `serve --hostname 127.0.0.1 --port 0`, cwd = workspace root, GeneratedServerEnv); parse endpoint from startup output; wait for `/api/health`.
  - `Endpoint() string`, `PID() int`, `Close(ctx) error` (graceful dispose → terminate split, pipes drained, exit evidence).
- `Probe(ctx, tpl execpolicy.ProbeLaunchTemplate) (ProbeResult, error)` — scratch dir; start probe serve; call `/api/health` + `/api/model`; terminate; return {ServerVersion, Models, Capabilities}.
- Park lifecycle: adapter stops the server after IdleGrace when parked.

**Steps:**

- [ ] **2.1 Failing lifecycle tests** (fake executor): start records PID/endpoint; health-gate failure ⇒ error + child termination with evidence; Close terminates gracefully; park + idle grace stops the server; probe uses scratch dir, calls only health+model, terminates before return, never registered as contributor server.
- [ ] **2.2 Implement** server + probe.
- [ ] **2.3 Suite + race; commit** `feat(adapter/opencode): per-session server lifecycle and probe`.

### Task 3: Typed HTTP Client + Deterministic Digest + DispatchIdentitySource

**Files:**
- Create: `internal/adapter/opencode/httpclient.go`
- Create: `internal/adapter/opencode/digest.go`
- Test: `internal/adapter/opencode/digest_test.go`, `internal/adapter/opencode/httpclient_test.go`

**Interfaces:**
- `type DispatchIdentitySource interface { AttemptFor(ctx context.Context, ref adapter.TurnRef) (attempt string, ok bool) }`.
- `NativeMessageID(sessionID, turnKey, attempt string) (string, error)` — NUL rejection, length-prefixed canonical encoding, SHA-256[:32] hex, `msg_council_` prefix.
- `type client struct { endpoint, username, password string; hc *http.Client }` — typed methods: `Health`, `CreateSession`, `GetSession`, `ListMessages`, `GetMessage`, `PromptAsync`, `Abort`, `SessionEvents` (SSE reader), `GetModels`, `PermissionReply`.
- Transport error classification: never-written ⇒ `errNeverWritten` sentinel wrapping.

**Steps:**

- [ ] **3.1 Failing digest tests:** deterministic; distinct tuples → distinct digests (length-prefixed encoding prevents boundary ambiguity); NUL rejected; `msg_council_` prefix; 32-hex length.
- [ ] **3.2 Implement** digest + identity seam type.
- [ ] **3.3 Failing client tests** against `httptest`: typed methods hit correct paths with correct bodies; transport-error classification (refused ⇒ never-written; post-write timeout ⇒ unknown).
- [ ] **3.4 Implement** client.
- [ ] **3.5 Suite + race; commit** `feat(adapter/opencode): typed client, digest, identity seam`.

### Task 4: Fake OpenCode Server (CI evidence)

**Files:**
- Create: `internal/adapter/opencode/opencodefake_test.go`

Cross-platform: runs on every OS (pure `httptest` + net/http).

**Steps:**

- [ ] **4.1 Implement** the fake server: `POST /session` (rejects mismatched directory context), `GET /session/{id}` (200/404), `POST /session/{id}/prompt_async` (accepts messageID, creates user message, scripts async events + assistant message with `parentID == messageID`), `GET /session/{id}/message` + `GET /session/{id}/message/{messageID}`, `POST /session/{id}/abort`, `GET /session/{id}/event` (SSE with scripted event sequence and delay control), `GET /api/health`, `GET /api/model`, permission request/reply endpoints.
- [ ] **4.2 Gate 1:** full suites + race ×3 on `internal/adapter/opencode` (Tasks 1–3 + fake server). Commit.

### Task 5: AC-006 Adapter Contract (dispatch/observe/collect/cancel/reconcile) + Event Pump

**Files:**
- Create: `internal/adapter/opencode/adapter.go`
- Create: `internal/adapter/opencode/eventpump.go`
- Test: `internal/adapter/opencode/adapter_test.go`
- Test: `internal/adapter/opencode/eventpump_test.go`

Cross-platform: runs on every OS (HTTP + fake server; no POSIX process/permission dependency).

**Interfaces:**
- `OpenCodeAdapter` implements `adapter.Adapter` over `OpenCodeServer` + `client` + `DispatchIdentitySource` + `EventPump`.
- Dispatch: resolve attempt via identity seam; reserve (per-native-session single-flight); record baseline cursor; PromptAsync with deterministic messageID; classify per evidence boundary; publish on success; release reservation on failure.
- EventPump: adapter-owned per-session SSE drain; per-TurnRef bounded buffers; cursor-based dedup/resync; caller-detach never closes the native stream.
- Collect: poll assistant message with parentID == deterministic user-message ID.
- Reconcile: evidence rules per spec §3.5 with parentID correlation.

**Steps:**

- [ ] **5.1 Failing contract tests** against the fake server (cross-platform): create (fresh session, model preset); resume (200 / 404 ⇒ ErrNativeSessionMissing); dispatch accepted (deterministic messageID); dispatch rejected (never-written); dispatch unknown (post-write timeout); concurrent duplicate dispatch (single launch); retry rules (GET-by-ID first; never resubmit after unknown); observe via event pump tap (caller detach doesn't close native stream); collect (parentID correlation, usage, pending ⇒ unavailable); cancel (abort → confirmed); reconcile three-state (live/finished/unrecorded ⇒ uncertain).
- [ ] **5.2 Implement** adapter + event pump.
- [ ] **5.3 Race + full suite; commit** `feat(adapter/opencode): AC-006 contract implementation`.

### Task 6: Service Wiring + Restricted Bridge Extension

**Files:**
- Modify: `internal/service` (adapter construction: wire OpenCodeAdapter with DispatchIdentitySource backed by storage dispatch_intents, ProbeLaunchTemplate from operator config)
- Test: `internal/service/review_ac007_wiring_test.go`

**Steps:**

- [ ] **6.1 Failing wiring test:** the service constructs an OpenCodeAdapter with a DispatchIdentitySource backed by the real store; the probe template is operator-owned (not synthesized).
- [ ] **6.2 Implement** wiring.
- [ ] **6.3 Commit** `feat(service): wire OpenCode adapter construction`.

### Task 7: Acceptance + Integration Script + Gate 2

**Files:**
- Create: `internal/adapter/opencode/acceptance_test.go`
- Create: `scripts/ac007-integration-evidence.sh` (manual, sanitized; operator-invoked; explicitly excluded from CI)
- Modify: PR body

**Steps:**

- [ ] **7.1 Primary acceptance story** through the bridge + fake server: adopt (secret once) → bridge connect → bridge queue → bridge release → gated execution → collect (parentID correlation) → bridge disconnect → park → resume replacement server → replacement connect → bridge collect → bridge release follow-up. Durable-outcome assertions throughout.
- [ ] **7.2 Matrix verification** against spec §10: every row maps to a committed test.
- [ ] **7.3 Integration evidence script** (manual; sanitized; **operator-invoked and optional** — the script does not automatically select or invoke any model, including free-tier models; the operator explicitly chooses and runs the provider/model step). Explicitly excluded from CI.
- [ ] **7.4 Full verification:** gofmt/vet/CGO_ENABLED=0/race ×3; windows/darwin vet+compile; POSIX-process tests scoped //go:build !windows (signal/shell/permission fixtures only — digest, HTTP client, fake server, dispatch reservation, parentID correlation, reconciliation, and event-pump tests run on every platform).
- [ ] **7.5 Gate 2:** push, CI green, PR body evidence map, request review.

## Self-Review: Invariant → Interface → Assertion

| Invariant | Interface | Assertion |
|---|---|---|
| Server launch through PolicyExecutor; never os/exec | `OpenCodeServer.Start` uses `executor.Start` with a fully validated `LaunchRequest` from the probe template | server_test: fake executor records the launch request shape; Task 1 generated_env_test validates the template produces complete LaunchRequests |
| GeneratedServerEnv: narrow, validated, shape-checked, redacted | executor env construction + launch-shape validation (`opencode serve` shape required) | Task 1: expansion to exactly two keys, collision rejection, non-serve launch rejection |
| Probe: scratch server via template, no session/workspace needed, provider-free | `Probe(ctx)` + operator-owned `ProbeLaunchTemplate` builder | server_test: probe uses scratch dir from the template, calls only health+model, terminates before return |
| Digest: deterministic, unambiguous length-prefixed encoding, NUL-rejected, collision-resistant | `NativeMessageID` | digest_test: distinct tuples distinct, NUL rejected, prefix+length |
| Single launch under concurrency; failed launch releases reservation | per-native-session launching map + ready channel | adapter_test (Task 5, cross-platform): concurrent callers → one invocation; failure releases reservation |
| Reconcile: verifiable state only, uncertainty for unrecorded turns | evidence rules per spec §3.5 | adapter_test (cross-platform): live → reachable-active; finished → uncertain; unrecorded → uncertain |
| Collect/Reconcile correlate via parentID | assistant message selected by parentID == deterministic user-message ID | adapter_test (cross-platform): parentID match required; latest-message-only rejected |
| Permission deny-all | permission responder in event pump | adapter_test: request → deny reply + tool-denied event |
| No secrets in views/logs | redaction in record/response/captured output | admin_test pattern: no known-secret substring |
| Dispatch identity via trusted seam (no storage reads, no invented attempt) | `DispatchIdentitySource` wired by the service | wiring_test (Task 6): service constructs adapter with real-store-backed seam |
| POSIX-only tests scoped; core tests cross-platform | //go:build !windows on signal/shell/permission fixture files only | CI: digest/client/fake-server/dispatch/parentID/reconcile/event-pump tests run on all three platforms |
