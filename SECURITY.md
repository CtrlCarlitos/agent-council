# Security

## Status

This is a pre-release foundation, not a reviewed sandbox or a production service.
Do not run untrusted agent code with access to a real home directory or publish a
network listener until authentication and isolation tests have passed.

## Report privately

Use this repository's GitHub Security tab -> Report a vulnerability after private
vulnerability reporting is enabled. The publisher attempts to enable it and records
whether verification succeeded. If unavailable, open only a non-sensitive request
for a private channel; do not post exploit details, tokens, private transcripts,
or customer information in a public issue. No private contact address is invented.

## Design requirements

- Local control socket with owner-only access or an equivalently authenticated
  transport; no unauthenticated localhost control API, including MCP endpoints.
- Approved native authentication only; no provider credential collection service.
- Role-scoped capabilities, one controller lease, no recursive councils, finite
  resource limits enforced by ordinary code, and no bypass of agent-guardrails.
- Isolated worker filesystems, scoped code indexes, and independently enforced
  credential/tool permissions. Git worktrees are not a security boundary.
- Treat task text, repository content, proposals, HTML, and tool output as untrusted
  data. Neither artifact text nor a fake approval can authorize a write.
- Reconcile uncertain external operations before retrying. Preserve incomplete
  states rather than claiming exactly-once provider or GitHub execution.
- Secret-free fixtures and least-privilege CI, without pull_request_target workflows.

## Repository hardening

Rulesets, CI, and CODEOWNERS are controls, not a guarantee against a compromised
owner account. An administrator who can edit repository rules can change them.
There is intentionally no configured merge-rule bypass actor.
