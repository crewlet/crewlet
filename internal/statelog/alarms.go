// Package statelog is the durable-state framework: one ordered stream per
// domain is the write-ahead log, N identical SQL copies are the durable
// state, and the checkpoint commits in the same transaction as the rows.
//
// This file is the ALARM TABLE, and it is here rather than beside any one
// subsystem because an alarm is the framework's answer to "is this node
// doing its job", and every subsystem above it asks the same question.
package statelog

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// The thresholds an alarm fires at.
//
// # Why they are named here rather than at each condition
//
// Every one of them is a number some OTHER decision already made — a stall
// grace is what sheds a node, a deferral grace is what moves its seats, a read
// budget is what a caller was promised. An alarm that invented its own
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
	// apply before its seats move. The alarm and the seat move share the
	// number deliberately: an operator who sees this alarm has thirty
	// minutes, and one who sees a different number has no idea how long
	// they have.
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
)

// Kind names one alarm.
//
// A named string rather than an integer, because it travels: it is a metric
// attribute, a log field and a row on a dashboard, and an operator searching
// for one has to be able to type it.
type Kind string

const (
	KindApplyLag         Kind = "apply_lag"
	KindReadRefusals     Kind = "read_refusals"
	KindBarrierSlow      Kind = "barrier_slow"
	KindLogHeadroom      Kind = "log_headroom"
	KindBackupAge        Kind = "backup_age"
	KindTrimBlocked      Kind = "trim_blocked"
	KindDeferredOld      Kind = "deferred_old"
	KindFloorUnknown     Kind = "floor_unknown"
	KindPrefetchSlow     Kind = "prefetch_slow"
	KindSearchSlow       Kind = "search_slow"
	KindSearchDegraded   Kind = "search_degraded"
	KindSearchScoped     Kind = "search_scoped"
	KindRecallBelowFloor Kind = "recall_below_floor"
	KindRecordsGated     Kind = "records_gated"
	KindFeedDeadLetters  Kind = "feed_dead_letters"
	KindMaintenanceOpen  Kind = "maintenance_open"
	KindVolumeLow        Kind = "volume_low"
	KindWALLarge         Kind = "wal_large"
	KindPoolStarved      Kind = "pool_starved"
	KindCensusDrift      Kind = "census_drift"
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

	// HeadroomFraction is how much of the log's byte ceiling is unused, as
	// a fraction.
	//
	// A POINTER, because zero is a real value here and it is the worst
	// one: a full log and a node that has not measured its log are
	// opposite facts, and a plain float64 gives them the same
	// representation. Nil is "not measured" — which is every node until
	// the trim runs once, and every node whose domain declares no ceiling.
	HeadroomFraction *float64

	// BackupAge is how old the newest verified backup is, and BackupMaxAge
	// what the operator asked for. Both zero on a node with no backup
	// policy, which is not an alarm — it is a deployment that has said it
	// does not want one.
	BackupAge, BackupMaxAge time.Duration

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

	// SemanticCoverage is the fraction of the corpus with current vectors.
	// A POINTER for the reason HeadroomFraction is one: a company with no
	// embeddings configured measures nothing, and zero coverage is the
	// alarm rather than the absence.
	SemanticCoverage *float64

	// RecordsGated and FeedDeadLetters are counts over the last day. Any
	// value above zero is an alarm: a gated record is recoverable by
	// nothing, and a dead-lettered wake reached nobody.
	RecordsGated, FeedDeadLetters int

	// MaintenanceOpenFor is how long a maintenance operation has been in
	// flight, with the phase it is stuck in.
	MaintenanceOpenFor time.Duration
	MaintenancePhase   string

	// FreeBytes and StoreBytes are the volume's free space and what this
	// node's databases occupy. WALBytes is the write-ahead log's size.
	FreeBytes, StoreBytes, WALBytes int64

	// PoolWaitP95 is how long a caller queues for a database connection.
	PoolWaitP95 time.Duration

	// LinearizableReads and LinearizableReadsExpected are the observed and
	// designed-for daily read rates.
	LinearizableReads, LinearizableReadsExpected int
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
			return fmt.Sprintf("this node is %s behind the log", round(r.ApplyLag)),
				r.ApplyLag > StallGrace
		},
		remedy: "Check this node's applier: `crewlet retention status` names the " +
			"domain and its position. A node that stays behind past the deferral " +
			"grace loses its seats to a peer.",
	},
	{
		kind: KindReadRefusals,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("reads have been refused for %s for a reason other "+
					"than ordinary lag", round(r.RefusalsSince)),
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
					"read budget", round(r.BarrierP95)),
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
			return fmt.Sprintf("%.0f%% of the log's byte ceiling is left",
					*r.HeadroomFraction*100),
				*r.HeadroomFraction < HeadroomAlarmFraction
		},
		remedy: "Raise the log's ceiling with `crewlet retention set-capacity` " +
			"during a maintenance window, or find out why the trim is not " +
			"advancing. A full log refuses writes; it does not drop records.",
	},
	{
		// ONE THRESHOLD. The form this replaces set a blocked flag once
		// the newest backup passed backup_max_age and then alarmed once
		// THAT had been true for backup_max_age again — so a 24-hour
		// policy alarmed at 48 hours, eight missed six-hourly runs after
		// the first one that mattered.
		kind: KindBackupAge,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the newest verified backup is %s old, and the "+
					"policy asks for %s", round(r.BackupAge), round(r.BackupMaxAge)),
				r.BackupMaxAge > 0 && r.BackupAge > r.BackupMaxAge
		},
		remedy: "Run `crewlet backup` against a node holding seats, and check " +
			"whatever was meant to run it. The trim will not advance past a " +
			"backup this old.",
	},
	{
		kind: KindTrimBlocked,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the trim has not advanced for %s: %s",
				round(r.TrimBlockedFor), r.TrimBlockedBy), r.TrimBlockedBy != ""
		},
		remedy: "The blocking term names what to fix. Until it is fixed the log " +
			"grows toward its ceiling.",
	},
	{
		kind: KindDeferredOld,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the oldest record this node cannot apply is %s old, "+
					"and its seats move at %s", round(r.DeferredAge), round(DeferralGrace)),
				r.DeferredAge > DeferralGrace
		},
		remedy: "This node is running a build that cannot decode records its peers " +
			"are writing. Upgrade it; its seats have already moved.",
	},
	{
		kind: KindFloorUnknown,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the trim floor has been unreadable for %s",
				round(r.FloorUnknownFor)), r.FloorUnknownFor > FloorCacheStale
		},
		remedy: "Coordination cannot be reached from this node. Every read is " +
			"refused until it can be.",
	},
	{
		kind: KindPrefetchSlow,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("turn-start context assembly is %s at p95, against "+
					"a %s budget", round(r.PrefetchP95), round(PrefetchScanBudget)),
				r.PrefetchP95 > PrefetchScanBudget
		},
		remedy: "Every turn on this node pays this before its first token. Check " +
			"the store's own latency and the knowledge backend's.",
	},
	{
		kind: KindSearchSlow,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("interactive search is %s at p95, against a %s target",
					round(r.SearchP95), round(InteractiveSearchTarget)),
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
		remedy: "A node did not answer its bucket range, so part of the corpus " +
			"went unscanned. The answers were complete for what was searched " +
			"and silent about what was not; the log line names who was absent.",
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
		kind: KindRecordsGated,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d record(s) were dropped by an apply gate in the "+
				"last day", r.RecordsGated), r.RecordsGated > 0
		},
		remedy: "A gated record is recoverable by nothing. The log line names the " +
			"gate, the operator and the position; this is worth reading today.",
	},
	{
		kind: KindFeedDeadLetters,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d wake(s) reached the dead-letter path in the last "+
				"day", r.FeedDeadLetters), r.FeedDeadLetters > 0
		},
		remedy: "A record no node could translate. Somebody was not told something " +
			"they were meant to be told.",
	},
	{
		kind: KindMaintenanceOpen,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("a maintenance operation has been open for %s in "+
					"phase %q", round(r.MaintenanceOpenFor), r.MaintenancePhase),
				r.MaintenanceOpenFor > MaintenanceAlarmAfter
		},
		remedy: "Maintenance stops every publisher on every node. Finish it or " +
			"abandon it; nothing is being written while it is open.",
	},
	{
		kind: KindVolumeLow,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%s free against %s of store, and the next restore, "+
					"vacuum or snapshot needs room for a second copy",
					bytesHuman(r.FreeBytes), bytesHuman(r.StoreBytes)),
				r.StoreBytes > 0 && float64(r.FreeBytes) < VolumeHeadroomFactor*float64(r.StoreBytes)
		},
		remedy: "Add space. A backup, a vacuum and a peer's join all need it, and " +
			"each fails partway through without it.",
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
				round(r.PoolWaitP95)), r.PoolWaitP95 > PoolWaitAlarm
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
			return fmt.Sprintf("%d linearizable reads a day against the %d this "+
					"deployment was sized for", r.LinearizableReads, r.LinearizableReadsExpected),
				r.LinearizableReadsExpected > 0 &&
					float64(r.LinearizableReads) > CensusDriftFactor*float64(r.LinearizableReadsExpected)
		},
		remedy: "Re-derive the log's ceiling and the trim's cadence from the real " +
			"rate. See `stream.tracker_retention` in " +
			"docs/getting-started/configuration.md.",
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

// Kinds is every alarm this engine can raise, sorted. For the reference doc
// and for a surface that renders a row per kind.
func Kinds() []Kind {
	out := make([]Kind, 0, len(table))
	for _, rule := range table {
		out = append(out, rule.kind)
	}
	slices.SortFunc(out, func(a, b Kind) int { return cmp.Compare(a, b) })
	return out
}

// round is a duration an operator reads rather than one a computer wrote.
func round(d time.Duration) time.Duration {
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute)
	case d >= time.Minute:
		return d.Round(time.Second)
	case d >= time.Second:
		return d.Round(10 * time.Millisecond)
	default:
		return d.Round(time.Millisecond)
	}
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
