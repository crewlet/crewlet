# 507 — The spend rollup: one aggregation, two sources

Status: decided, implemented
Relates to: [502 — the dashboard wire protocol is frozen](502-dashboard-wire-protocol.md), [506 — the observability pipeline](506-the-observability-pipeline.md)

## The second thing the gate found

With turns finally reaching the dashboard (506), the end-to-end gate went
looking at what else the socket carried. The `tokens` frame was a list of raw
per-phase records:

```json
{"kind":"tokens","data":[{"event_id":"…","phase":"plan","total_tokens":150}, …]}
```

The client reads an aggregated rollup. `store.js` accepts a snapshot's tokens
only `if (snap.tokens && snap.tokens.totals)` — an array has no `.totals`, so
the snapshot was dropped outright. A push was accepted (`applyTokens` stores
anything truthy) and `views/spend.js` then read `state.tokens.since_days` and
`d.by_phase.length` off an array, getting `undefined` and throwing.

So the whole Spend & Budgets room was blank, with every number it wanted
sitting in memory the entire time.

The cause was simpler than a bug: **the aggregation had never been written**.
`api/tokens.py` — named in the package layout as "aggregation", and described
in `live_state`'s own comment as the thing it defers to — had no Go
counterpart. Both the query surface and the stream shipped the records raw and
called it done, and `livestate.SpendRecords`' doc comment described an
implementation that did not exist:

> The aggregation itself lives with the REST endpoint that already implements
> it, so the live rollup and the queried one cannot disagree.

## Decision

**One aggregation, in a leaf package, fed by two sources.**

`internal/tokens` folds `[]Record` into the breakdown: totals, by phase, by
model, by worker, by agent (with a nested per-phase map), by turn (nested,
newest first, capped), plus the `aggregated_through` watermark the client folds
live events onto.

A **leaf** package — importing nothing from Crewlet — because both ends need
it. The live projection holds the records for its own window; the event store
answers for any other. Either importing the other is a cycle, and a second
record type to bridge them would be the same duplication one directory out. So
`livestate` holds `[]tokens.Record` and `store.PhaseTokens` returns the same
type, and there is one shape end to end.

That is the property worth having: one implementation, and never a second one
in JS. This aggregation had three copies once — the REST endpoint's, a
re-implementation in the browser, and whatever a reconnect left behind — and a
refresh routinely disagreed with the page it replaced.

### Where each source answers

- The **live window**, unfiltered, is the projection's: it is what the
  dashboard opens on, and a page load that scanned the store would put a query
  on the critical path of every tab for an answer already in memory.
- **Any other window**, or one seat alone, is a store scan. Filtering the
  projection here would be a second implementation of the store's own filter.

An empty rollup is labelled with the window **asked for**, not the live one
relabelled: a week's heading over an hour's numbers is a lie about the numbers
beside it. And the store's clamp is reported rather than the request, for the
same reason.

### The fold runs on the tick, not the publish path

`Ingest` marks the rollup dirty; the shared tick folds and pushes it.

Aggregating in `Ingest` would run inside the caller's `Publish` — which on a
merged node is the engine's own goroutine, mid-turn, between a model's answer
and its tools. A busy company would pay one aggregation per phase instead of
one every five seconds, on the hot path of the thing being measured.

The dirty flag is cleared **after** the fold, so a fold that panicked would not
consume the burst it failed on and leave the rollup stale until the next phase
completed.

### Ordering is not left to Go

Rows sort by tokens descending, ties broken on the row's own name; turns sort
newest-first because the table is a tail of recent activity, not a leaderboard
— ordering it by size would pin one expensive turn to the top for as long as it
stayed in the window.

The tie-break matters more than it looks: Go randomises map iteration, so
equal-token rows would order differently on every call, which makes a diff of
two captures unreadable and a golden test impossible. And every list is
initialised non-nil, because a nil slice marshals to `null` and the client does
`d.by_phase.length`.

## The gate

`tests/dashboard/js/replay.mjs` now drives the captured frames through
`store.js` both ways — the connect snapshot and the live push — and checks
`totals`, `since_days` and `by_phase`. The Go side waits for a `tokens` frame
before it stops capturing, because the rollup rides the tick and lands *after*
the turn ends; a capture that stopped at the turn would have passed on the
snapshot alone with the push path unexercised. The e2e node runs its tick at
25ms for that reason.

Verified by reverting to raw records. The replay reports:

```
- the spend rollup has no `totals`: applySnapshot requires it, so a
  reconnecting tab would drop the rollup it was just sent
- the spend rollup has no `since_days`: the view prints it beside the
  numbers and renders a 0-day window without it
- `by_phase` is not an array: views/spend.js reads `.length` off it and
  throws, taking the whole room down
```

Three sentences, each naming the line of client code that fails and what a
reader would have seen. That is what a gate over a frozen protocol is for.
