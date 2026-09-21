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
- All enforcement evidence POSIX-scoped (//go:build !windows); windows/darwin vet+compile must stay green.

## Verification Gates
- **Gate 1 (after Tasks 1–3):** server lifecycle via PolicyExecutor (launch shape validation, GeneratedServerEnv, scratch-dir probe lifecycle, park/resume replacement); typed HTTP client against the fake server; deterministic digest; dispatch reservation. Full suites + race ×3.
- **Gate 2 (after Tasks 4–6):** full adapter contract against the fake server (create/resume/dispatch/observe/collect/cancel/reconcile per semantics); permission deny-all with event mirroring; event pump tap/detach; acceptance story through the bridge; integration script (manual, sanitized) documented. Full suites + race ×3 + cross-platform CI.

---

### Task 1: Executor `GeneratedServerEnv` + Probe Launch Template

**Files:**
- Modify: `internal/adapter/execpolicy/executor.go` (add `GeneratedServerEnv *GeneratedServerEnv` to `LaunchRequest`; add `ProbeLaunchTemplate` type)
- Create: `internal/adapter/execpolicy/generated_env.go`
- Test: `internal/adapter/execpolicy/generated_env_test.go`

**Interfaces:**
- `type GeneratedServerEnv struct { Username, Password string }` — expanded verbatim to `OPENCODE_SERVER_USERNAME=<Username>` / `OPENCODE_SERVER_PASSWORD=<Password>` after allowlist/scrub; rejected if either key collides with an inherited/allowlisted key; rejected on non-`opencode serve` launches.
- `type ProbeLaunchTemplate struct { VersionArgs []string; ServeArgs []string; WorkingDir string }` — operator-owned template consumed by the OpenCode adapter's Probe (Task 2); the adapter never synthesizes a permissive profile.

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
- `type ServerConfig struct { WorkspaceRoot, StateDir string; Executor execpolicy.PolicyExecutor; ProbeTemplate execpolicy.ProbeLaunchTemplate; Identity DispatchIdentitySource; IdleGrace time.Duration }`
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

### Task 4: AC-006 Adapter Contract (dispatch/observe/collect/cancel/reconcile)

**Files:**
- Create: `internal/adapter/opencode/adapter.go`
- Test: `internal/adapter/opencode/adapter_test.go`

**Interfaces:**
- `OpenCodeAdapter` implements `adapter.Adapter` over `OpenCodeServer` + `client` + `DispatchIdentitySource` + `EventPump`.
- Dispatch: resolve attempt via identity seam (missing ⇒ DispatchRejected); reserve (per-native-session single-flight); baseline cursor; PromptAsync with deterministic messageID; classify per evidence boundary; publish on success; release reservation on failure.
- Observe: tap the event pump's per-turn buffer.
- Collect: poll assistant message with parentID == deterministic user-message ID; extract text/usage/completed-at; ResultUnavailable while pending.
- Cancel: Abort endpoint; verify; map to Confirmed/AlreadyTerminal/Unknown.
- Reconcile: evidence rules per spec §3.5 with parentID correlation.
- Probe: scratch-server capability check per §3.2.1 (uses injected ProbeLaunchTemplate).

**Steps:**

- [ ] **4.1 Failing contract tests** against the fake server: create (fresh session, model preset, project = workspace); resume (exact 200 / 404 ⇒ ErrNativeSessionMissing); dispatch accepted (messageID deterministic); dispatch rejected (never-written); dispatch unknown (post-write timeout); concurrent duplicate dispatch (single launch); retry rules (GET-by-ID first; never resubmit after unknown); observe via event pump tap (caller detach doesn't close native stream); collect (parentID correlation, usage extraction, pending ⇒ unavailable); cancel (abort → confirmed); reconcile three-state (live/finished/unrecorded).
- [ ] **4.2 Implement** adapter + event pump.
- [ ] **4.3 Race + full suite; commit** `feat(adapter/opencode): AC-006 contract implementation`.

### Task 5: Fake Server (CI evidence)

**Files:**
- Create: `internal/adapter/opencode/opencodefake_test.go`

**Steps:**

- [ ] **5.1 Implement** `httptest` fake server: `POST /session` (rejects mismatched directory context), `GET /session/{id}` (404 for unknown), `POST /session/{id}/prompt_async` (accepts messageID, creates user message, scripts async events + assistant message with parentID), `GET /session/{id}/message` + `/{messageID}`, `POST /session/{id}/abort`, `GET /session/{id}/event` (SSE), `GET /api/health`, `GET /api/model`, permission request/reply endpoints. Scripted delays for event-pump and timeout tests.
- [ ] **5.2 Gate 1:** full suites + race ×3 on `internal/adapter/opencode` + `internal/adapter/execpolicy`. Commit (if not already committed with Task 4).

### Task 6: Service Wiring + Restricted Bridge Extension

**Files:**
- Modify: `internal/service` (adapter registry / construction site: wire OpenCodeAdapter with DispatchIdentitySource backed by storage dispatch_intents, ProbeLaunchTemplate from operator config)
- Test: `internal/service/review_ac007_wiring_test.go`

**Steps:**

- [ ] **6.1 Failing wiring test:** the service constructs an OpenCodeAdapter with a DispatchIdentitySource backed by the real store, and the probe template is operator-owned (not synthesized).
- [ ] **6.2 Implement** wiring.
- [ ] **6.3 Commit** `feat(service): wire OpenCode adapter construction`.

### Task 7: Acceptance + Integration Script + Gate 2

**Files:**
- Create: `internal/adapter/opencode/acceptance_test.go`
- Create: `scripts/ac007-integration-evidence.sh` (manual, sanitized; documented as not required in CI)
- Modify: PR body

**Steps:**

- [ ] **7.1 Primary acceptance story** through the bridge + fake server: adopt (secret once) → bridge connect → queue → bridge release → gated execution → collect → disconnect → park → resume replacement server → B connect → B collect. Four harness rotations where applicable.
- [ ] **7.2 Matrix verification** against spec §10: every row maps to a committed test.
- [ ] **7.3 Integration evidence script** (manual; sanitized; labeled as real-installation): start a real `opencode serve`, adopt a controller via HTTP, create/resume a session, submit a trivial prompt with a free model, capture sanitized transcripts, abort, reconcile. Explicitly excluded from CI.
- [ ] **7.4 Full verification:** gofmt/vet/CGO_ENABLED=0/race ×3/cross-platform compile.
- [ ] **7.5 Gate 2:** push, CI green, PR body evidence map, request review.

## Self-Review: Invariant → Interface → Assertion

| Invariant | Interface | Assertion |
|---|---|---|
| Server launch through PolicyExecutor; never os/exec | `OpenCodeServer.Start` uses `executor.Start` | server_test: fake executor records the launch request shape |
| GeneratedServerEnv: narrow, validated, redacted | executor env construction + launch-shape validation | generated_env_test: expansion, collision rejection, non-serve rejection |
| Probe: scratch server, no session/workspace needed, provider-free | `Probe(ctx, tpl)` on scratch dir; template from operator config | server_test: probe uses scratch, calls only health+model, terminates |
| Digest: deterministic, unambiguous, NUL-rejected, collision-resistant | `NativeMessageID` | digest_test: distinct tuples distinct, NUL rejected, prefix+length |
| Single launch under concurrency | `launchReservation` per-turn ready channel | review_gate2_test pattern applied to OpenCodeAdapter |
| Reconcile: verifiable state only | evidence rules per spec §3.5 with parentID correlation | adapter_test: live/finished/unrecorded three-state |
| Permission deny-all | permission responder in event pump | adapter_test: request → deny reply + tool-denied event |
| No secrets in views/logs | redaction in record/response/captured output | admin_test pattern: no known-secret substring |
| ParentID correlation in Collect and Reconcile | `resolveAssistantByParentID` | adapter_test: parentID match required; latest-message-only rejected |
