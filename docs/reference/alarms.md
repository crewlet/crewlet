# Alarms

Every condition the engine raises about itself, what it means, and what to do
about it.

An alarm reaches you three ways, and they are the same table evaluated once: a
`crewlet.alarm.active{kind}` gauge your collector scrapes, a named `WARN`
line when it starts and another when it clears, carrying how long it was up,
and a count on the node's health envelope (`GET /health` and the
dashboard's health push carry `alarms: {count, worst}`, where `worst`
is the alarm that has been firing longest). Nothing has to be polled for any
of them: every node running a state log, which is every data node, evaluates
the whole table on its own position heartbeat, every 10s, and again the moment
the measurements it takes every 15m (each log's daily intake, the vector
coverage, the store's connection-pool waits) land, so an alarm whose
threshold is 1m is raised within a beat of passing it.

The log line is the one to read first. It carries the measurement that raised
the alarm, in the units of the thing measured, and the remedy from the table
below.

Each kind is one alarm, however many logs it stands on. These are about a log
rather than the node:

`log_headroom`, `log_ceiling_short`, `backup_age`, `trim_blocked`, `deferred_old`, `floor_unknown` and `census_drift`.

Each of those names every log it stands on in its `domains` list, each
entry a measurement and the logs it was taken on — one log where a trim term
holds it, and every log measured in the same words otherwise, so a fleet with
no backup yet is one entry naming every log — and leads with the most pressing
log's, so the health count and the gauge (one series per kind, reading 1 while
any log's stands) agree about how many problems there are. Their log lines are
written per log:
each log the alarm comes to stand on writes its own `alarm_raised`, and each
log it stops standing on its own `alarm_cleared`, carrying the log as
`domain`, so a second log joining an alarm that is already up, and the first
one leaving it, are each said.

These are about one model entry rather than the node:

`llm_credential_expiring`, `llm_credential_expires_soon`, `llm_sign_in_refused`, `llm_limit` and `llm_limit_waiting`.

Each names every entry it stands on in its `entries` list, with that
entry's own measurement, and writes its log lines per entry, carrying it as
`entry`. Every data node evaluates them. A refusal, a limit, a wait and
a stored credential's age come from the fleet's own records (each live node's
heartbeat and the secret store), so those read the same on every node; whether
anything signs a model in, and how long its token lasts, are the evaluating
node's own reading, so a model nothing signs in on one node is said by that
node. Their thresholds are fixed, with no setting: a credential's expiry
is said at 30 days and again at 7, and a limit turns into
`llm_limit_waiting` once a seat has waited 15m on it.

| Alarm | What it means | What to do |
|---|---|---|
| `apply_lag` | This node is more than a minute behind the log. Being behind does not take its copy out of service; a position that stops moving does. | Check this node's applier: `crewlet retention status` names the domain and its position. A node that is behind keeps the seats it holds and claims no new ones. Only if its position stops moving for the stall grace, or it holds a record it cannot decode past the deferral grace, is its copy wrong rather than behind: it then stops serving its copy of the estate, and its seats stay and read the estate from the other data nodes until the copy recovers. |
| `read_refusals` | Reads are being refused for something other than ordinary lag, and have been for longer than a heartbeat. | Read the refusal code in the logs. Anything other than `behind` or `too_stale` is a fault rather than a wait. |
| `barrier_slow` | The read barrier (the append every linearizable read waits on) is spending a quarter of the whole read budget. | The barrier is an append and a wait: check the broker's own latency and this node's apply drain before looking anywhere else. |
| `log_headroom` | A log is within a tenth of the byte ceiling its ordinary writes are held to. One alarm names every such log, leading with the one with the least room. A full log refuses writes rather than dropping records; on every log that claims identity (Worklet's, Doclet's, the identity estate's, the artifacts log's, the artifact datastore's, Ask Crewlet's and Chatlet's) an eviction still lands in the gate reserve above that ceiling. | Raise the log's ceiling with `crewlet retention set-capacity` during a maintenance window, or find out why the trim is not advancing (`crewlet retention status` names the term holding it). A full log refuses writes; it does not drop records. On a log that claims identity the top of the ceiling is kept for gate records, so if the term is a node that is gone, `crewlet retention evict` still lands and unpins the trim. |
| `log_ceiling_short` | At the rate this log took in over the last day, its byte ceiling holds less than the `min_age` window the trim keeps, so it will fill and refuse writes with every trim term satisfied. | Raise the log's ceiling with `crewlet retention set-capacity` during a maintenance window, to at least the window's worth at this rate, or shorten `stream.tracker_retention.min_age` if the deployment does not need that window. Unblocking the trim cannot help: it never removes a record younger than min_age. |
| `backup_age` | No verified backup of a log has been recorded, or the newest one covering it is older than the policy asks for. The trim does not advance that log either way. One alarm names every such log, leading with the one whose backup is missing or oldest, and a log it names whose trim the backup term is holding past its replay window raises no `trim_blocked` of its own. | Run `crewlet backup` against any node, whatever its roles, and check whatever was meant to run it; under `backup_floor: operator`, acknowledge each named log's off-host copy with `crewlet retention ack -stream <its stream> -position <n>`, n being the sequence the copy reaches. The trim does not advance a log past a backup older than the policy, and does not advance it at all until there is one. |
| `trim_blocked` | A log's trim has been blocked for longer than its `min_age` replay window plus one trim tick, and the log is keeping records older than that window, so it is holding what a working trim would have removed and growing toward its ceiling. One alarm names every such log and the term holding each, leading with the one held longest; a log the backup term holds while `backup_age` names it is named there instead. A young fleet blocked on its first backup or snapshot donors does not raise it: nothing in its log is past the window yet. | The term holding each log names what to fix, and `crewlet retention status` prints it with what it has and what it wants. Until it is fixed the log grows toward its ceiling by a day of records every day. |
| `deferred_old` | This node has been holding a record it cannot apply on a log for longer than the thirty-minute deferral grace. One alarm names every log that is, leading with the one held longest. Where a log judges this node's copy of the estate (Worklet's, Doclet's and Chatlet's do), the node no longer serves its copy, and its seats read the estate from the other data nodes. | This node is running a build that cannot decode records its peers are writing. Upgrade it. Where the record's log judges this node's copy of the estate (the detail says), the node has already stopped serving its copy, and its seats read the estate from the other data nodes. |
| `floor_unknown` | This node has been unable to read a log's trim floor for four heartbeats, so every read of that log here refuses. One alarm names every such log, which is every log at once when coordination is what cannot be reached. | Read the cause. An unreachable coordination store clears when coordination does, and every log on this node says so at once; a floor published at a later generation than this node's means that log was re-anchored and this node was not (see re-anchoring in docs/guides/retention.md). Every read of the log on this node is refused until its floor can be read. |
| `prefetch_slow` | Turn-start context assembly is over its budget. Every turn on this node pays it before its first token. | Every turn on this node pays this before its first token. Check the store's own latency and the knowledge backend's. |
| `search_slow` | Interactive search is over its target. The corpus has outgrown what one node's share of it can scan in the budget. | The corpus has outgrown what one node's share can scan in the budget. Adding a data node divides the buckets again, with no configuration and no rebuild; a node without `data` takes no range. See docs/guides/search.md. |
| `search_degraded` | Searches are being answered without their semantic half: the embeddings provider or the vector domain is failing. | The embeddings provider or the vector domain is failing. Search still answers; it answers less well, and silently. |
| `search_scoped` | Searches are being answered over part of the corpus because a node did not answer its bucket range. | A node did not cover its bucket range, so part of the corpus went unscanned: it was unreachable, or its own lexical index has not finished its first lap, which is what a node that joined a few minutes ago looks like and clears itself. The answers were complete for what was searched and silent about what was not; the log line names who was absent. |
| `history_partial` | Fleet history reads (turns, traces, the event log) are being answered without every data node, because one did not answer inside the fleet read budget. | Turn-level history lives only on the data node that published it or keeps it in custody, so a partial answer is missing that node's rows, and every answer names the node in its `coverage`. A node that has left the fleet is gone with its detail, and the aggregates survive it in the usage domain; a live node that keeps missing the budget is slow on its own store or its route, and its own `pool_starved` and `apply_lag` alarms say which. |
| `recall_below_floor` | Less of the corpus has current vectors than semantic recall claims to cover. | The embed duty is behind. Semantic recall is answering from a corpus it does not cover. |
| `ivf_recall_below_floor` | The latest measurement of the semantic index found recall against the exact scan below the floor, in a query shape a search is issued in, even probing every list, which is the full scan's own candidate pool, so the first stage is below the floor on this corpus with or without the index. | The 1-bit first stage is failing this corpus, index or not: `crewlet search eval` against a backup's copy of the estate measures the full scan beside the index in every query shape and will say the same. The remedy is the evaluation's: raise BinaryOversample, then an int8 first stage, both code changes (see docs/guides/search.md). No index is installed meanwhile, so searches answer from the full scan at the recall the evaluation reports. |
| `records_gated` | An apply gate dropped a record. A gated record is recoverable by nothing. | A gated record is recoverable by nothing. Each drop's `statelog_record_gated` log line names the `gate` that dropped it, the record's `position` and `kind`, and the `writer` (the node that wrote it); this is worth reading today. |
| `feed_unreadable` | A change record no build on this node can read. It redelivers for ever, so every wake behind it is waiting too. | A record no build on this node can read. It redelivers for ever rather than being dropped, so the wakes behind it are waiting too. Upgrade the node past it, or the feed stops moving. |
| `maintenance_open` | A capacity window has been open for an hour. While it's open no node starts in service, and every node restarted for it publishes nothing. A window this node last read open and can't read now is judged as old as it was when it was read, and says so. | While a capacity window is open no node starts in service, and every node restarted for it publishes nothing. Finish it or abandon it. |
| `volume_low` | A volume holding this node's databases has less free space than the next restore, vacuum or snapshot needs for a second copy, or could not be measured at all. The alarm names the volume. | Add space to the volume the alarm names. Where it could not be measured, fix what stops it being read. A backup, a vacuum and a peer's join all need the room, and each fails partway through without it. |
| `wal_large` | The write-ahead log has grown past a gibibyte, which means a checkpoint is not happening. | A checkpoint is not happening, which usually means a reader is holding a snapshot open. It has no other symptom until the volume fills. |
| `pool_starved` | Callers are queuing for a database connection before their query starts. | Raise `store.max_open_conns`, or find the caller holding one. Every read on this node is queuing before it starts. |
| `census_drift` | A log is taking more than twice the linearizable reads its census allows, so every sizing decision under it is stale. The census is the rate its domain declares for each agent seat, counting at least one seat, plus what the object store's collector reads on its own schedule. That rate is 125 a day for the tracker, the knowledge base, the artifacts and their datastore, 120 for chat, and none for the identity estate and Ask Crewlet. | Each read past the census is a barrier record on the named log, so re-derive that log's own ceiling — the field its line names — adding what its barriers take at the real rate to whatever else the field is sized from, and the trim's cadence (`stream.tracker_retention`). See docs/getting-started/configuration.md. |
| `objects_missing` | Some of the company's files cannot be read: the collector's last audit asked the object store for every file's bytes, and some were not there, or were there at the wrong size or under the wrong digest. Those files cannot be downloaded or read until they are restored. | The backend lost or changed bytes it had acknowledged: check its own health (the NATS bucket OBJ_crewlet_files on the data nodes, or the S3 bucket) and restore those files from a backup, or upload them again (see docs/guides/backup.md). `crewlet objects status` names them. |
| `objects_bucket_near_ceiling` | The fleet's own object bucket on the broker holds more than four fifths of the bytes it reserves. A full bucket refuses every upload, though nothing else on the broker is affected. | Once the bucket is full every upload is refused. Raise its reservation (`nats stream update OBJ_crewlet_files --max-bytes=<bytes>`, within what the broker can grant), or move the files to an S3-compatible bucket with `crewlet objects migrate --to s3` (see docs/concepts/object-store.md). |
| `objective_burn` | A service objective this node measures (publishing, serving a document, a frame becoming ready in a viewer's browser, a datastore verdict, a committed change reaching the sockets watching it, a wake leaving the change feed) is failing fast enough to spend its month's error budget early: in both the long and the short window of one of the two burn-rate pairs. | Find the failing events where the objective is measured (docs/reference/service-objectives.md names each one's source and its own instrument), starting from `crewlet.objectives.events` by `objective` and `outcome`. A fast burn spends a fiftieth of the month's error budget every hour and needs attention now; a slow burn spends a twentieth every six hours. The alarm clears within minutes of the fault ending, once the short window is healthy again. |
| `llm_credential_expiring` | A model's credential expires in thirty days or less, and more than seven. The expiry is computed from when the credential was stored and the lifetime its harness declares (a Claude Code setup-token lasts a year), because the token itself does not say. | Rotate the credential before it expires: Settings › Models › the entry › Rotate tests the new value before it replaces the old one, and the models that read it switch to it at once. Settings › Models names the entry, and `crewlet llm doctor <key>` checks it end to end on a node. |
| `llm_credential_expires_soon` | A model's credential expires in seven days or less. It replaces `llm_credential_expiring` for that model. | Rotate the credential now: Settings › Models › the entry › Rotate. Once it expires every seat on the model is refused. Settings › Models names the entry, and `crewlet llm doctor <key>` checks it end to end on a node. |
| `llm_sign_in_refused` | A model cannot sign in: its latest refusal on any node is newer than its latest answer on any node, its credential's computed expiry has passed, or nothing signs it in while a seat or Ask Crewlet runs on it. | Give the model a credential that signs in: Settings › Models › the entry › Rotate for a token or a key, or sign the harness in on the node (`crewlet llm login <key>`) for a host login. Every call on it is refused until then, and its seats fall back to the next entry in their chain if they have one. Settings › Models names the entry, and `crewlet llm doctor <key>` checks it end to end on a node. |
| `llm_limit` | A model sits out a plan limit on at least one node, and no seat has waited on it for fifteen minutes. Its seats fall back to the next entry in their chain meanwhile. | Nothing, if the plan's limit is expected: the engine tries the model again on its own and its seats fall back meanwhile. To end it now, Settings › Models › the entry › Retry asks the vendor once, and Clear stops every node sitting it out. Settings › Models names the entry, and `crewlet llm doctor <key>` checks it end to end on a node. |
| `llm_limit_waiting` | A seat has been parked for fifteen minutes or more on a chain naming a model that sits out a limit, with nothing to fall back to. It replaces `llm_limit` for that model. | Seats are not working. Add a fallback entry to their chain (a model on another sign-in), raise the plan, or wait for the reset the vendor named; Settings › Models › the entry › Retry asks the vendor once. Settings › Models names the entry, and `crewlet llm doctor <key>` checks it end to end on a node. |

An alarm that fires on a healthy node is a defect in this table, not a
threshold for an operator to tune: each one fires at the number that already
decides something: the grace that takes a copy out of service, the grace
that stops a node serving a copy it cannot decode, the budget a caller was
promised, the replay window a log's ceiling was sized to hold.

A seat whose token window is spent is NOT an alarm, because nothing is wrong
with the node: the ceiling is doing what it was set to do. Its mail is parked
on its inbox until the window turns over or a revision raises the ceiling.
The `seat_budget_parked` line names the window and when it resets, and
the budgets surfaces show the refusing window. See the budget park in
docs/concepts/agent-runtime.md.
