package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
)

// UNDER LAYOUT 0 THE ESTATE MAP'S DUTY WRITES NOTHING BUT ITS OWN LEASE. The
// node is a data node running the estate — its estate lease live, holding the
// one partition, its company activated with an estate block — which is every
// input a first map needs but a partitioned layout; and the duty, run by this node's own
// maintainer over this node's own stores, reads all of them, finds no map, wants
// none, paces at its interval, and leaves the store without one.
func TestTheEstateMapDutyWritesNothingUnderLayoutZero(t *testing.T) {
	t.Parallel()
	doc := companyWithoutSandboxDoc + "estate:\n  replicas: 2\n  failure_domain: zone\n"
	e := newSandboxNode(t, parseCompany(t, doc))
	eventually(t, "this node's estate lease and the estate map's duty", func() bool {
		lease := held(t, e.backends.Coord, coord.EstateResource(e.id))
		if lease == nil {
			return false
		}
		m, ok := partmap.MetaFromLease(lease.Meta)
		duty := held(t, e.backends.Coord, coord.WorkerResource(estateMapDuty))
		return ok && m.Partitions["estate.000"] != "" && duty != nil && duty.Owner == e.incarnation
	})
	company, ok := e.estateCompany()
	if !ok || company.Replicas != 2 || company.FailureDomain != "zone" || company.Block != "estate" {
		t.Fatalf("the duty reads the company as %+v (%v)", company, ok)
	}
	live, err := estateHolders(t.Context(), e.backends.Coord)
	if err != nil || len(live) != 1 || live[0].Node != e.id {
		t.Fatalf("the duty reads the live estate leases as %+v (%v)", live, err)
	}

	m, err := e.newEstateMaintainer()
	if err != nil {
		t.Fatal(err)
	}
	duty := mapDuty{event: "estate_map", tick: estateMapTick(m)}
	for range membership.OutTicks {
		if wait := duty.turn(t.Context()); wait != mapInterval {
			t.Fatalf("a layout-0 tick waits %v, want the interval %v: there is no map "+
				"to poll for", wait, mapInterval)
		}
	}
	if _, found, err := e.backends.Fleet.EstateMap(t.Context()); err != nil || found {
		t.Fatalf("the store holds an estate map after layout-0 ticks (found %v, %v)", found, err)
	}
}

// THE ESTATE MAP TAKES NOTHING FROM A COMPANY NO ACTIVATION NAMED, and from one
// that was activated it takes the copies — the default when it names none — and
// the failure domain of its OWN block, never the object store's, stamped with
// the activation's instant so a duty holder a revision behind cannot set the
// map back.
func TestTheEstateMapTakesTheCompanyStampedWithItsActivation(t *testing.T) {
	t.Parallel()
	cfg := parseCompany(t, companyWithoutSandboxDoc)
	e := &Engine{}
	e.epoch.current.Store(&Company{Config: cfg})
	if _, ok := e.estateCompany(); ok {
		t.Fatal("a company no activation named was applied to the map")
	}

	activated := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	e.epoch.current.Store(&Company{Config: cfg, ActivatedAt: activated})
	got, ok := e.estateCompany()
	if !ok {
		t.Fatal("an activated company was not applied")
	}
	want := membership.Company{
		Epoch:    uint64(configplane.ActivationStamp(activated)),
		Replicas: config.DefaultEstateReplicas,
		// Named, so a refusal of it names the field to change.
		Block: "estate",
	}
	if got != want {
		t.Errorf("company = %+v, want %+v", got, want)
	}

	zoned := *cfg
	zoned.Objects = config.Objects{Replicas: 7, FailureDomain: "rack"}
	zoned.Estate = config.Estate{Replicas: 5, FailureDomain: "zone"}
	e.epoch.current.Store(&Company{Config: &zoned, ActivatedAt: activated.Add(time.Second)})
	got, _ = e.estateCompany()
	if got.Replicas != 5 || got.FailureDomain != "zone" || got.Epoch <= want.Epoch {
		t.Errorf("a later revision naming 5 copies across zones reads as %+v", got)
	}
}
