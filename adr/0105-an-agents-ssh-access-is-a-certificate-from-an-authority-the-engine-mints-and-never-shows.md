# ADR-0105: An agent's SSH access is a certificate from an authority the engine mints and never shows

- **Status:** accepted
- **Authority:** `internal/ssh`
- **Enforced-by:** `internal/api/setupapi.TestTheCAIsMintedOnSaveAndNeverShown`, `internal/setup.TestAnSSHAuthorityThatIsNotOneIsRefused`, `internal/ssh.TestSSHAccessIsTheDocumentAlone`, `internal/ssh.TestOperateIsAStepUp`, `internal/config.TestEverySSHProblemIsReportedAtItsPath`, `internal/config.TestAnSSHSeatNeedsAHost`, `internal/accessmigrate.TestSSHIsBornMigrated`
- **Tag-status:** unreleased

## The decision

**Agents reach a company's servers only with OpenSSH user certificates signed
by one certificate authority the engine mints itself.** The connect form's Save
mints an unencrypted Ed25519 key (`setup.ShapeSSHCertificateAuthority`) and
seals it as `SSH_CERTIFICATE_AUTHORITY`; the document holds only the reference,
a value written inline is refused, and every answer renders only the
authority's public line, named by its own fingerprint. Servers trust that line
once. A rotation stages a second authority the engine never signs with until it
is promoted.

**A seat's access is the document alone.** Its level (`read` or `operate`, one
for every host it names) and its hosts live under `roles[].integrations.ssh`,
and each host, with its pinned host keys, under `integrations.ssh.hosts`. The
access sync takes no step and observes exactly what the document says, so
nothing outside the document can drift, and turning a seat or the integration
off stops the next certificate. Operate administers, so granting it needs a
step-up.

## Why the obvious alternative is wrong

A long-lived key pair per agent, sealed and written into each server's
`authorized_keys`, makes every grant and every revocation a change on every
server, leaves a key valid for ever in any box it reaches, and gives a
server's log only a fingerprint to trace. A certificate lasts minutes, names
the agent and its turn in its key ID, and carries a principal per host and
level, so a grant is a change to the document and a revocation is the next
certificate not being signed.

## What this does not decide

How certificates are signed and used (their validity, principals, critical
options and the engine's client), the server setup, and the agents' tools and
their approval: those are decided where they are built.
