package statelog

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// The operator's answer, and why it is ONE value.
//
// `crewlet retention status` and GET /work/retention answer the SAME BYTES —
// this type, encoded once by the node that assembled it. The CLI renders what
// it was given and exits non-zero from the alarms it was given; it does not
// re-derive either. That is not tidiness. An operator surface that computed
// its own verdict would be a second implementation of the retention gate,
// running on a machine that cannot see the register, and the first time the
// two disagreed the person reading the screen would have no way to tell which
// one the engine had acted on.
//
// # Why it is per fleet with the DOMAIN AS A COLUMN
//
// The framework carries N domains and every one of them has a stream, a trim
// point, six terms and a floor. The shape that survives a second domain is a
// row per domain inside one answer — not a second command, not a second
// route, and not a `?domain=` that returns a different document. A term that
// does not apply to a domain reports ABSENT rather than zero, because a
// compacted domain genuinely has no wake feed and rendering that as `0` is a
// claim that the feed has scanned nothing.
//
// # What is NOT here, and where it is instead
//
// The per-node SNAPSHOT INVENTORY — every artefact on a node's disk, its
// path, its digest and its verification status — is `crewlet retention
// snapshots`, its own verb, because the repository is per node: "which of my
// machines can donate, and how old is what they hold" is a disk question. The
// block here is what the REGISTER knows, which is one row per node and the
// newest artefact only.

// ReportVersion is the shape this build writes.
//
// Evolution is ADDITIVE, exactly as the event envelope's is and for the same
// reason: a fleet mid-upgrade has an older CLI reading a newer node's answer,
// and the CLI decodes into a struct of its own so an unknown field is ignored
// rather than refused. The version is here for the case a field ever changes
// MEANING, which is the one an added field cannot express.
const ReportVersion = 1

// Report is the fleet's retention answer.
type Report struct {
	V int `json:"v"`

	// NodeID is who assembled it. Load-bearing rather than decorative:
	// the replica block, the alarms and every "this node" figure are
	// facts about THIS node, and a report copied into a ticket without
	// its author is three of those facts attributed to a fleet.
	NodeID string    `json:"node_id"`
	At     time.Time `json:"at"`

	// ReadLevel is the level this answer was SERVED at, which is what
	// keeps a replication answer inside the read-level contract rather
	// than exempt from it.
	//
	// It is DERIVED rather than chosen — by nobody, on any surface, the
	// operator MCP included (see [SurfaceReplication]): `stale` while the
	// lag is a number, and `consistent_prefix` when it is not, because
	// `stale` is a claim about AGE and an unknown lag cannot make one.
	// The moment somebody needs this document is the moment the broker or
	// coordination may be unreachable, so the weaker answer is the one
	// that gets delivered — named, rather than delivered under the
	// stronger name.
	ReadLevel ReadLevel `json:"read_level"`

	// BackupOwner is `retention.backup_owner`, empty when unset. It is on
	// the report rather than looked up beside it because the one place it
	// matters is the backup term's remedy, and a remedy that named a
	// value the report did not carry would be a remedy the JSON reader
	// never sees.
	BackupOwner string `json:"backup_owner,omitempty"`

	Domains []DomainReport `json:"domains"`
	Nodes   []NodeReport   `json:"nodes"`

	// RegisterReadable says the node block is the fleet rather than what
	// could be read of it.
	//
	// AN EMPTY NODE BLOCK IS TWO DIFFERENT ANSWERS — "this fleet has no
	// nodes", which cannot happen, and "coordination could not be
	// listed", which happens during exactly the outage somebody is
	// running this command in. Without the flag a renderer has no way to
	// tell them apart and prints the impossible one.
	RegisterReadable bool `json:"register_readable"`

	Snapshots []SnapshotReport `json:"snapshots"`
	Replica   ReplicaReport    `json:"replica"`

	// Alarms is every condition currently true on this node, evaluated
	// once here. The CLI's exit code is derived from THIS field.
	Alarms []Alarm `json:"alarms"`

	// Maintenance is the capacity operation currently holding the fleet,
	// and nil when there is none.
	//
	// ABSENT RATHER THAN ZEROED, because a zeroed block is a claim that an
	// operation exists in phase "" with nobody outstanding — and the whole
	// point of this field is that maintenance is otherwise visible on NO
	// screen. It stops every publisher on every node: no seats, no duties,
	// no scheduler, no write routes. An hour of that is an outage nobody
	// is watching, which is what `maintenance_open` alarms on and what
	// this renders while it is happening rather than after.
	Maintenance *MaintenanceReport `json:"maintenance,omitempty"`
}

// MaintenanceReport is one open capacity operation, as a reader sees it.
type MaintenanceReport struct {
	// Stream is which log's configuration this is about, and OperationID
	// the identity every attempt of it reuses.
	Stream      string `json:"stream"`
	OperationID string `json:"operation_id"`

	// Phase is where the operation stands and Attempt which write-then-
	// seal cycle it is on. Both, because a second attempt in the same
	// phase is the shape a retry has and one number cannot show it.
	Phase   string `json:"phase"`
	Attempt int    `json:"attempt"`

	// TargetMaxBytes is the ceiling being written and OriginalMaxBytes
	// what it was, so a reader can see the direction without consulting
	// the stream.
	TargetMaxBytes   uint64 `json:"target_max_bytes"`
	OriginalMaxBytes uint64 `json:"original_max_bytes"`

	// Since is when the exclusion was taken and By which operator ran the
	// verb — the two things somebody finding this at an inconvenient hour
	// needs before anything else.
	Since time.Time `json:"since"`
	By    string    `json:"by,omitempty"`

	// ParticipantsMissing names the nodes whose acknowledgement the seal
	// is still waiting for. EMPTY IS NOT THE SAME AS UNKNOWN: an
	// operation with nobody outstanding is one waiting on its operator,
	// and that is the state an operator most needs to be able to see.
	ParticipantsMissing []string `json:"participants_missing,omitempty"`

	// Blocked names why the operation cannot proceed without a person,
	// empty while it can.
	Blocked string `json:"blocked,omitempty"`
}

// DomainReport is one registered domain's row.
type DomainReport struct {
	Domain     string         `json:"domain"`
	Stream     string         `json:"stream"`
	Generation uint32         `json:"generation"`
	Replay     ReplayProtocol `json:"replay"`

	// FirstSeq and LastSeq are the stream's own window, and Bytes and
	// MaxBytes its ceiling. Zero MaxBytes means the broker could not be
	// asked, which is why HeadroomFraction is a POINTER: a fraction of an
	// unknown ceiling is not zero headroom, and rendering it as zero
	// would fire the one alarm an operator cannot ignore.
	FirstSeq         uint64   `json:"first_seq"`
	LastSeq          uint64   `json:"last_seq"`
	Bytes            uint64   `json:"bytes"`
	MaxBytes         uint64   `json:"max_bytes,omitempty"`
	HeadroomFraction *float64 `json:"headroom_fraction,omitempty"`

	// TrimFloor is the floor the fleet has published — what has actually
	// been removed. TrimTo is what THIS tick concluded may be removed.
	// Two numbers because they answer different questions: the first is
	// where a joining node's replay can start, the second is whether the
	// gate is moving at all.
	TrimFloor uint64 `json:"trim_floor"`
	TrimTo    uint64 `json:"trim_to"`

	Terms        []TermReport `json:"terms"`
	BlockedBy    TermName     `json:"blocked_by,omitempty"`
	BlockedSince time.Time    `json:"blocked_since,omitzero"`

	// Prose is the sentence a blocked trim leads with, because it is the
	// answer to the only question anybody runs this command for.
	Prose string `json:"prose,omitempty"`

	// SnapshotBlockedBy is the snapshot loop's own skip reason on this
	// node.
	//
	// ITS OWN FIELD, NOT FOLDED INTO BlockedBy: a fleet that has stopped
	// trimming and a fleet that has stopped snapshotting are two different
	// problems with two different remedies, and the second one is silent
	// until a node tries to join.
	SnapshotBlockedBy SkipReason `json:"snapshot_blocked_by,omitempty"`
}

// TermState is a term's third value made explicit.
type TermState string

const (
	// TermKnown — the term was read and permits removal up to its Seq.
	TermKnown TermState = "ok"

	// TermUnreadable — the term could not be evaluated, which BLOCKS. It
	// renders differently from a term permitting zero because the remedy
	// is different: one is a thing to fix, the other is a thing to wait
	// for.
	TermUnreadable TermState = "unknown"

	// TermAbsent — this domain does not have the term at all. `n/a`
	// rather than `0`, on the rule that runs through the whole framework.
	TermAbsent TermState = "n/a"
)

// TermReport is one of the six terms, with the remedy for it.
type TermReport struct {
	Name  TermName  `json:"name"`
	State TermState `json:"state"`

	// Seq is what the term permits, and is meaningless unless State is
	// TermKnown.
	Seq uint64 `json:"seq,omitempty"`

	// Detail is what was read, named: which node, which holder, how old.
	Detail string `json:"detail,omitempty"`

	// Remedy is what to do about it. On EVERY term rather than on the
	// blocking one alone — an operator watching a term approach is the
	// case the whole surface exists for, and a remedy that appeared only
	// once the term had already blocked would arrive one incident late.
	Remedy string `json:"remedy"`
}

// NodeReport is one node's row: what the fleet counts, and what it has done.
type NodeReport struct {
	NodeID string `json:"node_id"`

	// Counted says the trim waits for this node. Live says it is holding
	// a presence lease right now. THEY ARE INDEPENDENT and the pair is
	// what an operator reads: counted-and-not-live is the node pinning
	// the log, and live-and-not-counted is a node inside its eviction
	// fence window, still writing.
	Counted bool `json:"counted"`
	Live    bool `json:"live"`

	// At is when it last wrote its row, and is ZERO for a node that is
	// live and has never reported. That case is rendered as counted with
	// no position yet rather than as a node at position zero, because the
	// two block the trim for very different lengths of time.
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

	// Lag is this node's distance from the stream's last sequence, and
	// NIL when the stream could not be read — the same pointer rule
	// [Health] uses, for the same reason: an unknown lag rendered as zero
	// is a node reported as caught up.
	Lag *uint64 `json:"lag,omitempty"`

	Deferred int `json:"deferred,omitempty"`
}

// EvictionReport is a tombstone as the operator surface renders it.
type EvictionReport struct {
	By string    `json:"by"`
	At time.Time `json:"at"`

	// EffectiveAt is when the trim stops counting the node — one fence
	// window after the gesture, printed because the gesture is NOT
	// immediate and an operator who does not know that reads the
	// unchanged watermark as a failure.
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

	// Skip is the loop's own reason for holding none, and is what turns
	// "node-4 none" into an answer.
	Skip SkipReason `json:"skip,omitempty"`
}

// ReplicaReport is what THIS node costs to replace.
//
// On the retention surface rather than a health one because it is the same
// question: the log's window is what a joining node replays from, and how
// long that takes is what decides whether the window is big enough.
type ReplicaReport struct {
	// StoreBytes is the replicated estate on this node's disk.
	StoreBytes int64 `json:"store_bytes"`

	// ProjectedJoinSeconds is how long a peer would need to become a
	// complete replica of it, and RejoinWindowSeconds is the operator's
	// budget for that. Both seconds rather than durations because this
	// document is read by a dashboard as often as by a person.
	ProjectedJoinSeconds float64 `json:"projected_join_seconds"`
	RejoinWindowSeconds  float64 `json:"rejoin_window_seconds"`
}

// WithinWindow reports whether a join fits the operator's budget.
//
// A method rather than a stored bool: it is a comparison of two fields in the
// same struct, and a third field could disagree with them.
func (r ReplicaReport) WithinWindow() bool {
	return r.RejoinWindowSeconds > 0 && r.ProjectedJoinSeconds <= r.RejoinWindowSeconds
}

// DomainInputs is one domain's contribution to the report.
type DomainInputs struct {
	Domain     string
	Stream     string
	Generation uint32
	Replay     ReplayProtocol

	// FirstSeq, LastSeq, Bytes and MaxBytes come from the stream itself,
	// and StreamReadable says whether they could be read at all. A stream
	// nobody could ask is not a stream with a zero ceiling.
	FirstSeq, LastSeq, Bytes, MaxBytes uint64
	StreamReadable                     bool

	// TrimFloor is the published floor, and Decision is what this tick
	// concluded from the six terms.
	TrimFloor uint64
	Decision  TrimDecision

	// BlockedSince is when this domain's trim last advanced, and is zero
	// when it is not blocked.
	BlockedSince time.Time

	// SnapshotSkip is this node's snapshot loop's own reason for taking
	// none, empty when it is taking them.
	SnapshotSkip SkipReason
}

// ReportInputs is everything the report is assembled from, already read.
//
// A STRUCT OF VALUES rather than a set of callbacks, so the assembly is a
// pure function: every case an operator ever screenshots — a blocked term, a
// node inside its fence, a register nobody could list — is reachable in a
// table test with no broker, no store and no fleet.
type ReportInputs struct {
	NodeID      string
	At          time.Time
	BackupOwner string

	// Reading is this node's alarm reading, MINUS the two conditions that
	// are per domain: log headroom and a blocked trim are evaluated per
	// domain here, from the domain rows, so a two-domain fleet reports
	// which domain is full rather than that something is.
	Reading Reading

	Domains []DomainInputs

	// Register is every node's row and RegisterReadable whether it could
	// be listed. An unreadable register is rendered as such rather than
	// as an empty fleet: the second is a claim that no node exists.
	Register         []coord.NodePositions
	RegisterReadable bool

	Live       []Presence
	Tombstones []Tombstone

	Replica ReplicaReport

	// Maintenance is the open capacity operation, or nil.
	Maintenance *MaintenanceReport
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
	rep.Nodes = in.nodes()
	rep.Snapshots = in.snapshots()
	rep.Alarms = in.alarms(rep.Domains)
	rep.Maintenance = in.Maintenance
	// THE LEVEL IS RESOLVED FROM THIS NODE'S OWN LAG, not from the
	// fleet's: the document's every "this node" figure is a fact this
	// process states about itself, and a peer whose row could not be read
	// says nothing about whether THIS answer can claim an age.
	rep.ReadLevel = ResolveReplicationLevel(rep.lagKnownHere())
	return rep
}

// lagKnownHere reports whether this node's own distance from every domain's
// log came back as a number.
//
// EVERY DOMAIN, not any: a document that could state an age for the tracker
// and not for the knowledge base is one whose `stale` claim is true of half
// its rows, and a reader bounding staleness against it would be bounding
// nothing on the other half.
//
// A NODE WITH NO ROW IN THE REGISTER IS NOT A KNOWN LAG. That is the state
// during exactly the coordination outage this document is opened for, and
// reading it as "no domains reported, so nothing is unknown" would answer
// `stale` with no evidence at all.
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

// ExitNonZero is the CLI's exit code, and the reason it is a method on the
// REPORT rather than a rule in the CLI.
//
// `crewlet retention status` exits non-zero exactly when this node has an
// active alarm — the same evaluation that drives the gauges and the log
// lines. A second rule in the CLI would be a second definition of "is
// something wrong", and the shell script watching the exit code would be
// watching the one nobody maintained.
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
		Domain:            d.Domain,
		Stream:            d.Stream,
		Generation:        d.Generation,
		Replay:            d.Replay,
		FirstSeq:          d.FirstSeq,
		LastSeq:           d.LastSeq,
		Bytes:             d.Bytes,
		TrimFloor:         d.TrimFloor,
		TrimTo:            d.Decision.To,
		BlockedBy:         d.Decision.BlockedBy,
		SnapshotBlockedBy: d.SnapshotSkip,
	}
	if d.StreamReadable && d.MaxBytes > 0 {
		out.MaxBytes = d.MaxBytes
		free := float64(d.MaxBytes) - float64(d.Bytes)
		if free < 0 {
			free = 0
		}
		out.HeadroomFraction = Frac(free / float64(d.MaxBytes))
	}
	for _, t := range d.Decision.Terms {
		out.Terms = append(out.Terms, TermReport{
			Name:   t.Name,
			State:  termState(t),
			Seq:    t.Seq,
			Detail: t.Detail,
			Remedy: in.remedy(t.Name),
		})
	}
	if out.BlockedBy != "" {
		// A BLOCKED DOMAIN ALWAYS CARRIES AN INSTANT. A caller whose
		// tick is the first to see the block has none to give, and
		// this tick is the honest answer: the alarm then reports a
		// duration of zero, which is true, where an absent instant
		// would render as the zero time and read as "blocked since
		// year one".
		out.BlockedSince = d.BlockedSince.UTC()
		if d.BlockedSince.IsZero() {
			out.BlockedSince = in.At.UTC()
		}
		out.Prose = fmt.Sprintf("Nothing is being trimmed on %s: %s.",
			d.Domain, strings.TrimSuffix(d.Decision.Detail, "."))
	}
	return out
}

// termState is the three-valued rendering of one term.
//
// ABSENT IS CHECKED FIRST. A term a domain does not have is also a term
// nobody read, so the other order would report every compacted domain's wake
// feed as unreadable and block-looking on a screen where nothing is wrong.
func termState(t Term) TermState {
	switch {
	case t.Absent:
		return TermAbsent
	case !t.Known:
		return TermUnreadable
	default:
		return TermKnown
	}
}

// remedy is what an operator does about one term.
//
// # Why every term has one, including the healthy one
//
// Five of the six name something to fix. The sixth — the age floor — is the
// term that binds on a fleet where NOTHING is wrong, and saying so is the
// whole value of it: an operator who reads "age_floor" beside five terms with
// remedies and none beside it concludes the surface is incomplete. The
// healthy answer has to be written down as an answer.
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
			"backup owner is configured — set `retention.backup_owner` so the " +
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

	// THE COUNTED SET IS NOT RE-DERIVED HERE. It is the same function the
	// trim itself calls, over the same three inputs, so the screen can
	// never name a different fleet from the one the gate is waiting for.
	var flat []NodePosition
	for id, row := range reported {
		flat = append(flat, NodePosition{NodeID: id, At: row.At})
	}
	counted := make(map[string]bool)
	for _, n := range CountedSet(in.At, flat, in.Live, in.Tombstones) {
		counted[n.NodeID] = true
	}

	ids := make([]string, 0, len(reported)+len(live)+len(tombs))
	for id := range reported {
		ids = append(ids, id)
	}
	for id := range live {
		if _, seen := reported[id]; !seen {
			ids = append(ids, id)
		}
	}
	for id := range tombs {
		if _, seen := reported[id]; !seen && !live[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)

	lastSeq := make(map[string]uint64, len(in.Domains))
	readable := make(map[string]bool, len(in.Domains))
	for _, d := range in.Domains {
		lastSeq[d.Domain], readable[d.Domain] = d.LastSeq, d.StreamReadable
	}

	out := make([]NodeReport, 0, len(ids))
	for _, id := range ids {
		row := NodeReport{NodeID: id, Counted: counted[id], Live: live[id]}
		if r, ok := reported[id]; ok {
			row.At = r.At.UTC()
			for name, d := range r.Domains {
				nd := NodeDomainReport{
					Generation:     d.Generation,
					Seq:            d.Seq,
					AppliedThrough: d.AppliedThrough,
					Deferred:       d.Deferred,
				}
				// LAG IS COMPUTED, NEVER REPORTED BY THE NODE
				// ITSELF: a lagging node's own idea of the
				// stream's end is exactly the number it is
				// behind on.
				if readable[name] {
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
				// THE SAME COMPARISON [CountedSet] MAKES, to
				// the instant. A report that called a
				// tombstone effective one tick before the gate
				// did would show an evicted node dropped from
				// the counted set while the trim was still
				// waiting for it.
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
		// A NODE WITH NO ARTEFACT AND NO REASON still gets a row. The
		// absence is the operator's answer to "why did the join fail",
		// and dropping the row would render it as a node that was
		// never asked.
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b SnapshotReport) int {
		return strings.Compare(a.NodeID, b.NodeID)
	})
	return rows
}

// alarms evaluates this node's conditions once.
//
// # Why the per-domain half is evaluated separately
//
// [Reading] describes ONE node, and two of its conditions — the log's
// headroom and a blocked trim — are properties of a DOMAIN. Evaluating the
// node-wide reading once and each domain's own fields once per domain is what
// lets a two-domain fleet say WHICH log is filling. Folding them into one
// reading would report the worst of the two with no name on it, which is the
// number an operator then has to go and find by hand.
func (in ReportInputs) alarms(domains []DomainReport) []Alarm {
	out := Evaluate(in.Reading)
	for _, d := range domains {
		perDomain := Reading{
			HeadroomFraction: d.HeadroomFraction,
			TrimBlockedBy:    string(d.BlockedBy),
		}
		if d.BlockedBy != "" && !d.BlockedSince.IsZero() {
			perDomain.TrimBlockedFor = in.At.Sub(d.BlockedSince)
		}
		for _, a := range Evaluate(perDomain) {
			a.Detail = d.Domain + ": " + a.Detail
			out = append(out, a)
		}
	}
	return out
}

// JoinSecondsPerGB and JoinFixedSeconds project how long a peer needs to
// become a complete replica of a store of a given size.
//
// # Where the two numbers come from
//
// The per-gigabyte term is the CONSERVATIVE profile — a network volume rather
// than local NVMe — because a projection an operator sizes a rejoin window
// against must not be the optimistic one: a window that fits only the fast
// path is a window that expires during exactly the incident it was set for.
// The fast profile is about 2.3× better and is deliberately not what this
// reports.
//
// The fixed term is 25 s of setup — opening the estates, running the
// migrator, establishing the donor — plus the 18 s the read barriers add to a
// twenty-four-hour replay, which costs 793 fetches rather than 272 at about
// 35 ms each.
//
// # Why it is a projection and says so
//
// Nothing here is measured on this host. It is arithmetic over a size, and its
// job is to answer "is the operator's window plausible" rather than to predict
// a particular join — which is why [ReplicaReport] carries the window beside it
// and the surfaces render both.
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
