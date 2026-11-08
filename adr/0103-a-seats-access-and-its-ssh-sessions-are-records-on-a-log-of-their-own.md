# ADR-0103: What a seat is told about its access, and every SSH certificate and command, are records on a log of their own

- **Status:** accepted
- **Authority:** `internal/accessdomain`
- **Enforced-by:** `internal/accessdomain.TestEveryAccessRecordIsSigned`, `internal/accessdomain.TestAnAccessRecordAppliesOnceOnEveryNode`, `internal/accessdomain.TestACommandIsSealedAtRest`, `internal/accessdomain.TestAccessRecordsAreKeptForTheirRetention`, `internal/accessdomain.TestTheAccessDomainIsACertifiedDomain`, `internal/estate.TestACommandCrossesTheRouterSealed`
- **Measured:** about 2 MB a day at the reference company's 100 seats, ten of them at SSH: thirty dispatch barriers a seat a day of about 140 bytes each, a told record of about 8 KiB whenever a seat's access changes, and about one certificate and three commands of about a kibibyte each a turn at SSH. The log's 128 MiB default holds about two months of a completely blocked trim.
- **Tag-status:** unreleased

## The decision

**The record of a seat's access is a state-log domain, `access`, on its own
stream, every record signed and applied into replicated tables on every data
node.** It holds three kinds of record, each created once on a subject of its
own: what one turn of a seat was told about its access (the snapshot it ran
under and the changes since its previous told record), one SSH certificate the
engine signed (its public facts, never a key), and one SSH command the engine
ran or a person denied. A seat's newest told record is its baseline: its next
turn, dispatched on any node, diffs against it, read at `linearizable`.

**A command's text is sealed under the fleet keyring before it leaves the node
that ran it**, bound to its record, written through by the applier and opened
only by an administrator's audit read. Its stdin and output are kept as their
sizes and SHA-256 digests alone.

**Records are kept for the company's `access_history.retention_days`** (400 by
default, 90 to 3650) and expired an hour at a time by the trash reaper, every
node deleting by the broker instants the rows hold; each seat's baseline stays
until the company removes the seat, and a removed seat's told records go with a
record that keeps a turn racing its removal from being told into it.

## Why the obvious alternative is wrong

The event store keeps a turn's detail for 30 days on the node that ran it, so
an audit asked of another node, or a year later, finds nothing, and a seat that
moved diffs against nothing. Records on the executor log would put an audit
trail on the conversation-session horizon and freeze that log's apply on any
record of this one a build cannot read. The coordination store holds current
values, not a history. And a command in the clear in replicated rows would be a
second copy of every production command on every node's disk and in every
backup.
