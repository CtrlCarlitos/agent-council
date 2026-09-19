# Bootstrap test evidence

Prepared: 2026-09-19. This records local tests, not a live GitHub deployment.

## Remote status

No remote repository, issue, PR, or ruleset was created by the preparing assistant.
The available connected GitHub actions were read-only. No alternate write-capable
connection was found, and no authenticated GitHub CLI was available in the runtime.
The operator must run scripts/publish.py --apply through their own authenticated gh.

## Local checks performed

- Go 1.23.2 on Linux/amd64: `go test -race -count=1 ./...` passed.
- Eleven top-level Go test functions, including seven invalid-ballot subcases.
- `go vet ./...` passed.
- `go build ./...` passed.
- `go run ./cmd/council about` correctly reports a foundation, not a running service.
- Python 3.13.5: sixteen bootstrap publisher unit/contract tests passed.
- All Markdown relative links checked; none broken.
- All five YAML configuration files parsed successfully.
- Roadmap dependency DAG, issue completeness, label/milestone references and rule
  shape validated offline.

The installed Go toolchain is older than the intended CI toolchain. CI is configured
for Go 1.27.1 on Linux/macOS/Windows, but those hosted jobs were not run. Downloading
a newer toolchain was unavailable in this environment. No current-toolchain or
cross-platform runtime validation is claimed from the local compilation results.

## Publisher tests: mocked GitHub, no account writes

The fake GitHub transport exercises creation, main protection before source
publication, twenty-two issue bodies and dependency references, label/milestone
read-back, an unmerged bootstrap PR, and remote-tree digest verification. It checks:

- Wrong user and unrelated existing repositories are rejected before mutation.
- Bad protection stops before issue/source publication.
- Rerun reconciles objects without duplicate issues/PRs/milestones/rulesets.
- User-edited issue text is preserved and flags verification failure.
- Missing labels, unexpected issue authors and altered source trees are rejected.
- Optional private-reporting failure becomes a warning, not a fake successful enable.
- Closed unmerged PRs are not reopened, and a missing recorded repository is not recreated.
- Source allowlist rejects traversal, secret-like paths, hashes changed after packaging,
  and symlinks. Issue cycles/unknown dependencies/duplicate keys are rejected.

These tests do not prove real GitHub permission scopes, ruleset API acceptance,
network reliability, hosted CI, branch enforcement, or native harness operation.

## Application boundary

Implemented: a small in-memory Go domain kernel for roster/ballot validation and
parked/pending session transitions. It is not durable, concurrency-safe, or an
authorization boundary. The daemon, SQLite store, native adapters, MCP bridge,
shared skill packaging, UI and external publishing features of Council remain issues.
The one-time repository bootstrap publisher is separate from Council's future
runtime GitHub integration.
