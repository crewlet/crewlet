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
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// THE ENUMERATION ORACLE, AND THE THREE THINGS THAT CLOSE IT.
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
// [Throttle.Decoy] is a fixed-cost HMAC run when there is no verifier to check
// — so the two arms DO the same shape of work rather than one of them doing
// none and returning immediately.
//
// It is an HMAC and NOT an argon2id derivation, which looks like the weaker
// choice and is not: a decoy that ran the real password cost would let an
// unauthenticated stranger spend [Memory] and a hundred milliseconds of this
// node's budget per request against names that do not exist, which is a denial
// of service dressed as a defence. The decoy exists to make the two arms
// similar in SHAPE; what makes them indistinguishable in TIME is the pad.
//
// # 3. Both arms are padded to ONE deadline, measured from admission
//
// [Throttle.Pad] sleeps until a fixed interval after the instant the request
// was ADMITTED — the last instant before anything about the subject is looked
// up — not after the verification started, and not for a fixed duration. That
// is the only one of the three that actually equalises the timing: argon2id's
// own cost varies with load and with how many verifications are queued behind
// [VerifyCap], and a decoy's does not vary at all, so without a common
// deadline the two curves are different shapes however similar their means.
// Admission rather than arrival, because admission can include the curve's own
// delay: that delay is the same for a name that exists and one that does not,
// and a deadline it had already spent would leave the verification after it
// unpadded.
//
// WHAT IT DOES NOT PROMISE: under enough load to push a real verification past
// the deadline, the pad has nothing left to add and the arms separate again.
// That is stated rather than hidden — at that point every request is slow, the
// node is already at its verify cap, and the leak is one an attacker has to
// generate a load spike to open.

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
// names from one address — every pair is fresh. What bounds it is the password
// floor and the blocklist, [VerifyCap] on the argon2id work a real name costs,
// and the pad on every answer; what makes it seen is the audit trail's
// per-client, per-minute failure tally, which counts the distinct names a
// client tried.
//
// AN ATTEMPT THAT NAMES NOBODY IS NOT COUNTED — an invitation link, a
// founder's one-time code, a provider's round trip. There is no subject to key
// a pair on, and each of those credentials is minted with 256 bits of
// crypto/rand, so there is nothing a curve would slow; keyed on the source it
// was a way for a stranger to hold a company's provider sign-ins shut, since a
// callback nobody started fails for free.
//
// # What the fleet holds, and what it does not
//
// The PAIR's window is seeded from [coord.Attempts] — read once when a node
// first meets the pair, written while its curve is still climbing, flushed by
// its success — so a run moved by the load balancer to another node starts
// that node's curve where the fleet left it. Nothing the fleet holds is what
// was typed: a pair is a keyed digest, and the key never leaves the
// deployment's keyring.
//
// # Concurrency is paid for, too
//
// An attempt that is admitted and not yet resolved counts against its pair as
// a failure until it resolves: a burst of concurrent guesses at one account is
// then served one after another along the curve rather than all at once. A
// success refunds it; a request that verified nothing releases it.

// PadDeadline is how long after admission BOTH arms of an authentication
// answer.
//
// 400 ms, and it is derived from the one cost it has to cover: an argon2id
// verification at [Memory] and [Time] measures in the low hundreds of
// milliseconds on the hardware this engine runs on, so the deadline has to sit
// above that or the pad is doing nothing on the arm that matters. It is also
// the figure a person perceives as "it thought about it" rather than "it is
// broken" — a sign-in is one request, once, and a fifth of a second either way
// is invisible where it would be intolerable on a page load.
//
// CHANGING [Memory] OR [Time] MOVES THIS. They are a pair: raise the cost
// without raising the deadline and the real arm overruns the pad on every
// request, which is the leak the pad exists to close, silently, with every
// test still passing.
const PadDeadline = 400 * time.Millisecond

// DegradeInterval is how often an unreachable attempts store is reported.
//
// ONCE PER [coord.AttemptWindow], because the alternative is a log line per
// request at exactly the moment the coordination store is already unwell — a
// guessing run against a degraded fleet would write the log that fills the
// disk. One line per window says the same thing and says it once.
const DegradeInterval = coord.AttemptWindow

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
	// to [DelayCeiling]: 1, 2, 4, 8, 16, then 30 seconds. A pair's fleet window is written only while its curve is
	// still climbing — a seventh failure changes no node's answer — so
	// this is also the most writes one pair costs the fleet per window.
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

	// LocalKeys is how many pairs one throttle holds, the least recently
	// used forgotten past it: 16384. Every pair costs at most
	// [CurveSteps] instants, so the bound is a few megabytes of memory an
	// unauthenticated caller cannot grow — where a map walked on every
	// failure was both unbounded and O(keys) per write. A pair forgotten
	// early is re-seeded from the fleet the next time it is met.
	LocalKeys = 16384
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

// Throttle is the sign-in curve with the two timing defences around it.
//
// SAFE FOR CONCURRENT USE.
type Throttle struct {
	attempts coord.Attempts
	key      []byte
	decoy    []byte
	deadline time.Duration
	now      func() time.Time
	sleep    func(context.Context, time.Duration)
	logger   *slog.Logger

	mu       sync.Mutex
	pairs    *keyed
	delayed  int
	degraded time.Time
}

// ThrottleDeps is what a throttle is built from.
type ThrottleDeps struct {
	// Attempts is the fleet's window the pair curve is seeded from. NIL IS
	// A REAL POSTURE, not a misconfiguration: a single node with no
	// coordination backend still throttles on its own curve.
	Attempts coord.Attempts

	// Key digests a (subject, source) pair before it is held anywhere,
	// here or in the fleet. REQUIRED with Attempts, and it must be the
	// same on every node — the deployment's keyring is where it comes
	// from — or two nodes would keep one pair under two names and seed
	// nothing from each other. Without Attempts, empty takes a key this
	// process generates, which is correct because nothing else ever
	// compares a digest this process made.
	Key []byte

	// Deadline is the pad. Zero takes [PadDeadline].
	Deadline time.Duration

	Now    func() time.Time
	Sleep  func(context.Context, time.Duration)
	Logger *slog.Logger
}

// NewThrottle builds one.
func NewThrottle(deps ThrottleDeps) (*Throttle, error) {
	if deps.Attempts != nil && len(deps.Key) == 0 {
		return nil, errors.New("credential: a throttle seeded from the fleet " +
			"needs a digest key every node shares; without one each node would " +
			"keep a pair under a name of its own and learn nothing from the " +
			"fleet")
	}
	return build(deps), nil
}

// build is [NewThrottle] past its one refusal.
func build(deps ThrottleDeps) *Throttle {
	key := deps.Key
	if len(key) == 0 {
		key = randomKey()
	}
	t := &Throttle{
		attempts: deps.Attempts, key: key,
		// THE DECOY KEY IS PER-PROCESS AND RANDOM, which is correct here
		// and would be wrong for anything that verifies across nodes:
		// nothing compares a decoy result to anything, on this node or
		// any other. It exists to be COMPUTED, never to be checked, so
		// the only property it needs is that it cost what a real HMAC
		// costs.
		decoy:    randomKey(),
		deadline: deps.Deadline,
		now:      deps.Now, sleep: deps.Sleep, logger: deps.Logger,
		pairs: newKeyed(LocalKeys),
	}
	if t.deadline <= 0 {
		t.deadline = PadDeadline
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.sleep == nil {
		t.sleep = sleepUntil
	}
	if t.logger == nil {
		t.logger = slog.Default()
	}
	return t
}

// ThrottleKeyDomain separates the throttle's digest key from every other thing
// the fleet's keyring derives: a pair digest is never a session signature, a
// run token or a seal, and a key derived for one is never the key of another.
const ThrottleKeyDomain = "crewlet/iam/throttle/v1"

// PairKey is the digest key every node of a fleet derives from the keyring's
// ACTIVE entry for [ThrottleDeps.Key], or false where the keyring names none.
//
// THE ACTIVE KEY AND NOT THE WHOLE RING, because every node must derive the
// same one and the active entry is the one they agree on. Flipping the active
// key moves every pair to a new name, so a window's worth of counts is left
// under the old ones to age out — one window of a curve forgotten per keyring
// rotation, which is the price of never holding a pair under a key the fleet
// does not share.
func PairKey(m runtoken.Material) ([]byte, bool) {
	for _, key := range m.Keys {
		if key.ID == m.ActiveID {
			return runtoken.DeriveKey(ThrottleKeyDomain, key.ID, key.Material), true
		}
	}
	return nil, false
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
// It is called before the subject resolves. The PAIR is decided on this
// node's own count first, with no I/O, so a pair already past [InlineDelay]
// costs this node a map lookup and the fleet nothing; then it is seeded from
// the fleet the first time this node meets it, and the attempt waits up to
// [InlineDelay] inside this call. A longer wait is a [*Throttled].
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
	pair := t.pairOf(a.Subject, sourceKeyOf(a.Source))

	// ON THIS NODE'S OWN COUNT FIRST, before anything is read from
	// anywhere: a pair this node already refuses costs the fleet nothing.
	t.mu.Lock()
	wait := t.waitLocked(pair, t.now())
	t.mu.Unlock()
	if wait > InlineDelay {
		return nil, &Throttled{RetryAfter: wait}
	}
	t.seed(ctx, pair)

	t.mu.Lock()
	now := t.now()
	wait = t.waitLocked(pair, now)
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

// Fail resolves the attempt as a credential that did not prove itself.
//
// Recorded against the pair, whose fleet window is written only while its
// curve is still climbing: a failure past [CurveSteps] changes no node's
// answer, and a write per attempt is a broker denial of service an
// unauthenticated caller would be pricing.
func (k *Ticket) Fail(ctx context.Context) {
	if k == nil || !k.done.CompareAndSwap(false, true) {
		return
	}
	t := k.t
	t.mu.Lock()
	at := t.now()
	if at.Before(k.at) {
		at = k.at
	}
	p := t.pairs.take(k.pair)
	p.unpend(k.at)
	// A STEP IS A FAILURE THE CURVE HAD NOT YET REACHED ITS CEILING
	// BEFORE — counted before this one lands, because a pair keeps only as
	// many instants as its curve has steps and its count after a failure
	// never reads past the last one.
	prior, _ := p.weight(at)
	p.fail(at, pairKeep)
	write := t.attempts != nil && prior < CurveSteps
	t.mu.Unlock()
	if write {
		if err := t.attempts.Fail(ctx, k.pair, at); err != nil {
			t.degrade(err)
		}
	}
}

// Succeed resolves the attempt as a credential that proved itself: its pair is
// forgotten, here and in the fleet — and NOTHING ELSE is, which is what keeps
// an account holder from wiping the record of their guesses at somebody
// else's by signing in as themselves between them.
func (k *Ticket) Succeed(ctx context.Context) {
	if k == nil || !k.done.CompareAndSwap(false, true) {
		return
	}
	t := k.t
	t.mu.Lock()
	// A PAIR THIS NODE NO LONGER HOLDS IS FLUSHED ANYWAY: admission took
	// it, so it is gone only because the bound forgot it, and what the
	// fleet has under it is then unknown here rather than nothing — left,
	// it would delay this person's next attempt on every other node for a
	// failure their success has answered.
	p := t.pairs.get(k.pair)
	flush := t.attempts != nil &&
		(p == nil || len(p.fails) > 0 || p.seeded.Count > 0)
	t.pairs.drop(k.pair)
	t.mu.Unlock()
	if flush {
		if err := t.attempts.Flush(ctx, k.pair); err != nil {
			t.degrade(err)
		}
	}
}

// Release resolves an attempt that never reached a verdict — a body that did
// not parse, a store that could not be read, a request that went away — so it
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
		t.pairs.tidy(k.pair, p, t.now())
	}
}

// Decoy does the work the real arm would have done, for a subject that does
// not exist.
//
// ITS RESULT IS DISCARDED BY CONSTRUCTION — it returns nothing — because a
// decoy whose answer a caller could branch on would be a second oracle. What
// it produces is TIME and nothing else.
func (t *Throttle) Decoy(presented string) {
	mac := hmac.New(sha256.New, t.decoy)
	mac.Write([]byte(presented))
	// The sum is taken and dropped. A compiler that elided the whole call
	// would reopen the gap, which is why the digest is written into a
	// package-level sink rather than left as an unused value.
	sink.Store(mac.Sum(nil))
}

// sink is where a decoy's digest goes, so the computation cannot be optimised
// away as dead. A [sync.Map]-free atomic pointer, because nothing ever reads
// it and a mutex here would serialise the one path that must not queue.
var sink atomicBytes

// Pad sleeps until deadline after the instant the request was admitted, so
// both arms of an authentication answer at the same moment.
//
// MEASURED FROM ADMISSION, which is the whole of it — see this file's head. A
// fixed sleep added AFTER the work leaks the work's duration unchanged; a
// deadline measured from when verification started leaks how long the lookup
// before it took. Admission is the last instant both arms share.
//
// IT DOES NOT EXTEND A REQUEST THAT ALREADY OVERRAN. Sleeping a negative
// duration is a no-op, and the arms separate — see this file's head for why
// that residue is stated rather than closed.
func (t *Throttle) Pad(ctx context.Context, admitted time.Time) {
	t.sleep(ctx, t.deadline-t.now().Sub(admitted))
}

// Deadline is the pad this throttle uses, for a caller that has to report it.
func (t *Throttle) Deadline() time.Duration { return t.deadline }

// Now is this throttle's clock, which is the instant a caller measures its pad
// from once an attempt is admitted.
func (t *Throttle) Now() time.Time { return t.now() }

// waitLocked is how long an attempt on pair must wait at now. Held under the
// lock.
func (t *Throttle) waitLocked(pair string, now time.Time) time.Duration {
	return max(t.pairs.next(pair, now).Sub(now), 0)
}

// seed merges the fleet's window for pair into this node's, the first time
// this node meets the pair.
//
// ONCE, AND FAILING OPEN. A pair already met is answered from here, and a read
// the fleet cannot answer is recorded as seeded-with-nothing: the local curve
// goes on applying from what this node has seen, and asking again on every
// attempt while coordination is down would put its latency under every
// sign-in. An unreachable store is reported once per window.
func (t *Throttle) seed(ctx context.Context, pair string) {
	if t.attempts == nil {
		return
	}
	t.mu.Lock()
	known := t.pairs.get(pair) != nil
	t.mu.Unlock()
	if known {
		return
	}
	window, err := t.attempts.Failures(ctx, pair, t.now())
	if err != nil {
		t.degrade(err)
		window = coord.Attempted{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := t.pairs.take(pair); !p.fromFleet {
		p.fromFleet, p.seeded = true, window
	}
}

// pairOf is the key a (typed subject, source) pair is held under — here and
// in the fleet: a keyed digest, never the value.
//
// THE VALUE IS WHAT WAS TYPED, and a password typed into the login box is what
// lands there often enough to matter. Held in the clear for a window, or
// hashed without a key, it would be recoverable from this process's memory or
// from the fleet's bucket against the company's own roster in one pass.
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

// degrade reports an unreachable attempts store, at most once per window.
func (t *Throttle) degrade(err error) {
	now := t.now()
	t.mu.Lock()
	quiet := now.Sub(t.degraded) < DegradeInterval
	if !quiet {
		t.degraded = now
	}
	t.mu.Unlock()
	if quiet {
		return
	}
	t.logger.Warn("credential_throttle_degraded",
		"error", err.Error(),
		"detail", "the fleet's failed-authentication window could not be "+
			"read or written, so this node is throttling on its own curve "+
			"alone: a guessing run moved to another node starts that node's "+
			"curve from nothing until coordination recovers")
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

// ---- the local curve --------------------------------------------------------- //

// keyed is the pairs' standings, the least recently used forgotten past its
// bound.
type keyed struct {
	bound int
	byKey map[string]*list.Element // of *standing
	order *list.List               // most recently used at the front
}

func newKeyed(bound int) *keyed {
	return &keyed{bound: bound, byKey: map[string]*list.Element{}, order: list.New()}
}

// get is key's standing, or nil, marking it used.
func (k *keyed) get(key string) *standing {
	el, ok := k.byKey[key]
	if !ok {
		return nil
	}
	k.order.MoveToFront(el)
	return el.Value.(*standing)
}

// take is key's standing, made if there is none — forgetting the least
// recently used key when that would pass the bound.
func (k *keyed) take(key string) *standing {
	if s := k.get(key); s != nil {
		return s
	}
	if k.order.Len() >= k.bound {
		oldest := k.order.Back()
		k.order.Remove(oldest)
		delete(k.byKey, oldest.Value.(*standing).key)
	}
	s := &standing{key: key}
	k.byKey[key] = k.order.PushFront(s)
	return s
}

// drop forgets key.
func (k *keyed) drop(key string) {
	if el, ok := k.byKey[key]; ok {
		k.order.Remove(el)
		delete(k.byKey, key)
	}
}

// tidy forgets key once nothing it holds can delay anybody.
func (k *keyed) tidy(key string, s *standing, now time.Time) {
	if s.empty(now) {
		k.drop(key)
	}
}

// next is the earliest instant key's next attempt may start — the zero instant
// when it owes no wait. A key nobody has met owes nothing.
func (k *keyed) next(key string, now time.Time) time.Time {
	s := k.get(key)
	if s == nil {
		return time.Time{}
	}
	count, last := s.weight(now)
	delay := delayFor(count)
	if delay == 0 {
		return time.Time{}
	}
	return last.Add(delay)
}

// len is how many keys this holds.
func (k *keyed) len() int { return k.order.Len() }

// standing is what one key has against it.
type standing struct {
	key string

	// fails are this node's failures inside the window, oldest first and
	// the newest at most keep of them.
	fails []time.Time

	// pending are the start instants of attempts admitted and not yet
	// resolved, each counted as a failure until it is.
	pending []time.Time

	// seeded is the fleet's window as it stood when this node first met
	// the pair, and fromFleet whether that read has happened.
	seeded    coord.Attempted
	fromFleet bool
}

// weight is how many failures count against this key at now, pending
// attempts included, and the latest instant among them.
//
// THE FLEET'S SEED AND THIS NODE'S OWN ARE NEVER ADDED: this node's failures
// were written to the fleet before a later seed could read them, so the larger
// of the two is the count and their sum would count one failure twice.
func (s *standing) weight(now time.Time) (int, time.Time) {
	cut := now.Add(-coord.AttemptWindow)
	s.prune(cut)
	count := len(s.fails)
	var last time.Time
	if count > 0 {
		last = s.fails[count-1]
	}
	if s.seeded.Count > 0 && s.seeded.Last.After(cut) {
		count = max(count, s.seeded.Count)
		if s.seeded.Last.After(last) {
			last = s.seeded.Last
		}
	}
	for _, at := range s.pending {
		count++
		if at.After(last) {
			last = at
		}
	}
	return count, last
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

// empty reports a standing that can delay nobody: no failure in the window,
// nothing pending, and nothing the fleet said that is still in it.
func (s *standing) empty(now time.Time) bool {
	cut := now.Add(-coord.AttemptWindow)
	s.prune(cut)
	return len(s.fails) == 0 && len(s.pending) == 0 &&
		(s.seeded.Count == 0 || !s.seeded.Last.After(cut))
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

// fail records a failure, keeping the newest keep of them.
func (s *standing) fail(at time.Time, keep int) {
	s.fails = append(s.fails, at)
	if over := len(s.fails) - keep; over > 0 {
		s.fails = append(s.fails[:0], s.fails[over:]...)
	}
}

// LocalKeysHeld is how many pairs this throttle's own curve holds, for the
// suite that asserts the bound.
//
// EXPORTED FOR A TEST AND SAYING SO: the bound is the property, and the only
// way to see it hold is to count what is left after more keys than it admits
// have been met.
func LocalKeysHeld(t *Throttle) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pairs.len()
}
