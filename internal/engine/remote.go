package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/org"
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
// seats have — so what is here is the SAME seam each of those tools takes,
// satisfied by [estate.Client] rather than by this node's own reader and
// writer. The tool layer cannot tell, and must not: a tool that behaved
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

// remoteNative is a stateless node's native runtime. Written once, like
// [native], before it is published.
type remoteNative struct {
	client *estate.Client

	// tracker and wiki are the halves this company runs natively. A half
	// the company runs on a vendor is not served from here at all.
	tracker bool
	wiki    bool

	// ready caches the last admission answer — see [remoteNative.admitted].
	mu      sync.Mutex
	readyAt time.Time
	ready   bool
}

// startRemote brings up a stateless node's native runtime, over the estate
// client [New] built before anything published.
func (e *Engine) startRemote(c *Company) error {
	if e.estate == nil {
		return errors.New("engine: this node holds no data and has no estate " +
			"client to reach a node that does")
	}
	e.remote.Store(&remoteNative{
		client:  e.estate,
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

// remoteAdmission is how long a stateless node trusts its last answer about
// whether a data node can serve its seats.
//
// FIVE SECONDS: the admission gate is asked on every placement sweep, and a
// fresh ask per sweep is a request to a data node per heartbeat per node for
// an answer that changes when a node joins or leaves — which the presence
// lease's own TTL already bounds at the same order.
const remoteAdmission = 5 * time.Second

// admitted is the stateless node's seat admission: a data node answered, and
// it runs the halves this company needs natively. A data node answers only
// once its own copy is established, so this is the same gate a data node's
// own seats wait on, asked of the node that will serve them.
func (r *remoteNative) admitted(ctx context.Context) bool {
	r.mu.Lock()
	if time.Since(r.readyAt) < remoteAdmission {
		ready := r.ready
		r.mu.Unlock()
		return ready
	}
	r.mu.Unlock()
	asked, cancel := context.WithTimeout(ctx, remoteAdmission)
	defer cancel()
	trackerServed, pagesServed, err := r.client.Serves(asked)
	ready := err == nil && (!r.tracker || trackerServed) && (!r.wiki || pagesServed)
	if !ready {
		detail := "no data node serves a half this company runs natively"
		if err != nil {
			detail = err.Error()
		}
		log.DebugContext(ctx, "seat_admission_withheld", "reason", "no_data_node",
			"detail", detail)
	}
	r.mu.Lock()
	r.ready, r.readyAt = ready, time.Now()
	r.mu.Unlock()
	return ready
}

// dataRoster is every live node that holds data, by id.
//
// ONE READING FOR EVERY QUESTION ABOUT DATA NODES — which nodes a stateless
// node may ask, which nodes the search fan-out divides the buckets between,
// whose positions the trim waits on and who the gate may evict — because a
// second reading of "which nodes hold data" is how one of them comes to count
// a stateless node and wait for ever on a position it will never publish.
func (e *Engine) dataRoster(ctx context.Context) ([]string, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return nil, nil
	}
	return dataNodes(ctx, e.backends.Coord)
}

// dataNodes is [Engine.dataRoster] over any lease reader.
func dataNodes(ctx context.Context, leases liveLeases) ([]string, error) {
	held, err := leases.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(held))
	for _, lease := range held {
		if profile, ok := placement.FromLease(lease); ok && profile.HoldsData() {
			out = append(out, profile.ID)
		}
	}
	return out, nil
}

// serveEstate makes this data node answer the fleet's stateless nodes.
//
// EVERY DATA NODE SERVES, whether or not such a node exists yet — one joins
// without anybody reconfiguring the members — and from BEFORE its native
// runtime is up: the backend is resolved per request, so a request arriving
// first is answered "not here" rather than refused, and custody of a
// stateless node's event records does not wait on the tracker at all.
func (e *Engine) serveEstate(ctx context.Context) error {
	if !holdsData(e.boot) || e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	stop, err := estate.Serve(ctx, e.backends.Queue, e.id, e.estateBackend)
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

// estateBackend is this data node's answer to a stateless node, resolved per
// request — see [estate.Backend].
//
// WRITES ONLY WHILE THIS NODE PUBLISHES. A node restarted into a maintenance
// mode is the evidence a capacity operation is established from, and a write
// it took on a stateless node's behalf would be the publish the mode exists to
// rule out — so its writers are absent and every write is answered "not here".
//
// ALWAYS A BACKEND, native runtime or not: the event log is this node's own
// and takes custody of a stateless node's records whatever backends the
// company runs. The native halves are there once the runtime is.
func (e *Engine) estateBackend() (estate.Backend, bool) {
	b := estate.Backend{Events: e.backends.Store.Events()}
	n := e.native.Load()
	if n == nil {
		return b, true
	}
	b = estate.Backend{
		Events: b.Events,
		Units:  liveUnits{engine: e}, Leads: liveLeads{engine: e},
		Seat: func(handle string) (*org.Role, *org.Organization) {
			c := e.Company()
			if c == nil || c.Org == nil {
				return nil, nil
			}
			return c.Org.AgentSeatByHandle(handle), c.Org
		},
		Committed:   e.WaitCommitted,
		Established: e.NativeHydrated,
	}
	if n.trackerReader != nil {
		b.Tracker = n.trackerReader
	}
	if n.itemSearch != nil {
		b.WorkSearch = n.itemSearch
	}
	if n.pageReader != nil {
		b.Pages = n.pageReader
	}
	if n.searcher != nil {
		b.Knowledge = n.searcher
	}
	if e.mode.Publishes() {
		if n.writer != nil {
			b.Writer = func(a estate.Actor) estate.TrackerWriter {
				// A NIL INTERFACE, never a typed nil: the server reads
				// nil as "the tracker refused to act as this party".
				if w := n.writer.As(a.Handle, a.Kind, a.Provenance); w != nil {
					return w
				}
				return nil
			}
		}
		if n.pages != nil {
			b.PageWriter = n.pages
		}
	}
	return b, true
}
