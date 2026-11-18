package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// A DATA NODE HOLDS ITS REPLICATED ESTATE WHATEVER ITS COMPANY RUNS, and its
// backup copies it.
//
// Holding is a fact about the node: a company that moved its tracker off
// Worklet still leaves every derived row in the node's file. So a data node
// running no native half (this one has no company) holds the estate open from
// boot, or its backup would copy its own file alone and report success.
func TestADataNodeHoldsItsEstateWhateverItsCompanyRuns(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	e, err := New(t.Context(), Options{Bootstrap: &b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if e.NativeStarted() {
		t.Fatal("a node with no company runs the native half, so this case shows nothing")
	}

	if !HoldsEstate(&b) {
		t.Fatal("a node running every role does not hold the replicated estate")
	}
	if !replicatedOpen(e.backends.Store) {
		t.Error("a data node running no native half does not hold its replicated estate open")
	}

	fleet := memory.NewFleet()
	opts := e.BackupOptions()
	opts.Holds, opts.Backups = fleet, fleet
	svc, err := backup.New(opts)
	if err != nil {
		t.Fatalf("backup.New: %v", err)
	}
	manifest, err := svc.Take(t.Context(), filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	var estates, files []string
	for _, st := range manifest.Stores {
		estates = append(estates, string(st.Estate))
		files = append(files, st.File)
	}
	if want := []string{"node", "replicated"}; !slices.Equal(estates, want) {
		t.Errorf("the backup of a data node running no state log copied the "+
			"estates %v, want %v — without the replicated one the backup is "+
			"missing every row the node's file holds and says nothing", estates, want)
	}
	if want := []string{"store.db", "store-replicated.db"}; !slices.Equal(files, want) {
		t.Errorf("the backup filed its copies as %v, want %v", files, want)
	}
	if got := manifest.Stores[len(manifest.Stores)-1].Source; got != e.backends.Store.ReplicatedFile() {
		t.Errorf("the replicated copy was taken from %s, want the node's own %s",
			got, e.backends.Store.ReplicatedFile())
	}
}

// A NODE KEEPS ITS REPLICATED ESTATE WHEN ITS STATE LOG STOPS.
//
// Only the store's own Close gives the file back. A runtime closing it on its
// way out (an apply failed after its runtime came up, an engine stopped over
// backends that outlive it) leaves nothing for a backup, a retention report or
// the next runtime.
func TestANodeKeepsItsEstateWhenItsStateLogStops(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	if c := e.core.Load(); c == nil || c.log == nil {
		t.Fatal("the running node runs no state log, so there is nothing to stop")
	}
	e.Stop(context.Background())
	if !replicatedOpen(e.backends.Store) {
		t.Error("stopping the engine's state log closed the replicated estate " +
			"the node holds, before the store that holds it was closed")
	}
}

// A NODE WITHOUT `data` HOLDS NOTHING, and opens nothing.
//
// Its store is scratch, deleted at every boot: an estate opened there would
// answer as an empty company, and the backup would copy a file no engine
// opens.
func TestANodeWithoutDataHoldsNothing(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Node.Roles = []string{"seats"}
	if HoldsEstate(&b) {
		t.Error("a node without data holds the replicated estate")
	}
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	if err := holdEstate(t.Context(), db, &b); err != nil {
		t.Fatalf("hold the estate of a node without data: %v", err)
	}
	if replicatedOpen(db) {
		t.Error("a node without data opened a replicated estate at boot")
	}
	b.Node.Roles = nil
	if !HoldsEstate(&b) {
		t.Error("a node running every role does not hold the replicated estate")
	}
}
