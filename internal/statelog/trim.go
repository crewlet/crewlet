package statelog

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
	"slices"
	"sort"
	"time"
)

// TermName is one of the six things that must permit a record's removal.
//
// NAMED, because `blocked_by` is what an operator reads when a log is growing
// and the trim is not advancing — and "the trim is blocked" without which term
// is a sentence that sends somebody to read code.
type TermName string

const (
	// TermApplied is the lowest committed position over the COUNTED SET.
	//
	// It reads each node's own committed sequence rather than what it has
	// applied THROUGH: reading the latter would let one un-upgraded node
	// holding a record it cannot decode block the whole fleet's trim for
	// ever. The committed sequence is safe because a deferred record's
	// bytes are durable before the checkpoint moves over it.
	TermApplied TermName = "applied"

	// TermMinHold is the lowest live hold. A joining node holds the tail
	// it is about to replay, so a transfer pins what it needs for its
	// whole duration.
	TermMinHold TermName = "min_hold"

	// TermBackupFloor is what has left the host. A company that never
	// backs up never trims — the log is the only copy of what no node has
	// applied yet — and that is the configuration working as asked.
	TermBackupFloor TermName = "backup_floor"

	// TermSnapshotFloor is the k-th HIGHEST verified snapshot position
	// over the counted set.
	//
	// The k-th rather than the minimum or the maximum: the minimum blocks
	// for ever on any node that has not snapshotted yet, and the maximum
	// makes one donor's disk the whole fleet's recovery plan.
	TermSnapshotFloor TermName = "snapshot_floor"

	// TermFeedAckFloor is how far the wake feed has scanned. A record the
	// feed has not seen is one nobody has been told about.
	TermFeedAckFloor TermName = "feed_ack_floor"

	// TermAgeFloor is the newest sequence older than the age floor.
	//
	// IT IS ONE TERM OF A MINIMUM, which is the whole grammar of this
	// gate: every term can only move the trim point DOWN. So the age
	// floor is a floor on TRIMMING and therefore a LOWER bound on how long
	// the log keeps a record — it is not, and cannot be, a ceiling on
	// retention, and it says nothing whatever about any node's own store
	// file. Raising it can only move the trim point down.
	TermAgeFloor TermName = "age_floor"
)

// TermNames are the six, in the order a blocked trim reports them.
var TermNames = []TermName{
	TermApplied, TermMinHold, TermBackupFloor,
	TermSnapshotFloor, TermFeedAckFloor, TermAgeFloor,
}

// Valid reports whether a term name off the wire is one this build knows.
func (t TermName) Valid() bool { return slices.Contains(TermNames, t) }

// Term is one term's answer.
//
// THREE-VALUED IN EFFECT: a term permits removal up to a sequence, refuses
// entirely, or CANNOT BE READ — and the third blocks exactly as the second
// does. A term nobody could read is not a term that is satisfied, and treating
// it as satisfied is how a trim advances past a node that could not report.
type Term struct {
	// Name is which term this is.
	Name TermName

	// Seq is the highest sequence this term permits removing up to,
	// exclusive. Meaningful only when Known.
	Seq uint64

	// Known reports whether this term could be evaluated at all.
	Known bool

	// Detail says why it is unknown, or why it is where it is, for the
	// operator reading a blocked trim.
	Detail string

	// Absent marks a term that does not exist for this domain rather than
	// one that could not be read — a compacted domain has no wake feed,
	// and an absent term is `n/a` rather than zero.
	Absent bool
}

// TrimDecision is what one tick concluded.
type TrimDecision struct {
	// To is the exclusive sequence the trim may remove up to. Zero when
	// blocked.
	To uint64

	// BlockedBy names the first term that could not be read, or that
	// permits nothing. Empty when the trim may proceed.
	BlockedBy TermName

	// Detail is that term's own reason.
	Detail string

	// Terms is every term's answer, for the operator surface.
	Terms []Term
}

// Blocked reports whether this tick may remove anything.
func (d TrimDecision) Blocked() bool { return d.BlockedBy != "" }

// Trim decides how far a tick may remove, from the terms as evaluated.
//
// # The whole grammar, in one sentence
//
// THE TERMS ENTER A MINIMUM, so every one of them can only move the trim point
// DOWN. That is what makes each of them a floor rather than a horizon, and it
// is why the age term is a LOWER bound on how long the log keeps a record
// rather than an upper one. Three sites downstream got this backwards, which
// is why it is stated at the arithmetic rather than beside it.
//
// A term that could not be READ blocks, on the same rule the read path uses
// for an unreadable floor: the third value is not the optimistic one. An
// ABSENT term — one this domain does not have at all — is skipped, because
// `n/a` and zero are different facts.
func Trim(terms []Term) TrimDecision {
	d := TrimDecision{Terms: terms}
	first := true
	for _, name := range TermNames {
		for _, t := range terms {
			if t.Name != name || t.Absent {
				continue
			}
			if !t.Known {
				d.BlockedBy, d.Detail = t.Name, t.Detail
				d.To = 0
				return d
			}
			if first || t.Seq < d.To {
				d.To, first = t.Seq, false
			}
		}
	}
	if first {
		d.BlockedBy = TermApplied
		d.Detail = "no term was evaluated at all, so nothing licenses removing anything"
		return d
	}
	if d.To == 0 {
		// Every term was read and one of them permits nothing yet, which
		// is an ordinary state on a young fleet rather than a fault.
		d.BlockedBy, d.Detail = lowest(terms), "this term permits removing nothing yet"
	}
	return d
}

// lowest names the term holding the trim at zero.
func lowest(terms []Term) TermName {
	for _, name := range TermNames {
		for _, t := range terms {
			if t.Name == name && !t.Absent && t.Known && t.Seq == 0 {
				return t.Name
			}
		}
	}
	return TermApplied
}

// NodePosition is one counted node's report, as the register holds it.
type NodePosition struct {
	// NodeID is whose report this is.
	NodeID string

	// Generation and Seq are its committed position.
	Generation uint32
	Seq        uint64

	// SnapshotSeq is its newest VERIFIED local snapshot's position, and
	// false when it holds none.
	SnapshotSeq uint64
	HasSnapshot bool

	// At is when it last reported.
	At time.Time
}

// Hold is one live pin on the log's tail.
type Hold struct {
	Owner      string
	Generation uint32
	Seq        uint64
	At         time.Time
}

// TrimInputs is everything a tick reads, already fetched.
//
// SEPARATED FROM THE ARITHMETIC deliberately: what the terms mean is a policy
// with an inversion in it that three readers got backwards, and a policy that
// can only be exercised through a live fleet is one nobody checks. This struct
// is what makes the six terms a pure function of what was read.
type TrimInputs struct {
	// Generation is the estate's current generation. A term reported at a
	// LOWER one is unknown and blocks — its sequences name a dead number
	// space, and comparing them would be comparing two different logs.
	Generation uint32

	// Now is the tick's instant.
	Now time.Time

	// Counted is every node the fleet counts, including any that has
	// reported no position yet.
	Counted []NodePosition

	// CountedReadable reports whether the register could be listed at all.
	CountedReadable bool

	// Holds are the live pins. One older than the stale bound is ignored,
	// because a crashed adopter must not pin the log for ever.
	Holds []Hold

	// HoldsReadable reports whether they could be listed.
	HoldsReadable bool

	// BackupFloor is what has left the host, and its age.
	BackupFloor    uint64
	BackupAt       time.Time
	BackupFloorGen uint32
	HasBackupFloor bool

	// BackupMaxAge is how stale that may be before the term refuses.
	BackupMaxAge time.Duration

	// FeedAckFloor is how far the wake feed has scanned, and whether this
	// domain HAS a feed at all — a compacted domain does not, and an
	// absent term is not a zero one.
	FeedAckFloor uint64
	HasFeed      bool
	FeedReadable bool

	// AgeFloor is the newest sequence older than the configured age, which
	// is computed locally and therefore never unknown.
	AgeFloor uint64

	// HoldStale is how old a hold may be before it is ignored.
	HoldStale time.Duration
}

// Terms evaluates the six from what was read.
func (in TrimInputs) Terms() []Term {
	return []Term{
		in.applied(),
		in.minHold(),
		in.backup(),
		in.snapshotFloor(),
		in.feed(),
		{Name: TermAgeFloor, Seq: in.AgeFloor, Known: true, Detail: fmt.Sprintf(
			"the newest sequence older than the configured age is %d", in.AgeFloor)},
	}
}

// applied is the lowest committed position over the counted set.
func (in TrimInputs) applied() Term {
	if !in.CountedReadable {
		return Term{Name: TermApplied, Detail: "the positions register could not " +
			"be listed, so which nodes are counted and where they are is unknown"}
	}
	if len(in.Counted) == 0 {
		return Term{Name: TermApplied, Detail: "the fleet counts no nodes, which " +
			"cannot be true while this one is running"}
	}
	lowest := ^uint64(0)
	var who string
	for _, n := range in.Counted {
		if n.Generation < in.Generation {
			// A POSITION FROM A PREVIOUS GENERATION IS UNKNOWN, not
			// old: its sequences name a dead number space, so
			// comparing it with this generation's would be comparing
			// two different logs.
			return Term{Name: TermApplied, Detail: fmt.Sprintf(
				"%s last reported at generation %d and this estate is on %d, so "+
					"its position names a sequence space that no longer exists",
				n.NodeID, n.Generation, in.Generation)}
		}
		if n.Seq < lowest {
			lowest, who = n.Seq, n.NodeID
		}
	}
	// A COUNTED NODE'S OWN POSITION IS THIS TERM, and the register has no
	// expiry — so an offline counted node pins the floor indefinitely and
	// no age setting bounds what it still holds. The only exit is an
	// eviction, which advances the trim and deletes nothing on that
	// machine.
	return Term{Name: TermApplied, Seq: lowest, Known: true, Detail: fmt.Sprintf(
		"%s is the furthest behind of %d counted node(s), at %d",
		who, len(in.Counted), lowest)}
}

// minHold is the lowest live pin.
func (in TrimInputs) minHold() Term {
	if !in.HoldsReadable {
		return Term{Name: TermMinHold, Detail: "the holds could not be listed"}
	}
	lowest := ^uint64(0)
	var who string
	var live int
	for _, h := range in.Holds {
		// A STALE HOLD IS IGNORED, because a crashed adopter must not pin
		// the log for the life of the deployment.
		if in.HoldStale > 0 && in.Now.Sub(h.At) > in.HoldStale {
			continue
		}
		if h.Generation < in.Generation {
			return Term{Name: TermMinHold, Detail: fmt.Sprintf(
				"the hold held by %s is at generation %d and this estate is on %d",
				h.Owner, h.Generation, in.Generation)}
		}
		live++
		if h.Seq < lowest {
			lowest, who = h.Seq, h.Owner
		}
	}
	if live == 0 {
		// NOTHING IS PINNED, which is not a refusal: it permits removing
		// everything the other terms permit.
		return Term{Name: TermMinHold, Seq: ^uint64(0), Known: true,
			Detail: "nothing is pinning the log"}
	}
	return Term{Name: TermMinHold, Seq: lowest, Known: true, Detail: fmt.Sprintf(
		"%s pins the log at %d", who, lowest)}
}

// backup is what has left the host.
func (in TrimInputs) backup() Term {
	if !in.HasBackupFloor {
		// A COMPANY THAT NEVER BACKS UP NEVER TRIMS, loudly and by
		// design: the log is the only copy of what no node has applied
		// yet.
		return Term{Name: TermBackupFloor, Detail: "no backup has been recorded, " +
			"so nothing has left this host and the log is the only copy of what " +
			"no node has applied yet"}
	}
	if in.BackupFloorGen < in.Generation {
		return Term{Name: TermBackupFloor, Detail: fmt.Sprintf(
			"the recorded backup is at generation %d and this estate is on %d",
			in.BackupFloorGen, in.Generation)}
	}
	if in.BackupMaxAge > 0 && in.Now.Sub(in.BackupAt) > in.BackupMaxAge {
		return Term{Name: TermBackupFloor, Detail: fmt.Sprintf(
			"the newest backup is %s old, past the %s this deployment accepts",
			in.Now.Sub(in.BackupAt).Round(time.Second), in.BackupMaxAge)}
	}
	return Term{Name: TermBackupFloor, Seq: in.BackupFloor, Known: true,
		Detail: fmt.Sprintf("the newest backup covers up to %d", in.BackupFloor)}
}

// snapshotFloor is the k-th highest verified snapshot position, and the trim
// may not pass one past it.
func (in TrimInputs) snapshotFloor() Term {
	if !in.CountedReadable {
		return Term{Name: TermSnapshotFloor, Detail: "the positions register could " +
			"not be listed, so how many nodes hold a snapshot is unknown"}
	}
	// A SOLO FLEET IS SATISFIED BY CONSTRUCTION: the snapshot loop does
	// not run there at all, and a single node's recovery artefact is a
	// backup — which the backup term already gates. Saying so here stops a
	// reader concluding this term deadlocks one node.
	if len(in.Counted) < 2 {
		return Term{Name: TermSnapshotFloor, Seq: ^uint64(0), Known: true,
			Detail: "a single node takes no snapshots; its recovery artefact is a " +
				"backup, which the backup term gates"}
	}
	var have []uint64
	for _, n := range in.Counted {
		if n.HasSnapshot {
			have = append(have, n.SnapshotSeq)
		}
	}
	k := min(len(in.Counted), SnapshotDonorsRequired)
	if len(have) < k {
		return Term{Name: TermSnapshotFloor, Detail: fmt.Sprintf(
			"%d of %d counted node(s) hold a verified snapshot and %d are needed "+
				"— losing any single donor must still leave a usable artefact",
			len(have), len(in.Counted), k)}
	}
	sort.Slice(have, func(i, j int) bool { return have[i] > have[j] })
	kth := have[k-1]
	// The trim may not pass the k-th snapshot's own position: a record at
	// it is the first one a node adopting that artefact has to replay.
	return Term{Name: TermSnapshotFloor, Seq: kth + 1, Known: true, Detail: fmt.Sprintf(
		"%d node(s) hold a snapshot and the %d-th highest is at %d",
		len(have), k, kth)}
}

// feed is how far the wake feed has scanned.
func (in TrimInputs) feed() Term {
	if !in.HasFeed {
		// ABSENT RATHER THAN ZERO. A compacted domain has no wake feed
		// at all, and reporting `0` would block its trim for ever on a
		// term it does not have.
		return Term{Name: TermFeedAckFloor, Absent: true,
			Detail: "this domain has no wake feed"}
	}
	if !in.FeedReadable {
		return Term{Name: TermFeedAckFloor, Detail: "the wake consumer could not be read"}
	}
	return Term{Name: TermFeedAckFloor, Seq: in.FeedAckFloor, Known: true,
		Detail: fmt.Sprintf("the wake feed has scanned past %d", in.FeedAckFloor)}
}

// TrimHoldStale is how old a hold may be before it is ignored and deleted.
//
// FOUR HEARTBEATS, the same derivation every other cached coordination fact
// uses: a holder may miss three beats and still be alive, so a hold older than
// four is a crashed adopter rather than a slow one — and one that pinned the
// log for ever would be a single failed join stopping the fleet trimming.
const TrimHoldStale = 4 * coord.ReconcileInterval
