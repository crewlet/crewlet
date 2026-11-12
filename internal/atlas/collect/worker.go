package collect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
)

var log = logging.Get("atlascollect")

// DutyName is the fleet singleton the duty claims.
const DutyName = "atlas-collect"

// Tick is how often the loop wakes to see which source is due. A minute,
// against a cadence whose floor is five: fine enough that a source is read
// within a minute of coming due, and a tick with nothing due costs one duty
// claim and one read of the standings on a connection the process holds.
const Tick = time.Minute

// VisitDeadline bounds one source's visit: its read and the records it
// publishes. Fifteen minutes: the largest read the first source makes, a
// Cloudflare account of 10,000 records in a hundred zones, is about 300
// calls at well under a second each, and its first recording of as many
// entries is a few thousand records at a few milliseconds each; a visit past
// this is waiting on a vendor or a log that does not answer, and is cut off
// so the duty's lease, sized from it, is never outlived.
const VisitDeadline = 15 * time.Minute

// MaxErrorLength bounds the fault text a standing carries, as an integration
// status bounds its own: a client that pastes a response body into its error
// is a megabyte, and the standing is a coordination record.
const MaxErrorLength = integration.MaxLastErrorLength

// Log is the atlas log as the duty reads and writes it (atlasdomain.Reader
// and Writer, through the estate router): one voice's live claims, and one
// integration's read of one entry.
type Log interface {
	LiveOf(ctx context.Context, src atlas.Source, fresh statelog.Freshness) (atlasdomain.Live, error)
	Read(ctx context.Context, c atlasdomain.Call, in atlasdomain.ReadInput) (statelog.Result, error)
}

// Store is where each source's standing is kept ([CoordStore]).
type Store interface {
	Load(ctx context.Context) (map[integration.Kind]Status, error)
	Save(ctx context.Context, st Status) error
	Forget(ctx context.Context, kind integration.Kind) error
}

// Options is what a worker is built from.
type Options struct {
	// Sources are the vendors to read; nil takes every registered one.
	Sources []Source

	Store Store
	Log   Log

	// Input is the live company document and the resolver its credentials
	// resolve through, read fresh on every tick; false where this node runs
	// no company yet, which reads nothing.
	Input func(ctx context.Context) (Input, bool)

	// Interval is the company's refresh interval, read fresh on every tick
	// for the reconcile loop's reason: it is a field of the live document.
	Interval func() time.Duration

	// ClaimDuty gates each tick on holding the fleet's duty; nil is the
	// single-node case, which always holds it.
	ClaimDuty func(ctx context.Context) (bool, error)

	// Admits gates the tick on this node's config posture, before the duty
	// is claimed, so a shedding node's lease lapses; nil admits.
	Admits func() bool

	// Tick overrides [Tick]; zero takes it. Now is the clock; nil is the
	// wall clock in UTC.
	Tick time.Duration
	Now  func() time.Time
}

// Worker is the loop.
type Worker struct {
	sources  []Source
	store    Store
	log      Log
	input    func(context.Context) (Input, bool)
	interval func() time.Duration
	claim    func(context.Context) (bool, error)
	admits   func() bool
	tick     time.Duration
	now      func() time.Time

	// shed remembers that the posture declined the last tick, so the
	// transition is logged once rather than once a tick.
	shed atomic.Bool

	mu      sync.Mutex
	stop    context.CancelFunc
	stopped chan struct{}
}

// New builds the worker.
func New(opts Options) (*Worker, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("collect: a store is required: every source's standing is recorded there")
	case opts.Log == nil:
		return nil, errors.New("collect: the atlas log is required: a read is recorded there")
	case opts.Input == nil:
		return nil, errors.New("collect: the company document is required: every read resolves its " +
			"credential from it")
	case opts.Interval == nil:
		return nil, errors.New("collect: the refresh interval is required")
	}
	w := &Worker{
		sources: opts.Sources, store: opts.Store, log: opts.Log, input: opts.Input,
		interval: opts.Interval, claim: opts.ClaimDuty, admits: opts.Admits, tick: opts.Tick, now: opts.Now,
	}
	if w.sources == nil {
		w.sources = Registered()
	}
	seen := map[integration.Kind]bool{}
	for _, s := range w.sources {
		if seen[s.Kind()] {
			return nil, fmt.Errorf("collect: the %s source is given twice", s.Kind())
		}
		seen[s.Kind()] = true
	}
	if w.tick <= 0 {
		w.tick = Tick
	}
	if w.now == nil {
		w.now = func() time.Time { return time.Now().UTC() }
	}
	return w, nil
}

// Start arms the loop. Safe to call twice.
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		return
	}
	if len(w.sources) == 0 {
		log.InfoContext(ctx, "atlas_collect_idle", "detail", "no source is registered in this build")
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	w.stop = cancel
	w.stopped = make(chan struct{})
	go w.run(ctx, w.stopped)
}

// Stop ends the loop, waiting for a tick in flight.
func (w *Worker) Stop() {
	w.mu.Lock()
	stop, stopped := w.stop, w.stopped
	w.stop, w.stopped = nil, nil
	w.mu.Unlock()
	if stop == nil {
		return
	}
	stop()
	<-stopped
}

func (w *Worker) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	log.InfoContext(ctx, "atlas_collect_started", "tick", w.tick, "sources", len(w.sources))
	// A tick at once, so a restart reads whatever came due while the
	// process was down rather than waiting out a tick.
	w.Tick(ctx)
	ticker := time.NewTicker(w.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.InfoContext(ctx, "atlas_collect_stopping")
			return
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}

// Tick reads every source that is due, where this node holds the duty.
func (w *Worker) Tick(ctx context.Context) {
	// POSTURE FIRST, BEFORE THE CLAIM, for the reconcile loop's reason: a
	// node whose document the fleet has replaced would read a vendor under
	// a credential that document no longer names. Declining without
	// claiming lets a peer on the current revision take the duty.
	if w.admits != nil && !w.admits() {
		if !w.shed.Swap(true) {
			log.WarnContext(ctx, "atlas_collect_shed",
				"detail", "this node's configuration is behind the fleet's, so it reads no source "+
					"for the atlas; a peer holding the current revision takes the duty when this node's "+
					"lease lapses")
		}
		return
	}
	if w.shed.Swap(false) {
		log.InfoContext(ctx, "atlas_collect_resumed", "detail", "this node's configuration is current again")
	}
	if !w.holdsDuty(ctx) {
		return
	}
	in, ok := w.input(ctx)
	if !ok || in.Config == nil {
		return
	}
	standings, err := w.store.Load(ctx)
	if err != nil {
		log.WarnContext(ctx, "atlas_source_standings_unreadable", "error", err,
			"detail", "no source is read this tick; the recorded standings are still what the API serves")
		return
	}
	now := w.now().UTC()
	interval := w.interval()
	known := map[integration.Kind]bool{}
	for _, src := range w.sources {
		if ctx.Err() != nil {
			return
		}
		kind := src.Kind()
		known[kind] = true
		st, found := standings[kind]
		if !in.Config.DeclaresIntegration(kind.String()) {
			// A SOURCE THE COMPANY DOES NOT CONNECT HAS NO STANDING: its
			// row is forgotten, as an integration's status is. What it
			// recorded stays on the log until a read or a person retires it.
			if found {
				w.forget(ctx, kind)
			}
			continue
		}
		if !found {
			st = Status{Kind: kind}
		}
		if !st.Due(now, interval) {
			continue
		}
		// STILL OURS? Re-claimed before every source, for the reconcile
		// loop's reason: a visit makes hundreds of vendor calls, and the
		// duty's lease is sized for one visit rather than a sweep.
		if !w.holdsDuty(ctx) {
			return
		}
		w.visit(ctx, src, in, st, now)
	}
	// A STANDING WITH NO SOURCE IN THIS BUILD is forgotten too: a build
	// that dropped a vendor's source would otherwise show it as read.
	for kind := range standings {
		if !known[kind] {
			w.forget(ctx, kind)
		}
	}
}

// holdsDuty claims the duty, and reads an unknown answer as not held: a
// coordination store that could not answer has not said the duty is ours,
// and two nodes reading one vendor is what the singleton exists to rule out.
func (w *Worker) holdsDuty(ctx context.Context) bool {
	if w.claim == nil {
		return true
	}
	held, err := w.claim(ctx)
	if err != nil {
		log.WarnContext(ctx, "atlas_collect_duty_unknown", "error", err,
			"detail", "skipping this tick rather than risking a second node reading the same vendor")
		return false
	}
	return held
}

func (w *Worker) forget(ctx context.Context, kind integration.Kind) {
	if err := w.store.Forget(ctx, kind); err != nil {
		log.WarnContext(ctx, "atlas_source_standing_kept", "source", kind.String(), "error", err)
	}
}

// visit reads one source whole, plans what changed against the log and
// publishes it, then records the source's standing whatever happened.
func (w *Worker) visit(ctx context.Context, src Source, in Input, st Status, now time.Time) {
	kind := src.Kind()
	bounded, cancel := context.WithTimeout(ctx, VisitDeadline)
	defer cancel()
	in.Now = now
	st.LastAttemptAt, st.Completed, st.LastError = now, false, ""
	st.Recorded, st.Retired, st.Purged = 0, 0, 0
	read, err := src.Collect(bounded, in)
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		w.forget(ctx, kind)
		return
	case err != nil:
		// A READ THAT DID NOT FINISH RECORDS AND RETIRES NOTHING, and
		// forgets no miss: the standing keeps what the last complete read
		// established, and says the fault.
		w.fault(ctx, st, "atlas_source_read_failed", err)
		return
	}
	live, err := w.log.LiveOf(bounded, atlas.IntegrationSource(kind.String()),
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		w.fault(ctx, st, "atlas_source_log_unread", err)
		return
	}
	plan := Decide(live.Claims, read, st, now)
	st = plan.Status
	published, err := w.publish(bounded, kind, plan.Records, now, &st)
	st.Recorded = published
	if err != nil {
		w.fault(ctx, st, "atlas_source_record_failed", err)
		return
	}
	st.Completed, st.LastCompleteAt = true, now
	log.InfoContext(ctx, "atlas_source_read", "source", kind.String(), "entries", st.Entries,
		"calls", st.Calls, "withheld", st.Withheld, "recorded", st.Recorded, "retired", st.Retired,
		"purged", st.Purged, "missing", len(st.Missing))
	w.save(ctx, st)
}

// publish records each entry's read in the plan's order, stopping at the
// first fault, and answers how many landed. A read the log refuses because
// an administrator purged what it names is counted on the standing, never
// said, and the rest go on: the record names what the purge erased.
func (w *Worker) publish(ctx context.Context, kind integration.Kind, records []EntryRead, now time.Time,
	st *Status) (int, error) {

	published := 0
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return published, err
		}
		_, err := w.log.Read(ctx, atlasdomain.Call{By: dutyActor, OpID: statelog.NewOpID(now, DutyName)},
			atlasdomain.ReadInput{
				Source: atlas.IntegrationSource(kind.String()), Entry: rec.Entry,
				Claims: rec.Claims, Withdrawn: rec.Withdrawn, ObservedAt: now,
			})
		switch {
		case errors.Is(err, atlasdomain.ErrPurged):
			st.Purged++
		case err != nil:
			return published, err
		default:
			published++
		}
	}
	return published, nil
}

// fault records a visit that did not finish.
func (w *Worker) fault(ctx context.Context, st Status, event string, err error) {
	st.LastError = textcut.Within(err.Error(), MaxErrorLength)
	if ctx.Err() == nil {
		log.WarnContext(ctx, event, "source", st.Kind.String(), "error", err,
			"detail", "the atlas keeps what the last complete read established; the source is read "+
				"again after "+RetryAfter.String())
	}
	w.save(ctx, st)
}

func (w *Worker) save(ctx context.Context, st Status) {
	if err := w.store.Save(ctx, st); err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, "atlas_source_standing_unsaved", "source", st.Kind.String(), "error", err)
	}
}

// dutyActor is who the duty's records are attributed to: the engine's own
// kind, named for the duty, and no credential, since none made the write.
var dutyActor = iam.Actor{Name: DutyName, Kind: iam.ActorSystem}
