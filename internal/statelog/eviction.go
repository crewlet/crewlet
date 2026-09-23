package statelog

import (
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// EvictionFenceWindow is how long an evicted node stays COUNTED after its
// tombstone lands.
//
// # Why an eviction is two-phase at all
//
// Dropping a node from the counted set is what lets the trim advance past its
// position — and the moment it does, that node's own writes become records
// every applier will drop, while it may still believe it holds everything it
// held. The window is how long the fleet waits for it to notice.
//
// FOUR HEARTBEATS, the same derivation every other cached coordination fact in
// this engine uses: a node may miss three beats and still be healthy, so a
// tombstone older than four has been available for at least one beat it should
// have read.
//
// # Nominally, and the honest worst case is zero
//
// A node three beats late reads its tombstone exactly when the trim may pass
// it. So the window is a CONVENIENCE rather than the safety property — the
// safety property is the applier's own eviction gate, which drops an evicted
// node's records whatever it manages to publish and depends on nothing but the
// log's own order. Widening the window buys nothing that gate does not already
// give.
const EvictionFenceWindow = 4 * coord.ReconcileInterval

// Tombstone is an eviction as coordination holds it.
type Tombstone struct {
	// NodeID is who was evicted.
	NodeID string

	// At is when the tombstone was written.
	At time.Time

	// By is the operator who ran it, which is what a refusal names.
	By string

	// Generation is the domain's generation at the time. One from a
	// previous generation is UNKNOWN rather than old, on the same rule
	// every other position-bearing term uses.
	Generation uint32
}

// EvictionRow is one node's standing on ONE identity-claiming domain's log, as
// that domain's own applied rows hold it — the APPLIED shape, where each
// domain's eviction record is what wrote it.
//
// # Why every domain answers for its own log
//
// An eviction is a record on a log like any other, keyed on a position in that
// log's own sequence space, so every node applies it into that domain's table
// and every node's copy is identical — which is what makes the applier's
// eviction gate hold when coordination cannot be reached at all. The trim reads
// the same rows for a different question: a node stops being counted on a log
// once its tombstone THERE is older than the fence window. Asked of another
// domain's table, the answer is about another number space; the trim asked the
// tracker's table on behalf of the pages log, filtered on the pages stream,
// found nothing, and counted an evicted node on that log for ever.
type EvictionRow struct {
	// NodeID is who was evicted.
	NodeID string

	// At is the broker's own instant for the eviction record — what the
	// fence window is measured from, identical on every node — and By the
	// operator who ran it.
	At time.Time
	By string

	// From is the COMPOSED position the eviction takes effect above, and
	// Readmitted the one a readmission takes effect at. A readmission is
	// an inverse commit rather than a delete, so a node that has been
	// taken back still has a row, and Back says so.
	From       uint64
	Readmitted uint64
	Back       bool
}

// Presence is a node holding a live lease, whether or not it has reported a
// position yet.
type Presence struct {
	NodeID string
}

// CountedSet is who the trim counts: the positions register's own keys, UNION
// the live presence leases, MINUS any eviction tombstone older than the fence
// window.
//
// # Each of the three does something the others cannot
//
// The register is what carries a POSITION, and its keys never expire — which
// is deliberate, and is why an offline node pins the floor rather than
// vanishing from it.
//
// The presence leases are what catch a node between boot and its first
// heartbeat — which is exactly a node adopting a snapshot. It counts at
// position ZERO and blocks every term derived from the set, for at most one
// heartbeat, and the operator surface renders it as counted with no position
// yet so the block has a visible cause.
//
// The tombstones are what let an operator advance a floor an absent node is
// pinning, and they take effect only after the window — because a node that
// has not yet noticed is a node still writing.
func CountedSet(now time.Time, reported []NodePosition, live []Presence, tombs []Tombstone) []NodePosition {
	byID := make(map[string]NodePosition, len(reported)+len(live))
	for _, n := range reported {
		byID[n.NodeID] = n
	}
	for _, p := range live {
		if _, known := byID[p.NodeID]; !known {
			// A NODE WITH A LIVE LEASE AND NO POSITION YET COUNTS AT
			// ZERO and blocks. It is a node between boot and its first
			// heartbeat — which is a node adopting a snapshot — and
			// treating it as absent would let the trim advance past
			// the tail it is about to replay.
			byID[p.NodeID] = NodePosition{NodeID: p.NodeID}
		}
	}
	for _, tomb := range tombs {
		if now.Sub(tomb.At) > EvictionFenceWindow {
			delete(byID, tomb.NodeID)
		}
	}
	out := make([]NodePosition, 0, len(byID))
	for _, n := range byID {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// EvictionRefusal reports why an eviction was not permitted.
type EvictionRefusal struct {
	NodeID string
	Detail string
}

func (e *EvictionRefusal) Error() string {
	return fmt.Sprintf("statelog: %s may not be evicted: %s", e.NodeID, e.Detail)
}

// PermitEviction decides whether a node may be evicted at all.
//
// # A LIVE LEASE REFUSES, unless the operator overrides it
//
// A node holding a presence lease has renewed it within the last few
// coordination round trips: it is reaching the fleet and, almost always,
// running. Its position is advancing, so it pins nothing its own progress will
// not release — and evicting it drops everything it writes above the eviction
// on every applier, stops its own writes the moment its applier reaches the
// eviction, and moves its seats. Eviction is the gesture for a node that is NOT
// coming back, and a live lease is the fleet's own evidence that this one is;
// the refusal is what stops a mistyped node id taking a healthy machine out.
//
// It is about safety rather than permission, and an operator can know something
// the lease does not say — a node wedged in a way that still renews — which is
// what force is for. Nothing about the fleet's data rests on it either way: the
// applier's eviction gate drops the evicted node's records whatever it manages
// to publish, and its own fence refuses its writes from its own applied rows,
// the one source still fresh when its coordination path is wedged.
//
// ASKED ONCE PER GESTURE, before any log is written — by the engine's node
// gate, over every identity-claiming log — because judged per log it could
// reach two answers about one node.
func PermitEviction(nodeID string, live []Presence, force bool) error {
	if nodeID == "" {
		return &EvictionRefusal{Detail: "no node was named"}
	}
	held := slices.ContainsFunc(live, func(p Presence) bool { return p.NodeID == nodeID })
	if held && !force {
		return &EvictionRefusal{NodeID: nodeID, Detail: "it holds a live presence " +
			"lease, so it is still reaching coordination and almost certainly " +
			"running — an eviction drops every record it writes on every node and " +
			"moves its seats, which is the gesture for a node that is not coming " +
			"back"}
	}
	return nil
}

// Remedy is what the operator does about an eviction refusal: wait for the
// lease to lapse, or force the eviction past it.
//
// IN NO SURFACE'S VOCABULARY — see [GateAction]: the command line renders
// [GateForce] as -force and the dashboard as a separately confirmed control,
// and a sentence spelling the flag was rendered word for word by a screen that
// has none.
func (e *EvictionRefusal) Remedy() GateRemedy {
	if e.NodeID == "" {
		return GateRemedy{Detail: "name the node to evict"}
	}
	return GateRemedy{
		Actions: []GateAction{GateWait, GateForce},
		Detail: fmt.Sprintf("stop %s and wait for its presence lease to lapse — "+
			"the retention report stops showing it live — then run the eviction "+
			"again; or, if it is wedged in a way that still renews its lease, "+
			"force the eviction past the lease", e.NodeID),
	}
}

// ReadmissionBound is one identity-claiming domain's bound, as the node that
// is asked to write a readmission reads it.
//
// THE SAME TWO WITNESSES THE WRITE FENCE TAKES THE HIGHER OF, for the reason
// the floor theorem gives: the published floor covers a purge in flight and
// one the stream's own answer has not caught up with, and the stream's first
// sequence covers what the floor cannot see — a floor published at another
// generation reads as zero. A readmission judged against either alone would
// clear a node the fence it is about to be subject to refuses.
type ReadmissionBound struct {
	// Domain names the log, as the positions register keys it.
	Domain string

	// Generation is the generation the asking node runs this log at, and
	// the one Floor was read at: a published floor at a LOWER generation
	// names a dead number space and reads as zero, which First then
	// covers.
	Generation uint32

	// Floor is the fleet's published [coord.TrimFloor.Floor] and First the
	// log's own first surviving sequence.
	Floor uint64
	First uint64
}

// Held is the higher of the two: every record below it may already be gone
// from the log, so a node has to have applied every record up to the one just
// before it — [Replayable]'s boundary.
func (b ReadmissionBound) Held() uint64 { return max(b.Floor, b.First) }

// ReadmissionRefusal reports why an evicted node may not be taken back yet.
//
// A TYPED ERROR CARRYING EVERY NUMBER the refusal is about, because the
// inequality IS the reason and the operator acts on the numbers: how far the
// node is behind decides whether to wait for its applier or to go and find out
// why it has not adopted a snapshot.
type ReadmissionRefusal struct {
	// NodeID is who was refused and Domain the first log, in the register's
	// own order, that refused it.
	NodeID string
	Domain string

	// Published reports whether the node has a row in the positions
	// register at all. One that has never published is judged as holding
	// nothing — the reading [CountedSet] gives a node with a live lease and
	// no row — and the refusal says so rather than printing a zero that
	// reads as a position it reported.
	Published bool

	// Generation and Seq are the node's own last published position in
	// Domain, and ReportedAt the heartbeat that carried it. Zero when
	// Published is false.
	Generation uint32
	Seq        uint64
	ReportedAt time.Time

	// Bound is the domain as the asking node read it.
	Bound ReadmissionBound
}

func (e *ReadmissionRefusal) Error() string {
	gone := fmt.Sprintf("records below %d may already be gone from the %s log "+
		"(published floor %d, first surviving sequence %d)",
		e.Bound.Held(), e.Domain, e.Bound.Floor, e.Bound.First)
	// THE HEARTBEAT'S OWN INSTANT, because the position is a heartbeat old
	// at best: a node that adopted a snapshot a moment ago is refused on
	// the row it wrote before, and the instant is what says so.
	reported := ""
	if !e.ReportedAt.IsZero() {
		reported = " (reported " + e.ReportedAt.UTC().Format(time.RFC3339) + ")"
	}
	switch {
	case !e.Published:
		return fmt.Sprintf("statelog: %s may not be readmitted: it has never "+
			"published a position, so as far as the fleet knows it holds "+
			"nothing, and %s", e.NodeID, gone)
	case e.Generation < e.Bound.Generation:
		return fmt.Sprintf("statelog: %s may not be readmitted: its last position "+
			"in %s%s is %d at generation %d and the log is at generation %d, a "+
			"sequence space that no longer exists — nothing it holds can be "+
			"compared with anything the log still has",
			e.NodeID, e.Domain, reported, e.Seq, e.Generation, e.Bound.Generation)
	}
	return fmt.Sprintf("statelog: %s may not be readmitted: its last position in "+
		"%s%s is %d and %s — it has to catch up before the fleet counts it again",
		e.NodeID, e.Domain, reported, e.Seq, gone)
}

// Remedy is what the operator does about it.
//
// NOTHING THAT FORCES IT. A node below the floor repairs itself — it replays
// while the log still holds what it is missing, and adopts a peer's snapshot
// where it does not, at boot or on the heartbeat that finds it there — and a
// readmission is the gesture that follows that repair rather than one that
// stands in for it.
func (e *ReadmissionRefusal) Remedy() GateRemedy {
	return GateRemedy{
		Actions: []GateAction{GateWait},
		Detail: fmt.Sprintf("start %s if it is not running: it catches up on its "+
			"own, replaying what the log still holds and adopting a peer's snapshot "+
			"where it does not — the retention report's snapshots say whether any "+
			"peer can donate one. Readmit it once it has applied every record up to "+
			"the one just before the higher of the floor and the first surviving "+
			"sequence this refusal names: its position on that log in the retention "+
			"report at that bound minus one or above, at the log's current "+
			"generation", e.NodeID),
	}
}

// PermitReadmission decides whether an evicted node may be counted again.
//
// # What a readmission does, and why a node below the floor is refused one
//
// A readmission lifts the node's tombstone, so the trim counts it again and
// its own position becomes a term of the applied minimum. A node the floor has
// passed is missing records the log has lost or is licensed to lose: it is not
// a replica again until it has replayed what is still there or adopted a
// snapshot for what is not, and meanwhile its position drags every tick's
// conclusion down to a point the log has already left. For a node that is
// still offline that is the very pin the eviction was run to lift, put back.
//
// # It is guidance, not the fence — which is why a stale read is harmless
//
// Nothing about the fleet's data rests on this refusal. A readmitted node below
// the floor is refused every write at an expectation of zero by its own fence,
// verified within the call (the floor theorem's clause (ii)); its readiness
// refuses every read — `behind` while the log still holds what it lacks and it
// is replaying it ([FloorReplaying]), `below_floor` once those records are gone
// from the log ([FloorBelow]); and the trim, counting it again, never purges
// above it. So the two inputs that are stale by construction — a register row that is
// a heartbeat old, a floor that may move between this check and the record
// landing — can at worst admit a state those mechanisms already hold safe, and
// there is no read-then-write race here worth a lock. What the refusal buys is
// the operator being told the truth about the node before they act.
//
// # Every identity-claiming domain, in the register's own order
//
// A node is a replica of every log it runs or of none — one snapshot holds
// them all — so a node behind in any of them is not one that can resume. A
// domain its row does not name is skipped, on [CountedSet]'s rule that a node
// which does not run a domain is not a node at position zero in it; a node
// with NO row is judged at position zero, on that function's other rule.
//
// # And a comparison this node cannot make is not a refusal
//
// A row at a HIGHER generation than the asking node runs is a fleet that has
// re-anchored the log while this node has not applied it yet. Nothing here can
// judge that position, so the answer is [ErrUnavailable] — retry, or ask a
// node that has caught up — rather than a refusal naming a fault in the node.
func PermitReadmission(nodeID string, register []coord.NodePositions,
	bounds []ReadmissionBound) error {

	if nodeID == "" {
		return fmt.Errorf("statelog: a readmission names no node")
	}
	var row *coord.NodePositions
	for i := range register {
		if register[i].NodeID == nodeID {
			row = &register[i]
			break
		}
	}
	for _, b := range bounds {
		refusal := &ReadmissionRefusal{NodeID: nodeID, Domain: b.Domain, Bound: b}
		if row == nil {
			if !Replayable(0, b.Held()) {
				return refusal
			}
			continue
		}
		at, runs := row.Domains[b.Domain]
		if !runs {
			continue
		}
		refusal.Published = true
		refusal.Generation, refusal.Seq, refusal.ReportedAt = at.Generation, at.Seq, row.At
		switch {
		case at.Generation > b.Generation:
			return fmt.Errorf("statelog: %s reports its %s position at generation %d "+
				"and this node runs that log at generation %d — the fleet has "+
				"re-anchored it and this node has not applied that yet, so it cannot "+
				"judge where %s stands; retry, or readmit through a node that has "+
				"caught up: %w", nodeID, b.Domain, at.Generation, b.Generation,
				nodeID, ErrUnavailable)
		case at.Generation < b.Generation, !Replayable(at.Seq, b.Held()):
			return refusal
		}
	}
	return nil
}

// GateStanding judges a retried node gate that this node's ledger answers:
// whether the record its operation landed at the held position is still what
// the node's standing on this log rests on. It is a node gate's
// [Request.Standing], handed the node's own eviction row read in the same
// snapshot ([EvictionRow], and held false for a node this log has no row for).
//
// # Why a ledger hit is not simply "applied"
//
// The ledger says a record under this id landed; it does not say that record
// is still in force. An eviction retried under its id after a readmission took
// the node back was answered "applied" at the eviction's old position while
// every row said the node was counted — a success reported for an operation
// the retry never performed. So the hit is checked against the node's own row:
// an eviction stands while the row's eviction is this record and no
// readmission follows it, a readmission while the row's readmission is this
// record and no eviction has come after. Anything later is [ReasonSuperseded],
// and a row that contradicts the ledger it was written beside is an error
// rather than a guess.
//
// # Why inside the snapshot, and not in front of the write
//
// The publisher reads the ledger and this row in the transaction the decision
// would have run in ([Snap.Held]), so a same-node retry through an applier that
// has not reached the first record is not answered here at all: its snapshot
// holds no row, its append is arbitrated on the node's own eviction subject,
// the broker refuses an expectation below the first record, the publisher
// waits for this node's applier to reach it, and the next round's snapshot
// finds the row. So it ends applied at the first record's position, or refused
// `behind` — never with a second record — and the broker's duplicate window
// plays no part in the argument.
//
// # What it cannot answer for
//
// An operation whose ledger row is gone — swept past [OpsRetention] — is not
// held, and it is judged by the ledger's watermark instead ([Publisher.vouches]):
// minted before it, the retry answers `unknown` rather than writing the gate a
// second time.
func GateStanding(opID, nodeID string, readmit bool, held Position,
	row EvictionRow, found bool) error {

	landed := uint64(held.Packed())
	superseded := func(what string, at uint64) error {
		return &Unavailable{
			Reason: ReasonSuperseded,
			Detail: fmt.Sprintf("operation %q's record for %s landed at %s and %s "+
				"at composed position %d has superseded it, so retrying it is not "+
				"a new gesture — start one, under a fresh operation id, if %s "+
				"should change again", opID, nodeID, held, what, at, nodeID),
			Position: held,
			OpID:     opID,
		}
	}
	inconsistent := func(detail string) error {
		return fmt.Errorf("statelog: operation %q's record for %s landed at %s and "+
			"%s — the row and the ledger were written by the same apply, so this "+
			"estate is not one a retry can be judged against", opID, nodeID,
			held, detail)
	}
	if !readmit {
		switch {
		case !found:
			return inconsistent("no eviction row holds " + nodeID)
		case row.From > landed:
			return superseded("a later eviction", row.From)
		case row.From < landed:
			return inconsistent(fmt.Sprintf(
				"the node's eviction row is at the earlier composed position %d", row.From))
		case row.Back:
			return superseded("a readmission", row.Readmitted)
		}
		return nil
	}
	switch {
	case !found:
		// A READMISSION OF A NODE THIS LOG NEVER EVICTED is a record that
		// changed no row, and nothing since has made one: the node is
		// counted, which is what the operation asked for.
		return nil
	case row.Back && row.Readmitted == landed:
		return nil
	case row.Readmitted > landed:
		return superseded("a later readmission", row.Readmitted)
	case row.From > landed:
		return superseded("a later eviction", row.From)
	}
	return inconsistent(fmt.Sprintf("the node's row is evicted at composed "+
		"position %d and readmitted at %d", row.From, row.Readmitted))
}
