# ADR-0073: A seat's executor session is a sealed object its turn's row names

- **Status:** accepted
- **Authority:** `internal/executordomain`
- **Enforced-by:** `internal/objstore/references.TestEveryTableThatNamesAnObjectIsDeclared`, `internal/objstore/references.TestTheGraceOutlastsTheLongestUnrecordedUpload`, `internal/executordomain.TestTheExecutorDomainIsACertifiedDomain`, `internal/executordomain.TestASessionOpensOnlyWhereItsRowNamesIt`, `internal/executordomain.TestACaptureOfAnObjectPastRecordWithinIsRefused`, `internal/executordomain.TestALateCaptureNeverRollsASessionBack`, `internal/executordomain.TestAnEndedTurnTakesNoFurtherCapture`, `internal/executordomain.TestAnEndAbsorbsExactlyWhatItsFirstReadHandedOut`, `internal/executordomain.TestAHeadShowsEveryTurnNoEndedTurnWasShown`, `internal/executordomain.TestARemovedSeatStaysRemoved`, `internal/executordomain.TestTheSeatBudgetReleasesTheOldestEndedSessionsFirst`, `internal/engine.TestTheExecutorSourceNeverEndsOrAgesOutALiveRun`, `internal/engine.TestTheExecutorLogFitsBesideTheNineAtTheOperatorsSmallestVolumes`, `internal/estate.TestASeatsOnlyNodesCaptureIsRecordedThroughADataNode`, `internal/estate.TestAConversationReadWaitsForAnEndAnotherNodeMade`, `internal/estate.TestEveryExecutorWriteIsTheEngines`, `internal/estate.TestNoSurfaceHoldsTheEnginesExecutorWrites`
- **Measured:** a harness's session of a day's work compresses to one or two mebibytes and is bounded here at 32 MiB; at the reference company's 3 000 turns a day the log that names the sessions grows about 29 MB a day, while the sessions themselves would be gigabytes; crewlet-operator's smallest data volumes leave 704 MiB (11Gi, objects in S3) and 320 MiB (14Gi, beside the nats bucket) above the nine logs before this one, and this log, scaled to its 64 MiB floor there, still fits both — the minimum classes do not move, but the least a fresh node boots in rises by 64 MiB of logs (10916375210 bytes of broker on nats, up 85.3 MiB with the bucket's quarter beside it, and 64 MiB on S3), which is what the operator's own floors follow. At that floor the log holds about two days at the reference company's rate, under the seven-day `min_age` a healthy trim keeps, so a company above about thirty seats on those volumes sets `stream.executor_log_max_bytes` or a larger volume, and `log_ceiling_short` says so
- **Tag-status:** unreleased

## The decision

A seat's executor session — the harness's own transcript, captured so a
later turn on the same conversation, in any box on any node, continues it — is
stored as **one sealed object per capture in the fleet's object store**
(ADR-0026), under a key the store mints, and is named by **one row per
(seat handle, turn)** of the `executor` state-log domain on
`CREWLET_EXECUTOR_LOG`, the tenth domain of the register. Every record about a
turn is **arbitrated on its conversation** (the seat's handle and a digest of
the conversation's key, or of the turn's own id for a turn with none) and
declares the conversation, or the whole seat, as its scope, so every rule over
a conversation is decided from one snapshot of it in log order. The object is
sealed under the fleet keyring in a streaming format with associated data
binding it to `executor/<seat>/<turn>/<object key>`; a capture naming an
object older than `objstore.RecordWithin` is refused, a capture whose
(launch, capture) ordinals are not newer than the row's writes nothing, and an
ended turn is terminal.

What happens to a row is the domain's and nothing else's: a later turn's `end`
absorbs exactly the ended turns its first launch's read handed out — the head
it continued, the endings it was told and the siblings it was offered, carried
on its captures — so what a conversation still holds is what no ended turn was
shown, and the next read shows all of it; a seat's byte budget releases the
oldest ended sessions; the company's conversation retention deletes a row past
its last instant, ended or not but never one whose run is live, and first ends
as `lost` a turn whose end never came, a day past its last capture with no run
live, so a lost end holds the seat's budget for a day rather than the whole
retention; and the mailbox retirement deletes a removed seat's rows with one
record, always published, whose position every capture and end is held to, so
one decided before the removal never recreates a row for a seat that is gone.

Two departures from the plan this piece was cut from are deliberate. A
retention delete takes the turn's tombstone with its row rather than leaving
one, because a tombstone refuses a record decided before its row went and by
the retention none can still land: every capture is past
`objstore.RecordWithin`, and the duty never nominates a turn whose run is live;
a late `end` writes an ending-only row the retention takes in its turn. And an
end absorbs only what its turn was handed rather than every ended row at or
below the position its launch read at, because a read bounded at sixteen
endings and eight offered sessions would otherwise have its end delete turns
nobody was ever shown.

A person's removal does not reach these rows yet, as it did not reach the
conversation ledger they replace; later records amend this one with the
erasure, the capture and the restore.

## Why the obvious alternatives are wrong

**A volume per node, or a PVC.** A session must reach the next turn's box,
which may be on another engine pod or on e2b; a volume would need every engine
pod to mount every other's, could not reach e2b at all, and would be a second,
Kubernetes-only store the object collector, its audit and the backup cannot
see — the store per node ADR-0026 already refused.

**The memory changelog.** A seat's memory is a compacted subject per row with
no ordering across subjects, and the rules here are over a whole conversation
in log order: which ended turn is the head, which rows a turn's end absorbs,
which attempt at one unit of work survives. A compacted log cannot decide
those, and it declares no barrier, so a launch could not read the head at
`linearizable`.

**The coordination store.** A value of mebibytes per turn in a bucket every
lease, ledger and counter of the company shares, held for the conversation
retention: the bucket is sized for kilobytes, and a full one stops the fleet
claiming seats.

**One row per conversation that each turn's capture takes over.** It reads as
simpler — the conversation's session is the row's object — and it is an object
carry: a row that stops naming one turn's object and starts naming another's,
which ADR-0027 (as ADR-0036 amends it) allows only inside one artifact, because
the collector judges an object by whether any row names it. Two turns running
on one conversation at once would also overwrite each other's session. A row
per turn names only its own object, and the conversation's head is a read.

## Amendment: every node reaches the log through the estate router, and only the engine writes it

A seat's box runs wherever its seat is held, a node without `data` among them,
and only a data node publishes, so the log is reached the way every other
domain of the estate is: through the estate router
([ADR-0025](0025-a-node-without-data-reaches-the-estate-through-one-that-holds-it.md)),
floored on this log. Each write is an idempotent operation under the id the
domain derives for its execution, so one a data node never answered is asked of
the next under that same id; each read is answered at the level its caller asks,
and a launch reads its conversation at `linearizable`, since a seat that moved
holds no floor for the end its last turn published through another node.

Its writes are a class of their own: ENGINE-ORIGINATED. A capture, an end, a
retention delete and a removed seat's are the engine's record of what its own
runs did, which no tool, no person and no operator surface asks for. Declaring
the log to the person's write surfaces, as the domains a surface writes are
declared, would have meant a refusal class and an object argument for writes
nobody can make of them, met only by borrowing an argument some operator tool
takes for something else. So the router declares who originates each write,
the surfaces are held to the domains a surface writes, and two gates hold the
engine's class: every write on this log is declared the engine's and nothing
else is, and no package that serves a person or a model names the writer the
router hands out, the domain's own writer or an engine hand-out of either. The
second is a walk of the source and sees names, not values, so a writer smuggled
inside another type is review's to catch. The reads are ordinary reads, which a
surface may hold. `internal/estate` states the class.
