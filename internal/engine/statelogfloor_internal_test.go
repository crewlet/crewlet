package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE FLOOR IS THE TRIM'S PUBLISHED DECISION, and every branch of reading it
// is reachable without a fleet.
func TestTheFloorIsReadFromThePublishedTrimDecision(t *testing.T) {
	t.Parallel()
	floors := []coord.TrimFloor{
		{Domain: "tracker", Generation: 3, TrimTo: 4_200},
		{Domain: "pages", Generation: 3, TrimTo: 0, BlockedBy: "backup_floor"},
		{Domain: "vectors", Generation: 5, TrimTo: 90},
	}
	for name, tc := range map[string]struct {
		domain string
		gen    uint32
		want   uint64
		err    bool
	}{
		"the trim's own conclusion":          {domain: "tracker", gen: 3, want: 4_200},
		"a blocked trim licenses nothing":    {domain: "pages", gen: 3, want: 0},
		"a domain the trim never reached":    {domain: "other", gen: 3, want: 0},
		"a floor from a previous generation": {domain: "tracker", gen: 4, want: 0},
		"a floor from a generation ahead":    {domain: "vectors", gen: 3, err: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := floorFor(floors, tc.domain, tc.gen)
			if (err != nil) != tc.err {
				t.Fatalf("err = %v, want error=%v", err, tc.err)
			}
			if got != tc.want {
				t.Fatalf("floor = %d, want %d", got, tc.want)
			}
		})
	}
}

// A PUBLISHED FLOOR AHEAD OF THIS NODE REFUSES IT.
//
// # The vacuous comparand this replaces
//
// The floor every fence, the readiness gate and the join compared against was
// the minimum over every node's published position — this node's own row
// included. A minimum that includes the reader can never exceed the reader, so
// the comparison was decided before it was made: a node genuinely below the
// fleet's floor kept serving, kept admitting seats, and kept publishing at an
// expectation of zero over records the trim had removed. This publishes a
// floor the way the trim does and requires the node to notice.
func TestANodeBelowThePublishedFloorRefusesToServe(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	s := e.native.log
	running := s.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	// THIS NODE'S OWN TRIM DUTY PUBLISHES THE SAME FIELD, and it ticks
	// immediately at boot: its first conclusion is `blocked_by
	// backup_floor` at zero, which lands on top of the floor published
	// below and reads back as ok. Stop it first — it waits out an
	// in-flight tick — so the floor under test is the only one there is.
	e.stopRetention()

	// THE TRIM CONCLUDES the fleet may remove everything below a point this
	// node has not reached — which is what happens to a node that was away
	// while its peers moved on and were counted without it.
	at := running.runner.Committed()
	if err := back.Fleet.PutFloor(t.Context(), coord.TrimFloor{
		Domain: tracker.Domain{}.Name(), Generation: at.Generation,
		TrimTo: at.Seq + 5, At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish a floor: %v", err)
	}
	health, err := s.health(t.Context(), running)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	var carried uint64
	if health.TrimFloor != nil {
		carried = *health.TrimFloor
	}
	if health.TrimFloor == nil || carried != at.Seq+5 {
		t.Fatalf("health carries a floor of %d (known=%v), want the published %d — "+
			"the positions minimum this replaces could never exceed this node's own row",
			carried, health.TrimFloor != nil, at.Seq+5)
	}
	if health.Floor.State != statelog.FloorBelow {
		t.Fatalf("health reads the floor as %s with a published floor 5 past the "+
			"checkpoint, want below", health.Floor.State)
	}
	if e.NativeHydrated() {
		t.Fatal("the node admits seats while below the published floor")
	}
	if ok, _ := e.SeatsServiceable(); ok {
		t.Fatal("the node keeps its seats while below the published floor")
	}

	// AND A FLOOR EXACTLY AT THE NEXT RECORD IS NOT BELOW: the node has
	// consumed everything the trim may have removed.
	if err := back.Fleet.PutFloor(t.Context(), coord.TrimFloor{
		Domain: tracker.Domain{}.Name(), Generation: at.Generation,
		TrimTo: at.Seq + 1, At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish a floor: %v", err)
	}
	health, err = s.health(t.Context(), running)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if health.Floor.State != statelog.FloorOK {
		t.Fatalf("health reads the floor as %s with a published floor at the next "+
			"record, want ok", health.Floor.State)
	}
}
