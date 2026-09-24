# PR draft: AC-010 Agy (Antigravity CLI) contributor adapter (#10)

> Draft for Gate 2. Do not open, push, or merge without explicit operator
> authorization. Branch `feat/ac-010-agy-adapter` → `main`.

## Summary

This PR adds the Agy (Antigravity CLI) persistent contributor adapter for
issue #10. It follows the approved design,
`docs/superpowers/specs/2026-09-24-ac-010-agy-adapter-design.md`
(v7 plus the binding §14 deltas 1–19). The work is split into eight tasks:

1. Storage migration v7.
2. The cprof-v4 frozen profile, the coverage map, and the evidence
   containment rules.
3. The memfd-sealed, ptrace-verified launch in `execpolicy`.
4. The stream-json protocol, the fixture `agy`, and `agytest`.
5. `AgyAdapter`: process per turn with stdin-only transport, `init`
   equality checked before the first prompt byte, verification of the
   required tools, and cancellation.
6. The eligibility, coverage, auth (`models`), and toolkit gates, plus
   `NewProductionAgyAdapter`.
7. The service wiring:
   - derived session birth with a durable pre-launch creation marker;
   - the run-bound cprot-v2 `RecordAgyProbeAttestation` operation;
   - `ResolveAgySessionCreationUncertainty`;
   - queue-time `required_tools` validation.
8. The acceptance story, the operator evidence script, and the evidence
   matrix.

Evidence matrix: `docs/superpowers/evidence/ac010-evidence-matrix.md`. It
maps every §6 and §6.1 row and every row of the plan's Task 8 self-review
table to committed tests, and has an explicit "unverified live" column.

## Issue #10 acceptance criteria

| Criterion | Where it is proven |
|---|---|
| Probe the installed CLI and its output fields with sanitized fixtures | Fixture protocol suite, committed 1.2.9 captures, `SHA256SUMS-ac010`. A fresh live capture needs Stage A of the operator script. |
| Start independently and resume a specified conversation | `TestAcceptance_Agy_Lifecycle` (provider-free creation, `--conversation <bound id>` on every turn, a post-restart turn) |
| Verify expected skills, tools, plugins, and enabled guardrails | The toolkit, coverage, and profile suites and construction-time drift. Hook execution and plugin skill loading are not claimed (§7). |
| Skipped or denied required tools keep verification incomplete regardless of exit code | `TestAcceptance_Agy_S06_…` (exit 0 plus a denial ⇒ incomplete), the lifecycle's required-tools path, and `TestServiceQueue_AgyRequiredToolsValidatedAtQueueTime` |
| Bounded cancellation, unknown outcomes, client-close recovery, no fallback harness | The cancel suite and §6.1 rows 2, 3, 4, 9, and 12. A client disconnect is not a cancellation (§14.14). The restart test runs with an open creation episode and then resolves it. |

## Risk

- **High-trust surface.** The adapter launches the operator's
  authenticated CLI against the operator's real `~/.gemini`. The child
  `HOME` is the parent of `expected_home` (§14.2); a relocated home would
  be unauthenticated. The controls:
  - the binary is pinned by version and digest and runs as a sealed memfd
    image;
  - the executor refuses forbidden flags (`--dangerously-skip-permissions`,
    `--continue`, `-i`, `--remote-control`, `install`, `update`);
  - the prompt goes only on stdin, and only after `init` equality;
  - production launches need a covering cprot-v2 attestation.
- **Native self-update** (research §0 hazard 1). A changed binary fails
  closed at the sealed-image digest check (`TestAcceptance_Agy_S10_…`). An
  update mid-run makes every attestation stale by construction.
- **Silent conversation fallback** (hazard 2). If `init` reports an id
  other than the requested one, the child is killed before any prompt byte
  and the orphan id is recorded (`TestAcceptance_Agy_S05_…`).
- **Unresolved gaps** (spec §14, also in the matrix):
  - §14.7: the §3.8 diagnostics are not implemented.
  - §14.17: no controller operation records a disposition for an
    Uncertain turn attempt. The durable block holds; clearing it needs
    follow-up work. AC-008 and AC-009 have the same gap.
  - §14.18: the first attestation cannot be recorded in production. A
    configured service refuses construction until a covering row exists,
    and `RecordAgyProbeAttestation` has no HTTP route.
- **Descendants.** A descendant that leaves the child's process group
  escapes the forced kill (§14.6).

## Permissions

- Guardrails and native authentication stay enabled. No bypass or yolo
  flag is used or accepted.
- Credentials: nothing reads, copies, or moves the keyring or the
  credential file. `settings.json`, `hooks.json`, `mcp_config.json`, and
  `permissions.allow` are never written.
- `hooks.json` is read for its canonical digest and required pointers
  only. The evidence script records the digest and the top-level key
  names, never the content.
- The attestation operation requires the operator credential (§14.16).
  Session birth and uncertainty resolution require the current controller
  lease; authority is re-validated inside the storage transaction.
- Only one native adapter can be wired per service instance.
- Construction refuses everything except Linux hosts with a frozen
  `linux/unix` platform (§3.12).
- The evidence script is operator-invoked and CI-excluded:
  - Stage A is provider-free.
  - Stage B needs `AC010_STAGE_B_CONFIRMED=yes` plus an interactive
    confirmation, and is hard-capped at 8 turns.
  - Stage C needs `AC010_STAGE_C_CONFIRMED=yes`.
  - The evidence directory is 0700 and gitignored.
  - The script states its side effects before any live stage: each
    stream-json launch creates a new conversation record under the
    operator's `~/.gemini`.

## Tests (what actually ran)

All runs were on the implementation host (Linux, WSL2 kernel 6.18),
fixture only:

- `gofmt -l cmd internal`: clean.
- `go vet ./...`, `GOOS=windows go vet ./internal/...`, and
  `GOOS=darwin go vet ./internal/...`: clean.
- `go vet -tags evidence` on the two evidence helpers: clean.
- `CGO_ENABLED=0 go test ./... -count=1`: all packages ok.
- `go test -race ./internal/adapter/execpolicy/ ./internal/adapter/agy/ ./internal/storage/ ./internal/service/ -count=3`:
  ok.
- `bash -n` and `shellcheck -x` on `scripts/ac010-integration-evidence.sh`:
  clean.
- `scripts/ac010-integration-evidence.sh --dry-run a|b|c|all` ran against
  a sentinel binary with command shims and a temp `HOME`. Zero sentinel
  or shim executions, no files created. An `xtrace` run with the real
  binary path configured showed that no command other than `dirname`
  executed.
- Acceptance: `internal/service/acceptance_agy_test.go` has 14 tests: the
  lifecycle, the production path, and §6.1 S01–S12.
- Mutation checks run during development: removing the print-timeout
  marker, the ignored-input marker, the denial ⇒ incomplete rule, or the
  unresolved-attempt block each fails the corresponding acceptance test.

Exact command outputs are in the Task 8 report.

## Limits: not verified by this PR

- **Live Agy behavior.** Every test uses the fixture `agy`. The operator
  must run:
  - **Stage A** before any freeze: version, digest, the `models` layout
    (§14.12), `mcp list`, the canonical `plugin list`, `init.tools`, the
    hooks digest, and the **sealed-vs-path comparison**.
  - **Stages B and C** after Gate 2.
- **Durability across a real process crash.** Crashes are simulated by
  cancelling workers and closing the service, or through seams.
- **Provider authentication.** The `models` auth gate has run only against
  the fixture catalog.
- **Windows and macOS.** They are refusal-only by design and were checked
  only by `go vet`.
- **Items §7 carries forward.** Hook execution, skill loading, the
  subagent and shared-browser boundaries, workspace trust, content-block
  messages, `AGY_ERROR` exit 3, the updater off switch, and
  `GEMINI_API_KEY` mode are not claimed.

## Process note

`SendMessage` was denied during execution so fix rounds used fresh implementers.

## References

- Closes #10 only after Gate 2 review and the operator's Stage A capture.
  Stages B and C follow Gate 2.
- Spec: `docs/superpowers/specs/2026-09-24-ac-010-agy-adapter-design.md`
  (§4 evidence plan, §6/§6.1, §7, §14.1–§14.19).
- Plan: `docs/superpowers/plans/2026-09-24-ac-010-agy-adapter.md`.
- Evidence: `docs/superpowers/evidence/ac010-evidence-matrix.md`,
  `ac010-agy-installed-interface-research.md`,
  `ac010-agy-{init,plugins,tool-coverage}-1.2.9.json`, `SHA256SUMS-ac010`.
- Operator script: `scripts/ac010-integration-evidence.sh`, with its
  helpers
  `internal/adapter/execpolicy/sealed_probe_evidence_test.go` and
  `internal/adapter/agy/evidence_digests_test.go` (`-tags evidence`).
