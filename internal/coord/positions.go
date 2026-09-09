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
	NodeID        string                    `json:"node_id"`
	At            time.Time                 `json:"at"`
	EngineVersion string                    `json:"engine_version,omitempty"`
	Domains       map[string]DomainPosition `json:"domains"`

	// SnapshotBytes is the size of the artefact those per-domain snapshot
	// positions came from.
	//
	// ON THE ROW RATHER THAN ON EACH DOMAIN, because a snapshot is ONE
	// file covering every domain: a byte count per domain would be the
	// same number written N times, and the first time they disagreed a
	// reader would have to decide which was the file.
	SnapshotBytes int64 `json:"snapshot_bytes,omitempty"`

	// SnapshotSkip is why this node holds no current snapshot, in its own
	// loop's words, and empty when it holds one.
	//
	// THE OPERATOR'S ANSWER TO "why can this node not donate", which is
	// the question a failed join raises and the one nothing else on this
	// row can answer: an absent snapshot position says a node has none and
	// is silent about whether that is a disk that filled, a node that is
	// lagging, or a loop that has simply not run yet.
	SnapshotSkip string `json:"snapshot_skip,omitempty"`
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

	// ForgetPositions removes a node's row.
	//
	// THE OPERATOR'S GESTURE, and the only way a row leaves. It is what an
	// eviction calls after its own record is written, so the trim stops
	// waiting for a node nobody is going to bring back.
	ForgetPositions(ctx context.Context, nodeID string) error
}

// PositionKey is a node's key in the register.
//
// THE REGISTER HOLDS FOUR KEY CLASSES — this one, [HoldKey], [BackupPointKey]
// and [FloorKey] — because all four answer the same question (what may the
// trim delete): the first three are its inputs and the fourth is its published
// answer. All four need the same retention, which is NONE. A listing over either class
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
	}
	return nil
}
