# council: ask other coding agents for independent advice, review or builds

Status: approved by the operator 2026-11-21 (approach 1, skill only).

Destination: `docs/superpowers/specs/2026-11-21-council-design.md` in `CtrlCarlitos/skills`. It was
drafted in `agent-council` because the session's guardrail does not allow writing outside the
repository the session started in.

## Why

The operator ran Claude and Codex through code-and-review cycles using nothing but a shared
`agent-to-agent/` directory of markdown files and a short rules file. It worked well ("90% of
satisfaction") with no code written.

The `agent-council` repository aimed at the same goal with a Go service: about 45,000 lines of code
and 64,000 lines of tests, and it has never run a council end to end. That project is being
archived. This skill replaces it with the smallest thing that delivers the goal.

The one improvement over the mailbox run: the coordinator **launches** each member as a one-shot
command instead of every agent watching a directory. The command returning is the "done" signal, so
there are no watchers, no polling, and no agent that cannot be woken.

## Goals

- From inside any coding-agent conversation, the operator says "ask the council" and the current
  agent (the **coordinator**) gets independent answers from other agents without copy and paste.
- A council is one or more **members**. A member is an agent CLI plus a model, for example
  `claude` + haiku, `claude` + fable, `codex`, `opencode` + GLM, `agy`. One member is allowed. Two
  members on the same CLI with different models are allowed.
- When the coordinator's own agent is also a member, that member is a separate fresh session with no
  access to the coordinator's conversation.
- Members answer without seeing each other's work. The coordinator reads everything and decides.
- No shared directory ever changes branch under an agent that is working in it.

## Non-goals

- No service, database, MCP server or background process.
- No crash recovery beyond the files on disk and the recorded session ids.
- No enforced sealing. Independence comes from separate sessions and separate worktrees plus rules
  the members are told to follow; the skill says so plainly and does not claim more.
- No UI.

## Anti-goals

- No bypass or "yolo" flags on any member, ever. Launch commands name the real binary, so a shell
  alias that adds such a flag is not picked up.
- No silent substitution: a missing member is reported, never replaced by another agent or model.
- No helper script until the same friction has shown up twice in real use (see Approaches).
- A member's reply is information. It never counts as the operator's approval, and no member can
  merge, push, or start its own council.

## Approaches

1. **Skill only (chosen).** A `SKILL.md` with the procedure and rules, plus a reference file of
   launch commands per agent. No code. Chosen by the operator on 2026-11-21 because rules alone
   already delivered most of the value.
2. **Skill plus one small launcher script.** Hides the differences between the CLIs (session ids,
   timeouts, where the answer lands). The expected first addition, made only when the same friction
   shows up twice.
3. **Finish the Go service.** Rejected: at least five more large pieces of work before a first run,
   and the effort so far went into rare failure cases.

## What the coordinator can ask

| Request | Each member gets | Comes back | Members write code? | Phase |
|---|---|---|---|---|
| **Advise** ("how should we approach this?", "I'm stuck, what's wrong?") | same brief, same commit | a proposal or diagnosis each | no | 1 |
| **Review** ("here is finished work, find the problems") | same brief, same commit of the work | a findings list each | no | 1 |
| **Build** ("each of you implement this") | same brief, same starting commit | a branch each, plus a short report | yes | 2 |

A one-member Review is an ordinary code-and-review pair; it uses the same procedure.

**Vote** is an optional second step for Advise and Build, available with three or more members:
each member is given the others' replies and picks one that is not its own, with reasons. One
ballot per member; the coordinator does not vote. The tally informs the coordinator and does not
bind it.

**Review rounds:** after the coordinator fixes findings, the same member sessions are resumed with
a new brief pointing at the new snapshot. In phase 1 only the coordinator fixes; a member that
fixes code is Build and waits for phase 2. (Builder's call.)

**Scope of the first build:** phase 1 only. `SKILL.md` and the brief templates cover Advise,
Review and Vote. Build is left out of the skill's text until phase 2 so that nothing unverified
reads as available. (Builder's call.)

## Who is on the council

The operator names the members in the request. If they do not, the coordinator proposes a panel,
states it, and asks once before launching, because every member spends the operator's quota.
(Builder's call recorded here: no saved default panel in phase 1.)

Each member has a unique name used for its files and branch, for example `claude-haiku`,
`claude-fable`, `codex`.

## How a round works

1. The coordinator creates the run directory and writes `brief.md`: the request, the commit, the
   required shape of the answer, and the member rules.
2. The coordinator creates one worktree per member (see Worktrees).
3. The coordinator launches each member in its worktree with a one-line prompt: read the brief at
   this path and follow it. Members are launched side by side where the coordinator's harness
   allows background commands; otherwise one after another.
4. Each member's final answer is saved as its reply file. The coordinator captures the answer from
   the command's output; a member does not write into the run directory. If an agent's output turns
   out to be unusable for capture, that agent's member writes its own reply file instead, and the
   reference file says so for that agent.
5. The coordinator records each member's session id in `run.md`.
6. The coordinator reads all replies and writes `decision.md`: what it concluded and, for each
   point, which member's reply it came from. Disagreements and minority points are listed, not
   dropped.
7. The coordinator reports to the operator. The operator accepts, asks for another round, or stops.

## The run directory

```
agent-to-agent/<run>/
  run.md            members (agent, model), session ids, commit, request type, round count, anything missing
  brief.md
  round-1/
    <member>.md     one reply per member
  round-2/          only for a follow-up
    brief.md
    <member>.md
  votes/            only for a vote
    <member>.md
  decision.md
```

`<run>` is a date plus a short slug. Inside a git repository the directory is `agent-to-agent/` at
the repository root, kept out of commits through `.git/info/exclude` so no tracked file changes.
Outside a repository it sits in the directory the coordinator is working in.

Reply shapes, stated in the brief:

- **Advise:** recommendation, reasons, risks, what the member did not check.
- **Review:** findings, each with severity, `path:line`, and why it matters; then what was not
  reviewed.
- **Build:** branch name, what was done, what was run and its result, what is unfinished.
- **Vote:** the chosen member, reasons, weaknesses of the choice, points worth keeping from the
  others.

## Worktrees

One rule: **no member works in the coordinator's directory, and that directory never changes
branch.**

- Every member gets its own worktree at the commit named in the brief. Advise and Review members
  get a detached worktree. Build members get a branch `council/<run>/<member>`.
- Members always read a fixed **snapshot**, for Advise as well as Review. If the work is already
  committed, the snapshot is that commit. If it is not, the coordinator records the current state
  (tracked changes and new files) as a snapshot commit on a council-only ref,
  `refs/council/<run>/round-<n>`, without touching the operator's branch, index or working tree.
  The coordinator never commits to the operator's branch on the council's behalf. Snapshot refs are
  deleted when the run is closed. (Builder's call; the exact git commands are left to the builder,
  the requirement is that nothing the operator sees changes.)
- The run directory is not inside any member's worktree.
- Worktree location and creation follow `superpowers:using-git-worktrees` where it is installed.
- After a round, the coordinator runs `git status` in each read-only member's worktree and reports
  any change as a rule violation in `run.md`.
- When a run is closed, detached worktrees are removed. Build branches and their worktrees stay
  until the operator decides. Merging a Build branch is the coordinator's job and needs the
  operator's approval.
- A request with no repository involved uses no worktrees; members run in an empty scratch
  directory.

## Limits

- **Rounds:** one for Advise. Up to three for Review, after which the coordinator stops and brings
  the open findings to the operator. One for Build.
- **Time:** each member turn has a time limit (default 15 minutes, stated in the brief). A member
  that runs over or fails is recorded as "no answer" with the reason. It is not retried
  automatically.
- **Missing members:** the round continues with the rest and the gap is stated in `run.md`,
  `decision.md` and the report. A vote that falls below three ballots is reported as not held.
- **No recursion:** the brief tells members they may not launch other agents or start a council.

## Per-agent notes (starting point for `references/agents.md`)

Read from each CLI's help text on 2026-11-21 and from earlier live findings in
`CtrlCarlitos/skills` (`skills/deliver/references/cross-harness.md`,
`vendor/skill-creator/DISPOSITION.md`) and in `agent-council`
(`docs/superpowers/evidence/ac010-agy-installed-interface-research.md`). Nothing in this table has
been run for this skill yet.

| Agent | Start | Follow up | Model | Read-only for Advise and Review |
|---|---|---|---|---|
| Claude | `claude -p` | `--resume <id>`; `--session-id <uuid>` sets the id up front | `--model` | `--permission-mode plan` with `--permission-prompts none`: untested |
| Codex | `codex exec` | `codex exec resume <id>` | `-m` | `--sandbox read-only`: confirmed live as a real block |
| OpenCode | `opencode run` | `--session <id>` (creates it if new) | `-m provider/model` | default allows everything; a block needs operator-written config |
| Agy | `agy --print=<prompt>` | `--conversation <id>` | `--model` | no complete block; `--sandbox` covers the shell only |

What a member may be launched with (builder's calls):

- **Advise and Review members** read files and run read-only inspection commands such as
  `git diff` and `git log`. They do not run the test suite; the coordinator runs it and puts the
  result in the brief. Each agent is launched in the tightest read-only setting it has.
- **Build members (phase 2)** may be pre-granted edits and commands inside their own worktree only,
  through the agent's normal guarded setting (for example Codex `--sandbox workspace-write`, Claude
  `--permission-mode acceptEdits`). These are not bypass flags. Phase 2 confirms the exact settings
  before Build ships.
- **An agent that cannot do the job headless without a forbidden flag** is reported as unavailable
  for that request, the same way as a missing member.

Known hazards to carry into the reference file:

- OpenCode and Agy cannot be made fully read-only from the command line. Their members are
  read-only by rule; the separate worktree and the `git status` check are what limit and reveal a
  stray write.
- `opencode run` has been seen to hang with no output on this machine. The time limit covers it.
- Agy exits 0 on a denied tool and on a fired time limit, so the exit code proves nothing; read the
  result. A bare `--print` swallows the next flag; use `--print=`.
- A Claude member launched from a Claude coordinator needs the `CLAUDECODE` variable removed from
  its environment.
- A member turn can outlast the coordinator's own command time limit, so members are launched as
  background commands where the harness supports it.
- On a machine with guardrail, launching another agent, creating a worktree, or writing outside the
  session's repository may need the operator's approval or be denied. The skill follows the
  guardrail's answer and never works around it.

## What gets verified, in order

1. **Launch check (throwaway).** One trivial prompt per agent: start, capture the answer, capture
   the session id, resume once. This settles capture-versus-self-written replies per agent and
   fills in `references/agents.md` with commands that actually ran.
2. **Advise and Review**, with Claude as coordinator and at least two members, on a real question
   and a real diff.
3. **Vote**, with three members.
4. **Build**, phase 2, after the operator has used phase 1.

Coordinating from Codex, OpenCode or Agy is intended but stays marked unverified until each has
actually run a round.

## Where it lives

`skills/council/` in `CtrlCarlitos/skills`:

```
skills/council/
  SKILL.md                 when to use, the procedure, the rules
  references/agents.md     launch, resume, model and read-only notes per agent, with what was verified
  references/briefs.md     brief and reply templates per request
  evals/evals.json
```

Plus a row in the repository `README.md` and the install example. Invocation is explicit: the
skill description triggers on the operator asking for a council, a second opinion from other
agents, or a multi-agent review, and not on ordinary tasks.

## Archiving agent-council (separate step, needs the operator's go-ahead)

1. Tag the last `main` commit.
2. PR a README notice pointing to this skill.
3. Close the open issues and the three open Dependabot PRs with a pointer.
4. Archive the repository on GitHub.

Lessons from issue #46 (mailbox evidence) are carried into this design: launching replaces
watching, a member's message never carries the operator's authority, and decisions cite the reply
they came from.

## Left to the builder

- Exact wording of `SKILL.md`, the brief templates, and the evals.
- The `<run>` slug format and the scratch directory used when no repository is involved.
- How the coordinator extracts the final answer and session id from each agent's output, decided
  by the launch check.
