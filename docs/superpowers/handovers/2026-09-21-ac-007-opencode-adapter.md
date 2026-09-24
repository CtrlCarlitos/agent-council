# AC-007 OpenCode adapter handover

## Repository state

- Branch: `feat/ac-007-opencode-adapter`
- HEAD: `910160cd27dbf81c941f272c11a3754e19621877`
- Remote tracking branch: `origin/feat/ac-007-opencode-adapter`
- Working tree before this handover document was added: clean
- Draft pull request: #32
- Base: AC-005 on `main`

Do not resume from `cb47224`; it is an earlier branch commit. Several prior
status reports also confused AC-003 controller-grant work with AC-007 tasks.
Use the task definitions in the current plan.

## Governing documents

- Design: `docs/superpowers/specs/2026-09-21-ac-007-opencode-adapter-design.md`
  - Initial design: `098b28d`
  - Later accepted revisions culminated in `53597ed`; `9739aa8` is not the
    final revision.
- Plan: `docs/superpowers/plans/2026-09-21-ac-007-opencode-adapter.md`
  - The final plan correction is `384cc2e`; `8a12344` is an earlier revision.
- AC-006 adapter contract:
  `docs/superpowers/plans/2026-09-19-ac-006-harness-adapter-contract.md`

Read `README.md`, `docs/ARCHITECTURE.md`, the applicable ADRs, and issue #7
before changing code. Repository documents override earlier chat summaries.

## Implemented code

- `execpolicy.GeneratedServerEnv` exists for the two generated OpenCode server
  credential variables.
- The server manager has per-session child ownership and launch serialization.
- Native message IDs use length-prefixed tuple encoding and reject NUL bytes.
- The adapter contains Dispatch, Observe, Cancel, Collect, Reconcile,
  CreateSession, and ResumeSession implementations.
- Storage-backed dispatch identity and session launch sources exist in
  `internal/adapter/opencode/wiring.go`.
- Service assembly can select OpenCode with `ServerConfig.OpenCodeBinaryPath`.
- Service assembly now creates one workspace manager and policy executor and
  passes those instances to the selected adapter and server.
- The fake OpenCode HTTP server supports the basic session, message, abort,
  health, model, and permission endpoints.

These statements describe code presence. They do not constitute Gate 1 or
live-provider evidence.

## Known incorrect or insufficient evidence

### Gate 1 tests

`internal/adapter/opencode/review_gate1_test.go` is insufficient:

- `TestGate1Review_ConcurrentDuplicateDispatchSingleLaunch` checks the size of
  the adapter's dispatch map. A map keyed by `TurnRef` has size one even if five
  native HTTP requests were sent. The fake server must count
  `prompt_async` requests and the test must assert exactly one.
- `TestGate1Review_CollectParentIDCorrelation` manually installs a dispatch
  record and constructs a message ID using an attempt value different from the
  fixture identity source. It does not prove Dispatch-to-Collect correlation.
- Portable HTTP/state-machine tests are guarded by `//go:build unix`. Remove
  that tag from portable tests and keep platform tags only on fixtures that use
  POSIX shells, signals, or permission semantics.

### Production wiring test

`internal/adapter/opencode/wiring_test.go` does not enter through service
construction. It manually inserts a fake endpoint into the server manager and
manually inserts a dispatch record. It never calls Dispatch and does not assert
the fake server's dispatch ledger.

`internal/service/review_ac007_gate1_test.go` proves only that configured
construction produces non-nil fields. It does not launch a controlled OpenCode
child or prove authenticated HTTP traffic.

### Fake server and HTTP path

- Add a thread-safe request ledger that records at least the number of
  `prompt_async` calls, addressed native session IDs, message IDs, abort calls,
  and authentication failures.
- The adapter still uses `http.DefaultClient` in core methods. Verify that all
  requests to a generated-credential server apply the retained Basic
  credentials and use a client/transport that can classify pre-write rejection
  separately from post-write ambiguity.
- Verify the retry rule by GET-by-message-ID: resubmit only after a verified
  404; if the message exists, collect or reconcile without resubmission.

### Lifecycle evidence

Add observable tests for:

- concurrent server starts producing exactly one native child;
- generated credentials being retained and used for health and contract calls;
- both stdout and stderr being drained for the child's lifetime;
- failed health checks terminating the child and not registering it;
- park/idle shutdown and resume starting a replacement server against the same
  workspace, followed by exact native-session verification;
- unsupported strict isolation failing closed.

Separate the POSIX process fixture from portable lifecycle/state tests.

## Required next implementation sequence

1. Remove overly broad build tags from portable adapter, fake-server, digest,
   wiring, and contract tests. Keep tags only where the implementation or test
   actually depends on platform process behavior.
2. Add the fake-server request ledger and authentication checks.
3. Replace the duplicate-dispatch test with five concurrent Dispatch calls and
   an assertion of exactly one native `prompt_async` request. Assert every
   caller receives the launcher's verdict. Add failed-launch reservation release
   and genuine retry evidence.
4. Add a Dispatch-to-Collect test that obtains its attempt through the injected
   identity source, lets Dispatch generate the native message ID, and returns an
   assistant message whose `parentID` is exactly that ID. Do not mutate adapter
   internals from the test.
5. Complete typed authenticated HTTP behavior and the rejected-versus-unknown
   transport boundary. Test pre-transmission connection refusal and a
   post-write disconnect independently.
6. Add the lifecycle evidence above.
7. Run Gate 1 only after Tasks 1-4 in the current plan are complete. Record the
   exact commands and results.
8. After Gate 1, finish service wiring/acceptance evidence required by Tasks
   5-7. Replace the recycled AC-004 acceptance story with an OpenCode-only story
   that enters through configured service construction and reaches a controlled
   fake OpenCode subprocess through `PolicyExecutor`.
9. Map every design-spec section 10 invariant to a named test and add the
   sanitized, provider-free-by-default manual integration script before Gate 2.

## Verification expectations

Run and report at minimum:

```text
gofmt -l cmd internal
go vet ./...
CGO_ENABLED=0 go test ./... -count=1
go test -race ./... -count=1
go test -race ./internal/adapter/opencode -run '^TestGate1Review_' -count=3
```

Also run Windows and Darwin vet/compile checks. A green suite containing only
mock-state assertions does not prove native request count, authenticated server
lifecycle, durable recovery, provider authentication, or live model execution.

## Scope boundaries

- OpenCode only; do not rotate Claude, Codex, or Agy harnesses in AC-007
  acceptance evidence.
- Never call `/config/providers`.
- Never use `--auto` or another permission bypass.
- Permission requests are denied by default and mirrored as explicit events.
- Do not copy or inspect provider credentials.
- Real provider invocation remains optional, operator-run, and never
  auto-selects a model.
- Do not represent fake-server evidence as proof of native authentication or a
  live provider call.
