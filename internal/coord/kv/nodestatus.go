package kv

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the nodes' status records ------------------------------------------ //

// A node's status record is one key of the node status bucket, the node's id
// encoded as every node key is, so a node writes only under its own name and a
// record cannot say otherwise: the node is stamped from the key. The bucket's
// age is the apply status's, which is what removes a dead node's record
// (coord's nodestatusrecord.go).

// PutNodeStatus writes this node's record, replacing it.
//
// A PLAIN PUT, as the apply status is: the only writer of the key is the node
// it names, so there is no race to arbitrate, and every write refreshes the
// key's age, which is what keeps a live node's record from ageing out.
func (f *FleetStore) PutNodeStatus(ctx context.Context, r coord.NodeStatusRecord) error {
	if err := r.Check(); err != nil {
		return err
	}
	raw, err := r.Encode()
	if err != nil {
		return err
	}
	if _, err := f.nodeStatus.Put(ctx, encodeKey(r.Node), raw); err != nil {
		return unavailable(fmt.Sprintf("write %s's status record", r.Node), err)
	}
	return nil
}

// NodeStatusRecords returns every record the bucket holds, ordered by node.
func (f *FleetStore) NodeStatusRecords(ctx context.Context) ([]coord.NodeStatusRecord, error) {
	var out []coord.NodeStatusRecord
	err := f.each(ctx, f.nodeStatus, func(kve jetstream.KeyValueEntry) error {
		node, ok := decodeKey(kve.Key())
		if !ok {
			return nil
		}
		r, err := coord.DecodeNodeStatusRecord(node, kve.Value())
		//nolint:nilerr // An undecodable record is SKIPPED rather than raised,
		// for the apply status's reason: failing the listing over one row
		// would blank every node's rows, where dropping it reads that one
		// node as not saying.
		if err != nil {
			return nil
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	coord.SortNodeStatusRecords(out)
	return out, nil
}

// DeleteNodeStatus removes a node's record where it was written at an epoch no
// later than through; one that is not there, or is a later epoch's, is no
// error.
//
// A READ, THEN A PURGE AT THE REVISION READ: the purge refuses where the key
// moved since, which is a later write — this node's writer has stopped, so a
// later incarnation's — and that record stays. Purge rather than delete, as a
// released claim is: the marker it leaves ages out with the bucket, and the
// node's next incarnation writes over it.
func (f *FleetStore) DeleteNodeStatus(ctx context.Context, node string, through int64) error {
	if node == "" {
		return fmt.Errorf("%w (removing one)", coord.ErrNoNodeStatus)
	}
	key := encodeKey(node)
	kve, err := f.get(ctx, f.nodeStatus, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil
	case err != nil:
		return unavailable(fmt.Sprintf("read %s's status record to remove it", node), err)
	}
	if r, decodeErr := coord.DecodeNodeStatusRecord(node, kve.Value()); decodeErr == nil && r.Epoch > through {
		return nil
	}
	// A record that does not decode is removed with the rest: no reader
	// counts it, and nothing later than this node wrote it that could.
	err = f.nodeStatus.Purge(ctx, key, jetstream.LastRevision(kve.Revision()))
	switch {
	case err == nil:
		return nil
	case lostUpdateRace(err):
		return nil
	default:
		return unavailable(fmt.Sprintf("remove %s's status record", node), err)
	}
}
