package coord

import (
	"context"
	"fmt"
	"time"
)

// Position is where a node stands in one domain's log.
//
// A TRIPLE rather than a sequence, and each part answers a question the other
// two cannot. Stream names the log — a recreated stream restarts sequences at
// 1, so a bare number from before the recreation compares as if it were
// current. Generation is that recreation's counter, which makes an old
// position COMPARABLE and safely stale rather than indistinguishable from a
// new one. Seq is the broker's own monotonic sequence within the generation.
type Position struct {
	Stream     string `json:"stream"`
	Generation uint32 `json:"generation"`
	Seq        uint64 `json:"seq"`
}

// DomainPosition is what one node has done with one domain's log.
//
// # Why Seq and AppliedThrough are two numbers
//
// Seq is the record this node has CONSUMED — its checkpoint, the contiguous
// prefix it has moved over. AppliedThrough is the prefix it has actually
// APPLIED, which is lower whenever a record was retained rather than applied
// (a version this build cannot read). Collapsing them would make a node that
// is applying nothing while its position advances look identical to one that
// is fully caught up, and the trim reads the first while a reader's coverage
// question reads the second.
type DomainPosition struct {
	Seq            uint64 `json:"seq"`
	Generation     uint32 `json:"generation"`
	AppliedThrough uint64 `json:"applied_through"`

	// StreamCreatedAt is the creation instant of the stream this node's
	// checkpoint is KEYED TO — the one its rows were derived from, which is
	// not the one the broker serves while this node knows its log was
	// rebuilt under it.
	//
	// THE THIRD COORDINATE OF A POSITION, and the one a generation cannot
	// stand in for. A stream deleted and remade keeps the generation and
	// counts from 1 again, so two rows at one generation are comparable
	// only if they are about one stream — and a reanchor's guards compare
	// exactly that: whether a peer went further along the stream this
	// node's rows came from. Without it every peer still on the lost
	// stream read as one caught up on the live one, and no node of a
	// fleet could ever re-anchor.
	//
	// ZERO IS UNKNOWN, never "no stream": a row written by a build that
	// did not publish it, which a reader weighs conservatively.
	StreamCreatedAt time.Time `json:"stream_created_at,omitzero"`

	// CheckpointStoredAt is the broker's instant for the record at this
	// node's checkpoint — the one it consumed at Seq — and with Seq it NAMES
	// that record, which a sequence alone cannot: after a broker restored
	// from an older copy is written past a node's rows, the same sequence
	// holds another record in the log than in those rows.
	//
	// WHAT IT LETS A PEER ASK is whether this node's history is the log's.
	// A node whose checkpoint record the log holds is on the log, whatever
	// its sequence — nothing it holds is lost by a reanchor that follows the
	// log — while one whose record the log does not hold, or holds another
	// record at, holds history the log lost; only the second is weighed
	// against a reanchoring node's own position
	// ([statelog.ReanchorInputs.Highest]).
	//
	// ZERO IS UNKNOWN — a checkpoint naming no record, or a build that did
	// not publish it — and a reader weighs it as history the log may not
	// hold.
	CheckpointStoredAt time.Time `json:"checkpoint_stored_at,omitzero"`

	// Snapshot is the newest VERIFIED snapshot this node holds, and its
	// generation travels with it: a snapshot from before a reanchor is not
	// a donor for a node that needs one after it, and a bare sequence
	// could not say so.
	SnapshotSeq        uint64    `json:"snapshot_seq,omitempty"`
	SnapshotGeneration uint32    `json:"snapshot_generation,omitempty"`
	SnapshotAt         time.Time `json:"snapshot_at,omitzero"`

	// Deferred is how many records this node has retained rather than
	// applied. Zero on a healthy node, and the number an operator reads
	// beside AppliedThrough to tell a lagging node from a stalled one.
	Deferred int `json:"deferred,omitempty"`

	// RecordVersion is the highest record version this node's build reads
	// on the domain's log — what a writer asks before it publishes a record
	// no older build can even DEFER.
	//
	// Deferral is a rolling upgrade's contract for a record a peer cannot
	// apply, and it rests on the peer reading the record's ENVELOPE; a kind
	// a build's envelope refuses stops that build's applier instead. So a
	// writer about to publish such a kind reads this across every node the
	// log counts, and waits for the last to advertise a build that reads it.
	//
	// ZERO IS "NOT ADVERTISED" — a row written by a build that predates the
	// field — and a reader weighs it as a build that reads nothing newer
	// than the field, never as one that reads everything.
	RecordVersion int `json:"record_version,omitempty"`

	// LogDiverged reports that the log holds, at this node's checkpoint,
	// another record than the one it consumed there: a broker restored from
	// an older copy and written past this node's rows, so they hold history
	// the log lost and the log holds history they never saw.
	//
	// PUBLISHED because it is the one such node the fleet cannot otherwise
	// see: its position is at or below the log's end, where a lagging node's
	// is, so nothing else on this row says its rows and the log are two
	// histories — and every other node on that log refuses its own writes of
	// it while this says so, as it does for a peer whose position is past the
	// log's end, until the operator decides which history the fleet keeps.
	LogDiverged bool `json:"log_diverged,omitempty"`

	// State is what this node is doing with the log, EMPTY while it runs it
	// — which is every log of every row a build before the field wrote, and
	// every log of a layout-0 row, byte for byte.
	//
	// [LogReleased] is a log this node has LEFT: its release is on the log
	// and applied, and the node is about to stop naming it. The trim stops
	// counting the node on the log the moment the row says so, rather than
	// once the row forgets the log, because between the two the position is
	// one the node will never move again — and a counted position that never
	// moves pins the log it names for as long as the row keeps it.
	State LogState `json:"state,omitempty"`
}

// LogState is what a node says it is doing with one log on its positions row.
//
// THE ZERO VALUE IS A STATE — running the log, which is what naming it has
// always meant — so a row that says nothing about a log's state says the one
// thing every earlier row meant.
type LogState string

// LogReleased is a log the node has left: see [DomainPosition.State].
const LogReleased LogState = "released"

// Valid reports whether a node may WRITE this state: running (the zero value)
// or released. A reader meeting a state it does not know — a newer build's —
// counts the node as running the log, the direction that keeps a tail rather
// than trims past one.
func (s LogState) Valid() bool { return s == "" || s == LogReleased }

// PartitionReport is what a node says about one PARTITION it holds, beside its
// logs' positions: how far its copy is, and its snapshot of the partition.
//
// # Why the snapshot is reported per partition
//
// A snapshot is a copy of ONE FILE, and every partition is a file of its own,
// so a node holding many partitions holds one artefact per partition, each
// taken — or skipped, for a reason of its own — on its own schedule. One byte
// count and one skip reason per row would describe whichever partition's
// snapshot the loop took last and say nothing about the rest.
//
// UNDER LAYOUT 0 A ROW CARRIES NONE: layout 0's one partition is the whole
// estate, and its report is the row's own [NodePositions.SnapshotBytes] and
// [NodePositions.SnapshotSkip], which is where every row a build before the
// partition wrote carries it.
type PartitionReport struct {
	// State is the node's own account of its copy of the partition — the
	// state its estate lease names for it (estate/partmap's
	// PartitionState, whose wire value this is).
	State string `json:"state"`

	// SnapshotBytes is the size of the node's newest verified artefact of
	// the partition, and zero when it holds none.
	SnapshotBytes int64 `json:"snapshot_bytes,omitempty"`

	// SnapshotSkip is why the node holds no CURRENT artefact of the
	// partition, in its snapshot loop's own words, and empty when it holds
	// one — the operator's answer to "why can this node not donate this
	// partition" ([NodePositions.SnapshotSkip] says why).
	SnapshotSkip string `json:"snapshot_skip,omitempty"`
}

// NodePositions is one node's row in the register: every domain it runs, and
// when it last said so.
//
// # Why one key per node rather than one bucket per domain
//
// A second domain costs a MAP ENTRY here. The alternative — a bucket per
// domain — costs a bucket, its retention decision, its replica count and a
// place in every sweep, for a fact that is always read together anyway: the
// trim asks "what has every node applied", and it asks it about every domain
// at once.
type NodePositions struct {
	NodeID        string    `json:"node_id"`
	At            time.Time `json:"at"`
	EngineVersion string    `json:"engine_version,omitempty"`

	// Layout is the number of the layout this node's logs are in — which
	// is what makes the keys below name ONE log each.
	//
	// A log's key does not carry its layout ([statelog.LogID.String]):
	// layout 1's `tracker@tracker.007` and a repartitioned layout 2's are
	// one string, so a row keyed by it says which log it means only with
	// the layout beside it. Read through [PositionsIn], which empties a
	// row of another layout. OMITTED AT ZERO, so a layout-0 row is the one
	// every build before the field wrote, byte for byte.
	Layout int `json:"layout,omitempty"`

	// MapEpoch is the estate map epoch this row ACTED on: the map whose
	// holder table the partitions below are this node's answer to. OMITTED
	// AT ZERO, which is every layout-0 row — that layout has no map.
	MapEpoch uint64 `json:"map_epoch,omitempty"`

	// Domains is this node's position on each log it runs, keyed by the
	// log's key — under layout 0 the domain's own name, which is the key
	// this map has always had.
	Domains map[string]DomainPosition `json:"domains"`

	// Partitions is this node's report on each partition it holds, keyed by
	// the partition's name — at every layout but 0, whose one partition's
	// report is the two fields below ([PartitionReport]).
	Partitions map[string]PartitionReport `json:"partitions,omitempty"`

	// SnapshotBytes is the size of the artefact those per-domain snapshot
	// positions came from — LAYOUT 0's ONE PARTITION'S, the whole estate;
	// a row of any other layout reports each partition's artefact in
	// [NodePositions.Partitions] and leaves this empty.
	//
	// ON THE ROW RATHER THAN ON EACH DOMAIN, because a snapshot is ONE
	// file covering every domain of its partition: a byte count per
	// domain would be the same number written N times, and the first time
	// they disagreed a reader would have to decide which was the file.
	SnapshotBytes int64 `json:"snapshot_bytes,omitempty"`

	// SnapshotSkip is why this node holds no current snapshot, in its own
	// loop's words, and empty when it holds one — layout 0's, as
	// SnapshotBytes is.
	//
	// THE OPERATOR'S ANSWER TO "why can this node not donate", which is
	// the question a failed join raises and the one nothing else on this
	// row can answer: an absent snapshot position says a node has none and
	// is silent about whether that is a disk that filled, a node that is
	// lagging, or a loop that has simply not run yet.
	SnapshotSkip string `json:"snapshot_skip,omitempty"`
}

// Report is what this row says about partition — its [PartitionReport] — and
// false when it says nothing: layout 0's one partition's report is the row's
// own snapshot fields, which are named for any partition a layout-0 row is
// asked about (that layout has one), and every other layout's is its entry in
// [NodePositions.Partitions]. The state of layout 0's is not on the row.
func (p NodePositions) Report(partition string) (PartitionReport, bool) {
	if p.Layout == 0 {
		return PartitionReport{SnapshotBytes: p.SnapshotBytes, SnapshotSkip: p.SnapshotSkip}, true
	}
	r, ok := p.Partitions[partition]
	return r, ok
}

// PositionsIn is the register as a reader running layout reads it: every row,
// and in a row of ANOTHER layout no position at all.
//
// THE ONE PLACE THE LAYOUT IS READ, and every read of the register passes
// through it, so no reader keyed by a log's key can take a position of
// another layout's log spelled the same way for its own. The row itself
// stays: its node is still a node — counted, present, a participant — and
// only its positions are about logs this reader does not run.
func PositionsIn(rows []NodePositions, layout int) []NodePositions {
	out := make([]NodePositions, len(rows))
	for i, row := range rows {
		if row.Layout != layout {
			row.Domains = nil
		}
		out[i] = row
	}
	return out
}

// PositionRegister is the fleet's record of where every node stands.
//
// # It has no TTL, and that is the sharpest retention decision in the estate
//
// Every other bucket's age answers a question about staleness. This one's
// would answer a question about DELETION: the trim reads these positions to
// decide what records every node has finished with, so a key that expired
// would read as a node that has applied NOTHING — which pins the trim for
// ever, or, read the other way round, lets it delete records that node still
// needs. An absent node is removed by an operator's audited gesture, never by
// a clock.
type PositionRegister interface {
	// PutPositions writes this node's row, replacing it wholesale.
	//
	// LAST WRITER WINS AND THAT IS CORRECT: the only writer of a node's row
	// is that node, so there is no race to arbitrate — and a
	// compare-and-set here would make a heartbeat fail on a value only
	// this process can have changed.
	PutPositions(ctx context.Context, p NodePositions) error

	// Positions reads every node's row.
	//
	// The whole register, because every caller wants it: the trim takes a
	// minimum across nodes, the fleet view renders a row per node, and a
	// donor search picks the newest snapshot. A per-node read would be N
	// round trips for one answer.
	Positions(ctx context.Context) ([]NodePositions, error)

	// ForgetPositions removes a node's row, and it is the only way a row
	// leaves.
	//
	// AN EVICTION DOES NOT CALL IT, and must not. What stops the trim
	// waiting for an evicted node is its TOMBSTONE, which the counted set
	// subtracts from these keys; the row itself is what a READMISSION is
	// judged by — the node's last position against the floor — so an
	// eviction that forgot it would turn every later readmission of a
	// machine that is still switched off into a judgement about a node
	// that has never reported anything.
	ForgetPositions(ctx context.Context, nodeID string) error
}

// PositionKey is a node's key in the register.
//
// THE REGISTER HOLDS SEVEN KEY CLASSES, and every one needs the same
// retention, which is NONE.
//
// Four are the trim's: this one, [HoldKey] and [BackupPointKey] are its
// inputs, and [FloorKey] is its published answer. Three are the capacity
// window's: [MaintenanceKey], whose existence IS the exclusion,
// [AdmissionKey], which is one node's positive record that it intends to
// publish, and [MaintenanceAckKey], which is one participant's evidence that
// its process restarted.
//
// They share a bucket because they share that retention and because a bucket
// is a stream with a replica count, a place in every sweep and a retention
// decision of its own. What none of them may have is an AGE: an expiring
// position reads as a node that has applied nothing, an expiring operation
// admits every publisher in the fleet, an expiring admission hides one, and an
// expiring acknowledgement un-seals a barrier that has already run. A listing over either class
// filters on the first segment, and that filter is load-bearing rather than
// tidy: a hold decoded as a positions row is a node id of "" with a domains
// map of zero values, which the trim reads as a node that has applied nothing
// and pins the floor at zero for ever.
func PositionKey(nodeID string) string { return DocumentKey("node", nodeID) }

// Validate reports why a row cannot be written.
//
// A row with no node id is unattributable — it would be written under whatever
// key the caller passed and read back as somebody else's progress, which is
// the one error here that corrupts another node's answer rather than this
// one's.
func (p NodePositions) Validate() error {
	if p.NodeID == "" {
		return fmt.Errorf("coord: a positions row needs a node id: an "+
			"unattributed row is read as some other node's progress, and the "+
			"trim takes a minimum across them (%d domain(s) offered)",
			len(p.Domains))
	}
	if p.Layout < 0 {
		return fmt.Errorf("coord: node %s reports layout %d, and a layout number "+
			"counts repartitions from 0", p.NodeID, p.Layout)
	}
	for name, d := range p.Domains {
		if name == "" {
			return fmt.Errorf("coord: node %s offered a position for an unnamed "+
				"domain", p.NodeID)
		}
		if d.AppliedThrough > d.Seq {
			return fmt.Errorf("coord: node %s reports domain %q applied through "+
				"%d with a checkpoint of %d: a node cannot have applied past "+
				"what it has consumed", p.NodeID, name, d.AppliedThrough, d.Seq)
		}
		if !d.State.Valid() {
			return fmt.Errorf("coord: node %s reports the log %q in the state %q; "+
				"a log is running (no state) or %q", p.NodeID, name, d.State, LogReleased)
		}
	}
	// ONE PLACE PER LAYOUT FOR A PARTITION'S REPORT. Layout 0's is the row's
	// own snapshot fields — where every earlier build wrote it, so a layout-0
	// row stays the bytes they wrote — and every other layout's is its entry
	// in Partitions. A row with both would carry two answers for one
	// partition, and a reader would take whichever it happened to look at.
	switch {
	case p.Layout == 0 && (len(p.Partitions) > 0 || p.MapEpoch != 0):
		return fmt.Errorf("coord: node %s reports layout 0 with a map epoch or a "+
			"per-partition report; layout 0 has no map, and its one partition's "+
			"report is the row's own snapshot fields", p.NodeID)
	case p.Layout != 0 && (p.SnapshotBytes != 0 || p.SnapshotSkip != ""):
		return fmt.Errorf("coord: node %s reports layout %d with a snapshot on the "+
			"row; a partitioned layout's snapshots are reported per partition",
			p.NodeID, p.Layout)
	}
	for name := range p.Partitions {
		if name == "" {
			return fmt.Errorf("coord: node %s reports an unnamed partition", p.NodeID)
		}
	}
	return nil
}
