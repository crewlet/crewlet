package engine

import (
	"context"
	"fmt"
	"strings"
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
type aheadRegister struct {
	wrappedFleet

	ahead atomic.Int64 // a time.Duration
	blind atomic.Bool
}

func (f *aheadRegister) Positions(ctx context.Context) ([]coord.NodePositions, error) {
	if f.blind.Load() {
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
// gauge.
//
// The broker cannot be given a clock of its own here, so the register stands
// in for one: the node's coordination store, wrapped to hand its row back
// stored an hour and a minute after the node stamped it — what a node that far
// behind its broker reads. Everything else is the running node's: the
// heartbeat that writes and reads the row, the measure it takes, the reading
// that carries it and the table that judges it. Fifty minutes is silent — the
// rule's own boundary is statelog.TestAClockOffTheBrokersRaisesClockSkew's,
// and the measure here also carries the beat's own work between its stamp and
// its write, which a loaded runner can stretch to seconds. A beat that cannot
// read the register keeps the last measure for two beats, and past them
// nothing is reported. A clock back in time clears it.
//
// Mutation: leave the heartbeat's measure out of the reading (skewReading's
// call in retention.reading), or measure the node's stamp against its previous
// stamp rather than the store's instant, and the alarm never fires; keep a
// measure past two beats, or drop it at the first beat that could not read,
// and the blind arm fails.
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

	before := time.Now().UTC()
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

	// A REGISTER THE NODE CANNOT READ. This beat waits out any beat of the
	// node's own still holding a reading from before, so every measure after
	// `before` is in by the time it returns, and none comes after it.
	register.blind.Store(true)
	r.state.publishPositions(ctx)
	blinded := time.Now().UTC()
	if !alarmed(evaluated(before.Add(statelog.PositionHeartbeat)), statelog.KindClockSkew) {
		t.Error("a beat that could not read the register dropped the measure the beat " +
			"before it took, so one blink of the store would clear a standing alarm")
	}
	if alarmed(evaluated(blinded.Add(2*statelog.PositionHeartbeat+time.Second)), statelog.KindClockSkew) {
		t.Error("a measure older than two beats, with no beat able to read the register " +
			"since, still raises clock_skew")
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

// kindsOf is every alarm a report raises, by kind, for a failure.
func kindsOf(report statelog.Report) []statelog.Kind {
	out := make([]statelog.Kind, 0, len(report.Alarms))
	for _, a := range report.Alarms {
		out = append(out, a.Kind)
	}
	return out
}
