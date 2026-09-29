package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A LOG COUNTS ITS OWN PARTITION'S HOLDERS, AND A HOLDER THAT HAS NOT REPORTED
// COUNTS AT ZERO.
//
// The trim may not remove a record a node that applies the log has not
// applied, and the nodes that apply a log are its partition's holders. So a
// holder still joining — no position yet — counts at zero on that partition's
// logs and blocks them, and on no other partition's: counted there, a node
// offline on one partition would pin every log in the company.
func TestALogCountsItsOwnPartitionsHolders(t *testing.T) {
	t.Parallel()
	e, s, _ := aPartitionedStateLog(t)
	tracker0 := statelog.PartitionID{Space: statelog.SpaceTracker}
	r := &retention{fleet: e.backends.Fleet, state: s, nodeID: "node-p",
		holders: fixedHolders{
			tracker0: {{NodeID: "node-p"}, {NodeID: "node-joining"}},
		}}
	for _, p := range partitionedTestLayout().Partitions() {
		if p != tracker0 {
			r.holders.(fixedHolders)[p] = []statelog.Presence{{NodeID: "node-p"}}
		}
	}
	shared, err := r.read(t.Context())
	if err != nil {
		t.Fatalf("read the tick's inputs: %v", err)
	}
	for _, running := range s.running() {
		counted := shared.counted(running, nil)
		var ids []string
		for _, n := range counted {
			ids = append(ids, n.NodeID)
			if n.NodeID == "node-joining" && (n.Seq != 0 || n.Generation != 0) {
				t.Errorf("%s counts the joining holder at %d@%d, want zero — it has "+
					"reported nothing, and the tail it is about to replay is what the "+
					"trim must keep", running.key, n.Seq, n.Generation)
			}
		}
		want := []string{"node-p"}
		if running.id.Partition == tracker0 {
			want = []string{"node-joining", "node-p"}
		}
		if !slices.Equal(ids, want) {
			t.Errorf("%s counts %v, want its partition's holders %v", running.key, ids, want)
		}
	}
}

// THIS BUILD'S HOLDERS ARE THE LIVE DATA NODES, for every partition asked about:
// every data node holds every partition of the layout it runs, so asking per
// partition counts exactly the nodes the trim always counted.
func TestEveryLiveDataNodeHoldsEveryPartition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := coordmem.New()
	for id, meta := range map[string]map[string]any{
		"data-a":  {"roles": []string{"data", "seats"}},
		"agent-1": {"roles": []string{"seats"}},
	} {
		if _, _, err := backend.TryAcquire(ctx, coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":1", TTL: time.Minute, Meta: meta,
		}); err != nil {
			t.Fatal(err)
		}
	}
	partitions := partitionedTestLayout().Partitions()
	held, err := presenceHolders{leases: backend}.Holders(ctx, partitions)
	if err != nil {
		t.Fatalf("Holders: %v", err)
	}
	if len(held) != len(partitions) {
		t.Fatalf("answered %d partition(s) of the %d asked about", len(held), len(partitions))
	}
	for _, p := range partitions {
		if got := held[p]; len(got) != 1 || got[0].NodeID != "data-a" {
			t.Errorf("%s is held by %v, want the one live data node", p, got)
		}
	}
}

// fixedHolders answers who holds each partition from a table a case wrote.
type fixedHolders map[statelog.PartitionID][]statelog.Presence

func (f fixedHolders) Holders(_ context.Context,
	partitions []statelog.PartitionID) (map[statelog.PartitionID][]statelog.Presence, error) {

	out := make(map[statelog.PartitionID][]statelog.Presence, len(partitions))
	for _, p := range partitions {
		out[p] = f[p]
	}
	return out, nil
}
