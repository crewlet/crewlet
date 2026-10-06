# Budgets and Spend

Two different numbers describe what a company's models have consumed, and most
confusion about spend comes from reading one as the other. This guide says what
each one is, where it comes from, how far back it reaches, and how the Spend
screen draws it.

| | The **budget counter** | The **spend rollup** |
|---|---|---|
| What it is | The fleet's shared counter the budget gate charges with every model round, the moment its reply arrives | A breakdown of what the calls consumed, by phase, model, provider entry, auxiliary purpose and seat (or person) |
| Its window | One calendar window — the day, the ISO week or the month — on the company's clock | The live 24 hours, or any run of 1 to 90 company days |
| Where it lives | The coordination store (`budgets`), one record per scope | The live projection for the last 24 hours; the replicated `usage` domain for every named window |
| What it is for | Stopping a turn whose round did not fit a ceiling, and every round after it | Understanding where the tokens went |
| Read by | `GET /budgets`, the `budget` push, the meters on the Spend and seat screens | `GET /tokens/breakdown`, `GET /tokens/series`, the Spend screen's figures and chart |

They are never comparable. The counter is the one figure a ceiling can be divided
into; the rollup is the one that can say *why*. Both count the same calls —
every round of a turn and every [auxiliary call](#auxiliary-spend) — and where
they part, [the reasons are listed](#why-the-counter-and-the-rollup-still-differ).

## Budget windows

A `token_budget` is a set of ceilings per calendar window — `day`, `week` and
`month`, each optional — on the company, on a role, or on both:

```yaml
timezone: Europe/Berlin
token_budget: {day: 3000000, month: 40000000}
```

Every window is cut on the company's one [clock](../getting-started/configuration.md#the-companys-clock):
a day runs from local midnight to local midnight, a week is the ISO week from
Monday, a month runs from the 1st. Every model round is charged in all three
windows it falls in at once, against the company and against its seat, as soon
as its reply arrives, and what it asked for runs only while every capped window
of both had room for it.

A round's size is known only from its reply, so by the time it is judged the
vendor has billed it, and **a refused round is counted like any other**. The
window that refused it therefore reads **over its ceiling** by that round — a
day at 98 000 of 100 000 whose next 4 000-token round is refused reads 102 000
of 100 000 on `GET /budgets`, the meters on the Spend and seat screens and
`crewlet budgets show`, and that is the figure the budget park judges — and
every later round is refused against it until the window turns over or the
ceiling is raised. The figure is what the company was billed; a counter that
left the refused round out read lower than the invoice and let the next, smaller
round in on room that was already spent.

A window's allowance comes back when the window turns over, rolled inside the
first charge after the boundary — nothing has to run at midnight. There is no
reset: to make room before a window turns over, raise its ceiling. The engine
judges each window's `state` — `ok`, `near` at 90% of the ceiling, or `refusing`
— and every screen draws the engine's word rather than dividing for itself. See
[Deployment § Token budgets](deployment.md#token-budgets) for the park a seat out
of room waits in, and [Coordination § Token budgets are windows](../concepts/coordination.md#token-budgets-are-windows)
for how the counter is kept.

### Raising a ceiling from the dashboard

**Spend › Budgets** (`#/spend/budgets`) draws every scope's day, week and month
with what it has spent and its ceiling, and an operator raises a ceiling there
in place: the pencil beside it takes `50M`, `2.5M` or the digits, and an empty
field removes the ceiling. Every save is checked against the whole company
first, so a ceiling that could never refuse a turn — a seat's at or above the
company's — is shown as a warning before it is stored. Home and the Inbox offer
the same change as **Raise budget** on a seat the engine stopped, opened on the
scope that is actually refusing it. It is an ordinary configuration revision,
recorded with a summary such as "Raise Agent PM's daily token ceiling from 2M
to 5M", and each node enforces it once it has applied that epoch. See
[Dashboard design § Spend › Budgets](../reference/dashboard-design.md#spend--budgets-raised-in-place).

## The spend rollup, and why it has two sources

**The live 24 hours** are held by each node's live projection: the phase records
and auxiliary records of the last day, folded on every node and pushed to the
dashboard every few seconds. It is instant and it can list recent turns one by
one.

**Every named window** — 7, 30 or 90 days, or two dates you name — is read from
the replicated [`usage` domain](replication.md#two-compacted-domains-the-embeddings-and-each-nodes-day).
Every node derives its own seats' day from its own event log and publishes it
whole; every node applies every node's days. So a named window is:

- **the company's**, whichever node answers — a three-node fleet no longer shows
  a third of its spend on the node the dashboard happened to reach;
- **whole company days**, cut on the company's clock, so "the last 7 days" is the
  same seven days on every screen and every node;
- **there after a node leaves** — a departed node's days stay in the history.

A company day holds no turn and nothing finer than a day, so a named window has
no list of turns and a chart has no hourly column. The costliest turns over a
window are the turn list sorted by tokens (`GET /turns?sort=-tokens`), which
reads the fleet's own event logs and so reaches back as far as those do.

The dashboard's Spend screen reads named windows only — 7, 30 or 90 company
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
| Turns, events, traces | 30 days | Each node's event log (`event_history_seconds`) |

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
| **Execute** | The seat's own turn doing its work — the executor, and a detached coding run it launched, which is the same work done in a box |
| **Review** | The reviewer's pass over that work |
| **Workers** | The short-lived workers an executor delegated tasks to |
| **Auxiliary** | Everything that is not the turn's own work: the seats' [auxiliary model](#auxiliary-spend) — the turn-start context, every compaction rewrite, the reflection workers, the background passes and a person's answered questions — the round-cap judge and the first-turn onboarding |

The other splits — model, provider entry, seat, unit, worker — draw the four
biggest and fold the rest into one "other" band. **Worker** is the auxiliary
model's spend by purpose (`memory_filter`, `condense_thread`, …), the same
calls the Auxiliary band holds, split by what each one was for. **Provider** answers "which of
our configured entries are we paying for", which **model** cannot: a fallback
chain serves several models under one entry.

## Auxiliary spend

Each seat names a cheap model, `llm_auxiliary` (falling back to its `llm`), for
the questions the engine asks on its behalf rather than the work it does: what
to remember, what to search for, how to fit a long text into a prompt. Every
such call goes through one seam that charges the [budget
counter](#budget-windows) and records the call as an `auxiliary_spend` event,
which every spend figure folds — the live window, the named windows, a turn's
page, the turn list and a task's spend.

Each call states its **stage** — whose cost it is — and its **purpose**:

| Stage | What spent it (purposes) | Charged to | In the turn's cost and its task's spend |
|---|---|---|---|
| `turn` | The turn-start memory filter, knowledge query and episode summary (`memory_filter`, `knowledge_query`, `episode_summary`); every rewrite the turn's ledgers, judge, tools and delegated workers needed, and its card (`condense_<kind>`) | the seat and the company | **yes** |
| `reflection` | The learning workers after the turn (`persist_decider`, `counterparty_profiler`, `skill_synthesizer`, `skill_refiner`) and the conversation ledger's account of it | the seat and the company | no — drawn beside the turn in its Reflection lane: it is what the seat remembers, not what the work cost |
| `background` | The episode compaction, clustered synthesis and skill promotion passes (`episode_compaction`, `skill_clustering`, `skill_promotion`) | the seat and the company — the company alone for a unit a person leads | no — no turn |
| `operator` | A person's question answered on the operator surface (`answer_knowledge`, and `condense_source` for a page too long to read whole) | the company alone: a person has no seat budget | no — no turn |

A compaction's purpose names what it rewrote — `condense_thread`,
`condense_produced`, `condense_report` — so a rewrite of a chat thread and one
of a coding run's report are told apart on every breakdown.

**A person is their own row.** What the auxiliary model spends for a person —
their questions, or a pass resolved on the chain of a unit they lead — names no
agent seat, so the per-seat breakdowns draw it as the person's row
(`person: true`), with no turns. It reaches the named windows as the `usage`
domain's person record, which a node publishes only once every node applying
the usage log runs a build that reads it; during a rolling upgrade from one
that does not, a person's spend is on the live window and the counter, and
joins the named windows when the last node is upgraded. The node holds those
days in memory until then, so a node restarted mid-upgrade publishes again only
today and yesterday — an older held day stays on the counter and out of the
named windows.

**Coalesced, a flush behind.** A node's ledger keeps one record per (stage,
seat or person, turn, purpose, model, provider entry, company day) and
publishes it every **15 seconds** — and a turn's own records as the turn ends,
so its page reads what its context cost when it reads the turn. A compaction of
seventy rewrites is one record of seventy calls. So the live window trails the
counter by up to one flush, and a named window by up to two (the flush, and the
usage publisher's own tick). Every figure's **calls** are provider calls — a
phase's model rounds, an auxiliary record's coalesced calls — never records.

**What is not counted: embeddings.** The calls that turn text into vectors for
semantic search — the fleet's embedding duty embedding the pages, tasks and
memories that changed, and each turn's one query vector at its start — are
metered by neither the counter nor the rollup: they run on the embeddings
provider rather than a seat's model chain, a token budget does not judge them,
and their usage is a different unit from a completion's. A company paying for
embeddings sees that bill at its embeddings provider.

### Why the counter and the rollup still differ

They count the same calls, so a gap between them is one of these, each with a
known size:

- **The flush.** An auxiliary call reaches the rollup up to one 15-second flush
  after the counter (two for a named window), and a phase record the moment
  its phase ends.
- **A node that stopped hard.** A process that dies loses at most one flush of
  unpublished auxiliary records: the counter holds that spend, the rollups are
  short by it.
- **A broker that refused records.** A node keeps up to 4 096 unpublished
  records for its next flush — most of an hour of a busy company — and logs
  any it has to drop past that, with their tokens.
- **A charge that failed.** A counter the node could not reach is short of
  that call; the record still reaches the rollup.
- **A person's day during a rolling upgrade** — above.
- **Upgrade order.** A node without `data` hands its records to a data node
  (custody), and a data node on an older build drops a type its build does
  not know. **Upgrade the data nodes first**, so the records of a node without
  `data` that is already upgraded are kept.

## Cache share

Every figure carries the prompt cache's share of its input: `cache_read_tokens`
(served from the cache) and `cache_write_tokens` (stored into it). They are a
**breakdown** of `input_tokens`, never an addition to it — every provider counts
the cached prefix in the input already — so the share of input a cache served is
`cache_read_tokens / input_tokens`, and `total_tokens` is input plus output.

## Tokens, never money

The dashboard measures spend in tokens only. A price is reported by one kind of
backend — a subscription coding CLI — and nothing else, so a currency figure
would cover a small minority of calls while reading as the company's spend.
