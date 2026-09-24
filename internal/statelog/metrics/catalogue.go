// Package metrics is the engine's instrument catalogue and the ONE recorder
// every measurement is fed through.
//
// # Why this package exists
//
// A number on a page is not a metric. What makes one is that an operator can
// draw it over time and alarm on it, and that requires it to leave the
// process: a broker whose fsync drifts from 1 ms to 40 ms, an applier whose
// drain halves, a barrier that starts spending half its read budget — each is
// a line on a collector's panel long before it is a refusal, and a refusal
// with no number behind it is all an operator has without one.
//
// # One recorder, two readers
//
// Everything records HERE — through [Recorder] — and it is read two ways: the
// OpenTelemetry instruments export its cumulative series, and the operator
// record's alarms read the measurements they need from its rolling 24-hour
// [Window]. That is deliberate and it is the lesson internal/tokens records
// for the token rollup: that aggregation had three copies once, and a refresh
// routinely disagreed with the page it replaced. A number an alarm reads and a
// number on a collector's panel come from the same write.
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

import (
	"fmt"
	"math"
)

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

	// UnitRatio is "1", UCUM's DIMENSIONLESS unit, for a fraction or a state
	// that is 0 or 1 — which is what the OpenTelemetry semantic conventions
	// write a utilization in. It is not how they write a count, so a
	// collector handed "1" for a count is told the count is a ratio, and
	// everything that counts something takes one of the annotations below
	// instead.
	UnitRatio = "1"

	// The COUNTS, each annotated with what it counts. The braces are UCUM's
	// annotation: they name the thing without changing the unit, which is
	// how the same conventions write an integer count of something —
	// `{thread}`, `{fault}`.
	UnitRecords    = "{record}"
	UnitRows       = "{row}"
	UnitWrites     = "{write}"
	UnitRounds     = "{round}"
	UnitRejections = "{rejection}"
	UnitReads      = "{read}"
	UnitBarriers   = "{barrier}"
	UnitYields     = "{yield}"
	UnitAttempts   = "{attempt}"
	UnitSequence   = "{sequence}"
	UnitCallers    = "{caller}"
	UnitHolds      = "{hold}"
	UnitScans      = "{scan}"
	UnitAnswers    = "{answer}"
	UnitCalls      = "{call}"

	// UnitRecordsPerSecond and UnitCommitsPerSecond are RATES, and UCUM
	// writes a rate as a quantity over a second: "1" alone declares a
	// number dimensionless, which a fraction is and a rate is not.
	UnitRecordsPerSecond = "{record}/s"
	UnitCommitsPerSecond = "{commit}/s"
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

	// Fractional marks a counter whose increments need not be whole — a
	// duration or a projection summed — so it is exported as a
	// floating-point sum and written with [Recorder.AddValue]. Every other
	// counter counts events and is exported as an integer.
	//
	// DECLARED rather than inferred from what a caller happens to pass,
	// because the exporter fixes each instrument's number type when it
	// registers it, before anything is recorded. A fractional counter
	// exported as an integer would reach the collector without the fraction
	// of its total — 0.9 seconds as 0 — while the recorder's own reading of
	// the same series kept it.
	//
	// Only a counter has the choice. A gauge and a histogram are
	// floating-point already, and [Validate] refuses the mark on them
	// rather than let it sit there meaning nothing.
	Fractional bool

	// Bounds are a histogram's own bucket boundaries in its own unit,
	// strictly increasing, for an instrument whose range the default set
	// ([DefaultBounds]) does not cover. Nil takes the default.
	//
	// DECLARED HERE rather than chosen by whoever records or exports, so
	// the recorder counts into them, the exporter registers them and every
	// node agrees on them — a boundary set that differed between nodes
	// could not be merged. [Validate] refuses them on anything but a
	// histogram.
	Bounds []float64

	// Shows is the failure this instrument makes visible. It is the reason
	// the instrument exists, and an entry that cannot fill it in is an
	// instrument nobody will alarm on.
	Shows string
}

// Buckets is a copy of the boundaries this histogram counts into: its own
// [Instrument.Bounds], or [DefaultBounds].
//
// A COPY, because the exporter hands it to an SDK view that keeps it, and the
// recorder's own counts are indexed by the slice it holds.
func (i Instrument) Buckets() []float64 { return append([]float64(nil), i.bounds()...) }

// bounds is the boundaries without the copy, for the recorder's own hot path.
func (i Instrument) bounds() []float64 {
	if i.Bounds != nil {
		return i.Bounds
	}
	return defaultBounds
}

// backupDurationBounds are the backup histogram's own buckets, in
// milliseconds: twenty-one powers of two from 2^7 (128 ms) to 2^27 (about 37
// hours) — the default set's count and resolution, moved up the scale.
//
// THE DEFAULT SET CANNOT HOLD A BACKUP. It tops out at 2^16 ms, about 65.5 s,
// and a backup is both estates' copies, every stream's snapshot and their
// verification: every copy slower than that lands past the last boundary and
// reads only as "longer than a minute", and on a node whose copies all take
// longer the p95 is infinite.
//
// THE TOP IS THE FIRST POWER OF TWO PAST A DAY, which is the default
// `backup_max_age`: a copy slower than the interval that policy allows
// between completed backups cannot keep it satisfied on any schedule, so past
// it there is no finer distinction an operator acts on.
//
// THE BOTTOM FOLLOWS FROM KEEPING TWENTY-ONE BUCKETS, the size every other
// histogram's series is: a copy faster than an eighth of a second reads as
// exactly that, and no maintenance window is sized against the difference.
var backupDurationBounds = powersOfTwo(7, 27)

// Catalogue is every instrument this engine records, grouped by subsystem.
//
// IN A FIXED ORDER, so the generated reference is stable and a diff is a change
// rather than a shuffle.
func Catalogue() []Instrument {
	return []Instrument{
		// ---- the write path -------------------------------------------
		{
			Name: StatelogPublishDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "outcome"},
			Shows: "A write path slowing down before it starts refusing. The " +
				"outcome dimension separates the three answers a write has " +
				"from its refusals, so a rise in `pending` reads as an applier " +
				"falling behind rather than as a broker getting slower.",
		},
		{
			Name: StatelogPublishRounds, Kind: KindHistogram, Unit: UnitRounds,
			Attributes: []string{"domain"},
			Shows: "Contention on one subject, which the compare-and-set round " +
				"cap bounds. A distribution creeping toward the cap is a hot " +
				"object; reaching it is a refusal a model reads as a colleague " +
				"editing the same thing.",
		},
		{
			Name: StatelogPublishOutcomes, Kind: KindCounter, Unit: UnitWrites,
			Attributes: []string{"domain", "outcome"},
			Shows: "The three-valued write outcome, counted, and only the " +
				"three — a refusal is on `publish.refusals` instead, because " +
				"it says the write never happened at all. `unknown` is the " +
				"one that matters most: without this count a broker flapping " +
				"into ambiguity is visible only to the model that received " +
				"the answer.",
		},
		{
			Name: StatelogPublishConflicts, Kind: KindCounter, Unit: UnitWrites,
			Attributes: []string{"domain", "subject_kind"},
			Shows: "Writes that spent their whole round budget losing races " +
				"on one subject, BY KIND. The refusal counter beside it " +
				"says a conflict happened and not what it was about, and " +
				"the remedy differs entirely: one contended object is a " +
				"design question and a contended kind is a hot subject.",
		},
		{
			Name: StatelogPublishRejections, Kind: KindCounter, Unit: UnitRejections,
			Attributes: []string{"domain", "subject_kind"},
			Shows: "How often a write loses a race, per kind of subject. It is " +
				"what says whether a counter, a rank order or an ordinary " +
				"object is the contended one.",
		},
		{
			Name: StatelogPublishRefusals, Kind: KindCounter, Unit: UnitWrites,
			Attributes: []string{"domain", "reason"},
			Shows: "Writes refused before or instead of an append, by the " +
				"refusal's own reason — `evicted`, `deferred`, `behind`, " +
				"`log_full`, `too_large` and `refused` among them — with " +
				"`conflict` for a write that lost every round, `exists` for a " +
				"create over an object that is there, and `error` for a " +
				"failure that is no refusal at all. A refusal is not one of " +
				"the three outcomes: it says the write never happened, and " +
				"each reason has its own remedy, which one counter with an " +
				"outcome dimension would hide.",
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
				"moves.",
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
			Name: StatelogReadRefusals, Kind: KindCounter, Unit: UnitReads,
			Attributes: []string{"domain", "level", "code"},
			Shows: "Every refusal code, counted, which is each code's own " +
				"rejection rate. The codes have different remedies, so a rate " +
				"folded across them would say reads are failing and not what " +
				"to do about it.",
		},
		{
			Name: StatelogReadServed, Kind: KindCounter, Unit: UnitReads,
			Attributes: []string{"domain", "level"},
			Shows: "Reads answered per level, which is the denominator every " +
				"refusal fraction needs and the check on the assumed read " +
				"rate the log's own size is derived from.",
		},
		{
			Name: StatelogBarrierAppends, Kind: KindCounter, Unit: UnitBarriers,
			Attributes: []string{"domain"},
			Shows: "Barrier records appended. Against reads served it is the " +
				"single-flight ratio, which says whether coalescing is doing " +
				"anything at all.",
		},
		{
			Name: StatelogLingerYields, Kind: KindCounter, Unit: UnitYields,
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
				"a policy about this quantity.",
		},
		{
			Name: StatelogApplyTxDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "bound_by"},
			Shows: "How long one apply transaction holds the store's writer, " +
				"and which budget ended it: from the start of the attempt that " +
				"committed to its commit. It is what every writer queued " +
				"behind it waits out, which is why the budgets bound it. The " +
				"wait for the writer before it, an attempt the store rolled " +
				"back and ran again, and the acknowledgement after it are all " +
				"outside it — and all inside the drain gauge's span, which is " +
				"what a backlog costs.",
		},
		{
			Name: StatelogApplyRecordDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "kind"},
			Shows: "One record's apply. A single record past the time budget " +
				"is still one transaction, so this is the real ceiling on how " +
				"long a read can be delayed.",
		},
		{
			Name: StatelogApplyBatchRows, Kind: KindHistogram, Unit: UnitRows,
			Attributes: []string{"domain"},
			Shows: "Rows per apply transaction, as the domain's applier " +
				"reports writing them, which is what the row budget bounds. " +
				"Rows, not records: the drain gauge counts records.",
		},
		{
			Name: StatelogApplyRecords, Kind: KindCounter, Unit: UnitRecords,
			Attributes: []string{"domain", "result"},
			Shows: "Records consumed, by what happened to them: applied, " +
				"retained, gated, skipped, or reprocessed by a build that could " +
				"read what an earlier one retained. A node applying nothing " +
				"while its position advances is healthy on lag alone.",
		},
		{
			Name: StatelogApplyRetries, Kind: KindCounter, Unit: UnitAttempts,
			Attributes: []string{"domain"},
			Shows: "Attempts the apply loop retried in place after a failure " +
				"that was not a stop — a fetch the broker did not answer, a " +
				"transaction the disk refused, an applier that errored. A rate " +
				"that stays up past the retry budget is a node whose rows have " +
				"stopped moving, and its health says so.",
		},
		{
			Name: StatelogApplyTxAborts, Kind: KindCounter, Unit: UnitAttempts,
			Attributes: []string{"domain"},
			Shows: "Apply attempts the store rolled back and ran again, because " +
				"the attempt failed transiently after it began. It reads zero " +
				"by construction: this database detects write conflicts per " +
				"file, so every write transaction takes the file's lock at its " +
				"BEGIN and queues for it, and no commit elsewhere in the file " +
				"can abort an apply. A non-zero count on the operator's own " +
				"hardware means the retry budget is being spent rather than " +
				"held in reserve.",
		},
		{
			Name: StatelogRecordsGated, Kind: KindCounter, Unit: UnitRecords,
			Attributes: []string{"gate", "subject_kind"},
			Shows: "Records an apply gate dropped. A dropped commit is " +
				"recoverable by nothing, and this is the only place anyone " +
				"would see that it happened.",
		},
		{
			Name: StatelogDrainRecordsPerSecond, Kind: KindGauge, Unit: UnitRecordsPerSecond,
			Attributes: []string{"domain"},
			Shows: "The applier's measured drain in RECORDS a second: every " +
				"record an apply run moves over, applied or not, over the " +
				"run's whole span — the wait for the writer and the " +
				"acknowledgement included — smoothed across runs. It is the " +
				"rate this node states a record backlog as a time with — a " +
				"stale read's staleness bound, a refused read's retry hint and " +
				"a bulk edit's projection — so a falling rate is an applier " +
				"slowing down. Zero until this process has consumed a batch: " +
				"nothing seeds it, and the first batch's rate is the first " +
				"reading.",
		},
		{
			Name: StatelogDrainCommitsPerSecond, Kind: KindGauge, Unit: UnitCommitsPerSecond,
			Attributes: []string{"domain"},
			Shows: "Commits per second, which is the fsync rate under " +
				"`synchronous = FULL` and the number a device budget is " +
				"spent by.",
		},

		// ---- position and health --------------------------------------
		{
			Name: StatelogApplyLagSeq, Kind: KindGauge, Unit: UnitRecords,
			Attributes: []string{"domain"},
			Shows:      "How many records this node is behind the log's head.",
		},
		{
			Name: StatelogApplyLagSeconds, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain"},
			Shows: "How old the oldest record this node has not applied is: " +
				"now less the broker's own timestamp on the first record past " +
				"its checkpoint that the log still holds, and zero when it is " +
				"caught up. An AGE rather than the backlog over the drain, " +
				"because an applier that has stopped keeps its last drain — its " +
				"projection stays as small as its backlog on a quiet log — while " +
				"the records it owes go on growing old. It is what the " +
				"`apply_lag` alarm fires on, at a minute. Exact on the compacted " +
				"vector log too: a record superseded on its subject before this " +
				"node reached it is gone from the log and is never applied here, " +
				"so the first survivor past the checkpoint is the oldest record " +
				"this node still owes.",
		},
		{
			Name: StatelogAppliedThrough, Kind: KindGauge, Unit: UnitSequence,
			Attributes: []string{"domain"},
			Shows: "The prefix this node has actually applied, which is lower " +
				"than its checkpoint whenever a record was retained.",
		},
		{
			Name: StatelogDeferredCount, Kind: KindGauge, Unit: UnitRecords,
			Attributes: []string{"domain"},
			Shows: "How many records this node has retained rather than " +
				"applied: those its build could not read, and those held back " +
				"behind one because their scopes meet. Non-zero is a rolling " +
				"upgrade in progress; the oldest one's age beside it says " +
				"whether the upgrade has stopped.",
		},
		{
			Name: StatelogDeferredOldestAgeSeconds, Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain"},
			Shows: "How long the oldest retained record has been retained, " +
				"which is what decides whether this node's seats move.",
		},
		{
			Name: StatelogWaiters, Kind: KindGauge, Unit: UnitCallers,
			Attributes: []string{"domain"},
			Shows: "Callers blocked on the applier, sampled on the position " +
				"heartbeat from the waiters themselves. It is the depth of the " +
				"queue a slow apply is making, and it goes on counting while " +
				"the applier is stopped — which is when callers pile up.",
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
			Name: StatelogLogHeadroomFraction, Kind: KindGauge, Unit: UnitRatio,
			Attributes: []string{"domain"},
			Shows: "How much of the ceiling is left. A full log refuses every " +
				"write AND every linearizable read, and the remedy is a " +
				"fleet-wide maintenance cycle, so this is the one number " +
				"worth alarming on long before it is small.",
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
			Shows: "How long ago the newest COMPLETE backup finished, read " +
				"from the manifests on disk rather than from a counter this " +
				"process keeps. A counter records that a process believed it " +
				"took a backup; the disk records that one exists, and they " +
				"differ in exactly the cases the alarm is for — a copy " +
				"deleted, a volume never mounted, a schedule pointing at a " +
				"path nobody ships from.",
		},
		{
			Name: BackupDuration, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: nil,
			Bounds:     backupDurationBounds,
			Shows: "How long a backup took, which is the window the trim " +
				"hold covers and the I/O the copy spends competing with the " +
				"applier's own commits. It is what turns the retention " +
				"guide's worked example into a number for THIS hardware.",
		},
		{
			Name: BackupHolds, Kind: KindGauge, Unit: UnitHolds,
			Attributes: nil,
			Shows: "Live trim holds. A pin that outlives its owner stops the " +
				"trim until the stale bound expires it, so a count that does " +
				"not return to zero is a backup that crashed mid-copy.",
		},

		// ---- the store ------------------------------------------------
		{
			Name: StorePoolWait, Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"file"},
			Shows: "How long callers queued for one of this file's pooled " +
				"connections: one observation per reporting tick in which a " +
				"caller queued, holding that tick's mean wait, because the " +
				"pool reports a total and a count rather than each wait. It " +
				"is what says the reader pool is too small on this node.",
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
			Shows: "The semantic scan, split by whether it ran for a turn's " +
				"prefetch or for somebody's deliberate search, because the " +
				"interactive path is the one with a target and a prefetch's " +
				"scans would dilute it.",
		},
		{
			Name: TrackerSearchConcurrency, Kind: KindGauge, Unit: UnitScans,
			Attributes: nil,
			Shows: "Scans in flight, which is the row of the supported-corpus " +
				"table this node is actually on. The published figure is a " +
				"single reader on an idle node.",
		},
		{
			Name: TrackerSearchAnswers, Kind: KindCounter, Unit: UnitAnswers,
			Attributes: []string{"coverage", "semantic"},
			Shows: "What each answer actually covered: whether every bucket " +
				"of the corpus was scanned, and whether the semantic half " +
				"ran. Both alarms below it are a FRACTION of this counter, " +
				"and a short answer is indistinguishable from a short " +
				"corpus without it.",
		},
		{
			Name: TrackerVectorCoverage, Kind: KindGauge, Unit: UnitRatio,
			Attributes: nil,
			Shows: "The fraction of sources carrying a current vector. It is " +
				"how a stalled embedding backlog is reported, since it never " +
				"drops a seat.",
		},

		// ---- the change feed ------------------------------------------
		{
			Name: TrackerFeedUnreadable, Kind: KindCounter, Unit: UnitRecords,
			Attributes: []string{"source"},
			Shows: "Change records this build could not translate into a " +
				"wake. Both domain consumers are deliberately uncapped, so " +
				"such a record is never dropped — it redelivers for ever at " +
				"the head of the consumer with every wake behind it " +
				"waiting, which has no other symptom at all.",
		},

		// ---- bulk -----------------------------------------------------
		{
			Name: TrackerBulkCalls, Kind: KindCounter, Unit: UnitCalls,
			Attributes: []string{"result"},
			Shows: "How often a bulk edit is issued, and how often one is " +
				"refused because another is applying: the rate at which the " +
				"one-bulk-at-a-time rule actually turns a caller away.",
		},
		{
			Name: TrackerBulkApplySeconds, Kind: KindCounter, Unit: UnitSeconds,
			// FRACTIONAL, because one bulk's projection is its records
			// over the applier's drain in records a second: any bulk
			// smaller than one second's drain projects a fraction of a
			// second, and only a fractional counter keeps one.
			Fractional: true,
			Attributes: nil,
			Shows: "Seconds of applier occupancy bulk edits projected, " +
				"summed. Over 24 hours it IS the fleet-wide read-degradation " +
				"budget: every second here is a second in which reads are " +
				"behind and writes are pending on every node.",
		},
		// ---- alarms ---------------------------------------------------
		{
			Name: AlarmActive, Kind: KindGauge, Unit: UnitRatio,
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
		case e.Fractional && e.Kind != KindCounter:
			return fmt.Errorf("metrics: %q is a %s marked Fractional: only a "+
				"counter has an integer form to choose against, so remove the "+
				"mark", e.Name, e.Kind)
		case e.Kind == KindCounter && e.Unit == UnitRatio:
			// A COUNTER COUNTS SOMETHING, and "1" declares a fraction:
			// a total that only rises is never one.
			return fmt.Errorf("metrics: %q is a counter in %q, which declares "+
				"a dimensionless fraction: annotate what it counts instead, "+
				"as {record} does", e.Name, e.Unit)
		case e.Bounds != nil && e.Kind != KindHistogram:
			return fmt.Errorf("metrics: %q is a %s with Bounds: only a "+
				"histogram has buckets, so remove them", e.Name, e.Kind)
		case e.Bounds != nil && !increasing(e.Bounds):
			return fmt.Errorf("metrics: %q declares Bounds that are empty or "+
				"not strictly increasing and finite: a value is counted into "+
				"the first boundary at or above it, which only a strictly "+
				"increasing set answers", e.Name)
		case e.Kind == KindCounter && !e.Fractional &&
			(e.Unit == UnitSeconds || e.Unit == UnitMilliseconds):
			// A SUMMED DURATION IS NOT A COUNT OF WHOLE UNITS, and as an
			// integer it loses every contribution shorter than one — a
			// counter that reads zero while the thing it sums is happening.
			return fmt.Errorf("metrics: %q sums a duration in %q as an integer, "+
				"which drops every contribution shorter than one unit: mark it "+
				"Fractional", e.Name, e.Unit)
		}
		seen[e.Name] = true
	}
	return nil
}

// increasing reports a boundary set a value can be counted into: non-empty,
// finite and strictly increasing.
func increasing(bounds []float64) bool {
	if len(bounds) == 0 {
		return false
	}
	for i, b := range bounds {
		if math.IsNaN(b) || math.IsInf(b, 0) || (i > 0 && b <= bounds[i-1]) {
			return false
		}
	}
	return true
}
