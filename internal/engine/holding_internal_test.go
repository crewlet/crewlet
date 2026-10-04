package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A DATA NODE HOLDS ITS PARTITION WHATEVER ITS COMPANY RUNS, and its backup
// copies it.
//
// Holding is a fact about the node: a company that moved its tracker off the
// native backend leaves a data node's file on disk with every row its log ever
// derived. While the file was opened only by a running state log, such a node
// held nothing open, and its backup wrote a manifest for its own file alone
// and reported success. So a data node that runs no state log at all — this
// one has no company — must hold layout 0's partition from boot, and a backup
// built on the engine's answer must carry it.
func TestADataNodeHoldsItsPartitionWhateverItsCompanyRuns(t *testing.T) {
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

	if !estateZeroOpen(e.backends.Store) {
		t.Error("a data node running no state log does not hold layout 0's partition open")
	}
	held, err := e.HeldPartitions()
	if err != nil {
		t.Fatalf("HeldPartitions: %v", err)
	}
	want, err := LayoutZero().File(statelog.EstatePartition)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(held, []store.PartitionFile{want}) {
		t.Errorf("a data node holds %+v, want layout 0's one partition %+v", held, want)
	}

	fleet := memory.NewFleet()
	svc, err := backup.New(backup.Options{
		Store: e.backends.Store, Partitions: e.HeldPartitions, NodeID: e.id,
		Holds: fleet, Backups: fleet,
	})
	if err != nil {
		t.Fatalf("backup.New: %v", err)
	}
	manifest, err := svc.Take(t.Context(), filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	var partitions []string
	for _, st := range manifest.Stores {
		if st.Estate == store.EstatePartition {
			partitions = append(partitions, st.Partition)
		}
	}
	if !slices.Equal(partitions, []string{want.Name}) {
		t.Errorf("the backup of a data node running no state log copied the "+
			"partitions %v, want %s — without it the backup is missing every row "+
			"the node's file holds and says nothing", partitions, want.Name)
	}
}

// A NODE KEEPS ITS PARTITION WHEN ITS STATE LOG STOPS.
//
// The state log runs the logs of what the node holds; the store's own Close is
// what gives the files back. A runtime that closed them on its way out left a
// node — an apply that failed after its runtime came up, an engine stopped
// over backends that outlive it — holding nothing a backup, a retention report
// or the next runtime would find.
func TestANodeKeepsItsPartitionWhenItsStateLogStops(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	if n := e.native.Load(); n == nil || n.log == nil {
		t.Fatal("the running node runs no state log, so there is nothing to stop")
	}
	e.Stop(context.Background())
	if !estateZeroOpen(e.backends.Store) {
		t.Error("stopping the engine's state log closed the partition the node " +
			"holds, before the store that holds it was closed")
	}
}

// A NODE WITHOUT `data` HOLDS NOTHING.
//
// Its store is scratch, deleted at every boot, and a partition opened in it
// would be an empty database answering as a company with nothing in it — and
// `crewlet migrate`, which reads the same answer, would create a file there
// that no engine ever opens.
func TestANodeWithoutDataHoldsNothing(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Node.Roles = []string{"seats"}
	held, err := HeldPartitions(&b)
	if err != nil {
		t.Fatalf("HeldPartitions: %v", err)
	}
	if len(held) != 0 {
		t.Errorf("a node without data holds %+v", held)
	}
	b.Node.Roles = nil
	if held, err = HeldPartitions(&b); err != nil || len(held) != 1 {
		t.Errorf("a node running every role holds %+v (%v), want layout 0's one partition", held, err)
	}
}

// A NODE SERVES EXACTLY THE PARTITIONS IT HOLDS — gate 3's answer, which only a
// node that serves a partition passes to write its logs.
//
// No partition is joined or left while this build runs, so serving is holding:
// a data node serves layout 0's one partition from boot and a node without
// `data` serves nothing, which is what it always was — it runs no applier and
// so no publisher. Read from the rule [HeldPartitions] reads, so the files a
// node keeps open and the logs it may write cannot be two answers: a data node
// running the partitioned test layout serves each of that layout's partitions
// and not layout 0's.
func TestANodeServesExactlyThePartitionsItHolds(t *testing.T) {
	t.Parallel()
	serves := func(h statelog.Holding, p statelog.PartitionID) bool {
		t.Helper()
		serving, err := h.Serving(p)
		if err != nil {
			t.Fatalf("Serving(%s): %v — a fixed set always answers", p, err)
		}
		return serving
	}
	tracker0 := statelog.PartitionID{Space: statelog.SpaceTracker}

	b := config.DefaultBootstrap()
	data := holdingOf(&b, LayoutZero())
	if !serves(data, statelog.EstatePartition) {
		t.Errorf("a data node does not serve %s, so it could write nothing", statelog.EstatePartition)
	}
	if serves(data, tracker0) {
		t.Errorf("a data node running layout 0 serves %s, a partition that layout does not have", tracker0)
	}

	partitioned := holdingOf(&b, partitionedTestLayout())
	for _, p := range partitionedTestLayout().Partitions() {
		if !serves(partitioned, p) {
			t.Errorf("a data node running the partitioned test layout does not serve its %s", p)
		}
	}
	if serves(partitioned, statelog.EstatePartition) {
		t.Errorf("a data node running the partitioned test layout serves layout 0's %s",
			statelog.EstatePartition)
	}

	b.Node.Roles = []string{"seats"}
	for _, layout := range []statelog.Layout{LayoutZero(), partitionedTestLayout()} {
		dataless := holdingOf(&b, layout)
		for _, p := range layout.Partitions() {
			if serves(dataless, p) {
				t.Errorf("a node without data serves %s of layout %d", p, layout.Number)
			}
		}
	}
}

// A RUNNING DATA NODE'S STATE LOG SERVES WHAT THE NODE HOLDS, and its writes
// pass gate 3 — the holding the runtime is built with is the one every
// publisher it builds asks.
func TestARunningDataNodesStateLogServesItsPartition(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	serving, err := s.holding.Serving(statelog.EstatePartition)
	if err != nil || !serving {
		t.Fatalf("the running node's state log serves %s = (%v, %v), want (true, nil)",
			statelog.EstatePartition, serving, err)
	}
}
