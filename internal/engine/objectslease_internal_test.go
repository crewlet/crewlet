package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
	"github.com/crewlet/crewlet/internal/statelog"
)

// held reads a lease, failing the case when the store does not answer.
func held(t *testing.T, b coord.Backend, resource string) *coord.Lease {
	t.Helper()
	l, err := b.Get(context.Background(), resource)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// THE MEMBERSHIP CARRIES THE STORE'S OWN ACCOUNT, and is given back when the
// store stops — so the map sees the node gone at once rather than a TTL later.
func TestTheObjectsLeaseCarriesTheStoresAccountAndIsGivenBack(t *testing.T) {
	t.Parallel()
	b := coordmemory.New()
	l := startObjectsLease(t.Context(), b, "n1", "n1:inc", 30*time.Second,
		func() objstore.ObjectsMeta {
			return objstore.ObjectsMeta{
				Weight: 3, Labels: map[string]string{"zone": "z2"},
				Health: &objstore.ObjectsHealth{State: string(disk.HealthNearFull), UsedPercent: 88},
			}
		})
	resource := coord.ObjectsResource("n1")
	eventually(t, "the objects lease", func() bool { return held(t, b, resource) != nil })

	lease := held(t, b, resource)
	if lease.Owner != "n1:inc" || lease.Preferred != "n1" {
		t.Errorf("held by %q preferring %q", lease.Owner, lease.Preferred)
	}
	meta, ok := objstore.ObjectsFromMeta(lease.Meta)
	if !ok || meta.Weight != 3 || meta.Labels["zone"] != "z2" || meta.Health == nil ||
		meta.Health.State != string(disk.HealthNearFull) {
		t.Errorf("the lease says %+v", meta)
	}
	// NOT PRESENCE: the node's presence is the seat host's, and nothing
	// here may write it.
	if held(t, b, coord.NodeResource("n1")) != nil {
		t.Error("the object store claimed the node's presence")
	}

	l.stop(t.Context())
	if held(t, b, resource) != nil {
		t.Error("the lease outlived the store that claimed it")
	}
}

// A MEMBERSHIP ANOTHER PROCESS HOLDS IS LEFT ALONE: this node claims nothing
// under it and gives back nothing it never held — a previous incarnation's
// lease lapses on its own, and taking or releasing it would be one process
// speaking for another.
func TestAnObjectsLeaseHeldElsewhereIsLeftAlone(t *testing.T) {
	t.Parallel()
	b := coordmemory.New()
	resource := coord.ObjectsResource("n1")
	if _, _, err := b.TryAcquire(t.Context(), resource, coord.AcquireOptions{
		Owner: "n1:old", TTL: time.Minute, Ungated: true,
		Meta: objstore.ObjectsMeta{Weight: 1}.Encode(),
	}); err != nil {
		t.Fatal(err)
	}
	var beats sync.WaitGroup
	beats.Add(1)
	var once sync.Once
	l := startObjectsLease(t.Context(), b, "n1", "n1:new", 30*time.Second,
		func() objstore.ObjectsMeta {
			once.Do(beats.Done)
			return objstore.ObjectsMeta{Weight: 2}
		})
	beats.Wait()
	l.stop(t.Context())
	lease := held(t, b, resource)
	if lease == nil || lease.Owner != "n1:old" {
		t.Fatalf("the other process's lease is now %+v", lease)
	}
}

// recordingLeases is a lease store that notes when a lease is given back.
type recordingLeases struct {
	coord.Backend
	mu    *sync.Mutex
	order *[]string
}

func (r recordingLeases) Release(ctx context.Context, resource, owner string, epoch int64) (bool, error) {
	r.mu.Lock()
	*r.order = append(*r.order, "membership")
	r.mu.Unlock()
	return r.Backend.Release(ctx, resource, owner, epoch)
}

// THE SERVER IS WITHDRAWN BEFORE THE MEMBERSHIP IS GIVEN BACK. A member is what
// writers send copies to: one whose lease outlived its server would be sent
// copies it can no longer take, and the other order — the membership first —
// counts a node absent while it still serves, which is the drain-long
// reshuffle the lease exists to prevent.
func TestTheMembershipIsGivenBackOnlyAfterTheServerIsWithdrawn(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var order []string
	b := recordingLeases{Backend: coordmemory.New(), mu: &mu, order: &order}
	o := &objectStore{
		stopServe: func(context.Context) error {
			mu.Lock()
			order = append(order, "server")
			mu.Unlock()
			return nil
		},
		lease: startObjectsLease(t.Context(), b, "n1", "n1:inc", 30*time.Second,
			func() objstore.ObjectsMeta { return objstore.ObjectsMeta{Weight: 1} }),
	}
	eventually(t, "the objects lease", func() bool {
		return held(t, b, coord.ObjectsResource("n1")) != nil
	})
	o.release(t.Context())
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "server" || order[1] != "membership" {
		t.Fatalf("stopped in the order %v, want the server then the membership", order)
	}
}

// THE LEASE IS RENEWED ON THE SEAT HEARTBEAT'S RATIO OF THE TTL IN FORCE, so it
// takes as many missed beats to lapse as a seat does whatever TTL the fleet
// adopted — never on the shipped interval against a shorter adopted TTL.
func TestTheObjectsLeaseIsRenewedOnTheTTLInForce(t *testing.T) {
	t.Parallel()
	for _, ttl := range []time.Duration{45 * time.Second, 12 * time.Second} {
		if got := objectsLeaseInterval(ttl); got*3 != ttl {
			t.Errorf("a %v lease renews every %v, want a third of it", ttl, got)
		}
	}
}

// THE READING SAYS WHAT THE PASSES FOUND ABOUT THE CURRENT PLACEMENT, and only
// that: pending copies from a pass that completed at the map's epoch, never an
// older epoch's pending count, which is a claim about a placement the map no
// longer makes. And a missing chunk is reported whatever epoch found it,
// because a chunk nobody holds is lost under every map.
//
// A PASS THAT STOPPED SHORT IS NOT EVIDENCE OF ABSENCE. Every repair pins the
// estate before it looks at a group, so a log at its ceiling or without a
// quorum fails every pass with zeroes for everything it never reached: that
// zero must neither clear a known loss nor stand in for a repair that has
// completed. How long this node has gone unrepaired runs from the later of
// the epoch being seen and the last pass that completed at it, so a loop that
// stops completing at an unchanged epoch is degraded after two intervals —
// and one merely between its scheduled passes is not.
func TestTheObjectsReadingDescribesTheCurrentEpoch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	seenAt := now.Add(-time.Hour)
	interval := upkeep.RepairInterval
	completed := func(epoch uint64, pending, missing int, ago time.Duration) upkeep.RepairStatus {
		return upkeep.RepairStatus{Epoch: epoch, Completed: true, Pending: pending,
			Unreachable: pending, Missing: missing, At: now.Add(-ago)}
	}
	// stoppedShort is a pass that failed at the pin a moment ago, having
	// looked at nothing: every count zero.
	stoppedShort := func(epoch uint64) upkeep.RepairStatus {
		return upkeep.RepairStatus{Epoch: epoch, At: now.Add(-30 * time.Second),
			Error: "the tracker log did not answer the barrier"}
	}
	for name, tc := range map[string]struct {
		status        upkeep.Status
		since         time.Time
		pending       int
		missing       int
		unrepairedFor time.Duration
		fires         statelog.Kind
	}{
		"a clean pass at this epoch": {
			status: upkeep.Status{Repair: completed(7, 0, 0, time.Minute),
				Repaired: completed(7, 0, 0, time.Minute)},
			since: seenAt, unrepairedFor: time.Minute,
		},
		"a pass at this epoch left copies behind": {
			status: upkeep.Status{Repair: completed(7, 4, 0, time.Minute),
				Repaired: completed(7, 4, 0, time.Minute)},
			since: seenAt, pending: 4, unrepairedFor: time.Minute,
			fires: statelog.KindObjectsDegraded,
		},
		"only an older epoch's pass completed": {
			status:        upkeep.Status{Repaired: completed(6, 9, 0, time.Minute)},
			since:         seenAt,
			unrepairedFor: time.Hour, fires: statelog.KindObjectsDegraded,
		},
		"a map that moved moments ago": {
			status:        upkeep.Status{Repaired: completed(6, 9, 0, 3*time.Minute)},
			since:         now.Add(-2 * time.Minute),
			unrepairedFor: 2 * time.Minute,
		},
		"no map seen yet": {status: upkeep.Status{Repaired: completed(6, 9, 0, time.Minute)}},
		"a chunk nobody holds": {
			status: upkeep.Status{Repair: completed(7, 0, 2, time.Minute),
				Repaired: completed(7, 0, 2, time.Minute)},
			since: seenAt, missing: 2, unrepairedFor: time.Minute,
			fires: statelog.KindObjectsMissing,
		},
		"a chunk nobody holds, found before the map moved": {
			status: upkeep.Status{Repair: completed(6, 0, 2, 3*time.Minute),
				Repaired: completed(6, 0, 2, 3*time.Minute)},
			since: now.Add(-2 * time.Minute), missing: 2, unrepairedFor: 2 * time.Minute,
			fires: statelog.KindObjectsMissing,
		},
		// The last pass's zero is not evidence: it never reached a group.
		"a pass that stopped short after one that found a chunk nobody holds": {
			status: upkeep.Status{Repair: stoppedShort(7),
				Repaired: completed(7, 0, 2, 5*time.Minute)},
			since: seenAt, missing: 2, unrepairedFor: 5 * time.Minute,
			fires: statelog.KindObjectsMissing,
		},
		// ... but what it did count is real, if only a floor.
		"a pass that stopped short having counted more": {
			status: upkeep.Status{
				Repair: upkeep.RepairStatus{Epoch: 7, Missing: 3,
					At: now.Add(-30 * time.Second), Error: "a member stopped answering"},
				Repaired: completed(7, 0, 1, 5*time.Minute)},
			since: seenAt, missing: 3, unrepairedFor: 5 * time.Minute,
			fires: statelog.KindObjectsMissing,
		},
		// The next pass is due an interval after the last one ended and
		// finishes after its own duration: past one interval is on time.
		"between scheduled passes": {
			status: upkeep.Status{Repair: completed(7, 0, 0, interval+2*time.Minute),
				Repaired: completed(7, 0, 0, interval+2*time.Minute)},
			since: seenAt, unrepairedFor: interval + 2*time.Minute,
		},
		// The map has not moved, and every pass since the last clean one
		// failed at the pin: the loop has stalled, and saying so must not
		// wait for the map to move.
		"passes that stopped short for two intervals at an unchanged epoch": {
			status: upkeep.Status{Repair: stoppedShort(7),
				Repaired: completed(7, 0, 0, 2*interval+5*time.Minute)},
			since: seenAt, unrepairedFor: 2*interval + 5*time.Minute,
			fires: statelog.KindObjectsDegraded,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var r statelog.Reading
			fillPassesReading(tc.status, 7, tc.since, now, &r)
			if r.ObjectsPending != tc.pending || r.ObjectsMissing != tc.missing ||
				r.ObjectsUnrepairedFor != tc.unrepairedFor {
				t.Errorf("pending %d missing %d unrepaired %v, want %d, %d and %v",
					r.ObjectsPending, r.ObjectsMissing, r.ObjectsUnrepairedFor,
					tc.pending, tc.missing, tc.unrepairedFor)
			}
			alarms := statelog.Evaluate(r)
			switch {
			case tc.fires == "" && len(alarms) != 0:
				t.Errorf("raised %v", alarms)
			case tc.fires != "" && (len(alarms) != 1 || alarms[0].Kind != tc.fires):
				t.Errorf("raised %v, want %s", alarms, tc.fires)
			}
		})
	}
}

// THE STORE'S OWN VERDICT REACHES THE ALARMS: failed and full are
// objects_store_unhealthy, nearly full objects_store_nearfull, ok nothing.
func TestTheStoresHealthReachesItsAlarms(t *testing.T) {
	t.Parallel()
	for state, want := range map[disk.HealthState]statelog.Kind{
		disk.HealthFailed:   statelog.KindObjectsUnhealthy,
		disk.HealthFull:     statelog.KindObjectsUnhealthy,
		disk.HealthNearFull: statelog.KindObjectsNearFull,
		disk.HealthOK:       "",
	} {
		var r statelog.Reading
		fillHealthReading(disk.Health{State: state, Detail: "why", UsedPercent: 90}, &r)
		alarms := statelog.Evaluate(r)
		switch {
		case want == "" && len(alarms) != 0:
			t.Errorf("%s raised %v", state, alarms)
		case want != "" && (len(alarms) != 1 || alarms[0].Kind != want):
			t.Errorf("%s raised %v, want %s", state, alarms, want)
		}
	}
}

// A DATA NODE IS A MEMBER OF THE OBJECT STORE BY ITS OWN LEASE, with the share
// its disk offers and its store's health, and the map it joins takes its
// replica count from the COMPANY — until it stops, when the membership is
// given back after the chunk server, not at the drain's first step.
//
// THE HEALTH IS THE STORE'S OWN ACCOUNT, WHATEVER IT IS. The store measures
// the volume its directory is on — this host's real disk — so on a nearly
// full host it is honestly `nearfull` or `full`, and asserting `ok` failed
// this case for a reason that has nothing to do with membership. What the
// lease must do is carry exactly what the store reported: its state, its
// detail and its measurement, so a peer deciding where to place reads the
// store rather than a guess about it.
func TestADataNodeJoinsTheObjectStoreByItsOwnLease(t *testing.T) {
	t.Parallel()
	doc := companyWithoutSandboxDoc + "objects:\n  replicas: 2\n"
	e := newSandboxNode(t, parseCompany(t, doc))
	resource := coord.ObjectsResource(e.id)
	eventually(t, "this node's objects lease", func() bool {
		return held(t, e.backends.Coord, resource) != nil
	})
	meta, ok := objstore.ObjectsFromMeta(held(t, e.backends.Coord, resource).Meta)
	if !ok || meta.Weight != 1 || meta.Health == nil {
		t.Fatalf("the lease says %+v (health %+v)", meta, meta.Health)
	}
	// EVENTUALLY, because the two are read at two instants: every beat
	// probes the volume again before it writes the lease, so a probe landing
	// between the read of the lease and the read of the store pairs one beat's
	// measurement with the next. Beats are seconds apart and the two reads
	// are not, so the first pair read inside one beat agrees — unless the
	// lease does not carry what the store said.
	var leased *objstore.ObjectsHealth
	var reported disk.Health
	deadline := time.Now().Add(20 * time.Second)
	for {
		meta, ok := objstore.ObjectsFromMeta(held(t, e.backends.Coord, resource).Meta)
		reported = e.objects.disk.Health()
		if ok && meta.Health != nil {
			leased = meta.Health
			if leased.State == string(reported.State) && leased.Detail == reported.Detail &&
				leased.UsedPercent == reported.UsedPercent {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lease carries health %+v, and the store reports %+v", leased, reported)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !reported.State.Valid() {
		t.Errorf("the store reported a state this build does not know: %q", reported.State)
	}
	if e.ObjectsControl() == nil {
		t.Error("a node running the object store offers no gestures on its map")
	}
	eventually(t, "the first placement map", func() bool {
		m, placed := e.objects.cache.Current()
		return placed && m.Replicas == 2 && m.Holds(e.id)
	})

	e.Stop(context.Background())
	if held(t, e.backends.Coord, resource) != nil {
		t.Error("the objects lease outlived the engine that claimed it")
	}
}
