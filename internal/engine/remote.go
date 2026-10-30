package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/seat/placement"
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

	// built is the choices both are derived from, which a later activation
	// is compared against — see [native.built].
	built builtWith
}

// startRemote brings up a stateless node's native runtime, over the router
// [New] built before anything published.
func (e *Engine) startRemote(c *Company) error {
	if e.router == nil {
		return errors.New("engine: this node holds no data and has no router " +
			"to reach a node that does")
	}
	// THE HALVES DERIVED FROM THE CHOICES, so the two cannot disagree.
	built := builtFrom(c.Config)
	e.remote.Store(&remoteNative{
		tracker: built.runsTracker(), wiki: built.runsWiki(), built: built,
	})
	log.Info("native_backends_remote", "node", e.id,
		"tracker", built.runsTracker(), "knowledge", built.runsWiki(), "code", built.runsCode(),
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
// its lease lapse. The view lists on the presence heartbeat's own cadence,
// hears every presence claim and release as it lands and lists again then —
// so a node that joins, or leaves cleanly, is in or out of the roster within
// about a second rather than a heartbeat later — and answers UNKNOWN rather
// than an old or an empty roster once its last listing is older than a lease
// survives; see [coord.LeaseView] and [newDataView].
//
// ONE FILTER FOR EVERY QUESTION ABOUT DATA NODES — [dataNodesOf], which the
// trim and the eviction gate apply to a listing of their own — because a
// second reading of "which nodes hold data" is how one of them comes to count
// a stateless node and wait for ever on a position it will never publish. (The
// capacity handshake asks two different questions with filters of their own:
// who publishes the estate's records and whose broker is a member — see
// [Engine.capacityParticipants].) Those list
// the store directly and deliberately:
// each runs on a duty's tick rather than per request, and each decides
// something a roster up to a heartbeat old must not — what may be trimmed,
// who may be evicted, whether a fleet-wide operation may proceed.
func (e *Engine) dataRoster(context.Context) ([]string, error) {
	return presenceRoster{view: e.dataView}.LiveDataNodes()
}

// RoutedDataNodes is every live data node this node's estate router asks, by
// id and sorted — its placement's own answer ([Engine.estatePlacement]), so
// the order a keyed operation is asked in (rendezvous.Order over these) is
// computed from exactly this — or why the view it reads cannot say, never
// "none".
//
// A LISTING, so a node that claimed its presence since the view last listed is
// not in it yet: the view hears the claim and lists again within about a
// second ([coord.LeaseView]), and until then routes without it — a push the
// joining node's repositories would send there is committed by its runner-up,
// which the broker's compare-and-set keeps correct and which is not the one
// group commit the keyed order exists for. A fleet booted at once is that
// second from each member naming the others, every first listing having been
// taken before any presence lease existed. A caller that must see the fleet's
// routing whole — a test asserting where pushes meet — waits for this to name
// every member.
func (e *Engine) RoutedDataNodes() ([]string, error) {
	return e.estatePlacement().Holders()
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
// do. Between listings it WATCHES the presence leases, so a claim or a release
// lists it again at once: what the cadence is left to catch is a lease that
// lapsed, which is no write — a node that stopped without releasing, as it
// leaves every roster.
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

// gitNodesOf is the nodes with the git role among a listing of presence
// leases: the git nodes a repository's git work is asked of (ADR-0059). Read
// as every role reads off a peer's row — one this build cannot read is every
// role — so a peer whose row is unreadable is asked as a git node too, and
// answers `no_backend` where it is none, which the walk asks last for half a
// minute.
func gitNodesOf(held []coord.Lease) []string {
	out := make([]string, 0, len(held))
	for _, lease := range held {
		if node, ok := gitNodeOf(lease); ok {
			out = append(out, node)
		}
	}
	return out
}

// gitNodeOf is the node a presence lease names where it has the git role, by
// the rule [gitNodesOf] states — the ONE rule a git listing
// ([presenceRoster.GitListing]) reads a peer's row by too, so the git nodes a
// repository's reads are asked of and those its maintenance is weighed among
// are one set.
func gitNodeOf(lease coord.Lease) (string, bool) {
	profile, ok := placement.FromLease(lease)
	if !ok || !profile.Roles.Has(placement.RoleGit) {
		return "", false
	}
	return profile.ID, true
}

// serveEstate makes this data node answer the fleet from its copy of the
// estate — and, where it has the git role, from its git half on the two git
// lanes, the reads' and the engine writes' (estate's gitLanes).
//
// EVERY DATA NODE SERVES, whether or not another node asks yet — one joins
// without anybody reconfiguring the others — and from BEFORE its native runtime
// is up: the backend is resolved per request, so a request arriving first is
// answered "not here" rather than refused. So is the git half ([engineGit]),
// and every git node holds data in this release (git needs ingress, and
// ingress needs data), so every git node serves here: each of its git lanes
// answers `no_backend` until its code host is built, a git read and an engine
// write alike.
func (e *Engine) serveEstate(ctx context.Context) error {
	if e.local == nil || e.router == nil || e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	opts := estate.ServerOptions{
		Self: e.id, Local: e.local, Seams: e.serverSeams(), Envelopes: e.envelopes,
	}
	if e.profile.Roles.Has(placement.RoleGit) {
		opts.Git = e.localGit()
	}
	stop, err := estate.Serve(ctx, e.backends.Queue, opts)
	if err != nil {
		return fmt.Errorf("engine: serve the estate: %w", err)
	}
	e.stopEstate = stop
	return nil
}

// takeCustody arms this data node's half of event custody (internal/observe):
// it settles what an earlier run of this node left unsettled, then joins the
// fleet-wide group the stateless nodes' batches arrive on, writing each into
// this node's own event log.
//
// A NODE WITH NO FLEET STORE TAKES NONE: which data node keeps a batch is
// decided there, and a keeper that could not ask would keep every copy it
// wrote. A node without `data` cannot run beside such a node in any case — it
// reaches the estate through the fleet's broker.
func (e *Engine) takeCustody(ctx context.Context) error {
	if e.backends == nil || e.backends.Store == nil ||
		e.backends.Fleet == nil || e.backends.Queue == nil {
		return nil
	}
	// THROUGH THE ONE DOOR every fleet-wide group passes ([Engine.joins]),
	// which keeps a node without `data` out of it.
	if !e.joins(ctx, observe.CustodyGroup) {
		return nil
	}
	keeper, err := observe.NewKeeper(e.backends.Store.Events(), e.backends.Fleet, e.id)
	if err != nil {
		return fmt.Errorf("engine: event custody: %w", err)
	}
	if err := keeper.Start(ctx, e.backends.Queue); err != nil {
		return fmt.Errorf("engine: event custody: %w", err)
	}
	e.keeper = keeper
	return nil
}

// stopKeeper takes this data node out of the custody group and ends the
// keeper's settling pass ([observe.Keeper.Stop]). Nil-safe.
//
// ON A CONTEXT OF ITS OWN, the caller's cancellation removed: a stop is
// routinely driven by the signal it is cleaning up after, and a leave that
// inherited it would quiesce and detach nothing. The keeper bounds its own
// wait for a batch in hand.
func (e *Engine) stopKeeper(ctx context.Context) {
	if e.keeper == nil {
		return
	}
	e.keeper.Stop(context.WithoutCancel(ctx))
	e.keeper = nil
}

// stopServingEstate withdraws this node from the stateless nodes' rosters of
// who to ask. BEFORE the native runtime stops, for the reason the search
// slices go first: a node that has decided to go away stops being asked
// before it stops being able to answer, and the next request goes to a peer
// rather than waiting out its budget here.
//
// AND BEFORE THE ADMISSION IS WITHDRAWN ([Engine.withdraw]), with every other
// state-log publisher: every write a peer routes here, and every
// linearizable read's barrier, is appended through this node's publishers,
// and one appended once the admission is gone is a write a capacity
// coordinator was told nobody was making.
func (e *Engine) stopServingEstate(ctx context.Context) {
	if e.stopEstate == nil {
		return
	}
	if err := e.stopEstate(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "estate_not_withdrawn", "error", err.Error())
	}
	e.stopEstate = nil
}

// remoteActor is a tool's actor as the estate carries it: its WHOLE
// provenance, through the one construction every writer of it uses
// ([builtin.Actor.Provenance]).
//
// It carried the turn and the chain alone once, written before a person's
// credential, its bound seat, a task's chat origin and the turn's set of
// written items were part of a provenance — so every write that crossed the
// router lost the four: an operator's own gesture landed on a person record
// named after their token, a task filed from a chat thread forgot the thread,
// and a turn that wrote one item was charged to none.
func remoteActor(actor builtin.Actor) estate.Actor {
	return estate.Actor{Handle: actor.Handle, Kind: actor.Kind, Provenance: actor.Provenance()}
}
