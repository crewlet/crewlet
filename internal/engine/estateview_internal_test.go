package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/statelog"
)

// viewClock is a clock the estate view and its watch share, moved by hand.
type viewClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *viewClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *viewClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// breakableMaps reads the estate map until it is told to stop answering, and
// its watch delivers nothing: the map half of a view goes quiet by construction
// while the lease half goes on being listed.
type breakableMaps struct {
	partmap.MapSource
	mu     sync.Mutex
	broken bool
}

func (b *breakableMaps) EstateMap(ctx context.Context) (coord.EstateMapRecord, bool, error) {
	b.mu.Lock()
	broken := b.broken
	b.mu.Unlock()
	if broken {
		return coord.EstateMapRecord{}, false, errors.New("coordination timed out")
	}
	return b.MapSource.EstateMap(ctx)
}

func (b *breakableMaps) WatchEstateMap(ctx context.Context) (<-chan coord.EstateMapRecord, error) {
	ch := make(chan coord.EstateMapRecord)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (b *breakableMaps) set(broken bool) {
	b.mu.Lock()
	b.broken = broken
	b.mu.Unlock()
}

// breakableLeases lists the estate leases until it is told to stop answering.
type breakableLeases struct {
	coord.Backend
	mu     sync.Mutex
	broken bool
}

func (b *breakableLeases) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	b.mu.Lock()
	broken := b.broken
	b.mu.Unlock()
	if broken {
		return nil, errors.New("coordination timed out")
	}
	return b.Backend.ListLive(ctx, class)
}

func (b *breakableLeases) set(broken bool) {
	b.mu.Lock()
	b.broken = broken
	b.mu.Unlock()
}

// runningWatch is an estate watch over maps and leases on clock c, running its
// view until the test ends, and fresh.
func runningWatch(t *testing.T, maps partmap.MapSource, leases coord.Backend,
	presence *coord.LeaseView, running statelog.Layout, c *viewClock) *estateWatch {

	t.Helper()
	return runningWatchOn(t, config.DefaultBootstrap(), maps, leases, presence, running, c)
}

// runningWatchOn is runningWatch on this node's own Tier A.
func runningWatchOn(t *testing.T, b config.Bootstrap, maps partmap.MapSource, leases coord.Backend,
	presence *coord.LeaseView, running statelog.Layout, c *viewClock) *estateWatch {

	t.Helper()
	w, err := newEstateWatch(&b, maps, leases, presence, running, c.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.view.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	confirm(t, w)
	return w
}

// confirm has the view read the map and list the leases at the clock's
// current instant, and waits until it is fresh.
func confirm(t *testing.T, w *estateWatch) {
	t.Helper()
	if _, _, err := w.view.Read(t.Context()); err != nil && !errors.Is(err, partmap.ErrNoMap) {
		t.Fatalf("read the map: %v", err)
	}
	w.view.Invalidate()
	deadline := time.Now().Add(5 * time.Second)
	for !w.view.Fresh() {
		if time.Now().After(deadline) {
			t.Fatal("the estate view never became fresh")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// claimLeases claims a healthy estate lease at layout for every node.
func claimLeases(t *testing.T, b coord.Backend, layout int, nodes ...string) {
	t.Helper()
	healthy := true
	for _, node := range nodes {
		meta, err := partmap.Meta{Weight: 1, Layout: &layout, Healthy: &healthy,
			Partitions: map[string]partmap.PartitionState{}}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := b.TryAcquire(t.Context(), coord.EstateResource(node), coord.AcquireOptions{
			Owner: node + ":1", TTL: time.Hour, Meta: meta, Ungated: true}); err != nil {
			t.Fatal(err)
		}
	}
}

// THE ESTATE ALARMS READ WHAT THIS NODE'S VIEW HAS SEEN, over the time it has
// seen it: a map whose every partition is still being joined — a first map,
// with no copy serving anywhere — is every partition unserved and short, and
// every join in flight, and past membership's grace and the rejoin window
// each alarm fires on the table's own evaluation, naming what it found.
func TestTheEstateAlarmsReadWhatTheViewHasSeen(t *testing.T) {
	t.Parallel()
	c := &viewClock{now: gestureNow}
	leases := coordmemory.New()
	claimLeases(t, leases, testLayoutOne.Number, "a", "b", "c", "d")
	w := runningWatch(t, storedEstateMap(t, "a", "b", "c", "d"), leases, nil, testLayoutOne, c)

	w.sample(c.Now())
	c.advance(31 * time.Minute)
	confirm(t, w)
	w.sample(c.Now())

	var r statelog.Reading
	w.reading(c.Now(), &r)
	parts := len(testLayoutOne.Partitions())
	if r.EstateUnserved != parts || r.EstateShort != parts || r.EstateShortFor != 31*time.Minute ||
		r.EstateJoiningFor != 31*time.Minute || r.EstateJoinBudget != 30*time.Minute {
		t.Fatalf("the reading is %+v, want every one of %d partitions unserved and short "+
			"for 31m, joins for 31m against a 30m window", r, parts)
	}
	if r.EstateView == nil || r.EstateView.Age != 0 {
		t.Errorf("a view confirmed this instant reads as %+v", r.EstateView)
	}
	fired := map[statelog.Kind]string{}
	for _, a := range statelog.Evaluate(r) {
		fired[a.Kind] = a.Detail
	}
	for _, kind := range []statelog.Kind{statelog.KindEstateUnserved,
		statelog.KindEstateShort, statelog.KindEstateMoveStalled} {
		if fired[kind] == "" {
			t.Errorf("%s did not fire on %+v", kind, r)
		}
	}
	if _, stale := fired[statelog.KindEstateViewStale]; stale {
		t.Error("a view confirmed this instant raised estate_view_stale")
	}
}

// A VIEW THIS NODE COULD NOT CONFIRM IS NO SIGHTING: the watch forgets what it
// had seen, the three map alarms go quiet, and estate_view_stale names the
// half that went quiet and how long ago — so an alarm never fires on a
// shortfall nobody saw hold across the gap. The MAP half goes quiet here,
// which the view still answers routing from: only its freshness says the
// sighting is not one.
func TestAStaleEstateViewSaysSoAndSeesNothing(t *testing.T) {
	t.Parallel()
	c := &viewClock{now: gestureNow}
	leases := coordmemory.New()
	claimLeases(t, leases, testLayoutOne.Number, "a", "b", "c", "d")
	maps := &breakableMaps{MapSource: storedEstateMap(t, "a", "b", "c", "d")}
	w := runningWatch(t, maps, leases, nil, testLayoutOne, c)
	w.sample(c.Now())

	maps.set(true)
	c.advance(statelog.FloorCacheStale + time.Minute)
	w.view.Invalidate()
	deadline := time.Now().Add(5 * time.Second)
	// LISTED AT THE NEW INSTANT once the map is the half the view names:
	// until then the leases, an equal age past a shorter bound, are.
	for st := w.view.Staleness(c.Now()); st.Half != "the estate map"; st = w.view.Staleness(c.Now()) {
		if time.Now().After(deadline) {
			t.Fatal("the leases were never listed at the new instant")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, _, found, err := w.view.Map(); err != nil || !found {
		t.Fatalf("the premise: a quiet view still answers the map it holds: (%v, %v)", found, err)
	}
	w.sample(c.Now())

	var r statelog.Reading
	w.reading(c.Now(), &r)
	if r.EstateUnserved != 0 || r.EstateShort != 0 || r.EstateJoiningFor != 0 {
		t.Errorf("a stale view still reports what it saw: %+v", r)
	}
	if v := r.EstateView; v == nil || v.Age != statelog.FloorCacheStale+time.Minute ||
		v.Half != "the estate map" || v.Bound != statelog.FloorCacheStale {
		t.Fatalf("the view reads %+v, want the estate map %v ago against %v", r.EstateView,
			statelog.FloorCacheStale+time.Minute, statelog.FloorCacheStale)
	}
	alarms := statelog.Evaluate(r)
	if len(alarms) != 1 || alarms[0].Kind != statelog.KindEstateViewStale {
		t.Errorf("a stale view raised %v, want estate_view_stale alone", alarms)
	}

	// AND SEEN AGAIN, A SHORTFALL IS COUNTED FROM AGAIN.
	maps.set(false)
	confirm(t, w)
	w.sample(c.Now())
	w.reading(c.Now(), &r)
	if r.EstateShortFor != 0 {
		t.Errorf("a shortfall seen again after a gap reads %v, want counted from again",
			r.EstateShortFor)
	}
}

// UNDER LAYOUT 0 THERE IS NO MAP, so nothing is short, unserved or joining —
// and the view is still judged, on its own two halves: the store answering
// that there is no map, and the estate leases. The fleet's presence it names
// layout 0's servers from is NOT one of them — it answers routing, which may
// use any age — so a presence view that has never listed raises nothing here.
func TestUnderLayoutZeroTheEstateAlarmsJudgeTheViewAlone(t *testing.T) {
	t.Parallel()
	c := &viewClock{now: gestureNow}
	presence, err := coord.NewLeaseView(coordmemory.New(), coord.ClassNode, coord.ViewOptions{
		Every: time.Hour, Trust: time.Hour, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	w := runningWatch(t, coordmemory.NewFleet(), coordmemory.New(), presence, LayoutZero(), c)
	c.advance(2 * time.Minute)
	confirm(t, w)
	w.sample(c.Now())

	var r statelog.Reading
	w.reading(c.Now(), &r)
	if r.EstateUnserved != 0 || r.EstateShort != 0 || r.EstateJoiningFor != 0 {
		t.Errorf("layout 0 reports a map's findings: %+v", r)
	}
	// THE PRESENCE VIEW NEVER LISTED — it is not run here — and the view,
	// confirmed this instant, is current all the same.
	if v := r.EstateView; v == nil || v.Age != 0 {
		t.Errorf("a layout-0 view confirmed this instant reads %+v", r.EstateView)
	}
	if got := statelog.Evaluate(r); len(got) != 0 {
		t.Errorf("a current layout-0 view raised %v", got)
	}
}

// THE ALARM AND THE VIEW'S OWN FRESHNESS NEVER DISAGREE, at any TTL: the estate
// leases stop being listed, and at every instant after it estate_view_stale
// fires exactly when the view has stopped being fresh — at the leases' TTL
// where that is shorter than the minute every cached fact is held to (the
// default, 45 seconds), and at the minute where the TTL is longer. Judged at
// the minute whatever the TTL, the alarm stayed quiet from 45 to 60 seconds
// while the view was no longer fresh and the watch had already forgotten what
// it saw: the map alarms silent, and nothing saying why.
func TestTheStaleAlarmFiresExactlyWhenTheViewIsNotFresh(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ttlSeconds float64
		bound      time.Duration
	}{
		"the default lease TTL": {0, 45 * time.Second},
		"a five-minute TTL":     {300, statelog.FloorCacheStale},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := &viewClock{now: gestureNow}
			leases := &breakableLeases{Backend: coordmemory.New()}
			claimLeases(t, leases.Backend, testLayoutOne.Number, "a", "b", "c", "d")
			b := config.DefaultBootstrap()
			b.Coordination.LeaseTTLSeconds = tc.ttlSeconds
			w := runningWatchOn(t, b, storedEstateMap(t, "a", "b", "c", "d"), leases, nil,
				testLayoutOne, c)
			w.sample(c.Now())
			leases.set(true)
			for _, age := range []time.Duration{30 * time.Second, 50 * time.Second,
				tc.bound, tc.bound + time.Second, 2 * time.Minute} {
				c.advance(age - c.Now().Sub(gestureNow))
				// THE MAP GOES ON ANSWERING: only the leases are quiet. And
				// the watch is NOT sampled again, so what it saw while the
				// view was fresh is still in it: a reading must not report
				// that beside the alarm saying the view is stale.
				if _, _, err := w.view.Read(t.Context()); err != nil {
					t.Fatalf("Read: %v", err)
				}
				var r statelog.Reading
				w.reading(c.Now(), &r)
				fired := firedKind(statelog.Evaluate(r), statelog.KindEstateViewStale)
				if fresh := w.view.Fresh(); fired == fresh || fired != (age > tc.bound) {
					t.Errorf("leases unlisted for %v: estate_view_stale fired %v and the view "+
						"is fresh %v; want it to fire exactly past %v (%+v)", age, fired, fresh,
						tc.bound, r.EstateView)
				}
				// AND WHILE IT FIRES NOTHING THE VIEW SAW IS REPORTED.
				if fired && (r.EstateUnserved != 0 || r.EstateShort != 0 || r.EstateJoiningFor != 0) {
					t.Errorf("a stale view still reports what it saw: %+v", r)
				}
			}
		})
	}
}

// THE RETENTION LOOP'S READING CARRIES WHAT THE ESTATE VIEW HAS SEEN, so the
// table the loop evaluates — the gauge, the log line and the operator's screen
// alike — raises the estate alarms; and a node running no estate view reads
// as nothing to report rather than as a view never confirmed.
func TestTheRetentionReadingCarriesTheEstateView(t *testing.T) {
	t.Parallel()
	c := &viewClock{now: gestureNow}
	leases := coordmemory.New()
	claimLeases(t, leases, testLayoutOne.Number, "a", "b", "c", "d")
	w := runningWatch(t, storedEstateMap(t, "a", "b", "c", "d"), leases, nil, testLayoutOne, c)
	w.sample(c.Now())

	e := &Engine{}
	r := &retention{state: &stateLog{}, fleet: coordmemory.NewFleet(), nodeID: "a",
		estate: e.estateReading}
	if got := r.reading(t.Context(), c.Now(), coord.BackupPoint{}, false, nil); got.EstateView != nil ||
		got.EstateJoinBudget != 0 {
		t.Errorf("a node running no estate view reads %+v", got)
	}
	e.estateWatch.Store(w)
	got := r.reading(t.Context(), c.Now(), coord.BackupPoint{}, false, nil)
	if got.EstateUnserved != len(testLayoutOne.Partitions()) || got.EstateView == nil {
		t.Fatalf("the reading does not carry the estate view: %+v", got)
	}
	if !firedKind(statelog.Evaluate(got), statelog.KindEstateUnserved) {
		t.Error("an estate map nobody serves raised no estate_partition_unserved")
	}
}

// AN ALARM'S LINE NAMES THE FIRST THREE FINDINGS AND COUNTS THE REST, so it
// names enough to start from and stays one line however many partitions a lost
// node held — and nothing at all where there is nothing to name.
func TestAnEstateAlarmNamesThreeFindingsAndCountsTheRest(t *testing.T) {
	t.Parallel()
	partitions := func(n int) []partmap.Finding {
		out := make([]partmap.Finding, n)
		for i := range out {
			out[i] = partmap.Finding{Partition: fmt.Sprintf("tracker.%03d", i)}
		}
		return out
	}
	name := func(f partmap.Finding) string { return f.Partition }
	for n, want := range map[int]string{
		0: "",
		1: "tracker.000",
		3: "tracker.000, tracker.001, tracker.002",
		4: "tracker.000, tracker.001, tracker.002 and 1 more",
		5: "tracker.000, tracker.001, tracker.002 and 2 more",
	} {
		if got := findingNames(partitions(n), name); got != want {
			t.Errorf("%d findings read %q, want %q", n, got, want)
		}
	}
}

// THE READING NAMES WHAT HAS HELD LONGEST, as the watch orders it: the partition
// short of copies longest, with its copies, and the oldest join, with the node
// joining — and the unserved partitions by name.
func TestTheEstateReadingNamesWhatHasHeldLongest(t *testing.T) {
	t.Parallel()
	c := &viewClock{now: gestureNow}
	leases := coordmemory.New()
	claimLeases(t, leases, testLayoutOne.Number, "a", "b", "c", "d")
	w := runningWatch(t, storedEstateMap(t, "a", "b", "c", "d"), leases, nil, testLayoutOne, c)
	w.mu.Lock()
	w.found = partmap.Findings{
		Unserved: []partmap.Finding{{Partition: "tracker.003"}, {Partition: "company.000"}},
		Short: []partmap.Finding{
			{Partition: "tracker.002", Serving: 1, Wanted: 3, For: 12 * time.Minute},
			{Partition: "tracker.000", Serving: 2, Wanted: 3, For: 3 * time.Minute},
		},
		Joining: []partmap.Finding{
			{Partition: "tracker.001", Node: "c", For: 40 * time.Minute},
			{Partition: "tracker.002", Node: "d", For: time.Minute},
		},
	}
	w.mu.Unlock()
	var r statelog.Reading
	w.reading(c.Now(), &r)
	if r.EstateUnserved != 2 || r.EstateUnservedWhich != "tracker.003, company.000" {
		t.Errorf("unserved reads %d: %q", r.EstateUnserved, r.EstateUnservedWhich)
	}
	if r.EstateShort != 2 || r.EstateShortFor != 12*time.Minute ||
		r.EstateShortWhich != "tracker.002 (1 of 3 copies)" {
		t.Errorf("short reads %d for %v: %q, want tracker.002's twelve minutes", r.EstateShort,
			r.EstateShortFor, r.EstateShortWhich)
	}
	if r.EstateJoiningFor != 40*time.Minute || r.EstateJoiningWhich != "tracker.001 on c" {
		t.Errorf("joining reads %v: %q, want tracker.001 on c for forty minutes",
			r.EstateJoiningFor, r.EstateJoiningWhich)
	}
}

// UNDER LAYOUT 0 THE ONE PARTITION IS UNSERVED WHEN NO DATA NODE A ROUTER WOULD
// ASK HAS A COPY THAT SERVES IT — every live data node's copy wrong, each of
// which stops serving it — and estate_partition_unserved says so, by the rule
// the router routes by: a data node from the presence view whose estate lease
// says its copy serves or is catching up. One copy that recovers clears it.
func TestUnderLayoutZeroAPartitionNoCopyServesIsUnserved(t *testing.T) {
	t.Parallel()
	c := &viewClock{now: gestureNow}
	presences := coordmemory.New()
	for _, id := range []string{"data-a", "data-b"} {
		if _, _, err := presences.TryAcquire(t.Context(), coord.NodeResource(id), coord.AcquireOptions{
			Owner: id + ":1", TTL: time.Hour, Meta: map[string]any{"roles": []string{"data", "seats"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	presence, err := coord.NewLeaseView(presences, coord.ClassNode, coord.ViewOptions{
		Every: time.Hour, Trust: time.Hour, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = presence.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, 5*time.Second, "the presence view to list", func() bool {
		return !presence.ListedAt().IsZero()
	})

	leases := coordmemory.New()
	layout := 0
	healthy := true
	claim := func(node string, state partmap.PartitionState) {
		t.Helper()
		meta, err := partmap.Meta{Weight: 1, Layout: &layout, Healthy: &healthy,
			Partitions: map[string]partmap.PartitionState{
				statelog.EstatePartition.String(): state}}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := leases.TryAcquire(t.Context(), coord.EstateResource(node), coord.AcquireOptions{
			Owner: node + ":1", TTL: time.Hour, Meta: meta, Ungated: true}); err != nil {
			t.Fatal(err)
		}
	}
	claim("data-a", partmap.PartFaulted)
	claim("data-b", partmap.PartFaulted)
	w := runningWatch(t, coordmemory.NewFleet(), leases, presence, LayoutZero(), c)
	confirm(t, w)
	w.sample(c.Now())

	var r statelog.Reading
	w.reading(c.Now(), &r)
	if r.EstateUnserved != 1 || r.EstateUnservedWhich != statelog.EstatePartition.String() {
		t.Fatalf("every data node's copy wrong reads %d unserved (%q), want estate.000",
			r.EstateUnserved, r.EstateUnservedWhich)
	}
	if !slices.ContainsFunc(statelog.Evaluate(r), func(a statelog.Alarm) bool {
		return a.Kind == statelog.KindEstateUnserved
	}) || r.EstateShort != 0 {
		t.Fatalf("an unserved layout-0 estate raised %v with %d short, want "+
			"estate_partition_unserved alone", statelog.Evaluate(r), r.EstateShort)
	}

	claim("data-b", partmap.PartCatchingUp)
	c.advance(2 * coord.MinViewRefresh)
	w.view.Invalidate()
	waitUntil(t, 5*time.Second, "the view to list data-b catching up", func() bool {
		live, err := w.view.Presences()
		return err == nil && slices.ContainsFunc(live, func(p partmap.Presence) bool {
			return p.Node == "data-b" &&
				p.Meta.Partitions[statelog.EstatePartition.String()] == partmap.PartCatchingUp
		})
	})
	w.sample(c.Now())
	r = statelog.Reading{}
	w.reading(c.Now(), &r)
	if r.EstateUnserved != 0 {
		t.Fatalf("a copy catching up — which serves — left the estate unserved: %+v", r)
	}
}
