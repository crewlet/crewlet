package engine

import (
	"context"
	"errors"
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

// runningWatch is an estate watch over maps and leases on clock c, running its
// view until the test ends, and fresh.
func runningWatch(t *testing.T, maps partmap.MapSource, leases coord.Backend,
	presence *coord.LeaseView, running statelog.Layout, c *viewClock) *estateWatch {

	t.Helper()
	b := config.DefaultBootstrap()
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
	if r.EstateViewAge == nil || *r.EstateViewAge != 0 {
		t.Errorf("a view confirmed this instant reads as %v old", r.EstateViewAge)
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
	for _, at := w.view.Confirmed(); !at.Equal(c.Now()); _, at = w.view.Confirmed() {
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
	if r.EstateViewAge == nil || *r.EstateViewAge != statelog.FloorCacheStale+time.Minute ||
		r.EstateViewStale != "the estate map" {
		t.Fatalf("the view's age reads %v of %q, want the estate map %v ago", r.EstateViewAge,
			r.EstateViewStale, statelog.FloorCacheStale+time.Minute)
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
// and the view is still judged, with the fleet's presence it routes the estate
// by among its halves.
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
	// THE PRESENCE VIEW NEVER LISTED — it is not run here — so it is as old
	// as the watch, and it is the half the alarm names.
	if r.EstateViewAge == nil || *r.EstateViewAge != 2*time.Minute ||
		r.EstateViewStale != "the fleet's presence" {
		t.Errorf("the view's age reads %v of %q, want the fleet's presence two minutes old",
			r.EstateViewAge, r.EstateViewStale)
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
	if got := r.reading(t.Context(), c.Now(), coord.BackupPoint{}, false, nil); got.EstateViewAge != nil ||
		got.EstateJoinBudget != 0 {
		t.Errorf("a node running no estate view reads %+v", got)
	}
	e.estateWatch.Store(w)
	got := r.reading(t.Context(), c.Now(), coord.BackupPoint{}, false, nil)
	if got.EstateUnserved != len(testLayoutOne.Partitions()) || got.EstateViewAge == nil {
		t.Fatalf("the reading does not carry the estate view: %+v", got)
	}
	if !firedKind(statelog.Evaluate(got), statelog.KindEstateUnserved) {
		t.Error("an estate map nobody serves raised no estate_partition_unserved")
	}
}
