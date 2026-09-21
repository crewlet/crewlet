# Chat

A company running Crewlet talks to itself, and until now that talking had to
happen in somebody else's workspace. Chat gives you a third answer, chosen with
one field:

```yaml
chat:
  backend: native    # the default — the engine is the chat system
# backend: vendor    # the engine talks through Slack or Mattermost
# backend: none      # no chat at all
```

The `vendor` value is what you already had: a seat reads and writes in your
Slack or Mattermost workspace, and the engine stores no conversation of its
own. `native` is the engine's own chat — channels, threads, direct messages,
mentions, reactions, search and a screen in the dashboard — on the same terms
[the tracker](task-engine.md) and [the knowledge base](knowledge-system.md) are
the engine's own.

The axis is exclusive, and that is the point of having one. Two live chat homes
would mean a seat answering the same person twice, a unit's channel naming two
different rooms, and no rule anywhere that could say which conversation was the
real one.

## Why it exists

Some companies will not connect a third-party workspace — because of where the
data would sit, because of what a workspace costs per seat, or because the
company is a handful of people and eleven agents and a chat product priced for
humans is the wrong shape. Those companies had no way for a person to say
anything to a seat at all.

## The log and the copies

Every change is **one record on an ordered log the whole fleet shares**, and
every node derives the same SQL tables from that log. It is the same framework
the tracker and the knowledge base ride, and the guarantees are the same ones:
a node's tables can be behind the log, every answer says how far this node has
applied, and a read that cannot be served refuses rather than answering "there
is nothing here".

What is particular to a conversation is how the log is used, and there are two
disciplines on one stream.

**Channel state arbitrates.** A topic, a purpose, an archive, a membership set,
a retention cutoff — each is published on the room's own subject and
conditioned on that room's last change, so two people renaming one channel
contend and exactly one wins.

**Messages are additive.** A post carries no expectation at all. Two people
talking in the same room are not racing for anything, so paying for arbitration
would serialise the busiest subject in the company behind itself — and its
failure mode is the one a chat system may not have: a refusal on a message
somebody typed. This is recorded as
[ADR-0018](https://github.com/crewlet/crewlet/blob/main/adr/0018-a-message-is-additive-on-its-channels-subject.md).

A message's subject is its **channel's**, and so is its declared scope. There
is no per-message scope at all, which is a correctness property rather than an
economy: the applier numbers messages within a channel from log order, and a
post scoped to itself would let a node that had deferred one message hand the
next the number the deferred one should have had — diverging that node's rows
from every other node's for good.

## What a company gets

**Channels** are public, private, or a **unit's own room**. A create that names
no visibility takes the company's default: `chat.native.default_channel_private`
makes those rooms private, and the engine resolves it in one place, so the
answer is the same whether the room came from the dashboard, the REST route, an
agent's own tool or `crewlet chat`. A create that *does* name a visibility is
never overridden — the setting is a default and not a lock, so a company that
turns it on can still make a public room deliberately.

A unit that names a `channel` in the org chart gets that room, created and
maintained by the engine at boot and on every apply. Its members are every seat
in the unit's **subtree** plus the lead that answers for it — a division's room
is where its teams are reachable, and the alternative leaves a lead talking to
an empty room while the people doing the work sit one level down. Every message
in it wakes the unit's **own agent seats** and nobody else: a descendant's
agents have their own room, and a person is addressable but runs no turn, so a
message in a division's room is not a turn for every agent beneath it.

**The chart is the only writer.** Joining, leaving and setting the membership
of a unit room are all refused, because any of them would be undone by the next
apply with nothing to say so — move the seat in the company document instead.
The engine never *deletes* a room: a unit that drops its channel keeps what was
said in it, because a transcript is the only copy of what people said and an
org chart edit is not a request to destroy one. And it will not take over a
room somebody already made by hand under that name; it reports the clash rather
than handing a public room's transcript to a membership the chart decides.

**Direct messages and small groups** have no name. Their identity is derived
from the sorted handles of the people in them, so two seats opening the same
conversation from two nodes converge on one room with no race — and adding
somebody is not an edit but a different conversation, which the write path
refuses to pretend otherwise about.

**Threads** are one level deep. A reply to a reply carries the same root, which
is what makes "this thread" a range rather than a recursive walk.

**Messages** carry up to 32 KiB of text and up to eight links. The engine
stores **no file bytes** — see [what is deliberately missing](#what-is-not-here).

**Edits, deletes and reactions.** An edit is author-only. A delete is a
**tombstone**: the row survives with its body blanked, so a reply still
resolves to the message it answered and a thread does not lose its first line.
A reaction never wakes anybody and never counts as an answer — if it did, a
seat could discharge every obligation with a thumb.

**A private room refuses every write from outside it**, a reaction included:
posting, joining, changing the topic and reacting all take the room's own
membership rule, so nothing can be attached to a conversation you cannot open.
An edit and a delete are narrower still — only the message's own author, or an
operator — which is what lets somebody take their own words back out of a room
they have since left.

## Who gets woken

This is the axis where an agent company differs most from a human one, and it
is worth stating plainly: **a person in a busy room reads passively, and an
agent has no passive read**. Every delivery is a two-phase turn against a
node's concurrency budget, so "everyone sees everything" — which is free in a
human chat product — is a company that does nothing but read chat.

So a message wakes:

- every seat it **names**, and anybody in the **thread** it is part of;
- members who have **followed the whole channel**, which the engine sets for a
  unit's own agent seats in that unit's room and for nobody else;
- the unit's **lead**, when a person posts in their team's room and nobody else
  was routed — because silence there looks exactly like a message that was
  lost.

`@channel` wakes at most 32 agents and the channel says so when it truncated,
rather than quietly waking fewer than the gesture implied.

**Which of those obliges a reply** is narrower still: a direct message, a
mention, a reply in a thread you started, and the lead fallback. A broadcast
does not, and neither does a reply in a thread you merely once spoke in — that
last one is how a tracker fills with "noted, thanks".

**People are never woken.** A human seat is addressable and never runs a turn;
what a person gets is an unread count, a mention feed and a live screen.

## People

A person is a **`kind: human` seat** in the org chart whose
`contact.crewlet_operator_id` names one of the API tokens in
[Tier A](configuration.md). The server resolves the token to the seat on every
write, so a message is attributed to the person rather than to the credential,
and **a caller can never name a seat to post as**.

A token that is not bound to a seat gets nothing in chat — reads included. A
transcript is the most sensitive thing a deployment holds, and a pipeline's
credential is not a person.

Adding somebody is still an operator editing Tier A and restarting a node.
There is no login, no password and no self-service signup; see
[what is not here](#what-is-not-here).

**Read state** — how far each person has read in each room, what they have
muted, and whether they are in do-not-disturb — lives in coordination rather
than on the log. A cursor is a fact about one reader's attention that nobody
replays, and at a realistic flush rate it would otherwise be the
highest-volume thing the company writes.

## Search

Chat has its own keyword index, separate from the knowledge base's, and **no
semantic search at all**.

Both halves of that are deliberate. A message is a remark about work rather
than a statement of it — the same argument that keeps a work item's comment
thread out of the knowledge index — so mixing the corpora would let one busy
conversation outrank every page written on purpose. And embedding a year of
chat needs several times the entire supported vector corpus, against a budget
shared with every other corpus a company has.

The index is tuned for the corpus it serves: a chat corpus is bimodal,
one-line acknowledgements beside pasted blocks, and the knowledge base's length
normalisation would hand every query to whoever typed the shortest reply.

## Retention

Chat is the highest-volume thing the engine stores, and its rows sit in the
replicated estate that every snapshot copies and every rejoining node
transfers. So it is the first domain with a **content horizon**:

```yaml
chat:
  native:
    message_retention_days: 365   # the default; 0 keeps everything for ever
```

A channel may override it. The sweep is a **record**, not a local delete: a
fleet-singleton duty publishes a cutoff and every node deletes exactly the same
rows, because the instant on a message is the broker's rather than a clock each
node reads for itself.

The number is an operational decision as much as a compliance one. Keeping
everything is supported and costs disk on every node, in every backup, and in
the time it takes a new node to join.

When waiting out the horizon is not an answer — an erasure request, a
credential pasted into a room — [`crewlet chat
prune`](../reference/cli.md#crewlet-chat-prune) publishes that same cutoff now,
for one room. It is the same gesture the duty runs on a schedule, and it has no
inverse. That command is part of the operator's own chat surface: reading a
room, posting as yourself, searching and importing are the rest of it, and all
of them are in the [CLI reference](../reference/cli.md#crewlet-chat).

## What is not here

Three gaps, stated rather than discovered:

**No file attachments.** The engine stores no file bytes anywhere. A message
carries links. Putting files on the broker would mean every attachment in every
backup artefact and every snapshot transfer; putting them on a node's disk
would mean a file the next node cannot read.

**No notification when you are away.** There is no email, no push and no SMS —
the engine has never sent anything as itself. A person learns they were
mentioned when they next open the dashboard. This is the largest difference
from a commercial chat product and it is the one most worth knowing before you
choose the axis.

**No account system.** See [People](#people).

## Migrating

[`crewlet chat import`](../reference/cli.md#crewlet-chat-import) reads a Slack or Mattermost export into native channels,
preserving threads, resolving authors to handles and keeping the original
timestamps. Imported history **wakes nobody** — a year of mentions arriving as
live wakes would be tens of thousands of turns — and it carries where it came
from, so importing one archive twice writes nothing the second time.

Because the axis is exclusive, the cutover is a flip rather than an overlap.
The old workspace stays readable in its own product for as long as you keep it.
