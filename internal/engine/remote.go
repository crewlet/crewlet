package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The native backends on a node that holds no data.
//
// # The same seams, answered elsewhere
//
// A node without the `data` role runs no state log, keeps no replicated
// estate and has no lexical index. Its seats still read and write the
// company's tracker and knowledge base through exactly the tools a data node's
// seats have — the same [estate.Router] facades every node's tools are handed,
// which on this node answer nothing in-process and ask a data node for
// everything. The tool layer cannot tell, and must not: a tool that behaved
// differently on a stateless node would be two tools, and only one of them
// tested.
//
// # What it deliberately does not run
//
// Everything a data node runs because it HOLDS the estate: the apply loops,
// the position heartbeat, the snapshotter and the donor, the lexical index and
// the search slices it answers, the change feeds, the chart apply, the trim
// and the embedding duty. Each is either a copy this node does not keep or a
// job a data node already does for the whole fleet.

// remoteNative is a stateless node's native runtime: which halves the company
// runs natively, reached through the router. Written once, like [native],
// before it is published.
type remoteNative struct {
	// tracker and wiki are the halves this company runs natively. A half
	// the company runs on a vendor is not served from here at all.
	tracker bool
	wiki    bool
}

// startRemote brings up a stateless node's native runtime, over the router
// [New] built before anything published.
func (e *Engine) startRemote(c *Company) error {
	if e.router == nil {
		return errors.New("engine: this node holds no data and has no router " +
			"to reach a node that does")
	}
	e.remote.Store(&remoteNative{
		tracker: c.Config.TrackerBackendFor() == config.TrackerNative,
		wiki:    c.Config.KnowledgeBackendFor() == config.KnowledgeNative,
	})
	log.Info("native_backends_remote", "node", e.id,
		"tracker", c.Config.TrackerBackendFor() == config.TrackerNative,
		"knowledge", c.Config.KnowledgeBackendFor() == config.KnowledgeNative,
		"detail", "this node holds no data: its seats read and write the "+
			"company's tracker and knowledge base through a data node")
	return nil
}

// dataRoster is every live node that holds data, by id, as this node's WATCHED
// VIEW of the presence leases answers it — for the questions asked PER
// REQUEST: which data node a stateless node's tool call goes to, and which
// nodes a search divides its buckets between.
//
// FROM MEMORY, because it is asked per request. It used to list every presence
// lease in the fleet on every call — an O(fleet) read of the coordination
// store, crossing the leaf link on a stateless node, per tool call and per
// search — for an answer that changes only when a node joins, leaves or lets
// its lease lapse. The view lists on the presence heartbeat's own cadence and
// answers UNKNOWN rather than an old or an empty roster once its last listing
// is older than a lease survives; see [coord.LeaseView] and [newDataView].
//
// ONE FILTER FOR EVERY QUESTION ABOUT DATA NODES — [dataNodesOf], which the
// trim and the eviction gate apply to a listing of their own — because a
// second reading of "which nodes hold data" is how one of them comes to count
// a stateless node and wait for ever on a position it will never publish. (The
// capacity handshake asks two different questions with filters of their own:
// who publishes the estate's records, which the ESTATE lease answers, and
// whose broker is a member — see [Engine.capacityParticipants].) Those list
// the store directly and deliberately:
// each runs on a duty's tick rather than per request, and each decides
// something a roster up to a heartbeat old must not — what may be trimmed,
// who may be evicted, whether a fleet-wide operation may proceed.
func (e *Engine) dataRoster(context.Context) ([]string, error) {
	return presenceRoster{view: e.dataView}.LiveDataNodes()
}

// newDataView builds the watched view of the fleet's presence leases, or nil
// where there is no coordination.
//
// ON THE PRESENCE LEASE'S OWN TERMS, from the TTL in force: it lists once per
// heartbeat (the TTL over [seat.HeartbeatRatio], which is how often a node
// renews the lease the view reads, so a listing more often would find nothing
// new) and trusts a listing for one TTL (past which a lease it named may have
// lapsed and one it missed may have been held all along). Neither is a knob
// of its own: both move with `coordination.lease_ttl_seconds`, as the leases
// do.
func newDataView(b *config.Bootstrap, backend coord.Backend) (*coord.LeaseView, error) {
	if backend == nil {
		return nil, nil
	}
	ttl := effectiveLeaseTTL(b, backend)
	view, err := coord.NewLeaseView(backend, coord.ClassNode, coord.ViewOptions{
		Every: ttl / seat.HeartbeatRatio, Trust: ttl,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: watch the fleet's data nodes: %w", err)
	}
	return view, nil
}

// startDataView runs the view until [Engine.stopWatchingDataNodes], detached
// from ctx like every loop a node owns.
func (e *Engine) startDataView(ctx context.Context) {
	if e.dataView == nil || e.stopDataView != nil {
		return
	}
	loop, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.dataView.Run(loop)
	}()
	e.stopDataView = func() {
		cancel()
		<-done
	}
}

// stopWatchingDataNodes ends the view's loop and waits for it. Nil-safe, and
// safe to call twice.
func (e *Engine) stopWatchingDataNodes() {
	if e.stopDataView == nil {
		return
	}
	e.stopDataView()
	e.stopDataView = nil
}

// dataNodes is every live data node, listed from the store NOW — for the duty
// ticks that decide something a heartbeat-old roster must not; see
// [Engine.dataRoster].
func dataNodes(ctx context.Context, leases liveLeases) ([]string, error) {
	held, err := leases.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, err
	}
	return dataNodesOf(held), nil
}

// dataNodesOf is the data nodes among a listing of presence leases.
func dataNodesOf(held []coord.Lease) []string {
	out := make([]string, 0, len(held))
	for _, lease := range held {
		if profile, ok := placement.FromLease(lease); ok && profile.HoldsData() {
			out = append(out, profile.ID)
		}
	}
	return out
}

// serveEstate makes this data node answer the fleet for the partitions it
// serves.
//
// EVERY DATA NODE SERVES, whether or not another node asks yet — one joins
// without anybody reconfiguring the others — and from BEFORE its native runtime
// is up: the backend is resolved per request, so a request arriving first is
// answered "not here" rather than refused, and custody of a stateless node's
// event records does not wait on the tracker at all.
func (e *Engine) serveEstate(ctx context.Context) error {
	if e.local == nil || e.router == nil || e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	stop, err := estate.Serve(ctx, e.backends.Queue, e.id, e.local, e.estatePlacement(),
		e.serverSeams())
	if err != nil {
		return fmt.Errorf("engine: serve the estate: %w", err)
	}
	e.stopEstate = stop
	return nil
}

// stopServingEstate withdraws this node from the stateless nodes' rosters of
// who to ask. BEFORE the native runtime stops, for the reason the search
// slices go first: a node that has decided to go away stops being asked
// before it stops being able to answer, and the next request goes to a peer
// rather than waiting out its budget here.
func (e *Engine) stopServingEstate(ctx context.Context) {
	if e.stopEstate == nil {
		return
	}
	if err := e.stopEstate(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "estate_not_withdrawn", "error", err.Error())
	}
	e.stopEstate = nil
}

// provenanceOf is where a tool's actor says a write came from, in the one
// shape both the local writer and the estate take.
func provenanceOf(actor builtin.Actor) tracker.Provenance {
	return tracker.Provenance{TurnID: actor.TurnID, Chain: actor.Chain}
}

// remoteActor is a tool's actor as the estate carries it.
func remoteActor(actor builtin.Actor) estate.Actor {
	return estate.Actor{Handle: actor.Handle, Kind: actor.Kind, Provenance: provenanceOf(actor)}
}
