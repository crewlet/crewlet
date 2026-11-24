package coord

import (
	"context"
	"fmt"
	"time"
)

// Position is where a node stands in one domain's log: a TRIPLE, not a
// sequence. Stream names the log (a recreated stream restarts at 1, so a bare
// number from before compares as current); Generation counts recreations, so
// an old position is comparable and safely stale; Seq is the broker's
// sequence within the generation.
type Position struct {
	Stream     string `json:"stream"`
	Generation uint32 `json:"generation"`
	Seq        uint64 `json:"seq"`
}

// DomainPosition is what one node has done with one domain's log.
//
// Seq is the record it has CONSUMED (its checkpoint); AppliedThrough the
// prefix it has APPLIED, lower whenever a record was retained (a version this
// build cannot read). One number would make a node applying nothing look
// caught up; the trim reads Seq, a reader's coverage question AppliedThrough.
type DomainPosition struct {
	Seq            uint64 `json:"seq"`
	Generation     uint32 `json:"generation"`
	AppliedThrough uint64 `json:"applied_through"`

	// StreamCreatedAt is the creation instant of the stream this node's
	// checkpoint is KEYED TO, which is not the one the broker serves while
	// this node knows its log was rebuilt under it. A generation cannot stand
	// in for it: a stream deleted and remade keeps the generation and counts
	// from 1, and a reanchor's guards ask whether a peer went further on the
	// stream this node's rows came from; without it every peer on the lost
	// stream reads as caught up on the live one, and no node could re-anchor.
	// ZERO IS UNKNOWN, never "no stream" (this build always publishes it,
	// [statelog.Runner.KeyedTo]): a reader never counts such a row as on its own.
	StreamCreatedAt time.Time `json:"stream_created_at,omitzero"`

	// CheckpointStoredAt is the broker's instant for the record at this
	// node's checkpoint, and with Seq it NAMES that record: after a broker
	// restored from an older copy is written past a node's rows, one sequence
	// holds different records in the log and in the rows. A peer asks with it
	// whether this node's history is the log's: only a node whose record the
	// log lacks or holds differently is weighed against a reanchoring node's
	// position ([statelog.ReanchorInputs.Highest]). ZERO IS UNKNOWN, weighed
	// as history the log may not hold.
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

	// RecordVersion is the highest record version this node's build reads on
	// the domain's log. Deferral, a rolling upgrade's contract for a record a
	// peer cannot apply, rests on the peer reading the ENVELOPE; a kind the
	// envelope refuses stops the applier. So a writer about to publish such a
	// kind waits until every node the log counts advertises a build that
	// reads it. ZERO IS "NOT ADVERTISED" (a node counted from its presence
	// before its first report), weighed as a build that reads nothing newer,
	// never as one that reads everything.
	RecordVersion int `json:"record_version,omitempty"`

	// LogDiverged reports that the log holds, at this node's checkpoint,
	// another record than the one it consumed: a broker restored from an
	// older copy was written past this node's rows, so each holds history the
	// other lacks. PUBLISHED because nothing else on the row shows it (its
	// position is where a lagging node's is), and every other node on that
	// log refuses its own writes while this says so, as for a peer past the
	// log's end, until the operator picks the history the fleet keeps.
	LogDiverged bool `json:"log_diverged,omitempty"`
}

// NodePositions is one node's row in the register: every domain it runs, and
// when it last said so. One key per node, not a bucket per domain: a domain
// costs a map entry rather than a bucket with its own retention, replicas and
// sweep, and the trim asks about every domain at once anyway.
type NodePositions struct {
	NodeID string `json:"node_id"`

	// At is when the node said so, by ITS OWN clock: the writer stamps it,
	// and where a writer left it zero the store's client fills it, in the
	// writer's process and from the same clock.
	At            time.Time `json:"at"`
	EngineVersion string    `json:"engine_version,omitempty"`

	// StoredAt is the instant the store took the row this one was read from,
	// by the STORE's clock (on the KV backend the stream leader's), filled by
	// every [PositionRegister.Positions] and never written, so no writer can
	// claim what the broker's clock read. Beside At (the writer's clock at the
	// same write) every row says how far its writer's clock stands from the
	// broker's, and since one broker clock stores every row, how far nodes'
	// clocks stand from each other: the engine's `clock_skew` alarm
	// (internal/engine, clockskew.go). ZERO ON A ROW NOTHING READ BACK, which a
	// reader takes as nothing measured.
	StoredAt time.Time `json:"-"`

	// Domains is this node's position on each log it runs, keyed by the
	// log's domain name.
	Domains map[string]DomainPosition `json:"domains"`

	// SnapshotBytes is the size of the artefact the per-domain snapshot
	// positions came from, the node's one artefact of the replicated estate.
	// On the row, not each domain: a snapshot is ONE file covering every
	// domain, and N copies of its size could disagree.
	SnapshotBytes int64 `json:"snapshot_bytes,omitempty"`

	// SnapshotSkip is why this node holds no current snapshot, in its own
	// loop's words, empty when it holds one: the operator's answer to "why can
	// this node not donate" after a failed join, which an absent snapshot
	// position cannot give (a full disk, a lag, a loop not yet run).
	SnapshotSkip string `json:"snapshot_skip,omitempty"`
}

// PositionRegister is the fleet's record of where every node stands.
//
// NO TTL: the trim reads these rows to decide what every node has finished
// with, so an expired row reads as a node that applied NOTHING, pinning the
// trim for ever or letting it delete records that node still needs. And NO ROW
// EVER LEAVES, by clock or by anybody: an absent node stops pinning the trim
// through an audited EVICTION's tombstone ([statelog.CountedSet], once older
// than the fence window), and its row is what a READMISSION is judged by
// ([statelog.PermitReadmission]), so a removal verb would invite wiring it into
// the eviction. The register is bounded by the node ids a deployment has ever
// configured: each node replaces its own row, so only a retired id leaves one.
type PositionRegister interface {
	// PutPositions writes this node's row, replacing it wholesale. Last writer
	// wins, correctly: only the node writes its row, so a compare-and-set
	// would only fail a heartbeat on a value no other process changes.
	PutPositions(ctx context.Context, p NodePositions) error

	// Positions reads every node's row, each with the instant the store took
	// it ([NodePositions.StoredAt]). The whole register, because every caller
	// wants it (the trim's minimum, the fleet view, a donor search); per-node
	// reads would be N round trips for one answer.
	Positions(ctx context.Context) ([]NodePositions, error)
}

// MaxEngineVersionBytes bounds the engine version a row names: a release's
// tag or a pseudo-version (internal/version), a few dozen bytes.
const MaxEngineVersionBytes = 128

// MaxSnapshotSkipBytes bounds the snapshot loop's skip reason a row names: a
// word of internal/statelog's closed SkipReason table.
const MaxSnapshotSkipBytes = 64

// PositionKey is a node's key in the register.
//
// The register holds every key class whose retention is NONE, and only those:
// the trim's inputs and answer, the capacity window's records, and records a
// decision or their own stamp ends (each class's doc says what an age would
// break). They share one bucket
// because each bucket costs a replica count, a sweep and a retention decision.
// A listing over any class MUST filter on the first segment: a hold decoded as
// a positions row is a node id of "" with zero domains, pinning the trim's
// floor at zero for ever. The chat read state wants the same retention but is
// NOT a class here: the busiest record would order every flush through the raft
// group serving the trim ([ChatReads], chatread.go's "Why a bucket of its own").
func PositionKey(nodeID string) string { return DocumentKey(PositionClass, nodeID) }

// PositionClass is the key class a node's row is filed under in the register.
const PositionClass = "node"

// Validate reports why a row cannot be written. A row with no node id would
// read back as somebody else's progress: the one error here that corrupts
// another node's answer rather than this one's.
func (p NodePositions) Validate() error {
	if p.NodeID == "" {
		return fmt.Errorf("coord: a positions row needs a node id: an "+
			"unattributed row is read as some other node's progress, and the "+
			"trim takes a minimum across them (%d domain(s) offered)",
			len(p.Domains))
	}
	switch {
	case !plainWithin(p.NodeID, MaxNodeIDBytes):
		return fmt.Errorf("coord: a positions row's node id is no node id: at most %d plain bytes",
			MaxNodeIDBytes)
	case len(p.Domains) > MaxLogDomains:
		return fmt.Errorf("coord: node %s offers positions on %d logs, past the %d a row holds",
			p.NodeID, len(p.Domains), MaxLogDomains)
	case !plainWithin(p.EngineVersion, MaxEngineVersionBytes):
		return fmt.Errorf("coord: node %s's engine version is past %d plain bytes", p.NodeID,
			MaxEngineVersionBytes)
	case !plainWithin(p.SnapshotSkip, MaxSnapshotSkipBytes):
		return fmt.Errorf("coord: node %s's snapshot skip is no skip reason: at most %d plain bytes",
			p.NodeID, MaxSnapshotSkipBytes)
	}
	for name, d := range p.Domains {
		if !ValidDomainName(name) {
			return fmt.Errorf("coord: node %s offered a position for a domain that is no "+
				"domain's name: %q", p.NodeID, name)
		}
		if d.AppliedThrough > d.Seq {
			return fmt.Errorf("coord: node %s reports domain %q applied through "+
				"%d with a checkpoint of %d: a node cannot have applied past "+
				"what it has consumed", p.NodeID, name, d.AppliedThrough, d.Seq)
		}
	}
	return nil
}
