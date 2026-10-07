# ADR-0027 — A deletion from the shared store needs no lock

- **Status:** accepted
- **Authority:** `internal/objstore/collect`
- **Enforced-by:** `internal/tracker.TestAPutNamingAnOldKeyIsRefused`, `internal/objstore/references.TestTheGraceOutlastsTheLongestUnrecordedUpload`, `internal/objstore.TestEveryPutMintsAKeyNeverWrittenBefore`, `internal/objstore/collect.TestAnObjectIsJudgedByBothItsInstants`, `internal/objstore/collect.TestAnIncompleteEstateDeletesNothing`, `internal/objstore/collect.TestAnUnfinishedUploadIsAbandonedOnlyAfterTheGrace`
- **Tag-status:** unreleased

## The decision

The object store's collector deletes an object only when all of these hold,
and the rule spans packages because no one of them can make it true alone:

- it was **stored more than a day ago** by the backend's own clock
  (`collect.PendingGrace`) — the grace an upload gets between storing its
  object and writing the row that names it;
- its **key was minted more than a day ago**, by the instant the key itself
  carries (a UUIDv7);
- and **no row names it** in an estate the collector has pinned with a barrier
  and read **completely** — a node holding a record it could not apply deletes
  nothing, since that record may name any object.

It takes no lock, and it needs none, because of the rule that replaces one:
**a key is named only by the write that uploaded it, and that write is refused
once the key is older than `objstore.RecordWithin`** (twelve hours) by the
clock of the node deciding it (`tracker.ErrUploadStale`). A key is minted by
`objstore.Store.Put` for one upload, at random, and never derived from the
content, so no second writer can be re-using an object the collector is
deleting; and a key past the grace is one no write can name any more, however
late its record arrives. Two collectors at once are safe for the same reason —
deleting what the other already deleted is not an error.

The inequalities are pinned in one test: the grace exceeds `RecordWithin` by
more than any two nodes' clocks are assumed to disagree, and `RecordWithin`
outlasts the slowest upload the engine accepts (a file of
`tracker.MaxFileBytes` at the `objstore.MiBPace` floor, eight and a half
hours). What the rule rests on, then, is that the clocks of the node deciding a
write and the node running the collector disagree by less than the twelve hours
between the two figures.

Five packages carry it: `objstore` (the key, its minting, and `RecordWithin`),
`tracker` (the write that refuses an old key), `objstore/collect` (the
judgement by both instants), `objstore/references` (the constants test) and
every backend, whose stored instant is no earlier than its put began —
`objstoretest` certifies that, and that a backend lists every upload it began
and never finished.

One more deletion follows the same rule, because it is the same question asked
of bytes no listing of the objects shows: **an upload that never finished** —
an S3 multipart upload nobody completed or aborted, broker pieces no metadata
names — is abandoned once it BEGAN more than the grace ago, and where it is
under a key, once that key was also minted more than the grace ago: every
upload in flight is pending too, and none takes as long as the grace. It needs
no estate, since no row names an upload that never finished.

## Why the obvious alternatives are wrong

**The grace alone**, by the backend's clock, leaves a window it cannot close: a
write naming an object can land after a collection pinned the estate and read
the object as nobody's, and the row then names bytes the collection just
deleted. The grace makes the window unlikely; the bound on the write makes it
impossible.

**A lock around the judgement and every re-put** is what this record decided
first, for content-addressed chunks a second writer could re-use. It costs a
coordination round trip per deletion and per re-put, a bucket of locks aged at
their own lifetime, and a rule a future gesture could forget to take. With a key
minted per upload there is no re-put to lock against.

**Refusing a key minted in the future**, beside one too old, buys nothing: the
collector deletes nothing whose minting instant is inside its grace, so a key a
fast clock minted ahead protects itself, and refusing it would only fail
uploads from a node whose clock runs ahead.

**Conditional deletes in the store** (delete-if-unmodified-since) are what an
S3 `If-Unmodified-Since` would give, and the JetStream object store has no
equivalent; a rule that holds on one backend only is a rule the default backend
breaks.

## What this does not decide

Which objects exist or where they are kept — that is
[ADR-0026](0026-the-estate-names-an-object-and-a-store-keeps-its-bytes.md).
