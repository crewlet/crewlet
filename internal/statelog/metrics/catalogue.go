// Package metrics is the engine's instrument catalogue and the ONE recorder
// every measurement is fed through.
//
// # Why this package exists
//
// The estate it replaces had forty-odd numbers on one operator record, polled
// once a minute at a stale read level, and nine benchmarks that run on a
// developer's machine. Not one latency was measured in production. A broker
// whose fsync drifted from 1 ms to 40 ms, an applier whose drain halved, a
// barrier that started spending half its read budget: each of them showed up
// as nothing at all until reads began refusing, and then as a refusal with no
// number behind it.
//
// A number on a page is not a metric. What makes one is that an operator can
// draw it over time and alarm on it, and that requires it to leave the
// process.
//
// # One recorder, two readers
//
// Everything records HERE — through [Recorder] — and exactly two things read
// it: the OpenTelemetry instruments, and the rolling 24-hour window the
// operator record renders. That is deliberate and it is the lesson
// internal/tokens records for the token rollup: that aggregation had three
// copies once, and a refresh routinely disagreed with the page it replaced. A
// number on a dashboard and a number on a collector's panel come from one
// atomic add.
//
// # The catalogue is written once
//
// [Catalogue] returns every instrument with its kind, its unit, its
// attributes and the sentence that says which failure it makes visible.
// docs/reference/metrics.md is generated from it and a test regenerates and
// diffs, exactly as schema/ is — because a metric whose meaning lives only in
// the code is one an operator cannot act on, and a doc maintained by hand is
// one that stops matching.
package metrics

import "fmt"

// Kind is what an instrument measures.
type Kind string

const (
	// KindCounter only ever rises. Rates and totals are the collector's
	// arithmetic, never this process's.
	KindCounter Kind = "counter"

	// KindGauge is a value that goes both ways: a queue depth, a lag, a
	// fraction.
	KindGauge Kind = "gauge"

	// KindHistogram is a distribution. Every latency here is one, because
	// a mean latency answers no operational question — the tail is what a
	// budget is spent by.
	KindHistogram Kind = "histogram"
)

// Unit is UCUM, which is what a collector expects.
const (
	UnitMilliseconds = "ms"
	UnitBytes        = "By"
	UnitSeconds      = "s"
	UnitCount        = "1"
)

// Instrument is one entry in the catalogue.
type Instrument struct {
	// Name is the OTel metric name: crewlet.<subsystem>.<noun>.<verb>.
	Name string

	Kind Kind
	Unit string

	// Attributes are the dimensions, and every one of them is a CLOSED SET.
	// Cardinality is bounded by construction here rather than by
	// convention: a subject id or a task key as an attribute is a time
	// series per object, which is how a metrics backend falls over.
	Attributes []string

	// Shows is the failure this instrument makes visible. It is the reason
	// the instrument exists, and an entry that cannot fill it in is an
	// instrument nobody will alarm on.
	Shows string
}

// Catalogue is every instrument this engine records, in name order.
//
// ORDERED, so the generated reference is stable and a diff is a change rather
// than a shuffle.
func Catalogue() []Instrument {
	return []Instrument{
		// ---- the write path -------------------------------------------
		{
			Name: StatelogPublishDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "outcome"},
			Shows: "A write path slowing down before it starts refusing. The " +
				"outcome dimension separates the three answers a write has, " +
				"so a rise in `pending` reads as an applier falling behind " +
				"rather than as a broker getting slower.",
		},
		{
			Name: StatelogPublishRounds, Kind: KindHistogram, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Contention on one subject, which the compare-and-set round " +
				"cap bounds and nothing measured. A distribution creeping " +
				"toward the cap is a hot object; reaching it is a refusal a " +
				"model reads as a colleague editing the same thing.",
		},
		{
			Name: StatelogPublishOutcomes, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "outcome"},
			Shows: "The three-valued write outcome, counted, and only the " +
				"three — a refusal is on `publish.refusals` instead, because " +
				"it says the write never happened at all. `unknown` is the " +
				"one that matters most and had no counter: a broker flapping " +
				"into ambiguity was visible only to the model that received " +
				"the answer.",
		},
		{
			Name: StatelogPublishConflicts, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "subject_kind"},
			Shows: "Writes that spent their whole round budget losing races " +
				"on one subject, BY KIND. The refusal counter beside it " +
				"says a conflict happened and not what it was about, and " +
				"the remedy differs entirely: one contended object is a " +
				"design question and a contended kind is a hot subject.",
		},
		{
			Name: StatelogPublishRejections, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "subject_kind"},
			Shows: "How often a write loses a race, per kind of subject. It is " +
				"what says whether a counter, a rank order or an ordinary " +
				"object is the contended one.",
		},
		{
			Name: StatelogPublishRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "reason"},
			Shows: "Writes refused before or instead of an append, by `reason`, and " +
				"EVERY value it carries is here, each with its remedy. The node: " +
				"`evicted` (this node is removed from the fleet — run the write " +
				"on another), `deferred` (it holds a record it cannot decode — a " +
				"newer build serves it), `behind` (it has not applied a position " +
				"the write needs — clears on its own), `below_floor` (it is below " +
				"the log and must adopt a snapshot — another node writes " +
				"meanwhile), `floor_unknown` (the floor or the log's ends could " +
				"not be read — clears when coordination answers). The log: " +
				"`log_full` (at its ceiling — raise it or unblock the trim), " +
				"`skew` (a store and a stream restored out of step — restore " +
				"both from one backup), `wrong_stream` (not the log this node's " +
				"rows came from: rebuilt, ending below its checkpoint, holding " +
				"another record there, or re-anchored past it by a peer — " +
				"re-anchor, or adopt where a peer holds the generation), " +
				"`log_truncated` (it lost records a peer's rows hold — settle " +
				"which history the fleet keeps). The object: `deleted` (a " +
				"permanent deletion marker — nothing recreates it). The " +
				"operation: `op_reused` (its id names a record on another " +
				"object) and `superseded` (its record landed and a later one " +
				"undid it) — both answered by a NEW operation under a fresh " +
				"id, never by a retry. A record that landed and a gate dropped " +
				"is counted under the gate that dropped it — `evicted`, " +
				"`deleted`, `abandoned` (written in a generation a reanchor abandoned) or " +
				"`overtaken` (written after a restored reanchor, below its " +
				"generation) — and is never re-decided, because republishing " +
				"makes another record nothing applies. Such a record holds its " +
				"operation id for the log's duplicate window, so the same id sent " +
				"inside it, by any node, is collapsed onto the record and refused " +
				"the same way: the write is retried under a fresh id, or under the " +
				"same one once the window has passed. Beside the refusals: " +
				"`conflict` for a write that lost every round, `exists` for a " +
				"create whose object already exists, and `error` for a write " +
				"that failed before it could answer. A refusal is not one of " +
				"the three outcomes: it says the write never happened, and each " +
				"reason has a different remedy, so one counter with an outcome " +
				"dimension would hide them all.",
		},
		{
			Name: StatelogWriteSessionWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain"},
			Shows: "The wait a write pays for its own previous write to apply. " +
				"Zero when the caller is caught up, which is the common case, " +
				"and the shape of the load that is not.",
		},

		// ---- the read path --------------------------------------------
		{
			Name: StatelogBarrierDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain"},
			Shows: "The broker round trip under every linearizable read, and " +
				"the first number a drifting fsync or a degrading quorum " +
				"moves. It was a benchmark's p50 on an idle loopback cluster " +
				"and nothing in production.",
		},
		{
			Name: StatelogReadWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "level"},
			Shows: "How much of the read budget a barrier or session wait " +
				"actually spends. A p95 approaching the budget is reads about " +
				"to start refusing, which is the warning the refusal itself " +
				"is too late to be.",
		},
		{
			Name: StatelogReadRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "level", "code"},
			Shows: "Every refusal code, counted. The codes, each with its own " +
				"remedy, had no counter between them, so an operator had no " +
				"rejection rate for any of them.",
		},
		{
			Name: StatelogReadServed, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "level"},
			Shows: "Reads answered per level, which is the denominator every " +
				"refusal fraction needs and the check on the assumed read " +
				"rate the log's own size is derived from.",
		},
		{
			Name: StatelogBarrierAppends, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Barrier records appended. Against reads served it is the " +
				"single-flight ratio, which says whether coalescing is doing " +
				"anything at all.",
		},
		{
			Name: StatelogBarriersApplied, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "stream"},
			Shows: "Barrier records applied from each log — every node's " +
				"linearizable reads on it, since every node applies every " +
				"record. It is the rate `census_drift` holds against the " +
				"census, per log: a node's own appends are only its share of " +
				"the fleet's reads, and on a fleet of several serving nodes " +
				"they describe a company that many times quieter than it is. " +
				"The alarm's 24-hour window files each barrier at the hour the " +
				"broker stored it, so a node replaying a backlog does not read " +
				"days of reads as one; this cumulative series counts them as " +
				"they are applied, so it jumps by a backlog's barriers when " +
				"one is replayed.",
		},
		{
			Name: StatelogLingerYields, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "How often a waiter cut a batch short. It is the batching " +
				"the applier gives up to answer a read promptly, and without " +
				"it that trade is invisible.",
		},

		// ---- the applier ----------------------------------------------
		{
			Name: StatelogApplyLatency, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain"},
			Shows: "THE COMMIT-TO-APPLY GAP: from the broker's own timestamp " +
				"on a record to this node committing it. Every read level is " +
				"a policy about this quantity and nothing measured it.",
		},
		{
			Name: StatelogApplyTxDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "bound_by"},
			Shows: "How long one apply transaction holds the store's writer, " +
				"and which budget ended it. A transaction is what every " +
				"waiter behind it pays, and rows were only ever a proxy for " +
				"the duration.",
		},
		{
			Name: StatelogApplyRecordDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "kind"},
			Shows: "One record's apply. A single record past the time budget " +
				"is still one transaction, so this is the real ceiling on how " +
				"long a read can be delayed — a sentence in a design document " +
				"until it was measured.",
		},
		{
			Name: StatelogApplyBatchRows, Kind: KindHistogram, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Rows per apply transaction, which is what the row budget " +
				"bounds and what the drain rate divides.",
		},
		{
			Name: StatelogApplyRecords, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "result"},
			Shows: "Records consumed, by what happened to them: applied, " +
				"retained, gated, skipped, or reprocessed by a build that could " +
				"read what an earlier one retained — a retained record a gate " +
				"drops when it is reprocessed is counted `gated`, as it would " +
				"have been live, since it wrote no row. A node applying nothing " +
				"while its position advances is healthy on lag alone.",
		},
		{
			Name: StatelogApplyRetries, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Attempts the apply loop retried in place after a failure " +
				"that was not a stop — a fetch the broker did not answer, a " +
				"transaction the disk refused, an applier that errored. A rate " +
				"that stays up past the retry budget is a node whose rows have " +
				"stopped moving, and its health says so.",
		},
		{
			Name: StatelogApplyTxAborts, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Apply transactions whose body the store ran more than " +
				"once, because an attempt failed transiently after it began. " +
				"It reads zero by construction: this database detects write " +
				"conflicts per file, so every write transaction takes the " +
				"file's lock at its BEGIN and queues for it, and no commit " +
				"elsewhere in the file can abort an apply. A non-zero count " +
				"on the operator's own hardware means the retry budget is " +
				"being spent rather than held in reserve.",
			// AND IT IS THE ONLY RETRY THE APPLIER HAS. A catalogued
			// `apply.retries` sat beside this one, declared as
			// "transient apply failures retried in place" — a
			// mechanism that does not exist: [statelog.Runner.Run]
			// returns on any error and the applier stays stopped
			// until a build that can read the record runs. The
			// transaction abort above IS the retry in place, so two
			// instruments named one phenomenon and nothing recorded
			// the vaguer of them. It was removed rather than given a
			// call site, because inventing a second count for one
			// event is the drift the single catalogue exists to
			// prevent.
		},
		{
			Name: StatelogRecordsGated, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"gate", "subject_kind"},
			Shows: "Records an apply gate dropped, by the gate that dropped " +
				"each — the domain's own (`evicted`, `deleted`) or " +
				"the framework's (`abandoned`, `overtaken`). " +
				"Each node counts a record once, where its own applier drops it, " +
				"when the transaction that drops it commits — never once per " +
				"attempt at that transaction. A write refused because its " +
				"record — or another node's copy it was collapsed onto — was " +
				"dropped is a refusal, counted under " +
				"`crewlet.statelog.publish.refusals`, and not a second drop. " +
				"A dropped commit is recoverable by nothing, and this is the " +
				"only place anyone would see that it happened.",
		},
		{
			Name: StatelogDrainRowsPerSecond, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "The applier's observed drain, which every retry hint " +
				"divides by. Seeded from a benchmark and then measured, so a " +
				"hint on real hardware stops being an extrapolation from " +
				"somebody else's.",
		},
		{
			Name: StatelogDrainCommitsPerSecond, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Commits per second, which is the fsync rate under " +
				"`synchronous = FULL` and the number a device budget is " +
				"spent by.",
		},

		// ---- position and health --------------------------------------
		{
			Name: StatelogApplyLagSeq, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows:      "How many records this node is behind the log's head.",
		},
		{
			Name: StatelogApplyLagSeconds, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain"},
			Shows: "How OLD the oldest unapplied record is. Seconds are what " +
				"a stall grace, a pending outcome and a copy taken out of " +
				"service all turn on; sequences are not, and a lag of 4 000 " +
				"says nothing about whether anything is wrong.",
		},
		{
			Name: StatelogAppliedThrough, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "The prefix this node has actually applied, which is lower " +
				"than its checkpoint whenever a record was retained.",
		},
		{
			Name: StatelogDeferredCount, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Records this build could not read and kept. Non-zero is a " +
				"rolling upgrade in progress; non-zero and not falling is one " +
				"that stopped.",
		},
		{
			Name: StatelogDeferredOldestAgeSeconds, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain"},
			Shows: "How long the oldest retained record has been retained, " +
				"which is what decides whether this node's copy stays in service.",
		},
		{
			Name: StatelogWaiters, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Callers blocked on the applier right now. It is the depth " +
				"of the queue a slow apply is making.",
		},

		// ---- retention and capacity -----------------------------------
		{
			Name: StatelogLogBytes, Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"domain"},
			Shows:      "What the log actually holds, against its ceiling below.",
		},
		{
			Name: StatelogLogMaxBytes, Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"domain"},
			Shows: "The ceiling, read from the running stream rather than " +
				"from this node's own configuration — the two differ, and the " +
				"running one is what refuses the append.",
		},
		{
			Name: StatelogLogHeadroomFraction, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "How much of the ceiling ordinary writes are held to is " +
				"left — on a log that claims identity, the ceiling less the " +
				"gate reserve kept above it for evictions. At zero the log " +
				"refuses every write AND every linearizable read, and the " +
				"remedy is a fleet-wide maintenance cycle, so this is the one " +
				"number worth alarming on long before it is small.",
		},
		{
			Name: StatelogTrimBlockedSeconds, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain", "term"},
			Shows: "How long one retention term has held the trim, named. A " +
				"trim blocked for weeks is a log walking toward its ceiling " +
				"with a cause an operator can act on.",
		},
		{
			Name: StatelogAnchorsSwept, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Arbitration anchors this node's applier removed under the " +
				"trim's `trimmed` records (ADR-0032), counted once the " +
				"transaction that removed them commits. Every node applies " +
				"every record, so every data node counts the same removals. " +
				"Flat is the normal reading of a log whose purged objects all " +
				"wrote again later — their anchors moved above the trim and " +
				"nothing is below it to remove; a sweep that is failing shows " +
				"as a `retention_anchor_sweep_failed` line from the trim's " +
				"holder, and as anchors below the log's first record that keep " +
				"growing while this stays flat.",
		},

		// ---- the backup -----------------------------------------------
		{
			Name: BackupAge, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: nil,
			Shows: "How long ago the newest COMPLETE backup finished, read " +
				"from the manifests on disk rather than from a counter this " +
				"process keeps. A counter records that a process believed it " +
				"took a backup; the disk records that one exists, and they " +
				"differ in exactly the cases the alarm is for — a copy " +
				"deleted, a volume never mounted, a schedule pointing at a " +
				"path nobody ships from. A node holding no manifest reports " +
				"NO SERIES rather than a value: zero is the freshest backup " +
				"imaginable and every other number is an age nobody " +
				"measured, so neither can stand for an absence. The absence " +
				"is what the `backup_age` alarm says in words, and an " +
				"`absent()` rule over this gauge is what a collector alerts " +
				"on.",
		},
		{
			Name: BackupDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: nil,
			Shows: "How long a backup took, which is the window the trim " +
				"hold covers and the I/O the copy spends competing with the " +
				"applier's own commits. It is what turns the retention " +
				"guide's worked example into a number for THIS hardware.",
		},
		{
			Name: BackupHolds, Kind: KindGauge, Unit: UnitCount,
			Attributes: nil,
			Shows: "Live trim holds. A pin that outlives its owner stops the " +
				"trim until the stale bound expires it, so a count that does " +
				"not return to zero is a backup that crashed mid-copy.",
		},

		// ---- the fleet's history ---------------------------------------
		{
			Name: HistoryAnswers, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"question", "coverage"},
			Shows: "What each fleet history read covered: `complete` when " +
				"every live node answered inside the fleet read budget, " +
				"`partial` when one did not. Turn-level detail lives only " +
				"on the node that published it, so a partial answer is " +
				"missing that node's rows — and the history_partial alarm " +
				"is a fraction of this counter.",
		},

		// ---- the store ------------------------------------------------
		{
			Name: StorePoolWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"file"},
			Shows: "How long a reader waited for a connection. It is what " +
				"says the reader pool is too small on this node, which " +
				"nothing could say before.",
		},
		{
			Name: StoreWalBytes, Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"file"},
			Shows: "A write-ahead log a checkpoint cannot pass grows, and " +
				"this is the only way to see it before the volume fills.",
		},
		{
			Name: StoreBytes, Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"file"},
			Shows: "The store's size on disk, which the snapshot's free-space " +
				"precondition and the provisioning rule are both derived from.",
		},

		// ---- search ---------------------------------------------------
		{
			Name: TrackerSearchScanDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"path", "rung"},
			Shows: "One search's scan, split by whether it ran for a turn's " +
				"prefetch or for somebody's deliberate search, and by the " +
				"ranking it actually served (`rung`: hybrid, keyword, " +
				"semantic, or none). Only the prefetch had a published " +
				"percentile, and the interactive path is the one with a " +
				"target; a keyword scan is a fraction of a fused one's cost, " +
				"so reading them as one series hides a slow meaning half.",
		},
		{
			Name: TrackerSearchConcurrency, Kind: KindGauge, Unit: UnitCount,
			Attributes: nil,
			Shows: "Searches in flight, which is the row of the " +
				"supported-corpus table this node is actually on: inside the " +
				"one-second budget, about 345 000 sources with one running " +
				"and about 136 000 with eight through the full scan, and " +
				"about 545 000 and 183 000 through an index probing half " +
				"its lists.",
		},
		{
			Name: TrackerSearchAnswers, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"coverage", "semantic"},
			Shows: "What each answer actually covered: whether every bucket " +
				"of the corpus was scanned, and whether the semantic half " +
				"ran (`full`), was asked for and lost (`skipped`), or was " +
				"never asked for (`off` — a keyword search, or a company " +
				"with no embeddings). Both alarms below it are a FRACTION " +
				"of this counter, and a short answer is indistinguishable " +
				"from a short corpus without it.",
		},
		{
			Name: TrackerVectorCoverage, Kind: KindGauge, Unit: UnitCount,
			Attributes: nil,
			Shows: "The fraction of sources carrying a current vector. It is " +
				"how a stalled embedding backlog is reported, since it never " +
				"drops a seat.",
		},

		// ---- the change feed ------------------------------------------
		{
			Name: TrackerFeedUnreadable, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"source"},
			Shows: "Change records this build could not translate into a " +
				"wake. Both domain consumers are deliberately uncapped, so " +
				"such a record is never dropped — it redelivers for ever at " +
				"the head of the consumer with every wake behind it " +
				"waiting, which has no other symptom at all.",
		},

		// ---- bulk -----------------------------------------------------
		{
			Name: TrackerBulkCalls, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"result"},
			Shows: "How often a bulk edit is issued, by `result`: " +
				"`admitted` under the fleet's bulk lease, `refused` because " +
				"another is applying, and `fail_open` — let through with NO " +
				"lease, because the coordination store did not answer the " +
				"admission or the mixed-version gate refused it. The refusal " +
				"arithmetic rested on an assumed ten a day, a number with " +
				"no counter behind it; this is that number. A `fail_open` " +
				"is a bulk the fleet-wide bound did not cover, and counted " +
				"as `admitted` it left no trace: a store that refused every " +
				"admission looked exactly like a fleet whose bulks never " +
				"collided.",
		},
		{
			Name: TrackerBulkApplySeconds, Kind: KindCounter, Unit: UnitSeconds,
			Attributes: nil,
			Shows: "Seconds of applier occupancy bulk edits projected, " +
				"summed. Over 24 hours it IS the fleet-wide read-degradation " +
				"budget: every second here is a second in which reads are " +
				"behind and writes are pending on every node.",
		},
		// ---- code -----------------------------------------------------
		{
			Name: CodeCommitBatchSize, Kind: KindHistogram, Unit: UnitCount,
			Shows: "Pushes per code record: group commit working, or not. The " +
				"committer takes everything already queued behind an in-flight " +
				"record — without lingering for more — so an idle repository's " +
				"records stay at one push and a busy one's grow, up to 64. A busy " +
				"repository whose batches stay at one is one whose pushes arrive " +
				"more slowly than a record commits.",
		},
		{
			Name: CodeCommitBusy, Kind: KindCounter, Unit: UnitCount,
			Shows: "Pushes refused `busy`, undecided — the client runs `git " +
				"push` again — for one of three causes that point at different " +
				"remedies. A BACKLOG: a repository taking pushes faster than one " +
				"node's committer commits them (its queue of 1 024 pushes, or " +
				"sixteen records' bytes, is full), or the node holding sixty-four " +
				"records' bytes of pushes across its repositories. A DEADLINE, " +
				"either way round: a push that waited behind earlier ones until " +
				"its asker's attempt had no more left than the 30-second commit " +
				"budget and a second for the answer, or one that ARRIVED with no " +
				"more than that left — the attempt spent before it reached the " +
				"committer, waiting for the node to take it (its proposal slots " +
				"full), for the node to reach the asker's own writes, or under a " +
				"caller deadline shorter than 31 seconds; asked with no deadline, " +
				"a push that waited that budget whole. Or a record that lost " +
				"every compare-and-set round it may take. No repository is an " +
				"attribute — which one is hot is a question for its rows, not a " +
				"time series a repository.",
		},
		// ---- git ------------------------------------------------------
		{
			Name: CodeCacheAdoptions, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Pushes this node's git cache was asked to adopt — the " +
				"pack the push's receive-pack wrote, linked into the cache " +
				"once its row applied rather than downloaded back by the " +
				"node that took the push — by `outcome`. `adopted`: linked " +
				"in by the adoption itself. `handed`: a sync or a verify " +
				"was writing the repository's directory, and the adoption " +
				"left the pack to it, staged; it installs the pack from " +
				"that staging rather than the object store before it lets " +
				"the directory go, unless it had already begun to download " +
				"it. `held`: the cache already held the pack. `uncached`: " +
				"the repository's cache left this node between the push's " +
				"sync and its adoption — the repository purged — so there " +
				"was nothing to adopt into. `failed`: the links failed, and " +
				"the next sync downloads the pack, one GET of its bundle; " +
				"rising, it is this node's disk refusing writes.",
		},
		{
			Name: CodeCacheBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "What this node's git cache holds on disk — every " +
				"repository's packs, indexes and refs, plus the downloads and " +
				"pushes in flight — against its byte budget, which it is " +
				"evicted to, least recently used first, and for " +
				"no other reason. Sitting at the budget is a cache doing its " +
				"job; held above it is a repository larger than the budget " +
				"being served, evicted once nothing reads it; climbing past " +
				"it while syncs keep starting cold is a budget too small for " +
				"the repositories this node serves.",
		},
		{
			Name: CodeCacheMaintenanceRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason", "step"},
			Shows: "Steps of the background work on a repository this node's " +
				"git cache found it will not run, counted once for each copy " +
				"of a repository the cache holds and each step, by `reason` " +
				"and `step`. `memory`: the step's charge — its own figure an " +
				"object of the repository, and 240 more for room beside it " +
				"for a clone of the repository — is more than this node can " +
				"give its background work, which is its memory (its cgroup's " +
				"limit where one is set) less what it keeps for its git " +
				"lanes, the store, the broker and the engine. `bitmap` (1000 " +
				"bytes an object): the repository is indexed without one, and " +
				"a clone walks its history; `index` (80): it is served from " +
				"its packs' own indexes, a round's index included; `graph` " +
				"(56): its walks parse every commit. Slower, never wrong. " +
				"Rising is repositories outgrowing the node; the " +
				"code_maintenance_refused alarm stands while any bitmap is " +
				"refused.",
		},
		{
			Name: CodeCacheMaintenanceRefused, Kind: KindGauge, Unit: UnitCount,
			Shows: "Repositories this node's git cache cannot write a bitmap " +
				"of now, because one needs more memory than the node can give " +
				"its background work — as its latest pass found them; each is " +
				"indexed without one. Above zero raises " +
				"code_maintenance_refused; falling back is a repository " +
				"purged, evicted or rebuilt smaller, or a node given more " +
				"memory, which the cache reads again every ten seconds.",
		},
		{
			Name: CodeCacheRebuildRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason", "rule"},
			Shows: "Rebuilds this node's git cache refused, by the `reason` " +
				"the git process gave (`corrupt` or `missing`) and the " +
				"`rule` that refused it. `unchanged`: the repository's " +
				"cache holds exactly what a rebuild that did not end the " +
				"failure read less than a day ago, and nothing has written " +
				"into it since, so another would make the same directory; " +
				"`cooldown`: the last attempt began too recently — ten " +
				"minutes after the first of a line of attempts that keep " +
				"failing, twice as long after each one after it, at most a " +
				"day, a line starting again once a day passes with no " +
				"rebuild asked for. Each refusal fails the request that asked " +
				"at once rather than downloading the whole repository again. " +
				"Rising at all is " +
				"damage no rebuild repairs — a pack the object store lost or " +
				"holds damaged, rows naming an object no pack holds — which " +
				"a verify of the repository names; flat while " +
				"`crewlet.code.cache.rebuilds` rises is damage each rebuild " +
				"ended.",
		},
		{
			Name: CodeCacheRebuilds, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Repositories this node's git cache read whole from the " +
				"rows again, by `reason`. `corrupt` and `missing` are a " +
				"rebuild a git process asked for — it found a pack or an " +
				"index it could not read, or an object a row names that no " +
				"pack held — into a new directory beside the damaged one; " +
				"either rising is disk or object-store damage, which a " +
				"verify of the repository names — every pack checked against " +
				"its row, the ref file against the digest written with it, " +
				"and git's connectivity check. `verify` is that check, asked " +
				"to repair what it " +
				"found, rebuilding the repository the same way. `stream` is " +
				"the code log " +
				"RECREATED under the cache (provisioned anew, its stamp not " +
				"the one the cache read under); `rewound` is the log gone " +
				"BACK under it on the same stream — a broker restored from " +
				"an older copy — seen in a read at the log's head below a " +
				"position the cache had read. Either of those counts every " +
				"repository on the node once, as its next sync converges to " +
				"the rows the log holds rather than taking a delta from a " +
				"position that log never had or has since given to other " +
				"records.",
		},
		{
			Name: CodeCacheRepos, Kind: KindGauge, Unit: UnitCount,
			Shows: "Repositories with a cache on this node. One leaves when " +
				"its rows are purged or the budget evicts it; every one that " +
				"arrives cost a cold sync of the whole repository. Falling " +
				"while the cache's bytes stay at the budget is eviction " +
				"churn: every repository it drops is a cold sync the next " +
				"time it is fetched.",
		},
		{
			Name: CodeCacheSyncDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"kind"},
			Shows: "How long a completed cache sync took, by `kind` (`delta`: " +
				"the rows moved since the cache's position; `full`: the whole " +
				"repository — a cold cache, the first sync of a repository " +
				"after a restart, a log recreated under it). A delta p95 " +
				"climbing is a node falling behind its pushes; a full sync's " +
				"time is mostly its downloads, sixteen at once.",
		},
		{
			Name: CodeGitWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"class"},
			Shows: "How long a git process waited for a slot on this node, by " +
				"`class` (`read`, `transfer`, `maintenance`). A p95 " +
				"approaching ten seconds is a node about to answer clones and pushes " +
				"with 503 — every slot of one lane busy for a whole read attempt. " +
				"`transfer` waiting is a node serving as many clones and pushes as " +
				"`git.max_transfers` admits (more git nodes, or more memory and a " +
				"wider `git.max_transfers`); `read` waiting is a node out of " +
				"`git.max_processes`, one core a slot.",
		},
		{
			Name: CodeGitCPU, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"class"},
			Shows: "CPU a git process tree spent, by lane: `transfer` is clones " +
				"and pushes, `read` the cache's reads and a push's scans and " +
				"fetch checks, `maintenance` the bitmaps, indexes and checks. " +
				"One observation per process, its whole cost, recorded when it " +
				"ends: its user and system time and that of every process it " +
				"waited for (upload-pack's pack-objects, receive-pack's " +
				"index-pack) — and for one the node ended (a deadline, a stall, " +
				"a client gone) at least what its whole group had spent when it " +
				"was signalled; on macOS, only what the process and the " +
				"children it had reaped spent. So the sum is what a lane's runs " +
				"cost, read over a window longer than the runs it counts — a " +
				"clone may run for hours and a bitmap for a quarter of an hour, " +
				"each landing whole in the window it ends in — and never the " +
				"share of the node git is taking now, which is the container's " +
				"own CPU usage. Read the sum and the count rather than the " +
				"percentiles: a run past 64 s of CPU lands in the last bin.",
		},
		{
			Name: CodeGitProcesses, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"class"},
			Shows: "Git processes running on this node, by `class`. `transfer` " +
				"pinned at `git.max_transfers` while `crewlet.code.git.wait` " +
				"climbs is a node serving as many clones and pushes as its memory " +
				"was sized for; `maintenance` is background index rebuilding, " +
				"at most half of `git.max_processes`.",
		},
		{
			Name: CodeGitLentCores, Kind: KindGauge, Unit: UnitCount,
			Shows: "Cores of this node's read lane lent right now to pushes as " +
				"extra index-pack threads — up to three a push, never the last " +
				"free core. A loan is made when a push's receive-pack starts — " +
				"the one that receives its pack, never the refs advertisement " +
				"its first request is answered with — and repaid once its pack " +
				"is indexed, so it covers the client's whole upload: " +
				"lent cores held high while `crewlet.code.git.processes` for " +
				"`read` sits low is slow uploads holding loans, and the reason " +
				"a later push on this node indexes on one core.",
		},
		// ---- the code host ---------------------------------------------
		{
			Name: CodePushDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long a git push took on this node, from its request to its " +
				"answer, by `outcome` (`applied`, `pending`, `unknown`, `refused`, " +
				"`failed`). `pending` growing is a code log whose records are " +
				"durable and slow to apply here; `unknown` is a committer that " +
				"stopped answering; the tail of `applied` is what a person waits " +
				"for a push.",
		},
		{
			Name: CodePushBytes, Kind: KindHistogram, Unit: UnitBytes,
			Shows: "The size of each push's bundle — the pack its receive-pack " +
				"kept and the two indexes it wrote — observed once the pack is " +
				"checked, whatever becomes of the push: the one object the object " +
				"store stores for it, against `code.limits.max_push_bytes` (what " +
				"the client may send) and the 1.25 GiB one push may store.",
		},
		{
			Name: CodeFetchDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"command", "sync"},
			Shows: "How long a fetch request took on this node, by `command` " +
				"(`advertise`, `ls-refs`, `fetch`) and by what the cache had to " +
				"do first (`sync`: `none`, `delta`, `full`). A `full` tail is cold " +
				"caches downloading whole repositories; a `delta` tail is the " +
				"barrier and the sync every answer that names a ref waits for.",
		},
		// ---- alarms ---------------------------------------------------
		{
			Name: AlarmActive, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"kind"},
			Shows: "Whether each named alarm is firing right now, 0 or 1. It " +
				"is the same table the operator record renders and the CLI " +
				"exits non-zero on, so a collector and a person see one " +
				"answer.",
		},
	}
}

// Validate reports why the catalogue is malformed.
//
// Run by the constructor rather than only by a test: an instrument with no
// name registers as an empty metric, and one with no reason is one nobody will
// alarm on — both are mistakes worth refusing at the point the recorder is
// built rather than discovering on a dashboard.
func Validate(entries []Instrument) error {
	seen := map[string]bool{}
	for _, e := range entries {
		switch {
		case e.Name == "":
			return fmt.Errorf("metrics: an instrument has no name")
		case seen[e.Name]:
			return fmt.Errorf("metrics: %q is declared twice: two instruments "+
				"under one name are one time series with two meanings", e.Name)
		case e.Kind == "":
			return fmt.Errorf("metrics: %q has no kind", e.Name)
		case e.Unit == "":
			return fmt.Errorf("metrics: %q has no unit: a number with no unit "+
				"is one a collector cannot convert and a reader cannot check",
				e.Name)
		case e.Shows == "":
			return fmt.Errorf("metrics: %q does not say which failure it makes "+
				"visible: an instrument nobody will alarm on is a number on a "+
				"page, which is what this catalogue replaces", e.Name)
		}
		seen[e.Name] = true
	}
	return nil
}
