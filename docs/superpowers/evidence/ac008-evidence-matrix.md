# AC-008 evidence matrix (Gate 2)

Maps the approved spec (§6 acceptance mapping, §6.1 scenarios) and the
governing invariants to the committed tests and manual evidence that
verify them. Branch: `feat/ac-008-claude-adapter`. Everything below
runs in CI except where explicitly marked **manual evidence**.

## §6.1 acceptance scenarios

| # | Scenario | Evidence (committed tests) |
|---|---|---|
| 1 | Concurrent duplicate dispatch — one native process, shared verdict | `TestAcceptance_Claude_ConcurrentDuplicateDispatch` (service; N concurrent bridge releases → exactly one accepted execution, one fixture invocation); adapter single-flight: `TestClaudeAdapter_PostTransmissionFailureBlocksDurally`, `TestClaudeAdapter_CancelSemantics` |
| 2 | Crash after process start — reconciliation cannot resolve | storage crash boundaries: `TestClaudeState_CrashAfterReservationBeforeStart`, `TestClaudeState_CrashAfterStartBeforeStartedAt`, `TestClaudeState_CrashAfterStdinBeforeAcceptance`, `TestClaudeState_StartFailedRetainedNoCollision`, `TestClaudeState_RedispatchDecisionBeforeConsumption`, `TestClaudeState_RedispatchConsumedCannotAuthorizeThird`, `TestClaudeState_AdvisoryModeBlocksRedispatch`; adapter: `TestClaudeAdapter_CancelSemantics` (terminate ⇒ uncertain, slot/durable block) |
| 3 | Orphan process after daemon death — durable block across restarts | `TestClaudeAdapter_RestartBlockingAndDispositionUnblock` (fresh adapter over the same store is blocked; recorded disposition unblocks; post-disposition turn completes) |
| 4 | Truncated JSONL — torn final line tolerated, no terminal claim | `TestInspectTranscript_TornTailToleratedMidstreamCorruptionRejected`; parser: `TestStreamParser_MalformedLines`, `TestStreamParser_SizeBounds`, `TestStreamParser_InitOnlyCleanEoFEmitsNoTerminal` |
| 5 | Exact-session mismatch — resume fails closed | `TestClaudeAdapter_ResumeSessionInspection` (uncorrelatable user entry, corrupt transcript, symlink, wrong native id/model/template); `TestClaudeAdapter_FreezesMatchingAttestation` (correlation through real dispatch) |
| 6 | Approval-required behavior — bounded run, no hang | `TestAcceptance_Claude_ApprovalRequiredBoundedRun` (denial event class, FAILED terminal via error_max_turns, bounded worker); `TestStreamParser_DenialClassification`; `TestClaudeAdapter_FailedTerminalMapping` |
| 7 | Toolkit/init mismatch — process terminated, Uncertain | `TestStreamParser_InitManifestChecks`, `TestStreamParser_MissingInitPoisons`, `TestStreamParser_SessionCorrelation` (poison ⇒ no verified terminal ⇒ uncertain per §3.9); durable uncertain blocking: `TestClaudeState_UnresolvedAttemptsReported` |
| 8 | Sibling-history denial / absence — advisory default | `TestTranscriptPath_DerivationGolden` + `TestTranscriptPath_RejectsMalformedNativeID` (only the bound session's path is derived); `TestInspectTranscript_*` containment family; `TestClaudeState_AdvisoryModeBlocksRedispatch`; **manual**: `scripts/ac008-integration-evidence.sh` §3 denial probes |
| 9 | Omitted tool cannot execute — deny complement via --disallowedTools | `TestClaudeTurnLaunch_VerifiesPinnedUniverse` (universe re-verification, complement recomputed + compared), `TestClaudeTurnLaunch_FirstTurnSessionIDContract` (exact argument shape); **live denial evidence: PENDING operator run (manual script §3 framing; enforcement claim stays evidence-pending per spec)** |
| 10 | Concurrent creation failure sharing | `TestClaudeAdapter_ConcurrentCreationFailureShared` (materialization counter: exactly one attempt, same typed error INSTANCE for all waiters, independent retry); `TestClaudeAdapter_CreateSessionReservation` (shared identity, typed mismatches) |

## Governing invariants

| Invariant | Evidence |
|---|---|
| §3.3 CreateSession: reservation, no adapter persistence, fail-closed mismatches, reserved projects/ | `TestClaudeAdapter_CreateSessionReservation`, `TestClaudeAdapter_CreateSessionMatchesPersistedBinding`, `TestClaudeAdapter_TemplateMayNotReserveProjectsDir`, `TestClaudeAdapter_ConcurrentCreationFailureShared` |
| §3.4 ResumeSession: validated local inspection | `TestClaudeAdapter_ResumeSessionInspection` (steps 1–2 + transcript trust incl. 0600 mode, ownership, parent-symlink, size bounds, prompt-digest correlation) |
| §3.5 durable dispatch correlation: restart blocking, disposition unblocking, start_failed ⇒ missing | `TestClaudeAdapter_RestartBlockingAndDispositionUnblock`, `TestClaudeAdapter_PostTransmissionFailureBlocksDurally`, `TestClaudeState_StartFailedClassifiesMissing`, `TestClaudeState_UnresolvedAttemptsReported` |
| §3.6 transcript trust model | `TestInspectTranscript_*` family, `TestTranscriptPath_*`, `TestClaudeAdapter_MaterializationRequiresTranscriptObservation`, cprot-v1 golden/normalization/rejection tests (`TestProtectionAttestation_*`), attestation journal: `TestServiceAttestation_OperatorAuthorityAndIdempotency` (operator credential, idempotency, conflict, duplicate), `TestServiceAttestation_AttemptFreezing` |
| §3.7 launch shape + probes | launch-source suite (`TestClaudeTurnLaunch_*`), stdin boundary (`TestStdinWriter_*`), probe: `TestClaudeAdapter_ProbeContractE2E`, `TestClaudeAdapter_ProbeDetectsHelpDrift`, `TestClaudeAdapter_ProbeRejectsDecoyFlagsAndMissingChoices`, `TestNewProductionClaudeAdapter_FailClosed`, `TestOperatorProbeLaunchTemplate_Rejections` |
| §3.9 stream/verdict semantics | `TestStreamParser_*` full §3.9 table; terminal mapping: `TestClaudeAdapter_FailedTerminalMapping`, `TestClaudeState_TerminalFailedStatusMapping`, `TestClaudeState_TerminalPersistenceExactlyOnce` |
| §3.10 reconciliation four-state | `TestClaudeAdapter_ReconcileActiveAndUnknownStates`, `TestClaudeAdapter_ReconcileTerminalAndMissingStates` |
| §3.11 durable state schema | `TestClaudeState_SchemaVersionIsExactly4` + full `TestClaudeState_*` transition suite; launch-state evidence: `TestClaudeState_LaunchStatesInReservationOrder` |
| Production service wiring (fail-closed) | `TestServiceWiring_ClaudeConfigurationFailClosed`, `TestServiceWiring_ClaudeConstructionSucceeds`, config-base/scratch validation (`claude_config_base_test.go`, scratch containment regressions) |
| Bridge lifecycle (acceptance story) | `TestAcceptance_Claude_BridgeLifecycle` (adopt-once → connect → CreateSession + §3.3 persistence → queue → release → gated execution → collect → disconnect → reconnect → --resume follow-up → durable outcomes; resume passes local inspection with transcript correlation end-to-end) |
| Production protection freezing | `TestClaudeAdapter_FreezesMatchingAttestation` (advisory before recording; protected with frozen id after; lookup failure rejects) |

## Manual evidence (not CI)

`scripts/ac008-integration-evidence.sh` — sanitized, operator-gated,
CI-excluded:

1. version + `--help` contract capture with the same exact-flag and
   complete-choice-set assertions as the probe (§2.1 verified sets);
2. minimal live step, only when the operator supplies native session
   id, model, workdir, and prompt — the script never selects them;
3. §3.6 transcript-path denial probe suite (Read / Glob / Grep /
   Bash-absolute / MCP / plugin classes against a sibling transcript):
   every executed record must be DENIED for a valid cprot-v1
   attestation; any NOT-DENIED record keeps the transcript advisory.

## Explicitly unverified capabilities (fail-closed today)

- `mungeCWD` transcript-path derivation matches the recorded native
  rule but is **fixture-verified only** until the manual capture is
  compared against a real installation.
- Help-contract parsing is line-granular: a real CLI that wraps a
  choices segment across lines fails the probe (fail-closed) rather
  than mis-parsing.
- Real-CLI transcript file mode (0600 enforcement) is validated against
  the fixture; the operator capture confirms the native umask behavior.
- Scenario 9's live omitted-tool denial is **evidence-pending**: the
  deny complement is verified at launch, but the structured native
  denial has only been recorded for fixture-class text so far.
- Native authentication is never read by Council; the probe reports
  auth state as unknown by design.
