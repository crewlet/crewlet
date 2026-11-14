# Budgets and Spend

Two different numbers describe what a company's models have consumed, and most
confusion about spend comes from reading one as the other. This guide says what
each one is, where it comes from, how far back it reaches, and how Settings ›
Spend draws it.

| | The **budget counter** | The **spend rollup** |
|---|---|---|
| What it is | The fleet's shared counter the budget gate charges with every model round on the engine host the moment its reply arrives, and with an executor run's whole spend when the run is collected | A breakdown of what the calls consumed, by phase, model, provider entry, auxiliary purpose and seat (or person) |
| Its window | One calendar window — the day, the ISO week or the month — on the company's clock | The live 24 hours, or any run of 1 to 90 company days |
| Where it lives | The coordination store (`budgets`), one record per scope | The live projection for the last 24 hours; the replicated `usage` domain for every named window |
| What it is for | Stopping a turn whose round did not fit a ceiling, and every round and launch after it | Understanding where the tokens went |
| Read by | `GET /budgets`, the `budget` push, the meters on the Spend, Budgets and seat screens | `GET /tokens/breakdown`, `GET /tokens/series`, the figures and chart on Settings › Token spend |

They are never comparable. The counter is the one figure a ceiling can be divided
into; the rollup is the one that can say *why*. Both count the same calls —
every round of a turn and every [auxiliary call](#auxiliary-spend) — and where
they part, [the reasons are listed](#why-the-counter-and-the-rollup-still-differ).

## Budget windows

A `token_budget` is a set of ceilings per calendar window — `day`, `week` and
`month`, each optional — on the company, on an agent seat, or on both:

```yaml
timezone: Europe/Berlin
token_budget: {day: 3000000, month: 40000000}
```

Every window is cut on the company's one [clock](../getting-started/configuration.md#the-companys-clock):
a day runs from local midnight to local midnight, a week is the ISO week from
Monday, a month runs from the 1st. Every model round on the engine host — the
reviewer's, a worker's, the onboarding pass's, a side pass's — is charged in all
three windows it falls in at once, against the company and against its seat, as
soon as its reply arrives, and what it asked for runs only while every capped
window of both had room for it. A seat's executor is its harness's own run in
a box, whose requests the engine never sees one by one, so its whole spend is
charged in those windows once, when the run is collected: a **post-charge**,
which never refuses and can take a window past its ceiling
(`sandbox_spend_over_budget`), after which the seat's next round and next
delivery are refused against it until the window turns over. What admits a run
is the floor at its launch.

**A ceiling has room for one executor run, or it is refused.** Every executor
launch needs `turn_engine.executor_min_budget_tokens` (default 25 000: the fixed
part of one run's opening request) with the brief the run is handed beside it
left in every capped window of its seat and of the company, the brief counted
at a token for every four bytes, and a seat short of it is parked until the
window turns over. That holds for a launch in the middle of a turn too. A
launch whose fresh brief takes that past the most room the seat's or the
company's capped windows can leave it is held instead (`executor_floor_held`),
at the floor alone where the floor fits in that room. The room is each window's
ceiling less what the launch's own turn spent there before it, the context it
gathered and the rewrites it made, since every turn spends that before its
launch reads the window. Held at the floor alone, the launch goes out once a
window has the floor left, whatever other seats spent there first, so a busy
company never keeps it waiting for a window nobody else has touched, and its
opening request may take that window past its ceiling
(`sandbox_spend_over_budget` reports what the run took). Only where the turn's own spend
left less than the floor is the launch held at the room, and then it waits for
a window that holds nothing but its own turn's spend. And **a launch refused
below the floor with its brief, or with a session's context, waits one turnover
at most**: in a busy company the other seats spend in the window first after
every turnover, so a figure within that room can be past what every window
leaves the launch when it reads it. In the window after the one it was refused
in, the launch is held at the floor alone, as above, and continues a session
only where that window leaves room for it. A reviewer sending the work back for another run
on a window that cannot pay for it **holds** the job the turn just collected: it waits collected in its paused box, with its seat held
behind it, and the turn resumes once the window turns over or a revision raises
the ceiling (`sandbox_resume_held` in the log), rather than reviewing it again
on every retry into the same refusal. A run that finishes while its window is
full is held the same way once it is collected, its tokens charged in the
window it ran in, since its review is refused too. A person's answer carrying a
parked run on is not let in while the window is full or too short for a launch,
and one whose relaunch is refused all the same is turned back to wait, uncharged,
in its own series of attempts (`sandbox_resume_refused`), until the window turns
over or a revision raises the ceiling (see [Mid-run clarification](../concepts/code-sandbox.md#mid-run-clarification-ask_and_wait)). A
ceiling below that floor never has it left, so on a seat that runs an executor,
or on a company that has one, it is refused when the document is validated,
at the ceiling: raise it to the floor, or lower the floor (`0` turns it off for
every launch whose session's context is not known).

**A launch that continues a session needs room for that session.** Within a
turn, a send-back, a person's answer, a lost job or a replaced sign-in
[continues the turn's harness session](../concepts/code-sandbox.md#a-relaunch-continues-the-turns-session)
where it can; a conversation's next turn
[continues the session its last turn left](../concepts/code-sandbox.md#a-conversations-next-turn-continues-its-last-turns-session),
and a schedule's next fire
[the session its last fire left](../concepts/scheduling.md#each-fire-continues-the-last-fires-session);
and a continued run sends the session's whole context again before it reads a
word of its brief. So its launch needs the larger of the floor and that context
with the brief that continues it: the whole input of the session's last
request, cache included, recorded when the session was captured, and the
estimate of the brief it is handed — a 60 000-token session handed a 2 000-token
continuation needs 62 000 left in every capped window. It never needs less than
the floor with its fresh brief beside it, held as above where that is past the
room, since its box starts a session of its own on that brief wherever it
cannot take the session up. That holds
for a run continued in its own box as much as for one restored into another,
and for the session a conversation's last turn left, whose context is what that
turn's capture recorded; a session no capture measured (keeping switched off)
has no known context, and its launch needs the floor with the brief that
continues it beside it. A relaunch below its figure is held, or for an answer
turned back, exactly as above, for one turnover at most. A turn's
first launch below its figure **parks the seat's mail on that figure**, never on
the floor alone: a window with room for a fresh run and not for the session
would read as having room, and hand the delivery straight back into the same
refusal. A colleague's ask waiting behind such a park is still owed its answer.
Either way the reason names what the figure is made of. A session whose figure
is past the most room the seat's or the company's capped windows can leave its
launch, each ceiling less what the launch's own turn spent there first, which no
window turning over ever leaves, is not continued: the launch
starts a session of its own, on the floor with its fresh brief beside it, held
as above where that is past the room. A seat
with no capped window continues a session of any size.

**And it pays for its context on every request.** Every request a harness makes
sends its whole conversation, and every one is charged its whole input, the
cache's share included ([Cache share](#cache-share)): a run that continues a
60 000-token session and makes 20 requests is charged about 1.2 million input
tokens for that context alone, most of them cache reads — a conversation's next
turn as much as a relaunch, so every turn of a long conversation starts from
what its last turn left — where the same run started fresh begins at its
harness's own prompt, its standing instructions and its brief and grows from
there. A session's context is bounded by its harness's compaction for that
reason. Every Claude Code run is pinned to the context window of the model it
works under (`CLAUDE_CODE_AUTO_COMPACT_WINDOW`, with the variables that would
switch compaction off or move the point it compacts at unset, and from 2.1.310
the same window and those two overrides held over a repository's own settings)
and compacts at
that window less its own headroom of about 33 000 tokens, so a continued run of
*n* requests on a 1 000 000-token model is charged up to about *n* × 967 000
input tokens before its output, and
on a 200 000-token model up to about *n* × 167 000. A smaller fixed window
would charge less per request and starve the run: its first request already
carries the harness's prompt, its tools and the brief, and a window too small
for those and the files a run reads compacts over and over until the harness
stops it ([What a continued launch costs](../concepts/code-sandbox.md#what-a-continued-launch-costs)).
OpenCode compacts near its model's own window
— once the conversation reaches the window less 15% of it, at least 16 000
tokens, on 2.2, and less 10% on 2.0 — which the engine does not move, since
OpenCode's setting is a share or a count of a window the engine does not know:
a continued OpenCode run of *n* requests on a 200 000-token model is charged up
to about *n* × 170 000. So is a Claude Code run whose repository's own settings
switch its compaction off, up to its model's window. A session is continued
wherever the seat's budget can carry it, by the floor above, and on a member
whose model's window can carry it.

**A schedule pays for its session on every fire.** A schedule is a
conversation of each seat that runs it, so each of its fires continues the
session its last fire left on that seat and pays for it on every request: up
to about its model's compaction point a request with Claude Code (967 000 input
tokens on a 1 000 000-token model), where a fire started fresh begins at its
prompt and its brief. An hourly check whose session has reached that point and
whose run makes ten requests is charged about 9.7 million input tokens a fire,
some 232 million a day on its seat and the company, most of them cache reads. Its first launch needs that session's
context left in every capped window and, short of it, parks the seat's mail
until a window has room, as any continued first launch does. There is no
per-schedule switch: the seat's and the company's ceilings bound it, and
`turn_engine.conversation_session.enabled: false` starts every fire fresh, as
it does every conversation's next turn
([Scheduling](../concepts/scheduling.md#each-fire-continues-the-last-fires-session)).

A round's size is known only from its reply, so by the time it is judged the
vendor has billed it, and **a refused round is counted like any other**. The
window that refused it therefore reads **over its ceiling** by that round — a
day at 98 000 of 100 000 whose next 4 000-token round is refused reads 102 000
of 100 000 on `GET /budgets`, the meters on the Spend, Budgets and seat screens and
`crewlet budgets show`, and that is the figure the budget park judges — and
every later round is refused against it until the window turns over or the
ceiling is raised. The figure is what the company was billed; a counter that
left the refused round out read lower than the invoice and let the next, smaller
round in on room that was already spent.

Because every later round is certain to be refused, **a refusal also stops every
later model call of the turn before it is sent**: the next phase, the round-cap
judge, the seat's other workers and an executor launch are refused without
being made, rather than each billed and then refused — and a call stopped that
way is the gate's refusal like any other, recorded as the window's `refused_at`
(the "last refused" on Company › Budgets and in `crewlet budgets show`) the
first time a turn holds one there. The refused round itself
is the one a turn pays for past its ceiling — and a turn running alongside it,
on the same company, can still pay for one round of its own before the counter
tells it. A turn's own [auxiliary calls](#auxiliary-spend) are charged through
the same meter as its rounds: a context assembly or a rewrite that takes a
window to its ceiling has its round refused before it is sent too, and the
turn makes no further auxiliary call while that window is full — its rewrite
would only feed a round that is never sent. The one exception is the task
card's rewrite, made after the turn's last round as its record, so a turn the
budget ended still has a readable card. "Certain" is on the turn's own clock: a turn that began before the
company's `timezone` moved east can stop on a day the other nodes have already
turned over. Its message then runs again, because the park finds nothing
refusing — unless the turn had already written outside the engine, in which
case it is recorded, as after any refusal.

The same holds wherever the engine turns work away on a window that is already
full before making a call: a seat's delivery parked until the window turns
over, a person's `answer_knowledge` question refused, a reflection pass
declined. Each is the gate's refusal, recorded as
the window's `refused_at` on the scope a charge would have been refused by —
the company's before the seat's — so a window that an executor's launch, a background
pass or a person's answers filled says when it last turned work away rather
than reading as one that has refused nothing.

A third ceiling holds each **person's** Ask Crewlet questions:
`assistant.token_budget`, the same three windows, charged per person on top of
the company's (see [Ask Crewlet's budgets](assistant.md#budgets-and-limits)).

Every ceiling is part of the company document. The **company's**
`token_budget`, a **seat's** and **each person's Ask Crewlet ceiling** are written through
[`/config`](configure-via-api.md) (`admin`) — a seat's on its own with
`PUT /config/roles/{handle}` — and every node enforces them once it has applied
that epoch. The write is a revision recorded with a summary such as "Raise the
company's daily token ceiling from 2M to 5M". A seat ceiling the company's own
ceiling makes unable to refuse a turn — a seat's day at or above the company's
day, say — is reported as a warning at the seat's ceiling by `crewlet validate` and on
every `/config` write's answer. A refusal one of them caused names it (its
`remedy`), so the dashboard offers whoever may raise it the way to that
ceiling on Company › Budgets, and tells anybody else who can. See
[Dashboard design § Company › Budgets](../reference/dashboard-design.md#company--budgets-raised-in-place).

## The spend rollup, and why it has two sources

**The live 24 hours** are held by each node's live projection: the phase records
and auxiliary records of the last day, folded on every node and pushed over the
dashboard's socket every few seconds (the
[`tokens` push](../reference/api-endpoints.md#pushes)). It is instant and it can
list recent turns one by one, for a client that wants the last day as it
happens; no screen of the bundled dashboard draws it.

**Every named window** — 7, 30 or 90 days, or two dates you name — is read from
the replicated [`usage` domain](replication.md#two-compacted-domains-the-embeddings-and-each-data-nodes-day).
Every data node derives its own day from its own event log — its own seats',
and those of the nodes without `data` whose events it keeps through
[custody](deployment.md#custody-the-rows-of-a-node-without-data) — and
publishes it whole; every data node applies every data node's days. So a named
window is:

- **the company's**, whichever node answers — a three-node fleet no longer shows
  a third of its spend on the node the dashboard happened to reach;
- **whole company days**, cut on the company's clock, so "the last 7 days" is the
  same seven days on every screen and every node;
- **there after a node leaves** — a departed node's days stay in the history.

A company day holds no turn and nothing finer than a day, so a named window has
no list of turns and a chart has no hourly column. The costliest turns over a
window are the turn list sorted by tokens (`GET /turns?sort=-tokens`), which
reads the fleet's own event logs and so reaches back as far as those do.

The dashboard's Settings › Token spend reads named windows only, 7, 30 or 90 company
days (it opens on 30), or two dates you name on the company's clock — so every
figure on it is the company's and every card says which window it is of. A
window the engine refuses is shown once, in the engine's own sentence, with no
figures under it — and the two refusals say different things, because they
have different fixes: a window longer than ninety days is told to narrow its
dates wherever it lies, and one starting behind the history's floor is told
the first day it may start on. See
[Dashboard design § Spend](../reference/dashboard-design.md#spend-tokens-by-phase-agent-and-model).

## How far back

| History | Reaches back | Bounded by |
|---|---|---|
| Spend (named windows) | **181 days** | The `usage` domain's history (`spend_history_seconds` on `GET /health`) |
| Turns, events, traces | 30 days | Each data node's event log (`event_history_seconds`) |

181 is not a round number: the longest window offered is 90 days, comparing it
with the 90 before it reaches back 180, and the day before that is the one a
window cut on a clock that moved can touch. A window whose first day is older
than the floor — or whose previous window's is — is refused, naming what to
change, rather than drawn short: every answer states its `horizon` (`days` and
`floor`, the oldest company day still answerable) so a screen can say where the
history ends.

## Reading the chart: four bands

Split by phase, the chart draws **four bands**, folded once by the engine so no
screen folds a phase differently from the next:

| Band | What spent it |
|---|---|
| **Execute** | The seat's own turn doing its work — the executor, which is its harness's own run in a box. What the run spent is recorded from the box once per launch (`executor_collected`), which names no phase and is filed here: the executor's own record carries no tokens |
| **Review** | The reviewer's pass over that work |
| **Workers** | The short-lived workers an executor delegated tasks to |
| **Auxiliary** | Everything that is not the turn's own work: the seats' [auxiliary model](#auxiliary-spend) — the turn-start context, every compaction rewrite, the reflection workers, the background passes and a person's answered questions — the round-cap judge and the first-turn onboarding |

The other splits — model, provider entry, seat, unit, worker — draw the four
biggest and fold the rest into one "other" band. **Worker** is the auxiliary
model's spend by purpose (`memory_filter`, `condense_thread`, …), split by what
each call was for. That is part of the Auxiliary band rather than the whole of
it: the band also holds the round-cap judge and the first-turn onboarding (and
any phase a newer peer records), which are no purpose, so the Worker split sums
to the band less those. **Provider** answers "which of
our configured entries are we paying for", which **model** cannot: two entries
can run one model — a subscription entry and an `auth.mode: api-key` entry for
the same Claude model, say — and only the entry says which account paid.

## Auxiliary spend

Each seat names a cheap model, `llm_auxiliary` (falling back to its `llm`), for
the questions the engine asks on its behalf rather than the work it does: what
to remember, what to search for, how to fit a long text into a prompt. Every
such call goes through one seam that charges the [budget
counter](#budget-windows) and records the call as an `auxiliary_spend` event,
which every spend figure folds — the live window, the named windows, a turn's
page, the turn list and a task's spend. A call a turn makes is charged through
that turn's own meter, the one its rounds are charged through, so the turn
knows of a window the call filled before its next round is sent.

Each call states its **stage** — whose cost it is — and its **purpose**:

| Stage | What spent it (purposes) | Charged to | In the turn's cost and its task's spend |
|---|---|---|---|
| `turn` | The turn-start memory filter, knowledge query and episode summary (`memory_filter`, `knowledge_query`, `episode_summary`); every rewrite the turn's ledgers, judge, tools and delegated workers needed, and its card (`condense_<kind>`); and what its executor cost between two parts of the turn — the executor's bridged tools and the condensing of a collected launch's report or failure (`condense_report`, `condense_tool_error`) | the seat and the company | **yes** — what its executor cost between two parts of the turn is paid by the part that resumes from it |
| `reflection` | The learning workers after the turn (`persist_decider`, `counterparty_profiler`, `skill_synthesizer`, `skill_refiner`) | the seat and the company | no — drawn beside the turn in its Reflection lane: it is what the seat remembers, not what the work cost |
| `background` | The episode compaction, clustered synthesis and skill promotion passes (`episode_compaction`, `skill_clustering`, `skill_promotion`), and the probe of a model entry at its usage limit when it is tried again, one for every seat parked on it, charged to the parked seat it runs as (`provider_probe`) | the seat and the company (the company alone for a unit a person leads) | no, there is no turn |
| `operator` | A person's question answered on the operator surface (`answer_knowledge`, and `condense_source` for a page too long to read whole) | the company alone: a person has no seat budget | no — no turn |

A compaction's purpose names what it rewrote — `condense_thread`,
`condense_produced`, `condense_report` — so a rewrite of a chat thread and one
of an executor's report are told apart on every breakdown.

**A person is their own row.** What the auxiliary model spends for a person —
their questions, or a pass resolved on the chain of a unit they lead — names no
agent seat, so the per-seat breakdowns draw it as the person's row
(`person: true`), with no turns. It reaches the named windows as the `usage`
domain's person record, which a node publishes with the rest of its day, as
it publishes a seat's.

**Coalesced, a flush behind.** A node's ledger keeps one record per (stage,
seat or person, turn, purpose, model, provider entry, company day) and
publishes it every **15 seconds** — and a turn's own records before the turn's
end is published, and its reflection pass's before that pass's
`reflection_completed`, so a Turn page open while the turn ends asks for it again
at each of those moments and reads what its context, its rewrites and its
learning cost. A compaction of
seventy rewrites is one record of seventy calls. So the live window trails the
counter by up to one flush, and a named window by up to two (the flush, and the
usage publisher's own tick). Every figure's **calls** are provider calls — a
phase's model rounds, an executor launch's own turns as its harness reported them
(one per launch where the harness reports none, as OpenCode does), an auxiliary
record's coalesced calls — never records. The executor's own record counts
none, since its run's calls are its launches'.

**What is not counted: embeddings.** The calls that turn text into vectors are
metered by neither the counter nor the rollup: they run on the embeddings
provider rather than a seat's model chain, and a token budget does not judge
them. A company paying for embeddings sees that bill at its embeddings
provider, and it is made of these calls:

- **The corpus.** The fleet's embedding duty embeds the pages and tasks that
  changed, once for the whole fleet.
- **What a seat stores.** A diary note and an episode are embedded as they are
  written, and the node holding a seat fills every note that has no vector of
  the current model — after a change of `providers.embeddings.model`, the whole
  diary, up to 512 notes a minute per node.
- **What a turn recalls by.** Each turn embeds its ask once at its start, for
  its memory and episode recall, and `refresh_memory` and `query_episodes`
  embed the hint they are given each time a seat calls them.
- **Every knowledge or work search that ranks by meaning** (`semantic`, or
  `hybrid`, the default, over Doclet and Worklet)
  embeds its query: the
  turn-start knowledge prefetch (so a turn start makes this call beside its ask
  vector), `search_knowledge`, `search_work_items`, the retrieval behind
  `answer_knowledge`, the operator tools, and the API's and the dashboard's
  search. A query vector is cached per node (1 024 entries, cleared when the
  model changes), so a phrase searched again soon costs nothing.

### Why the counter and the rollup still differ

They count the same calls, so a gap between them is one of these, each with a
known size:

- **The flush.** An auxiliary call reaches the rollup up to one 15-second flush
  after the counter (two for a named window), and a phase record the moment
  its phase ends.
- **A node that stopped hard.** A process that dies loses the auxiliary
  records it had not published: at most one flush of them, plus — if it dies
  while the broker is refusing them — every refused record it was holding for
  a retry, up to the 4 096 below. The counter holds that spend, the rollups
  are short by it.
- **A broker that refused records.** A node keeps up to 4 096 unpublished
  records for its next flush — most of an hour of a busy company — and logs
  any it has to drop past that, with their tokens.
- **A charge that failed.** A counter the node could not reach is short of
  that call; the record still reaches the rollup. A charge that reached the
  company's counter and then failed on the seat's keeps the call on the
  company and leaves only that seat's counter short of it, logged as
  `coord_kv_budget_spend_uncounted`.

## A seat's changes and reviews

Beside its turns and its spend, a seat's day counts what it did on
[Gitlet](../concepts/gitlet.md): the **changes it opened**, the **reviews
it gave**, and the **findings it received** — every finding a review raised on
a change the seat owns, counted on the owner rather than on the reviewer,
whose day counts the review — and what the merge gate did to its changes: the
**checks that failed** them and their **ejections** from a merge queue, each
counted on the change's owner too. They are kept in the same replicated `usage`
domain as the spend, for the same 181 days, and summed across every node.

| Figure | Counted when | On whose day |
|---|---|---|
| `changes_opened` | A change is opened — by a change tool, the API, or a push's `-o change` options | The change's owner, the seat that opened it |
| `reviews_given` | A review of a change lands | The seat that gave it |
| `findings_received` | A review that raised findings lands | The seat that owns the change it judged, whoever reviewed it — a person included |
| `checks_failed` | A check of a change's head ends failing it — failed, or errored past the home's own requeue (its third attempt, or an error its infrastructure did not cause) — whether the engine ran it or a person's CI reported it, which counts on the live change (open or draft) into its target at the commit reported | The seat that owns the change, whoever pushed the head or reported the check |
| `ejections` | A merge queue puts a change's entry out (a check failed, a conflict, the gate no longer met, the head moved) | The seat that owns the change, whoever queued it |

Each is counted once, by the data node whose record landed it, from the
`change_opened`, `review_submitted`, `code_check_finished` and
`code_queue_ejected` events that node announces (see
[the event system](../concepts/event-system.md)) — never once per data node,
and never again for a retried write the code log collapsed onto the first: a
failed check once by its attempt, whatever corrections of its ending follow,
and an ejection once by its queue entry. A
**person** has no seat day: a change a person opens or a review a person gives
is counted nowhere, though a person's review still counts its findings on the
seat whose change it judged. A change carries no spend of its own: its turns'
tokens are the seat's spend as ever.

The seat activity answer reads them (`GET /agents/activity`, the
`seat_activity` question in the
[API reference](../reference/api-endpoints.md)): `changes_opened`,
`reviews_given`, `findings_received`, `checks_failed` and `ejections` over the
window, and over the window before it when `previous=true`.

A seat's day that counts any of them is usage record version 3, and one that
counts failed checks or ejections version 4, so during a rolling upgrade a
node still on an older build keeps that day aside, unapplied, until it is
upgraded, rather than applying it without them; a day with none of them stays
readable by every build.

## Cache share

Every figure carries the prompt cache's share of its input: `cache_read_tokens`
(served from the cache) and `cache_write_tokens` (stored into it). They are a
**breakdown** of `input_tokens`, never an addition to it — the cache is the
harness's own, and the backend adds the cache counts the harness reports back
into its input total — so the share of input a cache served is
`cache_read_tokens / input_tokens`, and `total_tokens` is input plus output. A
harness that reports no cache counts carries zero for both, and one that reports no
usage at all is charged an estimate, four characters a token, which
`crewlet llm doctor` names on its `token usage` line.

## Tokens, never money

The dashboard measures spend in tokens only. A price is reported only by an
executor's launch — in its harness's own figure — and by no
call on this host, so a currency figure would leave out every reviewer,
worker and auxiliary call while reading as the company's spend.
