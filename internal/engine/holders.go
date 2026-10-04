package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// liveData is who the fleet says is a live DATA node: the half of every log's
// counted set that is not the positions register ([statelog.CountedSet]).
//
// # Why every log counts the live data nodes
//
// The trim may not remove a record a node that applies the log has not applied,
// and every data node applies every log from boot — the estate is one, held
// whole by each of them — while a node without `data` applies none. So a log's
// counted set is the register's rows naming it, UNION every live data node,
// whether or not that node has reported a position — one that has not is one
// about to replay, and counts at zero — MINUS the tombstones past their window.
// Counting every live node instead would pin every log for as long as a node
// without `data` ran, at a position it never reports.
type liveData interface {
	LiveData(ctx context.Context) ([]statelog.Presence, error)
}

// holdersOf is who holds the estate as every counted set on this node reads it:
// the live data nodes, listed afresh ([presenceHolders]).
//
// ONE CHOICE, made here, for the trim and the embedding duty alike: the two read
// one log's counted set for two questions — what may be removed, and who must
// read a record before it is published — and answered from two sources they
// could disagree about who the log's readers are.
func (e *Engine) holdersOf() liveData {
	return presenceHolders{leases: e.backends.Coord}
}

// watchedHolders is who holds the estate as this node's WATCHED presence view
// answers it ([viewHolders]), for a question asked often that decides nothing
// a view's age could make unsafe: whether the estate has anybody to donate a
// snapshot to ([Engine.counted]).
//
// NOT THE TRIM'S ANSWER, which lists presence afresh on every tick
// ([Engine.holdersOf]): the trim licenses removing records, and a listing up to
// a heartbeat old may miss a node that has just booted — the one node the trim
// must not pass.
func (e *Engine) watchedHolders() liveData {
	return viewHolders{view: e.dataView}
}

// viewHolders answers who the live data nodes are from the watched presence
// view: [presenceHolders]' answer, from memory rather than a listing.
type viewHolders struct{ view *coord.LeaseView }

// LiveData is every live data node the view names, or why the view cannot
// say.
func (h viewHolders) LiveData(context.Context) ([]statelog.Presence, error) {
	nodes, err := presenceRoster(h).LiveDataNodes()
	if err != nil {
		return nil, err
	}
	live := make([]statelog.Presence, 0, len(nodes))
	for _, id := range nodes {
		live = append(live, statelog.Presence{NodeID: id})
	}
	return live, nil
}

// presenceHolders answers who the live data nodes are from a fresh listing of
// the presence leases — the same live data nodes the router asks.
type presenceHolders struct{ leases liveLeases }

// LiveData is every live data node, listed now.
func (h presenceHolders) LiveData(ctx context.Context) ([]statelog.Presence, error) {
	return livePresences(ctx, h.leases)
}
