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
			Name: "crewlet.statelog.publish.duration", Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "outcome"},
			Shows: "A write path slowing down before it starts refusing. The " +
				"outcome dimension separates the three answers a write has, " +
				"so a rise in `pending` reads as an applier falling behind " +
				"rather than as a broker getting slower.",
		},
		{
			Name: "crewlet.statelog.publish.rounds", Kind: KindHistogram, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Contention on one subject, which the compare-and-set round " +
				"cap bounds and nothing measured. A distribution creeping " +
				"toward the cap is a hot object; reaching it is a refusal a " +
				"model reads as a colleague editing the same thing.",
		},
		{
			Name: "crewlet.statelog.publish.outcomes", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "outcome"},
			Shows: "The three-valued write outcome, counted, and only the " +
				"three — a refusal is on `publish.refusals` instead, because " +
				"it says the write never happened at all. `unknown` is the " +
				"one that matters most and had no counter: a broker flapping " +
				"into ambiguity was visible only to the model that received " +
				"the answer.",
		},
		{
			Name: "crewlet.statelog.publish.conflicts", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "subject_kind"},
			Shows: "Writes that spent their whole round budget losing races " +
				"on one subject, BY KIND. The refusal counter beside it " +
				"says a conflict happened and not what it was about, and " +
				"the remedy differs entirely: one contended object is a " +
				"design question and a contended kind is a hot subject.",
		},
		{
			Name: "crewlet.statelog.publish.rejections", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "subject_kind"},
			Shows: "How often a write loses a race, per kind of subject. It is " +
				"what says whether a counter, a rank order or an ordinary " +
				"object is the contended one.",
		},
		{
			Name: "crewlet.statelog.publish.refusals", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "reason"},
			Shows: "Writes refused before or instead of an append, by reason — " +
				"an evicted node, a deferred record covering the object, a " +
				"caller waiting on its own previous write, a full log. A " +
				"refusal is not one of the three outcomes: it says the write " +
				"never happened, and each reason has a different remedy, so " +
				"one counter with an outcome dimension would hide all four.",
		},
		{
			Name: "crewlet.statelog.write.session_wait", Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain"},
			Shows: "The wait a write pays for its own previous write to apply. " +
				"Zero when the caller is caught up, which is the common case, " +
				"and the shape of the load that is not.",
		},

		// ---- the read path --------------------------------------------
		{
			Name: "crewlet.statelog.barrier.duration", Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain"},
			Shows: "The broker round trip under every linearizable read, and " +
				"the first number a drifting fsync or a degrading quorum " +
				"moves. It was a benchmark's p50 on an idle loopback cluster " +
				"and nothing in production.",
		},
		{
			Name: "crewlet.statelog.read.wait", Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "level"},
			Shows: "How much of the read budget a barrier or session wait " +
				"actually spends. A p95 approaching the budget is reads about " +
				"to start refusing, which is the warning the refusal itself " +
				"is too late to be.",
		},
		{
			Name: "crewlet.statelog.read.refusals", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "level", "code"},
			Shows: "Every refusal code, counted. Twelve codes with different " +
				"remedies had no counter between them, so an operator had no " +
				"rejection rate for any of them.",
		},
		{
			Name: "crewlet.statelog.read.served", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "level"},
			Shows: "Reads answered per level, which is the denominator every " +
				"refusal fraction needs and the check on the assumed read " +
				"rate the log's own size is derived from.",
		},
		{
			Name: "crewlet.statelog.barrier.appends", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Barrier records appended. Against reads served it is the " +
				"single-flight ratio, which says whether coalescing is doing " +
				"anything at all.",
		},
		{
			Name: "crewlet.statelog.linger.yields", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "How often a waiter cut a batch short. It is the batching " +
				"the applier gives up to answer a read promptly, and without " +
				"it that trade is invisible.",
		},

		// ---- the applier ----------------------------------------------
		{
			Name: "crewlet.statelog.apply.latency", Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain"},
			Shows: "THE COMMIT-TO-APPLY GAP: from the broker's own timestamp " +
				"on a record to this node committing it. Every read level is " +
				"a policy about this quantity and nothing measured it.",
		},
		{
			Name: "crewlet.statelog.apply.tx.duration", Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "bound_by"},
			Shows: "How long one apply transaction holds the store's writer, " +
				"and which budget ended it. A transaction is what every " +
				"waiter behind it pays, and rows were only ever a proxy for " +
				"the duration.",
		},
		{
			Name: "crewlet.statelog.apply.record.duration", Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"domain", "kind"},
			Shows: "One record's apply. A single record past the time budget " +
				"is still one transaction, so this is the real ceiling on how " +
				"long a read can be delayed — a sentence in a design document " +
				"until it was measured.",
		},
		{
			Name: "crewlet.statelog.apply.batch.rows", Kind: KindHistogram, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Rows per apply transaction, which is what the row budget " +
				"bounds and what the drain rate divides.",
		},
		{
			Name: "crewlet.statelog.apply.records", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain", "result"},
			Shows: "Records consumed, by what happened to them: applied, " +
				"retained, gated or skipped. A node applying nothing while " +
				"its position advances is healthy on lag alone.",
		},
		{
			Name: "crewlet.statelog.apply.tx.aborts", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Apply transactions the store aborted on a conflict and the " +
				"loop retried. It is the number that says whether this " +
				"driver's transaction conflicts are row-scoped or " +
				"database-scoped, on the operator's own hardware rather than " +
				"on a benchmark's — measured at zero against a writer " +
				"committing to tables the applier never touches, so a " +
				"non-zero count means the retry budget is being spent rather " +
				"than held in reserve.",
		},
		{
			Name: "crewlet.statelog.apply.retries", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Transient apply failures retried in place, which are " +
				"otherwise a silent backoff inside the loop.",
		},
		{
			Name: "crewlet.statelog.records_gated", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"gate", "subject_kind"},
			Shows: "Records an apply gate dropped. A dropped commit is " +
				"recoverable by nothing, and this is the only place anyone " +
				"would see that it happened.",
		},
		{
			Name: "crewlet.statelog.drain.rows_per_second", Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "The applier's observed drain, which every retry hint " +
				"divides by. Seeded from a benchmark and then measured, so a " +
				"hint on real hardware stops being an extrapolation from " +
				"somebody else's.",
		},
		{
			Name: "crewlet.statelog.drain.commits_per_second", Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Commits per second, which is the fsync rate under " +
				"`synchronous = FULL` and the number a device budget is " +
				"spent by.",
		},

		// ---- position and health --------------------------------------
		{
			Name: "crewlet.statelog.apply.lag.seq", Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows:      "How many records this node is behind the log's head.",
		},
		{
			Name: "crewlet.statelog.apply.lag.seconds", Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain"},
			Shows: "How OLD the oldest unapplied record is. Seconds are what " +
				"a stall grace, a pending outcome and a seat move all turn " +
				"on; sequences are not, and a lag of 4 000 says nothing about " +
				"whether anything is wrong.",
		},
		{
			Name: "crewlet.statelog.applied_through", Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "The prefix this node has actually applied, which is lower " +
				"than its checkpoint whenever a record was retained.",
		},
		{
			Name: "crewlet.statelog.deferred.count", Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Records this build could not read and kept. Non-zero is a " +
				"rolling upgrade in progress; non-zero and not falling is one " +
				"that stopped.",
		},
		{
			Name: "crewlet.statelog.deferred.oldest_age_seconds", Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain"},
			Shows: "How long the oldest retained record has been retained, " +
				"which is what decides whether this node's seats move.",
		},
		{
			Name: "crewlet.statelog.waiters", Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "Callers blocked on the applier right now. It is the depth " +
				"of the queue a slow apply is making.",
		},

		// ---- retention and capacity -----------------------------------
		{
			Name: "crewlet.statelog.log.bytes", Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"domain"},
			Shows:      "What the log actually holds, against its ceiling below.",
		},
		{
			Name: "crewlet.statelog.log.max_bytes", Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"domain"},
			Shows: "The ceiling, read from the running stream rather than " +
				"from this node's own configuration — the two differ, and the " +
				"running one is what refuses the append.",
		},
		{
			Name: "crewlet.statelog.log.headroom_fraction", Kind: KindGauge, Unit: UnitCount,
			Attributes: []string{"domain"},
			Shows: "How much of the ceiling is left. A full log refuses every " +
				"write AND every linearizable read, and the remedy is a " +
				"fleet-wide maintenance cycle, so this is the one number " +
				"worth alarming on long before it is small.",
		},
		{
			Name: "crewlet.statelog.trim.blocked_seconds", Kind: KindGauge, Unit: UnitSeconds,
			Attributes: []string{"domain", "term"},
			Shows: "How long one retention term has held the trim, named. A " +
				"trim blocked for weeks is a log walking toward its ceiling " +
				"with a cause an operator can act on.",
		},

		// ---- the backup -----------------------------------------------
		{
			Name: "crewlet.backup.age", Kind: KindGauge, Unit: UnitSeconds,
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
			Name: "crewlet.backup.duration", Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: nil,
			Shows: "How long a backup took, which is the window the trim " +
				"hold covers and the I/O the copy spends competing with the " +
				"applier's own commits. It is what turns the retention " +
				"guide's worked example into a number for THIS hardware.",
		},
		{
			Name: "crewlet.backup.holds", Kind: KindGauge, Unit: UnitCount,
			Attributes: nil,
			Shows: "Live trim holds. A pin that outlives its owner stops the " +
				"trim until the stale bound expires it, so a count that does " +
				"not return to zero is a backup that crashed mid-copy.",
		},

		// ---- the store ------------------------------------------------
		{
			Name: "crewlet.store.pool.wait", Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"file"},
			Shows: "How long a reader waited for a connection. It is what " +
				"says the reader pool is too small on this node, which " +
				"nothing could say before.",
		},
		{
			Name: "crewlet.store.wal.bytes", Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"file"},
			Shows: "A write-ahead log a checkpoint cannot pass grows, and " +
				"this is the only way to see it before the volume fills.",
		},
		{
			Name: "crewlet.store.bytes", Kind: KindGauge, Unit: UnitBytes,
			Attributes: []string{"file"},
			Shows: "The store's size on disk, which the snapshot's free-space " +
				"precondition and the provisioning rule are both derived from.",
		},

		// ---- search ---------------------------------------------------
		{
			Name: "crewlet.tracker.search.scan.duration", Kind: KindHistogram, Unit: UnitMilliseconds,
			Attributes: []string{"path", "rung"},
			Shows: "The semantic scan, split by whether it ran for a turn's " +
				"prefetch or for somebody's deliberate search. Only the " +
				"prefetch had a published percentile, and the interactive " +
				"path is the one with a target.",
		},
		{
			Name: "crewlet.tracker.search.concurrency", Kind: KindGauge, Unit: UnitCount,
			Attributes: nil,
			Shows: "Scans in flight, which is the row of the supported-corpus " +
				"table this node is actually on. The published figure is a " +
				"single reader on an idle node.",
		},
		{
			Name: "crewlet.tracker.vector.coverage", Kind: KindGauge, Unit: UnitCount,
			Attributes: nil,
			Shows: "The fraction of sources carrying a current vector. It is " +
				"how a stalled embedding backlog is reported, since it never " +
				"drops a seat.",
		},

		// ---- bulk -----------------------------------------------------
		{
			Name: "crewlet.tracker.bulk.calls", Kind: KindCounter, Unit: UnitCount,
			Attributes: []string{"result"},
			Shows: "How often a bulk edit is issued and how often one is " +
				"refused because another is applying. The refusal " +
				"arithmetic rested on an assumed ten a day, a number with " +
				"no counter behind it; this is that number.",
		},
		{
			Name: "crewlet.tracker.bulk.apply_seconds", Kind: KindCounter, Unit: UnitSeconds,
			Attributes: nil,
			Shows: "Seconds of applier occupancy bulk edits projected, " +
				"summed. Over 24 hours it IS the fleet-wide read-degradation " +
				"budget: every second here is a second in which reads are " +
				"behind and writes are pending on every node.",
		},
		// ---- alarms ---------------------------------------------------
		{
			Name: "crewlet.alarm.active", Kind: KindGauge, Unit: UnitCount,
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
