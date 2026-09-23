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

	// AlarmInterval is how often every node evaluates the table: the
	// fifteen-second heartbeat, [coord.ReconcileInterval], on which every
	// other fleet fact is refreshed. It is what makes the thresholds here
	// mean what they say — [StallGrace] is four of these, so a condition
	// that has held past it is named within one more interval — and an
	// evaluation paced by anything slower silently raises every threshold
	// to its own period: the table ran on the trim's quarter-hour once,
	// which made the sixty-second alarms fire up to fifteen minutes late.
	AlarmInterval = coord.ReconcileInterval

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

	// LinearizableReadsPerDay is the read rate this engine's log sizing was
	// derived from.
	//
	// TWELVE AND A HALF THOUSAND, the census input every capacity decision
	// under the log rests on: a `linearizable` read appends a barrier
	// record, so this number is a term in the log's byte ceiling, in how
	// often the trim has to run to stay under it, and in how long a
	// rejoining node's replay takes. It is a DECLARED expectation rather
	// than a measurement, which is exactly why it needs an alarm — nothing
	// else notices when a company outgrows the assumptions its deployment
	// was sized against, and the symptom arrives as a full log rather than
	// as a slow one.
	LinearizableReadsPerDay = 12_500
)

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
	KindFeedUnreadable   Kind = "feed_unreadable"
	KindMaintenanceOpen  Kind = "maintenance_open"
	KindVolumeLow        Kind = "volume_low"
	KindWALLarge         Kind = "wal_large"
	KindPoolStarved      Kind = "pool_starved"
	KindCensusDrift      Kind = "census_drift"
	KindBindingDangling  Kind = "iam_binding_dangling"
	KindLogCeilingShort  Kind = "log_ceiling_short"
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
	// TrimBlockedBy names the term holding it. Per DOMAIN, like
	// HeadroomFraction.
	TrimBlockedFor time.Duration
	TrimBlockedBy  string

	// TrimPastWindow is how many records the log holds that are older than
	// ReplayWindow — the records the age term alone would release, and
	// which a blocked trim is therefore keeping. Per DOMAIN.
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
	// sighting of it, the instant [Health.Healthy] sheds this node's seats
	// from, through [DeferredSince.Age] — and DeferredRecord which record
	// that is, in the words an operator upgrades by: its position and the
	// record version it was written at against the one this build reads.
	// Per DOMAIN, like HeadroomFraction, and filled by the report from
	// [DomainInputs] rather than by a node-wide caller.
	//
	// DeferredSheds is whether that log is a [Domain.ReadinessInput] — the
	// tracker's, the knowledge base's and the chart's are; the identity
	// estate's and the vectors' are not — which is whether the grace is ALSO
	// when this node's seats move. The alarm fires at the grace either way,
	// because an upgrade is the remedy either way; what it may not do is tell
	// an operator a node's seats moved over a log that moves none — or that
	// they did not, over the one log among several that moved them, which is
	// why the three are a LOG's rather than the node's oldest.
	DeferredAge    time.Duration
	DeferredRecord string
	DeferredSheds  bool

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

	// SemanticCoverage is the fraction of the corpus with current vectors.
	// A POINTER for the reason HeadroomFraction is one: a company with no
	// embeddings configured measures nothing, and zero coverage is the
	// alarm rather than the absence.
	SemanticCoverage *float64

	// RecordsGated and FeedUnreadable are counts over the last day. Any
	// value above zero is an alarm: a gated record is recoverable by
	// nothing, and a change record no build could translate is a wake
	// circling for ever behind everything queued after it.
	RecordsGated, FeedUnreadable int

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

	// DanglingBindings is how many people this node's directory holds bound
	// to a seat its org chart does not hold as a human one, and
	// DanglingBindingFor how long the OLDEST of them has persisted, with
	// DanglingBindingSeat the seat it names — the half of the detail an
	// operator acts on.
	//
	// A DURATION THIS NODE OBSERVED, NOT ONE A ROW STATES. Nothing records
	// when a binding began to dangle: the residue is the product of two
	// logs, and the moment it arose is the moment THIS node applied the
	// later of a bind and a seat's removal, which no record carries. So
	// the age is how long this node's own evaluations have kept finding
	// the residue, from the first that found it to the latest — never a
	// persistence nobody saw.
	//
	// It is what the stall grace is compared against, and that is the
	// point of carrying a duration rather than a count: a bind and a
	// removal racing, or a hire this node's chart applier has not reached
	// yet, is a residue for seconds, and alarming on its first sighting
	// would page somebody for a state that was already clearing.
	DanglingBindings    int
	DanglingBindingFor  time.Duration
	DanglingBindingSeat string

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

	// Domain is the log a per-log condition is about, and empty for one
	// about the node. It is the other half of the alarm's IDENTITY: one
	// evaluation can carry a kind once per log ([Report]), and the kind
	// alone folded those into one — see the tracker's [instance]. The
	// detail still leads with the same name, for a reader who has only the
	// line.
	Domain string `json:"domain,omitempty"`

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
	// perLog marks a condition that is a property of ONE LOG rather than
	// of the node, which the report evaluates once per log from that log's
	// own inputs and stamps with its name ([Alarm.Domain]). DECLARED on the
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
		kind:   KindLogHeadroom,
		perLog: true,
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
				round(holds), round(r.ReplayWindow)), true
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
		kind: KindBackupAge,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the newest verified backup is %s old, and the "+
					"policy asks for %s", round(r.BackupAge), round(r.BackupMaxAge)),
				r.BackupMaxAge > 0 && r.BackupAge > r.BackupMaxAge
		},
		remedy: "Run `crewlet backup` against any node, whatever its roles, " +
			"and check whatever was meant to run it. The trim will not " +
			"advance past a backup this old.",
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
		// its own — from the first reading where none has been taken, at the
		// policy's age where one has gone stale). The tick is the block's
		// resolution
		// — `blocked_since` moves only when the trim does — so a block that
		// cleared at the window is seen to clear up to one tick later.
		//
		// AND ONLY WHILE THE LOG HOLDS A RECORD PAST THE WINDOW, which is
		// the half that is measured rather than borrowed: a block that keeps
		// nothing the age term would release has cost nothing, however long
		// it has lasted — a knowledge base nobody writes to is blocked for
		// the life of the deployment.
		kind:   KindTrimBlocked,
		perLog: true,
		fires: func(r Reading) (string, bool) {
			if r.TrimBlockedBy == "" || r.ReplayWindow <= 0 || r.TrimPastWindow == 0 {
				return "", false
			}
			return fmt.Sprintf("the trim has not advanced for %s: %s — and the log "+
					"is keeping %d record(s) older than its %s replay window",
					round(r.TrimBlockedFor), r.TrimBlockedBy, r.TrimPastWindow,
					round(r.ReplayWindow)),
				r.TrimBlockedFor > r.ReplayWindow+TrimInterval
		},
		remedy: "The blocking term names what to fix; `crewlet retention status` " +
			"says what it has and what it wants. Until it is fixed the log grows " +
			"toward its ceiling by a day of records every day.",
	},
	{
		// PER LOG, like the headroom, the ceiling and the blocked trim: the
		// report evaluates it once for each log this node holds a record on,
		// because what the grace does is the log's — see
		// [Reading.DeferredSheds].
		kind:   KindDeferredOld,
		perLog: true,
		fires: func(r Reading) (string, bool) {
			what := "the oldest record on this log that this node cannot apply"
			if r.DeferredRecord != "" {
				what += " (" + r.DeferredRecord + ")"
			}
			consequence := fmt.Sprintf("past the %s deferral grace, at which "+
				"this node's seats move to a peer", round(DeferralGrace))
			if !r.DeferredSheds {
				consequence = fmt.Sprintf("past the %s deferral grace; this log "+
					"does not gate seat admission, so it moves no seats",
					round(DeferralGrace))
			}
			return fmt.Sprintf("%s has been held for %s, %s", what,
					round(r.DeferredAge), consequence),
				r.DeferredAge > DeferralGrace
		},
		remedy: "This node is running a build that cannot decode records its peers " +
			"are writing. Upgrade it. Where the record's log gates seat admission — " +
			"the detail says — its seats have already moved to a peer.",
	},
	{
		// PER LOG, for the same reason: a read of one log refuses on that
		// log's floor, and a floor published ahead of this node is one
		// log's re-anchor.
		kind:   KindFloorUnknown,
		perLog: true,
		fires: func(r Reading) (string, bool) {
			detail := fmt.Sprintf("this log's trim floor has been unreadable here "+
				"for %s", round(r.FloorUnknownFor))
			if r.FloorUnknownCause != "" {
				detail += ": " + r.FloorUnknownCause
			}
			return detail, r.FloorUnknownFor > FloorCacheStale
		},
		remedy: "Read the cause. An unreachable coordination store clears when " +
			"coordination does, and every log on this node says so at once; a floor " +
			"published at a later generation than this node's means that log was " +
			"re-anchored and this node was not — see re-anchoring in " +
			"docs/guides/retention.md. Every read of the log on this node is " +
			"refused until its floor can be read.",
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
		remedy: "A node did not cover its bucket range, so part of the corpus " +
			"went unscanned — it was unreachable, or its own lexical index has " +
			"not finished its first lap, which is what a node that joined a " +
			"few minutes ago looks like and clears itself. The answers were " +
			"complete for what was searched and silent about what was not; the " +
			"log line names who was absent.",
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
	{
		// A LEGAL RESIDUE, NAMED. The binding lives on the identity log
		// and the seat on the chart's, and nothing orders the two: a
		// bind and a seat's removal each pass their own decide and both
		// land, and a node can apply a bind before the hire it names.
		// Neither is corruption, and each is repaired by one record —
		// but a person in either state is refused or held off on every
		// request, so it is worth a page once it has outlived the
		// race that makes it.
		//
		// AT THE STALL GRACE (ADR-0015), because that is the number that
		// already separates a node catching up from one that has
		// stopped: a residue that has persisted past it is not a hire
		// in flight.
		kind: KindBindingDangling,
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d person(s) are bound to a seat this node's "+
					"org chart does not hold as a human seat; the oldest, on "+
					"%q, has been for %s", r.DanglingBindings,
					r.DanglingBindingSeat, round(r.DanglingBindingFor)),
				r.DanglingBindings > 0 && r.DanglingBindingFor > StallGrace
		},
		remedy: "Run `crewlet iam check`, which names who and why. A seat that " +
			"was removed or is not a human seat needs its person unbound " +
			"(`crewlet iam unbind`) or bound to another (`crewlet iam bind`) — " +
			"one record either way. A seat this node's chart has not reached " +
			"yet is its chart applier: read `apply_lag` first.",
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

// PerLogKinds is every alarm raised once per log rather than once per node, in
// the table's own order — the five a reader of the reference has to know carry
// a log's name ([Alarm.Domain]) and stand once for each log they hold on.
func PerLogKinds() []Kind {
	var out []Kind
	for _, rule := range table {
		if rule.perLog {
			out = append(out, rule.kind)
		}
	}
	return out
}

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
