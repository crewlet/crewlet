// Package authevents is where this node's authentication facts become events:
// the one publisher every sign-in surface, the request guard and the identity
// directory hand what they saw to, and the loop that turns failed attempts into
// something an unauthenticated caller cannot pace.
//
// # Two kinds of fact, and they reach the estate by different doors
//
// A fact a VERIFIED credential or the engine itself authored — a sign-in, a
// logout, a token minted by somebody holding a session, a person's second
// factor reaching its ceiling — is published as it happens, through
// [Trail.Emit]. It is attributable (there is a credential to revoke and a name
// on the row) and it is paced by somebody the engine already trusts.
//
// A FAILED ATTEMPT is the opposite on both counts, and internal/events refuses
// to admit a type whose rate such a caller authors. So [Trail.Failed] never
// publishes anything. It adds one to a metrics counter and one to its
// source's count for the minute, and [Trail.Run] — the engine's own loop —
// publishes each minute ONCE, after it has closed, as one `iam_login_failures`
// for the node. The caller decides how many attempts there are; the ticker
// decides how many rows there are. The design this replaced wrote a
// synchronous row per attempt, which was 6.9 million rows a day from one host.
//
// # A row holds counts per source, and nothing that was presented
//
// How many attempts failed from each source — the client address as the
// trusted proxies resolve it, folded as the sign-in throttle folds it, so an
// IPv6 caller is its /64 — and how many from sources past the cap. Never
// what was typed or sent: the presented value is where a password typed into
// the login box lands, and an unsalted hash of it reverses against the
// company's own roster in one pass. The tally this replaced also counted the
// DIFFERENT names each client tried, over digests under a key the process
// generated, and named the people the engine resolved a failure to; a spray
// across the directory and a run at one person both show as a source's count
// climbing, and the metric's `method` says which door it was pushing on.
//
// # The one bound here is stated where it is enforced
//
// How many sources one minute names ([MaxSourcesPerMinute]) is a number an
// attacker would otherwise choose, so past it every further source's failures
// fold into one overflow count rather than a name each.
//
// # No fact is coalesced per window
//
// There was a third door, a once-per-window dedupe with a bounded set per
// class of fact: a Tier A token's first use in an hour and its overreach, a
// session noticed past its own deadline, a person's second factor at its
// ceiling. Each went the way that needs no remembered set. A token's use is
// on every record it writes, under its name; its overreach is a WARN log line;
// a session past its deadline is an expiry nobody authored and publishes
// nothing; and a second factor's ceiling is announced by the failure that
// takes the curve there, which the curve itself reports once per climb.
package authevents

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// Publisher is where an event goes: the node's event queue, on the ordinary
// path, so its publish listener writes this node's store row and every
// dashboard's projection hears it.
type Publisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// Counter is the metrics recorder's counter half.
type Counter interface {
	Add(name string, n uint64, attrs metrics.Attrs)
}

// Options is what a trail is built from.
type Options struct {
	// Publisher is REQUIRED: a trail that published nothing would report
	// every sign-in as having happened and leave no row of any of them.
	Publisher Publisher

	// Counter records each failed attempt. NIL COUNTS NOTHING, which is
	// the posture of an engine built with no recorder — every other
	// instrument in the process is absent there too, and refusing to
	// build would make a test engine unable to sign anybody in.
	Counter Counter

	// Node is this node's id, and the envelope's source on every event:
	// the row is this node's, and a fleet reading several nodes' feeds
	// side by side needs to know which one saw it.
	Node string

	// Now is the clock. Nil takes UTC wall time.
	Now func() time.Time

	// Logger reports a publish that failed. Nil takes the default.
	Logger *slog.Logger
}

// Failure is one failed attempt, as the surface that refused it saw it.
//
// NOTHING ABOUT WHO: not what was presented, and not whom the engine resolved
// it to. A failure is counted per source, and the metric is split by method
// and outcome; see the package doc for what the tally used to hold and why it
// does not.
type Failure struct {
	// Source is the client address, resolved through
	// `api.trusted_proxies` so that behind a proxy it is the caller rather
	// than the proxy. [Trail.Failed] counts it under the unit the sign-in
	// throttle keys on ([credential.SourceKey]) — an IPv4 address, or an
	// IPv6 address's /64 — so a surface hands over the address as it
	// resolved it and never folds it itself. Empty is counted under the
	// empty source, which is what a request with no resolvable address
	// honestly is.
	Source string

	// Method is what the attempt tried to prove itself with.
	Method types.FailureMethod

	// Throttled says the attempt was answered 429 before anything was
	// verified — the throttle's curve owed its key a longer wait than a
	// request is held open for. The metric counts it apart; the row
	// counts it as a failure like any other, because a source that keeps
	// going after it was told to wait is pushing exactly as hard.
	Throttled bool
}

// MaxSourcesPerMinute is how many sources one minute's row names. Past it,
// every further source's failures fold into the row's overflow count.
//
// SIXTY-FOUR, because it is where naming another source stops telling an
// operator anything the count would not: a minute with more distinct failing
// sources than that is a distributed attack, and what is worth writing down is
// its size, which the overflow carries. It also bounds the row — sixty-four
// addresses and counts is a few kilobytes, inside any event — and the memory a
// minute holds, where without it both would grow with the attacker's address
// pool, and an IPv6 /48 alone is 65,536 sources once each /64 is one. A source
// keeps its place once it has one, so a source already named goes on being
// counted under its name.
//
// A SOURCE IS THE THROTTLE'S UNIT, not the raw address ([Failure.Source]).
// Counted per address, one IPv6 host cycling addresses inside its own /64 —
// the cheapest caller there is — took all sixty-four places in the first
// milliseconds of every minute, and every other caller's failures, an IPv4
// guesser's included, folded into the overflow with no name on them.
const MaxSourcesPerMinute = 64

// PublishBudget bounds one publish.
//
// FIVE SECONDS, which is the JetStream client's own default API timeout: the
// broker acknowledges a publish in milliseconds on every topology this engine
// runs, so an audit row waits exactly as long as any other publish would and
// no longer. It exists because every publish here runs on a context DETACHED
// from the request that caused it — a client hanging up the moment its
// sign-in succeeds must not be what erases the row — and a detached context
// with no deadline is one a stalled broker could hold for ever.
const PublishBudget = 5 * time.Second

// Trail is one node's authentication event publisher.
//
// SAFE FOR CONCURRENT USE: every request goroutine on the node reports into it.
type Trail struct {
	pub     Publisher
	counter Counter
	node    string
	now     func() time.Time
	logger  *slog.Logger

	mu      sync.Mutex
	minutes map[time.Time]*minute
}

// minute is every failed attempt one minute held.
type minute struct {
	// sources is each named source's failures, at most
	// [MaxSourcesPerMinute] of them.
	sources map[string]int

	// overflow is the failures from every source past the cap.
	overflow int
}

// New builds a trail, or refuses a missing dependency by name.
func New(opts Options) (*Trail, error) {
	switch {
	case opts.Publisher == nil:
		return nil, errors.New("authevents: a trail with no publisher would " +
			"report every sign-in and record none of them")
	case opts.Node == "":
		return nil, errors.New("authevents: a trail needs this node's id — it " +
			"is the source on every row, and a fleet reading several nodes' " +
			"trails side by side cannot tell whose a row is without it")
	}
	t := &Trail{
		pub: opts.Publisher, counter: opts.Counter, node: opts.Node,
		now: opts.Now, logger: opts.Logger,
		minutes: map[time.Time]*minute{},
	}
	if t.now == nil {
		t.now = func() time.Time { return time.Now().UTC() }
	}
	if t.logger == nil {
		t.logger = slog.Default()
	}
	return t, nil
}

// Emit publishes one fact now.
//
// For a type whose rate a verified credential or the engine itself authors —
// internal/events' category map says which, and a failed attempt is never
// one. BEST EFFORT: a publish that fails is logged and returns, because the
// gesture it describes has already happened and the fleet-wide half of the
// trail is `iam_history`, which the record itself wrote.
func (t *Trail) Emit(ctx context.Context, payload events.Payload) {
	if t == nil || payload == nil {
		return
	}
	ev := events.NewFrom(payload, tracing.TraceOf(ctx))
	ev.Timestamp = t.now()
	ev.Source = t.node
	// DETACHED, and bounded. See [PublishBudget].
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), PublishBudget)
	defer cancel()
	if err := t.pub.Publish(publishCtx, topics.Event(ev.Type), ev); err != nil {
		t.logger.WarnContext(ctx, "auth_event_not_published", "type", ev.Type,
			"error", err.Error(),
			"detail", "the gesture happened and this node's feed has no row "+
				"for it; iam_history holds the fleet-wide record of every "+
				"identity write")
	}
}

// Failed records one failed attempt: a counter now, and one on its source's
// count for the minute, which the row publishes once the minute has closed.
//
// IT NEVER PUBLISHES, and that is the admission rule rather than an
// optimisation: the caller who failed decides how often this runs.
func (t *Trail) Failed(_ context.Context, f Failure) {
	if t == nil {
		return
	}
	outcome := "refused"
	if f.Throttled {
		outcome = "throttled"
	}
	if t.counter != nil {
		t.counter.Add(metrics.AuthAttemptsFailed, 1, metrics.Attrs{
			"method": string(f.Method), "outcome": outcome,
		})
	}

	// THE THROTTLE'S UNIT, folded here rather than by each surface, so no
	// surface can count under another one.
	source := credential.SourceKey(f.Source)

	t.mu.Lock()
	defer t.mu.Unlock()
	// THE CLOCK IS READ UNDER THE LOCK, which is what makes a minute's row
	// complete: a flush and an attempt are serialised, so an attempt that
	// takes the lock after a flush has published a minute reads a clock
	// already in the next one, rather than re-opening the minute just
	// published and writing a second row for it.
	start := t.now().Truncate(time.Minute)
	m := t.minutes[start]
	if m == nil {
		m = &minute{sources: map[string]int{}}
		t.minutes[start] = m
	}
	if _, named := m.sources[source]; named || len(m.sources) < MaxSourcesPerMinute {
		m.sources[source]++
		return
	}
	m.overflow++
}

// Run publishes each minute's failures once it has closed, until ctx ends —
// and then publishes whatever it is still holding, the open minute included,
// so a node stopping does not take its last minute's record with it.
//
// THE ENGINE'S OWN LOOP, which is the whole point: it is what makes
// `iam_login_failures` engine-rated. One goroutine, started and stopped by
// whoever built the trail.
func (t *Trail) Run(ctx context.Context) {
	for {
		now := t.now()
		// A HAIR PAST THE BOUNDARY, so the minute that just ended is the
		// one flushed rather than the one that is about to.
		next := now.Truncate(time.Minute).Add(time.Minute).Sub(now) + flushLag
		timer := time.NewTimer(next)
		select {
		case <-ctx.Done():
			timer.Stop()
			// ON THE CANCELLED CONTEXT, which is safe only because
			// [Trail.Emit] detaches every publish: the cancellation is
			// what ended the loop, and a final flush that inherited it
			// would publish nothing at all.
			t.flush(ctx, true)
			return
		case <-timer.C:
			t.Flush(ctx)
		}
	}
}

// flushLag is how far past a minute boundary the loop wakes.
//
// A QUARTER OF A SECOND, which is only there so a timer that fires a hair early
// — the runtime makes no promise it will not — still finds the minute closed.
// Nothing is lost if it were zero: an attempt's minute is decided under the
// same lock the flush takes, so an early wake merely publishes that minute on
// the next tick.
const flushLag = 250 * time.Millisecond

// Flush publishes every minute that has closed.
func (t *Trail) Flush(ctx context.Context) { t.flush(ctx, false) }

// flush publishes closed minutes, or every minute when all is set.
func (t *Trail) flush(ctx context.Context, all bool) {
	t.mu.Lock()
	current := t.now().Truncate(time.Minute)
	var rows []types.IAMLoginFailures
	for start, m := range t.minutes {
		if !all && !start.Before(current) {
			continue
		}
		rows = append(rows, m.row(start))
		delete(t.minutes, start)
	}
	t.mu.Unlock()

	// IN THE ORDER THE MINUTES HAPPENED, so a reader of the feed sees them
	// that way however the map iterated.
	slices.SortFunc(rows, func(a, b types.IAMLoginFailures) int {
		return a.Minute.Compare(b.Minute)
	})
	for _, row := range rows {
		t.Emit(ctx, row)
	}
}

// row is one minute as the event it publishes: its sources most failures
// first — the order an operator reads them in — and by address between
// equals, so two flushes of one state publish one row.
func (m *minute) row(start time.Time) types.IAMLoginFailures {
	sources := make([]types.SourceFailures, 0, len(m.sources))
	for source, failures := range m.sources {
		sources = append(sources, types.SourceFailures{Source: source, Failures: failures})
	}
	slices.SortFunc(sources, func(a, b types.SourceFailures) int {
		if c := cmp.Compare(b.Failures, a.Failures); c != 0 {
			return c
		}
		return cmp.Compare(a.Source, b.Source)
	})
	return types.IAMLoginFailures{Minute: start, Sources: sources, Overflow: m.overflow}
}
