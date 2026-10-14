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
every data node derives the same SQL tables from that log. It is the same
framework the tracker and the knowledge base ride, and the guarantees are the
same ones: a node's tables can be behind the log, every answer says how far this
node has applied, and a read that cannot be served refuses rather than answering
"there is nothing here".

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
[ADR-0031](https://github.com/crewlet/crewlet/blob/main/adr/0031-a-message-is-additive-on-its-channels-subject.md).

A message's subject carries its **channel's** id — one message subject per
room, apart from the room's own state subject — and its declared scope is that
channel's. There is no per-message scope at all, which is a correctness
property rather than an economy: the applier numbers messages within a channel
from log order, and a post scoped to itself would let a node that had deferred
one message hand the next the number the deferred one should have had —
diverging that node's rows from every other node's for good.

## What a company gets

**Channels** are public, private, or a **unit's own room**. A create that names
no visibility takes the company's default: `chat.native.default_channel_private`
makes those rooms private, and the engine resolves it in one place, so the
answer is the same whether the room was made from the dashboard or by a
person's own assistant — both make it with the operator catalogue's
`create_channel`, and no seat is given one. A create that *does* name a visibility is
never overridden — the setting is a default and not a lock, so a company that
turns it on can still make a public room deliberately.

A unit that names a `channel` in the org chart gets that room, created and
maintained by the engine at boot and on every apply. The room is the unit's that
*declares* the channel: a child unit that names none inherits its parent's
channel and gets no room of its own, because its seats are already in the
parent's. **A room belongs to exactly one unit** — it records the unit it was
opened for, once and for good, and that unit's lead is who answers a person
nobody else did — so two units declaring one channel is
[refused](configuration.md#what-a-stored-revision-is-held-to) on native chat,
beside each of them: give each its own channel, or leave a child's empty so it
inherits. Its members are every seat in the unit's **subtree** plus the lead
that answers for it — a division's room is where its teams are reachable, and
the alternative leaves a lead talking to an empty room while the people doing
the work sit one level down. Every message in it wakes the unit's **own agent
seats** and nobody else. A descendant's agents are members rather than
followers — a child that inherits the channel included, though its prompt names
the room as its team's — and are reached by a mention, a thread they are in,
`@channel` or the lead; a person is addressable but runs no turn. So a message
in a division's room is not a turn for every agent beneath it, which a channel
cascading down a whole branch would otherwise make it.

**The chart is the only writer.** Creating a unit room by hand, and joining,
leaving or setting the membership of one, are all refused: a room made by hand
under the unit kind would have a membership nothing could ever change, and any
of the rest would be undone by the next apply with nothing to say so — declare
the channel, or move the seat, in the company document instead. The membership
carries the activation of the configuration it came from, as a unit's project
and knowledge space do, so a node applying an older revision — booting on one
the fleet has since replaced — leaves the current team in the room rather than
putting last week's back.
The engine never *deletes* a room: a unit that drops its channel, or leaves the
chart, keeps what was said in it, because a transcript is the only copy of what
people said and an org chart edit is not a request to destroy one. What it does
change is who is in it. The chart is the only writer of a unit room's
membership, so a room no unit declares any more — and the room of a unit whose
last seat has left — is staffed by nobody: its members are taken out at the next
apply, nobody leads it (a person posting there wakes no lead, by the same rule),
every word of it stays readable, and declaring the channel again, on any unit,
staffs it again. A unit that never had anybody in it gets no room created, since an
empty room of this kind is one nothing could join.

**A room follows its channel.** The chart gives a unit a channel, so when it
gives that channel to another unit — moved to the team that replaces its own in
a reorg, or its unit renamed without an `id` — the room moves with it at the
next apply: it answers to the new unit, its people are that unit's, its lead
fallback is that unit's lead, and what was said in it stays. That move rides
the same activation stamp as the membership, so a node applying an older
revision does not move it back. Nothing is disclosed by it, since a unit's room
is readable by every seat of the company. A room never moves while the unit it
belongs to still declares it.

And it will not take over a room it does not own, and says which case it is
in. A room's name never changes, and an archived room keeps its name, so
renaming the room is never the remedy: for a room somebody made by hand under
that name — which it will not hand to a membership the chart decides — give
the unit a channel nobody holds; for a room another unit declares too, which
a written document is refused for, give one of them a channel of its own.

**Direct messages and small groups** have no name. Their identity is derived
from the sorted handles of the people in them, so two seats opening the same
conversation from two nodes converge on one room with no race — and adding
somebody is not an edit but a different conversation, which the write path
refuses to pretend otherwise about.

**Threads** are one level deep. A reply to a reply carries the same root, which
is what makes "this thread" a range rather than a recursive walk.

**Messages** carry up to 32 KiB of text and up to eight links, and no file:
a message links to one — see [what is not here](#what-is-not-here).
A page of a room's messages holds up to the limit asked for (500 at most), and
ends sooner where the messages would not fit the one reply a node that holds no
data is answered in — half of a broker message — so a client pages on the
cursor it is handed rather than on how many rows came back.

**Edits, deletes and reactions.** A person makes the first two — a seat
corrects what it said by replying, which the room sees arrive, rather than
rewriting a remark colleagues already acted on — through the [operator
surface](../reference/api-endpoints.md#the-chat-gestures-only-a-person-makes),
which is also where rooms are made, their settings and membership changed, and
read state recorded. An edit is author-only. A delete is a
**tombstone**: the row survives with its body blanked, so a reply still
resolves to the message it answered and a thread does not lose its first line.
A reaction never wakes anybody and never counts as an answer — if it did, a
seat could discharge every obligation with a thumb.

**A message is said once.** A turn that runs again after a crash, and a
person whose post came back `unknown` and who sends it again, post the same
message rather than a second one: its id is derived from the unit of work the
call belongs to — the turn's trigger, or the request the dashboard sent — and
from what it asked to say, and it carries the instant that work began, so a
node whose operation ledger has since swept its first attempt — the ledger
keeps thirty days, [the one horizon](../guides/retention.md#old-data-is-not-cold-data)
every domain's is kept to — answers `unknown` rather than saying it twice. Within one turn a seat may say several
things: the same call made again with no different call to that tool in
between is one message, while the same words posted again *after* a different
post are another, and so are the same words sent through a different tool
(`send_dm` rather than `post_message`). Every other gesture — an edit, a
reaction, a join, a room's settings — is made once the same way, and joining,
leaving and stating a room's whole membership count as one family of tools: a
seat that joins a room, leaves it and joins it again is in it, because the
leave is a different call to the same membership and makes the second join a
join of its own rather than the first one's retry. A write whose outcome the node cannot establish is answered
`unknown`, never with a receipt: the seat is told it may or may not have been
said, and how to repeat it as the same gesture. A direct message whose
conversation could not be opened says that nothing was sent.

**A private room refuses every write from outside it except moderation.**
Posting, joining, changing the topic and reacting all take the room's own
membership rule, a reaction included, so nothing can be attached to a
conversation you cannot open. An edit is narrower: it is the message's author's
alone. A delete is the author's or an operator's — which every person writing
through their token is. The author's half is narrower too, and it is what lets
somebody take their own words back out of a room they have since left. The
operator's half is moderation, and like erasing or pruning a room's history it
is not bounded by membership: it reaches a message in a private room or a
direct conversation the operator is not in.

**An archived room takes no new messages, and nothing else changes.** A post
and a reply are refused; what was already said stays its authors' and stays
moderated. An author may still edit or take back their own words, an operator
may still take a message down, a reaction is still taken, and the room's topic
and purpose can still be set for whoever reads it after. Each of those shows as
what it is — an edit says so, a delete leaves a tombstone naming who did it —
so the archive keeps an honest record rather than a frozen one, and a key
pasted into a room before it closed can be taken out without reopening the
room to every post the archive was there to stop.

**A room a newer build created takes nothing on this one.** During a rolling
upgrade a node can hold a room of a kind it does not know; it serves nothing
in that room and accepts nothing either — not a post, an edit, a delete, a
reaction, a join or a settings change, an author's and an operator's alike —
because whether that room's transcript is reachable is exactly what it cannot
say. Upgrade the node and the same gesture lands.

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
  lost. That is the lead of the unit that still declares the room: a room the
  chart staffs with nobody, because its unit dropped the channel or left the
  chart, wakes no lead either.

`@channel` wakes at most 32 agents and the channel says so when it truncated,
rather than quietly waking fewer than the gesture implied.

**Being named in a thread follows it.** A seat a message names — when it is
posted, or by an edit that adds the name — is woken by every later reply in
that thread, so somebody asked a question hears its answer. The follow lasts as
long as the thread still names them: an edit that drops the name, a delete or
an erase of the message that named them takes it back, unless another message
in the thread names them too. A colleague named by mistake and corrected away
costs one wake, not one for every reply the thread ever gets.

**Which of those obliges a reply** is narrower still: a direct message, a
mention, a reply in a thread you started, and the lead fallback. A broadcast
does not, a reply in a thread you follow does not, and neither does a reply in
a thread you merely once spoke in — that last one is how a tracker fills with
"noted, thanks".

**People are never woken.** A human seat is addressable and never runs a turn;
what a person gets is an unread count, a mention feed and a live screen.

**The mention feed lists the moments you were named**, newest first, each
once. An edit that still names you leaves the mention where it was — with the
new words, marked edited — because fixing a typo is not naming you again, and a
feed that lifted it to the top would show you as news something you had already
read. An edit that names you where the words before it did not *is* naming you:
it arrives at the top, dated by the edit, and wakes a seat it names the same
way. A delete takes the mention away, as an erase and a prune do: the room keeps
a tombstone because a thread hangs off it, but a message taken back asks you
nothing and has no words left to read.

**A seat woken in a thread is handed the thread.** The message that woke it
arrives whole, and the conversation around it is read at the start of the turn,
as that seat — so a private room answers only for its members — and put in
front of it as [the thread so
far](agent-runtime.md#the-thread-a-turn-was-woken-in): the root and the
newest replies verbatim, the middle of a long thread condensed, under the one
byte bound every chat surface shares. That read is the only copy of the
thread a turn gets; the message's own record carries none, so one thread is
never in a prompt twice. A seat on a node that holds no data reads it from a
data node, like every other read it makes.

**A seat that is working shows it.** While a woken seat's turn runs, the room
shows it working — "is thinking…", then the phase it is in — and the line goes
when the turn ends. `chat.native.typing_status` decides when: `always` (the
default) for every turn a message woke, `addressed` only where the message
obliged an answer, the same two values and meaning as the vendor surfaces'
setting. It is presence rather than a record: raised on the node running the
turn and read by the rooms that are watching, because it is true for minutes
and worthless the moment the turn is over. Every node answers for the
indicators its own seats raise, whether or not it serves the HTTP API, so a turn
run on a `seats`-only member or on a node with `api.port: 0` shows in the room
like any other.

## People

A person is a **`kind: human` seat** in the org chart whose
`contact.crewlet_operator_id` names one of the API tokens in
[Tier A](configuration.md). The server resolves the token to the seat on every
call — and an open dashboard's live channel on every frame it decides, so a
configuration change that unbinds a token or rebinds it to somebody else closes
that tab's connection, and it reconnects as whoever the token names now, rather
than going on receiving the old seat's rooms — so the name a room shows beside
the words is the person's, never the credential's, and **a caller can never
name a seat to post as**. The write is
recorded as every write made with a token is
([ADR-0024](https://github.com/crewlet/crewlet/blob/main/adr/0024-the-dashboard-acts-as-the-person-its-token-is-bound-to.md)):
author kind `operator`, the token's id beside the seat, so a message from the
dashboard and the same person's assistant's read identically in an audit.
Because the kind is `operator`, a person may also take back somebody else's
message — the moderation their credential, which can already erase a room's
history, carries — while an edit stays the author's alone. Moderation is not
bounded by membership: it reaches a message in any room, including a private
room or a direct conversation the person is not in and cannot read.

A token that is not bound to a seat gets nothing in chat — reads included. A
transcript is the most sensitive thing a deployment holds, and a pipeline's
credential is not a person. The two gestures that [destroy](#retention) are the
one exception: they are said in nobody's voice, so any operator credential may
make them, as it may purge a task — recorded as the person a token is bound
to, or under the token's own id where it is bound to nobody.

Adding somebody is still an operator editing Tier A and restarting a node.
There is no login, no password and no self-service signup; see
[what is not here](#what-is-not-here).

**Read state** — how far each person has read in each room, what they have
muted, and whether they are in do-not-disturb — lives in coordination rather
than on the log. A cursor is a fact about one reader's attention that nobody
replays, and at a realistic flush rate it would otherwise be the highest-volume
thing the company writes. It has no expiry, because an expired cursor would
mark a room its reader had finished as unread; a person's read state is
forgotten instead [24 hours after they leave the org
chart](seat-ownership.md#the-removed-seat), the same grace a removed seat's
mailbox gets, and kept if they are restored before then. It is written through
the operator surface's `mark_chat_read`, like every other gesture the dashboard
makes, and a flush that lands is pushed to that person's other open screens on
every node — nothing else ever announces it, because it is on no log, so the
node that took the flush tells its peers over the same ephemeral scatter
presence rides, and each hands it to that person's sockets of its own. A second
tab attached to another ingress node clears its badge with the first. Each
pushed state carries the revision the record stands at, and a screen keeps the
highest, so two flushes whose pushes arrive out of order cannot put an older
state back. A flush whose answer was lost may have landed: it answers
`unknown`, which is safe to repeat because a cursor only moves forward, and the
record is read back and pushed as it stands, so the other screens show it
whichever way the write went. A mute is a room at a time rather than a list:
a flush mutes or unmutes the rooms it names and every other keeps its state,
so a screen mutes the room it is on without first reading what else is muted,
and two tabs muting two different rooms at once both land. The same surface's
`list_channels` counts each room's unread messages against it, and marks the
rooms the person muted and a do-not-disturb they are in, so their own
assistant sees what the dashboard's badges show; a read state that cannot be
read is said rather than counted from zero, which would call every room
unread.

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

Every data node keeps the index beside its copy of the rooms. A node that holds
no data keeps neither, and a seat's `search_messages` there is answered by a
data node, which works out the rooms the asker may read and ranks over them in
one step — as is any search on a data node whose own copy is out of service. A query is
at most 400 bytes, the bound every ranked search in the engine shares, and a
longer one is refused rather than cut.

## Retention

Chat is the highest-volume thing the engine stores, and its rows sit in the
replicated estate that every snapshot copies and every rejoining node
transfers. So, like each data node's usage days, it keeps its rows to a **content
horizon** — and unlike theirs, which is fixed, this one is the company's to
set:

```yaml
chat:
  native:
    message_retention_days: 365   # the default; 0 keeps everything for ever
```

A channel may override it. The sweep is a **record**, not a local delete: a
fleet-singleton duty publishes a cutoff and every data node deletes exactly the
same rows, because the instant on a message is the broker's rather than a clock
each node reads for itself.

The number is an operational decision as much as a compliance one. Keeping
everything is supported and costs disk on every data node, in every backup, and
in the time it takes a new node to join.

When waiting out the horizon is not an answer, two gestures act now, and
neither has an inverse. A **credential pasted into a message** is
[`crewlet chat erase`](../reference/cli.md#crewlet-chat-erase)'s: it destroys
exactly the messages named, on every data node, and leaves the conversation
around them standing. An **erasure request** covering a room's history is
[`crewlet chat prune`](../reference/cli.md#crewlet-chat-prune)'s: it publishes
the duty's own cutoff now, for one room, and everything the room said before
that instant goes. Both are part of the operator's own chat surface: listing
the rooms, reading one, posting as yourself and searching are the rest of it,
and all of them are in the [CLI reference](../reference/cli.md#crewlet-chat).

## What is not here

Three gaps, stated rather than discovered:

**No file attachments.** A message carries links, and the file a link points
at lives somewhere built for files. Bytes on the chat log would be copied to
every data node, kept for the log's whole replay window and replayed by every
data node that joins — a file every member stores for good. The company's own
files have a home made for them instead, the [object store](object-store.md),
where a file has a path somebody can find it by and a collector that frees its
bytes once nothing names them; a message links to one of those, or to a page, a
work item or anything else with an address. A message that named a stored object
itself would be a second owner of that file's lifetime, which every prune and
every erase would then have to release on every data node, in step — and this
build has none of that, so a message names no object.

**No notification when you are away.** There is no email, no push and no SMS —
the engine has never sent anything as itself. A person learns they were
mentioned when they next open the dashboard. This is the largest difference
from a commercial chat product and it is the one most worth knowing before you
choose the axis.

**No account system.** See [People](#people).

## Migrating

Because the axis is exclusive, the cutover is a flip rather than an overlap.
The old workspace stays readable in its own product for as long as you keep it.
There is no importer: the engine does not replay another product's archive into
native rooms.
