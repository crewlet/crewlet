// Package credential is the key pool a provider rotates through.
//
// A backend holds a small bag of API keys. Each call leases one; a call that
// comes back with a rate-limit or auth failure benches that key for a TTL and
// the next lease picks a different one. When every key is benched the pool has
// nothing to give, and the seat's fallback chain moves on to another model.
//
// Four things here are load-bearing:
//
//   - COOLDOWNS ARE MEASURED ON A MONOTONIC CLOCK. An NTP correction, a VM
//     migration or container clock skew must not revive a benched key early
//     nor strand a live one for a decade. The only clock read is [Clock],
//     whose default is elapsed-since-process-start.
//   - SELECTION IS LEAST-IN-FLIGHT, declaration order breaking the tie. One
//     backend instance is shared by every agent in the process, so the
//     interesting case is concurrent leases: picking the head every time puts
//     the whole fleet on one key and rate-limits it while the rest idle. With
//     nothing in flight the tie-break makes a single-caller workload
//     fill-first, which is what an operator expects from a declaration order.
//   - REPEATED AUTH FAILURES ON ONE KEY BACK OFF EXPONENTIALLY, and one
//     success resets. A typo'd key would otherwise thrash forever: 401, bench
//     for auth_seconds, retry, 401.
//   - A BENCH IS A FACT ABOUT THE VENDOR, NOT ABOUT THIS PROCESS. A quota
//     belongs to the key, so four nodes each discover a 429 on the same key
//     separately unless one tells the others. [Pool.Share] attaches the
//     fleet's ledger and [Pool.Refresh] pulls what peers found; both are off
//     the request path. See fleetKey for why a shared record is scoped.
//
// The pool speaks [llm.ErrorKind] because that is the providers layer's one
// failure vocabulary — llm.go defines ErrorKind.ExhaustsCredential precisely
// so this package does not invent a second opinion about what benches a key.
package credential

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

var log = logging.Get("providers.credential")

// ErrExhausted reports that every key in a pool is cooling down.
//
// It is a sentinel rather than a distinct error type because the layer above
// does not branch on it: the backend wraps it in an [llm.Error] carrying the
// kind that did the cooling, and the fallback chain then treats it exactly
// like any other retryable failure. Python needed a dedicated
// AllCredentialsExhausted exception and a dedicated catch in the fallback
// wrapper; here the classification IS the return value, so the chain needs no
// special case at all. Callers that genuinely want to tell "no key left" from
// "the key was refused" use errors.Is.
var ErrExhausted = errors.New("credential: every key is cooling down")

// Clock is the pool's only source of time.
//
// It returns a MONOTONIC elapsed duration, not a wall-clock instant. That is
// the whole point: cooldown arithmetic compares two readings of this function,
// so a clock step cannot move a deadline that was already recorded. Tests
// inject a counter they advance by hand, which is also how a TTL is
// fast-forwarded without sleeping.
type Clock func() time.Duration

// processStart is captured once. time.Now carries a monotonic reading and
// time.Since uses it, so elapsed is immune to wall-clock adjustment.
var processStart = time.Now()

// Elapsed is the default [Clock]: monotonic time since process start.
func Elapsed() time.Duration { return time.Since(processStart) }

// Policy is the bench time applied per error class when no server hint says
// otherwise.
//
// The two classes clear on different clocks, which is why they are two
// numbers: a 429 typically clears in an hour, while a 401 or 403 often clears
// in minutes once a token refreshes. The defaults match the config layer's
// (internal/config/providers.go: defaultRateLimitCooldown, defaultAuthCooldown)
// so a pool built without a policy behaves like one built from a config that
// named nothing.
type Policy struct {
	RateLimit time.Duration
	Auth      time.Duration
}

// The policy defaults, applied to a zero field.
const (
	DefaultRateLimitCooldown = time.Hour
	DefaultAuthCooldown      = 5 * time.Minute
)

func (p Policy) forKind(kind llm.ErrorKind) time.Duration {
	if kind == llm.KindAuth {
		if p.Auth <= 0 {
			return DefaultAuthCooldown
		}
		return p.Auth
	}
	if p.RateLimit <= 0 {
		return DefaultRateLimitCooldown
	}
	return p.RateLimit
}

// maxAuthDoublings caps the exponential auth backoff at 2^6 = 64x the policy
// TTL. Ported from the Python pool (_MAX_AUTH_BACKOFF_DOUBLINGS), where it was
// chosen so a permanently-bad key stops thrashing without being retired
// outright — six doublings of the 5-minute default reaches five hours.
const maxAuthDoublings = 6

// maxCooldown bounds any single bench, backoff included.
//
// Same reason [llm.ParseRetryAfter] clamps a Retry-After at a day: past that
// the number is no longer an instruction about our quota. Without it a
// configured 24h auth TTL times 64 doublings would retire a key for four
// years, which no operator asked for and no restart-free path undoes.
const maxCooldown = 24 * time.Hour

// Hint is a stable, non-secret identifier for a key: 12 hex characters of
// SHA-256, which is 48 bits — ample to tell a handful of keys apart in a log
// or an event, and not reversible. The obvious alternative, a suffix of the
// key itself, leaks most of a short one.
func Hint(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

type entry struct {
	key  string
	hint string

	// cooledUntil is a [Clock] reading. Zero means ready.
	cooledUntil time.Duration
	useCount    int
	inFlight    int
	// authFailures counts consecutive auth failures since the last success
	// on this key, and drives the exponential backoff.
	authFailures int
}

// Pool is a bag of credentials with cooldown bookkeeping.
//
// Safe for concurrent use: one backend instance serves every agent in the
// process, so the pool is a contended resource by construction.
type Pool struct {
	mu      sync.Mutex
	entries []*entry
	policy  Policy
	now     Clock

	// wall is the WALL-CLOCK source, used only at the fleet boundary.
	//
	// The pool's own arithmetic never touches it — see [Clock] for why —
	// but a cooldown shared with a peer has to name an instant that peer
	// can also read, and "1m42s since MY process started" is not one. So a
	// bench crosses the boundary as a wall-clock deadline and is converted
	// straight back to a [Clock] reading on the way in.
	wall func() time.Time

	// shared is the fleet's cooldown ledger, nil on a node that has none
	// — a single-node deployment, or any pool nothing called [Pool.Share]
	// on. Nil means the pool behaves exactly as it did before sharing
	// existed rather than failing: cooldowns stay this process's own.
	shared Shared
	// scope namespaces this pool's keys in that ledger. See fleetKey.
	scope string
}

// Shared is the fleet's credential cooldown ledger, as this pool needs it.
//
// Declared here rather than imported because this is the consumer: the pool
// needs two methods, and internal/coord's Cooldowns satisfies them. Both are
// BEST EFFORT in the same direction — an unreachable ledger costs a peer one
// wasted call, which is what the situation cost before any of this existed.
type Shared interface {
	// Cool records that a credential is unusable until the given instant.
	Cool(ctx context.Context, key string, until time.Time) error
	// Since returns every unlapsed cooldown across the fleet.
	Since(ctx context.Context, now time.Time) (map[string]time.Time, error)
}

// publishTimeout bounds the write a bench does on its way out.
//
// Short on purpose. It runs inside [Lease.Fail], which is on the path of a
// request that has ALREADY failed and is about to be retried on another key;
// a coordination store that is slow must not add itself to the latency of
// working around a 429. Two seconds is several times the round trip of every
// backend the contract certifies, so it fires only when the store is
// genuinely unreachable — which is the case it exists to bound.
const publishTimeout = 2 * time.Second

// refreshTolerance is how much longer a peer's cooldown must run than the
// local bench before [Pool.Refresh] takes it as new information.
//
// The two clocks disagree slightly and drift between ticks, so re-reading the
// SAME record yields a target a hair either side of what it produced last
// time. Without a floor, half of those land on "longer" and the pool reports
// a peer-imposed bench on every refresh for as long as the record lives. A
// second is far below the shortest cooldown an operator can configure (60 s,
// internal/config/providers.go minCooldownSeconds) and far above the skew of
// two clocks a few seconds apart.
const refreshTolerance = time.Second

// fleetKey is one credential's name in the shared ledger.
//
// SCOPED BY THE POOL, not bare, because a pool is what benches: a 429 is
// scoped to a vendor's rate-limit bucket, and one config entry is exactly the
// (model, endpoint, key bag) triple that shares one. An operator who lists the
// same key under a fast entry and a smart entry has two buckets at the vendor,
// and a bare hint would bench both the moment either was limited — turning one
// model's burst into a company-wide outage.
//
// The hint, never the key: the ledger is a shared store and a credential must
// not be legible in it.
func fleetKey(scope, hint string) string { return scope + ":" + hint }

// Options configure a pool.
type Options struct {
	// Keys are the credentials in declaration order. Duplicates and empty
	// strings are dropped; a pool with NO usable key still gets one empty
	// entry, so a misconfigured provider produces a clean 401 from the API
	// rather than a different failure shape at construction time.
	Keys []string

	// Policy is the per-class bench time. Zero fields take the defaults.
	Policy Policy

	// Clock is the monotonic time source. Nil takes [Elapsed].
	Clock Clock

	// Wall is the wall-clock source used at the fleet boundary. Nil takes
	// [time.Now]. Tests that drive [Pool.Refresh] set both this and Clock,
	// because the conversion between them is the part worth pinning.
	Wall func() time.Time
}

// New builds a pool.
func New(opts Options) *Pool {
	clock := opts.Clock
	if clock == nil {
		clock = Elapsed
	}
	wall := opts.Wall
	if wall == nil {
		wall = time.Now
	}
	p := &Pool{policy: opts.Policy, now: clock, wall: wall}
	seen := make(map[string]struct{}, len(opts.Keys))
	for _, key := range opts.Keys {
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			// A duplicated key is one key: two entries would let the
			// pool believe it has somewhere to rotate TO after the
			// first copy is benched, and then hand out the same
			// benched credential.
			continue
		}
		seen[key] = struct{}{}
		p.entries = append(p.entries, &entry{key: key, hint: Hint(key)})
	}
	if len(p.entries) == 0 {
		p.entries = append(p.entries, &entry{})
	}
	return p
}

// Size is the number of distinct credentials in the pool.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// Stat is one key's public state. It carries the hint, never the key.
type Stat struct {
	Hint     string
	UseCount int
	InFlight int
	// Cooling is the time left on the bench, zero when the key is ready.
	Cooling time.Duration
}

// Stats reports the pool's state for operators — which key is hot, which is
// benched and for how long.
func (p *Pool) Stats() []Stat {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]Stat, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, Stat{
			Hint:     e.hint,
			UseCount: e.useCount,
			InFlight: e.inFlight,
			Cooling:  max(0, e.cooledUntil-now),
		})
	}
	return out
}

// Share attaches the fleet's cooldown ledger, under a scope that namespaces
// this pool's keys in it. See fleetKey for why the scope is not optional.
//
// AFTER construction, not in [Options], because a pool is built when the
// company epoch is — which must work with no network at all, so that
// `crewlet validate` runs on a laptop — while the ledger belongs to a running
// node. Re-attaching is how a config apply equips the epoch it just built;
// calling it twice with the same ledger is a no-op in effect.
//
// A nil ledger DETACHES, which is what an epoch built on a node with no
// coordination store gets. It leaves whatever cooldowns are already on the
// bench alone: forgetting a live bench because sharing went away would hand
// out a key the vendor is still refusing.
func (p *Pool) Share(scope string, s Shared) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shared, p.scope = s, scope
}

// publish tells the fleet a key is benched. Best effort, and never fatal.
//
// The context is DETACHED. A bench recorded here is the consequence of a call
// that already failed, and the most common way for that call's context to be
// dead by now is the turn being cancelled — which is exactly when losing the
// record would make every peer rediscover the same 429.
func (p *Pool) publish(ctx context.Context, hint string, bench time.Duration) {
	p.mu.Lock()
	shared, scope, wall := p.shared, p.scope, p.wall
	p.mu.Unlock()
	if shared == nil || hint == "" {
		// No ledger, or the placeholder entry a keyless provider gets:
		// "the empty credential is cooling" is not a fact a peer can use,
		// and every keyless pool in the fleet would collide on it.
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	if err := shared.Cool(ctx, fleetKey(scope, hint), wall().Add(bench)); err != nil {
		// Warned rather than returned: the caller is mid-rotation and has
		// nothing useful to do with this. What it costs is that peers
		// rediscover the same refusal, which is worth a line naming the
		// key it happened to.
		log.WarnContext(ctx, "credential_cooldown_not_shared",
			"scope", scope, "hint", hint, "error", err)
	}
}

// Refresh applies what the fleet found to this pool, and reports how many
// keys it benched that were not benched already.
//
// OFF THE REQUEST PATH, pulled on a ticker rather than read per lease. A
// cooldown runs for minutes at least (60 s is the configurable floor), so a
// pull every few seconds loses almost none of one — and putting a
// coordination read in front of every model call would make the store's
// latency the floor of every turn, and its availability the company's.
//
// It EXTENDS, never shortens. A peer's record is evidence a key is refused;
// the absence of one is not evidence a key works, so a pool whose own 429 a
// peer never heard about must not be talked out of it. That also makes an
// unreadable ledger a no-op rather than a mass un-benching.
func (p *Pool) Refresh(ctx context.Context) (int, error) {
	p.mu.Lock()
	shared, scope, wall := p.shared, p.scope, p.wall
	p.mu.Unlock()
	if shared == nil {
		return 0, nil
	}
	now := wall()
	cooled, err := shared.Since(ctx, now)
	if err != nil {
		return 0, fmt.Errorf("credential: reading the fleet's cooldowns: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := p.now()
	applied := 0
	for _, e := range p.entries {
		if e.hint == "" {
			continue
		}
		until, ok := cooled[fleetKey(scope, e.hint)]
		if !ok {
			continue
		}
		// CONVERTED HERE, at the boundary, exactly once: the ledger
		// speaks wall-clock instants and everything below this line is a
		// [Clock] reading.
		target := elapsed + until.Sub(now)
		// Two records are not benches. One that lands BEHIND the clock
		// has already lapsed — [Shared.Since] is asked to drop those and
		// the certified backends do, but this is an interface a caller
		// supplies, and a bench in the past reads as "ready" while still
		// being counted as applied. One that lands no further out than
		// this pool's own is not new information; see refreshTolerance.
		if target <= elapsed || target <= e.cooledUntil+refreshTolerance {
			continue
		}
		e.cooledUntil = target
		applied++
	}
	return applied, nil
}

// Lease is one credential checked out of the pool. Exactly one of Succeed or
// Fail returns it; a second call is a no-op, so a deferred release beside an
// explicit one cannot double-count in-flight.
type Lease struct {
	pool     *Pool
	e        *entry
	released bool
}

// Key is the credential to send. It is the secret; never log it.
func (l *Lease) Key() string { return l.e.key }

// Hint is the non-secret identifier for logs and events.
func (l *Lease) Hint() string { return l.e.hint }

// Acquire leases the least-loaded key that is not cooling down.
//
// The second return is false when every key is benched, which is the caller's
// signal to stop rotating.
func (p *Pool) Acquire() (*Lease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var pick *entry
	for _, e := range p.entries {
		if e.cooledUntil > now {
			continue
		}
		// Strictly less: declaration order breaks an in-flight tie, so a
		// single caller fills the head first.
		if pick == nil || e.inFlight < pick.inFlight {
			pick = e
		}
	}
	if pick == nil {
		return nil, false
	}
	pick.useCount++
	pick.inFlight++
	return &Lease{pool: p, e: pick}, true
}

// Succeed returns the lease and clears the auth backoff: a key that just
// worked is not in a bad-credential backoff, whatever it did before.
func (l *Lease) Succeed() {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	l.e.inFlight--
	l.e.authFailures = 0
}

// Fail returns the lease and benches the key when kind says the key itself is
// at fault. retryAfter, when positive, replaces the policy TTL — a provider
// that said "retry in 20s" must not have its key benched for the full hour.
//
// Transport failures (timeout, 5xx) release without benching anything:
// cooling a healthy key on a network blip is how a fleet talks itself out of
// every credential it has.
//
// A bench is PUBLISHED to the fleet where one is attached, so peers stop
// spending a call each to rediscover the same refusal. That write happens
// after the pool's own lock is released, because it reaches a network and
// nothing else may queue behind it.
func (l *Lease) Fail(ctx context.Context, kind llm.ErrorKind, retryAfter time.Duration) {
	l.pool.mu.Lock()
	if l.released {
		l.pool.mu.Unlock()
		return
	}
	l.released = true
	l.e.inFlight--
	if !kind.ExhaustsCredential() {
		l.pool.mu.Unlock()
		return
	}

	bench := l.pool.policy.forKind(kind)
	if retryAfter > 0 {
		bench = retryAfter
	}
	if kind == llm.KindAuth {
		l.e.authFailures++
		// The backoff multiplies a server hint too. A Retry-After on a 401
		// is rare, and where one appears it says nothing about the thing
		// the backoff exists for: a key that is simply wrong will be
		// wrong again in twenty seconds.
		bench = double(bench, min(l.e.authFailures-1, maxAuthDoublings))
	}
	bench = min(bench, maxCooldown)
	l.e.cooledUntil = l.pool.now() + bench
	hint, failures := l.e.hint, l.e.authFailures
	l.pool.mu.Unlock()

	log.WarnContext(ctx, "credential_cooled",
		"hint", hint,
		"kind", kind.String(),
		"cooldown_seconds", bench.Seconds(),
		"server_hinted", retryAfter > 0,
		"consecutive_auth_failures", failures)
	l.pool.publish(ctx, hint, bench)
}

// double returns d shifted left n places, saturating at [maxCooldown].
//
// The saturation is not decoration, but the reach of the bug it prevents is
// worth being exact about, because a wrong claim here tells the next reader
// not to look. Shifting a duration six places overflows int64 into a NEGATIVE
// value — which reads as "ready", so the permanently-bad key the backoff
// exists to bench comes back INSTANTLY on the sixth failure — and that needs a
// starting bench above roughly four and a half years.
//
// No config can produce one: cooldowns are bounded to a day
// (internal/config/providers.go maxCooldownSeconds) and a Retry-After hint is
// clamped to a day by llm.ParseRetryAfter. [Policy] is a plain struct in this
// package, though, so a caller that builds one directly can, and the final
// clamp below cannot rescue it — min() of a negative is the negative.
func double(d time.Duration, n int) time.Duration {
	if n <= 0 {
		return d
	}
	if n >= maxAuthDoublings+1 || d > maxCooldown>>n {
		return maxCooldown
	}
	return d << n
}

// Identity names the caller for the errors Rotate builds.
type Identity struct {
	Provider string
	Model    string
}

// Rotate walks the pool for ONE request.
//
// ctx is the REQUEST's, taken explicitly rather than left to the closure that
// already captures it: a bench publishes to the fleet's ledger, and a package
// that reaches a network on a context it cannot see is one whose cancellation
// and deadline nobody can reason about. What it does with it is detach — see
// [Pool.publish].
//
// call is invoked with a leased key. A failure that benches the credential
// (429, 402, 401, 403) moves to the next live key and tries the SAME request
// again; every other failure is the caller's to handle and comes straight
// back, because a 400 will be a 400 on every key in the bag and a timeout says
// nothing about the credential at all.
//
// classify must return a non-nil error; one that does not is treated as an
// unclassified failure rather than allowed to panic mid-turn.
//
// Unlike the Python pool this makes no exception for a single-key bag. That
// exemption existed so tests could patch the client attribute, and its side
// effect was that a lone key was never benched at all: every call re-paid the
// round trip to be told 429 again. One code path, whatever the pool size.
func Rotate[T any](
	ctx context.Context,
	p *Pool,
	id Identity,
	classify func(error) *llm.Error,
	call func(key string) (T, error),
) (T, error) {
	var zero T
	// A pool that is already fully benched when we arrive tells us nothing
	// about WHY, and the honest default is the kind that says "try again
	// elsewhere" without claiming the key is bad.
	last := llm.KindRateLimit

	// At most one attempt per key: each rotation benches the key it
	// consumed, so the bag cannot supply more.
	for range p.Size() {
		lease, ok := p.Acquire()
		if !ok {
			break
		}
		value, err := call(lease.Key())
		if err == nil {
			lease.Succeed()
			return value, nil
		}
		classified := classify(err)
		if classified == nil {
			// A backend that cannot classify its own failure still must
			// not panic in the middle of a turn, and the contract already
			// names the safe answer for an unrecognised error.
			classified = &llm.Error{
				Kind: llm.KindFatal, Provider: id.Provider, Model: id.Model, Err: err,
			}
		}
		lease.Fail(ctx, classified.Kind, classified.RetryAfter)
		if !classified.Kind.ExhaustsCredential() {
			return zero, classified
		}
		last = classified.Kind
	}
	return zero, &llm.Error{
		Kind:     last,
		Provider: id.Provider,
		Model:    id.Model,
		Err: fmt.Errorf("all %d credentials cooling after %s: %w",
			p.Size(), last, ErrExhausted),
	}
}
