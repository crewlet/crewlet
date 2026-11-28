# Chatlet

Chat has three homes, chosen with one field:

```yaml
chat:
  backend: chatlet   # the default — the engine is the chat system
# backend: vendor    # the engine talks through Slack or Mattermost
# backend: none      # no chat at all
```

`vendor`: a seat reads and writes in your Slack or Mattermost workspace; the engine stores no conversation. `chatlet` is Chatlet, the engine's own chat (channels, threads, direct messages, mentions, reactions, search, a dashboard screen), on the same terms [Worklet](task-engine.md) and [Doclet](knowledge-system.md) are the engine's own.

The axis is exclusive: two live chat homes would mean a seat answering one person twice, a unit's channel naming two rooms, and no rule saying which conversation is real.

## Why it exists

Some companies will not connect a third-party workspace (data location, per-seat cost, or a handful of people and agents). Without Chatlet a person could not talk to a seat at all.

## The log and the copies

Every change is **one record on an ordered log the whole fleet shares**, `CREWLET_CHAT_LOG`; every data node derives the same SQL tables from it. It is the framework Worklet, Doclet and the identity directory ride, with the same guarantees: a node's tables can be behind the log, every answer says how far this node has applied, and a read that cannot be served refuses rather than answering "there is nothing here". Every record is signed under the fleet keyring ([ADR-0031](../../adr/0031-every-state-log-record-is-signed.md)), so nothing that merely reaches the broker's port can speak in a room.

Two disciplines share the stream.

**Channel state arbitrates.** A topic, purpose, archive, membership set or retention horizon is published on the room's own subject, conditioned on that room's last change: two people renaming one channel contend and exactly one wins.

**Messages are sequenced.** A post carries no expectation: people talking in one room are not racing, arbitration would serialise the company's busiest subject, and a refusal on a message somebody typed is a failure a chat system may not have. The log orders messages; every node DECIDES each one where it applies it, from the room as it is at that record's position, because a message can depend on a change that landed just ahead of it:

- a post into a room archived a moment before, or (private room or direct conversation) by somebody taken out a moment before;
- an edit of a message its author or a moderator took back a moment before;
- a reaction that would be the message's thirty-third distinct emoji;
- an edit, delete or reaction of a message an erase or retention prune removed.

Each is refused as it would have been had the earlier change already reached the writer, in the same class. A seat's tool answer says which (the room is archived, they are not in it, the message is gone, the message carries all the emoji it can); a person is answered the class in its code's own words until the refusal catalog words it ([ADR-0060](../../adr/0060-a-caveat-or-a-refusal-is-worded-for-each-reader-and-a-person-is-never-shown-a-models.md)). An archived room and a full message are the STATE of what the gesture is about, so they are refused as a conflict (the same request lands once somebody reopens the room or takes an emoji off), like an artifact in the trash or a task filed into an archived project; see [which class a refusal is](../reference/api-endpoints.md#which-class-a-refusal-is). Every node reaches and records the same decision, so a retry of a refused message is refused again, not reported as said, and a refused message wakes nobody.

Decisions are kept thirty days, the operation ledger's horizon, and each node records how far back it has let them go: asked about a message whose decision is past that, a node answers that it cannot tell, never that the message was not said. A gesture answered `pending` (durable, not yet applied on the answering node) may still be refused when it applies, so asking again under the same `Idempotency-Key` is how a caller hears the decision: the ledger answers the repeat and writes nothing twice. The dashboard does this for a message it sent. Recorded as [ADR-0046](../../adr/0046-a-message-is-sequenced-on-its-channels-subject.md).

A message's subject carries its **channel's** id and its scope is the channel's; there is no per-message scope, because the applier numbers messages within a channel from log order and a self-scoped post could let a node that deferred one message give the next its number, diverging rows for good.

## What a company gets

**Channels** are public, private, or a **unit's own room**. A create naming no visibility takes `chat.chatlet.default_channel_private` (true makes them private), resolved in one place, so the dashboard and a person's assistant (both use the operator catalogue's `create_channel`; no seat is given it) get the same answer. A create that names a visibility is never overridden: the setting is a default, not a lock.

A unit that names a `channel` in the org chart gets that room, created and maintained by the engine at boot and on every apply. The room belongs to the unit that *declares* the channel; a child naming none inherits its parent's channel and gets no room, its seats already being in the parent's. **A room belongs to exactly one unit**: it records the unit it was opened for, and that unit's lead answers a person nobody else did. Two units declaring one channel is [refused](configuration.md#what-a-stored-revision-is-held-to) on Chatlet, beside each of them: give each its own channel, or leave a child's empty.

Members are every seat in the unit's **subtree** plus the lead that answers for it, so a division's room reaches its teams. Every message in it wakes the unit's **own agent seats** only. A descendant's agents (a child inheriting the channel included, though its prompt names the room as its team's) are members, not followers, reached by a mention, a thread they are in, `@channel` or the lead; a person is addressable but runs no turn. A division's room is thus not a turn for every agent beneath it.

**The chart is the only writer.** Creating a unit room by hand, and joining, leaving or setting the membership of one, are refused: a hand-made unit-kind room would have a membership nothing could change, and the rest would be undone by the next apply silently. Declare the channel, or move the seat, in the company document. Membership carries the activation of the configuration it came from, so a node applying an older revision (booting on one the fleet has replaced) leaves the current team in the room.

The engine never *deletes* a room: a unit dropping its channel or leaving the chart keeps what was said, since a transcript is the only copy and an org chart edit is no request to destroy one. It changes who is in it. A room no unit declares, or whose unit's last seat has left, is staffed by nobody: members are taken out at the next apply, nobody leads it (a person posting there wakes no lead), every word stays readable, and declaring the channel again, on any unit, staffs it again. A unit that never had anybody gets no room.

**A room follows its channel.** When the chart gives a channel to another unit (a reorg), the room moves at the next apply: it answers to the new unit, its people and answerer are that unit's, and what was said stays. The move rides the membership's activation stamp, so an older revision does not move it back. Nothing is disclosed, since a unit's room is readable by every seat. A room never moves while its unit still declares it.

The engine will not take over a room it does not own, and says which case applies. A room's name never changes (an archived room keeps it), so renaming is never the remedy: for a room made by hand under that name, give the unit a channel nobody holds; for a room another unit declares too, give one of them a channel of its own.

**Direct messages and small groups** have no name. Identity is derived from the sorted handles of the seats, so two seats opening the same conversation from two nodes converge on one room. Adding somebody is a different conversation, not an edit.

**Threads** are one level deep: a reply to a reply carries the same root, so "this thread" is a range. A reply is said and read in its thread; a room's transcript holds the room's own posts. A room post that was answered shows its thread's summary: reply count (a deleted reply included, its tombstone still sits there), when the newest arrived, and the first three repliers with the total. It is counted from the thread's rows when the page is read, so a delete, erase or prune changes it with no counter.

**Messages** carry up to 32 KiB of text and up to eight links, and no file: a message links to one, see [what is not here](#what-is-not-here). A link is an absolute `http`, `https` or `mailto` address kept exactly as sent, so a page or work item is linked by the whole address a browser shows. A seat and `crewlet chatlet read` take a link as it stands; a relative reference, a dashboard route alone (`#/...`) or any other scheme would mean a different place to each reader, so a message carrying one is refused `invalid`, and the refusal a model reads names every offending entry (`links[1]`). The dashboard draws links by the rule it draws an address in an agent's words by, whoever wrote the message ([Dashboard Design](../reference/dashboard-design.md#what-prose-names-is-drawn-as-what-it-is)): an address on a host the company configured opens as the dashboard rebuilds it, in a new tab; a GitHub or GitLab address only under an organization or group that host's configuration names; any other, the company's own dashboard included, is drawn whole and inert, with the host it names.

A page holds up to the limit asked for (500 at most) and ends sooner where messages would not fit half a broker message, so a client pages on the cursor it is handed, not the row count.

**Edits, deletes and reactions.** A person makes edits and deletes; a seat corrects itself by replying, which the room sees, rather than rewriting a remark colleagues acted on. They go through the [operator surface](../reference/api-endpoints.md#the-chat-gestures-only-a-person-makes), which also makes rooms, changes settings and membership, and records read state. An edit is the author's alone, an administrator's not included. A delete is a **tombstone**: the row survives with its body blanked, so a reply still resolves to its message. A reaction never wakes anybody and never counts as an answer (else a seat could discharge every obligation with a thumb).

**A message is said once.** A turn re-run after a crash, and a person re-sending a post that came back `unknown`, post the same message, not a second: its id is derived from the unit of work (the turn's trigger, or the operation key the dashboard sends in `Idempotency-Key`), from what it asked to say, and it carries the instant that work began. A node whose operation ledger has swept the first attempt (thirty days, [the one horizon](../guides/retention.md#old-data-is-not-cold-data) every domain's is kept to) answers `unknown` rather than saying it twice.

Within one turn a seat may say several things: the same call repeated with no different call to that tool in between is one message; the same words posted *after* a different post are another, and so are the same words through a different tool (`send_dm` rather than `post_message`). Every other gesture (edit, reaction, join, room settings) is made once the same way. Joining, leaving and stating a room's whole membership are one family of tools: join, leave, join again leaves the seat in the room, because the leave makes the second join its own rather than the first one's retry. A write whose outcome the node cannot establish is answered `unknown`, never a receipt: the seat is told it may or may not have been said, and how to repeat it as the same gesture. A direct message whose conversation could not be opened says nothing was sent.

**A private room refuses every write from outside it except moderation.** Posting, joining, changing the topic and reacting all take the room's membership rule, so nothing attaches to a conversation you cannot open. An outsider is told there is no such room, the same answer a read gives and the same as for an id nothing holds, so a refusal never reveals a private room's name or that two people talk (a direct conversation's id can be worked out from two names). A member joining a room they are already in changes nothing. The engine itself is no member and needs none: it tells any room, a private one or a direct conversation included, what befell a conversation there (a turn's run that [did not continue its session](conversation-sessions.md#when-a-run-does-not-continue-its-session)), in a line of its own that wakes nobody, and nothing but the engine writes as it. That line is the engine's only way past a private room's membership; it joins no thread, answers nobody's waiting message, is no replier and is nobody's own to edit or delete, and the engine's line and a seat's are told apart by the kind of author, never by the name alone (no seat may take the names the engine writes under, [`auto-join`, `org-chart` and `sessions`](humans-in-the-org.md#human-seat-fields)).

An edit is the author's alone. A delete is the author's, or an administrator's as moderator. The author's half lets somebody take their words out of a room they have left. The administrator's half is not bounded by membership, like erasing or pruning history: it reaches a message in a private room or direct conversation the administrator is not in, and is decided before the message is read, showing them nothing of the room.

**An archived room takes no new messages, and nothing else changes.** A post and a reply are refused; what was said stays its authors' and stays moderated. An author may still edit or take back their words, an administrator may still take a message down, a reaction is still taken, and topic and purpose can still be set. Each shows as what it is (an edit says so, a delete leaves a tombstone naming who did it), so the archive is an honest record, and a key pasted before closing can be removed without reopening the room. Archiving and reopening a room belongs to the room's creator, the lead of the unit whose room it is (or of a unit above it), or an administrator, never an agent.

**A room a newer build created takes nothing on this one.** In a rolling upgrade a node may hold a room of an unknown kind; it serves and accepts nothing in it (post, edit, delete, reaction, join, settings; author's and moderator's alike). Upgrade the node and the gesture lands.

## A room says what changed in it

A room's own changes are lines in its transcript: who joined, who put somebody in or took them out, who left, and what its topic and description were set to. These **room events** are written by the engine from the record that made the change, in that record's transaction, so a description never changes without its line and a line never claims a change that did not land. Each takes the room's next number like a message, so a page, cursor and live update carry it in order, and the retention prune and an erase reach it like any row.

| Event | Written when |
|---|---|
| `joined` | A named room is made (founding members), somebody joins themselves, or the engine puts a seat in: the org chart staffing a unit's room, or an auto-join room taking in a newcomer |
| `added` | Somebody puts other people or agents in the room |
| `left` | Somebody leaves themselves, or the org chart takes a seat out of its unit's room |
| `removed` | Somebody takes other people or agents out |
| `topic`, `description` | Somebody sets the topic or description (its `purpose`); an empty value is the old one cleared |
| `template` | Somebody sets topic and description from a template: one line naming the template, not two |

A direct conversation has none (nobody joins or leaves one, and it has no topic). Archiving writes none.

**A room event is not conversation.** It is never an unread message or mention, a room's preview message, or a search hit, and takes no reply, reaction, edit or delete. Its author is whoever made the change, or the engine for the org chart's and auto-join's changes. A seat reading the room sees it as a message carrying an `event`, and the words it records (topic, description, template name) are fenced as their author's, as a topic is.

**A room's topic and description are 250 characters each**, counted as a person sees them (an emoji is one), because a person edits both under a counter. A longer description is describing a project, which has a page.

**Adding people to a room** names who to add and nobody else, decided against the room as it stands when the write lands, so two people adding colleagues at once both land (stating the whole membership would let the second write the first's people out). The membership's rules apply: a private room is changed only by somebody in it, a unit's room is the org chart's, a direct conversation is its people.

**A public room can take in everybody the company takes on.** An administrator turns on a room's auto-join; from then every seat that joins the company, agent or person, is put in the room by the engine at the activation that brings it in, with a `joined` line. Turning it on adds nobody already in the company; the room accounts for the company as it stood and for every newcomer it puts in, so a newcomer who leaves is not put back by the next activation. Turning it off and on again starts from the company as it is then. A private room cannot take it (who may read one is no setting's to decide for people nobody has met), nor can a unit's room or a direct conversation. An archived room takes nobody in, and accounts for the newcomers it missed when reopened.

## Who gets woken

**An agent has no passive read**: every delivery is the seat's own executor in a [box](code-sandbox.md), paying the harness's whole cost even to decide a message was not for it, so "everyone sees everything" would make a company that only reads chat.

A message wakes:

- every seat it **names**, and every participant of a **direct conversation**;
- anybody in the **thread** it is part of: the seat that started it, the seats that have spoken in it and the seats that follow it;
- every agent in the room on a **`@channel`**;
- members who have **followed the whole channel**, which the engine sets for a unit's own agent seats in that unit's room and nobody else;
- the room's **answerer**, when a person posts in a public, private or unit room, names nobody and wakes no other agent (silence there looks like a lost message).

**Words wake only the seats they address** ([ADR-0113](../../adr/0113-an-agents-words-wake-only-the-seats-they-address.md)). All of the above except a name is a standing arrangement, and one wakes a seat only for a message that asks something and names nobody in particular:

- **A message that names somebody is addressed to them.** A person's message wakes the seats it @-mentions, an agent's only when it asks; either wakes the seat whose question it answers and, when it asks, the other side of a direct conversation; nobody else in the room or thread. A status note for a person (`@sara, the deploy is done`) wakes no agent.
- **An agent's message asks something only where the agent says so.** The posting tools (`post_message`, `reply_in_thread`, `send_dm`) require an agent to state `asks`: true when the message asks someone for an answer or action, obliging the named seat, or the other side of a direct conversation, to answer. With `asks: false` an @-mention is a reference: the named agent sees it and is not woken. Such a message wakes only the agent whose open question it answers, under `answer`, and the seats with a stake in its thread; no direct conversation's other side, thread author, speaker, follower or `@channel`. An acknowledgement, thank-you or status note is read where it was said. The receipt tells the posting agent so.
- **A stake in a thread is woken by any reply in it** by somebody else, whatever it names: the seat that asked something there with no answer yet, under `awaiting`, and the seat asked that still owes the answer, under `owing`. Exception, the co-asker: a reply by a seat whose only stake is awaiting the same addressee does not wake another seat awaiting it, and a person whose open message reached that addressee is a co-asker too. A top-level message in a named room answers the newest open top-level ask each agent holds of its author there, and wakes the agent that asked under `answer`; an older ask stays open for a later message.
- **An asking message gives each seat it asks a receipt**: a person's post always; an agent's for the seats it woke for anything but their own stake or an answer, the people it named and the other side of a direct conversation. The seat's reply in the thread, or anywhere later in a direct conversation, closes the receipt and wakes the asker under `answer`. A question and its answer each wake once; the thanks wake nobody. A busy seat's running turn that takes a person's message instead of a turn of its own says so on the receipt (a `take` record the engine writes): `joined` when it took it into its work, still open until it answers; `absorbed` when it read it there and found it asks nothing, which settles it ([ADR-0168](../../adr/0168-a-busy-seats-other-wakes-are-triaged-and-the-engine-gates-the-choice.md)).

What the rule held back rides the message's record (`held`), and the node that routes it logs `chat_wakes_held` with the count.

**A person is never answered by silence.** The answerer is one seat, woken only when the message woke no other agent, and obliged to reply. A message naming a colleague has been addressed, so it has no answerer even when the colleague is a person (never woken): "@sara, can you review?" is for Sara. A handle nobody holds names nobody, and neither does a member-only room naming somebody outside it, so those still reach the answerer.

A unit's room is answered by the lead of the unit that still declares it; a room the chart staffs with nobody has no answerer. Any other room (made by hand) is answered by its most senior agent member: fewest managers above it in the org chart, a tie going to the seat the chart lists first. A private room's answerer is therefore always a member who can read it, and a room with no agent member has none. Both are resolved when the message is written and carried on its record, so a later-delivered wake goes to whoever answered when the person spoke. An agent's post that reaches nobody wakes nobody: a company must not talk to itself.

**A person's `@channel` is a request; an agent's is news.** When a person addresses the whole room, every agent reached answers in the thread under the message, for its own role: what it knows, will do, or that it has nothing to add. Each is told how many seats were asked, so thirty agents do not each answer for the room. An agent's `@channel` tells the room something and nobody owes a reply. Either way it wakes at most 32 agents (as many as one message may name), and the channel says so when it truncated. That cap, with each seat's executors bounded by its `sandbox.max_concurrent_runs` (two by default), bounds a broadcast's boxes: `node.max_concurrent` holds a wake only while it launches and while its run is collected and reviewed ([Scaling](scaling.md#where-the-constants-come-from)).

**Nobody reads an unwoken message later.** An agent hears a message only when routing wakes it: no catch-up on its next turn, no pass over its rooms. A message that woke nobody is read by people, and by an agent only if it opens the room for another reason. To reach a particular agent, name it.

**Being named in a thread follows it.** A seat a message names (when posted, or by an edit adding the name) is woken by every later reply in that thread that asks something and names nobody else, so somebody asked a question hears its answer. The follow lasts while the thread still names them: an edit dropping the name, or a delete or erase of the naming message, takes it back unless another message in the thread names them. A colleague named by mistake costs one wake, not one per reply.

**Which of those obliges a reply** is narrower: a direct message, a mention, a reply in a thread you started, a person's `@channel`, and the room's answerer. Not an agent's broadcast, not an agent's name that asks nothing (wakes no agent; not `named`), not an answer to your question, not a reply in a thread you follow, not a reply in a thread you merely once spoke in (that is how a tracker fills with "noted, thanks"). A follow-up in a thread where you owe an answer adds to an obligation you already have.

**People are never woken.** A human seat is addressable and never runs a turn; a person gets an unread count, a mention feed and a live screen. A mention is not a notice in Worklet's inbox: Chatlet keeps its own.

**Activity's mentions list the moments you were named** (`chat_activity` with `kind: mention`), newest first, each once. An edit that still names you leaves the mention where it was, with the new words (a typo fix is not naming you again). An edit that names you where the words before did not *is* naming you: it arrives at the top, dated by the edit, and wakes a seat it names the same way. A delete takes the mention away, as an erase and a prune do: the room keeps a tombstone because a thread hangs off it, but a message taken back asks you nothing.

**A seat woken in a thread is handed the thread.** The waking message arrives whole, and the conversation around it is read at the start of the turn, as that seat (a private room answers only for its members) and put in front of it as [the thread so far](agent-runtime.md#the-thread-a-turn-was-woken-in): root and newest replies verbatim, the middle of a long thread condensed, under the one byte bound every chat surface shares. That read is the turn's only copy; the message's record carries none, so a thread is never in a prompt twice. A seat on a node holding no data reads it from a data node, like every other read.

**A seat that is working shows it.** While a woken seat's turn runs, the room shows it working ("is thinking…", then the phase) and the line goes when the turn ends. `chat.chatlet.typing_status` decides when: `always` (the default) for every turn a message woke, `addressed` only where the message obliged an answer (same values and meaning as the vendor surfaces' setting). It is [presence](#presence-typing-and-the-working-indicator), not a record: raised on the node running the turn, because it is true for minutes and worthless after. Every node raises the indicators its own seats hold, whether or not it serves the HTTP API, so a turn on a `seats`-only member or a node with `api.port: 0` shows in the room.

**A room stays live on a node whose own copy is out of service.** Every node that serves sockets hands each committed message to the screens watching its room. A node whose copy is out of service (an applier stopped on a record it cannot read) keeps doing so from a sound peer's batches, as for the inbox ([Humans in the org chart](humans-in-the-org.md#how-a-person-learns-they-have-work)), and every switch between its own applier and a peer's asks every chat screen on it to re-read its rooms, so a gap is a re-read, not a hole. What the peer relays is signed under the fleet keyring and bound to the ask it answers; a relayed answer the fleet did not sign is passed over and no screen sees it.

## Presence, typing and the working indicator

Who has a room open, who is typing and which seat is working on an answer is the room's **presence**, never a record: none of it is on the chat log, applied, or in a backup (a keystroke on the log would be a row every data node keeps as long as the room). A chat room's presence rides the one presence hub every kind of room shares ([ADR-0038](../../adr/0038-presence-is-told-never-stored.md)), as an artifact's room does:

- **A tab looks at one room at a time.** The room's screen sends a `presence` frame naming the room (`room: "chat"`, `channel_id`), viewing; that frame is the join. The engine puts the socket in the room's presence once the room's visibility admits its reader (the rule every message is filtered by) and takes it out of the room it was in. A room the reader may not see puts them in nobody's presence and shows them nobody's, in the words a nonexistent room gets. Leaving the screen sends an empty `channel_id`.
- **Typing is editing.** While a person types, the screen re-asserts the frame as editing at most every three seconds, and the engine puts a member nobody re-asserted back to viewing after six (clearing the line under a lid closed mid-word).
- **A seat at work is a member too.** The working indicator is the seat's member of the room's presence: an agent, editing, under the thread its answer will land in, with the phrase shown beside its name and the turn's run, raised and withdrawn by the node running the turn.
- **Who is told is decided per room and per reader.** A socket hears a room's presence only while in the room, so a private room's presence reaches its members only. A record that narrows a room (member removed, room made private) takes out every socket whose reader may no longer see it, with a roster naming nobody so the screen empties. A gap in what a node heard (the one that asks screens to re-read) decides every socket in a room again.
- **Between nodes, every frame is sealed.** Each node tells its peers its own members' moves in frames sealed under the fleet keyring in a key domain of their own, and shows nobody a frame the fleet did not seal, so a forged frame shows nobody typing. A node gaining a room's first socket asks its peers once who is there, and every node rebroadcasts its members every fifteen seconds, so a member whose node went away is gone within forty-five.

The socket's [`chat_presence`](../reference/api-endpoints.md#pushes) push is the room whole as the socket joins it and every change after.

## People

A person in chat is somebody in the identity directory acting as their human seat ([Identity and Access](identity-and-access.md#a-person-holds-a-human-seat-and-acts-as-it)), holding the **`chatlet`** app ([permissions](identity-and-access.md#access-admin-and-one-permission-per-app)). A password sign-in's session is how the dashboard reaches chat; a person's own machine token ([machine tokens](identity-and-access.md#machine-tokens-a-persons-own-and-a-service-accounts)) is how their assistant or a shell does. Either way rooms are read and messages written **as the seat**: a room shows each speaker by seat, members are seats, a private room's contents are its members'. A dashboard message and the same person's assistant's read identically (the seat's handle, author kind `human`), with the credential (`session:…` or `pat:…`) recorded beside it for audit. **A caller can never name a seat to post as**, and a dashboard socket's seat is fixed for its life: a person moved to another seat, or whose `chatlet` is taken away, has their sockets closed, to reconnect as who they are now.

**A credential acting as itself has no voice here.** The deployment's Tier A token, or a service account bound to no seat, has no seat to speak as or read by, so every chat verb refuses it, reads and a public room's included: `403 unauthorized` with reason `unseated`. That is the authority table's row for each chat verb, not a rule beside it: the act route refuses nobody for being unbound ([ADR-0024](../../adr/0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md)), and chat's verbs are made as a seat. A pipeline that should post gets a human seat nobody holds, and its service account is bound to it (`crewlet iam bind`). The three gestures that [destroy](#retention) are the exception: said in nobody's voice, an administrator's own credential may make them, with a recent proof, each recorded under whoever made it (a person's seat, or a credential's own login).

**A seat's chat is the seat's, not the person's.** Rooms, direct conversations (identity derived from seat handles) and read state are kept under the seat. When a person moves seat, or leaves and somebody else takes theirs, the seat's **next holder inherits its chat**: memberships, direct conversations, read cursors, mutes and do-not-disturb, as the seat's inbox, pins and personal views follow it ([ADR-0032](../../adr/0032-every-person-holds-a-human-seat.md)). The person who moved starts their new seat with that seat's chat. Moderation follows nobody's seat: a delete of somebody else's words is the administrator's, whichever seat they hold.

**Read state** (how far each seat has read in each room, what it muted, whether its person is in do-not-disturb) lives in [coordination](coordination.md), not on the log: a cursor is one reader's attention that nobody replays, and at a realistic flush rate it would be the highest-volume thing the company writes. It has no expiry (an expired cursor would mark a finished room unread); a seat's read state is forgotten [24 hours after the seat leaves the org chart](seat-ownership.md#the-removed-seat), the grace a removed seat's mailbox gets, and kept if the seat is restored before then.

It is written through the operator surface's `mark_chat_read`, like every dashboard gesture, and every node that serves sockets **watches** those records, so a flush that lands is pushed to the seat's open screens wherever attached. It is derived from the record itself (no message for a peer to forge), and a second tab on another ingress node clears its badge with the first. Each pushed state carries the revision the record stands at and a screen keeps the highest, so out-of-order pushes cannot restore an older state. A flush whose answer was lost may have landed: it answers `unknown`, safe to repeat because a cursor only moves forward, and the watch pushes the record as it stands. A mute is per room, not a list: a flush mutes or unmutes the rooms it names and every other keeps its state, so a screen mutes its room without reading what else is muted, and two tabs muting two rooms both land.

The same surface's `list_channels` counts each room's unread room posts (a reply is read in its thread, so does not badge the room), how many name the person or the whole room (`@channel`), and marks the seat's muted rooms and a do-not-disturb its person is in, so their assistant sees what the dashboard's badges show. A read state that cannot be read is said, not counted from zero, which would call every room unread.
## What a reader keeps

A person keeps a few marks of their own in chat, stored with their read state: in
[coordination](coordination.md), under the seat they hold. They pass to the next
holder of the seat and reach every tab through the same push as a read cursor.
None is on the chat log or affects anybody else.

- **How a room reaches them.** Every message, mentions only (the default) or
  muted. A person is never woken, so the level only decides what their own
  screens raise; it is not the membership's `follow_all`, the wake policy for a
  seat (the org chart's in a unit's room).
- **Starred rooms**, listed first on their screens.
- **Saved messages.** A save names its room and message. The message shows only
  in that room and only while they may read it: one taken back, erased, pruned by
  the room's horizon or in a room they left is listed as gone, and the save stays
  until they let it go.
- **Threads followed or unfollowed by hand.** A thread they started, replied in
  or were named in is followed without a mark. A mark decides either way: an
  unfollow quiets a thread they spoke in, a follow takes in one they never spoke
  in. Replying again does not undo an unfollow; the dashboard follows the thread
  again when they reply in it.
- **A place per thread.** Reading a room does not open its threads, so a reply is
  new to them while it is above both the room's place and the thread's own.
- **Mark unread.** A place only moves forward, so two tabs flushing at once never
  move it back; setting a room's place back before a message is the one
  exception, made where asked.

Each mark is named on its own, never as the whole list, so two tabs marking two
things at once both land. A person may star 256 rooms, set 256 rooms to every
message and save 256 messages; one more is refused. Thread marks are bounded at
256 too, but past that the oldest mark goes: every unfollow leaves one, and a cap
that refused would keep somebody in a thread they asked to leave. Every mark is
written through `mark_chat_read`, as a read cursor is, and every room, message
and thread it names is by the id Chatlet gave it: a flush naming any other is
refused, so a person's whole read state stays within 144 KiB.

From those marks and the conversation, each reader has views of their own, each
over the rooms they may read only:

- **Activity**: the moments they were named (or the whole of a room they are in
  was), replies in threads they follow, and reactions to their messages, newest
  first. A mention or reply is unread above their places for it, the measure the
  room's badge counts; each row carries the position that marks it read. A
  reaction is never unread.
- **Threads**: threads they follow, newest reply first, each with its root and
  newest replies.
- **Saved** and **Sent**: what they saved, and what they said in every room they
  may still read.
- **Shared in a room**: every link the room's messages carried, newest first,
  with who shared it and when.
- **Link previews**: a link to one of this dashboard's own tasks, pages or
  artifacts is drawn as that record, read as the reader (one they may not open is
  a plain link); a link to a pull request in the company's own code on GitHub is
  drawn with its title and state
  ([what that discloses](../integrations/github.md#pull-request-previews-in-chat)).
- **One message in place**: a message by id, with its thread's root and the
  replies just before it; how Activity and Saved open one where it was said.

## Search

Chatlet has its own keyword index, separate from Doclet's, and **no semantic
search at all**
([ADR-0071](../../adr/0071-chat-is-a-keyword-corpus-and-takes-no-vectors.md)).

Why: a message is a remark about work, not a statement of it (the argument that
keeps a work item's comments out of the knowledge index), so mixing corpora would
let one busy conversation outrank every page written on purpose. And embedding a
year of chat needs several times the entire supported vector corpus, against a
budget shared with every other corpus.

The index is tuned for a bimodal corpus, one-line acknowledgements beside pasted
blocks; Doclet's length normalisation would hand every query to whoever typed the
shortest reply.

**A search is answered for its asker.** It ranks over the rooms that seat may
read, worked out by the ranking node in the same step, and a hit is scored by
those rooms' own statistics, so how often a word is said in a room you cannot
read never moves your ranking. An answer says how many rooms it searched, and one
from an index still being built says so rather than reading as nothing said.

Every data node keeps the index beside its copy of the rooms. A node with no data
keeps neither: a seat's `search_messages` there is answered by a data node, as is
any search on a data node whose own copy is out of service. A query is at most
400 bytes, the bound every ranked search shares; a longer one is refused, not
cut.

## Retention

Chat is the highest-volume thing the engine stores, in the replicated estate
every snapshot copies and every rejoining node transfers. So, like each data
node's usage days, it keeps its rows to a **content horizon**, which unlike
theirs is the company's to set:

```yaml
chat:
  chatlet:
    message_retention_days: 365   # the default; 0 keeps everything for ever
```

A room may carry a horizon of its own, which beats the company's; setting one is
an administrator's, because a shorter horizon is a prune on a delay. The sweep is
a **record**, not a local delete: a fleet-singleton duty (`chat-prune`) publishes
a cutoff and every data node deletes exactly the same rows, because a message's
instant is the broker's, not a clock each node reads.

The duty prunes only by the horizon of the revision the fleet's
[activation pointer](control-plane.md#the-company-and-what-each-node-keeps-of-it)
names, and only on a node running that revision. A node serving another one
(it refused the fleet's revision, or booted on an offline import the fleet has
not taken) publishes no cutoff and does not claim the duty, so a node running the
fleet's revision takes it over. A horizon is enforced late, never early: an hour
missed is pruned on the next tick, and a message pruned by a horizon the company
has since raised, or set to `0`, is gone from every data node for good.

Keeping everything is supported and costs disk on every data node, in every
backup, and in the time a new node takes to join.

When waiting out the horizon is no answer, two gestures act now, and neither has
an inverse:

- A **credential pasted into a message**:
  [`crewlet chatlet erase`](../reference/cli.md#crewlet-chatlet-erase) destroys
  exactly the messages named, on every data node, and leaves the conversation
  around them standing.
- An **erasure request** covering a room's history:
  [`crewlet chatlet prune`](../reference/cli.md#crewlet-chatlet-prune) publishes
  the duty's own cutoff now, for one room, and everything the room said before
  that instant goes.

Both, and a room's own horizon, are routes of the
[human write surface](../reference/api-endpoints.md#a-chat-erase-prune-or-horizon):
`admin`, with
[a recent proof](identity-and-access.md#some-gestures-ask-how-recently-you-proved-who-you-are),
never a seat. None reads the room first, so an administrator can take a
credential out of a private room they are not in without seeing the rest. Listing
rooms, reading one, posting as yourself and searching are the rest of
`crewlet chatlet`, all in the [CLI reference](../reference/cli.md#crewlet-chatlet).

In the dashboard an administrator finds the same three on a room they can open:
the room's horizon and **Delete older messages** in its details (About), and
**Erase for good** in a message's More. Each asks in the page first (horizon and
prune with what they destroy typed back, erase with its reason), and the request
carries the confirmation the route checks. Archiving or reopening a room sits
beside them for the room's creator, its team's lead and an administrator, and is
held with that sentence for anybody else.

## Who may do what

One table decides every chat gesture, for a seat's own tools, the dashboard and a
person's assistant alike. Chatlet is one **app**, and its permission is
`chatlet`: every agent seat holds it, and a person holds it when an administrator
ticks it. **Every chat verb is made as a seat**, reads included, so each asks that
the caller acts as one. *Which room* is the chat domain's own rule, not a
permission: a public room and a unit's room are every seat's to read, a private
room and a direct conversation their members'; a room you may not read answers
exactly as one that does not exist. No permission widens that: `admin` reads no
room it is not in.

| Gesture | Who may |
|---|---|
| List rooms, read a room or a thread, search | `chatlet`, as a seat — over the rooms that seat may read |
| Post, reply, send a direct message, react | `chatlet`, as a seat, in a room it may reach; a post or a reply is refused in an archived room |
| Join or leave a room | `chatlet`, as a seat, its own membership only; a private room cannot be joined, and a unit's room can be neither joined nor left |
| Make a room, open a direct conversation, set a room's topic or purpose, add people to it, state its whole membership | `chatlet`, as a person (never an agent) in a room they can reach; a unit's room is the org chart's, and a direct conversation's membership is its identity |
| Make a public room take in everybody the company takes on | `admin`, as a person, never an agent |
| Archive a room, or open it again | the room's creator, the lead of the unit whose room it is or of a unit above it, or `admin` — as a person, never an agent |
| Edit a message | its author, as a person — nobody else, an administrator included |
| Take a message down | its author, as a person, or `admin` as a moderator — in any room, without reading it |
| Mark what you read, mute a room, set how it reaches you, star it, save a message, follow or unfollow a thread, do-not-disturb | your own record only, as a person, kept under your seat |
| Set a room's horizon, erase messages, prune a room | `admin`, with a recent proof, and never a seat — a person, or an administrator's own credential acting as itself |

## What is not here

Two gaps, stated rather than discovered:

**No file attachments.** A message carries links, and the file lives somewhere
built for files. Bytes on the chat log would be copied to every data node, kept
for the log's whole replay window and replayed by every joining node. Company
files have a home, the [object store](object-store.md), where a file has a path
and a collector frees its bytes once nothing names them; a message links to one
of those, or to a page, a work item or anything else with an address. A message
naming a stored object would be a second owner of its lifetime, which every prune
and erase would have to release on every data node in step; this build has none
of that, so a message names no object.

**No notification when you are away.** No email, push or SMS; the engine has
never sent anything as itself. A person learns they were mentioned when they next
open the dashboard. This is the largest difference from a commercial chat product
and the one most worth knowing before you choose the axis.

## Migrating

Because the axis is exclusive, the cutover is a flip, not an overlap. The old
workspace stays readable in its own product for as long as you keep it. There is
no importer: the engine does not replay another product's archive into Chatlet
rooms.
