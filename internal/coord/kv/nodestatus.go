package kv

import (
	"context"
	"errors"
	"fmt"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the nodes' status records ------------------------------------------ //

// A node's status record is one key of the node status bucket, its encoded
// node id: a node writes only under its own name, and the node is stamped from
// the key. The bucket's age, the apply status's, removes a dead node's record
// (coord's nodestatusrecord.go).

// PutNodeStatus writes this node's record, replacing it.
//
// A PLAIN PUT: the key's only writer is the node it names, and every write
// refreshes the key's age, keeping a live node's record from ageing out.
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
		// since failing the listing over one row blanks every node's rows,
		// where dropping it reads that one node as not saying.
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
// A READ, THEN A PURGE AT THE REVISION READ: this node's writer has stopped,
// so a key that moved since was written by a later incarnation, and that
// record stays. A purge's marker ages out with the bucket.
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
	// An undecodable record is removed too: no reader counts it.
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
