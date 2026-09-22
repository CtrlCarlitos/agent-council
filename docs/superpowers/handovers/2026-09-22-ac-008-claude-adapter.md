# AC-008 Claude adapter handover

## Repository state

- Branch: `feat/ac-008-claude-adapter`
- HEAD: `179fbde`
- Draft PR: none yet (Tasks 6–7 must wait)
- Spec (v8, approved): `docs/superpowers/specs/2026-09-22-ac-008-claude-adapter-design.md`
- Plan: `docs/superpowers/plans/2026-09-22-ac-008-claude-adapter.md`
- Issue: #8

## What IS implemented and tested (do not redo)

### Storage layer (complete, race-tested)

- Schema v4 migration: `claude_session_bindings`, `claude_turn_attempts`,
  `claude_attempt_launches`, `claude_protection_attestations` tables.
- `claude_state.go` transitions: `InsertClaudeSessionBinding`,
  `GetClaudeSessionBinding`, `MarkClaudeSessionMaterialized`,
  `InsertClaudeTurnAttempt`, `ReserveClaudeLaunch` (with
  protected-mode gate, attestation-table JOIN, positive-absence
  check), `RecordClaudeLaunchState` (atomic start_failed slot
  release), `RecordClaudeStdinTransmitted` (first-write-wins),
  `RecordClaudeAbsenceVerified` (JOINs attestations, checks
  RowsAffected), `SetClaudeAttemptTerminal` (exactly-once guard),
  `SetClaudeAttemptObservedStatus`, `GetClaudeTurnAttempt`,
  `GetLatestClaudeTurnAttempt`, `HasClaudeUnresolvedAttempts`.
- Crash-boundary regressions: all five boundaries (before
  reservation, after reservation before Start, after Start before
  started_at, after stdin before acceptance, redispatch consumption).
- Exact schema-v4 assertion: version must be 4, all four tables exist.

### Parser (complete, tested)

- `stream.go`: full §3.9 state table with session correlation,
  ordering enforcement, exact toolkit sets, model identity check,
  usage/cost retention, denial classification (approval_denied /
  guardrail_denied / tool_disabled), tool_use_id → tool_name
  correlation, EventTerminal gated on verified result.
- `stream_test.go`: every §3.9 row plus session-correlation,
  usage-retention, and tool-name regressions.

### Launch source (complete, tested)

- `launchsource.go`: frozen model (exact native identity), frozen
  turns_bound from the manifest, verified deny complement (universe −
  approved recomputed and compared), exact argument contract,
  evidence-containment checks.
- `launchsource_test.go`: first-turn/resume contract, contributor
  rejection, legacy-profile rejection, universe verification.

### Other complete pieces

- `template_digest.go` + tests (ctmpl-v1 framing, golden vectors)
- `evidence_universe.go` + tests (containment, symlinks, digests)
- `protection_attestation.go` + tests (typed records, numeric enum
  sort, canonical framing, fixed golden vector, RFC3339 UTC)
- `configroot.go` + tests (atomic materialization, concurrent,
  post-rename verification cleanup)
- `stdin.go` + tests (first-byte boundary, partial transmission,
  zero-progress fail-closed)
- Service config base (`claude_config_base.go`): disjoint containment,
  symlink-safe, per-direction checks

## What is NOT implemented (Task 5 remaining)

### Adapter contract — `adapter.go` needs a production rewrite

Current state: skeletal. Dispatch flow exists (reservation → Start →
stdin → parse) but has these P1 gaps:

1. **CreateSession is a shell** — must generate UUID, materialize
   per-session config root, and persist via
   `InsertClaudeSessionBinding`. The test currently does this manually.
2. **Dispatch always uses --resume** — must read the binding's
   `materialized` flag: `--session-id` when false (first turn),
   `--resume` when true.
3. **Post-transmission failures release the slot** — must NOT release
   on ambiguous writes; the native session is blocked until disposition.
4. **First-byte evidence recorded too late** — must persist at the
   write boundary via `RecordClaudeStdinTransmitted`, not after the
   full prompt is written. Storage errors must not be silently dropped.
5. **Failed results become completed** — `SetClaudeAttemptTerminal`
   must accept a completion status parameter; `error_max_turns` maps to
   observed_status='failed'.
6. **Reconcile is not four-state** — live in-memory process ⇒
   ReachableActive; unknown refs without positive pre-start evidence ⇒
   Uncertain (not DefinitivelyMissing); failed terminals ⇒ TurnFailed.
7. **Stderr drained twice concurrently** — drain exactly once, from
   process start.
8. **Observe/Cancel need test coverage** — Observe handles absent
   map entries and Cancel returns CancelUnknown (both implemented);
   add regression tests but do not rewrite.
10. **Terminal-persistence failure releases the slot** — must retain
    the slot (outcome is uncertain, not resolvable).

### Contract tests — `adapter_contract_test.go` needs expansion

Missing test coverage:
- first-vs-resume selection from binding materialization state
- ambiguous-write slot blocking (slot NOT released after post-
  transmission failure)
- failed terminal mapping (error_max_turns → TurnFailed)
- Observe detach and tool-denial event delivery
- Cancel behavior
- Four-state Reconcile (terminal / active / uncertain / missing)
- Real-executor adapter contract with fixture child lifecycle

## Implementation sequence for the next session

1. Rewrite `adapter.go` CreateSession: generate UUIDv4, call
   `MaterializeConfigRoot`, persist via `InsertClaudeSessionBinding`.
2. Rewrite Dispatch: read binding → select --session-id/--resume from
   materialization → persist stdin boundary at write time → start
   process with detached context → run parser → persist terminal →
   release slot only on terminal or pre-acceptance rejection.
3. Fix runTurn: drain stderr once, use out.Completed to select
   TurnCompleted vs TurnFailed, retain slot on non-terminal outcomes.
4. Add regression tests for Observe nil-check and Cancel
   CancelUnknown (already implemented, insufficiently tested).
5. Write adapter contract tests (first-vs-resume, ambiguous-write,
   failed terminal, detach, cancel, reconcile).
6. Write storage crash-boundary reopen tests if not already present.
7. Run full verification suite + race ×3.

## Task 6/7 (wait until Task 5 closes)

- Task 6: service wiring (`NewServerWithAdapter` with Claude config),
  probe template, attestation journal operation
- Task 7: acceptance story through the bridge, integration script,
  evidence matrix, PR

## Scope boundaries

- Never bypass permissions, never use `--bare`, never disable session
  persistence.
- Native auth is the operator's; Council never reads credentials.
- JSONL is advisory by default; protected evidence requires the
  attestation.
- Fixture binaries are test-only; native execution is manual/sanitized.
