# Security Policy

## Supported versions

Security fixes go to the latest release and `main`; there are no backports to
older releases.

## Reporting a vulnerability

Please report vulnerabilities **privately** via
[GitHub's private vulnerability reporting](https://github.com/crewlet/crewlet/security/advisories/new)
("Report a vulnerability" on the repository's Security tab). Do not open a
public issue for anything you believe is exploitable.

Include what you can: affected version/commit, a reproduction or proof of
concept, and the impact you believe it has. You should receive an
acknowledgement within a few days; we'll keep you updated as we triage and
fix.

## Scope notes for operators

A few things worth knowing when deploying Crewlet:

- **Agent credentials are per-seat by design.** Each agent's external
  identities (Plane/GitLab/Slack tokens) are separate service accounts —
  scope them minimally; the engine never needs a personal admin token at
  runtime (provisioning CLIs do need an admin credential, once).
- **The API's read surface is open by default.** Writes and every `/config`
  route require a token from `api.auth.tokens`; reads do not, so `/events`,
  `/agents/{id}/memory` and `/ws/stream` serve full LLM transcripts to anyone
  who can reach the port. That is a reasonable default for a laptop and a
  decision to make deliberately anywhere else — set
  `api.auth.allow_anonymous_read: false` to require a token for reads too, and
  never expose the API publicly with a dev-literal token.
- **Config encryption at rest** is available and recommended when your
  company config carries secrets — see
  `docs/concepts/configuration.md#secrets`.
- **Sandbox isolation.** Coding-agent runs execute inside an isolated sandbox
  (E2B); the sandbox boundary — not the coding agent's own permission
  prompts — is the isolation model. Treat anything you inject into a sandbox
  (tokens in `role.sandbox.env`) as visible to the code that runs there.
