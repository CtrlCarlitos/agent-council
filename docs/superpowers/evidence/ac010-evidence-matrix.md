# AC-010 evidence matrix (Gate 2)

This matrix maps the approved spec's acceptance mapping (§6), its concrete
scenarios (§6.1, as amended by the binding §14 deltas), and the plan's
Task 8 self-review table (Invariant → Interface → Assertion) to committed
tests. Spec: `docs/superpowers/specs/2026-09-24-ac-010-agy-adapter-design.md`.
Plan: `docs/superpowers/plans/2026-09-24-ac-010-agy-adapter.md`. Branch:
`feat/ac-010-agy-adapter`. Issue: #10.

Everything in the "Committed tests" columns runs in CI (`go test ./...`)
against the compiled `agytest` fixture `agy`. On Linux the fixture runs
through the real sealed, ptrace-verified launch. Test names are given as
`file:TestName`. Paths are relative to `internal/`.

The "Unverified live" column lists the parts that only the operator's
`scripts/ac010-integration-evidence.sh` can establish. Stage A is
provider-free and runs before any freeze. Stages B and C run after
Gate 2. None of those stages has been run for this PR: every entry in that
column is **open**.

## §6.1 concrete scenarios

| # | Scenario (§6.1; §14 deltas applied) | Committed tests (acceptance → supporting) | Unverified live |
|---|---|---|---|
| 1 | Concurrent duplicate dispatch: one process, shared verdict | `service/acceptance_agy_test.go:TestAcceptance_Agy_S01_ConcurrentDuplicateDispatch` (4 concurrent releases over HTTP → 1 accepted, 1 turn process, 1 stdin line, identical collected verdicts); `adapter/agy/dispatch_test.go:TestAgyDispatch_SingleFlightPerConversation`; `adapter/agy/adapter_test.go:TestAgyAdapter_CreateSessionConcurrentDuplicatesShareOneReservation`; `storage/agy_state_test.go:TestAgyState_LaunchCountCheckRejectsTwo` | — |
| 2 | Crash after the stdin write, result lost: Uncertain; block persists across restart; disposition required | `service/acceptance_agy_test.go:TestAcceptance_Agy_S02_CrashAfterStdinWriteResultLost` (prompt written, no result: Uncertain; the service records no terminal; after a restart with a fresh adapter, Reconcile is Uncertain, `HasAgyUnresolvedAttempts` holds, the next release is refused, and no new process starts); `adapter/agy/reconcile_test.go:TestAgyCrashGap_FirstByteBeforeUserInput`, `TestAgyCrashGap_AcceptedBeforeTerminal`, `TestAgyReconcile_Matrix`; `storage/agy_state_test.go:TestAgyState_CrashAfterFirstByteBeforeUserInput` | A real service-process crash (simulated by cancelling workers and closing the service). **Gap §14.17:** no controller-disposition operation is shipped. |
| 3 | Process death mid-turn: Uncertain, BLOCKS; only after a disposition does the next turn start as a new process on the same conversation after `init` equality | `service/acceptance_agy_test.go:TestAcceptance_Agy_S03_ProcessDeathBlocksUntilDisposition` (accepted, then exit without result: Uncertain; the next dispatch is rejected "unresolved attempt" with no process; after the disposition, the next turn is launch #3 with `--conversation <id>` and the prompt is written after `init`); `adapter/agy/dispatch_test.go:TestAgyDispatch_ExitWithoutResultUncertain` | **Gap §14.17:** the test writes the disposition to the row directly, because no operator surface exists. |
| 4 | Controller disconnect: the turn continues; the observer re-attaches | `service/acceptance_agy_test.go:TestAcceptance_Agy_S04_ControllerDisconnectObserverReattaches` (SSE observer and controller both disconnect while the gated child runs; the child is alive per the `/proc` cwd scan and launch state `started`; the turn completes; a re-attached SSE observer gets the durable terminal); `…:TestAcceptance_Agy_Lifecycle` step 5; `adapter/agy/dispatch_test.go:TestAgyDispatch_ObserverDetachDoesNotCancelTurn`; `service/agy_creation_transitions_test.go:TestServiceAgySession_CancelledRequestStillRecordsTheCreatedConversation` (§14.14) | — |
| 5 | Exact identity: absent/malformed id ⇒ `ErrConversationDrift`, prompt never written, orphan recorded; non-UUID never transmitted | `service/acceptance_agy_test.go:TestAcceptance_Agy_S05_ExactIdentity` (the 1.2.9 silent fallback to a new UUID, and a malformed id: the turn fails "agy conversation drift"; 0 stdin lines; orphan id durable; attempt `missing`; only the bound UUID is ever passed as `--conversation`; a non-UUID binding is refused by the schema CHECK); `adapter/agy/dispatch_test.go:TestAgyDispatch_FallbackIDDriftNeverWritesPrompt`; `adapter/agy/durability_test.go:TestAgyDispatch_OrphanConversationDurableAcrossRestart`; creation side: `…:TestAcceptance_Agy_Lifecycle` step 10, `service/review_ac010_wiring_test.go:TestServiceAgySession_UncertainCreationEpisodeAndResolution`, `adapter/agy/adapter_test.go:TestAgyAdapter_CreateSessionNonUUIDConversationUncertain` | Stage B b2 (live resume, init id equality) |
| 6 | Permission-requiring tool auto-denied natively; `tool_denied`; `verification_incomplete`; exit 0 does not verify | `service/acceptance_agy_test.go:TestAcceptance_Agy_S06_PermissionToolAutoDeniedIncomplete` (a live SSE `tool_denied` progress event; terminal `completed`; every required tool ran, yet verification is incomplete; launch `exit_code` = 0; raw evidence has `verification_incomplete:true`); `…:TestAcceptance_Agy_Lifecycle` step 6; `adapter/agy/dispatch_test.go:TestAgyDispatch_DeniedToolEmitsToolDeniedAndIncomplete`, `TestAgyDispatch_DenialClassesRecorded`, `TestAgyDispatch_RequiredToolSkippedIncomplete` | Stage B b3 (live `denied_actions` under `request-review`, re-observed on the frozen build) |
| 7 | Config drift: `init.permission_mode`/`model`/`cwd` mismatch fails before transmission | `service/acceptance_agy_test.go:TestAcceptance_Agy_S07_ConfigDriftFailsBeforeTransmission` (`permission_mode` through the bridge); `adapter/agy/dispatch_test.go:TestAgyDispatch_ProfileAndToolDriftRejectedPreWrite`; `adapter/agy/durability_test.go:TestAgyDispatch_EmptyInitFieldsAreDriftPreWrite` (model/cwd/empty fields); `service/agy_creation_transitions_test.go:TestServiceAgySession_CreationDriftAnnotatesTheServiceMarker` | — |
| 8 | Tool inventory drift: `init.tools` ≠ frozen ⇒ pre-transmission failure | `service/acceptance_agy_test.go:TestAcceptance_Agy_S08_ToolInventoryDriftFailsBeforeTransmission`; `adapter/agy/dispatch_test.go:TestAgyDispatch_ProfileAndToolDriftRejectedPreWrite`; `adapter/agy/profile_test.go:TestValidateAgyHarness_InitEvidenceToolsMismatchRejected` | Stage A A.7 (does live `init.tools` equal the frozen set?) |
| 9 | Print-timeout marker: Uncertain even with `result` present | `service/acceptance_agy_test.go:TestAcceptance_Agy_S09_PrintTimeoutMarkerUncertain` (marker + ERROR result: attempt Uncertain; the classification reason is "stderr print-timeout marker"; no service terminal; the conversation is blocked); `adapter/agy/dispatch_test.go:TestAgyDispatch_PrintTimeoutMarkerUncertain` | Stage B b5 (the marker on a healthy turn; only the 429-retry case has been observed) |
| 10 | Binary drift: digest mismatch ⇒ no process started | `service/acceptance_agy_test.go:TestAcceptance_Agy_S10_BinaryDriftStartsNoProcess` (the pinned path holds different bytes, so production construction is refused with `ErrSealedImageMismatch` even when a covering attestation exists; no construction child); `adapter/execpolicy/sealed_linux_test.go:TestNewSealedImage_DigestMismatchRefused`, `TestBuildSealedCmd_DigestMismatchRefusedAtLaunch`; `adapter/agy/eligibility_test.go:TestEligibility_SealedImageRequiredAndPinned`; `adapter/agy/durability_test.go:TestAgyValidateLaunch_TiedToAllocationImageAndHome` | Stage A A.3/A.4 (agy's behavior when exec'd from a sealed memfd: install-dir discovery, sidecars, updater) |
| 11 | Unattested authenticated dispatch: `ErrProductionEligibilityMissing` before any child | `service/acceptance_agy_test.go:TestAcceptance_Agy_S11_UnattestedDispatchRefusedBeforeAnyChild` (without a row the production-configured service starts WITHOUT the agy adapter in the §14.18 `awaiting_attestation` state; birth, release, reconcile and queue-time validation refuse with `ErrNotEligible` wrapping `ErrProductionEligibilityMissing`; no child starts; if the row is purged after construction, the next released turn fails "agy production eligibility missing" with no attempt row and no turn process); `service/review_ac010_bootstrap_test.go:TestServiceBootstrap_AgyAwaitingAttestationRecordRestart` (birth, `required_tools` queueing and release refuse typed with no child); `adapter/agy/wiring_test.go:TestNewProductionAgyAdapter_EligibilityReCheckedPerDispatch`; `adapter/agy/adapter_test.go:TestAgyAdapter_ProductionEligibilityMissingPreChild`; `adapter/agy/production_guard_test.go:TestProductionGuard_NilAttestationFailsClosedPreChild` | Stage C (the first real attestation, recorded on the awaiting server). |
| 12 | Ignored input event: stderr marker ⇒ Uncertain, never assumed sent | `service/acceptance_agy_test.go:TestAcceptance_Agy_S12_IgnoredInputEventUncertain` (the envelope is rewritten on the wire; the child drops it; the attempt is Uncertain with reason "stderr ignored-input marker"; no terminal; the conversation is blocked); `adapter/agy/dispatch_test.go:TestAgyDispatch_IgnoredInputMarkerUncertain`; `adapter/agy/agyfixture_protocol_test.go:TestAgyFixture_EnvelopeValidation_UnsupportedEventIgnored` | — |

## §6 acceptance mapping (issue #10 criteria)

| Criterion | Committed tests | Unverified live |
|---|---|---|
| Probe the installed CLI and supported output fields with sanitized fixtures | `adapter/agy/agyfixture_protocol_test.go` (full directive matrix, verbatim 1.2.9 envelope texts); `adapter/agy/stream_test.go`; `adapter/agy/agytest/guard_test.go:TestAgyTest_FixtureSourceByteIdenticalToFakeChild`; committed `docs/superpowers/evidence/ac010-agy-{init,plugins,tool-coverage}-1.2.9.json` + `SHA256SUMS-ac010` | All of Stage A. §14.12: the `models` column layout is unrecorded. |
| Start independently and resume a specified conversation | `service/acceptance_agy_test.go:TestAcceptance_Agy_Lifecycle` (provider-free creation with empty stdin, no `--conversation`, no `models` gate; turns 1, 2, and the post-restart turn 3 each run as a new process with `--conversation <bound id>`); `service/review_ac010_wiring_test.go:TestServiceAgySession_BirthDerivesModelWorkspaceAndDigest`; `adapter/agy/adapter_test.go:TestAgyAdapter_CreateSessionBindsInitConversationID`; `adapter/agy/authgate_test.go:TestAuthGate_CreationIsNotGated` | Stage B b1/b2 |
| Verify expected skills/tools/plugins and enabled guardrails | `adapter/agy/toolkit_test.go` (all); `adapter/agy/wiring_test.go:TestNewProductionAgyAdapter_ToolkitDriftAtConstruction`; `adapter/agy/coverage_test.go` (all); `adapter/agy/profile_test.go:TestValidateAgyHarness_UnmodifiedInstallHasUncoveredTools`, `TestValidateAgyHarness_NonEmptyMCPInventoryRejected`; `storage/canonical_profile_v4_test.go:TestCanonicalProfileV4_NonEmptyInventoriesRejectedAtFreeze` | Stage A A.6/A.8/A.9 (live plugin, hooks, and skills captures; `mcp list`). Hook execution and plugin skill loading are not claimed (§7). |
| Required skipped/denied tools keep verification incomplete regardless of exit code | `service/acceptance_agy_test.go:TestAcceptance_Agy_Lifecycle` (queue-time `required_tools` → missing + denial → incomplete), `TestAcceptance_Agy_S06_…`; `service/review_ac010_wiring_test.go:TestServiceQueue_AgyRequiredToolsValidatedAtQueueTime`; `adapter/agy/verification_test.go`; `adapter/agy/dispatch_test.go:TestAgyDispatch_RequiredToolSkippedIncomplete`, `TestAgyDispatch_DenialClassesRecorded`; `storage/agy_state_test.go:TestAgyState_ReplacePendingPromptKeepsImmutableRequiredTools` | Stage B b3 and b7 (the `denied_tools` rule's denial shape) |
| Bounded cancellation, unknown outcomes, client-close recovery, no fallback harness | `adapter/agy/cancel_test.go` (all); `adapter/agy/durability_test.go:TestAgyCancel_ForcedKillReapsSealedDescendants`, `TestAgyDispatch_ExternalInterruptIsFailedNotCancelled`; `adapter/execpolicy/sealed_group_linux_test.go:TestSealedLaunch_NormalExitKillsGroupDescendants`, `TestSealedLaunch_GracefulTerminateKillsGroupDescendants`, `TestSealedLaunch_ForcedTerminateKillsGroupDescendants` (a sealed launch's same-group descendant — one that ignores SIGTERM — outlives neither a normal exit, a graceful Terminate, nor a forced one); §6.1 rows 2/3/9/12 above; `…:TestAcceptance_Agy_Lifecycle` steps 5 and 10–12 (disconnect is not cancellation; restart with an open creation episode → resolution); `adapter/council_boundary_test.go:TestCouncilBoundary_ProductionRegistryRejectsFake` | Stage B b4 (live SIGINT on the frozen build). A descendant that leaves its process group (setsid/setpgid) is not reaped (§14.6). |

## Task 8 self-review table (Invariant → Interface → Assertion → tests)

| Invariant | Interface | Assertion | Committed tests | Unverified live |
|---|---|---|---|---|
| Migration v7 first; crash-safe transitions; `launch_count ≤ 1` | `storage/agy_state.go` | schema version 7; five crash gaps; second reservation refused | `storage/migration_v7_test.go:TestAC010_MigrationV7_SchemaVersionIsExactly7`, `TestAC010_MigrationV7_UpgradesV6Database`; `storage/agy_state_test.go:TestAgyState_CrashBeforeLaunchReservation`, `TestAgyState_CrashAfterReservationBeforeStart`, `TestAgyState_CrashAfterStartBeforeFirstByte`, `TestAgyState_CrashAfterFirstByteBeforeUserInput`, `TestAgyState_CrashAfterTerminalWrite`, `TestAgyState_LaunchCountCheckRejectsTwo`; `storage/agy_orphan_and_reservation_test.go:TestAgyState_InsertAndReserveIsAtomic`; `adapter/agy/reconcile_test.go:TestAgyCrashGap_*` | Durability across a real process crash. Every crash in the tests is simulated. |
| cprof-v4 canonical; inventories gated to empty; captures canonical and contained | `storage/canonical_profile.go`, `adapter/evidence.ReadFile`, `adapter/agy.ValidateAgyHarness` | golden vector; enum/inventory rejections; containment/strict-decode matrix; compatibility matrix across four adapters | `storage/canonical_profile_v4_test.go:TestCanonicalProfileV4_GoldenDigestVector` (+18 siblings); `adapter/evidence/evidence_test.go:TestReadFile_ContainmentDigestAndSymlinkMatrix`, `TestDecodeStrictObject_Matrix`; `adapter/agy/profile_test.go` (all); compatibility: `adapter/claude/launchsource_v4_test.go:TestClaudeTurnLaunch_AcceptsCprofV4Profile`, `adapter/codex/profile_v4_test.go:TestValidateCodexHarness_*CprofV4*`, `adapter/opencode/eligibility_v4_test.go:TestOpenCodeEligibility_AcceptsCprofV4Profile`, `adapter/execpolicy/executor_v4_test.go:TestPolicyExecutor_AcceptsCprofV4AtValidityGate` | Stage A freeze inputs: `a-plugins.canonical.json`, `a-init-evidence.candidate.json`, `a-digests.txt` |
| Sealed image; verification inside `Start`; no stopped child escapes | `execpolicy.SealedImage`, `startSealed` | seals, digest, mismatch, fast/slow child, tracer, EXITKILL, CLOEXEC; non-Linux refusal | `adapter/execpolicy/sealed_linux_test.go` (all, e.g. `TestSealedImage_SealsPreventWriteAndTruncate`, `TestStart_SealedImage_FastChildVerifiedAndReleased`, `TestStart_SealedImage_SlowChildVerifiedBeforeOutput`, `TestStart_SealedImage_TracerFailure`, `TestStart_SealedImage_NoLeakedMemfdDescriptor`); `adapter/execpolicy/sealed_other_test.go` (all); `adapter/execpolicy/executor_agy_test.go:TestPolicyExecutor_AgyLaunch_ProductionRefusesUnsealed`; `service/review_ac010_wiring_test.go:TestServiceWiring_AgyConstructionRefusesNonLinuxPlatform`; `…:TestAcceptance_Agy_S10_…` | Stage A A.3/A.4: the real agy exec'd from a sealed memfd. macOS/Windows are refusal-only (§3.12) and were checked only by `go vet`. |
| Prompt only after `init` equality; first byte is the boundary; exit code never classifies | `AgyAdapter.Dispatch` | fallback id ⇒ empty fixture input; crash gaps; denied ⇒ incomplete with exit 0 | `…:TestAcceptance_Agy_S05_ExactIdentity`, `…_S06_…`, `…_S07_…`, `…_S08_…`; `adapter/agy/dispatch_test.go:TestAgyDispatch_FallbackIDDriftNeverWritesPrompt`, `TestAgyDispatch_AcceptedOnUserInputAndCompleted`, `TestLaunchArgv_ExactGrammar`; `adapter/agy/reconcile_test.go:TestAgyCrashGap_StartedBeforeFirstByte` | Stage B b1 (the live `user_input` DONE step as the §14.5 acceptance) |
| Required tools ⊆ expected, immutable, verified with the denial map | `Server.handleQueuePrompt`, `agy.ComputeVerification` | queue-time 400; skipped ⇒ incomplete; ambiguous/unattributed/unmapped classes | `service/review_ac010_wiring_test.go:TestServiceQueue_AgyRequiredToolsValidatedAtQueueTime`; `service/agy_creation_transitions_test.go:TestServiceAgyDispatch_MissingIntentRefusedBeforeReservation`; `adapter/agy/dispatch_test.go:TestAgyDispatch_RequiredToolsSourceErrorRejectsBeforeReservation`, `TestAgyDispatch_DenialClassesRecorded`; `adapter/agy/verification_test.go`; `storage/agy_state_test.go:TestAgyState_ReplacePendingPromptKeepsImmutableRequiredTools`; `…:TestAcceptance_Agy_Lifecycle` (queue → attempt `required_tools` → missing) | Stage B b3/b7 |
| Auth gate provider-free; `none` rows fixture-only | `adapter/agy/authgate.go`, capability checker | not signed in ⇒ `ErrAgyAuthRequired`; network failure ⇒ inconclusive; production `none` refused | `adapter/agy/authgate_test.go` (all); `adapter/agy/adapter_test.go:TestAgyAdapter_LaunchMatrix`; `…:TestAcceptance_Agy_Lifecycle` (the `models` gate runs before the first turn child and not at creation) | Stage A A.5: the real `models` layout (§14.12). The gate fails closed on any layout other than one id per row. |
| Coverage derived from `expected_tools` ∩ map; uncovered ⇒ rejected | `adapter/agy/coverage.go`, `ValidateAgyHarness` | subagent/browser tools ⇒ freeze refused; an attestation missing a mapped tool ⇒ refused at record and lookup | `adapter/agy/coverage_test.go` (all); `adapter/agy/eligibility_test.go:TestAttestationLookup_UncoveredRowIneligible`; `service/review_ac010_wiring_test.go:TestServiceAttestation_AgyAuthorityIdempotencyCoverage` ("missing mapped tool", "extra record"); `…:TestAcceptance_Agy_ProductionConstructionUnlockedByServiceRecordedAttestation` | Stage C probe outcomes. |
| Toolkit configured state re-derived at every launch | `adapter/agy/toolkit.go` | plugin/skills/hooks drift; disabled marker; canonical-bytes mismatch | `adapter/agy/toolkit_test.go` (all); `adapter/agy/wiring_test.go:TestNewProductionAgyAdapter_ToolkitDriftAtConstruction` | Stage A A.6/A.8 (the live captures). Hook execution is not claimed (§7). |
| Bounded cancellation; Uncertain until disposition; no fallback harness | `Cancel`, `Reconcile`, episodes | SIGINT ⇒ interrupted ⇒ confirmed; grace ⇒ Uncertain; blocked across restart until resolution | `adapter/agy/cancel_test.go` (all); `…:TestAcceptance_Agy_S02_…`, `…_S03_…`, `…_Lifecycle` (creation episode → restart → blocked → resolved → birth); `service/agy_creation_transitions_test.go:TestServiceAgySession_CrashBetweenCreateAndBindIsBlockedByTheMarker` (§14.13); `service/agy_live_marker_test.go:TestServiceAgySession_ResolveLiveCreationMarkerRefusedTyped` (a live creation's marker cannot be resolved: typed `ErrCreationInProgress`), `TestServiceAgySession_OrphanFallbackOpensNewEpisodeWhenMarkerGone` (a created id whose marker is gone opens a NEW episode; the next birth is blocked); `storage/agy_uncertainty_test.go` (all); `storage/agy_inflight_test.go` (all) | Stage B b4. **Gap §14.17:** no turn-attempt disposition surface. |

## Production path through the attestation bootstrap (spec §14.18)

`service/acceptance_agy_test.go:TestAcceptance_Agy_ProductionConstructionUnlockedByServiceRecordedAttestation`
(Linux only) drives the shipped bootstrap from configuration alone
(`NewServerWithAdapter(…, nil)`):

1. With no covering row, the production-configured service starts
   WITHOUT the agy adapter in the typed `awaiting_attestation` state
   (`Server.AgyStatus()`, the `agy` block of `GET /v1/status`, one
   warning log line). No construction child runs.
2. The covering cprot-v2 row is recorded through
   `Server.RecordAgyProbeAttestation` ON THAT AWAITING SERVER, using the
   operator token and actor and the run-derived tuple. Recording needs
   only the store, the operator credential and the configured evidence
   root, never a wired adapter.
3. A restart over the same store and state directory runs
   `agy.NewProductionAgyAdapter`; the construction `plugin list` runs and
   the status is `wired`. There is no hot reload.
4. A birth and a queued turn then run through the production adapter:
   - the attempt identity comes from the journaled dispatch intent;
   - the queued `required_tools` verify;
   - every sealed launch runs with `HOME` = the parent of `expected_home`
     (§14.2).

`service/review_ac010_bootstrap_test.go` covers the state itself: the
awaiting state on `AgyStatus()` and `GET /v1/status`; `CreateAgySession`,
queue-time `required_tools` and release refused with the wrapped
`ErrNotEligible` (HTTP 503 `agy_not_eligible`) and no child; recording on
the awaiting server; the restart constructing the production adapter;
and a non-attestation construction error (a wrong `AgyHomeDir`) still
failing the server.

Limit: `RecordAgyProbeAttestation` is a Go method on the running
`Server`; like the codex and claude attestation operations it has no HTTP
route or CLI command in this branch. The sequence above is therefore
proven for **in-process callers only**: an operator running the shipped
binary cannot record the first row, so this PR cannot enable production
agy on its own. Operator enablement needs a follow-up surface, filed
alongside §14.17.

## Unresolved gaps (recorded in spec §14; not closed by this PR)

- **§14.7:** the §3.8 conversation-file diagnostics are not implemented.
- **§14.17:** no controller operation records a disposition for an
  Uncertain turn attempt. The block is durable and specified, but it can
  only be cleared by editing storage by hand. AC-008 and AC-009 have the
  same gap.

- **§14.18 operator surface:** the attestation bootstrap is closed for
  **in-process callers only**. A server configured with `AgyBinaryPath`
  but without a covering row starts in the typed `awaiting_attestation`
  state (status surface + log; no agy child). Birth, release (dispatch),
  reconcile and queue-time `required_tools` validation refuse typed with
  `ErrNotEligible`; the other operations are not agy-gated and, with no
  adapter wired, act on durable state only or report the harness
  unavailable. The first row can be recorded on that server through
  `RecordAgyProbeAttestation` (no wired adapter needed) and a restart
  constructs the adapter — but only from Go code in the service process:
  there is no HTTP route or CLI command, so operator enablement needs a
  follow-up surface (filed alongside §14.17), and this PR cannot enable
  production agy on its own. See "Production path" above.

## What only the operator's stages can establish (all open)

**Stage A** (provider-free, before any freeze):
- the real `--version` and binary digest;
- the `models` column layout (§14.12);
- the `mcp list` inventory, which must be empty to freeze;
- the canonical `plugin list` digest;
- `init.tools` versus the frozen set;
- the hooks canonical digest and skill names;
- the **sealed-vs-path comparison**: agy exec'd from a sealed memfd, with
  install-dir discovery, sidecars, and the updater.

**Stage B** (≤ 8 turns):
- the live `user_input` acceptance, resume, and denial shape under
  `request-review`;
- SIGINT → `interrupted`;
- print-timeout on a healthy turn;
- `--mode plan` edit blocking;
- the `denied_tools` rule's denial shape.

**Stage C:**
- structured denial of every `sibling_read_path` tool and every
  `own_mutation_path` tool in the frozen inventory;
- the first cprot-v2 attestation, recorded through
  `RecordAgyProbeAttestation` on the awaiting server, then a restart —
  which needs the follow-up operator surface (§14.18 is closed for
  in-process callers only).
- Write/append self-mutation is judged on the structured `denied_actions`
  marker alone, because the CLI rewrites its own conversation file on
  every turn.

**Not claimed by any stage (§7):**
- hook execution and plugin skill loading;
- the subagent and shared-browser boundaries;
- workspace-trust gating;
- content-block user messages;
- the `AGY_ERROR` exit-3 path;
- an auto-updater off switch;
- `GEMINI_API_KEY` mode;
- behavior on Windows or macOS.
