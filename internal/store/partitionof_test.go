package store_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// partitionOf is layout 0's one partition on a node's store as the file it is
// open as now, failing the test when it is not open — for a case about the
// file's own mechanics (its pool, its pins, its lock) rather than about a
// holder that keeps a handle on it.
func partitionOf(tb testing.TB, node *store.DB) *store.DB {
	tb.Helper()
	return storetest.Partition(tb, storetest.EstateOf(node))
}

// openPartitioned opens a node's store at path with opts, and layout 0's one
// partition beside it carrying logs logs, and answers the node's handle and
// the partition's. The caller closes the node, which closes both.
func openPartitioned(tb testing.TB, path string, opts store.Options, logs int) (*store.DB, *store.DB) {
	tb.Helper()
	node, _ := storetest.OpenEstate(tb, path, opts, logs)
	return node, partitionOf(tb, node)
}
