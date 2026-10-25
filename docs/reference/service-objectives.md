# Service Objectives

What the engine promises about artifacts, as numbers an alarm can hold it to:
publishing, serving a document, a frame becoming ready in a viewer's browser,
and the datastore's verdicts. Each objective is declared once, in
`internal/objectives`, with its reason; every recording site, the
`objective_burn` [alarm](alarms.md) and the
`crewlet.objectives.events` [metric](metrics.md) read the same constants, so a
retune is one reviewed change rather than a drift between a panel and a pager.

## How an objective is stated

Every objective is an **event objective**: over a thirty-day window, a target
fraction of events must be good, and a good event is one answered without
fault on the engine's side within the objective's bound. A latency percentile
is the same shape stated the other way round: "p95 under 250 ms" is "95% of
operations answered within 250 ms". That is what lets a latency objective
carry an error budget and a burn rate exactly like an availability one.

A client's own refusal is an answer, not a fault: a publish refused because
its base version moved, a quota, a policy finding or a malformed request is a
good event; a `5xx`, or an outcome nobody could establish, is a bad one. A
publish refused because the object store has no room for its bytes
(`507 store_full`) is the deployment's refusal rather than the client's, and
a bad event: a full bucket refuses every publish until an operator makes room,
and that is what the budget is there to burn.

## The objectives

| Objective | Measures | Where | Good event | Within | Target |
|---|---|---|---|---|---|
| `publish_success` | Publish success | the publish pipeline (`internal/artifactupload`), every create and publish | published, or refused for the client's own reason (a moved head, a quota, a policy, a bad request; a full object store, `507 store_full`, is a bad event), excluding the upload transfer | 30 s | 99.9% |
| `publish_applied` | Publish latency | the publish pipeline, for a publish that moves no new bytes (a rollback, carries, edits) | the domain's decision to the record applied | 2 s | 95% |
| `view_first_byte` | View time to first byte | the artifact origin (`internal/artifactserve`), every document request | the first byte written, under any status but a 5xx | 300 ms | 99.9% |
| `artifact_ready` | Artifact ready, reported by the viewer's browser | the dashboard, through `POST /artifacts/ready` | the frame's ticket minted to the page's bridge hello | 3 s | 99.5% |
| `data_round_trip` | Datastore op round trip | the datastore writer (`internal/artifactdata`), every write and undo | a verdict, not an unknown outcome | 250 ms | 95% |
| `data_resolved` | Datastore op resolution | the datastore writer, every write and undo | a verdict, not an unknown outcome | 5 s | 99.9% |
| `change_visibility` | Change visibility | the stream push of a committed artifact or datastore change | the applier's commit to the frame written to the socket | 1 s | 99% |
| `wake_latency` | Wake latency | the changefeed, for a watch that fired or a mention | the end of the debounce window to the woken turn's start | 10 s | 95% |
| `presence_join` | Presence join | the presence hub | a join to the first roster | 1 s | best effort, no budget |
| `presence_cursor` | Presence cursor | the presence hub, across nodes | a cursor frame to its delivery on another node | 150 ms | best effort, no budget |

A best-effort indicator has a bound and no budget: it is counted and exported,
and it never burns. Presence is best effort by design, since it is told and
never stored, and degrades silently.

## Where each is measured

| Objective | Recorded by | The event |
|---|---|---|
| `publish_success` | the publish pipeline's `Create` and `Publish`, whichever surface called them (the dashboard, an agent's tool, the CLI) | the request to its answer; the upload transfer is a separate request and is never counted |
| `publish_applied` | the same, for a rollback and a publish whose files are all carried, restored, removed or edited | the domain's decision to its record applied; a refusal is no latency event |
| `view_first_byte` | the artifact origin's handler, for every request it answers as a document (a frame's page, or a page the frame loads as one) | the request to the instant the handler committed to its status |
| `artifact_ready` | `POST /artifacts/ready`, reported by the dashboard for every frame it opens | the frame handed to the dashboard to the page's bridge hello, or to the frame failing |
| `data_round_trip`, `data_resolved` | the datastore writer, for every write and undo | the request to the verdict read back from this node's rows; a write still pending on this node delivered no verdict and is a bad event |

`change_visibility`, `wake_latency`, `presence_join` and `presence_cursor`
are declared with their bounds and targets so that their recording sites (the
stream's push of a committed change, the wake of a fired watch or a mention,
and the presence hub) borrow them rather than invent their own. An objective
nothing records counts no events, exports no series and never burns.

## Burn-rate alarms

An objective alarms when it spends its error budget faster than the month
allows, judged over two windows at once. The burn rate of a window is its
fraction of bad events divided by the objective's budget (one minus its
target): a burn rate of 1 spends exactly the month's budget in a month.

| Pair | Long window | Short window | Fires at | Spends |
|---|---|---|---|---|
| fast | 1 hour | 5 minutes | 14.4 | a fiftieth of the month's budget every hour |
| slow | 6 hours | 30 minutes | 6 | a twentieth of the month's budget every six hours |

The long window says the burn is real; the short window says it is still
happening, so `objective_burn` clears within minutes of the fault ending rather
than an hour later. The long window must also hold at least two bad events:
on a quiet node a single failure among a dozen requests is a burn of a hundred
times the budget, and one failure is a fault report the request's own log line
already carries, not a rate.

The alarm names, for each burning objective, the fastest pair that fires and
both windows' burn rates. It is judged **per node**, like every alarm the
engine raises, over the events that node measured; the
`crewlet.objectives.events` counter, summed across the fleet by your collector,
is the company's figure. A self-hosted install gets the same constants as
reference objectives.

## The ready report

Every server-side indicator reads an outage that keeps requests from reaching
the engine at all (the artifact domain's DNS or certificate, an edge, a
corporate filter) as a quiet node. The ready report is the indicator that sees
one: the dashboard reports, for every frame it opens, how long the frame took
from its view ticket to the page's bridge hello, through
`POST /artifacts/ready` (see the [API reference](api-endpoints.md#viewing-a-version)).

A report is held to what it could honestly have measured:

- it names a live frame this deployment minted for the reporting credential,
  verified exactly as the artifact origin verifies the frame's ticket, so no
  caller reports about a frame somebody else opened;
- each frame reports once, and the node remembers a reported frame until its
  ticket ends (at most 65 536 at a time; past that a report waits for the
  earliest to end rather than being remembered loosely and counted twice);
- no report is longer than the frame has existed;
- one credential sends at most a sustained report a second, after a burst of
  thirty (a dashboard reopening every frame it had after a reconnect).

The node's memory of reported frames is its own: a frame reported to two nodes
is counted once on each, which the per-credential limit bounds. Each admitted
report is recorded into `crewlet.artifacts.ready.duration` by its outcome and
counted as one `artifact_ready` event.
