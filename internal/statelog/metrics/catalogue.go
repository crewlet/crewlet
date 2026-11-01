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
	UnitBytesPerDay  = "By/d"
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
				"on another), `eviction_unknown` (its own eviction state could " +
				"not be read — clears when it is read again), `deferred` (it " +
				"holds a record it cannot decode — a " +
				"newer build serves it), `behind` (it has not applied a position " +
				"the write needs — clears on its own), `below_floor` (it is below " +
				"the log and must adopt a snapshot — another node writes " +
				"meanwhile), `floor_unknown` (the floor or the log's ends could " +
				"not be read — clears when coordination answers). The log: " +
				"`log_full` (at its ceiling — raise it or unblock the trim), " +
				"`record_too_large` (the record is larger than the broker takes " +
				"in one message — split the change; the detail names which " +
				"limit refused it), `broker_refused` (a refusal this build has " +
				"no remedy for — the detail carries the broker's own words), " +
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
				"linearizable reads on it, since every data node applies every " +
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
			Name: StatelogRecordsTampered, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Records whose signature failed under a key this node " +
				"holds, which means something that is not this fleet wrote " +
				"to the broker. The applier stops on the first one, so this " +
				"counts at most one per domain per stop, and any value above " +
				"zero is an incident rather than a rate.",
		},
		{
			Name: StatelogRecordsUnverifiable, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Records signed under a key this node does not hold, " +
				"retained until it does. Expected and transient during a " +
				"keyring rotation; sustained, it means a key reached one " +
				"node's secrets.keys and not this one's.",
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
			Name: StatelogLogBytesPerDay, Kind: KindGauge, Unit: UnitBytesPerDay,
			Attributes: []string{"domain"},
			Shows: "What the log took in over the trailing day, measured from " +
				"its own records at each trim tick. The ceiling has to hold " +
				"`min_age` of this, because the trim never removes a record " +
				"younger than that — a log whose ceiling is smaller fills with " +
				"every trim term satisfied, which is what `log_ceiling_short` " +
				"fires on. Absent for a compacted log and for one in its first " +
				"two days, whose trailing day would contain the day it was " +
				"imported into; a log that could not be read keeps its last " +
				"measurement, as the report does.",
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
				"left on every log that claims identity (Worklet's, Doclet's, " +
				"the identity estate's, the artifacts log's, the artifact " +
				"datastore's, Ask Crewlet's, Chatlet's and the executor " +
				"log): the ceiling " +
				"less the gate reserve kept above it for evictions. At zero the log " +
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

		// ---- the backup -----------------------------------------------
		{
			Name: BackupAge, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: nil,
			Shows: "How long ago the newest COMPLETE backup THIS NODE took " +
				"finished, read from the manifests in the directories it " +
				"announced rather than from the fleet's register. The " +
				"register records that a copy was announced; the disk " +
				"records that it still exists, and they differ in exactly " +
				"the cases worth catching — a copy deleted, a volume never " +
				"mounted, a schedule pointing at a path nobody ships from. A " +
				"node holding no manifest of its own reports NO SERIES " +
				"rather than a value: zero is the freshest backup imaginable " +
				"and every other number is an age nobody measured, so " +
				"neither can stand for an absence, and an `absent()` rule " +
				"over this gauge is what a collector alerts on. It is NOT " +
				"what the `backup_age` alarm reads: the alarm ages, per log " +
				"and fleet-wide, the newest verified point the trim counts " +
				"in the register (under `backup_floor: operator`, the " +
				"operator's acknowledgement of that log), so the two answer " +
				"different questions and can disagree — a copy deleted " +
				"after it was announced still counts for the alarm and the " +
				"trim, and leaves this gauge.",
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
			Shows: "Trim holds in the register: a backup copying the log, or " +
				"a joining node fetching a snapshot and replaying its tail. A " +
				"holder renews its pin every fifteen seconds and removes it " +
				"when it is done, and the trim ignores one nobody has renewed " +
				"for a minute; nothing else removes it, so a count that does " +
				"not return to zero is a holder that crashed — a backup " +
				"mid-copy, or a join before its node published a position.",
		},

		// ---- the fleet's history ---------------------------------------
		{
			Name: HistoryAnswers, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"question", "coverage"},
			Shows: "What each fleet history read covered: `complete` when " +
				"every live data node answered inside the fleet read budget, " +
				"`partial` when one did not. Turn-level detail lives only " +
				"on the data node that published it or keeps it in custody, " +
				"so a partial answer is missing that node's rows — and the " +
				"history_partial alarm is a fraction of this counter.",
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
				"drops a seat. Absent where the company configures no " +
				"embeddings and while the corpus cannot be measured — an " +
				"unreadable corpus is neither covered nor uncovered.",
		},

		// ---- the change feed ------------------------------------------
		{
			Name: TrackerFeedUnreadable, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"source"},
			Shows: "Change records this build could not translate into a " +
				"wake. Every domain feed's consumer is deliberately uncapped, so " +
				"such a record is never dropped — it redelivers for ever at " +
				"the head of the consumer with every wake behind it " +
				"waiting, which has no other symptom at all.",
		},

		// ---- authentication -------------------------------------------
		{
			Name: AuthAttemptsFailed, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"method", "outcome"},
			Shows: "Every failed attempt to prove an identity, as it happens: " +
				"a sign-in, a step-up, an invitation or password reset " +
				"link that did not prove itself, or a bearer presented " +
				"to a route that " +
				"needs one and refused — a credential an unguarded route " +
				"verifies itself, such as a webhook relay's own JWT, is " +
				"never counted (`method`), and whether the attempt was " +
				"verified and wrong or answered 429 by the throttle's " +
				"curve (`outcome`). It is " +
				"the ONLY per-attempt record there is, by design: an " +
				"unauthenticated caller authors this rate, so the event log " +
				"gets one `iam_login_failures` row per node per minute, " +
				"counting each source, and the rate itself lives here, where " +
				"a guessing run costs a counter increment rather than a row " +
				"in every backup.",
		},
		// ---- alarms ---------------------------------------------------
		{
			Name: AlarmActive, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"kind"},
			Shows: "Whether each named alarm is firing right now, 0 or 1 — " +
				"for one that stands on several logs, whether it stands on " +
				"any; its `domains` and its log lines say which. It is the same table " +
				"the operator record renders and the CLI exits non-zero on, " +
				"so a collector and a person see one answer.",
		},
		// ---- the object store's bucket ----------------------------------
		{
			Name: ObjectsBucketBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "How many bytes the fleet's own object bucket on the broker " +
				"holds (OBJ_crewlet_files: the company's files, their " +
				"metadata and any upload in flight), as the object " +
				"collector's duty holder read it; only that node reports it, " +
				"and only on the nats backend. Read against " +
				"`crewlet.objects.bucket.max_bytes`: a full bucket refuses " +
				"every upload, and `objects_bucket_near_ceiling` fires at " +
				"four fifths.",
		},
		{
			Name: ObjectsBucketMaxBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "The byte reservation the broker holds the object bucket " +
				"to (store.objects.nats_max_bytes, or the share the node " +
				"that created the bucket derived): the ceiling the bucket " +
				"keeps for its life, whatever a node's Tier A says later.",
		},
		// ---- artifact uploads and the publish pipeline -------------------
		{
			Name: ArtifactsUploadBytes, Kind: KindCounter, Unit: UnitBytes,
			Attributes: []string{"transport"},
			Shows: "Bytes of artifact files that reached the object store, by " +
				"how they got there: streamed through this node (`engine`, " +
				"which counts what the pipeline writes itself: a linted page, " +
				"an edit's result, a copied CDN file) or put straight to an " +
				"S3 bucket on a presigned URL and read back here (`direct`). " +
				"A fleet whose engine share stays high on s3 is streaming " +
				"uploads its bucket could have taken.",
		},
		{
			Name: ArtifactsUploadReservedBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "Bytes the company's upload reservations hold, as this node " +
				"last wrote the reservation ledger: bytes in the object store, " +
				"or on their way to it, that no record names yet. Read against " +
				"`artifacts.limits.pending_bytes` times four, the company's " +
				"bound: a value that stays near it is clients abandoning " +
				"uploads, each held until the object collector could have " +
				"deleted its bytes.",
		},
		{
			Name: ArtifactsPublishDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long one create, publish or asset finalize took through " +
				"the pipeline (read back, edits, lint, CDN copies, the " +
				"domain's decide), by `outcome`: `published`, `refused` (the " +
				"request was judged and turned down) or `failed` (no outcome " +
				"could be established, which is a store or a broker not " +
				"answering). The publish latency objective is read from it.",
		},
		{
			Name: ArtifactsPublishRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Uploads and publishes refused, by `reason` from a closed set " +
				"(internal/artifactupload.Refusals): a client sending the " +
				"wrong bytes (`upload_mismatch`), a page breaking the policy " +
				"(`policy_refused`), a company out of quota (`quota_exceeded`) " +
				"or reservations (`uploads_outstanding`), an agent's turn past " +
				"its guardrails (`turn_quota`), a moved head (`version_moved`) " +
				"and the rest. Each reason is a different person to call.",
		},
		// ---- the artifact origin ----------------------------------------
		{
			Name: ArtifactsServeRequests, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"dest", "status"},
			Shows: "Every request the artifact origin (the api.artifacts " +
				"listener) answered, by what it was (`dest`: `document`, a " +
				"page decided again on every request; `subresource`, a file " +
				"a page loads; `runtime` and `kit`, the embedded public " +
				"files; `brand`, the company's adopted brand's files) and by " +
				"status class (`status`: `2xx` to `5xx`). A " +
				"rise in `document` `5xx` is viewers whose frames this node " +
				"would not serve: a copy of the artifacts or identity log it " +
				"could not vouch for, or an object store that did not answer. " +
				"The artifact id is never an attribute; per-artifact detail " +
				"is in the log lines.",
		},
		{
			Name: ArtifactsServeDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"dest"},
			Shows: "How long the artifact origin took to answer, by `dest`, " +
				"the whole response included. A document's tail is the time " +
				"to first byte a viewer waits on before a frame can render, " +
				"which the service objective for viewing reads; a slow " +
				"subresource tail with fast documents is the object store.",
		},
		{
			Name: ArtifactsServeBytes, Kind: KindCounter, Unit: UnitBytes,
			Shows: "Body bytes the artifact origin sent, compressed where " +
				"they were sent compressed: what artifact viewing costs this " +
				"node in egress, per node and never per artifact.",
		},
		{
			Name: ArtifactsTicketRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "View tickets the artifact origin refused, by `reason`: " +
				"`invalid` (forged, malformed, or signed under a key this " +
				"node does not hold), `expired` (a frame past its hour, " +
				"which the dashboard re-mints), `host` (a ticket presented " +
				"on a host other than its artifact's), `navigation` (a " +
				"ticket URL opened in a tab of its own), `revoked` (the " +
				"share, the credential, the artifact or the version is " +
				"gone) and `stale` (this node could not vouch for what it " +
				"would decide from, and answered 503). `invalid` above zero " +
				"sustained is somebody presenting tickets this fleet never " +
				"minted, or a keyring rotated past a key still in use.",
		},
		{
			Name: ArtifactsAuthzPeerChecks, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Authorizations the artifact origin decided through " +
				"another data node because its own artifacts copy was out of " +
				"service or lagging, by `outcome` (`served`, `refused`, " +
				"`unavailable`). Any at all is a node whose copy is not " +
				"answering; `unavailable` is a viewer refused because no " +
				"data node could decide for them.",
		},
		// ---- the artifact datastore ---------------------------------------
		{
			Name: ArtifactsDataWriteDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"verdict"},
			Shows: "How long a datastore write takes from the request to its verdict " +
				"read back from this node's rows, by verdict (applied, conflict, " +
				"invalid, forbidden, quota, gone, or pending where this node had not " +
				"applied it yet, rate_limited and refused_early before anything was " +
				"appended). The datastore op objective is a p95 of 250 ms; a rise in " +
				"pending reads as this node's applier falling behind.",
		},
		{
			Name: ArtifactsDataWrites, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"verdict"},
			Shows: "Datastore writes by verdict. A climb in conflict is people editing " +
				"the same rows over each other; in forbidden, a page or an agent writing " +
				"what its collection's rules refuse; in rate_limited, one client " +
				"spending this node's write budget.",
		},
		{
			Name: ArtifactsDataFootprintBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "The company's datastore footprint in real bytes held on every data " +
				"node (each document row with its index and field-index entries, and hot " +
				"history, as the applier meters what the rows cost on disk), sampled " +
				"after each committed batch. Read against `artifacts.limits.data_bytes`: " +
				"a store or a company at its limit refuses every write that grows it.",
		},
		// ---- the Dashboard Assistant -------------------------------------
		{
			Name: AssistantTurns, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Ask Crewlet turn segments this node ran, by how each ended " +
				"(`completed`, `waiting_for_approval`, `stopped`, `failed`, " +
				"`interrupted`, `budget_exhausted`). A climb in `failed` is a model " +
				"chain or a tool route failing people; in `budget_exhausted`, the " +
				"company's windows refusing Ask Crewlet.",
		},
		{
			Name: AssistantTurnDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long an Ask Crewlet turn segment runs from the question to its " +
				"end, by outcome. A three-step read is held to 15 s at the median; a " +
				"distribution reaching the turn's own timeout is a model or a CLI too " +
				"slow for an interactive drawer.",
		},
		{
			Name: AssistantFirstOutputLatency, Kind: KindHistogram, Unit: UnitMilliseconds,
			Shows: "How long a person waits from asking to the first step or text the " +
				"drawer shows. Held to 4 s at the median; a rise is a CLI's cold start " +
				"or its slots queued behind other turns.",
		},
		{
			Name: AssistantSteps, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"state"},
			Shows: "Ask Crewlet tool steps by how each ended (`done`, `failed`, " +
				"`refused`, `stopped`, `waiting`). A climb in `refused` is the model " +
				"asking for what its people may not read; in `failed`, a route or the " +
				"dispatcher breaking under Ask Crewlet.",
		},
		{
			Name: AssistantBriefs, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Brief answers Ask Crewlet's model wrote, by what the engine did with " +
				"each (`written`, `refused`). A `refused` answer is written again once; a " +
				"climb in it is a model that cannot keep to the brief's shape.",
		},
		{
			Name: ToolPolicyRefusalLoops, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"tool"},
			Shows: "Runs of policy refusals a seat's or a worker's write tool answered in a " +
				"row on one target until it reached the limit (internal/tools." +
				"PolicyRefusalLimit), each counted once when it did, by `tool`: the model was " +
				"told to stop retrying and raise a blocker. A climb names a tool whose " +
				"contract the model cannot see or cannot meet.",
		},
		{
			Name: ToolArgsOutsideSchema, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"tool"},
			Shows: "Calls a first-party tool answered successfully although their arguments " +
				"were outside the schema it advertises (an argument it does not take, a value " +
				"outside its closed set or its bounds), by `tool`. The model was told on the " +
				"answer. A tool that keeps climbing here is one whose schema and reading " +
				"disagree, and the drift a later refusal at the surface would have to fix first.",
		},
		{
			Name: AssistantBriefRowsDropped, Kind: KindCounter, Unit: UnitCount,
			Shows: "Records a brief cited (a row, or its recommendation's press) that the " +
				"engine left out because the record was not in what its turn read, or " +
				"the person may not open it. A steady climb is a model citing records " +
				"it did not read.",
		},
		{
			Name: AssistantCardsShownAsNotice, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"type"},
			Shows: "Stored Ask Crewlet cards read back as a plain notice because this build's " +
				"contract refuses their shape, by card `type`. Expected for briefs recorded " +
				"before the brief's current shape, and during a rolling upgrade when an older " +
				"node reads a newer node's card. A climb outside both is a card written past " +
				"its own validation.",
		},
		{
			Name: AssistantStreamsActive, Kind: KindGauge, Unit: UnitCount,
			Shows: "Ask Crewlet live streams this node is serving. Each holds a " +
				"connection and a subscription to one turn's frame log; a count that " +
				"never falls is streams whose clients went away without the " +
				"connection closing.",
		},
		// ---- presence and the artifact rooms' pushes ----------------------
		{
			Name: PresenceFrames, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"direction"},
			Shows: "Presence frames this node handled, by `direction`: `client` (a " +
				"socket's presence frame taken), `peer` (a sealed frame from another " +
				"node admitted) and `push` (a room's coalesced fan-out to this node's " +
				"sockets). Presence is told, never stored, so this is the whole of " +
				"its cost: a `peer` climb with flat `client` is a busy room held on " +
				"other nodes, and `push` is bounded at five a second per room.",
		},
		{
			Name: PresenceRooms, Kind: KindGauge, Unit: UnitCount,
			Shows: "Artifact rooms this node holds a member of, a socket's or an agent's. " +
				"Each costs a map in memory and, while one of this node's sockets is in " +
				"it, one listen on the broker.",
		},
		{
			Name: ArtifactsPushLag, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"kind"},
			Shows: "How long after the broker stored a committed record its push was " +
				"handed to this node's sockets, by `kind`: `changed` for an artifact's " +
				"movement (`artifact_changed`) and `data` for a datastore's documents " +
				"(`artifact_data`) — the applier's lag plus the fan-out, measured " +
				"whether or not a socket is in the room. A change is held to reach " +
				"the sockets watching it within a second (the `change_visibility` " +
				"objective, which counts only the pushes some socket was handed).",
		},
		// ---- service objectives -------------------------------------------
		{
			Name: ArtifactsReadyDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long a frame took from its view ticket being minted to the " +
				"page's bridge hello, as the viewer's browser reported it through " +
				"`POST /artifacts/ready`, by `outcome` (`ready`, or `failed` for a frame " +
				"that never said hello). The only artifacts instrument that sees an " +
				"outage keeping requests from reaching the engine at all (the artifact " +
				"domain's DNS or certificate, an edge, a corporate filter): every " +
				"server-side instrument reads such an outage as a quiet node. The " +
				"`artifact_ready` objective holds 99.5% of opens within 3 s.",
		},
		{
			Name: ObjectivesEvents, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"objective", "outcome"},
			Shows: "Every service objective's events, by `objective` (the closed set " +
				"in docs/reference/service-objectives.md) and `outcome` (`good`: " +
				"answered without fault on the engine's side within the objective's " +
				"bound; `bad`: anything else). `bad` over all, per objective, against " +
				"one minus its target is the error budget's burn rate, which the " +
				"`objective_burn` alarm judges per node over an hour and five minutes " +
				"and over six hours and thirty minutes; summed across the fleet it is " +
				"the company's.",
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
