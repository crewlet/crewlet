# ADR-0046: A chat message is sequenced on its channel's subject, and scoped to the channel

- **Status:** accepted
- **Authority:** `internal/chatlet`
- **Enforced-by:** `internal/chatlet.TestTheScopeAlphabetHasNoPerMessageTerm`, `internal/chatlet.TestAPostIntoARoomArchivedAheadOfItIsRefusedAndSoIsItsRetry`, `internal/chatlet.TestAReactionPastTheCapIsRefusedAndTheWriterIsTold`, `internal/chatlet.TestASecondCopyOfAnOperationDoesNothing`
- **Measured:** a declared census of 20,000 messages a day against a compare-and-set budget of 16 rounds, and one deferral cascade in which a message-scoped post lets a node mint a sequence a deferred sibling should have had
- **Cost-when-tried:** two shapes. ARBITRATED, the one the framework's other strict domains use everywhere: the busiest subject in the company serialises behind itself, and its failure mode is the one a chat system may not have — `ErrConflict` on a message somebody typed. ADDITIVE, the one this record first chose: it called the records commutative while the applier decided four of them against rows the writer's snapshot could not see — it declined the thirty-third distinct emoji, skipped an edit of a tombstone, and wrote a post into a room a record ahead of it had archived or taken its author out of — and the writer answered `applied` for every one, a record the applier dropped included, while the change feed woke seats about posts the room never held; and a second copy of an edit, landing after a later edit, put the older words back.
- **Tag-status:** unreleased

## The decision

A chat message is published on the subject kind `message` whose id is the
**channel's**, with `statelog.PatternSequenced` — no expectation, no
compare-and-set round, no anchor row — and its declared scope is that
**channel's** path. The applier DECIDES each record at its position, from the
record and the rows it holds there, and writes what it decided (a verdict, in
`chat_verdicts`), which the writer reads back before it says what the write
did. Channel state (topic, purpose, membership, archive, retention, erase,
prune) arbitrates on the channel's own subject as usual.

Three decisions, and they hold each other up:

- **Sequenced rather than arbitrated, because posts do not race.** Two people
  talking in one room are not contending for anything, and the order they land
  in is the order they were said in. An expectation there would serialise the
  single hottest subject in the company behind itself and would refuse a write
  with `ErrConflict` — a message a person typed and lost.
- **Sequenced rather than additive, because not every record commutes.** A
  post's effect depends on whether a record ahead of it archived the room or —
  in a room that admits its members only — took the author out; an edit's on
  whether a delete ahead of it left a tombstone; a reaction's on whether
  thirty-two other emoji are already on the message; and any of them on
  whether an erase or a prune ahead of it removed the message. The writer
  refuses what its own snapshot shows; what only the log's order can show is
  decided by the applier, identically on every node because every node applies
  the same records in the same order with the same code. The record carries the
  writer's answer to any RULE a later build might change — whether the room
  admits its members only travels on the post, and the most distinct emoji a
  message may carry on the reaction — so the applier decides only the facts at
  its position. A second copy of an operation (two publishers that
  each found its ledger silent) does nothing and is decided `repeated`, because
  a copy applied again lands after whatever happened in between. One verdict
  per record: `applied`, or refused and why — `archived`, `not_member`, `gone`,
  `deleted`, `reactions_full` — or `repeated`. The writer answers a refusal
  with the sentinel its own decision would have refused with had the record
  ahead been in its snapshot, so every surface answers the two the same way; a
  retry the ledger answers reads its first copy's verdict the same way; and the
  change feed withholds the wake of a record this node's applier refused.
- **Scoped to the channel, because the applier mints the sequence.** A
  per-channel `channel_seq` cannot be minted by the writer (writers form no
  expectation and would compute the same number), so the applier mints it from
  log order. That is only deterministic while a deferred record BLOCKS its
  channel: the deferral cascade blocks records whose scope intersects a
  deferred one, so a message scoped to itself would let a node step over a
  deferred sibling, hand the next post the sequence the deferred one should
  have had, and diverge from every other node for the life of the deployment.

The subject and the scope are therefore the same object, which is why there is
no per-message path in the scope alphabet at all.

## Why the obvious alternatives are wrong

The obvious alternative is one subject per message — it reads as the natural
reading of "the subject is the arbitration unit", and it gives each message its
own scope for free. It is wrong twice.

The broker keeps a per-subject index, and a subject per message makes that
index grow with the transcript rather than with the room count: a year at the
declared census is millions of subjects on one stream, for an arbitration
nobody needs because nothing contends.

And it breaks the sequence, as above — silently, on one node, under a rolling
upgrade, in a way no read repairs because neither copy is the original.

The second tempting alternative is to drop `channel_seq` and let the client
order by log position. That works for ordering and fails for the thing the
sequence is for: the socket drops the oldest envelope when a tab falls behind,
and a client can only tell it missed a message if the numbers it receives are
CONTIGUOUS PER CHANNEL. A global log position has gaps in every channel by
construction, so a reader could never distinguish a dropped frame from a
message that belonged to another room.

The third is to decide everything in the writer's snapshot and call the record
additive, which is what this record first said. A snapshot is taken before the
append, and the records ahead of the append are exactly what it cannot hold: a
writer that decided alone answered `applied` for records the applier then
declined, because the applier is the only party that holds the room as it is
at the record's position.

## What it costs

A deferred record blocks its whole channel rather than one message in it. That
is the intended trade — a room stops while a node cannot read a record in it,
rather than a room that silently renumbers — and it is bounded by the same
deferral grace every other domain has.

A verdict row per message record, kept for the operation ledger's horizon
(`statelog.OpsRetention`, thirty days) by the broker's clock and pruned inside
each record's own apply, sixty-four at a time; one read of it per gesture,
after the apply the writer already waited for; and one per post or edit the
change feed handles. The feed never waits on an applier that cannot answer —
one more than a pull behind, or holding the record retained — and derives the
wake from the record there, so a post refused in a race a node has not applied
yet can still wake a seat that then finds nothing.

A write answered `pending` or `unknown` has no verdict yet, so the writer
cannot say whether the record will be refused; it says so by carrying none.
