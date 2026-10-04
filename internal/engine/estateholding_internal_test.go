package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// A DATA NODE HOLDS ITS REPLICATED ESTATE WHATEVER ITS COMPANY RUNS, and its
// backup copies it.
//
// Holding is a fact about the node: a company that moved its tracker off the
// native backend leaves a data node's file on disk with every row its log ever
// derived. While the file was opened only by a running state log, such a node
// held nothing open, and its backup wrote a manifest for its own file alone
// and reported success. So a data node that runs no state log at all — this
// one has no company — must hold the replicated estate open from boot, and a
// backup built on the engine's answer must carry it.
func TestADataNodeHoldsItsEstateWhateverItsCompanyRuns(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	e, err := New(t.Context(), Options{Bootstrap: &b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if n := e.native.Load(); n != nil && n.log != nil {
		t.Fatal("a node with no company runs a state log, so this case shows nothing")
	}

	if !HoldsEstate(&b) {
		t.Fatal("a node running every role does not hold the replicated estate")
	}
	if !replicatedOpen(e.backends.Store) {
		t.Error("a data node running no state log does not hold its replicated estate open")
	}

	fleet := memory.NewFleet()
	svc, err := backup.New(backup.Options{
		Store: e.backends.Store, Estate: backup.HoldingFor(HoldsEstate(e.boot)), NodeID: e.id,
		Holds: fleet, Backups: fleet,
	})
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
// The state log runs the logs of what the node holds; the store's own Close is
// what gives the file back. A runtime that closed it on its way out left a
// node — an apply that failed after its runtime came up, an engine stopped
// over backends that outlive it — holding nothing a backup, a retention report
// or the next runtime would find.
func TestANodeKeepsItsEstateWhenItsStateLogStops(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	if n := e.native.Load(); n == nil || n.log == nil {
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
// Its store is scratch, deleted at every boot, and a replicated estate opened
// in it would be an empty database answering as a company with nothing in it
// — and the backup, which reads the same answer, would copy a file there that
// no engine ever opens. (`crewlet migrate` refuses the scratch store outright.)
func TestANodeWithoutDataHoldsNothing(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Node.Roles = []string{"seats"}
	if HoldsEstate(&b) {
		t.Error("a node without data holds the replicated estate")
	}
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "node.db"), store.Options{})
	if err != nil {
		t.Fatalf("open the node's store: %v", err)
	}
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
