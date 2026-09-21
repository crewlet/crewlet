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

	// Generation is the estate's generation at the time. One from a
	// previous generation is UNKNOWN rather than old, on the same rule
	// every other position-bearing term uses.
	Generation uint32
}

// Presence is a node holding a live lease, whether or not it has reported a
// position yet.
type Presence struct {
	NodeID string

	// Domains is the set of state-log domains that node RUNS, which is
	// what decides whether its silence about this one is a node still
	// booting or a node that will never report.
	//
	// WITHOUT IT A DECLINED DOMAIN IS A PERMANENT BLOCK. A live node
	// publishes a position for every domain it applies and none for the
	// rest, so a satellite whose roles exclude a domain would be unioned
	// in at position zero on every tick, for ever, and that domain's log
	// would grow without bound on any fleet with one.
	//
	// EMPTY MEANS EVERY DOMAIN, because that is the only safe reading of
	// a row written by a build that predates the field — the same rule
	// [placement.RoleSet] takes for an absent role list, and for the same
	// reason: a rolling upgrade puts exactly that row in front of the new
	// nodes, and reading it as "runs nothing" would trim past a peer that
	// is still applying.
	Domains []string
}

// runs reports whether this node applies the named domain.
//
// AN EMPTY NAME ASKS "for anything at all", which is what the fleet-wide
// operator report wants: its block is per NODE rather than per domain, and
// every node that is up runs at least one domain or it would have refused to
// start. Answering false there would render a live peer as uncounted on a
// screen whose whole job is to say who the trim is waiting for.
func (p Presence) runs(domain string) bool {
	if domain == "" || len(p.Domains) == 0 {
		return true
	}
	for _, name := range p.Domains {
		if name == domain {
			return true
		}
	}
	return false
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
func CountedSet(now time.Time, domain string, reported []NodePosition,
	live []Presence, tombs []Tombstone) []NodePosition {

	byID := make(map[string]NodePosition, len(reported)+len(live))
	for _, n := range reported {
		byID[n.NodeID] = n
	}
	for _, p := range live {
		if _, known := byID[p.NodeID]; known {
			continue
		}
		if !p.runs(domain) {
			// A NODE THAT DOES NOT RUN THIS DOMAIN IS NOT COUNTED
			// FOR IT. Its silence is a declaration rather than a
			// node between boot and its first heartbeat, so unioning
			// it in at zero would block this domain's trim for as
			// long as the node lives.
			continue
		}
		// A NODE WITH A LIVE LEASE AND NO POSITION YET COUNTS AT
		// ZERO and blocks. It is a node between boot and its first
		// heartbeat — which is a node adopting a snapshot — and
		// treating it as absent would let the trim advance past
		// the tail it is about to replay.
		byID[p.NodeID] = NodePosition{NodeID: p.NodeID}
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
// # A LIVE LEASE REFUSES, and that refusal is a precondition of the whole
// design
//
// Evicting a node that is still talking to coordination would let the trim
// advance past a node that is about to write — and the write-path fence that
// is supposed to catch that reads a CACHED tombstone, refreshed on the same
// coordination loop the node has by definition still got.
//
// So the only state in which an eviction is permitted is one where the target
// has missed at least three consecutive coordination round trips. Which is
// exactly why the publisher's fence checks a THIRD source — that node's own
// applied eviction rows, fresh to its applied prefix and the only one still
// fresh when its coordination path is wedged.
func PermitEviction(nodeID string, live []Presence, force bool) error {
	if nodeID == "" {
		return &EvictionRefusal{Detail: "no node was named"}
	}
	held := slices.ContainsFunc(live, func(p Presence) bool { return p.NodeID == nodeID })
	if held && !force {
		return &EvictionRefusal{NodeID: nodeID, Detail: "it holds a live presence " +
			"lease, so it is still reaching coordination — and an eviction is " +
			"only safe once the target has missed enough round trips to be out " +
			"of contact, because the fence that stops it writing reads a value " +
			"refreshed on the connection it still has"}
	}
	return nil
}
