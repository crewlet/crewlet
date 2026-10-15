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
  identities (Atlassian/GitLab/Slack tokens) are separate service accounts —
  scope them minimally; the engine never needs a personal admin token at
  runtime (provisioning CLIs do need an admin credential, once).
- **The API's read surface is open by default.** Writes and every `/config`
  route require a token from `api.auth.tokens`; reads do not, so `/events`,
  `/agents/{id}/memory` and `/ws/stream` serve full LLM transcripts to anyone
  who can reach the port. That is a reasonable default for a laptop and a
  decision to make deliberately anywhere else — set
  `api.auth.allow_anonymous_read: false` to require a token for reads too, and
  never expose the API publicly with a dev-literal token.
- **Only the webhook and sandbox routes need to be public.** The routes
  outside parties call — `/webhooks/*` (vendor deliveries, verified by each
  vendor's signature or shared token) and the sandbox endpoints
  `/otlp/{token}` and `/mcp/{token}` (a signed, expiring per-run token in the
  path) — hold no operator credential. Setting `api.public.port` serves them
  on a listener of their own and nowhere else, and every other route,
  `/config` and `/secrets` included, only on `api.port`, so a deployment can
  publish the first and keep the second private without filtering paths in a
  proxy. The public listener requires no operator token, and answers every
  route it does not serve with the same `404` it gives a path nothing serves,
  whatever credential is sent — so the published socket neither reveals the
  admin routes behind it nor answers differently to a valid token
  (`docs/guides/deployment.md#exposing-webhooks-without-the-admin-api`).
- **`api.auth.company_writers` prevents drift; it is not a privilege
  boundary.** It refuses a change to a managed company document from any
  token it does not list, but every token still writes the secret store and
  can reload, so it can rewrite any value the document references through a
  `${VAR}` — a model key, a webhook secret, an endpoint. Do not issue a token
  to anyone you would not let change what the company does
  (`docs/concepts/configuration.md#managed-configuration`).
- **Trace context leaves the process with every Claude call.** Each call an
  `anthropic` provider makes carries a W3C `traceparent` (and a `tracestate`,
  when the trace has one) naming the turn's trace, to Anthropic or to whatever
  gateway `base_url` names — an identifier rather than content, but one a
  gateway can log and correlate. The Claude API nests its own spans under it
  and exports them to the OTLP destination an organization admin configures in
  the Claude Console, outside Crewlet; in that destination's full-content mode
  those spans carry prompts and answers, so its endpoint and credentials
  deserve the care the event store does. The engine's own spans of those calls
  carry metadata only, whatever an inherited `ANTHROPIC_OPEN_TELEMETRY_*`
  variable says (`docs/guides/deployment.md#spans-from-claude`). A text-mode
  `claude-code` cli-agent call is handed the same trace context and forwards
  it, to Anthropic or to the gateway its `ANTHROPIC_BASE_URL` names; it is
  handed no telemetry destination, so it exports nothing and never holds the
  collector's credential — but Claude Code's own managed settings on the
  engine host (`/etc/claude-code/` on Linux) can switch its telemetry on for
  every seat, outside the engine's control
  (`docs/concepts/subscription-llm-backends.md#trace-context`).
- **A coding box exports its agent's traces, through the engine.** With
  `CREWLET_SANDBOX_OTEL_RECEIVER_URL` set, a Claude Code run in a box is
  switched on to export its own spans to the engine's `/otlp/` receiver,
  which forwards them to your collector with the collector's credential added
  outside the box. Traces only: its metrics and its log events — its content
  channel — are switched off, and so is every switch that would put a prompt,
  a tool's arguments or output, or an answer on a span. A box image can bake
  none of that back on, but `role.sandbox.env` can, deliberately: it wins over
  every variable the engine sets in a box
  (`docs/concepts/code-sandbox.md#durability-completion-and-observability`).
- **Config encryption at rest** is available and recommended when your
  company config carries secrets — see
  `docs/concepts/configuration.md#secrets`.
- **Sandbox isolation.** Coding-agent runs execute inside an isolated sandbox
  (E2B); the sandbox boundary — not the coding agent's own permission
  prompts — is the isolation model. Treat anything you inject into a sandbox
  (tokens in `role.sandbox.env`) as visible to the code that runs there.
  E2B boxes are created with secured access, so a box's in-box agent runs a
  command or serves a file only for a holder of that box's access token,
  which the engine reads from E2B and never stores
  (`docs/concepts/code-sandbox.md#sandbox-backends`).
