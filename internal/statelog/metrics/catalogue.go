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
				"meanwhile), `floor_unknown` (the domain's floor or the log's " +
				"ends could not be read — clears when coordination answers, or " +
				"once a floor row that does not read as its floor is removed). " +
				"The log: " +
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
				"is too late to be. What a read waits for is this node " +
				"reaching the position: every record up to it that writes a " +
				"row committed, every barrier decoded — never a barrier's " +
				"own commit, so a disk's stalled fsync shows here only " +
				"behind a commit that writes a row.",
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
				"and which budget ended it. A transaction with rows is what " +
				"every waiter behind it pays, and rows were only ever a " +
				"proxy for the duration. A run of nothing but barriers " +
				"commits beside the apply loop, one transaction at a time " +
				"carrying every barrier since the last: no reader waits on " +
				"it, and a transaction with rows waits for it to return.",
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
				"left — on every log that claims identity (the tracker, the " +
				"pages, the identity estate, the artifacts, their datastores " +
				"and the code log), the ceiling " +
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
		{
			Name: StatelogAnchorsSwept, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Arbitration anchors this node's applier removed under the " +
				"trim's `trimmed` records (ADR-0050), counted once the " +
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

		// ---- the embedded broker ---------------------------------------
		{
			Name: BrokerPeerRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"listener", "presented"},
			Shows: "A connection to this node's embedded broker refused for its " +
				"credential, by the listener it reached (`route`, `leaf`, " +
				"`client`) and what it presented (`none`; `unknown`: a key " +
				"this node does not hold; `invalid`: a held key whose proof " +
				"failed). `unknown` rising during a keyring rotation is a " +
				"node whose keyring differs from this one's; `none` is a " +
				"process that is not a node of this fleet, or one on an " +
				"older build. Every refusal is counted; the " +
				"`broker_peer_refused` line says which key, from where, once " +
				"a minute a class. No alarm reads it: an operator alarms on " +
				"any rise.",
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
			Shows: "One search's scan, recorded by the node that coordinated " +
				"it, split by what it ran for (`path`: `prefetch` for a turn's " +
				"knowledge prefetch, which the turn waits on before its first " +
				"token, `interactive` for somebody's deliberate search) and by " +
				"the ranking it actually served (`rung`: hybrid, keyword, " +
				"semantic, or none). Each path has its own budget — half a " +
				"second for a prefetch (`prefetch_slow`), a second for an " +
				"interactive search (`search_slow`) — and read as one series " +
				"the quicker one hides the other; a keyword scan is a fraction " +
				"of a fused one's cost, so reading rungs as one series hides a " +
				"slow meaning half.",
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
		// ---- the object store's S3 requests ---------------------------
		{
			Name: ObjectsS3Attempts, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"operation", "outcome"},
			Shows: "Every attempt this node's S3 client sent its bucket, by S3 " +
				"`operation` (the API's own name: `HeadObject`, " +
				"`GetObject`, `PutObject`, `CreateMultipartUpload`, " +
				"`UploadPart`, `CompleteMultipartUpload`, " +
				"`AbortMultipartUpload`, `DeleteObject`, `DeleteObjects`, " +
				"`ListObjectsV2`, `ListMultipartUploads`, and S3 Express's " +
				"`CreateSession`) and what the bucket made of it (`outcome`): " +
				"`answered` — served, a success or a refusal such as a missing " +
				"key; `throttled` — asked to slow down (`503 SlowDown`, any " +
				"`429`, MinIO's `SlowDownRead` and `SlowDownWrite`), which cuts " +
				"the node's gate; `failed` — any other answer: a server error " +
				"that is no throttle, or a status no request expects, such as a " +
				"redirect to another region; `unreached` — no answer at all: a " +
				"connection refused or reset, a name that did not resolve, a TLS " +
				"failure, or an attempt never sent because its S3 Express " +
				"session could not be had; `stalled` — the bucket stopped " +
				"moving the attempt for a minute: took no more of its body, " +
				"acknowledged none of it at the node's socket and sent none of " +
				"its answer, so the request was given up; `ended` — the " +
				"attempt's own deadline or its caller ended it in flight. An " +
				"operation's RETRIES are " +
				"this counter's total for it, less the count of " +
				"`crewlet.objects.s3.request.attempts` for it, plus that " +
				"histogram's `unsent` count for it. `throttled` rising is the " +
				"fleet over the bucket's rate for the prefix (S3 serves 5 500 " +
				"reads and 3 500 writes a second a prefix); `failed` the " +
				"bucket's own fault; `unreached` the network or the endpoint.",
		},
		{
			Name: ObjectsS3GateLimit, Kind: KindGauge, Unit: UnitCount,
			Shows: "How many attempts this node's S3 client lets its bucket " +
				"hold at once while a throttle holds it back — seven tenths of " +
				"the most the node had out at once when the bucket last " +
				"throttled it, one more each round of answers — and zero while " +
				"nothing does: the gate lifts a minute after the bucket's last " +
				"throttle. Above zero for long is a fleet held over the " +
				"prefix's rate; every request of the node — git syncs, the " +
				"collector, backups, people's files — shares what it allows.",
		},
		{
			Name: ObjectsS3GateWaiting, Kind: KindGauge, Unit: UnitCount,
			Shows: "Attempts of this node's S3 client waiting in line for the " +
				"gate, first come first served: what a throttle is costing the " +
				"node now. High while `crewlet.objects.s3.gate.limit` stays low " +
				"is a node sending more than the prefix will take from it — its " +
				"requests wait rather than fail, until their own deadlines.",
		},
		{
			Name: ObjectsS3RequestAttempts, Kind: KindHistogram, Unit: UnitCount,
			Attributes: []string{"operation", "outcome"},
			Shows: "How many attempts each request of this node's S3 client " +
				"took, observed once as it ends, by `operation` and how it " +
				"ended (`outcome`): `answered` — the bucket's final answer, a " +
				"success or a refusal; `given_up` — the bucket answered nothing " +
				"but failures (server errors that are no throttle, or no answer " +
				"at all) from the first to a bound drawn for it between forty " +
				"seconds and a minute after it, or stopped moving one of its " +
				"attempts for a minute (`stalled` on the attempts counter), so " +
				"the request was not asked again and its caller was told the " +
				"bucket's last answer; " +
				"`ended` — its deadline or its caller ended it after at least " +
				"one attempt, one still throttled among them; `unsent` — it " +
				"ended before its first attempt, observed at zero: its deadline " +
				"or its caller ended it while it waited at the gate (the gate " +
				"starving the node's requests), or it was refused before " +
				"sending; `failed` — a failure asking again cannot change, such " +
				"as a certificate the node does not trust, or an S3 Express " +
				"session that could not be had for it. Nothing counts a " +
				"request's attempts but its deadline and that bound, so the " +
				"tail is how long the bucket made requests wait: a p95 above " +
				"one is a bucket throttling or failing, and `given_up` above " +
				"zero is a bucket that failed every attempt for most of a " +
				"minute, or held one for a whole minute without moving it.",
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
			Name: CodeChangeProposals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"op", "outcome"},
			Shows: "Writes of a change's lifecycle this node's committer was " +
				"asked to decide, by `op` (`change_open`, `change_edit`, " +
				"`change_draft`, `change_retarget`, `change_close`, " +
				"`change_reopen`, and `change_landed`, a landing its " +
				"repository's home git node found) and what became of each, by " +
				"`outcome`: " +
				"`applied`, its record landed and its change's row says so; " +
				"`unchanged`, it asked for what the change already said, and " +
				"nothing was appended; `pending` and `unknown`, its record was " +
				"appended and is not yet known here to have applied; a " +
				"refusal's own code — `not_permitted`, `stale`, `change_exists`, " +
				"`no_source`, `no_target`, `same_ref`, `no_difference`, " +
				"`change_closed`, `change_merged`, `change_not_found`, " +
				"`repo_archived`, `repo_importing`, `repo_mirrored`, " +
				"`repo_deleted`, `busy`, `not_applied`, " +
				"`unavailable` — which decided nothing; `handed_on`, one this " +
				"node decided nothing of for a reason about itself (stopping, " +
				"no chart to judge a seat by, behind the company's " +
				"configuration), which the estate asks the repository's " +
				"runner-up; and `failed`, any other error. `stale` climbing is " +
				"askers writing against a change pushes keep moving; " +
				"`handed_on` on one node and `applied` on another, a node that " +
				"cannot judge its seats.",
		},
		{
			Name: CodeQueueProposals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"op", "outcome"},
			Shows: "Writes of a target's merge queue this node's committer was " +
				"asked to decide, by `op` (`enqueue`, a change put in its " +
				"target's queue through its merge gate, and `dequeue`, an " +
				"entry taken out at a principal's ask; and the queue's own " +
				"records its home asks: `candidate`, a train built onto the " +
				"target's tip, `eject`, entries put out, `land`, entries " +
				"landed with every one's gate proved again, `green` and " +
				"`broken`, a landing its tip checks passed or failed) and what " +
				"became of each, by `outcome`: `applied`, its record landed and " +
				"the rows it wrote say so; `pending` and `unknown`, its record " +
				"was appended and is not yet known here to have applied; a " +
				"refusal's own code — `gate_unmet`, `already_queued`, " +
				"`queue_full`, `no_queue`, `head_moved`, " +
				"`stale`, `target_moved`, `change_not_open`, `change_draft`, " +
				"`change_not_found`, `not_queued`, `not_permitted`, " +
				"`not_green`, `not_red`, `no_landing`, `record_full`, " +
				"`upload_expired`, `repo_size`, `compaction_behind`, " +
				"`repo_archived`, `repo_importing`, `repo_mirrored`, " +
				"`expunging`, `repo_deleted`, `busy`, `not_applied`, " +
				"`unavailable` — which decided nothing; `handed_on`, one this " +
				"node decided nothing of for a reason about itself, which the " +
				"estate asks the repository's runner-up; and `failed`, any " +
				"other error. `gate_unmet` climbing is askers queueing changes " +
				"before their gate is met, or, on `land`, gates that stop " +
				"holding between an enqueue and its landing; `queue_full`, a " +
				"queue landing slower than it is fed; `target_moved` on " +
				"`candidate` and `land`, a branch moving under its queue.",
		},
		{
			Name: CodeMergeOutcomes, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Merges of changes this node's committer was asked to decide " +
				"— each one record that merges the change and moves its target, " +
				"its merge gate proved from one snapshot or a person's override " +
				"recorded — by what became of each, `outcome`: `applied`, its " +
				"record landed and the change's row says so; `pending` and " +
				"`unknown`, its record was appended and is not yet known here to " +
				"have applied; a refusal's own code — `gate_unmet`, " +
				"`target_moved`, `head_moved`, `stale`, `queue_target`, " +
				"`not_person`, `source_refused`, `upload_expired`, `repo_size`, " +
				"`compaction_behind`, `change_not_open`, `change_draft`, " +
				"`change_not_found`, `repo_archived`, `repo_importing`, " +
				"`repo_mirrored`, `expunging`, `repo_deleted`, `busy`, " +
				"`not_applied`, `unavailable` — which decided nothing; " +
				"`handed_on`, one this node decided nothing of for a reason " +
				"about itself, which the estate asks the repository's runner-up; " +
				"and `failed`, any other error. `target_moved` climbing is a " +
				"branch moving faster than its merges are built onto it; " +
				"`gate_unmet`, askers merging changes before their gate is met.",
		},
		{
			Name: CodeChecksQueued, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"purpose"},
			Shows: "Attempts of checks this node's checks writer queued — each by a " +
				"record on its repository's checks subject that applied here (a " +
				"head's dispatch record carries all of its own), counted once, by " +
				"the node that published it — by `purpose`: " +
				"`head`, a change's head; `candidate`, a merge queue's candidate; " +
				"`tip`, a protected branch's tip; `bisect`, a landing between a " +
				"green tip and a red one. A head recorded skipped at its dispatch " +
				"is counted under crewlet.code.checks.finished alone.",
		},
		{
			Name: CodeChecksFinished, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"state", "placement", "replaced"},
			Shows: "Endings of attempts of checks, as their rows keep the ending " +
				"— each counted once, by the node whose checks writer published " +
				"it — by `state` (`passed`, `failed`, `errored`, `cancelled`, " +
				"`skipped`), `placement` (`container`, `e2b`, `external`, or " +
				"`none` for one no runner claimed) and `replaced`: `none` for an " +
				"attempt's first ending, and otherwise the ending this one " +
				"REPLACED on its row — a measurement after an infrastructure " +
				"ending, a slow runner's exit after its attempt was errored " +
				"`runner_lost` — so the attempts that ended are the count under " +
				"`replaced=none` and the rest are corrections of them, never " +
				"second endings. An ending the row did not keep — a later " +
				"infrastructure ending beside a measurement — is not counted. " +
				"`errored` climbing on one placement is that placement's cells " +
				"failing the checks they run; `replaced=errored` climbing is " +
				"runners declared lost that were only slow.",
		},
		{
			Name: CodeChecksClaims, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Claims of queued attempts this node's checks writer decided, " +
				"by `outcome`: `won`, the claim landed and its runner holds the " +
				"attempt; `claimed`, refused before it appended — another runner " +
				"holds the attempt, or it ended; `lost`, the claim landed and " +
				"the row keeps another runner's, which a second node publishing " +
				"for the repository landed first; `unchanged`, the holder's own " +
				"claim sent again; `no_such_check`, `repo_deleted`, `busy`, " +
				"`not_applied` and `unavailable`, which decided nothing; " +
				"`pending` and `unknown`, appended and not yet known here to " +
				"have applied; `handed_on`, one this node decided nothing of for " +
				"a reason about itself, which the estate asks the repository's " +
				"runner-up; and `failed`, any other error. `claimed` is git " +
				"nodes racing for one attempt, each losing one box it never " +
				"made; `lost` above zero is two nodes publishing one " +
				"repository's checks at once.",
		},
		{
			Name: CodeChecksRunning, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"placement"},
			Shows: "Checks this git node's runner is running now — each an " +
				"attempt it claimed, from its claim to its ending — by " +
				"`placement`: `container`, a container on this node, at most " +
				"`git.max_checks` of them; `e2b`, a remote box. `container` " +
				"pinned at `git.max_checks` while code_checks_backlog stands is " +
				"a fleet whose container cells are too few for its checks.",
		},
		{
			Name: CodeChecksRemoteCapped, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Walks of this git node's check runner that left a remote " +
				"(e2b) check queued rather than start its box — one a walk, " +
				"since a walk held back once tries no further remote check — by " +
				"`reason`: `max_running`, the company's " +
				"`code.checks.remote.max_running` boxes already run; " +
				"`box_hours_per_day`, the UTC day's " +
				"`code.checks.remote.box_hours_per_day` are spent; `unknown`, " +
				"the coordination store could not say, which starts nothing. " +
				"`max_running` climbing while code_checks_backlog stands is a " +
				"company whose remote cap is below its checks; " +
				"`box_hours_per_day` is its remote checks waiting for the next " +
				"UTC midnight.",
		},
		{
			Name: CodeChecksDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"placement"},
			Shows: "How long the checks this git node's runner ran took, from " +
				"the claim to the ending it recorded — the box's making and " +
				"setup, the command and its results read — by `placement` " +
				"(`container`, `e2b`). A check's time is its own definition's " +
				"to bound (`timeout_seconds`, held to " +
				"`code.checks.max_timeout_seconds`); a p95 climbing on one " +
				"placement with no change to the checks is that placement's " +
				"boxes slower to make or to set up.",
		},
		{
			Name: CodeConversationWrites, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"op", "outcome"},
			Shows: "Writes of a change's conversation this node's change writer " +
				"was asked to decide — each a record on the change's own " +
				"subject, never in the repository's committer queue — by `op` " +
				"(`reviewers`, `comment`, `thread_resolve`, `review`, " +
				"`finding_resolve`, and three its repository's home git node " +
				"asks: `revise`, a revision's facts it read, " +
				"`mergeability`, whether the change's head revision merges " +
				"into its target's tip, and `asks`, the change's asks for " +
				"review settled with its lifecycle — ended once it closed or " +
				"merged, asked again once it is reopened) and what became of " +
				"each, by " +
				"`outcome`: " +
				"`applied`, its record landed and the change's conversation " +
				"holds it; `unchanged`, it asked for what the conversation " +
				"already said, and nothing was appended; `pending` and " +
				"`unknown`, its record was appended and is not yet known here " +
				"to have applied; a refusal's own code — `not_permitted`, " +
				"`no_reviewer`, `too_many_reviewers`, `unknown_revision`, " +
				"`change_closed`, `change_merged`, `thread_gone`, " +
				"`not_author`, `not_raiser`, `finding_gone`, " +
				"`too_many_findings`, `change_not_found`, `repo_archived`, " +
				"`repo_deleted`, `busy`, `not_applied`, `unavailable`, and the " +
				"home's `superseded` — facts or a merge of a revision, a head or " +
				"a target the change has moved from; facts no later than the " +
				"facts it holds, or matched against path owners changed since; " +
				"a merge into a tip before the one its mergeability is of, or " +
				"into the same tip once decided; a settling of asks for a " +
				"change that moved since its home read it, or holds no such " +
				"ask — which decided nothing; `handed_on`, one this node " +
				"decided nothing of " +
				"for a reason about itself (stopping, no chart to judge a seat " +
				"or a reviewer by, behind the company's configuration), which " +
				"the estate asks the repository's runner-up; and `failed`, any " +
				"other error. `busy` climbing is a change written faster than " +
				"one node writes it — its writes wait their turn one at a time " +
				"— or one the change kept moving under for every round.",
		},
		{
			Name: CodeCommitBusy, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"kind", "reason"},
			Shows: "Proposals refused `busy`, undecided, by `kind` and `reason`. " +
				"`kind`: `push`, whose client runs `git push` again; " +
				"`compaction`, which the git node that built it plans again at " +
				"its next pass; `expunge`, a record of an expunge of a " +
				"repository's unreachable objects — a person's purge request, " +
				"which they ask for again, or the begin or the commit its " +
				"maintainer built, which it plans again at its next pass; " +
				"`change`, a write of a change's lifecycle, which its asker " +
				"sends again; `merge_queue`, an enqueue or a dequeue of a " +
				"target's merge queue, which its asker sends again; `merge`, " +
				"a change's merge, which its builder asks again; `import`, a " +
				"chunk or the end of a repository's import from another host, " +
				"which its home reads the repository and proposes again at its " +
				"next round; and `mirror_sync`, a pull mirror's sync, which its " +
				"home fetches and builds again at its next round. `reason` is " +
				"one of five causes, each pointing at " +
				"a different remedy. Three are a BACKLOG, which raises " +
				"`code_commit_busy` on the committer's node: `queue`, a " +
				"repository taking pushes faster than one node's committer " +
				"commits them (its queue of 1 024 proposals, or sixteen " +
				"records' bytes, is full); `node`, the node holding " +
				"sixty-four records' bytes of proposals across its " +
				"repositories; and `waited`, the queue's TIME bound, the one a " +
				"committer slower than its pushes meets before its count: a " +
				"proposal that queued with more of its asker's attempt left " +
				"than the 30-second commit budget and a second for the " +
				"answer, and lost it waiting behind earlier ones — asked with " +
				"no deadline, one that waited that budget whole. `late` is a " +
				"proposal that ARRIVED with no more than that left — the " +
				"attempt spent before it reached the committer, waiting for " +
				"the node to take it (its proposal slots full), for the node " +
				"to reach the asker's own writes, or under a caller deadline " +
				"shorter than 31 seconds. `contended` is a record that " +
				"lost every compare-and-set round it may take: the repository " +
				"kept changing, a second committer deciding it during a " +
				"handoff. Neither of the last two says this committer turns " +
				"proposals away, so neither raises an alarm. A `compaction` " +
				"busy is a repository whose pushes keep its committer's queue " +
				"full, and whose pack count grows meanwhile; an `expunge` busy, " +
				"one whose unreachable objects stay in the store meanwhile. No " +
				"repository is an attribute — which one is hot is a question " +
				"for its rows and the node's `code_commit_busy` lines, a " +
				"repository a minute, not a time series a repository.",
		},
		{
			Name: CodeHomePasses, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"worker", "outcome"},
			Shows: "Turns this node took as the HOME git node of a repository — " +
				"one worker's work in one repository, scheduled by a nudge from " +
				"the code log or by the minute's backstop pass — by `worker` " +
				"(`changes`: its live changes' facts and mergeability recorded — " +
				"a moved target's merges again, at most sixty-four a repository " +
				"a minute — or a change whose head its target holds landed; " +
				"`check_runners`: its running checks whose runner left its git " +
				"order, or ran past its check's timeout and a setup's grace, " +
				"ended `errored runner_lost`; `mirrors`: its import or its pull " +
				"mirror's sync STARTED, on a goroutine of its own, since a fetch " +
				"from another host outlives a turn — `crewlet.code.import.chunks` " +
				"and `crewlet.code.mirror.runs` count what each came to) and " +
				"`outcome`: `worked`, its due " +
				"work done; `failed`, a read or a record that decided nothing for " +
				"a reason of the node's or the log's (`code_home_turn_failed` " +
				"says which), the rest worked and the repository listed again by " +
				"the next pass; and `cut`, more work than one turn's minute, the " +
				"rest listed again by the next pass. A refusal that the rows moved " +
				"since — a revision's facts superseded, a landing stale — is the " +
				"rows answering, and the turn `worked`. `cut` climbing on one node " +
				"is a home with more changes than its two turns keep up with.",
		},
		{
			Name: CodeHomeNudges, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "The code log's word to this node's home loop, one a record " +
				"on a repository's subject, by `outcome`: `scheduled`, this node " +
				"is the repository's home and its work was queued at the " +
				"record's position; `elsewhere`, another git node is its home; " +
				"`unknown`, who serves git could not be read, and the backstop " +
				"lists the repository within a minute; `ignored`, a subject that " +
				"names no repository; and `gap`, the watch may have missed records " +
				"— its consumer lost, its connection back — and a pass was run at " +
				"once at the position it reaches. Every git node watches every " +
				"repository, so `elsewhere` is most of every node's count but its " +
				"homes'; `gap` climbing is a watch that keeps losing its consumer.",
		},
		{
			Name: CodeHomeFactsDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "One revision's facts as this node's home loop asked them of " +
				"the repository's git nodes — its merge base, the files it touches " +
				"and its patch-id against its target's tip — by `outcome`: " +
				"`answered` or `failed`. Read by the home itself unless its copy " +
				"cannot answer, the runner-up then: a 1 MiB patch takes some 20 " +
				"ms and one at the 256 MiB bound some 2.3 s on two cores. " +
				"Durations near the read's ten seconds of git are changes past " +
				"what a home identifies — their patch-ids omitted — or a home " +
				"short of the CPU its reads run on.",
		},
		{
			Name: CodeHomeMergeDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"work", "outcome"},
			Shows: "One merge of a change's head into its target's tip on this " +
				"git node — `git merge-tree --write-tree` in a scratch of its " +
				"own, never in a cache — by `work`: `mergeability`, the home's " +
				"own, its ancestry asked first and its merge given thirty " +
				"seconds of the maintenance lane, timed from the copy's sync to " +
				"the ancestry's or the merge's answer — never the record of it " +
				"on the change or the landing, which are the change writer's " +
				"and the committer's; or `preview`, a comparison's merge " +
				"preview a person or a seat asked, given a read's ten seconds " +
				"of the read lane under a unit of the node's seat git-work " +
				"share, its merge-tree alone timed: a preview refused before " +
				"it (not held, not permitted, a commit absent), or answered " +
				"from the node's memo of the same two commits, is no merge, and " +
				"is not observed. `outcome`: `clean`, `conflict`, `unknown` — " +
				"the merge ran out of its time and decided nothing, which a " +
				"change's home records and asks again ten minutes on — " +
				"`landed`, the home's ancestry finding the head inside its " +
				"target, the change landed with no merge run, and `failed`, a " +
				"merge that could not run (a copy that would not sync, a busy " +
				"lane, git failing) or a preview's merge its asker's attempt " +
				"ended first. `unknown` climbing is changes past what a merge " +
				"decides in its time; `failed` climbing on the `mergeability` " +
				"work, a home whose maintenance lane is full.",
		},
		{
			Name: CodeCompactRuns, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Compactions this node made as the MAINTAINER of their " +
				"repositories — the first git node in each repository's order " +
				"whose background budget holds its publication, the home on a " +
				"fleet of alike nodes — each a plan over a repository's live " +
				"packs that no published layer names, and what became of it, by " +
				"`outcome`. `applied`: its record landed and " +
				"its outputs replaced its inputs. `pending`, `unknown`: its record " +
				"was appended and is not yet known to have applied — the next poll " +
				"reads the rows. `not_due`: the plan found nothing worth merging, " +
				"and the repository is not planned again until its row moves. " +
				"`stale`: another compaction, a publication or a purge moved an " +
				"input first. `busy`: no maintenance slot or no memory room was " +
				"free, a publication of the repository was running on the node, " +
				"or the repository's committer was busy. `refused`: by the committer or " +
				"before it (`crewlet.code.compact.refusals` says why). `failed`: " +
				"the build, an upload or the answer failed " +
				"(`code_compaction_failed` names which). Every outcome but " +
				"`applied` is tried again at a later poll. `applied` keeping pace " +
				"with the pushes is a node keeping its repositories' pack counts " +
				"bounded; `failed` rising is its disk, its git or its object store.",
		},
		{
			Name: CodeCompactDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "One compaction from its plan to its answer, by `outcome` (as " +
				"`crewlet.code.compact.runs` counts them): the sync, the plan, the " +
				"build, every output's upload and the committer's decide. A merge " +
				"of a few small packs takes under a second; one of several " +
				"gigabytes, minutes of its build and of the store's pace. " +
				"`applied` durations rising while the pushes hold steady are a " +
				"maintaining node short of the CPU its builds run on, or of the " +
				"bandwidth to its object store.",
		},
		{
			Name: CodeCompactRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Compactions refused before or at their decide, by `reason`. " +
				"This node's own: `memory`, the build's charge past what the node " +
				"can give its background work (`code_cache_compaction_refused` " +
				"names it, and nothing is built); `store_full`, an object store " +
				"with no room for an output (the repository is held back until " +
				"the store shows the room — on a store that reserves nothing, " +
				"tried again once an hour, as the collector frees room); " +
				"`outputs`, a build that wrote more packs than one compaction may " +
				"record, and `bundle_too_large`, an output's bundle past the bound " +
				"even at a smaller cut — either uploads nothing and halves what " +
				"the repository's next plan merges. The committer's: `stale`, " +
				"`busy`, `repo_deleted`, `upload_expired`, `pack_too_large`, " +
				"`not_applied` and `unavailable`, as for a push. " +
				"`upload_expired` is a build and its uploads longer than the " +
				"twelve hours a key may wait to be named, `pack_too_large` the " +
				"builder's bug: each is logged by name.",
		},
		{
			Name: CodeCompactBehind, Kind: KindGauge, Unit: UnitCount,
			Shows: "Repositories this node maintains that its last pass found at " +
				"the most live packs a repository may hold, whose pushes carrying " +
				"objects are refused `compaction_behind` until a compaction lands: " +
				"`code_compaction_behind` fires on any. Zero on a node that " +
				"maintains no such repository.",
		},
		{
			Name: CodePublishRuns, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Publications this node made as the MAINTAINER of their " +
				"repositories — each a plan of the repository's next published " +
				"multi-pack-index layer, its bitmap and its commit-graph layer " +
				"over every live pack its base does not cover, which every git " +
				"node installs — and what became of it, by `outcome`. `applied`: " +
				"its record landed and its layer is the top of the chain. " +
				"`pending`, `unknown`: its record was appended and is not yet " +
				"known to have applied. `not_due`: nothing lay outside the chain " +
				"to publish, and the repository is not planned again until its row " +
				"moves. `waiting`: a layer of the chain was still at this node's " +
				"installer, so the plan waited for it rather than replace it. " +
				"`stale`: another publication, a compaction, an expunge or a purge " +
				"moved the chain or a pack first. `busy`: no maintenance slot or no memory " +
				"room was free, a compaction of the repository was running on the " +
				"node, or its committer was busy. `refused`: by the committer or " +
				"before it (`crewlet.code.publish.refusals` says why). `failed`: " +
				"the build, an upload or the answer failed " +
				"(`code_publication_failed` names which). Every outcome but " +
				"`applied` is tried again at a later poll; `failed` rising is the " +
				"node's disk, its git or its object store.",
		},
		{
			Name: CodePublishDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "One publication from its plan to its answer, by `outcome` (as " +
				"`crewlet.code.publish.runs` counts them): the sync, the plan, the " +
				"repack, the layer's bitmap — a walk of the repository's whole " +
				"history, seconds at a gigabyte and minutes at the largest — every " +
				"part's and output's upload, and the committer's decide. Its " +
				"build is what paces the repository's next publication: at most a " +
				"tenth of the node's time.",
		},
		{
			Name: CodePublishRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Publications refused before or at their decide, by `reason`. " +
				"This node's own: `memory`, the build's charge — 600 bytes an " +
				"object of the repository for the bitmap, with room beside it for a " +
				"clone — past what the node can give its background work " +
				"(`code_cache_maintenance_refused` names it with step `publish`, " +
				"and the repository counts toward code_maintenance_refused for as " +
				"long; asked again at every pass, and a git node with the memory " +
				"takes it over); `store_full`, an object store with no room for a " +
				"part or an output (held back until the store shows the room); " +
				"`outputs` and `bundle_too_large`, its repack's, as a " +
				"compaction's; `no_bitmap`, a layer git wrote without a bitmap — no " +
				"pack it covers holds a commit — even over one more layer; " +
				"`layer_too_large`, a layer past what sixteen parts of a gibibyte " +
				"hold. The committer's: `stale`, `busy`, `repo_deleted`, " +
				"`upload_expired`, `pack_too_large`, `not_applied` and " +
				"`unavailable`, as for a compaction.",
		},
		{
			Name: CodePublishBytes, Kind: KindCounter, Unit: UnitBytes,
			Attributes: []string{"kind"},
			Shows: "Bytes this node stored for its publications, by `kind`: " +
				"`layer`, a layer's three files — its multi-pack-index, its bitmap " +
				"and its commit-graph layer — stored as parts of a gibibyte; " +
				"`outputs`, its repack's packs. A layer is about 37 bytes an " +
				"object and 69 a commit of what it covers, so a repository's root " +
				"layer is the one large write and every later one is its tail's.",
		},
		{
			Name: CodeExpungeRuns, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Expunges this node ran as the MAINTAINER of their " +
				"repositories — a person's purge served, a repository its weekly " +
				"listing names judged, or an expunge left open finished from its " +
				"rows — each one cycle: a scan of what a ref " +
				"reaches, a build sorting some live packs into kept and dropping " +
				"ones, their begin, a rescue of what a push revived meanwhile and " +
				"their commit — by `outcome`. `applied`: its commit landed, and " +
				"what no ref reached left the repository's live packs. `pending`, " +
				"`unknown`: a record was appended and is not yet known to have " +
				"applied — the next pass reads the rows and finishes it. " +
				"`not_due`: nothing was left to rewrite — a purge already served, " +
				"every live pack arrived within the two weeks' grace the weekly " +
				"expunge keeps whole, or what no ref reaches is a quarter of the " +
				"live bytes or less, or under 64 MiB — each but a served purge leaving the " +
				"repository unscanned on this node for a week. `stale`: a compaction, a " +
				"publication or a purge moved an input first, or the expunge was " +
				"finished elsewhere. `busy`: no maintenance slot or memory room " +
				"was free, another build of the repository was running on the " +
				"node, its copy's objects/ still held a begin's inputs, or its " +
				"committer was busy. `refused`: by the committer or before it " +
				"(`code_expunge_refused` says why: this node's memory, an object " +
				"store with no room, a build past what one record carries). " +
				"`failed`: a sync, a build, an upload or an answer failed " +
				"(`code_expunge_failed` names the step). Every outcome but " +
				"`applied` is tried again at a later pass, and a purge whose " +
				"repository holds more than one expunge rewrites at once is " +
				"served by several `applied`.",
		},
		{
			Name: CodeExpungeDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"stage"},
			Shows: "One stage of an expunge, by `stage`: `scan`, its sync and " +
				"its reads of what it keeps; `build`, its kept and dropping " +
				"packs' builds — a walk of the repository's whole history from " +
				"every tip it keeps, minutes at the largest; `upload`, every " +
				"output's upload at once; `begin`, its begin's decide and the " +
				"adoption of its kept packs; `rescue`, the sync past the begin " +
				"and the rescue's build and upload; `commit`, its commit's decide. " +
				"`build` and `rescue` climbing with the repository's history are " +
				"the walks; `upload` alone climbing is the object store.",
		},
		{
			Name: CodeExpungeDroppedBytes, Kind: KindCounter, Unit: UnitBytes,
			Shows: "Bytes the expunges this node ran took out of their " +
				"repositories' packs: each commit's dropping packs less the " +
				"rescue it kept of them. The store keeps them for code's " +
				"RetireGrace (a day) after the commit, so a reader already on one " +
				"finishes, and the collector deletes them at its next pass after " +
				"that.",
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
			Name: CodeCacheChained, Kind: KindGauge, Unit: UnitCount,
			Shows: "Repositories this node's git cache serves from an " +
				"installed published chain — the multi-pack-index layers, " +
				"each with its bitmap and commit-graph layer, that the " +
				"repository's maintainer wrote once for every git node and " +
				"this node fetched and placed — rather than in its own mode, " +
				"from the index and graph its background work writes itself. " +
				"Beside `crewlet.code.cache.repos`: the rest are repositories " +
				"no publication has reached yet, or whose chain this node " +
				"refused or has not installed (`crewlet.code.cache.layers`). " +
				"Falling all at once across the fleet, with `layers{outcome=" +
				"missing}` rising, is an object store that lost the layers' " +
				"parts — a restore carries none — which every maintainer " +
				"publishes again; a node serves correctly either way, only " +
				"slower while it waits.",
		},
		{
			Name: CodeCacheChecks, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"file", "outcome", "trigger"},
			Shows: "Files of this node's git cache held to their own " +
				"checksum — the trailer git writes at the end of every " +
				"pack and index file, the SHA-1 of the bytes before it — " +
				"because git checks none of them when it reads: a damaged " +
				"index file can answer wrong without a word, and a damaged " +
				"pack is sent to clients byte for byte. Before a copy this " +
				"process did not write answers its first request, every " +
				"index file it holds; and behind the serving, a SCRUB of " +
				"every file a copy holds, packs included — once after a " +
				"copy a previous process left first answers, daily while it " +
				"is used, and at once when a git process over it reports a " +
				"pack it could not read or crashes; and when a request — " +
				"or the node's own sync — fails on a pack or an object, " +
				"every index file again, " +
				"whatever was checked before, then every file if that " +
				"repaired nothing. By `file` " +
				"(`commit-graph`, `multi-pack-index`, `bitmap`, `midx-rev`: " +
				"files the node derives from its packs; `layer-midx`, " +
				"`layer-bitmap`, `graph-layer`: a published layer's files as " +
				"its repository's maintainer wrote them, the `.midx` and the " +
				"`.graph` held to the checksum their names carry, and one " +
				"that fails is fetched again with every layer above it; " +
				"`layers-chain`, `graph-chain`: the two chain files the " +
				"node writes over the layers it installed, held line for " +
				"line to them and written again where they differ; " +
				"`provenance`, a run " +
				"of the node's index of which push introduced each commit, " +
				"which no git reads, held to its own SHA-256 trailer — a run " +
				"a previous process left before anything first reads it, " +
				"behind the serving, and every run by a scrub; " +
				"`codeindex-segment`, `codeindex-tip`: a segment and a tip " +
				"map of a repository's code index, which no git reads " +
				"either, held to their own SHA-256 trailers by a scrub; " +
				"`idx`, `rev`: a " +
				"pack's index and reverse index, stored with its push; " +
				"`pack`, the pack itself, read only by a scrub), `outcome` " +
				"(`sound`; `damaged` — a derived file dropped to be written " +
				"again, a stored one fetched again with its pack, one " +
				"download; `unreadable`, a read of it that failed — " +
				"repaired as though damaged, since a file the disk cannot " +
				"read is one git cannot serve, and never a refused read " +
				"slot or an ended request, which decide nothing and are not " +
				"counted; `replaced`, " +
				"the file renamed over or removed while it was read, so no " +
				"verdict is its) and `trigger` (`first`, a copy's first full " +
				"sync in this process, which reads every index file a " +
				"previous process left, or the scrub after it, which reads " +
				"every pack and every file that sync did not, or the check of " +
				"a `provenance` run a previous process left or a rebuild " +
				"linked; `unvouched`, a " +
				"later full sync reading only what this process has neither " +
				"written nor checked since; `scrub`, a used copy's daily " +
				"scrub; `report`, a scrub a git process's report or crash " +
				"made due; `forced`, the in-place repair a pack's or an " +
				"object's failure asks first — a request's git, or the " +
				"node's own sync's — every index file held to its checksum " +
				"whatever was checked before; `request`, the scrub that " +
				"repair runs at once when it found nothing, when it is " +
				"skipped (a forced check having run within the hour with " +
				"nothing repaired since, whatever it found and however its " +
				"sync ended, or every pack it named held off after the " +
				"store refused it), or when the sync's own git met the damage " +
				"again past it, and a pack a " +
				"git process named in words, confirmed against its row and " +
				"fetched again). Any `damaged` is a disk losing data, or something " +
				"outside the engine writing into the cache: the copy repairs " +
				"it, and a verify of the repository names what else that " +
				"disk holds damaged. The answers the cache keeps of seats' " +
				"reads are no file here: every read of one holds it to its " +
				"own SHA-256, and one that fails is " +
				"`crewlet.code.cache.memo.lookups`' `corrupt`.",
		},
		{
			Name: CodeCacheCheckDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"trigger"},
			Shows: "How long one check of a copy's index files, or one scrub " +
				"of every file it holds, took, by `trigger` (`first`, " +
				"`unvouched`, `scrub`, `report`, `forced` or `request`, as " +
				"`crewlet.code.cache.checks`), counted only where it read " +
				"something. A check is what a copy's first request after a " +
				"restart waited for beside its sync — about a quarter of the " +
				"copy's pack bytes read; a scrub runs behind the serving and " +
				"nobody waits for it, reading every byte the copy holds. " +
				"Both hash on one core, a read slot a 64 MiB piece, and at " +
				"the volume's own throughput when read cold.",
		},
		{
			Name: CodeCacheReports, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"part", "outcome"},
			Shows: "Damage any git process over this node's git cache " +
				"named — whether its request failed or git read around the " +
				"damage and answered — as the cache took it, by the `part` " +
				"named (`commit-graph`, `multi-pack-index`, `chunk`: a " +
				"derived file of either, dropped by a repair round with no " +
				"request waiting and written again at once — on a copy " +
				"serving from an installed published chain " +
				"(`crewlet.code.cache.chained`), which keeps its files on a " +
				"report, the copy's scrub made due instead, which holds every " +
				"file of the chain to its checksum and fetches again a layer " +
				"that fails; `pack`, " +
				"`object`: stored damage, and `hint`, a pack git named as " +
				"holding a corrupt object — git 2.56's bare message, or the " +
				"pack git 2.55's report of that damage names — each making " +
				"the copy's scrub due at once, " +
				"which holds every file it holds to its checksum and fetches " +
				"again what fails; `crash`, a git process dying of a signal " +
				"the node did not send — what a damaged reverse index or " +
				"bitmap does to a clone, naming nothing — which makes the " +
				"scrub due the same way; any of these four named by the git " +
				"a request ran, which repairs what that git fails with " +
				"before its first byte, is held for that request until it " +
				"asks its repair or lets the copy go, and counted then — " +
				"and one the node's own sync's git names, for that sync's " +
				"round until it is answered) and " +
				"its `outcome`: `queued`, a " +
				"repair asked — a round, or the scrub; `deferred`, asked " +
				"while a check of the repository ran, repaired once it ends; " +
				"`stale`, not dropped — the node wrote the file after the " +
				"process that named it began, so the report was of the file " +
				"it replaced, or the copy holds no file of the part named any " +
				"more — and, for a `pack` or an `object`, a failed " +
				"request whose process began before the node last wrote a " +
				"repair into the copy, answered from the copy as it stands " +
				"once no round is repairing it and run once more rather " +
				"than repaired further, or a sync whose own git met it so, " +
				"synced again; `unrouted`, a pack, an object or a hint named " +
				"by a push's own throwaway repository, which may be the " +
				"client's pack, or a derived file named by a compaction's " +
				"build, whose store of the packs it merges holds none; " +
				"`again`, a derived file named damaged within " +
				"a day of its last drop for damage, whatever the node wrote " +
				"of it between: dropped and left to the ordinary schedule " +
				"rather than written again at once; `cooldown`, a stored " +
				"report or a crash within an hour of a scrub of the copy " +
				"that found nothing, which makes no other; `covered`, one " +
				"within an hour of a scrub whose findings still wait for " +
				"their repair, which is its answer; `request`, one held for " +
				"the request whose git named it — or for the sync whose own " +
				"git did — and answered by the repair that request or that " +
				"sync's round asked — its in-place round, its scrub and, " +
				"past both, its rebuild — with no scrub of the copy made due " +
				"beside it (one the request did not repair — it failed past " +
				"its first byte, failed in a way the node does not repair, " +
				"or never failed — is counted under the others as the " +
				"request lets the copy go). `queued` rising is a disk " +
				"losing data under the cache; `again`, a disk that keeps " +
				"losing it; `cooldown` rising, a git process failing on " +
				"something the copy's checksums do not show; `request` " +
				"rising, requests failing on damage they had repaired " +
				"under them.",
		},
		{
			Name: CodeCacheHints, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Packs of this node's git cache that a git process named " +
				"as holding a corrupt object, held to the row that names " +
				"them, one a naming, by the verdict that answered it " +
				"(`outcome`). git 2.56 says a packed object is corrupt " +
				"through a bare message, which nothing reads as damage — " +
				"beside whatever else of the damage it reports, and alone " +
				"for an object whose header states the wrong size — and git " +
				"2.55 through a report of its own, read as a pack's damage. " +
				"A naming is hashed and counted here only where a " +
				"repair waits on its verdict, on either git: a request's git " +
				"in the copy that failed before its first byte, and the " +
				"node's own sync's git — the peel of the tags it sets — " +
				"each with no damage it repairs other than a pack's or an " +
				"object's. Any other naming — by a run that did not fail on " +
				"it, a request that failed past its first byte, or a push's " +
				"own throwaway repository — is hashed by nobody: it makes " +
				"the copy's scrub due, counted only as " +
				"`crewlet.code.cache.reports{part=hint}`. `confirmed`, the " +
				"pack's bytes are not its row's, and that pack is fetched " +
				"again IN PLACE with one download, whichever git named it " +
				"(counted under `crewlet.code.cache.rebuilds{reason=stored}`), " +
				"the repository read whole into a new directory (`corrupt`) " +
				"only when that repair and a scrub of the copy find nothing " +
				"to fetch again; `sound`, its bytes are the row's — after " +
				"2.56's message nothing is repaired, and after 2.55's report " +
				"the damage it names is repaired as any pack's is, with no " +
				"pack named to the repair; `unknown`, no row names the pack " +
				"at the log's head; `unread`, no verdict — the rows or the " +
				"pack could not be read in time, or its files moved under " +
				"the hash; `gone`, the request left before the verdict, the " +
				"hash going on for the next. `confirmed` rising is disk " +
				"damage a verify of the repository names; `sound` rising is " +
				"git naming damage the pack does not hold — a git bug, or, " +
				"on 2.56, a request that made git print the sentence — which " +
				"costs one hash of the pack's files and, on 2.56, no repair " +
				"at all; `unread` and `gone` rising is a node too busy or a " +
				"disk too slow to decide a hash inside its bound. Beside " +
				"`crewlet.code.cache.hint.hashes`, the namings one hash " +
				"answered.",
		},
		{
			Name: CodeCacheHintHashes, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Hashes of a pack of this node's git cache that git named " +
				"as holding a corrupt object (`crewlet.code.cache.hints`), by what " +
				"each decided (`outcome`): `confirmed`, `sound`, `unknown` and " +
				"`unread`, as there. One a pack's files however many namings " +
				"wait on it — a naming of a pack being hashed waits for that " +
				"hash, and a verdict is kept for as long as the pack's files " +
				"are the ones it was reached on — each run on the cache's own " +
				"time rather than a request's, a read slot a 64 MiB piece, and " +
				"bounded by the hash alone. Namings less hashes is what " +
				"waiting saved; `unread` here is a hash the node could not " +
				"fit, which the next naming starts again.",
		},
		{
			Name: CodeCacheLayers, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Published layers of a repository's chain — a " +
				"multi-pack-index layer with its bitmap and commit-graph " +
				"layer, written once by the repository's maintainer for every " +
				"git node — that this node's git cache installed, carried, " +
				"cut or refused, by `outcome`. `installed`: fetched from the " +
				"object store, held to its row and placed — by the full sync " +
				"that first read the chain, before it answered, or behind the " +
				"serving by the installer after a delta that named it. " +
				"`adopted`: placed from the files the maintainer itself wrote " +
				"on this node, no download. `kept`: carried into the " +
				"directory a placement or a rebuild made, the chain moving " +
				"around it. `cut`: taken out — the rows no longer name it, a " +
				"pack it names left, one of its files failed its checksum, or " +
				"a layer below it went. `missing`: a part the object store " +
				"holds nothing for while the log's head still names the " +
				"layer — after a restore, which carries no layer, every one, " +
				"until each maintainer publishes again. `corrupt`: bytes that " +
				"are not its row's, files its row does not describe, or a " +
				"placement whose one bitmapped read through it failed — a " +
				"layer over the wrong base. `newer_git`: written by a git " +
				"newer than this node's, which it would not read. `failed`: " +
				"a try the store failed to answer, this node's disk failed a " +
				"write of, whose missing part the log's head could not be " +
				"read about, or whose placement's read through the bitmap " +
				"could not run — no read slot free within ten seconds, or " +
				"past its deadline — by a full sync, which leaves the layer " +
				"to the installer, or by the installer, which tries again a " +
				"minute later, doubling to thirty: counted once a try. A " +
				"refused layer, and every one above it, is never installed " +
				"while the rows name it: the copy serves from the layers " +
				"below it, or in its own mode, correctly and slower, until " +
				"the maintainer's next publication replaces it.",
		},
		{
			Name: CodeCacheLayerInstall, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long one published layer took this node's git cache " +
				"to install, from its staging to its placement — its parts' " +
				"downloads, each beside the others in one of the node's " +
				"fetch slots, its files' syncs and its check against its " +
				"row, and the placement's bitmapped read through it — by " +
				"`outcome` (`installed`, `adopted`, `missing`, `corrupt`, " +
				"`failed`, as `crewlet.code.cache.layers`). A full sync " +
				"fetches its layers beside its packs and a cold copy's first " +
				"answer waits for them; the installer, behind the serving, " +
				"keeps at most half the node's downloads for itself. A " +
				"p95 climbing with `installed` steady is an object store " +
				"slowing down; `failed` climbing, one failing.",
		},
		{
			Name: CodeCacheMaintenanceRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason", "step"},
			Shows: "Steps of the background work on a repository this node's " +
				"git cache found it will not run, counted once for each copy " +
				"of a repository the cache holds and each step, by `reason` " +
				"and `step`. `memory`: the step's charge — its own figure an " +
				"object of what it walks, and 240 more an object of the " +
				"repository for room beside it for a clone of the repository " +
				"(of the packs merged, for `compact`) — is more than this node " +
				"can give its background work, which " +
				"is its memory (its cgroup's limit where one is set) less what " +
				"it keeps for its git lanes, the store, the broker and the " +
				"engine. `bitmap` (600 bytes an object of the repository): " +
				"the repository is indexed without one, and a clone walks its " +
				"history; `index` (80): it is served from its packs' own " +
				"indexes, a round's index included; `graph` (67, and 100 MiB " +
				"whatever the repository's size for its changed-path filters): its walks go " +
				"by date, and a push of a commit dated before its parent walks " +
				"every commit; `tip-graph` (50 an object of the tail, the " +
				"packs pushed since the repository's last publication, and " +
				"100 MiB whatever its length): a copy " +
				"serving from a published chain walks those packs' commits by " +
				"date. Slower, never wrong. `compact` (550 bytes an object of " +
				"the packs merged, beside a transfer's room of them — 240 " +
				"bytes an object merged, never of the whole repository, whose " +
				"size alone would refuse every merge of a large one — and six " +
				"times max_blob_bytes for the build's delta search): the " +
				"repository's maintainer cannot merge its small packs, " +
				"so its pack count grows with its pushes until a git node with " +
				"more memory maintains it or this one is given more — and at " +
				"the pack bound its pushes are refused, which " +
				"code_compaction_behind raises. `publish` (600 bytes an object " +
				"of the repository for the layer's bitmap, beside the clone's " +
				"room): the repository's maintainer cannot publish its next " +
				"layer, so every pack pushed since its last publication is served " +
				"outside every bitmap, and every one pushed after. `expunge` (550 " +
				"bytes an object of the packs an expunge sorts, beside the room " +
				"for a clone of the whole repository, whose history its build " +
				"walks, and its delta search): a person's purge of the " +
				"repository waits until a git node with the memory maintains it " +
				"(`code_cache_expunge_refused` names it). Rising is " +
				"repositories outgrowing the node; the code_maintenance_refused " +
				"alarm stands while any bitmap or publication is refused, and " +
				"never for a tip, whose copy is served through its chain's bitmap.",
		},
		{
			Name: CodeCacheMaintenanceRefused, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"step"},
			Shows: "Repositories this node's git cache cannot write a bitmap " +
				"(`step` `bitmap`), a commit-graph (`graph`) or, serving from " +
				"a published chain, its own tip commit-graph over the chain " +
				"(`tip-graph`) of now, and repositories this node maintains whose " +
				"next layer it cannot publish (`publish`), because one needs more " +
				"memory than the node can give its background work — as its latest " +
				"pass found them. Each refused a bitmap is indexed without one; each " +
				"refused a commit-graph walks its history by date, so a push " +
				"of a commit dated before its parent walks every commit; each " +
				"refused its tip walks the commits pushed since its last " +
				"publication by date — slower, never refused; each refused its " +
				"publication serves every pack pushed since its last one outside " +
				"every bitmap. A bitmap, a graph or a publication above zero raises " +
				"code_maintenance_refused, since a repository refused its graph is " +
				"refused its bitmap too; a tip raises nothing, its copy served " +
				"through its chain's bitmap. Falling back is a repository purged, " +
				"evicted or rebuilt smaller, a node given more memory, which the " +
				"cache reads again every ten seconds, or a publication — by this " +
				"node or by another with the memory, which takes its maintenance " +
				"over — that its copy installs: its bitmap and graph are then the " +
				"chain's, and its tail shorter, which lifts a tip refused for its " +
				"tail's length but never one refused for its repository's size, " +
				"since a tip keeps room beside its tail for a walking clone of the " +
				"whole repository.",
		},
		{
			Name: CodeCacheMemoBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "What this node's git cache holds of answers to seats' reads " +
				"by object id — a page of a listing, a file, a diff, a " +
				"comparison — kept so the next read that asks the same is " +
				"answered from a file rather than from git: part of " +
				"`crewlet.code.cache.bytes`, each answer counted at the whole " +
				"disk blocks it takes. Held, with the pages reads are still " +
				"cutting for it, to a sixteenth of the cache's budget before " +
				"any repository is evicted — and no repository is evicted for " +
				"what it holds past that — by dropping the sixteenth of it used " +
				"least recently at once, so on a node seats read busily it " +
				"moves between fifteen and sixteen sixteenths of that share, " +
				"less what is being cut; falling to nothing at once is a " +
				"history restored or recreated under the node, which drops " +
				"every answer read from it.",
		},
		{
			Name: CodeIndexBuilds, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Looks this node's indexer took at the code index of a " +
				"`search: indexed` repository — each one every minute, and one " +
				"whose refs a round moved at once — by `outcome`. `built`: the " +
				"index now answers for the " +
				"default branch's tip, only the blobs the tip brought read. " +
				"`current`: it already did, or another build stored first. " +
				"`none`: the repository keeps no index here — its search is " +
				"grep, its default branch names no commit, or this node is not " +
				"among the first two git nodes of its order, which keep it " +
				"(an index this node kept then dropped), so on a fleet of more " +
				"than two git nodes most looks are `none`. " +
				"`failed`: the copy could not be synced or read, and the next " +
				"look tries again (`code_cache_index_failed` says why). A " +
				"steady `built` on a quiet company is an index the share keeps " +
				"trimming: see `crewlet.code.index.bytes`.",
		},
		{
			Name: CodeIndexBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "What this node's git cache holds of code indexes — the " +
				"trigram segments and tip maps of the repositories searched by " +
				"index that it holds — part of `crewlet.code.cache.bytes`. Held " +
				"to a third of the cache's budget before any repository is " +
				"evicted, by dropping the index of the copy used least recently " +
				"whole; the indexer builds a dropped one again at its next look, " +
				"so a gauge pinned at the third is a node keeping more " +
				"indexes than its budget holds.",
		},
		{
			Name: CodeSearchQueries, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"via"},
			Shows: "Code searches this node asked, by `via`: `index` and `grep`, " +
				"a search of one repository read through its code index or with " +
				"git grep; `scatter`, a search of several, asked of every git " +
				"node holding them at once. A steady `grep` share of " +
				"one-repository searches of indexed repositories is indexes that " +
				"do not keep up with the pushes: see `crewlet.code.index.builds`.",
		},
		{
			Name: CodeSearchDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"via"},
			Shows: "How long a code search this node asked took, from its " +
				"asking to its answer, by `via` as `crewlet.code.search.queries`. " +
				"A scatter takes one round, or two where a git node did not " +
				"answer the first, each at most 9 s: a scatter near 9 s is a git " +
				"node that did not answer, which its answer's coverage names.",
		},
		{
			Name: CodeSearchCoverage, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Repositories a code search this node asked did not search, " +
				"or searched more slowly, by `reason`, as the last git node " +
				"asked for it answered: `not_answering`, a git node that did " +
				"not answer for it; `building`, its index being built, with no " +
				"other git node in its order to ask; `stale`, a node behind the " +
				"log or the company's configuration; `not_permitted`, a grant the " +
				"answering node's chart refused; `no_ref`, no such ref in it; " +
				"`grep_not_named`, a `search: grep` repository an unfiltered " +
				"search leaves out; `not_indexed`, an indexed repository read " +
				"with git grep. A steady `not_answering` is a git node to look " +
				"at; `grep_not_named` alone is a company's choice.",
		},
		{
			Name: CodeImportChunks, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "The chunks this node, as a repository's home, proposed " +
				"of an import from another host, by `outcome`: `applied`, the " +
				"chunk's packs and refs landed; `ended`, refused for a limit " +
				"the source is past (`repo_size`, `repo_refs`, `ref_conflict`, " +
				"`pack_too_large`), which ends the import with its error; and " +
				"`failed`, refused for anything else — a busy committer, rows " +
				"that moved — or not seen applied, the import tried again from " +
				"its rows a minute later and longer after each failure in a " +
				"row. `failed` climbing on one node with no `applied` beside it " +
				"is an import that cannot make progress: its " +
				"`code_mirror_attempt_failed` lines say why.",
		},
		{
			Name: CodeMirrorRuns, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"dir", "outcome"},
			Shows: "One attempt to bring a mirror level with the other host, " +
				"by `dir` (`pull`: a pull mirror's sync from its source, run " +
				"by its home every 15 minutes) and `outcome`: `synced`, a sync " +
				"landed what the source moved; `current`, the source moved " +
				"nothing; `failed`, the source unreachable, its credential " +
				"refused, or a step refused or not seen applied — the mirror " +
				"tried again a minute later, and longer after each failure in " +
				"a row, up to its interval. A mirror whose source went away " +
				"shows as `failed` four times an hour, every one with its " +
				"`code_mirror_attempt_failed` line.",
		},
		{
			Name: CodeCacheDroppingBytes, Kind: KindGauge, Unit: UnitBytes,
			Shows: "What this node's git cache holds of packs an expunge's " +
				"begin has left dropping — what no ref reached when the " +
				"expunge was planned, kept apart from the live packs so a " +
				"client still reading one of their objects is served it and " +
				"no push may rely on one: part of `crewlet.code.cache.bytes`. " +
				"Zero but while an expunge is open on a repository this node " +
				"holds; it falls back as each expunge commits and the next " +
				"sync of the repository takes the dropping packs out. Held " +
				"up for long is an expunge that began and does not commit — " +
				"its maintainer's rescue failing — which also refuses every " +
				"layer the repository would publish meanwhile.",
		},
		{
			Name: CodeCacheMemoLookups, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Seats' reads by object id that this node's git cache looked " +
				"up among the answers it keeps, by `outcome`: `hit`, answered " +
				"from the answer kept, with no git run; `miss`, none kept, so " +
				"the read ran its git and keeps what it cut; `corrupt`, a kept " +
				"answer that did not read back whole — removed, and computed " +
				"again, so rising is this node's disk damaging what it " +
				"holds. Misses climbing while `crewlet.code.cache.memo.bytes` " +
				"sits at its sixteenth of the budget, with few repositories " +
				"evicted, are a share too small for the answers seats ask " +
				"again.",
		},
		{
			Name: CodeCacheProvenanceBackfilled, Kind: KindCounter, Unit: UnitCount,
			Shows: "Pushes this node's git cache read into its indexes of which " +
				"push introduced each commit BEHIND the serving: the range a " +
				"full sync passed over — the pushes made while the node was " +
				"down, every node holding a copy reading its own — and a " +
				"repository's history back to the start of the push history " +
				"the data nodes keep, read only by the node that answers a " +
				"seat's reads of it as its home, once a seat has asked. Every " +
				"sync folds the pushes it applied into the index before it " +
				"answers, and none of those is counted here. A page at a " +
				"time, each followed by a pause of twice its own duration " +
				"times the fleet's git nodes for each data node, so the " +
				"fleet's backfills hold under half of one reader of each data " +
				"node. Rising after a restart is the gap closing; rising for " +
				"hours on one node is one large history read once — " +
				"`crewlet.code.cache.provenance.behind` says what is left.",
		},
		{
			Name: CodeCacheProvenanceBehind, Kind: KindGauge, Unit: UnitCount,
			Shows: "Repositories whose index of which push introduced each " +
				"commit is behind on this node: a range a full sync passed " +
				"over that the backfill has not yet read, or a history a " +
				"seat's reads asked this node, as the repository's home, to " +
				"read and that it has not yet read to its start. A copy " +
				"nobody asked about is never read back further than its first " +
				"full sync, so it is never counted. Until it is zero, an " +
				"answer naming a commit's push says which range it does not " +
				"hold rather than answering that no push introduced it. " +
				"Staying above zero is a backfill that cannot read the data " +
				"nodes — its warnings, code_cache_backfill_failed, say why — " +
				"or a history larger than its pace reads in the time it has " +
				"been asked for.",
		},
		{
			Name: CodeCacheRebuildRefusals, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason", "rule"},
			Shows: "Rebuilds this node's git cache refused, by the `reason` " +
				"the git process gave (`corrupt`, `missing` or `shape` — a " +
				"shape only once reading the directory whole in place left " +
				"the pool still refusing it, since that repair downloads " +
				"nothing and is never refused) and " +
				"the `rule` that refused it. `unchanged`: the repository's " +
				"cache holds exactly what a rebuild that did not end the " +
				"failure read less than a day ago, and nothing has written " +
				"into it since, so another would make the same directory — " +
				"never for `shape`, whose refusal is itself something " +
				"written into it; " +
				"`cooldown`: the last attempt began too recently — ten " +
				"minutes after the first of a line of attempts that keep " +
				"failing, twice as long after each one after it, at most a " +
				"day, a line starting again once a day passes with no " +
				"rebuild asked for. Each refusal fails the request that asked " +
				"at once rather than downloading the whole repository again. " +
				"Rising at all is " +
				"damage no rebuild repairs — a pack the object store lost or " +
				"holds damaged, rows naming an object no pack holds, a " +
				"directory something keeps writing into — which " +
				"a verify of the repository names; flat while " +
				"`crewlet.code.cache.rebuilds` rises is damage each rebuild " +
				"ended.",
		},
		{
			Name: CodeCacheRebuilds, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"reason"},
			Shows: "Repositories this node's git cache read whole from the " +
				"rows again, by `reason`. `corrupt`, `derived`, `missing` and " +
				"`shape` are a repair a git process asked for — a request's " +
				"or a sync's own — read from the format strings of git's " +
				"own reports, which no request can write. `corrupt` and " +
				"`missing` are a rebuild into " +
				"a new directory beside the damaged one: it found a pack or " +
				"an index it could not read, or an object a row names that " +
				"no pack held — the last step of its repair, whichever git " +
				"named it, a request's or a sync's own, after an in-place " +
				"repair and a scrub of every file the copy holds found " +
				"nothing to fetch again, so it links every pack the scrub " +
				"found whole and downloads only the rest, nothing at all " +
				"when the damage is in the rows. `derived` is a " +
				"damaged derived file (the multi-pack-index with its bitmap " +
				"and reverse index, the commit-graph, or a chunk-format file " +
				"of either), named by any git process over the copy and " +
				"repaired IN PLACE — the same directory read whole again, " +
				"the files the damage named DROPPED so the round or the " +
				"maintainer rewrites sound ones over the same packs, none " +
				"downloaded — since those are derived files, not the " +
				"repository; damage the drop does not end escalates to " +
				"`corrupt`; a pack or object failure — a request's or a " +
				"sync's own — whose in-place repair only dropped derived " +
				"files is counted `derived` too. `shape` is the " +
				"pool refusing to run git there because the directory is " +
				"not the shape the engine writes (its config edited, a " +
				"commondir, an objects directory not its own, or an " +
				"alternates file planted in it), repaired IN PLACE — the " +
				"same directory read whole again, every pack it holds " +
				"reused and none downloaded — and rebuilt into a new one " +
				"only when the pool still refuses it after that; " +
				"any of them rising is disk or object-store damage, or " +
				"something outside the engine writing into the cache, which " +
				"a verify of the repository names — every pack checked " +
				"against its row, the ref file against the digest written " +
				"with it, the directory's shape, " +
				"and git's connectivity check. `verify` is that check, asked " +
				"to repair what it found: read whole IN PLACE, as `shape` " +
				"is, where everything it found is the copy's own " +
				"directory out of place — a file or entry the engine does " +
				"not write there, a pack no row names, its shape — every " +
				"pack, index, bitmap and graph kept and none downloaded, " +
				"but a pack no row names, which leaves as the copy moves " +
				"into its next directory, the index and graph built over " +
				"it along (`crewlet.code.cache.swaps`); " +
				"rebuilt into a new directory otherwise, or for what that " +
				"read leaves — a shape written past its sweep included, " +
				"never counted under `shape` — each counted once it has " +
				"read the copy whole, so a read in place that shape " +
				"stopped is not. `stored` is " +
				"no read of the rows' history at all: a pack whose stored " +
				"files — the pack, its index or its reverse index — a check " +
				"before serving or a scrub behind it found failing their " +
				"own checksum, a full sync found at another size than " +
				"their row's, a full sync found the copy had lost (named " +
				"by the rows at a position it had read), " +
				"or a git process named in words " +
				"and its row confirmed, fetched again IN PLACE with one " +
				"download a pack, renamed over the damaged files, counted " +
				"once a sync that did it — the first repair of a pack or " +
				"object failure, a request's git or a sync's own, which " +
				"reaches `corrupt` or " +
				"`missing` only when it and a scrub find nothing; rising is " +
				"a disk losing data under the cache. " +
				"`stream` is " +
				"the code log " +
				"RECREATED under the cache (provisioned anew, its stamp not " +
				"the one the cache read under); `rewound` is the log gone " +
				"BACK under it on the same stream — a broker restored from " +
				"an older copy — seen in a read at the log's head below a " +
				"position the cache had read. Either of those counts every " +
				"repository on the node once, as its next sync converges to " +
				"the rows the log holds rather than taking a delta from a " +
				"position that log never had or has since given to other " +
				"records. A repository marked to be read whole for more " +
				"than one reason at once is read once and counted once, " +
				"under the first: damaged derived files found on a " +
				"copy already marked for its shape or for the log are " +
				"counted under that reason, dropped all the same " +
				"(logged `code_cache_derived_repaired`).",
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
				"after a restart, a log recreated under it), from its round's " +
				"start: the read of its rows, its downloads and its writes, " +
				"and for a `full` sync its wait for one of the eight " +
				"full reads the node runs at once " +
				"(`crewlet.code.cache.full_rounds`). A delta p95 climbing is a " +
				"node falling behind its pushes. A full sync's time is mostly " +
				"its downloads, sixteen at once, on a cache that never held " +
				"the repository — but on a node bringing up many " +
				"repositories at once, above all one restarted over the cache " +
				"it kept, which downloads nothing, it is mostly that wait: " +
				"`full_rounds` `waiting` climbing says so, not the object " +
				"store.",
		},
		{
			Name: CodeCacheSwaps, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"graph"},
			Shows: "Repositories whose cache on this node moved into its next " +
				"directory because packs left it — a compaction retired them, " +
				"or a full read found pack files no row names — or because a " +
				"published chain was placed in a copy a request may be " +
				"reading, built by hard links of every pack still live beside " +
				"the old directory, which the requests reading it keep until " +
				"they end; by what became of the commit-graph. `kept`: every " +
				"pack that left was a compaction's, which drops no commit, so " +
				"the graph came along and the multi-pack index and its bitmap " +
				"alone are written again. `dropped`: the copy held a graph — " +
				"its own, or its chain's graph layers — and the next " +
				"directory holds none: a pack left that no retirement " +
				"accounts for — the code log recreated or restored under the " +
				"node, or a full read that could not say which were retired " +
				"— or the chain was cut whole, and the background indexing " +
				"writes the graph again at once. `none`: the copy held no " +
				"graph to keep — not written yet, or past what the node's " +
				"memory holds for the repository — so nothing was lost. " +
				"`chain`: the next directory serves from an installed " +
				"published chain — one placed, or carried while packs left — " +
				"whose commit-graph layers came with it, so nothing is " +
				"written again. `failed`: the swap could not be " +
				"made — a link refused, the disk full — and the old directory " +
				"goes on serving, every live pack in it, until the " +
				"repository's next sync tries again (`code_cache_swap_failed` " +
				"names the step). `failed` rising is a cache volume out of " +
				"room or links; `dropped` beside a quiet log is full reads " +
				"that cannot see their retirements, every one a graph " +
				"rewritten.",
		},
		{
			Name: CodeCacheFullRounds, Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"state"},
			Shows: "Full reads of a repository's rows this node has against " +
				"the estate now, by `state`: `running`, each holding one of " +
				"the node's eight slots from its first page to its last, and " +
				"`waiting`, in line for one, first come first served. A full " +
				"read is every repository's first sync after the node starts " +
				"or joins, a rebuild, every repository's next sync once the " +
				"code log was recreated or went back under the cache, and a " +
				"check's or a hint's read of the same rows; a sync after a " +
				"push reads only what moved and takes no slot. Every page of " +
				"a full read is one read on a data node — one of the 64 " +
				"answers its lane gives the whole fleet's reads at once, and " +
				"one of the reader connections every one of those reads " +
				"takes, eight on the smallest data node (on a node that holds " +
				"the data, its own store's) — so the slots keep a node coming " +
				"up with hundreds of repositories to what the smallest data " +
				"node scans at once, rather than every page while the " +
				"fleet's reads wait behind them. `running` at eight while " +
				"`waiting` climbs is a node bringing up more repositories " +
				"than it reads at once: each waits inside its own request's " +
				"time, and they come up eight reads at a time — about four " +
				"seconds for 500 repositories of a full page of refs each, " +
				"from a data node of eight cores.",
		},
		{
			Name: CodeCacheReadPageDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"kind"},
			Shows: "How long one page of a repository's rows took the estate " +
				"to answer this node's cache, by `kind` (`full`: a page of a " +
				"whole repository's rows — a full sync's, a check's or a " +
				"hint's; `delta`: a page of what moved since the cache's " +
				"position), from the ask to the answer: the data node's lane " +
				"and reader connection, its scan of the page and the crossing " +
				"— or, on a node that holds the data, its own store's. A full " +
				"page of refs, ten thousand of them, takes about 60 ms to " +
				"scan, and a repository of a few hundred refs is one page of " +
				"a few milliseconds: a `full` p95 far under 60 ms is full " +
				"reads holding the node's eight slots " +
				"(`crewlet.code.cache.full_rounds`) briefly, and one climbing " +
				"past it is a data node whose readers queue — " +
				"`crewlet.store.pool.wait` there says so. It is production's " +
				"reading of the page the node's eight full reads at once " +
				"were measured against.",
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
		{
			Name: CodeGitShareWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"share"},
			Shows: "How long a clone, fetch or push waited for its transfer " +
				"slot on this node, by the `share` it waited in (`person`: the " +
				"holder of a human seat — a person through any credential of " +
				"theirs, or a Tier A token or service account the identity " +
				"directory binds to a human seat nobody else holds; " +
				"`machine`: a credential acting as nobody, a Tier A token or a " +
				"service account's machine token bound to no seat; `run`: a " +
				"box's git on `/box/git/` — a coding box's, with its run's " +
				"credential, or a check's box's, with its check's). " +
				"When every slot is held a released one goes to the class " +
				"furthest below its weighted share of what the lane spent " +
				"lately (person 4 : machine 2 : run 1), so `machine` climbing " +
				"while `person` stays flat is the share doing its job — CI " +
				"waiting behind CI, not people behind CI — and `person` and " +
				"`machine` both climbing is a lane too narrow for its traffic: " +
				"more memory and a wider " +
				"`git.max_transfers`, or more git nodes. Coding boxes together " +
				"hold at most half the lane, so `run` climbing alone is boxes " +
				"waiting behind each other at that bound, never a person " +
				"behind a box.",
		},
		{
			Name: CodeGitShare, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"share"},
			Shows: "What each `share` of this node's transfer lane has spent " +
				"lately, as the scheduler reads it: core-seconds, every run " +
				"charged the greater of the CPU it spent and the time it held " +
				"its slot at the share of a core one slot stands for — the " +
				"node's cores over its transfer slots, never more than one: a " +
				"quarter at four slots a core, a whole core where memory or " +
				"`git.max_transfers` leaves fewer slots than cores — decayed " +
				"over a minute, the figure the next contended slot is granted " +
				"by. Set at each " +
				"admission and release, so a lane idle since holds its last " +
				"reading. One class's reading far above its weight's share of " +
				"the total while the others' waits are short is a class alone " +
				"on the lane, which runs it at full width — `person` and " +
				"`machine` alike, while `run`, a coding box's, holds at most " +
				"half of it.",
		},
		{
			Name: CodeSeatworkHeld, Kind: KindGauge, Unit: UnitCount,
			Shows: "Seat git work running at once on this git node, of half its " +
				"read lane: the units of the node's seat git-work share held " +
				"now — server-side reads of a repository by object id running " +
				"their git, the engine's own among them, and the builds of " +
				"engine writes, a seat's or a person's commit made through a " +
				"tool. The share is half of " +
				"`git.max_processes` " +
				"(at least one), and the other half of the read lane stays the " +
				"node's own and its persons' — a push's scans, a sync's peel, " +
				"a fetch's check of its wants. Pinned at the share while the " +
				"count of `crewlet.code.seatwork.waits{outcome=unready}` " +
				"climbs is a node whose seats ask more git than half its " +
				"cores, and sends the rest to another git node.",
		},
		{
			Name: CodeSeatworkWaits, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome", "work"},
			Shows: "How long every unit of this git node's seat git-work " +
				"share asked for waited, by `outcome` — `granted`, taken " +
				"within two seconds; `unready`, none free in two seconds, " +
				"answered so the asker is served by another git node, its " +
				"wait the whole two — and by `work` (`read`: a seat's or a " +
				"person's server-side read of a repository by object id; " +
				"`write`: the build of an engine write, a commit made through " +
				"a tool; `engine`: the engine's own read — a revision's facts " +
				"or a checks file a repository's home reads, or a read made " +
				"for a check). One whose " +
				"caller went away while it waited is not counted. The wait " +
				"is a whole term of the attempt a seat's read or write is " +
				"given on one git node, so the `granted` tail nearing two " +
				"seconds is a node about to send its seats' work on; the " +
				"`unready` count is a node at its share for whole seconds: " +
				"more git nodes, or more cores and a wider " +
				"`git.max_processes`, which widens the share with the lane.",
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
		{
			Name: CodeHostSpoolWaits, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "Fetch requests large enough to spool — a partial clone's " +
				"lazy fetch past a mebibyte of wants — that could not start " +
				"spooling, or take room for the rest of their body, at once on " +
				"this node and waited their turn, by `outcome`: `granted`, " +
				"served after its wait; `expired`, answered 503 after waiting " +
				"55 s unserved; `abandoned`, its request ended while it " +
				"waited; and `refused`, answered 503 at once because it had to " +
				"wait and found no place to — the node's waiting room full, " +
				"four places a git transfer slot, or its credential's quarter " +
				"of it, a lane's width. git does not retry a lazy fetch, so " +
				"every `expired` or `refused` is a checkout that failed, and " +
				"git prints only the status: the `code_fetch_spool_unserved` " +
				"line names the token and what held it back. `granted` " +
				"climbing is a credential at its quarter of the node's " +
				"spooling — one token behind several CI runners, which a " +
				"token per runner ends — or a node whose spooling is full: " +
				"more git transfer slots (more memory, a wider " +
				"`git.max_transfers`) or more git nodes.",
		},
		{
			Name: CodeHostSpoolWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"outcome"},
			Shows: "How long each fetch counted by " +
				"`crewlet.code.host.spool.waits` waited for room to spool, by " +
				"the same `outcome` (`granted`, `expired`, `abandoned`; a " +
				"`refused` one waited for nothing). The `granted` tail is what " +
				"the wait adds to a checkout; one nearing 55 s is a node about " +
				"to answer checkouts 503.",
		},
		{
			Name: CodeBoxResyncs, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"outcome"},
			Shows: "A coding box's reads that went to the code log's head on " +
				"this node — a fetch whose want the node's copy lacked, a " +
				"request past the bound its admitted row set, a repository " +
				"its run was given that its run's floor would refuse, a run " +
				"whose row names no floor — and a check's box's alike, its " +
				"reads at /box/code/ this node answers among them, each " +
				"a barrier on the code log, " +
				"by `outcome`: `taken`, paid from its run's bucket on its " +
				"seat — the seat bucket its turns' code tools on this node " +
				"pay too — or a check's attempt's on its repository's checks " +
				"(16 at once, one more every 11.5 minutes: the census's " +
				"125 a seat-day); `spent`, answered at the run's floor " +
				"instead, the box told what it asked for is not there and " +
				"when its re-syncs come back. `taken` is the boxes' share of " +
				"the code log's barriers; `spent` climbing is a box asking " +
				"in a loop for what nobody pushed — the " +
				"`code_box_resyncs_spent` line names its seat and run.",
		},
		{
			Name: CodeBoxInflightRefused, Kind: KindCounter, Unit: UnitCount,
			Shows: "A coding box's git requests this node answered 503 " +
				"(`run_busy`) because its run — or a check's box's attempt " +
				"— already had 16 in flight here, twice the repositories " +
				"a run is given. Any is a box " +
				"sending requests faster than they are served, which its " +
				"retry absorbs; climbing is one run's loop, which the " +
				"`code_box_unavailable` line names by its turn and launch.",
		},
		{
			Name: CodeHostWrites, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"kind", "outcome"},
			Shows: "Every engine write this git node built or answered — a " +
				"commit, a revert, a cherry-pick, a branch created or deleted, " +
				"a review's suggestions applied through a tool, a change merged or rebased, " +
				"a seat's or a person's — by `kind` (`commit_files`, `revert`, " +
				"`cherry_pick`, `create_branch`, `delete_branch`, " +
				"`apply_suggestions`, `merge_change`, `rebase_change`) and " +
				"`outcome`: `applied`, `pending` and `unknown` as a push's, " +
				"from its row — a merge's from its merge record, or its " +
				"enqueue's where its target's queue took it; `refused`, a " +
				"refusal of the change or of its push (a conflict, nothing to " +
				"change, a suggestion outdated or overlapping, a stale head, a " +
				"protection, the grant, a merge its gate or its target's moves " +
				"refused) or a write the call " +
				"named wrong; `unready`, this node could not answer in time " +
				"and another git node was asked; and `failed`, the node's own " +
				"failure. `unready` climbing is a node behind its syncs or at " +
				"its share of seat git work; `failed` is never routine — the " +
				"`code_write` line names it.",
		},
		{
			Name: CodeHostWriteDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"kind", "stage"},
			Shows: "How long each engine write took on this node, by `kind` " +
				"and `stage`: `sync`, bringing the copy to the caller's floor " +
				"and judging the write on it, the check of where its branch is " +
				"and of the objects it names among it; `build`, its git — the " +
				"read of what each path it names and each directory above one " +
				"is, the lookups of what it changes, an apply's placement of its " +
				"suggestions, its objects, its commit and its pack, under a unit " +
				"of the seat git-work share; `land`, the " +
				"landing every push takes — the upload, the proposal and the " +
				"wait for its row, a merge's its merge record's and every " +
				"rebuild onto a target that moved; and `total`. A `build` tail " +
				"near ten seconds — thirty for a `merge_change`, whose one " +
				"merge is a mergeability's, and forty for a `rebase_change`, " +
				"each of whose commits replays one — is changes too wide or too deep " +
				"for an engine write, answered too_large; a `land` tail is the " +
				"code log's, as a push's is.",
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
		{
			Name: AlarmTransitions, Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"change"},
			Shows: "Alarm transitions this node published for the seats whose " +
				"role carries `health_tools` to be woken by, once the broker " +
				"took each — `fired`, `cleared` or `firing_at_boot`, the last " +
				"what a node's first evaluation found up. A gauge flipping " +
				"while this stands still is a node whose wakes are not getting " +
				"out: its `alarm_transition_unpublished` line names the error.",
		},
		{
			Name: AlarmTransitionsDropped, Kind: KindCounter, Unit: UnitCount,
			Shows: "Alarm transitions this node dropped unpublished: its outbox " +
				"holds 1024 while the broker refuses them and drops the oldest " +
				"past that, so no seat is woken by those. Any rise is a broker " +
				"this node could not publish to for a long while; " +
				"`fleet_health` still reads what is firing now.",
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
				"or reservations (`uploads_outstanding`), an object store with " +
				"no room left (`store_full`, the operator's to make), an " +
				"agent's turn past its guardrails (`turn_quota`), a moved head " +
				"(`version_moved`) and the rest. Each reason is a different " +
				"person to call.",
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
			Shows: "How long after the broker stored a committed datastore batch's newest " +
				"record its `artifact_data` push was handed to this node's sockets " +
				"(`kind` `data`): the applier's lag plus the fan-out. A datastore " +
				"change is held to reach the socket within a second. An artifacts " +
				"movement carries no broker instant, so `artifact_changed` is not " +
				"measured here; its lag is the artifacts applier's own, in the state " +
				"log's apply instruments.",
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
		case e.Kind == KindHistogram && boundsOf(e.Unit) == nil:
			return fmt.Errorf("metrics: %q is a histogram in %q, which has no "+
				"boundaries: a histogram counts into its unit's (Bounds), and a "+
				"unit with none would count every value into one bucket — "+
				"record it in a unit that has them, or give that unit a set",
				e.Name, e.Unit)
		case e.Shows == "":
			return fmt.Errorf("metrics: %q does not say which failure it makes "+
				"visible: an instrument nobody will alarm on is a number on a "+
				"page, which is what this catalogue replaces", e.Name)
		}
		seen[e.Name] = true
	}
	return nil
}
