package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
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
		// left is a floor at a generation this node has left, which is
		// its own error rather than any error: its remedy is an adoption,
		// not coordination answering, and the alarm it feeds says so.
		left bool
	}{
		"the trim's own conclusion":          {domain: "tracker", gen: 3, want: 4_200},
		"a blocked trim licenses nothing":    {domain: "pages", gen: 3, want: 0},
		"a domain the trim never reached":    {domain: "other", gen: 3, want: 0},
		"a floor from a previous generation": {domain: "tracker", gen: 4, want: 0},
		"a floor from a generation ahead":    {domain: "vectors", gen: 3, left: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := floorFor(floors, tc.domain, tc.gen)
			if errors.Is(err, errGenerationLeft) != tc.left || (err != nil && !tc.left) {
				t.Fatalf("err = %v, want a generation left=%v", err, tc.left)
			}
			if got != tc.want {
				t.Fatalf("floor = %d, want %d", got, tc.want)
			}
		})
	}
}

// AN UNREADABLE FLOOR AND A FLOOR AT A GENERATION THIS NODE HAS LEFT ARE TWO
// RUNS, AND ONLY THE SECOND RAISES `generation_left`.
//
// Both refuse every read at once, and both are about duration. But an
// unreadable register is coordination not answering, which clears when it
// answers; a floor at a newer generation answered perfectly well and names a
// number space this node's rows are not in, which nothing clears but an
// adoption. One run for both would tell the operator of the second to wait for
// coordination that is already answering.
//
// Mutation: classify a floor ahead as unreadable in [readOf] and the alarm
// that fires is `floor_unknown`; keep one run for both in [floorWatch] and a
// read at this node's own generation leaves the other run standing.
func TestAnUnreadableFloorAndAGenerationLeftAreTwoRuns(t *testing.T) {
	t.Parallel()
	floors := []coord.TrimFloor{{Domain: "tracker", Generation: 3, TrimTo: 7}}
	for name, tc := range map[string]struct {
		err  error
		gen  uint32
		want floorRead
	}{
		"a register that did not answer":    {err: errors.New("nats: timeout"), gen: 3, want: floorUnreadable},
		"a floor at this node's generation": {gen: 3, want: floorReadable},
		"a floor at a generation this left": {gen: 2, want: floorLeft},
		"a floor at a generation behind it": {gen: 4, want: floorReadable},
	} {
		t.Run(name, func(t *testing.T) {
			if got := readOf(floors, tc.err, "tracker", tc.gen); got != tc.want {
				t.Fatalf("readOf = %d, want %d", got, tc.want)
			}
		})
	}

	at := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	var watch floorWatch
	watch.observe(at, readOf(floors, nil, "tracker", 2))
	watch.observe(at.Add(10*time.Second), readOf(floors, nil, "tracker", 2))
	now := at.Add(statelog.FloorCacheStale + time.Second)
	left, behind := watch.leftFor(now)
	if !behind || left != statelog.FloorCacheStale+time.Second {
		t.Fatalf("a floor at a generation this node left reads as a run of (%s, %v), "+
			"want %s from the first beat that saw it", left, behind,
			statelog.FloorCacheStale+time.Second)
	}
	if lost, unreadable := watch.unreadableFor(now); unreadable {
		t.Fatalf("a floor that answered reads as unreadable for %s", lost)
	}
	var fired []statelog.Kind
	for _, alarm := range statelog.Evaluate(statelog.Reading{GenerationLeftFor: left}) {
		fired = append(fired, alarm.Kind)
	}
	if !slices.Contains(fired, statelog.KindGenerationLeft) ||
		slices.Contains(fired, statelog.KindFloorUnknown) {
		t.Fatalf("a floor at a generation this node left fires %v, want "+
			"generation_left and not floor_unknown", fired)
	}

	// AN ADOPTION ENDS IT: the next read finds the floor at this node's own
	// generation.
	watch.observe(now, readOf(floors, nil, "tracker", 3))
	if left, behind := watch.leftFor(now.Add(time.Minute)); behind {
		t.Fatalf("a read at this node's own generation left the run standing (%s)", left)
	}
}

// A PUBLISHED FLOOR AHEAD OF THIS NODE REFUSES IT.
//
// The floor every fence, the readiness gate and the join compare against is
// the trim's published decision, not a minimum over the fleet's positions: a
// minimum that includes the reader's own row can never exceed the reader, so a
// node genuinely below the fleet's floor would keep serving, keep admitting
// seats, and keep publishing at an expectation of zero over records the trim
// removed. This publishes a floor the way the trim does and requires the node
// to notice.
func TestANodeBelowThePublishedFloorRefusesToServe(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
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
			"a minimum over the fleet's positions can never exceed this node's own row",
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
