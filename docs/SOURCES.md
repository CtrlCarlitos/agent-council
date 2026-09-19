# Source and verification notes

Product decisions derive from the operator's Agent Council design conversation.
No full user conversations, attached transcripts, private repository contents,
provider sessions, or credentials are included in this public scaffold.

Official references checked while preparing this scaffold on 2026-09-19:

- Ruleset API: https://docs.github.com/en/rest/repos/rules
- Rulesets: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/about-rulesets
- PR authors cannot approve their own PRs: https://docs.github.com/en/pull-requests/how-tos/review-pull-requests/approving-a-pull-request-with-required-reviews
- GitHub CLI API: https://cli.github.com/manual/gh_api
- Official Go downloads: https://go.dev/dl/?mode=json (Go 1.27.1 listed stable)
- actions/checkout v6 ref inspected via connected GitHub: d23441a48e516b6c34aea4fa41551a30e30af803
- actions/setup-go v6 ref inspected via connected GitHub: 924ae3a1cded613372ab5595356fb5720e22ba16

Native harness SDK versions, transport details, credential persistence and provider
terms must be rechecked during adapter implementation. No documentation statement
is treated as proof of local integration, and no unsupported API is invented.
