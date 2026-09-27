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
- **Session cookies are signed with the Tier A keyring, and a node without
  one does not start.** Every node needs a keyring (`crewlet validate`
  refuses a file without one), and the engine refuses to build a session
  signer from a keyring that cannot sign for the fleet rather than falling
  back to a per-process key: with a fallback, every ingress node would accept
  only cookies it minted itself, so a browser would be signed in on whichever
  node its request happened to reach. The cookie is `HttpOnly`, `Secure`,
  `SameSite=Lax` and carries the `__Host-` prefix on any deployment whose
  `api.external_url` is not plain http, which is what stops a sibling
  subdomain writing one. It carries no login, no address and no grants —
  only ids, two deadlines and a MAC — so a cookie recovered from a proxy log
  discloses nothing and authenticates nothing. **Dropping a keyring entry ends
  every session signed under it**; see the rotation runbook in
  `docs/concepts/secret-store.md`.
- **A sign-in endpoint discloses nothing about who exists.** The refusal is
  one generic error for every arm — no such login, wrong password, wrong
  second-factor code, code already spent — because telling a caller which one
  applies tells an attacker the same, and the first of them is the company's
  roster. Three mechanisms keep the timing from saying it instead: admission
  is keyed on the request's SOURCE and happens before the subject is resolved
  (a throttle keyed on who you claim to be is one only real people can
  trigger, so the 429 becomes the oracle); a subject that does not exist is
  still verified against, with a fixed-cost decoy; and both arms answer at one
  deadline measured from the instant the request arrived. Under enough load to
  push a real verification past that deadline the arms separate again — stated
  rather than hidden, and at that point every request on the node is slow.
- **Passwords are argon2id at 64 MiB, t=3, p=1, with a twelve-character
  minimum and no composition rules.** The parameters are in the stored
  verifier, so raising the cost re-hashes each person's on their next
  successful sign-in — the only instant a stronger digest can be computed,
  because the plaintext is not stored. Machine tokens and recovery codes are
  SHA-256 rather than argon2id, deliberately: both are minted by this engine
  from `crypto/rand`, so there is no dictionary to grind and the memory cost
  would buy nothing while adding a hundred milliseconds to every request a CI
  job makes.
- **An identity provider's assertion is never a link by address.** A person is
  bound to a provider subject by an invitation somebody issued or by an
  administrator — never because the provider asserted an address that matches
  an existing person's. At most providers a user can set their own address, so
  an email match is a claim the attacker controls, and the person it would
  link them to is whoever is most worth becoming. There is deliberately no
  `auto_provision` setting: it is the same decision written as a field, and a
  field is how it ends up on by accident.
- **Every API route requires a credential, reads included.** `allow_anonymous_read`
  is gone: it served `/events`, `/agents/{id}/memory` and `/ws/stream` — full
  LLM transcripts, diary entries and the roster — without one, by default, and
  it could not be closed durably because it was an `omitempty` bool whose safe
  value was its zero, so `false` did not survive an export round trip. A
  deliberately public reader is a named `api.auth.tokens` entry holding read
  grants and nothing else. What stays reachable without one is a short fixed
  list — the probes, the signed webhook edges, the per-run token paths, and the
  dashboard shell, which ships no data — and it is a list of *exemptions*
  rather than a posture anything can widen.

  `api.auth.disabled`, which authenticated the *empty* credential into full
  operator authority with no bind check anywhere, is gone with it.
  `crewlet run -dev-principal <login>` is what replaces it for local work, and
  it is a **flag rather than a config field** because a field reaches
  production by being copied into an image. It is refused unless `api.host`
  binds loopback *and* the binary is a development build, grants
  `api.auth.max_grants` and no more, and logs a warning on every boot that
  enables it.
- **Carrying a credential is not the same as being allowed to use it.** Every
  route and every socket question declares one of
  [eleven grants](docs/concepts/identity-and-access.md#grants-the-eleven-things-there-are-to-allow),
  and both transports are decided by the same registry, so a question cannot be
  reached by choosing a channel. A route registered with no grant is a build
  failure rather than a route that answers to anyone.
- **`config:write` is running code on every engine host.** A stdio
  `mcp_servers` entry is a `command` the engine starts as its own user on every
  node that runs seats; a `cli-agent` provider names a binary it runs; a
  `run_in: direct` sandbox cell runs a coding agent, and its setup steps, as
  that same user; and a seat's runtime half — its model chain, credentials,
  sandbox cell and `mcp_env`, written through `/chart` — takes the same grant.
  Whoever holds it can therefore read whatever the engine's user can: the
  Tier A file, the keyring that signs every session, and every credential the
  store holds. It is deliberately one grant, and this is what it confers: give
  it as you would give a shell on those hosts. Every write under it that can
  start a process — the company document, the chart's runtime half, connecting
  an integration — asks for a recent step-up, so a stolen session cookie alone
  does not reach it.
- **A child process is handed an allowlisted environment, never the
  engine's.** The engine's environment is where Tier A's `${VAR}` references
  resolve from — the keyring, every `api.auth.tokens` value, the identity
  provider's client secret, any credential in an external `stream.url` — and
  where the engine reads its collector credential (`OTEL_EXPORTER_OTLP_HEADERS`),
  often beside an operator's own provisioning tokens (`GITLAB_ADMIN_TOKEN`,
  `MATTERMOST_ADMIN_TOKEN`); so no process the engine starts inherits it: a
  stdio MCP server, a coding CLI, a local sandbox's coding agent and the
  container runtime's own CLI each get `PATH`, locale, TLS trust and proxy
  settings, the host user's home and temporary directories where they run
  in no box of their own, and what their configuration declares. That keeps the
  engine from *handing* its secrets to code that never asked for them. It is
  not isolation: a child runs as the engine's user and can read what that user
  can, `/proc/<engine pid>/environ` included. A tool server you do not trust
  needs a different user or a container around it.
- **A cross-site write is refused by its `Origin`.** CORS decides who may
  *read* an answer; on a state change that is the part an attacker does not
  need, so a separate check refuses a non-read whose `Origin` is not an address
  this deployment is reached at. An absent `Origin` is allowed for a bearer
  client — a cross-site page cannot make a bearer travel — and refused for a
  cookie-authenticated request, which a browser would always have sent one on.
- **A node that cannot read identity answers `503`, never `401`.** The
  principal a node could not check and the principal that presented nothing are
  the same empty value, and reporting the first as the second tells everybody
  holding a valid credential that theirs is invalid for the length of the
  outage — which is how a company gets taught to reset working passwords during
  one.
- **`api.auth.max_grants` is the ceiling, and it is required.** A person's
  grants live in the replicated store and an identity provider's group mapping
  is written at the provider — neither is in a tier this deployment's operator
  controls. This is the bound on what either may confer, stated in the tier
  that holds the keyring. It is intersected per node, per request, so lowering
  it needs no write and no restart of the fleet; each node publishes a hash of
  its own so a mixed fleet mid-rollout is visible rather than silent.
- **A Tier A token is a real credential and is checked as one.** Each entry
  states its own `grants` — required and non-empty — and its value must clear
  a 26-character floor on what the `${VAR}` *resolves to*, so a reference
  cannot be the way around the rule. At least one is required on every
  backend: a fresh deployment's identity estate is empty, so it is what creates
  the first person, and on a running one it is the way back in when the
  identity provider is down.
- **The company configuration is always sealed at rest.** Every revision is
  encrypted and authenticated under the Tier A keyring before it reaches the
  store — there is no plaintext mode, and a node reads only sealed revisions —
  so a copied store file, a backup or a volume snapshot carries ciphertext
  alone. The keyring is the root of trust: keep it out of the store's backup
  domain. See
  [Encrypted at rest, and authenticated](docs/concepts/configuration.md#encrypted-at-rest-and-authenticated).
- **Removing a person destroys their key, not only their row.** Each person's
  name and address are sealed under a data key that is theirs alone, and
  removing them destroys it — so those values become unrecoverable at once
  from every copy that already exists: the identity log, donated snapshots,
  backups and every node. What outlives the removal is deliberate: their id,
  the tombstone (who removed them, when, and which claims they held — an
  address as its blind, never its value), and the audit trail's rows naming
  them, because a history whose authors evaporate is not an audit trail.
  **Their login outlives it too, in the clear**, and nothing destroys it: it
  is on the removal record in the identity log (so in every backup and
  donated snapshot), in the tombstone's `iam_removed.claims_json`, and in the
  audit rows that record an unbound person's changes under it. A login is
  deliberately not sealed — it is printed beside everything its holder does —
  and the one the sign-up form proposes is derived from the address
  (`jane.doe@example.com` proposes `jane.doe`), so after a removal the
  address's local part is usually still readable. If a person's login has to
  be erasable, do not derive it from a personal address: give them one that
  names nothing about them. The rows commit before the key is destroyed; a key deletion that fails — a
  coordination outage — is retried by the identity key duty, and `crewlet iam
  check` (`GET /iam/check`) names every removed person whose key still lives
  as `removal_key_live` until it is gone. See
  [Removing somebody destroys a key, not a row](docs/concepts/identity-and-access.md#removing-somebody-destroys-a-key-not-a-row).
- **Personal data in configuration revisions written before this release.**
  Every node keeps its own copy of every company-config revision it has ever
  met, in an append-only table that nothing deleted from and that is in every
  backup of that node. The org chart used to live inside that document, so a
  human seat's `email` and `contact` account ids are archived in every
  revision that carried them — and removing the seat never reached them,
  because the removal writes a *new* revision. Revisions written after the
  chart moved onto its own log carry no chart at all, so nothing new enters
  the archive. Run `crewlet config scrub` **on every node** to erase what is
  already there; it refuses the active revision, which is edited instead. It
  does not reach backups taken before the run, so treat those as still
  holding the original revisions and apply your own retention to them. Going
  forward the revision table is also swept: 400 days, plus the active
  revision and its parent chain (see
  `docs/guides/retention.md#the-configuration-archive-and-the-one-thing-a-purge-cannot-reach`).
- **Sandbox isolation depends on the cell a seat runs in.** A coding agent
  runs fully permissioned — the sandbox boundary, not the agent's own
  permission prompts, is the isolation model — so where it runs is the
  security decision. `run_in: e2b` is a remote VM per run and `run_in:
  container` a Docker or Podman container on the engine host; `run_in: direct`
  is a process tree running **as the engine's own user on the engine host**,
  which isolates each box's state and not the host — it can read what that
  user can. Use `direct` on a workstation or a dedicated VM, never where the
  work is untrusted. Treat anything you inject into a sandbox (tokens in
  `role.sandbox.env`) as visible to the code that runs there. See
  [Code Sandbox § Local sandboxes](docs/concepts/code-sandbox.md#local-sandboxes).
