# ADR-0014 — A compacted changelog is the fourth answer to "who has to agree on it?"

- **Status:** accepted
- **Authority:** `internal/learning/memsync`
- **Enforced-by:** `internal/learning/memsync.TestASeatsMemoryCrossesToANodeThatHasNeverRunIt`
- **Cost-when-tried:** the three-answer rule was applied to a seat's memory and returned the wrong answer. Its stated test — "would a peer reading this change any answer?" — was answered *no*, because a seat's memory is read by the node running that seat. The node running that seat CHANGES: placement converges in both directions, so a seat moves on a node joining, leaving, draining or upgrading, and its rows do not move with it. The seat kept working, silently having forgotten everything it learned somewhere else, and on a node that died, forgotten it permanently.
- **Tag-status:** unreleased

## The decision

ADR-0003 asks every piece of state "who has to agree on it?" and names three
answers: *this node alone* is the node's own database; *every node, identically,
derived from an ordered log* is the replicated estate; *the whole company, now*
is the coordination store.

There is a fourth, and this record names it so nobody has to rediscover it by
picking one of the three wrongly. **"Whichever node holds this seat, and only
the current value"** is a COMPACTED CHANGELOG: one subject per row, on a stream
retaining exactly one message per subject, replayed into the acquiring node's
own database before the seat is allowed to take work.

It is a distinct answer, not a variant of the ordered-log one, and the
difference is what a reader has to know:

| | ordered log (ADR-0002) | compacted changelog |
|---|---|---|
| The stream holds | every record, in order | the current value of each subject |
| A new node | replays from a checkpoint | replays the subjects it needs |
| Identity | a checkpoint and a generation answer a recreation | there is none — a recreated stream is simply EMPTY |
| Ordering | strict, and an applier depends on it | none across subjects, and nothing may |

That third row is the one that bites, and `internal/learning` declares it
rather than assuming it. A recreated changelog is empty, so hydrating from one
reports success and hands every seat on the node a blank diary. A compacted
stream has no checkpoint and no generation with which to notice, so its only
honest move is to REFUSE the seat.

Deletes deliberately do not travel. The lifecycle drops rows constantly and a
tombstone protocol would be a second thing to keep correct for ever; a hydrated
node may resurrect rows its predecessor swept, and the next lifecycle pass
drops them again by the same rules.

## Why the obvious alternative is wrong

Each of the three existing answers was considered and each is worse for this
state.

**The node's own database alone** is where it started, and is the bug above.

**The replicated estate** would make a seat's diary an ordered log with an
applier, a checkpoint and a strict replay — so a node could not run a seat
until it had applied every memory record every seat in the company ever wrote,
and the log would grow for ever with superseded values of rows that only ever
matter in their latest form. The whole content of this state is "the current
value", which is precisely what compaction is.

**Coordination** is sized for what the whole company must agree on NOW, and
retention there is a bucket's age. A seat's memory is neither: no peer needs it
while the seat is elsewhere, and there is no age at which forgetting it is
correct.

## What this does not decide

It does not make the compacted changelog a general-purpose fourth estate.
Everything on it shares one property — the current value is the whole value,
and losing an intermediate one costs nothing — and state without that property
belongs in one of ADR-0003's three.

It does not change ADR-0002. The ordered log is still the write-ahead log for
state a fleet DERIVES, and a domain on the state-log framework may declare
`ReplayCompacted` for its own tables (the vector domain does) without becoming
this: the framework's compacted replay and a seat's memory changelog answer the
same question about retention and different ones about who reads the result.
