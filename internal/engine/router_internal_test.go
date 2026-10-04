package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// localWith is a local estate judging its copy by read.
func localWith(e *Engine, now func() time.Time,
	read func(context.Context, *native) copyVerdict) *localEstate {
	return &localEstate{e: e, now: now, read: read}
}

// landed waits for the verdict read in flight, if there is one, to land.
func landed(l *localEstate) {
	l.mu.Lock()
	reading := l.slot.reading
	l.mu.Unlock()
	if reading != nil {
		<-reading
	}
}

// judged is For after the verdict it finds stale has been read again: the
// request that finds it stale answers from the verdict before it, and the one
// after the read lands answers from the new one.
func judged(ctx context.Context, l *localEstate) (estate.Backend, bool) {
	_, _ = l.For(ctx)
	landed(l)
	return l.For(ctx)
}

// sound is a copy that is not wrong and answers requests.
func sound(context.Context, *native) copyVerdict {
	return copyVerdict{answers: true}
}

// A DATA NODE'S COPY IS SERVED BEFORE ITS RUNTIME IS UP, with no halves, so
// every operation on it is answered "not here" and moves on rather than being
// refused as an estate nobody serves — and with its halves, answering, once
// the runtime is.
func TestTheLocalEstateServesItsCopyWithWhatItRuns(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{}}
	l := localWith(e, time.Now, sound)

	b, ok := l.For(t.Context())
	if !ok || b.Tracker != nil {
		t.Fatalf("a copy with no runtime = (%+v, %v), want served with no halves", b, ok)
	}
	e.native.Store(&native{trackerReader: &tracker.Reader{}})
	if b, ok = judged(t.Context(), l); !ok || b.Tracker == nil || b.Answers == nil ||
		!b.Answers(t.Context()) {
		t.Fatalf("a copy with a runtime = (%+v, %v), want its halves, answering", b, ok)
	}
}

// A COPY THAT IS WRONG GOES OUT OF SERVICE, AND THE NODE KEEPS ITS SEATS.
//
// A halted applier, an eviction, rows below the log, a checkpoint on another
// stream, a stalled prefix, a record held past its grace: this node's router
// no longer answers from its own copy — its seats' calls go to the other data
// nodes, and another node asking is told `out_of_service` — while
// serviceability, which is about routing, keeps every seat. It serves again
// the moment a reading finds the copy sound.
func TestAWrongCopyStopsServingAndTheSeatsStay(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	verdict := copyVerdict{answers: true}
	e := &Engine{backends: &Backends{}}
	e.native.Store(&native{trackerReader: &tracker.Reader{}})
	l := localWith(e, func() time.Time { return now },
		func(context.Context, *native) copyVerdict { return verdict })

	if _, ok := judged(t.Context(), l); !ok {
		t.Fatal("a sound copy is not served")
	}
	now = now.Add(servingRecheck)
	verdict = copyVerdict{fault: "tracker", answers: true}
	if _, ok := judged(t.Context(), l); ok {
		t.Fatal("a copy that is wrong is still served")
	}
	if ok, reason := e.SeatsServiceable(); !ok {
		t.Fatalf("a wrong copy shed the node's seats (%s) — it goes out of "+
			"service, and the seats read the estate from another data node", reason)
	}

	// A BLIP DOES NOT BRING IT BACK: a reading that reached no broker keeps
	// the fault it had.
	now = now.Add(servingRecheck)
	verdict = copyVerdict{refusal: statelog.RefuseBrokerUnreachable}
	if _, ok := judged(t.Context(), l); ok {
		t.Fatal("a reading that reached no broker put a wrong copy back into service")
	}
	now = now.Add(servingRecheck)
	verdict = copyVerdict{answers: true}
	if _, ok := judged(t.Context(), l); !ok {
		t.Fatal("a copy found sound again is still not served")
	}
}

// WHETHER A COPY ANSWERS IS READ AT MOST ONCE A RECHECK, and a read that could
// not reach the broker keeps the verdict before it — while a copy never judged
// answers nothing.
//
// The verdict reads every log's health from the broker, so read per request it
// would put those round trips in front of every tool call; and one blip must
// not send every request away from a copy that was answering them.
func TestACopysVerdictIsReadOncePerRecheckAndKeptThroughABlip(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	var reads atomic.Int32
	verdict := copyVerdict{answers: true}
	l := localWith(&Engine{}, func() time.Time { return now },
		func(context.Context, *native) copyVerdict {
			reads.Add(1)
			return verdict
		})
	n := &native{}
	fresh := func() copyVerdict {
		l.verdict(t.Context(), n)
		landed(l)
		return l.verdict(t.Context(), n)
	}

	// A COPY NEVER JUDGED, whose first read found no broker, answers
	// nothing.
	verdict = copyVerdict{refusal: statelog.RefuseBrokerUnreachable}
	if l.verdict(t.Context(), n).answers {
		t.Fatal("a copy nobody could measure answered")
	}
	now = now.Add(servingRecheck)
	verdict = copyVerdict{answers: true}
	if !fresh().answers {
		t.Fatal("a copy measured as answering did not")
	}
	for range 10 {
		l.verdict(t.Context(), n)
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
// its whole budget. So ONE read runs however many requests found the verdict
// stale, nobody but a request that finds the copy never judged waits on it, a
// request that stops waiting leaves when its own context ends — and
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
		func(context.Context, *native) copyVerdict {
			reads.Add(1)
			started <- struct{}{}
			<-release
			// SLOWER THAN THE RECHECK, as a broker mid-election is.
			clock.Add(int64(3 * servingRecheck))
			return copyVerdict{answers: true}
		})
	n := &native{}

	// NEVER JUDGED: every request waits on the ONE read, and a request
	// whose caller gives up leaves without it.
	const requests = 8
	var wg sync.WaitGroup
	answered := make(chan bool, requests)
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answered <- l.verdict(t.Context(), n).answers
		}()
	}
	<-started
	impatient, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	v, returned := within(time.Second, func() copyVerdict { return l.verdict(impatient, n) })
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
	l.verdict(t.Context(), n)
	landed(l)
	if got := reads.Load(); got != 1 {
		t.Fatalf("a verdict read slower than the recheck was stale when stored: %d reads", got)
	}

	// JUDGED BEFORE: a stale verdict is answered at once from the one
	// before it while the read that replaces it is in flight.
	clock.Add(int64(servingRecheck))
	stuck := make(chan struct{})
	l.read = func(context.Context, *native) copyVerdict {
		reads.Add(1)
		<-stuck
		return copyVerdict{answers: true}
	}
	answered2, returned := within(time.Second, func() bool {
		for range requests {
			if !l.verdict(t.Context(), n).answers {
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
	landed(l)
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
// fact — a node that can name no data node at all — on a clock it answers.
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

// dataNodeOver is a data node serving the estate from a copy judged by read,
// whose router routes by view.
func dataNodeOver(t *testing.T, view *coord.LeaseView, now func() time.Time,
	read func(context.Context, *native) copyVerdict) *Engine {

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

// A NODE THAT ANSWERS ITS SEATS FROM ITS OWN COPY ROUTES THEM WITHOUT THE
// VIEW — the router asks this node first and asks nobody where it answers — so
// a view gone unknown past the bound sheds none of its seats and withholds
// none of its claims. That is every data node whose copy is sound: a partial
// coordination fault that stopped the fleet listing and not the seat renewals
// used to release a single data node's every seat, and then refuse to claim
// them back, while its own copy served them all along. A node that must ask
// another data node still sheds on the same view, since its seats' calls have
// nowhere it can name to go.
func TestANodeServingItsSeatsItselfNeedsNoViewToKeepOrClaimThem(t *testing.T) {
	t.Parallel()
	view, now := staleView(t)
	var faulted atomic.Bool
	e := dataNodeOver(t, view, now, func(context.Context, *native) copyVerdict {
		if faulted.Load() {
			return copyVerdict{fault: "tracker", answers: true}
		}
		return copyVerdict{answers: true}
	})
	judged(t.Context(), e.local)

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
	e.local.slot = verdictSlot{}
	e.local.mu.Unlock()
	judged(t.Context(), e.local)
	if ok, _ := e.serviceable(now()); ok {
		t.Fatal("a node whose copy is wrong and whose view cannot name a data node kept its seats")
	}
	if e.NativeHydrated(t.Context()) {
		t.Fatal("a node whose copy is wrong and whose view cannot name a data node claimed a seat")
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

// BOTH HALVES THE COMPANY RUNS NATIVELY MUST BE SERVED BY THE COPY THAT WILL
// SERVE THE SEAT, because a seat's tools reach both: a node holding no data
// that admitted a seat on a data node running only the tracker would hand that
// seat a wiki whose every call is answered "not here" — and a node admitting
// on the wiki alone, a tracker the same. And a half the company runs on a
// vendor is not asked for at all, or a company with its pages on Confluence
// would never admit a seat anywhere.
//
// Each step moves the clock past the window [estate.Router.Serves] trusts a
// data node's answer for, so every step is a fresh ask of the fleet.
func TestASeatIsAdmittedOnlyWhereBothHalvesItRunsAreServed(t *testing.T) {
	t.Parallel()
	for _, step := range []struct {
		name           string
		runs           remoteNative
		tracker, pages bool
		want           bool
	}{
		{name: "the data node runs the wiki and not the tracker",
			runs: remoteNative{tracker: true, wiki: true}, tracker: false, pages: true},
		{name: "the data node runs the tracker and not the wiki",
			runs: remoteNative{tracker: true, wiki: true}, tracker: true, pages: false},
		{name: "the data node runs both",
			runs: remoteNative{tracker: true, wiki: true}, tracker: true, pages: true, want: true},
		{name: "the company's wiki is on a vendor",
			runs: remoteNative{tracker: true}, tracker: true, pages: false, want: true},
		{name: "the company's tracker is on a vendor",
			runs: remoteNative{wiki: true}, tracker: false, pages: true, want: true},
	} {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			e, answer := statelessOver(t)
			e.remote.Store(&step.runs)
			answer(step.tracker, step.pages)
			if got := e.NativeHydrated(t.Context()); got != step.want {
				t.Fatalf("a node running tracker=%v wiki=%v, asking a data node serving "+
					"tracker=%v pages=%v, admitted a seat: %v, want %v",
					step.runs.tracker, step.runs.wiki, step.tracker, step.pages, got, step.want)
			}
		})
	}

	// AND THE ANSWER IS ASKED AGAIN once its trust has run out: a data node
	// that comes to serve the half it lacked admits the next sweep's seat.
	e, answer := statelessOver(t)
	e.remote.Store(&remoteNative{tracker: true, wiki: true})
	answer(true, false)
	if e.NativeHydrated(t.Context()) {
		t.Fatal("a seat was admitted on a data node that does not run the wiki the company runs")
	}
	answer(true, true)
	if !e.NativeHydrated(t.Context()) {
		t.Fatal("a seat was withheld after the data node came to serve both halves")
	}
}

// statelessOver is a node holding no data whose router asks one data node,
// and a function that sets what that data node answers admission's ping with
// and moves the router's clock past the trust it holds an answer for.
func statelessOver(t *testing.T) (*Engine, func(tracker, pages bool)) {
	t.Helper()
	var clock atomic.Int64
	clock.Store(time.Unix(1_700_000_000, 0).UnixNano())
	asker := &pingAnswerer{}
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "agent-1", Queue: asker, Placement: oneDataNode{},
		Session: estate.NewSession(),
		Now:     func() time.Time { return time.Unix(0, clock.Load()) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{router: router}, func(tracker, pages bool) {
		asker.set(tracker, pages)
		clock.Add(int64(time.Minute))
	}
}

// oneDataNode is a fleet with one data node in it.
type oneDataNode struct{}

func (oneDataNode) Holders() ([]string, error) { return []string{"data-1"}, nil }
func (oneDataNode) Unanswered(string)          {}

// pingAnswerer is a data node answering admission's ping with the halves it is
// told it runs.
type pingAnswerer struct {
	mu     sync.Mutex
	answer []byte
}

func (p *pingAnswerer) set(tracker, pages bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answer = fmt.Appendf(nil, `{"node":"data-1","result":{"Tracker":%t,"Pages":%t}}`,
		tracker, pages)
}

func (p *pingAnswerer) Ask(context.Context, string, []byte, int) ([][]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return [][]byte{p.answer}, nil
}
