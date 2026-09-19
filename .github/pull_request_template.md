## Problem and scope

Closes # (implementation issue only, not merely a related discovery).

## What changed

## Risks and permissions

Describe process, filesystem, credentials, provider spend, Git/GitHub effects,
and operator-approval changes. Explicitly state when there are none.

## Evidence

- Checks actually run:
- Native integration evidence (or explicitly not tested):
- Limitations / remaining findings:

## Review checklist

- [ ] Acceptance criteria are met or exceptions are explicit.
- [ ] Tests protect the changed invariant.
- [ ] No credentials/private sessions/customer data in the diff.
- [ ] No permission bypass, silent model fallback, or recursive council.
- [ ] Synthesis/review evidence applies to this exact artifact revision.
- [ ] Changes are on a task branch; no direct main push or automatic merge.
