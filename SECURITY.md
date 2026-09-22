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
- **Session cookies are signed with the Tier A keyring, and a deployment
  without one cannot serve sign-ins.** The engine refuses to build a session
  signer from a keyring that cannot sign for the fleet, rather than falling
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
  grants and nothing else. `api.auth.disabled`, which authenticated the *empty*
  credential into full operator authority with no bind check anywhere, is gone
  with it.
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
- **Config encryption at rest** is available and recommended when your
  company config carries secrets — see
  `docs/concepts/configuration.md#secrets`.
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
- **Sandbox isolation.** Coding-agent runs execute inside an isolated sandbox
  (E2B); the sandbox boundary — not the coding agent's own permission
  prompts — is the isolation model. Treat anything you inject into a sandbox
  (tokens in `role.sandbox.env`) as visible to the code that runs there.
