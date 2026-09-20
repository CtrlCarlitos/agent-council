# AC-004: Controller Adoption and Exclusive Revocable Leases — Design

- **Status:** Approved with amendments (operator design review at `b325bb1`)
- **Date:** 2026-09-20
- **Issue:** [#4 (AC-004)](https://github.com/CtrlCarlitos/agent-council/issues/4)
- **Dependencies:** AC-001 (#1, closed), AC-002 (#2, closed), AC-003 (#3, closed at `b325bb1`)
- **Target Packages:** `internal/storage`, `internal/service`, `internal/council`

---

## 1. Problem and Scope

The service now outlives its clients (AC-003). AC-004 answers which existing
agent conversation is authorized to control a run, and how that authority
survives disconnection or changes hands safely.

In scope: controller identity adoption, exclusive revocable run-level
leases, authority-before-replay ordering, service-owned persistence of
already-accepted execution outcomes, run-scoped connection/attachment
semantics, and the operator/controller privilege boundary at the endpoint
level.

Out of scope (settled or separate roadmap items): service lifetime
architecture (AC-003, closed), MCP packaging and the typed tool bridge
(AC-011), native contributor adapters (AC-005–AC-010), budget enforcement
(AC-014), voting (AC-013), UI, SaaS, process-level sandboxing against a
hostile same user (AC-005), and any heartbeat/time-based lease expiry
protocol. These are exclusive, explicitly revocable grants — not a
distributed time-lease protocol.

**Central rule:** changing who may control the run must immediately stop
stale decisions, without erasing the authority or evidence of work that was
already accepted.

## 2. Authority Model

### 2.1 Credentials and roles

| Credential | Holder | Grants | Never grants |
|---|---|---|---|
| Operator bearer token (`auth.token`, AC-003) | The operator's local tooling (CLI/MCP bridge) | Transport authentication for all endpoints, plus explicit operator administration (adoption, handoff authorization, lost-lease recovery, credential recovery) | Nothing implicitly: operator administration still requires the operation's own authorization inputs (current lease, expected generation, or verified recovery intent). An operator token alone is not a controller lease and cannot dispatch or finalize run decisions. |
| Controller lease (per run, per generation) | The adopted controller conversation, delivered through the operator's bridge | Controller commands for that run only (queue/release/replace/discard/cancel-request/reconnect/decision), when its generation is active and the attachment is connected | Operator administration; authority over any other run; any authority once superseded or revoked |
| Contributor/adapter credential | None — adapters receive only `(TurnRef, prompt)` and supply observations | Nothing at the HTTP surface | Controller or operator authority, lease acquisition, allowance changes, recursive council creation |

A lease presented as a transport bearer credential does not authenticate:
transport auth remains the operator token (the trusted local bridge model).
Endpoint-level denial must be proven with the actual credentials a
contributor or controller client possesses (a lease, or nothing).

### 2.2 Authorization matrix

| Operation | Required authority |
|---|---|
| First adoption | Explicit operator authority; no active controller may be silently replaced |
| Normal handoff | Explicit operator authorization for the target, plus the expected current generation and current lease |
| Revoke own active lease | Current controller authority; cannot affect another generation |
| Lost-lease recovery/revocation | Verified operator authority plus explicit recovery intent (`operator_recovery`), a reason, an operation ID, and the expected target generation |
| Reconnect | Current lease and matching adopted controller identity; no replacement identity and no implicit handoff |
| Contributor request to adopt, hand off, or recover | Denied before mutation |

`operator_recovery: true` is an intent flag, not proof of authority: it is
only meaningful on an operator-authenticated transport, with `op_id`,
`reason`, and `expected_generation`, and it is journaled as an
operator-recovery transition.

Revoking a run lease does not revoke an independently held operator
credential — an old controller that still possesses the operator token
remains an operator under this authentication model. That is why controller
conversations never possess the operator token: the bridge (AC-011) holds
it, and the conversation holds only its lease.

## 3. Data Model and Migration v2

### 3.1 New table: `controller_leases`

```sql
CREATE TABLE controller_leases (
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    generation INTEGER NOT NULL CHECK (generation >= 0),
    harness TEXT,            -- NULL only for the legacy bootstrap row
    controller_ref TEXT NOT NULL,
    lease TEXT NOT NULL,     -- current secret only while status = 'active'
    status TEXT NOT NULL CHECK (status IN ('active','superseded','revoked')),
    attachment_id TEXT,      -- current connection episode, active rows only
    connected INTEGER NOT NULL DEFAULT 0 CHECK (connected IN (0,1)),
    attached_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, generation)
);
```

Rules:

- At most one `status = 'active'` row per run (enforced in every transition
  transaction; a unique partial index may be added where supported).
- The active row's `lease` and `runs.controller_lease` agree within the same
  transaction. The history table is never a competing source of truth:
  `runs.controller_lease` remains the current-secret authority.
- First adoption is generation 1; every later grant uses a strictly greater
  generation, **including adoption after revocation**. Disconnect never
  increments the generation; reconnect never creates a second controller.
- Empty credentials must never pass through an empty-string comparison: the
  shared authorizer rejects empty presented leases before any equality
  check, and revocation clears `runs.controller_lease` to `''`.

### 3.2 Run-level attachment state

`runs` gains `controller_adopted INTEGER NOT NULL DEFAULT 0` (1 once a
verified adoption exists). The active `controller_leases` row carries the
current `attachment_id` and `connected` flag — run-scoped by construction.
Existing session `controller_status` columns are retained as compatibility
projections: they are updated transactionally (with version bumps) when the
run-level state changes, and are not independent grants of authority.
Handlers answer "is the current controller connected for this run?" from
the run-level record, never by guessing from one contributor session.

### 3.3 Legacy-run rule (migration v2)

Existing v1 runs were created with a caller-supplied lease and no adopted
identity. Migration v2:

1. Creates the `controller_leases` table and the `runs.controller_adopted`
   column in one versioned transaction (schema version 2).
2. For each existing run, inserts a **generation 0 bootstrap row**:
   `harness = NULL`, `controller_ref = 'legacy-v1'`, `status = 'active'`,
   `lease` = the run's existing `controller_lease`. This preserves queued,
   running, and cancelling work and all historical evidence without
   fabricating an adopted identity; `controller_adopted` stays 0.
3. The first explicit adoption supersedes the bootstrap row with
   generation 1 (requiring the current legacy lease or verified operator
   recovery). Generation 0/bootstrap is clearly separate from an adopted
   controller in every view and journal entry.

Tests and fixtures adopt controllers through the new contract; nothing
quietly grants new authority to old arbitrary lease strings. Work
preservation across migration is tested with queued, running, and
cancelling turns: no fabricated identity, no automatic execution.

## 4. Grant Lifecycle

All transitions are single-transaction storage operations following the
existing `checkOrRecordIdempotency`/journal conventions, with the
authority-before-replay ordering of §5.

- **Adopt** (`AdoptController`): requires operator intent (service-level
  transport) and that no active adopted controller exists; a generation-0
  bootstrap row may be superseded. Refuses when an active controller
  already exists, except a matching idempotent replay of the original
  adoption operation. Installs generation = max+1, records harness and
  controller reference, sets `runs.controller_lease` and
  `controller_adopted = 1`.
- **Handoff** (`HandoffController`): requires the current active lease and
  the expected current generation; supersedes the old row
  (`status = 'superseded'`), installs the new grant, updates attachment
  state, and journals the transition atomically. `expected_generation`
  prevents two simultaneous handoffs or a stale recovery from affecting the
  wrong controller.
- **Revoke** (`RevokeController`): with the current lease (controller
  revoking itself) or via verified operator recovery (`op_id`, `reason`,
  `expected_generation`, `operator_recovery` intent). Marks the row
  `revoked`, clears `runs.controller_lease` to `''`. No active grant
  remains.
- **Failure atomicity:** a failed transition leaves authority, connection
  state, and pending work unchanged (transaction rollback; no partial
  journal entries).

### 4.1 Secret issuance: generated once per committed operation

One secret is generated per committed adoption/handoff operation — not per
response. A response can be lost after commit; the client then holds
neither the new secret nor a valid old lease. The remedy is **recovery of
the issued credential, never re-issuance**:

- An explicitly operator-authorized `RecoverControllerCredential` operation
  returns the secret of a specified committed adoption/handoff operation
  while that exact generation remains active.
- A superseded controller cannot use its old lease to recover the
  replacement's secret. Once the issued generation is revoked or
  superseded, operation inspection returns its historical status only — no
  resurrected credential.
- Never issue a second generation merely because the first response was
  lost.

### 4.2 Secret redaction

Lease secrets must not appear in generic operation receipts, ordinary
history/status responses, readiness/status diagnostics, or error messages.
The journal stores `caller_lease` inside the protected local database
(that is accurate today and remains so); every outward-facing view that
exposes journal or controller history redacts lease values (hash-prefix or
elision) rather than reproducing raw payloads. Deferring database-at-rest
hashing is an accepted, explicit security tradeoff within the
protected-local-store boundary (candidate for the security-hardening
roadmap item); deferring credential-response and redaction rules is not.

## 5. Authorization and Replay Ordering

### 5.1 Entry-point inventory by responsibility

Completeness is defined by this inventory, not a count of lease
comparisons:

| Category | Entry points | Authorization rule |
|---|---|---|
| Controller commands | queue prompt, replace prompt, discard prompt, release turn (incl. `FindCommittedRelease` replay path), cancel request (incl. `:req`/`:term` composite stages), reconcile (incl. `:reconcile`/`:host_loss` composite stages and `FindOperationReceipt` replay), record decision, controller connect/disconnect | Current controller authority and generation first; then idempotent replay; then state validation |
| Operator administration | adopt, handoff, revoke, credential recovery, controller record read | Operator transport; operation-specific inputs per §2.2 |
| Internal execution observations | supervisor outcome persistence, dispatch observations, reconciliation outcomes for accepted executions | Persisted execution identity (§6); never a controller lease; never HTTP-exposed |
| Read-only inspection | turn read, status, readiness, controller record read, journal/history views | Authenticated read; no execution, no reconnection side effects, no secret disclosure |

### 5.2 Authoritative transaction ordering

For controller-originated commands, storage transitions must apply:

```
resolve the requested run and complete resource scope
  → validate current controller authority and generation
  → classify superseded / revoked / unknown credentials
  → look up and validate an existing operation (idempotency)
  → return its receipt, or validate current state and commit a new operation
```

Current authority precedes receipt replay. A matching receipt still
precedes ordinary `expected_version` checks, so a valid current controller
recovers a committed response after state advances. The shared
`checkOrRecordIdempotency` helper, the custom release-receipt path
(`FindCommittedRelease`), and the composite cancellation/reconciliation
stages all follow this ordering.

### 5.3 Replay decision table

| Request | Result |
|---|---|
| Current controller retries its matching committed operation | Original receipt; no additional execution or transition |
| Superseded/revoked controller retries an operation it previously committed | `ErrLeaseSuperseded` (HTTP 403 `lease_superseded`); no successful controller replay |
| Current controller reuses an operation ID with different content or scope | Idempotency conflict |
| New controller or operator inspects an earlier decision/result | An authorized read of historical evidence — not execution of the previous controller's command |

Classification: a presented lease equal to the empty string is rejected
before comparison; a lease matching a `superseded` or `revoked` history row
is `ErrLeaseSuperseded`; otherwise a non-matching lease is
`ErrUnauthorizedOperation`. These verdicts are durable across restarts
because history is persisted.

## 6. Accepted-Execution Outcome Authority

Authority to make a **new decision** is separate from authority to
**record evidence for an accepted execution**.

- A release records the issuing controller generation and the original
  execution identity (session, turn, attempt) in its durable records.
- The service retains a narrowly scoped responsibility — held by the
  execution supervisor, not by any controller — to observe and persist that
  execution's outcome, even if controller authority later changes. This
  responsibility cannot authorize another release, alter budgets, adopt a
  controller, or approve a result.
- Outcome persistence for an accepted execution is a service-internal
  storage operation authorized by the complete persisted
  session/turn/attempt identity plus the existing outcome-validation rules
  (nonterminal turn state, foreign-result quarantine, terminal-conflict
  handling). It performs no controller-lease comparison. It is not exposed
  as a generic HTTP operation; no caller can set an `internal` flag to use
  it. Adapters continue to supply observations, not controller authority.
- The solution is neither broad acceptance of obsolete controller
  credentials nor substituting the new controller's secret into an old
  supervisor. The audit trail reads: controller A authorized the work; the
  service recorded its verified outcome; controller B later controlled the
  run (journal entries carry the issuing generation from the release
  record).

Required evidence (in addition to completed-before-handoff): A authorizes
a gated turn → operator hands control to B while the turn is active → A's
new commands and replays are rejected → the original worker finishes → the
service records its verified outcome under the original execution → the
follow-up remains queued until B explicitly releases it. The same sequence
repeated with revocation (no controller at all) still allows the accepted
work to finish and park.

## 7. Connection and Attachment Semantics

- The run-level record stores the current controller's connection state and
  an attachment identifier distinguishing connection episodes without
  rotating the lease.
- Same controller, same generation: reconnecting creates attachment B; a
  delayed disconnect arriving from attachment A leaves B connected.
- Handoff invalidates the old attachment. The new controller must connect
  explicitly; it never inherits A's connected flags. Compatibility
  projections on sessions are updated transactionally and their versions
  advance when they change.
- **Restart rule:** a persisted `connected` flag never establishes that a
  live controller connection exists. The new service instance requires
  explicit reattachment before new decisions; already-authorized execution
  still completes per §6. This is an in-instance attachment gate, not a
  lease change — no generation bump, no journal entry implying the
  controller acted.
- No heartbeat or automatic time-based lease expiry exists in this
  implementation.

## 8. HTTP Surface

New run-scoped endpoints (all behind the operator bearer transport):

- `GET /v1/runs/{run_id}/controller` — current controller record:
  harness, controller reference, generation, status, adoption flag,
  attachment/connection summary. Redacted: no lease secret.
- `POST /v1/runs/{run_id}/controller/adopt` — `{op_id, harness,
  controller_ref}`; installs the first (or post-revocation next)
  generation; returns the secret once per §4.1 recovery rules.
- `POST /v1/runs/{run_id}/controller/handoff` — `{op_id, current_lease,
  expected_generation, harness, controller_ref}`.
- `POST /v1/runs/{run_id}/controller/revoke` — `{op_id, controller_lease}`
  (self-revoke) or `{op_id, operator_recovery: true, reason,
  expected_generation}` (verified operator recovery).
- `POST /v1/runs/{run_id}/controller/credential/recover` — `{op_id,
  source_op_id, expected_generation}`; operator-authorized; returns the
  still-active generation's secret.

Error codes: `lease_superseded` (403) for superseded/revoked leases,
`lease_required`/`unauthorized` (401/403) per existing conventions,
`generation_mismatch` (409) for stale `expected_generation`,
`adoption_required` (409) for run-scoped controller commands on a
not-yet-adopted run that has no legacy bootstrap validity left, and
`not_connected` (409) when the current controller must reattach. Denial
tests use the actual credentials available to contributor and controller
clients (a lease presented as transport bearer fails authentication;
contributor clients hold nothing).

## 9. Worker and Contributor Separation

Structural enforcement within AC-004's boundary:

- Lease-affecting operations accept no session/contributor parameters and
  require operator transport plus their own authorization inputs.
- Adapters receive `(TurnRef, prompt)` and observation results only; no
  lease, token, or controller material crosses the adapter interface.
- No run-creation or council-spawn operation exists in the service HTTP
  surface; adapters cannot create runs.
- No allowance or budget field exists that any path could raise (recorded
  as an AC-014 dependency).
- Endpoint-level denial is tested with real credentials (§8); hostile
  same-user filesystem isolation remains AC-005's boundary and is
  documented, not used to defer these checks.

## 10. Acceptance Evidence Matrix

Primary story (operator's sequence): Controller A attaches and authorizes a
turn → A disconnects; the authorized work finishes and its follow-up
remains queued → the operator authorizes handoff to Controller B → B sees
the existing records while requests using A's superseded authority are
rejected → only B's explicit release starts the queued follow-up. Repeat
the relevant transitions across a service restart. Controlled fixtures
first; fixture evidence is explicitly distinguished from actual
native-conversation integration.

| Scenario | Required evidence |
|---|---|
| Two concurrent adoption or handoff attempts | One active generation; loser cannot replace its credential |
| Handoff/revocation while work is active | Old controller is fenced out; original verified outcome still persists (§6) |
| Old operation replay after handoff and restart | Stale controller rejected before receipt success |
| Adoption/handoff commits but response is lost | Authorized recovery returns the same active grant; no duplicate generation |
| Lost-lease recovery targets an outdated generation | Replacement controller remains untouched |
| Contributor submits `operator_recovery: true` | Denied; no authority or journaled transition created |
| Delayed disconnect from an earlier attachment | Current connection remains valid |
| Migration of v1 runs with queued, running, and cancelling work | Work preserved; no fabricated adopted identity or automatic execution |
| Readiness/status/history inspection | No lease secret disclosed; inspection does not reconnect a controller |

All four harness identifiers (`opencode`, `claude`, `codex`, `agy`) are
exercised as metadata and controlled-fixture identities.

## 11. Boundaries and Limitations

- Fixture controllers are test identities; real native-conversation
  adoption (an OpenCode/GLM, Claude, Codex, or Agy conversation actually
  holding and presenting a lease) arrives with the adapter and bridge work
  (AC-005–AC-011) and is not claimed here.
- Transport is the single operator capability; a hostile same-user process
  can read local credentials. Endpoint-level separation is enforced and
  tested; process/filesystem isolation is AC-005.
- Leases are stored at the same protection level as today (plaintext
  current secret plus history) inside the protected local store; at-rest
  hashing is deferred as an explicit, recorded tradeoff (§4.2). Response
  and view redaction is not deferred.
- No time-based expiry, heartbeat, or quorum protocol.
