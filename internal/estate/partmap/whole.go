package partmap

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Whole is the placement of a fleet that has no estate map: under layout 0,
// every live data node serves the one partition, as this node's watched
// presence view names them ([Roster]).
//
// ONE RULE FOR BOTH READERS. A router under layout 0 routes by it directly —
// it reads nothing but presence, so a node whose store has never answered for
// the estate map still reaches the data nodes it can see — and a [View] answers
// with it while the store holds no map. Written twice, the router and the view
// could come to disagree about who serves a fleet with no map, which is the
// question every seat tool asks first.
//
// Why presence and not the estate leases is the [View]'s doc: a build from
// before the estate lease claims none while it serves the whole estate, and a
// lease's `serving` is how far its copy has applied rather than whether the
// node answers for it.
type Whole struct {
	// Running is the layout this node runs, which must be layout 0: a
	// partitioned layout with no map has no servers at all.
	Running statelog.Layout

	// Roster is every live data node, from memory.
	Roster Roster
}

// Layout is the layout this node runs, refused unless it is a valid layout 0.
func (w Whole) Layout() (statelog.Layout, error) {
	if err := w.valid(); err != nil {
		return statelog.Layout{}, err
	}
	return w.Running, nil
}

// Serving is every live data node, sorted, at epoch 0 — there is no map to
// count epochs of — for the layout's one partition, and [ErrUnknownPartition]
// for any other. An error from the roster is UNKNOWN, never "no holder".
func (w Whole) Serving(p statelog.PartitionID) ([]string, uint64, error) {
	if err := w.valid(); err != nil {
		return nil, 0, err
	}
	if parts := w.Running.Partitions(); len(parts) != 1 || parts[0] != p {
		return nil, 0, fmt.Errorf("%w: %q in layout 0", ErrUnknownPartition, p.String())
	}
	nodes, err := w.Roster.LiveDataNodes()
	if err != nil {
		return nil, 0, fmt.Errorf("estate/partmap: who serves %s under layout 0: %w", p.String(), err)
	}
	out := slices.Clone(nodes)
	slices.Sort(out)
	return out, 0, nil
}

// Refresh asks the roster to list again: under layout 0 there is no map to
// read, and a server's newer epoch — the only reason a router refreshes — is
// never met, since every node's epoch is 0.
func (w Whole) Refresh(context.Context) error {
	if err := w.valid(); err != nil {
		return err
	}
	w.Roster.Invalidate()
	return nil
}

// Unanswered asks the roster to list again, because a node it named went
// silent.
func (w Whole) Unanswered(string) {
	if w.Roster != nil {
		w.Roster.Invalidate()
	}
}

// valid refuses a Whole that is not layout 0's, or has nobody to name its
// servers — the zero value among them, which would otherwise read as a fleet
// nobody serves.
func (w Whole) valid() error {
	switch {
	case w.Running.Validate() != nil:
		return fmt.Errorf("estate/partmap: a placement with no map needs the layout this "+
			"node runs: %w", w.Running.Validate())
	case w.Running.Number != 0:
		return fmt.Errorf("%w: this node runs layout %d and the fleet has no estate "+
			"map yet, so no partition has been placed", ErrNoMap, w.Running.Number)
	case w.Roster == nil:
		return errors.New("estate/partmap: a placement under layout 0 needs the fleet's " +
			"live data nodes, which serve that layout's one partition")
	}
	return nil
}
