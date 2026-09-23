// Package authevents is where this node's authentication facts become events:
// the one publisher every sign-in surface, the request guard and the identity
// directory hand what they saw to, and the loop that turns failed attempts into
// something an unauthenticated caller cannot pace.
//
// # Two kinds of fact, and they reach the estate by different doors
//
// A fact a VERIFIED credential authored — a sign-in, a logout, a token minted
// by somebody holding a session — is published as it happens, through
// [Trail.Emit]. It is attributable (there is a credential to revoke and a name
// on the row) and it is paced by somebody the engine already trusts.
//
// A FAILED ATTEMPT is the opposite on both counts, and internal/events refuses
// to admit a type whose rate such a caller authors. So [Trail.Failed] never
// publishes anything. It adds one to a metrics counter and folds the attempt
// into an in-memory tally keyed on (client, minute), and [Trail.Run] — the
// engine's own loop — publishes each tally ONCE, after its minute has closed,
// as `iam_login_failures`. The caller decides how many attempts there are; the
// ticker decides how many rows there are. The design this replaced wrote a
// synchronous row per attempt, which was 6.9 million rows a day from one host.
//
// # What a tally holds, and what it is never allowed to hold
//
// Counts, the methods tried, how many the throttle turned away, and the ids of
// people the ENGINE resolved a failed attempt to. It also answers "how many
// DIFFERENT names did this client try", and that is the one number that needs
// the presented value — so the value is keyed under an HMAC key this process
// generated at start and never wrote anywhere, the digest is truncated to eight
// bytes, and the set is thrown away when the minute is published. Nothing
// leaves the process but the size of the set.
//
// The alternatives were each measured against what they leak. The presented
// value itself is where a password typed into the login box ends up. An
// UNSALTED hash of it is reversible against the company's own roster in one
// pass — the attacker who can read the audit trail has the roster too. A salted
// hash that is published is a per-deployment rainbow table away from the same
// thing. A digest under a key nobody can read is none of those, and it is the
// only one that is.
//
// # Every bound here is stated where it is enforced
//
// The number of clients one minute names ([MaxClientsPerMinute]), the distinct
// subjects one tally can count ([MaxSubjectsCounted]), the people one row names
// ([MaxPeopleNamed]), the clients the overflow row can count
// ([MaxFoldedClients]) and the keys the once-per-window dedupe remembers of
// each class ([OnceBound]) are all things an attacker would otherwise choose.
// Each is a constant with its reason at its definition, and a count that
// reaches its cap
// SATURATES rather than being dropped, so a row reads "at least" rather than
// understating what happened.
//
// # Coalescing that is not failure accounting
//
// [Trail.EmitOnce] is the other shape a rate needs: a fact that repeats and is
// worth one row per window — a Tier A token's first use in an hour or its
// overreach, a replayed cookie, a session noticed past its deadline. It is the
// notification digest's idiom (one row stands for the window) applied to a
// single key, and it reports whether it published so a caller that has to act
// exactly once alongside the row — the revocation a replayed cookie triggers —
// can hang the action on the same decision. [Trail.Claim] is the same decision
// for a caller that must READ before it knows what the fact is, and hands the
// key back when the read cannot say.
//
// EACH CLASS OF FACT KEEPS ITS OWN BOUNDED SET ([OnceClass]), so a class
// remembered for the life of the process can never evict one that expires —
// see once.go for what one shared set cost.
package authevents

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
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
type Failure struct {
	// Client is the address the throttle keys on: resolved through
	// `api.trusted_proxies`, so behind a proxy it is the caller rather
	// than the proxy. Empty is folded under the empty client, which is
	// what a request with no resolvable address honestly is.
	Client string

	// Method is what the attempt tried to prove itself with.
	Method types.FailureMethod

	// Subject is WHAT WAS PRESENTED AS WHO: the login or address typed, or
	// the bearer value sent. It is keyed under this process's own secret
	// the moment it arrives and never held, logged or published — see the
	// package doc. Empty for an attempt that named nobody (a missing
	// flight, a wrong founder code), which is then not a subject at all.
	Subject string

	// Person is the id of somebody the ENGINE resolved the attempt to —
	// a wrong password for a real login — or empty.
	Person string

	// Throttled says the attempt was refused at the throttle's ceiling
	// before anything was verified. It is counted apart, because "this
	// client kept going after it was stopped" is a different fact from
	// "this client guessed wrong".
	Throttled bool
}

// The bounds on what a caller can make this package hold.
const (
	// MaxClientsPerMinute is how many clients one minute names a row for.
	// Past it, every further client folds into ONE row whose client is
	// "*" and whose `clients` field says how many were folded.
	//
	// SIXTY-FOUR, because it is where a row per client stops telling an
	// operator anything a single row would not: a minute with more
	// distinct guessers than that is a distributed attack, and what is
	// worth writing down is its size, which the folded row carries. It
	// also makes the rows a minute can write a constant — sixty-five a
	// minute, about ninety-four thousand a day at the very worst — where
	// without it the ceiling is the attacker's address pool, and an IPv6
	// /64 alone is 2^64 of those.
	MaxClientsPerMinute = 64

	// MaxSubjectsCounted is how many distinct subjects one tally counts
	// before it saturates.
	//
	// 256, well past any honest caller and past what the throttle lets a
	// guesser reach: somebody mistyping their own login presents one or
	// two, and a client the throttle admits presents at most
	// credential.AdmitLimit VERIFIED attempts a window. The one arm
	// that can present hundreds of distinct values in a minute from one
	// client is a bearer spray, which nothing throttles — and for that,
	// "at least 256" is already all an operator does anything with. At
	// eight bytes a digest it bounds the counting to about 130 KiB across
	// every tally a minute can hold.
	MaxSubjectsCounted = 256

	// MaxPeopleNamed is how many resolved people one row names.
	//
	// SIXTEEN. The throttle admits six verified attempts per client per
	// window, so more than a handful of distinct real people from one
	// client in one minute arrives only through a fleet whose
	// coordination store is down (each node then throttles on its own
	// count); sixteen covers that with room. Past it, an attempt is
	// still COUNTED — only the name list stops growing.
	MaxPeopleNamed = 16

	// MaxFoldedClients is how many distinct clients the "*" row counts
	// before its `clients` field saturates, which is the same "at least"
	// reading [MaxSubjectsCounted] has. 4096 eight-byte digests is 32 KiB.
	MaxFoldedClients = 4096
)

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

	// key keys the subject digests. Per process, random, never written
	// anywhere: see the package doc for why nothing weaker will do.
	key []byte

	mu      sync.Mutex
	minutes map[time.Time]*minute
	once    map[OnceClass]*onceSet
}

// minute is every failed attempt one minute held.
type minute struct {
	clients map[string]*tally

	// overflow is the "*" row: every client past [MaxClientsPerMinute].
	overflow *tally
}

// tally is one row's worth of failed attempts.
type tally struct {
	attempts  int
	throttled int
	subjects  map[[8]byte]struct{}
	methods   map[types.FailureMethod]struct{}
	people    map[string]struct{}

	// folded counts distinct clients, on the overflow row only.
	folded map[[8]byte]struct{}
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
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("authevents: read the subject key: %w", err)
	}
	t := &Trail{
		pub: opts.Publisher, counter: opts.Counter, node: opts.Node,
		now: opts.Now, logger: opts.Logger, key: key,
		minutes: map[time.Time]*minute{},
		once:    newOnceSets(),
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

// Failed records one failed attempt: a counter now, and a share of the row its
// client's minute publishes once it has closed.
//
// IT NEVER PUBLISHES, and that is the admission rule rather than an
// optimisation: the caller who failed decides how often this runs.
func (t *Trail) Failed(ctx context.Context, f Failure) {
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

	var subject [8]byte
	named := f.Subject != ""
	if named {
		subject = t.digest("subject", f.Subject)
	}

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
		m = &minute{clients: map[string]*tally{}}
		t.minutes[start] = m
	}
	row := m.clients[f.Client]
	if row == nil {
		if len(m.clients) < MaxClientsPerMinute {
			row = newTally()
			m.clients[f.Client] = row
		} else {
			if m.overflow == nil {
				m.overflow = newTally()
				m.overflow.folded = map[[8]byte]struct{}{}
			}
			row = m.overflow
			if len(row.folded) < MaxFoldedClients {
				row.folded[t.digest("client", f.Client)] = struct{}{}
			}
		}
	}
	if f.Throttled {
		row.throttled++
	} else {
		row.attempts++
	}
	if f.Method != "" {
		row.methods[f.Method] = struct{}{}
	}
	if named && len(row.subjects) < MaxSubjectsCounted {
		row.subjects[subject] = struct{}{}
	}
	if f.Person != "" && len(row.people) < MaxPeopleNamed {
		row.people[f.Person] = struct{}{}
	}
}

func newTally() *tally {
	return &tally{
		subjects: map[[8]byte]struct{}{},
		methods:  map[types.FailureMethod]struct{}{},
		people:   map[string]struct{}{},
	}
}

// digest is a value keyed under this process's secret, truncated to eight
// bytes. The class keeps a subject and a client in different namespaces even
// where their strings coincide.
func (t *Trail) digest(class, value string) [8]byte {
	mac := hmac.New(sha256.New, t.key)
	mac.Write([]byte(class))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	var out [8]byte
	copy(out[:], mac.Sum(nil))
	return out
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
	var rows []*types.IAMLoginFailures
	for start, m := range t.minutes {
		if !all && !start.Before(current) {
			continue
		}
		rows = append(rows, m.rows(start)...)
		delete(t.minutes, start)
	}
	t.mu.Unlock()

	// ORDERED BY MINUTE THEN CLIENT, so two flushes of the same state
	// publish in the same order and a reader of the feed sees minutes in
	// the order they happened.
	slices.SortFunc(rows, func(a, b *types.IAMLoginFailures) int {
		if c := a.Minute.Compare(b.Minute); c != 0 {
			return c
		}
		switch {
		case a.Client < b.Client:
			return -1
		case a.Client > b.Client:
			return 1
		}
		return 0
	})
	for _, row := range rows {
		t.Emit(ctx, *row)
	}
}

// rows renders one minute as the events it publishes.
func (m *minute) rows(start time.Time) []*types.IAMLoginFailures {
	out := make([]*types.IAMLoginFailures, 0, len(m.clients)+1)
	for client, row := range m.clients {
		out = append(out, row.event(client, start, 1))
	}
	if m.overflow != nil {
		out = append(out, m.overflow.event("*", start, len(m.overflow.folded)))
	}
	return out
}

// event is one tally as the row it publishes.
func (row *tally) event(client string, start time.Time, clients int) *types.IAMLoginFailures {
	return &types.IAMLoginFailures{
		Client: client, Minute: start,
		Attempts: row.attempts, Throttled: row.throttled,
		Subjects: len(row.subjects), Clients: clients,
		Methods: slices.Sorted(maps.Keys(row.methods)),
		People:  slices.Sorted(maps.Keys(row.people)),
	}
}
