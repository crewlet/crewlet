
# Conversation Sessions

A seat that answered a Jira comment on Monday and is woken by a follow-up on
Wednesday **continues the session Monday's turn left**: its executor's harness
picks up the transcript where that turn's run stopped and is told only what no
transcript can say: how the conversation's earlier turns ended, that a new turn
begins, and that its box is new.

Every launch's harness session is kept: taken out of its box when the run ends,
and [saved as it works](code-sandbox.md#a-running-jobs-session-saved-as-it-works),
sealed into the fleet's [object store](object-store.md) and named by the
turn's row of the `executor` state log
([ADR-0073](https://github.com/crewlet/crewlet/blob/main/adr/0073-a-seats-executor-session-is-a-sealed-object-its-turns-row-names.md);
[Code Sandbox](code-sandbox.md#each-launchs-session-kept)). The turn's own
relaunches continue it, and so does the next turn of its conversation (the
thread, issue, pull request or work item its trigger named, or the schedule that
fired it).

---

## What continues what

| Launch | Continues |
|---|---|
| A relaunch of a turn whose job before left a kept capture | That job's newest kept capture, in any box: in that job's own box where the relaunch carries the round on there, its own copy where it went on past that capture ([A relaunch continues the turn's session](code-sandbox.md#a-relaunch-continues-the-turns-session)). A job lost with its box continues from its newest save, as jobs are saved as they work and as their node drains ([saved as it works](code-sandbox.md#a-running-jobs-session-saved-as-it-works)) |
| A turn's **first** launch | The conversation's **head**: the ended turn whose row still names a kept session and whose read of the conversation was the latest ([Read once, frozen, held](#read-once-frozen-held)), restored into a box of the launch's own. This includes a session the turn **went on past**: an earlier capture its row keeps where its last word was not kept (final capture refused, stop or loss with the box gone, a relaunch that kept nothing); the run is told the session ends where it was kept |
| A relaunch of a turn that has kept no session of its own (its first job lost with its box, its capture refused or unanswered), in a box of its own | The head its first launch was handed (the turn's **base**), never a second read of the conversation, handed after its lead the relaunch's reseed whole: what became of the run before it, and every call the round's runs made so far ([A relaunch continues the turn's session](code-sandbox.md#a-relaunch-continues-the-turns-session)) |
| A schedule's **next fire** | The session the schedule's last fire left on the seat: a schedule is a conversation of each seat that runs it ([Schedules](#schedules)) |
| Any launch, of a session a run [ran out of context](#a-session-that-ran-out-of-context-is-retired) in | Never that session: a session of its own |
| Anything else | A session of its own, and where a session was lost, it says why ([When a run does not continue its session](#when-a-run-does-not-continue-its-session)) |

A turn that serves no conversation (a colleague's ask, a generic source's wake)
continues nothing from an earlier turn: its own relaunches continue its session,
and its next wake starts fresh.

## What the engine adds

Besides the session, the engine adds three things and nothing else
([Code Sandbox](code-sandbox.md#a-conversations-next-turn-continues-its-last-turns-session)
has the mechanism):

- **How the conversation's earlier turns ended.** Every brief of the turn (fresh
  brief, relaunch reseed, and the brief a continued session gets after its lead)
  carries `## How this conversation's earlier turns
  ended` above `## Task`. For each turn, oldest first: its outcome and its
  reviewer's last word; whether its reply reached the party waiting and on what
  surface, or that this is not known; what its reviewer said landed; what blocked
  it; who stopped it and why; what failed, as the engine recorded it; that its
  newest session was not kept, and why; the engine's answer to a colleague's ask
  it took up; and what it was asked. A reply that reached the party waiting is
  not said again; one that did not may still be owed; everything else a turn did
  took effect, so follow up rather than redo. It is history, not current state:
  where it and the task disagree, the task wins.
- **Where the new turn begins.** A continued session gets a lead first, saying:
  everything above is the conversation through the base's turn and the instant it
  ended (where that turn's end said it went on past the session, up to the instant
  the session was kept, with what it did after and how it ended told below, and
  where the turn was then lost; for a lost turn whose end said nothing of going
  on, through it, the session perhaps not holding the last of what it did); a new
  message has woken it; what it read above may be stale; tools it activated and
  skills it loaded were set up anew, so its
  [skill guard](tool-skills.md#session-scope--why-per-phase-not-per-turn) starts
  empty.
- **That its box is new.** A base is continued only in a box of the launch's own,
  so the lead says it is a fresh checkout, that what the session had not
  committed and pushed went with its old box, and that absolute paths under the
  base's home were that box's. A run offered `code_checkout` is told that only a
  repository the box's setup clones is a fresh checkout: one the session checked
  out of Gitlet is absent until checked out again, at its branch's head
  ([Code Sandbox](code-sandbox.md#repositories-on-gitlet)).

A schedule's conversation is told all three in its own words: the block is
`## How this schedule's earlier fires ended`, and the lead
`## A new fire of this schedule begins here`, saying the session above is the
last fire's and the schedule firing again, not a message, woke it
([Schedules](#schedules)).

**No reply text.** The turn's own reply is in the session that continues it; a
fresh session is told whether the reply reached anybody and what landed, and the
surface's own context shows the reply. A second recorded copy could disagree.

**Fenced.** Every account in the block is somebody's words, so each is fenced
under the turn's token, and the name of whoever stopped a turn is defused in place
([ADR-0040](https://github.com/crewlet/crewlet/blob/main/adr/0040-content-a-model-reads-carries-its-provenance.md)).
The block is rendered once, at dispatch, and every later brief of the turn carries
it byte for byte.

---

## Read once, frozen, held

**Read once, at the turn's dispatch.** A turn with a conversation reads it once
at `linearizable` through the [estate router](../guides/satellite-nodes.md), so
an end another node published is included: the head, and the conversation's other
ended turns no ended turn was shown (the newest sixteen, the rest counted), with
the siblings that ended beside the head with a session of their own. No launch of
the turn reads it again. A read that fails, or that the read ladder refuses, fails
open: the turn runs with no endings and continues nothing, logged as
`executor_conversation_unread`.

**Bounded by whole endings.** The block tells the head, then the others newest
first while they fit **24 000 bytes** (about six thousand tokens), and counts the
rest, never cutting inside a turn's ending; a head whose accounts alone would not
fit is told without them, saying so. Bytes are counted as the run record carries
the block, escapes included.

**Frozen at the first launch.** The first launch writes the read onto the run's
record (where it was read, the turns its brief told, the head as the turn's base),
and every relaunch builds from it, in any box on any node. The rendered block
rides the run's parked state, so every later fallback is told the same endings.

**Held on the log.** The first launch records its read on the executor log as the
turn's `begin`; from then until the turn's first landed capture or its end, **no
release takes the base**: not the seat's session budget, not a sibling turn's end,
not the retention. A turn whose run is gone with no end recorded is ended `lost` a
day after its last record, which lifts its hold; a first launch abandoned once its
record was written ends its turn at once.

**The head is the turn that read the conversation last.** Every first launch
continues the head its read named, so a conversation's sessions are a line. Two
turns of a conversation can run beside each other (a turn parked on a person's
question frees its seat, and a message not taken as its answer runs beside it),
so the head is never simply the turn that ended last: it is the ended turn with a
kept session whose read was the latest, the last to end among turns that read at
one position, and a turn whose read failed comes after every turn that read. A
turn that read early and ended late is told and offered beside the head, never
continued, and the next turn's end absorbs it. A schedule's fires run beside a
parked fire every time ([Schedules](#schedules)). The seat's session budget
releases the oldest end first ([`max_bytes_per_seat`](#configuration)), so it can
release a head before a stale sibling; the next turn then continues the kept
session that read the conversation last, and is told the released head's ending
like every other.
A turn whose run read a wake of another conversation serves that one too, which
reads its session as a head placed where the turn ended ([Code
Sandbox](code-sandbox.md#a-conversations-next-turn-continues-its-last-turns-session)).

**A turn's end absorbs what its brief told.** Once a turn that kept a session of
its own ends, the turns its brief told (the head it was handed, whether or not it
could continue it, and the endings it was told) are gone from the log, their
sessions released: the new session holds the head's transcript and its own. A turn
the bound left out is not absorbed, and the next turn is told it. Siblings are
told and absorbed, never continued or held.

## When a run does not continue its session

A run that does not continue the newest session its turn or conversation left
says so, and why. The verdict names a **loss** (below), what the run did
**instead** (`fresh`, a session of its own; `older`, an older session that ends
before the lost work: the conversation's older head, the head's own older capture
where its end names its newest not kept, or an older capture of the run's own
turn), the turn whose session was lost where it is another turn's, and the
engine's detail. It is said in one sentence everywhere, for example "The run did
not continue turn 4f1a...'s session: its session was larger than the 48 MiB kept
for one session. It continued turn 9d8e...'s older session instead, which ends
before that work."

| Loss | Fault | What happened |
|---|---|---|
| `too_large` | yes | the session was past the 48 MiB kept for one, taken or sealed |
| `keep_failed` | yes | its take, pack, scrub or stream failed, a taker lease or the company's policy could not be read, the run's bridged log could not be read, or a minted session was not bound |
| `no_spool_room` | yes | the node's spools held their whole room for the take's minute ([code sandbox](code-sandbox.md#a-sessions-spools-on-its-node)) |
| `store_refused` | yes | the object store refused the upload, or took none of it however often it was asked within the keep's budget; its reservation or the seat's session budget spent |
| `log_refused` | yes | the executor log refused the capture's record or never answered it, its row names another, or the conversation could not be read |
| `capture_late` | yes | a newer capture was still being kept past what a capture of its size takes at the floors (five and a half minutes for half a mebibyte, thirty-one at the bound), which a relaunch waits out and no longer ([code sandbox](code-sandbox.md#a-relaunch-continues-the-turns-session)) |
| `release_floor` | yes | the session was written by a harness release or archive format no box of this build continues; its capture is refused unpacked ([code sandbox](code-sandbox.md#each-launchs-session-kept)) |
| `box_refused` | yes | the box's harness said it does not continue the session, or could not be asked |
| `restore_failed` | yes | the session's object could not be fetched or read, or the box's restore failed and its script started a session of its own |
| `box_lost` | yes | the job was lost with its box before any capture of it |
| `context_exhausted` | no | the session ran out of its model's context, and is [retired](#a-session-that-ran-out-of-context-is-retired) |
| `request_refused` | no | the model's provider refused the request the session makes, and it is [retired](#a-session-its-provider-refuses-is-retired-too) |
| `harness_changed` | no | the member the launch picked runs another harness: a session is continued only by the harness that wrote it |
| `budget_floor` | no | the seat's capped windows cannot carry what the session sends again (where one could but is full now, the seat's mail waits instead, [Cost](#cost)) |
| `entry_changed` | no | the launch runs under another `providers.llm` entry (key, auth mode or route) than the session ran under, on a harness not measured to resume on its launch's route ([Code sandbox](code-sandbox.md#a-relaunch-continues-the-turns-session)) |

A build that meets a loss it does not name (a newer build's) says it "for a reason
this build does not name", and counts it a fault. No verdict is said where nothing
was lost: a conversation's first turn, every earlier turn's session released,
absorbed or past the retention, a turn that serves no conversation, the switch
off, a seat whose chain keeps no session, or a launch that continues the copy its
own box holds, which went on past every capture of it.

**Where it is said:**

- every capture refused is stamped with its loss on the run's record and logged
  `executor_session_not_kept` with `loss`; the turn's end names it on the executor
  log (`unkept.loss`), and each session a launch spent with how
  (`spent_sessions`), so the next turn knows why;
- a launch logs `executor_session_not_continued` with `loss`, `instead` and
  `said`, the sentence; its `executor_launched` event carries the verdict as the
  launch decided it, and its `executor_collected` event the final one
  (`session_not_continued`), a restore its box fell back from included;
- the turn's end on the executor log keeps its first verdict and its newest
  (`not_continued`), and a seat's [Sessions](#reading-it) say each in the
  engine's sentence (`not_continued_note`) beside the turn;
- a turn's page says each of its launches' verdicts above its tabs, a running
  launch's as it decided at its start and a collected one's final; a launch's
  card on the turn says it in its header and its body, and an executor's page on
  the running launch and on each collected one;
- the turn's conversation is told the turn's first verdict, once per turn and in
  its sentence alone, never the detail: a colleague who asked over
  [A2A](event-system.md#ephemeral-a2a-channels-internala2a) beside the engine's
  answer, in a field of its own read after the colleague's words and outside
  their fence; a conversation on the engine's own [chat](chatlet.md), once the
  turn's end is on the executor log, in a line of the engine's own, in the
  thread the turn answers or its direct conversation, which wakes nobody. A turn
  whose run's record is gone by its end (finished by the seat's next holder, or
  a launch its fence refused) tells its conversation nothing, since the record
  held what it lost; each launch's own record still says it on the turn's page.
  A reply the seat posts on another surface (Slack, Mattermost, an issue) is the
  seat's own words, to which the engine adds nothing; the turn's page and its
  Sessions say it there;
- `crewlet.executor.session.not_continued{reason, instead, summary}` counts each
  collected launch's loss once, with what the brief its run read handed of the
  lost session's summary ([Metrics](../reference/metrics.md)), and every
  collected launch that had a session to continue is an event of the
  `session_carry` [objective](../reference/service-objectives.md), bad for a
  fault alone.

### What the run is handed

The brief tells the run the session it does not continue, in one section placed
before the round's calls ([What a harness is told](code-sandbox.md#what-a-harness-is-told)):

- **the verdict's sentence**, as every surface words it, never the detail;
- **that session's newest summary**, which its harness wrote as it compacted and a
  box read kept apart from the session
  ([kept apart](code-sandbox.md#a-sessions-newest-summary-kept-apart)), fenced as
  the seat's own past session: the session's own account to carry the work on
  from, checked against the repository and the calls told after it. A summary
  past the brief's room is handed by its end, marked as cut; one that could not
  be read is said to be kept and unread. A run that continues an older session in
  the lost one's place is handed the newer one's summary only where it is newer
  than what it continues;
- **where `read_session` reads the kept session** the probe read, where the seat
  is offered that tool ([A seat reads its own](code-sandbox.md#a-seat-reads-its-own)).
  A relaunch's own turn's is kept only until the run first saves its own, which
  takes its place on the turn's row; a relaunch of a turn that has kept nothing
  names the head its first launch froze only where the turn's row was read
  holding it.

A run starting a session of its own is also told what each of its round's
successful writes returned (the key of what it created, a message's id), up to a
line each ([Prior-work ledger](turn-engine.md#prior-work-ledger-across-self_iterate-rounds)).
What its box had not committed and pushed went with that box, and a new box's
checkout is at its branch's head, so what was pushed is already there. Cursor
Agent's sessions are handed no summary: no summary read of its chat store is
measured ([Cursor Agent](code-sandbox.md#cursor-agent)).

---

## A session that ran out of context is retired

A run whose conversation no longer fits its model's context fails with
`error_kind` `context_exhausted`; another launch of the same session fails the
same way, so the engine continues it no further:

- **Within the turn**, the collection records the session on the run's record, so
  a relaunch continues neither its kept capture, nor the base it froze, nor its
  box's copy. It starts a session of its own on the fresh brief, kept and
  continued as any other.
- **After the turn**, its end names the session on the executor log with how it
  was spent (`spent_sessions`), which takes it off every ended row of the
  conversation that names it, on every data node, keeping that loss on each. The
  next turn's read does not hand it out, a live turn's hold on it does not keep
  it, and the next turn says that loss, never one guessed from the end's unkept
  capture.

The turns' endings carry the conversation on, as the brief told them. A turn whose
own row loses its session as it ends absorbs nothing, so the next turn is told how
that turn ended and how the turns it read ended too; a later turn absorbs them once
it has kept a session of its own. The brief says how the session was spent.

**The job's final session is not kept at all.** A launch's record says why
(`session_not_kept`), and nothing is uploaded: no launch would continue it, and its
bytes could release another conversation's session against the seat's budget. A
session retired after an earlier capture landed goes the same way, and how its turn
ended stays.

**An operator sees it.** The launch that starts fresh says so as
`context_exhausted`, naming the session and the turn it ran out in
([When a run does not continue its session](#when-a-run-does-not-continue-its-session)),
and a seat's [Sessions](#reading-it) say so of each row. Another session of the
conversation that has not run out stays, and is continued as the head where it is
one. A conversation that keeps running out on fresh sessions is reported as
[`executor.context_exhausted_loop`](../guides/engine-issue-reports.md).

## A session its provider refuses is retired too

A continued session sends what it holds on every request, so one holding something
the provider will not take (a tool definition under a name outside the provider's
pattern, a message it no longer accepts) is refused on every launch. The engine
reads the provider's status, never the sentence:

- **The launch failed, with a kind of its own.** Its record says
  `error_kind` `request_refused` and carries the provider's message as `error`.
  It is never the run's report, so no reviewer judges it and the stall guard does
  not compare it with the round before.
- **The session is retired**, as one out of context is: nothing kept
  (`session_not_kept` says why), no relaunch continues it, and the turn's end takes
  it off every ended row of the conversation, so the next turn (a schedule's next
  fire included) starts a session of its own.
- **The round is carried on once, in a fresh session**, where the refused job had
  continued one: the same round, relaunched on the fresh brief, told the session
  before it was retired. The launch logs `run_relaunched_fresh_session`.
- **A refusal a fresh session does not cure ends the turn.** A job refused in a
  session it started, or refused again after that one relaunch, ends its turn
  `failed` with a `turn.guard_breach` of kind `request_refused`, whose `detail` is
  the provider's message.
- **Twice in a row on a fresh session holds the conversation.** It stands as a
  platform notice (`conversation.held`, cause `request_refused`) until a person
  writes on it or the seat's models or the company's tool servers change, so a
  schedule whose every fire is refused stops firing and an administrator is told
  ([Advisor](../guides/advisor.md)).

Statuses read as a refusal: 400, 413 and 422. A spent plan (429), an outage and a
refused sign-in are handled as before; a conversation past its model's context
keeps its own kind.

## Where a session lives

**On the executor log, the same on every data node.** A turn's row (conversation,
how it ended, the session it kept) is a row of the `executor` state log, a
replicated domain every data node derives identically from one ordered stream
([Where state lives](architecture.md#5-where-state-lives)); a node without `data`
reads it through the estate router. The session's bytes are one sealed object in
the fleet's [object store](object-store.md), and each large payload it keeps
apart (an image, a whole PDF it read) one more, stored once however many of the
seat's sessions hold it and spliced back into the session, byte for byte,
wherever it is read or restored. A payload gone from the store leaves the session
unreadable, so the next launch starts fresh (`restore_failed`). So a seat that
moves between nodes finds its conversations' sessions wherever it lands.

**Keyed by the seat and the conversation.** A row is the seat's, under its handle,
and its conversation's: `conversation_key` is the **conversation identity** the
trigger was stamped with, the `{source}:{local}` grammar of
[the event system](event-system.md#inbox-batching--coalescing): `jira:POC-7`,
`slack:C9:1718.001`, `github:acme/api#42`, `work:task:<id>`, or a schedule's own,
`schedule/unit/backend/daily-standup`. It is never the partition key the seat's
inbox coalesces on. The two are the same string for every source but chat: **a
direct message is one conversation however it is threaded**, so a DM's turns key
on the bare channel (`slack:D1`) while a reply inside it still partitions on its
thread, and the reply's turn continues the burst's session.

Two seats serving one conversation (a lead and its report on one ticket) each
continue their own sessions, never each other's.

## Reading it

A seat's sessions are its **Sessions**, under the Memory section of its page's
Activity tab ([A seat's profile](../reference/dashboard-design.md#a-seats-profile)),
and [`GET /agents/{id}/sessions`](../reference/api-endpoints.md#get-agentsidsessions):
one row per turn, from its first launch where the turn read its conversation and
from its first capture otherwise, latest begun first, with how it ended, what it
was asked, whether its session is kept and how large (its payloads kept apart
counted apart) and, where not, why, and **which launch
would continue it** (`continued_by`: `relaunch` for a live turn's, `next_turn` for
an ended turn's, `next_fire` for a schedule's ended fire's), whether it would and
where. A row that is not its conversation's head names the turn whose session the
next turn is handed instead, and `base_turn` names the session a turn's first
launch was handed. A turn's transcript is `GET /agents/{id}/sessions/{turn_id}`.
Rows go as the turns that were handed them end, with the retention, and with their
seat ([Reading a seat's sessions](code-sandbox.md#reading-a-seats-sessions)).

**A session not kept still has its harness's own account.** Every read of a run's
box keeps the newest compaction summary its harness wrote of the session, apart
from it ([A session's newest summary, kept apart](code-sandbox.md#a-sessions-newest-summary-kept-apart)),
so a row whose session is not kept — too large, refused, retired as spent — reads
as that summary (`summary_kept`), on the screen and through `read_session`.

**A session kept trimmed says so.** A session packed past the bound a kept session
is held to keeps only what its harness's resume loads, from its newest compaction
on ([Each launch's session, kept](code-sandbox.md#each-launchs-session-kept)). Its
row says, in the engine's one sentence (`trimmed_note`), that the history before
that compaction was trimmed and its transcript begins there; a seat's
`read_session` and `list_sessions` say the same. The session is kept and continues
as the whole one would: nothing is lost to the next turn, only to a reader of its
earlier history.

## Schedules

A schedule is a **conversation of each seat that runs it**. The scheduler stamps
every fire with the schedule's key, `schedule/<scope>/<scope id>/<name>` (each part
escaped), so one seat's fires of one schedule are one conversation: the next
fire's first launch continues the session the last fire left, its brief tells how
earlier fires ended, and its end absorbs the fires its brief told. A unit schedule
each member runs is one conversation on each member's seat, and fires that waited
in a seat's inbox together run once, on that one key.

- **What keeps the line, and what starts it afresh.** An edit to `task`, `cron`,
  `timezone` or `timeout_seconds` keeps the conversation. A schedule renamed, moved
  between a role and a unit, or on a role of a company whose `name` changed has a
  new key and starts fresh; a seat that starts running a schedule (a `lead`
  schedule's new lead) starts it on its own seat.
- **Kept for the retention.** A schedule whose fires are further apart than
  `retention_days` never continues.
- **Paid for on every request.** A continued fire sends the schedule's session with
  every request of its run, up to about its model's compaction point with Claude
  Code. There is no per-schedule switch: `enabled: false` starts every fire fresh,
  as it does every conversation's next turn.
- **Nobody replies into it.** Nothing a person writes carries the key, so a fire is
  never taken as the answer to the question the last fire's run parked on; such a
  run is answered by its turn.
- **A fire parked on a question holds nothing up.** The next fires run beside it,
  each continuing the one before; when the parked fire ends after them, the next
  fire still continues the last of them (the head is the turn that read the
  conversation last), while the seat's session budget keeps its session, and is
  told how the parked one ended
  ([Read once, frozen, held](#read-once-frozen-held)).

[Scheduling](scheduling.md#each-fire-continues-the-last-fires-session) has the
whole of it.

---

## Cost

**A continued turn sends its session's context first**, and every request of its
run carries it, mostly read from the prompt cache, every token counted against the
seat's windows. The harness's compaction bounds it, each at its model's window less
the harness's headroom, so a request is never much larger than one turn's
([What a continued launch costs](code-sandbox.md#what-a-continued-launch-costs)).
A schedule pays it on every fire: ten Claude Code requests on a session at the
window are charged about a million input tokens, mostly cache reads
([Schedules](#schedules)).

**Its first launch is held to that context.** The launch needs the larger of
`turn_engine.executor_min_budget_tokens` with the turn's fresh brief beside it
(where its box cannot take the base up and starts a session of its own, held at the floor alone, or at the room below it where that is past the most room
a window can leave the launch,
[What a continued launch costs](code-sandbox.md#what-a-continued-launch-costs)),
and the base's context with the lead and the brief beside it. A base no capped
window can ever carry starts fresh; a first launch refused only because a window is
full right now **parks the seat's mail on that figure** until a window has room, as
a held resume does
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
                                  #   may hold, and again its saves'
                                  #   allowance (49 MiB..64 GiB)
    save_interval_seconds: 600    # shortest time between saves of a running
                                  #   executor job's session (300..3600;
                                  #   0 takes 600), paced by size beyond it
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
counts beside the seat's live turns for as long. A payload a session keeps apart
counts once however many of the seat's sessions hold it, and is freed only with
the last of them. The default, 64 MiB, holds a
live turn's session at its 48 MiB bound beside about ten ordinary ones (a
day's work compresses to one or two mebibytes); the least it accepts, 49 MiB,
is one session at that bound once sealed. It is also the **allowance of the
seat's saves** of its running sessions: a save another replaces is kept by the
object store until its collector deletes it, a day and more, so a seat holds up
to twice its budget. Every seat's sessions share the object store with the
company's files, and on the default `nats` bucket, about 2.6 GiB at the
smallest data volume crewlet-operator admits, five seats at both halves take a
quarter of it, so a company of twenty seats or more belongs on S3
(`store.objects.backend`): a full bucket refuses every upload, the company's
files included.

**`save_interval_seconds`** is the shortest time between saves of a running
executor job's session, 600 s by default, 300 to 3600 taken and anything
else refused, never clamped (0 takes the default). It bounds what a run lost
with its box does again: about the interval and one save's take. A seat's saves
are spaced by size beyond it, so its superseded saves stay within its
allowance: at the defaults a session past about 0.4 MiB sealed is saved about
every 27 minutes per sealed MiB, about every 54 once the seat has saved for a
day, so a session growing 1.2 to 1.8 MiB sealed an hour is saved every 0.54 to 0.81
times its age, 1.1 to 1.6 in a busy seat
([Paced by size](code-sandbox.md#a-running-jobs-session-saved-as-it-works)).
Each node reads it off the revision it runs, since a cadence binds only the
node driving the box.

---

## Related

- [Code Sandbox](code-sandbox.md) — where a session is kept, continued and read
- [Turn Engine](turn-engine.md) — the phases, the within-turn prior-work ledger, and how a turn ends on the executor log
- [Agent Learning](agent-learning.md) — episodes, the diary, counterparty profiles
- [Event System](event-system.md) — the two keys, and inbox coalescing
- [Scheduling](scheduling.md#each-fire-continues-the-last-fires-session) — a schedule as a conversation of each seat that runs it
- [Architecture](architecture.md#5-where-state-lives) — where state lives, and the replicated domains the executor log is one of
