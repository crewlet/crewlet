# ADR-0075: A backup carries every object the log it replays names

- **Status:** accepted
- **Authority:** `internal/backup`
- **Enforced-by:** `internal/backup.TestABackupCarriesAnObjectNamedAfterItsCopy`, `internal/backup.TestTheObjectStreamIsSnapshottedAfterEveryOtherStream`, `internal/backup.TestABackupRefusesALogRecordItCannotRead`, `internal/objstore/references.TestEveryRequiredTableSaysHowItsLogNamesObjects`, `internal/engine.TestEveryRequiredTableNamesItsDomainsLog`, `internal/worklet.TestAFileRecordNamesTheObjectItsRowNames`
- **Measured:** the pinned broker's stream snapshot (nats-server v2.15.0, `server/stream_backup.go`) takes the stream's state when the request arrives and copies records only through that state's last sequence, clamping its consumers there too. A stream snapshotted while a publisher appended 1,647 records during the transfer restored to exactly the last sequence the snapshot's answer stated (120,361), not the 121,647 the source held when the transfer ended (2026-10-11; four earlier runs of the same program, with 778 to 1,394 records appended, agreed)
- **Cost-when-tried:** the backup carried only the objects its store copy's rows name. The copy is taken first and the streams after it, so every file uploaded and named while the backup ran was a row the restored log writes and an object no backup carried: a restore into an empty bucket brought those files back with no bytes, and nothing said so until the collector's next daily audit. The same reading listed as LOST every object of the copy whose file a record after the copy had removed and the collector had then deleted — files no restored row names
- **Tag-status:** unreleased

## The decision

A restore brings the store copy back at position P on each declaring log and
replays the log from P to the last sequence L the snapshot captured — and a
build that can read a record the copy's applier deferred applies that too. So
the objects the restored rows name are exactly the copy's Required rows'
objects overlaid by what the records of (P, L], and the deferred ones, write.
A backup carries the copy's set (read before the streams are snapshotted) and
every object any of those records names (read after), and records an object it
could not carry as lost only where the restored rows name it: an object the
copy names whose row a later record replaced or removed is owed by nothing.
Every record is read, applied or not, so whatever a restore's applier drops the
set carried is never short of what it names; and a record this build cannot
read that may write a declared table refuses the backup, since it may name an
object nobody can tell. Whether it may is answered from what every build reads
of the record — a newer version or a kind this build has never heard of is
judged by what its writer stated it may touch — never from what this build's
applier does with its kind, which says nothing about a newer build's.

So a Required declaration in `internal/objstore/references` also names its
domain's log (`ReferenceTable.Log`), the table its applier defers that log's
records into (`ReferenceTable.Deferred`) and how one record names its objects
(`ReferenceTable.Records`) — the declaring domain's own decoder, the one its
applier writes the row from. The object stream (`nats`) is snapshotted after
every log, so an object a captured record names was in the bucket before the
bucket's snapshot began. It binds the declaration (`objstore`), the gates
holding every Required table to it (`objstore/references`, `engine`), each
declaring domain (`tracker`'s reader of a file record) and the reading
(`backup`).

## Why the obvious alternatives are wrong

**Wait for this node's applier to reach L, then read the live rows.** The
applier keeps applying while the backup waits and commits records in runs, so
the read lands at some R ≥ L that nothing can pin at L: a file written during
the copy and rewritten between L and R is named by the restore and by neither
read — the hole moved rather than closed. It also needs an applier, which a
node in maintenance or seal mode does not run, so a backup there would wait
out its budget.

**The copy's set alone** — the Cost line.

**Every object the store holds.** It carries the bucket's garbage and the
retired and derived objects ADR-0074 says no backup carries, and lists the
whole namespace on every run.

**Apply (P, L] into the copy.** A second applier outside the framework, and a
copy whose position no longer describes the file the stores were verified as.

## What this does not decide

The restore procedure (`docs/guides/backup.md`). The external-NATS topology,
where the node takes no snapshot and so has no L: its backup carries what its
copy names, and the cluster's own backup is taken at another moment. How a
domain encodes its records. ADR-0074's standing rule, which is unchanged — a
retired or derived table is neither walked nor read past its copy.
