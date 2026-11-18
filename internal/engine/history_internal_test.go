package engine

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A NODE WITHOUT DATA BUILDS NO HISTORY READER: it holds no history (its store
// is scratch; custody puts every event it publishes in one data node's log) and
// nothing on it asks, so a reader there would read its scratch store as a part
// and name the node in every coverage. A data node's reader reads its own
// store.
//
// Mutation: build the reader before armHistory's role check, and the node
// without data has one.
func TestANodeWithoutDataBuildsNoHistoryReader(t *testing.T) {
	t.Parallel()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })

	stateless := &Engine{id: "agent-1", backends: &Backends{Store: db}}
	if err := stateless.armHistory(t.Context()); err != nil {
		t.Fatalf("arm a node without data: %v", err)
	}
	if stateless.History() != nil {
		t.Error("a node without data built a history reader over a store that holds no history")
	}

	data := &Engine{id: "data-1", backends: &Backends{Store: db}}
	data.local = newLocalEstate(data)
	if err := data.armHistory(t.Context()); err != nil {
		t.Fatalf("arm a data node: %v", err)
	}
	if h := data.History(); h == nil || h.Local == nil || h.Self != "data-1" {
		t.Errorf("a data node's reader is %+v, want one reading its own store as data-1", h)
	}
}
