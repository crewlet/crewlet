package kv

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the nodes' status records ------------------------------------------ //

// A node's status record is rows of the node status bucket, each under a key
// of its class ([coord.NodeStatusRowKey]): `node_status.<node>`,
// `mcp_status.<node>.<server>`, `model_status.<node>.<entry>` and
// `growth_status.<node>`, each class declared at [coord.MaxNodeStatusRowBytes]
// and read at the term ([nodeStatusHolding]). A node writes only under its own
// name, and the node is stamped from the key. The bucket's age, the apply
// status's, removes a dead node's rows (coord's nodestatusrecord.go).

// PutNodeStatusRow writes one row of this node's status, replacing it unless
// it holds a later epoch's.
//
// CONDITIONAL ON THE REVISION READ, as a removal is: the key's writers are
// its node's incarnations, and an earlier one's write still in flight never
// lands over a later one's row ([coord.NodeStatusRecords]). A round repeats
// only when the key moved between the read and the write, so rounds stay far
// below the shared bound ([fleetCASAttempts]). Every write refreshes the key's
// age, keeping a live node's row from ageing out.
func (f *FleetStore) PutNodeStatusRow(ctx context.Context, row coord.NodeStatusRow) error {
	if err := row.Check(); err != nil {
		return err
	}
	if b, ok := f.nodeStatus.(settledBucket); ok {
		if err := b.carriedBack(row.Key.Key(), row.Value); err != nil {
			return err
		}
	}
	epoch, _ := coord.NodeStatusRowEpoch(row.Value)
	key, what := row.Key.Key(), fmt.Sprintf("write %s's status row %s", row.Key.Node, row.Key.Key())
	settled, err := casRetry(ctx, &f.nodeStatusTrips, what, func() (bool, error) {
		kve, err := f.get(ctx, f.nodeStatus, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			_, err = f.create(ctx, f.nodeStatus, key, row.Value)
			if errors.Is(err, jetstream.ErrKeyExists) {
				return false, nil
			}
		case err != nil:
			return false, unavailable(what, err)
		default:
			if was, ok := coord.NodeStatusRowEpoch(kve.Value()); ok && was > epoch {
				// A LATER INCARNATION'S ROW stands: this writer's
				// presence has moved on, and it is no error.
				return true, nil
			}
			_, err = f.nodeStatus.Update(ctx, key, row.Value, kve.Revision())
			if errors.Is(err, jetstream.ErrKeyRevisionMismatch) {
				return false, nil
			}
		}
		if err != nil {
			return false, unavailable(what, err)
		}
		return true, nil
	})
	switch {
	case err != nil:
		return err
	case !settled:
		return contended("PutNodeStatusRow", key)
	}
	return nil
}

// NodeStatusRecords returns every node's record, ordered by node: a pass over
// the nodes' own rows, one over each class read names, and the own rows again,
// never the whole bucket.
func (f *FleetStore) NodeStatusRecords(ctx context.Context, read coord.NodeStatusRead) ([]coord.NodeStatusRecord, error) {
	var rows []coord.NodeStatusRow
	classes := read.Classes()
	// THE OWN ROWS AGAIN, LAST: a node whose own row went or moved while
	// the other classes were read is not counted whole
	// ([coord.AssembleNodeStatusRecords]).
	if len(classes) > 1 {
		classes = append(classes, coord.NodeStatusOwn)
	}
	reread := []coord.NodeStatusRow{}
	for i, class := range classes {
		err := f.eachUnder(ctx, f.nodeStatus, coord.DocumentFilter(string(class)), "the nodes' "+string(class)+" rows",
			func(kve jetstream.KeyValueEntry) error {
				key, ok := coord.ParseNodeStatusKey(kve.Key())
				if !ok || key.Class != class {
					// A key this backend did not write names no row.
					return nil
				}
				// COPIED: the entry's buffer belongs to the client.
				row := coord.NodeStatusRow{Key: key, Value: bytes.Clone(kve.Value())}
				if i > 0 && class == coord.NodeStatusOwn {
					reread = append(reread, row)
				} else {
					rows = append(rows, row)
				}
				return nil
			})
		if err != nil {
			return nil, err
		}
	}
	if len(classes) == 1 {
		// ONE PASS IS ONE READ of every row it returns.
		return coord.AssembleNodeStatusRecords(rows, nil), nil
	}
	return coord.AssembleNodeStatusRecords(rows, reread), nil
}

// RemoveNodeStatusRow removes one row where it was written at an epoch no
// later than through; one that is not there, or is a later epoch's, is no
// error.
func (f *FleetStore) RemoveNodeStatusRow(ctx context.Context, key coord.NodeStatusRowKey, through int64) error {
	if err := key.Check(); err != nil {
		return err
	}
	kve, err := f.get(ctx, f.nodeStatus, key.Key())
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil
	case err != nil:
		return unavailable(fmt.Sprintf("read %s to remove it", key.Key()), err)
	}
	return f.removeNodeStatusEntry(ctx, kve, through)
}

// removeNodeStatusEntry purges a row as read unless it was written at an
// epoch past through.
//
// A PURGE AT THE REVISION READ: a key that moved since was written by a
// later incarnation, or by this node's writer again, and that row stays. A
// purge's marker ages out with the bucket. An undecodable row is removed
// too: no reader counts it.
func (f *FleetStore) removeNodeStatusEntry(ctx context.Context, kve jetstream.KeyValueEntry, through int64) error {
	if epoch, ok := coord.NodeStatusRowEpoch(kve.Value()); ok && epoch > through {
		return nil
	}
	err := f.nodeStatus.Purge(ctx, kve.Key(), jetstream.LastRevision(kve.Revision()))
	switch {
	case err == nil:
		return nil
	case lostUpdateRace(err):
		return nil
	default:
		return unavailable(fmt.Sprintf("remove %s", kve.Key()), err)
	}
}

// DeleteNodeStatus removes every row of a node written at an epoch no later
// than through: its own row FIRST, so a listing that met it before reads it
// gone when it reads the own rows again after its other passes, and counts
// none of the node ([coord.AssembleNodeStatusRecords]); then its growth row,
// then a pass over each named class under the node. One that is not there, or is a later epoch's, is no
// error.
func (f *FleetStore) DeleteNodeStatus(ctx context.Context, node string, through int64) error {
	if node == "" {
		return fmt.Errorf("%w (removing one)", coord.ErrNoNodeStatus)
	}
	for _, class := range []coord.NodeStatusClass{coord.NodeStatusOwn, coord.NodeStatusGrowth} {
		if err := f.RemoveNodeStatusRow(ctx, coord.NodeStatusRowKey{Class: class, Node: node}, through); err != nil {
			return err
		}
	}
	for _, class := range []coord.NodeStatusClass{coord.NodeStatusMCP, coord.NodeStatusModel} {
		var found []jetstream.KeyValueEntry
		err := f.eachUnder(ctx, f.nodeStatus, coord.DocumentFilter(string(class), node),
			"the "+string(class)+" rows of "+node+" to remove them", func(kve jetstream.KeyValueEntry) error {
				found = append(found, kve)
				return nil
			})
		if err != nil {
			return err
		}
		for _, kve := range found {
			if err := f.removeNodeStatusEntry(ctx, kve, through); err != nil {
				return err
			}
		}
	}
	return nil
}
