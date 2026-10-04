package storetest

import (
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// OpenEstate opens a node's own store at path and its replicated estate beside
// it — the shape of every data node — and answers the node's handle with the
// handle the runtime would hand the estate's appliers and readers. logs is how
// many logs' apply loops run on the estate, each of which pins one writer on
// the file for the life of the loop. The caller closes the node, which closes
// the estate with it.
func OpenEstate(t testing.TB, path string, opts store.Options, logs int) (*store.DB, store.ReplicatedHandle) {
	t.Helper()
	node, err := store.OpenNode(t.Context(), path, opts)
	if err != nil {
		t.Fatalf("open the node's store at %s: %v", path, err)
	}
	if _, err := node.OpenReplicated(t.Context(), logs); err != nil {
		_ = node.Close()
		t.Fatalf("open the replicated estate beside %s: %v", path, err)
	}
	return node, node.Replicated()
}

// ReplicatedDB is the open handle behind estate, failing the test when it is
// not open — for a test that needs the file itself, its path or a copy of it,
// rather than the rows through it.
func ReplicatedDB(t testing.TB, estate store.ReplicatedHandle) *store.DB {
	t.Helper()
	db, err := estate.DB()
	if err != nil {
		t.Fatalf("the replicated estate: %v", err)
	}
	return db
}
