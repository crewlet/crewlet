package storetest

import (
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// LayoutZero is layout 0's one partition — the whole estate, as every node
// held it before it was divided — carrying logs logs, each of which pins one
// writer on the file for the life of its apply loop.
func LayoutZero(logs int) store.PartitionFile {
	return store.PartitionFile{Layout: 0, Name: "estate.000", Logs: logs}
}

// OpenEstate opens a node's own store at path and layout 0's one partition
// beside it — the shape of every data node — and answers the node's handle
// with the handle the runtime would hand the partition's appliers and
// readers. The caller closes the node, which closes the partition with it.
func OpenEstate(t testing.TB, path string, opts store.Options, logs int) (*store.DB, store.PartitionHandle) {
	t.Helper()
	node, err := store.OpenNode(t.Context(), path, opts)
	if err != nil {
		t.Fatalf("open the node's store at %s: %v", path, err)
	}
	file := LayoutZero(logs)
	if _, err := node.OpenPartition(t.Context(), file); err != nil {
		_ = node.Close()
		t.Fatalf("open %s beside %s: %v", file.Name, path, err)
	}
	return node, node.PartitionHandle(file.Name)
}

// Partition is the open handle behind estate, failing the test when it is
// not open — for a test that needs the file itself, its path or a copy of
// it, rather than the rows through it.
func Partition(t testing.TB, estate store.PartitionHandle) *store.DB {
	t.Helper()
	db, err := estate.DB()
	if err != nil {
		t.Fatalf("the partition %s: %v", estate.Name(), err)
	}
	return db
}

// EstateOf is layout 0's one partition on a node [OpenEstate] opened, as the
// handle the runtime would hand its appliers and readers — for a test that
// keeps the node's own handle and reaches the partition beside it.
func EstateOf(node *store.DB) store.PartitionHandle {
	return node.PartitionHandle(LayoutZero(1).Name)
}
