package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// aheadRegister is the coordination store of a node whose broker's clock runs
// ahead of its own: every positions row it hands back carries the instant the
// store took it moved `ahead` later, as a broker whose clock stood that far
// ahead would have stamped it. While `blind` is set its listing of the register
// fails, as a store the node cannot reach answers.
//
// A LISTING CAN ALSO BE HELD OR FAILED ON ITS OWN, by the context it is asked
// under ([listingHeld], [listingBlind]): a case runs the beat it means to slow
// down or blind under such a context, while the node's own heartbeat and the
// alarm reading's listings go through as they are.
type aheadRegister struct {
	wrappedFleet

	ahead atomic.Int64 // a time.Duration
	blind atomic.Bool
}

// listingHeld is the context key of a [listingGate] the listing asked under it
// waits on: a listing of the register as slow as the case wants.
type listingHeld struct{}

// listingBlind is the context key whose presence fails the listing asked under
// it, as a register this node could not reach for that one beat.
type listingBlind struct{}

// listingGate holds the first listing asked under its context until released,
// and says when that listing has begun.
type listingGate struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func newListingGate() *listingGate {
	return &listingGate{entered: make(chan struct{}), release: make(chan struct{})}
}

// hold is the listing's wait: the first listing asked under the gate's context
// says it has begun and waits for the release, and any later one in the same
// beat goes through.
func (g *listingGate) hold(ctx context.Context) {
	g.once.Do(func() {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
		}
	})
}

func (f *aheadRegister) Positions(ctx context.Context) ([]coord.NodePositions, error) {
	if gate, held := ctx.Value(listingHeld{}).(*listingGate); held {
		gate.hold(ctx)
	}
	if f.blind.Load() || ctx.Value(listingBlind{}) != nil {
		return nil, fmt.Errorf("%w: the case's register is blind", coord.ErrUnavailable)
	}
	rows, err := f.wrappedFleet.Positions(ctx)
	ahead := time.Duration(f.ahead.Load())
	for i := range rows {
		rows[i].StoredAt = rows[i].StoredAt.Add(ahead)
	}
	return rows, err
}

// A NODE WHOSE CLOCK STANDS MORE THAN AN HOUR FROM ITS BROKER'S FIRES
// `clock_skew` — measured by the running node's own position heartbeat off its
// own row, and evaluated through the retention loop's own reading, report and
// gauge — AND THE ALARM STANDS FOR AS LONG AS THE HEARTBEAT IS BEATING.
//
// The broker cannot be given a clock of its own here, so the register stands
// in for one: the node's coordination store, wrapped to hand its row back
// stored an hour and a minute after the node stamped it — what a node that far
// behind its broker reads. Everything else is the running node's: the
// heartbeat that writes and reads the row, the measure it takes, the reading
// that carries it and the table that judges it. Fifty minutes is silent — the
// rule's own boundary is statelog.TestAClockOffTheBrokersRaisesClockSkew's,
// and the measure here also carries the beat's own work between its stamp and
// its write, which a loaded runner can stretch to seconds.
//
// A MEASURE IS KEPT IN BEATS, never by an age counted from the beat that took
// it. So a raised alarm stands — never cleared and raised again, its standing
// instant unmoved — at every evaluation while the next beat's listing is slow,
// whatever the instant, and across one beat that cannot read the register
// between two slow listings. Two beats in a row that cannot read drop it, and
// a clock back in time clears it.
//
// Mutation: leave the heartbeat's measure out of the reading (skewReading's
// call in retention.reading), or measure the node's stamp against its previous
// stamp rather than the store's instant, and the alarm never fires; drop a
// measure by its age from the beat that took it, as the alarm once did, and
// the slow arms fail; drop it at the first beat that could not read, and the
// arm between two slow listings fails; keep it past two, and the blind arm
// fails.
func TestTheClockSkewAlarmFiresOnARunningNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	register := &aheadRegister{}
	e, _ := aRunningNodeOver(t, nativeCleanupCompany, Options{Metrics: rec},
		func(b *Backends) {
			register.wrappedFleet = b.Fleet
			b.Fleet = register
		})
	r := quietRetention(t, e)
	if r.state == nil || r.state.fleet != coord.Fleet(register) || r.fleet != coord.Fleet(register) {
		t.Fatal("the node's heartbeat or its retention loop does not read the case's register, " +
			"so this case is not measuring what it names")
	}

	// measured runs the heartbeat with the broker `ahead`: TWO BEATS, the
	// first writing this node's row where no beat has yet, the second reading
	// it back. A beat the node's own loop runs meanwhile is serialised with
	// these, so the last measure is one taken with `ahead` in force.
	measured := func(ahead time.Duration) {
		register.ahead.Store(int64(ahead))
		r.state.publishPositions(ctx)
		r.state.publishPositions(ctx)
	}
	// evaluated runs the alarm table at `at`, as the retention loop's beat
	// does, and answers the report every surface reads.
	evaluated := func(at time.Time) statelog.Report {
		r.now = func() time.Time { return at }
		r.beat(ctx)
		return r.Report(ctx)
	}

	measured(50 * time.Minute)
	if alarmed(evaluated(time.Now().UTC()), statelog.KindClockSkew) {
		t.Fatal("a node fifty minutes behind its broker's clock raised clock_skew")
	}

	measured(time.Hour + time.Minute)
	report := evaluated(time.Now().UTC())
	fired, up := alarmOf(report, statelog.KindClockSkew)
	if !up {
		t.Fatalf("a node an hour and a minute behind its broker's clock raised no "+
			"clock_skew (raised: %v)", kindsOf(report))
	}
	if !strings.Contains(fired.Detail, "stands 1h 1m from its broker's, past the 1h") {
		t.Errorf("the alarm reads %q, want the measure and the bound it is past", fired.Detail)
	}
	if got := alarmGauge(t, e, statelog.KindClockSkew); got != 1 {
		t.Errorf("the alarm gauge for %s reads %v, want 1", statelog.KindClockSkew, got)
	}

	// THE ALARM STANDS, never cleared and raised again: up at the evaluation,
	// and up since the instant it was first raised.
	since := clockSkewSince(t, r)
	stands := func(what string, at time.Time) {
		t.Helper()
		report := evaluated(at)
		if !alarmed(report, statelog.KindClockSkew) {
			t.Errorf("%s: clock_skew is down (raised: %v), so a standing alarm was "+
				"cleared while the heartbeat was still beating", what, kindsOf(report))
			return
		}
		if got := clockSkewSince(t, r); !got.Equal(since) {
			t.Errorf("%s: clock_skew has stood since %s, want %s: it was cleared and raised "+
				"again", what, got, since)
		}
		if got := alarmGauge(t, e, statelog.KindClockSkew); got != 1 {
			t.Errorf("%s: the alarm gauge for %s reads %v, want 1", what, statelog.KindClockSkew, got)
		}
	}

	// A SLOW LISTING: the beat after a measure takes longer to read the
	// register than two intervals, so the next measure lands more than two
	// beats after the instant the last one's beat began.
	began := time.Now().UTC()
	r.state.publishPositions(ctx)
	slow := slowBeat(ctx, t, r.state)
	for _, after := range []time.Duration{
		2*statelog.PositionHeartbeat + time.Second,
		3 * statelog.PositionHeartbeat,
		6 * statelog.PositionHeartbeat,
	} {
		stands(fmt.Sprintf("%s after the last measure's beat began, its successor's listing "+
			"still running", after), began.Add(after))
	}
	slow.finish()
	stands("once the slow listing returned", began.Add(6*statelog.PositionHeartbeat+time.Second))

	// ONE BEAT THAT CANNOT READ THE REGISTER, BETWEEN TWO SLOW LISTINGS: the
	// blink the measure is kept through, with the next measure late too.
	began = time.Now().UTC()
	slowBeat(ctx, t, r.state).finish()
	r.state.publishPositions(context.WithValue(ctx, listingBlind{}, true))
	stands("after one beat that could not read the register",
		began.Add(statelog.PositionHeartbeat+time.Second))
	slow = slowBeat(ctx, t, r.state)
	for _, after := range []time.Duration{
		2*statelog.PositionHeartbeat + time.Second,
		3 * statelog.PositionHeartbeat,
		4 * statelog.PositionHeartbeat,
	} {
		stands(fmt.Sprintf("%s after the last measure's beat began, one beat blind and the "+
			"next one's listing still running", after), began.Add(after))
	}
	slow.finish()
	stands("once the second slow listing returned", began.Add(4*statelog.PositionHeartbeat+time.Second))

	// TWO BEATS IN A ROW THAT CANNOT READ THE REGISTER drop the measure:
	// every listing fails, the node's own beats' included, so the misses only
	// add up.
	register.blind.Store(true)
	r.state.publishPositions(ctx)
	r.state.publishPositions(ctx)
	if alarmed(evaluated(time.Now().UTC()), statelog.KindClockSkew) {
		t.Error("two beats in a row that could not read the register left the measure " +
			"standing, so a heartbeat that cannot read still raises clock_skew")
	}

	// AND A CLOCK BACK IN TIME WITH ITS BROKER'S CLEARS IT.
	register.blind.Store(false)
	measured(0)
	if alarmed(evaluated(time.Now().UTC()), statelog.KindClockSkew) {
		t.Error("a node back in time with its broker's clock still raises clock_skew")
	}
	if got := alarmGauge(t, e, statelog.KindClockSkew); got != 0 {
		t.Errorf("the alarm gauge for %s reads %v once the clock is back, want 0",
			statelog.KindClockSkew, got)
	}
}

// A HEARTBEAT THAT HAS STOPPED HOLDS NO CLOCK MEASURE: the measure stands until
// the heartbeat drops it, and once its loop has ended nothing will.
//
// Mutation: take the drop out of the heartbeat's loop, and the measure the last
// beat took is reported after the loop ended.
func TestAHeartbeatThatStoppedHoldsNoClockMeasure(t *testing.T) {
	t.Parallel()
	s := &stateLog{heartbeatNudge: make(chan struct{}, 1)}
	ctx, stop := context.WithCancel(t.Context())
	measured := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.beat(ctx, time.Hour, func(context.Context) {
			s.publishing.Lock()
			s.recordSkew(&skewMeasure{off: 2 * time.Hour})
			s.publishing.Unlock()
			measured <- struct{}{}
		})
	}()
	select {
	case <-measured:
	case <-time.After(10 * time.Second):
		t.Fatal("waited 10s for the heartbeat's first beat")
	}
	var held statelog.Reading
	s.skewReading(&held)
	if held.ClockSkew != 2*time.Hour {
		t.Fatalf("a beating heartbeat's measure reads %v, want 2h: this case measures nothing",
			held.ClockSkew)
	}
	stop()
	<-done
	var after statelog.Reading
	s.skewReading(&after)
	if after.ClockSkew != 0 || after.ClockSkewBound != 0 {
		t.Errorf("a heartbeat that has stopped still reports a measure of %v against %v",
			after.ClockSkew, after.ClockSkewBound)
	}
}

// A MEASURE STANDS UNTIL TWO BEATS IN A ROW CANNOT RENEW IT: one beat that
// measured nothing keeps the last measure, any measure renews it, and the
// second miss in a row drops it.
//
// Mutation: drop the measure at the first miss, keep it past the second, or
// leave the misses counted across a measure, and a step below fails.
func TestAClockMeasureStandsUntilTwoBeatsInARowCannotRenewIt(t *testing.T) {
	t.Parallel()
	s := &stateLog{}
	held := func() time.Duration {
		var r statelog.Reading
		s.skewReading(&r)
		return r.ClockSkew
	}
	for i, step := range []struct {
		measured *skewMeasure
		want     time.Duration
	}{
		{nil, 0}, // nothing measured yet
		{&skewMeasure{off: 2 * time.Hour}, 2 * time.Hour},
		{nil, 2 * time.Hour}, // one blink: kept
		{&skewMeasure{off: 3 * time.Hour}, 3 * time.Hour},
		{nil, 3 * time.Hour}, // a blink after a measure: kept again
		{nil, 0},             // the second in a row: dropped
		{nil, 0},
		{&skewMeasure{off: time.Hour}, time.Hour}, // measured again
	} {
		s.recordSkew(step.measured)
		if got := held(); got != step.want {
			t.Errorf("after beat %d (measured %v) the reading holds %v, want %v",
				i+1, step.measured != nil, got, step.want)
		}
	}
}

// slowHeartbeat is one beat of the heartbeat whose listing of the register is
// held until finish releases it.
type slowHeartbeat struct {
	t    *testing.T
	gate *listingGate
	done chan struct{}
}

// slowBeat starts one beat of s under ctx with its listing of the register
// held, and answers once the listing has begun.
func slowBeat(ctx context.Context, t *testing.T, s *stateLog) *slowHeartbeat {
	t.Helper()
	b := &slowHeartbeat{t: t, gate: newListingGate(), done: make(chan struct{})}
	held := context.WithValue(ctx, listingHeld{}, b.gate)
	go func() {
		defer close(b.done)
		s.publishPositions(held)
	}()
	select {
	case <-b.gate.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("waited 30s for the slow beat's listing to begin")
	}
	return b
}

// finish releases the beat's listing and waits for the beat to end.
func (b *slowHeartbeat) finish() {
	b.t.Helper()
	close(b.gate.release)
	select {
	case <-b.done:
	case <-time.After(30 * time.Second):
		b.t.Fatal("waited 30s for the slow beat to end once its listing was released")
	}
}

// clockSkewSince is the instant the node's tracker raised clock_skew, which
// stays put for as long as the alarm stands.
func clockSkewSince(t *testing.T, r *retention) time.Time {
	t.Helper()
	marks, observed := r.alarms.Marks()
	if !observed {
		t.Fatal("the node's alarm table has not been evaluated")
	}
	for _, m := range marks {
		if m.Kind == string(statelog.KindClockSkew) {
			return m.Since
		}
	}
	t.Fatal("clock_skew is not standing")
	return time.Time{}
}

// kindsOf is every alarm a report raises, by kind, for a failure.
func kindsOf(report statelog.Report) []statelog.Kind {
	out := make([]statelog.Kind, 0, len(report.Alarms))
	for _, a := range report.Alarms {
		out = append(out, a.Kind)
	}
	return out
}
