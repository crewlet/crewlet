package memory

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the nodes' status records ------------------------------------------ //

// PutNodeStatusRow writes one row of a node's status, replacing it unless it
// holds a later epoch's, held to
// [coord.MaxNodeStatusRowBytes] as the KV holds it. Kept as the bytes the KV
// keeps, so a caller's slice is never the stored row and a reader decodes
// what a real store would hand it.
func (f *Fleet) PutNodeStatusRow(_ context.Context, row coord.NodeStatusRow) error {
	if err := row.Check(); err != nil {
		return err
	}
	key := row.Key.Key()
	if err := coord.HoldValue(key, len(row.Value), coord.MaxNodeStatusRowBytes); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	epoch, _ := coord.NodeStatusRowEpoch(row.Value)
	if was, ok := coord.NodeStatusRowEpoch(f.nodeStatuses[key]); ok && was > epoch {
		// A later incarnation's row stands, as the KV leaves it.
		return nil
	}
	f.nodeStatuses[key] = slices.Clone(row.Value)
	return nil
}

// RemoveNodeStatusRow removes one row where it was written at an epoch no
// later than through; one that is not there, or is a later epoch's, is no
// error.
func (f *Fleet) RemoveNodeStatusRow(_ context.Context, key coord.NodeStatusRowKey, through int64) error {
	if err := key.Check(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeNodeStatusRow(key.Key(), through)
	return nil
}

// removeNodeStatusRow removes the row under key unless it was written at an
// epoch past through. An undecodable row is removed too: no reader counts
// it. Called under mu.
func (f *Fleet) removeNodeStatusRow(key string, through int64) {
	raw, ok := f.nodeStatuses[key]
	if !ok {
		return
	}
	if epoch, ok := coord.NodeStatusRowEpoch(raw); ok && epoch > through {
		return
	}
	delete(f.nodeStatuses, key)
}

// NodeStatusRecords returns every node's record, ordered by node, from its
// own row and the classes read names.
//
// The twin keeps no age: it serves the nodes of one process, so a node that
// died is the process itself, and readers join every record to the presence
// it was written under ([coord.LiveNodeStatuses]) whatever the store keeps.
func (f *Fleet) NodeStatusRecords(_ context.Context, read coord.NodeStatusRead) ([]coord.NodeStatusRecord, error) {
	classes := read.Classes()
	f.mu.Lock()
	defer f.mu.Unlock()
	var rows []coord.NodeStatusRow
	for key, raw := range f.nodeStatuses {
		if k, ok := coord.ParseNodeStatusKey(key); ok && slices.Contains(classes, k.Class) {
			rows = append(rows, coord.NodeStatusRow{Key: k, Value: slices.Clone(raw)})
		}
	}
	return coord.AssembleNodeStatusRecords(rows, nil), nil
}

// DeleteNodeStatus removes every row of a node written at an epoch no later
// than through; one that is not there, or is a later epoch's, is no error.
func (f *Fleet) DeleteNodeStatus(_ context.Context, node string, through int64) error {
	if node == "" {
		return fmt.Errorf("%w (removing one)", coord.ErrNoNodeStatus)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for key := range f.nodeStatuses {
		if k, ok := coord.ParseNodeStatusKey(key); ok && k.Node == node {
			f.removeNodeStatusRow(key, through)
		}
	}
	return nil
}
