package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// liveData is who the fleet says is a live DATA node: the half of every log's
// counted set that is not the positions register ([statelog.CountedSet]).
//
// The trim may not remove a record a node applying the log has not applied;
// every data node applies every log from boot and a node without `data`
// applies none. So a log's counted set is the register's rows naming it, UNION
// every live data node (one with no row yet counts at zero), MINUS the
// tombstones past their window. Counting every live node instead would pin
// every log while a node without `data` ran, at a position it never reports.
type liveData interface {
	LiveData(ctx context.Context) ([]statelog.Presence, error)
}

// holdersOf is who holds the estate as every counted set on this node reads it:
// the live data nodes, listed afresh ([presenceHolders]). The one choice for
// every reading of a counted set that licenses removing records (the trim's),
// so no two can disagree about who the log's readers are.
func (e *Engine) holdersOf() liveData {
	return presenceHolders{leases: e.backends.Coord}
}

// watchedHolders is who holds the estate as this node's WATCHED presence view
// answers it ([viewHolders]), for a frequent question no stale view makes
// unsafe: whether the estate has anybody to donate a snapshot to
// ([Engine.counted]).
//
// Not the trim's fresh listing ([Engine.holdersOf]): a view up to a heartbeat old can
// miss a live data node that has just taken its lease with no row landed yet,
// the node the trim counts at position zero ([statelog.CountedSet]) and must
// not pass.
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
