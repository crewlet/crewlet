package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeEstate is an estate runtime whose copy is as the case says.
type fakeEstate struct {
	mu          sync.Mutex
	healthy     bool
	established bool
	refusal     statelog.ReadRefusal
}

func (f *fakeEstate) Healthy(context.Context) (bool, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthy, ""
}

func (f *fakeEstate) Serving(context.Context) (bool, statelog.ReadRefusal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.established, f.refusal
}

func (f *fakeEstate) set(healthy, established bool, refusal statelog.ReadRefusal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthy, f.established, f.refusal = healthy, established, refusal
}

// THE LEASE SAYS WHAT THE COPY IS DOING, WRONG BEFORE BEHIND. A copy that is
// wrong — an applier halted, an eviction, rows below the log — is faulted
// whatever else is true of it; one that is established serves; one that is
// merely not established yet is catching up. A beat that could not ask the
// broker at all knows none of it, and says what the last beat that could
// said — catching up, before any has.
func TestTheEstateLeaseSaysWhatTheCopyIsDoing(t *testing.T) {
	t.Parallel()
	rt := &fakeEstate{}
	a := &estateLeaseAccount{runtime: rt}
	for _, step := range []struct {
		name                 string
		healthy, established bool
		refusal              statelog.ReadRefusal
		want                 partmap.PartitionState
	}{
		{"unmeasurable before anything was said", true, false, statelog.RefuseBrokerUnreachable, partmap.PartCatchingUp},
		{"established", true, true, "", partmap.PartServing},
		{"unmeasurable after serving", true, false, statelog.RefuseBrokerUnreachable, partmap.PartServing},
		{"behind", true, false, statelog.RefuseBehind, partmap.PartCatchingUp},
		{"wrong", false, true, "", partmap.PartFaulted},
		{"wrong and behind", false, false, statelog.RefuseBehind, partmap.PartFaulted},
		{"unmeasurable after a fault", true, false, statelog.RefuseBrokerUnreachable, partmap.PartFaulted},
	} {
		rt.set(step.healthy, step.established, step.refusal)
		if got := a.state(t.Context()); got != step.want {
			t.Fatalf("%s: the lease says %q, want %q", step.name, got, step.want)
		}
	}
}

// A COPY WITH A RECORD IN FLIGHT STILL SERVES. A busy company has a record on
// its log that the applier has not reached yet on most beats, and a lease that
// sampled that instant — seat admission's strict test — said `catching_up` on
// an established, drained copy each time, dropping it from its partition's
// servers for as long as the company kept writing. Serving is the contract's
// definition instead: drained, and within the snapshot slack of the log.
func TestACopyWithARecordInFlightStillServes(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	n := e.native.Load()
	waitUntil(t, 20*time.Second, "the copy to serve", func() bool {
		ok, _ := n.log.Serving(t.Context())
		return ok
	})
	// A RECORD THE APPLIER NEVER REACHES: the appliers stop, and a write
	// lands on the log and waits in vain for its own application.
	n.log.haltAppliers()
	write, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	_, _ = n.writer.EvictNode(write, "op-in-flight", "node-x")
	cancel()
	if ok, refusal := n.log.Established(t.Context(), true); ok || refusal != statelog.RefuseBehind {
		t.Fatalf("the premise: seat admission reads (%v, %q), want the copy behind "+
			"by the record in flight", ok, refusal)
	}
	if ok, refusal := n.log.Serving(t.Context()); !ok {
		t.Fatalf("an established, drained copy one record behind does not serve: %q", refusal)
	}
	a := &estateLeaseAccount{runtime: n.log}
	if got := a.state(t.Context()); got != partmap.PartServing {
		t.Errorf("the lease says %q of a copy one record behind, want serving", got)
	}
}

// UNDER LAYOUT 0 THE LEASE SAYS WHAT EVERY DATA NODE HOLDS: layout 0's one
// partition, whole, in the state the copy is in — with the node's own share
// and labels, the free space of the estate's volume, its health, and whether
// its lexical index is still being built. A volume that cannot be measured is
// a store that is not healthy, with the reason; a node with no runtime writes
// no lease at all, because it could not say what it holds.
func TestTheEstateLeaseSaysLayoutZerosOnePartition(t *testing.T) {
	t.Parallel()
	rt := &fakeEstate{healthy: true, established: true}
	building := true
	a := &estateLeaseAccount{
		weight: 3, labels: map[string]string{"zone": "z1"}, layout: LayoutZero(),
		runtime: rt, volume: "/var/lib/crewlet",
		building: func() bool { return building },
		free:     func(string) (int64, error) { return 5 << 30, nil },
	}
	raw, err := a.meta(t.Context())
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	m, ok := partmap.MetaFromLease(raw)
	if !ok {
		t.Fatalf("the lease offers no share: %+v", raw)
	}
	switch {
	case m.Weight != 3 || m.Labels["zone"] != "z1":
		t.Errorf("the lease offers weight %d with labels %v", m.Weight, m.Labels)
	case m.Layout == nil || *m.Layout != 0:
		t.Errorf("the lease runs layout %v, want 0", m.Layout)
	case len(m.Partitions) != 1 || m.Partitions["estate.000"] != partmap.PartServing:
		t.Errorf("the lease holds %v, want estate.000 serving", m.Partitions)
	case m.Healthy == nil || !*m.Healthy || m.FreeBytes != 5<<30:
		t.Errorf("the lease says healthy %v with %d bytes free", m.Healthy, m.FreeBytes)
	case len(m.Building) != 1 || m.Building[0] != "estate.000":
		t.Errorf("the lease says %v is building, want estate.000", m.Building)
	case m.MapEpoch != 0:
		t.Errorf("the lease acted on map epoch %d, and there is no map", m.MapEpoch)
	}

	building = false
	a.free = func(string) (int64, error) { return 0, errors.New("input/output error") }
	raw, err = a.meta(t.Context())
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	m, _ = partmap.MetaFromLease(raw)
	if m.Healthy == nil || *m.Healthy || m.Detail == "" {
		t.Errorf("a volume that cannot be measured says healthy %v (%q)", m.Healthy, m.Detail)
	}
	if len(m.Building) != 0 {
		t.Errorf("a built index still says %v is building", m.Building)
	}

	if _, err := (&estateLeaseAccount{layout: LayoutZero()}).meta(t.Context()); err == nil {
		t.Error("a node with no estate runtime wrote a lease")
	}
}

// THE MEMBERSHIP IS GIVEN BACK AFTER THE RUNTIME HAS STOPPED: after the search
// slices are withdrawn, the runtime's own loops have ended and its appliers
// have stopped. A member is what
// the estate map places partitions on and a capacity window waits for, so one
// given back while its runtime still ran would read as gone while it served —
// the drain-long reshuffle the lease exists to prevent.
func TestTheEstateLeaseIsGivenBackAfterTheRuntimeStops(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var order []string
	record := func(what string) {
		mu.Lock()
		order = append(order, what)
		mu.Unlock()
	}
	b := recordingLeases{Backend: coordmemory.New(), mu: &mu, order: &order}
	run, stop := context.WithCancel(context.Background())
	// THE STATE LOG'S OWN STOP, whose last step is ending the appliers: a
	// log with no loops running and one apply context, whose cancel is the
	// appliers stopping.
	appliers := &stateLog{stop: func() {}}
	appliers.applyStop = func() { record("appliers") }
	n := &native{run: run, stop: stop, log: appliers,
		stopSlices: func(context.Context) error { record("slices"); return nil },
		lease: startMemberLease(t.Context(), b, memberLeaseSpec{
			resource: coord.EstateResource("n1"), node: "n1", owner: "n1:inc",
			ttl: 30 * time.Second, what: "estate membership", event: "estate",
			meta: func(context.Context) (map[string]any, error) {
				return map[string]any{"weight": 1}, nil
			},
		}),
	}
	n.done.Add(1)
	go func() {
		defer n.done.Done()
		<-run.Done()
		record("runtime")
	}()
	eventually(t, "the estate lease", func() bool {
		return held(t, b, coord.EstateResource("n1")) != nil
	})
	n.shutdown(t.Context())
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"slices", "runtime", "appliers", "membership"}; !slices.Equal(order, want) {
		t.Fatalf("stopped in the order %v, want %v: the membership last, once nothing "+
			"of the runtime serves or applies", order, want)
	}
}

// A DATA NODE RUNNING THE ESTATE HOLDS ITS ESTATE LEASE, under its own
// incarnation, saying layout 0's one partition — serving once its copy is
// established — until it stops, when it is given back. A node with no company
// runs no estate and claims none.
func TestADataNodeHoldsItsEstateLeaseWhileItsRuntimeRuns(t *testing.T) {
	t.Parallel()
	e := newSandboxNode(t, parseCompany(t, companyWithoutSandboxDoc))
	resource := coord.EstateResource(e.id)
	eventually(t, "this node's estate lease, serving", func() bool {
		lease := held(t, e.backends.Coord, resource)
		if lease == nil {
			return false
		}
		m, ok := partmap.MetaFromLease(lease.Meta)
		return ok && m.Partitions["estate.000"] == partmap.PartServing
	})
	lease := held(t, e.backends.Coord, resource)
	if lease.Owner != e.incarnation {
		t.Errorf("the estate lease is held by %q, not this process's %q", lease.Owner, e.incarnation)
	}
	m, _ := partmap.MetaFromLease(lease.Meta)
	if m.Layout == nil || *m.Layout != 0 || m.Weight != 1 || m.Healthy == nil || !*m.Healthy {
		t.Errorf("the lease says layout %v, weight %d, healthy %v", m.Layout, m.Weight, m.Healthy)
	}
	e.Stop(context.Background())
	if held(t, e.backends.Coord, resource) != nil {
		t.Error("the estate lease outlived the engine that claimed it")
	}

	// THE ONLY PLACE THE LEASE IS STARTED is the runtime's start, so a node
	// that has none holds none — its first beat would have landed at once.
	bare := newSandboxNode(t, nil)
	if bare.native.Load() != nil {
		t.Fatal("the premise: a node with no company runs no estate")
	}
	time.Sleep(200 * time.Millisecond)
	if live, err := bare.backends.Coord.ListLive(t.Context(), coord.ClassEstate); err != nil || len(live) != 0 {
		t.Errorf("a node with no company holds estate leases %+v (%v)", live, err)
	}
}
