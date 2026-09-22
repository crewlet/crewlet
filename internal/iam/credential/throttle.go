package credential

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE ENUMERATION ORACLE, AND THE THREE THINGS THAT CLOSE IT.
//
// A sign-in endpoint is the one surface an unauthenticated stranger may
// address, and EVERYTHING about how it answers is evidence. The failure this
// file exists for is not a guessed password — it is an attacker learning WHO
// WORKS HERE, in as many requests as they care to make, from nothing but how
// long each one took or which of them was throttled.
//
// # 1. Admission happens before the subject resolves
//
// [Throttle.Admit] takes the request's SOURCE and knows nothing at all about
// who it claims to be. A throttle keyed on the subject is one that only REAL
// subjects can trigger, so the 429 becomes the oracle it was added to prevent:
// an attacker submits six attempts per name and reads the roster off which
// names start refusing.
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
// # 3. Both arms are padded to ONE deadline, measured from arrival
//
// [Throttle.Pad] sleeps until a fixed interval after the instant the request
// ARRIVED — not after the verification started, and not for a fixed duration.
// That is the only one of the three that actually equalises the timing:
// argon2id's own cost varies with load and with how many verifications are
// queued behind [VerifyCap], and a decoy's does not vary at all, so without a
// common deadline the two curves are different shapes however similar their
// means.
//
// WHAT IT DOES NOT PROMISE: under enough load to push a real verification past
// the deadline, the pad has nothing left to add and the arms separate again.
// That is stated rather than hidden — at that point every request is slow, the
// node is already at its verify cap, and the leak is one an attacker has to
// generate a load spike to open.

// AdmitLimit is how many failed authentications one source may make inside
// [coord.AttemptWindow] before it is refused.
//
// SIX, which is the number a person gets wrong before they reach for a
// password manager, and far below the fifteen-minute rate a guessing run needs
// to make progress against even a weak twelve-character password. It is under
// [coord.AttemptCap] deliberately: the count a caller reads has to be a real
// count rather than a saturation, or a throttle that refuses at the cap can
// never tell "sixteen attempts" from "a hundred".
const AdmitLimit = 6

// PadDeadline is how long after arrival BOTH arms of an authentication
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

// ErrThrottled reports a source that has failed too often.
//
// ITS OWN SENTINEL, because the caller's answer is a 429 rather than the
// generic refusal — and because it is the ONE refusal in this package that
// says something true about the request rather than nothing at all. It is
// safe to be specific precisely because it is keyed on the source: a stranger
// learns that they have been rate-limited, which they already knew.
var ErrThrottled = errors.New("credential: too many failed attempts")

// Throttle is the fleet's failed-authentication window with the two timing
// defences around it.
//
// SAFE FOR CONCURRENT USE.
type Throttle struct {
	attempts coord.Attempts
	decoy    []byte
	deadline time.Duration
	limit    int
	now      func() time.Time
	sleep    func(context.Context, time.Duration)
	logger   *slog.Logger

	mu       sync.Mutex
	local    map[string][]time.Time
	degraded time.Time
}

// ThrottleDeps is what a throttle is built from.
type ThrottleDeps struct {
	// Attempts is the fleet's window. NIL IS A REAL DEPLOYMENT, not a
	// misconfiguration: a single node with no coordination backend still
	// has to throttle, and it does so on the local curve alone.
	Attempts coord.Attempts

	// Limit is the refusal threshold. Zero takes [AdmitLimit].
	Limit int

	// Deadline is the pad. Zero takes [PadDeadline].
	Deadline time.Duration

	Now    func() time.Time
	Sleep  func(context.Context, time.Duration)
	Logger *slog.Logger
}

// NewThrottle builds one.
func NewThrottle(deps ThrottleDeps) (*Throttle, error) {
	// THE DECOY KEY IS PER-PROCESS AND RANDOM, which is correct here and
	// would be wrong for anything that verifies across nodes: nothing
	// compares a decoy result to anything, on this node or any other. It
	// exists to be COMPUTED, never to be checked, so the only property it
	// needs is that it cost what a real HMAC costs.
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("credential: read the decoy key: %w", err)
	}
	t := &Throttle{
		attempts: deps.Attempts, decoy: key,
		deadline: deps.Deadline, limit: deps.Limit,
		now: deps.Now, sleep: deps.Sleep, logger: deps.Logger,
		local: map[string][]time.Time{},
	}
	if t.deadline <= 0 {
		t.deadline = PadDeadline
	}
	if t.limit <= 0 {
		t.limit = AdmitLimit
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
	return t, nil
}

// Admit reports whether a request from source may be attempted at all.
//
// IT IS CALLED BEFORE ANYTHING IS LOOKED UP. The source is whatever identifies
// the caller — a client address, a proxy-resolved one — and never the login
// they typed: see this file's head for what keying on the subject costs.
func (t *Throttle) Admit(ctx context.Context, source string) error {
	if source == "" {
		// AN UNIDENTIFIABLE SOURCE IS ADMITTED, and that is the safe
		// direction rather than the lax one. Refusing would mean a
		// misconfigured proxy — one that strips the header this is
		// derived from — locks every person in the company out at once,
		// which is an outage the throttle caused. What still bounds
		// that caller is the password cost and the verify cap.
		return nil
	}
	now := t.now()
	count, err := t.failures(ctx, source, now)
	if err != nil {
		// FAILS OPEN, which is coord.Attempts' own documented polarity
		// and worth restating where it is relied on: a throttle that
		// closed on an unreachable store would lock every operator out
		// of /config, /secrets and the dashboard — the one surface an
		// incident is fixed from — at the moment coordination is
		// already unwell. What is left is the local curve below, plus a
		// constant-time comparison against a high-entropy secret, which
		// is what this is defence in depth over rather than a
		// replacement for.
		t.degrade(err)
		count = t.localCount(source, now)
	}
	if count >= t.limit {
		return fmt.Errorf("%w: %d failures from this source inside %s",
			ErrThrottled, count, coord.AttemptWindow)
	}
	return nil
}

// Fail records one failed authentication against source.
//
// AGAINST THE SOURCE AND NOT THE SUBJECT, for [Throttle.Admit]'s reason. It
// records LOCALLY as well as in the fleet, always — the local curve is not a
// fallback that switches on, it is a second counter that is simply also there,
// so a node whose coordination store fails mid-run does not start from zero.
func (t *Throttle) Fail(ctx context.Context, source string) {
	if source == "" {
		return
	}
	now := t.now()
	t.recordLocal(source, now)
	if t.attempts == nil {
		return
	}
	if _, err := t.attempts.Fail(ctx, source, now); err != nil {
		t.degrade(err)
	}
}

// Flush forgets every attempt against source, which is what a SUCCESSFUL
// authentication does — fleet-wide, so a lockout earned on one node is lifted
// on all of them by the credential that proves the caller is not who the
// throttle was protecting against.
func (t *Throttle) Flush(ctx context.Context, source string) {
	if source == "" {
		return
	}
	t.mu.Lock()
	delete(t.local, source)
	t.mu.Unlock()
	if t.attempts == nil {
		return
	}
	if err := t.attempts.Flush(ctx, source); err != nil {
		t.degrade(err)
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

// Pad sleeps until deadline after the instant the request arrived, so both
// arms of an authentication answer at the same moment.
//
// MEASURED FROM ARRIVAL, which is the whole of it. A fixed sleep added AFTER
// the work leaks the work's duration unchanged; a deadline measured from when
// verification started leaks how long the lookup before it took. Arrival is
// the only instant both arms share.
//
// IT DOES NOT EXTEND A REQUEST THAT ALREADY OVERRAN. Sleeping a negative
// duration is a no-op, and the arms separate — see this file's head for why
// that residue is stated rather than closed.
func (t *Throttle) Pad(ctx context.Context, arrived time.Time) {
	t.sleep(ctx, t.deadline-t.now().Sub(arrived))
}

// Deadline is the pad this throttle uses, for a caller that has to report it.
func (t *Throttle) Deadline() time.Duration { return t.deadline }

// failures reads the fleet's count, or reports that it could not.
func (t *Throttle) failures(ctx context.Context, source string, now time.Time) (int, error) {
	if t.attempts == nil {
		return t.localCount(source, now), nil
	}
	return t.attempts.Failures(ctx, source, now)
}

// recordLocal appends one attempt to this node's own curve, dropping what has
// aged out of the window and capping the slice at [coord.AttemptCap] for the
// reason that constant gives.
func (t *Throttle) recordLocal(source string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := prune(t.local[source], now)
	kept = append(kept, now)
	if len(kept) > coord.AttemptCap {
		kept = kept[len(kept)-coord.AttemptCap:]
	}
	t.local[source] = kept
	// THE MAP IS BOUNDED BY PRUNING EVERY SOURCE, not by an LRU: an
	// unauthenticated caller can otherwise grow it one entry per address
	// they can spoof, which is a memory exhaustion reached through the
	// throttle itself. Pruning on write is O(sources) on a map whose
	// entries all expire within the window, so it stays small by
	// construction rather than by a cap somebody has to choose.
	for key, seen := range t.local {
		if key == source {
			continue
		}
		if left := prune(seen, now); len(left) == 0 {
			delete(t.local, key)
		} else {
			t.local[key] = left
		}
	}
}

// localCount is this node's own count inside the window.
func (t *Throttle) localCount(source string, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := prune(t.local[source], now)
	if len(kept) == 0 {
		delete(t.local, source)
	} else {
		t.local[source] = kept
	}
	return len(kept)
}

// prune drops the attempts that have aged out of the window.
func prune(seen []time.Time, now time.Time) []time.Time {
	cut := now.Add(-coord.AttemptWindow)
	kept := seen[:0]
	for _, at := range seen {
		if at.After(cut) {
			kept = append(kept, at)
		}
	}
	return kept
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
			"read, so this node is throttling on its own count alone: a "+
			"guessing run spread across nodes counts once per node rather "+
			"than once per fleet until coordination recovers")
}

// sleepUntil is the default pad, cancellable so a shutting-down node does not
// hold a request open for the deadline.
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

// LocalSources is how many sources this throttle's own curve is holding, for
// the suite that asserts the map is bounded.
//
// EXPORTED FOR A TEST AND SAYING SO: the bound is the pruning rather than a
// cap, so the only way to see it working is to count what is left after every
// entry has aged out — and a map that grows one entry per address an
// unauthenticated caller can spoof is a memory exhaustion reached through the
// throttle itself.
func LocalSources(t *Throttle) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.local)
}
