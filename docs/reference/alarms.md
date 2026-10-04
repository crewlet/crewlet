# Alarms

Every condition the engine raises about itself, what it means, and what to do
about it.

An alarm reaches you three ways, and they are the same table evaluated once,
every ten seconds on every node: a `crewlet.alarm.active{kind}` gauge your
collector scrapes, a named `WARN` line when it starts and another when it
clears, carrying how long it was up, and a count on the node's health envelope
(`GET /health` and the dashboard's health push carry `alarms: {count, worst}`,
where `worst` is the alarm that has been firing longest). Nothing has to be
polled for any of them — alarms are evaluated on the node's own position
heartbeat, so a condition with a one-minute grace is named within that minute.

The log line is the one to read first. It carries the measurement that raised
the alarm, in the units of the thing measured, and the remedy from the table
below.

| Alarm | What it means | What to do |
|---|---|---|
| `apply_lag` | This node is more than a minute behind the log. Being behind does not take its copy out of service; a position that stops moving does. | Check this node's applier: `crewlet retention status` names the domain and its position. A node that is behind keeps the seats it holds and claims no new ones. Only if its position stops moving for the stall grace, or it holds a record it cannot decode past the deferral grace, is its copy wrong rather than behind: it then stops serving its copy of the estate, and its seats stay and read the estate from the other data nodes until the copy recovers. |
| `read_refusals` | Reads are being refused for something other than ordinary lag, and have been for longer than a heartbeat. | Read the refusal code in the logs. Anything other than `behind` or `too_stale` is a fault rather than a wait. |
| `barrier_slow` | The read barrier — the append every linearizable read waits on — is spending a quarter of the whole read budget. | The barrier is an append and a wait: check the broker's own latency and this node's apply drain before looking anywhere else. |
| `log_headroom` | The log is within a tenth of the byte ceiling its ordinary writes are held to. A full log refuses writes rather than dropping records; on the tracker and pages logs an eviction still lands in the gate reserve above that ceiling. | Raise the log's ceiling with `crewlet retention set-capacity` during a maintenance window, or find out why the trim is not advancing (`crewlet retention status` names the term holding it). A full log refuses writes; it does not drop records. On a log that claims identity the top of the ceiling is kept for gate records, so if the term is a node that is gone, `crewlet retention evict` still lands and unpins the trim. |
| `backup_age` | No verified backup has been recorded, or the newest is older than the policy asks for. The trim does not advance either way. | Run `crewlet backup` against any node, whatever its roles, and check whatever was meant to run it. The trim does not advance past a backup older than the policy, and does not advance at all until there is one. |
| `trim_blocked` | The trim has a term it cannot satisfy, so the log is growing toward its ceiling. | The blocking term names what to fix. Until it is fixed the log grows toward its ceiling. |
| `deferred_old` | This node has been holding records it cannot apply for longer than the deferral grace. It no longer serves its copy of the estate; its seats read the estate from the other data nodes. | This node is running a build that cannot decode records its peers are writing. Upgrade it; it has already stopped serving its copy of the estate, and its seats read the estate from the other data nodes. |
| `floor_unknown` | The trim floor has been unreadable for four heartbeats, so every read on this node refuses. | Coordination cannot be reached from this node. Every read is refused until it can be. |
| `prefetch_slow` | Turn-start context assembly is over its budget. Every turn on this node pays it before its first token. | Every turn on this node pays this before its first token. Check the store's own latency and the knowledge backend's. |
| `search_slow` | Interactive search is over its target. The corpus has outgrown what one node's share of it can scan in the budget. | The corpus has outgrown what one node's share can scan in the budget. Adding a node divides the buckets again, with no configuration and no rebuild. See docs/guides/search.md. |
| `search_degraded` | Searches are being answered without their semantic half — the embeddings provider or the vector domain is failing. | The embeddings provider or the vector domain is failing. Search still answers; it answers less well, and silently. |
| `search_scoped` | Searches are being answered over part of the corpus because a node did not answer its bucket range. | A node did not cover its bucket range, so part of the corpus went unscanned — it was unreachable, or its own lexical index has not finished its first lap, which is what a node that joined a few minutes ago looks like and clears itself. The answers were complete for what was searched and silent about what was not; the log line names who was absent. |
| `history_partial` | Fleet history reads — turns, traces, the event log — are being answered without every node, because one did not answer inside the fleet read budget. | Turn-level history lives only on the node that published it, so a partial answer is missing that node's rows — every answer names the node in its `coverage`. A node that has left the fleet is gone with its detail, and the aggregates survive it in the usage domain; a live node that keeps missing the budget is slow on its own store or its route, and its own `pool_starved` and `apply_lag` alarms say which. |
| `recall_below_floor` | Less of the corpus has current vectors than semantic recall claims to cover. | The embed duty is behind. Semantic recall is answering from a corpus it does not cover. |
| `ivf_recall_below_floor` | The latest measurement of the semantic index found recall against the exact scan below the floor, in a query shape a search is issued in, even probing every list — which is the full scan's own candidate pool, so the first stage is below the floor on this corpus with or without the index. | The 1-bit first stage is failing this corpus, index or not: `crewlet search eval` against a backup's copy of the estate measures the full scan beside the index in every query shape and will say the same. The remedy is the evaluation's — raise BinaryOversample, then an int8 first stage, both code changes (see docs/guides/search.md). No index is installed meanwhile, so searches answer from the full scan at the recall the evaluation reports. |
| `records_gated` | An apply gate dropped a record. A gated record is recoverable by nothing. | A gated record is recoverable by nothing. Each drop's `statelog_record_gated` log line names the `gate` that dropped it, the record's `position` and `kind`, and the `writer` — the node that wrote it; this is worth reading today. |
| `feed_unreadable` | A change record no build on this node can read. It redelivers for ever, so every wake behind it is waiting too. | A record no build on this node can read. It redelivers for ever rather than being dropped, so the wakes behind it are waiting too — upgrade the node past it, or the feed stops moving. |
| `maintenance_open` | A maintenance operation has been open for an hour. Maintenance stops every publisher on every node. | Maintenance stops every publisher on every node. Finish it or abandon it; nothing is being written while it is open. |
| `volume_low` | A volume holding this node's databases has less free space than the next restore, vacuum or snapshot needs for a second copy — or could not be measured at all. The alarm names the volume. | Add space to the volume the alarm names — or, where it could not be measured, fix what stops it being read. A backup, a vacuum and a peer's join all need the room, and each fails partway through without it. |
| `wal_large` | The write-ahead log has grown past a gibibyte, which means a checkpoint is not happening. | A checkpoint is not happening, which usually means a reader is holding a snapshot open. It has no other symptom until the volume fills. |
| `pool_starved` | Callers are queuing for a database connection before their query starts. | Raise `store.max_open_conns`, or find the caller holding one. Every read on this node is queuing before it starts. |
| `census_drift` | A log is taking more than twice the linearizable reads its census allows — 125 a day per agent seat (at least one seat), plus what the object store's collector reads on its own schedule — so every sizing decision under it is stale. | Re-derive the log's ceiling and the trim's cadence from the real rate. See `stream.tracker_retention` in docs/getting-started/configuration.md. |
| `objects_missing` | Parts of the company's files are not in the object store: the collector's last audit asked the store for every chunk the estate names, and some were not there. Those files cannot be read in full. | The backend lost bytes it had acknowledged: check its own health (the NATS bucket OBJ_crewlet_files on the data nodes, or the S3 bucket) and restore the missing chunks from a backup — see docs/guides/backup.md. `crewlet objects status` names them. |

An alarm that fires on a healthy node is a defect in this table, not a
threshold for an operator to tune: each one fires at the number that already
decides something — the grace that takes a copy out of service, the grace
that stops a node serving a copy it cannot decode, the budget a caller was
promised.

A seat whose token window is spent is NOT an alarm, because nothing is wrong
with the node: the ceiling is doing what it was set to do. Its mail is parked
on its inbox until the window turns over or a revision raises the ceiling —
the `seat_budget_parked` line names the window and when it resets, and
the budgets surfaces show the refusing window. See the budget park in
docs/concepts/agent-runtime.md.
