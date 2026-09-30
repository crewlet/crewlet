package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// localWith is a local estate serving estate.000, judging its copy by read.
func localWith(e *Engine, now func() time.Time,
	read func(context.Context, *native, statelog.PartitionID) copyVerdict) *localEstate {
	return &localEstate{e: e, holding: statelog.ServesOnly(statelog.EstatePartition), now: now,
		read: read, verdicts: map[statelog.PartitionID]*verdictSlot{}}
}

// landed waits for p's verdict read in flight, if there is one, to land.
func landed(l *localEstate, p statelog.PartitionID) {
	l.mu.Lock()
	var reading chan struct{}
	if slot := l.verdicts[p]; slot != nil {
		reading = slot.reading
	}
	l.mu.Unlock()
	if reading != nil {
		<-reading
	}
}

// judged is For after the verdict it finds stale has been read again: the
// request that finds it stale answers from the verdict before it, and the one
// after the read lands answers from the new one.
func judged(ctx context.Context, l *localEstate, p statelog.PartitionID) (estate.Backend, bool) {
	_, _, _ = l.For(ctx, p)
	landed(l, p)
	b, ok, _ := l.For(ctx, p)
	return b, ok
}

// sound is a copy that is not wrong and answers requests.
func sound(context.Context, *native, statelog.PartitionID) copyVerdict {
	return copyVerdict{answers: true}
}

// A DATA NODE ANSWERS ONLY WHAT IT SERVES: the partitions gate 3 says it
// serves ([holdingOf]), never one it does not hold and never one whose holding
// it cannot tell — the router asks another holder for those. A partition it
// holds before its runtime is up is still served, with no halves, so every
// operation on it is answered "not here" and moves on rather than being
// refused as a partition nobody serves.
func TestTheLocalEstateAnswersOnlyWhatThisNodeServes(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{}}
	l := localWith(e, time.Now, sound)

	b, ok, err := l.For(t.Context(), statelog.EstatePartition)
	if !ok || err != nil || b.Tracker != nil {
		t.Fatalf("a held partition with no runtime = (%+v, %v, %v), want served with no halves",
			b, ok, err)
	}
	e.native.Store(&native{trackerReader: &tracker.Reader{}})
	if b, ok, err = l.For(t.Context(), statelog.EstatePartition); !ok || err != nil ||
		b.Tracker == nil || b.Answers == nil || !b.Answers(t.Context()) {
		t.Fatalf("a held partition with a runtime = (%+v, %v, %v), want its halves, answering",
			b, ok, err)
	}
	if _, ok, err := l.For(t.Context(), statelog.PartitionID{Space: statelog.SpaceTracker, Index: 7}); ok ||
		err != nil {
		t.Errorf("a partition this node does not hold = (%v, %v), want definitively not served",
			ok, err)
	}
	// CANNOT TELL is its own answer, never a "no": another node asking is
	// told `holding_unknown`, not `not_holder`.
	l.holding = failingHolding{}
	if _, ok, err := l.For(t.Context(), statelog.EstatePartition); ok || err == nil {
		t.Errorf("a partition whose holding could not be told = (%v, %v), want not served "+
			"and unknown", ok, err)
	}
}

type failingHolding struct{}

func (failingHolding) Serving(statelog.PartitionID) (bool, error) {
	return false, errors.New("the executor could not say")
}

// A COPY THAT IS WRONG STOPS SERVING ITS PARTITION, AND THE NODE KEEPS ITS
// SEATS.
//
// A halted applier, an eviction, rows below the log, a checkpoint on another
// stream, a stalled prefix, a record held past its grace: this node's router
// no longer answers the partition from its own copy — its seats' calls go to
// the partition's other holders, and another node asking is told `not_holder`
// — while serviceability, which is about routing, keeps every seat. It serves
// again the moment a reading finds the copy sound.
func TestAWrongCopyStopsServingAndTheSeatsStay(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	verdict := copyVerdict{answers: true}
	e := &Engine{backends: &Backends{}}
	e.native.Store(&native{trackerReader: &tracker.Reader{}})
	l := localWith(e, func() time.Time { return now },
		func(context.Context, *native, statelog.PartitionID) copyVerdict { return verdict })

	if _, ok, _ := l.For(t.Context(), statelog.EstatePartition); !ok {
		t.Fatal("a sound copy is not served")
	}
	now = now.Add(servingRecheck)
	verdict = copyVerdict{fault: "tracker", answers: true}
	if _, ok := judged(t.Context(), l, statelog.EstatePartition); ok {
		t.Fatal("a copy that is wrong still serves its partition")
	}
	if ok, reason := e.SeatsServiceable(); !ok {
		t.Fatalf("a wrong copy shed the node's seats (%s) — it stops serving its "+
			"partition, and the seats read it from another holder", reason)
	}

	// A BLIP DOES NOT BRING IT BACK: a reading that reached no broker keeps
	// the fault it had.
	now = now.Add(servingRecheck)
	verdict = copyVerdict{refusal: statelog.RefuseBrokerUnreachable}
	if _, ok := judged(t.Context(), l, statelog.EstatePartition); ok {
		t.Fatal("a reading that reached no broker put a wrong copy back into service")
	}
	now = now.Add(servingRecheck)
	verdict = copyVerdict{answers: true}
	if _, ok := judged(t.Context(), l, statelog.EstatePartition); !ok {
		t.Fatal("a copy found sound again is still not served")
	}
}

// WHETHER A COPY ANSWERS IS READ AT MOST ONCE A RECHECK, and a read that could
// not reach the broker keeps the verdict before it — while a copy never judged
// answers nothing.
//
// The verdict reads every log's bounds from the broker, so read per request
// it would put three round trips in front of every tool call; and one blip
// must not send every request away from a copy that was answering them.
func TestACopysVerdictIsReadOncePerRecheckAndKeptThroughABlip(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	var reads atomic.Int32
	verdict := copyVerdict{answers: true}
	l := localWith(&Engine{}, func() time.Time { return now },
		func(context.Context, *native, statelog.PartitionID) copyVerdict {
			reads.Add(1)
			return verdict
		})
	n := &native{}
	p := statelog.EstatePartition
	fresh := func() copyVerdict {
		l.verdict(t.Context(), n, p)
		landed(l, p)
		return l.verdict(t.Context(), n, p)
	}

	// A COPY NEVER JUDGED, whose first read found no broker, answers
	// nothing.
	verdict = copyVerdict{refusal: statelog.RefuseBrokerUnreachable}
	if l.verdict(t.Context(), n, p).answers {
		t.Fatal("a copy nobody could measure answered")
	}
	now = now.Add(servingRecheck)
	verdict = copyVerdict{answers: true}
	if !fresh().answers {
		t.Fatal("a copy measured as answering did not")
	}
	for range 10 {
		l.verdict(t.Context(), n, p)
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("the verdict was read %d times, want 2 — once per recheck", got)
	}
	now = now.Add(servingRecheck)
	verdict = copyVerdict{refusal: statelog.RefuseBrokerUnreachable}
	if !fresh().answers {
		t.Fatal("a broker blip turned an answering copy away")
	}
	now = now.Add(servingRecheck)
	verdict = copyVerdict{refusal: statelog.RefuseBehind}
	if fresh().answers {
		t.Fatal("a copy that fell behind still answers")
	}
}

// A SLOW BROKER NEVER QUEUES THE REQUESTS THAT ASK ABOUT A COPY.
//
// Every tool call a data node's seats make and every request another node
// sends it asks the copy's verdict first, and a read of the broker can take
// its whole budget. So ONE read runs per partition however many requests found
// the verdict stale, nobody but a request for a partition never judged waits
// on it, a request that stops waiting leaves when its own context ends — and
// the read is stamped with the instant it LANDED, so a read slower than the
// recheck is not stale the moment it is stored and read again by the next
// request in line.
func TestASlowVerdictReadQueuesNoRequest(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(time.Unix(1_700_000_000, 0).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	var reads atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{}, 16)
	l := localWith(&Engine{}, now,
		func(context.Context, *native, statelog.PartitionID) copyVerdict {
			reads.Add(1)
			started <- struct{}{}
			<-release
			// SLOWER THAN THE RECHECK, as a broker mid-election is.
			clock.Add(int64(3 * servingRecheck))
			return copyVerdict{answers: true}
		})
	n := &native{}
	p := statelog.EstatePartition

	// NEVER JUDGED: every request waits on the ONE read, and a request
	// whose caller gives up leaves without it.
	const requests = 8
	var wg sync.WaitGroup
	answered := make(chan bool, requests)
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answered <- l.verdict(t.Context(), n, p).answers
		}()
	}
	<-started
	impatient, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	v, returned := within(time.Second, func() copyVerdict { return l.verdict(impatient, n, p) })
	close(release)
	switch {
	case !returned:
		t.Fatal("a request whose caller gave up waited on the read in flight")
	case v.answers || v.fault != "":
		t.Fatalf("a request that stopped waiting on a copy never judged answered %+v", v)
	}
	wg.Wait()
	close(answered)
	for ok := range answered {
		if !ok {
			t.Fatal("a request waiting on the read did not get its answer")
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("%d concurrent requests read the verdict %d times, want once", requests, got)
	}
	// STAMPED WHEN IT LANDED: three rechecks passed during the read, and
	// the verdict it stored is still fresh.
	l.verdict(t.Context(), n, p)
	landed(l, p)
	if got := reads.Load(); got != 1 {
		t.Fatalf("a verdict read slower than the recheck was stale when stored: %d reads", got)
	}

	// JUDGED BEFORE: a stale verdict is answered at once from the one
	// before it while the read that replaces it is in flight.
	clock.Add(int64(servingRecheck))
	stuck := make(chan struct{})
	l.read = func(context.Context, *native, statelog.PartitionID) copyVerdict {
		reads.Add(1)
		<-stuck
		return copyVerdict{answers: true}
	}
	answered2, returned := within(time.Second, func() bool {
		for range requests {
			if !l.verdict(t.Context(), n, p).answers {
				return false
			}
		}
		return true
	})
	close(stuck)
	switch {
	case !returned:
		t.Fatal("requests with a verdict to answer from waited on the read in flight")
	case !answered2:
		t.Fatal("a stale verdict was not answered from the one before it")
	}
	landed(l, p)
	if got := reads.Load(); got != 2 {
		t.Fatalf("%d requests on a stale verdict read it %d times in all, want one more read",
			requests, got)
	}
}

// A NODE KEEPS ITS SEATS WHILE IT CAN ROUTE, and sheds them only once its view
// of who serves the estate answers unknown AND was last read longer ago than
// every decider trusts a cached coordination fact — never before its first
// listing, when it holds no seat to shed.
func TestANodeShedsItsSeatsOnlyOnceItCannotRoute(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(time.Unix(1_700_000_000, 0).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	advance := func(d time.Duration) { clock.Add(int64(d)) }
	store := &flakyLister{Lister: coordmem.New()}
	view, err := coord.NewLeaseView(store, coord.ClassNode, coord.ViewOptions{
		Every: 15 * time.Second, Trust: 45 * time.Second, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, reason := routable(view, now()); !ok {
		t.Fatalf("a view that has never listed shed the seats: %s", reason)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = view.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, 5*time.Second, "the view to list", func() bool { return !view.ListedAt().IsZero() })

	if ok, reason := routable(view, now()); !ok {
		t.Fatalf("a node with a current view shed its seats: %s", reason)
	}
	// UNKNOWN, but inside the bound: the view's own trust has lapsed and
	// the decider's has not.
	store.fail()
	advance(50 * time.Second)
	if _, _, err := view.Leases(); err == nil {
		t.Fatal("the premise: a view past its trust answers")
	}
	if ok, reason := routable(view, now()); !ok {
		t.Fatalf("a node shed its seats inside the %s bound: %s", statelog.FloorCacheStale, reason)
	}
	advance(statelog.FloorCacheStale)
	ok, reason := routable(view, now())
	if ok || !strings.Contains(reason, "cannot say who serves the estate") {
		t.Fatalf("a node that cannot route kept its seats (%v, %q)", ok, reason)
	}

	// A VIEW STILL ANSWERING IS ONE THE NODE CAN ROUTE BY, however old its
	// listing: on a deployment whose leases live five minutes, a listing
	// ninety seconds old is still an answer, and the router reads it.
	long := &flakyLister{Lister: coordmem.New()}
	patient, err := coord.NewLeaseView(long, coord.ClassNode, coord.ViewOptions{
		Every: 100 * time.Second, Trust: 300 * time.Second, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	longCtx, stopLong := context.WithCancel(t.Context())
	longDone := make(chan struct{})
	go func() { defer close(longDone); _ = patient.Run(longCtx) }()
	t.Cleanup(func() { stopLong(); <-longDone })
	waitUntil(t, 5*time.Second, "the long view to list", func() bool { return !patient.ListedAt().IsZero() })
	long.fail()
	advance(90 * time.Second)
	if ok, reason := routable(patient, now()); !ok {
		t.Fatalf("a node whose view still answers shed its seats: %s", reason)
	}
}

// within runs f and answers what it returned, or false when it had not
// returned within d — for a case whose failure is a caller that never comes
// back, which must fail the case rather than hang it. f is left running.
func within[T any](d time.Duration, f func() T) (T, bool) {
	done := make(chan T, 1)
	go func() { done <- f() }()
	select {
	case v := <-done:
		return v, true
	case <-time.After(d):
		var zero T
		return zero, false
	}
}

// flakyLister lists from its store until it is told to fail.
type flakyLister struct {
	coord.Lister
	failing atomic.Bool
}

func (f *flakyLister) fail() { f.failing.Store(true) }

func (f *flakyLister) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	if f.failing.Load() {
		return nil, errors.New("the store is unreachable")
	}
	return f.Lister.ListLive(ctx, class)
}

// staleView is a view of the fleet's data nodes that listed once and has since
// answered unknown for longer than any decider trusts a cached coordination
// fact — a node that can name no holder of anything — on a clock it answers.
func staleView(t *testing.T) (*coord.LeaseView, func() time.Time) {
	t.Helper()
	var clock atomic.Int64
	clock.Store(time.Unix(1_700_000_000, 0).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	store := &flakyLister{Lister: coordmem.New()}
	view, err := coord.NewLeaseView(store, coord.ClassNode, coord.ViewOptions{
		Every: 15 * time.Second, Trust: 45 * time.Second, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = view.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	waitUntil(t, 5*time.Second, "the view to list", func() bool { return !view.ListedAt().IsZero() })
	store.fail()
	clock.Add(int64(45*time.Second + statelog.FloorCacheStale + time.Second))
	if ok, _ := routable(view, now()); ok {
		t.Fatal("the premise: a view unknown past the bound cannot route")
	}
	return view, now
}

// dataNodeOver is a data node serving estate.000 from a copy judged by read,
// whose router routes by view.
func dataNodeOver(t *testing.T, view *coord.LeaseView, now func() time.Time,
	read func(context.Context, *native, statelog.PartitionID) copyVerdict) *Engine {

	t.Helper()
	e := &Engine{backends: &Backends{}, dataView: view}
	e.native.Store(&native{trackerReader: &tracker.Reader{}})
	e.local = localWith(e, now, read)
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "data-self", Queue: silentAsker{}, Placement: e.estatePlacement(),
		Local: e.local, Session: estate.NewSession(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.router = router
	return e
}

// silentAsker is a fleet nobody else answers in.
type silentAsker struct{}

func (silentAsker) Ask(context.Context, string, []byte, int) ([][]byte, error) { return nil, nil }

// A NODE THAT ANSWERS EVERY PARTITION ITS SEATS NEED FROM ITS OWN COPY ROUTES
// THEM WITHOUT THE VIEW — the router asks this node first and asks nobody
// where it answers — so a view gone unknown past the bound sheds none of its
// seats and withholds none of its claims. Under layout 0 that is every data
// node whose copy is sound: a partial coordination fault that stopped the
// fleet listing and not the seat renewals used to release a single data
// node's every seat, and then refuse to claim them back, while its own copy
// served them all along. A node that must ask another holder for any
// partition still sheds on the same view, since its seats' calls have nowhere
// it can name to go.
func TestANodeServingItsSeatsItselfNeedsNoViewToKeepOrClaimThem(t *testing.T) {
	t.Parallel()
	view, now := staleView(t)
	var faulted atomic.Bool
	e := dataNodeOver(t, view, now, func(context.Context, *native, statelog.PartitionID) copyVerdict {
		if faulted.Load() {
			return copyVerdict{fault: "tracker", answers: true}
		}
		return copyVerdict{answers: true}
	})
	judged(t.Context(), e.local, statelog.EstatePartition)

	if ok, reason := e.serviceable(now()); !ok {
		t.Fatalf("a data node serving its seats from its own copy shed them over a stale "+
			"view: %s", reason)
	}
	if !e.NativeHydrated(t.Context()) {
		t.Fatal("a data node whose own copy admits a seat withheld its claims over a stale view")
	}

	// ITS COPY WRONG, it serves nothing itself: the view decides, and
	// cannot.
	faulted.Store(true)
	e.local.mu.Lock()
	delete(e.local.verdicts, statelog.EstatePartition)
	e.local.mu.Unlock()
	judged(t.Context(), e.local, statelog.EstatePartition)
	if ok, _ := e.serviceable(now()); ok {
		t.Fatal("a node whose copy is wrong and whose view cannot name a holder kept its seats")
	}
	if e.NativeHydrated(t.Context()) {
		t.Fatal("a node whose copy is wrong and whose view cannot name a holder claimed a seat")
	}

	// A NODE HOLDING NO DATA, on the same view, sheds.
	stateless := &Engine{dataView: view}
	stateless.remote.Store(&remoteNative{tracker: true})
	if ok, reason := stateless.serviceable(now()); ok ||
		!strings.Contains(reason, "cannot say who serves the estate") {
		t.Fatalf("a node holding no data kept its seats over a view that cannot route (%v, %q)",
			ok, reason)
	}
	// AND A COMPANY ENTIRELY ON VENDORS routes nothing, so nothing sheds.
	vendors := &Engine{dataView: view}
	vendors.remote.Store(&remoteNative{})
	if ok, reason := vendors.serviceable(now()); !ok {
		t.Fatalf("a company routing nothing shed its seats: %s", reason)
	}
}

// ADMISSION WAITS FOR THE PARTITION A SEAT CANNOT DO WITHOUT, under every
// layout: the tracker's CATALOGUE, which every create reads — the one
// partition under layout 0, the company space's under a divided layout — and
// the knowledge base's one partition where there is one. A layout that gives a
// native half nothing to wait for is refused rather than read as nothing to
// wait for, which would admit a seat onto a company nobody checked.
func TestSeatAdmissionWaitsForThePartitionsASeatCannotDoWithout(t *testing.T) {
	t.Parallel()
	company := statelog.PartitionID{Space: statelog.SpaceCompany}
	noCompany := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 4, Domains: []string{tracker.Domain{}.Name()}},
		{Space: statelog.SpacePages, Partitions: 2, Domains: []string{pages.Domain{}.Name()}},
	}}
	noPages := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 4, Domains: []string{tracker.Domain{}.Name()}},
		{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{tracker.Domain{}.Name()}},
	}}
	for _, c := range []struct {
		name             string
		layout           statelog.Layout
		runTracker, wiki bool
		want             []seatNeed
		unaddressed      bool
	}{
		{name: "layout 0, both halves", layout: LayoutZero(), runTracker: true, wiki: true,
			want: []seatNeed{{partition: statelog.EstatePartition, tracker: true, pages: true}}},
		{name: "layout 0, the wiki alone", layout: LayoutZero(), wiki: true,
			want: []seatNeed{{partition: statelog.EstatePartition, pages: true}}},
		{name: "layout 0, nothing native", layout: LayoutZero()},
		{name: "layout 1, both halves", layout: DefaultLayoutOne(), runTracker: true, wiki: true,
			want: []seatNeed{{partition: company, tracker: true}}},
		{name: "layout 1, the wiki alone", layout: DefaultLayoutOne(), wiki: true},
		{name: "no company space", layout: noCompany, runTracker: true, unaddressed: true},
		{name: "no pages log", layout: noPages, wiki: true, unaddressed: true},
	} {
		got, err := seatNeeds(c.layout, c.runTracker, c.wiki)
		switch {
		case c.unaddressed:
			if !errors.Is(err, estate.ErrUnaddressed) {
				t.Errorf("%s: seatNeeds = (%v, %v), want ErrUnaddressed", c.name, got, err)
			}
		case err != nil || !slices.Equal(got, c.want):
			t.Errorf("%s: seatNeeds = (%+v, %v), want %+v", c.name, got, err, c.want)
		}
	}
}
