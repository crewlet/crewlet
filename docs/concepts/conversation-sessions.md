# Conversation Sessions

A seat that answered a Jira comment on Monday and is woken by a follow-up on
Wednesday **continues the session Monday's turn left**: its executor's harness
picks up the transcript where that turn's run stopped — what it was asked,
every call it made and what came back, what it replied — and is told only what
no transcript can say: how the conversation's earlier turns ended, that a new
turn begins here, and that its box is new.

Every launch's harness session is kept: taken out of its box when the run
ends, sealed into the fleet's [object store](object-store.md) and named by the
turn's row of the `executor` state log
([ADR-0073](https://github.com/crewlet/crewlet/blob/main/adr/0073-a-seats-executor-session-is-a-sealed-object-its-turns-row-names.md);
[Code Sandbox](code-sandbox.md#each-launchs-session-kept)). The turn's own
relaunches continue it, and so does the next turn of its conversation — the
thread, the issue, the pull request or the work item its trigger named, or the
schedule that fired it.

---

## What continues what

| Launch | Continues |
|---|---|
| A relaunch of a turn whose job before left a kept capture | That capture, in any box ([A relaunch continues the turn's session](code-sandbox.md#a-relaunch-continues-the-turns-session)) |
| A turn's **first** launch | The session its conversation's last turn left: the conversation's **head**, the ended turn whose row still names a kept session and whose read of the conversation was the latest ([Read once, frozen, held](#read-once-frozen-held)), restored into a box of the launch's own — a session that turn **went on past** included: an earlier capture its row keeps where its last word was not kept (its final capture refused, a stop or a loss whose box was gone, a relaunch that kept nothing), which its run is told ends where it was kept |
| A relaunch of a turn that has kept no session of its own — its first job lost with its box, its capture refused or unanswered — in a box of its own | The head its first launch was handed (the turn's **base**), never a second read of the conversation, handed after its lead the relaunch's reseed whole: what became of the run before it, and every call the round's runs made so far ([A relaunch continues the turn's session](code-sandbox.md#a-relaunch-continues-the-turns-session)) |
| A schedule's **next fire** | The session the schedule's last fire left on the seat: a schedule is a conversation of each seat that runs it ([Schedules](#schedules)) |
| Any launch, of a session a run [ran out of context](#a-session-that-ran-out-of-context-is-retired) in | Never that session: a session of its own |
| Anything else | A session of its own, the reason logged as `executor_session_not_continued` |

A turn that serves no conversation — a colleague's ask, a generic source's
wake — continues nothing from an earlier turn: its own relaunches continue its
session, and its next wake starts fresh.

## What the engine adds

Everything a seat did and said in the conversation is in the session it
continues. The engine adds three things a transcript cannot know, and nothing
else ([Code Sandbox](code-sandbox.md#a-conversations-next-turn-continues-its-last-turns-session)
has the mechanism):

- **How the conversation's earlier turns ended.** Every brief of the turn —
  the fresh brief, a relaunch's reseed, and the brief a continued session is
  handed after its lead — carries `## How this conversation's earlier turns
  ended` above `## Task`: for each turn, oldest first, its outcome and its
  reviewer's last word; whether its reply reached the party waiting and on
  what surface, or that whether it did is not known; what its reviewer said
  landed; what blocked it; who stopped it and why; what failed, as the engine
  recorded it; that its newest session was not kept, and why; the engine's
  answer to a colleague's ask it took up; and what it was asked. Its rules: a
  reply that reached the party waiting is said and is not said again; one that
  did not may still be owed; everything else a turn did already took effect,
  to follow up rather than redo; and this is history, not the current state —
  where it and the task disagree, the task wins.
- **Where the new turn begins.** A continued session is handed a lead first:
  everything above is the conversation as its turns left it, through the
  base's turn and the instant it ended — or, where that turn's end said it went
  on past the session, up to the instant the session was kept while it still
  worked, what it did after not being above and how it ended told below,
  saying too where the turn was then lost; or, of a turn that was lost and
  whose end said nothing of going on, through it, the session perhaps not
  holding the last of what it did; a new message has woken it; what it
  read above may be stale; and tools it activated and skills it loaded above
  were set up anew — its [skill guard](tool-skills.md#session-scope--why-per-phase-not-per-turn)
  starts empty, whatever the base's session loaded.
- **That its box is new.** A base is continued only in a box of the launch's
  own, so the lead says it is on a fresh checkout, that what the session had
  not committed and pushed went with the box it was worked in, and that
  absolute paths under the base's home were that box's. A run offered
  `code_checkout` is told that only a repository the box's setup clones is a
  fresh checkout: one the session checked out of Gitlet is
  not in the box until it checks it out again, at its branch's head
  ([Code Sandbox](code-sandbox.md#repositories-on-gitlet)).

A schedule's conversation is told all three in its own words: the block is
`## How this schedule's earlier fires ended`, each of its accounts a fire's,
and the lead `## A new fire of this schedule begins here`, saying that the
session above is the schedule's last fire's and that the schedule firing
again, not a message, woke it ([Schedules](#schedules)).

**No reply text.** The turn's own reply is in the session that continues it;
a session started fresh is told whether the reply reached anybody and what
landed, and the surface's own context — the thread block, the item's comments
— shows the reply itself. The engine records no second copy of what a seat
said to put beside the transcript: two histories of one conversation are two
answers to "what did I already do here", free to disagree.

**Fenced.** Every account in the block is somebody's words — what the turn was
asked, what its reviewer and executor wrote, a stop's reason, what a box or a
store said when something broke — so each is fenced under the turn's token,
and the name of whoever stopped a turn is defused in place
([ADR-0040](https://github.com/crewlet/crewlet/blob/main/adr/0040-content-a-model-reads-carries-its-provenance.md)).
The block is rendered once, at the turn's dispatch, and every later brief of
the turn carries it byte for byte.

---

## Read once, frozen, held

**Read once, at the turn's dispatch.** A turn with a conversation reads it once,
as its dispatch starts it, at `linearizable` through the
[estate router](../guides/satellite-nodes.md), so an end another node
published is in the copy that answers: the head, and the conversation's other
ended turns no ended turn was shown — the newest sixteen, the rest counted —
with the siblings that ended beside the head with a session of their own. No
launch of the turn reads it again, so a turn of the same conversation that ends
meanwhile moves nothing. A read that fails, or that the read ladder refuses,
fails open: the turn runs with no endings and continues nothing, logged as
`executor_conversation_unread`.

**Bounded by whole endings.** The block always tells the head, then the others
newest first while they fit **24 000 bytes** — about six thousand tokens — and
counts the rest, never cutting inside a turn's ending; a head whose accounts
alone would not fit is told without them, saying so. The bytes are counted as
the turn's run record carries the block, escapes included, so an account that
quotes code or markup is told in fewer turns rather than taking more of the
record.

**Frozen at the first launch.** The turn's first launch writes the read onto the
run's record it creates — where it was read, the turns its brief told, and the
head as the turn's base — and every relaunch builds from that record, in any
box on any node. The rendered block rides the run's parked state, so every
fresh fallback of every later segment is told the same endings.

**Held on the log.** The first launch records its read on the executor log as the
turn's `begin`, and from then until the turn's first landed capture or its
end, **no release takes the base**: not the seat's session budget, not a
sibling turn's end, not the retention. A turn whose run is gone with no end
recorded is ended `lost` a day after its last record, which lifts its hold; a
first launch abandoned once its record was written ends its turn at once.

**The head is the turn that read the conversation last.** Every first launch
continues the head its read named, so a conversation's sessions are a line,
each continuing the one before. Two turns of a conversation can run beside each
other — a turn parked on a person's question frees its seat, and a message that
is not taken as its answer runs beside it — so a turn that read the
conversation early can end after turns that read it later and continued each
other. It continued none of what they left, so the head is never simply the
turn that ended last: it is the ended turn with a kept session whose read was
the latest, the last to end among turns that read at one position, and a turn
whose read failed comes after every turn that read. A turn that read early and
ended late is told and offered beside the head, never continued, and the next
turn's end absorbs it. A schedule's fires run beside a parked fire every time
([Schedules](#schedules)). The seat's session budget still releases the oldest
end first ([`max_bytes_per_seat`](#configuration)), so it can release a head
before a stale sibling — a turn that read the conversation earlier and ended
later — and the next turn then continues, of the sessions the conversation
still keeps, the one that read it last, and is told the released head's ending
as it is told every other.

**A turn's end absorbs what its brief told.** Once a turn of the conversation
that kept a session of its own ends, the turns its brief told — the head it
was handed, whether or not it could continue it, and the endings it was told —
are gone from the log, their sessions released: the new turn's session holds
the head's transcript and its own, and its brief told the rest. A turn the
bound left out is not absorbed, and the next turn is told it. Siblings are
told and absorbed, never continued or held.

## When a turn starts fresh

The turn is handed the brief a session of its own gets — which still carries
the endings — and, but for the first, the reason is logged as
`executor_session_not_continued`:

- the conversation has no head: its first turn, or every earlier turn's
  session released, absorbed or past the retention;
- a run [ran out of context](#a-session-that-ran-out-of-context-is-retired) in
  the session the turn would have continued;
- the head was not read: the turn has no conversation key, the switch is off,
  or the read failed;
- no member of the seat's chain restores the base's harness, format and
  release, or the launch picked a member that runs another harness;
- the base's context fits no capped window of the seat — where it fits one
  that is full right now, the seat's mail waits instead ([Cost](#cost));
- the `begin` record did not hold the base, or the log did not answer it
  within fifteen seconds;
- the box's harness cannot take the session up, the store no longer holds its
  object, or the restore fails in the box;
- or the launch reuses the box its turn's earlier job ran in, whose own
  session is that job's to continue in place.

Where the turn read a head — every case above but the first three — the brief
also names it, under `## A kept session this run does not continue`: that the
conversation's last turn worked in a session this run does not continue (on a
schedule's fire, in the schedule's words: that its last fire did), and that
`read_session` reads it under that turn's id
([A seat reads its own](code-sandbox.md#a-seat-reads-its-own)) — never why,
which is the log's to say. Only where the seat is offered `read_session`, and
never in the continuation a session reads; a relaunch that cannot take up the
turn's own kept session names that one the same way, a round lost a second
time included, and a relaunch of a turn that has kept nothing names the head
its first launch froze only where the turn's row was read holding it.

---

## A session that ran out of context is retired

A run whose conversation no longer fits its model's context fails with
`error_kind` `context_exhausted`, and another launch of the same session fails
the same way, so the engine continues it no further:

- **Within the turn**, the collection records the session on the run's record,
  so a relaunch continues neither its kept capture, nor the base it froze, nor
  the copy its box holds. It starts a session of its own, on the fresh brief, and
  a session started that way is kept and continued as any other.
- **After the turn**, its end names the session on the executor log, which takes
  it off every ended row of the conversation that names it, on every data node.
  So the next turn's read does not hand it out, whichever turn's row kept it, and
  a hold on it by a live turn does not keep it either: a hold exists for a
  relaunch to continue the session, which would only meet the same end.

**What carries the conversation on is its turns' endings**, as the brief told
them, rather than the transcript. A turn whose own row loses its session as it
ends absorbs nothing, so the next turn is told how that turn ended and how the
turns it read ended too; a later turn absorbs them once it has kept a session of
its own. The brief says
a session ran out, so the turn knows why it holds no transcript.

**The job's final session is not kept at all.** A launch's record says why
(`session_not_kept`), and nothing is uploaded: no launch would ever continue it,
and its bytes could release a session of another conversation against the seat's
budget. A session retired after an earlier capture of it landed goes the same
way, and how its turn ended stays.

**An operator sees it.** The launch that starts fresh logs
`executor_session_not_continued`, naming the session and the turn it ran out in,
and a seat's [Sessions](#reading-it) say so of each row. Another session of the
conversation that has not run out stays, and is continued as the head where it
is one. A conversation that keeps running out on fresh sessions, whose brief or
window is the problem rather than any one session, is reported as
[`executor.context_exhausted_loop`](../guides/engine-issue-reports.md).

## A session its provider refuses is retired too

A continued session sends what it holds on every request, so a session that
holds something the model's provider will not take (a tool definition under a
name outside the provider's pattern, a message it no longer accepts) is refused
before the model reads a word, on every launch. The harness answers such a
request as text, under its own failure flag and the provider's status. The
engine reads the status, never the sentence:

- **The launch failed, with a kind of its own.** Its record says
  `error_kind` `request_refused` and carries the provider's message as `error`.
  It is never the run's report, so no reviewer judges it and the stall guard
  does not compare it with the round before.
- **The session is retired**, exactly as one out of context is: nothing of it is
  kept (`session_not_kept` says why), no relaunch continues it, and the turn's
  end takes it off every ended row of the conversation, so the next turn, a
  schedule's next fire included, starts a session of its own.
- **The round is carried on once, in a fresh session**, where the refused job
  had continued one: the same round, relaunched on the fresh brief, told that
  the session before it was retired. The launch logs
  `run_relaunched_fresh_session`.
- **A refusal a fresh session does not cure ends the turn.** A job refused in a
  session it started, or refused again after that one relaunch, ends its turn
  `failed` with a `turn.guard_breach` of kind `request_refused`, whose `detail`
  is the provider's message.
- **Twice in a row on a fresh session holds the conversation.** It stands as a
  platform notice (`conversation.held`, cause `request_refused`) until a person
  writes on it or the seat's models or the company's tool servers change, so a
  schedule whose every fire is refused stops firing into the refusal and an
  administrator is told ([Advisor](../guides/advisor.md)).

The statuses read as a refusal are 400, 413 and 422. A spent plan (429), an
outage and a refused sign-in are each handled as before, and a conversation
past its model's context keeps its own kind.

## Where a session lives

**On the executor log, the same on every data node.** A turn's row — its
conversation, how it ended, and the session it kept — is a row of the
`executor` state log, a replicated domain every data node derives identically
from one ordered stream ([Where state lives](architecture.md#5-where-state-lives));
a node without `data` reads it through the estate router. The session's bytes are one sealed object in the
fleet's [object store](object-store.md). So a seat that moves between nodes
finds its conversations' sessions wherever it lands, and a turn launched on
any node continues them.

**Keyed by the seat and the conversation.** A row is the seat's, under its
handle, and its conversation's: `conversation_key` is the
**conversation identity** the trigger was stamped with — the `{source}:{local}`
grammar of [the event system](event-system.md#inbox-batching--coalescing),
`jira:POC-7`, `slack:C9:1718.001`, `github:acme/api#42`, `work:task:<id>`, or a
schedule's own, `schedule/unit/backend/daily-standup` —
never the partition key the seat's inbox coalesces on. The two are the same
string for every source but chat, and for chat they differ on exactly one
surface: **a direct message is one conversation however it is threaded**, so
a DM's turns key on the bare channel (`slack:D1`) while a reply inside it still
partitions on its thread, and the reply's turn continues the burst's session.

Two seats serving one conversation — a lead and its report on one ticket —
each continue their own sessions, never each other's.

## Reading it

A seat's sessions are its **Sessions**, under the Memory section of its page's Activity tab
([A seat's profile](../reference/dashboard-design.md#a-seats-profile)), and
[`GET /agents/{id}/sessions`](../reference/api-endpoints.md#get-agentsidsessions):
one row per turn, from its first launch where the turn read its conversation
and from its first capture otherwise, the latest begun first, with how it
ended, what it was asked, whether its session is kept and, where not, why —
and **which launch would continue it** (`continued_by`: `relaunch` for a live
turn's, `next_turn` for an ended turn's, `next_fire` for a schedule's ended
fire's), whether it would and where, decided
by the decision that launch takes. A row that is not its conversation's head
names the turn whose session the conversation's next turn is handed instead,
and `base_turn` names the session a turn's first launch was handed, so a
conversation's line of sessions reads from row to row. A turn's transcript is
`GET /agents/{id}/sessions/{turn_id}`. Rows go as the turns that were handed
them end, with the retention, and with their seat
([Reading a seat's sessions](code-sandbox.md#reading-a-seats-sessions)).

## Schedules

A schedule is a **conversation of each seat that runs it**. The scheduler
stamps every fire with the schedule's key — `schedule/<scope>/<scope id>/<name>`,
each part escaped — so one seat's fires of one schedule are one conversation of
that seat: the next fire's first launch continues the session the last fire
left, its brief tells how the earlier fires ended, and its end absorbs the
fires its brief told, as a conversation's next turn does. A unit schedule each
member runs is one conversation on each member's seat, and the fires that
waited in a seat's inbox together run once, on that one key.

- **What keeps the line, and what starts it afresh.** An edit to the schedule's
  `task`, `cron`, `timezone` or `timeout_seconds` keeps its conversation. A
  schedule renamed, moved between a role and a unit, or on a role of a company
  whose `name` changed has a new key and starts fresh; and a seat that starts
  running a schedule — a `lead` schedule's new lead — starts it on its own
  seat.
- **Kept for the retention.** A schedule whose fires are further apart than
  `retention_days` never continues: its last session is gone before its next
  fire comes.
- **Paid for on every request.** A continued fire sends the schedule's session
  with every request of its run, up to about its model's compaction point each
  with Claude Code, where a fire started fresh begins at its brief. There
  is no per-schedule switch: `enabled: false` starts every fire fresh, as it
  does every conversation's next turn.
- **Nobody replies into it.** Every fire carries the key and nothing a person
  writes does, so a fire is never taken as the answer to the question the last
  fire's run parked on; such a run is answered by its turn.
- **A fire parked on a question holds nothing up.** The schedule's next fires
  run beside it, each continuing the one before; when the parked fire is
  answered or stopped and ends after them, the next fire still continues the
  last of them, while the seat's session budget keeps its session — the head is
  the turn that read the conversation last — and is told how the parked one
  ended ([Read once, frozen, held](#read-once-frozen-held)).

[Scheduling](scheduling.md#each-fire-continues-the-last-fires-session) has the
whole of it.

---

## Cost

**A continued turn sends its session's context first**, and every request of its
run carries that context, mostly read from the prompt cache and every token of
it counted against the seat's windows. It is bounded by the harness's own
compaction, each at its model's own window less the harness's headroom, so a
conversation's session, however many turns it spans, never makes a request much
larger than one turn's
([What a continued launch costs](code-sandbox.md#what-a-continued-launch-costs)).
A schedule pays it on every fire: a fire of ten Claude Code requests on a
session at the window is charged about a million input tokens, most of them
cache reads ([Schedules](#schedules)).

**Its first launch is held to that context.** The launch needs the larger of
`turn_engine.executor_min_budget_tokens` with the turn's fresh brief beside it,
which its box starts a session of its own on wherever it cannot take the base
up, held at the floor alone, or at the room below it, where that is past the
most room a window can leave the launch
([What a continued launch costs](code-sandbox.md#what-a-continued-launch-costs)),
and the base's context with the lead and the brief beside it. A base no
capped window can ever carry starts fresh; a
first launch refused only because a window is full right now **parks the seat's
mail on that figure** until a window has room for it, as a held resume does
([Budgets and Spend](../guides/budgets-and-spend.md#budget-windows)).

**The endings ride every brief of the turn**, at most 24 000 bytes, and nothing
else is added: a continued session is not re-told what it holds.

---

## Configuration

```yaml
turn_engine:
  conversation_session:
    enabled: true                 # keep and continue sessions, and tell a
                                  #   conversation's next turn how its earlier
                                  #   turns ended; a live kill switch
    retention_days: 30            # days a seat's sessions are kept past their
                                  #   turn's last record, read at every pass
    max_bytes_per_seat: 67108864  # object-store bytes one seat's sessions
                                  #   may hold (33 MiB..64 GiB)
```

Nested under `turn_engine` rather than beside it, which is load-bearing: it
rides the live turn-engine settings cell, so it hot-reloads through the
existing turn-engine diff handler with no extra apply-config wiring.

**`enabled: false`** keeps no executor launch's session — nothing is captured
or stamped ([Code Sandbox](code-sandbox.md#each-launchs-session-kept)) —
continues none, reads no conversation's head, tells no turn how its
conversation's earlier turns ended, and offers no seat a tool to read its
sessions ([A seat reads its own](code-sandbox.md#a-seat-reads-its-own)): one
switch for everything a conversation carries between turns. The sessions kept
before it went off are read by no model — a launch already running when its
node applies the revision is refused at its next call to either tool, which
asks the revision its node runs — and stay a person's to read as the seat's
trail. How each turn ended is still recorded on the executor log, so
turning it back on finds the conversation's endings.

**`retention_days`** is how long a seat's sessions are kept past their turn's
last record: the trash reaper deletes a row, and the session it names, that no
later turn took up first, reading the value off the revision the fleet runs at
every hourly pass — a revision that moves it is honoured at the next pass, not
the next start — and never deletes one whose turn's run is still live or whose
session a live turn holds. A turn that never ended, a day past its last record
with no run of it live, it first ends as `lost`, so a lost end holds the seat's
budget for a day rather than for the whole retention. A conversation quiet for
longer than the retention starts its next turn fresh, and so a schedule whose
fires come further apart than the retention starts every fire fresh
([Schedules](#schedules)).

**`max_bytes_per_seat`** is the most one seat's sessions may hold: past it a
capture releases the seat's oldest ended sessions first, keeping how each turn
ended, and a capture the seat's live turns leave no room for is refused, so
that turn's next launch starts fresh. A session a live turn's first launch was
handed is held until that turn keeps a session of its own or ends, so it
counts beside the seat's live turns for as long. The default, 64 MiB, holds a
live turn's session at its 32 MiB bound beside about twenty ordinary ones (a
day's work compresses to one or two mebibytes); the least it accepts, 33 MiB,
is one session at that bound once sealed, and every seat's sessions share the
object store with the company's files, so a company of forty seats or more on
the default `nats` bucket belongs on S3 (`store.objects.backend`).

---

## Related

- [Code Sandbox](code-sandbox.md) — where a session is kept, continued and read
- [Turn Engine](turn-engine.md) — the phases, the within-turn prior-work ledger, and how a turn ends on the executor log
- [Agent Learning](agent-learning.md) — episodes, the diary, counterparty profiles
- [Event System](event-system.md) — the two keys, and inbox coalescing
- [Scheduling](scheduling.md#each-fire-continues-the-last-fires-session) — a schedule as a conversation of each seat that runs it
- [Architecture](architecture.md#5-where-state-lives) — where state lives, and the replicated domains the executor log is one of
