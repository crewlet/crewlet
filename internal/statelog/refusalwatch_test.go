package statelog_test

import (
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// testClock is a clock a case moves, so a run of refusals can span the
// interval that ends it without the case sleeping through it.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// watchedReader is a reader on the case's clock, over a node whose applier can
// be stalled on demand and whose barrier appends through appender.
func watchedReader(t *testing.T, clock *testClock, stalled *atomic.Bool,
	appender statelog.Appender, h *harness) *statelog.Reader {

	t.Helper()
	index, err := statelog.NewReadIndex(probeDomain{}, appender, probeEncode, h.gen.Load, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	r, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: probeDomain{},
		DB:     &stubStore{},
		Index:  index,
		Waiter: &stubWaiter{at: healthy().Position},
		Health: func() statelog.Health {
			s := healthy()
			// THE FLOOR IS READ ON THE CASE'S CLOCK, so moving the clock
			// ages nothing but what the case is about.
			s.Floor.ReadAt = clock.now()
			s.Stalled = stalled.Load()
			return s
		},
		Drain: func() float64 { return 2000 },
		Now:   clock.now,
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return r
}

func read(t *testing.T, r *statelog.Reader, level statelog.ReadLevel) error {
	t.Helper()
	_, err := r.Read(t.Context(), pointQuery(level), func(*sql.Tx) error { return nil })
	return err
}

// ONE FAULT-CLASS REFUSAL DOES NOT RAISE THE ALARM OR LATCH IT, AND A SUSTAINED
// RUN RAISES IT AT ITS REAL LENGTH — HOWEVER SLOWLY ITS READER ASKS.
//
// A refusal during a rolling restart is not a fault; refusals that go on past
// one reconcile interval are. So a run is how long reads at a level have been
// refused, first refusal to last: a single one is a run of nothing, and is
// forgotten once its level has been quiet for the run's expiry rather than
// reported for a day. A reader asking less often than once a reconcile
// interval — a screen on a twenty-second refresh — still forms a run.
//
// Mutation: never expire a quiet run and the single refusal still reads as
// current long after; expire runs at one reconcile interval and the twenty-
// second reader's run restarts on every refusal and never passes the
// threshold; measure a run to now and the single refusal's grows into an alarm.
func TestAFaultRefusalRunIsItsRealLengthAndEndsWhenQuiet(t *testing.T) {
	t.Parallel()
	clock := &testClock{at: time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)}
	var stalled atomic.Bool
	stalled.Store(true)
	h := newHarness(t)
	r := watchedReader(t, clock, &stalled, h.log, h)
	fires := func(since time.Duration) bool {
		for _, alarm := range statelog.Evaluate(statelog.Reading{RefusalsSince: since}) {
			if alarm.Kind == statelog.KindReadRefusals {
				return true
			}
		}
		return false
	}

	if err := read(t, r, statelog.ReadStale); err == nil {
		t.Fatal("a stale read on a stalled node was served, so this case is not " +
			"the shape it names")
	}
	if _, current := r.FaultRefusingSince(clock.now()); !current {
		t.Fatal("a fault-class refusal started no run")
	}
	clock.advance(coord.ReconcileInterval + time.Second)
	if since, _ := r.FaultRefusingSince(clock.now()); fires(since) {
		t.Errorf("one refusal an interval ago reads as a run %s long and raises "+
			"the alarm", since)
	}
	clock.advance(4 * coord.ReconcileInterval)
	if since, current := r.FaultRefusingSince(clock.now()); current {
		t.Errorf("one refusal a minute and more ago still reads as a current run "+
			"(%s) — the alarm would latch on it", since)
	}

	// A SUSTAINED FAULT MET BY A SLOW READER: refused every twenty seconds.
	for i := range 3 {
		if i > 0 {
			clock.advance(20 * time.Second)
		}
		if err := read(t, r, statelog.ReadStale); err == nil {
			t.Fatal("the stalled node served a read")
		}
	}
	since, current := r.FaultRefusingSince(clock.now())
	if !current || since != 40*time.Second {
		t.Fatalf("a run refused at 0, 20 and 40 seconds reads as (%s, %v), want 40s",
			since, current)
	}
	if !fires(since) {
		t.Errorf("read_refusals did not fire on a run %s long", since)
	}
}

// A RUN ENDS AT A READ SERVED AT ITS OWN LEVEL, AND ORDINARY LAG STARTS NONE.
//
// A barrier that did not commit refuses `linearizable` while `stale` goes on
// answering, so a stale read served says nothing about whether the barrier
// commits now — only a linearizable one served does.
//
// Mutation: end every run on any served read and the stale read ends the
// linearizable one.
func TestARunEndsOnlyAtAReadServedAtItsLevel(t *testing.T) {
	t.Parallel()
	clock := &testClock{at: time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)}
	var stalled atomic.Bool
	h := newHarness(t)
	// A BARRIER NOBODY COMMITS, until the case lets it through.
	barrier := &scripted{log: h.log, always: errors.New("nats: timeout")}
	r := watchedReader(t, clock, &stalled, barrier, h)

	var refusal *statelog.Refused
	if err := read(t, r, statelog.ReadLinearizable); !errors.As(err, &refusal) ||
		refusal.Code != statelog.RefuseNoQuorum {
		t.Fatalf("a linearizable read over a barrier nobody commits = %v, want no_quorum", err)
	}
	clock.advance(time.Second)
	if err := read(t, r, statelog.ReadStale); err != nil {
		t.Fatalf("a stale read = %v, want it served", err)
	}
	if _, current := r.FaultRefusingSince(clock.now()); !current {
		t.Error("a stale read served ended the linearizable run — it says nothing " +
			"about whether the barrier commits")
	}

	barrier.mu.Lock()
	barrier.always = nil
	barrier.mu.Unlock()
	if err := read(t, r, statelog.ReadLinearizable); err != nil {
		t.Fatalf("a linearizable read over a barrier that commits = %v", err)
	}
	if since, current := r.FaultRefusingSince(clock.now()); current {
		t.Errorf("a linearizable read served left the run current (%s)", since)
	}

	// ORDINARY LAG IS NOT A FAULT: a read bounded tighter than this node's
	// lag is refused and starts nothing.
	lagging := statelog.Query{
		Level: statelog.ReadStale, MaxLagSeq: 1,
		Scope: statelog.ScopeSet{Paths: []string{"object/a"}},
	}
	behind, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: probeDomain{}, DB: &stubStore{},
		Waiter: &stubWaiter{at: healthy().Position},
		Health: func() statelog.Health {
			s := healthy()
			lag := uint64(50)
			s.Lag = &lag
			s.Floor.ReadAt = clock.now()
			return s
		},
		Now: clock.now,
	})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := behind.Read(t.Context(), lagging, func(*sql.Tx) error { return nil }); !errors.As(err, &refusal) ||
		refusal.Code != statelog.RefuseTooStale {
		t.Fatalf("a read bounded tighter than the lag = %v, want too_stale", err)
	}
	if since, current := behind.FaultRefusingSince(clock.now()); current {
		t.Errorf("ordinary lag started a fault run (%s)", since)
	}
}
