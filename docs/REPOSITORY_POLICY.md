# Repository policy

Target: CtrlCarlitos/agent-council (public). These are requested settings, not a
claim that the remote repository exists. scripts/publish.py installs and reads
back them through the operator's already authenticated GitHub CLI.

| Setting | Planned value |
|---|---|
| Default branch | main |
| PR required | Yes |
| Required independent approvals | 0 initially; owner merges |
| Required status | ci, bound to the GitHub Actions app |
| Must test current base | Yes |
| Review thread resolution | Required |
| Merge method | Squash only |
| Linear history | Required |
| Force push / delete main | Blocked |
| Bypass actors | None, including no admin merge bypass |
| Actions default token | Read-only; cannot approve pull requests |
| CODEOWNERS | @CtrlCarlitos; advisory until another reviewer exists |
| Branch cleanup | Delete branch on merge |
| Auto-merge | Disabled |
| Private vulnerability reporting | Attempt enable + read-back, report any failure |
| License | Explicit owner decision pending |

The required CI workflow must exist on the proposed branch before merging. Its
aggregate job is named exactly `ci` and fails if any platform check fails or is
cancelled. The ruleset does not claim protection until its installed JSON is
read back. A configuration document or successful API write alone is not proof.

Rules are editable by authorized repository administrators; no file prevents that.
See docs/SOURCES.md for the official GitHub rule semantics and solo-review limit.
