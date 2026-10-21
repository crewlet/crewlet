# Chatlet

A company running Crewlet talks to itself, and until now that talking had to
happen in somebody else's workspace. Chatlet gives you a third answer, chosen with
one field:

```yaml
chat:
  backend: chatlet   # the default — the engine is the chat system
# backend: vendor    # the engine talks through Slack or Mattermost
# backend: none      # no chat at all
```

The `vendor` value is what you already had: a seat reads and writes in your
Slack or Mattermost workspace, and the engine stores no conversation of its
own. `chatlet` is Chatlet, the engine's own chat — channels, threads, direct messages,
mentions, reactions, search and a screen in the dashboard — on the same terms
[Worklet](task-engine.md) and [Doclet](knowledge-system.md) are
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

Every change is **one record on an ordered log the whole fleet shares**,
`CREWLET_CHAT_LOG`, and every data node derives the same SQL tables from that
log. It is the same framework Worklet, Doclet and the identity
directory ride, and the guarantees are the same ones: a node's tables can be
behind the log, every answer says how far this node has applied, and a read
that cannot be served refuses rather than answering "there is nothing here".
Every record is signed under the fleet keyring like every other state-log
record ([ADR-0031](../../adr/0031-every-state-log-record-is-signed.md)), so
nothing that merely reaches the broker's port can say something in a room.

What is particular to a conversation is how the log is used, and there are two
disciplines on one stream.

**Channel state arbitrates.** A topic, a purpose, an archive, a membership set,
a retention horizon — each is published on the room's own subject and
conditioned on that room's last change, so two people renaming one channel
contend and exactly one wins.

**Messages are sequenced.** A post carries no expectation at all. Two people
talking in the same room are not racing for anything, so paying for arbitration
would serialise the busiest subject in the company behind itself — and its
failure mode is the one a chat system may not have: a refusal on a message
somebody typed. The log orders the messages instead, and every node DECIDES
each one where it applies it, from the room as it is at that record's position
— because a message can depend on something that landed just ahead of it,
which the node it was written on could not yet see:

- a post into a room somebody archived a moment before, or — in a private room
  or a direct conversation — by somebody taken out of it a moment before;
- an edit of a message its author or a moderator took back a moment before;
- a reaction that would be the message's thirty-third distinct emoji;
- an edit, a delete or a reaction of a message an erase or a retention prune
  removed.

Each is refused exactly as it would have been had the earlier change already
reached whoever wrote it, in the same class: a seat's tool answer says which it
was (the room is archived, they are not in it, the message is gone, the message
carries all the emoji it can), and a person is answered that class in its
code's own words until the refusal catalog words it for them
([ADR-0060](../../adr/0060-a-caveat-or-a-refusal-is-worded-for-each-reader-and-a-person-is-never-shown-a-models.md)). An archived room and a full message are each the STATE of what
the gesture is about, so they are refused as a conflict — the same request
lands once somebody reopens the room or takes an emoji off — exactly as an
artifact in the trash and a project archived before a task is filed into it
are; see [which class a refusal is](../reference/api-endpoints.md#which-class-a-refusal-is). Every node reaches the same decision and records it, so a retry
of a refused message is refused again rather than reported as said, and a
refused message wakes nobody. Each decision is kept for thirty days, the
operation ledger's own horizon, and every node records how far back it has let
them go: asked about a message whose decision is past that, a node answers that
it cannot tell — never that the message was not said. A gesture answered `pending` — durable, and not
yet applied on the node that answered — may still be refused this way when it
applies, so asking again under the same `Idempotency-Key` is how a caller hears
the decision: the operation ledger answers the repeat with it and writes
nothing twice. The dashboard does exactly that for a message it sent. This is recorded as
[ADR-0046](../../adr/0046-a-message-is-sequenced-on-its-channels-subject.md).

A message's subject carries its **channel's** id — one message subject per
room, apart from the room's own state subject — and its declared scope is that
channel's. There is no per-message scope at all, which is a correctness
property rather than an economy: the applier numbers messages within a channel
from log order, and a post scoped to itself would let a node that had deferred
one message hand the next the number the deferred one should have had —
diverging that node's rows from every other node's for good.

## What a company gets

**Channels** are public, private, or a **unit's own room**. A create that names
no visibility takes the company's default: `chat.chatlet.default_channel_private`
makes those rooms private, and the engine resolves it in one place, so the
answer is the same whether the room was made from the dashboard or by a
person's own assistant — both make it with the operator catalogue's
`create_channel`, and no seat is given one. A create that *does* name a
visibility is never overridden — the setting is a default and not a lock, so a
company that turns it on can still make a public room deliberately.

A unit that names a `channel` in the org chart gets that room, created and
maintained by the engine at boot and on every apply. The room is the unit's that
*declares* the channel: a child unit that names none inherits its parent's
channel and gets no room of its own, because its seats are already in the
parent's. **A room belongs to exactly one unit** — it records the unit it was
opened for, and that unit's lead is who answers a person nobody else did — so
two units declaring one channel is
[refused](configuration.md#what-a-stored-revision-is-held-to) on Chatlet,
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
putting last week's back. The engine never *deletes* a room: a unit that drops
its channel, or leaves the chart, keeps what was said in it, because a
transcript is the only copy of what people said and an org chart edit is not a
request to destroy one. What it does change is who is in it. A room no unit
declares any more — and the room of a unit whose last seat has left — is
staffed by nobody: its members are taken out at the next apply, nobody leads
it (a person posting there wakes no lead, by the same rule), every word of it
stays readable, and declaring the channel again, on any unit, staffs it again.
A unit that never had anybody in it gets no room created, since an empty room
of this kind is one nothing could join.

**A room follows its channel.** The chart gives a unit a channel, so when it
gives that channel to another unit — moved to the team that replaces its own in
a reorg — the room moves with it at the next apply: it answers to the new unit,
its people are that unit's, its lead fallback is that unit's lead, and what was
said in it stays. That move rides the same activation stamp as the membership,
so a node applying an older revision does not move it back. Nothing is
disclosed by it, since a unit's room is readable by every seat of the company.
A room never moves while the unit it belongs to still declares it.

And it will not take over a room it does not own, and says which case it is
in. A room's name never changes, and an archived room keeps its name, so
renaming the room is never the remedy: for a room somebody made by hand under
that name — which it will not hand to a membership the chart decides — give
the unit a channel nobody holds; for a room another unit declares too, which
a written document is refused for, give one of them a channel of its own.

**Direct messages and small groups** have no name. Their identity is derived
from the sorted handles of the seats in them, so two seats opening the same
conversation from two nodes converge on one room with no race — and adding
somebody is not an edit but a different conversation, which the write path
refuses to pretend otherwise about.

**Threads** are one level deep. A reply to a reply carries the same root, which
is what makes "this thread" a range rather than a recursive walk.

**Messages** carry up to 32 KiB of text and up to eight links, and no file:
a message links to one — see [what is not here](#what-is-not-here).
A link is an absolute `http`, `https` or `mailto` address, kept exactly as it
was sent, so a page or a work item is linked by the whole address a browser
shows for it. A seat reading the thread and `crewlet chatlet read` take a link
as it stands, and a relative reference, a dashboard route on its own (`#/...`)
or any other scheme would mean a different place to each reader, or none, so a
message carrying one is refused `invalid`, and the refusal a model reads names
every entry that is not one (`links[1]`). The dashboard draws a message's
links by the rule it draws an address in an agent's words by, whoever wrote
the message: an address on a host the company configured opens as the
dashboard rebuilds it, in a new tab, and any other, a link to the company's
own dashboard among them, is drawn whole and inert, with the host it names.
A page of a room's messages holds up to the limit asked for (500 at most), and
ends sooner where the messages would not fit the one reply a node that holds no
data is answered in — half of a broker message — so a client pages on the
cursor it is handed rather than on how many rows came back.

**Edits, deletes and reactions.** A person makes the first two — a seat
corrects what it said by replying, which the room sees arrive, rather than
rewriting a remark colleagues already acted on — through the [operator
surface](../reference/api-endpoints.md#the-chat-gestures-only-a-person-makes),
which is also where rooms are made, their settings and membership changed, and
read state recorded. An edit is the author's alone, an administrator's not
included. A delete is a **tombstone**: the row survives with its body blanked,
so a reply still resolves to the message it answered and a thread does not
lose its first line. A reaction never wakes anybody and never counts as an
answer — if it did, a seat could discharge every obligation with a thumb.

**A message is said once.** A turn that runs again after a crash, and a
person whose post came back `unknown` and who sends it again, post the same
message rather than a second one: its id is derived from the unit of work the
call belongs to — the turn's trigger, or the operation key the dashboard sends
in `Idempotency-Key` — and from what it asked to say, and it carries the
instant that work began, so a node whose operation ledger has since swept its
first attempt — the ledger keeps thirty days,
[the one horizon](../guides/retention.md#old-data-is-not-cold-data) every
domain's is kept to — answers `unknown` rather than saying it twice. Within one
turn a seat may say several things: the same call made again with no different
call to that tool in between is one message, while the same words posted again
*after* a different post are another, and so are the same words sent through a
different tool (`send_dm` rather than `post_message`). Every other gesture — an
edit, a reaction, a join, a room's settings — is made once the same way, and
joining, leaving and stating a room's whole membership count as one family of
tools: a seat that joins a room, leaves it and joins it again is in it, because
the leave is a different call to the same membership and makes the second join
a join of its own rather than the first one's retry. A write whose outcome the
node cannot establish is answered `unknown`, never with a receipt: the seat is
told it may or may not have been said, and how to repeat it as the same
gesture. A direct message whose conversation could not be opened says that
nothing was sent.

**A private room refuses every write from outside it except moderation.**
Posting, joining, changing the topic and reacting all take the room's own
membership rule, a reaction included, so nothing can be attached to a
conversation you cannot open. An edit is narrower: it is the message's author's
alone. A delete is the author's, or an administrator's as a moderator. The
author's half is narrower too, and it is what lets somebody take their own
words back out of a room they have since left. The administrator's half is
moderation, and like erasing or pruning a room's history it is not bounded by
membership: it reaches a message in a private room or a direct conversation the
administrator is not in, and it is decided before the message is read, so
taking it down shows them nothing of the room.

**An archived room takes no new messages, and nothing else changes.** A post
and a reply are refused; what was already said stays its authors' and stays
moderated. An author may still edit or take back their own words, an
administrator may still take a message down, a reaction is still taken, and the
room's topic and purpose can still be set for whoever reads it after. Each of
those shows as what it is — an edit says so, a delete leaves a tombstone naming
who did it — so the archive keeps an honest record rather than a frozen one,
and a key pasted into a room before it closed can be taken out without
reopening the room to every post the archive was there to stop. Archiving a
room, and opening it again, is not any member's: it is the room's creator's,
the lead's of the unit whose room it is (or of a unit above it), or an
administrator's, and never an agent's.

**A room a newer build created takes nothing on this one.** During a rolling
upgrade a node can hold a room of a kind it does not know; it serves nothing
in that room and accepts nothing either — not a post, an edit, a delete, a
reaction, a join or a settings change, an author's and a moderator's alike —
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
what a person gets is an unread count, a mention feed and a live screen. A
mention is not a notice in Worklet's inbox either: the inbox is
Worklet's, and Chatlet keeps its own.

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
when the turn ends. `chat.chatlet.typing_status` decides when: `always` (the
default) for every turn a message woke, `addressed` only where the message
obliged an answer, the same two values and meaning as the vendor surfaces'
setting. It is [presence](#presence-typing-and-the-working-indicator) rather
than a record: raised on the node running the turn, because it is true for
minutes and worthless the moment the turn is over. Every node raises the
indicators its own seats hold, whether or not it serves the HTTP API, so a turn
run on a `seats`-only member or on a node with `api.port: 0` shows in the room
like any other.

**A room stays live on a node whose own copy is out of service.** Every node
that serves sockets hands each committed message to the screens watching its
room. A node whose copy has gone out of service — an applier stopped on a
record it cannot read — keeps doing so from a sound peer's batches, as it does
for the inbox
([Humans in the org chart](humans-in-the-org.md#how-a-person-learns-they-have-work)),
and every switch between its own applier and a peer's asks every chat screen on
it to read its rooms again, so a gap in what it heard is a re-read rather than
a hole in the transcript. What the peer relays is signed under the fleet
keyring and bound to the ask it answers, because it is shown as what somebody
said: a relayed answer the fleet did not sign is passed over, and no screen
sees it.

## Presence, typing and the working indicator

Who has a room open, who is typing in it and which seat is working on an answer
there is the room's **presence**, and it is never a record: none of it is on
the chat log, none of it is applied, and no backup carries it. A keystroke on
the log would be a row every data node applies and keeps for as long as the
room does, to say something nobody will ask about a minute later. A chat room's
presence rides the one presence hub every kind of room shares
([ADR-0038](../../adr/0038-presence-is-told-never-stored.md)), as an
artifact's room does:

- **A tab looks at one room at a time.** The room's screen sends a `presence`
  frame naming the room (`room: "chat"`, `channel_id`), viewing, and that frame
  is the join: the engine puts the socket in the room's presence once the
  room's own visibility admits its reader — the rule every message is filtered
  by — and takes it out of the room it was in. A room the reader may not see
  puts them in nobody's presence and shows them nobody's, in the words a room
  that does not exist gets. Leaving the screen sends an empty `channel_id`.
- **Typing is editing.** While a person types, the screen re-asserts the frame
  as editing at most every three seconds, and the engine puts a member nobody
  re-asserted back to viewing after six, which is what clears the line under a
  lid closed mid-word.
- **A seat at work is a member too.** The working indicator is the seat's member
  of the room's presence: an agent, editing, under the thread its answer will
  land in, with the phrase the room shows beside its name and the run of the
  turn, raised and withdrawn by the node running the turn.
- **Who is told is decided per room and per reader.** A socket hears a room's
  presence only while it is in the room, so a private room's presence reaches
  its members and nobody else, and a record that narrows a room (a member
  removed, a room made private) takes out of it every socket whose reader may
  no longer see it, with a roster naming nobody so the screen empties rather
  than standing. A gap in what a node heard — the same one that asks its
  screens to read their rooms again — decides every socket in a room again,
  since it may have held such a record.
- **Between nodes, every frame is sealed.** The node holding a seat's turn and
  the node holding a reader's socket are rarely the same one. Each node tells
  its peers its own members' moves, in frames sealed under the fleet keyring in
  a key domain of their own, and a node shows nobody a frame the fleet did not
  seal — a frame forged by anybody who can reach the cluster port shows nobody
  typing. A node that gains a room's first socket asks its peers once who is
  there, and every node rebroadcasts its members every fifteen seconds, so a
  member whose node went away is gone within forty-five.

The socket's [`chat_presence`](../reference/api-endpoints.md#pushes) push
is the room whole as the socket joins it and every change after.

## People

A person in chat is somebody in the identity directory acting as their human
seat ([Identity and Access](identity-and-access.md#a-person-holds-a-human-seat-and-acts-as-it)),
holding the **`chatlet`** app
([permissions](identity-and-access.md#access-admin-and-one-permission-per-app)).
The session a password sign-in mints is how the dashboard reaches chat, and a
person's own machine token
([machine tokens](identity-and-access.md#machine-tokens-a-persons-own-and-a-service-accounts))
is how their assistant or a shell does. Either way the room is read, and every
message written, **as the seat**: a room shows each speaker by seat, its
members are seats, and a private room's contents are its members'. So a
message from the dashboard and the same person's assistant read identically —
the seat's handle, author kind `human` — with the credential that made it
(`session:…` or `pat:…`) recorded beside it, so an audit can still tell the two
apart. **A caller can never name a seat to post as**, and a dashboard socket's
seat is fixed for its life: a person moved to another seat, or whose `chatlet` is
taken away, has their sockets closed, to reconnect as who they are now.

**A credential acting as itself has no voice here.** The deployment's own Tier
A token, or a service account bound to no seat, has no seat to speak as and no
membership to read a room by, so every chat verb refuses it — reads included, a
public room's too — `403 unauthorized` with reason `unseated`. That is the
authority table's own row for each chat verb deciding, not a rule beside it:
the act route refuses nobody for being unbound
([ADR-0024](../../adr/0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md)),
and chat's verbs are simply made as a seat. A pipeline that should post gets a
human seat nobody holds, and its service account is bound to it
(`crewlet iam bind`). The three gestures that [destroy](#retention) are the
exception: they are said in nobody's voice, so an administrator's own
credential may make them, with a recent proof, and each is recorded under
whoever made it — a person's seat, or a credential's own login.

**A seat's chat is the seat's, not the person's.** Everything a person has in
chat is kept under the seat they hold: the rooms they are in, the direct
conversations they are part of — whose identity is derived from the seats'
handles — and their read state. So when a person moves to another seat, or
leaves and somebody else is put on theirs, the seat's **next holder inherits
its chat**: its memberships, its direct conversations, its read cursors, its
mutes and its do-not-disturb, exactly as the seat's inbox, pins and personal
views already follow it — whoever holds a seat next reads what accumulated on
it ([ADR-0032](../../adr/0032-every-person-holds-a-human-seat.md)). The person
who moved starts their new seat with that seat's chat. Moderation follows
nobody's seat: a delete of somebody else's words is the administrator's,
whichever seat they hold.

**Read state** — how far each seat has read in each room, what it has muted,
and whether its person is in do-not-disturb — lives in
[coordination](coordination.md) rather than on the log. A cursor is a fact
about one reader's attention that nobody replays, and at a realistic flush
rate it would otherwise be the highest-volume thing the company writes. It has
no expiry, because an expired cursor would mark a room its reader had finished
as unread; a seat's read state is forgotten instead
[24 hours after the seat leaves the org chart](seat-ownership.md#the-removed-seat),
the same grace a removed seat's mailbox gets, and kept if the seat is restored
before then. It is written through the operator surface's `mark_chat_read`,
like every other gesture the dashboard makes, and every node that serves
sockets **watches** those records, so a flush that lands is pushed to that
seat's open screens wherever they are attached — derived from the record
itself, so there is no message for a peer to forge, and a second tab on
another ingress node clears its badge with the first. Each pushed state carries
the revision the record stands at, and a screen keeps the highest, so two
flushes whose pushes arrive out of order cannot put an older state back. A
flush whose answer was lost may have landed: it answers `unknown`, which is
safe to repeat because a cursor only moves forward, and the watch pushes the
record as it stands, so the other screens show it whichever way the write
went. A mute is a room at a time rather than a list: a flush mutes or unmutes
the rooms it names and every other keeps its state, so a screen mutes the room
it is on without first reading what else is muted, and two tabs muting two
different rooms at once both land. The same surface's `list_channels` counts
each room's unread messages against it, and marks the rooms the seat muted and
a do-not-disturb its person is in, so their own assistant sees what the
dashboard's badges show; a read state that cannot be read is said rather than
counted from zero, which would call every room unread.

## Search

Chatlet has its own keyword index, separate from Doclet's, and **no
semantic search at all**
([ADR-0065](../../adr/0065-chat-is-a-keyword-corpus-and-takes-no-vectors.md)).

Both halves of that are deliberate. A message is a remark about work rather
than a statement of it — the same argument that keeps a work item's comment
thread out of the knowledge index — so mixing the corpora would let one busy
conversation outrank every page written on purpose. And embedding a year of
chat needs several times the entire supported vector corpus, against a budget
shared with every other corpus a company has.

The index is tuned for the corpus it serves: a chat corpus is bimodal,
one-line acknowledgements beside pasted blocks, and Doclet's length
normalisation would hand every query to whoever typed the shortest reply.

**A search is answered for its asker.** The rooms it ranks over are the rooms
that seat may read, worked out by the node that ranks it in the same step, and
a hit is scored by those rooms' own statistics — so how often a word is said in
a room you cannot read never moves your ranking. An answer says how many rooms
it searched, and one from an index still being built says so rather than
reading as nothing said.

Every data node keeps the index beside its copy of the rooms. A node that holds
no data keeps neither, and a seat's `search_messages` there is answered by a
data node — as is any search on a data node whose own copy is out of service.
A query is at most 400 bytes, the bound every ranked search in the engine
shares, and a longer one is refused rather than cut.

## Retention

Chat is the highest-volume thing the engine stores, and its rows sit in the
replicated estate that every snapshot copies and every rejoining node
transfers. So, like each data node's usage days, it keeps its rows to a
**content horizon** — and unlike theirs, which is fixed, this one is the
company's to set:

```yaml
chat:
  chatlet:
    message_retention_days: 365   # the default; 0 keeps everything for ever
```

A room may carry a horizon of its own, which beats the company's, and setting
one is an administrator's, because a shorter horizon is a prune on a delay. The
sweep is a **record**, not a local delete: a fleet-singleton duty
(`chat-prune`) publishes a cutoff and every data node deletes exactly the same
rows, because the instant on a message is the broker's rather than a clock each
node reads for itself.

The duty prunes only by the horizon of the revision the fleet's
[activation pointer](control-plane.md#the-company-and-what-each-node-keeps-of-it)
names, and only on a node that runs that very revision. A node still serving
another one, because it refused the fleet's revision or booted on an offline
import the fleet has not taken, publishes no cutoff and does not claim the
duty, so a node that runs the fleet's revision takes it over. A horizon is
enforced late rather than early: an hour missed is pruned on the next tick,
and a message pruned by a horizon the company has since raised, or set to `0`,
is gone from every data node for good.

The number is an operational decision as much as a compliance one. Keeping
everything is supported and costs disk on every data node, in every backup, and
in the time it takes a new node to join.

When waiting out the horizon is not an answer, two gestures act now, and
neither has an inverse. A **credential pasted into a message** is
[`crewlet chatlet erase`](../reference/cli.md#crewlet-chatlet-erase)'s: it destroys
exactly the messages named, on every data node, and leaves the conversation
around them standing. An **erasure request** covering a room's history is
[`crewlet chatlet prune`](../reference/cli.md#crewlet-chatlet-prune)'s: it publishes
the duty's own cutoff now, for one room, and everything the room said before
that instant goes. Both, and a room's own horizon, are routes of the
[human write surface](../reference/api-endpoints.md#a-chat-erase-prune-or-horizon):
`admin`, with
[a recent proof](identity-and-access.md#some-gestures-ask-how-recently-you-proved-who-you-are),
and never a seat. None of them reads the room first, so an administrator takes
a credential out of a private room they are not in without being shown the
rest of it. Listing the rooms, reading one, posting as yourself and searching
are the rest of `crewlet chatlet`, and all of it is in the
[CLI reference](../reference/cli.md#crewlet-chatlet).

## Who may do what

One table decides every chat gesture, for a seat's own tools, the dashboard
and a person's assistant alike. Chatlet is one **app**, and the permission that
uses it is `chatlet`: every agent seat holds it, and a person holds it when an
administrator ticks it. **Every chat verb is made as a seat** — the reads
included — so beside the app each asks that the caller acts as one. *Which
room* is the chat domain's own rule rather than a permission: a public room
and a unit's room are every seat's to read, a private room and a direct
conversation their members', and a room you may not read answers exactly as a
room that does not exist. No permission widens that: `admin` reads no room it
is not in.

| Gesture | Who may |
|---|---|
| List rooms, read a room or a thread, search | `chatlet`, as a seat — over the rooms that seat may read |
| Post, reply, send a direct message, react | `chatlet`, as a seat, in a room it may reach; a post or a reply is refused in an archived room |
| Join or leave a room | `chatlet`, as a seat, its own membership only; a private room cannot be joined, and a unit's room can be neither joined nor left |
| Make a room, open a direct conversation, set a room's topic or purpose, state its whole membership | `chatlet`, as a person — never an agent — in a room they can reach; a unit's room is the org chart's, and a direct conversation's membership is its identity |
| Archive a room, or open it again | the room's creator, the lead of the unit whose room it is or of a unit above it, or `admin` — as a person, never an agent |
| Edit a message | its author, as a person — nobody else, an administrator included |
| Take a message down | its author, as a person, or `admin` as a moderator — in any room, without reading it |
| Mark what you read, mute a room, do-not-disturb | your own record only, as a person — kept under your seat |
| Set a room's horizon, erase messages, prune a room | `admin`, with a recent proof, and never a seat — a person, or an administrator's own credential acting as itself |

## What is not here

Two gaps, stated rather than discovered:

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

## Migrating

Because the axis is exclusive, the cutover is a flip rather than an overlap.
The old workspace stays readable in its own product for as long as you keep it.
There is no importer: the engine does not replay another product's archive into
Chatlet rooms.
