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
// Two-phase because dropping a node from the counted set lets the trim pass
// its position, after which its writes are records every applier drops, while
// it may still believe it holds everything. The window is how long the fleet
// waits for it to notice: FOUR HEARTBEATS, the engine's usual derivation for a
// cached coordination fact (a node three beats late is still healthy).
//
// A CONVENIENCE, not the safety property: a node three beats late reads its
// tombstone just as the trim may pass it. Safety is the applier's eviction
// gate, which drops its records by log order alone; a wider window adds none.
const EvictionFenceWindow = 4 * coord.ReconcileInterval

// Tombstone is an operator's eviction of a node from a log, as the counted set
// and the report read it.
type Tombstone struct {
	// NodeID is who was evicted.
	NodeID string

	// At is when the tombstone was written.
	At time.Time

	// By is the operator who ran it, which is what a refusal names.
	By string

	// Generation is the domain's generation at the time. One from a
	// previous generation is UNKNOWN rather than old, as for every
	// position-bearing term.
	Generation uint32
}

// EvictionRow is one node's standing on ONE identity-claiming domain's log, as
// that domain's applied rows hold it, written by its own eviction record.
//
// Each domain answers for its own log: an eviction is keyed on a position in
// that log's sequence space and applied identically on every data node, which
// lets the applier's eviction gate hold without coordination. The trim stops
// counting a node on a log once its tombstone THERE is older than the fence
// window; read from another domain's table, the answer is about another number
// space and the node is counted for ever.
type EvictionRow struct {
	// NodeID is who was evicted.
	NodeID string

	// At is the broker's own instant for the eviction record (what the
	// fence window is measured from, identical on every node), and By the
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

// Presence is a node the fleet says is there, reported position or not: a
// data node holding a live presence lease, which the eviction gate refuses to
// evict and every log's counted set counts.
//
// A LIVE DATA NODE COUNTS FOR EVERY LOG: every data node runs every domain,
// so one with no position for a log has not yet written its row, never
// declined the log.
type Presence struct {
	NodeID string
}

// CountedSet is who the trim counts on ONE LOG: the positions register's rows
// naming the log, UNION every live data node, MINUS any eviction tombstone
// older than [EvictionFenceWindow].
//
// Register keys never expire, so an offline node pins the floor. A live data
// node with no row yet counts at ZERO and blocks every derived term until its
// row lands; only DATA nodes, since a node without `data` applies no log and
// would pin them all. A booting node is not live until after its first row
// (package doc, "Who may write a log: every writer is counted, but for its own
// boot"); an adopter is kept by its own hold ([AdoptDeps.Hold]). Tombstones
// unpin an absent node only after the window: one that has not noticed is
// still writing.
func CountedSet(now time.Time, reported []NodePosition,
	live []Presence, tombs []Tombstone) []NodePosition {

	byID := make(map[string]NodePosition, len(reported)+len(live))
	for _, n := range reported {
		byID[n.NodeID] = n
	}
	for _, p := range live {
		if _, known := byID[p.NodeID]; !known {
			// At zero: it applies every log and nothing says where it
			// stands yet, so absent would let the trim pass what it has
			// still to apply.
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

// Readers is the record version every node of a counted set reads on its log,
// zero for one that has not said: what a writer asks before it publishes a
// record an older build cannot even defer.
//
// Deliberately the trim's counted set: a live node with no row still applies
// the log, and an offline, unevicted node on an old build applies it on return
// and would stop on such a record, so it holds the writer back as it holds the
// trim, and an eviction releases both.
func Readers(counted []NodePosition) map[string]int {
	out := make(map[string]int, len(counted))
	for _, n := range counted {
		out[n.NodeID] = n.RecordVersion
	}
	return out
}

// EvictionRefusal reports why an eviction was not permitted.
type EvictionRefusal struct {
	NodeID string
	Detail string
}

func (e *EvictionRefusal) Error() string { return "statelog: " + e.Tell() }

// Tell is the refusal as the operator who asked reads it ([Told]).
func (e *EvictionRefusal) Tell() string {
	if e.NodeID == "" {
		return e.Detail
	}
	return fmt.Sprintf("%s may not be evicted: %s", e.NodeID, e.Detail)
}

// PermitEviction decides whether a node may be evicted at all.
//
// A LIVE LEASE REFUSES unless forced: the node is reaching the fleet and almost
// surely running, so it pins nothing its progress will not release, while an
// eviction drops its writes on every applier and takes its copy of each log
// out of service. The refusal stops a mistyped id taking a healthy machine
// out; force is for what the lease cannot say (wedged but still renewing). The
// fleet's data rests on neither: the eviction gate and the node's own fence,
// read from its applied rows, hold anyway.
//
// ASKED ONCE PER GESTURE, by the engine's node gate before any log is
// written: judged per log, it could answer two ways about one node.
func PermitEviction(nodeID string, live []Presence, force bool) error {
	if nodeID == "" {
		return &EvictionRefusal{Detail: "no node was named"}
	}
	held := slices.ContainsFunc(live, func(p Presence) bool { return p.NodeID == nodeID })
	if held && !force {
		return &EvictionRefusal{NodeID: nodeID, Detail: "it holds a live presence " +
			"lease, so it's still reaching coordination and almost certainly " +
			"running. An eviction drops every record it writes on every node and " +
			"takes its copy of every log it is evicted from out of service, which " +
			"is the gesture for a node that isn't coming back"}
	}
	return nil
}

// Remedy is what the operator does about an eviction refusal: wait for the
// lease to lapse, or force the eviction past it. Worded in no surface's
// vocabulary ([GateAction]): each surface renders [GateForce] its own way.
func (e *EvictionRefusal) Remedy() GateRemedy {
	if e.NodeID == "" {
		return GateRemedy{Detail: "name the node to evict"}
	}
	return GateRemedy{
		Actions: []GateAction{GateWait, GateForce},
		Detail: fmt.Sprintf("stop %s and wait for its presence lease to lapse "+
			"(the retention report stops showing it live), then run the eviction "+
			"again; or, if it's wedged in a way that still renews its lease, "+
			"force the eviction past the lease", e.NodeID),
	}
}

// ReadmissionBound is one identity-claiming domain's bound, as the node asked
// to write a readmission reads it: the two witnesses the write fence takes the
// higher of (package doc, "THE FLOOR THEOREM"). Judged against either alone, a
// readmission would clear a node that fence refuses.
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

// ReadmissionRefusal reports why an evicted node may not be taken back yet. It
// carries every number the inequality turns on: how far behind the node is
// decides whether the operator waits for its applier or finds out why it has
// not adopted a snapshot.
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

func (e *ReadmissionRefusal) Error() string { return "statelog: " + e.Tell() }

// Tell is the refusal as the operator who asked reads it ([Told]), every number
// it turns on included, since the inequality is the reason.
func (e *ReadmissionRefusal) Tell() string {
	gone := fmt.Sprintf("records below %d may already be gone from the %s log "+
		"(published floor %d, first surviving sequence %d)",
		e.Bound.Held(), e.Domain, e.Bound.Floor, e.Bound.First)
	// The heartbeat's instant, since the position is a heartbeat old at
	// best: a node that just adopted a snapshot is refused on its old row.
	reported := ""
	if !e.ReportedAt.IsZero() {
		reported = " (reported " + e.ReportedAt.UTC().Format(time.RFC3339) + ")"
	}
	switch {
	case !e.Published:
		return fmt.Sprintf("%s may not be readmitted: it has never "+
			"published a position, so as far as the fleet knows it holds "+
			"nothing, and %s", e.NodeID, gone)
	case e.Generation < e.Bound.Generation:
		return fmt.Sprintf("%s may not be readmitted: its last position "+
			"in %s%s is %d at generation %d and the log is at generation %d, a "+
			"sequence space that no longer exists, so nothing it holds can be "+
			"compared with anything the log still has",
			e.NodeID, e.Domain, reported, e.Seq, e.Generation, e.Bound.Generation)
	}
	return fmt.Sprintf("%s may not be readmitted: its last position in "+
		"%s%s is %d and %s. It has to catch up before the fleet counts it again",
		e.NodeID, e.Domain, reported, e.Seq, gone)
}

// Remedy is what the operator does about it: NOTHING THAT FORCES IT. A node
// below the floor repairs itself (replaying what the log holds, adopting a
// peer's snapshot where it does not), and a readmission follows that repair
// rather than standing in for it.
func (e *ReadmissionRefusal) Remedy() GateRemedy {
	return GateRemedy{
		Actions: []GateAction{GateWait},
		Detail: fmt.Sprintf("start %s if it isn't running: it catches up on its "+
			"own, replaying what the log still holds and adopting a peer's snapshot "+
			"where it doesn't (the retention report's snapshots say whether any "+
			"peer can donate one). Readmit it once it has applied every record up to "+
			"the one just before the higher of the floor and the first surviving "+
			"sequence this refusal names: its position on that log in the retention "+
			"report at that bound minus one or above, at the log's current "+
			"generation", e.NodeID),
	}
}

// ReanchorNotApplied is a readmission this node can't judge on one log: the
// node reports its position there at a later generation than this node runs
// the log at, so the fleet has re-anchored it and this node hasn't applied
// that yet. A position in a generation this node doesn't hold compares to
// nothing it has. It is a CONDITION ([ErrUnavailable]): it clears when this
// node applies the re-anchor, and a node that already has judges it now, so
// the remedy is to ask again or to ask another node.
type ReanchorNotApplied struct {
	Node   string
	Domain string
	// Reported is the generation the node's position is at, and Runs the
	// one this node runs the log at.
	Reported, Runs uint32
}

func (e *ReanchorNotApplied) Error() string {
	return fmt.Sprintf("statelog: %s reports its %s position at generation %d and "+
		"this node runs that log at generation %d: the fleet has re-anchored it "+
		"and this node hasn't applied that yet", e.Node, e.Domain, e.Reported, e.Runs)
}

// Unwrap is the condition it is: one a wait, or another node, clears.
func (e *ReanchorNotApplied) Unwrap() error { return ErrUnavailable }

// PermitReadmission decides whether an evicted node may be counted again.
// A readmission makes its position a term of the applied minimum, so a node the
// floor has passed is refused until it has replayed or adopted a snapshot: its
// position would drag the trim down, an offline one restoring the pin its
// eviction lifted. Guidance, not the fence, so stale inputs need no lock: its
// own fence (floor theorem clause (ii)), its readiness ([FloorReplaying],
// [FloorBelow]) and the trim already hold a node below the floor safe.
//
// Every bound is judged, in the register's order: one snapshot holds every
// log. A domain the node's row omits is skipped, a node with no row judged at
// zero ([CountedSet]'s rules). A row at a HIGHER generation than this node runs
// is a re-anchor not yet applied here: [ErrUnavailable], not a refusal.
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
			return &ReanchorNotApplied{Node: nodeID, Domain: b.Domain,
				Reported: at.Generation, Runs: b.Generation}
		case at.Generation < b.Generation, !Replayable(at.Seq, b.Held()):
			return refusal
		}
	}
	return nil
}

// GateStanding is a node gate's [Request.Standing]: whether the record a
// retried operation landed at the held position still decides the node's
// standing, per its [EvictionRow] in the same snapshot (found false if none).
//
// A ledger hit says a record landed, not that it is in force, so a retried
// eviction after a readmission must not answer "applied". An eviction stands
// while the row's eviction is this record and no readmission follows; a
// readmission while the row's readmission is this record and no eviction
// follows. Later is [ReasonSuperseded]; a row contradicting its ledger is an
// error. A retry whose applier lacks the first record is not judged here
// ([Snap.Held]): the broker refuses its append, so no second record. An op
// swept past [OpsRetention] falls to [Publisher.vouches] and answers `unknown`.
func GateStanding(opID, nodeID string, readmit bool, held Position,
	row EvictionRow, found bool) error {

	landed := uint64(held.Packed())
	superseded := func(what string, at uint64) error {
		return &Unavailable{
			Reason: ReasonSuperseded,
			Detail: fmt.Sprintf("operation %q's record for %s landed at %s and %s "+
				"at composed position %d has superseded it, so retrying it is not "+
				"a new gesture. Start one, under a fresh operation id, if %s "+
				"should change again", opID, nodeID, held, what, at, nodeID),
			Position: held,
			OpID:     opID,
		}
	}
	inconsistent := func(detail string) error {
		return fmt.Errorf("statelog: operation %q's record for %s landed at %s and "+
			"%s. The row and the ledger were written by the same apply, so this "+
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
		// A readmission of a node this log never evicted changed no row,
		// and the node is counted, which is what it asked for.
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
