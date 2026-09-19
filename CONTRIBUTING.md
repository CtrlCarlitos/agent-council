# Contributing

The first goal is one real, recoverable four-contributor README council controlled
from an existing native harness session. See docs/ROADMAP.md for the dependency order.

## Before implementation

Choose an issue with explicit acceptance criteria. State the scope and owned files.
Coordinate shared interfaces before running parallel coding tasks. Do not infer
that an issue's proposal is a validated implementation; investigate and record evidence.

## Pull request workflow

Create a branch from current main, make a focused change, add tests, and open a PR.
Include the problem, behavior change, risks/permissions, checks actually run, and
known limitations. No direct main push, force push, or automatic merge. Resolve
review threads; maintain a linear history using squash merges. A reviewer working
under the same GitHub identity is not a separate approving GitHub reviewer.

The initial protection requires PRs and CI but zero independent approvals because
this is initially a single-maintainer workflow. Once another authorized reviewer
exists, the owner can deliberately require one independent approval. CODEOWNERS
identifies responsibility but does not impose a self-approval deadlock.

## Checks

```sh
gofmt -w cmd internal
go test -race ./...
go vet ./...
go build ./...
python3 scripts/verify_seed.py
python3 -m unittest discover -s scripts/tests -v
```

CI must not call paid providers or use personal harness credentials. Native
integration evidence is a separate, explicitly authorized activity with sanitized
fixtures. Do not claim all operating systems work because compilation succeeds.

## Secrets and disclosure

Never commit .env files, native provider session stores, logs from private projects,
MCP credentials, tokens, or SSH material. SECURITY.md explains private disclosure.

## Licensing

This project is licensed under the MIT License (see [LICENSE](LICENSE)). Contributions
are accepted under the same terms.
