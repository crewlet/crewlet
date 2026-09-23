# Metrics

**Generated from `metrics.Catalogue()`. Do not edit — change the catalogue.**

The engine exports OpenTelemetry metrics through the same `OTEL_*` environment
its traces use, because a company has one telemetry backend and two settings
would let you split signals across two of them where a correlation resolves on
neither:

| Variable | What it does |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | The collector. Metrics are posted to `<endpoint>/v1/metrics`. |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Overrides the base for metrics alone. |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` (default) or `grpc`. |
| `OTEL_EXPORTER_OTLP_HEADERS` | Sent with every export — an API key, a tenant. |
| `OTEL_METRICS_EXPORTER` | `none` switches the export off and leaves traces on. |
| `OTEL_METRIC_EXPORT_INTERVAL` | Export period in milliseconds. Default 60000. |

**The provider is installed whether or not you are collecting.** Every
instrument exists either way, so no code path in the engine branches on
whether metrics are "on", and the numbers on `crewlet retention status` are
read from the same recorder a collector would be reading. With no endpoint set
the measurements are taken and not shipped.

**Every attribute below is a closed set.** No metric carries a task key, a
subject id, a page title or a node id as an attribute — the node is the
resource, set once — so the number of time series is bounded by this page
rather than by how much work your company has done.

## Histograms

A distribution, exported with each instrument's own bucket boundaries, so a bucket on your panel is the bucket the engine counted into. The default set is twenty-one powers of two from 1/16 to 65 536 in the instrument's own unit — 62.5 µs to about 65.5 s for a duration in milliseconds — so a percentile read from them is within a factor of two anywhere in that range. An instrument whose range that does not cover declares its own, listed in its row. The SDK's default boundaries stop at 10 000, which for a duration in milliseconds is 10 s: anything slower would land in the overflow bucket and read only as "more than 10 s".

| Metric | Unit | Buckets | Attributes | What it makes visible |
|---|---|---|---|---|
| `crewlet.statelog.publish.duration` | `ms` | default | `domain`, `outcome` | A write path slowing down before it starts refusing. The outcome dimension separates the three answers a write has from its refusals, so a rise in `pending` reads as an applier falling behind rather than as a broker getting slower. |
| `crewlet.statelog.publish.rounds` | `{round}` | default | `domain` | Contention on one subject, which the compare-and-set round cap bounds. A distribution creeping toward the cap is a hot object; reaching it is a refusal a model reads as a colleague editing the same thing. |
| `crewlet.statelog.write.session_wait` | `ms` | default | `domain` | The wait a write pays for its own previous write to apply. Zero when the caller is caught up, which is the common case, and the shape of the load that is not. |
| `crewlet.statelog.barrier.duration` | `ms` | default | `domain` | The broker round trip under every linearizable read, and the first number a drifting fsync or a degrading quorum moves. |
| `crewlet.statelog.read.wait` | `ms` | default | `domain`, `level` | How much of the read budget a barrier or session wait actually spends. A p95 approaching the budget is reads about to start refusing, which is the warning the refusal itself is too late to be. |
| `crewlet.statelog.apply.latency` | `ms` | default | `domain` | THE COMMIT-TO-APPLY GAP: from the broker's own timestamp on a record to this node committing it. Every read level is a policy about this quantity. |
| `crewlet.statelog.apply.tx.duration` | `ms` | default | `domain`, `bound_by` | How long one apply transaction holds the store's writer, and which budget ended it: from the start of the attempt that committed to its commit. It is what every writer queued behind it waits out, which is why the budgets bound it. The wait for the writer before it, an attempt the store rolled back and ran again, and the acknowledgement after it are all outside it — and all inside the drain gauge's span, which is what a backlog costs. |
| `crewlet.statelog.apply.record.duration` | `ms` | default | `domain`, `kind` | One record's apply. A single record past the time budget is still one transaction, so this is the real ceiling on how long a read can be delayed. |
| `crewlet.statelog.apply.batch.rows` | `{row}` | default | `domain` | Rows per apply transaction, as the domain's applier reports writing them, which is what the row budget bounds. Rows, not records: the drain gauge counts records. |
| `crewlet.backup.duration` | `ms` | 21, from 128 to 134217728 | — | How long a backup took, which is the window the trim hold covers and the I/O the copy spends competing with the applier's own commits. It is what turns the retention guide's worked example into a number for THIS hardware. |
| `crewlet.store.pool.wait` | `ms` | default | `file` | How long callers queued for one of this file's pooled connections: one observation per reporting tick in which a caller queued, holding that tick's mean wait, because the pool reports a total and a count rather than each wait. It is what says the reader pool is too small on this node. |
| `crewlet.tracker.search.scan.duration` | `ms` | default | `path`, `rung` | The semantic scan, split by whether it ran for a turn's prefetch or for somebody's deliberate search, because the interactive path is the one with a target and a prefetch's scans would dilute it. |

## Gauges

A value that goes both ways, sampled at each export.

| Metric | Unit | Attributes | What it makes visible |
|---|---|---|---|
| `crewlet.statelog.drain.records_per_second` | `{record}/s` | `domain` | The applier's measured drain in RECORDS a second: every record an apply run moves over, applied or not, over the run's whole span — the wait for the writer and the acknowledgement included — smoothed across runs. It is the rate this node states a record backlog as a time with — a stale read's staleness bound, a refused read's retry hint and a bulk edit's projection — so a falling rate is an applier slowing down. Zero until this process has consumed a batch: nothing seeds it, and the first batch's rate is the first reading. |
| `crewlet.statelog.drain.commits_per_second` | `{commit}/s` | `domain` | Commits per second, which is the fsync rate under `synchronous = FULL` and the number a device budget is spent by. |
| `crewlet.statelog.apply.lag.seq` | `{record}` | `domain` | How many records this node is behind the log's head. |
| `crewlet.statelog.apply.lag.seconds` | `s` | `domain` | How old the oldest record this node has not applied is: now less the broker's own timestamp on the first record past its checkpoint, and zero when it is caught up. An AGE rather than the backlog over the drain, because an applier that has stopped keeps its last drain — its projection stays as small as its backlog on a quiet log — while the records it owes go on growing old. It is what the `apply_lag` alarm fires on, at a minute. Exact on a strict log; on the compacted vector log, where that record may have been superseded, it is the age of the newest one instead, which never overstates. |
| `crewlet.statelog.applied_through` | `{sequence}` | `domain` | The prefix this node has actually applied, which is lower than its checkpoint whenever a record was retained. |
| `crewlet.statelog.deferred.count` | `{record}` | `domain` | How many records this node holds that its build could not read. Non-zero is a rolling upgrade in progress; the oldest one's age beside it says whether the upgrade has stopped. |
| `crewlet.statelog.deferred.oldest_age_seconds` | `s` | `domain` | How long the oldest retained record has been retained, which is what decides whether this node's seats move. |
| `crewlet.statelog.waiters` | `{caller}` | `domain` | Callers blocked on the applier, sampled on the position heartbeat from the waiters themselves. It is the depth of the queue a slow apply is making, and it goes on counting while the applier is stopped — which is when callers pile up. |
| `crewlet.statelog.log.bytes` | `By` | `domain` | What the log actually holds, against its ceiling below. |
| `crewlet.statelog.log.max_bytes` | `By` | `domain` | The ceiling, read from the running stream rather than from this node's own configuration — the two differ, and the running one is what refuses the append. |
| `crewlet.statelog.log.headroom_fraction` | `1` | `domain` | How much of the ceiling is left. A full log refuses every write AND every linearizable read, and the remedy is a fleet-wide maintenance cycle, so this is the one number worth alarming on long before it is small. |
| `crewlet.statelog.trim.blocked_seconds` | `s` | `domain`, `term` | How long one retention term has held the trim, named. A trim blocked for weeks is a log walking toward its ceiling with a cause an operator can act on. |
| `crewlet.backup.age` | `s` | — | How long ago the newest COMPLETE backup finished, read from the manifests on disk rather than from a counter this process keeps. A counter records that a process believed it took a backup; the disk records that one exists, and they differ in exactly the cases the alarm is for — a copy deleted, a volume never mounted, a schedule pointing at a path nobody ships from. |
| `crewlet.backup.holds` | `{hold}` | — | Live trim holds. A pin that outlives its owner stops the trim until the stale bound expires it, so a count that does not return to zero is a backup that crashed mid-copy. |
| `crewlet.store.wal.bytes` | `By` | `file` | A write-ahead log a checkpoint cannot pass grows, and this is the only way to see it before the volume fills. |
| `crewlet.store.bytes` | `By` | `file` | The store's size on disk, which the snapshot's free-space precondition and the provisioning rule are both derived from. |
| `crewlet.tracker.search.concurrency` | `{scan}` | — | Scans in flight, which is the row of the supported-corpus table this node is actually on. The published figure is a single reader on an idle node. |
| `crewlet.tracker.vector.coverage` | `1` | — | The fraction of sources carrying a current vector. It is how a stalled embedding backlog is reported, since it never drops a seat. |
| `crewlet.alarm.active` | `1` | `kind` | Whether each named alarm is firing right now, 0 or 1. It is the same table the operator record renders and the CLI exits non-zero on, so a collector and a person see one answer. |

## Counters

Only ever rises. Rates and totals are your collector's arithmetic, never this engine's.

| Metric | Unit | Attributes | What it makes visible |
|---|---|---|---|
| `crewlet.statelog.publish.outcomes` | `{write}` | `domain`, `outcome` | The three-valued write outcome, counted, and only the three — a refusal is on `publish.refusals` instead, because it says the write never happened at all. `unknown` is the one that matters most: without this count a broker flapping into ambiguity is visible only to the model that received the answer. |
| `crewlet.statelog.publish.conflicts` | `{write}` | `domain`, `subject_kind` | Writes that spent their whole round budget losing races on one subject, BY KIND. The refusal counter beside it says a conflict happened and not what it was about, and the remedy differs entirely: one contended object is a design question and a contended kind is a hot subject. |
| `crewlet.statelog.publish.rejections` | `{rejection}` | `domain`, `subject_kind` | How often a write loses a race, per kind of subject. It is what says whether a counter, a rank order or an ordinary object is the contended one. |
| `crewlet.statelog.publish.refusals` | `{write}` | `domain`, `reason` | Writes refused before or instead of an append, by the refusal's own reason — `evicted`, `deferred`, `behind`, `log_full`, `too_large` and `refused` among them — with `conflict` for a write that lost every round, `exists` for a create over an object that is there, and `error` for a failure that is no refusal at all. A refusal is not one of the three outcomes: it says the write never happened, and each reason has its own remedy, which one counter with an outcome dimension would hide. |
| `crewlet.statelog.read.refusals` | `{read}` | `domain`, `level`, `code` | Every refusal code, counted, which is each code's own rejection rate. The codes have different remedies, so a rate folded across them would say reads are failing and not what to do about it. |
| `crewlet.statelog.read.served` | `{read}` | `domain`, `level` | Reads answered per level, which is the denominator every refusal fraction needs and the check on the assumed read rate the log's own size is derived from. |
| `crewlet.statelog.barrier.appends` | `{barrier}` | `domain` | Barrier records appended. Against reads served it is the single-flight ratio, which says whether coalescing is doing anything at all. |
| `crewlet.statelog.linger.yields` | `{yield}` | `domain` | How often a waiter cut a batch short. It is the batching the applier gives up to answer a read promptly, and without it that trade is invisible. |
| `crewlet.statelog.apply.records` | `{record}` | `domain`, `result` | Records consumed, by what happened to them: applied, retained, gated, skipped, or reprocessed by a build that could read what an earlier one retained. A node applying nothing while its position advances is healthy on lag alone. |
| `crewlet.statelog.apply.retries` | `{attempt}` | `domain` | Attempts the apply loop retried in place after a failure that was not a stop — a fetch the broker did not answer, a transaction the disk refused, an applier that errored. A rate that stays up past the retry budget is a node whose rows have stopped moving, and its health says so. |
| `crewlet.statelog.apply.tx.aborts` | `{attempt}` | `domain` | Apply attempts the store rolled back and ran again, because the attempt failed transiently after it began. It reads zero by construction: this database detects write conflicts per file, so every write transaction takes the file's lock at its BEGIN and queues for it, and no commit elsewhere in the file can abort an apply. A non-zero count on the operator's own hardware means the retry budget is being spent rather than held in reserve. |
| `crewlet.statelog.records_gated` | `{record}` | `gate`, `subject_kind` | Records an apply gate dropped. A dropped commit is recoverable by nothing, and this is the only place anyone would see that it happened. |
| `crewlet.tracker.search.answers` | `{answer}` | `coverage`, `semantic` | What each answer actually covered: whether every bucket of the corpus was scanned, and whether the semantic half ran. Both alarms below it are a FRACTION of this counter, and a short answer is indistinguishable from a short corpus without it. |
| `crewlet.tracker.feed.unreadable` | `{record}` | `source` | Change records this build could not translate into a wake. Both domain consumers are deliberately uncapped, so such a record is never dropped — it redelivers for ever at the head of the consumer with every wake behind it waiting, which has no other symptom at all. |
| `crewlet.tracker.bulk.calls` | `{call}` | `result` | How often a bulk edit is issued, and how often one is refused because another is applying: the rate at which the one-bulk-at-a-time rule actually turns a caller away. |
| `crewlet.tracker.bulk.apply_seconds` | `s` | — | Seconds of applier occupancy bulk edits projected, summed. Over 24 hours it IS the fleet-wide read-degradation budget: every second here is a second in which reads are behind and writes are pending on every node. |

## What is deliberately not here

**Rates, bar the applier's own.** Every counter is a monotonic total and your
collector divides: a rate computed in this process would be a rate over a
window nobody chose, disagreeing with the one on your dashboard. The two
`crewlet.statelog.drain.*` gauges are the exception, because the engine needs
the rate itself: it divides a record backlog by the records-a-second drain to
state that backlog as a time — against a stale read's staleness bound, in a
refused read's retry hint and in a bulk edit's projection — and the commits a
second beside it is measured over the same apply runs. Both are smoothed across
those runs rather than taken over a window.

**A `/metrics` route.** OTLP reaches Prometheus through the collector you
already run for traces. A second wire format would be a second thing to
authenticate on an API whose guard and whose exemptions are both load-bearing.

**Per-object series.** See the attribute rule above.
