# Roadmap

These are issue specifications, not completed functionality. The publisher creates
actual GitHub issues in dependency order and writes returned links to `.bootstrap`.
Stable `AC-xxx` keys are not assumed GitHub issue numbers.

Start with AC-001 and AC-006. Keep the first milestone independent of UI and SaaS.

## M0 — Foundation

Domain contracts, conformance tests, and owner decisions.

### AC-001 — Specify and enforce run, session, turn, and pending-prompt state contracts

The earlier prototype treated each phase as a fresh conversation. The accepted design needs persistent native sessions, parked contributors, and explicit controller release of pending prompts.

**Outcome:** Complete the domain state contract and evolve the small kernel with transactional-store-ready invariants, without creating a speculative workflow DSL.

**Acceptance:**

- Distinguish session, turn, process, pending prompt, run, checkpoint, archive, and purge.
- A completed turn parks; queued prompts never auto-run. Replacement/discard require the current controller lease.
- Exactly one active turn per contributor; stable turn IDs prevent silent replay across reconnects.
- Invalid transitions and stale completion events leave state unchanged.
- Archive retains evidence; cancellation, interruption, disconnect, and host loss have distinct states.

**Dependencies:** None.

### AC-006 — Implement the harness adapter contract and adversarial fake adapter

Harness integrations differ in session identity, tool approval, cancellation, output, and persistence. A generic subprocess exit cannot prove success.

**Outcome:** Create a small native-adapter interface plus deterministic fixtures for lifecycle/failure behavior.

**Acceptance:**

- Define capability probe, fresh session, resume, dispatch, observe, cancel, collect, and reconcile operations.
- Represent unknown model/usage/cancellation explicitly rather than successful zero values.
- Fake adapter can stall, duplicate events, deny a required tool, disconnect mid-turn and return malformed output.
- Conformance tests reject stale session IDs, misrouted output, unauthorized follow-ups and hidden fallback.
- Production mode cannot select a fake adapter without an explicit test-only mode.

**Dependencies:** AC-001.

### AC-020 — Define native-provider commercial boundaries

The repository is licensed under MIT, but terms around native provider authentication, subscription use, and future customer-owned-runner or hosted delivery remain separate research.

**Outcome:** Document native-provider commercial boundaries and guidelines for customer-owned-runner and hosted coordination, using primary provider terms.

**Acceptance:**

- The project-license decision is complete (MIT); keep project licensing separate from provider usage terms.
- Review each provider/harness separately, distinguishing native end-user auth from token collection or subscription pooling.
- Treat selling orchestration separately from selling model usage; record uncertainties and dated primary sources.
- Evaluate customer-owned-runner versus future hosted delivery boundaries under provider terms.
- Do not implement billing, acquire paid services, or copy tokens as part of this issue.

**Dependencies:** None.

### AC-021 — Strengthen CI and security/failure conformance for the service

A successful mocked run must not be used as evidence of isolation, persistent native sessions, or safe production cancellation.

**Outcome:** Extend the seeded CI and adversarial fixtures into a capability/evidence matrix with explicit native gates.

**Acceptance:**

- Keep default CI provider-free and read-only; use SHA-pinned actions and required aggregate ci check.
- Add failure tests for stale leases, race conditions, artifact tampering, unauthorized tool calls and malformed results.
- Run Go tests across intended OS targets without equating compile success to live functionality.
- Protect workflow/rules changes through PR review; do not add automatic secret-bearing fork workflows.
- Document native capability evidence per version and unknown/unsupported states without invented green statuses.

**Dependencies:** AC-001, AC-006.

## M1 — Durable runner

Service lifetime, persistent sessions, limits and recovery.

### AC-002 — Persist run journals, native session mappings, artifacts, and recovery state

A terminal transcript or native session ID alone cannot restore a council after a client or service restart.

**Outcome:** Implement SQLite-backed transactions plus a protected artifact directory, with explicit schema migration and retention boundaries.

**Acceptance:**

- Persist native harness/session IDs, workspace, tooling fingerprint, turn attempts, controller decisions and pending prompts.
- Seal artifact revisions with digests; refer approvals and ballots to exact revisions.
- Crash tests cover the boundary between durable dispatch intent and uncertain external execution.
- Recover interrupted/unknown work by reconciliation instead of blindly retrying it.
- Do not store provider tokens; exclude local runtime records from Git; implement permissions and redaction tests.

**Dependencies:** AC-001.

### AC-003 — Run the Go service independently of CLI and MCP client lifetime

Closing a terminal or stdio client currently risks losing the job owner. The service must outlive every view.

**Outcome:** Implement start/status/stop and a local authenticated control boundary. Add Linux/WSL service guidance first, documenting other platforms honestly.

**Acceptance:**

- Client disconnect does not terminate the service or authorized active workers.
- Service restart detects unfinished work and reports unknown outcomes rather than inventing completion.
- No unauthenticated localhost listener; use owner-scoped socket/named pipe or authenticated equivalent.
- Explicit cancellation waits for confirmation or reports cancellation uncertain.
- Host and container paths use persistent volumes for state; neither client exit nor archive purges native sessions.

**Dependencies:** AC-002.

### AC-004 — Adopt an existing controller session with an exclusive revocable lease

The current OpenCode/GLM, Claude, Codex, or Agy conversation must be able to control a run without a second implicit controller.

**Outcome:** Add controller attachment, lease ownership, reconnect, and explicit handoff under operator authorization.

**Acceptance:**

- Each of the four harness types can own a controller lease; contributor identity remains separate.
- Only one controller can dispatch or finalize per run; stale clients cannot replay commands.
- Disconnect permits only already-authorized bounded work to finish; new decisions wait.
- Operator can reattach or authorize a new controller using recorded state.
- No worker can acquire a controller lease, raise its allowance, or spawn a recursive council.

**Dependencies:** AC-001, AC-003.

### AC-005 — Freeze tooling and source profiles and prove independent worker isolation

Equal skill catalogs do not guarantee the same effective toolkit, and Git worktrees do not prevent sibling reads, shared indexes, or credential leakage.

**Outcome:** Define an approved workspace/profile contract compatible with dotfiles and guardrails; prove isolation rather than merely naming separate branches.

**Acceptance:**

- Record brief/source digest, effective model, harness version, tool/skill/plugin configuration and code-index scope.
- First-pass workers cannot read sibling proposals, controller conversation history, or mutable sibling indexes.
- Native auth stays native; no copied provider tokens or global permission relaxations.
- Attempted unauthorized file/tool/network operations produce explicit evidence in isolation tests.
- Only explicitly released artifacts become available during vote/review phases.

**Dependencies:** AC-001.

### AC-014 — Enforce finite budgets, retries, and missing-participant policy

Autonomous orchestration must not run in circles, silently degrade the quartet, or duplicate expensive workers after an uncertain failure.

**Outcome:** Put finite dispatch, elapsed-time, retry, and concurrency limits in ordinary code; add honest usage accounting.

**Acceptance:**

- Controller cannot reset or increase a limit by reconnecting, restarting, or changing a session ID.
- Default requires four contributors; reduced-panel behavior needs an explicit configured policy.
- Reconcile a potentially active attempt before retry; quota/auth/approval failures do not loop.
- Unknown token/cost telemetry is labeled unknown, with counters distinguishing harness turns from model calls.
- Cancellation requested/confirmed/unknown and deadline exhaustion are persisted and shown to controller.

**Dependencies:** AC-002, AC-003, AC-004.

### AC-022 — Document installation, service ownership, and local recovery procedures

A user must know what remains running after closing a terminal and how to recover safely without killing or duplicating a council.

**Outcome:** Document the actual implemented start/attach/stop/archive/recover paths with tested examples.

**Acceptance:**

- Explain exactly which process/container owns work and where persistent volumes/state live.
- Demonstrate the difference between disconnect, pause-after-turn, cancel, service stop, archive and purge.
- Never require copied tokens or hidden permission bypasses in the setup guide.
- Examples match executable tests where possible and flag manual/native verification steps.
- Report first-platform support honestly; do not promise universal installers or remote execution prematurely.

**Dependencies:** AC-003, AC-004, AC-014.

## M2 — Native agents and MCP

Four native adapters, controller tools and explicit skill integration.

### AC-007 — Build OpenCode/GLM persistent contributor adapter

OpenCode/GLM must contribute independently even while an existing OpenCode conversation controls the run.

**Outcome:** Implement the adapter against the installed OpenCode session interface and record real conformance evidence.

**Acceptance:**

- Create a fresh contributor session without copying controller history; validate intended provider/model.
- Load approved dotfiles tools/skills/plugins and leave guardrails enabled.
- Resume the exact native session for a targeted follow-up; do not select the most recent unrelated session.
- Map async result, approval wait, tool denial, usage, and abort semantics accurately.
- Demonstrate close/reopen/recovery with a real installation and sanitized fixtures; no quota-bypass flags.

**Dependencies:** AC-003, AC-005, AC-006.

### AC-008 — Build Claude persistent contributor adapter

Claude programmatic startup must preserve the shared toolkit and an explicit native conversation identity.

**Outcome:** Implement version-verified native CLI/SDK integration without using startup modes that silently omit hooks and skills.

**Acceptance:**

- Probe installed supported arguments instead of inventing them; capture resolved model/version.
- Create and resume exact contributor sessions with approved configuration and native login.
- Capture structured output and distinguish tool denials/approval needs from completed acceptance checks.
- Bound invocation/cancellation and reconcile uncertain completion before retry.
- Supply real toolkit-loading and recovery evidence, plus output fixtures for regression tests.

**Dependencies:** AC-003, AC-005, AC-006.

### AC-009 — Build Codex persistent contributor adapter

Codex thread start/resume and approval behavior must remain explicit when used as a contributor or controller.

**Outcome:** Implement a verified native App Server or CLI adapter selected from installed capabilities, not a direct model API substitute.

**Acceptance:**

- Fresh thread for independent proposal, exact thread resumption for later turns.
- Preserve configured sandbox and guardrail behavior; do not add yolo/approval-bypass flags.
- Route permission requests and tool outcomes as structured state, not terminal scraping guesses.
- Test thread interruption and service/client reconnection without duplicate execution.
- Verify shared toolkit and report unavailable usage fields or runtime coverage gaps.

**Dependencies:** AC-003, AC-005, AC-006.

### AC-010 — Build Agy persistent contributor adapter

Agy conversations and headless outcomes need native verification; an apparently successful exit may not establish that required tools executed.

**Outcome:** Implement the adapter with explicit conversation identity, capabilities, and honest approval/error mapping.

**Acceptance:**

- Probe the installed CLI and supported output fields, using actual sanitized fixtures.
- Start independently and resume a specified conversation for follow-up.
- Verify expected skills/tools/plugins and enabled guardrails.
- Required skipped/denied tools keep verification incomplete regardless of process exit code.
- Demonstrate bounded cancellation, unknown outcomes, and client-close recovery without fallback to another harness.

**Dependencies:** AC-003, AC-005, AC-006.

### AC-011 — Implement typed Council MCP tools and a thin CLI bridge

The operator should stay in their current agent, which invokes Council explicitly rather than manually copying messages.

**Outcome:** Expose start/attach, dispatch, collect, record-decision, resume, cancel and archive through a narrowly authorized interface.

**Acceptance:**

- A stdio MCP subprocess proxies to the durable service; terminating it does not terminate workers.
- Each tool has a strict schema, capability scope, controller lease, run ID and request identity.
- No generic arbitrary-tool/command tunnel or automatic permission escalation.
- Long tasks return durable identifiers; bounded collection does not spin model-powered polling loops.
- CLI and MCP use the same policies/state transitions; unimplemented capabilities fail explicitly.

**Dependencies:** AC-003, AC-004, AC-006.

### AC-012 — Package an explicitly invoked controller skill for dotfiles

Each supported harness needs the same council procedure without mandatory hooks or copied instructions on every task.

**Outcome:** Create one shared skill with minimal harness-specific discovery wrappers; prepare dotfiles integration as a separate authorized PR.

**Acceptance:**

- Skill uses current conversation as controller and creates a separate contributor from that same harness.
- Common frozen brief, no-self voting, reasoned synthesis and minority findings are explicit.
- Only an operator request/approved run policy invokes a council; simple tasks are not auto-expanded.
- Contributor credentials cannot invoke controller tools or recursively create a council.
- Verify discoverability/invocation in all four harnesses; do not silently edit global host settings.

**Dependencies:** AC-011.

### AC-015 — Deliver completion events to the correct controller without polling models

Removing copy/paste is not enough if the operator must repeatedly ask whether contributors are finished.

**Outcome:** Build durable event cursors/collection first; add an OpenCode-specific convenience bridge only when required by its actual capabilities.

**Acceptance:**

- Events map to run, contributor, turn and active controller lease.
- Reconnect replays missed events without creating duplicate follow-up turns.
- Distinguish notify-only from permission to start another controller model turn.
- No accidental injection into a different project or unrelated controller conversation.
- Document and test equivalent capability or explicit limitations for all four controller harnesses.

**Dependencies:** AC-011, AC-014.

## M3 — First real council

Reproduce the README experiment with real harnesses.

### AC-013 — Coordinate sealed proposals, private ballots, synthesis, and findings

Selecting a vote winner alone would discard the complementary discoveries that motivated the project.

**Outcome:** Implement the fixed deliberation workflow and persistent finding dispositions over the tested kernel.

**Acceptance:**

- Exactly four proposals from one frozen brief/profile/source; digests seal the proposal set.
- Four ballots, no self-vote/fifth vote/duplicate contributor; keep ballots private until phase close.
- Preference and acceptance are distinct; ties or a non-leading choice need a recorded reason.
- Synthesis is a new revision with separate review evidence; stale reviews cannot approve it.
- Every material finding has provenance and incorporated/deferred/duplicate/rejected/unresolved disposition.

**Dependencies:** AC-001, AC-002, AC-004, AC-011.

### AC-016 — Run the real README council acceptance experiment

Unit tests and a simulated council do not prove that the full quartet reduces manual coordination with native tooling and durable sessions.

**Outcome:** Execute the agreed README experiment on an approved source snapshot and publish a sanitized evidence report.

**Acceptance:**

- Existing OpenCode/GLM controls four independent real contributors without manual prompt/result copying.
- Close the client, reconnect, recover results, and resume exactly one parked contributor; others stay parked.
- Complete private peer voting, reasoned synthesis, finding dispositions and bounded review.
- Report provider usage/unknowns, operator intervention time, verification gaps and final acceptance.
- Repeat controller adoption with Claude, Codex and Agy; no live repo mutation or issue publication without distinct authorization.

**Dependencies:** AC-007, AC-008, AC-009, AC-010, AC-012, AC-013, AC-014, AC-015.

## M4 — Git and optional interface

Safe publication and optional graphical inspection based on observed friction.

### AC-017 — Add safe Git workspaces, checkpoints, and integration operations

Repository operations must be reviewable and recoverable; worktrees alone do not preserve dirty state or ensure isolation.

**Outcome:** Wrap native Git with explicit typed operations and operator/role policies rather than implementing Git or a full IDE.

**Acceptance:**

- Separate Council checkpoints from commits; preserve staged/unstaged/new files through an explicit snapshot policy.
- Record base SHA, workspace ownership, target branch, diff and operation ID.
- No direct main push, force-push, silent rebase/reset, or unreviewed merge.
- Conflicts and uncertain external state surface as blockers; inspect actual Git outcome before claiming success.
- Tests cover dirty worktrees, concurrent ownership, interrupted operations and rollback boundaries.

**Dependencies:** AC-002, AC-005, AC-014.

### AC-018 — Publish approved findings to GitHub with idempotent mappings

The README experiment created duplicate issues and competing backlog-cleanup PRs. Discovery and publication need separate ownership.

**Outcome:** Use a single authorized publisher for finding-to-issue mapping and shared-document cleanup.

**Acceptance:**

- Findings become drafts unless publication was explicitly authorized.
- Stable finding IDs and external IDs survive retries; reconcile after ambiguous network outcomes.
- Preserve all sources when deduplicating; dependencies have actual issue cross-references.
- Read back bodies, requested labels and links before reporting successful migration.
- Delete temporary backlog only after verified destinations; never close implementation issues merely because they were created.

**Dependencies:** AC-013, AC-017.

### AC-019 — Prototype a minimal optional Go/templ/HTMX operator view

The current agent is the first interface. A later graphical view may improve inspection and recovery but must not own execution or drive premature layout design.

**Outcome:** Evaluate one-screen run/session/decision pages against observed friction from the real experiment.

**Acceptance:**

- Use the same service/API; closing the page has no lifecycle effect.
- Show pending prompts, current controller, parked/busy/blocked state, proposals, votes and findings.
- No terminal emulator, monitor-count assumptions, or second backend required.
- Protect command endpoints, browser origins and untrusted rendered artifact content.
- Keep UI framework choice explicitly reversible until usability evidence justifies it.

**Dependencies:** AC-016.

