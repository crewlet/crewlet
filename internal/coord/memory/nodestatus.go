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
// it was written under ([coord.LiveNodeStatuses]) whatever the store keeps.
func (f *Fleet) NodeStatusRecords(context.Context) ([]coord.NodeStatusRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]coord.NodeStatusRecord, 0, len(f.nodeStatuses))
	for _, node := range slices.Sorted(maps.Keys(f.nodeStatuses)) {
		r, err := coord.DecodeNodeStatusRecord(node, f.nodeStatuses[node])
		if err != nil {
			return nil, fmt.Errorf("coord/memory: %w", err)
		}
		out = append(out, r)
	}
	return out, nil
}

// DeleteNodeStatus removes a node's record; one that is not there is no error.
func (f *Fleet) DeleteNodeStatus(_ context.Context, node string) error {
	if node == "" {
		return fmt.Errorf("%w (removing one)", coord.ErrNoNodeStatus)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nodeStatuses, node)
	return nil
}
