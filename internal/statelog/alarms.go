package statelog

// THE ALARM TABLE, and it is in this package rather than beside any one
// subsystem because an alarm is the framework's answer to "is this node doing
// its job", and every subsystem above it asks the same question.
//

// The package's own doc is in doc.go and is NOT restated here. It was, and
// `go doc` concatenates every package comment in file order — so this file's
// copy, sorting first, told a reader that the ordered stream is "the
// write-ahead log" twelve lines before doc.go's own heading told them "This is
// NOT the store's write-ahead log". Two package comments are ADR-0008's
// failure inside one package: one rule, written twice, disagreeing.

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objectives"
)

// The thresholds an alarm fires at.
//
// # Why they are named here rather than at each condition
//
// Every one of them is a number some OTHER decision already made — a stall
// grace is what takes a copy out of service, a deferral grace is what stops
// a node serving a copy it cannot decode, a read budget is what a caller was
// promised. An alarm that invented its own
// threshold would be a second opinion about the same event, and the two would
// drift: the plan this replaces tinted a dashboard row `caution` past one
// apply linger (250 ms) on a position refreshed every 15 seconds, so every row
// was lit always, and `critical` past `min_age` — seven days, which is 10 080
// times the grace that actually does something.
//
// So an alarm fires at the threshold that MATTERS, and where the constant
// belongs to another package it is taken from there.
const (
	// PositionHeartbeat is how often a node running a state log republishes
	// its row in the fleet's position register — the engine's position
	// heartbeat, which the trim, the snapshot gate and a joiner's donor
	// count all read.
	//
	// TEN SECONDS, taken from what the readers need rather than from what
	// the writer can afford. The trim runs on a horizon of days and is
	// conservative with a stale row; the snapshot gate's question — is
	// there anybody else — changes when a node joins or leaves, which an
	// operator expects to see reflected in seconds rather than minutes. Ten
	// is well inside that and is a single small write per node per interval
	// against a bucket with no age.
	//
	// DECLARED HERE rather than beside the loop that ticks on it, for
	// [TrimInterval]'s reason: it is also the cadence of everything this
	// package derives from the register — [AlarmInterval] IS it — so the
	// one number lives where both of its readers can name it.
	PositionHeartbeat = 10 * time.Second

	// StallGrace is how far behind a node may be before its lag is worth
	// a person's attention. Sixty seconds, which is four of the fleet's
	// coordination heartbeats ([coord.ReconcileInterval]): long
	// enough that a rolling restart or one oversized record passes
	// through it, short enough that a node that has actually stopped is
	// named within a minute.
	StallGrace = 60 * time.Second

	// AlarmInterval is how often a node running a state log — every data
	// node — evaluates the table: on its [PositionHeartbeat], and it IS that
	// constant rather than a second spelling of its number. The two were
	// once one declaration in the engine, then two literals in two packages
	// that agreed only because nobody had retuned either — and the first
	// retune of the heartbeat would have left the table evaluating at a
	// cadence its inputs are not published at, with nothing to say so.
	//
	// THE HEARTBEAT, and never the trim's quarter-hour: the table ran on
	// that once, so `apply_lag` — which fires at the one-minute
	// [StallGrace] and whose remedy promises a stopped node is named
	// within a minute — was named up to sixteen minutes after it stopped,
	// and every surface fed by the evaluation lagged the condition by the
	// same quarter hour. An evaluation paced by anything slower silently
	// raises every threshold to its own period. FASTER BUYS NOTHING: the
	// fastest input, this node's lag behind the log, is measured against
	// positions the heartbeat publishes, so a tighter loop would
	// re-evaluate the same facts. At the heartbeat's ten seconds
	// [StallGrace] is six evaluations, so a condition that has held past
	// it is named within one more.
	AlarmInterval = PositionHeartbeat

	// DeferralGrace is how long a node may hold records it could not
	// apply before its copy of that log goes out of service — its seats
	// stay, and that log's operations go to the other data nodes
	// (ADR-0049). The alarm and
	// that step share the number deliberately: an operator who sees this
	// alarm has thirty minutes, and one who sees a different number has no
	// idea how long they have.
	DeferralGrace = 30 * time.Minute

	// FloorCacheStale is how old a cached trim floor may be before the
	// node stops answering from it. Four heartbeats, the same idiom every
	// other cached coordination fact uses.
	FloorCacheStale = 4 * coord.ReconcileInterval

	// ReadBudget is what a linearizable read was promised. The barrier
	// alarm fires at a QUARTER of it, because a barrier is one term of a
	// read and a barrier already spending the whole budget is a read that
	// has been failing for some time.
	//
	// WHAT A READ SPENDS IT ON is its barrier's append and this node
	// committing every record before that barrier that writes a row —
	// never a barrier's own checkpoint, its own or any other's
	// ([Runner.Reached]). Waiting for those, on a disk whose fsyncs
	// stalled 2.1–9.3 s, refused reads at this bound with every row they
	// needed already on the node: 30 of 33 refused code-log reads sat in
	// their own barrier's checkpoint-only commit. Now three reads back to
	// back behind a checkpoint held at its commit for the whole case are
	// served in 2.0–2.4 ms each under the race detector
	// (TestNoReadWaitsOnACommitThatWritesNoRow), and a whole read alone
	// takes 1.4 / 5.3 ms at p50 / p99 (BenchmarkArbitratedWrite, solo,
	// 2,000 reads), both on two laptop E-cores.
	//
	// TWO SECONDS STAYS, and a refusal here is the answer rather than a
	// bound to grow: what can still spend it is a commit that writes a
	// row, and a device stall has no length to size against — covering the
	// stalls above would have taken at least 9.3 s — while in a fleet the
	// bound is also how long a walk waits on one data node before the
	// estate router asks the next.
	ReadBudget = 2 * time.Second

	// PrefetchScanBudget is what the knowledge scan a turn's prefetch runs
	// was promised: the turn waits for it before its first token, so it is
	// held to half of what a person searching on purpose waits
	// ([InteractiveSearchTarget]). Read off the node that coordinates the
	// scan — the one whose store and share of the corpus it is.
	PrefetchScanBudget = 500 * time.Millisecond

	// HeadroomAlarmFraction is how little of a log's byte ceiling may
	// remain before an operator is told. A tenth: a log that fills refuses
	// writes rather than dropping records, so the failure is loud — but
	// raising a ceiling needs a maintenance window, and a tenth of a
	// 29 GiB log is days of writing at this engine's rate.
	//
	// OF THE CEILING ORDINARY WRITES ARE HELD TO
	// ([Reservation.Headroom]), which on a log that keeps a gate reserve
	// is [Reservation.Bytes] below the broker's: that is where writes
	// start being refused, so it is what "full" means to everybody but an
	// eviction.
	HeadroomAlarmFraction = 0.10

	// ObjectsHeadroomAlarmFraction is how little of the nats object
	// bucket's byte reservation may remain before an operator is told:
	// TWICE A LOG'S, a fifth, borrowed from [HeadroomAlarmFraction] so a
	// retune of the one moves the other. Twice, because both remedies for
	// a full bucket take longer than a log's: raising its reservation is a
	// capacity change on a stream every member holds, and moving the files
	// to a bucket (`crewlet objects migrate --to s3`) copies every one of
	// them and ends inside a maintenance window. A full bucket refuses
	// uploads, and only uploads.
	ObjectsHeadroomAlarmFraction = 2 * HeadroomAlarmFraction

	// WALAlarmBytes is a write-ahead log large enough to say a checkpoint
	// is not happening. A gibibyte: between checkpoints the driver's
	// 1 000-frame autocheckpoint keeps it at a few megabytes, normal
	// operation in the tens, and the failure it names — a reader holding a
	// snapshot open for ever — has no other symptom until the volume fills.
	//
	// A COPY HOLDS IT TOO, for the copy's whole run (store.DB.Copies): a
	// backup's or a donor snapshot's copy stops every checkpoint at its
	// moment, so the log grows by what the node writes while the copy
	// runs, and the run is the file's size over the rate the copy reads it
	// — about seven minutes for the reference company's year-five estate of
	// ≈ 44 GB at the ~104 MB/s measured on the pinned driver, so a
	// gibibyte is about 2.5 MB/s of log written through one. The bound
	// stays where it says a checkpoint is wrong rather than rising to clear
	// every copy: a gibibyte of log takes the same room from the volume
	// whatever holds it, and the reading names the copy holding it
	// ([Reading.WALCopy]), so the alarm says which of the two it is and
	// that a copy's end folds the log back in.
	WALAlarmBytes = 1 << 30

	// VolumeHeadroomFactor is how much free space a node's volume must
	// have relative to its store, expressed as a multiple. A restore, a
	// vacuum and a snapshot each need room for a second copy, so 1.1 is
	// the point at which the next routine operation would fail.
	//
	// THE LOG A COPY HOLDS BACK IS ADDED to it ([Reading.CopyHeldBytes]),
	// not left to the tenth: a backup's or a snapshot's copy stops every
	// checkpoint at its moment for its run, so the live file's log grows
	// on the same volume by what the node writes meanwhile, and a tenth of
	// the store covers that only while the node writes its log at under a
	// tenth of the rate the copy reads the file — about 10 MB/s at the
	// ~104 MB/s measured on the pinned driver. What a copy holds back is
	// measured by the last one; before any copy has run, the tenth stands
	// for it.
	VolumeHeadroomFactor = 1.1

	// PoolWaitAlarm is how long a caller may queue for a database
	// connection before the pool is the problem. A hundred milliseconds
	// is already half a dashboard query's budget spent waiting to start.
	PoolWaitAlarm = 100 * time.Millisecond

	// MaintenanceAlarmAfter is how long a maintenance operation may stay
	// open. Maintenance stops every publisher on every node, so an hour
	// of it is an outage nobody is watching.
	MaintenanceAlarmAfter = time.Hour

	// InteractiveSearchTarget is what an interactive search was promised.
	InteractiveSearchTarget = time.Second

	// SemanticCoverageFloor is the fraction of a corpus that must have
	// current vectors for semantic recall to be what it claims.
	SemanticCoverageFloor = 0.95

	// CensusDriftFactor is how far an observed read rate may exceed the
	// rate the log's own sizing was derived from. Twice: the log's byte
	// ceiling, its trim cadence and its join model are all functions of
	// that number, so at 2× they are describing a different company.
	CensusDriftFactor = 2.0

	// LinearizableReadsPerSeatDay is the read rate this engine's log sizing
	// was derived from, per agent seat.
	//
	// ONE HUNDRED AND TWENTY-FIVE: the reference company's 12 500
	// linearizable reads a day over its 100 seats. It is the census input
	// every capacity decision under the log rests on — a `linearizable`
	// read appends a barrier record, so the rate is a term in a log's byte
	// ceiling, in how often the trim has to run to stay under it, and in
	// how long a rejoining node's replay takes — and it is PER SEAT because
	// a seat's tools are what issue those reads: stated for the whole
	// company it was a fixed 12 500 that a two-hundred-seat company doing
	// exactly the reference company's work per seat exceeded twofold, so
	// the alarm fired on the one company whose sizing was still right. It
	// is a DECLARED expectation rather than a measurement, which is exactly
	// why it needs an alarm — nothing else notices when a company outgrows
	// the assumptions its deployment was sized against, and the symptom
	// arrives as a full log rather than as a slow one.
	//
	// It is the SEATS' term of a log's census and not the whole of it: the
	// engine's own periodic reads append barriers too, whatever the seats
	// do, and [Census.Background] is where they are counted; and a log whose
	// readers are not seats at all — the code log's git clients — declares
	// them in its own sizing, which [Census.Clients] carries. [Census] turns
	// all three into one log's figure.
	LinearizableReadsPerSeatDay = 125
)

// Census is what one log's expected read rate is derived from.
type Census struct {
	// Seats is the running company's agent seats.
	Seats int

	// Background is the barrier records a day the ENGINE'S OWN periodic
	// reads put on this log, whatever the company's seats do — the object
	// store's collector, which pins the estate on a fixed cadence from one
	// data node at a time (the engine's figure is collect.PinsPerDay).
	// Each pin is a barrier on every log it reads, so each log takes all
	// of them.
	//
	// It exists because the seats' term alone is not what a log receives,
	// and the engine's own reads do not shrink with the company: when every
	// data node pinned the estate on its own passes, a one-seat company on
	// one data node reading exactly its census put 293 barriers a day on a
	// log expected to take 125, and census_drift fired on companies whose
	// sizing was right — LOUDER the smaller the company.
	Background int

	// Clients is the linearizable reads a day a log's OWN SIZING declares
	// for readers that are NEITHER seats NOR the engine's own passes — the
	// code log's git clients (code.GitReadsPerDay): a company's people and
	// their CI, whose number no seat count predicts. Zero on every other
	// log. Its caller sizes it by the ceiling the log's stream HOLDS, which
	// every node reads alike and only a resize moves — and counts it on
	// that log whatever the company hosts at the moment, because the log
	// keeps its ceiling either way and the rate it is held against is
	// every node's over the alarm's day, which outlives a switch and spans
	// a rolling restart.
	//
	// It exists for Background's reason, worse: the code log's linearizable
	// reads are mostly a person's or a pipeline's, so held to the seats'
	// term alone a five-seat company's code log alarmed past 1 300 git reads
	// a day, which one CI polling ten repositories a minute exceeds tenfold.
	// A seat's own reads of that log — a ref resolved for it, a read by
	// object id's one re-sync at the head — are the seats' term, which every
	// log is given whole, and are no part of this one. So are its coding
	// boxes': a box reads at its run's floor and pays no barrier but where
	// what it asks for is not there. And the re-syncs a seat's work may
	// append at will are HELD to the seats' term, not merely counted in it: a
	// box's, a turn's code tools' second look and an engine write's
	// re-judgement each draw on a bucket of its run or its turn and on its
	// seat's ONE bucket of the git node that answers, which all three pay —
	// LinearizableReadsPerSeatDay a day, after a burst of code.ResyncBurst
	// (internal/code's NewResyncs) — and past it are answered at the seat's
	// floor with no barrier. A check's box's re-syncs are no seat's and no
	// client's: bounded alike by its repository's checks on each git node,
	// and counted in no term here.
	Clients int
}

// Expected is the log's census: the linearizable reads a day it was sized to
// take.
//
// # Per seat, and at least one
//
// The seats' reads come from their tools, so the company's figure is
// [LinearizableReadsPerSeatDay] times its seats. A company with NO agent seat
// is counted as one: its operators still read through the dashboard and the
// operator MCP, and an expectation of zero would be an alarm that could never
// fire, on exactly the company whose first seat has not been hired yet.
//
// # The whole company's figure on every log, not a share of it
//
// The census says how many reads a company makes and not how they split
// between the tracker and the knowledge base, so each log is sized for all of
// them — which is how their ceilings are sized too: each domain's log has its
// own budget. Divided across the logs instead, the tracker's log would be
// expected to take a fraction of the census, and the reference company, whose
// reads are mostly the tracker's, would fire the alarm doing exactly the work
// it was sized for.
//
// # Plus what the engine reads on its own, and what the log was sized for
//
// [Census.Background] is added to it, and [Census.Clients], each for the
// reason it gives.
func (c Census) Expected() int {
	return LinearizableReadsPerSeatDay*max(c.Seats, 1) + max(c.Background, 0) + max(c.Clients, 0)
}

// Kind names one alarm.
//
// A named string rather than an integer, because it travels: it is a metric
// attribute, a log field and a row on a dashboard, and an operator searching
// for one has to be able to type it.
type Kind string

// The alarm kinds, one per rule in [table].
//
// CONSTANTS RATHER THAN THE LITERALS AT EACH USE, because the value travels
// (see [Kind]): a kind spelled at two sites is one that is eventually spelled
// two ways, and the misspelling surfaces as a dashboard row nobody can find
// rather than as a compile error. What each one MEANS in an operator's words
// is [alarmMeaning] and what to do about it is its rule's remedy, which
// [AlarmReference] renders into the published page — so adding a kind is three
// things, and the reference suite is what refuses a rule whose meaning nobody
// wrote.
const (
	KindApplyLag            Kind = "apply_lag"
	KindReadRefusals        Kind = "read_refusals"
	KindBarrierSlow         Kind = "barrier_slow"
	KindLogHeadroom         Kind = "log_headroom"
	KindLogCeilingShort     Kind = "log_ceiling_short"
	KindBackupAge           Kind = "backup_age"
	KindTrimBlocked         Kind = "trim_blocked"
	KindDeferredOld         Kind = "deferred_old"
	KindFloorUnknown        Kind = "floor_unknown"
	KindPrefetchSlow        Kind = "prefetch_slow"
	KindSearchSlow          Kind = "search_slow"
	KindSearchDegraded      Kind = "search_degraded"
	KindSearchScoped        Kind = "search_scoped"
	KindHistoryPartial      Kind = "history_partial"
	KindRecallBelowFloor    Kind = "recall_below_floor"
	KindIVFRecallBelowFloor Kind = "ivf_recall_below_floor"
	KindRecordsGated        Kind = "records_gated"
	KindFeedUnreadable      Kind = "feed_unreadable"
	KindMaintenanceOpen     Kind = "maintenance_open"
	KindVolumeLow           Kind = "volume_low"
	KindWALLarge            Kind = "wal_large"
	KindPoolStarved         Kind = "pool_starved"
	KindCensusDrift         Kind = "census_drift"
	KindObjectsMissing      Kind = "objects_missing"

	KindObjectsBucketNearCeiling Kind = "objects_bucket_near_ceiling"
	KindObjectiveBurn            Kind = "objective_burn"
	KindClockSkew                Kind = "clock_skew"

	// The code host's: a git node's own work, beside the logs', and the
	// committer's on the data node that decides a repository's pushes.
	KindCodeMaintenanceRefused Kind = "code_maintenance_refused"
	KindCodeCompactionBehind   Kind = "code_compaction_behind"
	KindCodeCommitBusy         Kind = "code_commit_busy"
	KindCodeChecksBacklog      Kind = "code_checks_backlog"
)

// Reading is everything an alarm evaluation looks at, gathered once per tick.
//
// ONE STRUCT rather than a callback per condition, because the whole table is
// evaluated together on ticks that already run — every [AlarmInterval], and
// again straight after the trim's quarter-hour measurements land — and a
// condition that fetched its own input would make the cost of the table a
// function of how many alarms are defined rather than of how many facts it
// reads.
//
// A field this node cannot measure is left at its zero value, and every
// condition is written so that a zero reads as "nothing to report" rather than
// as "at the floor". That is what lets a node with no search backend, no
// snapshot and no maintenance in flight evaluate the same table as one with
// all three.
type Reading struct {
	// ApplyLag is how old the oldest unapplied record is.
	ApplyLag time.Duration

	// RefusalsSince is how long reads have been refused for a reason other
	// than ordinary lag. Zero when nothing is being refused.
	RefusalsSince time.Duration

	// BarrierP95 is the read barrier's 95th percentile.
	//
	// EVERY …P95 HERE IS A HISTOGRAM'S QUANTILE, the upper boundary of the
	// bucket the percentile falls in ([metrics.Snapshot.Quantile]): "up to"
	// this, and past the boundary below it. An alarm holds one to a budget
	// that is itself a boundary, so it fires exactly when the percentile is
	// past the budget (TestEveryPercentileAlarmIsDecidedAtABoundary), and its
	// detail says "up to" rather than naming the boundary as the percentile.
	BarrierP95 time.Duration

	// HeadroomFraction is how much of the byte ceiling the log's ordinary
	// writes are held to is unused, as a fraction ([Reservation.Headroom]).
	//
	// A POINTER, because zero is a real value here and it is the worst
	// one: a full log and a node that has not measured its log are
	// opposite facts, and a plain float64 gives them the same
	// representation. Nil is "not measured" — which is every node until
	// the trim runs once, and every node whose domain declares no ceiling.
	HeadroomFraction *float64

	// BackupAge is how old the newest verified backup is, and BackupMaxAge
	// what the operator asked for. A zero BackupMaxAge is a node with no
	// backup policy, which is not an alarm — it is a deployment that has
	// said it does not want one.
	//
	// BackupAge IS A POINTER, for the reason HeadroomFraction is one: zero
	// is a real and reassuring value here — a copy verified this second —
	// and "no verified backup has ever been recorded" is the opposite
	// fact. A plain duration gave them one representation, so the reading
	// was filled with a fabricated age past the policy to make the alarm
	// fire, and the alarm then told a four-second-old company that its
	// newest backup was twenty-five hours old. Nil is the absence, and the
	// condition below says so in words.
	BackupAge    *time.Duration
	BackupMaxAge time.Duration

	// TrimBlockedFor is how long the trim has been unable to advance, and
	// TrimBlockedBy names the term holding it. Per DOMAIN, like
	// HeadroomFraction.
	TrimBlockedFor time.Duration
	TrimBlockedBy  string

	// TrimPastWindow is how many of the log's SEQUENCES lie below the age
	// term — the records older than ReplayWindow that the age term alone
	// would release, and which a blocked trim is therefore keeping. Per
	// DOMAIN. Exactly that many records on a strict log, whose sequences are
	// dense above its first; AT MOST that many on a compacted one, whose
	// superseded records are already gone from the span, which is why the
	// detail says "up to".
	//
	// MEASURED, from the age term's own sequence against the log's first,
	// rather than assumed from how long the block has lasted, because the
	// two come apart in exactly the cases a healthy fleet is in: a log
	// nobody has written to is blocked for ever (every node sits at
	// position zero) and holds nothing, and a log whose first record
	// arrived yesterday under a block a month old holds nothing past the
	// window either. ZERO IS THE BENIGN END and so is unmeasured — a stream
	// this node could not read — so it needs no pointer: both say the block
	// is keeping nothing anybody knows of.
	TrimPastWindow uint64

	// DeferredAge is how long this node has held the oldest record it
	// could not apply on one log — from the position heartbeat's first
	// sighting of it, the instant [Health.Fault] measures the grace that
	// takes this node's copy of that log out of service from, through
	// [DeferredSince.Age] — and DeferredRecord which record that is, in the
	// words an operator upgrades by: its position and the record version it
	// was written at against the one this build reads. Per DOMAIN, like
	// HeadroomFraction, and filled by the report from [DomainInputs] rather
	// than by a node-wide caller.
	//
	// A LOG'S, never the node's oldest, because EVERY LOG'S COPY IS JUDGED
	// on its own (ADR-0049): past the grace a held record takes this node's
	// copy of THAT log out of service and no other's, so two logs holding
	// records at once are two different answers, and one alarm naming the
	// node's oldest hid every other log held beside it.
	DeferredAge    time.Duration
	DeferredRecord string

	// FloorUnknownFor is how long one log's trim floor has been unreadable
	// on this node — from the first alarm heartbeat that could not read it
	// to the latest, never a persistence nobody observed — and
	// FloorUnknownCause what the latest read said: the log's floor key could
	// not be read (coordination unreachable, or its row in the register does
	// not read as a floor) or the floor is published at a generation this
	// node has not reached — one refusal with different remedies.
	//
	// A DURATION A DOMAIN, each floor being its own key, read, refused and
	// aged on its own: a domain that began failing a moment ago is still
	// served from its memo ([FloorCacheStale]), and one age printed beside
	// every failing name told an operator that domain's reads were refused,
	// with another domain's age as the evidence — while one alarm naming the
	// node's longest-unreadable floor hid every other log re-anchored beside
	// it.
	FloorUnknownFor   time.Duration
	FloorUnknownCause string

	// PrefetchP95 and SearchP95 are the knowledge scans this node
	// coordinated, at p95: those a turn's prefetch ran, which the turn waits
	// on before its first token, and those somebody ran on purpose. Two
	// series, never one, since each is held to its own budget.
	PrefetchP95, SearchP95 time.Duration

	// SearchDegradedFraction and SearchScopedFraction are the fractions of
	// searches answered without the semantic half and without the whole
	// corpus.
	SearchDegradedFraction, SearchScopedFraction float64

	// HistoryPartialFraction is the fraction of this node's fleet history
	// reads answered without every live node, and HistoryReadBudget the
	// budget those reads waited on for the other nodes — internal/eventfan's
	// FleetReadBudget, which a node that did not answer inside is what makes
	// an answer partial.
	//
	// THE BUDGET TRAVELS WITH THE READING rather than being imported here,
	// because the scatter that owns it is a layer ABOVE this framework: it
	// reads every node's own event log, and the framework every replicated
	// domain is built on depending on the fleet's history reader for one
	// constant would put that reader under every domain's build. The node
	// that counted the reads is the one that knows what they waited on, and
	// it states it in the same breath as the fraction, so the detail still
	// names the number the scatter waits on rather than a second opinion of
	// it (ADR-0015).
	HistoryPartialFraction float64
	HistoryReadBudget      time.Duration

	// SemanticCoverage is the fraction of the corpus with current vectors.
	// A POINTER for the reason HeadroomFraction is one: a company with no
	// embeddings configured measures nothing, and zero coverage is the
	// alarm rather than the absence.
	SemanticCoverage *float64

	// IVFRecall is the recall the latest measurement of the semantic index
	// found against the exact scan (ADR-0028) — its training's, or the
	// duty's later re-measurement's — in the query shape nearest its floor,
	// IVFShape; IVFRecallFloor is that shape's floor, and IVFMeasuredOn how
	// many sources the corpus held.
	//
	// THE FLOOR IS SUPPLIED rather than named here, because the curve is
	// internal/search's — its FloorAt, the same curve `crewlet search eval`
	// judges against, at the size of the corpus each shape searches — and
	// that package imports this one. A POINTER, for SemanticCoverage's
	// reason: an index that was never trained, or was retired for the
	// corpus's size, measured nothing, and zero recall is the alarm rather than
	// the absence.
	IVFRecall      *float64
	IVFRecallFloor float64
	IVFMeasuredOn  int
	IVFShape       string

	// RecordsGated and FeedUnreadable are counts over the last day. Any
	// value above zero is an alarm: a gated record is recoverable by
	// nothing, and a change record no build could translate is a wake
	// circling for ever behind everything queued after it.
	RecordsGated, FeedUnreadable int

	// MaintenanceOpenFor is how long a maintenance operation has been in
	// flight, with the phase it is stuck in.
	MaintenanceOpenFor time.Duration
	MaintenancePhase   string

	// FreeBytes and StoreBytes are one volume's free space and what this
	// node's databases occupy ON IT, and StoreVolume the directory that
	// names it: of the volumes the databases are on — the two files need
	// not share one — the one with the least room for the next copy of what
	// it holds. CopyHeldBytes is the write-ahead log that copy is expected
	// to hold back there while it runs, as each file's last copy measured
	// it (store.DB.CopyHoldsBack): a copy stops every checkpoint at its
	// moment for its run, so the next one needs room for its log beside
	// itself — zero where no copy has measured it yet.
	FreeBytes, StoreBytes, CopyHeldBytes int64
	StoreVolume                          string

	// WALBytes is the larger write-ahead log's size and WALFile the
	// database it is beside. WALCopy is the copy holding that log now — the
	// part file it writes, and how many more hold the log beside it — and
	// WALCopyFor how long it has, empty and zero when no copy does: a copy
	// holds its file's log for its whole run (store.DB.Copies), so a large
	// log a copy holds is one the copy's end folds back in, where one
	// nothing holds is a checkpoint that is not happening.
	WALBytes   int64
	WALFile    string
	WALCopy    string
	WALCopyFor time.Duration

	// StoreVolumeUnmeasured says which database, or which volume, could not
	// be measured and why — empty when every one was. It is the alarm, not
	// an absence: a volume nobody can measure is one nothing shows has
	// room, and leaving it out would judge the node on its other volume,
	// which is the blind spot measuring each file's own volume removed.
	StoreVolumeUnmeasured string

	// PoolWaitP95 is how long a caller queues for a database connection.
	PoolWaitP95 time.Duration

	// LinearizableReads and LinearizableReadsExpected are one log's
	// observed and designed-for daily read rates — the barrier records
	// committed to the log in the last day, from every node, and its census
	// ([Census.Expected]). Per DOMAIN, like HeadroomFraction, and filled by
	// the report from [DomainInputs]: a log over its census is over it
	// however quiet the others are, and one reading naming only the log
	// furthest past its census said nothing of a second log past its own
	// until the first came back under.
	LinearizableReads, LinearizableReadsExpected int

	// LogBytesPerDay is what one log took in over the trailing day,
	// LogMaxBytes the ceiling its broker enforces and ReplayWindow the
	// `min_age` the trim keeps — the window the log has to be able to
	// hold. Per DOMAIN, like HeadroomFraction, and evaluated per domain
	// by the report.
	//
	// A POINTER, and here zero is the BENIGN end rather than the alarming
	// one, which is exactly why it needs one: a log that took in nothing
	// yesterday is a measurement — it can hold any window — while a node
	// that has not measured, or a log in its first two days (whose
	// trailing day would contain its import), knows nothing. Given the zero's representation,
	// the unmeasured log would be reported as idle on every screen, and
	// any form of the rule that divides by the rate — how long the ceiling
	// holds — reads it as a ceiling that holds nothing and fires on every
	// boot.
	LogBytesPerDay *uint64
	LogMaxBytes    uint64
	ReplayWindow   time.Duration

	// ObjectsMissing is how many files' bytes the object store does not
	// hold, or holds wrong — at another size, or under another digest, or
	// as no object it can read — or would not answer for, as the
	// collector's last audit to run to its end found: on the node
	// holding the collector's duty, and zero on every other, since the
	// store is one the whole fleet shares and one node's count of it is
	// the fleet's. Files nobody can read. Any value above zero is an alarm,
	// for the gated record's reason: there is no threshold below which a
	// file that cannot be opened is acceptable.
	ObjectsMissing int

	// ObjectsBucketBytes is how many bytes the nats object bucket holds,
	// and ObjectsBucketMaxBytes the reservation the broker holds it to,
	// as the collector's duty holder last read them: zero on every other
	// node, and on a fleet whose files are in an S3 bucket, which reserves
	// nothing. A zero MaxBytes is "not measured", never "full".
	ObjectsBucketBytes, ObjectsBucketMaxBytes int64

	// ObjectiveBurns is every service objective this node is spending the
	// error budget of too fast, each over the fastest pair of windows that
	// fires ([objectives.Meter.Burning]): empty on a node whose objectives
	// are inside their budgets, and on one that measured none.
	//
	// JUDGED BY internal/objectives, which owns the targets, the window
	// pairs and their factors, and carried here already judged: the alarm
	// borrows the decision whole (ADR-0015) rather than restating a factor
	// beside the one the objectives declare.
	ObjectiveBurns []objectives.Burn

	// ClockSkew is how far this node's clock stands from the fleet's, and
	// BrokerClockSkew how far the clock of the broker member leading the
	// coordination store's position register stands from it, each signed —
	// positive is ahead — as the position heartbeat last measured them off
	// the register: every data node's row carries the instant the node
	// stamped on it and the instant the broker stored it, so each says how
	// far that node's clock stands from the member's, and the MEDIAN of them
	// is the fleet's clock. ClockSkewNodes is how many data nodes' clocks
	// that median was taken over, this node's among them. ClockSkewBound is
	// the most either clock may stand from the fleet's:
	// objstore.AssumedClockSkew, the hour every rule that judges one
	// machine's instant by another machine's clock rests on, which the node
	// that measured hands in with the measure.
	//
	// TWO CLOCKS, because a row's two instants alone cannot say which of its
	// two clocks moved: judged against the member, a node read the member's
	// drift as its own, and the member — a data node on an embedded broker,
	// whose own row one clock stamps and stores — read nothing at all. Against
	// the fleet's median, the clock that moved is the one off it.
	//
	// THE BOUND TRAVELS WITH THE READING, for HistoryReadBudget's reason:
	// the constant is the object store's, which this framework does not
	// import, and the alarm fires at that decision rather than at a number
	// of its own (ADR-0015). All zero on a node that measured nothing — no
	// beat yet, two beats in a row that could not read the register, or a
	// heartbeat that has stopped — and a zero bound judges nothing, the
	// BackupAge/BackupMaxAge pair's shape: a zero skew is a clock in time
	// with the fleet's, never an alarm.
	ClockSkew, BrokerClockSkew time.Duration
	ClockSkewNodes             int
	ClockSkewBound             time.Duration

	// CodeMaintenanceRefused is how many repositories this node's git cache
	// cannot write a bitmap of, because the bitmap — charged by the
	// repository's objects, with room beside it for one clone of the
	// repository — needs more memory than the node can give its background
	// work; CodeMaintenanceLargest is the largest of those charges, and
	// CodeMaintenanceBudget what the node can give: its memory as the cache
	// last read it, less what it keeps for its git lanes, the store, the
	// broker and the engine (internal/code/cache). The node's own
	// measurement, never a threshold of the table's: the alarm fires where
	// the largest charge passes it. All zero on a node that serves no git.
	CodeMaintenanceRefused                        int
	CodeMaintenanceLargest, CodeMaintenanceBudget int64

	// CodeCompactionBehind is how many of the repositories this node
	// maintains — as their MAINTAINER, the git node that merges their packs
	// and publishes their layers — its compactor found at the most live packs
	// a repository may hold on its last pass, so every push carrying a pack
	// into them is refused `compaction_behind` until a compaction lands;
	// CodeCompactionBehindLargest is the most live packs among them and
	// CodeCompactionBehindRepo that repository's name. Counted against
	// code.MaxPacksPerRepo by the compactor, which already judged each
	// repository it names (internal/code/compact): this framework imports no
	// code-host package, and the alarm borrows the bound the push decide
	// refuses at (ADR-0015) rather than holding a number of its own. All zero
	// on a node that serves no git, compacts nothing, or found none.
	CodeCompactionBehind        int
	CodeCompactionBehindLargest int64
	CodeCompactionBehindRepo    string

	// CodeCommitBusy is how many proposals this node's COMMITTER — the data
	// node a repository's pushes are decided on, the first live data node in
	// its rendezvous order — refused busy for a BACKLOG over the last
	// TrimInterval: a repository's queue full, the node holding its whole
	// bound across every repository it commits for, or a proposal that
	// queued with time to spare and lost it waiting behind earlier ones
	// (internal/code's Committer.AlarmReading). CodeCommitBusyRepo is the
	// repository with the most of them, by the name this node's rows hold
	// for it, else its id; CodeCommitBusyOfRepo is how many were that
	// repository's; CodeCommitBusyNode is how many met the node's whole bound
	// rather than one repository's queue; and CodeCommitBusyWaited is how
	// many waited in a queue until their asker could wait no longer — the
	// queue's TIME bound, which a committer slower than its pushes meets
	// before its count. A proposal that arrived with its asker's attempt
	// already spent, or a record that lost every compare-and-set round, is
	// no part of it.
	//
	// EACH WAS ALREADY JUDGED AT THE COMMITTER'S OWN BOUNDS, which this
	// framework does not import, so the alarm borrows them whole (ADR-0015)
	// and fires at any count. And the window is TrimInterval itself, which
	// the committer borrows in turn (code.CommitBusyWindow), so the detail
	// names the span from this package's own constant rather than from a
	// copy of it. All zero on a node that decides no push — no data role, or
	// a company that hosts no code — and on one that turned none away.
	CodeCommitBusy       int
	CodeCommitBusyRepo   string
	CodeCommitBusyOfRepo int
	CodeCommitBusyNode   int
	CodeCommitBusyWaited int

	// CodeChecksBacklog is how many queued attempts of checks have waited for
	// a runner longer than their OWN timeout — each held to the
	// timeout_seconds of the definition it was queued under, the time its
	// author gave the check to run — as this node's rows hold them
	// (internal/code's Reader.ChecksBacklog, read in the order they fell
	// overdue), exact up to code.BacklogScan; CodeChecksBacklogMore reports
	// more than that many, the count then the bound;
	// CodeChecksBacklogOldest is the longest wait among those counted and
	// CodeChecksBacklogCheck that attempt, by its repository's name, its
	// commit, the branch it is checked for, its check and its attempt.
	//
	// MEASURED FROM THE ROWS by every data node, never by a runner: a fleet
	// whose every git node is gone runs no check at all, and is the case the
	// alarm must not miss. EACH ATTEMPT WAS JUDGED AT ITS OWN TIMEOUT, which
	// this framework does not import, so the alarm borrows it (ADR-0015) and
	// fires at any count. All zero on a node holding no code log's rows — no
	// data role — and on one whose queued attempts are all within their
	// time.
	CodeChecksBacklog       int
	CodeChecksBacklogMore   bool
	CodeChecksBacklogOldest time.Duration
	CodeChecksBacklogCheck  string
}

// Alarm is one condition currently true on this node.
//
// TAGGED, like every other struct in [Report], and the tags are load-bearing
// for a reader this package does not have yet.
//
// This is reached through `GET /work/retention`, so its field names are a wire
// contract. Untagged it was the ONE struct in that document emitting
// `Kind`/`Detail`/`Remedy` beside siblings emitting `node_id` and `first_seq`
// — which `crewlet retention status` survived only because Go's own decoder
// matches field names case-INSENSITIVELY, so the CLI kept printing the alarm
// correctly and nothing reported the drift. Every other reader is case
// sensitive: `alarm.kind` in the dashboard, `.alarms[].kind` in `jq`, a key
// lookup in Python. Each of those reads the alarm as absent rather than as an
// error, which is the failure this document exists to prevent an operator
// from having.
type Alarm struct {
	// Kind is which alarm.
	Kind Kind `json:"kind"`

	// Detail says what was measured, in the operator's units. It is the
	// half of an alarm that makes it actionable: "apply_lag" is a name,
	// and "this node is 4m12s behind" is a fact. For a per-log condition
	// it is the most pressing log's measurement; each log in [Alarm.Domains]
	// carries its own.
	Detail string `json:"detail"`

	// Remedy is what to do about it. Carried on the alarm rather than
	// looked up beside it, because every surface renders the same alarm
	// and a remedy that lived on one of them would be missing from the
	// other two.
	Remedy string `json:"remedy"`

	// Domains is every log this alarm stands on, for a condition that is a
	// property of a log rather than of the node ([PerLogKinds]): the logs
	// nearing or short of their ceiling, the logs whose trim is held, the
	// logs holding a record past the deferral grace, the logs whose floor
	// cannot be read and the logs past their census — and under
	// [KindBackupAge] the logs whose [KindTrimBlocked] the missing or stale
	// backup absorbs: held past their replay window and keeping records
	// older than it. A younger hold is listed under no alarm. Empty for a
	// node-wide condition.
	//
	// ONE ALARM PER KIND, WITH ITS LOGS IN IT, rather than one alarm per log.
	// The kind is the alarm's identity on the count, the gauge and every
	// screen: the gauge carries one series per kind, and a report that
	// raised `trim_blocked` once per log told the health count four while
	// the gauge said one, and drew one cause as four problems on the
	// operator's screen. Detail is the most pressing log's measurement;
	// each entry here carries its own.
	//
	// THE LOG LINES ARE STILL PER LOG. The [Tracker] forms one instance per
	// (kind, log) from this list, so a second log joining a kind that is
	// already up writes its own `alarm_raised`, and the first log leaving it
	// writes its own `alarm_cleared` with its own start and detail: two
	// things an operator has to be told started and ended, which a line per
	// kind folded into one.
	Domains []AlarmDomain `json:"domains,omitempty"`
}

// AlarmDomain is one log an alarm stands on, and what was measured there.
type AlarmDomain struct {
	// Domain is the log's registered name.
	Domain string `json:"domain"`

	// Detail is this log's own measurement, in the operator's units.
	Detail string `json:"detail"`

	// Term is the trim term holding this log, where the alarm is about its
	// trim. It is the term's own name, which is what `crewlet retention
	// status` prints and the retention guide is indexed by.
	Term TermName `json:"term,omitempty"`

	// Since is when this log's trim stopped advancing, where the alarm is
	// about its trim. An instant rather than an age, so a reader renders
	// the age against its own clock instead of trusting one frozen at the
	// evaluation.
	Since time.Time `json:"since,omitzero"`
}

// rule is one row of the table.
type rule struct {
	kind Kind
	// fires reports whether the condition holds, and with what detail. A
	// closure rather than a threshold plus an accessor, because half the
	// conditions compare two fields of the reading against each other.
	fires func(Reading) (string, bool)
	// remedy is what an operator does about it.
	remedy string
	// perLog marks a condition that is a property of ONE LOG rather than
	// of the node, which the report evaluates once per log from that log's
	// own inputs and lists under the kind's one alarm ([Alarm.Domains]), and
	// whose log lines the [Tracker] writes per log. DECLARED on the
	// row so the published reference can say which they are, and held
	// against what the report actually raises per log by a test in both
	// directions — a flag nothing checked would be the second opinion the
	// table exists to prevent.
	perLog bool
}

// table is every alarm this engine can raise, in the order they are reported.
//
// ONE PLACE, and that is the point of the file. Before it, the two capacity
// alarms existed only as the exit code of a CLI verb nobody said who ran; the
// backup alarm fired at twice the threshold it documented, because one clause
// checked the age and another checked how long the first clause had been true;
// and maintenance mode — which stops every publisher on every node — was
// visible on one verb and no screen at all.
var table = []rule{
	{
		kind: KindApplyLag,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("this node is %s behind the log", spoken(r.ApplyLag)),
				r.ApplyLag > StallGrace
		},
		remedy: "Check this node's applier: `crewlet retention status` names the " +
			"domain and its position. A node that is behind keeps the seats it " +
			"holds and claims no new ones. Only if its position stops moving for " +
			"the stall grace, or it holds a record it cannot decode past the " +
			"deferral grace, is its copy wrong rather than behind: that log's copy " +
			"then goes out of service, and its operations go to the other data " +
			"nodes until the copy recovers; the node's seats stay.",
	},
	{
		kind: KindReadRefusals,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("reads have been refused for %s for a reason other "+
					"than ordinary lag", spoken(r.RefusalsSince)),
				r.RefusalsSince > coord.ReconcileInterval
		},
		remedy: "Read the refusal code in the logs. Anything other than `" +
			string(RefuseBehind) + "` or `" + string(RefuseTooStale) +
			"` is a fault rather than a wait.",
	},
	{
		kind: KindBarrierSlow,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the read barrier's p95 is past %s, a quarter of the "+
					"read budget: up to %s", spoken(ReadBudget/4), spoken(r.BarrierP95)),
				r.BarrierP95 > ReadBudget/4
		},
		remedy: "The barrier is an append and a wait: check the broker's own " +
			"latency and this node's apply drain before looking anywhere else.",
	},
	{
		kind:   KindLogHeadroom,
		perLog: true,
		fires: func(r Reading) (string, bool) {
			if r.HeadroomFraction == nil {
				return "", false
			}
			return fmt.Sprintf("%.0f%% of the log's byte ceiling for "+
					"ordinary writes is left", *r.HeadroomFraction*100),
				*r.HeadroomFraction < HeadroomAlarmFraction
		},
		remedy: "Raise the log's ceiling with `crewlet retention set-capacity` " +
			"during a maintenance window, or find out why the trim is not " +
			"advancing (`crewlet retention status` names the term holding it). " +
			"A full log refuses writes; it does not drop records. On a log " +
			"that claims identity the top of the ceiling is kept for gate " +
			"records, so if the term is a node that is gone, `crewlet " +
			"retention evict` still lands and unpins the trim.",
	},
	{
		// THE FORWARD-LOOKING HALF OF THE HEADROOM ALARM, and the one
		// that names a different remedy. Headroom says the log is nearly
		// full; this says it WILL be, with a trim that is working
		// perfectly — the trim never removes a record younger than
		// `min_age`, so a log whose ceiling is smaller than `min_age` of
		// its own writing fills and refuses appends with every term
		// satisfied. Unblocking a term cannot fix that; only the ceiling
		// or the window can.
		//
		// NO THRESHOLD OF ITS OWN (ADR-0015): the two numbers compared
		// are the ceiling the operator set and the window they set, and
		// the alarm fires at the point where the second no longer fits
		// in the first.
		kind:   KindLogCeilingShort,
		perLog: true,
		fires: func(r Reading) (string, bool) {
			if r.LogBytesPerDay == nil || r.LogMaxBytes == 0 || r.ReplayWindow <= 0 {
				return "", false
			}
			rate := float64(*r.LogBytesPerDay)
			need := rate * r.ReplayWindow.Hours() / 24
			if need <= float64(r.LogMaxBytes) {
				return "", false
			}
			holds := time.Duration(float64(r.LogMaxBytes) / rate * float64(24*time.Hour))
			return fmt.Sprintf("at the %s a day this log took in over the last "+
				"day, its %s ceiling holds %s of records, and the trim keeps %s",
				bytesHuman(int64(*r.LogBytesPerDay)), bytesHuman(int64(r.LogMaxBytes)),
				spoken(holds), spoken(r.ReplayWindow)), true
		},
		remedy: "Raise the log's ceiling with `crewlet retention set-capacity` " +
			"during a maintenance window, to at least the window's worth at this " +
			"rate, or shorten `stream.tracker_retention.min_age` if the " +
			"deployment does not need that window. Unblocking the trim cannot " +
			"help: it never removes a record younger than min_age.",
	},
	{
		// ONE THRESHOLD. The form this replaces set a blocked flag once
		// the newest backup passed backup_max_age and then alarmed once
		// THAT had been true for backup_max_age again — so a 24-hour
		// policy alarmed at 48 hours, eight missed six-hourly runs after
		// the first one that mattered.
		//
		// TWO STATES, because a company with no backup at all is not a
		// company with an old one. Both fire — the trim does not advance
		// either way and an operator has to hear it — but they are
		// different facts and the detail says which. Fabricating an age
		// to make the first condition cover the second is what this
		// replaces, and it put "the newest verified backup is 25h0m0s
		// old" in the log of a company four seconds after its first
		// boot, one line above the trim term saying no backup had been
		// recorded at all.
		kind: KindBackupAge,
		fires: func(r Reading) (string, bool) {
			if r.BackupMaxAge <= 0 {
				return "", false
			}
			if r.BackupAge == nil {
				return fmt.Sprintf("no verified backup has been recorded, and "+
					"the policy asks for one every %s", spoken(r.BackupMaxAge)), true
			}
			return fmt.Sprintf("the newest verified backup is %s old, and the "+
					"policy asks for %s", spoken(*r.BackupAge), spoken(r.BackupMaxAge)),
				*r.BackupAge > r.BackupMaxAge
		},
		remedy: "Run `crewlet backup` against any node, whatever its roles, " +
			"and check whatever was meant to run it. The trim does not " +
			"advance past a backup older than the policy, and does not " +
			"advance at all until there is one.",
	},
	{
		// A BLOCKED TRIM IS AN ORDINARY STATE until it has cost something,
		// and every fresh deployment is in it: blocked on its first backup,
		// on its first snapshot donors, on a log nobody has written to. The
		// form this replaced fired on the block itself, so every young fleet
		// raised it within one heartbeat and held it for days, and `crewlet
		// retention status` exited non-zero on a fleet with nothing wrong.
		//
		// AT THE REPLAY WINDOW PLUS ONE TICK (ADR-0015), because `min_age`
		// is the number the configuration already holds every SANCTIONED
		// block under: a node joining inside its rejoin window (whose ceiling
		// is the window's floor), a young fleet waiting for its donors (the
		// cross-field rule keeps `snapshot_interval` × (kept + 1) below it),
		// a trim waiting for its first backup (which `backup_age` raises on
		// its own: from the first reading where none has been taken, at the
		// policy's age where one has gone stale). The tick is the block's
		// resolution (`blocked_since` moves only when the trim does), so a
		// block that cleared at the window is seen to clear up to one tick
		// later.
		//
		// AND ONLY WHILE THE LOG HOLDS A RECORD PAST THE WINDOW, which is
		// the half that is measured rather than borrowed: a block that keeps
		// nothing the age term would release has cost nothing, however long
		// it has lasted. A knowledge base nobody writes to is blocked for
		// the life of the deployment.
		//
		// THE TERM IS NOT IN THE SENTENCE. It is a field of the log the
		// alarm stands on ([AlarmDomain.Term]), because one alarm carries
		// every held log and two of them can be held by different terms.
		kind:   KindTrimBlocked,
		perLog: true,
		fires: func(r Reading) (string, bool) {
			if r.TrimBlockedBy == "" || r.ReplayWindow <= 0 || r.TrimPastWindow == 0 {
				return "", false
			}
			return fmt.Sprintf("the trim has not advanced for %s, and the log is "+
					"keeping up to %d record(s) older than its %s replay window",
					spoken(r.TrimBlockedFor), r.TrimPastWindow, spoken(r.ReplayWindow)),
				r.TrimBlockedFor > r.ReplayWindow+TrimInterval
		},
		remedy: "The term holding each log names what to fix, and `crewlet " +
			"retention status` prints it with what it has and what it wants. " +
			"Until it is fixed the log grows toward its ceiling by a day of " +
			"records every day.",
	},
	{
		// PER LOG, like the headroom, the ceiling and the blocked trim: the
		// report evaluates it once for each log this node holds a record on,
		// because what the grace does is the log's: every log's copy is
		// judged on its own (ADR-0049), see [Reading.DeferredAge].
		kind:   KindDeferredOld,
		perLog: true,
		fires: func(r Reading) (string, bool) {
			what := "the oldest record on this log that this node cannot apply"
			if r.DeferredRecord != "" {
				what += " (" + r.DeferredRecord + ")"
			}
			return fmt.Sprintf("%s has been held for %s, past the %s deferral grace, "+
					"at which this node's copy of this log goes out of service", what,
					spoken(r.DeferredAge), spoken(DeferralGrace)),
				r.DeferredAge > DeferralGrace
		},
		remedy: "This node is running a build that cannot decode records its peers " +
			"are writing. Upgrade it. Its copy of each log named has already gone " +
			"out of service, and what the estate routes to that log is answered " +
			"by the other data nodes; every other log's copy keeps serving here. " +
			"On the identity log, what this node answers from its own rows is " +
			"not routed: the sign-ins, sessions and tokens of anybody the held " +
			"record may be about, and every directory decision, answer 503 here " +
			"until the node can read the record.",
	},
	{
		// PER LOG, for the same reason: a read of one log refuses on that
		// log's floor, and a floor published ahead of this node is one
		// log's re-anchor.
		kind:   KindFloorUnknown,
		perLog: true,
		fires: func(r Reading) (string, bool) {
			detail := fmt.Sprintf("this log's trim floor has been unreadable here "+
				"for %s", spoken(r.FloorUnknownFor))
			if r.FloorUnknownCause != "" {
				detail += ": " + r.FloorUnknownCause
			}
			return detail, r.FloorUnknownFor > FloorCacheStale
		},
		remedy: "Read the cause. An unreachable coordination store clears when " +
			"coordination does, and every log on this node says so at once. A " +
			"log's row in the register that does not read as its floor is named " +
			"by the trim's `retention_trim_failed` line and by every write it " +
			"refuses, with the key to remove. A floor published at a later " +
			"generation than this node's means that log was re-anchored and this " +
			"node was not (see re-anchoring in docs/guides/retention.md). Every " +
			"read of a named log on this node, and its every write at an " +
			"expectation of zero, is refused until its floor can be read. A log " +
			"whose floor has gone unread for less than four heartbeats is not " +
			"named: its reads are still served from the floor it last read, " +
			"though its writes at an expectation of zero are already refused. See " +
			"docs/guides/retention.md, \"An unreadable trim floor\".",
	},
	{
		kind: KindPrefetchSlow,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the knowledge scans this node ran for turns' prefetches "+
					"are past their %s budget at p95: up to %s", spoken(PrefetchScanBudget),
					spoken(r.PrefetchP95)),
				r.PrefetchP95 > PrefetchScanBudget
		},
		remedy: "Every turn whose knowledge prefetch this node scans waits for the " +
			"scan before its first token, whichever node the turn runs on. Check " +
			"this node's store latency (`pool_starved`, `barrier_slow`) and the " +
			"corpus against what one node's share can scan in the budget: adding " +
			"a data node divides the buckets again, with no configuration and no " +
			"rebuild. See docs/guides/search.md.",
	},
	{
		kind: KindSearchSlow,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("interactive search is past its %s target at p95: up to %s",
					spoken(InteractiveSearchTarget), spoken(r.SearchP95)),
				r.SearchP95 > InteractiveSearchTarget
		},
		remedy: "The corpus has outgrown what one node's share can scan in " +
			"the budget. Adding a node divides the buckets again, with no " +
			"configuration and no rebuild. See docs/guides/search.md.",
	},
	{
		kind: KindSearchDegraded,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%.0f%% of searches were answered without their "+
					"semantic half", r.SearchDegradedFraction*100),
				r.SearchDegradedFraction > 0
		},
		remedy: "The embeddings provider or the vector domain is failing, or a " +
			"node's copy of the vectors is out of service (its " +
			"`estate_copy_not_served` line names the fault), which the asking " +
			"node answers by giving that node's range (its own included) to " +
			"the others for half a minute. Search still answers; it answers " +
			"less well, and silently.",
	},
	{
		kind: KindSearchScoped,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%.0f%% of searches were answered over part of the "+
				"corpus", r.SearchScopedFraction*100), r.SearchScopedFraction > 0
		},
		remedy: "A node did not cover its bucket range, so part of the corpus " +
			"went unscanned: it was unreachable; or its own lexical index has " +
			"not finished its first lap, which is what a node that joined a " +
			"few minutes ago looks like and clears itself; or its copy of what " +
			"the search reads is out of service (its `estate_copy_not_served` " +
			"line names the domain and the fault), which does NOT clear itself: " +
			"it lasts until that copy recovers or the node adopts a snapshot. " +
			"Either reply costs only the search that hears it (the asking node " +
			"gives that node's range, its own included, to the others for " +
			"half a minute), so a " +
			"fraction that stays up is a node that keeps answering so. The " +
			"answers were complete for what was searched and silent about what " +
			"was not; the log line names who was absent.",
	},
	{
		// THE BUDGET IS BORROWED (ADR-0015): what makes a history answer
		// partial is a node that did not answer inside the fleet read
		// budget, so that budget is the threshold — named in the detail
		// from the value the reading carries, which is the constant the
		// scatter waits on ([Reading.HistoryReadBudget]), never restated.
		// PROCESS-WIDE rather than per log: the reads are of each node's
		// own event log, which is no state log at all.
		kind: KindHistoryPartial,
		fires: func(r Reading) (string, bool) {
			budget := "the fleet read budget"
			if r.HistoryReadBudget > 0 {
				budget = "the " + spoken(r.HistoryReadBudget) + " fleet read budget"
			}
			return fmt.Sprintf("%.0f%% of fleet history reads were answered without "+
					"every node, because one did not answer inside %s",
					r.HistoryPartialFraction*100, budget),
				r.HistoryPartialFraction > 0
		},
		remedy: "Turn-level history lives only on the node that published it, so " +
			"a partial answer is missing that node's rows, and every answer names " +
			"the node in its `coverage`. A node that has left the fleet is gone " +
			"with its detail, and the aggregates survive it in the usage domain; " +
			"a live node that keeps missing the budget is slow on its own store " +
			"or its route, and its own `pool_starved` and `apply_lag` alarms say " +
			"which.",
	},
	{
		kind: KindRecallBelowFloor,
		fires: func(r Reading) (string, bool) {
			if r.SemanticCoverage == nil {
				return "", false
			}
			return fmt.Sprintf("%.0f%% of the corpus has current vectors, against a "+
					"%.0f%% floor", *r.SemanticCoverage*100, SemanticCoverageFloor*100),
				*r.SemanticCoverage < SemanticCoverageFloor
		},
		remedy: "The embed duty is behind. Semantic recall is answering from a " +
			"corpus it does not cover.",
	},
	{
		// THE FLOOR IS THE EVALUATION'S, borrowed (ADR-0015): the recall
		// curve `crewlet search eval` judges a corpus against, at the size
		// of the corpus the shape searched. A training installs the
		// smallest probe count that meets it in every shape, and a
		// re-measurement that finds none within the ceiling retrains in the
		// same tick rather than recording the failure — so a recall below
		// it was measured probing EVERY list, the full scan's own pool, and
		// says the codes fail this corpus, not that the index was trained
		// badly.
		kind: KindIVFRecallBelowFloor,
		fires: func(r Reading) (string, bool) {
			if r.IVFRecall == nil {
				return "", false
			}
			return fmt.Sprintf("the semantic index's latest measurement found "+
					"recall %.4f against the exact scan in the %s shape over %d "+
					"sources, below its %.4f floor", *r.IVFRecall, r.IVFShape,
					r.IVFMeasuredOn, r.IVFRecallFloor),
				*r.IVFRecall < r.IVFRecallFloor
		},
		remedy: "The 1-bit first stage is failing this corpus, index or not: " +
			"`crewlet search eval` against a backup's copy of the estate " +
			"measures the full scan beside the index in every query shape and " +
			"will say the same. The remedy is the evaluation's: raise " +
			"BinaryOversample, then an int8 first stage, both code changes (see " +
			"docs/guides/search.md). No index is installed meanwhile, so " +
			"searches answer from the full scan at the recall the evaluation " +
			"reports.",
	},
	{
		kind: KindRecordsGated,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d record(s) were dropped by an apply gate in the "+
				"last day", r.RecordsGated), r.RecordsGated > 0
		},
		// THE FIELDS BY THEIR KEYS ON THE LINE, because the one fact an
		// operator needs from it — which node wrote what nothing will apply
		// — is under `writer`, and a remedy that called it "the operator"
		// sent the reader looking for a field no line carries.
		// TestTheRecordsGatedRemedyNamesWhatItsLineCarries holds every field
		// named here to the line the applier writes.
		remedy: "A gated record is recoverable by nothing. Each drop's " +
			"`statelog_record_gated` log line names the `gate` that dropped it, " +
			"the record's `position` and `kind`, and the `writer` (the node " +
			"that wrote it); this is worth reading today.",
	},
	{
		// NOT `feed_dead_letters`, WHICH NAMED A PATH THIS ENGINE DOES
		// NOT HAVE. Both domain consumers set `MaxDeliver: -1`
		// deliberately — a record nobody can handle yet is a retry
		// rather than a poison message, and a delivery budget that ran
		// out would drop a wake silently — so nothing ever reaches a
		// dead letter and an alarm counting them could never fire. What
		// an operator actually has to know about is the state that
		// decision creates: a record circling for ever, at the head of a
		// consumer, with everything behind it waiting.
		kind: KindFeedUnreadable,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d change record(s) could not be translated in the "+
				"last day", r.FeedUnreadable), r.FeedUnreadable > 0
		},
		remedy: "A record no build on this node can read. It redelivers for ever " +
			"rather than being dropped, so the wakes behind it are waiting too. " +
			"Upgrade the node past it, or the feed stops moving.",
	},
	{
		kind: KindMaintenanceOpen,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("a maintenance operation has been open for %s in "+
					"phase %q", spoken(r.MaintenanceOpenFor), r.MaintenancePhase),
				r.MaintenanceOpenFor > MaintenanceAlarmAfter
		},
		remedy: "Maintenance stops every publisher on every node. Finish it or " +
			"abandon it; nothing is being written while it is open.",
	},
	{
		kind: KindVolumeLow,
		fires: func(r Reading) (string, bool) {
			if r.StoreVolumeUnmeasured != "" {
				return fmt.Sprintf("a store volume could not be measured (%s), so nothing "+
					"shows the next restore, vacuum or snapshot has room for a second "+
					"copy", r.StoreVolumeUnmeasured), true
			}
			detail := fmt.Sprintf("%s free on the volume holding %s against %s of store "+
				"there, and the next restore, vacuum or snapshot needs room for a "+
				"second copy", bytesHuman(r.FreeBytes), r.StoreVolume,
				bytesHuman(r.StoreBytes))
			if r.CopyHeldBytes > 0 {
				detail += fmt.Sprintf(" and for the %s of write-ahead log a copy holds "+
					"back there while it runs", bytesHuman(r.CopyHeldBytes))
			}
			return detail, r.StoreBytes > 0 && float64(r.FreeBytes) <
				VolumeHeadroomFactor*float64(r.StoreBytes)+float64(r.CopyHeldBytes)
		},
		remedy: "Add space to the volume the alarm names. Where it could not be " +
			"measured, fix what stops it being read. A backup, a vacuum and a peer's " +
			"join all need the room, and each fails partway through without it; a " +
			"backup's or a snapshot's copy also needs room there for the write-ahead " +
			"log it holds back while it runs, which the alarm counts.",
	},
	{
		kind: KindWALLarge,
		fires: func(r Reading) (string, bool) {
			detail := fmt.Sprintf("the write-ahead log is %s", bytesHuman(r.WALBytes))
			if r.WALFile != "" {
				detail = fmt.Sprintf("the write-ahead log beside %s is %s", r.WALFile,
					bytesHuman(r.WALBytes))
			}
			if r.WALCopy != "" {
				detail += fmt.Sprintf(", held there for %s by a copy to %s, which "+
					"folds it back in when it ends", spoken(r.WALCopyFor), r.WALCopy)
			}
			return detail, r.WALBytes > WALAlarmBytes
		},
		remedy: "A checkpoint is not happening past what something still reads. When " +
			"the alarm names a copy — a backup's or a donor snapshot's — the copy holds " +
			"the log back for its whole run, so it grows by what the node writes " +
			"meanwhile, and the first checkpoint after the copy ends folds it back in " +
			"and clears the alarm: make room for it, or take the copy at a quieter hour. " +
			"When it names none, a reader is holding a snapshot open, and the log has " +
			"no other symptom until the volume fills.",
	},
	{
		kind: KindPoolStarved,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("a database connection takes past %s at p95 to acquire: up to %s",
				spoken(PoolWaitAlarm), spoken(r.PoolWaitP95)), r.PoolWaitP95 > PoolWaitAlarm
		},
		remedy: "Raise `store.max_open_conns`, or find the caller holding one. " +
			"Every read on this node is queuing before it starts.",
	},
	{
		// THE ONE THAT SAYS AN INPUT HAS STOPPED BEING TRUE. Every sizing
		// decision under this framework — the log's ceiling, the trim's
		// cadence, a joining node's transfer — was derived from a read
		// rate. At twice it, they are describing a different company.
		//
		// PER LOG, like the headroom: each log was sized for its own
		// census and is past it on its own, so the report evaluates it once
		// per log and lists every log past its census under the one alarm
		// ([Alarm.Domains]). A reading naming only the log furthest past its
		// census hid every other log past its own.
		kind:   KindCensusDrift,
		perLog: true,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d linearizable reads a day against the %d this "+
					"log was sized for", r.LinearizableReads,
					r.LinearizableReadsExpected),
				r.LinearizableReadsExpected > 0 &&
					float64(r.LinearizableReads) > CensusDriftFactor*float64(r.LinearizableReadsExpected)
		},
		remedy: "Re-derive the log's ceiling and the trim's cadence from the real " +
			"rate. See `stream.tracker_retention` in " +
			"docs/getting-started/configuration.md. A code log's git census is a " +
			"share of the ceiling its stream holds, so it moves only when the log " +
			"is resized: `crewlet retention set-capacity CREWLET_CODE_LOG` " +
			"(docs/guides/retention.md); `stream.code_log_max_bytes` sizes only a " +
			"log not yet created.",
	},
	{
		kind: KindObjectsMissing,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d file(s) are missing from the object store, damaged "+
				"in it, or not answered for by it", r.ObjectsMissing), r.ObjectsMissing > 0
		},
		remedy: "The backend lost or changed bytes it had acknowledged, or will not " +
			"serve them: check its own health (the NATS bucket OBJ_crewlet_files on " +
			"the data nodes, or the S3 bucket) and restore those files from a backup, " +
			"or upload them again (see docs/guides/backup.md). `crewlet objects " +
			"status` names them, and what was found of each.",
	},
	{
		kind: KindObjectsBucketNearCeiling,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the nats object bucket holds %s of its %s reservation",
					bytesHuman(r.ObjectsBucketBytes), bytesHuman(r.ObjectsBucketMaxBytes)),
				r.ObjectsBucketMaxBytes > 0 && float64(r.ObjectsBucketMaxBytes-r.ObjectsBucketBytes) <
					ObjectsHeadroomAlarmFraction*float64(r.ObjectsBucketMaxBytes)
		},
		remedy: "Once the bucket is full every upload is refused. Raise its reservation " +
			"(`nats stream update OBJ_crewlet_files --max-bytes=<bytes>`, within what the " +
			"broker can grant), or move the files to an S3-compatible bucket with " +
			"`crewlet objects migrate --to s3` (see docs/concepts/object-store.md).",
	},
	{
		// ONE ALARM FOR EVERY BURNING OBJECTIVE, the detail naming each:
		// the kind is the alarm's identity on the gauge and the screen, and
		// a burn is one cause (this node failing what it promised) whichever
		// objective shows it first.
		kind: KindObjectiveBurn,
		fires: func(r Reading) (string, bool) {
			if len(r.ObjectiveBurns) == 0 {
				return "", false
			}
			parts := make([]string, 0, len(r.ObjectiveBurns))
			for _, b := range r.ObjectiveBurns {
				parts = append(parts, fmt.Sprintf("%s is spending its error budget %.1f "+
					"times as fast as its objective allows over the last %s and %.1f "+
					"times over the last %s (the %s pair fires at %g)", b.Objective,
					b.LongRate, spoken(b.Window.Long), b.ShortRate, spoken(b.Window.Short),
					b.Window.Pace, b.Window.Factor))
			}
			return strings.Join(parts, "; "), true
		},
		remedy: "Find the failing events where the objective is measured " +
			"(docs/reference/service-objectives.md names each one's source and its own " +
			"instrument), starting from `crewlet.objectives.events` by `objective` and " +
			"`outcome`. A fast burn spends a fiftieth of the month's error budget every " +
			"hour and needs attention now; a slow burn spends a twentieth every six hours. " +
			"The alarm clears within minutes of the fault ending, once the short window " +
			"is healthy again.",
	},
	{
		// THE THRESHOLD IS THE ASSUMPTION ITSELF (ADR-0015), handed in with
		// the measure: objstore.AssumedClockSkew is what the collector's
		// grace and a retired reference's grace were sized against, so the
		// alarm fires exactly where a clock stops being one those rules hold
		// on. AT the bound is still inside it, as every rule here is silent at
		// its threshold.
		//
		// TWO CLOCKS, EACH NAMED: this node's and the broker member's leading
		// the position register, each against the fleet's — the median of the
		// data nodes' clocks — and the detail says which stands past the bound,
		// because that is the machine whose clock an operator has to fix. A
		// node measured against the member alone read the member's drift as
		// its own and sent the operator to the wrong machine.
		kind: KindClockSkew,
		fires: func(r Reading) (string, bool) {
			if r.ClockSkewBound <= 0 {
				return "", false
			}
			fleet := fleetClock(r.ClockSkewNodes)
			var off []string
			if magnitude(r.ClockSkew) > r.ClockSkewBound {
				off = append(off, fmt.Sprintf("this node's clock runs %s %s %s",
					spoken(r.ClockSkew), clockSide(r.ClockSkew), fleet))
				fleet = "it"
			}
			if magnitude(r.BrokerClockSkew) > r.ClockSkewBound {
				off = append(off, fmt.Sprintf("the clock of the broker member leading the "+
					"position register runs %s %s %s", spoken(r.BrokerClockSkew),
					clockSide(r.BrokerClockSkew), fleet))
			}
			if len(off) == 0 {
				return "", false
			}
			return fmt.Sprintf("%s, past the %s every deletion that judges one machine's "+
				"instant by another machine's clock assumes", strings.Join(off, ", and "),
				spoken(r.ClockSkewBound)), true
		},
		remedy: "Run an NTP client (chrony, systemd-timesyncd) on the machine whose clock " +
			"the alarm names. Where it names this node's clock, that is this node. Where it " +
			"names the clock of the broker member leading the position register, every node " +
			"raises it alike and the clock to fix is that member's alone: on an embedded " +
			"broker one of the data nodes — the one also raising this alarm about its own " +
			"clock — and on an external NATS cluster the server leading the register's " +
			"stream, which `nats stream info KV_crewlet_statelog_positions` names. With two " +
			"data nodes their median is the midpoint, which neither clock outvotes: both are " +
			"named once they stand more than twice the bound apart, and either may be the one " +
			"that moved. Two deletions rest on every node and that member keeping within the " +
			"bound of the fleet's clock, and so on any two machines keeping within twice it: " +
			"the object collector deletes an object once its key is a day old by the " +
			"collector's clock, while a write may still name a key twelve hours old by the " +
			"deciding node's; and the code host's code-retention duty sweeps a retired pack's " +
			"row once the broker's instant on its retirement is a day old by the duty holder's " +
			"clock, while a clone that began on the pack may still be reading it for most of " +
			"that day. A clock past the bound can delete an object a write or a read still " +
			"needs. The measure is the coordination store's position register, read on every " +
			"heartbeat: each data node's row carries the instant the node stamped on it and the " +
			"instant the broker stored it, so each says how far that node's clock stands from " +
			"the member's, and the median of them is the fleet's clock, off by no more than a " +
			"heartbeat's own work between the two instants. See docs/guides/deployment.md.",
	},
	{
		// THE THRESHOLD IS THE NODE'S OWN BUDGET, measured rather than set:
		// the git cache reads the node's memory and keeps back what its
		// other work is sized to hold, and a bitmap past what is left is
		// one the kernel kills at every duty cycle — so the number that
		// already decides whether a bitmap is written is the number the
		// alarm fires at. Silent at the budget, as a bitmap at it is written.
		kind: KindCodeMaintenanceRefused,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d repository(ies) on this node are served without a bitmap: "+
					"the largest's needs about %s, and the node can give its background "+
					"work %s", r.CodeMaintenanceRefused,
					bytesHuman(r.CodeMaintenanceLargest), bytesHuman(r.CodeMaintenanceBudget)),
				r.CodeMaintenanceLargest > r.CodeMaintenanceBudget
		},
		remedy: "Each repository is still served and still indexed: the cache " +
			"writes its multi-pack-index without the bitmap, so lookups stay " +
			"fast, but a clone counts objects by walking the history rather " +
			"than reading the bitmap: slower, never wrong. A bitmap is charged " +
			"at 600 bytes an object of the repository, and 240 more for room " +
			"beside it for a clone of the repository; each " +
			"`code_cache_maintenance_refused` line names a repository, the step " +
			"refused and its charge (a step `index` or `graph` refused as well " +
			"is a repository served from its packs' own indexes; a step " +
			"`publish` is a repository this node maintains whose next published " +
			"layer it cannot write, at 600 bytes an object of the repository, " +
			"which falls to this node because no git node advertises a budget " +
			"that holds it — give one git node that memory, and it becomes the " +
			"repository's maintainer: docs/guides/deployment.md, Sizing git " +
			"nodes). The budget is " +
			"the node's memory (its cgroup's limit where one is set, read again " +
			"every ten seconds) less what it keeps for its git lanes, the " +
			"store, the broker and the engine, on the `code_cache_opened` line. " +
			"Give the node more memory; a container resized in place is held to " +
			"its new limit within ten seconds, without a restart. Or, where " +
			"git.max_transfers is set, give it fewer transfers. What the node " +
			"keeps is fixed when it boots: 224 MiB for every transfer its git " +
			"lane admits, whatever it serves, and 896 MiB for each core its " +
			"memory backs at 2 GiB a core. An unset git.max_transfers is sized " +
			"to the memory, no more than one for every 512 MiB, and leaves the " +
			"background work 256 MiB for each backed core; a figure set by hand " +
			"is kept as written, and leaves it nothing once its transfers' " +
			"224 MiB each reach the memory less the cores' 896 MiB each: at as " +
			"little as 56% of the transfers the memory holds at 224 MiB each, " +
			"where the node has a core for every 2 GiB of it or more. A budget " +
			"of zero clears when git.max_transfers is lowered or unset, or when " +
			"the node is given memory past what it keeps, in place or at a " +
			"boot. A restart alone keeps the same lanes over the same memory, " +
			"and clears it only where git.max_transfers is unset and the memory " +
			"was lowered after the node booted, since the lanes are then sized " +
			"to what it has. See docs/concepts/code-host.md.",
	},
	{
		// THE THRESHOLD IS THE PUSH DECIDE'S (ADR-0015): the most live packs
		// a repository may hold, past which a push carrying a pack is refused
		// compaction_behind. This framework imports no code-host package, so
		// the node's compactor counts the repositories it already judged
		// against that bound and the rule fires at any — never at a number of
		// its own. Raised by the node that COMPACTS them, the repository's
		// maintainer, because it is the one node that can act on it and the
		// receiver refuses most such pushes before any committer sees them;
		// held from the listing of every pass, whatever that pass's compaction
		// of the repository decided, so it stands for as long as pushes are
		// refused.
		kind: KindCodeCompactionBehind,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d repository(ies) this node maintains hold the most live packs a "+
					"repository may — the most, %s, holds %d — so every push carrying a pack "+
					"into them is refused compaction_behind until a compaction of theirs lands",
					r.CodeCompactionBehind, r.CodeCompactionBehindRepo, r.CodeCompactionBehindLargest),
				r.CodeCompactionBehind > 0
		},
		remedy: "Every push carrying objects adds a pack, and the repository's " +
			"maintainer merges them on a minute's poll; pushes that carry " +
			"nothing — deletions, refs moved onto commits the repository holds — " +
			"still land meanwhile. Read this node's log for the repository. " +
			"`code_cache_compaction_refused` says the build's memory charge, on " +
			"the line beside the budget, is past what this node can give its " +
			"background work: give it more memory, or fewer git transfers, as " +
			"for a refused bitmap. `code_compaction_refused` names a committer's " +
			"refusal, a repository deleted, or an object store with no room " +
			"(`store_full`: make the room the line names, and the compaction is " +
			"tried again once the store has it). `code_compaction_failed` names " +
			"the build, an upload or the committer's answer that failed, each " +
			"tried again at the next poll. With none of those and the count " +
			"still standing, the repository takes pushes faster than one node " +
			"merges them: `crewlet.code.compact.duration` beside the pushes' " +
			"rate says by how much. See docs/concepts/code-host.md.",
	},
	{
		// THE THRESHOLD IS THE COMMITTER'S OWN BOUNDS (ADR-0015): a
		// repository's queue — its count, its bytes, and how long a proposal
		// may wait in it, its asker's attempt less a batch and the answer —
		// and the node's bytes across every repository it commits for, at
		// which the committer already refused each proposal it counts. This
		// framework imports no code-host package, so the rule fires at any
		// count, never at a number of its own. Only a BACKLOG is counted,
		// because only a backlog says this committer turns proposals away —
		// the queue's time bound among them: a committer slower than its
		// pushes meets it before its count, and turns every push past it away
		// for as long as they keep coming. A proposal that arrived with its
		// asker's attempt already spent is a cause upstream of the committer,
		// and a record that lost every compare-and-set round is a second
		// committer during a handoff — both counted by crewlet.code.commit.busy
		// and logged, never alarmed. Raised by the node that decides the
		// repository's pushes, whatever node received them, since it is the
		// one whose queue is full or slow.
		kind: KindCodeCommitBusy,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d proposal(s) were refused busy in the last %s because this node's "+
					"committer could not hold them or reach them in time — %d for %s alone, %d at the "+
					"node's whole bound across its repositories, %d after waiting in the queue until their "+
					"asker could wait no longer — each deciding nothing, and sent again by whoever made it; "+
					"read code_commit_busy in this node's log",
					r.CodeCommitBusy, spoken(TrimInterval), r.CodeCommitBusyOfRepo, r.CodeCommitBusyRepo,
					r.CodeCommitBusyNode, r.CodeCommitBusyWaited),
				r.CodeCommitBusy > 0
		},
		remedy: "Nothing of a refused proposal landed, and nothing is lost: a git " +
			"client runs the push again, a compaction is planned again at its " +
			"node's next pass and a change's write is sent again, each paying " +
			"another round trip, and a person meeting `busy` pushes again " +
			"themselves. A repository's queue fills when it takes pushes faster " +
			"than one committer commits them (docs/concepts/scaling.md measures " +
			"the rate), so spread the pushes over repositories: each has a queue " +
			"and a chain of records of its own, and repositories rank different " +
			"data nodes first. A proposal that waited in the queue until its " +
			"asker could wait no longer is the same backlog drained too slowly " +
			"for the queue to fill: the committer's batches take longer than the " +
			"pushes keep arriving — the code log slow to append " +
			"(`crewlet.statelog.publish.duration` on the `code` domain), or a " +
			"repository's protection rules costly to decide — so the same " +
			"remedies hold, and `crewlet.code.commit.batch.size` beside the busy " +
			"counter says how full the batches ran. The node's whole bound is " +
			"one node committing for too many busy repositories at once: each " +
			"repository's committer is the first live data node in its " +
			"rendezvous order, so more data nodes spread them. A seat pushing in " +
			"a loop is held to `code.limits.pushes_per_seat_day` a day. Each " +
			"`code_commit_busy` line in this node's log names a repository and a " +
			"minute, with how many proposals were refused at its own queue " +
			"(`queue`), at the node's bound (`node`), after waiting in the queue " +
			"until their asker could wait no longer (`waited`), too late for " +
			"their asker when they arrived (`late`) and after every " +
			"compare-and-set round (`contended`); the last two raise no alarm, " +
			"and the `reason` attribute of `crewlet.code.commit.busy` counts all " +
			"five. See docs/concepts/code-host.md.",
	},
	{
		// THE THRESHOLD IS EACH QUEUED CHECK'S OWN TIMEOUT (ADR-0015): a
		// check's definition says how long it may run, which is how long its
		// author expects its result to take, so an attempt that waited that
		// long for a runner before it even began is one its change waits on
		// past anything its checks file planned for. One threshold of the
		// table's would fire on every long check's ordinary queue and never
		// on a one-minute lint stuck for an hour. This framework imports no
		// code-host package, so the reader judges each attempt at its own
		// timeout and the rule fires at any count. Raised by every data node
		// from its rows, never by a runner: a fleet with no live git node
		// runs no check, and still raises it.
		kind: KindCodeChecksBacklog,
		fires: func(r Reading) (string, bool) {
			count := fmt.Sprintf("%d", r.CodeChecksBacklog)
			if r.CodeChecksBacklogMore {
				// PAST THE READING'S BOUND the count is the bound, and the
				// sentence says there are more rather than how many.
				count = "more than " + count
			}
			return fmt.Sprintf("%s queued check attempt(s) have waited for a runner longer than their "+
					"own timeout — the longest of those counted, %s, for %s",
					count, r.CodeChecksBacklogCheck, spoken(r.CodeChecksBacklogOldest)),
				r.CodeChecksBacklog > 0
		},
		remedy: "A queued attempt waits for a git node that runs checks to " +
			"claim it, so the backlog is no git node claiming — the fleet's git " +
			"nodes gone, stopped or failing their checks' boxes, or none setting " +
			"CREWLET_BOX_GIT_URL (`code_checks_not_run`) — or every cell a check " +
			"may run in busy: a git node's own container cells, `git.max_checks` " +
			"of them (`crewlet.code.checks.running` pinned there), and the remote " +
			"cells, as many at once and for as long a day as the company allows. " +
			"Read the git nodes' logs and " +
			"`crewlet.code.checks.claims`: claims refused `claimed` are nodes " +
			"racing for the same attempts, which is no backlog. Against the " +
			"rate attempts end (`crewlet.code.checks.finished`) the queue's " +
			"wait is capacity: more git nodes, or more cells allowed to run at " +
			"once. Nothing is lost while it waits: a change whose required " +
			"check waits cannot merge until it runs, and an attempt cancelled " +
			"meanwhile — its head moved on — leaves the queue. See " +
			"docs/concepts/code-host.md.",
	},
}

// Evaluate reports every alarm currently true, in the table's own order.
//
// PURE, over a reading gathered once. What makes an alarm real is that the
// same evaluation feeds every surface — the gauge, the log line and the
// operator's screen — so the three can never disagree about whether something
// is wrong.
func Evaluate(r Reading) []Alarm {
	var out []Alarm
	for _, rule := range table {
		if detail, firing := rule.fires(r); firing {
			out = append(out, Alarm{Kind: rule.kind, Detail: detail, Remedy: rule.remedy})
		}
	}
	return out
}

// Frac is a measured fraction, for the two Reading fields whose zero value is
// a real and alarming measurement rather than an absent one.
func Frac(v float64) *float64 { return &v }

// PerDay is a measured daily byte count, for the one Reading field whose zero
// is a real and BENIGN measurement rather than an absent one — see
// [Reading.LogBytesPerDay].
func PerDay(v uint64) *uint64 { return &v }

// PerLogKinds is every alarm evaluated once per log rather than once per node,
// in the table's own order: the six a reader of the reference has to know list
// the logs they stand on ([Alarm.Domains]) and write a pair of log lines for
// each of those logs ([Tracker]), while counting once on the health envelope
// and the gauge.
func PerLogKinds() []Kind {
	var out []Kind
	for _, rule := range table {
		if rule.perLog {
			out = append(out, rule.kind)
		}
	}
	return out
}

// perLogKind reports whether kind is one of [PerLogKinds]: a condition whose
// log lines the [Tracker] writes once per log it stands on. A kind the table
// does not hold is not one.
func perLogKind(kind Kind) bool {
	for _, rule := range table {
		if rule.kind == kind {
			return rule.perLog
		}
	}
	return false
}

// Age is a measured age, for the Reading field whose zero value is a real and
// REASSURING measurement — a backup verified this second — rather than an
// absent one.
func Age(d time.Duration) *time.Duration { return &d }

// Kinds is every alarm this engine can raise, sorted. For the reference doc
// and for a surface that renders a row per kind.
func Kinds() []Kind {
	out := make([]Kind, 0, len(table))
	for _, rule := range table {
		out = append(out, rule.kind)
	}
	slices.Sort(out)
	return out
}

// spoken is a duration as an operator reads it rather than as a computer
// wrote it.
//
// `time.Duration.String` wrote every alarm's number: "85h58m0s", "11.39s",
// "24h0m0s" — a trailing zero field on every whole value, hundredths nobody
// measured to, and a spelling the dashboard beside it never uses. This is the
// dashboard's own convention (`fmtDuration`, lib/format.ts): unit letters with
// no space inside a field, a space between fields, two fields at most, and no
// field that is zero — "340ms", "1.2s", "45s", "2m", "1m 2s", "2h 3m", "24h".
// One reading, one spelling, whichever surface says it.
func spoken(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	pair := func(a int64, au string, b int64, bu string) string {
		if b == 0 {
			return fmt.Sprintf("%d%s", a, au)
		}
		return fmt.Sprintf("%d%s %d%s", a, au, b, bu)
	}
	// EACH MAGNITUDE ROUNDS AT ITS OWN GRAIN AND HANDS A CARRY UP, so 999.6ms
	// is "1s" and 59m59.6s is "1h" rather than "1000ms" and "60m".
	if ms := d.Round(time.Millisecond).Milliseconds(); ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	if tenths := d.Round(100 * time.Millisecond); tenths < 10*time.Second {
		// A tenth, where a tenth still means something.
		return strings.TrimSuffix(fmt.Sprintf("%.1f", tenths.Seconds()), ".0") + "s"
	}
	secs := int64(d.Round(time.Second) / time.Second)
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	if secs < 3600 {
		return pair(secs/60, "m", secs%60, "s")
	}
	mins := int64(d.Round(time.Minute) / time.Minute)
	return pair(mins/60, "h", mins%60, "m")
}

// magnitude is a signed duration's size, for a rule that fires on a clock
// ahead and a clock behind alike.
func magnitude(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// clockSide says which side of a reference clock a signed offset stands on, as
// a sentence reads it before the reference: positive is ahead.
func clockSide(d time.Duration) string {
	if d < 0 {
		return "behind"
	}
	return "ahead of"
}

// fleetClock names the fleet's clock a `clock_skew` measure stands against:
// the median of the data nodes' clocks the position register holds a recent
// row of, which with one node is that node's own.
func fleetClock(nodes int) string {
	if nodes <= 1 {
		return "the fleet's clock (this node's own: the register holds no other data " +
			"node's recent row)"
	}
	return fmt.Sprintf("the fleet's clock (the median of %d data nodes')", nodes)
}

// bytesHuman is a size in the units an operator's disk is sold in.
func bytesHuman(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// RefusalAlarmFloor is what a surface reports when it can see that reads are
// being refused for a fault but cannot say for how long.
//
// # Why a floor rather than a duration
//
// The condition is written in time — reads refused for longer than one
// reconcile interval — because a single refusal during an election is not a
// fault and a sustained one is. A recorder holds a COUNT, not an age: it can
// say a fault-class refusal happened, and cannot say when it started.
//
// So a surface with only the count reports this floor, which is one interval
// past the threshold: the alarm fires, and the detail says what was measured.
// The alternative — reporting zero because the age is unknown — is the
// three-valued mistake this whole engine is organised against, and it silences
// the alarm on exactly the fault it exists for.
const RefusalAlarmFloor = 2 * coord.ReconcileInterval
