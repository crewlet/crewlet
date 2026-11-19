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
// A sign-in's timing and throttling are evidence: the failure is WHO WORKS HERE.
//
// 1. KEYED ON WHAT WAS TYPED, never on what it resolved to. [Throttle.Admit]
// decides before any lookup. A throttle keyed on the resolved subject is one
// only real subjects trigger, so the roster reads off which names slow down;
// keyed on the typed value, a name nobody holds climbs the curve as a real one
// does. (Two spellings of one person are two keys, a curve each.)
//
// 2. A SUBJECT THAT DOES NOT EXIST IS STILL VERIFIED AGAINST: [Hasher.Decoy]
// makes a miss one argon2id derivation under the same cap as a hit.

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

// THE CURVE, AND WHY IT IS NEVER A LOCKOUT.
//
// A hard refusal after N failures is a lockout an outsider can cause: keyed on
// a login it locks the company's only administrator out for ever at under a
// request a minute. So a failure costs DELAY: each doubles the wait before its
// key's next attempt, from [DelayFloor] to [DelayCeiling], and a correct
// credential after the wait succeeds. A wait up to [InlineDelay] is served
// inside the request; a longer one is answered 429 with the time left, the only
// specific answer this surface gives.
const (
	// DelayFloor is the wait a pair's first failure costs: one second,
	// which is invisible to a person who mistyped and is already a
	// sixty-fold cut in what a script gets through.
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

	// MaxPairs is how many pairs' failures one throttle holds, the least
	// recently used forgotten past it: 16384. A pair costs at most [pairKeep]
	// failure instants plus its digest and place, about half a kilobyte, so
	// memory an unauthenticated caller can grow stays under ten megabytes.
	//
	// A PAIR FORGOTTEN EARLY STARTS ITS CURVE AGAIN, which is why the bound is
	// this high: to push one climbing pair out, a guesser must fail 16384
	// others inside [Window], each an argon2id derivation under the verify cap:
	// minutes of a whole node's cap, every sign-in queueing behind them.
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

// Throttle is the sign-in curve. SAFE FOR CONCURRENT USE.
//
// THE KEY IS THE PAIR: the subject as typed, from one source, with no allowance.
// A success clears THIS PAIR only, so holding any account cannot wipe the record
// of guesses at another's.
//
// NEVER THE SOURCE ALONE: a refusal decided on an address is one ANYBODY SHARING
// IT (a NAT, a VPN egress, behind an untrusted proxy the whole internet) holds
// shut for the rest, and a gate nobody can hold shut never refuses. One password
// at many names from one address is bounded by COST (a derivation per name, the
// floor, the blocklist) and made seen by the audit trail's per-source count.
type Throttle struct {
	key   []byte
	now   func() time.Time
	sleep func(context.Context, time.Duration)

	mu    sync.Mutex
	pairs *lru

	// pending are the start instants of attempts admitted and not yet
	// resolved, by pair, each counted against its pair as a failure until
	// it resolves.
	//
	// APART FROM pairs, and that is the bound's whole meaning: forgetting
	// a climbing pair early has to cost a guesser [MaxPairs] FAILURES,
	// each a verification. Filed in the bounded memory at admission, a
	// burst of fresh names that were never verified — admitted, then
	// abandoned before their turn at the verify cap — evicted a climbing
	// pair for the price of the requests alone. This map needs no bound
	// of its own: an entry is an attempt in flight, so it is bounded by
	// the requests this node holds open, and it is gone when they are.
	pending map[string][]time.Time

	delayed int
}

// ThrottleDeps is what a throttle is built from. Every field is optional.
type ThrottleDeps struct {
	// Now is the clock. Nil takes [time.Now], whose monotonic reading keeps a
	// wall-clock step (NTP, a VM migration) from stretching or ending a wait,
	// so a production caller leaves it nil. Nothing held is what was typed: a
	// pair is a keyed digest under a key this process never wrote anywhere.
	Now func() time.Time

	// Sleep is how a wait is served. Nil takes a sleep the request's
	// context cancels.
	Sleep func(context.Context, time.Duration)
}

// NewThrottle builds one.
//
// EACH NODE KEEPS ITS OWN CURVE, decided rather than overlooked. A balancer
// spreads a guessing run over N nodes, each counting only what it saw, so the
// run is admitted up to N times as often as on one. Sharing the curve through
// the coordination store put a round trip on the sign-in path and a subsystem
// of its own to buy a factor of N on a rate the curve already cuts sixty-fold
// at its first step, and nothing on the single node most deployments are. A
// run at one account is bounded as on one node, times N: 30 s per attempt per
// node at the ceiling, each an argon2id verification under the verify cap,
// against a password of twelve characters off the blocklist and, where held, a
// second factor.
func NewThrottle(deps ThrottleDeps) *Throttle {
	t := &Throttle{
		key: randomKey(), now: deps.Now, sleep: deps.Sleep,
		pairs: newLRU(MaxPairs), pending: map[string][]time.Time{},
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
// It runs before the subject resolves and decides on the pair's count alone,
// with no I/O. The attempt waits up to [InlineDelay] inside the call; a longer
// wait is a [*Throttled]. The caller resolves the [Ticket] exactly once:
// [Ticket.Fail], [Ticket.Succeed], or [Ticket.Release] (safe to defer).
//
// ADMITTED UNCOUNTED, with a nil ticket: an attempt that names nobody (no pair
// to key, an invitation's 256-bit secret gives a curve nothing to slow) and an
// UNIDENTIFIABLE source, the safe direction: refusing would let a proxy that
// strips the source header lock everybody out. The password cost and verify
// cap still bound both.
func (t *Throttle) Admit(ctx context.Context, a Attempt) (*Ticket, error) {
	if a.Source == "" || a.Subject == "" {
		return nil, nil
	}
	return t.admit(ctx, t.pairOf(a.Subject, SourceKey(a.Source)))
}

// AdmitSecondFactor decides whether a second factor may be checked for person,
// on the PERSON'S OWN CURVE whatever address the code arrives from.
//
// On the pair's curve alone somebody holding the password is barely slowed:
// each address is a fresh pair (65 thousand in an IPv6 /48) and a six-digit
// code falls in about an hour. A code is ALSO decided on a curve keyed on the
// person, which all addresses climb together; a completed sign-in lifts it.
//
// Keying on who the login RESOLVED to is what the pair's curve must never do
// (this file's head); it is safe here because it is asked only past the
// password, so only a caller holding it can drive it. Its digest has its own
// domain, so no typed subject names it. An empty person is admitted uncounted.
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
	t.pend(pair, start)
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
//
// Until resolved it counts against its pair as a failure, so a burst of
// concurrent guesses at one account is served one after another along the
// curve. A success refunds it; a request that verified nothing releases it.
type Ticket struct {
	t    *Throttle
	pair string
	at   time.Time
	done atomic.Bool
}

// Fail resolves the attempt as a credential that did not prove itself, and
// records the failure against its pair.
//
// It reports whether THIS failure took the curve to its ceiling ([CurveSteps]
// failures in the window, where there had been fewer), which a caller whose key
// is a person ([Throttle.AdmitSecondFactor]) announces as somebody guessing.
//
// THE TRANSITION, NOT THE STATE, so the curve says "once": a run held at the
// ceiling keeps [CurveSteps] failures, each replacing the oldest, and reaches
// it again only after aging back down. The state would announce every failure.
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
	t.unpend(k.pair, k.at)
	p := t.pairs.take(k.pair)
	p.prune(now.Add(-Window))
	before := len(p.fails)
	p.fail(at)
	return before < CurveSteps && len(p.fails) >= CurveSteps
}

// Succeed resolves the attempt as a credential that proved itself: its pair's
// failures are forgotten — and NOTHING ELSE is, which is what keeps an account
// holder from wiping the record of their guesses at somebody else's by signing
// in as themselves between them. Other attempts at the pair still in flight
// go on counting until they resolve themselves.
func (k *Ticket) Succeed() {
	if k == nil || !k.done.CompareAndSwap(false, true) {
		return
	}
	t := k.t
	t.mu.Lock()
	defer t.mu.Unlock()
	t.unpend(k.pair, k.at)
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
	t.unpend(k.pair, k.at)
	if p := t.pairs.get(k.pair); p != nil && p.empty(t.now()) {
		t.pairs.drop(k.pair)
	}
}

// pend counts an admitted attempt on pair that starts at. Held under the lock.
func (t *Throttle) pend(pair string, at time.Time) {
	t.pending[pair] = append(t.pending[pair], at)
}

// unpend resolves one admitted attempt on pair that started at, forgetting the
// pair's entry once nothing of it is in flight. Held under the lock.
func (t *Throttle) unpend(pair string, at time.Time) {
	held := t.pending[pair]
	for i, p := range held {
		if p.Equal(at) {
			held = append(held[:i], held[i+1:]...)
			break
		}
	}
	if len(held) == 0 {
		delete(t.pending, pair)
		return
	}
	t.pending[pair] = held
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
	var count int
	var last time.Time
	if s := t.pairs.get(pair); s != nil {
		count, last = s.weight(now)
	}
	for _, at := range t.pending[pair] {
		count++
		if at.After(last) {
			last = at
		}
	}
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

// SourceKey is the unit one caller's attempts are counted under: the address,
// and for IPv6 its /64. It is the source half of a pair's key and the source a
// failure is counted under in the audit trail's per-minute row
// (internal/iam/authevents): ONE FOLD, because a row counting per address while
// the curve counted per /64 let one host cycling addresses in its /64 take
// every source the row names within a minute, folding other callers' failures
// into the anonymous overflow.
//
// THE /64 IS WHAT ONE IPv6 CUSTOMER IS GIVEN: keyed per address, a run from one
// host rotates through 2^64 fresh pairs and its curve never starts. A value
// that is not an address (a test's name, or whatever a misconfigured proxy
// forwarded) is keyed as it is.
func SourceKey(source string) string {
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

// standing is what one pair's failures are.
type standing struct {
	// fails are the pair's failures inside the window, in the order they
	// were recorded, the latest [pairKeep] of them.
	fails []time.Time
}

// weight is how many failures count against this pair at now, and the latest
// of them. Its attempts in flight are the throttle's to add.
func (s *standing) weight(now time.Time) (int, time.Time) {
	s.prune(now.Add(-Window))
	var last time.Time
	for _, at := range s.fails {
		if at.After(last) {
			last = at
		}
	}
	return len(s.fails), last
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

// empty reports a standing with no failure left in the window.
func (s *standing) empty(now time.Time) bool {
	s.prune(now.Add(-Window))
	return len(s.fails) == 0
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
