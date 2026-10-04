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
	"github.com/crewlet/crewlet/internal/eventfan"
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
	// StallGrace is how far behind a node may be before its lag is worth
	// a person's attention. Sixty seconds, which is four heartbeats: long
	// enough that a rolling restart or one oversized record passes
	// through it, short enough that a node that has actually stopped is
	// named within a minute.
	StallGrace = 60 * time.Second

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
	// OF THE CEILING ORDINARY WRITES ARE HELD TO ([Headroom]), which on a
	// log that keeps a gate reserve is [GateReserve] below the broker's:
	// that is where writes start being refused, so it is what "full"
	// means to everybody but an eviction.
	HeadroomAlarmFraction = 0.10

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
)

// Reading is everything an alarm evaluation looks at, gathered once per tick.
//
// ONE STRUCT rather than a callback per condition, because the whole table is
// evaluated together on ticks that already run — the trim's fifteen minutes
// and the heartbeat's fifteen seconds — and a condition that fetched its own
// input would make the cost of the table a function of how many alarms are
// defined rather than of how many facts it reads.
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
	// writes are held to is unused, as a fraction ([Headroom]).
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
	// TrimBlockedBy names the term holding it.
	TrimBlockedFor time.Duration
	TrimBlockedBy  string

	// DeferredAge is how old the oldest record this node could not apply
	// is.
	DeferredAge time.Duration

	// FloorUnknownFor is how long the trim floor has been unreadable.
	FloorUnknownFor time.Duration

	// PrefetchP95 and SearchP95 are the two latencies a turn waits on.
	PrefetchP95, SearchP95 time.Duration

	// SearchDegradedFraction and SearchScopedFraction are the fractions of
	// searches answered without the semantic half and without the whole
	// corpus.
	SearchDegradedFraction, SearchScopedFraction float64

	// HistoryPartialFraction is the fraction of this node's fleet history
	// reads answered without every live node — one did not answer inside
	// [eventfan.FleetReadBudget].
	HistoryPartialFraction float64

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
	// ([Census.Expected]) — and CensusLog names that log: of every log this
	// node applies, the one furthest past its census, since the reading
	// describes one node and a log over its census is over it however quiet
	// the others are.
	LinearizableReads, LinearizableReadsExpected int
	CensusLog                                    string

	// ObjectsMissing is how many chunks the estate names that the object
	// store does not hold, as the collector's last COMPLETED audit found —
	// on the node holding the collector's duty, and zero on every other,
	// since the store is one the whole fleet shares and one node's count of
	// it is the fleet's. Parts of files nobody can read in full. Any value
	// above zero is an alarm, for the gated record's reason: there is no
	// threshold below which a file that cannot be opened is acceptable.
	ObjectsMissing int
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
	// and "this node is 4m12s behind" is a fact.
	Detail string `json:"detail"`

	// Remedy is what to do about it. Carried on the alarm rather than
	// looked up beside it, because every surface renders the same alarm
	// and a remedy that lived on one of them would be missing from the
	// other two.
	Remedy string `json:"remedy"`
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
		kind: KindLogHeadroom,
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
		kind: KindTrimBlocked,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the trim has not advanced for %s: %s",
				spoken(r.TrimBlockedFor), r.TrimBlockedBy), r.TrimBlockedBy != ""
		},
		remedy: "The blocking term names what to fix. Until it is fixed the log " +
			"grows toward its ceiling.",
	},
	{
		kind: KindDeferredOld,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the oldest record this node cannot apply is %s old, "+
					"and it stops serving its copy of the estate at %s", spoken(r.DeferredAge),
					spoken(DeferralGrace)),
				r.DeferredAge > DeferralGrace
		},
		remedy: "This node is running a build that cannot decode records its peers " +
			"are writing. Upgrade it; it has already stopped serving its copy of " +
			"the estate, and its seats read the estate from the other data nodes.",
	},
	{
		kind: KindFloorUnknown,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the trim floor has been unreadable for %s",
				spoken(r.FloorUnknownFor)), r.FloorUnknownFor > FloorCacheStale
		},
		remedy: "Coordination cannot be reached from this node. Every read is " +
			"refused until it can be.",
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
			"went unscanned — it was unreachable, or its own lexical index has " +
			"not finished its first lap, which is what a node that joined a " +
			"few minutes ago looks like and clears itself. The answers were " +
			"complete for what was searched and silent about what was not; the " +
			"log line names who was absent.",
	},
	{
		// THE BUDGET IS BORROWED (ADR-0015): what makes a history answer
		// partial is a node that did not answer inside the fleet read
		// budget, so that budget is the threshold — named in the detail
		// from the constant the scatter waits on, never restated.
		kind: KindHistoryPartial,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%.0f%% of fleet history reads were answered without "+
					"every node, because one did not answer inside the %s fleet read budget",
					r.HistoryPartialFraction*100, spoken(eventfan.FleetReadBudget)),
				r.HistoryPartialFraction > 0
		},
		remedy: "Turn-level history lives only on the node that published it, so " +
			"a partial answer is missing that node's rows — every answer names " +
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
			"will say the same. The remedy is the evaluation's — raise " +
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
			"the record's `position` and `kind`, and the `writer` — the node " +
			"that wrote it; this is worth reading today.",
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
			"rather than being dropped, so the wakes behind it are waiting too — " +
			"upgrade the node past it, or the feed stops moving.",
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
		remedy: "Add space to the volume the alarm names — or, where it could not be " +
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
		kind: KindCensusDrift,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d linearizable reads a day on %s against the %d "+
					"this deployment was sized for", r.LinearizableReads, r.CensusLog,
					r.LinearizableReadsExpected),
				r.LinearizableReadsExpected > 0 &&
					float64(r.LinearizableReads) > CensusDriftFactor*float64(r.LinearizableReadsExpected)
		},
		remedy: "Re-derive the log's ceiling and the trim's cadence from the real " +
			"rate. See `stream.tracker_retention` in " +
			"docs/getting-started/configuration.md.",
	},
	{
		kind: KindObjectsMissing,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d chunk(s) the company's files are made of are not "+
				"in the object store", r.ObjectsMissing), r.ObjectsMissing > 0
		},
		remedy: "The backend lost bytes it had acknowledged: check its own health " +
			"(the NATS bucket OBJ_crewlet_files on the data nodes, or the S3 " +
			"bucket) and restore the missing chunks from a backup — see " +
			"docs/guides/backup.md. `crewlet objects status` names them.",
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
