package jetstream

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

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

// THE DATA PATH IS PROVEN END TO END through a leaf's client and data link,
// counted in the meter's readings: (a) blackholed idle, refused within a probe
// period and a bound, never before a bound, the lease link standing; (b) a
// mebibyte crossing four bounds ahead of the probe, proven throughout; (c) two
// mebibytes blackholed six seconds in, proven until then and refused within
// two bounds; (d) a probe never answered while bulk moves, refused at the cap
// as a link that moves, never before.
//
// Mutant: the silence clause dropped — (b). Mutant: silence counted over the
// probe's age, not the bound — (c). Mutant: no cap — (d). Mutant: DataPath
// reads only the first hop — (a).
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
			case n > 2*boundReadings+int(pathEvery/meterPeriod):
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
			if err := path(); err != nil {
				t.Fatalf("%d readings into a crossing at the link's rate, the path is refused: %v",
					crossed, err)
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
			if err := path(); err != nil {
				t.Fatalf("%d readings into a crossing at the link's rate, the path is refused: %v", n, err)
			}
			return n >= before
		})
		p.link.Blackhole(server.DEFAULT_GLOBAL_ACCOUNT)
		t.Cleanup(func() { p.link.Mend(server.DEFAULT_GLOBAL_ACCOUNT) })
		readings(t, p.meter, "the path refused after its last byte", func(n int) bool {
			err := path()
			switch {
			case err == nil:
				if n > 2*boundReadings {
					t.Fatalf("%d readings after the link's last byte the path still reads proven", n)
				}
				return false
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
// (nats.go's, the same), and on a member, which crosses no link, none.
//
// Mutant: the tolerance left out — every cap is six seconds.
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
}
