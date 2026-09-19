# Working on Agent Council

## Read first

Read README.md, docs/ARCHITECTURE.md, the relevant ADR, and the assigned issue.
Treat accepted decisions in this repository as the current design; earlier chat
prototypes are not a specification. Native integrations are planned until proved.

## Product invariants

- Explicit invocation: no automatic council for every task; no mandatory hooks.
- Four independent contributors and one interchangeable controller. The current
  operator harness can control; its independent proposal uses a separate session.
- Initial brief/source/profile are frozen. No sibling access during generation.
- Contributor identity, not session ID, controls voting. No self/fifth/duplicate
  ballot. Tally is evidence, not authority over acceptance requirements.
- Preserve minority findings with a disposition and source provenance.
- Sessions persist; completed turns park. Only an authorized controller can release
  a queued prompt. Archive preserves evidence; purge is a separate operator action.
- Client disconnect is not cancellation. A disconnected interactive controller
  cannot make new decisions. Bounded active work may finish and wait.
- Only one controller lease per run. Workers cannot create recursive councils,
  increase budgets, accept their own result, or loosen permissions.
- Fake adapters are test utilities, never production fallbacks.
- Guardrails and native authentication remain enabled. Do not use bypass/yolo flags,
  copy provider tokens, inspect unrelated secrets, or broaden shared toolkit access.

## Repository changes

Use a task branch or assigned worktree. Never push directly to main, force-push,
reset someone else's work, change repository rules, or auto-merge without explicit
operator authorization. A Git commit is not the same as a Council checkpoint.
Stage named files after checking the diff. Keep code, findings, and checkpoints
separate from credentials and real session exports. Respect CODEOWNERS.

## Evidence and completion

Add a failing test for the changed invariant, implement the smallest change, run
relevant checks, and report what actually ran. Passing unit fixtures do not prove
live harness mediation, durable recovery, process cancellation, or provider auth.
Describe unverified capabilities explicitly. Include risk, permissions, tests,
limits, and issue references in each PR. Never lower protections to get a green CI.

## Scope control

No UI framework, SaaS, billing, terminal emulator, or generic workflow designer is
required for the first live council. Go + templ + HTMX is a later candidate. Do not
build a second backend. Preserve public interfaces without speculative abstractions.
