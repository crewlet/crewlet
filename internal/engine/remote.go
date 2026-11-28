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
	"github.com/crewlet/crewlet/internal/store"
)

// The native backends on a node that holds no data.
//
// A node without the `data` role runs no state log, replicated estate or
// lexical index. Its seats reach the tracker, knowledge base and chat through
// the same [estate.Router] facades every node's tools are handed, which here
// ask a data node for everything: a tool that behaved differently on a
// stateless node would be two tools, only one of them tested. It runs nothing
// a data node runs because it HOLDS the estate (apply loops, position
// heartbeat, snapshotter and donor, lexical index and search slices, change
// feeds, chart apply, trim, embedding duty): each is a copy it does not keep
// or a job a data node does for the whole fleet.

// remoteNative is a stateless node's native runtime: which halves the company
// runs natively, reached through the router. Written once, like [native],
// before it is published.
type remoteNative struct {
	// tracker, wiki and chat are the halves this company runs natively; a
	// half run on a vendor is not served from here at all.
	tracker bool
	wiki    bool
	chat    bool

	// built is the choices the three are derived from, which a later
	// activation is compared against — see [native.built].
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
		tracker: built.runsTracker(), wiki: built.runsWiki(), chat: built.runsChat(),
		built: built,
	})
	log.Info("native_backends_remote", "node", e.id,
		"tracker", built.runsTracker(), "knowledge", built.runsWiki(), "chat", built.runsChat(),
		"code", built.runsCode(),
		"detail", "this node holds no data: its seats read and write "+
			"Worklet, Doclet, Chatlet and Gitlet through a data node")
	return nil
}

// dataRoster is every live data node, by id, from this node's WATCHED VIEW of
// the presence leases, for the questions asked PER REQUEST (which data node a
// tool call goes to, which nodes a search divides its buckets between): from
// memory, as listing the store per call is an O(fleet) read across the leaf
// link. The view answers UNKNOWN, never an old or empty roster, once its
// listing is older than a lease survives ([coord.LeaseView], [newDataView]).
//
// ONE FILTER, [dataNodesOf], for every question about data nodes: a second
// reading is how one comes to count a stateless node and wait for ever on a
// position it never publishes. The trim and the eviction gate list the store
// on their duty ticks, as a heartbeat-old roster must not decide what they do;
// [Engine.capacityParticipants] asks other questions with filters of its own.
func (e *Engine) dataRoster(context.Context) ([]string, error) {
	return presenceRoster{view: e.dataView}.LiveDataNodes()
}

// RoutedDataNodes is every live data node this node's estate router asks, by
// id and sorted: its placement's own answer ([Engine.estatePlacement]), which
// the keyed order (rendezvous.Order) is computed from, or why the view cannot
// say, never "none".
//
// A LISTING: a node that claimed its presence since is missing for about a
// second ([coord.LeaseView]), and a push meant for it commits on its
// runner-up, correct under the broker's compare-and-set but not the one group
// commit the keyed order exists for. A fleet booted at once takes that second
// for each member to name the others, so a caller that must see the routing
// whole (a test asserting where pushes meet) waits for this to name every one.
func (e *Engine) RoutedDataNodes() ([]string, error) {
	return e.estatePlacement().Holders()
}

// newDataView builds the watched view of the fleet's presence leases, or nil
// where there is no coordination.
//
// ON THE PRESENCE LEASE'S OWN TERMS, from the TTL in force: it lists once per
// heartbeat (TTL over [seat.HeartbeatRatio], how often a node renews; listing
// more often finds nothing new) and trusts a listing for one TTL (past which a
// lease it named may have lapsed). Neither is a knob of its own: both move with
// `coordination.lease_ttl_seconds`. Between listings it WATCHES, so a claim or
// release relists at once; the cadence is left to catch a lapse, which is no
// write: a node that stopped without releasing.
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

// dataNodes is every live data node, listed from the store NOW, for the duty
// ticks that decide what a heartbeat-old roster must not ([Engine.dataRoster]).
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

// gitletNodesOf is the nodes with the gitlet role among a listing of presence
// leases: the Gitlet nodes a repository's git work is asked of (ADR-0089). A
// row this build cannot read is every role, so such a peer is asked too and
// answers `no_backend` where it is none, which the walk then asks last for half
// a minute.
func gitletNodesOf(held []coord.Lease) []string {
	out := make([]string, 0, len(held))
	for _, lease := range held {
		if node, ok := gitletNodeOf(lease); ok {
			out = append(out, node)
		}
	}
	return out
}

// gitletNodeOf is the node a presence lease names where it has the gitlet role,
// by [gitletNodesOf]'s rule: the ONE rule a git listing
// ([presenceRoster.GitListing]) reads a peer's row by too, so the nodes a
// repository's reads are asked of and its maintenance is weighed among are one
// set.
func gitletNodeOf(lease coord.Lease) (string, bool) {
	profile, ok := placement.FromLease(lease)
	if !ok || !profile.Roles.Has(placement.RoleGitlet) {
		return "", false
	}
	return profile.ID, true
}

// serveEstate makes this data node answer the fleet from its copy of the
// estate and, with the gitlet role, from its git half on the two git lanes,
// the reads' and the engine writes' (estate's gitLanes).
//
// EVERY DATA NODE SERVES, asked yet or not (one joins without reconfiguring
// the others), from BEFORE its native runtime is up: the backend resolves per
// request, so an early request is answered "not here" rather than refused. So
// does the git half ([engineGit]): every Gitlet node holds data in this
// release (gitlet needs ingress, which needs data), and each git lane answers
// `no_backend` until Gitlet is built there.
func (e *Engine) serveEstate(ctx context.Context) error {
	if e.local == nil || e.router == nil || e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	opts := estate.ServerOptions{
		Self: e.id, Local: e.local, Seams: e.serverSeams(), Envelopes: e.envelopes,
		Answers: e.answerBudget, Taken: store.NewTakenRequests(e.backends.Store),
	}
	if e.profile.Roles.Has(placement.RoleGitlet) {
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
// wrote. No node without `data` runs beside such a node: it reaches the estate
// through the fleet's broker.
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
// WITHOUT THE CALLER'S CANCELLATION, which is often the signal it cleans up
// after: a leave inheriting it would detach nothing. The keeper bounds its own
// wait for a batch in hand.
func (e *Engine) stopKeeper(ctx context.Context) {
	if e.keeper == nil {
		return
	}
	e.keeper.Stop(context.WithoutCancel(ctx))
	e.keeper = nil
}

// stopServingEstate withdraws this node from the stateless nodes' rosters of
// who to ask. BEFORE the native runtime stops, as the search slices go first:
// a leaving node stops being asked before it stops being able to answer, so
// the next request goes to a peer rather than waiting out its budget here.
//
// AND BEFORE THE ADMISSION IS WITHDRAWN ([Engine.withdraw]), with every other
// state-log publisher: a peer's routed write and a linearizable read's barrier
// are appended through this node's publishers, and one appended after the
// admission is a write a capacity coordinator was told nobody was making.
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
// ([builtin.Actor.Provenance]), as a field left behind is lost on every write
// that crosses the router (TestAToolsActorCrossesWithItsWholeProvenance).
func remoteActor(actor builtin.Actor) estate.Actor {
	return estate.Actor{Handle: actor.Handle, Kind: actor.Kind, Provenance: actor.Provenance()}
}
