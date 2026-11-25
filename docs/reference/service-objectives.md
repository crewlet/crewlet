# Service Objectives

The engine's promises as numbers an alarm holds it to: publishing an artifact, serving a document, a frame becoming ready in a viewer's browser, the datastore's verdicts, a committed change reaching watching screens, a wake leaving the change feed, and presence crossing the fleet. Each objective is declared once, with its reason, in `internal/objectives`; every recording site, the `objective_burn` [alarm](alarms.md) and the `crewlet.objectives.events` [metric](metrics.md) read the same constants.

## How an objective is stated

Every objective is an **event objective**: over thirty days, a target fraction of events must be good, meaning answered without engine-side fault within the objective's bound. A latency percentile is the same shape ("p95 under 250 ms" is "95% of operations within 250 ms"), so it carries an error budget and burn rate like an availability objective.

- A client's own refusal is a good event: a publish refused because its base version moved, a quota, a policy finding or a malformed request.
- A `5xx`, or an outcome nobody could establish, is a bad event.
- A publish refused because the object store has no room (`507 store_full`) is the deployment's refusal, a bad event: a full bucket refuses every publish until an operator makes room, which is what the budget is there to burn.

A caller that went away is no fault:

- An event its caller cancelled before it was answered (a frame torn down, a browser that left, a dropped connection, a stopped turn) is no event while inside its bound, since nobody can say how it would have ended.
- One already past its bound when the caller left was late first, and is a bad event, so a store that stops answering still burns the budget.
- Only a cancellation is a caller leaving: a request that hits a deadline failed a bound, a bad event.
- The artifact origin counts such a request under `status="abandoned"` in `crewlet.artifacts.serve.requests`, not as a `5xx`, and logs it at debug as `artifacts_serve_abandoned`, not as the store being unavailable.

## The objectives

| Objective | Measures | Where | Good event | Within | Target |
|---|---|---|---|---|---|
| `publish_success` | Publish success | the publish pipeline (`internal/artifactupload`), every create and publish | published, or refused for the client's own reason (a moved head, a quota, a policy, a bad request; a full object store, `507 store_full`, is a bad event), excluding the upload transfer | 30 s | 99.9% |
| `publish_applied` | Publish latency | the publish pipeline, for a publish that moves no new bytes (a rollback, carries, edits) | the domain's decision to the record applied | 2 s | 95% |
| `view_first_byte` | View time to first byte | the artifact origin (`internal/artifactserve`), every document request | the first byte written, under any status but a 5xx | 300 ms | 99.9% |
| `artifact_ready` | Artifact ready, reported by the viewer's browser | the dashboard, through `POST /artifacts/ready` | the frame's ticket minted to the page's bridge hello | 3 s | 99.5% |
| `data_round_trip` | Datastore op round trip | the datastore writer (`internal/artifactdata`), every write and undo | a verdict, not an unknown outcome | 250 ms | 95% |
| `data_resolved` | Datastore op resolution | the datastore writer, every write and undo | a verdict, not an unknown outcome | 5 s | 99.9% |
| `change_visibility` | Change visibility | the socket's push of a committed batch (`internal/api/stream`): a seat's inbox movement, an artifact's movement, a datastore's changed documents and a chat record, on every node pushing to a watching socket, whether from its own applier or a peer's relayed | the broker storing the record to its push to the sockets watching it | 1 s | 99% |
| `wake_latency` | Wake latency | the change feed (`internal/changefeed`), for every wake of every domain that wakes: the tracker, the knowledge base and chat | the broker storing the record that wakes somebody to its wake published on the inbound edge | 10 s | 95% |
| `presence_join` | Presence join | the presence hub (`internal/presence`), for a node's first socket in a room | the room's first socket on the node to its peers' members shown (the last answer to its ask applied, or the ask sent where nobody answers) | 1 s | best effort, no budget |
| `presence_cursor` | Presence cursor | the presence hub, for every member a peer's frame says moved | the member's move on its node to the frame naming it heard on this one | 150 ms | best effort, no budget |
| `session_carry` | Session carry | the sandbox's publish of a collected launch (`internal/sandbox`), for every launch that had a session to continue: its turn's or its conversation's newest | it continued that session, or did not for a deliberate reason (a session out of its model's context or refused by its provider, another harness or entry, the seat's budget floor) | no bound | 99% |

A best-effort indicator has a bound and no budget: it is counted and exported, and never burns. Presence is best effort by design (told, never stored; degrades silently).

An **outcome objective** has no bound: its event is good or bad by what happened, not how long it took. Session carry is one. Its target is a judgement awaiting production data: a session's text grows 1.2 to 1.8 MiB an hour against the 48 MiB kept, so a loss is rare, and each costs a conversation its working memory. Under the burn windows below it fires at about a 14% loss rate within an hour, or 6% over six hours, and never on one loss.

## Where each is measured

| Objective | Recorded by | The event |
|---|---|---|
| `publish_success` | the publish pipeline's `Create` and `Publish`, from any surface (dashboard, agent tool, CLI) | the request to its answer; the upload transfer is a separate request, never counted |
| `publish_applied` | the same, for a rollback and a publish whose files are all carried, restored, removed or edited | the domain's decision to its record applied; a refusal is no latency event |
| `view_first_byte` | the artifact origin's handler, for every request it answers as a document (a frame's page, or a page the frame loads as one) | the request to the instant the handler committed to its status |
| `artifact_ready` | `POST /artifacts/ready`, reported by the dashboard for every frame it opens | the frame handed to the dashboard to the page's bridge hello, or to the frame failing |
| `data_round_trip`, `data_resolved` | the datastore writer, for every write and undo | the request to the verdict read back from this node's rows; a write still pending on this node delivered no verdict and is a bad event |
| `change_visibility` | the socket's push, on every node, of a committed batch's inbox movements, artifact movement, datastore documents and chat records, from its own appliers or a sound peer's relayed while its own copy is out of service | the broker storing the record to its push to the sockets watching it; an inbox movement is one push per record routed to that seat, timed from the oldest. No event: a change no socket was handed (an unwatched inbox, an empty room, a private room no watcher may read, a datastore row no reader in the room may see), and the re-read a source switch asks of every watched inbox |
| `wake_latency` | the change feed, for every wake it publishes, from any domain's record (tracker, knowledge base, chat) | the broker storing the record to its wake published on the inbound edge. No event: a record that wakes nobody or that another node already claimed. A failed publish is timed by the redelivery that lands it, from the same stored instant |
| `presence_join` | the presence hub, once per room a node starts listening on | the room's first socket on the node to its peers' members shown: the last answer to the node's ask applied, or the ask sent where nobody answers. A socket joining a room the node already listens on is shown everybody at once and is no event |
| `presence_cursor` | the presence hub, for every member a peer's frame says moved | the move, by the instant the peer stamped it (a typing line's lapse included), to the frame heard here. A full roster (rebroadcast, answer to an ask) restates members and is no event |
| `session_carry` | the sandbox, once per collected launch, as it publishes the launch's `executor_collected` record | a launch whose brief continued a session, or that says it did not continue one (`session_not_continued`). Bad for every loss but `context_exhausted`, `request_refused`, `harness_changed`, `budget_floor` and `entry_changed`, which the engine chose. No event: a conversation's first turn, a company or harness that keeps no session ([Conversation sessions](../concepts/conversation-sessions.md#when-a-run-does-not-continue-its-session)) |

**Three are measured across two clocks**: change visibility and wake latency start at the broker's instant for a record, a presence move at the peer's, and each ends on the measuring node's clock. A start slightly after the end is skew and counts as zero, not a bad event; skew the other way is inside the bound, which no healthy fleet's skew approaches. A badly off clock shows as that node's burn alone.

## Burn-rate alarms

An objective alarms when it spends its error budget faster than the month allows, judged over two windows at once. A window's burn rate is its fraction of bad events divided by the budget (one minus the target); a burn rate of 1 spends exactly the month's budget in a month.

| Pair | Long window | Short window | Fires at | Spends |
|---|---|---|---|---|
| fast | 1 hour | 5 minutes | 14.4 | a fiftieth of the month's budget every hour |
| slow | 6 hours | 30 minutes | 6 | a twentieth of the month's budget every six hours |

The long window says the burn is real; the short says it is still happening, so `objective_burn` clears within minutes of the fault ending. The long window must also hold at least two bad events: on a quiet node one failure among a dozen requests is a burn of a hundred times the budget, and a single failure is a fault the request's own log line already carries, not a rate.

The alarm names, per burning objective, the fastest pair that fires and both windows' burn rates. It is judged **per node**, like every alarm, over that node's events, on a node without `data` (a satellite, an operator's seat pod) by that node's own table ([Alarms](alarms.md#how-an-alarm-reaches-you)); the `crewlet.objectives.events` counter, summed across the fleet by your collector, is the company's figure. A self-hosted install gets the same constants as reference objectives.

## The ready report

Every server-side indicator reads an outage that stops requests reaching the engine (the artifact domain's DNS or certificate, an edge, a corporate filter) as a quiet node. The ready report sees one: for every frame it opens, the dashboard reports how long the frame took from its view ticket to the page's bridge hello, through `POST /artifacts/ready` (see the [API reference](api-endpoints.md#viewing-a-version)).

A report is admitted only if:

- it names a live frame this deployment minted for the reporting credential, verified as the artifact origin verifies the frame's ticket, so no caller reports a frame somebody else opened;
- the frame has not reported before; the node remembers reported frames until their ticket ends (at most 65 536; past that a report waits for the earliest to end rather than be remembered loosely and counted twice);
- it is no longer than the frame has existed;
- its credential sends at most a sustained one per second, after a burst of thirty (a dashboard reopening every frame after a reconnect).

Each node's memory is its own: a frame reported to two nodes is counted once on each, which the per-credential limit bounds. Each admitted report is recorded into `crewlet.artifacts.ready.duration` by its outcome and counted as one `artifact_ready` event.
