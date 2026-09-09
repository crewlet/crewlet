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

A distribution, exported with the engine's own bucket boundaries: twenty-one powers of two from 64 µs to 64 s, which resolves a percentile to within a factor of two at every scale here — from a 40 µs index probe to a 16-second bulk apply. The SDK's default boundaries stop at 10 s, so a slow apply would land in an overflow bucket and read as "at least 10 s" for ever.

| Metric | Unit | Attributes | What it makes visible |
|---|---|---|---|
| `crewlet.statelog.publish.duration` | `ms` | `domain`, `outcome` | A write path slowing down before it starts refusing. The outcome dimension separates the three answers a write has, so a rise in `pending` reads as an applier falling behind rather than as a broker getting slower. |
| `crewlet.statelog.publish.rounds` | `1` | `domain` | Contention on one subject, which the compare-and-set round cap bounds and nothing measured. A distribution creeping toward the cap is a hot object; reaching it is a refusal a model reads as a colleague editing the same thing. |
| `crewlet.statelog.write.session_wait` | `ms` | `domain` | The wait a write pays for its own previous write to apply. Zero when the caller is caught up, which is the common case, and the shape of the load that is not. |
| `crewlet.statelog.barrier.duration` | `ms` | `domain` | The broker round trip under every linearizable read, and the first number a drifting fsync or a degrading quorum moves. It was a benchmark's p50 on an idle loopback cluster and nothing in production. |
| `crewlet.statelog.read.wait` | `ms` | `domain`, `level` | How much of the read budget a barrier or session wait actually spends. A p95 approaching the budget is reads about to start refusing, which is the warning the refusal itself is too late to be. |
| `crewlet.statelog.apply.latency` | `ms` | `domain` | THE COMMIT-TO-APPLY GAP: from the broker's own timestamp on a record to this node committing it. Every read level is a policy about this quantity and nothing measured it. |
| `crewlet.statelog.apply.tx.duration` | `ms` | `domain`, `bound_by` | How long one apply transaction holds the store's writer, and which budget ended it. A transaction is what every waiter behind it pays, and rows were only ever a proxy for the duration. |
| `crewlet.statelog.apply.record.duration` | `ms` | `domain`, `kind` | One record's apply. A single record past the time budget is still one transaction, so this is the real ceiling on how long a read can be delayed — a sentence in a design document until it was measured. |
| `crewlet.statelog.apply.batch.rows` | `1` | `domain` | Rows per apply transaction, which is what the row budget bounds and what the drain rate divides. |
| `crewlet.backup.duration` | `ms` | — | How long a backup took, which is the window the trim hold covers and the I/O the copy spends competing with the applier's own commits. It is what turns the retention guide's worked example into a number for THIS hardware. |
| `crewlet.store.pool.wait` | `ms` | `file` | How long a reader waited for a connection. It is what says the reader pool is too small on this node, which nothing could say before. |
| `crewlet.tracker.search.scan.duration` | `ms` | `path`, `rung` | The semantic scan, split by whether it ran for a turn's prefetch or for somebody's deliberate search. Only the prefetch had a published percentile, and the interactive path is the one with a target. |

## Gauges

A value that goes both ways, sampled at each export.

| Metric | Unit | Attributes | What it makes visible |
|---|---|---|---|
| `crewlet.statelog.drain.rows_per_second` | `1` | `domain` | The applier's observed drain, which every retry hint divides by. Seeded from a benchmark and then measured, so a hint on real hardware stops being an extrapolation from somebody else's. |
| `crewlet.statelog.drain.commits_per_second` | `1` | `domain` | Commits per second, which is the fsync rate under `synchronous = FULL` and the number a device budget is spent by. |
| `crewlet.statelog.apply.lag.seq` | `1` | `domain` | How many records this node is behind the log's head. |
| `crewlet.statelog.apply.lag.seconds` | `s` | `domain` | How OLD the oldest unapplied record is. Seconds are what a stall grace, a pending outcome and a seat move all turn on; sequences are not, and a lag of 4 000 says nothing about whether anything is wrong. |
| `crewlet.statelog.applied_through` | `1` | `domain` | The prefix this node has actually applied, which is lower than its checkpoint whenever a record was retained. |
| `crewlet.statelog.deferred.count` | `1` | `domain` | Records this build could not read and kept. Non-zero is a rolling upgrade in progress; non-zero and not falling is one that stopped. |
| `crewlet.statelog.deferred.oldest_age_seconds` | `s` | `domain` | How long the oldest retained record has been retained, which is what decides whether this node's seats move. |
| `crewlet.statelog.waiters` | `1` | `domain` | Callers blocked on the applier right now. It is the depth of the queue a slow apply is making. |
| `crewlet.statelog.log.bytes` | `By` | `domain` | What the log actually holds, against its ceiling below. |
| `crewlet.statelog.log.max_bytes` | `By` | `domain` | The ceiling, read from the running stream rather than from this node's own configuration — the two differ, and the running one is what refuses the append. |
| `crewlet.statelog.log.headroom_fraction` | `1` | `domain` | How much of the ceiling is left. A full log refuses every write AND every linearizable read, and the remedy is a fleet-wide maintenance cycle, so this is the one number worth alarming on long before it is small. |
| `crewlet.statelog.trim.blocked_seconds` | `s` | `domain`, `term` | How long one retention term has held the trim, named. A trim blocked for weeks is a log walking toward its ceiling with a cause an operator can act on. |
| `crewlet.backup.age` | `s` | — | How long ago the newest COMPLETE backup finished, read from the manifests on disk rather than from a counter this process keeps. A counter records that a process believed it took a backup; the disk records that one exists, and they differ in exactly the cases the alarm is for — a copy deleted, a volume never mounted, a schedule pointing at a path nobody ships from. |
| `crewlet.backup.holds` | `1` | — | Live trim holds. A pin that outlives its owner stops the trim until the stale bound expires it, so a count that does not return to zero is a backup that crashed mid-copy. |
| `crewlet.store.wal.bytes` | `By` | `file` | A write-ahead log a checkpoint cannot pass grows, and this is the only way to see it before the volume fills. |
| `crewlet.store.bytes` | `By` | `file` | The store's size on disk, which the snapshot's free-space precondition and the provisioning rule are both derived from. |
| `crewlet.tracker.search.concurrency` | `1` | — | Scans in flight, which is the row of the supported-corpus table this node is actually on. The published figure is a single reader on an idle node. |
| `crewlet.tracker.vector.coverage` | `1` | — | The fraction of sources carrying a current vector. It is how a stalled embedding backlog is reported, since it never drops a seat. |
| `crewlet.alarm.active` | `1` | `kind` | Whether each named alarm is firing right now, 0 or 1. It is the same table the operator record renders and the CLI exits non-zero on, so a collector and a person see one answer. |

## Counters

Only ever rises. Rates and totals are your collector's arithmetic, never this engine's.

| Metric | Unit | Attributes | What it makes visible |
|---|---|---|---|
| `crewlet.statelog.publish.outcomes` | `1` | `domain`, `outcome` | The three-valued write outcome, counted, and only the three — a refusal is on `publish.refusals` instead, because it says the write never happened at all. `unknown` is the one that matters most and had no counter: a broker flapping into ambiguity was visible only to the model that received the answer. |
| `crewlet.statelog.publish.conflicts` | `1` | `domain`, `subject_kind` | Writes that spent their whole round budget losing races on one subject, BY KIND. The refusal counter beside it says a conflict happened and not what it was about, and the remedy differs entirely: one contended object is a design question and a contended kind is a hot subject. |
| `crewlet.statelog.publish.rejections` | `1` | `domain`, `subject_kind` | How often a write loses a race, per kind of subject. It is what says whether a counter, a rank order or an ordinary object is the contended one. |
| `crewlet.statelog.publish.refusals` | `1` | `domain`, `reason` | Writes refused before or instead of an append, by reason — an evicted node, a deferred record covering the object, a caller waiting on its own previous write, a full log. A refusal is not one of the three outcomes: it says the write never happened, and each reason has a different remedy, so one counter with an outcome dimension would hide all four. |
| `crewlet.statelog.read.refusals` | `1` | `domain`, `level`, `code` | Every refusal code, counted. Twelve codes with different remedies had no counter between them, so an operator had no rejection rate for any of them. |
| `crewlet.statelog.read.served` | `1` | `domain`, `level` | Reads answered per level, which is the denominator every refusal fraction needs and the check on the assumed read rate the log's own size is derived from. |
| `crewlet.statelog.barrier.appends` | `1` | `domain` | Barrier records appended. Against reads served it is the single-flight ratio, which says whether coalescing is doing anything at all. |
| `crewlet.statelog.linger.yields` | `1` | `domain` | How often a waiter cut a batch short. It is the batching the applier gives up to answer a read promptly, and without it that trade is invisible. |
| `crewlet.statelog.apply.records` | `1` | `domain`, `result` | Records consumed, by what happened to them: applied, retained, gated or skipped. A node applying nothing while its position advances is healthy on lag alone. |
| `crewlet.statelog.apply.tx.aborts` | `1` | `domain` | Apply transactions the store aborted on a conflict and the loop retried. It is the number that says whether this driver's transaction conflicts are row-scoped or database-scoped, on the operator's own hardware rather than on a benchmark's — measured at zero against a writer committing to tables the applier never touches, so a non-zero count means the retry budget is being spent rather than held in reserve. |
| `crewlet.statelog.apply.retries` | `1` | `domain` | Transient apply failures retried in place, which are otherwise a silent backoff inside the loop. |
| `crewlet.statelog.records_gated` | `1` | `gate`, `subject_kind` | Records an apply gate dropped. A dropped commit is recoverable by nothing, and this is the only place anyone would see that it happened. |
| `crewlet.tracker.bulk.calls` | `1` | `result` | How often a bulk edit is issued and how often one is refused because another is applying. The refusal arithmetic rested on an assumed ten a day, a number with no counter behind it; this is that number. |
| `crewlet.tracker.bulk.apply_seconds` | `s` | — | Seconds of applier occupancy bulk edits projected, summed. Over 24 hours it IS the fleet-wide read-degradation budget: every second here is a second in which reads are behind and writes are pending on every node. |

## What is deliberately not here

**Rates.** Every counter is a monotonic total and your collector divides. A
rate computed in this process would be a rate over a window nobody chose,
disagreeing with the one on your dashboard.

**A `/metrics` route.** OTLP reaches Prometheus through the collector you
already run for traces. A second wire format would be a second thing to
authenticate on an API whose guard and whose exemptions are both load-bearing.

**Per-object series.** See the attribute rule above.
