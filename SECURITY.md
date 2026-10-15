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
  (`docs/concepts/subscription-llm-backends.md#trace-context`). With in-box
  telemetry on (`CREWLET_SANDBOX_OTEL_RECEIVER_URL`, below), a Claude Code
  run in a coding box sends it too: on each Messages call to Anthropic or the
  gateway its `ANTHROPIC_BASE_URL` names, and on each MCP tool call to every
  HTTP MCP server `role.sandbox.mcp.servers` renders into the box — a
  vendor's included — naming the turn's trace and the box's tool span, which
  that server can log and correlate across the seats and turns that share a
  trace. One Claude Code switch governs both, and on the first-party API its
  tracing beta sends the header without it, so neither can be kept from one
  and given to the other; list only servers you would show the trace id to
  (`docs/concepts/code-sandbox.md#durability-completion-and-observability`).
- **A coding box exports its agent's traces, through the engine.** With
  `CREWLET_SANDBOX_OTEL_RECEIVER_URL` set, a Claude Code run in a box is
  switched on to export its own spans to the engine's `/otlp/` receiver,
  which forwards them to your collector with the collector's credential added
  outside the box. Traces only: its metrics and its log events — its content
  channel — are switched off, and so is every switch that would put a prompt,
  a tool's arguments or output, or an answer on a span. The engine sets that
  where a box image's or template's own settings cannot undo it — the run
  environment over the image's `ENV` and over the login profile the box's
  setup steps and coding job run under, and the same values as Claude Code
  flag settings over a
  `~/.claude/settings.json` in the box — but NOT over a Claude Code MANAGED
  settings file (`/etc/claude-code/managed-settings.json` on Linux), which
  Claude Code applies above every other layer: an image or template that
  ships one, or the engine host's own for a `direct` box, can switch content
  capture and log events back on or send the export to another collector.
  Do not ship one in a box image or template. `role.sandbox.env` wins over
  every one of those telemetry variables, deliberately, so a seat can switch
  its agent's export off or point it at a collector of its own; it does not
  win over what a box sets for its own isolation — a local box's `HOME` (and
  a `direct` box's XDG directories and `TMPDIR`), a container launch's
  `IS_SANDBOX=1` (`docs/concepts/code-sandbox.md#credentials-and-the-run-environment`).
  The box holds a token in its endpoint, readable by whatever runs there,
  valid for a day after the launch:
  with it, code in the box can send spans in the trace its run was launched in
  into your telemetry backend through the receiver until it expires, and
  nothing else — never the backend's credential, never a metric or a log
  record (the receiver serves traces only), and never a span in any other
  trace, which the receiver refuses. The scope is that TRACE, not the run: a
  trace can also hold other seats' turns and coding runs — an agent-to-agent
  ask and the turn that answers it, every seat one webhook delivery woke — and
  the token admits spans among theirs too
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
