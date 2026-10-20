# Service Objectives

What the engine promises, as numbers an alarm can hold it to: publishing an
artifact, serving a document, a frame becoming ready in a viewer's browser,
the datastore's verdicts, a committed change reaching the screens watching it,
a wake leaving the change feed, and presence crossing the fleet. Each objective is declared once, in
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
good event; a `5xx`, or an outcome nobody could establish, is a bad one.

## The objectives

| Objective | Measures | Where | Good event | Within | Target |
|---|---|---|---|---|---|
| `publish_success` | Publish success | the publish pipeline (`internal/artifactupload`), every create and publish | published, or refused for the client's own reason (a moved head, a quota, a policy, a bad request), excluding the upload transfer | 30 s | 99.9% |
| `publish_applied` | Publish latency | the publish pipeline, for a publish that moves no new bytes (a rollback, carries, edits) | the domain's decision to the record applied | 2 s | 95% |
| `view_first_byte` | View time to first byte | the artifact origin (`internal/artifactserve`), every document request | the first byte written, under any status but a 5xx | 300 ms | 99.9% |
| `artifact_ready` | Artifact ready, reported by the viewer's browser | the dashboard, through `POST /artifacts/ready` | the frame's ticket minted to the page's bridge hello | 3 s | 99.5% |
| `data_round_trip` | Datastore op round trip | the datastore writer (`internal/artifactdata`), every write and undo | a verdict, not an unknown outcome | 250 ms | 95% |
| `data_resolved` | Datastore op resolution | the datastore writer, every write and undo | a verdict, not an unknown outcome | 5 s | 99.9% |
| `change_visibility` | Change visibility | the socket's push of a committed batch (`internal/api/stream`): an artifact's movement, a datastore's changed documents and a chat record, on every node pushing to a watching socket, whether from its own applier or a peer's relayed | the broker storing the record to its push to the sockets watching it | 1 s | 99% |
| `wake_latency` | Wake latency | the change feed (`internal/changefeed`), for every wake of every domain that wakes: the tracker, the knowledge base and chat | the broker storing the record that wakes somebody to its wake published on the inbound edge | 10 s | 95% |
| `presence_join` | Presence join | the presence hub (`internal/presence`), for a node's first socket in a room | the room's first socket on the node to its peers' members shown (the last answer to its ask applied, or the ask sent where nobody answers) | 1 s | best effort, no budget |
| `presence_cursor` | Presence cursor | the presence hub, for every member a peer's frame says moved | the member's move on its node to the frame naming it heard on this one | 150 ms | best effort, no budget |

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
| `change_visibility` | the socket's push, on every node, of a committed batch's artifact movement, datastore documents and chat records — from its own appliers, or a sound peer's relayed while its own copy is out of service | the broker storing the record to its push to the sockets watching it. A change no socket was handed — a room nobody is in, a private room nobody watching may read, a datastore row no reader in the room may see — is no event |
| `wake_latency` | the change feed, for every wake it publishes, whichever domain's record it came from (the tracker, the knowledge base, chat) | the broker storing the record to its wake published on the inbound edge. A record that wakes nobody, or that another node already claimed, is no event; a publish that failed is timed by the redelivery that lands it, from the same stored instant |
| `presence_join` | the presence hub, once for each room a node starts listening on | the room's first socket on the node to its peers' members shown: the last answer to the node's ask applied, or the ask sent where nobody answers. A socket joining a room the node already listens on is shown everybody at once and is no event |
| `presence_cursor` | the presence hub, for every member a peer's frame says moved | the move, by the instant the peer stamped it (a typing line's lapse included), to the frame heard here. A full roster — a rebroadcast, an answer to an ask — restates members rather than moving them and is no event |

**Three are measured across two clocks**: change visibility and wake latency
start at the broker's own instant for a record, and a presence move at the
peer's instant for it, while each ends on the measuring node's clock. A start
a little after the end is the clocks' skew and counts as zero rather than as a
bad event; skew the other way is inside the bound, which no skew a healthy
fleet runs with comes near. A clock badly off on one node shows as that node's
burn alone.

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
