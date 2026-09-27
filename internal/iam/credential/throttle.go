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
// [Hasher.Decoy] spends the turn a verification would when there is no
// verifier to check — in the same source's lane, holding a slot of the verify
// cap to its end for as long as a derivation takes, drawn from the node's own
// recent derivations at its current cost — so the two arms do the same shape
// of work rather than one of them doing none and returning immediately, and
// wait for the same things. It derives nothing once the hasher has measured a
// derivation: holding the slot costs no memory and no CPU, and a stranger
// with no real name to try costs no more than one who has one — one slot, in
// their own turn (turns.go).
//
// # 3. Both arms are padded to ONE deadline, measured from admission
//
// [Throttle.Pad] sleeps until a fixed interval after the instant the request
// was ADMITTED — the last instant before anything about the subject is looked
// up — not after the verification started, and not for a fixed duration. That
// is the one of the three that actually equalises the timing: argon2id's own
// cost varies with load, and a decoy's hold is a measure of it rather than the
// thing itself, so without a common deadline the two arms are different shapes
// however similar their means. Admission rather than arrival, because
// admission can include the curve's own delay: that delay is the same for a
// name that exists and one that does not, and a deadline it had already spent
// would leave the verification after it unpadded.
//
// WHAT IT DOES NOT PROMISE: under enough load to push a real verification past
// the deadline, the pad has nothing left to add. The turn a decoy takes keeps
// the two arms queueing alike even then, so what separates them is only how
// far a derivation now is from the recent ones a decoy draws its hold from —
// and what generates that load is at least [VerifyCap] sources each holding
// its one turn, because one address holds one slot however much it sends.
// Nor does a decoy stand in for a verifier at a HIGHER cost than this build's
// — one a newer build of a rolling upgrade wrote — which takes longer than
// anything this node measures, and exists only until the rollout finishes.

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
// names from one address — every pair is fresh. What bounds it is the turn:
// every verification and every decoy a source causes waits for that source's
// one turn at the verify cap (turns.go), so one address is served one name per
// derivation, however many it sends at once, and never ahead of another
// address's turn; and its fleet cost is its allowance of fresh pairs
// ([FreshPairBurst]). Past that, the password floor and the blocklist. What
// makes it seen is the audit trail's per-client, per-minute failure tally,
// which counts the distinct names a client tried.
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
// drive it. It lifts on the success that completes a sign-in, and reaching its
// ceiling is announced, because it means somebody holding a person's password
// is guessing at their second factor.
//
// AN ATTEMPT THAT NAMES NOBODY IS NOT COUNTED — an invitation link, a
// founder's one-time code, a provider's round trip. There is no subject to key
// a pair on, and each of those credentials is minted with 256 bits of
// crypto/rand, so there is nothing a curve would slow; keyed on the source it
// was a way for a stranger to hold a company's provider sign-ins shut, since a
// callback nobody started fails for free.
//
// # The fleet shares the pair, at every step
//
// A load balancer puts each attempt of a guessing run on whichever node it
// likes, so a curve each node kept to itself would be one the attacker divides
// by the number of nodes without knowing it. So a pair's curve is the FLEET's:
// every failure is written to [coord.Attempts], a success flushes it, and a
// node READS the pair's record before each attempt it admits on a pair that is
// already climbing — that is, before every step of the curve — and counts, on
// top of what it read, only its own failures the record could not yet have
// held. A clean pair is read when a node first meets it and not again for the
// window while it stays clean, so an honest sign-in pays one read, and its
// second-factor step none.
//
// What that costs the store is bounded by the curve itself rather than by
// whoever is sending: a pair whose wait this node already knows is past
// [InlineDelay] is refused with no round trip at all, so a pair is read at
// most once per attempt the curve admits and written once per failure. Every
// failure is written, the ceiling's included: the fleet's newest failure is
// what every node measures a pair's wait from, and a record that stopped at
// the sixth left every node timing the ceiling from its own last failure, so a
// round-robin run was admitted once per node per thirty seconds rather than
// once.
//
// No curve bounds a spray of FRESH names, though — every one is a pair nobody
// has met — and each cost a read and, failing, a write, as fast as one source
// could type them. So a source may ask the fleet about pairs this node holds
// no record of only as fast as its ALLOWANCE refills ([FreshPairBurst],
// [FreshPairEvery]); past it a fresh pair is decided on this node's count and
// its failure kept here, which costs a name tried once its fleet-wide view and
// nothing else — the pair's next attempt finds a record here and is shared
// again, and a success on a pair never asked about still clears the fleet.
//
// EVERY INSTANT IN IT IS THE WRITING NODE'S CLOCK, and a node whose clock runs
// fast stamps its failures into every reader's future. So a reader takes none
// of the fleet's failures as later than its own now, and none of its own as
// later than the start it could have scheduled — each re-dated once, where it
// is first found past that — and no clock anywhere can stretch a wait past the
// ceiling: taken as written, one failure on a node ten minutes fast was a
// ten-minute 429 for that login on every other node. See [standing.weight].
//
// Nothing the fleet holds is what was typed: a pair is a keyed digest, and the
// key never leaves the deployment's keyring.
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

// Window is how long a failure counts against its key: the fleet's attempts
// window, which every node counts a curve over, so a caller that has to say
// "once per curve" — one row per person whose second factor reached its
// ceiling — says it over the same span.
const Window = coord.AttemptWindow

// DegradeInterval is how often an unreachable attempts store is reported.
//
// ONCE PER [coord.AttemptWindow], because the alternative is a log line per
// request at exactly the moment the coordination store is already unwell — a
// guessing run against a degraded fleet would write the log that fills the
// disk. One line per window says the same thing and says it once.
const DegradeInterval = coord.AttemptWindow

// FleetRetry is how long a throttle leaves the attempts store alone after it
// failed to answer: no read and no failure written until it has passed.
//
// A round trip to a store that is down costs whoever is waiting on it the
// store's own timeout, and a read now precedes every step of every climbing
// curve — so without a pause an outage would put that timeout under every
// sign-in that follows a mistake. THIRTY SECONDS, the curve's own
// [DelayCeiling]: a store that answers again is back in use within one
// ceiling's wait, so no pair at full strength takes more than one attempt on
// this node's count alone, and one that stays down costs one failing round
// trip per half minute rather than one per attempt. A success's flush is
// still attempted, because it is what lifts a person's wait on every other
// node and it happens once per sign-in.
const FleetRetry = DelayCeiling

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

	// FreshPairBurst is how many pairs this node holds no record of that
	// one source's attempts may ask the fleet about at once: sixteen, a
	// room signing in together. Each is a read of the pair's window and,
	// if the attempt fails, the write of its failure — so a source typing a
	// new name every time costs the coordination store at most this, and
	// then what [FreshPairEvery] refills, rather than two round trips per
	// name at whatever rate it can send.
	FreshPairBurst = 16

	// FreshPairEvery is how often one more comes back: a second, the
	// curve's own [DelayFloor] — the fastest one account's run is allowed
	// its second attempt, and far above the rate any address signs honest
	// people in at. An honest sign-in past it loses nothing: a pair that
	// nobody here has met owes no wait either way, and its success clears
	// the fleet's record regardless ([Ticket.Succeed]).
	FreshPairEvery = time.Second

	// LocalKeys is how many pairs one throttle holds, the least recently
	// used forgotten past it: 16384 — and as many sources' allowances. Every pair costs at most
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

// Throttle is the sign-in curve, and the pad that makes both arms of a sign-in
// answer at one deadline.
//
// SAFE FOR CONCURRENT USE.
type Throttle struct {
	attempts coord.Attempts
	key      []byte
	deadline time.Duration
	now      func() time.Time
	sleep    func(context.Context, time.Duration)
	logger   *slog.Logger

	mu       sync.Mutex
	pairs    *keyed[standing]
	sources  *keyed[allowance]
	delayed  int
	degraded time.Time
	// quietUntil is when the attempts store may be asked again after it
	// failed to answer. See [FleetRetry].
	quietUntil time.Time
}

// ThrottleDeps is what a throttle is built from.
type ThrottleDeps struct {
	// Attempts is the fleet's window every pair's curve is shared through.
	// NIL IS A REAL POSTURE, not a misconfiguration: a single node with no
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
		deadline: deps.Deadline,
		now:      deps.Now, sleep: deps.Sleep, logger: deps.Logger,
		pairs:   newKeyed(LocalKeys, func() *standing { return &standing{} }),
		sources: newKeyed(LocalKeys, func() *allowance { return &allowance{} }),
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
// costs this node a map lookup and the fleet nothing; then, unless the pair is
// clean and was read inside the window, the fleet's record of it is read and
// the wait decided again on both, and the attempt waits up to [InlineDelay]
// inside this call. A longer wait is a [*Throttled].
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
	source := sourceKeyOf(a.Source)
	return t.admit(ctx, t.pairOf(a.Subject, source), source)
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
// decided on a curve keyed on the person, shared across the fleet like every
// pair, and every address's guesses climb it together.
//
// It is keyed on who the login RESOLVED to, which is exactly what the pair's
// curve must never be (this file's head), and the difference is who can reach
// it: this is asked only once the password has proved itself, so the only
// caller who can drive it holds the password, and it tells them nothing about
// who exists that the password did not. Its digest is taken in a domain of its
// own, so no typed subject can ever name it.
//
// NOT ON THE SOURCE'S ALLOWANCE ([FreshPairBurst]): a person's curve is met
// only past their password, so what it costs the fleet is bounded by the
// people whose passwords are known to somebody guessing, not by what a stranger
// can type. An empty person is admitted uncounted.
func (t *Throttle) AdmitSecondFactor(ctx context.Context, person string) (*Ticket, error) {
	if person == "" {
		return nil, nil
	}
	return t.admit(ctx, t.factorOf(person), "")
}

// admit is [Throttle.Admit] and [Throttle.AdmitSecondFactor] past the key: the
// curve on key, whose fresh-pair reads are charged to source's allowance, or
// to none where source is empty.
func (t *Throttle) admit(ctx context.Context, pair, source string) (*Ticket, error) {
	// ON THIS NODE'S OWN COUNT FIRST, before anything is read from
	// anywhere: a pair this node already refuses costs the fleet nothing.
	t.mu.Lock()
	now := t.now()
	wait := t.waitLocked(pair, now)
	var read *pendingRead
	if wait <= InlineDelay {
		read = t.issueRead(pair, source, now)
	}
	t.mu.Unlock()
	if wait > InlineDelay {
		return nil, &Throttled{RetryAfter: wait}
	}
	if read != nil {
		t.refresh(ctx, read)
	}

	t.mu.Lock()
	now = t.now()
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
// Recorded against the pair, here and in the fleet — every failure, the
// ceiling's included, because the fleet's newest failure is what every node
// measures the pair's wait from; see this file's head for why that costs the
// store no more than the curve admits. A failure the store did not
// acknowledge stays this node's own, and is counted on top of whatever the
// fleet says until a read could have seen it — and so does one on a FRESH
// pair its source's allowance could not pay to ask about ([FreshPairBurst]):
// the read it went without would have been paid for with this write.
//
// It reports whether the key's curve is now AT ITS CEILING — [CurveSteps]
// failures inside the window, as this node counts them — which a caller whose
// key is a person reads as somebody guessing at them ([Throttle.AdmitSecondFactor]).
func (k *Ticket) Fail(ctx context.Context) (ceiling bool) {
	if k == nil || !k.done.CompareAndSwap(false, true) {
		return false
	}
	t := k.t
	t.mu.Lock()
	at := t.now()
	if at.Before(k.at) {
		at = k.at
	}
	p := t.pairs.take(k.pair)
	p.unpend(k.at)
	seq := p.fail(at)
	count, _ := p.weight(t.now())
	ceiling = count >= CurveSteps
	write := t.attempts != nil && !at.Before(t.quietUntil) && !p.unasked
	t.mu.Unlock()
	if !write {
		return ceiling
	}
	if err := t.attempts.Fail(ctx, k.pair, at); err != nil {
		t.unanswered(ctx, err)
		return ceiling
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur := t.pairs.get(k.pair); cur == p {
		p.shared(seq)
	}
	return ceiling
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
	// A PAIR WHOSE FLEET RECORD THIS NODE DOES NOT KNOW IS FLUSHED ANYWAY
	// — one the bound forgot after admission took it, and one admitted
	// without a read, because its source's allowance was spent or the store
	// was being left alone. Unknown is not nothing: left, what the fleet has
	// under it would delay this person's next attempt on every other node
	// for a failure their success has answered. Only a pair read clean and
	// failed nowhere since costs nothing, which is an honest sign-in's.
	p := t.pairs.get(k.pair)
	flush := t.attempts != nil &&
		(p == nil || p.readIndex == 0 || len(p.fails) > 0 || p.fleet.Count > 0)
	t.pairs.drop(k.pair)
	t.mu.Unlock()
	if flush {
		if err := t.attempts.Flush(ctx, k.pair); err != nil {
			t.unanswered(ctx, err)
		}
	}
}

// Release resolves an attempt that never reached a verdict — a body that did
// not parse, a store that could not be read, a request that went away, a
// password that proved itself where a second factor is still to come — so it
// counts as nothing. A no-op once the ticket is resolved, so it is what a
// caller defers.
//
// THE PAIR'S READ IS KEPT: the attempt that follows a released one is almost
// always the same person's next step — the code after the password — and a
// clean pair read inside the window needs no second round trip.
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
//
// NEVER MORE THAN [DelayCeiling] PAST THE LATER OF NOW AND THIS NODE'S NEWEST
// ADMITTED ATTEMPT, whatever any clock says — so never more than
// [InlineDelay] plus [DelayCeiling] in all, and never more than the ceiling on
// the fleet's word alone: the curve measures from the newest failure it knows,
// the fleet's is taken to be no later than now, and this node's own no later
// than the start it could have scheduled ([standing.weight]). That start may
// be ahead of now, because it is when this node scheduled the attempt to
// begin; the lead is what serialises a burst at one pair, and an admission
// never schedules one further ahead than [InlineDelay].
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

// pendingRead is one read of the fleet's record of a pair, issued and not yet
// answered.
type pendingRead struct {
	pair  string
	into  *standing
	index uint64
	at    time.Time
}

// issueRead is the read an attempt on pair, from source, must make before its
// wait is decided, or nil when none is needed. Held under the lock.
//
// NONE WHERE NOTHING COULD HAVE MOVED IT: a pair this node has read inside the
// window and found clean — nothing in the fleet's record, and no failure here
// since — is answered from here, which is what spares an honest sign-in's
// second step a round trip. EVERY OTHER PAIR IS READ, because another node may
// have failed on it since this one last looked, and a curve decided on a
// stale count is the curve divided by the number of nodes. None while the
// store is being left alone after failing to answer ([FleetRetry]).
//
// AND A FRESH PAIR ONLY AS FAST AS ITS SOURCE'S ALLOWANCE REFILLS. A pair this
// node holds no record of is what every new name a guessing run types
// arrives as, and no curve slows that: without the allowance one source drove
// a read per name, and a write per name that failed, at whatever rate it
// could send. One the allowance cannot pay for is decided on this node's
// count — which for a pair nobody here has met is nothing — and marked
// UNASKED, so its failure stays here too ([Ticket.Fail]). The pair's next
// attempt finds a record here and is read like any other, so a run at one
// account is shared at every step after its first on each node, whatever its
// source has spent; what the allowance withholds is the fleet's view of names
// a source tries once.
func (t *Throttle) issueRead(pair, source string, now time.Time) *pendingRead {
	if t.attempts == nil || now.Before(t.quietUntil) {
		return nil
	}
	fresh := t.pairs.get(pair) == nil
	s := t.pairs.take(pair)
	if source != "" && (fresh || s.untouched()) {
		if !t.sources.take(source).spend(now) {
			s.unasked = true
			return nil
		}
	} else if s.clean(now) {
		return nil
	}
	s.unasked = false
	s.reads++
	return &pendingRead{pair: pair, into: s, index: s.reads, at: now}
}

// refresh performs read and folds what the fleet said into the pair it was
// issued for.
//
// FAILING OPEN: a store that cannot answer leaves the pair on this node's own
// count and is left alone for [FleetRetry] — asking again on every attempt
// while coordination is down would put its timeout under every sign-in. An
// unreachable store is reported once per window.
//
// A READ THAT LOST A RACE CHANGES NOTHING. Two attempts on one pair may each
// issue a read, and the later-issued one is the one that can have seen more:
// an answer older than the one already held is dropped, and so is one for a
// standing the bound has since forgotten.
func (t *Throttle) refresh(ctx context.Context, read *pendingRead) {
	window, err := t.attempts.Failures(ctx, read.pair, read.at)
	if err != nil {
		t.unanswered(ctx, err)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if cur := t.pairs.get(read.pair); cur == read.into {
		cur.learn(window, read.index, read.at)
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

// factorOf is the key a person's second-factor curve is held under — here and
// in the fleet: a keyed digest of the person's id, in a domain no pair's digest
// shares, so no subject anybody types names it.
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

// unanswered is an attempts store call that failed: the store's fault unless
// the request it was made for went away first.
//
// A CALLER HANGING UP IS NOT AN OUTAGE. The call runs on the request's
// context, so a client that disconnects mid-read cancels it — and read as the
// store failing, one closed tab would leave the whole node deciding every
// curve on its own count for [FleetRetry].
func (t *Throttle) unanswered(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	t.degrade(err)
}

// degrade leaves an unreachable attempts store alone for [FleetRetry], and
// reports it at most once per window.
func (t *Throttle) degrade(err error) {
	now := t.now()
	t.mu.Lock()
	t.quietUntil = now.Add(FleetRetry)
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

// keyed is a bounded set of records by key, the least recently used forgotten
// past its bound — the pairs' standings and the sources' allowances, each of
// which an unauthenticated caller grows one key at a time, so each is bounded
// rather than a map walked or kept for ever.
type keyed[T any] struct {
	bound int
	fresh func() *T
	byKey map[string]*list.Element // of *entry[T]
	order *list.List               // most recently used at the front
}

type entry[T any] struct {
	key string
	val *T
}

func newKeyed[T any](bound int, fresh func() *T) *keyed[T] {
	return &keyed[T]{bound: bound, fresh: fresh,
		byKey: map[string]*list.Element{}, order: list.New()}
}

// get is key's record, or nil, marking it used.
func (k *keyed[T]) get(key string) *T {
	el, ok := k.byKey[key]
	if !ok {
		return nil
	}
	k.order.MoveToFront(el)
	return el.Value.(*entry[T]).val
}

// take is key's record, made if there is none — forgetting the least recently
// used key when that would pass the bound.
func (k *keyed[T]) take(key string) *T {
	if v := k.get(key); v != nil {
		return v
	}
	if k.order.Len() >= k.bound {
		oldest := k.order.Back()
		k.order.Remove(oldest)
		delete(k.byKey, oldest.Value.(*entry[T]).key)
	}
	v := k.fresh()
	k.byKey[key] = k.order.PushFront(&entry[T]{key: key, val: v})
	return v
}

// drop forgets key.
func (k *keyed[T]) drop(key string) {
	if el, ok := k.byKey[key]; ok {
		k.order.Remove(el)
		delete(k.byKey, key)
	}
}

// len is how many keys this holds.
func (k *keyed[T]) len() int { return k.order.Len() }

// standing is what one pair has against it.
type standing struct {
	// fails are this node's failures inside the window, oldest first and
	// the newest at most [pairKeep] of them.
	fails []failure

	// pending are the start instants of attempts admitted and not yet
	// resolved, each counted as a failure until it is.
	pending []time.Time

	// fleet is the fleet's record of the pair as the newest read to land
	// found it, readIndex which read that was (zero: none has landed),
	// and readAt when it was issued. reads is how many have been issued.
	fleet     coord.Attempted
	readIndex uint64
	readAt    time.Time
	reads     uint64

	// fleetRaw is the newest instant the fleet's record carried as it
	// was written, before [standing.weight] re-dated one it found ahead —
	// kept so a record re-read unchanged keeps its first dating.
	fleetRaw time.Time

	// seq numbers this node's failures, so a write acknowledged after the
	// lock was let go can find the one it recorded.
	seq uint64

	// unasked marks a pair admitted without a read because its source's
	// allowance was spent: its failures are kept here and not written,
	// until an attempt that finds a record here reads it again.
	unasked bool
}

// untouched reports a standing that holds nothing at all — no failure, nothing
// pending, and no read ever issued: a pair this node has met only by name.
func (s *standing) untouched() bool {
	return len(s.fails) == 0 && len(s.pending) == 0 && s.reads == 0
}

// failure is one failed attempt on this node.
type failure struct {
	at  time.Time
	seq uint64

	// sharedFrom is the index of the first read that could hold this
	// failure — the one issued after the store acknowledged it — or zero
	// while it is unacknowledged. A read issued before the acknowledgement
	// may or may not have seen it, so it is counted on top of that read's
	// answer: the strict direction, one step at most, and only in a race.
	sharedFrom uint64
}

// weight is how many failures count against this pair at now, pending
// attempts included, and the latest instant among them.
//
// NO FAILURE IS FURTHER AHEAD OF THIS NODE'S CLOCK THAN IT COULD HAVE BEEN.
// The curve measures its wait from the newest instant it knows, and an instant
// ahead of where it could be is a clock that was wrong — a peer's that runs
// fast, or this node's own before it stepped back: taken as recorded, one
// failure from a node ten minutes fast owed ten minutes here, which is no
// ceiling at all. How far ahead an instant may be is where it came from:
//
//   - THE FLEET'S: not at all. A peer's failure had happened by the time the
//     store answered, so one dated after now is the peer's clock.
//   - THIS NODE'S OWN: up to [InlineDelay]. An attempt is dated no earlier
//     than the start this node scheduled for it ([Ticket.Fail]), which is up
//     to [InlineDelay] ahead of the clock at its admission, and an admitted
//     attempt still pending counts from that start.
//
// An instant past its bound is RE-DATED to the bound the first time it is
// found there, in place, and not merely capped on each look: capped afresh
// every time, it would stay "just now" for as long as the skew lasted, and a
// pair at the ceiling would owe thirty seconds on every attempt for ten
// minutes. Together that is [Throttle.waitLocked]'s bound.
//
// THE FLEET'S RECORD PLUS THIS NODE'S FAILURES IT CANNOT HOLD YET, never the
// larger of the two: the record is every node's failures, this node's that it
// had acknowledged before the read was issued among them, so those are
// counted once through it; every other failure here — made after the read, or
// never acknowledged — is on top of it. Taking the larger held a run moved to
// this node at the count it arrived with until this node's own failures
// overtook it.
func (s *standing) weight(now time.Time) (int, time.Time) {
	cut := now.Add(-coord.AttemptWindow)
	s.prune(cut)
	if s.fleet.Last.After(now) {
		s.fleet.Last = now
	}
	var count int
	var last time.Time
	if s.readIndex > 0 && s.fleet.Count > 0 && s.fleet.Last.After(cut) {
		count, last = s.fleet.Count, s.fleet.Last
	}
	lead := now.Add(InlineDelay)
	for i := range s.fails {
		f := &s.fails[i]
		if f.at.After(lead) {
			f.at = lead
		}
		if !s.holds(*f) {
			count++
		}
		if f.at.After(last) {
			last = f.at
		}
	}
	for _, at := range s.pending {
		count++
		if at.After(lead) {
			at = lead
		}
		if at.After(last) {
			last = at
		}
	}
	return count, last
}

// holds reports whether the fleet's record as last read already counts f.
func (s *standing) holds(f failure) bool {
	return f.sharedFrom != 0 && s.readIndex >= f.sharedFrom
}

// clean reports a pair read inside the window and found with nothing against
// it, fleet or local: one whose read still answers for it.
func (s *standing) clean(now time.Time) bool {
	cut := now.Add(-coord.AttemptWindow)
	s.prune(cut)
	return s.readIndex > 0 && s.readAt.After(cut) && len(s.fails) == 0 &&
		(s.fleet.Count == 0 || !s.fleet.Last.After(cut))
}

// learn folds a read that landed into the pair: the newest answer wins, and a
// local failure that answer already counts is forgotten here, since the
// record holds it and its instant.
//
// A RECORD READ BACK UNCHANGED KEEPS THE DATING IT WAS GIVEN. Every instant
// in the fleet's record is the WRITING node's clock, and one a fast writer
// stamped into this node's future is re-dated to the moment it is first
// found ahead ([standing.weight]); the next read carries the same instant
// again, and taken as written it would be found ahead — and re-dated to that
// later moment — on every read, so the wait it earns would never run out
// while the skew lasted. A writer running SLOW needs nothing: its failures
// fall inside the window and count, and only their wait has already passed.
func (s *standing) learn(window coord.Attempted, index uint64, at time.Time) {
	if index <= s.readIndex {
		return
	}
	raw := window.Last
	if s.readIndex > 0 && raw.Equal(s.fleetRaw) {
		window.Last = s.fleet.Last
	}
	s.fleetRaw = raw
	s.fleet, s.readIndex, s.readAt = window, index, at
	kept := s.fails[:0]
	for _, f := range s.fails {
		if !s.holds(f) {
			kept = append(kept, f)
		}
	}
	clear(s.fails[len(kept):])
	s.fails = kept
}

// prune drops the failures that have aged out of the window.
func (s *standing) prune(cut time.Time) {
	kept := s.fails[:0]
	for _, f := range s.fails {
		if f.at.After(cut) {
			kept = append(kept, f)
		}
	}
	clear(s.fails[len(kept):])
	s.fails = kept
}

// empty reports a standing that can delay nobody and spare no read: no
// failure in the window, nothing pending, and no read still answering for it.
func (s *standing) empty(now time.Time) bool {
	cut := now.Add(-coord.AttemptWindow)
	s.prune(cut)
	return len(s.fails) == 0 && len(s.pending) == 0 &&
		(s.readIndex == 0 || !s.readAt.After(cut))
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

// fail records a failure, keeping the newest [pairKeep], and answers its
// number.
func (s *standing) fail(at time.Time) uint64 {
	s.seq++
	s.fails = append(s.fails, failure{at: at, seq: s.seq})
	if over := len(s.fails) - pairKeep; over > 0 {
		s.fails = append(s.fails[:0], s.fails[over:]...)
	}
	return s.seq
}

// shared marks failure seq acknowledged by the store: every read issued from
// now on holds it.
func (s *standing) shared(seq uint64) {
	for i := range s.fails {
		if s.fails[i].seq == seq {
			s.fails[i].sharedFrom = s.reads + 1
			return
		}
	}
}

// allowance is what one source may still ask the fleet about pairs this node
// holds no record of: [FreshPairBurst] at once, one more every
// [FreshPairEvery].
type allowance struct {
	left float64
	at   time.Time
}

// spend takes one from the allowance at now, reporting false when there is
// none. A clock that stepped back refills nothing until it has caught up.
func (a *allowance) spend(now time.Time) bool {
	if a.at.IsZero() {
		a.left, a.at = FreshPairBurst, now
	}
	if gone := now.Sub(a.at); gone > 0 {
		a.left = min(FreshPairBurst, a.left+float64(gone)/float64(FreshPairEvery))
		a.at = now
	}
	if a.left < 1 {
		return false
	}
	a.left--
	return true
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
