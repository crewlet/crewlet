# ADR-0106: An SSH certificate reaches one host and level for minutes, and only a pinned host is dialled

- **Status:** accepted
- **Authority:** `internal/ssh`
- **Enforced-by:** `internal/ssh.TestACertificateCarriesTheLockedFields`, `internal/ssh.TestARequestOutsideTheLockedShapesIsNotSigned`, `internal/ssh.TestACertificateForOneHostIsRefusedByAnother`, `internal/ssh.TestAnOperateCertificateRunsOnlyItsForcedCommand`, `internal/ssh.TestAnUnpinnedHostKeyIsRefusedBeforeAuthentication`, `internal/ssh.TestTheClientNeverDialsTheEngineHost`, `internal/ssh.TestTheSetupChangesNothingForOtherUsers`, `internal/ssh.TestAReadCertificateGetsNoShellPtyOrForwarding`
- **Tag-status:** unreleased

## The decision

**Every certificate is signed for one use over a key pair made in memory for
it.** Its key ID names the agent and its turn (an approved call's id beside
them for operate, the person and the press for Test connection), its serial
is random and non-zero, its principals are `crewlet.<host>.<level>` for the
hosts it reaches (exactly one host for operate), it is valid from a minute
back for 15 minutes (read), 30 (read in a box), 5 (operate) or 2 (test), and
it carries no extension at all. An operate certificate's `force-command` is
the gate handed the approved command as base64, so the server runs those
bytes and nothing the client asks for.

**The engine dials only a registered server, and trusts only its pinned
keys.** The dialer judges the socket's own address by `httpx.ServerAddress`,
the client accepts only a host key byte-equal to a pinned one and offers only
the pinned algorithms, and a session carries one command with no PTY, agent,
environment or forwarding.

**The server enforces the rest, and its setup changes nothing for anyone
else.** The rendered setup trusts the authority for the Crewlet users alone,
forces the read user through the gate's allowlist, refuses plain keys, PTYs
and forwarding for them, and logs each certificate's key ID and serial.

## Why the obvious alternative is wrong

A certificate valid for a turn on every host, with the usual `permit-pty` and
forwarding extensions, is what `ssh-keygen -s` signs by default. Leaked, it
opens a shell on every server the agent holds, and tunnels from them, for as
long as it lasts. Trusting a server's key on first use hands the first answer,
from whoever gave it, the agent's certificate; pinning makes interception a
refused handshake before any certificate is sent.

## What this does not decide

Which tools sign certificates for agents and how an operate command is
approved, which are decided where they are built.
