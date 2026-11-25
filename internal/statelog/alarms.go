package statelog

// The alarm table lives here, not beside a subsystem: an alarm answers "is
// this node doing its job" for every subsystem above it. The package doc is
// doc.go's alone (two package comments are ADR-0008's failure).

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/words"
)

// The thresholds an alarm fires at.
//
// An alarm fires at the threshold some OTHER decision already made (a stall
// grace takes a copy out of service, a read budget is what a caller was
// promised), taken from the owning package where the constant is another's;
// its own number would be a second opinion that drifts.
const (
	// PositionHeartbeat is how often a node running a state log republishes
	// its row in the fleet's position register, which the trim, the snapshot
	// gate and a joiner's donor count read. Ten seconds, from what the readers
	// need: the snapshot gate's question (is there anybody else) changes when a
	// node joins or leaves, which an operator expects in seconds. Declared here,
	// for [TrimInterval]'s reason.
	PositionHeartbeat = 10 * time.Second

	// StallGrace is how far behind a node may be before its lag is worth a
	// person's attention: four coordination heartbeats
	// ([coord.ReconcileInterval]), long enough for a rolling restart or one
	// oversized record, short enough to name a stopped node within a minute.
	StallGrace = 60 * time.Second

	// FeedReopenCeiling is the longest a change feed waits between two attempts
	// to open its consumer (internal/changefeed), and half the stall grace
	// `feed_stopped` fires at: a feed whose broker answers again is reopened
	// inside the grace, so the alarm names a broker or consumer still refusing.
	// Shorter buys little, as each attempt is a consumer create with its own
	// deadline. Declared here because the alarm's remedy states it.
	FeedReopenCeiling = StallGrace / 2

	// AlarmInterval is how often a node running a state log evaluates the
	// table: the [PositionHeartbeat] itself, so a retune cannot leave the table
	// running at a cadence its inputs are not published at.
	//
	// Slower, `apply_lag` (fires at [StallGrace], remedy promises a stopped
	// node is named within a minute) would be late by up to that period; faster
	// buys nothing, as the fastest input is measured against positions the
	// heartbeat publishes.
	AlarmInterval = PositionHeartbeat

	// DeferralGrace is how long a node may hold records it could not apply
	// before its copy of that log goes out of service (its seats stay, its
	// operations go to the other data nodes, ADR-0079). The alarm and that step
	// share the number so an operator who sees the alarm knows how long they have.
	DeferralGrace = 30 * time.Minute

	// RelapseWindow is how long an alarm must stay clear before its next raise
	// starts a new run; one raised sooner is dated from its run's first raise,
	// so a flapping condition reaches a person as a steady one.
	RelapseWindow = DeferralGrace

	// FloorCacheStale is how old a cached trim floor may be before the node
	// stops answering from it: four heartbeats, as every cached coordination
	// fact.
	FloorCacheStale = 4 * coord.ReconcileInterval

	// ReadBudget is what a linearizable read was promised; the barrier alarm
	// fires at a quarter of it. A read spends it on its barrier's append and this
	// node committing every earlier record that writes a row, never a barrier's
	// own checkpoint ([Runner.Reached];
	// TestNoReadWaitsOnACommitThatWritesNoRow). Two seconds stays, and a refusal
	// is the answer, not a bound to grow: a device stall has no length to size
	// against, and in a fleet it is how long a walk waits on one data node before
	// the estate router asks the next.
	ReadBudget = 2 * time.Second

	// PrefetchScanBudget is what the knowledge scan a turn's prefetch runs was
	// promised: the turn waits for it before its first token, so it is half of
	// what a person searching on purpose waits ([InteractiveSearchTarget]).
	// Read off the node that coordinates the scan.
	PrefetchScanBudget = 500 * time.Millisecond

	// HeadroomAlarmFraction is how little of a log's byte ceiling may remain
	// before an operator is told: a tenth, as raising a ceiling needs a
	// maintenance window and a tenth of a 29 GiB log is days of writing. The
	// ceiling is the one ordinary writes are held to ([Reservation.Headroom]),
	// [Reservation.Bytes] below the broker's on a log with a gate reserve: where
	// writes start being refused.
	HeadroomAlarmFraction = 0.10

	// ObjectsHeadroomAlarmFraction is how little of the nats object bucket's
	// byte reservation may remain before an operator is told: twice a log's,
	// derived from [HeadroomAlarmFraction]: raising the reservation waits for a
	// data node restart and `crewlet objects migrate --to s3` copies every file
	// inside a maintenance window. A full bucket refuses only uploads.
	ObjectsHeadroomAlarmFraction = 2 * HeadroomAlarmFraction

	// WALAlarmBytes is a write-ahead log large enough to say a checkpoint is not
	// happening: a gibibyte, where the driver's 1 000-frame autocheckpoint keeps
	// it at megabytes. A copy holds it too, for its whole run (store.DB.Copies):
	// it stops every checkpoint (a gibibyte is about 2.5 MB/s through a
	// seven-minute copy of the reference company's ≈ 44 GB estate). The bound
	// stays, as a gibibyte takes the same volume whatever holds it;
	// [Reading.WALCopy] names the copy.
	WALAlarmBytes = 1 << 30

	// VolumeHeadroomFactor is how much free space a node's volume must have
	// relative to its store: a restore, a vacuum and a snapshot each need room
	// for a second copy, so 1.1 is where the next routine operation would fail.
	// The log a copy holds back is added ([Reading.CopyHeldBytes]), as a tenth
	// covers it only while the node writes under about 10 MB/s (a tenth of the
	// ~104 MB/s the copy reads); before any copy has run, the tenth stands.
	VolumeHeadroomFactor = 1.1

	// VolumeClearFactor is the multiple of the store, plus the held log, a
	// raised `volume_low` waits for before it clears: 15% above the raise, so a
	// volume hovering at the raise keeps one alarm up rather than flapping.
	VolumeClearFactor = 1.25

	// PoolWaitAlarm is how long a caller may queue for a database connection
	// before the pool is the problem: already half a dashboard query's budget.
	PoolWaitAlarm = 100 * time.Millisecond

	// MaintenanceAlarmAfter is how long a maintenance operation may stay open:
	// it stops every publisher on every node.
	MaintenanceAlarmAfter = time.Hour

	// InteractiveSearchTarget is what an interactive search was promised.
	InteractiveSearchTarget = time.Second

	// SemanticCoverageFloor is the fraction of a corpus that must have
	// current vectors for semantic recall to be what it claims.
	SemanticCoverageFloor = 0.95

	// CensusDriftFactor is how far an observed read rate may exceed the rate
	// the log's sizing was derived from. Twice: the byte ceiling, trim cadence
	// and join model are all functions of that number, so at 2× they describe
	// a different company.
	CensusDriftFactor = 2.0

	// LinearizableReadsPerSeatDay is the reference company's read rate per agent
	// seat (12 500 reads a day over 100 seats), what its seats' `linearizable`
	// logs are sized from and their read models declare ([ReadModel]); each read
	// is a barrier record, a term in the byte ceiling, trim cadence and replay
	// time. Per seat because seats' tools issue the reads: company-wide it fired
	// on a two-hundred-seat company sized right. Declared, so it needs an alarm:
	// nothing else notices outgrowing it. The seats' term of [Census].
	LinearizableReadsPerSeatDay = 125

	// CredentialNoticeDays and CredentialWarningDays are how far ahead a model
	// credential's expiry is said: thirty days as a notice, seven as a warning.
	// Fixed, with no setting: a one-year token needs a month's notice to be
	// rotated calmly and a week's to be rotated at all.
	CredentialNoticeDays  = 30
	CredentialWarningDays = 7

	// LimitWaitingGrace is how long a seat may wait parked on a model's limit
	// before it is a person's problem: fifteen minutes, equal to the provider
	// park's longest re-probe step (harness's limitBackoff), so a park past it
	// is one the engine will not end soon.
	LimitWaitingGrace = 15 * time.Minute

	// MCPOutageGrace is how long an instance of an MCP server may stay down
	// before the outage is a person's problem: the restart schedule's ceiling
	// (internal/mcp's RestartCeiling, five minutes) plus one heartbeat
	// ([coord.ReconcileInterval]); past it every attempt waits the whole
	// ceiling. Spelled here, not imported, so no package reading an alarm
	// depends on the MCP client; a test holds it to the schedule's constant.
	MCPOutageGrace = 5*time.Minute + coord.ReconcileInterval

	// MCPWaitingGrace is how long an agent's instance of an MCP server may be
	// held back for a credential a provisioning pass mints before the wait is
	// a person's problem: the longest an integration waits between two passes
	// that still owe work (internal/integration's DefaultSchedule.WaitingMax,
	// five minutes) plus one heartbeat. A pass that cannot mint until a person
	// acts is not a wait: the node calls that instance failed ([MCPOutageGrace]).
	// Held to the schedule by the same test.
	MCPWaitingGrace = 5*time.Minute + coord.ReconcileInterval

	// MCPSignInNotice is how long before an MCP server's OAuth grant runs out
	// `mcp_oauth_expiring` stands: a working week and a weekend to sign in again
	// before the agents lose the server, where a vendor that states a grant's
	// life at all states months.
	MCPSignInNotice = 7 * 24 * time.Hour

	// MCPApprovalGrace is how long a call held for a person's approval
	// (ADR-0077) may wait before an administrator is told: a third of the
	// default pause TTL of a parked run's box (internal/sandbox's
	// DefaultPauseTTL, 30 minutes), leaving the rest for an answer to carry the
	// run on in its own box. Sooner, every call at Needs approval would raise
	// it. Held to the TTL by a test (internal/engine).
	MCPApprovalGrace = 10 * time.Minute
)

// ReadModel is what one domain DECLARES about the linearizable reads its log
// takes: the barrier records a day each agent seat's reads append to it.
//
// Per domain, as a barrier lands on the log of the domain a read is about: one
// figure for every log held the identity estate's, which takes none, to a census
// it was never sized for. Required of every domain with a barrier (the engine's
// register): zero is a declaration, not a default; `census_drift` then names
// the first barrier.
type ReadModel struct {
	// PerSeatDay is the linearizable reads a day each agent seat's reads put on
	// this log; zero declares that no read of the domain takes a barrier.
	PerSeatDay int
}

// Census is what one log's expected read rate is derived from.
type Census struct {
	// Reads is the log's domain's declared read model.
	Reads ReadModel

	// Seats is the running company's agent seats.
	Seats int

	// Background is the barrier records a day the engine's own periodic reads
	// put on this log whatever the seats do: the object store's collector pins
	// the estate on a fixed cadence from one data node at a time
	// (collect.PinsPerDay), each pin a barrier on every log it reads. Without
	// it a one-seat company fired census_drift, louder the smaller the company.
	Background int

	// Clients is the linearizable reads a day a log's own sizing declares for
	// readers that are neither seats nor the engine's passes: the code log's git
	// clients (gitlet.GitReadsPerDay), people and their CI, which no seat count
	// predicts. Zero on every other log. Sized by the ceiling the log's stream
	// holds, not what the company hosts now. Without it a code log alarmed past
	// 1 300 git reads a day, which one CI exceeds tenfold. A seat's and its
	// coding boxes' reads are the seats' term, re-syncs held to it
	// (gitlet.ResyncBurst); a check box's are no term's.
	Clients int
}

// Expected is the log's census: the linearizable reads a day it was sized for.
//
// [ReadModel.PerSeatDay] times seats, a company with no agent seat counting as
// one or zero would fire on its operators' first read, plus [Census.Background]
// and [Census.Clients]. It is the whole company's figure on each log it reads,
// not a share: the ceiling is sized that way, and divided, the tracker's log
// would fire doing the work it was sized for.
func (c Census) Expected() int {
	return max(c.Reads.PerSeatDay, 0)*max(c.Seats, 1) + max(c.Background, 0) + max(c.Clients, 0)
}

// Kind names one alarm: a string because it travels (a metric attribute, a log
// field, a dashboard row) and an operator has to be able to type it.
type Kind string

// The alarm kinds, one per rule in [table]: constants so a misspelling is a
// compile error, not a dashboard row nobody can find. A kind's meaning is
// [alarmMeaning] and its remedy is its rule's, both rendered by
// [AlarmReference], whose suite refuses a rule whose meaning nobody wrote.
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
	KindFeedDeadLettered    Kind = "feed_dead_lettered"
	KindInboundDeadLettered Kind = "inbound_dead_lettered"
	KindFeedStopped         Kind = "feed_stopped"
	KindMaintenanceOpen     Kind = "maintenance_open"
	KindVolumeLow           Kind = "volume_low"
	KindWALLarge            Kind = "wal_large"
	KindPoolStarved         Kind = "pool_starved"
	KindCensusDrift         Kind = "census_drift"
	KindObjectsMissing      Kind = "objects_missing"

	KindObjectsBucketNearCeiling  Kind = "objects_bucket_near_ceiling"
	KindObjectsBucketRaiseRefused Kind = "objects_bucket_raise_refused"
	KindObjectiveBurn             Kind = "objective_burn"
	KindClockSkew                 Kind = "clock_skew"

	// Gitlet's: a Gitlet node's own work, beside the logs', and the
	// committer's on the data node that decides a repository's pushes.
	KindCodeMaintenanceRefused Kind = "code_maintenance_refused"
	KindCodeCompactionBehind   Kind = "code_compaction_behind"
	KindCodeCommitBusy         Kind = "code_commit_busy"
	KindCodeChecksBacklog      Kind = "code_checks_backlog"
	KindCodeMergeRateOver      Kind = "code_merge_rate_over_capacity"
	KindCodeRefsNearLimit      Kind = "code_refs_near_limit"

	// The model kinds, each about ONE model entry ([PerEntryKinds]).
	KindLLMCredentialExpiring    Kind = "llm_credential_expiring"
	KindLLMCredentialExpiresSoon Kind = "llm_credential_expires_soon"
	KindLLMSignInRefused         Kind = "llm_sign_in_refused"
	KindLLMLimit                 Kind = "llm_limit"
	KindLLMLimitWaiting          Kind = "llm_limit_waiting"
	KindCodeMirrorFailing        Kind = "code_mirror_failing"

	// The executor's: a seat's run whose finished job waits for an operator.
	KindExecutorResumeBlocked Kind = "executor_resume_blocked"

	// The MCP kinds, each about ONE configured MCP server ([PerEntryKinds]).
	KindMCPServerFailing     Kind = "mcp_server_failing"
	KindMCPServerPartial     Kind = "mcp_server_partial"
	KindMCPServerWaitingLong Kind = "mcp_server_waiting_long"
	KindMCPToolsRefused      Kind = "mcp_tools_refused"
	KindMCPOAuthNeedsSignIn  Kind = "mcp_oauth_needs_sign_in"
	KindMCPOAuthExpiring     Kind = "mcp_oauth_expiring"
	KindMCPApprovalWaiting   Kind = "mcp_approval_waiting"
	KindMCPToolsChanged      Kind = "mcp_tools_changed"
)

// Reading is everything an alarm evaluation looks at, gathered once per tick.
//
// One struct, not a callback per condition: the table is evaluated together on
// ticks that already run (every [AlarmInterval], and after the trim's
// measurements land), so its cost follows the facts read, not the alarms defined.
//
// A field this node cannot measure stays zero, and every condition reads zero
// as "nothing to report", not "at the floor".
type Reading struct {
	// Up is the kinds this node's tracker had up at its latest evaluation
	// ([Tracker.Standing]): a rule that clears past its raise ([rule.stays])
	// is judged by it.
	Up []Kind

	// ApplyLag is how old the oldest unapplied record is.
	ApplyLag time.Duration

	// RefusalsSince is how long reads have been refused for a reason other
	// than ordinary lag. Zero when nothing is being refused.
	RefusalsSince time.Duration

	// BarrierP95 is the read barrier's 95th percentile.
	//
	// Every …P95 here is a histogram quantile, the upper boundary of the bucket
	// the percentile falls in ([metrics.Snapshot.Quantile]): "up to" this. An
	// alarm holds one to a budget that is itself a boundary, so it fires exactly
	// when the percentile is past the budget
	// (TestEveryPercentileAlarmIsDecidedAtABoundary), and its detail says "up to".
	BarrierP95 time.Duration

	// HeadroomFraction is how much of the byte ceiling the log's ordinary writes
	// are held to is unused ([Reservation.Headroom]). A pointer: zero (a full
	// log) and "not measured" are opposite facts. Nil is every node until the
	// trim runs once, and every node whose domain declares no ceiling.
	HeadroomFraction *float64

	// BackupAge is how old the newest verified backup point the policy counts
	// for one log is, and BackupMaxAge what the operator asked for. A zero
	// BackupMaxAge is a deployment that wants no backup policy, not an alarm.
	// Per DOMAIN ([ReportInputs.BackupMaxAge]). BackupAge is a pointer for
	// HeadroomFraction's reason: zero is reassuring (verified this second), nil
	// is "none ever recorded", and the condition says so in words.
	BackupAge    *time.Duration
	BackupMaxAge time.Duration

	// TrimBlockedFor is how long the trim has been unable to advance, and
	// TrimBlockedBy names the term holding it. Per DOMAIN.
	TrimBlockedFor time.Duration
	TrimBlockedBy  string

	// TrimPastWindow is how many of the log's sequences lie below the age term:
	// records older than ReplayWindow that a blocked trim is keeping. Per
	// DOMAIN. Exact on a strict log, at most that on a compacted one (hence the
	// detail's "up to").
	//
	// Measured from the age term's sequence against the log's first, not
	// assumed from how long the block has lasted: a log nobody wrote to is
	// blocked for ever and holds nothing. Zero is the benign end and so is
	// unmeasured, so it needs no pointer.
	TrimPastWindow uint64

	// DeferredAge is how long this node has held the oldest record it could not
	// apply on one log, from the position heartbeat's first sighting of it (the
	// instant [Health.Fault] measures the grace from, via [DeferredSince.Age]),
	// and DeferredRecord which record: its position and the record version it
	// was written at against the one this build reads. Per DOMAIN, from
	// [DomainInputs], never the node's oldest: every log's copy is judged on its
	// own (ADR-0079), and one alarm naming the oldest hid the others.
	DeferredAge    time.Duration
	DeferredRecord string

	// FloorUnknownFor is how long one log's trim floor has been unreadable on
	// this node (from the first alarm heartbeat that could not read it) and
	// FloorUnknownCause what the latest read said: the key could not be read
	// (coordination unreachable, or its row is no floor) or the floor is at a
	// generation this node has not reached, with different remedies. One
	// duration a domain: one still served from its memo ([FloorCacheStale])
	// would otherwise be given another's age as evidence.
	FloorUnknownFor   time.Duration
	FloorUnknownCause string

	// PrefetchP95 and SearchP95 are the knowledge scans this node coordinated:
	// a turn's prefetch, and those somebody ran on purpose. Two series, each
	// held to its own budget.
	PrefetchP95, SearchP95 time.Duration

	// SearchDegradedFraction and SearchScopedFraction are the fractions of
	// searches answered without the semantic half and without the whole
	// corpus.
	SearchDegradedFraction, SearchScopedFraction float64

	// HistoryPartialFraction is the fraction of this node's fleet history reads
	// answered without every live data node, and HistoryReadBudget the budget
	// those reads waited on the others (internal/eventfan's FleetReadBudget).
	// The budget travels with the reading, not imported: the scatter is a layer
	// above this framework (ADR-0015).
	HistoryPartialFraction float64
	HistoryReadBudget      time.Duration

	// SemanticCoverage is the fraction of the corpus with current vectors. A
	// pointer, for HeadroomFraction's reason: with no embeddings configured
	// nothing is measured, and zero coverage is the alarm, not the absence.
	SemanticCoverage *float64

	// IVFRecall is the recall the latest measurement of the semantic index found
	// against the exact scan (ADR-0028), in the query shape nearest its floor,
	// IVFShape; IVFRecallFloor is that shape's floor, IVFMeasuredOn how many
	// sources the corpus held.
	//
	// The floor is supplied: the curve is internal/search's FloorAt, which
	// imports this package. A pointer, for SemanticCoverage's reason: an index
	// never trained, or retired for the corpus's size, measured nothing.
	IVFRecall      *float64
	IVFRecallFloor float64
	IVFMeasuredOn  int
	IVFShape       string

	// RecordsGated and FeedUnreadable are counts over the last day. Any
	// value above zero is an alarm: a gated record is recoverable by
	// nothing, and a change record no build could translate is a wake
	// nobody gets until a build that reads it runs.
	RecordsGated, FeedUnreadable int

	// FeedDeadLetters is every record one log's wake feed has dead-lettered, as
	// this node last read the dead-letter stream: how long ago each was kept
	// and how long until retention lets it go, oldest first. Per DOMAIN, from
	// [DomainInputs] (internal/changefeed's DeadLetterSubject).
	//
	// Every entry is a wake nobody has had: a record no node could read, kept
	// whole until a node able to read it replays it or retention lets it go.
	FeedDeadLetters []FeedDeadLetter

	// InboundDeadLetters is every delivery the notification service's inbound
	// group never handled, kept on its unhandled subject (internal/notify's
	// UnhandledSubject), as FeedDeadLetters says. Node-wide: the group is one
	// for the fleet. Every entry is a wake nobody has had.
	InboundDeadLetters []FeedDeadLetter

	// FeedsDown is every change feed on this node that holds no consumer: which
	// feed, how long, how many attempts have failed and what the latest said.
	//
	// The outages themselves, not a count: a broker that cannot be reached
	// stops every feed, a consumer it refuses stops one. A fact of its own, as
	// a feed that cannot reach its consumer publishes no wake and
	// `objective_burn` judges only wakes that were published.
	FeedsDown []FeedOutage

	// MaintenanceOpenFor is how long the oldest capacity window has been open,
	// with the log it is on and the phase it is stuck in.
	//
	// MaintenanceUnreadable says that window is the one this node last read
	// open and can't read now; MaintenanceOpenFor is then its age when that
	// read answered, MaintenanceReadAgo how long ago. Read as absent, a
	// coordination blip cleared a firing alarm; aged on this node's clock, a
	// window last read at 59 minutes crossed the hour during the blip.
	MaintenanceOpenFor    time.Duration
	MaintenanceStream     string
	MaintenancePhase      string
	MaintenanceUnreadable bool
	MaintenanceReadAgo    time.Duration

	// FreeBytes and StoreBytes are one volume's free space and what this node's
	// databases occupy on it, and StoreVolume its directory: of the volumes the
	// databases are on, the one with the least room for the next copy.
	// CopyHeldBytes is the write-ahead log that copy is expected to hold back
	// while it runs, as each file's last copy measured it
	// (store.DB.CopyHoldsBack); zero where none has.
	FreeBytes, StoreBytes, CopyHeldBytes int64
	StoreVolume                          string

	// WALBytes is the larger write-ahead log's size and WALFile its database.
	// WALCopy is the copy holding that log now (the part file it writes, and how
	// many more hold the log beside it) and WALCopyFor how long, empty and zero
	// when none does: a copy holds its file's log for its whole run
	// (store.DB.Copies) and its end folds the log back in, where a log nothing
	// holds is a checkpoint that is not happening.
	WALBytes   int64
	WALFile    string
	WALCopy    string
	WALCopyFor time.Duration

	// StoreVolumeUnmeasured says which database or volume could not be measured
	// and why, empty when all were. It is the alarm, not an absence: leaving it
	// out would judge the node on its other volume.
	StoreVolumeUnmeasured string

	// PoolWaitP95 is how long a caller queues for a database connection.
	PoolWaitP95 time.Duration

	// LinearizableReads and LinearizableReadsExpected are one log's observed
	// and designed-for daily read rates: the barrier records committed to the
	// log in the last day, from every node, and its census ([Census.Expected]).
	// Per DOMAIN, from [DomainInputs]: one reading naming only the log furthest
	// past its census said nothing of a second log past its own.
	LinearizableReads, LinearizableReadsExpected int

	// LogCeilingField is the Tier A field that sets this log's byte ceiling
	// (`stream.iam_log_max_bytes`, say), named in a log's census line: one
	// remedy naming the mutation log's sent every other log's operator to the
	// wrong field. Empty where the caller named none. Per DOMAIN, from
	// [DomainInputs.CeilingField].
	LogCeilingField string

	// LogBytesPerDay is what one log took in over the trailing day, LogMaxBytes
	// the ceiling its broker enforces and ReplayWindow the `min_age` the trim
	// keeps, the window the log must be able to hold. Per DOMAIN.
	//
	// A pointer: a log that took in nothing yesterday is a measurement (it can
	// hold any window), while an unmeasured node or a log in its first two days
	// (whose trailing day contains its import) knows nothing. One
	// representation would show it as idle, and any rule dividing by the rate
	// would fire on every boot.
	LogBytesPerDay *uint64
	LogMaxBytes    uint64
	ReplayWindow   time.Duration

	// ObjectsMissing is how many files' bytes the object store does not hold,
	// holds wrong (another size or digest, or no readable object) or would not
	// answer for, as the collector's last complete audit found: on the node
	// holding the collector's duty, zero on every other, as one node's count of
	// the shared store is the fleet's. Any value above zero is an alarm, for the
	// gated record's reason: no threshold makes an unopenable file acceptable.
	ObjectsMissing int

	// ObjectsBucketBytes is how many bytes the nats object bucket holds, and
	// ObjectsBucketMaxBytes the reservation the broker holds it to, as the
	// collector's duty holder last read them: zero on every other node and on a
	// fleet on S3, which reserves nothing. A zero MaxBytes is "not measured",
	// never "full".
	ObjectsBucketBytes, ObjectsBucketMaxBytes int64

	// ObjectsBucketWanted is the store.objects.nats_max_bytes this node's boot
	// did not raise the nats bucket to, ObjectsBucketKept what it kept, and
	// ObjectsBucketWhy why; zero where no raise was refused.
	ObjectsBucketWanted, ObjectsBucketKept int64
	ObjectsBucketWhy                       string

	// ObjectiveBurns is every service objective this node is spending the error
	// budget of too fast, each over the fastest pair of windows that fires
	// ([objectives.Meter.Burning]). Carried already judged by internal/objectives,
	// which owns the targets, windows and factors (ADR-0015).
	ObjectiveBurns []objectives.Burn

	// ClockSkew is how far this node's clock stands from the fleet's, and
	// BrokerClockSkew how far the clock of the broker member leading the
	// position register stands from it, each signed (positive is ahead) as the
	// heartbeat last measured: the median of the data nodes' stamped-vs-stored
	// skew is the fleet's clock, over ClockSkewNodes clocks. ClockSkewBound is
	// objstore.AssumedClockSkew, handed in as this framework does not import the
	// object store (ADR-0015). Zero when unmeasured; a zero bound judges nothing.
	// Two clocks: against the member alone a node read the member's drift as its
	// own.
	ClockSkew, BrokerClockSkew time.Duration
	ClockSkewNodes             int
	ClockSkewBound             time.Duration

	// CodeMaintenanceRefused is how many repositories this node's git cache
	// cannot write a bitmap of: it (charged by the repository's objects, with
	// room for one clone) needs more memory than the node can give background
	// work. CodeMaintenanceLargest is the largest charge, CodeMaintenanceBudget
	// what the node can give (internal/gitlet/cache). Zero on a node serving no
	// git.
	CodeMaintenanceRefused                        int
	CodeMaintenanceLargest, CodeMaintenanceBudget int64

	// CodeCompactionBehind is how many repositories this node maintains (as
	// their MAINTAINER, the Gitlet node that merges packs and publishes layers)
	// its compactor found at the most live packs allowed, so pushes carrying a
	// pack are refused `compaction_behind`; ...Largest is the most live packs
	// among them and ...Repo that repository. Judged against
	// gitlet.MaxPacksPerRepo by the compactor (internal/gitlet/compact; ADR-0015).
	CodeCompactionBehind        int
	CodeCompactionBehindLargest int64
	CodeCompactionBehindRepo    string

	// CodeCommitBusy is how many proposals this node's COMMITTER (the first live
	// data node in a repository's rendezvous order) refused busy for a backlog
	// over the last TrimInterval: a full repository queue, the node's whole
	// bound, or a proposal that lost its time behind earlier ones
	// (internal/gitlet's Committer.AlarmReading). ...Repo has the most, ...OfRepo
	// is how many were its, ...Node met the node's bound, ...Waited waited until
	// their asker gave up. Not counted: an arrival with its attempt spent, or one
	// that lost every compare-and-set round. Judged at the committer's bounds
	// (ADR-0015); window gitlet.CommitBusyWindow.
	CodeCommitBusy       int
	CodeCommitBusyRepo   string
	CodeCommitBusyOfRepo int
	CodeCommitBusyNode   int
	CodeCommitBusyWaited int

	// CodeChecksBacklog is how many queued check attempts have waited for a
	// runner longer than their own timeout (timeout_seconds), as this node's rows
	// hold them (internal/gitlet's Reader.ChecksBacklog), exact up to
	// gitlet.BacklogScan; ...More reports more. ...Oldest is the longest wait
	// and ...Check that attempt (repository, commit, branch, check, attempt).
	// Measured from the rows by every data node, never a runner: with every
	// Gitlet node gone no check runs, the case the alarm must not miss. Judged
	// at each attempt's own timeout (ADR-0015).
	CodeChecksBacklog       int
	CodeChecksBacklogMore   bool
	CodeChecksBacklogOldest time.Duration
	CodeChecksBacklogCheck  string

	// CodeMergeRateOver is how many merge queues this node is the home of were
	// asked to land more changes within CodeMergeRateWindow (internal/gitlet's
	// MergeRateWindow) than they can: enqueues, live or ended, past the capacity
	// (config's LandingCapacity, depth × batch a round at the mean round).
	// ...Target is the queue asked furthest past it, by branch and repository,
	// with ...Asked, ...Capacity and ...Round. Judged at the queue's own capacity
	// (ADR-0015, M5-D28); raised by the home, which alone builds and times its
	// trains.
	CodeMergeRateOver         int
	CodeMergeRateOverTarget   string
	CodeMergeRateOverAsked    int
	CodeMergeRateOverCapacity int
	CodeMergeRateOverRound    time.Duration
	CodeMergeRateWindow       time.Duration

	// CodeRefsNearLimit is how many active repositories, as this node's rows hold
	// them, will reach the most live refs allowed within the ref lifecycle's
	// window unless refs are deleted (internal/gitlet's NearLimit,
	// Reader.RefsNearLimit). ...Repo is the one with the most, ...Live its live
	// refs, ...Recent those set within the window. CodeRefsLimit and
	// CodeRefsWindow are gitlet.MaxRefsPerRepo and gitlet.RefExpiry. Judged at
	// those two decisions (ADR-0015), measured by every data node on the trim's
	// tick, not a Gitlet node.
	CodeRefsNearLimit       int
	CodeRefsNearLimitRepo   string
	CodeRefsNearLimitLive   int
	CodeRefsNearLimitRecent int
	CodeRefsLimit           int
	CodeRefsWindow          time.Duration

	// CodeMirrorFailing is how many mirrors of repositories this node is home to
	// (a pull mirror's syncs, a push mirror's pushes) have not been level for
	// longer than their refresh promise: gitlet.MirrorInterval plus mirror's
	// AttemptDeadline (Mirrors.AlarmReading). ...Repo is the one failing longest
	// and ...For how long since it was last level, or since this node first tried
	// it. Judged at the mirror's own promise (ADR-0015); raised by the home alone,
	// from its memory of its attempts.
	CodeMirrorFailing     int
	CodeMirrorFailingRepo string
	CodeMirrorFailingFor  time.Duration

	// ExecutorResumesBlocked is how many seats' runs, across the fleet, finished
	// their executor's job and failed their last resume on a condition only an
	// operator clears (internal/sandbox's Coordinator.ResumesBlocked).
	// ...Seat is the run waiting longest, ...Node the node holding it, ...Kind
	// the condition, ...For how long since the first such failure in a row. Read
	// fleet-wide, as a satellite holding the seat evaluates no table (the package
	// doc's "A run waiting for an operator is raised fleet-wide").
	ExecutorResumesBlocked    int
	ExecutorResumeBlockedSeat string
	ExecutorResumeBlockedNode string
	ExecutorResumeBlockedKind string
	ExecutorResumeBlockedFor  time.Duration

	// Models is each configured model entry's condition across the fleet, and
	// ModelsAt the instant it was gathered, which expiry and waiting are
	// measured against. A zero ModelsAt gathered none and evaluates no model
	// alarm.
	//
	// Fleet-wide, as a limit met on one node and a refusal on another are both
	// the entry's: every data node reads the live leases' model rows and the
	// secret store's write instants. [ModelCondition.NoSignIn] is the gathering
	// node's own reading.
	Models   []ModelCondition
	ModelsAt time.Time

	// MCPServers is each MCP server the active configuration runs, its condition
	// joined across the fleet's live nodes, and MCPServersAt the instant it was
	// gathered. A zero MCPServersAt gathered none and evaluates no MCP alarm.
	// Fleet-wide, like [Reading.Models], so the alarm reads the same on every
	// node.
	MCPServers   []MCPServerCondition
	MCPServersAt time.Time
}

// FeedDeadLetter is one dead letter as the alarm reads it: a record a log's wake
// feed dead-lettered ([Reading.FeedDeadLetters]), or a delivery the inbound
// group never handled ([Reading.InboundDeadLetters]).
type FeedDeadLetter struct {
	// Age is how long ago the dead-letter stream kept it.
	Age time.Duration

	// ExpiresIn is how long until retention (`stream.event_retention_hours`)
	// lets it go, its wake with it: zero or less once due.
	ExpiresIn time.Duration
}

// FeedOutage is one change feed that holds no consumer.
type FeedOutage struct {
	// Source is the estate the feed reads: `work`, `page` or `chat`.
	Source string

	// For is how long it has held none: since its consumer failed, or since it
	// began, for a feed that never opened one.
	For time.Duration

	// Failures counts the failed attempts in that time and Cause is what the
	// latest said. Zero and empty before the first open answers.
	Failures int
	Cause    string
}

// ModelCondition is one model entry's condition, gathered across the fleet.
type ModelCondition struct {
	// Key is the entry's key, and Variable the variable its credential is read
	// from ("" where it reads none).
	Key, Variable string

	// ExpiresAt is when its credential expires, zero when unknown: a key, an
	// environment value, a login, or a harness that declares no lifetime.
	ExpiresAt time.Time

	// RefusedAt is the latest auth failure on any live node when newer than the
	// latest answer on any node, zero otherwise; RefusedSaid is what the harness
	// said.
	RefusedAt   time.Time
	RefusedSaid string

	// NoSignIn is an entry nothing signs in on the gathering node while a seat
	// or the assistant names it. An entry nobody runs is no alarm.
	NoSignIn bool

	// SittingOut is the latest window it sits out across live nodes, nil if none.
	SittingOut *coord.ModelSitOut

	// WaitingSince is the earliest instant a seat was parked on a chain naming
	// it, across live nodes; zero when none waits.
	WaitingSince time.Time
}

// MCPServerCondition is one MCP server's condition, joined from every live
// node's status record row for it ([coord.MCPServerStatus]).
//
// One per server the configuration runs: a server turned off (`enabled:
// false`) raises nothing, and one only a node on another revision still
// reports is that node's apply, which the control plane already names.
type MCPServerCondition struct {
	// Server is the server's name in `mcp_servers`.
	Server string

	// Started, Failed and Waiting are its instances summed over every node:
	// serving, not serving, and held back unstarted for a credential a
	// provisioning pass mints. The MCP answer decides a server's state from
	// these sums, so `mcp_server_failing` stands on a server the page calls
	// failing and `mcp_server_partial` on one it calls partial.
	Started, Failed, Waiting int

	// DownSince is the earliest instant a failed instance on any node stopped
	// serving or first failed to start, zero when none has, and Down the
	// instance it dates: an instance that keeps failing across retries keeps
	// one clock.
	DownSince time.Time
	Down      MCPInstanceNote

	// WaitingSince is the earliest instant an instance on any node was held back
	// for its credential, zero when none waits, and Wait that instance, its
	// Said the wait's own sentence (the variables, and the integration that
	// mints them).
	WaitingSince time.Time
	Wait         MCPInstanceNote

	// Refused are the tools an instance lists that are offered under no name,
	// each by the name it would be filed under, once across the fleet and sorted
	// (a node names at most [coord.MaxMCPToolNotes]); RefusedCount is how many
	// there are as the node counting the most counts them, never fewer than
	// Refused lists.
	Refused      []string
	RefusedCount int

	// SignIn is the server's OAuth sign-in, nil for a server not using OAuth.
	SignIn *MCPSignInCondition

	// Approvals is how many calls to the server's tools agents' runs hold for a
	// person's approval (ADR-0077), ApprovalSince when the longest-waiting was
	// held (zero with none), and Approval that call.
	Approvals     int
	ApprovalSince time.Time
	Approval      MCPApprovalNote

	// Changes is how many of the server's tools are held at Needs approval
	// because their definition changed since their level was decided, or the
	// server added them after its first listing (ADR-0078); Loosened how many
	// claim less risk than before; ChangeSince when the fleet first saw the
	// change seen first (zero where schemas are not stored); and Change that
	// change.
	Changes     int
	Loosened    int
	ChangeSince time.Time

	// ChangesUnread is a server whose tool changes the gathering node could not
	// judge (live listings or accepted levels unread, or no live node lists the
	// server): not the same fact as no change.
	ChangesUnread bool
	Change        MCPChangeNote
}

// MCPChangeNote is the one change to a server's tools its condition names: the
// tool, by the server's own name for it, whether it was added or changed, and
// whether it now claims less risk.
type MCPChangeNote struct {
	Tool, Kind string
	Loosened   bool
}

// MCPApprovalNote is the one held call a server's condition names: the seat
// whose run made it, and the tool as the run called it.
type MCPApprovalNote struct {
	Seat, Tool string
}

// MCPSignInCondition is an MCP server's OAuth sign-in, as the fleet's record
// of its grant holds it ([coord.OAuthGrant]): never a token.
type MCPSignInCondition struct {
	// Held is whether anybody holds a grant for the server.
	Held bool
	// Failure is its vendor's words for the renewal it refused for good,
	// bounded and redacted, and FailedAt when; "" while the grant renews.
	Failure  string
	FailedAt time.Time
	// Expiry is when the grant runs out where its vendor said, zero where
	// it did not.
	Expiry time.Time
}

// needsSignIn reports whether s is a server nobody is signed in to, whose vendor
// refused to renew its grant, or whose every instance is held back for a
// sign-in (a grant made for an address the server is no longer at): what
// `mcp_oauth_needs_sign_in` stands on, and the failing and partial kinds leave
// to it.
func (s MCPServerCondition) needsSignIn() bool {
	return s.SignIn != nil && (!s.SignIn.Held || s.SignIn.Failure != "" || s.heldForSignIn())
}

// heldForSignIn reports whether no instance of s serves and the one down
// longest is held back for a sign-in, in the nodes' own class.
func (s MCPServerCondition) heldForSignIn() bool {
	return s.Started == 0 && s.Failed > 0 && s.Down.Class == MCPNeedsSignInClass
}

// MCPNeedsSignInClass is the failure class a node reports an instance held
// back for a sign-in under (internal/mcp's FailNeedsSignIn), spelled here as
// the outage grace is, and held to the client's by a test.
const MCPNeedsSignInClass = "needs_sign_in"

// MCPInstanceNote is the one instance a server's condition names: the node it
// is on, its seat's handle ("" for a shared server's), its failure class ("" for
// a wait, or a failure no class fits) and what was said, as the heartbeat
// carries it (redacted, and bounded to [coord.MaxMCPErrorBytes]).
type MCPInstanceNote struct {
	Node, Seat, Class, Said string
}

// Alarm is one condition currently true on this node.
//
// Tagged like every struct in [Report]: it is reached through
// `GET /worklet/retention`, so its field names are a wire contract. Untagged,
// `crewlet retention status` survived only because Go's decoder matches field
// names case-insensitively; every other reader (the dashboard, `jq`) is case
// sensitive and read the alarm as absent.
type Alarm struct {
	// Kind is which alarm.
	Kind Kind `json:"kind"`

	// Title and Person are the kind's static words for a company owner: what is
	// wrong, then what it affects and what to do. Detail and Remedy stay the
	// operator's.
	Title  string `json:"title"`
	Person string `json:"person"`

	// Detail says what was measured, in the operator's units: "this node is
	// 4m12s behind", not "apply_lag". For a per-log condition it is the most
	// pressing log's measurement; each [Alarm.Domains] entry carries its own.
	Detail string `json:"detail"`

	// Remedy is what to do about it, carried on the alarm so every surface
	// renders the same one.
	Remedy string `json:"remedy"`

	// Domains is every log this alarm stands on, for a condition of a log
	// rather than the node ([PerLogKinds]). Empty for a node-wide one.
	//
	// One alarm per kind, its logs in it: one alarm per log told the health
	// count four while the gauge said one. Where no trim term holds them, one
	// entry names every log it was measured on. The log lines stay per log: the
	// [Tracker] forms one instance per (kind, log), so a log joining or leaving
	// writes its own `alarm_raised` or `alarm_cleared`.
	Domains []AlarmDomain `json:"domains,omitempty"`

	// Entries is every configured entry this alarm stands on, for a condition
	// about one model entry or MCP server ([PerEntryKinds]), Domains' analogue:
	// one alarm per kind, leading with the longest-standing entry's detail, and
	// one pair of log lines per entry. Empty for every other kind.
	Entries []AlarmEntry `json:"entries,omitempty"`
}

// AlarmEntry is one configured entry an alarm stands on: a model entry, or an
// MCP server.
type AlarmEntry struct {
	// Key is the entry's key in `providers.llm`, or the server's name in
	// `mcp_servers`.
	Key string `json:"key"`

	// Detail is this entry's own measurement.
	Detail string `json:"detail"`

	// Since is when the condition began for this entry: the report that opened
	// a sit-out, the first park, the refusal, the instant an expiry came within
	// the threshold, or an MCP instance went down or was held back. Zero where
	// the condition has no instant of its own.
	Since time.Time `json:"since,omitzero"`

	// Seat is the agent seat, by handle, whose instance the measurement names:
	// an agent's instance of an MCP server that is down or waiting. A shared
	// server's instance and every model entry name none. A remedy aimed at one
	// agent's instance acts on it.
	Seat string `json:"seat,omitempty"`
}

// AlarmDomain is one measurement an alarm stands on, and the logs it was
// taken on.
type AlarmDomain struct {
	// Domains is the logs' registered names, in the report's order, never
	// empty. One log wherever Term is set: a held trim's entry carries that
	// log's own instant and count. Otherwise every log measured identically is
	// carried once, naming them all, and each surface renders what it is given:
	// a young fleet's missing backup was printed once per log under a headline
	// that already said it.
	Domains []string `json:"domains"`

	// Detail is these logs' measurement, in the operator's units; under
	// [KindBackupAge] followed by the held trim's where Term is set, since that
	// log raises no [KindTrimBlocked] to say it.
	Detail string `json:"detail"`

	// Term is the trim term holding this log, where the alarm is about its trim:
	// under [KindTrimBlocked], and under [KindBackupAge] for a log the backup
	// term holds, which raises no [KindTrimBlocked]. It is the term's own name,
	// which `crewlet retention status` prints and the retention guide is
	// indexed by.
	Term TermName `json:"term,omitempty"`

	// Since is when this log's trim stopped advancing, wherever Term is set: an
	// instant, so a reader renders the age against its own clock.
	Since time.Time `json:"since,omitzero"`
}

// rule is one row of the table.
type rule struct {
	kind Kind
	// fires reports whether the condition holds, and with what detail. A
	// closure, as half the conditions compare two fields of the reading.
	fires func(Reading) (string, bool)
	// stays, where set, keeps a node-wide alarm that is up ([Reading.Up])
	// standing: a wider condition than fires, so a reading hovering at the
	// raise threshold does not flap.
	stays func(Reading) (string, bool)
	// remedy is what an operator does about it.
	remedy string
	// title and person are what a company owner reads ([Alarm.Title],
	// [Alarm.Person]), declared on the row so no kind ships without them.
	title, person string
	// perLog marks a condition of one log rather than the node, evaluated once
	// per log from that log's inputs and listed under the kind's one alarm
	// ([Alarm.Domains]), its log lines written per log by the [Tracker]. Held
	// against what the report raises per log by a test in both directions: a
	// flag nothing checked would be the second opinion the table prevents.
	perLog bool
	// model and server mark a condition about one configured entry, evaluated
	// once per [Reading.Models] row at [Reading.ModelsAt] or per
	// [Reading.MCPServers] row at [Reading.MCPServersAt] and listed under the
	// kind's one alarm ([Alarm.Entries]). A row sets one of the two and fires is
	// unused. Each answers with the key left for [rule.evaluateEntries] to fill.
	model  func(m ModelCondition, now time.Time) (AlarmEntry, bool)
	server func(s MCPServerCondition, now time.Time) (AlarmEntry, bool)

	// unread reports a server row whose condition this rule cannot judge from
	// this reading: its instance is neither raised nor cleared ([Unjudged]).
	unread func(s MCPServerCondition) bool
}

// perEntry reports whether the rule is about one configured entry
// ([PerEntryKinds]).
func (rule rule) perEntry() bool { return rule.model != nil || rule.server != nil }

// table is every alarm this engine can raise, in the order they are reported.
var table = []rule{
	{
		kind:  KindApplyLag,
		title: "A server is falling behind",
		person: "A server is behind on the company's recent changes, so it takes on no new agents. " +
			"Check it if it does not catch up.",
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
		kind:  KindReadRefusals,
		title: "A server is refusing reads",
		person: "A server refuses reads for a reason other than catching up, so some pages fail to load. " +
			"Check its log for the refusal reason.",
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
		kind:  KindBarrierSlow,
		title: "Reads are slow on a server",
		person: "Reads that need the latest changes wait too long on a server, so pages load slowly. " +
			"Check the message broker and the server's storage.",
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the read barrier's p95 is past %s, a quarter of the "+
					"read budget: up to %s", spoken(ReadBudget/4), spoken(r.BarrierP95)),
				r.BarrierP95 > ReadBudget/4
		},
		remedy: "The barrier is an append and a wait: check the broker's own " +
			"latency and this node's apply drain before looking anywhere else.",
	},
	{
		kind:  KindLogHeadroom,
		title: "A data log is almost full",
		person: "A data log is close to its size limit, and once full it refuses new changes. " +
			"Raise its limit or unblock its cleanup.",
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
		// The forward-looking half of the headroom alarm, with a different
		// remedy: the trim never removes a record younger than `min_age`, so a
		// log whose ceiling is smaller than `min_age` of its writing fills with
		// every term satisfied. Only the ceiling or the window can fix that.
		// No threshold of its own (ADR-0015): it fires where the window no longer
		// fits in the ceiling.
		kind:  KindLogCeilingShort,
		title: "A data log will fill up",
		person: "At its current rate a data log fills before its cleanup may remove anything, " +
			"and then refuses new changes. " +
			"Raise its size limit.",
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
		// One threshold: the newest backup passing backup_max_age (a second clause
		// on how long the first had held made a 24-hour policy alarm at 48).
		// Two states, as no backup at all is not an old one: both fire, the detail
		// says which, and no age is fabricated for the first.
		kind:  KindBackupAge,
		title: "No recent backup",
		person: "Without a recent verified backup, " +
			"data logs are not cleaned up and grow until they refuse changes. " +
			"Take a backup now.",
		fires: func(r Reading) (string, bool) {
			if r.BackupMaxAge <= 0 {
				return "", false
			}
			// Worded for one log or many: carried once after all their names.
			if r.BackupAge == nil {
				return fmt.Sprintf("no verified backup has been "+
					"recorded, and the policy asks for one every %s",
					spoken(r.BackupMaxAge)), true
			}
			return fmt.Sprintf("the newest verified backup is %s "+
					"old, and the policy asks for %s", spoken(*r.BackupAge),
					spoken(r.BackupMaxAge)),
				*r.BackupAge > r.BackupMaxAge
		},
		remedy: "Run `crewlet backup` against any node, whatever its roles, " +
			"and check whatever was meant to run it; under `backup_floor: " +
			"operator`, acknowledge each named log's off-host copy with " +
			"`crewlet retention ack -stream <its stream> -position <n> " +
			"-taken-at <t>`, n being the sequence the copy reaches and t the " +
			"instant the copy was taken. The trim does not advance " +
			"a log past a backup older than the policy, and does not " +
			"advance it at all until there is one.",
		perLog: true,
	},
	{
		// A blocked trim is ordinary until it has cost something (every fresh
		// deployment has one). Fires at the replay window plus one tick
		// (ADR-0015): `min_age` is what every sanctioned block is held under, the
		// tick the block's resolution (`blocked_since` moves only with the trim);
		// and only while the log holds a record past the window. The term is a
		// field ([AlarmDomain.Term]), not in the sentence: one alarm carries logs
		// held by different terms.
		kind:  KindTrimBlocked,
		title: "Log cleanup is stuck",
		person: "A data log's cleanup has stalled, so the log keeps growing toward its size limit. " +
			"Fix what is holding it.",
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
		// Per log: every log's copy is judged on its own (ADR-0079), see
		// [Reading.DeferredAge].
		kind:  KindDeferredOld,
		title: "A server cannot read new changes",
		person: "A server runs an older version that cannot read changes other servers write, " +
			"so it stopped serving the affected logs. " +
			"Upgrade that server.",
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
		// Per log: a read refuses on that log's floor, and a floor ahead of this
		// node is one log's re-anchor.
		kind:  KindFloorUnknown,
		title: "A server cannot serve a data log",
		person: "A server cannot read a data log's cleanup point, so it refuses reads of that log. " +
			"If it lasts, check the message broker.",
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
		kind:  KindPrefetchSlow,
		title: "Knowledge lookups are slow",
		person: "Agents wait longer than planned for knowledge lookups before each turn. " +
			"Add a data server, or check the server's storage.",
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
		kind:  KindSearchSlow,
		title: "Search is slow",
		person: "Search is slower than its target because the " +
			"content has outgrown what one server can scan. " +
			"Add a data server to share the work.",
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("interactive search is past its %s target at p95: up to %s",
					spoken(InteractiveSearchTarget), spoken(r.SearchP95)),
				r.SearchP95 > InteractiveSearchTarget
		},
		remedy: "The corpus has outgrown what one node's share can scan in " +
			"the budget. Adding a data node divides the buckets again, with " +
			"no configuration and no rebuild; a node without `data` takes " +
			"no range. See docs/guides/search.md.",
	},
	{
		kind:  KindSearchDegraded,
		title: "Search is missing meaning-based results",
		person: "Some searches ran without meaning-based matching, so their results are less relevant. " +
			"Check the embeddings provider.",
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
		kind:  KindSearchScoped,
		title: "Search covered only part of the content",
		person: "Some searches skipped part of the content because a server did not answer, " +
			"so results may be incomplete. " +
			"Check that server if it lasts.",
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
		// The fleet read budget is the threshold, borrowed (ADR-0015): the
		// detail names the value the reading carries ([Reading.HistoryReadBudget]).
		// Process-wide, not per log: the reads are of each data node's own event
		// log, which is no state log.
		kind:  KindHistoryPartial,
		title: "History is missing some entries",
		person: "Some history views missed a server's entries because it answered too slowly. " +
			"Check that server's storage and network.",
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
		kind:  KindRecallBelowFloor,
		title: "Search indexing is behind",
		person: "Some content is not yet indexed for meaning-based search, so results can miss it. " +
			"Check the embeddings provider if it does not catch up.",
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
		// The floor is the evaluation's, borrowed (ADR-0015): the recall curve
		// `crewlet search eval` judges against, at the searched corpus's size.
		// A training installs the smallest probe count that meets it, and a
		// re-measurement finding none retrains in the same tick, so a recall
		// below it was measured probing every list and says the codes fail this
		// corpus, not that the index was trained badly.
		kind:  KindIVFRecallBelowFloor,
		title: "Search accuracy is below target",
		person: "Meaning-based search misses more good matches than it should on this content. " +
			"Report it, since only a product update can fix it.",
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
		kind:  KindRecordsGated,
		title: "Some changes were dropped",
		person: "Some changes were dropped and cannot be recovered. " +
			"Check today's server logs to see which server wrote them.",
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d record(s) were dropped by an apply gate in the "+
				"last day", r.RecordsGated), r.RecordsGated > 0
		},
		// Fields by their keys on the line: the writer is under `writer`, and a
		// remedy calling it "the operator" sent readers looking for a field no
		// line carries. TestTheRecordsGatedRemedyNamesWhatItsLineCarries holds
		// every field named here to the line the applier writes.
		remedy: "A gated record is recoverable by nothing. Each drop's " +
			"`statelog_record_gated` log line names the `gate` that dropped it, " +
			"the record's `position` and `kind`, and the `writer` (the node " +
			"that wrote it); this is worth reading today.",
	},
	{
		// A record this build's translator fails on, counted on the node that met
		// it. Every feed consumer sets `MaxDeliver: -1` (a spent delivery budget
		// would drop a wake silently), so the feed bounds the record: handed back
		// with a backoff from two to thirty seconds, then at an hour old kept as
		// a dead letter (`feed_dead_lettered`, internal/changefeed's
		// DeadLetterAge). This names the build that cannot read it while a peer
		// that can still has the chance.
		kind:  KindFeedUnreadable,
		title: "A server cannot read some changes",
		person: "A server cannot read some change records, " +
			"so the agents they should wake are woken late or not at all. " +
			"Upgrade that server.",
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d change record(s) could not be translated in the "+
				"last day", r.FeedUnreadable), r.FeedUnreadable > 0
		},
		remedy: "A record no build on this node can read. It is handed back for a " +
			"node that can, with a backoff, and dead-lettered once it is an hour old " +
			"(`feed_dead_lettered`): its wake waits until then and is lost if nothing " +
			"ever reads it. Upgrade the node past it; each attempt's " +
			"`changefeed_untranslatable` log line names the record and the error.",
	},
	{
		// A wake no node could derive, kept rather than lost: a record its feed
		// handed back for an hour (a roll that outlasted it, a key no node holds,
		// an untranslatable record), kept in CREWLET_DLQ, sealed, until a node
		// able to read it replays it, an operator discards it, or retention lets
		// it go. Raised from the first entry, per log (each feed has its own
		// subject); the expiring count is taken every beat.
		kind:  KindFeedDeadLettered,
		title: "Some agents were never woken",
		person: "Some changes no server could read have not woken their agents, and will expire unread. " +
			"Upgrade a server or restore the missing encryption key.",
		perLog: true,
		fires: func(r Reading) (string, bool) {
			if len(r.FeedDeadLetters) == 0 {
				return "", false
			}
			return feedDeadLetterDetail(r.FeedDeadLetters), true
		},
		remedy: "Each entry is a wake nobody has had; `crewlet feeds dead-letters` " +
			"lists them by id, each with its record and why no node read it. Bring a " +
			"node that can read the records up — the build they need, or the keyring: " +
			"a key in `secrets.keys` on every node and a restart — and the first node " +
			"whose feed starts able to read an entry derives its wake, once, and drops " +
			"the entry. A record no build reads needs a build that fixes it. An entry no " +
			"node will ever read, such as one sealed under a key retired from every node, " +
			"is given up with `crewlet feeds discard <id> -confirm <id>`, its wake lost for good " +
			"(`crewlet.tracker.feed.dead_letter_discarded`). An entry nothing replays " +
			"or discards expires after `stream.event_retention_hours` and its wake is " +
			"lost (`crewlet.tracker.feed.dead_letter_expired`). Each record's " +
			"`changefeed_record_dead_lettered` log line names the record and why, " +
			"logged by the data node that counted it: whichever node took the " +
			"record's dead-letter claim, whichever of its keeps stored the entry.",
	},
	{
		// A wake the inbound group never handled, kept rather than lost: a newer
		// record's wake the older nodes of a roll passed round until its 25
		// deliveries were spent, or one a node that shed or stopped handed back,
		// kept until a node that reads it replays it (internal/notify) or
		// retention lets it go. Raised from the first entry, node-wide (one group
		// for the fleet). A running node of the reading build delivers within a
		// beat or two; otherwise it waits for one to start, which a roll paused
		// for good never brings.
		kind:  KindInboundDeadLettered,
		title: "Messages to agents wait for an upgrade",
		person: "Some messages from connected apps wait for a server on the newer version, " +
			"and are lost if none delivers them. " +
			"Finish the upgrade.",
		fires: func(r Reading) (string, bool) {
			if len(r.InboundDeadLetters) == 0 {
				return "", false
			}
			return deadLetterDetail(r.InboundDeadLetters, func(held, soon int) string {
				return fmt.Sprintf("the notification service's inbound group holds %d "+
					"delivery(ies) no node handled, wakes waiting for a node that reads "+
					"them, %d of them expiring within a day", held, soon)
			}), true
		},
		remedy: "Each entry is a wake nobody has had, which every node that reads it " +
			"delivers, once, dropping the entry — one already running within a few " +
			"seconds of the keep. An entry that stays is one no running node reads: " +
			"finish the roll, or start a node of the newer build if the roll is " +
			"paused, and it is delivered. Each replay that handles entries logs " +
			"`inbound_dead_letters_replayed`, and a line for each, named for what " +
			"became of it; one kept for a reason that may clear is tried again within " +
			"a minute. An entry no node delivers expires after " +
			"`stream.event_retention_hours` and its wake is lost " +
			"(`crewlet.notify.inbound.dead_letter_expired`). Each was logged as " +
			"`dead_lettered` when its budget was spent, naming the return that spent it.",
	},
	{
		// A node with no consumer at all, beside `feed_unreadable`'s stuck one.
		// At the stall grace (ADR-0015), to which the feed's schedule is held
		// ([FeedReopenCeiling]), so a feed down past it has a broker or consumer
		// still refusing.
		kind:  KindFeedStopped,
		title: "A server stopped following changes",
		person: "A server's change feed stopped, so agents may be woken late, though nothing is lost. " +
			"Check the message broker.",
		fires: func(r Reading) (string, bool) {
			var parts []string
			for _, f := range r.FeedsDown {
				if f.For > StallGrace {
					parts = append(parts, feedOutageDetail(f))
				}
			}
			return strings.Join(parts, "; "), len(parts) > 0
		},
		remedy: "Nothing is lost: the feed opens its group again on its own, at " +
			"most every " + spoken(FeedReopenCeiling) + ", and is handed every " +
			"record the group has not acknowledged, so the wakes it owes are late " +
			"rather than dropped, and any other data node consuming the group " +
			"delivers them meanwhile. Read the cause. A timeout, a missed heartbeat or no " +
			"responders is the broker, and the alarm clears at the first attempt " +
			"after the broker answers again: check the broker, or the members of " +
			"an external cluster. A refusal that names the consumer or the stream " +
			"is the broker answering and saying no, and each attempt's " +
			"`changefeed_open_failed` line carries it.",
	},
	{
		kind:  KindMaintenanceOpen,
		title: "A maintenance window is still open",
		person: "While a capacity maintenance window is open, restarted servers do no work. " +
			"Finish or cancel the window.",
		fires: func(r Reading) (string, bool) {
			fires := r.MaintenanceOpenFor > MaintenanceAlarmAfter
			if !r.MaintenanceUnreadable {
				return fmt.Sprintf("a capacity window on %s has been open for %s "+
					"in phase %q", r.MaintenanceStream, spoken(r.MaintenanceOpenFor),
					r.MaintenancePhase), fires
			}
			// Finishing or abandoning goes through the failing read, and the
			// window may have closed since.
			when := "when this node last read it"
			if r.MaintenanceReadAgo > 0 {
				when += ", " + spoken(r.MaintenanceReadAgo) + " ago"
			}
			return fmt.Sprintf("a capacity window on %s had been open for %s in "+
				"phase %q %s. This node can't read the window now, so it can't "+
				"tell whether it has closed, and it can't be finished or "+
				"abandoned through this node until it can", r.MaintenanceStream,
				spoken(r.MaintenanceOpenFor), r.MaintenancePhase, when), fires
		},
		// Only a node in service evaluates this table, so "nothing is being
		// written" was false of the node saying it; what a window does is hold
		// the fleet.
		remedy: "While a capacity window is open no node starts in service, and " +
			"every node restarted for it publishes nothing. Finish it or abandon " +
			"it.",
	},
	{
		kind:  KindVolumeLow,
		title: "A disk is low on space",
		person: "A disk holding company data may be too full for backups and restores, " +
			"which then fail partway. " +
			"Add space to that disk.",
		fires: volumeLow(VolumeHeadroomFactor),
		stays: volumeLow(VolumeClearFactor),
		remedy: "Add space to the volume the alarm names. Where it could not be " +
			"measured, fix what stops it being read. A backup, a vacuum and a peer's " +
			"join all need the room, and each fails partway through without it; a " +
			"backup's or a snapshot's copy also needs room there for the write-ahead " +
			"log it holds back while it runs, which the alarm counts. The alarm " +
			"clears once the volume has room for the copy and a quarter of the " +
			"store besides, which its detail states in bytes.",
	},
	{
		kind:  KindWALLarge,
		title: "A database file has grown large",
		person: "A database file has grown large because a backup or reader holds it open, " +
			"and could fill the disk. " +
			"Make room on the disk.",
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
		kind:  KindPoolStarved,
		title: "A server's database is overloaded",
		person: "Requests on a server wait for a database connection, so everything there is slower. " +
			"Allow more connections, or find what holds them.",
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("a database connection takes past %s at p95 to acquire: up to %s",
				spoken(PoolWaitAlarm), spoken(r.PoolWaitP95)), r.PoolWaitP95 > PoolWaitAlarm
		},
		remedy: "Raise `store.max_open_conns`, or find the caller holding one. " +
			"Every read on this node is queuing before it starts.",
	},
	{
		// Says a sizing input has stopped being true: the ceiling, trim cadence
		// and joining transfer were derived from a read rate, and at twice it
		// they describe a different company. Per log, each sized for its own
		// census ([Alarm.Domains]).
		kind:  KindCensusDrift,
		title: "A data log is busier than planned",
		person: "A data log handles more than twice the reads it was sized for, " +
			"so its size limit may be too small. " +
			"Resize that log.",
		perLog: true,
		fires: func(r Reading) (string, bool) {
			// The remedy is one sentence for every log; the ceiling field is
			// what differs.
			ceiling := ""
			if r.LogCeilingField != "" {
				ceiling = fmt.Sprintf("; its ceiling is `%s`", r.LogCeilingField)
			}
			// A declared zero fires on the first barrier: such a log was sized
			// with no barrier bytes, so any is a reader nobody counted.
			return fmt.Sprintf("%d linearizable reads a day against the %d this "+
					"log was sized for%s", r.LinearizableReads,
					r.LinearizableReadsExpected, ceiling),
				float64(r.LinearizableReads) > CensusDriftFactor*float64(r.LinearizableReadsExpected)
		},
		remedy: "Each read past the census is a barrier record on the named log, " +
			"so re-derive that log's own ceiling — the field its line names — " +
			"adding what its barriers take at the real rate to whatever else the " +
			"field is sized from, and the trim's cadence " +
			"(`stream.tracker_retention`). See " +
			"docs/getting-started/configuration.md. A code log's git census is a " +
			"share of the ceiling its stream holds, so it moves only when the log " +
			"is resized: `crewlet retention set-capacity CREWLET_CODE_LOG` " +
			"(docs/guides/retention.md); `stream.code_log_max_bytes` sizes only a " +
			"log not yet created.",
	},
	{
		kind:  KindObjectsMissing,
		title: "Some files cannot be read",
		person: "Some company files are missing or damaged in file storage, so they cannot be opened. " +
			"Restore them from a backup or upload them again.",
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
		kind:  KindObjectsBucketNearCeiling,
		title: "File storage is almost full",
		person: "Uploads will be refused once file storage is full. " +
			"Raise its size, or move the files to a larger store.",
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("the nats object bucket holds %s of its %s reservation",
					bytesHuman(r.ObjectsBucketBytes), bytesHuman(r.ObjectsBucketMaxBytes)),
				r.ObjectsBucketMaxBytes > 0 && float64(r.ObjectsBucketMaxBytes-r.ObjectsBucketBytes) <
					ObjectsHeadroomAlarmFraction*float64(r.ObjectsBucketMaxBytes)
		},
		// The setting comes first, as the full bucket's own refusal says
		// (natsobj.FullRemedy).
		remedy: "Once the bucket is full every upload is refused. Raise " +
			"`store.objects.nats_max_bytes` and restart a data node: its boot raises the " +
			"bucket's reservation where the broker can grant the difference and still keep " +
			"1 GiB for the coordination records, and `objects_bucket_raise_refused` says why " +
			"where it cannot. Or move the files to an S3-compatible bucket with `crewlet " +
			"objects migrate -to s3` (see docs/concepts/object-store.md), or let the collector " +
			"remove the files nothing names any more (Settings, File storage, shows what it holds).",
	},
	{
		kind:  KindObjectsBucketRaiseRefused,
		title: "File storage could not grow",
		person: "File storage could not grow to its configured size, so uploads are held to its old limit. " +
			"Give the message broker more room.",
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("store.objects.nats_max_bytes asks for %s and the nats object "+
					"bucket keeps its %s reservation: %s", bytesHuman(r.ObjectsBucketWanted),
					bytesHuman(r.ObjectsBucketKept), r.ObjectsBucketWhy),
				r.ObjectsBucketWanted > 0
		},
		remedy: "A data node raises the bucket at boot once the broker can grant the difference " +
			"and still keep 1 GiB for the coordination records. Give the broker more room " +
			"(raise `stream.store_max_bytes`, or free the volume holding `stream.store_dir`), " +
			"or lower `store.objects.nats_max_bytes` to what the detail says fits, and restart " +
			"this node. Where the broker states no limit this node can read (an external one " +
			"whose account sets none), raise it there with `nats stream update " +
			"OBJ_crewlet_files --max-bytes=<bytes>`. See docs/concepts/object-store.md.",
	},
	{
		// One alarm for every burning objective, the detail naming each: a burn
		// is one cause (this node failing what it promised) whichever objective
		// shows it first.
		kind:  KindObjectiveBurn,
		title: "Service quality is slipping",
		person: "A service target is failing fast enough to use up its monthly error allowance early. " +
			"Find what is failing now.",
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
		// The threshold is the assumption itself (ADR-0015), handed in with the
		// measure: objstore.AssumedClockSkew is what every rule its doc lists was
		// sized against. At the bound is still inside it.
		//
		// Two clocks, each named: this node's and the register-leading broker
		// member's, each against the fleet's (the median of the data nodes'), and
		// the detail says which stands past the bound, as that machine's clock is
		// the one to fix; measured against the member alone, a node read the
		// member's drift as its own.
		kind:  KindClockSkew,
		title: "A server clock is wrong",
		person: "A server's clock is far off the others', so cleanup may delete files in use " +
			"and timed decisions go wrong. Turn on time sync there.",
		fires: func(r Reading) (string, bool) {
			if r.ClockSkewBound <= 0 {
				return "", false
			}
			// Two clauses, not a list: each has its own verb and the second reads
			// the first's fleet as "it", joined by ", and"; words.And would run
			// them together without the comma.
			fleet := fleetClock(r.ClockSkewNodes)
			var off string
			if magnitude(r.ClockSkew) > r.ClockSkewBound {
				off = fmt.Sprintf("this node's clock runs %s %s %s",
					spoken(r.ClockSkew), clockSide(r.ClockSkew), fleet)
				fleet = "it"
			}
			if magnitude(r.BrokerClockSkew) > r.ClockSkewBound {
				broker := fmt.Sprintf("the clock of the broker member leading the "+
					"position register runs %s %s %s", spoken(r.BrokerClockSkew),
					clockSide(r.BrokerClockSkew), fleet)
				if off == "" {
					off = broker
				} else {
					off = fmt.Sprintf("%s, and %s", off, broker)
				}
			}
			if off == "" {
				return "", false
			}
			return fmt.Sprintf("%s, past the %s every rule that judges one machine's "+
				"instant by another machine's clock assumes", off,
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
			"that moved. Every rule that judges an instant one machine stamped by another " +
			"machine's clock rests on every node and that member keeping within the bound of " +
			"the fleet's clock, and so on any two machines keeping within twice it; " +
			"docs/guides/deployment.md (\"Every machine's clock\") lists each with what a " +
			"clock past the bound does, the object collector deleting an object a write still " +
			"names and Gitlet's code-retention duty sweeping a retired pack a clone is still " +
			"reading among them. The measure is the coordination store's position register, " +
			"read on every heartbeat: each data node's row carries the instant the node stamped on it and the " +
			"instant the broker stored it, so each says how far that node's clock stands from " +
			"the member's, and the median of them is the fleet's clock, off by no more than a " +
			"heartbeat's own work between the two instants. See docs/guides/deployment.md.",
	},
	{
		// The threshold is the node's own budget, measured: the number that
		// decides whether a bitmap is written (one past what is left is killed by
		// the kernel every duty cycle). Silent at the budget, where a bitmap is
		// written.
		kind:  KindCodeMaintenanceRefused,
		title: "Repositories are served more slowly",
		person: "A server lacks the memory to index some repositories, " +
			"so clones are slower but still correct. " +
			"Give that server more memory.",
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
			"which falls to this node because no Gitlet node advertises a budget " +
			"that holds it — give one Gitlet node that memory, and it becomes the " +
			"repository's maintainer: docs/guides/deployment.md, Sizing Gitlet " +
			"nodes). The budget is " +
			"the node's memory (its cgroup's limit where one is set, read again " +
			"every ten seconds) less what it keeps for its git lanes, the " +
			"store, the broker and the engine, on the `code_cache_opened` line. " +
			"Give the node more memory; a container resized in place is held to " +
			"its new limit within ten seconds, without a restart. Or, where " +
			"gitlet.max_transfers is set, give it fewer transfers. What the node " +
			"keeps is fixed when it boots: first the Go runtime's share for the " +
			"executor's pools it runs (97 MiB of transcript reads, 1 805 MiB on " +
			"a node running seats) and the answers in flight (284 MiB), the " +
			"store's page cache (a sixteenth of its " +
			"memory) and its read lane (128 MiB a core), then of " +
			"the memory they leave 224 MiB for every transfer its git " +
			"lane admits, whatever it serves, and 896 MiB for each core its " +
			"memory backs at 2 GiB a core. An unset gitlet.max_transfers is sized " +
			"to that memory, no more than one for every 512 MiB, and leaves the " +
			"background work 256 MiB for each backed core; a figure set by hand " +
			"is kept as written, and leaves it nothing once its transfers' " +
			"224 MiB each reach the memory less the cores' 896 MiB each: at as " +
			"little as 56% of the transfers the memory holds at 224 MiB each, " +
			"where the node has a core for every 2 GiB of it or more. A budget " +
			"of zero clears when gitlet.max_transfers is lowered or unset, or when " +
			"the node is given memory past what it keeps, in place or at a " +
			"boot. A restart alone keeps the same lanes over the same memory, " +
			"and clears it only where gitlet.max_transfers is unset and the memory " +
			"was lowered after the node booted, since the lanes are then sized " +
			"to what it has. A node whose boot logged `code_cache_memory_short` " +
			"keeps more than its memory, so its budget is zero: give it the " +
			"`memory_needed` that line names, 4 GiB or more in all where it runs " +
			"seats beside Gitlet at the default node.max_concurrent. See " +
			"docs/concepts/gitlet.md.",
	},
	{
		// The threshold is the push decide's (ADR-0015): the most live packs a
		// repository may hold, past which a pack-carrying push is refused
		// compaction_behind. The compactor counts repositories it already judged
		// (no Gitlet import here), so the rule fires at any. Raised by the
		// repository's maintainer, the one node that can act on it and whose
		// receiver refuses most such pushes before a committer sees them; held
		// from every pass's listing, so it stands as long as pushes are refused.
		kind:  KindCodeCompactionBehind,
		title: "Pushes to a repository are refused",
		person: "A repository's storage upkeep has fallen behind, " +
			"so pushes to it are refused until it catches up. " +
			"Check the server's log for the cause.",
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
			"rate says by how much. See docs/concepts/gitlet.md.",
	},
	{
		// The threshold is the committer's own bounds (ADR-0015): a repository's
		// queue (count, bytes, how long a proposal may wait) and the node's bytes
		// across every repository, at which it already refused each proposal
		// counted; the rule fires at any count. Only a backlog is counted, as it
		// alone says this committer turns proposals away: an arrival with its
		// attempt spent is upstream, and one that lost every compare-and-set round
		// is a second committer in a handoff; both are counted by
		// crewlet.code.commit.busy and logged, never alarmed. Raised by the node
		// that decides the repository's pushes.
		kind:  KindCodeCommitBusy,
		title: "Pushes are being turned away",
		person: "A server cannot keep up with pushes, so some were turned away and retried. " +
			"Spread pushes over more repositories, or add data servers.",
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
			"five. See docs/concepts/gitlet.md.",
	},
	{
		// The threshold is each queued check's own timeout (ADR-0015), how long
		// its author expects it to take; one table threshold would fire on every
		// long check's ordinary queue and never on a one-minute lint stuck for an
		// hour. The reader judges each attempt, so the rule fires at any count.
		// Raised by every data node from its rows, never a runner: a fleet with no
		// live Gitlet node runs no check and still raises it.
		kind:  KindCodeChecksBacklog,
		title: "Checks are waiting to run",
		person: "Checks have waited past their time limit for a runner, so changes wait to merge. " +
			"Add Gitlet servers or allow more checks at once.",
		fires: func(r Reading) (string, bool) {
			count := fmt.Sprintf("%d", r.CodeChecksBacklog)
			if r.CodeChecksBacklogMore {
				// Past the reading's bound the count is the bound: say "more".
				count = "more than " + count
			}
			return fmt.Sprintf("%s queued check attempt(s) have waited for a runner longer than their "+
					"own timeout — the longest of those counted, %s, for %s",
					count, r.CodeChecksBacklogCheck, spoken(r.CodeChecksBacklogOldest)),
				r.CodeChecksBacklog > 0
		},
		remedy: "A queued attempt waits for a Gitlet node that runs checks to " +
			"claim it, so the backlog is no Gitlet node claiming — the fleet's Gitlet " +
			"nodes gone, stopped or failing their checks' boxes, none setting " +
			"CREWLET_BOX_GIT_URL (`code_checks_not_run`), or none with a container " +
			"backend (`code_checks_container_unconfigured`) — or every Gitlet " +
			"node's containers busy, `gitlet.max_checks` of them a node " +
			"(`crewlet.code.checks.running` pinned there). Read the Gitlet nodes' " +
			"logs and `crewlet.code.checks.claims`: claims refused `claimed` are " +
			"nodes racing for the same attempts, which is no backlog. Against the " +
			"rate attempts end (`crewlet.code.checks.finished`) the queue's " +
			"wait is capacity: more Gitlet nodes, or a higher `gitlet.max_checks` " +
			"on those there are. Nothing is lost while it waits: a change whose required " +
			"check waits cannot merge until it runs, and an attempt cancelled " +
			"meanwhile — its head moved on — leaves the queue. See " +
			"docs/concepts/gitlet.md.",
	},
	{
		// The threshold is the queue's own capacity (ADR-0015, M5-D28), judged by
		// the home's queue worker, so the rule fires at any count; a rate of its
		// own would fire on every deep queue's ordinary day and never on a
		// depth-one queue with hour-long checks. Raised by the home Gitlet node,
		// which builds and times its trains.
		kind:  KindCodeMergeRateOver,
		title: "A merge queue is falling behind",
		person: "A merge queue is asked to land more changes than it can, so waits keep growing. " +
			"Speed up its checks or batch more changes.",
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d merge queue(s) this node is home to were asked to land more changes in "+
					"the last %s than they can land in it — the furthest, %s, %d enqueue(s) against %d at "+
					"its mean round of %s — so their waiting entries grow for as long as the asking lasts",
					r.CodeMergeRateOver, spoken(r.CodeMergeRateWindow), r.CodeMergeRateOverTarget,
					r.CodeMergeRateOverAsked, r.CodeMergeRateOverCapacity, spoken(r.CodeMergeRateOverRound)),
				r.CodeMergeRateOver > 0
		},
		remedy: "A merge queue lands at most depth × batch changes a " +
			"round, a round as long as its candidates' slowest required " +
			"check takes, so a branch asked faster than that falls further " +
			"behind every round: each change waits a round longer for every " +
			"round's worth ahead of it, and once 1 024 changes wait the " +
			"queue refuses the next enqueue `queue_full`. Raise the rule's `landing.depth` (at " +
			"most 8) or `landing.batch` (at most 8) to land more a round, " +
			"make the required checks faster — `crewlet.code.checks.duration` " +
			"says how long each takes — or require fewer of them on this " +
			"branch, and spread the work over more branches or " +
			"repositories, each of which has a queue of its own. Nothing is " +
			"lost meanwhile: every waiting entry lands in its turn or is " +
			"ejected naming why. `crewlet validate` gives the same judgment " +
			"from the configured figures at the longest a check may run. " +
			"See docs/concepts/gitlet.md.",
	},
	{
		// The threshold is two other decisions' (ADR-0015): the most live refs a
		// repository may hold (a create is refused repo_refs) and the ref
		// lifecycle's window (a seat's idle branch is then deleted). Live refs
		// plus those set within the window reaching the bound means refs come fast
		// enough to reach it, and nothing set within it is the lifecycle's to
		// delete. Every data node's reader judges each repository, so the rule
		// fires at any count, before the first create is refused.
		kind:  KindCodeRefsNearLimit,
		title: "A repository is near its branch limit",
		person: "A repository creates branches and tags so fast it will reach its limit, " +
			"and then new ones are refused. " +
			"Delete branches it no longer needs.",
		fires: func(r Reading) (string, bool) {
			window := spokenDays(r.CodeRefsWindow)
			return fmt.Sprintf("%d repository(ies) set refs so fast that they reach the %d-ref bound within "+
					"%s unless refs are deleted (the most: %s, %d live and %d set in the last %s); a create "+
					"at the bound is refused repo_refs",
					r.CodeRefsNearLimit, r.CodeRefsLimit, window, r.CodeRefsNearLimitRepo,
					r.CodeRefsNearLimitLive, r.CodeRefsNearLimitRecent, window),
				r.CodeRefsNearLimit > 0
		},
		remedy: "Delete what the repository no longer needs — branches (`git " +
			"push origin --delete <branch>`, or a seat's `" + toolname.CodeDeleteBranch + "`) " +
			"and tags. A seat's idle branches are deleted by themselves 14 days " +
			"after their last push (the ref lifecycle), so a repository at " +
			"this point is one whose refs are set faster than they go idle, or " +
			"whose refs are persons' branches and tags, which never expire. A " +
			"seat outside the repository's owners is held to half the bound, " +
			"and is refused repo_refs there whatever this alarm says. A " +
			"repository that needs more than 100 000 live refs is past what " +
			"Gitlet serves: split it. Evaluated from this node's copy every " +
			"quarter hour; a node that cannot read it logs " +
			"`code_refs_near_limit_unread`. See docs/concepts/gitlet.md, " +
			"\"The ref lifecycle\".",
	},
	{
		kind:  KindLLMCredentialExpiring,
		title: "A model sign-in expires soon",
		person: "A model's sign-in expires within a month, and agents using it then stop. " +
			"Renew it before it expires.",
		model: func(m ModelCondition, now time.Time) (AlarmEntry, bool) {
			left := m.ExpiresAt.Sub(now)
			return AlarmEntry{Detail: expiryDetail(m, left), Since: m.ExpiresAt.Add(-CredentialNoticeDays * day)},
				!m.ExpiresAt.IsZero() && left <= CredentialNoticeDays*day && left > CredentialWarningDays*day
		},
		remedy: modelRemedy("Rotate the credential before it expires: Settings › Models & Fleets › the " +
			"entry › Rotate tests the new value before it replaces the old one, and the " +
			"models that read it switch to it at once."),
	},
	{
		kind:  KindLLMCredentialExpiresSoon,
		title: "A model sign-in expires this week",
		person: "A model's sign-in expires within days, and agents using it then stop. " +
			"Renew it now.",
		model: func(m ModelCondition, now time.Time) (AlarmEntry, bool) {
			left := m.ExpiresAt.Sub(now)
			return AlarmEntry{Detail: expiryDetail(m, left), Since: m.ExpiresAt.Add(-CredentialWarningDays * day)},
				!m.ExpiresAt.IsZero() && left <= CredentialWarningDays*day && left > 0
		},
		remedy: modelRemedy("Rotate the credential now: Settings › Models & Fleets › the entry › " +
			"Rotate. Once it expires every seat on the model is refused."),
	},
	{
		kind:  KindLLMSignInRefused,
		title: "A model cannot sign in",
		person: "A model's sign-in is refused, expired or missing, " +
			"so agents using it stop unless they have a fallback model. " +
			"Give it a working sign-in.",
		model: func(m ModelCondition, now time.Time) (AlarmEntry, bool) {
			switch {
			case !m.RefusedAt.IsZero():
				detail := fmt.Sprintf("%s's sign-in was refused at %s", m.Key,
					m.RefusedAt.UTC().Format(time.RFC3339))
				if m.RefusedSaid != "" {
					detail += ": " + m.RefusedSaid
				}
				return AlarmEntry{Detail: detail, Since: m.RefusedAt}, true
			case !m.ExpiresAt.IsZero() && !now.Before(m.ExpiresAt):
				return AlarmEntry{Detail: fmt.Sprintf("%s's %s expired on %s", m.Key, credentialName(m),
					m.ExpiresAt.UTC().Format("2 Jan 2006")), Since: m.ExpiresAt}, true
			case m.NoSignIn:
				return AlarmEntry{Detail: fmt.Sprintf("nothing signs %s in on this node, and a seat or "+
					"Ask Crewlet runs on it", m.Key)}, true
			}
			return AlarmEntry{}, false
		},
		remedy: modelRemedy("Give the model a credential that signs in: Settings › Models & Fleets › " +
			"the entry › Rotate for a token or a key, or sign the harness in on the node " +
			"(`crewlet llm login <key>`) for a host login. Every call on it is refused " +
			"until then, and its seats fall back to the next entry in their chain if " +
			"they have one."),
	},
	{
		kind:  KindLLMLimit,
		title: "A model reached its usage limit",
		person: "A model hit its plan's usage limit, so its agents use their fallback until it resets. " +
			"No action is needed unless this is unexpected.",
		model: func(m ModelCondition, now time.Time) (AlarmEntry, bool) {
			out := m.SittingOut
			if out == nil || !now.Before(out.Until) {
				return AlarmEntry{}, false
			}
			waiting := !m.WaitingSince.IsZero() && now.Sub(m.WaitingSince) >= LimitWaitingGrace
			return AlarmEntry{Detail: limitDetail(m.Key, out), Since: out.Since}, !waiting
		},
		remedy: modelRemedy("Nothing, if the plan's limit is expected: the engine tries the " +
			"model again on its own and its seats fall back meanwhile. To end it now, " +
			"Settings › Models & Fleets › the entry › Retry asks the vendor once, and Clear " +
			"stops every node sitting it out."),
	},
	{
		kind:  KindLLMLimitWaiting,
		title: "Agents are waiting on a usage limit",
		person: "Agents with no fallback model are stopped until a model's usage limit resets. " +
			"Add a fallback model or raise the plan.",
		model: func(m ModelCondition, now time.Time) (AlarmEntry, bool) {
			if m.WaitingSince.IsZero() {
				return AlarmEntry{}, false
			}
			waited := now.Sub(m.WaitingSince)
			detail := fmt.Sprintf("a seat has waited %s parked on %s's limit, with no "+
				"entry in its chain to fall back to", spoken(waited), m.Key)
			if m.SittingOut != nil {
				detail += "; " + limitDetail(m.Key, m.SittingOut)
			}
			return AlarmEntry{Detail: detail, Since: m.WaitingSince}, waited >= LimitWaitingGrace
		},
		remedy: modelRemedy("Seats are not working. Add a fallback entry to their chain " +
			"(a model on another sign-in), raise the plan, or wait for the reset the " +
			"vendor named; Settings › Models & Fleets › the entry › Retry asks the vendor once."),
	},
	{
		// A mirror not brought level for longer than its own refresh promise (the
		// interval its home brings it level at, plus an attempt's grace). The home
		// judges each mirror (no Gitlet import here), so the rule fires at any
		// count (ADR-0015); raised by the home that runs the mirror, no other node.
		kind:  KindCodeMirrorFailing,
		title: "A repository mirror is out of date",
		person: "A repository mirror has not synced within its refresh time, so the copy is out of date. " +
			"Check the mirror's address and credential.",
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d mirror(s) of repositories this node is home to have not been brought "+
					"level for longer than their refresh (the longest: %s, %s)",
					r.CodeMirrorFailing, r.CodeMirrorFailingRepo, spoken(r.CodeMirrorFailingFor)),
				r.CodeMirrorFailing > 0
		},
		remedy: "Read the node's `code_mirror_attempt_failed` lines for the " +
			"repository: each names the attempt (`sync` from a pull " +
			"mirror's source, `push` to a push mirror's target) and why it " +
			"failed. A host that " +
			"cannot be reached, or whose address is private while Tier A's " +
			"gitlet.import_allow_private is off, is the network's or the " +
			"configuration's to mend; a credential the other host refuses is " +
			"the variable the mirror's `credential` names, set with `crewlet " +
			"secrets set`; a ref the other host rejects is a protection or a " +
			"hook there refusing a force-push or a deletion the mirror needs; " +
			"a seat named by `as` that no longer reads the repository is " +
			"refused before any push: name another in the repository's " +
			"mirror_to — and a person's seat whose holder does not use " +
			"Gitlet (not given the gitlet permission, unable to act, or " +
			"nobody at all) is refused the same way until an administrator " +
			"gives them Gitlet or mirror_to names another seat. A mirror is " +
			"tried a minute after a failure, longer " +
			"after each one in a row, and at least once an interval, and the " +
			"alarm clears once it is brought level. A mirror with no such " +
			"line has waited behind the node's other attempts: a node runs " +
			"two at once and queues the rest, so one home to more mirrors " +
			"than two attempts at a time bring level within the interval " +
			"falls behind, and more Gitlet nodes spread the repositories' homes. " +
			"See docs/concepts/gitlet.md, " +
			"\"Mirrors and imports\".",
	},
	{
		// A seat's run whose finished job cannot be resumed until an operator
		// clears a condition: the give-up never ends it. The sandbox judged each
		// failure, so the rule fires at any count (ADR-0015). Read fleet-wide off
		// the heartbeat of the node holding the seat, which may evaluate no table.
		kind:  KindExecutorResumeBlocked,
		title: "An agent's finished work is stuck",
		person: "An agent's finished work waits, and the agent takes nothing new, until a server is fixed. " +
			"Fix that server's disk, file limit, keys or version.",
		fires: func(r Reading) (string, bool) {
			return fmt.Sprintf("%d run(s) of seats finished their executor's job but cannot be resumed "+
					"until an operator clears a condition (the longest: %s's, held by %s, on %s, for %s)",
					r.ExecutorResumesBlocked, r.ExecutorResumeBlockedSeat, r.ExecutorResumeBlockedNode,
					r.ExecutorResumeBlockedKind, spoken(r.ExecutorResumeBlockedFor)),
				r.ExecutorResumesBlocked > 0
		},
		remedy: "Read the `sandbox_resume_failed` lines of the node holding the seat, which " +
			"the alarm's line names: each names the condition in its `error`. A disk that is full " +
			"(`disk_full`), mounted read-only (`disk_read_only`) or failing to " +
			"read or write (`disk_failed`) is the host's: make room on it, " +
			"remount it read-write or replace it. A process at its limit of " +
			"open files (`open_files_exhausted`) needs the limit raised. Nodes " +
			"holding different keys (`fleet_keys_differ`) need the same " +
			"`secrets.keys` on every node, and nodes running different builds " +
			"(`fleet_builds_differ`) need the upgrade finished. The run waits " +
			"rather than ends, so the review of its finished job is not thrown " +
			"away: the next attempt after the fix resumes it, and the alarm " +
			"clears. A turn with a wall-clock cap still ends at its cap.",
	},
	{
		// No instance serving, past the restart schedule's ceiling: what the MCP
		// answer calls failing ([MCPServerCondition.Started]), measured from the
		// instance down longest, not the latest failure: a server whose last
		// serving instance just went down was already in an outage, and waiting
		// the grace again would clear `mcp_server_partial` and say nothing while
		// matters got worse. A server nobody is signed in to is
		// `mcp_oauth_needs_sign_in`'s: no restart ends that.
		kind:  KindMCPServerFailing,
		title: "A tool server is down",
		person: "A tool server is not running, so no agent can use its tools. " +
			"Fix the error it reports, then restart it.",
		server: func(s MCPServerCondition, now time.Time) (AlarmEntry, bool) {
			if s.needsSignIn() || s.Started > 0 || s.Failed == 0 || !heldPast(s.DownSince, now, MCPOutageGrace) {
				return AlarmEntry{}, false
			}
			return AlarmEntry{Detail: fmt.Sprintf("no instance of %s is serving, and one has been down "+
				"for %s%s", s.Server, spoken(now.Sub(s.DownSince)), noted(s.Down)),
				Since: s.DownSince, Seat: s.Down.Seat}, true
		},
		remedy: mcpRemedy("No agent is offered the server's tools until an instance serves. " +
			"Fix what the failure names (its address, its command, or the credentials it " +
			"reads); the engine keeps trying every five minutes, and Restart on the " +
			"server's page (`POST /mcp-servers/<name>/retry`) tries every instance now."),
	},
	{
		// Some instances serving and some not, past the same ceiling: a node's
		// environment, one agent's credentials or one child that keeps dying.
		kind:  KindMCPServerPartial,
		title: "A tool server is down for some agents",
		person: "A tool server is not running everywhere, so some agents cannot use its tools. " +
			"Fix the error it reports, then restart it.",
		server: func(s MCPServerCondition, now time.Time) (AlarmEntry, bool) {
			if s.needsSignIn() || s.Started == 0 || s.Failed == 0 || !heldPast(s.DownSince, now, MCPOutageGrace) {
				return AlarmEntry{}, false
			}
			verb := "are"
			if s.Failed == 1 {
				verb = "is"
			}
			return AlarmEntry{Detail: fmt.Sprintf("%d of %d instances of %s %s not serving, and one has "+
				"been down for %s%s", s.Failed, s.Started+s.Failed, s.Server, verb,
				spoken(now.Sub(s.DownSince)), noted(s.Down)), Since: s.DownSince, Seat: s.Down.Seat}, true
		},
		remedy: mcpRemedy("The agents and nodes whose instance serves are not affected. Fix " +
			"what the failure names, usually one node's environment or one agent's " +
			"credentials; Restart on the alarm (`POST /mcp-servers/<name>/retry`, with " +
			"`seat` for one agent's instance) tries it now rather than on the five-minute " +
			"schedule."),
	},
	{
		// An instance held back past a provisioning pass for a credential that
		// pass mints. Not folded into the two above: a wait is no failure, but
		// one that outlived a whole pass is an integration not delivering.
		kind:  KindMCPServerWaitingLong,
		title: "A tool server waits for a credential",
		person: "An agent cannot use a tool server because its integration has not delivered a credential. " +
			"Open that integration to see why.",
		server: func(s MCPServerCondition, now time.Time) (AlarmEntry, bool) {
			if s.Waiting == 0 || !heldPast(s.WaitingSince, now, MCPWaitingGrace) {
				return AlarmEntry{}, false
			}
			return AlarmEntry{Detail: fmt.Sprintf("an instance of %s has waited %s for a credential a "+
				"provisioning pass mints%s", s.Server, spoken(now.Sub(s.WaitingSince)), noted(s.Wait)),
				Since: s.WaitingSince, Seat: s.Wait.Seat}, true
		},
		remedy: mcpRemedy("Open the integration that mints the agent's credential (Settings › " +
			"Integrations): its last pass says what it is waiting for. The instance starts " +
			"once the credential is sealed and reaches its node; until then that agent is " +
			"offered none of the server's tools."),
	},
	{
		// Raised as soon as a node lists the tool: a name is refused by a rule,
		// so no wait would change it.
		kind:  KindMCPToolsRefused,
		title: "Some tools cannot be offered",
		person: "Some of a tool server's tools have names agents cannot use, so no agent sees them. " +
			"Shorten or change the server's tool prefix.",
		server: func(s MCPServerCondition, _ time.Time) (AlarmEntry, bool) {
			if s.RefusedCount == 0 {
				return AlarmEntry{}, false
			}
			return AlarmEntry{Detail: refusedDetail(s)}, true
		},
		remedy: mcpRemedy("The server's Tools tab says why each name is refused. A name too " +
			"long for the harnesses fits with a shorter `tool_prefix`, or none; a name a " +
			"built-in holds needs a prefix; a name with characters a model cannot read is " +
			"the server's own to change. No agent is offered those tools until then."),
	},
	{
		// An OAuth server nobody is signed in to, or whose vendor refused to renew
		// its grant (in the vendor's words). Raised at once: it is a person's
		// step and no wait ends it.
		kind:  KindMCPOAuthNeedsSignIn,
		title: "A tool server needs a sign-in",
		person: "A tool server's sign-in is missing or can no longer be renewed, " +
			"so no agent can use its tools. " +
			"Have an administrator sign in.",
		server: func(s MCPServerCondition, _ time.Time) (AlarmEntry, bool) {
			if !s.needsSignIn() {
				return AlarmEntry{}, false
			}
			if s.SignIn.Failure != "" {
				return AlarmEntry{Detail: fmt.Sprintf("%s's sign-in can no longer be renewed: %s",
					s.Server, s.SignIn.Failure), Since: s.SignIn.FailedAt}, true
			}
			if s.SignIn.Held {
				return AlarmEntry{Detail: fmt.Sprintf("%s is held back for a sign-in%s", s.Server,
					noted(s.Down)), Since: s.DownSince}, true
			}
			return AlarmEntry{Detail: fmt.Sprintf("nobody has signed in to %s, so it is not started",
				s.Server)}, true
		},
		remedy: mcpRemedy("Sign in on the server's page (`POST /mcp-servers/<name>/oauth`), as " +
			"an administrator who can approve the server's access at its vendor. The engine " +
			"renews the sign-in from then on."),
	},
	{
		// A grant running out within [MCPSignInNotice], where its vendor said
		// when: nothing renews the grant itself but a person signing in again.
		kind:  KindMCPOAuthExpiring,
		title: "A tool server sign-in runs out soon",
		person: "A tool server's sign-in is running out, and agents lose its tools when it does. " +
			"Sign in to it again.",
		server: func(s MCPServerCondition, now time.Time) (AlarmEntry, bool) {
			if s.SignIn == nil || !s.SignIn.Held || s.SignIn.Failure != "" || s.SignIn.Expiry.IsZero() ||
				s.SignIn.Expiry.Sub(now) > MCPSignInNotice {
				return AlarmEntry{}, false
			}
			left := s.SignIn.Expiry.Sub(now)
			if left <= 0 {
				return AlarmEntry{Detail: fmt.Sprintf("%s's sign-in ran out at %s", s.Server,
					s.SignIn.Expiry.UTC().Format(time.RFC3339)), Since: s.SignIn.Expiry}, true
			}
			return AlarmEntry{Detail: fmt.Sprintf("%s's sign-in runs out in %s, at %s", s.Server,
				spoken(left), s.SignIn.Expiry.UTC().Format(time.RFC3339)),
				Since: s.SignIn.Expiry.Add(-MCPSignInNotice)}, true
		},
		remedy: mcpRemedy("Sign in again on the server's page (`POST /mcp-servers/<name>/oauth`) " +
			"before then; a new sign-in replaces the grant, and the agents keep the server " +
			"throughout."),
	},
	{
		// A call waiting on a person past the grace: its run is parked and its
		// seat free, so nothing fails, but the work waits on somebody.
		kind:  KindMCPApprovalWaiting,
		title: "A tool call waits for approval",
		person: "An agent's work is paused until someone answers a tool call it asked to make. " +
			"Allow or deny it.",
		server: func(s MCPServerCondition, now time.Time) (AlarmEntry, bool) {
			if s.Approvals == 0 || !heldPast(s.ApprovalSince, now, MCPApprovalGrace) {
				return AlarmEntry{}, false
			}
			detail := fmt.Sprintf("%s's run has waited %s for a person to approve a call to %s on %s",
				s.Approval.Seat, spoken(now.Sub(s.ApprovalSince)), s.Approval.Tool, s.Server)
			if s.Approvals > 1 {
				detail += fmt.Sprintf(", and %d calls to its tools wait in all", s.Approvals)
			}
			return AlarmEntry{Detail: detail, Since: s.ApprovalSince, Seat: s.Approval.Seat}, true
		},
		remedy: mcpRemedy("Answer the call from the Inbox or the server's page " +
			"(`POST /mcp-approvals/<id>`, the ids in `GET /mcp-servers` `approvals`): Allow " +
			"once, Always allow (which sets the tool to run without asking, at a recent " +
			"step-up) or Deny. The run stays parked, its seat free, until somebody does."),
	},
	{
		// A tool held for a change, raised at once: a changed or added tool asks a
		// person before every call, which nobody is told otherwise until a call is
		// held. No span: a definition is changed or it is not.
		kind:  KindMCPToolsChanged,
		title: "A tool changed and needs review",
		person: "A tool server changed or added tools, which now ask a person before every call. " +
			"Review the change, then accept or block it.",
		unread: func(s MCPServerCondition) bool { return s.ChangesUnread },
		server: func(s MCPServerCondition, _ time.Time) (AlarmEntry, bool) {
			if s.ChangesUnread || s.Changes == 0 {
				return AlarmEntry{}, false
			}
			what := "changed since its level was decided"
			if s.Change.Kind == "added" {
				what = "was added since the server's first listing"
			}
			detail := fmt.Sprintf("%s on %s %s, and asks a person before each call until somebody "+
				"accepts the change", s.Change.Tool, s.Server, what)
			if s.Changes > 1 {
				detail += fmt.Sprintf("; %d of its tools wait so in all", s.Changes)
			}
			switch {
			case s.Loosened == 1 && s.Change.Loosened:
				detail += "; it now claims to be less risky than before"
			case s.Loosened > 0:
				detail += fmt.Sprintf("; %d of them now claim to be less risky than before", s.Loosened)
			}
			return AlarmEntry{Detail: detail, Since: s.ChangeSince}, true
		},
		remedy: mcpRemedy("Review each change on the server's page, the definition its level " +
			"was decided on beside the one listed now (`GET /mcp-servers/<name>` `changes`), " +
			"and accept it or keep the tool blocked (`POST /mcp-servers/<name>/changes`, at a " +
			"recent step-up). A change that claims less risk than before is the one to read " +
			"first. Until then each changed tool asks before every call."),
	},
}

// feedOutageDetail is one down feed in the alarm's sentence.
func feedOutageDetail(f FeedOutage) string {
	if f.Failures == 0 {
		return fmt.Sprintf("the %s change feed has held no consumer for %s, and "+
			"its first open has not answered", f.Source, spoken(f.For))
	}
	return fmt.Sprintf("the %s change feed has held no consumer for %s, after "+
		"%d failed attempt(s), the latest: %s", f.Source, spoken(f.For),
		f.Failures, f.Cause)
}

// feedDeadLetterNamed is how many of a log's dead letters the alarm's detail
// names one by one, the soonest to expire first. Five: the detail is one log
// line and one screen row, and the count beside them says how many more and how
// many expire within a day, which a thousand names would bury.
const feedDeadLetterNamed = 5

// feedDeadLetterDetail is feed_dead_lettered's measurement on one log: how many
// entries, how many expire within a day, and the soonest to expire, each by its
// age and the time it has left.
func feedDeadLetterDetail(letters []FeedDeadLetter) string {
	return deadLetterDetail(letters, func(held, soon int) string {
		return fmt.Sprintf("this log's wake feed holds %d dead letter(s), wakes no node could "+
			"derive within the hour their records were handed back, %d of them expiring "+
			"within a day", held, soon)
	})
}

// deadLetterDetail is a dead-letter alarm's measurement: what holds the
// entries, as holds says it given how many there are and how many expire
// within a day, and the soonest to expire, each by its age and the time it has
// left.
func deadLetterDetail(letters []FeedDeadLetter, holds func(held, soon int) string) string {
	letters = slices.Clone(letters)
	slices.SortStableFunc(letters, func(a, b FeedDeadLetter) int {
		return cmp.Compare(a.ExpiresIn, b.ExpiresIn)
	})
	soon := 0
	for _, l := range letters {
		if l.ExpiresIn <= day {
			soon++
		}
	}
	named := make([]string, 0, min(len(letters), feedDeadLetterNamed))
	for _, l := range letters[:cap(named)] {
		left := "due to expire now"
		if l.ExpiresIn > 0 {
			left = "expiring in " + spokenSpan(l.ExpiresIn)
		}
		named = append(named, fmt.Sprintf("one kept %s ago, %s", spokenSpan(l.Age), left))
	}
	detail := holds(len(letters), soon) + ": " + strings.Join(named, "; ")
	if more := len(letters) - len(named); more > 0 {
		detail += fmt.Sprintf("; and %d more", more)
	}
	return detail
}

// spokenSpan is a span that may run to days in days and hours — a dead letter
// thirty days from expiring reads "29d 23h", where spoken's "719h 0m" makes the
// reader divide — and any shorter one in spoken's form.
func spokenSpan(d time.Duration) string {
	if d < 2*day {
		return spoken(d)
	}
	hours := int64(d.Round(time.Hour) / time.Hour)
	if hours%24 == 0 {
		return fmt.Sprintf("%dd", hours/24)
	}
	return fmt.Sprintf("%dd %dh", hours/24, hours%24)
}

// mcpRemedy is an MCP alarm's remedy, ending with where a person reads the
// server from.
func mcpRemedy(what string) string {
	return what + " The server's page under Settings › MCP & tools " +
		"(`GET /mcp-servers/<name>`) lists each instance with what it last said."
}

// heldPast reports whether a condition that began at since has held for at
// least grace by now. A zero since is a condition nobody dated, which no
// threshold measured in time can judge, so it is never past one.
func heldPast(since, now time.Time, grace time.Duration) bool {
	return !since.IsZero() && now.Sub(since) >= grace
}

// noted is the instance a server's condition names, as the alarm's sentence
// ends: whose it is, the node it is on, its failure class and what it said,
// each left out where the heartbeat carried none.
func noted(n MCPInstanceNote) string {
	var about []string
	if n.Seat != "" {
		about = append(about, n.Seat+"'s")
	}
	if n.Node != "" {
		about = append(about, "on "+n.Node)
	}
	if n.Class != "" {
		about = append(about, n.Class)
	}
	out := ""
	if len(about) > 0 {
		out = " (" + strings.Join(about, ", ") + ")"
	}
	if n.Said != "" {
		out += ": " + n.Said
	}
	return out
}

// refusedDetail names a server's tools that cannot be offered under their
// names, and how many more there are than the heartbeat lists.
func refusedDetail(s MCPServerCondition) string {
	named := make([]words.Said, 0, len(s.Refused)+1)
	for _, tool := range s.Refused {
		named = append(named, words.Said("`"+tool+"`"))
	}
	if more := s.RefusedCount - len(s.Refused); more > 0 {
		named = append(named, words.Said(fmt.Sprintf("%d more", more)))
	}
	detail := fmt.Sprintf("%d of %s's tools cannot be offered under their names", s.RefusedCount, s.Server)
	if s.RefusedCount == 1 {
		detail = fmt.Sprintf("1 of %s's tools cannot be offered under its name", s.Server)
	}
	if len(named) > 0 {
		detail += ": " + words.And(named)
	}
	return detail
}

// day is the unit the credential thresholds are written in.
const day = 24 * time.Hour

// modelRemedy is a model alarm's remedy, ending with the two places a person
// reads the entry from: the page and the doctor.
func modelRemedy(what string) string {
	return what + " Settings › Models & Fleets names the entry, and `crewlet llm doctor <key>` " +
		"checks it end to end on a node."
}

// credentialName is how a model alarm names an entry's credential: its
// variable, or "credential" where it reads none.
func credentialName(m ModelCondition) string {
	if m.Variable != "" {
		return m.Variable
	}
	return "credential"
}

// expiryDetail says when an entry's credential expires and how far away that
// is, in whole days rounded up, so seven days and an hour reads "8 days".
func expiryDetail(m ModelCondition, left time.Duration) string {
	days := int64((left + day - 1) / day)
	unit := "days"
	if days == 1 {
		unit = "day"
	}
	return fmt.Sprintf("%s's %s expires on %s (%d %s)", m.Key, credentialName(m),
		m.ExpiresAt.UTC().Format("2 Jan 2006"), days, unit)
}

// limitDetail says which limit an entry sits out and until when.
func limitDetail(key string, out *coord.ModelSitOut) string {
	window := out.Window
	if window == "" || window == "unknown" {
		window = "plan"
	}
	detail := fmt.Sprintf("%s sits out its %s limit until %s", key, window,
		out.Until.UTC().Format(time.RFC3339))
	if !out.ResetsAt.IsZero() {
		detail += fmt.Sprintf(" (the vendor said it resets at %s)", out.ResetsAt.UTC().Format(time.RFC3339))
	}
	if out.Said != "" {
		detail += ": " + out.Said
	}
	return detail
}

// spokenDays is a window of whole days in days — the ref lifecycle's fourteen
// rather than spoken's "336h" — and any other in spoken's form.
func spokenDays(d time.Duration) string {
	if d <= 0 || d%day != 0 {
		return spoken(d)
	}
	if d == day {
		return "1 day"
	}
	return fmt.Sprintf("%d days", d/day)
}

// volumeLow judges the store's tightest volume against factor times its store
// plus the log a copy holds back there: the raise's factor, or the wider one a
// raised alarm stays up within ([VolumeClearFactor]).
func volumeLow(factor float64) func(Reading) (string, bool) {
	return func(r Reading) (string, bool) {
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
		clears := VolumeClearFactor*float64(r.StoreBytes) + float64(r.CopyHeldBytes)
		detail += fmt.Sprintf("; the alarm clears once %s is free", bytesHuman(int64(math.Ceil(clears))))
		return detail, r.StoreBytes > 0 && float64(r.FreeBytes) <
			factor*float64(r.StoreBytes)+float64(r.CopyHeldBytes)
	}
}

// Evaluate reports every alarm currently true, in the table's own order.
//
// Pure, over a reading gathered once: the same evaluation feeds the gauge, the
// log line and the screen, so they cannot disagree.
func Evaluate(r Reading) []Alarm {
	var out []Alarm
	for _, rule := range table {
		if rule.perEntry() {
			if a, firing := rule.evaluateEntries(r); firing {
				out = append(out, a)
			}
			continue
		}
		judge := rule.fires
		if rule.stays != nil && slices.Contains(r.Up, rule.kind) {
			judge = rule.stays
		}
		if detail, firing := judge(r); firing {
			out = append(out, rule.alarm(detail))
		}
	}
	return out
}

// evaluateEntries is a per-entry rule over every row it judges, model entries
// or MCP servers: one alarm listing the entries it holds for, ordered by key,
// leading with the longest-standing entry's detail (the earliest Since, ties
// to the lower key). A reading that gathered no rows of the rule's kind
// (a zero instant) judges none.
func (rule rule) evaluateEntries(r Reading) (Alarm, bool) {
	var entries []AlarmEntry
	keep := func(key string, e AlarmEntry, firing bool) {
		if firing {
			e.Key = key
			entries = append(entries, e)
		}
	}
	switch {
	case rule.model != nil && !r.ModelsAt.IsZero():
		for _, m := range r.Models {
			e, firing := rule.model(m, r.ModelsAt)
			keep(m.Key, e, firing)
		}
	case rule.server != nil && !r.MCPServersAt.IsZero():
		for _, s := range r.MCPServers {
			e, firing := rule.server(s, r.MCPServersAt)
			keep(s.Server, e, firing)
		}
	}
	if len(entries) == 0 {
		return Alarm{}, false
	}
	slices.SortFunc(entries, func(a, b AlarmEntry) int { return strings.Compare(a.Key, b.Key) })
	lead := entries[0]
	for _, e := range entries[1:] {
		if e.Since.Before(lead.Since) {
			lead = e
		}
	}
	alarm := rule.alarm(lead.Detail)
	alarm.Entries = entries
	return alarm, true
}

// alarm is the rule's alarm with its detail, in the table's words.
func (rule rule) alarm(detail string) Alarm {
	return Alarm{Kind: rule.kind, Title: rule.title, Person: rule.person, Detail: detail, Remedy: rule.remedy}
}

// AlarmRef names the instances of a per-entry kind an evaluation could not
// judge: one entry's, or every entry's where Entry is empty.
type AlarmRef struct {
	Kind  Kind
	Entry string
}

// Unjudged is every per-entry instance r cannot judge, which [Tracker.Observe]
// keeps as it stands rather than clearing: a kind whose rows r did not gather
// at all (a zero instant), and a server row a rule cannot read
// ([MCPServerCondition.ChangesUnread]). A source unread at a boot or a stop is
// not a condition cleared, so a standing alarm is not raised again after it.
func Unjudged(r Reading) []AlarmRef {
	var out []AlarmRef
	for _, rule := range table {
		switch {
		case rule.model != nil && r.ModelsAt.IsZero(),
			rule.server != nil && r.MCPServersAt.IsZero():
			out = append(out, AlarmRef{Kind: rule.kind})
		case rule.server != nil && rule.unread != nil:
			for _, s := range r.MCPServers {
				if rule.unread(s) {
					out = append(out, AlarmRef{Kind: rule.kind, Entry: s.Server})
				}
			}
		}
	}
	return out
}

// Frac is a measured fraction, for the two Reading fields whose zero value is
// a real and alarming measurement rather than an absent one.
func Frac(v float64) *float64 { return &v }

// PerDay is a measured daily byte count, for [Reading.LogBytesPerDay], whose zero
// is a real, benign measurement.
func PerDay(v uint64) *uint64 { return &v }

// PerLogKinds is every alarm evaluated once per log rather than once per node,
// in table order: these list the logs they stand on ([Alarm.Domains]) and write
// a pair of log lines per log ([Tracker]), counting once on the health envelope
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

// PerEntryKinds is the kinds evaluated once per configured entry, a model
// entry or an MCP server, in table order.
func PerEntryKinds() []Kind {
	return kindsWhere(rule.perEntry)
}

// PerModelKinds is the per-entry kinds about one model entry, and
// PerServerKinds the ones about one MCP server, each in table order: the two
// halves of [PerEntryKinds], for the surface that reads one of them.
func PerModelKinds() []Kind {
	return kindsWhere(func(r rule) bool { return r.model != nil })
}

// PerServerKinds is the per-entry kinds about one MCP server; see
// [PerModelKinds].
func PerServerKinds() []Kind {
	return kindsWhere(func(r rule) bool { return r.server != nil })
}

// kindsWhere is the table's kinds whose rule holds, in table order.
func kindsWhere(holds func(rule) bool) []Kind {
	var out []Kind
	for _, rule := range table {
		if holds(rule) {
			out = append(out, rule.kind)
		}
	}
	return out
}

// Age is a measured age, for [Reading.BackupAge], whose zero is a real,
// reassuring measurement.
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

// PersonWords is kind's [Alarm.Title] and [Alarm.Person], and false for a kind
// this build does not raise: for a surface that names an alarm by its kind
// alone, such as a node's heartbeat marks.
func PersonWords(kind Kind) (title, person string, ok bool) {
	for _, rule := range table {
		if rule.kind == kind {
			return rule.title, rule.person, true
		}
	}
	return "", "", false
}

// Spoken is a duration as a person reads it ("45s", "2m 5s", "22h 33m"), for a
// sentence outside this package that states one. It is the dashboard's own
// spelling (`fmtDuration`, lib/format.ts), so a figure and the sentence beside
// it agree. Never a bare count of seconds.
func Spoken(d time.Duration) string { return spoken(d) }

// SpokenSpan is a span that may run to days in days and hours ("29d 23h") and
// any shorter one as [Spoken] says it: a dead letter's age and time left, so
// the alarm and `crewlet feeds dead-letters` say one entry alike.
func SpokenSpan(d time.Duration) string { return spokenSpan(d) }

// SpokenWait is a wait a person is told to sit out ("retry in about 22h 33m"):
// the largest two units, rounded up at every magnitude, so a reader who comes
// back when told is not refused again ([Spoken] rounds to nearest, which sends
// "4m" said of four and a half back early). The dashboard says a wait by the
// same rule (`fmtWait`, dashboard/src/lib/format.ts). Never under a second.
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

// spoken is a duration as an operator reads it, not `time.Duration.String`
// ("85h58m0s", hundredths nobody measured to): the dashboard's convention
// (`fmtDuration`, lib/format.ts), two fields at most and none that is zero:
// "340ms", "1.2s", "2m", "1m 2s", "2h 3m", "24h".
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
	// Each magnitude rounds at its own grain and carries up: 999.6ms is "1s",
	// not "1000ms".
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
// being refused for a fault but cannot say for how long. The condition is time
// (longer than one reconcile interval; one refusal in an election is no fault),
// and a recorder holds a count, not an age, so such a surface reports one
// interval past the threshold. Zero for an unknown age is the three-valued
// mistake and silences the alarm on the fault it exists for.
const RefusalAlarmFloor = 2 * coord.ReconcileInterval
