# AC-010 handover — Agy (Antigravity CLI) contributor adapter (#10)

Date: 2026-09-24. Written by the Claude controller session for the next
agent (GPT). Read this first, then `AGENTS.md`, then the spec's §14.

## Where things stand (one paragraph)

Branch `feat/ac-010-agy-adapter` (HEAD `0106382`, 64 commits ahead of
`main`, 100 files, +28762/−163) holds the complete AC-010 implementation:
all eight plan tasks, a completion wave, a whole-branch review, one fix
wave, and a clean scoped re-review. The full CI-equivalent suite is green
on the final tree. Nothing has been pushed. The next step is the
integration decision, which belongs to Carlitos (push + PR to `main`,
squash-only). No live provider run has happened in this branch; every
proof is fixture-only. See "What is NOT proven" before promising anything.

## Governing documents (in authority order)

1. `AGENTS.md` — repository invariants (never push to main, no auto-merge,
   no bypass flags, never copy provider tokens, fake adapters never
   production fallbacks).
2. `docs/superpowers/specs/2026-09-24-ac-010-agy-adapter-design.md` — the
   ACCEPTED design (v7). **§14 lists 19 implementation deltas recorded
   during execution; each is a binding controller ruling** (child `HOME`
   = parent of `expected_home`, `MFD_EXEC` with `EINVAL` fallback, durable
   pre-launch creation marker, awaiting-attestation server state, and so
   on). Treat §14 as overriding earlier §3/§6 wording where they differ.
3. `docs/superpowers/plans/2026-09-24-ac-010-agy-adapter.md` — the plan
   (eight tasks; Global Constraints section is binding).
4. `.superpowers/sdd/2026-09-24-ac-010-agy-adapter/` (git-ignored, on this
   machine only; a copy sits in the Claude session scratchpad) —
   `progress.md` is the execution ledger with **35 `Ruling:` lines**,
   `rulings.txt` extracts them, `task-N-brief.md` / `task-N-report.md`
   hold each task's requirements and evidence, `final-fix-report.md` and
   `final-wave-report.md` the last two waves, `review-*.diff` the review
   packages. If this directory is gone (`git clean -fdx` destroys it),
   `git log main..HEAD` and spec §14 are the recovery map.

## Operator-facing artifacts already written

- `docs/superpowers/evidence/ac010-pr-body-draft.md` — PR body (risk,
  permissions, tests, limits, #10 references, evidence-matrix link, the
  guardrail note). Use it verbatim as the PR description after reading it
  once more against HEAD.
- `docs/superpowers/evidence/ac010-evidence-matrix.md` — every spec §6/§6.1
  row and plan self-review row mapped to committed test names, with an
  honest "unverified live" column.
- `scripts/ac010-integration-evidence.sh` — the OPERATOR evidence script.
  Stage A (provider-free on the real binary), Stage B (≤ 8 authenticated
  turns, env flag + interactive confirmation), Stage C (probe suite that
  drafts the first cprot-v2 attestation record set). `--dry-run` executes
  nothing. Agents never run Stages A/B/C; Carlitos does.
- `docs/superpowers/evidence/ac010-agy-*.json`, `SHA256SUMS-ac010` —
  canonical captures from the research phase (57 tools, plugins, coverage
  map).

## Verification state at HEAD `0106382`

| Check | Result |
|---|---|
| `gofmt -l cmd internal` | clean |
| `go vet ./...`, `GOOS=windows`/`GOOS=darwin` vet of `./internal/...` | clean |
| `go build ./...` | clean |
| `CGO_ENABLED=0 go test ./... -count=1` | 16 packages ok, exit 0 |
| `go test -race` on execpolicy, agy, storage, service (`-count=3`, fix wave) | ok |
| `bash -n` + `--dry-run` of the evidence script | ok, nothing executed |
| `python3 scripts/verify_seed.py` + unittest | ok |

Run the same list before opening the PR; CI (`.github/workflows/ci.yml`)
runs tests on ubuntu/macos/windows plus `gofmt`, `-race`, and the seed
checks.

## Immediate next steps (in order)

1. **Integration decision (Carlitos only).** Options: push and open a PR
   against `main`; or keep the branch local. Do not merge without his
   explicit authorization; `main` is squash-only with required checks.
   Commit messages carry no attribution lines.
2. If a PR is opened: paste `ac010-pr-body-draft.md`, request review, and
   watch CI on all three OSes (macOS path canonicalization and Windows
   typed no-ops were vetted, not run).
3. After Gate 2 review findings arrive: fix on the branch, re-run the
   verification list, never lower protections to get green.
4. Operator-only afterwards: Stage A before any profile freeze (records
   the real `models` column layout, spec §14.12, and the sealed-vs-path
   comparison), then Stages B/C after Gate 2.

## What is NOT proven (say this plainly in any status)

- No real `agy` invocation happened in this branch. Live mediation,
  provider auth, the real `models` output layout, the real `hooks.json`
  shape, descendant behaviour of real tool-spawned processes, and macOS/
  Windows runtime are unverified.
- Two documented gaps remain by design (spec §14.17, §14.18): there is no
  service operation that records a controller disposition for an
  Uncertain turn attempt (the codex adapter shares this gap), and there
  is no HTTP/CLI route to record the first attestation on a running
  daemon, so a shipped binary cannot enable production Agy on its own.
  Both need follow-up issues; do not silently implement them inside this
  PR.
- §3.8 diagnostics (conversation file size / step counts) are not
  implemented (spec §14.7).

## Hazards and habits that cost time this session

- `agy` is aliased to `agy --dangerously-skip-permissions` in the
  interactive shell. Never rely on the alias; use the absolute path only
  with explicit operator authorization, never from tests.
- `ls`, `grep`, `rm`, `cp` are aliased (ugrep, interactive prompts). Use
  `/bin/ls`, `/usr/bin/grep`, `/bin/rm -f`, `/bin/cp -f` in scripts.
- zsh expands a leading `=word` (`echo ===` errors).
- IDE diagnostics reported in tool output are frequently stale after
  subagent commits; trust `go build`/`go vet`.
- The guardrail denies the `SendMessage` tool, so running subagents cannot
  be resumed; fix rounds were fresh dispatches carrying brief + report +
  findings. Never ask another agent to perform a denied action.
- Never read `~/.gemini`, the keyring, or credentials; tests use temp
  homes only. The fixture's `materialize` directive writes under the
  test's temp `HOME`.

## Architecture cheat sheet (where to look)

- Sealed launch: `internal/adapter/execpolicy/sealed*.go`, `executor.go`
  (`IsAgyLaunch`, `HomeDir` seam, forbidden args), `process.go`
  (`Terminate`, group kill, WNOWAIT peek), `terminate_unix.go`.
- Protocol + fixture: `internal/adapter/agy/stream.go`, `agyfake_test.go`
  (fixture source constant, directive reference in its doc comment),
  `agytest/fixture.go` (byte-identical copy, guarded), `agytest/guard_test.go`.
- Adapter: `internal/adapter/agy/adapter.go` (create/dispatch/observe/
  cancel/collect/reconcile), `launch.go` (frozen argv grammar), `process.go`,
  `verification.go` (§3.5 denial attribution), `eligibility.go`,
  `authgate.go`, `toolkit.go`, `wiring.go` (`NewProductionAgyAdapter`).
- Storage: `internal/storage/migrations.go` (`agyStateV7DDL`, amended in
  place — v7 is unreleased), `agy_state.go`, `agy_attestation.go`
  (episodes, in-flight marker), `canonical_profile.go` (cprof-v4).
- Service: `internal/service/agy_probe_attestation.go`
  (`CreateAgySession`, `RecordAgyProbeAttestation`,
  `ResolveAgySessionCreationUncertainty`, required-tools seam),
  `server.go` (Agy config, awaiting-attestation status), `commands.go`
  (`required_tools` at queue time), tests `acceptance_agy_test.go`,
  `review_ac010_*_test.go`, `agy_live_marker_test.go`.

## Ruling style, if you continue as controller

Decide conflicts against the spec, record `Ruling: <what> — <why> — <cost
if wrong>` in the ledger, keep going. Stop only for destructive or
outward-facing actions (push, merge, publish, live provider runs) and ask
Carlitos.
