# Agent Council

**Keep your preferred agent. Let Council coordinate the other perspectives.**

Agent Council is a local-first orchestration project for four independent native
harness contributors and one interchangeable controller. Your current OpenCode +
GLM, Claude, Codex, or Antigravity conversation can be the controller. It invokes
Council explicitly through a skill and MCP tools instead of asking you to copy
prompts and answers between terminals.

## Status: foundation, not a working orchestrator

This repository currently contains a tested, dependency-free Go domain kernel,
architecture decisions, contribution and security policies, and a provisionable
issue roadmap. The daemon, durable database, MCP bridge, native harness adapters,
and graphical interface are **not implemented**. There are no production install
commands or native-provider compatibility claims yet.

Run the foundation checks:

```sh
go test -race ./...
go vet ./...
go run ./cmd/council about
python3 -m unittest discover -s scripts/tests -v
```

Use a currently supported Go toolchain for development. The module's minimum
language level is 1.23; CI is configured for Go 1.27.1. Python 3.10+ and an
authenticated GitHub CLI are needed only for the one-time repository publisher,
not for the future Council executable.

## The workflow we are building

1. The operator authorizes an assignment, a tooling profile, and finite limits.
2. The current harness adopts the controller role; it does not become its own
   independent submission.
3. Council starts four fresh contributors: OpenCode/GLM, Claude, Codex, and Agy.
   They receive the same frozen brief and source baseline, without sibling work.
4. Each contributor submits a sealed proposal, then votes for one of the other
   three. Ballots remain private until the ballot phase closes. No self-votes,
   duplicate votes, or fifth controller ballot.
5. The controller weighs reasons and evidence, not just the tally, and can create
   a new synthesis. Valuable findings survive even when their proposal loses.
6. The same saved contributor sessions can be resumed for review and targeted
   follow-ups. Results park the contributors; queued prompts never auto-run.
7. The operator accepts the requested outcome. Further opportunities become
   findings or issue drafts, not an unbounded expansion of the assignment.

Four contributors plus one controller are five logical roles. The controller
harness also gets a **separate** contributor session. Provider/model selection
is explicit; Council must never silently substitute models or a smaller panel.

## Sessions survive views

The intended Go service owns execution and durable state. A CLI, MCP stdio bridge,
or future web view is just a client. Closing a view must not kill workers. If the
interactive controller disconnects, already-authorized bounded turns may finish;
new controller decisions wait. We do not claim that a closed conversation keeps
thinking. A native session ID, Council journal, workspace, and artifacts are all
part of recovery; a transcript alone is not a resumable session.

## Architecture boundary

- **Go core and service:** lifecycle, sessions, artifacts, policy, limits, recovery.
- **Native adapters:** use each harness's supported interface and native login.
- **MCP bridge + shared skill:** explicit agent invocation, not mandatory hooks.
- **Dotfiles:** approved tools, skills, and plugins; never copied credentials.
- **Optional UI:** Go + templ + HTMX is a candidate, not a first-milestone dependency.
- **Separate projects:** not a feature inside `agent-guardrails`; no Takumi dependency.
- **Possible SaaS later:** customer-owned runners and hosted coordination are a
  research direction, not a current service or a provider-licensing promise.

Council is not a sandbox and must not bypass guardrails. Branches or worktrees
alone do not establish filesystem, credential, memory, or code-index isolation.

## Start contributing

Read [AGENTS.md](AGENTS.md), [CONTRIBUTING.md](CONTRIBUTING.md), and
[the architecture](docs/ARCHITECTURE.md). The [roadmap](docs/ROADMAP.md) uses stable
`AC-xxx` keys; the publisher converts it into elaborated GitHub issues with actual
dependency references. Start with the core contracts and adapter conformance work.

All changes go through a PR to `main`, including agent-generated work. The planned
repository rule requires CI, resolved review threads, and linear history; it blocks
force-pushes and deletion, with no bypass actors. See [repository policy](docs/REPOSITORY_POLICY.md).
A policy file is not proof the remote rule is installed: the bootstrap publisher
performs read-back verification and saves its result locally.

## First live acceptance test

From an existing OpenCode + GLM session, invoke Council; obtain four independent
README proposals without manual copying; close the client; reconnect; recover all
results; resume exactly one parked contributor with an authorized follow-up; then
complete peer voting, synthesis, and operator acceptance. Repeat controller adoption
with Claude, Codex, and Agy. Record real evidence; never substitute a fake adapter
and report that a native harness passed.

## Publishing this starting repository

The distribution can create `CtrlCarlitos/agent-council` from your authenticated
GitHub CLI without asking a coding agent to interpret these documents:

```sh
python3 scripts/publish.py          # offline plan and payload verification
python3 scripts/publish.py --apply  # creates the public repo, rules, issues, and bootstrap PR
```

It refuses to overwrite an unrelated existing repository. It initializes only a
minimal GitHub README on `main`, installs the main rules **before** publishing the
full scaffold on `chore/bootstrap`, and leaves that PR open for review. It never
merges it or creates additional admin credentials. See [publishing instructions](docs/PUBLISHING.md).

## License

This project is licensed under the MIT License. See [LICENSE](LICENSE) for details.
