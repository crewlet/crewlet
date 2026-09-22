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
