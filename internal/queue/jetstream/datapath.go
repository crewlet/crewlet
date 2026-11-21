package jetstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
)

// ErrDataPathUnproven is what [Queue.DataPath] wraps once the probe of this
// node's data path has gone unanswered past its bound, or been answered "not
// available" for too long: the fleet's JetStream may be out of this node's
// reach even though its first hop is up.
var ErrDataPathUnproven = errors.New("jetstream: this node's data path to the fleet's JetStream is unproven")

// dataPathProbeEvery is how often the data path is probed: once a heartbeat
// at the shortest lease config allows ([leaseDeadAfter]), so at least once a
// heartbeat at every TTL.
const dataPathProbeEvery = leaseDeadAfter

// dataPathBound is how long one probe may go unanswered across a link that
// carries nothing before the path is refused: the request term and its
// reply's crossing at the floor, as every request across a link is given
// (#1005's rule): about 5.03 s, the reply being under a kibibyte.
var dataPathBound = jsapi.RequestTerm + jsapi.Crossing(0, objstore.MiBPace)

// dataPathTiming is what the data path's verdict waits on. The zero value of
// each field is production's ([Queue.dataPathTiming]); a field so this
// package's tests reach a refusal without waiting out minutes.
type dataPathTiming struct {
	// every is how often a probe is sent ([dataPathProbeEvery]).
	every time.Duration
	// bound is how long a probe may go unanswered while the link carries
	// nothing ([dataPathBound]).
	bound time.Duration
	// notAvailable is how long the path may answer "not available" with no
	// success ([jsprovision.LookupBudget]).
	notAvailable time.Duration
	// cap is how long a probe may go unanswered while the link moves: the
	// link's own stale tolerance, a probe period and a read term
	// ([Queue.linkTolerance]).
	cap time.Duration
	// probe asks the fleet's JetStream once: an AccountInfo on the queue's
	// own client.
	probe func(context.Context) error
}

// dataPath is a queue's proof that the fleet's JetStream answers on the path
// its work takes (package doc, "A node's leases ride a link of their own"): one
// probe in flight at a time, sent every timing.every, and the verdict
// [Queue.DataPath] reads with no round trip.
type dataPath struct {
	timing dataPathTiming
	// firstHop is the queue's connection and, on a leaf, its data link
	// ([Queue.Link]).
	firstHop func() error
	// meter reads whether the data link moves; nil on a member, whose
	// connection crosses no link and never moves.
	meter *LinkMeter
	log   *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	// proven is when the path last answered with success, initialised to
	// the queue's boot readiness ([Queue.readyAt]).
	proven time.Time
	// asked is when the first probe sent since the path last answered went
	// out; zero while none waits. An abandoned probe's successor keeps it.
	asked time.Time
	// refused is the path's last explicit failure since its last success.
	refused error
	// unproven is whether the last verdict read refused, for the lines a
	// change of it logs.
	unproven bool
}

// startDataPath starts the probe, proven as of proven, until stop.
func startDataPath(timing dataPathTiming, firstHop func() error, meter *LinkMeter,
	log *slog.Logger, proven time.Time) *dataPath {
	ctx, cancel := context.WithCancel(context.Background())
	d := &dataPath{timing: timing, firstHop: firstHop, meter: meter, log: log,
		cancel: cancel, done: make(chan struct{}), proven: proven}
	go d.run(ctx)
	return d
}

// stop ends the probe and waits for it.
func (d *dataPath) stop() {
	d.cancel()
	<-d.done
}

// run probes every timing.every, one probe in flight at a time, until ctx
// ends. A probe still out past its bound while the link carries nothing is
// abandoned and asked again, since a request a member dropped (one that lost
// its leader) is never answered; one out while the link moves is waited for
// up to the cap, since its answer queues behind what the link carries.
func (d *dataPath) run(ctx context.Context) {
	defer close(d.done)
	tick := time.NewTicker(d.timing.every)
	defer tick.Stop()
	answered := make(chan struct{}, 1)
	var cancel context.CancelFunc
	var sent time.Time
	out := false
	ask := func() {
		sent, out = time.Now(), true
		d.mu.Lock()
		if d.asked.IsZero() {
			d.asked = sent
		}
		d.mu.Unlock()
		var probeCtx context.Context
		probeCtx, cancel = context.WithTimeout(ctx, d.timing.cap)
		go func() {
			d.answer(d.timing.probe(probeCtx))
			answered <- struct{}{}
		}()
	}
	ask()
	for {
		select {
		case <-ctx.Done():
			if out {
				cancel()
				<-answered
			}
			return
		case <-answered:
			cancel()
			out = false
		case <-tick.C:
			switch {
			case !out:
				ask()
			case time.Since(sent) >= d.timing.bound && !d.moving(d.timing.bound):
				cancel()
				<-answered
				ask()
			}
		}
	}
}

// answer records what one probe was answered, where it was answered at all.
func (d *dataPath) answer(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case err == nil:
		d.proven, d.asked, d.refused = time.Now(), time.Time{}, nil
	case explicitFailure(err):
		d.asked, d.refused = time.Time{}, err
	}
}

// explicitFailure reports whether err is the fleet's JetStream ANSWERING: an
// API error (a member whose metadata group lost its leader answers
// JSClusterNotAvail, jetstream_api.go jsAccountInfoRequest, v2.15.3) or no
// responders, against a request nobody answered or one abandoned.
func explicitFailure(err error) bool {
	var api *jetstream.APIError
	return errors.As(err, &api) || errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, jetstream.ErrJetStreamNotEnabled) ||
		errors.Is(err, jetstream.ErrJetStreamNotEnabledForAccount)
}

// moving reports whether the data link carried at least the meter's share of
// the floor over the last d; never on a member, which has no link to move.
func (d *dataPath) moving(span time.Duration) bool {
	return d.meter != nil && d.meter.Moving(span)
}

// verdict is [Queue.DataPath]'s answer at now.
func (d *dataPath) verdict(now time.Time) error {
	if err := d.firstHop(); err != nil {
		return err
	}
	d.mu.Lock()
	proven, asked, refused := d.proven, d.asked, d.refused
	d.mu.Unlock()
	if refused != nil && now.Sub(proven) >= d.timing.notAvailable {
		return fmt.Errorf("%w: the fleet's JetStream answers it that it is not available "+
			"(%v), with no success for %s", ErrDataPathUnproven, refused,
			now.Sub(proven).Round(time.Millisecond))
	}
	if asked.IsZero() {
		return nil
	}
	age := now.Sub(asked)
	if age >= d.timing.bound && !d.moving(d.timing.bound) {
		return fmt.Errorf("%w: the data link carried nothing for %s and the fleet's JetStream "+
			"has not answered its probe for %s", ErrDataPathUnproven, d.timing.bound,
			age.Round(time.Millisecond))
	}
	if age >= d.timing.cap {
		return fmt.Errorf("%w: the data link moves but the fleet's JetStream has not answered "+
			"its probe for %s, the link's own tolerance", ErrDataPathUnproven,
			age.Round(time.Millisecond))
	}
	return nil
}

// report logs a change of the verdict: data_path_unproven when it starts
// refusing, naming why, and data_path_proven when it stops.
func (d *dataPath) report(err error) {
	d.mu.Lock()
	changed := (err != nil) != d.unproven
	d.unproven = err != nil
	d.mu.Unlock()
	switch {
	case !changed:
	case err != nil:
		d.log.Warn("data_path_unproven", "error", err.Error(),
			"detail", "this node's data path to the fleet's JetStream is not proven, so "+
				"nothing may take or keep a hold on its word")
	default:
		d.log.Info("data_path_proven",
			"detail", "this node's data path to the fleet's JetStream answers again")
	}
}

// ProveDataPath starts probing this queue's data path, if nothing has yet,
// and returns [Queue.DataPath] for the caller to gate its holds on. The probe
// runs until the queue stops. Started by the one caller whose holds depend
// on it, never lazily by a first [Queue.DataPath]: a node that held nothing for
// hours would then meet its first claim with a proof hours old.
func (q *Queue) ProveDataPath() func() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.path != nil {
		return q.DataPath
	}
	q.path = startDataPath(q.dataPathTiming(), q.Link, q.link.meter, q.log, q.readyAt)
	return q.DataPath
}

// DataPath reports whether this queue's data path reaches the fleet's
// JetStream: nil while it does, and an error naming what is down or unproven
// while it does not. Read per call, with no round trip.
//
// Until [Queue.ProveDataPath] starts the probe it answers from the first hop
// alone, as [Queue.Link] does. Once started it also refuses, wrapping
// [ErrDataPathUnproven], a probe out past its bound while the data link
// carried nothing over that bound, one out past the link's own tolerance while
// the link moves, and a path answering "not available" with no success for
// [jsprovision.LookupBudget].
func (q *Queue) DataPath() error {
	q.mu.Lock()
	d, closed := q.path, q.closed
	q.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if d == nil {
		return q.Link()
	}
	err := d.verdict(time.Now())
	d.report(err)
	return err
}

// dataPathTiming is the probe's timing: production's in each field this
// package's tests left unset ([Queue.pathTiming]).
func (q *Queue) dataPathTiming() dataPathTiming {
	t := q.pathTiming
	if t.every <= 0 {
		t.every = dataPathProbeEvery
	}
	if t.bound <= 0 {
		t.bound = dataPathBound
	}
	if t.notAvailable <= 0 {
		t.notAvailable = jsprovision.LookupBudget
	}
	if t.cap <= 0 {
		t.cap = q.linkTolerance() + t.every + jsprovision.ReadTerm
	}
	if t.probe == nil {
		t.probe = func(ctx context.Context) error {
			_, err := q.js.AccountInfo(ctx)
			return err
		}
	}
	return t
}

// linkTolerance is how long the connection carrying the probe may go without a
// pong before it is closed as stale, read from the options in force: the leaf
// server's on a leaf ([leafPingTolerance]), the client's on an external dial
// (nats.go processPingTimer), and none on a member, which crosses no link.
func (q *Queue) linkTolerance() time.Duration {
	if q.embedded != nil {
		return q.embedded.pings
	}
	return time.Duration(q.nc.Opts.MaxPingsOut) * q.nc.Opts.PingInterval
}
