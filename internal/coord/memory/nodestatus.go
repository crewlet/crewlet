package memory

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the nodes' status records ------------------------------------------ //

// PutNodeStatus writes a node's record, replacing it. Kept as the bytes the KV
// keeps, so a caller's slices are never the stored record's and a reader
// decodes what a real store would hand it.
func (f *Fleet) PutNodeStatus(_ context.Context, r coord.NodeStatusRecord) error {
	if err := r.Check(); err != nil {
		return err
	}
	raw, err := r.Encode()
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodeStatuses[r.Node] = raw
	return nil
}

// NodeStatusRecords returns every record, ordered by node.
//
// The twin keeps no age: it serves the nodes of one process, so a node that
// died is the process itself, and readers join every record to the presence
// it was written under ([coord.LiveNodeStatuses]) whatever the store keeps. A
// record that does not decode is SKIPPED, as the KV skips it: one node read as
// not saying, never every node's rows failed over one.
func (f *Fleet) NodeStatusRecords(context.Context) ([]coord.NodeStatusRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.NodeStatusRecord, 0, len(f.nodeStatuses))
	for _, node := range slices.Sorted(maps.Keys(f.nodeStatuses)) {
		if r, err := coord.DecodeNodeStatusRecord(node, f.nodeStatuses[node]); err == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// DeleteNodeStatus removes a node's record where it was written at an epoch no
// later than through; one that is not there, or is a later epoch's, is no
// error.
func (f *Fleet) DeleteNodeStatus(_ context.Context, node string, through int64) error {
	if node == "" {
		return fmt.Errorf("%w (removing one)", coord.ErrNoNodeStatus)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, ok := f.nodeStatuses[node]
	if !ok {
		return nil
	}
	if r, err := coord.DecodeNodeStatusRecord(node, raw); err == nil && r.Epoch > through {
		return nil
	}
	delete(f.nodeStatuses, node)
	return nil
}
