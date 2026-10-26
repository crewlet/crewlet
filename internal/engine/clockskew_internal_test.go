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
// store took it moved `ahead` later, as a broker member whose clock stood that
// far ahead of this node's would have stamped it — the rows of other nodes a
// case writes included, so every row's instant is the one member's clock. While
// `blind` is set its listing of the register fails, as a store the node cannot
// reach answers.
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

// A BROKER MEMBER WHOSE CLOCK STANDS MORE THAN AN HOUR FROM THE FLEET'S FIRES
// `clock_skew` — measured by the running node's own position heartbeat off the
// register, and evaluated through the retention loop's own reading, report and
// gauge — AND THE ALARM STANDS FOR AS LONG AS THE HEARTBEAT IS BEATING.
//
// The broker cannot be given a clock of its own here, so the register stands
// in for one: the node's coordination store, wrapped to hand its row back
// stored an hour and a minute after the node stamped it — what a node reads of
// a member that far ahead of it. The node is the fleet's only data node, so the
// fleet's clock is its own and the clock the alarm names is the member's.
// Everything else is the running node's: the heartbeat that writes and reads
// the row, the measure it takes, the reading that carries it and the table
// that judges it. Fifty minutes is silent — the rule's own boundary is
// statelog.TestAClockOffTheFleetsRaisesClockSkew's, and the measure here also
// carries the beat's own work between its stamp and its write, which a loaded
// runner can stretch to seconds.
//
// A MEASURE IS KEPT IN BEATS, never by an age counted from the beat that took
// it. So a raised alarm stands — never cleared and raised again, its standing
// instant unmoved — at every evaluation while the next beat's listing is slow,
// whatever the instant, and across one beat that cannot read the register
// between two slow listings. Two beats in a row that cannot read drop it, and
// a clock back in time clears it.
//
// Mutation: leave the heartbeat's measure out of the reading (skewReading's
// call in retention.reading), or measure a row's stamp against its previous
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
		t.Fatal("a broker fifty minutes ahead of the fleet's clock raised clock_skew")
	}

	measured(time.Hour + time.Minute)
	report := evaluated(time.Now().UTC())
	fired, up := alarmOf(report, statelog.KindClockSkew)
	if !up {
		t.Fatalf("a broker an hour and a minute ahead of the fleet's clock raised no "+
			"clock_skew (raised: %v)", kindsOf(report))
	}
	if want := "the clock of the broker member leading the position register runs 1h 1m " +
		"ahead of the fleet's clock (this node's own"; !strings.Contains(fired.Detail, want) {
		t.Errorf("the alarm reads %q, want it to say %q", fired.Detail, want)
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

	// AND A CLOCK BACK IN TIME WITH THE FLEET'S CLEARS IT.
	register.blind.Store(false)
	measured(0)
	if alarmed(evaluated(time.Now().UTC()), statelog.KindClockSkew) {
		t.Error("a broker back in time with the fleet's clock still raises clock_skew")
	}
	if got := alarmGauge(t, e, statelog.KindClockSkew); got != 0 {
		t.Errorf("the alarm gauge for %s reads %v once the clock is back, want 0",
			statelog.KindClockSkew, got)
	}
}

// ON A RUNNING NODE, `clock_skew` NAMES THE CLOCK THAT MOVED: the node's own
// heartbeat reads the register — its own row and two peers' — and its own
// retention loop judges what it measured.
//
// The broker cannot be given a clock of its own here, so the register stands
// in for one: every row it hands back is stored `ahead` of this node's clock,
// as one member's clock stamps them all, and each peer's row is stamped by the
// peer's own clock as the case sets it. Three fleets of three data nodes, each
// read from this node:
//
//   - the member leading the register is a PEER whose clock runs ninety
//     minutes ahead: every row but that member's own is stored ninety minutes
//     after it was stamped. This node keeps the fleet's time, so it names the
//     member — never its own clock, which is what it said when it was measured
//     against the member alone;
//   - this node IS the member, its clock ninety minutes ahead of both peers':
//     the register stores every row by this node's clock, so its own row reads
//     in time. It names its own clock, where measured against the member alone
//     it read nothing while both peers blamed their own;
//   - this node's clock runs ninety minutes ahead, and the member is a peer
//     keeping the fleet's time: it names its own clock, and not the member's.
//
// Mutation: measure each node against the member alone (the median left out),
// and the first two fleets name the wrong clock.
func TestClockSkewNamesTheClockThatMovedOnARunningNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	register := &aheadRegister{}
	e, _ := aRunningNodeOver(t, nativeCleanupCompany, Options{},
		func(b *Backends) {
			register.wrappedFleet = b.Fleet
			b.Fleet = register
		})
	r := quietRetention(t, e)
	if r.state == nil || r.state.fleet != coord.Fleet(register) {
		t.Fatal("the node's heartbeat does not read the case's register, so this case is " +
			"not measuring what it names")
	}

	const drift = 90 * time.Minute
	const thisNode, member = "this node's clock", "the clock of the broker member"
	for _, c := range []struct {
		name string
		// ahead is the member's clock from this node's, and peers each peer's.
		ahead time.Duration
		peers map[string]time.Duration
		says  string
		never string
	}{{
		name:  "a peer leads the register, its clock ahead",
		ahead: drift,
		peers: map[string]time.Duration{"peer-a": drift, "peer-b": 0},
		says: "the clock of the broker member leading the position register runs 1h 30m " +
			"ahead of the fleet's clock (the median of 3 data nodes')",
		never: thisNode,
	}, {
		name:  "this node leads the register, its clock ahead",
		ahead: 0,
		peers: map[string]time.Duration{"peer-a": -drift, "peer-b": -drift},
		says: "this node's clock runs 1h 30m ahead of the fleet's clock (the median of 3 " +
			"data nodes'), and the clock of the broker member leading the position register " +
			"runs 1h 30m ahead of it",
	}, {
		name:  "a peer leads the register in time, this node's clock ahead",
		ahead: -drift,
		peers: map[string]time.Duration{"peer-a": -drift, "peer-b": -drift},
		says:  "this node's clock runs 1h 30m ahead of the fleet's clock (the median of 3 data nodes')",
		never: member,
	}} {
		// The peers' rows are written first, each stamped by its peer's clock,
		// then two of this node's beats: the first writes its row, the second
		// reads every row back.
		register.ahead.Store(int64(c.ahead))
		for peer, clock := range c.peers {
			if err := register.wrappedFleet.PutPositions(ctx, coord.NodePositions{
				NodeID: peer, At: time.Now().UTC().Add(clock)}); err != nil {
				t.Fatalf("%s: write %s's row: %v", c.name, peer, err)
			}
		}
		r.state.publishPositions(ctx)
		r.state.publishPositions(ctx)
		r.now = func() time.Time { return time.Now().UTC() }
		r.beat(ctx)
		report := r.Report(ctx)
		fired, up := alarmOf(report, statelog.KindClockSkew)
		if !up {
			t.Errorf("%s: raised no clock_skew (raised: %v)", c.name, kindsOf(report))
			continue
		}
		if !strings.Contains(fired.Detail, c.says) {
			t.Errorf("%s: the alarm reads %q, want it to say %q", c.name, fired.Detail, c.says)
		}
		if c.never != "" && strings.Contains(fired.Detail, c.never) {
			t.Errorf("%s: the alarm reads %q, naming %q, whose clock keeps the fleet's time",
				c.name, fired.Detail, c.never)
		}
	}
}

// THE CLOCK THAT MOVED IS THE ONE NAMED, on every node's reading of one
// register, judged by the alarm table.
//
// Each case is one register as the broker hands it back: every row stamped by
// its node's clock and stored by the clock of the member leading the register.
// The case the alarm got wrong when each node was measured against the member
// alone is the first: three data nodes on an embedded broker, whose member, a,
// has a clock ninety minutes ahead — a stamps and stores its own row by one
// clock, so it read nothing, while b and c each raised clock_skew about their
// own clocks. Against the fleet's clock — the median of the three — a raises
// it about its own clock, and b and c name the member.
//
// Mutation: measure each node against the member alone, take the mean for the
// median, read a row the broker stored far from this node's own, or judge
// whose row counts by the writer's stamp rather than the broker's instants,
// and a case below fails.
func TestClockSkewNamesTheClockThatMoved(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	// row is node's row written at the true instant `at`, stamped by node's
	// clock (`clock` off true time) and stored by the member's (`member` off).
	row := func(node string, clock, member, at time.Duration) coord.NodePositions {
		return coord.NodePositions{NodeID: node,
			At: base.Add(at + clock), StoredAt: base.Add(at + member)}
	}
	const drift = 90 * time.Minute
	const thisNode, member = "this node's clock", "the clock of the broker member"
	type view struct {
		says  []string // what the node's alarm says; none: it raises nothing
		never []string // what it never says
	}
	for name, c := range map[string]struct {
		rows  []coord.NodePositions
		views map[string]view
	}{
		"the member of an embedded broker, its clock ahead": {
			rows: []coord.NodePositions{
				row("a", drift, drift, 0), row("b", 0, drift, -3*time.Second),
				row("c", 0, drift, -7*time.Second),
			},
			views: map[string]view{
				"a": {says: []string{"this node's clock runs 1h 30m ahead of the fleet's clock " +
					"(the median of 3 data nodes'), and the clock of the broker member leading " +
					"the position register runs 1h 30m ahead of it"}},
				"b": {says: []string{"the clock of the broker member leading the position " +
					"register runs 1h 30m ahead of the fleet's clock (the median of 3 data nodes')"},
					never: []string{thisNode}},
				"c": {says: []string{"the clock of the broker member leading the position " +
					"register runs 1h 30m ahead of the fleet's clock"}, never: []string{thisNode}},
			},
		},
		"a node ahead, the member keeping the fleet's time": {
			rows: []coord.NodePositions{
				row("a", drift, 0, 0), row("b", 0, 0, -3*time.Second),
				row("c", 0, 0, -7*time.Second),
			},
			views: map[string]view{
				"a": {says: []string{"this node's clock runs 1h 30m ahead of the fleet's clock " +
					"(the median of 3 data nodes')"}, never: []string{member}},
				"b": {},
				"c": {},
			},
		},
		"a node behind, the member keeping the fleet's time": {
			rows: []coord.NodePositions{
				row("a", -drift, 0, 0), row("b", 0, 0, -3*time.Second),
				row("c", 0, 0, -7*time.Second), row("d", 0, 0, -9*time.Second),
			},
			views: map[string]view{
				"a": {says: []string{"this node's clock runs 1h 30m behind the fleet's clock " +
					"(the median of 4 data nodes')"}, never: []string{member}},
				"d": {},
			},
		},
		"an external member, its clock behind, every node in time": {
			rows: []coord.NodePositions{
				row("a", 0, -drift, 0), row("b", 0, -drift, -3*time.Second),
				row("c", 0, -drift, -7*time.Second),
			},
			views: map[string]view{
				"a": {says: []string{"the clock of the broker member leading the position " +
					"register runs 1h 30m behind the fleet's clock (the median of 3 data nodes')"},
					never: []string{thisNode}},
				"c": {says: []string{"the clock of the broker member leading the position " +
					"register runs 1h 30m behind"}, never: []string{thisNode}},
			},
		},
		"two nodes three hours apart, neither a majority": {
			rows: []coord.NodePositions{
				row("a", 3*time.Hour, 3*time.Hour, 0), row("b", 0, 3*time.Hour, -3*time.Second),
			},
			views: map[string]view{
				"a": {says: []string{"this node's clock runs 1h 30m ahead of the fleet's clock " +
					"(the median of 2 data nodes'), and the clock of the broker member leading " +
					"the position register runs 1h 30m ahead of it"}},
				"b": {says: []string{"this node's clock runs 1h 30m behind the fleet's clock " +
					"(the median of 2 data nodes'), and the clock of the broker member leading " +
					"the position register runs 1h 30m ahead of it"}},
			},
		},
		"two nodes under twice the bound apart": {
			rows: []coord.NodePositions{
				row("a", 110*time.Minute, 110*time.Minute, 0),
				row("b", 0, 110*time.Minute, -3*time.Second),
			},
			views: map[string]view{"a": {}, "b": {}},
		},
		"stopped nodes' rows, stored long before this node's": {
			rows: []coord.NodePositions{
				row("a", 0, 0, 0), row("b", 0, 0, -3*time.Second),
				row("gone-1", -3*time.Hour, 0, -10*time.Minute),
				row("gone-2", -3*time.Hour, 0, -20*time.Minute),
			},
			views: map[string]view{"a": {}, "b": {}},
		},
	} {
		for node, want := range c.views {
			s := &stateLog{nodeID: node}
			s.recordSkew(s.measureSkew(c.rows))
			var reading statelog.Reading
			s.skewReading(&reading)
			fired, up := alarmOf(statelog.Report{Alarms: statelog.Evaluate(reading)},
				statelog.KindClockSkew)
			if fires := len(want.says) > 0; up != fires {
				t.Errorf("%s, read by %s: raised clock_skew %v, want %v (it reads %q)",
					name, node, up, fires, fired.Detail)
				continue
			}
			for _, says := range want.says {
				if !strings.Contains(fired.Detail, says) {
					t.Errorf("%s, read by %s: the alarm reads %q, want it to say %q",
						name, node, fired.Detail, says)
				}
			}
			for _, never := range want.never {
				if strings.Contains(fired.Detail, never) {
					t.Errorf("%s, read by %s: the alarm reads %q, naming %q, whose clock keeps "+
						"the fleet's time", name, node, fired.Detail, never)
				}
			}
		}
	}

	// AND A REGISTER WITH NO ROW OF THIS NODE'S, OR THIS NODE'S WITH NO
	// INSTANT ON IT, MEASURES NOTHING.
	for name, rows := range map[string][]coord.NodePositions{
		"no row of this node's": {row("b", drift, 0, 0)},
		"this node's row with no instant from the store": {
			{NodeID: "a", At: base}, row("b", drift, 0, 0)},
	} {
		if measured := (&stateLog{nodeID: "a"}).measureSkew(rows); measured != nil {
			t.Errorf("%s: measured %+v, want nothing", name, *measured)
		}
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
			s.recordSkew(&skewMeasure{node: 2 * time.Hour, nodes: 3})
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
	if after.ClockSkew != 0 || after.BrokerClockSkew != 0 || after.ClockSkewNodes != 0 ||
		after.ClockSkewBound != 0 {
		t.Errorf("a heartbeat that has stopped still reports a measure of %v (the member %v, "+
			"%d nodes) against %v", after.ClockSkew, after.BrokerClockSkew, after.ClockSkewNodes,
			after.ClockSkewBound)
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
		{&skewMeasure{node: 2 * time.Hour}, 2 * time.Hour},
		{nil, 2 * time.Hour}, // one blink: kept
		{&skewMeasure{node: 3 * time.Hour}, 3 * time.Hour},
		{nil, 3 * time.Hour}, // a blink after a measure: kept again
		{nil, 0},             // the second in a row: dropped
		{nil, 0},
		{&skewMeasure{node: time.Hour}, time.Hour}, // measured again
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
