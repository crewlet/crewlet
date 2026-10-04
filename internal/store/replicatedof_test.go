package store_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// replicatedOf is a node's replicated estate as the file it is open as now,
// failing the test when it is not open — for a case about the file's own
// mechanics (its pool, its pins, its lock) rather than about a holder that
// keeps a handle on it.
func replicatedOf(tb testing.TB, node *store.DB) *store.DB {
	tb.Helper()
	return storetest.ReplicatedDB(tb, node.Replicated())
}

// openReplicated opens a node's store at path with opts, and its replicated
// estate beside it carrying logs logs, and answers the node's handle and the
// estate's. The caller closes the node, which closes both.
func openReplicated(tb testing.TB, path string, opts store.Options, logs int) (*store.DB, *store.DB) {
	tb.Helper()
	node, _ := storetest.OpenEstate(tb, path, opts, logs)
	return node, replicatedOf(tb, node)
}
