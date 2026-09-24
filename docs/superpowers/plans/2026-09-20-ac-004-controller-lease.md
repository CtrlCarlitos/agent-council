# AC-004 Controller Adoption & Revocable Lease Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (Native inline TDD with 1 implementation owner and 2 internal verification gates). Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Adopt an existing controller conversation with an exclusive, revocable, generation-stamped run lease — fencing stale authority immediately while preserving the authority and evidence of already-accepted work.

**Architecture:** Run-scoped lease generations in a `controller_leases` history table (frozen-v1 + v2 sequential migrations; legacy runs become generation-0 provenance, never an alternate controller); `runs.controller_lease` remains the current-secret authority behind an adopted/active/consistent grant check. Storage transitions apply authority-before-replay ordering over a complete entry-point inventory; the execution supervisor persists accepted-execution outcomes and dispatch acknowledgements under typed execution references (session, turn, attempt); run-level attachment episodes are identity-tracked per service instance with a restart reattachment gate. HTTP adds operator-administered grant endpoints and run-scoped connect/disconnect; a restricted controller invoker proves the controller/operator boundary positively.

**Tech Stack:** Go 1.25.0, SQLite (modernc.org/sqlite v1.59.0 via database/sql), `net/http`, `crypto/rand`.

**Spec:** [`../specs/2026-09-20-ac-004-controller-lease-design.md`](../specs/2026-09-20-ac-004-controller-lease-design.md)

## Global Constraints

- Go 1.25.0 baseline; dual CI verification (`CGO_ENABLED=0` and `-race -count=3`).
- AC-003 architecture is closed: no service-lifetime redesign, no fake adapters in production.
- Migration inputs are immutable and versioned: released v1 DDL/checksum frozen verbatim; fresh databases apply v1 then v2 sequentially; no divergent "latest schema" path.
- Every unadopted run requires explicit adoption before new controller decisions; generation-0 rows are provenance, not grants.
- `runs.controller_lease` stays the current-secret authority; authorization requires an internally consistent adopted, active grant (never a bare string match); empty credentials are rejected before comparison; revoked runs hold `''`.
- Current authority precedes idempotent receipt replay on every controller command path; stale-authority classification precedes `generation_mismatch`; matching receipts precede `expected_version` for current controllers.
- Outcome evidence for accepted executions authorizes by typed execution reference (session, turn, attempt) validated against the persisted release/intent; issuing generation is read from that record.
- One secret per committed grant operation; candidates are never idempotency inputs; recovery — never re-issuance — remedies lost responses.
- Lease secrets never appear in generic receipts, status/readiness/history views, or error strings.
- No heartbeat/time expiry; no new role-management framework; contributor/lease credentials never authenticate transport; disconnect is an explicit command.
- Migration preserves pending prompts, turns, attempt identities, native bindings, and journals without automatic execution.
- Draft PR opens with the first code commit and stays draft through both gates.

## Verification Gates
- **Gate 1 (after Task 5 — first integration gate):** frozen-input migration (repeat-open, concurrent init, interrupted migration, checksum mismatch, newer-version), grant-transition invariants (atomicity, monotonicity, candidate-not-input replay, generated-once recovery, mandatory one-active index, redaction), complete-inventory authorization ordering with legacy denial, accepted-execution outcome persistence (typed attempt reference, dispatch ack after rotation, atomic reconciliation), and attachment semantics (episode identity, delayed disconnect, restart gate) work together with converted fixtures. Full suites: `CGO_ENABLED=0 go test ./...` and `go test -race -count=3 ./...`.
- **Gate 2 (after Task 7 — final gate):** restricted-controller path (positive + escalation), full HTTP acceptance matrix including restart repetition and lost-response recovery, redaction audits, and cross-platform CI.

## Review Focus

1. **Replay after rotation**: a superseded controller retrying its own committed operation receives `lease_superseded`, never the old receipt.
2. **Mid-flight rotation**: handoff/revocation during an active turn never prevents the verified outcome (or dispatch acknowledgement) from persisting under the original execution reference.
3. **Concurrent transitions**: racing handoffs resolve to one winner; the loser is classified by stale authority, not merely generation mismatch.
4. **Lost response recovery**: replay and `RecoverControllerCredential` return the same active-generation secret; no second generation, no candidate leakage.
5. **Restart reattachment gate**: persisted `connected` never authorizes new decisions on a fresh instance; old connect replays don't recreate attachment.

---

### Task 1: Frozen Migration Inputs and Complete v2 Schema

**Files:**
- Create: `internal/storage/schema_v1.go` (frozen released v1 DDL string + checksum constant, moved verbatim from `schema.sql` at `b325bb1`)
- Create: `internal/storage/schema_v2.go` (v2 DDL: `controller_leases` table with `granted_by_op_id`, mandatory partial unique active index, `runs.controller_adopted`, `dispatch_intents.issuing_controller_generation`, generation-0 provenance backfill for existing runs, attempt-generation backfill preserving attempt IDs)
- Modify: `internal/storage/schema.sql` (retired or generated from the versioned inputs), `internal/storage/migrations.go` (sequential versioned application: fresh DBs run v1 then v2; existing v1 DBs verify the frozen v1 checksum then apply v2; `ErrUnsupportedSchemaVersion` retained for newer versions)
- Test: `internal/storage/controller_lease_migration_test.go`

**Interfaces:**
- Consumes: `Store.Migrate`, `schema_migrations` versioning.
- Produces: schema v2 per spec §3.1–§3.3, including the `issuing_controller_generation` column (present from v2's first commit — not deferred to a later task).

**Steps:**

- [ ] **1.1 Write the failing migration test fixture**: construct a verified v1 database by applying the frozen v1 inputs directly (not through the upgraded store), seed queued/running/cancelling turns, a native binding, and journal evidence; then open with the upgraded store. Assert version 2; per existing run a generation-0 row `(harness IS NULL, controller_ref='legacy-v1', status='legacy', lease = v1 lease, granted_by_op_id='legacy-v1')`; `controller_adopted = 0`; accepted intents stamped `issuing_controller_generation = 0` with original attempt IDs unchanged.
- [ ] **1.2 Preservation assertions compare explicit records** — pending prompts (never-released included), active and historical turns, attempt identities, native bindings, journal evidence — not `GetTurnDetails` alone; plus no dispatch/automatic execution occurred.
- [ ] **1.3 Adversarial migration cases**: repeat-open (idempotent), concurrent initialization (single applier), interrupted migration (pre-v2 state reopens and completes), checksum mismatch (frozen-v1 mismatch → error, no partial apply), unsupported newer version.
- [ ] **1.4 Implement** the frozen inputs and sequential migrations; update checksum/version tests.
- [ ] **1.5 Full storage suite; commit** `feat(storage): frozen-input schema v2 with controller lease provenance`. **Open the draft PR** (`feat/ac-004-controller-lease` → `main`, "Closes #4", draft).

### Task 2: Grant Transitions with Request Contracts and Generated-Once Secrets

**Files:**
- Modify: `internal/storage/transitions.go`, `internal/storage/session_store.go` (journal helper adaptations)
- Test: `internal/storage/controller_lease_transitions_test.go`

**Interfaces (one request contract per operation; single signatures — no duplicates):**
- `type OperatorRecovery struct { Reason string; ExpectedGeneration uint64 }` — intent carried on an operator-verified transport; journaled.
- `AdoptController(ctx, opID, runID, harness, controllerRef, bootstrapLease string, recovery *OperatorRecovery, newLeaseCandidate string) (ControllerGrantReceipt, error)` — refuses while an adopted active grant exists (idempotent replay of this op excepted); supersedes a `legacy` row via `bootstrapLease` match or verified recovery; `generation = max+1`; sets `runs.controller_lease`, `controller_adopted=1`, `granted_by_op_id`.
- `HandoffController(ctx, opID, runID, currentLease string, expectedGeneration uint64, harness, controllerRef, newLeaseCandidate string) (ControllerGrantReceipt, error)`
- `RevokeController(ctx, opID, runID, controllerLease string, recovery *OperatorRecovery) (OperationReceipt, error)`
- `RecoverControllerCredential(ctx, opID, runID, sourceOpID string, expectedGeneration uint64) (ControllerGrantReceipt, error)` — resolves `granted_by_op_id`; active generation only.
- `GetControllerRecord(ctx, runID) (ControllerRecord, error)` — no secret field.
- `ControllerGrantReceipt{OperationReceipt; Generation uint64; LeaseSecret string}` — secret only in adopt/handoff/recover responses.
- Errors: `ErrAdoptionExists`, `ErrAdoptionRequired`, `ErrGenerationMismatch`, `ErrLeaseSuperseded`, `ErrRecoveryUnavailable`.
- Fingerprint rule: grant-operation fingerprints cover operator command + target (harness, controller_ref, expected generation/provenance inputs) — never the candidate. Replay returns the original issuance from `granted_by_op_id`. Journal helpers adapted per spec §4.3 (replay authority for grant ops is the operator context, not a recorded caller lease; payloads redacted in outward views).

**Steps:**

- [ ] **2.1 Failing invariant tests**: adopt→gen 1 (from legacy), adopt-after-revoke→gen 2 (never reset); concurrent handoffs on one `expected_generation` → single winner, loser `ErrLeaseSuperseded` (stale authority precedes generation mismatch — asserted explicitly); an otherwise-authorized op with stale `expected_generation` → `ErrGenerationMismatch`; adopt refusal while adopted-active (non-replay); rollback atomicity; revocation clears `runs.controller_lease=''` and empty credentials never pass; replay-with-different-candidate returns the original issuance (no rotation, no conflict, no candidate leakage); recovery returns the same secret while active, `ErrRecoveryUnavailable` after supersession; recovery requires no old lease; `GetControllerRecord` exposes no secret; mandatory one-active index rejects a second active row at the SQL level; all four harness values as metadata.
- [ ] **2.2 Verify failures; implement** the four transitions + record read with the adapted journal semantics.
- [ ] **2.3 Redaction guard test** over outward views.
- [ ] **2.4 `go test -race -count=3 ./internal/storage/`; commit** `feat(storage): controller grant transitions with generated-once secrets`.

### Task 3: Complete Authorization Inventory and Replay Ordering

**Files:**
- Modify: `internal/storage/transitions.go`, `internal/storage/session_store.go`, `internal/storage/store.go` (run creation provenance), `internal/storage/artifact_store.go` (publication ordering)
- Test: `internal/storage/controller_authorization_test.go`

**Interfaces:**
- `authorizeRunController(ctx, tx, runID, presentedLease string, requireAttachment bool) error` — precedence: empty → `ErrAdoptionRequired` (unadopted, including legacy-provenance match) → `ErrLeaseSuperseded` (superseded/revoked match) → adopted/active/consistent grant match → nil; attachment requirement validated at this boundary where the operation class demands it (spec §2.3 table).
- Retrofitted ordering across the complete inventory: controller commands (`QueuePrompt`, `ReplacePendingPrompt`, `DiscardPendingPrompt`, `ReleaseTurn` + `FindCommittedRelease`, `RequestCancel` + `:term`, `ReconcileSession` + receipt path, `RecordDecision`, run-scoped connect/disconnect from Task 5); reclassified operator administration (`CreateRun`, `ArchiveSession`, `SetNativeBinding`, `PublishArtifact`) with corrected ordering, unchanged HTTP exposure; internal observations (`RecordDispatchObservation`) move to execution-reference authority in Task 4; `CreateSession` ordering fixed in `session_store.go` though no HTTP route reaches it (documented).
- `CreateRun` records its lease argument as generation-0 provenance only (no authority); fixtures converted as enforcement lands.

**Steps:**

- [ ] **3.1 Inventory test fixture** enumerating every entry point above with its category, asserting the gate table (spec §2.3) per class — including that reconnect-class operations never require an established attachment.
- [ ] **3.2 Replay decision-table tests** (spec §5.3) across queue/release (`FindCommittedRelease`)/cancel/reconcile/decision: current retry → original receipt; superseded retry of its own committed op → `ErrLeaseSuperseded`, no successful replay; op-ID reuse with different content → `ErrIdempotencyConflict`; historical inspection remains an authorized read.
- [ ] **3.3 Legacy denial tests**: migrated unadopted run — legacy lease (even matching) cannot queue/release/cancel/decide (`ErrAdoptionRequired`); combined acceptance shape: running turn + queued follow-up post-migration → legacy lease denied, adopted controller (via `AdoptController` with `bootstrapLease`) can subsequently release the follow-up (outcome recording of the running turn completes via Task 4's path — full sequence asserted at Gate 1).
- [ ] **3.4 Fixture conversion**: existing service/storage fixtures adopt controllers through the new contract wherever they exercise controller commands; no quiet grants to arbitrary lease strings.
- [ ] **3.5 Full focused suites; commit** `fix(storage): authority-before-replay across complete entry-point inventory`.

### Task 4: Execution-Reference Outcome Persistence

**Files:**
- Modify: `internal/storage/transitions.go`, `internal/service/release.go`, `internal/service/supervisor.go`, `internal/service/commands.go`
- Test: `internal/storage/execution_outcome_authority_test.go`, `internal/service/review_ac004_outcome_authority_test.go`

**Interfaces:**
- `type ExecutionRef struct { SessionID, TurnKey, AttemptID string }` — the original reference captured at release.
- `RecordObservedExecutionOutcome(ctx, opID string, ref ExecutionRef, status council.TurnStatus, rawResult string) (OperationReceipt, error)` — validates ref against the persisted release/dispatch-intent (session, turn, attempt) inside the write transaction; issuing generation read from that record; no controller-lease comparison; terminal-conflict/exact-duplicate/quarantine semantics unchanged; journal carries the issuing generation. Service-internal only.
- `RecordDispatchObservation` authorized by `ExecutionRef` (dispatch acknowledgement after rotation never depends on the issuing controller's lease).
- `ReconcileSession` gains the two-boundary form (spec §6): controller authorizes the probe; verified observation persists under the captured execution + recovery references; terminal-conflict, duplicate, intent resolution, and recovery-episode consumption stay one atomic transition. `handleCancel`'s confirmed commit and `handleReconcile`'s terminal write call the observed-outcome operation internally after adapter evidence.
- Supervisor carries `ExecutionRef` from the release receipt instead of `callerLease` for evidence writes.

**Steps:**

- [ ] **4.1 Failing storage tests**: wrong-attempt rejection; stale recovery generation; release under gen N → handoff → outcome still commits with gen N as journaled issuer; dispatch ack after rotation; duplicate terminal evidence → exact-duplicate receipt; conflicting terminal evidence → `ErrConflictingTerminalOutcome`; reconciliation atomicity (no split writes) under mid-probe handoff.
- [ ] **4.2 Failing service tests** (fake adapter, gated): A releases gated turn → handoff to B mid-flight → A's commands/replays rejected → worker finishes → verified outcome persists under the original execution → follow-up queued until B (explicitly connected) releases it; repeat with revocation (no controller) — accepted work finishes and parks.
- [ ] **4.3 Implement**; audit that no endpoint accepts caller-authored terminal observations or internal-authority flags.
- [ ] **4.4 Race + full suites; commit** `feat(service): execution-reference outcome persistence`.

### Task 5: Attachment Identity and Restart Gate

**Files:**
- Modify: `internal/storage/transitions.go` (run-scoped connect/disconnect + projections), `internal/service/server.go`, `internal/service/commands.go`, `internal/service/coordinator.go`
- Test: `internal/storage/controller_attachment_test.go`, `internal/service/review_ac004_attachment_test.go`

**Interfaces:**
- `ConnectRunController(ctx, opID, runID, lease string, expectedGeneration uint64) (ConnectReceipt, error)` — current grant + matching identity (no replacement identity); new `attachment_id`; run-level `connected=1`; transactional session projections with version bumps; does not require prior attachment. Idempotent replay within the same live episode recovers the existing receipt without a second episode.
- `DisconnectRunController(ctx, opID, runID, lease, attachmentID string, expectedGeneration uint64) (OperationReceipt, error)` — stale attachment ignored (A's delayed disconnect leaves B connected).
- Coordinator: `attachmentState{runID, generation, attachmentID, instanceID}` map — `MarkControllerAttached` only from validated connect transitions in this instance; `ControllerAttached(runID, generation) bool`; handoff/revoke invalidate the exact old attachment and project sessions transactionally; a delayed connect response from an earlier generation cannot mark the replacement attached; readiness/status inspection never mutates attachment.
- HTTP: run-scoped `/controller/connect` + `/controller/disconnect` per spec §8.2 (session-scoped route delegates). Disconnect is an explicit command; HTTP request completion is never disconnection.

**Steps:**

- [ ] **5.1 Failing tests for the case table** (spec §7): lost connect response (same instance) → receipt recovery, no second episode; old connect replay after restart → receipt, no attachment; delayed disconnect A-after-B → B connected; delayed connect from earlier generation → replacement unmarked; handoff/revoke → exact invalidation + projections; restart → persisted `connected` doesn't authorize new decisions until explicit reattach while accepted executions complete.
- [ ] **5.2 Implement**; disconnect stays generation-neutral; reconnect never creates a second controller.
- [ ] **5.3 Gate 1**: full-repo `CGO_ENABLED=0` + `-race -count=3` green with converted fixtures; the Task 1 combined legacy scenario (running turn + queued follow-up) passes end-to-end. Commit `feat(service): identity-tracked controller attachment with restart gate`.

### Task 6: HTTP Administration and Restricted Controller Path

**Files:**
- Create: `internal/service/controller_admin.go`
- Create: `internal/client/controller_bridge.go` (restricted invoker)
- Modify: `internal/service/server.go`
- Test: `internal/service/controller_admin_test.go`, `internal/client/controller_bridge_test.go`

**Interfaces:**
- Routes per spec §8.2 (operator bearer transport, `TrackControl` admission, strict JSON; `ControllerGrantReceipt` secrets only in adopt/handoff/recover successes). Candidates generated in the service (`crypto/rand` 32-byte hex) and passed as non-fingerprinted inputs.
- `client.NewControllerBridge(stateDir, runID, lease)`: holds the operator credential internally (outside controller-supplied arguments and responses); exposes only controller commands bound to the run (queue/release/replace/discard/cancel/connect/disconnect/get-turn/events); rejects administration, credential recovery, and arbitrary routes (`ErrBridgeEscalation`).

**Steps:**

- [ ] **6.1 Failing privilege tests**: lease-as-bearer on every admin route → 401; no-credential → 401; contributor fixture (holds nothing) → 401 before mutation; `operator_recovery` requires operator transport and records reason/op_id/expected_generation; wrong `expected_generation` leaves the replacement untouched.
- [ ] **6.2 Failing restricted-path tests**: positive — adopted controller performs a permitted command through the bridge; escalation — bridge attempts on adopt/handoff/recover/raw-route are denied through the same interface. Direct-HTTP authentication tests remain a separate layer.
- [ ] **6.3 Failing redaction tests**: readiness/status/controller-record/history contain no known-secret substring.
- [ ] **6.4 HTTP replay table** (superseded retry → 403 `lease_superseded`). Implement; race + suites; commit `feat(service,client): grant administration and restricted controller bridge`.

### Task 7: Acceptance Matrix and Final Verification

**Files:**
- Create: `internal/service/review_ac004_acceptance_test.go`
- Modify: PR description (evidence map)

**Steps:**

- [ ] **7.1 Primary story test** (HTTP-level, fake adapter, all four harness identities across subtests): A adopts → **connects** → releases gated turn → explicit disconnect → work completes, follow-up queued, A's new decisions rejected → handoff to B → B **connects explicitly** → B reads records, A's replays `lease_superseded` → only B releases the follow-up. Adoption/handoff grant no connection state automatically. Transitions repeat across a service restart (fresh `Server`, same state dir).
- [ ] **7.2 Matrix tests** from spec §10: concurrency, mid-flight rotation (handoff and revocation), lost-response recovery, stale recovery generation, contributor denial, delayed disconnect, v1-migration inspection, redaction/no-reconnect-on-inspection, restricted-path pair, wrong-attempt/stale-generation/dispatch-ack/duplicate/conflict evidence.
- [ ] **7.3 Evidence discipline**: fixture-only tests named `…_ControlledFixture`; headers state native-conversation adoption is unverified.
- [ ] **7.4 Explicit per-platform verification**: `gofmt -l cmd internal`; `go vet ./...`; `CGO_ENABLED=0 go test ./... -count=1`; `go test -race -count=3 ./...`; `GOOS=windows CGO_ENABLED=0 go vet ./...`; `GOOS=windows CGO_ENABLED=0 go test -c -o /dev/null ./internal/service/`; `GOOS=windows CGO_ENABLED=0 go test -c -o /dev/null ./cmd/council/`; `GOOS=darwin CGO_ENABLED=0 go vet ./...`; `GOOS=darwin CGO_ENABLED=0 go test -c -o /dev/null ./internal/service/`. UNIX service evidence stays distinct from the Windows unsupported-service contract.
- [ ] **7.5 Gate 2**: push, cross-platform CI green, update the draft PR's evidence map, verification boundaries, and limitations; request independent review.

## Self-Review: Invariant → Interface → Assertion

| Invariant | Implementing interface | Observable assertion |
|---|---|---|
| Legacy credentials are provenance, never authority | `authorizeRunController` precedence; `CreateRun` provenance-only | Task 3.3: legacy lease denied on migrated run; adopted controller releases follow-up |
| Current authority precedes replay | `authorizeRunController` called before `checkOrRecordIdempotency`/`FindCommittedRelease` in every inventory path | Task 3.2: superseded retry of own committed op → `ErrLeaseSuperseded`, no receipt |
| Authorization requires adopted/active/consistent grant | authorizer's success contract + one-active unique index | Task 2.1: second active row rejected at SQL level; string-match alone never authorizes |
| Stale classification precedes generation mismatch | transition ordering inside handoff/recovery | Task 2.1: concurrent-handoff loser → `ErrLeaseSuperseded`; stale-but-authorized → `ErrGenerationMismatch` |
| Generated-once secrets; candidates never inputs | fingerprint excludes candidate; `granted_by_op_id` issuance resolution | Task 2.1: replay with different candidate returns original issuance |
| Outcomes authorize by original execution reference | `ExecutionRef{SessionID, TurnKey, AttemptID}`; `RecordObservedExecutionOutcome`; `RecordDispatchObservation` | Task 4.1: wrong attempt rejected; outcome + dispatch ack persist after rotation |
| Reconciliation stays atomic across authority change | two-boundary `ReconcileSession` | Task 4.1: mid-probe handoff → single atomic transition, no split writes |
| Attachment is identity-tracked per instance | `attachmentState` map + `ConnectRunController`/`DisconnectRunController` | Task 5.1 case table incl. connect-replay-after-restart |
| Restart requires explicit reattachment | instance-scoped attachment gate | Task 5.1: persisted `connected` denies new decisions until connect |
| Restricted controller path works and cannot escalate | `client.NewControllerBridge` | Task 6.2: positive permitted command; escalation denied via same interface |
| Secrets never disclosed outward | redacted `ControllerRecord`, view redaction | Tasks 2.3/6.3: no known-secret substring in any response |
| Migration genuinely upgrades frozen v1 | versioned immutable inputs; sequential application | Task 1.3: checksum mismatch, interrupted, concurrent, repeat-open cases |
