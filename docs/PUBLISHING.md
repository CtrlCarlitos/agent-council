# Publish the starting repository

## Prerequisites

Python 3.10+ and GitHub CLI (`gh`) authenticated to github.com as `CtrlCarlitos`,
with permission to create repositories, administer rules/Actions settings, create
issues, and write Git content/workflows. Use your normal `gh auth login` if needed.
Never paste tokens into a chat or put them in this package. No GitHub write access
was available to the preparing assistant; the remote operation was not performed.

From this extracted directory:

```sh
python3 scripts/publish.py          # offline payload integrity checks and plan
python3 scripts/publish.py --apply  # the only command that writes to GitHub
```

On Windows, use `py -3` in place of `python3` if that is your installed launcher.
You do not need Go, a coding agent, or local Git configuration to publish: files
are committed through GitHub's Git data API by your already authenticated `gh`.

## What the publisher does

1. Verifies an explicit file allowlist and SHA-256 hashes. It never sweeps your
   current directory, includes a .env, copies real sessions, or uploads .bootstrap.
2. Checks the authenticated owner. Refuses an unrelated existing repository.
3. Creates a public repository with a minimal GitHub-initialized README. Ensures
   its branch is called main, installs settings and rules, and reads them back.
4. Creates labels, five milestones, and 22 elaborated issues in dependency order.
   Issue bodies contain stable IDs and real dependency references. Retries reconcile
   known remote records rather than blindly repeating creation.
5. Submits all allowlisted source files on chore/bootstrap and opens a bootstrap PR.
   Main is already protected. The script does not auto-merge or weaken the rules.
6. Re-reads the settings, rules, issue mappings and PR; saves a local report under
   .bootstrap and prints actual returned URLs. CI may still be pending.

This order intentionally leaves the complete README on the bootstrap PR until you
review and merge it. The initial main README is just GitHub's auto-initialization.

## Failure and recovery

The script saves a local receipt with the repository ID and completed steps in
.bootstrap/state.json. Keep it when resuming. It does not store credentials.
Re-running checks identities and remote state. If a response is ambiguous, do not
manually repeat a create: re-run so existing issues/PRs can be reconciled.
If repository creation succeeded but no receipt was saved, the script refuses to
take over that existing repository. Inspect it manually; do not use a force option.

A failed ruleset/configuration check stops publishing. The remote repository may
already exist with incomplete settings; the script states that explicitly. It never
deletes the repository, disables protection to make progress, or reports a partial
setup as secured. Private vulnerability reporting is optional and reported separately.

GitHub API behavior and live permissions cannot be exercised offline. Mock tests
validate request sequencing and defensive checks, not real account administration.
