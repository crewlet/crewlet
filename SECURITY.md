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
- **Every API key names its role, and a member key is safe to hand to a
  teammate.** A `role: member` key reads what the company published — its
  work, its pages, its chart, who is working on what — and acts as the person
  it is linked to; it is refused every prompt, transcript, diary, event and
  trace an agent produced, the spend, the configuration, the secrets and the
  fleet. A `role: admin` key reaches all of that and can change what the
  company does, so issue one only to the people and systems that run the
  engine. A caller with no key reaches the company's name, mission and chart
  (`api.auth.anonymous: public`, the default) or nothing (`none`), never a
  member's view. Never expose the API publicly with a dev-literal key
  (`docs/concepts/configuration.md#auth`).
- **Only the webhook and sandbox routes need to be public.** The routes
  outside parties call — `/webhooks/*` (vendor deliveries, verified by each
  vendor's signature or shared token) and the sandbox endpoints
  `/otlp/{token}` and `/mcp/{token}` (a signed, expiring per-run token in the
  path) — hold no API key. Setting `api.public.port` serves them
  on a listener of their own and nowhere else, and every other route,
  `/config` and `/secrets` included, only on `api.port`, so a deployment can
  publish the first and keep the second private without filtering paths in a
  proxy. The public listener requires no key, and answers every route it does
  not serve with the same `404` it gives a path nothing serves, whatever key
  is sent — so the published socket neither reveals the admin routes behind it
  nor answers differently to a valid key
  (`docs/guides/deployment.md#exposing-webhooks-without-the-admin-api`).
- **The role is the boundary; `api.auth.company_writers` only prevents
  drift.** It refuses a change to a managed company document from any admin
  key it does not list, but every admin key still writes the secret store and
  can reload, so it can rewrite any value the document references through a
  `${VAR}` — a model key, a webhook secret, an endpoint. Do not issue an admin
  key to anyone you would not let change what the company does; give them a
  member key (`docs/concepts/configuration.md#managed-configuration`).
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
