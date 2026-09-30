package engine

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The estate map's membership lease: `estate:{node}` (coord.ClassEstate), kept
// by the membership-lease loop both placement maps share (memberlease.go).
//
// # Who claims it, and when
//
// A data node's ESTATE RUNTIME — the state log that applies the replicated
// estate's records into this node's copy — claims it once the runtime is up,
// renews it on the seat heartbeat's cadence, and gives it back only after the
// runtime has stopped ([native.shutdown]): after the search slices are
// withdrawn, the runtime's own loops have ended and its appliers have
// stopped. That order is the lease's whole point (ADR-0020's reason for not
// reading membership off presence): a drain gives presence back at its first
// step while the node still serves, and a map reading presence would count a
// draining node gone and move everything it holds.
//
// A data node that runs no estate — a company on vendor backends for both its
// tracker and its knowledge base, or a node with no company yet — claims
// none: it holds no copy anybody reads, and its presence and its broker are
// what a capacity window counts it by.
//
// # What it says under layout 0
//
// The layout this build runs is the single-file one: one space, one
// partition, `estate.000`, held whole by every data node. So the lease says
// exactly that — layout 0, no map acted on (there is none), and the one
// partition this node holds, in the state this node's copy is in. WHICH
// partitions it names is [heldIn]'s answer, the rule the files a node keeps
// open ([HeldPartitions]) and the logs it may write ([holdingOf]) are read
// from, so the lease can never describe a partition the node does not hold,
// or leave out one whose logs it writes:
//
//   - `serving` when the copy is established, has drained since its
//     appliers started and is within the snapshot slack of every log's end
//     ([stateLog.Serving]) — the contract's definition, and deliberately NOT
//     a seat's admission, which refuses at a lag of one: a busy company has
//     a record in flight on most beats, and a lease sampling that instant
//     would leave `serving` on every one of them;
//   - `catching_up` while it is not yet — never drained, or past the slack;
//   - `faulted` when it is WRONG rather than behind — an applier halted, an
//     eviction, rows below the log, a checkpoint naming another stream, a
//     stalled prefix or a record held past its grace ([stateLog.Healthy]).
//
// A beat that cannot read the copy's health says what it said last, and
// `catching_up` before it has said anything: the node holds the file and is
// applying it, which is all a beat that could not ask the broker knows. The
// states of a join and a leave — `adopting`, `draining`, `released` — belong
// to a layout that moves partitions between nodes, and nothing here writes
// them.
//
// Nothing under layout 0 reads the partition's state: there is no map to
// promote a joiner or release a leaver on. And the write authority does not
// read it either. `serving` HERE is how far the copy has applied — what a map
// promotes a joiner on — while serving a partition's WRITES ([holdingOf]) is
// the node's own account of where it stands in a join or a leave, and under
// layout 0 a data node stands in none: it serves estate.000 from boot, while
// this lease may still say `catching_up`. Refusing its writes until the lease
// said `serving` would refuse every write a restarted node takes before its
// appliers drain, which no layout-0 fleet has ever done. The two become one
// step once partitions move: a joiner's lease says `serving` at the moment it
// begins to serve the partition's writes, and a leaver stops serving them at
// the moment its lease says `draining`.
//
// # The store's health, and its free space
//
// `healthy` is whether the volume the estate's file is on answers a
// measurement, and `free_bytes` what that measurement found. A copy that has
// stopped applying is its PARTITION's `faulted`, not the store's health: the
// map counts an unhealthy node absent and, past the grace, removes it and
// every partition it holds, which is right for a store that has failed and
// wrong for one partition's fault on a node that serves the rest.

// estateRuntime is the state log as the lease reads it.
type estateRuntime interface {
	Healthy(ctx context.Context) (bool, string)
	Serving(ctx context.Context) (bool, statelog.ReadRefusal)
}

// estateLeaseAccount builds this node's estate lease, beat after beat,
// remembering the last partition state it could read.
type estateLeaseAccount struct {
	// weight and labels are what this node's membership offers, fixed for
	// the process: they are Tier A, which a restart changes.
	weight int
	labels map[string]string

	// layout is the layout this node runs.
	layout statelog.Layout

	// held is every partition of layout this node holds ([heldIn]) — what
	// the lease describes.
	held []statelog.PartitionID

	// runtime is the estate runtime whose copy the lease describes.
	runtime estateRuntime

	// volume is where the estate's file is, measured for its free space.
	volume string

	// building reports whether this node's lexical index is still being
	// built, nil where it keeps none.
	building func() bool

	// free measures a directory's volume ([volumeFree]); a parameter for
	// the tests.
	free func(dir string) (int64, error)

	mu sync.Mutex
	// last is the partition state the last beat that could read it said.
	last partmap.PartitionState
}

// newEstateLeaseAccount is the account of this data node's native runtime n,
// under the node's Tier A.
func newEstateLeaseAccount(boot *config.Bootstrap, db *store.DB, n *native) *estateLeaseAccount {
	layout := LayoutZero()
	a := &estateLeaseAccount{
		weight: boot.Store.Estate.EstateWeight(),
		layout: layout,
		held:   heldIn(boot, layout),
		// THE RUNTIME AS AN INTERFACE ONLY WHEN THERE IS ONE: a nil
		// *stateLog in an interface would read as a runtime and be
		// asked, where a nil interface is refused by meta below.
		// THE DIRECTORY EVERY PARTITION FILE IS IN: layout 0's file is at
		// [store.ReplicatedPath] and every later layout's beside it.
		volume: filepath.Dir(store.ReplicatedPath(db.Path(), boot.Store.ReplicatedPath)),
		free:   volumeFree,
	}
	if n.log != nil {
		a.runtime = n.log
	}
	if len(boot.Node.Labels) > 0 {
		a.labels = maps.Clone(boot.Node.Labels)
	}
	if n.indexer != nil {
		a.building = func() bool { return !n.indexer.Ready() }
	}
	return a
}

// meta is this beat's lease: refused when there is no runtime to describe,
// since a lease that cannot say what the node holds must not be written.
func (a *estateLeaseAccount) meta(ctx context.Context) (map[string]any, error) {
	if a.runtime == nil {
		return nil, fmt.Errorf("engine: this node has no estate runtime to describe")
	}
	// A DATA NODE UNDER LAYOUT 0 HOLDS ONE PARTITION, and a node holding
	// more would need a runtime that says what it holds of each — which is
	// the join and leave executor's, not this account's — while one
	// holding none has no copy to describe.
	parts := a.held
	if len(parts) != 1 {
		return nil, fmt.Errorf("engine: the estate lease describes a copy of one "+
			"partition, and this node holds %d of layout %d", len(parts), a.layout.Number)
	}
	layout := a.layout.Number
	m := partmap.Meta{
		Weight: a.weight, Labels: a.labels, Layout: &layout,
		Partitions: map[string]partmap.PartitionState{parts[0].String(): a.state(ctx)},
	}
	healthy := true
	free, err := a.free(a.volume)
	if err != nil {
		healthy = false
		m.Detail = fmt.Sprintf("the volume holding the estate (%s) could not be "+
			"measured: %v", a.volume, err)
	}
	m.Healthy, m.FreeBytes = &healthy, free
	if a.building != nil && a.building() {
		m.Building = []string{parts[0].String()}
	}
	return m.Encode()
}

// state is what this node's copy of the one partition is doing, or what the
// last beat that could read it said when this one cannot.
func (a *estateLeaseAccount) state(ctx context.Context) partmap.PartitionState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, known := readPartitionState(ctx, a.runtime); known {
		a.last = s
	}
	if a.last == "" {
		return partmap.PartCatchingUp
	}
	return a.last
}

// readPartitionState is what the runtime says of its copy, and false when it
// could not be asked: WRONG before behind, since a copy that is wrong is also
// not established, and "faulted" is the one an operator has to act on.
func readPartitionState(ctx context.Context, rt estateRuntime) (partmap.PartitionState, bool) {
	if ok, _ := rt.Healthy(ctx); !ok {
		return partmap.PartFaulted, true
	}
	switch ok, refusal := rt.Serving(ctx); {
	case ok:
		return partmap.PartServing, true
	case refusal == statelog.RefuseBrokerUnreachable:
		// UNKNOWN, NOT BEHIND: the health could not be read at all, and a
		// copy nobody could measure is not one that is catching up.
		return "", false
	default:
		return partmap.PartCatchingUp, true
	}
}

// startEstateLease claims this data node's estate membership for the runtime
// n, once n is up. Nil where there is no coordination store to claim it in.
func (e *Engine) startEstateLease(ctx context.Context, n *native) *memberLease {
	if e.backends == nil || e.backends.Coord == nil || e.backends.Store == nil {
		return nil
	}
	account := newEstateLeaseAccount(e.boot, e.backends.Store, n)
	return startMemberLease(ctx, e.backends.Coord, memberLeaseSpec{
		resource: coord.EstateResource(e.id), node: e.id, owner: e.incarnation,
		ttl: e.leaseTTL, what: "estate membership", event: "estate",
		meta: account.meta,
	})
}
