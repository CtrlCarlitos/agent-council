# ADR-0002: PR-only main, solo-maintainer review policy, license pending

Status: accepted bootstrap defaults, pending live installation.

Use an active main ruleset with no bypass actors, required PR, resolved review
threads, required CI, linear history, no force-push, and no branch deletion.
Allow squash merge only. Restrict default Actions permissions to read and do not
permit Actions to approve PRs. Pin workflow actions to verified commit SHAs.

Initially require zero independent GitHub approvals: the owner and all local agents
may use the same account, which cannot approve its own PR. This does not allow
direct pushes or skip CI. CODEOWNERS routes responsibility without an impossible
required self-review. The owner explicitly merges. Raise approvals only after a
second authorized identity is available; never manufacture review identities.

Bootstrap creates a GitHub-initialized main first, protects it, then submits the
actual scaffold through a PR. It does not create an admin bypass to seed files.
An owner able to edit settings can later change rules; the policy does not pretend
it constrains a compromised administrator.

The repository is public but licensing remains undecided. Do not select a permissive
or copyleft license on the owner's behalf while commercial/SaaS options are open.
