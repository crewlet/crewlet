# Budgets and Spend

Two different numbers describe what a company's models have consumed, and most
confusion about spend comes from reading one as the other. This guide says what
each one is, where it comes from, how far back it reaches, and how the Spend
screen draws it.

| | The **budget counter** | The **spend rollup** |
|---|---|---|
| What it is | The fleet's shared counter the budget gate charges before every model round | A breakdown of what the calls consumed, by phase, model, provider entry, worker and seat |
| Its window | One calendar window — the day, the ISO week or the month — on the company's clock | The live 24 hours, or any run of 1 to 90 company days |
| Where it lives | The coordination store (`budgets`), one record per scope | The live projection for the last 24 hours; the replicated `usage` domain for every named window |
| What it is for | Refusing a charge that does not fit a ceiling | Understanding where the tokens went |
| Read by | `GET /budgets`, the `budget` push, the meters on the Spend and seat screens | `GET /tokens/breakdown`, `GET /tokens/series`, the Spend screen's figures and chart |

They are never comparable. The counter is the one figure a ceiling can be divided
into; the rollup is the one that can say *why*.

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
windows it falls in at once, against the company and against its seat, and it
runs only while every capped window of both has room.

A window's allowance comes back when the window turns over, rolled inside the
first charge after the boundary — nothing has to run at midnight. There is no
reset: to make room before a window turns over, raise its ceiling. The engine
judges each window's `state` — `ok`, `near` at 90% of the ceiling, or `refusing`
— and every screen draws the engine's word rather than dividing for itself. See
[Deployment § Token budgets](deployment.md#token-budgets) for the park a seat out
of room waits in, and [Coordination § Token budgets are windows](../concepts/coordination.md#token-budgets-are-windows)
for how the counter is kept.

## The spend rollup, and why it has two sources

**The live 24 hours** are held by each node's live projection: the phase records
of the last day, folded on every node and pushed to the dashboard every few
seconds. It is instant and it can list recent turns one by one.

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
| **Auxiliary** | Everything that is not the turn's own work: the learning workers, the round-cap judge and the first-turn onboarding |

The other splits — model, provider entry, seat, unit, worker — draw the four
biggest and fold the rest into one "other" band. **Provider** answers "which of
our configured entries are we paying for", which **model** cannot: a fallback
chain serves several models under one entry.

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
