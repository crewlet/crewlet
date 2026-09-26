# Alarms

Every condition the engine raises about itself, what it means, and what to do
about it.

An alarm reaches you two ways, and they are the same table evaluated once: a
`crewlet.alarm.active{kind}` gauge your collector scrapes, and a named
`WARN` line when it starts and another when it clears, carrying how long it
was up. Nothing has to be polled for either — every node evaluates its own
table every fifteen seconds, whether or not it holds any fleet duty.

The log line is the one to read first. It carries the measurement that raised
the alarm, in the units of the thing measured, and the remedy from the table
below.

| Alarm | What it means | What to do |
|---|---|---|
| `apply_lag` | The oldest record this node has not applied is more than a minute old. A node whose applied prefix stands still that long with records waiting refuses reads as `stalled` and gives its seats to a peer; one that is still applying keeps them. | Check this node's applier: `crewlet retention status` names the domain and this node's position in it, and a `statelog_apply_faulted` or `statelog_applier_stopped` line in its log names any error holding it. A node whose applied prefix stands still for a minute with records waiting refuses reads as `stalled` and gives its seats to a peer until it moves again. |
| `read_refusals` | Reads at one level have been refused for something other than ordinary lag, with none served at that level, for longer than fifteen seconds. | The `code` attribute on `crewlet.statelog.read.refusals` names what is refusing them. Anything other than `behind` or `too_stale` is a fault rather than a wait, and each refusal's own detail says what it needs. |
| `barrier_slow` | The read barrier — the append every linearizable read waits on — is spending a quarter of the whole read budget. | The barrier is an append and a wait: check the broker's own latency and this node's apply drain before looking anywhere else. |
| `log_headroom` | The log is within a tenth of its byte ceiling. A full log refuses writes rather than dropping records. | Raise the log's ceiling with `crewlet retention set-capacity` during a maintenance window, or find out why the trim is not advancing. A full log refuses writes; it does not drop records. |
| `backup_age` | The newest verified backup is older than the policy asks for. The trim will not advance past it. | Run `crewlet backup` against any node, whatever its roles, and check whatever was meant to run it. The trim will not advance past a backup this old. |
| `trim_blocked` | The trim has a term it cannot satisfy, so the log is growing toward its ceiling. | The blocking term names what to fix. Until it is fixed the log grows toward its ceiling. |
| `deferred_old` | This node has been holding records it cannot apply for longer than the deferral grace. Its seats have moved. | This node is running a build that cannot decode records its peers are writing. Upgrade it; its seats have already moved. |
| `deferred_coverage` | This node has been holding records it cannot apply for longer than the deferral grace, in a domain whose gaps cost coverage rather than seats. Its seats stay; that domain's answers on it are missing what those records carry. | This node is running a build that cannot decode records its peers are writing to a domain whose gaps cost coverage rather than seats. Upgrade it: nothing moves on its own, and until then that domain's answers on this node lack what those records carry. |
| `floor_unknown` | The trim floor has been unreadable from this node for longer than a minute. Every read that checks this node's health refuses while it is. | Coordination cannot be reached from this node. Its reads refuse until it can be. |
| `generation_left` | The trim floor is published at a generation this node's rows are not on: the fleet re-anchored the domain and this node did not follow. Every read that checks this node's health refuses, and a domain that gates seats gives them to a peer. | The fleet re-anchored this domain and this node did not follow, so its rows are keyed to the generation before. Restart it: its boot asks the fleet for a snapshot of the current generation and adopts it. |
| `prefetch_slow` | The knowledge search in turn-start context assembly is over its budget at p95. A turn that runs it waits for it before its executor starts. | Check the store's own latency and the knowledge backend's: each turn that searches at its start waits this long first. |
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

An alarm that fires on a healthy node is a defect in this table, not a
threshold for an operator to tune: each one fires at the number that already
decides something — the grace that sheds a node, the grace that moves its
seats, the budget a caller was promised.
