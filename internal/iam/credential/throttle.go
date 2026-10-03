package credential

import (
	"container/list"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// THE ENUMERATION ORACLE, AND THE TWO THINGS THAT CLOSE IT.
//
// A sign-in endpoint is the one surface an unauthenticated stranger may
// address, and EVERYTHING about how it answers is evidence. The failure this
// file exists for is not a guessed password — it is an attacker learning WHO
// WORKS HERE, in as many requests as they care to make, from nothing but how
// long each one took or which of them was throttled.
//
// # 1. The throttle is keyed on what was TYPED, never on what it resolved to
//
// [Throttle.Admit] decides before anything is looked up, on the subject as the
// caller TYPED it — normalised, never resolved — from the request's source. A
// throttle keyed on the RESOLVED subject is one only real subjects can
// trigger, so its delay becomes the oracle it was added to prevent: an
// attacker fails a few times per name and reads the roster off which names
// start slowing down. Keyed on the typed value, a name nobody holds climbs
// the curve exactly as a real one does, and the curve says nothing about
// anybody. (Two spellings of one person — their address and their login —
// are two keys, which costs a guesser one more curve per spelling and is the
// price of the key never depending on a lookup.)
//
// # 2. A subject that does not exist is still verified against
//
// [Hasher.Decoy] verifies what was presented against one dummy verifier at the
// hasher's own cost when there is no real verifier to check, so a miss is one
// argon2id derivation under the same cap exactly as a hit is — and the
// derivation is what dominates how long a sign-in takes.
//
// WHAT IS NOT EQUALISED is said there and in the package doc: the directory
// read before the derivation, and a real verifier written at another cost. No
// answer is padded to a common deadline. One was, measured from admission, and
// keeping it honest under load took a turn per address at the verify cap and a
// decoy whose hold was drawn from the node's recent derivations — a scheduler
// and a sampler to hide a difference of one indexed read.

// THE CURVE, AND WHY IT IS NEVER A LOCKOUT.
//
// A hard refusal after N failures is a lockout an outsider can cause: keyed on
// a login it locks the company's only administrator out for ever at less than
// a request a minute. So a failure costs DELAY, never refusal: each one
// doubles the wait before its key's next attempt, from [DelayFloor] to
// [DelayCeiling], and a correct credential after the wait succeeds. A wait up
// to [InlineDelay] is served inside the request; a longer one is answered 429
// with the time left, which is the whole of the specific answer this surface
// gives.
//
// # One key: the subject as typed, from one source
//
// THE PAIR catches a run at ONE account, and it has no allowance: the first
// failure is already a second's wait. A success clears THIS PAIR and nothing
// else, so somebody holding any account cannot sign in as themselves between
// guesses at somebody else's and wipe the record of them.
//
// # And never the source alone
//
// A curve on the source alone would catch a run across many accounts, and this
// throttle had one: ten failures free, then the same doubling wait, refused
// past five seconds. A refusal decided on an address is one ANYBODY SHARING IT
// holds shut for everybody else at it — an office behind one NAT, a VPN's
// egress, and on a deployment whose proxy is not in `api.trusted_proxies` the
// whole internet — and it took one failed attempt every twenty-five seconds,
// from anybody, at any name, to keep every sign-in from that address at 429,
// the right passwords included, since a refusal before the verification never
// reaches one. And because an attempt still being checked counted against it
// as a failure, a dozen honest people signing in at once from one office met
// the same 429 with nobody failing at all. A gate on the source that nobody
// else could hold shut would have to be one that never refuses, and a wait
// that is never a refusal slows the guesser no more than the verify cap
// already does.
//
// WHAT THAT LEAVES UNBOUNDED BY A CURVE is one password tried against many
// names from one address — every pair is fresh. What bounds it is COST: every
// name tried, held or not, is an argon2id derivation waiting for a slot of the
// verify cap, and past that the password floor and the blocklist. What makes
// it seen is the failure count per source the audit trail publishes each
// minute.
//
// # And a second factor on the person, whatever the address
//
// A code is decided on the pair's curve AND on one keyed on the person the
// login resolved to ([Throttle.AdmitSecondFactor]). The pair alone lets
// somebody holding the password divide the curve by every address they own —
// sixty-five thousand in a /48 — and a six-digit code falls to that in about
// an hour. Keying on the resolved person is what the first rule above forbids,
// and it is safe here for the one reason it is safe anywhere: it is reached
// only past the password, so only a caller who already knows who this is can
// drive it. It lifts on the success that completes a sign-in, and the failure
// that takes it to its ceiling says so ([Ticket.Fail]) — once per climb — so
// the caller can announce that somebody holding a person's password is
// guessing at their second factor.
//
// AN ATTEMPT THAT NAMES NOBODY IS NOT COUNTED — an invitation link. There is
// no subject to key a pair on, and its secret is minted with 256 bits of
// crypto/rand, so there is nothing a curve would slow; keyed on the source it
// was a way for a stranger to hold every invitation from an address shut.
//
// # Each node keeps its own curve
//
// A load balancer puts each attempt of a guessing run on whichever node it
// likes, and a node counts only the failures it saw. So on a fleet of N nodes
// serving sign-ins, a run rotated across all of them meets each node's curve
// separately and is admitted up to N times as often as it would be on one —
// and that residual is DECIDED, not overlooked. The curve was once shared
// through the coordination store: every failure written to a fleet bucket, a
// climbing pair read back before each step, each writer's clock re-dated, a
// per-source allowance bounding the reads a spray of fresh names drove, a
// pause when the store stopped answering, and a digest key derived from the
// keyring so every node named a pair alike. That is a coordination round trip
// on the sign-in path and a subsystem of its own, bought for a factor of N on
// a rate the curve has already cut sixty-fold at its first step — and on the
// single node most deployments are, it bought nothing at all. What bounds a
// run at one account on N nodes is what bounds it on one, multiplied by N:
// thirty seconds per attempt per node at the ceiling, each attempt an
// argon2id verification under the verify cap, against a password at least
// twelve characters long that is not on the blocklist, and — for a person who
// holds one — a second factor, whose own curve a guesser meets only once they
// already hold the password.
//
// EVERY INSTANT HERE IS THIS PROCESS'S OWN CLOCK, read through [time.Now]'s
// monotonic reading, so a wall-clock step — an NTP correction, a VM migration
// — neither stretches a pair's wait nor ends it early: a monotonic reading is
// what every comparison below is between.
//
// Nothing held is what was typed: a pair is a keyed digest under a key this
// process generated and never wrote anywhere.
//
// # Concurrency is paid for, too
//
// An attempt that is admitted and not yet resolved counts against its pair as
// a failure until it resolves: a burst of concurrent guesses at one account is
// then served one after another along the curve rather than all at once. A
// success refunds it; a request that verified nothing releases it.

// Window is how long a failure counts against its key.
//
// FIFTEEN MINUTES is the interval the curve actually has to reason over. From
// below it is bounded by the curve itself: a run held at [DelayCeiling] makes
// one attempt per ceiling, so a window shorter than [CurveSteps] of those
// would let the oldest failures age out under a run that never stopped, and
// the run would slide back down the curve while it was being throttled. From
// above it is bounded by the person who mistyped: past a quarter hour an
// honest mistake should stop costing anything, and a failure that counted for
// longer would meet them at their next sign-in as a wait nobody could explain.
const Window = 15 * time.Minute

// The curve.
const (
	// DelayFloor is the wait a pair's first failure costs: one second, which is invisible to a person who mistyped and
	// is already a sixty-fold cut in what a script gets through.
	DelayFloor = time.Second

	// DelayCeiling is the most one failure ever costs: thirty seconds,
	// reached at the sixth ([CurveSteps]). Past it the curve stops
	// climbing rather than locking anybody out, because a key an outsider
	// can drive — any login they can type — must never be one they can
	// close.
	DelayCeiling = 30 * time.Second

	// CurveSteps is how many failures take a pair's delay from nothing
	// to [DelayCeiling]: 1, 2, 4, 8, 16, then 30 seconds.
	CurveSteps = 6

	// InlineDelay is the longest wait served inside the request: five
	// seconds, which a person at a sign-in form reads as a slow answer
	// rather than a broken one. A longer wait is answered 429 with the
	// time left, so a client is told when to come back rather than held
	// open for it.
	InlineDelay = 5 * time.Second

	// DelayedCap is how many requests one throttle holds asleep at once:
	// sixty-four. A waiting request is a goroutine and an open
	// connection, and an attacker who can make every attempt wait could
	// otherwise park as many of both as they can open; past the cap the
	// wait is answered at once, 429 with the time left. Honest traffic
	// almost never waits — a person's first mistake costs their next
	// attempt one second — so the cap is set against a flood, not
	// against a morning.
	DelayedCap = 64

	// MaxPairs is how many pairs one throttle holds, the least recently
	// used forgotten past it: 16384. Every pair costs at most [pairKeep]
	// failure instants and its pending attempts, beside its digest and
	// its place in the order — about half a kilobyte — so the bound is
	// under ten megabytes of memory an unauthenticated caller cannot
	// grow, where a map walked on every failure was both unbounded and
	// O(keys) per write.
	//
	// A PAIR FORGOTTEN EARLY STARTS ITS CURVE AGAIN, which is what the
	// bound costs, and why it is set this high: to push one climbing pair
	// out, a guesser has to fail 16384 others inside [Window] — each an
	// argon2id derivation under the verify cap, which at the shipped cost
	// is minutes of a whole node's cap, every sign-in on it queueing
	// behind them while they run.
	MaxPairs = 16384
)

// pairKeep is how many failure instants a pair keeps, NEWEST FIRST OUT LAST:
// the curve reads the count up to where it stops climbing and the newest
// instant, and keeping the newest N answers both exactly — the older failures
// age out of the window before any of these do.
const pairKeep = CurveSteps

// ErrThrottled reports an attempt the curve says must wait longer than
// [InlineDelay]. The error that carries it is a [*Throttled], whose
// RetryAfter is the wait left.
//
// THE ONE SPECIFIC REFUSAL ON A SIGN-IN SURFACE, and it is safe because it
// says only what the caller already knows: that THEY failed recently, from
// here, on what they typed. A name nobody holds is throttled exactly as a
// real one is.
var ErrThrottled = errors.New("credential: too many failed attempts")

// Throttled is a refusal to attempt now, and when to come back.
type Throttled struct {
	// RetryAfter is the time left until the curve admits this attempt.
	RetryAfter time.Duration
}

func (e *Throttled) Error() string {
	return fmt.Sprintf("%v: the next attempt is admitted in %s", ErrThrottled,
		e.RetryAfter.Round(time.Second))
}

// Is makes a [*Throttled] match [ErrThrottled].
func (e *Throttled) Is(target error) bool { return target == ErrThrottled }

// RetryAfter is the wait a refusal names, or zero for any other error.
func RetryAfter(err error) time.Duration {
	var throttled *Throttled
	if errors.As(err, &throttled) {
		return throttled.RetryAfter
	}
	return 0
}

// Attempt is what one authentication attempt is keyed on.
type Attempt struct {
	// Source is the caller's own address as the deployment's trusted
	// proxies resolve it. Empty is UNIDENTIFIABLE, and admitted uncounted
	// — see [Throttle.Admit].
	Source string

	// Subject is who the caller TYPED that they are — a login or an
	// address — or empty for a credential that names nobody, which is
	// admitted uncounted: see this file's head. Never what it resolved
	// to.
	Subject string
}

// Throttle is the sign-in curve.
//
// SAFE FOR CONCURRENT USE.
type Throttle struct {
	key   []byte
	now   func() time.Time
	sleep func(context.Context, time.Duration)

	mu      sync.Mutex
	pairs   *lru
	delayed int
}

// ThrottleDeps is what a throttle is built from. Every field is optional.
type ThrottleDeps struct {
	// Now is the clock. Nil takes [time.Now], whose monotonic reading is
	// what keeps a wall-clock step out of every wait — see this file's
	// head — so a production caller leaves it nil.
	Now func() time.Time

	// Sleep is how a wait is served. Nil takes a sleep the request's
	// context cancels.
	Sleep func(context.Context, time.Duration)
}

// NewThrottle builds one.
func NewThrottle(deps ThrottleDeps) *Throttle {
	t := &Throttle{
		key: randomKey(), now: deps.Now, sleep: deps.Sleep,
		pairs: newLRU(MaxPairs),
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.sleep == nil {
		t.sleep = sleepUntil
	}
	return t
}

// randomKey is thirty-two bytes nothing outside this process ever sees.
func randomKey() []byte {
	key := make([]byte, sha256.Size)
	// crypto/rand does not fail: it aborts the process rather than
	// return an error, so there is nothing here to handle.
	_, _ = rand.Read(key)
	return key
}

// Admit decides whether an attempt may proceed, and when.
//
// # Nothing is looked up before it, and nothing about the subject is looked up
// inside it
//
// It is called before the subject resolves, and decides on the pair's own
// count with no I/O: a map lookup under a lock. The attempt waits up to
// [InlineDelay] inside this call; a longer wait is a [*Throttled].
//
// # What it hands back
//
// A [Ticket] the caller resolves exactly once: [Ticket.Fail] for a credential
// that did not prove itself, [Ticket.Succeed] for one that did, and
// [Ticket.Release] — safe to defer, and a no-op after either — for an attempt
// that never reached a verdict. Until then the attempt counts against its pair
// as a failure, which is what serialises a burst at one account.
//
// AN ATTEMPT THAT NAMES NOBODY IS ADMITTED, and uncounted: there is no pair to
// key it on, and the source alone is a key anybody at the address could hold
// shut — see this file's head.
//
// AN UNIDENTIFIABLE SOURCE IS ADMITTED, and uncounted, which is the safe
// direction rather than the lax one: refusing would mean a misconfigured proxy
// — one that strips the header a source is derived from — locks every person
// in the company out at once, which is an outage the throttle caused. What
// still bounds that caller is the password cost and the verify cap.
//
// An uncounted attempt's ticket is nil, whose methods do nothing.
func (t *Throttle) Admit(ctx context.Context, a Attempt) (*Ticket, error) {
	if a.Source == "" || a.Subject == "" {
		return nil, nil
	}
	return t.admit(ctx, t.pairOf(a.Subject, sourceKeyOf(a.Source)))
}

// AdmitSecondFactor decides whether a second factor may be checked for
// person, and when — on the PERSON'S OWN CURVE, whatever address the code
// arrives from.
//
// # Why this one key is the resolved person
//
// A second factor exists to stop somebody who already has the password, and
// on the pair's curve alone that somebody is barely slowed: each address and
// each spelling of the login is a fresh pair, so a /48 of IPv6 addresses is
// sixty-five thousand of them, each allowed a handful of guesses a minute, and
// what is left to bound six digits is the verify cap. So a code is ALSO
// decided on a curve keyed on the person, and every address's guesses climb
// it together.
//
// It is keyed on who the login RESOLVED to, which is exactly what the pair's
// curve must never be (this file's head), and the difference is who can reach
// it: this is asked only once the password has proved itself, so the only
// caller who can drive it holds the password, and it tells them nothing about
// who exists that the password did not. Its digest is taken in a domain of its
// own, so no typed subject can ever name it. An empty person is admitted
// uncounted.
func (t *Throttle) AdmitSecondFactor(ctx context.Context, person string) (*Ticket, error) {
	if person == "" {
		return nil, nil
	}
	return t.admit(ctx, t.factorOf(person))
}

// admit is [Throttle.Admit] and [Throttle.AdmitSecondFactor] past the key.
func (t *Throttle) admit(ctx context.Context, pair string) (*Ticket, error) {
	t.mu.Lock()
	now := t.now()
	wait := t.waitLocked(pair, now)
	if wait > InlineDelay || (wait > 0 && t.delayed >= DelayedCap) {
		t.mu.Unlock()
		return nil, &Throttled{RetryAfter: wait}
	}
	start := now.Add(wait)
	t.pairs.take(pair).pend(start)
	if wait > 0 {
		t.delayed++
	}
	t.mu.Unlock()

	ticket := &Ticket{t: t, pair: pair, at: start}
	if wait > 0 {
		t.sleep(ctx, wait)
		t.mu.Lock()
		t.delayed--
		t.mu.Unlock()
		if err := ctx.Err(); err != nil {
			ticket.Release()
			return nil, err
		}
	}
	return ticket, nil
}

// Ticket is one admitted attempt, until it is resolved.
//
// A NIL TICKET IS AN UNCOUNTED ATTEMPT — one that names nobody, or an
// unidentifiable source — and every method on it does nothing, so a caller
// never branches on which it holds.
type Ticket struct {
	t    *Throttle
	pair string
	at   time.Time
	done atomic.Bool
}

// Fail resolves the attempt as a credential that did not prove itself, and
// records the failure against its pair.
//
// It reports whether THIS failure took the key's curve to its ceiling —
// [CurveSteps] failures inside the window, where there had been fewer — which a
// caller whose key is a person reads as somebody guessing at them
// ([Throttle.AdmitSecondFactor]).
//
// THE TRANSITION AND NOT THE STATE, so the curve itself is what says "once":
// a run held at the ceiling keeps [CurveSteps] failures in the window, each
// new one replacing the oldest, and reaches it again only after it has aged
// back down. A caller announcing on the state would announce every failure of
// a run, and one deduplicating that would keep a second memory of what the
// curve already knows.
func (k *Ticket) Fail() (reachedCeiling bool) {
	if k == nil || !k.done.CompareAndSwap(false, true) {
		return false
	}
	t := k.t
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	// DATED NO EARLIER THAN THE START ADMISSION SCHEDULED, so the wait
	// the next attempt owes runs from when this one could first have
	// been made rather than from a clock that has not reached it yet.
	at := now
	if at.Before(k.at) {
		at = k.at
	}
	p := t.pairs.take(k.pair)
	p.unpend(k.at)
	p.prune(now.Add(-Window))
	before := len(p.fails)
	p.fail(at)
	return before < CurveSteps && len(p.fails) >= CurveSteps
}

// Succeed resolves the attempt as a credential that proved itself: its pair is
// forgotten — and NOTHING ELSE is, which is what keeps an account holder from
// wiping the record of their guesses at somebody else's by signing in as
// themselves between them.
func (k *Ticket) Succeed() {
	if k == nil || !k.done.CompareAndSwap(false, true) {
		return
	}
	t := k.t
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pairs.drop(k.pair)
}

// Release resolves an attempt that never reached a verdict — a body that did
// not parse, a store that could not be read, a request that went away, a
// password that proved itself where a second factor is still to come — so it
// counts as nothing. A no-op once the ticket is resolved, so it is what a
// caller defers.
func (k *Ticket) Release() {
	if k == nil || !k.done.CompareAndSwap(false, true) {
		return
	}
	t := k.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := t.pairs.get(k.pair); p != nil {
		p.unpend(k.at)
		if p.empty(t.now()) {
			t.pairs.drop(k.pair)
		}
	}
}

// waitLocked is how long an attempt on pair must wait at now. Held under the
// lock.
//
// NEVER MORE THAN [InlineDelay] PLUS [DelayCeiling]: the curve measures from
// the newest failure or pending attempt it holds, and neither is dated past the
// start an admission scheduled, which is never more than [InlineDelay] ahead of
// the clock it was scheduled at. The lead is what serialises a burst at one
// pair.
func (t *Throttle) waitLocked(pair string, now time.Time) time.Duration {
	s := t.pairs.get(pair)
	if s == nil {
		return 0
	}
	count, last := s.weight(now)
	delay := delayFor(count)
	if delay == 0 {
		return 0
	}
	return max(last.Add(delay).Sub(now), 0)
}

// pairOf is the key a (typed subject, source) pair is held under: a keyed
// digest, never the value.
//
// THE VALUE IS WHAT WAS TYPED, and a password typed into the login box is what
// lands there often enough to matter. Held in the clear for a window, or
// hashed without a key, it would be recoverable from this process's memory
// against the company's own roster in one pass.
//
// NORMALISED, NEVER RESOLVED. An address folds the way the identity estate
// folds one ([iam.NormalizeEmail]) — so a plus tag, which reaches the same
// person, reaches the same curve — and anything else folds its case.
func (t *Throttle) pairOf(subject, source string) string {
	typed := strings.TrimSpace(subject)
	if strings.Contains(typed, "@") {
		typed = iam.NormalizeEmail(typed)
	} else {
		typed = strings.ToLower(typed)
	}
	mac := hmac.New(sha256.New, t.key)
	mac.Write([]byte("pair\x00"))
	mac.Write([]byte(typed))
	mac.Write([]byte{0})
	mac.Write([]byte(source))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// factorOf is the key a person's second-factor curve is held under: a keyed
// digest of the person's id, in a domain no pair's digest shares, so no subject
// anybody types names it.
func (t *Throttle) factorOf(person string) string {
	mac := hmac.New(sha256.New, t.key)
	mac.Write([]byte("factor\x00"))
	mac.Write([]byte(person))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// sourceKeyOf is the source half of a pair's key: the address, and for IPv6
// its /64.
//
// THE /64 IS WHAT ONE IPv6 CUSTOMER IS GIVEN, and every address inside it is
// theirs to use: keyed per address, a run at one account from one host rotates
// through 2^64 fresh pairs and its curve never starts. A value that is not an
// address — a test's name for a caller, or whatever a misconfigured proxy
// forwarded — is keyed as it is.
func sourceKeyOf(source string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(source))
	if err != nil {
		return source
	}
	addr = addr.Unmap()
	if addr.Is6() {
		prefix, err := addr.WithZone("").Prefix(64)
		if err == nil {
			return prefix.String()
		}
	}
	return addr.String()
}

// sleepUntil is the default wait, cancellable so a shutting-down node does not
// hold a request open for it.
func sleepUntil(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// delayFor is the wait a pair's next attempt owes after count failures.
func delayFor(count int) time.Duration {
	switch {
	case count <= 0:
		return 0
	case count >= CurveSteps:
		return DelayCeiling
	}
	return min(DelayFloor<<(count-1), DelayCeiling)
}

// ---- the curve's memory ------------------------------------------------------ //

// lru is the pairs' standings by key, the least recently used forgotten past
// its bound — an unauthenticated caller grows it one key at a time, so it is
// bounded rather than a map walked or kept for ever.
type lru struct {
	bound int
	byKey map[string]*list.Element // of *entry
	order *list.List               // most recently used at the front
}

type entry struct {
	key string
	val *standing
}

func newLRU(bound int) *lru {
	return &lru{bound: bound, byKey: map[string]*list.Element{}, order: list.New()}
}

// get is key's standing, or nil, marking it used.
func (l *lru) get(key string) *standing {
	el, ok := l.byKey[key]
	if !ok {
		return nil
	}
	l.order.MoveToFront(el)
	return el.Value.(*entry).val
}

// take is key's standing, made if there is none — forgetting the least
// recently used key when that would pass the bound.
func (l *lru) take(key string) *standing {
	if v := l.get(key); v != nil {
		return v
	}
	if l.order.Len() >= l.bound {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.byKey, oldest.Value.(*entry).key)
	}
	v := &standing{}
	l.byKey[key] = l.order.PushFront(&entry{key: key, val: v})
	return v
}

// drop forgets key.
func (l *lru) drop(key string) {
	if el, ok := l.byKey[key]; ok {
		l.order.Remove(el)
		delete(l.byKey, key)
	}
}

// len is how many keys this holds.
func (l *lru) len() int { return l.order.Len() }

// standing is what one pair has against it.
type standing struct {
	// fails are the pair's failures inside the window, in the order they
	// were recorded, the latest [pairKeep] of them.
	fails []time.Time

	// pending are the start instants of attempts admitted and not yet
	// resolved, each counted as a failure until it is.
	pending []time.Time
}

// weight is how many failures count against this pair at now, pending
// attempts included, and the latest instant among them.
func (s *standing) weight(now time.Time) (int, time.Time) {
	s.prune(now.Add(-Window))
	var last time.Time
	for _, at := range s.fails {
		if at.After(last) {
			last = at
		}
	}
	for _, at := range s.pending {
		if at.After(last) {
			last = at
		}
	}
	return len(s.fails) + len(s.pending), last
}

// prune drops the failures that have aged out of the window.
func (s *standing) prune(cut time.Time) {
	kept := s.fails[:0]
	for _, at := range s.fails {
		if at.After(cut) {
			kept = append(kept, at)
		}
	}
	clear(s.fails[len(kept):])
	s.fails = kept
}

// empty reports a standing that can delay nobody: no failure in the window and
// nothing pending.
func (s *standing) empty(now time.Time) bool {
	s.prune(now.Add(-Window))
	return len(s.fails) == 0 && len(s.pending) == 0
}

// pend counts an admitted attempt that starts at.
func (s *standing) pend(at time.Time) { s.pending = append(s.pending, at) }

// unpend resolves one admitted attempt that started at.
func (s *standing) unpend(at time.Time) {
	for i, p := range s.pending {
		if p.Equal(at) {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			return
		}
	}
}

// fail records a failure, keeping the newest [pairKeep].
func (s *standing) fail(at time.Time) {
	s.fails = append(s.fails, at)
	if over := len(s.fails) - pairKeep; over > 0 {
		s.fails = append(s.fails[:0], s.fails[over:]...)
	}
}

// PairsHeld is how many pairs this throttle holds, for the suite that asserts
// the bound.
//
// EXPORTED FOR A TEST AND SAYING SO: the bound is the property, and the only
// way to see it hold is to count what is left after more keys than it admits
// have been met.
func PairsHeld(t *Throttle) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pairs.len()
}
