package engine

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
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

// LAYOUT 0's HOLDERS ARE THE LIVE DATA NODES: the estate is one partition,
// which every data node holds whole, so its counted set's holder half is the
// presence the trim has always counted — the answer the layout's routing reads
// too, and no estate map is asked.
func TestLayoutZerosHoldersAreTheLiveDataNodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend := coordmem.New()
	claimPresences(t, backend, map[string][]string{
		"data-a": {"data", "seats"}, "agent-1": {"seats"},
	})
	e := &Engine{backends: &Backends{Coord: backend}}
	holders := e.holdersOf(LayoutZero())
	if _, byPresence := holders.(presenceHolders); !byPresence {
		t.Fatalf("layout 0's holders are answered by %T, want the presence roster", holders)
	}
	held, err := holders.Holders(ctx, LayoutZero().Partitions())
	if err != nil {
		t.Fatalf("Holders: %v", err)
	}
	if got := held[statelog.EstatePartition]; len(got) != 1 || got[0].NodeID != "data-a" {
		t.Errorf("estate.000 is held by %v, want the one live data node", got)
	}
}

// A PARTITIONED LAYOUT COUNTS THE MAP'S HOLDERS, NOT THE DATA NODES.
//
// Once partitions are placed a log's readers and writers are its partition's
// holders (§F2): the map's holders of it in EVERY state — a joiner whose tail
// must be kept, a leaver still deciding writes — whether or not their leases
// are live. A data node holding nothing has no position on the log, so counting
// it would put it at zero there and block the log's trim for ever; counting a
// holder of another partition would let one node offline there pin this one.
// And a view too stale to decide from is UNKNOWN, so the trim blocks on the
// applied term saying so rather than counting whom a stale map named.
func TestAPartitionedLayoutCountsTheMapsHoldersNotTheDataNodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	layout := partitionedTestLayout()
	tracker0 := statelog.PartitionID{Space: statelog.SpaceTracker}
	tracker1 := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}
	pages0 := statelog.PartitionID{Space: statelog.SpacePages}
	fleet := placedMap(t, layout, map[statelog.PartitionID][]partmap.Holder{
		tracker0: {{Node: "node-a", State: partmap.Serving, Since: 1},
			{Node: "node-j", State: partmap.Joining, Since: 2}},
		tracker1: {{Node: "node-b", State: partmap.Serving, Since: 1},
			{Node: "node-l", State: partmap.Leaving, Since: 2}},
		pages0: {{Node: "node-a", State: partmap.Serving, Since: 1}},
	})
	backend := coordmem.New()
	claimLeases(t, backend, layout.Number, "node-a", "node-b")
	// THE DATA NODES ARE NOT THE HOLDERS: node-idle holds nothing, and the
	// joiner and the leaver have let their presence lapse.
	claimPresences(t, backend, map[string][]string{
		"node-a": {"data"}, "node-b": {"data"}, "node-idle": {"data"},
	})
	clock := &viewClock{now: time.Now()}
	e := &Engine{backends: &Backends{Coord: backend}}
	e.estateWatch.Store(runningWatch(t, fleet, backend, nil, layout, clock))

	holders := e.holdersOf(layout)
	held, err := holders.Holders(ctx, layout.Partitions())
	if err != nil {
		t.Fatalf("Holders: %v", err)
	}
	want := map[statelog.PartitionID][]string{
		tracker0: {"node-a", "node-j"}, tracker1: {"node-b", "node-l"}, pages0: {"node-a"},
	}
	for p, nodes := range want {
		var got []string
		for _, h := range held[p] {
			got = append(got, h.NodeID)
		}
		if !slices.Equal(got, nodes) {
			t.Errorf("%s is held by %v, want the map's holders in every state %v", p, got, nodes)
		}
	}

	// AND THE TRIM COUNTS THEM: the joiner at zero on its own partition's
	// log, the idle data node on none.
	r := &retention{fleet: coordmem.NewFleet(), state: &stateLog{layout: layout}, holders: holders}
	shared, err := r.read(ctx)
	if err != nil {
		t.Fatalf("read the tick's inputs: %v", err)
	}
	running := &runningLog{id: statelog.LogID{Domain: "tracker", Partition: tracker0},
		key: "tracker@tracker.000"}
	var counted []string
	for _, n := range shared.counted(running, nil) {
		counted = append(counted, n.NodeID)
	}
	if !slices.Equal(counted, []string{"node-a", "node-j"}) {
		t.Errorf("tracker@tracker.000 counts %v, want its partition's holders", counted)
	}

	// A STALE VIEW IS UNKNOWN, and the tick says so rather than counting.
	clock.advance(statelog.FloorCacheStale + time.Second)
	if _, err := holders.Holders(ctx, layout.Partitions()); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a stale view answered %v, want unknown", err)
	}
	shared, err = r.read(ctx)
	if err != nil {
		t.Fatalf("a stale view failed the whole tick: %v", err)
	}
	if !strings.Contains(shared.holdersUnknown, "freshness") {
		t.Errorf("the tick's inputs say %q about who holds the partitions, want the "+
			"stale view named", shared.holdersUnknown)
	}
	in := statelog.TrimInputs{Now: time.Now(), CountedReadable: shared.holdersUnknown == "",
		CountedUnknown: shared.holdersUnknown, HoldsReadable: true,
		Counted: shared.counted(running, nil)}
	d := statelog.Trim(in.Terms())
	if d.BlockedBy != statelog.TermApplied || !strings.Contains(d.Detail, "freshness") {
		t.Errorf("the trim over a stale view is blocked by %q (%s), want the applied "+
			"term naming the stale view", d.BlockedBy, d.Detail)
	}
}

// placedMap stores an estate map of layout whose holder tables are holders,
// every holder a member, at an epoch every holder's Since fits under.
func placedMap(t *testing.T, layout statelog.Layout,
	holders map[statelog.PartitionID][]partmap.Holder) *coordmem.Fleet {

	t.Helper()
	number, healthy := layout.Number, true
	members := map[string]bool{}
	for _, hs := range holders {
		for _, h := range hs {
			members[h.Node] = true
		}
	}
	var live []partmap.Presence
	for _, node := range slices.Sorted(maps.Keys(members)) {
		live = append(live, partmap.Presence{Node: node, Meta: partmap.Meta{Weight: 1,
			Layout: &number, Healthy: &healthy, Partitions: map[string]partmap.PartitionState{}}})
	}
	state, changed := partmap.Next(partmap.MapState{}, partmap.Input{
		Layout: layout, Live: live, Now: time.Now(),
		Company: membership.Company{Epoch: 1, Replicas: 2, Block: "estate"},
	})
	if !changed {
		t.Fatal("the fixture wrote no first map")
	}
	state.Map.Epoch = 2
	for i, table := range state.Map.Partitions {
		p, err := statelog.ParsePartitionID(table.ID)
		if err != nil {
			t.Fatal(err)
		}
		state.Map.Partitions[i].Holders = slices.Clone(holders[p])
	}
	raw, err := state.Encode()
	if err != nil {
		t.Fatalf("encode the fixture map: %v", err)
	}
	fleet := coordmem.NewFleet()
	if _, won, err := fleet.CreateEstateMap(t.Context(), raw); err != nil || !won {
		t.Fatalf("create the map: %v", err)
	}
	return fleet
}

// claimPresences claims a presence lease for every node, with its roles.
func claimPresences(t *testing.T, backend coord.Backend, roles map[string][]string) {
	t.Helper()
	for id, r := range roles {
		if _, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":1", TTL: time.Hour, Meta: map[string]any{"roles": r},
		}); err != nil {
			t.Fatal(err)
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
