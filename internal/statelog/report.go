package statelog

import (
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// The operator's answer is ONE value: `crewlet retention status` and GET
// /worklet/retention serve the same bytes, encoded once by the node that
// assembled them; the CLI renders them and exits non-zero from their alarms,
// re-deriving neither. A surface computing its own verdict would be a second
// retention gate on a machine that cannot see the register, and when the two
// disagreed nobody could tell which one the engine acted on.
//
// It is per fleet with the domain as a column: a row per domain, never a second
// command, route or `?domain=`. A term that does not apply to a domain is ABSENT,
// not `0`, which would claim e.g. a wake feed that scanned nothing. The per-node
// snapshot inventory is `crewlet retention snapshots`, a disk question; here is
// only what the register knows, the newest artefact per node.

// ReportVersion is the shape this build writes. Evolution is additive, as the
// event envelope's is: mid-upgrade, an older CLI decodes a newer node's answer
// into its own struct and ignores unknown fields. The version is for a field
// that changes MEANING, which an added field cannot express.
const ReportVersion = 1

// Report is the fleet's retention answer.
type Report struct {
	V int `json:"v"`

	// NodeID is who assembled it: the replica block, the alarms and every
	// "this node" figure are facts about that node, not the fleet.
	NodeID string    `json:"node_id"`
	At     time.Time `json:"at"`

	// ReadLevel is the level this answer was served at, which keeps it inside
	// the read-level contract. No surface chooses it ([SurfaceReplication]):
	// `stale` while the lag is a number, `consistent_prefix` when it is not,
	// since `stale` claims an AGE. This document is wanted when the broker or
	// coordination may be down, so the weaker answer is delivered, by its name.
	ReadLevel ReadLevel `json:"read_level"`

	// BackupOwner is `retention.backup_owner`, empty when unset; carried
	// here because the backup term's remedy names it, and a JSON reader sees
	// only the report.
	BackupOwner string `json:"backup_owner,omitempty"`

	Domains []DomainReport `json:"domains"`
	Nodes   []NodeReport   `json:"nodes"`

	// RegisterReadable says the node block is the whole fleet. Without it an
	// empty block is either "no nodes" (impossible) or "coordination could
	// not be listed" (the outage this command is run in).
	RegisterReadable bool `json:"register_readable"`

	Snapshots []SnapshotReport `json:"snapshots"`
	Replica   ReplicaReport    `json:"replica"`

	// Alarms is every condition currently true on this node, evaluated once
	// here; an empty list, never null. The CLI's exit code derives from it.
	Alarms []Alarm `json:"alarms"`

	// Unjudged is the per-entry instances this evaluation could not judge
	// ([Unjudged]), which the node's tracker keeps standing.
	Unjudged []AlarmRef `json:"-"`

	// Maintenance is every capacity operation holding the fleet, oldest
	// first; absent, not zeroed, when none is, since a zeroed block claims
	// an operation in phase "" with nobody outstanding. Maintenance stops
	// every publisher on every node and shows on no other screen
	// (`maintenance_open` alarms on it). Every one, because a window is per
	// log and two can be open at once, each waiting on its own nodes.
	Maintenance []MaintenanceReport `json:"maintenance,omitempty"`

	// MaintenanceUnread is every log whose capacity window this node
	// couldn't read, absent when it read every one: an unread window is not
	// no window, and the coordination outage that fails the read is when
	// somebody opens this. It is this node's read; a data node's boot won't
	// start in service without the same read either.
	MaintenanceUnread []string `json:"maintenance_unread,omitempty"`
}

// MaintenanceReport is one open capacity operation, as a reader sees it.
type MaintenanceReport struct {
	// Stream is which log's configuration this is about, and OperationID
	// the identity every attempt of it reuses.
	Stream      string `json:"stream"`
	OperationID string `json:"operation_id"`

	// Phase is where the operation stands and Attempt which write-then-
	// seal cycle it is on: a retry is a second attempt in the same phase.
	Phase   string `json:"phase"`
	Attempt int    `json:"attempt"`

	// TargetMaxBytes is the ceiling being written and OriginalMaxBytes
	// what it was, so a reader sees the direction without the stream.
	TargetMaxBytes   uint64 `json:"target_max_bytes"`
	OriginalMaxBytes uint64 `json:"original_max_bytes"`

	// Since is when the exclusion was taken, By which operator ran the
	// verb, and OperatorID the credential they ran it through (their own
	// gesture or their token's).
	Since      time.Time `json:"since"`
	By         string    `json:"by,omitempty"`
	OperatorID string    `json:"operator_id,omitempty"`

	// ParticipantsMissing names the nodes the window's next step waits for:
	// at `opened`, those with no maintenance-mode restart recorded
	// ([BaselineMissing]); from the baseline to the seal, those whose
	// acknowledgement the seal awaits ([SealOutstanding]); once sealed, none.
	// Empty is not unknown: nobody outstanding means it waits on its operator.
	ParticipantsMissing []string `json:"participants_missing,omitempty"`

	// AcksUnread says the acknowledgements ParticipantsMissing comes from
	// couldn't be read, and AdmissionsUnread the same of AdmissionsBlocking's
	// admissions. That list is then absent, never empty: one formed from
	// nothing names every participant as owing a restart, or no admission as
	// holding the window, as a fact.
	AcksUnread       bool `json:"acks_unread,omitempty"`
	AdmissionsUnread bool `json:"admissions_unread,omitempty"`

	// AdmissionsBlocking names the nodes holding an admission that keeps a
	// window at `opened` from sending anything ([AdmissionBlocks]); empty
	// past `opened`. It is what holds a window no participant owes: a node
	// that stopped without giving its admission back.
	AdmissionsBlocking []string `json:"admissions_blocking,omitempty"`

	// Blocked is why the operation cannot proceed without a person, empty
	// while it can: a code for scripts. BlockedDetail ([BlockedDetail]) is
	// the same for a person, who blocked it and what clears it, and every
	// surface shows it.
	Blocked       coord.MaintenanceBlock `json:"blocked,omitempty"`
	BlockedDetail string                 `json:"blocked_detail,omitempty"`

	// AbandonedBy is who abandoned the operation: a window blocked by its
	// abandonment carries out that person's decision, and the block names
	// nobody.
	AbandonedBy coord.MaintenanceParty `json:"abandoned_by,omitzero"`
}

// DomainReport is one registered domain's row.
type DomainReport struct {
	Domain     string         `json:"domain"`
	Stream     string         `json:"stream"`
	Generation uint32         `json:"generation"`
	Replay     ReplayProtocol `json:"replay"`

	// FirstSeq and LastSeq are the stream's window, Bytes and MaxBytes its
	// ceiling. Zero MaxBytes means the broker could not be asked, so
	// HeadroomFraction is a pointer: unknown headroom rendered as zero would
	// fire the one alarm an operator cannot ignore. ReserveBytes is the top
	// of MaxBytes kept for gate records on a log that keeps a reserve
	// ([Reservation.Bytes]); HeadroomFraction is what is left of the rest,
	// where ordinary writes are refused `log_full`.
	FirstSeq         uint64   `json:"first_seq"`
	LastSeq          uint64   `json:"last_seq"`
	Bytes            uint64   `json:"bytes"`
	MaxBytes         uint64   `json:"max_bytes,omitempty"`
	ReserveBytes     uint64   `json:"reserve_bytes,omitempty"`
	HeadroomFraction *float64 `json:"headroom_fraction,omitempty"`

	// BytesPerDay is the log's intake over the trailing day as this node
	// last measured it, the rate `log_ceiling_short` holds the ceiling
	// against. Absent, not zero (an idle log), when unmeasured: every
	// compacted log, every log in its first two days (whose trailing day
	// would hold its import), and every node before its first trim tick.
	BytesPerDay *uint64 `json:"bytes_per_day,omitempty"`

	// TrimFloor is the fleet's published floor, never moving down: anything
	// below may already be removed, so it is where a joining node's replay
	// can start. TrimTo is what THIS tick concluded may be removed (zero
	// while blocked, below the floor whenever the lowest counted node is): it
	// says whether the gate moves. Both mean something only where
	// TrimFloorState is published; a zero is otherwise no conclusion yet, or
	// an unreadable register.
	TrimFloor uint64 `json:"trim_floor"`
	TrimTo    uint64 `json:"trim_to"`

	// TrimFloorState says whether TrimFloor, TrimTo, Terms and BlockedBy
	// are a conclusion at all ([TrimFloorState]). Always written: its zero
	// value is none of the three states.
	TrimFloorState TrimFloorState `json:"trim_floor_state"`

	// Terms is the last tick's six terms, and an empty list, never null,
	// where the trim has concluded nothing about this generation: a null
	// crashes both retention screens, read right after a reanchor.
	Terms        []TermReport `json:"terms"`
	BlockedBy    TermName     `json:"blocked_by,omitempty"`
	BlockedSince time.Time    `json:"blocked_since,omitzero"`

	// NotReady is why THIS node refuses every read of this domain (the
	// [Health.Refusal] its readiness reads), nil while it serves them.
	// WritesRefused is why it refuses the domain's writes while serving its
	// reads: a peer's rows hold records the log lost ([ErrLogTruncated]).
	// Both are this node's facts and carry the refusal's own sentence, which
	// the guide tells an operator the status prints.
	NotReady      *DomainRefusal `json:"not_ready,omitempty"`
	WritesRefused *DomainRefusal `json:"writes_refused,omitempty"`

	// Prose is the sentence a blocked trim leads with: the answer to the
	// question this command is run for.
	Prose string `json:"prose,omitempty"`

	// SnapshotBlockedBy is the snapshot loop's skip reason on this node,
	// apart from BlockedBy: stopped snapshotting has its own remedy and is
	// silent until a node tries to join.
	SnapshotBlockedBy SkipReason `json:"snapshot_blocked_by,omitempty"`

	// EvictionsUnreadable says this node could not read its rows' evictions
	// for this log (the store read failed, or the estate was closed for an
	// adoption's rename or a shutdown); set only on a log that claims
	// identity, the only kind carrying evictions, and absent when read. The
	// node block cannot say it: an unread log keeps every node counted, as
	// the trim does, but also shows every node as not evicted, and a screen
	// taking that for a readmission releases an eviction and re-dates it.
	EvictionsUnreadable bool `json:"evictions_unreadable,omitempty"`

	// BackupAt is when the backup this log's trim counts was taken: the
	// newest verified point the backup policy trusts that covers this log's
	// stream (a node's copy under `engine`, the operator's acknowledgement
	// of this log under `operator`). Absent where none does, the state the
	// backup term blocks on and the backup-age alarm names. Per log: an
	// acknowledgement names one stream, and a node's copy covers only the
	// logs it ran.
	BackupAt time.Time `json:"backup_at,omitzero"`
}

// TrimFloorState says what a domain's trim floor, conclusion and terms are.
//
// A state, not zeros: the floor is filled only from a row published at the
// domain's own generation, so after a reanchor, until the trim's first tick on
// the adopted stream, there is none, and zeros read as "advancing". A floor
// register nobody could read is unreadable, never empty.
type TrimFloorState string

const (
	// TrimFloorPublished — the fleet has published a floor at this
	// domain's generation, and the floor, the conclusion, the terms and
	// the blocking term are that tick's.
	TrimFloorPublished TrimFloorState = "published"

	// TrimFloorNoneAtGeneration — the trim has concluded nothing about
	// this generation of the log yet: a fresh fleet before its first tick,
	// or any fleet just after a reanchor. Not blocked and not advancing.
	TrimFloorNoneAtGeneration TrimFloorState = "none_at_generation"

	// TrimFloorUnreadable — the floor register could not be read, so
	// nothing is known about the trim here.
	TrimFloorUnreadable TrimFloorState = "unreadable"
)

// TrimFloorStates is every [TrimFloorState] this build names.
func TrimFloorStates() []TrimFloorState {
	return []TrimFloorState{TrimFloorPublished, TrimFloorNoneAtGeneration, TrimFloorUnreadable}
}

// Valid reports whether s is a state this build names.
func (s TrimFloorState) Valid() bool { return slices.Contains(TrimFloorStates(), s) }

// IdentityCause names which finding put a domain's log out of step with this
// node's rows, behind one `wrong_stream`: all four refuse the same reads and
// writes, but each has its own remedy, and the guide sends an operator to the
// status for it. A stream rebuilt under this node, a broker restored from a
// copy older than its rows, that restore written past them, or a peer
// re-anchoring past them (remedied by this node's adoption, not a reanchor).
type IdentityCause string

const (
	// CauseRecreated — the live stream is not the one this node's rows are
	// keyed to: deleted and rebuilt under the same name ([ErrStreamRecreated]).
	CauseRecreated IdentityCause = "recreated"

	// CauseAheadOfLog — this node's checkpoint is past the log's end: the
	// broker was restored from a copy older than its rows ([ErrAheadOfLog]).
	CauseAheadOfLog IdentityCause = "ahead_of_log"

	// CauseLogDiverged — the log holds another record at this node's
	// checkpoint: the same restore, since written past its rows
	// ([ErrLogDiverged]).
	CauseLogDiverged IdentityCause = "log_diverged"

	// CauseGenerationPassed — a peer re-anchored the log past this node's
	// generation ([ErrGenerationPassed]).
	CauseGenerationPassed IdentityCause = "generation_passed"
)

// IdentityCauses is every [IdentityCause] this build names.
func IdentityCauses() []IdentityCause {
	return []IdentityCause{CauseRecreated, CauseAheadOfLog, CauseLogDiverged,
		CauseGenerationPassed}
}

// Valid reports whether c is a cause this build names.
func (c IdentityCause) Valid() bool { return slices.Contains(IdentityCauses(), c) }

// DomainRefusal is why this node is refusing a domain right now.
type DomainRefusal struct {
	// Code is the refusal's word: a [ReadRefusal] on a report's NotReady,
	// and a [Reason] on its WritesRefused (`log_truncated`, today).
	Code string `json:"code"`

	// Causes names each identity finding that holds behind a
	// `wrong_stream` — more than one can at once — and is empty for every
	// other code.
	Causes []IdentityCause `json:"causes,omitempty"`

	// Detail is the sentence the refusal carries everywhere (the applier's
	// stop, a read's or a write's refusal), naming the instants, sequences
	// and peer it is about.
	Detail string `json:"detail"`
}

// NotReady is this domain's read refusal as a report renders it, or nil while
// it serves reads. identity is the applier's answer about the log's identity
// ([Runner.StreamIdentity]), so a `wrong_stream` reads here as it did where a
// read or write met it; a checkpoint past an end the applier has not observed
// yet is decided from this health's own end, as [Health.Refusal] does.
func (h Health) NotReady(now time.Time, stream string, identity error) *DomainRefusal {
	code := h.Refusal(now)
	if code == "" {
		return nil
	}
	out := &DomainRefusal{Code: string(code)}
	switch code {
	case RefuseEvicted:
		out.Detail = "this node is evicted: every record it publishes is dropped on " +
			"every node, and it serves nothing until it is readmitted"
	case RefuseFloorUnknown:
		out.Detail = "the published trim floor or the log's own ends could not be " +
			"read, and a floor nobody could read is not one these rows are known to " +
			"be above; it clears once coordination and the broker answer"
	case RefuseBelowFloor:
		out.Detail = fmt.Sprintf("the record this node's applier needs next is gone "+
			"from %s (its checkpoint is %d): it adopts a peer's snapshot, and another "+
			"node serves meanwhile", stream, h.Position.Seq)
	case RefuseWrongStream:
		if h.StreamRecreated {
			out.Causes = append(out.Causes, CauseRecreated)
		}
		if h.AheadOfLog() {
			out.Causes = append(out.Causes, CauseAheadOfLog)
		}
		if h.LogDiverged {
			out.Causes = append(out.Causes, CauseLogDiverged)
		}
		if h.GenerationPassed {
			out.Causes = append(out.Causes, CauseGenerationPassed)
		}
		switch {
		case identity != nil:
			out.Detail = identity.Error()
		case h.AheadOfLog():
			out.Detail = aheadOfLog{at: h.Position, last: *h.LastSeq}.err(stream).Error()
		default:
			out.Detail = "the log under this domain's name is not the one this " +
				"node's rows were derived from"
		}
	case RefuseStalled:
		out.Detail = h.Err
		if out.Detail == "" {
			out.Detail = fmt.Sprintf("its applied prefix has not moved for %s", StallGrace)
		}
	case RefuseBehind:
		out.Detail = fmt.Sprintf("it is replaying up to the published trim floor "+
			"from its checkpoint %d. The log still holds what it lacks, and this "+
			"clears on its own", h.Position.Seq)
	}
	return out
}

// TermState is a term's third value made explicit.
type TermState string

const (
	// TermKnown — the term was read and permits removal up to its Seq.
	TermKnown TermState = "ok"

	// TermUnreadable — the term could not be evaluated, which BLOCKS;
	// rendered apart from a term permitting zero because it is a thing to
	// fix, not one to wait for.
	TermUnreadable TermState = "unknown"

	// TermAbsent — this domain does not have the term: `n/a`, never `0`.
	TermAbsent TermState = "n/a"

	// TermUnbounded — the term was read and binds nothing (no hold pins the
	// log, or a solo fleet takes no snapshots). Not TermKnown, because its
	// Seq is [SeqUnbounded], the trim minimum's identity and no position;
	// not TermAbsent, because the domain has the term and it may bind later.
	TermUnbounded TermState = "unbounded"
)

// TermReport is one of the six terms, with the remedy for it.
type TermReport struct {
	Name  TermName  `json:"name"`
	State TermState `json:"state"`

	// Seq is what the term permits, meaningless unless State is TermKnown.
	// Always written, never `omitempty`: zero is a young fleet's binding
	// term, the one worth reading, and omitted it is indistinguishable from
	// a term with no sequence.
	Seq uint64 `json:"seq"`

	// Detail is what was read, named: which node, which holder, how old.
	Detail string `json:"detail,omitempty"`

	// Remedy is what to do about it, on every term: an operator watches a
	// term approach, and a remedy shown only once it blocks is too late.
	Remedy string `json:"remedy"`
}

// NodeReport is one node's row: what the fleet counts, and what it has done.
type NodeReport struct {
	NodeID string `json:"node_id"`

	// Counted says the trim waits for this node, Live that it holds a
	// presence lease now. They are independent: counted-and-not-live is a
	// node pinning the log, live-and-not-counted one inside its eviction
	// fence window, still writing.
	Counted bool `json:"counted"`
	Live    bool `json:"live"`

	// At is when it last wrote its row, zero for a live node that has never
	// reported: rendered as counted with no position yet, not position zero,
	// since the two block the trim for very different times.
	At time.Time `json:"at,omitzero"`

	// Domains is what it has done, per domain. A domain absent from the
	// map is one this node has not reported on.
	Domains map[string]NodeDomainReport `json:"domains,omitempty"`

	// Evicted is its tombstone, when it has one.
	Evicted *EvictionReport `json:"evicted,omitempty"`
}

// NodeDomainReport is one node's progress in one domain.
type NodeDomainReport struct {
	Generation     uint32 `json:"generation"`
	Seq            uint64 `json:"seq"`
	AppliedThrough uint64 `json:"applied_through"`

	// GenerationState is this position's generation against the domain's
	// ([GenerationState]); Seq and AppliedThrough compare with the domain's
	// sequences only where it is `current`.
	GenerationState GenerationState `json:"generation_state"`

	// Lag is this node's distance from the stream's last sequence: nil when
	// the stream could not be read (as in [Health], an unknown lag as zero
	// reads as caught up), and nil for a position from another generation,
	// whose sequence is in another number space.
	Lag *uint64 `json:"lag,omitempty"`

	Deferred int `json:"deferred,omitempty"`

	// LogDiverged is the node's own report that the log holds another
	// record at its checkpoint than the one it consumed; nothing else on its
	// row shows it, since its position is not past the log's end. Every
	// other node on the log refuses its writes while it is set.
	LogDiverged bool `json:"log_diverged,omitempty"`

	// StreamCreatedAt is the creation instant of the stream the node's rows
	// are keyed to, and CheckpointStoredAt the broker's instant for its
	// checkpoint's record, absent where it names none: what a reanchor
	// weighs to tell a peer on the lost stream from one on the live one, and
	// one holding the log's history from one holding history the log lost.
	StreamCreatedAt    time.Time `json:"stream_created_at,omitzero"`
	CheckpointStoredAt time.Time `json:"checkpoint_stored_at,omitzero"`
}

// GenerationState is a node's position generation against its domain's: a
// named state, not numbers for the reader to compare, since subtracting
// sequences across generations shows a node on a sequence space the log has
// left as caught up.
type GenerationState string

const (
	// GenerationCurrent — the node's position is at the domain's
	// generation, so its sequences compare with the log's.
	GenerationCurrent GenerationState = "current"

	// GenerationLeft — the node's position is from a generation the log
	// has since left, a sequence space that no longer exists; a readmission
	// is refused on it.
	GenerationLeft GenerationState = "left"

	// GenerationAhead — the node's position is from a generation above the
	// one this node runs the domain at: the fleet has re-anchored past the
	// node answering, whose own row is the stale one.
	GenerationAhead GenerationState = "ahead"

	// GenerationUnknown — the domain is not one the answering node runs,
	// so there is no generation to compare against.
	GenerationUnknown GenerationState = "unknown"
)

// GenerationStates is every [GenerationState] this build names.
func GenerationStates() []GenerationState {
	return []GenerationState{GenerationCurrent, GenerationLeft, GenerationAhead,
		GenerationUnknown}
}

// Valid reports whether s is a state this build names.
func (s GenerationState) Valid() bool { return slices.Contains(GenerationStates(), s) }

// EvictionReport is a tombstone as the operator surface renders it.
type EvictionReport struct {
	By string    `json:"by"`
	At time.Time `json:"at"`

	// EffectiveAt is when the trim stops counting the node, one fence window
	// after the gesture; printed because an operator unaware of the delay
	// reads the unchanged watermark as a failure.
	EffectiveAt time.Time `json:"effective_at"`

	// Effective says the window has passed.
	Effective bool `json:"effective"`
}

// SnapshotReport is one node's newest artefact, as the register holds it.
type SnapshotReport struct {
	NodeID string `json:"node_id"`

	// Domains is the artefact's position per domain. Empty when the node
	// holds none, in which case Skip says why.
	Domains map[string]uint64 `json:"domains,omitempty"`

	At    time.Time `json:"at,omitzero"`
	Bytes int64     `json:"bytes,omitempty"`

	// Skip is the loop's reason for holding none, making "node-4 none" an
	// answer.
	Skip SkipReason `json:"skip,omitempty"`
}

// ReplicaReport is what THIS node costs to replace. It is a retention fact:
// a joining node replays the log's window, and how long that takes decides
// whether the window is big enough.
type ReplicaReport struct {
	// StoreBytes is the replicated estate on this node's disk.
	StoreBytes int64 `json:"store_bytes"`

	// ProjectedJoinSeconds is how long a peer would need to become a
	// complete replica of it, and RejoinWindowSeconds the operator's budget
	// for that; seconds, not durations, since a dashboard reads this too.
	ProjectedJoinSeconds float64 `json:"projected_join_seconds"`
	RejoinWindowSeconds  float64 `json:"rejoin_window_seconds"`
}

// WithinWindow reports whether a join fits the operator's budget; a method,
// not a stored bool that could disagree with the two fields.
func (r ReplicaReport) WithinWindow() bool {
	return r.RejoinWindowSeconds > 0 && r.ProjectedJoinSeconds <= r.RejoinWindowSeconds
}

// DomainInputs is one domain's contribution to the report.
type DomainInputs struct {
	Domain     string
	Stream     string
	Generation uint32
	Replay     ReplayProtocol

	// FirstSeq, LastSeq, Bytes and MaxBytes come from the stream, and
	// StreamReadable says whether they could be read: an unread stream
	// is not one with a zero ceiling.
	FirstSeq, LastSeq, Bytes, MaxBytes uint64
	StreamReadable                     bool

	// BytesPerDay is the log's measured daily intake, nil where nothing
	// was measured. See [DomainReport.BytesPerDay].
	BytesPerDay *uint64

	// Reservation is how the log divides MaxBytes between ordinary writes
	// and gate records ([ReservationOf]), which its headroom is measured
	// against; its zero value keeps no reserve.
	Reservation Reservation

	// TrimFloor is the published floor, and Decision is what this tick
	// concluded from the six terms — both meaningful only where FloorState
	// is [TrimFloorPublished].
	TrimFloor  uint64
	Decision   TrimDecision
	FloorState TrimFloorState

	// NotReady and WritesRefused are this node's own refusals of the
	// domain — see [DomainReport.NotReady].
	NotReady      *DomainRefusal
	WritesRefused *DomainRefusal

	// BlockedSince is when this domain's trim last advanced, and is zero
	// when it is not blocked.
	BlockedSince time.Time

	// EvictionsUnreadable is whether this node's read of the log's
	// evictions failed — see [DomainReport.EvictionsUnreadable].
	EvictionsUnreadable bool

	// BackupAt is when the backup this log's trim counts was taken — see
	// [DomainReport.BackupAt] — and zero where none covers the log.
	BackupAt time.Time

	// SnapshotSkip is this node's snapshot loop's own reason for taking
	// none, empty when it is taking them.
	SnapshotSkip SkipReason

	// DeferredAge and DeferredRecord are the oldest record this node holds
	// on this log and cannot decode, as [Reading] defines them, zero while
	// it holds none. Per log, not on the node's reading: each log's copy is
	// judged on its own (ADR-0079), so a record past the grace takes only
	// that log's copy out, and one node-wide alarm naming the oldest record
	// would hide the other logs it took out.
	DeferredAge    time.Duration
	DeferredRecord string

	// FloorUnknownFor and FloorUnknownCause are how long this node has been
	// unable to read this log's trim floor and what the latest read said,
	// as [Reading] defines them — zero while it can.
	FloorUnknownFor   time.Duration
	FloorUnknownCause string

	// FeedDeadLetters is what this log's wake feed holds dead-lettered, as
	// [Reading] defines it; empty with no feed or none held. A failed
	// listing carries the last reading that answered, aged to now: an entry
	// is not gone because one listing failed.
	FeedDeadLetters []FeedDeadLetter

	// LinearizableReads and LinearizableReadsExpected are this log's
	// observed daily read rate and its census, as [Reading] defines them;
	// per log, because each log is sized for its own census.
	LinearizableReads, LinearizableReadsExpected int

	// CeilingField is the Tier A field that sets this log's byte ceiling,
	// which a log past its census names ([Reading.LogCeilingField]).
	CeilingField string

	// Tombstones is this log's own evictions: with the register's rows
	// naming the log and the live data nodes ([ReportInputs.Live]), one of
	// the three inputs the trim and the node block's counted mark
	// ([ReportInputs.nodes]) count the log's nodes from.
	Tombstones []Tombstone
}

// ReportInputs is everything the report is assembled from, already read:
// values, not callbacks, so the assembly is a pure function and every case an
// operator screenshots is reachable in a table test with no broker, store or
// fleet.
type ReportInputs struct {
	NodeID      string
	At          time.Time
	BackupOwner string

	// Reading is this node's alarm reading, minus the per-domain
	// conditions (headroom, ceiling against window, a blocked trim, backup
	// age, a record past the deferral grace, an unreadable floor, feed dead
	// letters, a read rate past the census), which are evaluated here from
	// the domain rows and [DomainInputs] so an alarm names its log.
	Reading Reading

	// ReplayWindow is `stream.tracker_retention.min_age`: the history every
	// log keeps, since the trim removes no younger record. One value, as one
	// age term governs every domain's trim; `log_ceiling_short` asks each
	// log's ceiling to hold it at that log's measured rate.
	ReplayWindow time.Duration

	// BackupMaxAge is the policy every log's backup is aged against
	// (`backup_max_age`), zero for a deployment with none. Here, not on
	// Reading, because a backup's age is per log (under `backup_floor:
	// operator` each log is acknowledged on its own), and a node-wide age
	// would show green beside every log the newest point left out.
	BackupMaxAge time.Duration

	Domains []DomainInputs

	// Register is every node's row and RegisterReadable whether it could
	// be listed; an unreadable register is not an empty fleet.
	Register         []coord.NodePositions
	RegisterReadable bool

	// Live is every live data node, which the node block marks live and
	// every log's counted set counts ([CountedSet]); nil where they could
	// not be listed, when the mark counts the rows alone and each log's
	// published floor says its counted set was unknown.
	Live []Presence

	// Tombstones is one per node evicted on EVERY identity log, the latest,
	// which the node block renders as evicted; each log's own are its
	// [DomainInputs.Tombstones].
	Tombstones []Tombstone

	Replica ReplicaReport

	// Maintenance is every open capacity operation, the oldest first, and
	// MaintenanceUnread every log whose window couldn't be read.
	Maintenance       []MaintenanceReport
	MaintenanceUnread []string
}

// NewReport assembles the answer.
func NewReport(in ReportInputs) Report {
	rep := Report{
		V:                ReportVersion,
		NodeID:           in.NodeID,
		At:               in.At.UTC(),
		BackupOwner:      in.BackupOwner,
		Replica:          in.Replica,
		RegisterReadable: in.RegisterReadable,
	}
	for _, d := range in.Domains {
		rep.Domains = append(rep.Domains, in.domain(d))
	}
	// Every list the wire requires is `[]`, never null: a reader indexing a
	// required array crashes on `null`.
	rep.Domains = nonNil(rep.Domains)
	rep.Nodes = nonNil(in.nodes())
	rep.Snapshots = nonNil(in.snapshots())
	rep.Alarms = nonNil(in.alarms(rep.Domains))
	rep.Unjudged = Unjudged(in.Reading)
	rep.Maintenance = in.Maintenance
	rep.MaintenanceUnread = in.MaintenanceUnread
	// The level comes from this node's own lag, not the fleet's: an
	// unreadable peer row says nothing about whether THIS answer can claim
	// an age.
	rep.ReadLevel = ResolveReplicationLevel(rep.lagKnownHere())
	return rep
}

// nonNil is a list that encodes as `[]` rather than `null` when empty, so a
// reader indexes it without a presence check.
func nonNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

// lagKnownHere reports whether this node's own distance from EVERY domain's
// log came back as a number: a `stale` claim true of some rows bounds nothing
// on the rest. A node with no row in the register (the coordination outage
// this document is opened in) has no known lag, not a vacuously known one.
func (r Report) lagKnownHere() bool {
	for _, node := range r.Nodes {
		if node.NodeID != r.NodeID {
			continue
		}
		if len(node.Domains) != len(r.Domains) {
			return false
		}
		for _, d := range node.Domains {
			if d.Lag == nil {
				return false
			}
		}
		return len(r.Domains) > 0
	}
	return false
}

// ExitNonZero is the CLI's exit code: `crewlet retention status` exits
// non-zero exactly when this node has an active alarm, the evaluation that
// drives the gauges and log lines. A rule in the CLI would be a second
// definition of "something is wrong".
func (r Report) ExitNonZero() bool { return len(r.Alarms) > 0 }

// Blocked names every domain whose trim is not advancing.
func (r Report) Blocked() []string {
	var out []string
	for _, d := range r.Domains {
		if d.BlockedBy != "" {
			out = append(out, d.Domain)
		}
	}
	return out
}

// domain builds one domain's row.
func (in ReportInputs) domain(d DomainInputs) DomainReport {
	out := DomainReport{
		Domain:              d.Domain,
		Stream:              d.Stream,
		Generation:          d.Generation,
		Replay:              d.Replay,
		FirstSeq:            d.FirstSeq,
		LastSeq:             d.LastSeq,
		Bytes:               d.Bytes,
		BytesPerDay:         d.BytesPerDay,
		TrimFloor:           d.TrimFloor,
		TrimTo:              d.Decision.To,
		TrimFloorState:      d.FloorState,
		BlockedBy:           d.Decision.BlockedBy,
		SnapshotBlockedBy:   d.SnapshotSkip,
		NotReady:            d.NotReady,
		WritesRefused:       d.WritesRefused,
		EvictionsUnreadable: d.EvictionsUnreadable,
		BackupAt:            d.BackupAt.UTC(),
		// Never null, though a domain with no conclusion has no terms
		// ([DomainReport.Terms]).
		Terms: make([]TermReport, 0, len(d.Decision.Terms)),
	}
	if !out.TrimFloorState.Valid() {
		// A caller that said nothing has no conclusion: read as
		// published, its zeros would claim an advancing trim.
		out.TrimFloorState = TrimFloorNoneAtGeneration
	}
	if d.StreamReadable && d.MaxBytes > 0 {
		out.MaxBytes = d.MaxBytes
		out.ReserveBytes = d.Reservation.Bytes(d.MaxBytes)
		// Of the ceiling ordinary writes are refused at
		// ([Reservation.Headroom]).
		out.HeadroomFraction = d.Reservation.Headroom(d.Bytes, d.MaxBytes)
	}
	for _, t := range d.Decision.Terms {
		state := termState(t)
		// A sequence goes out only where it is one: unbounded's 2^64-1
		// looks like a position, and a JSON float64 cannot carry it.
		seq := t.Seq
		if state != TermKnown {
			seq = 0
		}
		out.Terms = append(out.Terms, TermReport{
			Name:   t.Name,
			State:  state,
			Seq:    seq,
			Detail: t.Detail,
			Remedy: in.remedy(t.Name),
		})
	}
	if out.BlockedBy != "" {
		// A blocked domain always carries an instant: on the tick that
		// first sees the block it is now (a true duration of zero), not
		// the zero time, "blocked since year one".
		out.BlockedSince = d.BlockedSince.UTC()
		if d.BlockedSince.IsZero() {
			out.BlockedSince = in.At.UTC()
		}
		out.Prose = fmt.Sprintf("Nothing is being trimmed on %s: %s.",
			d.Domain, strings.TrimSuffix(d.Decision.Detail, "."))
	}
	return out
}

// termState is the rendering of one term, and the only place [SeqUnbounded] is
// turned back into words. Absent is checked first: an absent term is also
// unread, and would otherwise show every feedless domain's wake feed as
// unreadable. Unbounded precedes known, since the sentinel arrives with Known
// set and must not render as a sequence.
func termState(t Term) TermState {
	switch {
	case t.Absent:
		return TermAbsent
	case !t.Known:
		return TermUnreadable
	case t.Seq == SeqUnbounded:
		return TermUnbounded
	default:
		return TermKnown
	}
}

// remedy is what an operator does about one term. The age floor, which binds
// on a fleet where nothing is wrong, gets one too: a blank beside it reads as
// an incomplete surface, so the healthy answer is written as an answer.
func (in ReportInputs) remedy(name TermName) string {
	switch name {
	case TermApplied:
		return "A counted node is behind the rest. Bring it back, or evict it " +
			"with `crewlet retention evict` once you know it is not coming."
	case TermMinHold:
		return "A live hold pins the log's tail: a backup or a joining node is " +
			"running, or a crashed one is still heartbeating. It clears itself " +
			"when the holder finishes or goes stale."
	case TermBackupFloor:
		if in.BackupOwner != "" {
			return fmt.Sprintf("No complete backup inside `backup_max_age`. Run "+
				"`crewlet backup`, or set `backup_floor: operator` and run "+
				"`crewlet retention ack`. This company's backup owner is %q.",
				in.BackupOwner)
		}
		return "No complete backup inside `backup_max_age`. Run `crewlet backup`, " +
			"or set `backup_floor: operator` and run `crewlet retention ack`. No " +
			"backup owner is configured: set `retention.backup_owner` so the " +
			"next person reading this knows whose job it is."
	case TermSnapshotFloor:
		return fmt.Sprintf("Fewer than %d nodes hold a verified snapshot. The "+
			"snapshot block names each node and its loop's own skip reason.",
			SnapshotDonorsRequired)
	case TermFeedAckFloor:
		return "The wake feed has not scanned this far. A record it has not seen " +
			"is one nobody has been told about, so the trim waits for it."
	case TermAgeFloor:
		return "Nothing is wrong: `min_age` is the binding term, which is what a " +
			"healthy fleet looks like."
	}
	return ""
}

// nodes builds the per-node block: the counted set, what each has done, and
// the tombstones.
func (in ReportInputs) nodes() []NodeReport {
	reported := make(map[string]coord.NodePositions, len(in.Register))
	for _, row := range in.Register {
		reported[row.NodeID] = row
	}
	live := make(map[string]bool, len(in.Live))
	for _, p := range in.Live {
		live[p.NodeID] = true
	}
	tombs := make(map[string]Tombstone, len(in.Tombstones))
	for _, t := range in.Tombstones {
		tombs[t.NodeID] = t
	}

	// The counted set is the trim's own [CountedSet], log by log over the
	// same three inputs (the rows naming the log, the live data nodes, the
	// log's tombstones), so the screen names the fleet the gate waits for.
	// One set over every log's rows would count a node evicted on one log
	// beside another it never ran.
	counted := make(map[string]bool)
	for _, d := range in.Domains {
		var named []NodePosition
		for _, row := range in.Register {
			if _, runs := row.Domains[d.Domain]; runs {
				named = append(named, NodePosition{NodeID: row.NodeID, At: row.At})
			}
		}
		for _, n := range CountedSet(in.At, named, in.Live, d.Tombstones) {
			counted[n.NodeID] = true
		}
	}

	named := map[string]bool{}
	ids := make([]string, 0, len(reported)+len(live)+len(counted)+len(tombs))
	for _, group := range []iter.Seq[string]{maps.Keys(reported), maps.Keys(live),
		maps.Keys(counted), maps.Keys(tombs)} {
		for id := range group {
			if !named[id] {
				named[id] = true
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)

	lastSeq := make(map[string]uint64, len(in.Domains))
	readable := make(map[string]bool, len(in.Domains))
	generation := make(map[string]uint32, len(in.Domains))
	for _, d := range in.Domains {
		lastSeq[d.Domain], readable[d.Domain] = d.LastSeq, d.StreamReadable
		generation[d.Domain] = d.Generation
	}

	out := make([]NodeReport, 0, len(ids))
	for _, id := range ids {
		row := NodeReport{NodeID: id, Counted: counted[id], Live: live[id]}
		if r, ok := reported[id]; ok {
			row.At = r.At.UTC()
			for name, d := range r.Domains {
				nd := NodeDomainReport{
					Generation:         d.Generation,
					Seq:                d.Seq,
					AppliedThrough:     d.AppliedThrough,
					Deferred:           d.Deferred,
					LogDiverged:        d.LogDiverged,
					StreamCreatedAt:    d.StreamCreatedAt.UTC(),
					CheckpointStoredAt: d.CheckpointStoredAt.UTC(),
				}
				// The generation first: it decides whether the sequence
				// compares with the log's at all.
				current, runs := generation[name]
				switch {
				case !runs:
					nd.GenerationState = GenerationUnknown
				case d.Generation < current:
					nd.GenerationState = GenerationLeft
				case d.Generation > current:
					nd.GenerationState = GenerationAhead
				default:
					nd.GenerationState = GenerationCurrent
				}
				// Lag is computed here, never taken from the node,
				// whose idea of the stream's end is what it lags on;
				// and only within one generation.
				if readable[name] && nd.GenerationState == GenerationCurrent {
					var lag uint64
					if last := lastSeq[name]; last > d.Seq {
						lag = last - d.Seq
					}
					nd.Lag = &lag
				}
				if row.Domains == nil {
					row.Domains = make(map[string]NodeDomainReport, len(r.Domains))
				}
				row.Domains[name] = nd
			}
		}
		if t, ok := tombs[id]; ok {
			row.Evicted = &EvictionReport{
				By:          t.By,
				At:          t.At.UTC(),
				EffectiveAt: t.At.Add(EvictionFenceWindow).UTC(),
				// [CountedSet]'s comparison, to the instant, so
				// the screen never drops a node the trim still
				// waits for.
				Effective: in.At.Sub(t.At) > EvictionFenceWindow,
			}
		}
		out = append(out, row)
	}
	return out
}

// snapshots builds the register's view of who can donate.
func (in ReportInputs) snapshots() []SnapshotReport {
	rows := make([]SnapshotReport, 0, len(in.Register))
	for _, r := range in.Register {
		row := SnapshotReport{
			NodeID: r.NodeID,
			Bytes:  r.SnapshotBytes,
			Skip:   SkipReason(r.SnapshotSkip),
		}
		for name, d := range r.Domains {
			if d.SnapshotSeq == 0 {
				continue
			}
			if row.Domains == nil {
				row.Domains = make(map[string]uint64, len(r.Domains))
			}
			row.Domains[name] = d.SnapshotSeq
			if d.SnapshotAt.After(row.At) {
				row.At = d.SnapshotAt.UTC()
			}
		}
		// A node with no artefact and no reason still gets a row: the
		// absence answers "why did the join fail", where no row reads as
		// a node never asked.
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b SnapshotReport) int {
		return strings.Compare(a.NodeID, b.NodeID)
	})
	return rows
}

// alarms evaluates this node's conditions once. [Reading] describes one node,
// so the per-log kinds ([PerLogKinds]) are evaluated per domain row, which shares
// its index with [ReportInputs.Domains], to name WHICH log: one folded reading
// would report the worst unnamed, misstate the grace and floor of the others,
// and give every log the newest backup's age. Each kind is still ONE alarm (its
// identity on the count, the gauge and every screen) listing its logs
// ([Alarm.Domains]) and leading with the most pressing log's detail; the same
// words on several logs no term holds are one entry ([withEntry]). The
// [Tracker] forms the per-log log lines from it. While a log's [KindBackupAge]
// fires, its [KindTrimBlocked] held by the backup term folds into the backup
// entry, the backup being its remedy; a backup-term hold whose own backup is
// inside the policy stays a [KindTrimBlocked].
func (in ReportInputs) alarms(domains []DomainReport) []Alarm {
	out := Evaluate(in.Reading)
	// Where each per-domain kind sits in out, and the reading its detail is
	// from.
	at := make(map[Kind]int)
	lead := make(map[Kind]Reading)
	for i, d := range domains {
		held := in.Domains[i]
		perDomain := Reading{
			HeadroomFraction: d.HeadroomFraction,
			TrimBlockedBy:    string(d.BlockedBy),
			TrimPastWindow:   pastWindow(d),
			// The row's MaxBytes is set only where the stream answered
			// with a ceiling: an unknown ceiling is not a small one.
			LogBytesPerDay: d.BytesPerDay,
			LogMaxBytes:    d.MaxBytes,
			ReplayWindow:   in.ReplayWindow,
			// One policy; each log's backup is aged against it.
			BackupMaxAge: in.BackupMaxAge,
			// As the caller measured them from its own applier and
			// floor observations.
			DeferredAge:               held.DeferredAge,
			DeferredRecord:            held.DeferredRecord,
			FloorUnknownFor:           held.FloorUnknownFor,
			FloorUnknownCause:         held.FloorUnknownCause,
			FeedDeadLetters:           held.FeedDeadLetters,
			LinearizableReads:         held.LinearizableReads,
			LinearizableReadsExpected: held.LinearizableReadsExpected,
			LogCeilingField:           held.CeilingField,
		}
		if d.BlockedBy != "" && !d.BlockedSince.IsZero() {
			perDomain.TrimBlockedFor = in.At.Sub(d.BlockedSince)
		}
		if !d.BackupAt.IsZero() {
			perDomain.BackupAge = Age(in.At.Sub(d.BackupAt))
		}
		fired := Evaluate(perDomain)
		// Folded only where both fire on this log: the trim_blocked raised
		// on the backup term and the log's own backup_age.
		trimAt := slices.IndexFunc(fired, func(a Alarm) bool { return a.Kind == KindTrimBlocked })
		heldByBackup := d.BlockedBy == TermBackupFloor && trimAt >= 0 &&
			slices.ContainsFunc(fired, func(a Alarm) bool { return a.Kind == KindBackupAge })
		for _, a := range fired {
			entry := AlarmDomain{Domains: []string{d.Domain}, Detail: a.Detail}
			switch a.Kind {
			case KindTrimBlocked:
				if heldByBackup {
					continue
				}
				entry.Term, entry.Since = d.BlockedBy, d.BlockedSince
			case KindBackupAge:
				if heldByBackup {
					// The unraised hold's measurement rides here:
					// how long the trim has stood still is what
					// makes the missing backup urgent.
					entry.Term, entry.Since = d.BlockedBy, d.BlockedSince
					entry.Detail += "; " + fired[trimAt].Detail
				}
			}
			slot, seen := at[a.Kind]
			if !seen {
				slot = len(out)
				at[a.Kind] = slot
				out = append(out, Alarm{Kind: a.Kind, Title: a.Title, Person: a.Person, Remedy: a.Remedy})
			}
			out[slot].Domains = withEntry(out[slot].Domains, entry)
			if current, has := lead[a.Kind]; !has || morePressing(a.Kind, perDomain, current) {
				lead[a.Kind] = perDomain
				out[slot].Detail = entry.Detail
			}
		}
	}
	// The table's order, which every surface reads in; the per-domain kinds
	// were appended last.
	slices.SortStableFunc(out, func(a, b Alarm) int {
		return tableIndex(a.Kind) - tableIndex(b.Kind)
	})
	return out
}

// withEntry is an alarm's entries with one more log's: carried on the entry
// already naming the same measurement where no trim term holds either, and as
// an entry of its own otherwise ([AlarmDomain.Domains]).
func withEntry(entries []AlarmDomain, entry AlarmDomain) []AlarmDomain {
	if entry.Term == "" {
		if i := slices.IndexFunc(entries, func(e AlarmDomain) bool {
			return e.Term == "" && e.Detail == entry.Detail
		}); i >= 0 {
			entries[i].Domains = append(entries[i].Domains, entry.Domains...)
			return entries
		}
	}
	return append(entries, entry)
}

// pastWindow is how many records a domain's log holds older than the replay
// window, the ones the age term alone would release: the age term's sequence
// (the published tick's first record young enough to keep) less the log's
// first. The term is a tick old and the bounds current, which can only
// undercount, since a blocked trim has removed nothing since. It counts
// sequences: on a compacted log, with holes, an upper bound, and zero is still
// exactly nothing. Zero where nothing is known: an unread stream, an empty log,
// or no published floor.
func pastWindow(d DomainReport) uint64 {
	if d.LastSeq == 0 || d.LastSeq < d.FirstSeq {
		return 0
	}
	for _, t := range d.Terms {
		if t.Name != TermAgeFloor || t.State != TermKnown {
			continue
		}
		keep := min(t.Seq, d.LastSeq+1)
		if keep <= d.FirstSeq {
			return 0
		}
		return keep - d.FirstSeq
	}
	return 0
}

// morePressing reports whether one log's reading of a per-domain alarm is more
// urgent than current's, which decides whose measurement the alarm leads with:
// the log with the least headroom left, the ceiling that holds the least of its
// window, the trim held longest, the log with no backup at all or else the
// oldest one, the record held longest, the floor unreadable longest, the dead
// letter nearest its expiry and the read rate furthest past its census. A tie
// keeps the log the report reached first, which is the report's own order.
func morePressing(kind Kind, r, current Reading) bool {
	switch kind {
	case KindLogHeadroom:
		return r.HeadroomFraction != nil && current.HeadroomFraction != nil &&
			*r.HeadroomFraction < *current.HeadroomFraction
	case KindLogCeilingShort:
		// The ceiling that holds the fewest days at its log's own rate. Both
		// rates are measured and non-zero, or the rule would not have fired.
		if r.LogBytesPerDay == nil || current.LogBytesPerDay == nil ||
			*r.LogBytesPerDay == 0 || *current.LogBytesPerDay == 0 {
			return false
		}
		return float64(r.LogMaxBytes)/float64(*r.LogBytesPerDay) <
			float64(current.LogMaxBytes)/float64(*current.LogBytesPerDay)
	case KindTrimBlocked:
		return r.TrimBlockedFor > current.TrimBlockedFor
	case KindBackupAge:
		// NO BACKUP AT ALL LEADS: a nil age is a log nothing covers, which
		// is worse than any copy that is merely old.
		if r.BackupAge == nil {
			return current.BackupAge != nil
		}
		return current.BackupAge != nil && *r.BackupAge > *current.BackupAge
	case KindDeferredOld:
		return r.DeferredAge > current.DeferredAge
	case KindFloorUnknown:
		return r.FloorUnknownFor > current.FloorUnknownFor
	case KindFeedDeadLettered:
		// THE ENTRY WITH THE LEAST TIME LEFT: its wake is the next one the
		// retention takes. Both hold one, or the rule would not have fired.
		return soonestExpiry(r.FeedDeadLetters) < soonestExpiry(current.FeedDeadLetters)
	case KindCensusDrift:
		if r.LinearizableReadsExpected <= 0 || current.LinearizableReadsExpected <= 0 {
			return false
		}
		return float64(r.LinearizableReads)/float64(r.LinearizableReadsExpected) >
			float64(current.LinearizableReads)/float64(current.LinearizableReadsExpected)
	}
	return false
}

// soonestExpiry is the least time any of letters has left, and the longest
// duration there is for none.
func soonestExpiry(letters []FeedDeadLetter) time.Duration {
	soonest := time.Duration(math.MaxInt64)
	for _, l := range letters {
		soonest = min(soonest, l.ExpiresIn)
	}
	return soonest
}

// tableIndex is a kind's position in [table], which is the order alarms are
// reported in. A kind the table does not hold sorts last.
func tableIndex(kind Kind) int {
	for i, rule := range table {
		if rule.kind == kind {
			return i
		}
	}
	return len(table)
}

// JoinSecondsPerGB and JoinFixedSeconds project how long a peer needs to
// become a complete replica of a store of a given size: arithmetic, not
// measured on this host, answering whether the operator's window is plausible
// ([ReplicaReport] carries both). The per-gigabyte term is the conservative
// network-volume profile, not local NVMe (about 2.3x faster): a window that
// fits only the fast path expires in the incident it was set for. The fixed
// term is 25 s of setup (opening the estates, the migrator, the donor) plus
// the 18 s read barriers add to a 24-hour replay (793 fetches, not 272, at
// about 35 ms each).
const (
	JoinSecondsPerGB = 27.33
	JoinFixedSeconds = 43.0
)

// ProjectJoinSeconds is the conservative projection for a store of this size.
func ProjectJoinSeconds(storeBytes int64) float64 {
	if storeBytes <= 0 {
		return JoinFixedSeconds
	}
	return JoinSecondsPerGB*(float64(storeBytes)/(1<<30)) + JoinFixedSeconds
}
