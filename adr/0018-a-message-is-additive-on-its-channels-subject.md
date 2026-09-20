# ADR-0018 — A chat message is additive on its channel's subject, and scoped to the channel

- **Status:** accepted
- **Authority:** `internal/chat`
- **Enforced-by:** `internal/chat.TestTheScopeAlphabetHasNoPerMessageTerm`
- **Measured:** a declared census of 20,000 messages a day against a compare-and-set budget of 16 rounds, and one deferral cascade in which a message-scoped post lets a node mint a sequence a deferred sibling should have had
- **Cost-when-tried:** the shape this refuses is the one the framework's other two domains use everywhere. Arbitrating a post would make the busiest subject in the company serialise behind itself, and its failure mode is the one a chat system may not have: `ErrConflict` on a message somebody typed.
- **Tag-status:** unreleased

## The decision

A chat message is published on the subject kind `message` whose id is the
**channel's**, with `statelog.PatternAdditive` — no expectation, no
compare-and-set round, no anchor row — and its declared scope is that
**channel's** path. Channel state (topic, purpose, membership, archive,
retention, erase, prune) arbitrates on the channel's own subject as usual.

Two decisions, and they hold each other up:

- **Additive, because posts commute.** Two people talking in one room are not
  racing for anything: neither message decides against the other's state, and
  the order they land in is the order they were said in. An expectation there
  would serialise the single hottest subject in the company behind itself and
  would refuse a write with `ErrConflict` — a message a person typed and lost.
- **Scoped to the channel, because the applier mints the sequence.** A
  per-channel `channel_seq` cannot be minted by the writer (additive writers
  form no expectation and would compute the same number), so the applier mints
  it from log order. That is only deterministic while a deferred record BLOCKS
  its channel: the deferral cascade blocks records whose scope intersects a
  deferred one, so a message scoped to itself would let a node step over a
  deferred sibling, hand the next post the sequence the deferred one should
  have had, and diverge from every other node for the life of the deployment.

The subject and the scope are therefore the same object, which is why there is
no per-message path in the scope alphabet at all.

## Why the obvious alternative is wrong

The obvious alternative is one subject per message — it reads as the natural
reading of "the subject is the arbitration unit", and it gives each message its
own scope for free. It is wrong twice.

The broker keeps a per-subject index, and a subject per message makes that
index grow with the transcript rather than with the room count: a year at the
declared census is millions of subjects on one stream, for an arbitration
nobody needs because nothing contends.

And it breaks the sequence, as above — silently, on one node, under a rolling
upgrade, in a way no read repairs because neither copy is the original.

The other tempting alternative is to drop `channel_seq` and let the client
order by log position. That works for ordering and fails for the thing the
sequence is for: the socket drops the oldest envelope when a tab falls behind,
and a client can only tell it missed a message if the numbers it receives are
CONTIGUOUS PER CHANNEL. A global log position has gaps in every channel by
construction, so a reader could never distinguish a dropped frame from a
message that belonged to another room.

## What it costs

A deferred record blocks its whole channel rather than one message in it. That
is the intended trade — a room stops while a node cannot read a record in it,
rather than a room that silently renumbers — and it is bounded by the same
deferral grace every other domain has.
