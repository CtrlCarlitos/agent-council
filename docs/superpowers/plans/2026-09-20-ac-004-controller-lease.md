# AC-004 Controller Adoption & Revocable Lease Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native inline TDD with 1 implementation owner and 2 internal verification gates). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Adopt an existing controller conversation with an exclusive, revocable, generation-stamped run lease — fencing stale authority immediately while preserving the authority and evidence of already-accepted work.

**Architecture:** Run-scoped lease generations recorded in a new `controller_leases` history table (migration v2, legacy runs become generation-0 bootstrap rows); `runs.controller_lease` remains the current-secret authority. Storage transitions apply authority-before-replay ordering; the execution supervisor persists accepted-execution outcomes under persisted execution identity (no controller-lease check); run-level attachment state governs connection with an in-instance reattachment gate after restart. HTTP adds operator-administered adopt/handoff/revoke/credential-recovery endpoints behind the existing operator bearer transport.

**Tech Stack:** Go 1.25.0, SQLite (modernc.org/sqlite v1.59.0 via database/sql), `net/http`, `crypto/rand`.

**Spec:** [`../specs/2026-09-20-ac-004-controller-lease-design.md`](file:///home/carlitos/projects/CtrlCarlitos/agent-council/docs/superpowers/specs/2026-09-20-ac-004-controller-lease-design.md)

## Global Constraints

- Go 1.25.0 baseline; dual CI verification (`CGO_ENABLED=0` and `-race -count=3`).
- AC-003 architecture is closed: no service-lifetime redesign, no transport changes beyond the new endpoints, no fake adapters in production.
- `runs.controller_lease` stays the current-secret authority; the history table never competes as a source of truth.
- Empty presented credentials are rejected before any equality check; revoked runs hold `runs.controller_lease = ''`.
- Current authority precedes idempotent receipt replay on every controller command path (shared helper, `FindCommittedRelease`, composite cancel/reconcile stages).
- Lease secrets never appear in generic receipts, status/readiness/history views, or error strings; journal payloads in outward views are redacted.
- One secret per committed adopt/handoff operation; recovery (never re-issuance) is operator-authorized and generation-bound.
- No heartbeat/time expiry; no new role-management framework; contributor/lease credentials never authenticate transport.
- Migration must preserve queued, running, and cancelling work without fabricating adopted identity.

## Verification Gates
- **Gate 1 (after Tasks 1–3)**: migration v2 legacy rule, grant-transition invariants (atomicity, monotonic generations, concurrency via `expected_generation`, generated-once recovery, redaction), and authority-before-replay ordering across all inventory categories. `CGO_ENABLED=0 go test ./...` and `go test -race -count=3 ./internal/storage ./internal/service`.
- **Gate 2 (after Tasks 4–7)**: mid-flight handoff/revocation outcome persistence, attachment/restart semantics, endpoint privilege-boundary denials, and the full acceptance matrix including restart repetition and subprocess-free HTTP-level evidence.

## Review Focus

1. **Replay after rotation**: a superseded controller retrying its own committed operation must receive `lease_superseded`, never the old receipt.
2. **Mid-flight rotation**: handoff/revocation during an active turn must not prevent the verified outcome from persisting under the original execution identity.
3. **Concurrent transitions**: two handoffs (or handoff + stale recovery) racing on `expected_generation` — exactly one wins.
4. **Lost response recovery**: operator recovery returns the same active-generation secret; no second generation is ever issued for a lost response.
5. **Restart reattachment gate**: persisted `connected` never authorizes new decisions on a fresh service instance.

---

### Task 1: Schema Migration v2 and Legacy Bootstrap Rows

**Files:**
- Modify: `internal/storage/schema.sql`
- Modify: `internal/storage/migrations.go`
- Test: `internal/storage/controller_lease_migration_test.go`

**Interfaces:**
- Consumes: `Store.Migrate`, `schema_migrations` versioning, `checkOrRecordIdempotency` conventions.
- Produces:
  - `controller_leases` table per spec §3.1 (PK `(run_id, generation)`, statuses `active|superseded|revoked`, nullable `harness`, `attachment_id`, `connected`).
  - `runs.controller_adopted INTEGER NOT NULL DEFAULT 0`.
  - Migration v2 that, for each existing run, inserts the generation-0 bootstrap row (`harness NULL`, `controller_ref 'legacy-v1'`, `status 'active'`, `lease = runs.controller_lease`).

**Steps:**

- [ ] **1.1 Write the failing migration test.** Create runs with queued, running, and cancelling turns under schema v1 semantics (open a store, seed via `CreateRun`/`QueuePrompt`/`ReleaseTurn`/`RequestCancel`, close). Reopen (triggering migration) and assert: `CurrentSchemaVersion() == 2`; a `controller_leases` row `(run_id, generation=0, harness IS NULL, controller_ref='legacy-v1', status='active')` exists per run with `lease` equal to the supplied v1 lease; `controller_adopted = 0`; the queued/running/cancelling turns are byte-identical to pre-migration `GetTurnDetails`; no dispatch occurred (no automatic execution).
- [ ] **1.2 Run and verify it fails** (`undefined: controller_leases` / version 1). 
- [ ] **1.3 Implement migration v2** in `schema.sql` + `migrations.go`: new DDL applied inside the versioned transaction; bootstrap-row backfill for existing runs; fresh databases start directly at v2 with no bootstrap rows (adoption required before any controller command on a new run — see Task 3 for the `adoption_required` gate that makes this safe).
- [ ] **1.4 Run the migration test and the full storage suite** (`CGO_ENABLED=0 go test ./internal/storage/ -count=1`); fix regressions in checksum/version tests.
- [ ] **1.5 Commit**: `feat(storage): controller_leases schema v2 with legacy bootstrap rows`.

### Task 2: Grant Transitions — Adopt, Handoff, Revoke, Credential Recovery

**Files:**
- Modify: `internal/storage/transitions.go` (new section; do not disturb AC-003 corrections)
- Test: `internal/storage/controller_lease_transitions_test.go`

**Interfaces:**
- Produces (all journal through `recordJournalEntry`; all single-transaction; all follow §5.2 ordering):
  - `AdoptController(ctx, opID, runID, harness string, controllerRef string) (ControllerGrantReceipt, error)` — harness ∈ roster, `controller_ref` non-empty sanitized; refuses when an active row with `controller_adopted` semantics exists (except idempotent replay of this op); supersedes a generation-0 bootstrap row; picks `generation = max(generation)+1`; sets `runs.controller_lease` and `controller_adopted=1`. Secret is supplied by the caller (service generates it) so storage stays deterministic: signature `AdoptController(ctx, opID, runID, harness, controllerRef, newLeaseSecret string)`.
  - `HandoffController(ctx, opID, runID, currentLease string, expectedGeneration uint64, harness, controllerRef, newLeaseSecret string) (ControllerGrantReceipt, error)`.
  - `RevokeController(ctx, opID, runID, lease string, operatorRecovery bool, expectedGeneration uint64, reason string) (OperationReceipt, error)`.
  - `RecoverControllerCredential(ctx, opID, runID, sourceOpID string, expectedGeneration uint64) (ControllerGrantReceipt, error)` — returns the active generation's secret for the committed source operation.
  - `GetControllerRecord(ctx, runID) (ControllerRecord, error)` — `{Generation, Harness, ControllerRef, Status, Adopted, Connected, AttachmentID}`; no secret field.
  - `ControllerGrantReceipt{OperationReceipt; Generation uint64; LeaseSecret string}` — the secret appears only in adopt/handoff/recovery responses.
- Errors: `ErrAdoptionExists`, `ErrGenerationMismatch`, `ErrLeaseSuperseded`, `ErrRecoveryUnavailable` (source op unknown, not a grant op, or its generation no longer active).

**Steps:**

- [ ] **2.1 Failing transition tests** covering the invariant table: adopt→gen 1; adopt-after-revoke→gen 2 (never reset); handoff supersedes and bumps; two concurrent handoffs on the same `expected_generation` → exactly one commits, loser `ErrGenerationMismatch`; adopt refusal while active (non-replay); rollback atomicity (invalid harness leaves no row/journal); revocation clears `runs.controller_lease` to `''` and no later empty-string comparison passes; recovery returns the *same* secret for a lost adopt response while active, `ErrRecoveryUnavailable` after supersession; `GetControllerRecord` exposes no secret; a superseded lease presented to handoff/recover/self-revoke → `ErrLeaseSuperseded`; all four harness values accepted as metadata.
- [ ] **2.2 Run to verify failures**, then implement the four transitions + record read following existing transaction conventions (authority check → classification → idempotency → commit).
- [ ] **2.3 Add redaction guard test**: `GetControllerRecord` and `HydrateState`-based views redact lease values from any `Payload` they surface for grant operations (journal `caller_lease` stays in-DB; outward views elide).
- [ ] **2.4 Full storage suite + race (`go test -race -count=3 ./internal/storage/`)**; commit `feat(storage): controller grant transitions with generated-once secrets`.

### Task 3: Authority-Before-Replay Ordering and Stale-Lease Classification

**Files:**
- Modify: `internal/storage/transitions.go` (authorization section)
- Test: `internal/storage/controller_authorization_test.go`

**Interfaces:**
- Produces:
  - `authorizeRunController(ctx, tx, runID, presentedLease string) error` — rejects empty; matches `runs.controller_lease` → nil; matches a `superseded|revoked` history row → `ErrLeaseSuperseded`; else `ErrUnauthorizedOperation`.
  - Retrofit ordering in every Task-3-category entry point from spec §5.1: `QueuePrompt`, `ReplacePendingPrompt`, `DiscardPendingPrompt`, `ReleaseTurn` (+ `FindCommittedRelease`), `RequestCancel` (+ `:term` composite), `ReconcileSession` (+ `FindOperationReceipt` path in the handler), `RecordTerminalOutcome` (controller path only; Task 4 supersedes), `SetControllerConnection`, `RecordDecision`.
  - `adoption_required` gate: on runs with `controller_adopted = 0` and no active bootstrap row (fresh v2 runs), controller commands fail with `ErrAdoptionRequired` before state validation.

**Steps:**

- [ ] **3.1 Write the replay decision-table tests** (spec §5.3): current controller retries committed op → original receipt, no new transition; superseded controller retries its own committed op (queue, release via `FindCommittedRelease`, cancel `:req`, reconcile `:reconcile`) → `ErrLeaseSuperseded` with **no** successful replay; current controller reuses op ID with different content → `ErrIdempotencyConflict`; historical inspection stays an authorized read.
- [ ] **3.2 Verify failures** (today the historical lease replays successfully — this is the reviewed defect), then implement `authorizeRunController` and reorder the inventory entry points so scope resolution → authority → classification → idempotency → state validation.
- [ ] **3.3 Ordering-preservation test**: a *current* controller's receipt replay still precedes `expected_version` checks (recovery of a committed response after state advanced).
- [ ] **3.4 Full suites + race; commit** `fix(storage): current authority precedes idempotent replay for controller commands`.

### Task 4: Service-Owned Outcome Persistence for Accepted Executions

**Files:**
- Modify: `internal/storage/transitions.go` (release stamping + observed-outcome op)
- Modify: `internal/service/release.go`, `internal/service/supervisor.go`
- Test: `internal/storage/execution_outcome_authority_test.go`, `internal/service/review_ac004_outcome_authority_test.go`

**Interfaces:**
- Produces:
  - `ReleaseTurn` additionally stamps `dispatch_intents.issuing_controller_generation` (schema v2 column; 0 for bootstrap).
  - `RecordObservedExecutionOutcome(ctx, opID, sessionID, turnKey string, status council.TurnStatus, rawResult string) (OperationReceipt, error)` — authorizes by persisted execution identity: turn exists in nonterminal state for the session with `active_key`/recovery-context match per existing `RecordTerminalOutcome` validation; **no controller-lease comparison**; same terminal-conflict/quarantine semantics; journal entry records the issuing generation from the release record. Service-internal only (no HTTP route).
- Supervisor switches from `RecordTerminalOutcome(callerLease, …)` to `RecordObservedExecutionOutcome`; `handleCancel`'s confirmed-commit and `handleReconcile`'s terminal commit keep controller-authority validation of their own request and then use the observed-outcome op for the write.

**Steps:**

- [ ] **4.1 Failing storage test**: release under generation N → rotate lease (handoff) → `RecordObservedExecutionOutcome` still commits the terminal outcome; the journal shows generation N as issuer; a *controller* replaying `RecordTerminalOutcome` with the superseded lease is fenced (Task 3).
- [ ] **4.2 Failing service test** (fake adapter, gated execution): A releases gated turn → handoff to B mid-flight → A's commands and replays rejected (`lease_superseded`) → worker finishes → turn reaches its verified terminal state under the original execution → follow-up prompt stays queued until B explicitly releases it. Repeat with revocation (no controller) — accepted work still finishes and parks.
- [ ] **4.3 Implement** the stamping, the observed-outcome operation, and the supervisor/handler switch. Audit that no HTTP route reaches the observed-outcome op and that no "internal" flag exists on any request body.
- [ ] **4.4 Race + full suites; commit** `feat(service): persist accepted-execution outcomes under execution identity`.

### Task 5: Run-Level Attachment Semantics and Restart Gate

**Files:**
- Modify: `internal/storage/transitions.go` (`SetControllerConnection` becomes run-scoped projection updater), `internal/service/server.go`, `internal/service/commands.go` (connect/disconnect handlers), `internal/service/coordinator.go` (in-instance attachment gate)
- Test: `internal/storage/controller_attachment_test.go`, `internal/service/review_ac004_attachment_test.go`

**Interfaces:**
- Produces:
  - `ConnectRunController(ctx, opID, runID, lease string, expectedGeneration uint64) (OperationReceipt, error)` — validates current lease + matching adopted identity (harness/controller_ref unchanged); creates a new `attachment_id` (e.g. `uuid`-style random); sets run-level `connected=1`; transactionally projects session `controller_status='connected'` with version bumps; rejects replacement identity (no implicit handoff).
  - `DisconnectRunController(ctx, opID, runID, lease, attachmentID string, expectedGeneration uint64) (OperationReceipt, error)` — ignores a stale attachment (delayed disconnect from A leaves B connected).
  - Handoff/revoke invalidate the old attachment (`connected=0`, `attachment_id` cleared) and project sessions to `disconnected` transactionally; the new controller never inherits connected flags.
  - Coordinator exposes `ControllerAttached(runID) bool` + `MarkControllerAttached(runID)`: on `Server.Start`, no run is attached; controller commands require both durable connected state and in-instance attachment (`not_connected` 409). Readiness/status inspection never marks attachment.
- The existing session-scoped `/controller/connect` route delegates to `ConnectRunController` (compatibility: session path resolves the run).

**Steps:**

- [ ] **5.1 Failing tests**: same-generation reconnect (attachment B) then delayed disconnect from attachment A → B remains connected; handoff → old attachment invalid, sessions projected disconnected, B must connect explicitly; restart (close/reopen service) → persisted `connected` does not authorize new decisions until explicit reattach, while an in-flight accepted execution still completes; inspection endpoints never change attachment state.
- [ ] **5.2 Implement**, keeping disconnect generation-neutral (no generation bump) and reconnect single-controller.
- [ ] **5.3 Race + full suites; commit** `feat(service): run-scoped controller attachment with restart reattachment gate`.

### Task 6: HTTP Surface and Privilege-Boundary Denials

**Files:**
- Create: `internal/service/controller_admin.go`
- Modify: `internal/service/server.go` (routes), `internal/service/error.go` if needed
- Test: `internal/service/controller_admin_test.go`

**Interfaces:**
- Produces routes per spec §8: `GET /v1/runs/{run_id}/controller`, `POST …/controller/adopt`, `POST …/controller/handoff`, `POST …/controller/revoke`, `POST …/controller/credential/recover`. All behind `authMiddleware` (operator bearer), all under `TrackControl` admission, all with strict JSON. Response envelopes carry `ControllerGrantReceipt` (secret included only in adopt/handoff/recover success responses) or the redacted `ControllerRecord`. Error codes: `lease_superseded`, `generation_mismatch`, `adoption_exists`, `adoption_required`, `not_connected`, `recovery_unavailable`.
- Secret generation: `crypto/rand` 32-byte hex, generated once per committed operation in the service layer and passed into storage.

**Steps:**

- [ ] **6.1 Failing privilege tests using real client credentials**: lease-as-bearer on every admin route → 401 (transport never accepts a lease); no-credential → 401; contributor-fixture client (holds nothing) → 401 before any mutation; `operator_recovery: true` from a lease-authenticated... (impossible by construction — assert instead that recovery demands operator transport and records reason/op_id/expected_generation, and that a wrong `expected_generation` leaves the replacement untouched).
- [ ] **6.2 Failing redaction tests**: readiness/status/controller-record/history responses contain no lease substring for any known secret.
- [ ] **6.3 Implement handlers + wiring; verify the full replay table over HTTP** (superseded retry → 403 `lease_superseded`).
- [ ] **6.4 Race + full suites; commit** `feat(service): operator-administered controller lease endpoints`.

### Task 7: End-to-End Acceptance Suite and Evidence

**Files:**
- Create: `internal/service/review_ac004_acceptance_test.go`
- Modify: PR description (evidence map) at draft-PR time
- Test: `cmd/council` untouched (no subprocess requirement — HTTP-level controlled fixtures per the issue's fixture-first boundary)

**Steps:**

- [ ] **7.1 Primary story test** (fake adapter, HTTP-level, all four harness identities exercised across subtests): A adopts (secret returned once) → connects → releases gated turn → disconnects → work completes, follow-up queued, A's new decisions rejected (`not_connected` family) → operator handoff to B → B reads records, A's replays → `lease_superseded` → only B releases the follow-up. Repeat the adoption→handoff→replay transitions across a service restart (new `Server` on the same state dir).
- [ ] **7.2 Acceptance-matrix tests** from spec §10 (concurrency, mid-flight rotation, lost-response recovery, stale recovery generation, contributor denial, delayed disconnect, v1 migration inspection, redaction/no-reconnect-on-inspection).
- [ ] **7.3 Evidence discipline**: name fixture-only tests explicitly (`…_ControlledFixture`); document in each header that native-conversation adoption is unverified.
- [ ] **7.4 Full verification**: `gofmt -l`, `go vet ./...`, `CGO_ENABLED=0 go test ./...`, `go test -race -count=3 ./...`, `GOOS=windows|darwin go vet ./... && go test -c`. Commit `test(service): AC-004 acceptance matrix`.
- [ ] **7.5 Open the draft PR** (`feat/ac-004-controller-lease` → `main`, "Closes #4") with the evidence map, verification boundaries, and limitations per AGENTS.md.

## Self-Review Notes

- Spec coverage: §2 matrix → Tasks 2/6; §3 migration → Task 1; §4 lifecycle+recovery+redaction → Tasks 2/6; §5 ordering/inventory → Task 3; §6 outcome authority → Task 4; §7 attachment → Task 5; §8 HTTP → Task 6; §10 matrix → Task 7; §9 separation → Tasks 6 denials + Task 4 audit step; §11 boundaries → Task 7 evidence discipline + PR description.
- Type consistency: `ControllerGrantReceipt`/`ControllerRecord`/error names are used identically across Tasks 2, 6, and 7; `RecordObservedExecutionOutcome` appears in Task 4 definition and Task 4-only consumers.
- No placeholders; every task carries concrete failing-test definitions and interfaces.
