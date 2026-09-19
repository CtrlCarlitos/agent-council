# Architecture: controller-led Council

Status: accepted direction; implementation tracked by AC issue keys.

## Interfaces and ownership

The current OpenCode/GLM, Claude, Codex, or Agy conversation is the external
controller. A shared skill explains the procedure. Typed MCP tools and a supporting
CLI communicate with a persistent Go service. The service owns the durable run
journal, native session mappings, scheduling, finite limits, artifacts, and worker
lifecycle. A stdio MCP subprocess is a bridge, never the owner of the workers.

The same harness/model used by the controller also contributes in a separate fresh
session. There are exactly four contributor identities. The controller's existing
conversation is not copied wholesale to them: it produces a common approved brief.

## Data concepts

Profile: approved tooling and model selections with versions/capabilities.
Run: immutable source/brief/profile identifiers, controller lease and policies.
Contributor: stable logical identity independent of process restart or session ID.
Native session: provider-side/local conversation identity and resume metadata.
Turn: one dispatched assignment, its idempotency key, attempt identity and status.
Pending prompt: not executable until released by the current authorized controller.
Artifact: versioned proposal, patch, ballot, synthesis, finding, or verification record.
Process: transient execution instance; a process exit is not session deletion.
Checkpoint: saved Council state; not an implicit Git commit.

## Lifecycle

Prepare -> independent contribution -> sealed proposals -> private peer votes ->
controller adjudication/synthesis -> review/verification -> operator acceptance.
After a turn completes the contributor parks. Reviews normally resume the same
native sessions. A clean-room reviewer is an explicit exception, not the default.
Fresh session IDs without context isolation do not prove independent contributions.

Disconnect is not cancel. Authorized active turns can finish within their limits.
New controller decisions wait for reconnect or an explicit lease handoff. A browser
or MCP client cannot resurrect a cancelled run or reset a budget through reconnect.
Archive retains the run. Purge is a distinct, operator-authorized retention action.

## Voting and synthesis

One peer ballot per contributor. The proposal cannot belong to the voter. All
ballots bind a proposal-set digest and acceptance-criteria version. They are sealed
before reveal. No fifth controller vote and no automatic popularity-based merge.
A ballot has preference, criteria-based reasons, weaknesses, salvageable findings,
and a separate acceptance assessment. None may pass even though one is preferred.

The controller can depart from the vote leader with a reason grounded in the task.
A synthesized artifact is a new revision requiring its own review; old approvals
cannot silently apply. Findings have stable IDs, source provenance and explicit
incorporated/deferred/duplicate/rejected/unresolved dispositions.

## Policy versus model judgment

Go enforces identities, access, states, dispatch barriers, budgets, retries, and
retention. Models compare alternatives and propose decisions. A model cannot raise
its own allowance, change a role, unlock an unauthorized Git action, or silently
replace a missing model. Finite turns are not a substitute for honest usage accounting:
unavailable tokens/cost are unknown, not zero.

## Integrations

Use native harness interfaces selected after inspecting the installed version.
No copied OAuth/session tokens and no global permission edits. Tooling is inherited
from an approved dotfiles profile, with what actually loaded recorded and verified.
A nonzero/zero exit alone is insufficient to prove or disprove required actions.

Native Git performs explicit workspace/branch/commit operations under policy.
GitHub publication is separate from finding creation and uses stable mappings,
read-back verification and ambiguity reconciliation. No automatic issue creation
or publishing merely because another idea was discovered.

## Deferred choices

Go + templ + HTMX is a candidate operator UI. Wails/Tauri, full terminal embedding,
remote fleets, Takumi integration, and SaaS are not prerequisites. SQLite is the
local durable store candidate; the initial kernel adds no database dependency.
Licensing and provider commercial-use review remain explicit owner decisions.

## First live test

Four real contributors, one interactive controller, no manual message copying;
close/reopen client, recover results, resume exactly one parked contributor, then
complete ballots and synthesis. Record versions, auth/profile checks, budgets,
limitations, and actual native evidence. Repeat for all four controller choices.
