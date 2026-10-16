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
	// apply before it stops serving its copy of the estate — its seats
	// stay, and read the estate from the other data nodes. The alarm and
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
	ReadBudget = 2 * time.Second

	// PrefetchScanBudget is what a turn's context assembly was promised.
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
	// is not happening. A gibibyte: normal operation keeps it in the tens
	// of megabytes, and the failure it names — a reader holding a snapshot
	// open for ever — has no other symptom until the volume fills.
	WALAlarmBytes = 1 << 30

	// VolumeHeadroomFactor is how much free space a node's volume must
	// have relative to its store, expressed as a multiple. A restore, a
	// vacuum and a snapshot each need room for a second copy, so 1.1 is
	// the point at which the next routine operation would fail.
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
	// do, and [Census.Background] is where they are counted. [Census]
	// turns both into one log's figure.
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
// # Plus what the engine reads on its own
//
// [Census.Background] is added to it, for the reason it gives.
func (c Census) Expected() int {
	return LinearizableReadsPerSeatDay*max(c.Seats, 1) + max(c.Background, 0)
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

	// BackupAge is how old the backup one log's trim counts is — the
	// newest verified point the policy counts that covers the log — and
	// BackupMaxAge what the operator asked for. A zero BackupMaxAge is a
	// node with no backup policy, which is not an alarm — it is a
	// deployment that has said it does not want one.
	//
	// A PER-DOMAIN CONDITION, evaluated once per log from the report's
	// rows ([ReportInputs.BackupMaxAge]) for the reason the headroom is:
	// the logs' backups have one age each, and the one a node-wide
	// reading carried was the newest point's, whatever it covered.
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
	// sighting of it, the instant [Health.Healthy] measures the grace that
	// takes this node's copy out of service from, through
	// [DeferredSince.Age] — and DeferredRecord which record that is, in the
	// words an operator upgrades by: its position and the record version it
	// was written at against the one this build reads. Per DOMAIN, like
	// HeadroomFraction, and filled by the report from [DomainInputs] rather
	// than by a node-wide caller.
	//
	// DeferredJudgesCopy is whether that log is a [Domain.ReadinessInput] —
	// the tracker's and the knowledge base's are; the identity estate's,
	// the vectors' and the usage domain's are not — which is whether the
	// grace is ALSO when this node stops serving its copy of the estate
	// (its seats stay, and read it from the other data nodes). The alarm
	// fires at the grace either way, because an upgrade is the remedy
	// either way; what it may not do is tell an operator a node's copy was
	// taken out of service over a log that judges none — or that it was
	// not, over the one log among several that took it out, which is why
	// the three are a LOG's rather than the node's oldest.
	DeferredAge        time.Duration
	DeferredRecord     string
	DeferredJudgesCopy bool

	// FloorUnknownFor is how long one log's trim floor has been unreadable
	// on this node — from the first alarm heartbeat that could not read it
	// to the latest, never a persistence nobody observed — and
	// FloorUnknownCause what the latest read said: an unreachable
	// coordination store and a floor published at a generation this node
	// has not reached are one refusal with two remedies. Per DOMAIN too:
	// the refusal is that log's, and one alarm naming the node's
	// longest-unreadable floor hid every other log re-anchored beside it.
	FloorUnknownFor   time.Duration
	FloorUnknownCause string

	// PrefetchP95 and SearchP95 are the two latencies a turn waits on.
	PrefetchP95, SearchP95 time.Duration

	// SearchDegradedFraction and SearchScopedFraction are the fractions of
	// searches answered without the semantic half and without the whole
	// corpus.
	SearchDegradedFraction, SearchScopedFraction float64

	// HistoryPartialFraction is the fraction of this node's fleet history
	// reads answered without every live data node, and HistoryReadBudget the
	// budget those reads waited on for the other data nodes —
	// internal/eventfan's FleetReadBudget, which a node that did not answer
	// inside is what makes an answer partial.
	//
	// THE BUDGET TRAVELS WITH THE READING rather than being imported here,
	// because the scatter that owns it is a layer ABOVE this framework: it
	// reads every data node's own event log, and the framework every replicated
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
	// not share one — the one with the least room for a second copy of what
	// it holds. WALBytes is the larger write-ahead log's size.
	FreeBytes, StoreBytes, WALBytes int64
	StoreVolume                     string

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

	// LogCeilingField is the Tier A field that sets this log's byte ceiling
	// (`stream.iam_log_max_bytes`, say), which a log past its census names
	// in its own line: each log's ceiling is its own field, sized from
	// what that log holds, and one remedy naming the mutation log's sent an
	// operator of every other log to the wrong field. Empty where the
	// caller named none. Per DOMAIN, filled by the report from
	// [DomainInputs.CeilingField].
	LogCeilingField string

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
	// hold, or holds wrong — at another size, or under another digest — as
	// the collector's last audit to run to its end found: on the node
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
}

// Alarm is one condition currently true on this node.
//
// TAGGED, like every other struct in [Report], and the tags are load-bearing
// for a reader this package does not have yet.
//
// This is reached through `GET /worklet/retention`, so its field names are a wire
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
	// logs whose backup is missing or older than the policy, the logs
	// holding a record past the deferral grace, the logs whose floor cannot
	// be read and the logs past their census. Empty for a node-wide
	// condition.
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

	// Detail is this log's own measurement, in the operator's units —
	// under [KindBackupAge], followed by its held trim's where Term is set,
	// since that log raises no [KindTrimBlocked] to say it.
	Detail string `json:"detail"`

	// Term is the trim term holding this log, where the alarm is about its
	// trim: under [KindTrimBlocked], and under [KindBackupAge] for a log
	// whose trim the backup term is holding, which raises no
	// [KindTrimBlocked] of its own. It is the term's own name, which is what
	// `crewlet retention status` prints and the retention guide is indexed
	// by.
	Term TermName `json:"term,omitempty"`

	// Since is when this log's trim stopped advancing, wherever Term is
	// set. An instant rather than an age, so a reader renders the age
	// against its own clock instead of trusting one frozen at the
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
			"deferral grace, is its copy wrong rather than behind: it then stops " +
			"serving its copy of the estate, and its seats stay and read the " +
			"estate from the other data nodes until the copy recovers.",
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
			return fmt.Sprintf("the read barrier's p95 is %s, a quarter of the "+
					"read budget", spoken(r.BarrierP95)),
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
				return fmt.Sprintf("no verified backup of this log has been "+
					"recorded, and the policy asks for one every %s",
					spoken(r.BackupMaxAge)), true
			}
			return fmt.Sprintf("the newest verified backup of this log is %s "+
					"old, and the policy asks for %s", spoken(*r.BackupAge),
					spoken(r.BackupMaxAge)),
				*r.BackupAge > r.BackupMaxAge
		},
		remedy: "Run `crewlet backup` against any node, whatever its roles, " +
			"and check whatever was meant to run it; under `backup_floor: " +
			"operator`, acknowledge each named log's off-host copy with " +
			"`crewlet retention ack -stream <its stream> -position <n>`, n " +
			"being the sequence the copy reaches. The trim does not advance " +
			"a log past a backup older than the policy, and does not " +
			"advance it at all until there is one.",
		perLog: true,
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
		// because what the grace does is the log's — see
		// [Reading.DeferredJudgesCopy].
		kind:   KindDeferredOld,
		perLog: true,
		fires: func(r Reading) (string, bool) {
			what := "the oldest record on this log that this node cannot apply"
			if r.DeferredRecord != "" {
				what += " (" + r.DeferredRecord + ")"
			}
			consequence := fmt.Sprintf("past the %s deferral grace, at which "+
				"this node stops serving its copy of the estate", spoken(DeferralGrace))
			if !r.DeferredJudgesCopy {
				consequence = fmt.Sprintf("past the %s deferral grace; this log "+
					"does not judge this node's copy of the estate, so nothing is "+
					"taken out of service", spoken(DeferralGrace))
			}
			return fmt.Sprintf("%s has been held for %s, %s", what,
					spoken(r.DeferredAge), consequence),
				r.DeferredAge > DeferralGrace
		},
		remedy: "This node is running a build that cannot decode records its peers " +
			"are writing. Upgrade it. Where the record's log judges this node's copy " +
			"of the estate (the detail says), the node has already stopped serving " +
			"its copy, and its seats read the estate from the other data nodes.",
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
			"coordination does, and every log on this node says so at once; a floor " +
			"published at a later generation than this node's means that log was " +
			"re-anchored and this node was not (see re-anchoring in " +
			"docs/guides/retention.md). Every read of the log on this node is " +
			"refused until its floor can be read.",
	},
	{
		kind: KindPrefetchSlow,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("turn-start context assembly is %s at p95, against "+
					"a %s budget", spoken(r.PrefetchP95), spoken(PrefetchScanBudget)),
				r.PrefetchP95 > PrefetchScanBudget
		},
		remedy: "Every turn on this node pays this before its first token. Check " +
			"the store's own latency and the knowledge backend's.",
	},
	{
		kind: KindSearchSlow,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("interactive search is %s at p95, against a %s target",
					spoken(r.SearchP95), spoken(InteractiveSearchTarget)),
				r.SearchP95 > InteractiveSearchTarget
		},
		remedy: "The corpus has outgrown what one node's share can scan in " +
			"the budget. Adding a data node divides the buckets again, with " +
			"no configuration and no rebuild; a node without `data` takes " +
			"no range. See docs/guides/search.md.",
	},
	{
		kind: KindSearchDegraded,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%.0f%% of searches were answered without their "+
					"semantic half", r.SearchDegradedFraction*100),
				r.SearchDegradedFraction > 0
		},
		remedy: "The embeddings provider or the vector domain is failing. Search " +
			"still answers; it answers less well, and silently.",
	},
	{
		kind: KindSearchScoped,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%.0f%% of searches were answered over part of the "+
				"corpus", r.SearchScopedFraction*100), r.SearchScopedFraction > 0
		},
		remedy: "A node did not cover its bucket range, so part of the corpus " +
			"went unscanned: it was unreachable, or its own lexical index has " +
			"not finished its first lap, which is what a node that joined a " +
			"few minutes ago looks like and clears itself. The answers were " +
			"complete for what was searched and silent about what was not; the " +
			"log line names who was absent.",
	},
	{
		// THE BUDGET IS BORROWED (ADR-0015): what makes a history answer
		// partial is a node that did not answer inside the fleet read
		// budget, so that budget is the threshold — named in the detail
		// from the value the reading carries, which is the constant the
		// scatter waits on ([Reading.HistoryReadBudget]), never restated.
		// PROCESS-WIDE rather than per log: the reads are of each data
		// node's own event log, which is no state log at all.
		kind: KindHistoryPartial,
		fires: func(r Reading) (string, bool) {
			budget := "the fleet read budget"
			if r.HistoryReadBudget > 0 {
				budget = "the " + spoken(r.HistoryReadBudget) + " fleet read budget"
			}
			return fmt.Sprintf("%.0f%% of fleet history reads were answered without "+
					"every data node, because one did not answer inside %s",
					r.HistoryPartialFraction*100, budget),
				r.HistoryPartialFraction > 0
		},
		remedy: "Turn-level history lives only on the data node that published " +
			"it or keeps it in custody, so a partial answer is missing that node's " +
			"rows, and every answer names the node in its `coverage`. A node that " +
			"has left the fleet is gone with its detail, and the aggregates survive " +
			"it in the usage domain; a live node that keeps missing the budget is " +
			"slow on its own store or its route, and its own `pool_starved` and " +
			"`apply_lag` alarms say which.",
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
			return fmt.Sprintf("%s free on the volume holding %s against %s of store "+
					"there, and the next restore, vacuum or snapshot needs room for a "+
					"second copy", bytesHuman(r.FreeBytes), r.StoreVolume,
					bytesHuman(r.StoreBytes)),
				r.StoreBytes > 0 && float64(r.FreeBytes) < VolumeHeadroomFactor*float64(r.StoreBytes)
		},
		remedy: "Add space to the volume the alarm names. Where it could not be " +
			"measured, fix what stops it being read. A backup, a vacuum and a peer's " +
			"join all need the room, and each fails partway through without it.",
	},
	{
		kind: KindWALLarge,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the write-ahead log is %s", bytesHuman(r.WALBytes)),
				r.WALBytes > WALAlarmBytes
		},
		remedy: "A checkpoint is not happening, which usually means a reader is " +
			"holding a snapshot open. It has no other symptom until the volume fills.",
	},
	{
		kind: KindPoolStarved,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("a database connection takes %s at p95 to acquire",
				spoken(r.PoolWaitP95)), r.PoolWaitP95 > PoolWaitAlarm
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
			// THE LOG'S OWN CEILING FIELD, where the caller named it: the
			// remedy is one sentence for every log, and the field is
			// what differs between them.
			ceiling := ""
			if r.LogCeilingField != "" {
				ceiling = fmt.Sprintf("; its ceiling is `%s`", r.LogCeilingField)
			}
			return fmt.Sprintf("%d linearizable reads a day against the %d this "+
					"log was sized for%s", r.LinearizableReads,
					r.LinearizableReadsExpected, ceiling),
				r.LinearizableReadsExpected > 0 &&
					float64(r.LinearizableReads) > CensusDriftFactor*float64(r.LinearizableReadsExpected)
		},
		remedy: "Each read past the census is a barrier record on the named log, " +
			"so re-derive that log's own ceiling — the field its line names — " +
			"adding what its barriers take at the real rate to whatever else the " +
			"field is sized from, and the trim's cadence " +
			"(`stream.tracker_retention`). See " +
			"docs/getting-started/configuration.md.",
	},
	{
		kind: KindObjectsMissing,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d file(s) are missing from the object store or "+
				"damaged in it", r.ObjectsMissing), r.ObjectsMissing > 0
		},
		remedy: "The backend lost or changed bytes it had acknowledged: check its " +
			"own health (the NATS bucket OBJ_crewlet_files on the data nodes, or " +
			"the S3 bucket) and restore those files from a backup, or upload them " +
			"again (see docs/guides/backup.md). `crewlet objects status` names them.",
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
// in the table's own order: the seven a reader of the reference has to know
// list the logs they stand on ([Alarm.Domains]) and write a pair of log lines
// for each of those logs ([Tracker]), while counting once on the health
// envelope and the gauge.
func PerLogKinds() []Kind {
	var out []Kind
	for _, rule := range table {
		if rule.perLog {
			out = append(out, rule.kind)
		}
	}
	return out
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

// Spoken is a duration as a person reads it ("45s", "2m 5s", "22h 33m"),
// for a sentence outside this package that states one: a wait, a bound, an
// age. The one spelling the engine uses, and the dashboard's own
// (`fmtDuration`, lib/format.ts), so a figure and the sentence beside it
// agree. Never a bare count of seconds.
func Spoken(d time.Duration) string { return spoken(d) }

// SpokenWait is a WAIT a person is told to sit out ("retry in about 22h
// 33m"): the largest two units, ROUNDED UP at every magnitude, so a wait never
// reads shorter than it is and a reader who comes back when told is not
// refused again. [Spoken] measures a duration and rounds to the nearest unit,
// which is right for a figure and wrong for a wait: "4m" said of four and a
// half sends the reader back early. The dashboard says a wait by the same rule
// (`fmtWait`, dashboard/src/lib/format.ts), so the engine's sentence and the
// screen beside it agree. Never under a second: a wait that is over is said
// as the shortest one.
func SpokenWait(d time.Duration) string {
	secs := int64((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	mins := (secs + 59) / 60
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	if hours := mins / 60; hours < 24 {
		if mins%60 == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh %dm", hours, mins%60)
	}
	hours := (mins + 59) / 60
	if hours%24 == 0 {
		return fmt.Sprintf("%dd", hours/24)
	}
	return fmt.Sprintf("%dd %dh", hours/24, hours%24)
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
