# ADR-0076: A stream's identity is the instant its creator stamped, and one function reads it

- **Status:** accepted
- **Authority:** `internal/statelog`
- **Enforced-by:** `internal/statelog.TestOnlyStreamCreatedAtReadsAStreamsCreationInstant`, held able to fail by `internal/statelog.TestTheStreamInstantWalkJudgesAKnownTree` and kept current with the NATS packages by `internal/statelog.TestEveryCreatedFieldOfTheNATSPackagesIsClassified`; `internal/queue/jetstream.TestAStreamsIdentityGoesWhereItsConfigurationGoes`; `internal/statelog.TestAStreamIsIdentifiedByTheInstantItsCreatorStamped`; `internal/engine.TestTheApplierIsHandedTheStreamsStampedIdentity`; `internal/learning/memsync.TestARestoredChangelogIsTheOneThisNodeSaw`; and a real restore of a backup's snapshots, `internal/backup/backuptest.TestARestoredStreamHoldsWhatWasSnapshotted`
- **Measured:** nats-server 2.15.0's solo restore dated a stream created at 20:03:35.088 to 20:03:35.772, every record and its storage instant identical; its clustered restore assigns the stream `time.Now()` (`jsClusteredStreamRestoreRequest`). The gate reads 25 files and 18 packages in 0.46 s, 1.8 s under the race detector.
- **Cost-when-tried:** every consumer of a stream's identity read the broker's instant on its own — each state log's applier through `DomainLog.Stats`, the memory changelog's hydration and the backup's read past its copy — so a node restored exactly as `docs/guides/backup.md` said found every log "recreated": its appliers stopped, its reads and writes refused `wrong_stream`, the memory changelog refused every seat, and the restored company served nothing until an operator had re-anchored every log (whose recreated case then read a log holding the whole history as one holding none of it) and cleared every node's identity rows by hand. All three passed every test that was not a restore.
- **Tag-status:** unreleased

## The decision

A stream's identity — what a node's checkpoint, its memory changelog's
identity row and a backup's copy are keyed to — is the instant the create that
made the stream stamped into the stream's CONFIGURATION, as the metadata key
`crewlet.created_at` (`statelog.CreatedAtKey`, written by
`statelog.StampCreatedAt`). `queue/jetstream`'s `ensureStream` stamps every
stream it creates, minted once per ensure so a retried create asks for the same
stream.

`statelog.StreamCreatedAt` is the ONE reading: every reader of a stream's
identity goes through it, and no other non-test file selects the `Created`
field of a stream's information — the client's `jetstream.StreamInfo` or
`nats.StreamInfo`, the server's `StreamInfo`, `StreamDetail` or
`FileStreamInfo`, or any type that embeds one. A stream carrying no stamp — one
an operator made by hand, a coordination bucket, the object store's — answers
the broker's own instant, which is what it always answered; a stamp that does
not parse is an error, never the broker's instant instead, because unknown is
not same.

So a snapshot restore, which hands the broker the configuration the snapshot
recorded, is the SAME stream, judged by where it ends and what it holds at the
checkpoint; a stream the engine provisions again after it was lost is stamped
anew, which is the recreation the identity exists to detect; and a stream an
operator remakes from a configuration that still carries the stamp is declared
the same history, deliberately — removing the key is how a rebuild is made to
read as a recreation.

## Why the obvious alternatives are wrong

**Keep reading the broker's instant.** It names one INSTANCE of a stream, and a
restore is a new instance of the same history: the instant then identifies the
restore rather than the records, and a node restored from its own backup
refuses its own log.

**Carry the broker's instant through the restore.** The restore request names
a configuration and a state and nothing else, the server dates the stream it
makes (measured on its solo path, read in its code on the clustered one), and
no client request sets a stream's creation instant. The configuration is the
one thing a restore is told to keep.

**Identify a stream by its records instead** — its first sequence, its count,
the record at the checkpoint. A recreated stream's sequences are perfectly
plausible and an empty stream and an emptied one have the same count. The
record at the checkpoint does separate two histories once the log is written
past it (`statelog.ErrLogDiverged`), but it cannot tell a log that was
RESTORED from an older copy from one that was RECREATED, and the two are
re-anchored oppositely: a restored log is followed from its end, a recreated
one from its first surviving record.

**A convention each reader keeps.** That is what there was: three readers, one
rule, written three times, each correct until the day a restore was tried.

## What this does not decide

How a recreated, restored or diverged log is re-anchored
(`statelog.Reanchor`, `crewlet retention reanchor`), or what a node does while
its log is not its own — `statelog`'s identity findings. Whether streams no
identity check reads — the coordination buckets, the object store's, the
mailboxes — carry a stamp: none of them is keyed to its instant, and a stream
that ever is must be stamped by its creator first.
