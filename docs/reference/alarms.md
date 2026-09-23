# Alarms

Every condition the engine raises about itself, what it means, and what to do
about it.

An alarm reaches you two ways, and they are the same table evaluated once: a
`crewlet.alarm.active{kind}` gauge your collector scrapes, and a named
`WARN` line when it starts and another when it clears, carrying how long it
was up. Nothing has to be polled for either — every node evaluates the whole
table every fifteen seconds, and again the moment its quarter-hourly
measurements (each log's daily intake, the vector coverage, the store's
connection-pool waits) land, so an alarm with a sixty-second threshold is
raised within a beat of passing it.

The log line is the one to read first. It carries the measurement that raised
the alarm, in the units of the thing measured, and the remedy from the table
below.

| Alarm | What it means | What to do |
|---|---|---|
| `apply_lag` | This node is more than a minute behind the log. Its seats move if it stays behind for thirty. | Check this node's applier: `crewlet retention status` names the domain and its position. A node that stays behind past the deferral grace loses its seats to a peer. |
| `read_refusals` | Reads are being refused for something other than ordinary lag, and have been for longer than a heartbeat. | Read the refusal code in the logs. Anything other than `behind` or `too_stale` is a fault rather than a wait. |
| `barrier_slow` | The read barrier — the append every linearizable read waits on — is spending a quarter of the whole read budget. | The barrier is an append and a wait: check the broker's own latency and this node's apply drain before looking anywhere else. |
| `log_headroom` | The log is within a tenth of its byte ceiling. A full log refuses writes rather than dropping records. | Raise the log's ceiling with `crewlet retention set-capacity` during a maintenance window, or find out why the trim is not advancing. A full log refuses writes; it does not drop records. |
| `log_ceiling_short` | At the rate this log took in over the last day, its byte ceiling holds less than the `min_age` window the trim keeps, so it will fill and refuse writes with every trim term satisfied. | Raise the log's ceiling with `crewlet retention set-capacity` during a maintenance window, to at least the window's worth at this rate, or shorten `stream.tracker_retention.min_age` if the deployment does not need that window. Unblocking the trim cannot help: it never removes a record younger than min_age. |
| `backup_age` | The newest verified backup is older than the policy asks for. The trim will not advance past it. | Run `crewlet backup` against any node, whatever its roles, and check whatever was meant to run it. The trim will not advance past a backup this old. |
| `trim_blocked` | The trim has been blocked for longer than the log's `min_age` replay window plus one trim tick, and the log is keeping records older than that window — so it is holding what a working trim would have removed, and growing toward its ceiling. A young fleet blocked on its first backup or snapshot donors does not raise it: nothing in its log is past the window yet. | The blocking term names what to fix; `crewlet retention status` says what it has and what it wants. Until it is fixed the log grows toward its ceiling by a day of records every day. |
| `deferred_old` | This node has been holding a record it cannot apply for longer than the thirty-minute deferral grace. Where that record's log gates seat admission — the tracker's, the knowledge base's and the chart's do — its seats have moved to a peer. | This node is running a build that cannot decode records its peers are writing. Upgrade it. Where the record's log gates seat admission — the detail says — its seats have already moved to a peer. |
| `floor_unknown` | The trim floor has been unreadable for four heartbeats, so every read on this node refuses. | Read the cause. An unreachable coordination store clears when coordination does; a floor published at a later generation than this node's means the log was re-anchored and this node was not — see re-anchoring in docs/guides/retention.md. Every read on this node is refused until the floor can be read. |
| `prefetch_slow` | Turn-start context assembly is over its budget. Every turn on this node pays it before its first token. | Every turn on this node pays this before its first token. Check the store's own latency and the knowledge backend's. |
| `search_slow` | Interactive search is over its target. The corpus has outgrown what one node's share of it can scan in the budget. | The corpus has outgrown what one node's share can scan in the budget. Adding a node divides the buckets again, with no configuration and no rebuild. See docs/guides/search.md. |
| `search_degraded` | Searches are being answered without their semantic half — the embeddings provider or the vector domain is failing. | The embeddings provider or the vector domain is failing. Search still answers; it answers less well, and silently. |
| `search_scoped` | Searches are being answered over part of the corpus because a node did not answer its bucket range. | A node did not cover its bucket range, so part of the corpus went unscanned — it was unreachable, or its own lexical index has not finished its first lap, which is what a node that joined a few minutes ago looks like and clears itself. The answers were complete for what was searched and silent about what was not; the log line names who was absent. |
| `recall_below_floor` | Less of the corpus has current vectors than semantic recall claims to cover. | The embed duty is behind. Semantic recall is answering from a corpus it does not cover. |
| `records_gated` | An apply gate dropped a record. A gated record is recoverable by nothing. | A gated record is recoverable by nothing. The log line names the gate, the operator and the position; this is worth reading today. |
| `feed_unreadable` | A change record no build on this node can read. It redelivers for ever, so every wake behind it is waiting too. | A record no build on this node can read. It redelivers for ever rather than being dropped, so the wakes behind it are waiting too — upgrade the node past it, or the feed stops moving. |
| `maintenance_open` | A maintenance operation has been open for an hour. Maintenance stops every publisher on every node. | Maintenance stops every publisher on every node. Finish it or abandon it; nothing is being written while it is open. |
| `volume_low` | The volume has less free space than the next restore, vacuum or snapshot needs for a second copy. | Add space. A backup, a vacuum and a peer's join all need it, and each fails partway through without it. |
| `wal_large` | The write-ahead log has grown past a gibibyte, which means a checkpoint is not happening. | A checkpoint is not happening, which usually means a reader is holding a snapshot open. It has no other symptom until the volume fills. |
| `pool_starved` | Callers are queuing for a database connection before their query starts. | Raise `store.max_open_conns`, or find the caller holding one. Every read on this node is queuing before it starts. |
| `census_drift` | This company is doing more than twice the reads its log was sized for, so every sizing decision under it is stale. | Re-derive the log's ceiling and the trim's cadence from the real rate. See `stream.tracker_retention` in docs/getting-started/configuration.md. |
| `iam_binding_dangling` | A person has been bound for longer than a minute to a seat this node's org chart does not hold as a human seat — removed, turned into an agent seat, or not applied here yet — so every request they make is refused or held off. | Run `crewlet iam check`, which names who and why. A seat that was removed or is not a human seat needs its person unbound (`crewlet iam unbind`) or bound to another (`crewlet iam bind`) — one record either way. A seat this node's chart has not reached yet is its chart applier: read `apply_lag` first. |

An alarm that fires on a healthy node is a defect in this table, not a
threshold for an operator to tune: each one fires at the number that already
decides something — the grace that sheds a node, the grace that moves its
seats, the budget a caller was promised, the replay window a log's ceiling was
sized to hold.
