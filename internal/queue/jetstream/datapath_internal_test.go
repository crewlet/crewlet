package jetstream

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
)

// pathEvery and pathBound are the end-to-end case's probe period and bound:
// a bound of the meter's span, so a bound is eight of its readings.
const (
	pathEvery = 500 * time.Millisecond
	pathBound = meterSpan
)

// pathFleet is a meter fleet across a first-in, first-out link at
// [meterRate], read moving at half of it, with a client of its leaf whose
// probe signals each answer; the probe is not started.
type pathFleet struct {
	meterFleet
	q        *Queue
	answered chan error
}

func newPathFleet(t *testing.T) pathFleet {
	t.Helper()
	f := newMeterFleet(t, func(t testing.TB, target string) *linktest.Link {
		return linktest.SharedFIFO(t, target, meterRate, meterInflight)
	})
	f.meter.setFloor(meterRate / 2)
	q := clientOf(t, f.leaf)
	p := pathFleet{meterFleet: f, q: q, answered: make(chan error, 1)}
	q.pathTiming = dataPathTiming{every: pathEvery, bound: pathBound,
		probe: func(ctx context.Context) error {
			_, err := q.js.AccountInfo(ctx)
			select {
			case p.answered <- err:
			default:
			}
			return err
		}}
	return p
}

// proven waits for the probe's next success.
func (p pathFleet) proven(t *testing.T) {
	t.Helper()
	bound := time.NewTimer(30 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	for {
		select {
		case err := <-p.answered:
			if err == nil {
				return
			}
		case <-bound.C:
			t.Fatal("the probe was never answered on a whole link")
		}
	}
}

// bulkUp sends n incompressible bytes from the leaf to the member, which
// answers on the channel it returns once they have all crossed; where the
// probe runs, just after an answer, so its next request crosses behind them.
func (p pathFleet) bulkUp(t *testing.T, n int, probing bool) <-chan struct{} {
	t.Helper()
	arrived := make(chan struct{}, 1)
	if _, err := p.down.Subscribe("datapath.bulk", func(*nats.Msg) { arrived <- struct{}{} }); err != nil {
		t.Fatal(err)
	}
	if err := p.down.Flush(); err != nil {
		t.Fatal(err)
	}
	until(t, "the member's interest across the link", func() bool {
		_, err := p.up.Request("meter.member.echo", nil, time.Second)
		return err == nil
	})
	if probing {
		p.proven(t)
	}
	if err := p.up.Publish("datapath.bulk", incompressible(t, n)); err != nil {
		t.Fatal(err)
	}
	return arrived
}

// readings calls f after each of the meter's readings, with how many it has
// taken since the call, until f answers true. A reading is the case's only
// clock: the bounds are counted in them.
func readings(t *testing.T, m *LinkMeter, what string, f func(n int) bool) {
	t.Helper()
	bound := time.NewTimer(2 * time.Minute) //fixedwait:liveness
	defer bound.Stop()
	for n := 1; ; n++ {
		next := m.nextReading()
		select {
		case <-next:
		case <-bound.C:
			t.Fatalf("still waiting for %s after %d readings", what, n)
		}
		if f(n) {
			return
		}
	}
}

// boundReadings is how many of the meter's readings a bound spans.
const boundReadings = int(pathBound / meterPeriod)

// refusedMoving reads path once, between two of the meter's readings, and
// reports whether it refused while the meter read the link moving over a
// bound, and its answer: the one refusal for silence the code never makes. A
// reading landing between the two leaves the case undecided, read as not.
// Judged by the meter, not by a count of readings, which a loaded runner's
// late ticks spread over a bound in fewer than its eight.
func refusedMoving(m *LinkMeter, path func() error) (bool, error) {
	next := m.nextReading()
	err := path()
	moving := m.Moving(pathBound)
	select {
	case <-next:
		return false, err
	default:
	}
	return err != nil && moving, err
}

// THE DATA PATH IS PROVEN END TO END through a leaf's client and data link,
// counted in the meter's readings: (a) blackholed idle, refused within a probe
// period and a bound, never before a bound, the lease link standing; (b) a
// mebibyte crossing four bounds ahead of the probe, never refused while the
// link reads moving (a relay a loaded runner stalls past a bound is a still
// link, rightly refused); (c) two mebibytes blackholed six seconds in, refused
// once the link reads still over a bound, never while it reads moving, and
// within two bounds; (d) a probe never answered while bulk moves, refused at
// the cap as a link that moves, never before.
//
// Mutant: the silence clause dropped — (b). Mutant: silence read over less
// than the bound — (c). Mutant: no cap — (d). Mutant: DataPath reads only the
// first hop — (a).
func TestTheDataPathIsProvenEndToEnd(t *testing.T) {
	t.Parallel()
	t.Run("(a) blackholed while idle", func(t *testing.T) {
		t.Parallel()
		p := newPathFleet(t)
		leasesOf(t, p.q)
		path := p.q.ProveDataPath()
		p.proven(t)
		p.link.Blackhole(server.DEFAULT_GLOBAL_ACCOUNT)
		t.Cleanup(func() { p.link.Mend(server.DEFAULT_GLOBAL_ACCOUNT) })
		readings(t, p.meter, "the blackholed path refused", func(n int) bool {
			err := path()
			switch {
			case err == nil:
				return false
			case n < boundReadings-1:
				t.Fatalf("the path was refused %d readings after the blackhole, under a bound's %d: %v",
					n, boundReadings, err)
			case n > boundReadings+2*int(pathEvery/meterPeriod)+4:
				t.Fatalf("the path was refused only %d readings after the blackhole: %v", n, err)
			case !errors.Is(err, ErrDataPathUnproven) || !strings.Contains(err.Error(), "carried nothing"):
				t.Fatalf("the blackholed path was refused for %v, want its silence", err)
			}
			return true
		})
		if err := p.q.LeaseLink(); err != nil {
			t.Errorf("the lease link, not blackholed, reads %v", err)
		}
	})

	t.Run("(b) a mebibyte crossing", func(t *testing.T) {
		t.Parallel()
		p := newPathFleet(t)
		path := p.q.ProveDataPath()
		arrived := p.bulkUp(t, 1<<20, true)
		crossed := 0
		readings(t, p.meter, "the mebibyte across", func(int) bool {
			select {
			case <-arrived:
				return true
			default:
			}
			crossed++
			if moving, err := refusedMoving(p.meter, path); moving {
				t.Fatalf("%d readings into a crossing, the path is refused while its link reads "+
					"moving: %v", crossed, err)
			}
			return false
		})
		if crossed < 3*boundReadings {
			t.Fatalf("the mebibyte crossed in %d readings, under three bounds' %d: the probe was "+
				"never held past its bound", crossed, 3*boundReadings)
		}
	})

	t.Run("(c) blackholed mid-crossing", func(t *testing.T) {
		t.Parallel()
		p := newPathFleet(t)
		path := p.q.ProveDataPath()
		p.bulkUp(t, 2<<20, true)
		const before = 3 * boundReadings
		readings(t, p.meter, "six seconds of the crossing", func(n int) bool {
			if moving, err := refusedMoving(p.meter, path); moving {
				t.Fatalf("%d readings into a crossing, the path is refused while its link reads "+
					"moving: %v", n, err)
			}
			return n >= before
		})
		p.link.Blackhole(server.DEFAULT_GLOBAL_ACCOUNT)
		t.Cleanup(func() { p.link.Mend(server.DEFAULT_GLOBAL_ACCOUNT) })
		readings(t, p.meter, "the path refused after its last byte", func(n int) bool {
			moving, err := refusedMoving(p.meter, path)
			switch {
			case err == nil:
				if n > 2*boundReadings {
					t.Fatalf("%d readings after the link's last byte the path still reads proven", n)
				}
				return false
			case moving:
				t.Fatalf("the path was refused %d readings after the link's last byte, while its "+
					"bound still read moving: %v", n, err)
			case !strings.Contains(err.Error(), "carried nothing"):
				t.Fatalf("the path blackholed mid-crossing was refused for %v, want its silence", err)
			}
			return true
		})
	})

	t.Run("(d) never answered while bulk moves", func(t *testing.T) {
		t.Parallel()
		p := newPathFleet(t)
		const capReadings = 2 * boundReadings
		arrived := p.bulkUp(t, 2<<20, false)
		until(t, "the bulk moving the link", func() bool { return p.meter.Moving(pathBound) })
		p.q.pathTiming.cap = 2 * pathBound
		p.q.pathTiming.probe = func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}
		path := p.q.ProveDataPath()
		readings(t, p.meter, "the path refused at its cap", func(n int) bool {
			select {
			case <-arrived:
				t.Fatalf("the bulk crossed before the path was refused (%d readings)", n)
			default:
			}
			err := path()
			switch {
			case err == nil:
				return false
			case n < capReadings-1:
				t.Fatalf("a path whose link moves was refused %d readings in, under the cap's %d: %v",
					n, capReadings, err)
			case !strings.Contains(err.Error(), "moves"):
				t.Fatalf("a path whose link moves was refused for %v, want the cap", err)
			}
			return true
		})
	})
}

// THE DATA PATH'S CAP IS ITS LINK'S OWN TOLERANCE, read from the options in
// force, a probe period and a read term on: on a leaf the leaf server's pings
// (nats-server's two minutes and two out), on an external dial the client's
// (nats.go's, the same), and on a member, which crosses no link, none. Pings
// set otherwise are read as set, a leaf's leaf-node ones over its server's.
//
// Mutant: the tolerance left out — every cap is six seconds. Mutant: the
// defaults hard-coded, or the leaf-node pings ignored — the set rows.
func TestTheDataPathsCapIsItsLinksOwnTolerance(t *testing.T) {
	t.Parallel()
	f := newMeterFleet(t, linktest.Lagging)
	external := newQueueWith(t, Config{URL: externaltest.Start(t, queue.MaxPayloadBytes).URL(),
		Token: externaltest.Token})
	beyond := dataPathProbeEvery + jsprovision.ReadTerm
	for _, c := range []struct {
		name string
		q    *Queue
		want time.Duration
	}{
		{"a leaf", clientOf(t, f.leaf), server.DEFAULT_PING_MAX_OUT*server.DEFAULT_PING_INTERVAL + beyond},
		{"an external dial", external, nats.DefaultMaxPingOut*nats.DefaultPingInterval + beyond},
		{"a member", clientOf(t, f.member), beyond},
	} {
		if got := c.q.dataPathTiming().cap; got != c.want {
			t.Errorf("%s's data path is capped at %v, want %v", c.name, got, c.want)
		}
	}

	remotes := server.LeafNodeOpts{Remotes: []*server.RemoteLeafOpts{{}}}
	leafPings := remotes
	leafPings.PingInterval, leafPings.MaxPingsOut = 5*time.Second, 4
	for _, c := range []struct {
		name string
		opts server.Options
		want time.Duration
	}{
		{"a leaf's server pings", server.Options{PingInterval: 7 * time.Second, MaxPingsOut: 3,
			LeafNode: remotes}, 21 * time.Second},
		{"a leaf's leaf-node pings", server.Options{PingInterval: 7 * time.Second, MaxPingsOut: 3,
			LeafNode: leafPings}, 20 * time.Second},
		{"a member's pings", server.Options{PingInterval: 7 * time.Second, MaxPingsOut: 3}, 0},
	} {
		if got := leafPingTolerance(&c.opts); got != c.want {
			t.Errorf("%s: a link tolerates %v without a pong, want %v", c.name, got, c.want)
		}
	}
	dialled := &Queue{nc: &nats.Conn{Opts: nats.Options{PingInterval: 9 * time.Second, MaxPingsOut: 4}}}
	if got := dialled.linkTolerance(); got != 36*time.Second {
		t.Errorf("an external dial pinging every 9s, 4 out, tolerates %v without a pong, want 36s", got)
	}
}

// startPath starts a data path on timing with no link to meter, which never
// moves, its first hop up, proven now; stopped with the case, if not before.
func startPath(t *testing.T, timing dataPathTiming) *dataPath {
	t.Helper()
	d := startDataPath(timing, func() error { return nil }, nil, slog.New(slog.DiscardHandler), time.Now())
	t.Cleanup(d.stop)
	return d
}

// signalled waits for c within a liveness bound, failing the case with what.
func signalled[T any](t *testing.T, c chan T, what string) T {
	t.Helper()
	bound := time.NewTimer(30 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	select {
	case v := <-c:
		return v
	case <-bound.C:
		t.Fatalf("waited 30s for %s", what)
	}
	var zero T
	return zero
}

// A PROBE NOBODY ANSWERS IS ASKED AGAIN past its bound on a link carrying
// nothing, as a member drops a request while its metadata group has no leader:
// let go no sooner than its bound, its age kept, so the path reads refused for
// its silence until the next probe is answered, and proven once it is.
//
// Mutant: the abandon deleted — the dropped probe is waited out to its cap.
// Mutant: the next ask restarts the age — the path reads proven, still silent.
func TestADroppedProbeIsAskedAgainPastItsBound(t *testing.T) {
	t.Parallel()
	// In a bubble, the drop and the ask read one clock, so the age is exact.
	synctest.Test(t, func(t *testing.T) {
		const bound = 100 * time.Millisecond
		type retry struct {
			asked   time.Time
			verdict error
		}
		var calls atomic.Int32
		self := make(chan *dataPath, 1)
		dropped, retried := make(chan time.Time, 1), make(chan retry, 1)
		answered := make(chan struct{})
		d := startPath(t, dataPathTiming{every: 10 * time.Millisecond, bound: bound,
			notAvailable: time.Hour, cap: time.Hour,
			probe: func(ctx context.Context) error {
				switch calls.Add(1) {
				case 1:
					<-ctx.Done()
					dropped <- time.Now()
					return ctx.Err()
				case 2:
					d := <-self
					d.mu.Lock()
					asked := d.asked
					d.mu.Unlock()
					retried <- retry{asked, d.verdict(time.Now())}
				case 3:
					close(answered)
				}
				return nil
			}})
		self <- d
		let := signalled(t, dropped, "the dropped probe to be let go")
		r := signalled(t, retried, "the probe asked again")
		if age := let.Sub(r.asked); age < bound {
			t.Errorf("the dropped probe was let go %v after it was asked, under its %v bound", age, bound)
		}
		if !errors.Is(r.verdict, ErrDataPathUnproven) || !strings.Contains(r.verdict.Error(), "carried nothing") {
			t.Errorf("asked again past its bound with nothing heard, the path read %v, want its silence", r.verdict)
		}
		signalled(t, answered, "the probe asked again to be answered")
		if err := d.verdict(time.Now()); err != nil {
			t.Errorf("once the probe asked again was answered, the path read %v", err)
		}
	})
}

// "NOT AVAILABLE" REFUSES THE PATH ONLY PAST ITS BUDGET with no success: a
// member answers so while its metadata group elects a leader, which a rolling
// restart makes routine. Refused once the answer has stood the budget since
// the last success, and proven again by the next.
//
// Mutant: refused on the first "not available" — refused inside the budget.
// Mutant: a success leaves the answer standing — refused a budget after it.
func TestNotAvailableRefusesThePathOnlyPastItsBudget(t *testing.T) {
	t.Parallel()
	const budget, never = time.Hour, 2 * time.Hour
	notAvailable := &jetstream.APIError{Code: 503, ErrorCode: 10008,
		Description: "JetStream system temporarily unavailable"}
	var healed atomic.Bool
	var calls, successes atomic.Int32
	second, recorded := make(chan struct{}), make(chan struct{})
	d := startPath(t, dataPathTiming{every: 10 * time.Millisecond, bound: never,
		notAvailable: budget, cap: never,
		probe: func(context.Context) error {
			if calls.Add(1) == 2 {
				close(second)
			}
			if !healed.Load() {
				return notAvailable
			}
			// One at a time: a second success asked means the first is recorded.
			if successes.Add(1) == 2 {
				close(recorded)
			}
			return nil
		}})
	d.mu.Lock()
	proven := d.proven
	d.mu.Unlock()
	signalled(t, second, "a probe after the first \"not available\"")
	if err := d.verdict(proven.Add(budget - time.Millisecond)); err != nil {
		t.Errorf("answered \"not available\" inside its budget, the path read %v", err)
	}
	err := d.verdict(proven.Add(budget))
	if !errors.Is(err, ErrDataPathUnproven) || !strings.Contains(err.Error(), "not available") {
		t.Errorf("answered \"not available\" for its whole budget, the path read %v", err)
	}
	healed.Store(true)
	signalled(t, recorded, "a success to be recorded")
	d.stop()
	d.mu.Lock()
	proven = d.proven
	d.mu.Unlock()
	if err := d.verdict(proven.Add(budget)); err != nil {
		t.Errorf("a budget after a success, the path still read %v", err)
	}
}
