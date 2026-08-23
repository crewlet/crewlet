// Package credential is the key pool a provider rotates through.
//
// A backend holds a small bag of API keys. Each call leases one; a call that
// comes back with a rate-limit or auth failure benches that key for a TTL and
// the next lease picks a different one. When every key is benched the pool has
// nothing to give, and the seat's fallback chain moves on to another model.
//
// Three things here are load-bearing:
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
//
// The pool speaks [llm.ErrorKind] because that is the providers layer's one
// failure vocabulary — llm.go defines ErrorKind.ExhaustsCredential precisely
// so this package does not invent a second opinion about what benches a key.
package credential

import (
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
}

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
}

// New builds a pool.
func New(opts Options) *Pool {
	clock := opts.Clock
	if clock == nil {
		clock = Elapsed
	}
	p := &Pool{policy: opts.Policy, now: clock}
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
func (l *Lease) Fail(kind llm.ErrorKind, retryAfter time.Duration) {
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

	log.Warn("credential_cooled",
		"hint", hint,
		"kind", kind.String(),
		"cooldown_seconds", bench.Seconds(),
		"server_hinted", retryAfter > 0,
		"consecutive_auth_failures", failures)
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
		lease.Fail(classified.Kind, classified.RetryAfter)
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
