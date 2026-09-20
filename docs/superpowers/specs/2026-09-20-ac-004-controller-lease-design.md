# AC-004: Controller Adoption and Exclusive Revocable Leases — Design

- **Status:** Approved with amendments (operator design review at `b325bb1`; specification review at `49fc79b`)
- **Date:** 2026-09-20
- **Issue:** [#4 (AC-004)](https://github.com/CtrlCarlitos/agent-council/issues/4)
- **Dependencies:** AC-001 (#1, closed), AC-002 (#2, closed), AC-003 (#3, closed at `b325bb1`)
- **Target Packages:** `internal/storage`, `internal/service`, `internal/client`

---

## 1. Problem and Scope

The service now outlives its clients (AC-003). AC-004 answers which existing
agent conversation is authorized to control a run, and how that authority
survives disconnection or changes hands safely.

In scope: controller identity adoption, exclusive revocable run-level
leases, authority-before-replay ordering, service-owned persistence of
already-accepted execution outcomes, run-scoped connection/attachment
semantics, the operator/controller privilege boundary at the endpoint
level, and a minimal restricted controller invocation path.

Out of scope (settled or separate roadmap items): service lifetime
architecture (AC-003, closed), MCP packaging and the full typed tool bridge
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
| Controller lease (per run, per generation) | The adopted controller conversation, delivered through a restricted controller invocation path (§8.1) | Controller commands for that run only (queue/release/replace/discard/cancel-request/connect/disconnect/decision), when its generation is active and its attachment connected | Operator administration; authority over any other run; any authority once superseded or revoked |
| Contributor/adapter credential | None — adapters receive only `(TurnRef, prompt)` and supply observations | Nothing at the HTTP surface | Controller or operator authority, lease acquisition, allowance changes, recursive council creation |

A lease presented as a transport bearer credential does not authenticate:
transport auth remains the operator token (the trusted local bridge model).
Endpoint-level denial must be proven with the actual credentials a
contributor or controller client possesses (a lease, or nothing) — and the
functioning restricted controller path must be tested positively, not only
the failed-transport cases (§8.1).

### 2.2 Authorization matrix

| Operation | Required authority |
|---|---|
| First adoption | Explicit operator authority; no active controller may be silently replaced |
| Normal handoff | Explicit operator authorization for the target, plus the expected current generation and current lease |
| Revoke own active lease | Current controller authority; cannot affect another generation |
| Lost-lease recovery/revocation | Verified operator authority plus explicit recovery intent (`operator_recovery`), a reason, an operation ID, and the expected target generation |
| Reconnect | Current lease and matching adopted controller identity; no replacement identity and no implicit handoff; does **not** require an already-established attachment |
| Contributor request to adopt, hand off, or recover | Denied before mutation |

`operator_recovery: true` is an intent flag, not proof of authority: it is
only meaningful on an operator-authenticated transport, with `op_id`,
`reason`, and `expected_generation`, and it is journaled as an
operator-recovery transition.

Revoking a run lease does not revoke an independently held operator
credential — an old controller that still possesses the operator token
remains an operator under this authentication model. That is why controller
conversations never possess the operator token: the restricted invocation
path (§8.1) holds it, and the conversation holds only its lease.

### 2.3 Operation-class gate table

"Authorize before replay" applies to controller commands; it never means
"require a connected controller for every operation". The authoritative
mutation boundary applies exactly these prerequisites:

| Operation class | Required gate |
|---|---|
| New controller decisions | Current adopted grant and current attachment (generation- and episode-matched, §7) |
| Controller-command receipt replay | Current grant (superseded/revoked credentials are classified first, §5); replay applies the documented disconnected-read policy without creating new authority |
| Reconnect | Current adopted grant and matching identity; cannot require an already-established attachment |
| Operator adoption/recovery | Operator authority and operation-specific generation/scope checks |
| Accepted execution evidence | Internal execution responsibility and the original execution reference (§6) |

## 3. Data Model and Migration v2

### 3.1 New table: `controller_leases`

```sql
CREATE TABLE controller_leases (
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    generation INTEGER NOT NULL CHECK (generation >= 0),
    harness TEXT,            -- NULL only for generation-0 provenance rows
    controller_ref TEXT NOT NULL,
    lease TEXT NOT NULL,     -- verification representation, retained for the row's lifetime (§4.2)
    status TEXT NOT NULL CHECK (status IN ('active','legacy','superseded','revoked')),
    granted_by_op_id TEXT NOT NULL,  -- operation that issued this generation
    attachment_id TEXT,      -- current connection episode, active rows only
    connected INTEGER NOT NULL DEFAULT 0 CHECK (connected IN (0,1)),
    attached_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, generation)
);
CREATE UNIQUE INDEX idx_controller_leases_active
    ON controller_leases(run_id) WHERE status = 'active';
```

Rules:

- **Mandatory** partial unique index enforces at most one `active` row per
  run (the repository already uses this mechanism for active contributor
  sessions).
- The active row's `lease` and `runs.controller_lease` agree within the same
  transaction; the history table is never a competing source of truth.
- First adoption is generation 1; every later grant uses a strictly greater
  generation, **including adoption after revocation**. Disconnect never
  increments the generation; reconnect never creates a second controller.
- The `lease` column is the credential's verification representation for
  the row's entire lifetime: retained (not erased) when the row becomes
  `superseded`/`revoked` so stale credentials can be classified. Retaining
  plaintext historical values inside the protected local store is the
  accepted, explicit tradeoff (§4.2); outward disclosure is forbidden.
- Empty presented credentials are rejected before any equality check;
  revocation clears `runs.controller_lease` to `''`.

### 3.2 Run-level attachment state

`runs` gains `controller_adopted INTEGER NOT NULL DEFAULT 0` (1 only once a
verified adoption exists). The active `controller_leases` row carries the
current `attachment_id` and `connected` flag — run-scoped by construction.
Existing session `controller_status` columns are compatibility projections:
updated transactionally (with version bumps) when run-level state changes,
never independent grants of authority.

`dispatch_intents` gains `issuing_controller_generation INTEGER NOT NULL
DEFAULT 0` — part of migration v2 (not a later addition), recording which
generation authorized each accepted execution.

### 3.3 Migration v2: frozen v1 inputs and the legacy provenance rule

Migration uses immutable versioned inputs. The released v1 DDL and checksum
are frozen verbatim as the v1 migration; a distinct v2 migration adds the
table, index, columns, and backfills. A fresh database applies v1 then v2
sequentially — there is no separate "latest schema" shortcut that could
diverge from the upgrade path.

For every existing run, v2 inserts a **generation-0 provenance row**:
`harness = NULL`, `controller_ref = 'legacy-v1'`, `status = 'legacy'`,
`lease` = the run's existing `controller_lease`, `granted_by_op_id =
'legacy-v1'`. `controller_adopted` stays 0, and existing accepted attempts
are backfilled with `issuing_controller_generation = 0` **without modifying
their original attempt IDs**.

**The legacy credential is provenance, not an alternate controller.** Every
unadopted run requires explicit adoption before new controller decisions:
the authorizer returns `adoption_required` for any controller command on a
run with `controller_adopted = 0`, including a presented lease that matches
the generation-0 value. A generation-0 record may support authorized
adoption or operator recovery (as the credential being superseded), nothing
else. Previously accepted generation-0 executions still finish through the
internal outcome path (§6), which is what preserves migrated work — not
continued usability of the legacy lease. New runs created after v2 follow
the same rule uniformly: `CreateRun` records its supplied lease as
generation-0 provenance (or empty) and no authority.

Migration tests create a verified v1 database from the frozen v1 fixture,
then open it with the upgraded store, covering: repeat-open, concurrent
initialization, interrupted migration, checksum mismatch, and
unsupported-newer-version. Preservation is compared explicitly across
pending prompts, active and historical turns, attempt identities, native
bindings, and journal evidence — `GetTurnDetails` alone is not sufficient
for prompts never released.

## 4. Grant Lifecycle

All transitions are single-transaction storage operations with one request
contract per operation (authority context, generation/recovery inputs, and
a **secret candidate** — never a retry input, §4.1).

- **Adopt** (`AdoptController`): requires operator intent (service-level
  transport) and that no active adopted controller exists (idempotent
  replay of the original adoption operation excepted). A generation-0
  `legacy` row is superseded by supplying its credential or verified
  operator recovery. Installs `generation = max(generation)+1`, records
  harness and controller reference, sets `runs.controller_lease`,
  `controller_adopted = 1`, and `granted_by_op_id = opID`.
- **Handoff** (`HandoffController`): requires the current active lease and
  `expected_generation`; supersedes the old row, installs the new grant,
  updates attachment state, and journals atomically.
- **Revoke** (`RevokeController`): with the current lease (controller
  self-revocation) or via verified operator recovery (`op_id`, `reason`,
  `expected_generation`, recovery intent). Marks the row `revoked`, clears
  `runs.controller_lease` to `''`. No active grant remains.
- **Failure atomicity:** a failed transition leaves authority, connection
  state, and pending work unchanged.

### 4.1 Secret issuance: generated once per committed operation

The service generates a random **candidate** secret per attempt and passes
it into storage; the candidate is never an idempotency input. The
transaction fingerprint covers the operator's command and target — not the
candidate. On idempotent replay, the transaction resolves the **original
committed issuance** (via `granted_by_op_id`) before any candidate is
installed: it is acceptable to discard an unused candidate on a concurrent
retry; it is not acceptable to rotate again, return the unused candidate,
or report an idempotency conflict merely because the candidate differs.

One secret therefore exists per committed adoption/handoff operation. A
lost response is remedied by **recovery, never re-issuance**:

- Transactional replay of the grant operation returns the original issuance
  while its generation remains active.
- `RecoverControllerCredential(sourceOpID, expectedGeneration)` is the
  explicit operator-authorized inspection path over the persisted
  `granted_by_op_id` association.
- A superseded controller cannot recover the replacement's secret. Once the
  issued generation is revoked or superseded, operation inspection returns
  historical status only — no resurrected credential.

### 4.2 Secret redaction

Lease secrets never appear in generic operation receipts, ordinary
history/status responses, readiness/status diagnostics, or error messages.
The journal stores `caller_lease` inside the protected local database
(accurate today and unchanged); every outward-facing view exposing journal
or controller history redacts lease values rather than reproducing raw
payloads. Deferring database-at-rest hashing is an accepted, explicit
tradeoff within the protected-local-store boundary (security-hardening
roadmap candidate); deferring credential-response and redaction rules is
not.

### 4.3 Journal helper adaptation

The existing `checkOrRecordIdempotency`/`recordJournalEntry` helpers assume
a recorded `caller_lease` is the replay authority. Grant administration
(operator-authorized, candidate-carrying) and internal outcome writes
(execution-identity-authorized) require deliberate adaptations of those
semantics — fingerprint composition, replay authorization, and payload
redaction are specified per operation rather than inherited unchanged.

## 5. Authorization and Replay Ordering

### 5.1 Entry-point inventory by responsibility

Completeness is defined by this inventory of **actual** storage entry
points, each assigned a category or explicitly documented as unreachable in
this scope. No new HTTP endpoints are added for them implicitly.

| Category | Entry points | Authorization rule |
|---|---|---|
| Controller commands | queue prompt, replace prompt, discard prompt, release turn (incl. `FindCommittedRelease` replay), cancel request (incl. `:req`/`:term` stages), reconcile (incl. composite stages and receipt replay), record decision, run-scoped connect/disconnect | Current adopted grant (and attachment for new decisions) first; then idempotent replay; then state validation |
| Operator administration | adopt, handoff, revoke, credential recovery, controller record read, run creation, archival, native-binding updates (`SetNativeBinding`), artifact publication (`PublishArtifact`) | Operator transport; operation-specific inputs. Run creation, archival, binding updates, and artifact publication currently validate a caller lease against `runs.controller_lease`; they are reclassified as operator administration (or controller command where the design says so) and their ordering fixed in the same pass — their HTTP exposure is unchanged in this iteration |
| Internal execution observations | supervisor outcome persistence, dispatch observations, reconciliation outcomes for accepted executions | Persisted execution identity incl. attempt (§6); never a controller lease; never HTTP-exposed |
| Read-only inspection | turn read, status, readiness, controller record read, journal/history views | Authenticated read; no execution, no reconnection side effects, no secret disclosure |
| Explicitly unreachable in this scope | session creation (`CreateSession`) is storage-internal seeding with no HTTP route; documented rather than retrofitted | n/a |

`CreateSession` currently invokes the historical-receipt helper before
current-authority checking; its ordering is fixed as part of the same
storage pass even though no HTTP route reaches it, so the invariant holds
for future exposure. The shared helper lives in `session_store.go`; the
retrofit spans both files.

### 5.2 Authoritative transaction ordering

For controller-originated commands, storage transitions apply:

```
resolve the requested run and complete resource scope
  → validate current controller authority (adopted, active, generation-consistent grant)
  → classify superseded / revoked / legacy-provenance / unknown credentials
  → look up and validate an existing operation (idempotency)
  → return its receipt, or validate current state and commit a new operation
```

The authorizer's success contract is an **internally consistent adopted,
active grant**: the presented credential matches `runs.controller_lease`,
`controller_adopted = 1`, and the single active history row agrees — a bare
string match authorizes nothing. Classification precedence: empty →
`adoption_required` (unadopted run, including legacy-provenance match) →
`lease_superseded` (superseded/revoked history match) → active match →
`unauthorized`. Operations requiring connection state validate it at this
same authoritative mutation boundary.

**Error precedence rule:** stale-authority classification precedes
generation mismatch. A losing concurrent handoff whose lease was superseded
by the winner receives `ErrLeaseSuperseded`; `ErrGenerationMismatch` is
reserved for an otherwise-authorized operation whose `expected_generation`
is stale. Tests enforce this ordering.

A matching receipt still precedes ordinary `expected_version` checks for a
current controller, so committed responses remain recoverable after state
advances.

### 5.3 Replay decision table

| Request | Result |
|---|---|
| Current controller retries its matching committed operation | Original receipt; no additional execution or transition |
| Superseded/revoked controller retries an operation it previously committed | `ErrLeaseSuperseded` (HTTP 403 `lease_superseded`); no successful controller replay |
| Current controller reuses an operation ID with different content or scope | Idempotency conflict |
| New controller or operator inspects an earlier decision/result | An authorized read of historical evidence — not execution of the previous controller's command |

## 6. Accepted-Execution Outcome Authority

Authority to make a **new decision** is separate from authority to
**record evidence for an accepted execution**.

- A release records the issuing controller generation and the original
  execution identity — session, turn, **and attempt** — in its durable
  records.
- The service retains a narrowly scoped responsibility — held by the
  execution supervisor, not by any controller — to observe and persist that
  execution's outcome and dispatch acknowledgements, even if controller
  authority later changes. This responsibility cannot authorize another
  release, alter budgets, adopt a controller, or approve a result.
- Outcome persistence is a service-internal storage operation authorized by
  a **typed execution reference** carrying the original session, turn, and
  attempt ID, validated against the persisted release/dispatch-intent
  inside the write transaction; the issuing generation is read from that
  accepted release record, never inferred from the current controller. It
  performs no controller-lease comparison. No endpoint accepts a
  caller-authored terminal observation or internal-authority flag;
  authorized cancellation/reconciliation handlers call the operation
  internally after obtaining adapter evidence. Adapters continue to supply
  observations, not controller authority.
- Reconciliation preserves its transaction: the current controller
  authorizes a particular recovery operation; the service then records its
  verified observation using the captured execution and recovery
  references, even if control changes while the authorized probe is in
  flight. A stale controller cannot initiate a new probe; a previously
  authorized probe never needs the new controller's secret substituted into
  it merely to persist evidence. Terminal-conflict handling, exact-duplicate
  behavior, intent resolution, and recovery-episode consumption remain one
  atomic transition.
- The audit trail reads: controller A authorized the work; the service
  recorded its verified outcome; controller B later controlled the run.

Required evidence: handoff mid-active-turn and revocation mid-active-turn
(A fenced, original verified outcome persists, follow-up queued until B —
or a newly adopted controller — explicitly releases it); plus wrong-attempt
rejection, stale recovery generation, handoff between probe and
persistence, dispatch acknowledgement after rotation, duplicate terminal
evidence, and conflicting terminal evidence.

## 7. Connection and Attachment Semantics

- The run-level record stores the current controller's connection state and
  an attachment identifier distinguishing connection episodes without
  rotating the lease.
- In-instance attachment state is an **identity record** — run, controller
  generation, attachment ID, and owning service instance — not a Boolean.
  It is published or cleared only when the corresponding authoritative
  transition still matches.

| Event | Required behavior |
|---|---|
| Lost connect response, same live instance and attachment | Recover the existing attachment receipt without creating a second episode |
| Old connect operation replayed after restart | Returns its receipt; cannot by itself satisfy the fresh-instance reattachment requirement |
| Delayed disconnect from attachment A after B connected | B remains connected |
| Delayed connect response from an earlier generation | Cannot mark the replacement controller attached |
| Handoff/revoke | Invalidate the exact old attachment and update session projections transactionally |

- **Restart rule:** a persisted `connected` flag never establishes that a
  live controller connection exists; the new service instance requires
  explicit reattachment before new decisions, while accepted executions
  still complete (§6). Reconnect prerequisites never include being already
  connected.
- **Disconnect evidence** in this iteration is an explicit controller
  command; ordinary HTTP request completion is never interpreted as
  controller disconnection.
- No heartbeat or automatic time-based lease expiry exists.

## 8. HTTP Surface

### 8.1 Restricted controller invocation path

A minimal controller-facing invoker (local capability wrapper in
`internal/client`, the seed of the AC-011 bridge): it holds the operator
credential internally — outside controller-supplied arguments and responses
— and exposes only the defined controller commands, bound to one run and
lease. It rejects attempts to invoke adoption, handoff administration,
credential recovery, or any arbitrary route through that interface. Evidence
includes a positive test (an adopted controller performs a permitted
command through this path) and an escalation test through the same
interface. Real harness integration remains later work; the evidence is
labeled accordingly.

### 8.2 Endpoints

New run-scoped endpoints (operator bearer transport):

- `GET /v1/runs/{run_id}/controller` — redacted controller record.
- `POST /v1/runs/{run_id}/controller/adopt` — `{op_id, harness,
  controller_ref, bootstrap_lease?}` (legacy provenance credential) or
  `{op_id, harness, controller_ref, operator_recovery: true, reason,
  expected_generation}`.
- `POST /v1/runs/{run_id}/controller/handoff` — `{op_id, current_lease,
  expected_generation, harness, controller_ref}`.
- `POST /v1/runs/{run_id}/controller/revoke` — `{op_id, controller_lease}`
  (self-revoke) or `{op_id, operator_recovery: true, reason,
  expected_generation}`.
- `POST /v1/runs/{run_id}/controller/credential/recover` — `{op_id,
  source_op_id, expected_generation}`.
- `POST /v1/runs/{run_id}/controller/connect` — `{op_id, controller_lease,
  expected_generation}`; creates a new attachment episode.
- `POST /v1/runs/{run_id}/controller/disconnect` — `{op_id,
  controller_lease, expected_generation, attachment_id}`; stale
  attachments are ignored.

Adoption and handoff deliberately grant **no** connection state; the new
controller connects explicitly. Error codes: `lease_superseded` (403),
`adoption_required` (409), `adoption_exists` (409), `generation_mismatch`
(409), `not_connected` (409), `recovery_unavailable` (409), plus existing
conventions.

## 9. Worker and Contributor Separation

- Lease-affecting operations accept no session/contributor parameters and
  require operator transport plus their own authorization inputs.
- Adapters receive `(TurnRef, prompt)` and observation results only; no
  lease, token, or controller material crosses the adapter interface.
- No run-creation or council-spawn operation exists in the service HTTP
  surface.
- No allowance or budget field exists that any path could raise (recorded
  as an AC-014 dependency).
- Endpoint-level denial is tested with real credentials (lease-as-bearer
  fails transport; contributor clients hold nothing) **and** the
  restricted invoker's positive/escalation pair (§8.1). Hostile same-user
  filesystem isolation remains AC-005's boundary, documented without
  deferring these checks.

## 10. Acceptance Evidence Matrix

Primary story: Controller A adopts (secret once) → **connects** →
authorizes a gated turn → disconnects explicitly → the authorized work
finishes and its follow-up remains queued → the operator authorizes handoff
to Controller B → B connects explicitly → B sees the existing records while
requests using A's superseded authority are rejected → only B's explicit
release starts the queued follow-up. Relevant transitions repeat across a
service restart. Controlled fixtures first, explicitly distinguished from
native-conversation integration.

| Scenario | Required evidence |
|---|---|
| Two concurrent adoption or handoff attempts | One active generation; loser classified by stale authority, not merely generation mismatch |
| Handoff/revocation while work is active | Old controller fenced out; original verified outcome still persists (§6) |
| Old operation replay after handoff and restart | Stale controller rejected before receipt success |
| Adoption/handoff commits but response is lost | Authorized recovery returns the same active grant; no duplicate generation |
| Lost-lease recovery targets an outdated generation | Replacement controller remains untouched |
| Contributor submits `operator_recovery: true` | Denied; no authority or journaled transition created |
| Delayed disconnect from an earlier attachment | Current connection remains valid |
| Migration of v1 runs with queued, running, and cancelling work | Work preserved (prompts, turns, attempts, bindings, journals); no fabricated adopted identity or automatic execution; legacy lease cannot release or decide; the running turn's result still records; an adopted controller can subsequently release the queued follow-up |
| Readiness/status/history inspection | No lease secret disclosed; inspection does not reconnect a controller |
| Restricted controller path | Positive permitted-command execution and escalation denial through the same interface |
| Wrong-attempt outcome evidence / stale recovery generation / dispatch ack after rotation / duplicate and conflicting terminal evidence | Rejected or atomically resolved per §6 |

All four harness identifiers (`opencode`, `claude`, `codex`, `agy`) are
exercised as metadata and controlled-fixture identities.

## 11. Boundaries and Limitations

- Fixture controllers are test identities; real native-conversation
  adoption arrives with adapter and bridge work (AC-005–AC-011) and is not
  claimed here.
- Transport is the single operator capability; a hostile same-user process
  can read local credentials. Endpoint-level and restricted-invoker
  separation is enforced and tested; process/filesystem isolation is AC-005.
- Leases (current and retired verification representations) are stored
  plaintext inside the protected local store; at-rest hashing is deferred
  as an explicit, recorded tradeoff (§4.2). Response and view redaction is
  not deferred.
- No time-based expiry, heartbeat, or quorum protocol.
