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
//
// Each file is SEEDED from the binary's migrated image when nothing is there
// yet ([Seed]) — the replicated one where [store.ReplicatedPath] puts it for
// opts, so a caller's store.replicated_path is honoured — and a file that IS
// there is opened as it is: a test reopening its own estate, or handing in a
// replicated copy it advanced itself, gets exactly that file.
func OpenEstate(t testing.TB, path string, opts store.Options, logs int) (*store.DB, store.ReplicatedHandle) {
	t.Helper()
	if !opts.Scratch {
		Seed(t, store.EstateNode, path)
	}
	Seed(t, store.EstateReplicated, store.ReplicatedPath(path, opts.ReplicatedPath))
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
