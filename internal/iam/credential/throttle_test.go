package credential_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam/credential"
)

// --- the rig ----------------------------------------------------------------- //

// attempts is a coord.Attempts that counts what it is asked and can be made
// unreachable.
type attempts struct {
	mu      sync.Mutex
	records map[string][]time.Time
	err     error
	reads   int
	writes  int
	flushes int
}

func newAttempts() *attempts { return &attempts{records: map[string][]time.Time{}} }

func (a *attempts) Fail(_ context.Context, subject string, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.writes++
	if a.err != nil {
		return a.err
	}
	a.records[subject] = append(a.records[subject], now)
	return nil
}

func (a *attempts) Failures(_ context.Context, subject string, now time.Time) (coord.Attempted, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	if a.err != nil {
		return coord.Attempted{}, a.err
	}
	var out coord.Attempted
	for _, at := range a.records[subject] {
		if at.After(now.Add(-coord.AttemptWindow)) {
			out.Count++
			if at.After(out.Last) {
				out.Last = at
			}
		}
	}
	return out, nil
}

func (a *attempts) Flush(_ context.Context, subject string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.flushes++
	if a.err != nil {
		return a.err
	}
	delete(a.records, subject)
	return nil
}

func (a *attempts) breaks(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
}

// io is how many round trips the fleet window has been asked for.
func (a *attempts) io() (reads, writes int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reads, a.writes
}

// subjects is every key the fleet window holds.
func (a *attempts) subjects() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.records))
	for s := range a.records {
		out = append(out, s)
	}
	return out
}

// clockOf is a movable clock shared by a throttle and its test.
type clockOf struct {
	mu sync.Mutex
	at time.Time
}

func (c *clockOf) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clockOf) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// counting records what each sleep was asked for — the pad's and the curve's
// — so the timing properties are asserted on the DECISION rather than on the
// wall clock, and a test that measured real sleeps would be the flakiest thing
// in this tree. It does not move the clock: a case moves it itself where the
// time a wait took is the point.
type counting struct {
	mu    sync.Mutex
	slept []time.Duration
}

func (c *counting) sleep(_ context.Context, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
}

func (c *counting) all() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

func (c *counting) take() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.slept
	c.slept = nil
	return out
}

// fleetKey is the digest key every node of a rig's fleet shares.
var fleetKey = []byte("a-key-every-node-of-this-fleet-shares-32")

func newThrottle(t *testing.T, a *attempts) (*credential.Throttle, *clockOf, *counting) {
	t.Helper()
	clock := &clockOf{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	return onClock(t, a, clock)
}

// onClock builds a node of the rig's fleet on a clock another node may share.
func onClock(t *testing.T, a *attempts, clock *clockOf) (*credential.Throttle, *clockOf, *counting) {
	t.Helper()
	pad := &counting{}
	deps := credential.ThrottleDeps{
		Now: clock.now, Sleep: pad.sleep,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if a != nil {
		deps.Attempts, deps.Key = a, fleetKey
	}
	th, err := credential.NewThrottle(deps)
	if err != nil {
		t.Fatalf("build a throttle: %v", err)
	}
	return th, clock, pad
}

// failOnce admits one attempt and fails it, reporting what the admission
// waited or refused with.
func failOnce(t *testing.T, th *credential.Throttle, a credential.Attempt) error {
	t.Helper()
	ticket, err := th.Admit(t.Context(), a)
	if err != nil {
		return err
	}
	ticket.Fail(t.Context())
	return nil
}

// --- the curve --------------------------------------------------------------- //

// A FAILURE COSTS DELAY, NEVER REFUSAL — AND NEVER A LOCKOUT.
//
// Each failure on a (subject, source) pair doubles the wait before the pair's
// next attempt, from one second to a ceiling of thirty. Up to five seconds it
// is served inside the request; past that the answer is a 429 naming the time
// left, and once that time has passed the attempt is admitted. It used to be a
// refusal at the sixth failure lasting the whole fifteen-minute window, which
// is a lockout anybody who can type a login can cause. Mutation: refuse at a
// count, and the attempt after the wait is still refused.
func TestAFailureCostsDelayAndNeverALockout(t *testing.T) {
	t.Parallel()
	th, clock, sleeps := newThrottle(t, newAttempts())
	who := credential.Attempt{Source: "203.0.113.7", Subject: "sarah.chen"}

	// The curve's own waits, each attempt made the moment the last was
	// refused or failed: 1s and 2s and 4s served in-process, the next
	// three refused with the time left.
	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second}
	for i, wait := range want {
		if err := failOnce(t, th, who); err != nil {
			t.Fatalf("attempt %d was refused (%v), want a %s wait served in "+
				"the request", i+1, err, wait)
		}
		got := sleeps.take()
		switch {
		case wait == 0 && len(got) != 0:
			t.Errorf("attempt %d waited %v, want none", i+1, got)
		case wait > 0 && (len(got) != 1 || got[0] != wait):
			t.Errorf("attempt %d waited %v, want %s", i+1, got, wait)
		}
		clock.advance(wait)
	}
	for _, left := range []time.Duration{8 * time.Second, 16 * time.Second,
		credential.DelayCeiling, credential.DelayCeiling} {

		_, err := th.Admit(t.Context(), who)
		if !errors.Is(err, credential.ErrThrottled) {
			t.Fatalf("an attempt owed %s answered %v, want throttled", left, err)
		}
		if got := credential.RetryAfter(err); got != left {
			t.Errorf("Retry-After %s, want the %s left", got, left)
		}
		// NEVER A LOCKOUT: the attempt after the wait is admitted.
		clock.advance(left)
		if err := failOnce(t, th, who); err != nil {
			t.Fatalf("the attempt after a %s wait was refused: %v — a "+
				"curve an outsider can drive must never close", left, err)
		}
	}
	// And a correct credential after the wait is simply admitted and
	// clears the pair.
	clock.advance(credential.DelayCeiling)
	ticket, err := th.Admit(t.Context(), who)
	if err != nil {
		t.Fatalf("the right password after the wait was refused: %v", err)
	}
	ticket.Succeed(t.Context())
	if _, err := th.Admit(t.Context(), who); err != nil {
		t.Errorf("a pair that just succeeded owes a wait: %v", err)
	}
}

// A NAME NOBODY HOLDS CLIMBS THE CURVE EXACTLY AS A REAL ONE DOES.
//
// The throttle is keyed on what was TYPED and never on what it resolved to,
// so there is nothing for it to answer differently about somebody who exists:
// a curve only real subjects could climb would make the curve itself the
// roster. Two spellings that reach one person — an address with a plus tag and
// in another case — are one curve, because the identity estate folds them to
// one person.
func TestTheCurveNeverDependsOnWhoExists(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{
		{"sarah.chen", "nobody.here"},
		{"Sarah.Chen@Example.com", "sarah.chen+work@example.com"},
	} {
		th, _, _ := newThrottle(t, newAttempts())
		first := credential.Attempt{Source: "203.0.113.7", Subject: pair[0]}
		second := credential.Attempt{Source: "203.0.113.7", Subject: pair[1]}
		for range 5 {
			if err := failOnce(t, th, first); err != nil &&
				!errors.Is(err, credential.ErrThrottled) {
				t.Fatal(err)
			}
		}
		_, a := th.Admit(t.Context(), first)
		if pair[0] == "sarah.chen" {
			// Two different names: the second owes nothing.
			ticket, err := th.Admit(t.Context(), second)
			if err != nil {
				t.Errorf("%s owes a wait for %s's failures: %v", pair[1], pair[0], err)
			}
			ticket.Release()
			if credential.RetryAfter(a) == 0 {
				t.Errorf("%s owes no wait after five failures", pair[0])
			}
			continue
		}
		_, b := th.Admit(t.Context(), second)
		if credential.RetryAfter(a) == 0 || credential.RetryAfter(a) != credential.RetryAfter(b) {
			t.Errorf("%s owes %s and %s owes %s: two spellings of one person "+
				"are two curves, so a plus tag is a fresh set of guesses",
				pair[0], credential.RetryAfter(a), pair[1], credential.RetryAfter(b))
		}
	}
}

// A SUCCESS CLEARS ITS OWN PAIR AND NOTHING ELSE.
//
// A success used to flush the whole SOURCE, so anybody holding an account could
// guess at somebody else's five times, sign in as themselves, and guess again
// with a clean record — for ever. Their own sign-in clears their own pair, and
// the target's pair keeps every failure. Mutation: clear every pair from the
// source on a success and the insider's fourth guess owes nothing.
func TestASuccessClearsOnlyItsOwnPair(t *testing.T) {
	t.Parallel()
	th, _, sleeps := newThrottle(t, newAttempts())
	target := credential.Attempt{Source: "198.51.100.9", Subject: "the.cfo"}
	self := credential.Attempt{Source: "198.51.100.9", Subject: "an.insider"}
	for range 3 {
		if err := failOnce(t, th, target); err != nil {
			t.Fatal(err)
		}
	}
	ticket, err := th.Admit(t.Context(), self)
	if err != nil {
		t.Fatalf("the insider's own sign-in was refused: %v", err)
	}
	ticket.Succeed(t.Context())
	if owed := owedBy(t, th, sleeps, target); owed == 0 {
		t.Error("after the insider's own sign-in their fourth guess at the " +
			"CFO owed nothing — a success wiped somebody else's record")
	}
}

// owedBy admits one attempt and reports the wait it owed — served in the
// request or named by a refusal — releasing it.
func owedBy(t *testing.T, th *credential.Throttle, sleeps *counting,
	a credential.Attempt) time.Duration {

	t.Helper()
	sleeps.take()
	ticket, err := th.Admit(t.Context(), a)
	if err != nil {
		return credential.RetryAfter(err)
	}
	ticket.Release()
	var owed time.Duration
	for _, d := range sleeps.take() {
		owed += d
	}
	return owed
}

// NO ADDRESS IS EVER REFUSED ON ITS OWN.
//
// Many people share one address — an office, a VPN's egress, and every caller
// behind a proxy the deployment was not told to trust — and a stranger among
// them shares it too. The source had a curve of its own: ten failures free,
// then a doubling wait, refused past five seconds. So a stranger who warmed it
// up and then failed once every twenty-five seconds, at any name, kept every
// colleague at that address at 429, their right passwords never compared. The
// curve is keyed on the (typed subject, source) PAIR alone, so the stranger's
// failures are theirs and a colleague signing in owes nothing — run here for
// ten simulated minutes, the colleague trying every second.
//
// Mutation: put the source's own curve back and the colleague is refused on
// every attempt after the warm-up.
func TestNoAddressIsEverRefusedOnItsOwn(t *testing.T) {
	t.Parallel()
	th, clock, sleeps := newThrottle(t, newAttempts())
	const office = "192.0.2.10"

	// THE WARM-UP: a spray across the directory from the shared address.
	for i := range 48 {
		if err := failOnce(t, th, credential.Attempt{Source: office,
			Subject: fmt.Sprintf("name.%d", i)}); err != nil {
			t.Fatalf("a spray's name %d was refused on the address alone: %v", i, err)
		}
	}
	if got := sleeps.take(); len(got) != 0 {
		t.Errorf("a spray across fresh names waited %v — every pair was new", got)
	}

	colleague := credential.Attempt{Source: office, Subject: "alice"}
	for second := range 600 {
		if second%25 == 0 {
			if err := failOnce(t, th, credential.Attempt{Source: office,
				Subject: fmt.Sprintf("stranger.%d", second)}); err != nil {
				t.Fatalf("the stranger's failure at %ds was refused: %v", second, err)
			}
		}
		ticket, err := th.Admit(t.Context(), colleague)
		if err != nil {
			t.Fatalf("the colleague's correct password at %ds was refused (%v) — a "+
				"stranger at the address held it shut", second, err)
		}
		ticket.Succeed(t.Context())
		clock.advance(time.Second)
	}
	if got := sleeps.take(); len(got) != 0 {
		t.Errorf("the colleague was made to wait %v for a stranger's failures", got)
	}
}

// HONEST SIGN-INS IN FLIGHT TOGETHER FROM ONE ADDRESS ARE NEVER HELD.
//
// A dozen people signing in at once from one office is a morning, not an
// attack. When an attempt still being checked counted against its SOURCE as a
// failure, the twelfth waited a second, the thirteenth three and the
// fourteenth and every one after it answered 429 — with nobody failing at
// all. Pending attempts count against their own pair, which serialises a burst
// at ONE account, and nowhere else.
//
// Mutation: count a pending attempt against its source and the requests past
// the eleventh wait or are refused.
func TestHonestSignInsInFlightTogetherAreNeverHeld(t *testing.T) {
	t.Parallel()
	th, _, sleeps := newThrottle(t, newAttempts())
	var tickets []*credential.Ticket
	for i := range 32 {
		ticket, err := th.Admit(t.Context(), credential.Attempt{
			Source: "192.0.2.10", Subject: fmt.Sprintf("person.%d", i)})
		if err != nil {
			t.Fatalf("sign-in %d of a morning was refused: %v", i, err)
		}
		tickets = append(tickets, ticket)
	}
	if got := sleeps.take(); len(got) != 0 {
		t.Errorf("honest sign-ins in flight together were held %v", got)
	}
	for _, ticket := range tickets {
		ticket.Succeed(t.Context())
	}
}

// A PAIR THIS NODE ALREADY REFUSES COSTS THE FLEET NOTHING.
//
// The pair is decided on this node's own count before anything is read, so a
// flood at a pair past its curve is refused with no round trip at all — an
// unauthenticated caller never prices a coordination read per request.
// Mutation: seed the pair before deciding it, and the refused attempts read.
func TestARefusedPairCostsTheFleetNothing(t *testing.T) {
	t.Parallel()
	store := newAttempts()
	th, _, _ := newThrottle(t, store)
	who := credential.Attempt{Source: "203.0.113.66", Subject: "sarah.chen"}
	for {
		if err := failOnce(t, th, who); err != nil {
			break
		}
	}
	reads, writes := store.io()
	for i := range 200 {
		if _, err := th.Admit(t.Context(), who); !errors.Is(err, credential.ErrThrottled) {
			t.Fatalf("attempt %d at a refused pair answered %v", i, err)
		}
	}
	if r, w := store.io(); r != reads || w != writes {
		t.Errorf("200 refused attempts cost %d reads and %d writes of the "+
			"fleet window, want none", r-reads, w-writes)
	}
}

// A PAIR IS READ ONCE, AND WRITTEN ONLY WHILE ITS CURVE IS CLIMBING.
//
// A node seeds a pair from the fleet the first time it meets it and never again
// while it holds it, and writes a failure only while the curve still moves:
// past [credential.CurveSteps] a failure changes no node's answer, and a write
// per attempt is a broker denial of service a stranger would be pricing.
func TestAPairIsReadOnceAndWrittenOnlyWhileItClimbs(t *testing.T) {
	t.Parallel()
	store := newAttempts()
	th, clock, _ := newThrottle(t, store)
	who := credential.Attempt{Source: "203.0.113.7", Subject: "sarah.chen"}
	for range 20 {
		if err := failOnce(t, th, who); err != nil {
			clock.advance(credential.RetryAfter(err))
			if err := failOnce(t, th, who); err != nil {
				t.Fatal(err)
			}
		}
		clock.advance(time.Second)
	}
	reads, writes := store.io()
	if reads != 1 {
		t.Errorf("twenty attempts on one pair read the fleet %d times, want once", reads)
	}
	if writes != credential.CurveSteps {
		t.Errorf("twenty failures wrote the fleet %d times, want %d — one per "+
			"step the curve climbs", writes, credential.CurveSteps)
	}
}

// A RUN MOVED TO ANOTHER NODE STARTS WHERE THE FLEET LEFT IT.
//
// A load balancer puts a guessing run on whichever node it likes. The second
// node meets the pair for the first time, reads the fleet's window, and owes
// the wait the first node's failures earned — from the newest of them, which
// is why the window answers it. Mutation: seed from a count alone and date it
// to the read, and the second node's wait runs from now instead.
func TestARunMovedToAnotherNodeStartsWhereTheFleetLeftIt(t *testing.T) {
	t.Parallel()
	store := newAttempts()
	clock := &clockOf{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	first, _, _ := onClock(t, store, clock)
	second, _, _ := onClock(t, store, clock)
	who := credential.Attempt{Source: "203.0.113.7", Subject: "sarah.chen"}
	for range 4 {
		if err := failOnce(t, first, who); err != nil {
			clock.advance(credential.RetryAfter(err))
			if err := failOnce(t, first, who); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Four failures: the next attempt owes eight seconds from the last.
	clock.advance(2 * time.Second)
	_, err := second.Admit(t.Context(), who)
	if got := credential.RetryAfter(err); got != 6*time.Second {
		t.Errorf("the second node owes %s (%v), want the 6s left of the 8s "+
			"the first node's four failures earned", got, err)
	}
}

// THE FLEET NEVER HOLDS WHAT WAS TYPED.
//
// A password typed into the login box is what lands in the subject often
// enough to matter. Held in the clear, or hashed without a key, it is
// recoverable from the coordination store against the company's own roster in
// one pass.
func TestTheFleetNeverHoldsWhatWasTyped(t *testing.T) {
	t.Parallel()
	store := newAttempts()
	th, _, _ := newThrottle(t, store)
	typed := "correct horse battery staple"
	if err := failOnce(t, th, credential.Attempt{Source: "203.0.113.7",
		Subject: typed}); err != nil {
		t.Fatal(err)
	}
	unkeyed := sha256.Sum256([]byte(typed))
	for _, subject := range store.subjects() {
		if strings.Contains(subject, "correct") || strings.Contains(subject, "203.0.113.7") ||
			strings.Contains(subject, hex.EncodeToString(unkeyed[:8])) {
			t.Errorf("the fleet window holds %q, which carries what was typed "+
				"or where from", subject)
		}
	}
	if len(store.subjects()) != 1 {
		t.Errorf("the fleet holds %d records for one failed pair", len(store.subjects()))
	}
}

// A BURST AT ONE PAIR IS SERVED ONE AFTER ANOTHER.
//
// An attempt admitted and not yet resolved counts as a failure until it is, so
// four guesses fired at once are not four guesses before the curve notices:
// the first proceeds, the second waits a second behind it, the third two more
// behind that, and the fourth — four more — is refused with the seven seconds
// it would have waited. Mutation: count only resolved failures and all four
// proceed at once.
func TestABurstAtOnePairIsServedOneAfterAnother(t *testing.T) {
	t.Parallel()
	th, _, sleeps := newThrottle(t, newAttempts())
	who := credential.Attempt{Source: "203.0.113.7", Subject: "sarah.chen"}
	var tickets []*credential.Ticket
	for range 3 {
		ticket, err := th.Admit(t.Context(), who)
		if err != nil {
			t.Fatalf("a burst's attempt was refused: %v", err)
		}
		tickets = append(tickets, ticket)
	}
	if got := sleeps.take(); len(got) != 2 || got[0] != time.Second ||
		got[1] != 3*time.Second {
		t.Errorf("a burst of three waited %v, want the second a second and "+
			"the third three behind the first", got)
	}
	_, err := th.Admit(t.Context(), who)
	if got := credential.RetryAfter(err); got != 7*time.Second {
		t.Errorf("the fourth of a burst answered %v, want a 429 naming the "+
			"7s it would have waited", err)
	}
	for _, ticket := range tickets {
		ticket.Fail(t.Context())
	}
}

// A WAIT PAST THE CAP ON SLEEPERS IS ANSWERED AT ONCE.
//
// A waiting request is a goroutine and an open connection; past
// [credential.DelayedCap] of them the wait is answered 429 with the time left
// rather than slept.
func TestSleepersAreCapped(t *testing.T) {
	t.Parallel()
	clock := &clockOf{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	release := make(chan struct{})
	asleep := make(chan struct{}, credential.DelayedCap+1)
	th, err := credential.NewThrottle(credential.ThrottleDeps{
		Now: clock.now,
		Sleep: func(ctx context.Context, _ time.Duration) {
			asleep <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range credential.DelayedCap {
		who := credential.Attempt{Source: fmt.Sprintf("198.51.100.%d", i),
			Subject: "sarah.chen"}
		if err := failOnce(t, th, who); err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			if ticket, err := th.Admit(t.Context(), who); err == nil {
				ticket.Release()
			}
		})
		<-asleep
	}
	late := credential.Attempt{Source: "203.0.113.200", Subject: "sarah.chen"}
	if err := failOnce(t, th, late); err != nil {
		t.Fatal(err)
	}
	_, err = th.Admit(t.Context(), late)
	if !errors.Is(err, credential.ErrThrottled) || credential.RetryAfter(err) != time.Second {
		t.Errorf("a wait past the sleeper cap answered %v, want a 429 naming "+
			"the second left", err)
	}
	close(release)
	wg.Wait()
}

// A RELEASED ATTEMPT COUNTS AS NOTHING.
//
// An attempt that never reached a verdict — a body that did not parse, a store
// that could not be read — neither fails nor succeeds, and must not leave the
// pair owing a wait for a guess nobody made.
func TestAReleasedAttemptCountsAsNothing(t *testing.T) {
	t.Parallel()
	th, _, sleeps := newThrottle(t, newAttempts())
	who := credential.Attempt{Source: "203.0.113.7", Subject: "sarah.chen"}
	for range 3 {
		ticket, err := th.Admit(t.Context(), who)
		if err != nil {
			t.Fatal(err)
		}
		ticket.Release()
		// And a release after a verdict is a no-op, which is what makes
		// it safe to defer.
		ticket.Release()
	}
	if got := sleeps.all(); len(got) != 0 {
		t.Errorf("three released attempts cost waits %v", got)
	}
}

// AN IPv6 HOST IS ONE SOURCE ACROSS ITS /64.
//
// One customer is given a /64 and every address in it is theirs to use: keyed
// per address, a run at one account rotates through fresh pairs and its curve
// never starts. Mutation: key an IPv6 source by its full address and the run
// below is never throttled.
func TestAnIPv6HostIsOneSourceAcrossItsSlash64(t *testing.T) {
	t.Parallel()
	th, _, _ := newThrottle(t, newAttempts())
	var refused error
	for i := range credential.CurveSteps {
		if err := failOnce(t, th, credential.Attempt{
			Source:  fmt.Sprintf("2001:db8:1:2::%x", i+1),
			Subject: "sarah.chen",
		}); err != nil {
			refused = err
			break
		}
	}
	if !errors.Is(refused, credential.ErrThrottled) {
		t.Errorf("a run at one account rotating through one /64 was never throttled")
	}
	// A neighbouring /64 is somebody else.
	if err := failOnce(t, th, credential.Attempt{Source: "2001:db8:1:3::1",
		Subject: "sarah.chen"}); err != nil {
		t.Errorf("another /64 paid for the first one's run: %v", err)
	}
}

// AN UNREACHABLE STORE KEEPS THE LOCAL CURVE, AND NEVER FAILS CLOSED.
//
// Failing closed would lock every operator out of /config, /secrets and the
// dashboard — the one surface an incident is fixed from — at the moment
// coordination is already unwell. What is left is this node's own curve, which
// is still a curve.
func TestAnUnreachableStoreKeepsTheLocalCurve(t *testing.T) {
	t.Parallel()
	store := newAttempts()
	store.breaks(errors.New("the coordination store is unreachable"))
	th, clock, sleeps := newThrottle(t, store)
	who := credential.Attempt{Source: "203.0.113.7", Subject: "sarah.chen"}

	if err := failOnce(t, th, who); err != nil {
		t.Fatalf("the first attempt was refused while the store was down: %v", err)
	}
	if err := failOnce(t, th, who); err != nil {
		t.Fatalf("the second attempt was refused: %v", err)
	}
	if got := sleeps.take(); len(got) != 1 || got[0] != time.Second {
		t.Errorf("with the store down the second attempt waited %v, want the "+
			"local curve's second", got)
	}
	// The window still ends.
	clock.advance(coord.AttemptWindow + time.Minute)
	if got := owedBy(t, th, sleeps, who); got != 0 {
		t.Errorf("the local curve outlived the window: owed %s", got)
	}
}

// A FLEET WINDOW WITH NO DIGEST KEY IS REFUSED.
//
// Every node has to keep one pair under one name, or each keeps it under its
// own and seeds nothing from the others — a fleet that looks like it shares a
// curve and does not.
func TestAFleetWindowNeedsASharedKey(t *testing.T) {
	t.Parallel()
	if _, err := credential.NewThrottle(credential.ThrottleDeps{
		Attempts: newAttempts(),
	}); err == nil {
		t.Error("a throttle over the fleet window with no shared key was built")
	}
}

// THE LOCAL CURVE IS BOUNDED, SO THE THROTTLE IS NOT ITSELF A MEMORY
// EXHAUSTION.
//
// An unauthenticated caller can otherwise grow it one key per address they can
// reach it from, through the very mechanism that is supposed to bound them —
// and the map it replaced was walked whole on every failure.
func TestTheLocalCurveIsBounded(t *testing.T) {
	t.Parallel()
	th, _, _ := newThrottle(t, nil)
	for i := range credential.LocalKeys + 500 {
		source := fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff)
		if err := failOnce(t, th, credential.Attempt{Source: source,
			Subject: "sarah.chen"}); err != nil {
			t.Fatal(err)
		}
	}
	if pairs := credential.LocalKeysHeld(th); pairs > credential.LocalKeys {
		t.Errorf("the local curve holds %d pairs, past its bound of %d",
			pairs, credential.LocalKeys)
	}
}

// A SUCCESS ON A PAIR THIS NODE HAS FORGOTTEN STILL CLEARS THE FLEET.
//
// Admission always takes the pair, so a success that finds nothing here means
// the bound forgot it in between, and what the fleet holds under it is then
// unknown rather than nothing. Left, those failures would delay this person's
// next attempt on every other node for a mistake their success has answered.
// Mutation: flush only a pair this node still holds, and the fleet keeps the
// record.
func TestASuccessOnAPairThisNodeForgotStillClearsTheFleet(t *testing.T) {
	t.Parallel()
	store := newAttempts()
	clock := &clockOf{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	elsewhere, _, _ := onClock(t, store, clock)
	here, _, _ := onClock(t, store, clock)
	who := credential.Attempt{Source: "203.0.113.7", Subject: "sarah.chen"}
	if err := failOnce(t, elsewhere, who); err != nil {
		t.Fatal(err)
	}

	ticket, err := here.Admit(t.Context(), who)
	if err != nil {
		t.Fatal(err)
	}
	for i := range credential.LocalKeys {
		source := fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff)
		if err := failOnce(t, here, credential.Attempt{Source: source,
			Subject: "somebody.else"}); err != nil {
			t.Fatal(err)
		}
	}
	ticket.Succeed(t.Context())

	// A node meeting the pair for the first time reads what the fleet
	// holds under it, at the instant of the failure: nothing, or the
	// second that failure would still cost.
	third, _, sleeps := onClock(t, store, clock)
	if _, err := third.Admit(t.Context(), who); err != nil {
		t.Fatal(err)
	}
	if waited := sleeps.all(); len(waited) != 0 {
		t.Errorf("a person who signed in still owes the fleet %v on another "+
			"node, for a failure their success answered", waited)
	}
}

// AN UNIDENTIFIABLE SOURCE IS ADMITTED, NOT REFUSED — AND SO IS AN ATTEMPT
// THAT NAMES NOBODY.
//
// Refusing an unidentifiable source would mean a misconfigured proxy — one that
// strips the header the source is derived from — locks every person in the
// company out at once, which is an outage the throttle caused. What still
// bounds that caller is the password cost and the verify cap. And a
// credential that names nobody — an invitation link, a founder's code, a
// provider's round trip — has no pair to key, and keyed on its source alone it
// was a way to hold every provider sign-in at an address shut, since a
// callback nobody started fails for free. Either ticket is nil, and does
// nothing. Mutation: count a subject-less attempt against its source and the
// callbacks below meet 429.
func TestAnUncountableAttemptIsAdmitted(t *testing.T) {
	t.Parallel()
	th, _, sleeps := newThrottle(t, newAttempts())
	for _, a := range []credential.Attempt{
		{Subject: "sarah.chen"}, {Source: "203.0.113.7"},
	} {
		for range 40 {
			ticket, err := th.Admit(t.Context(), a)
			if err != nil {
				t.Fatalf("an uncountable attempt %+v was refused: %v", a, err)
			}
			if ticket != nil {
				t.Fatalf("an uncountable attempt %+v was handed a ticket", a)
			}
			ticket.Fail(t.Context())
		}
	}
	if got := sleeps.take(); len(got) != 0 {
		t.Errorf("uncountable attempts were made to wait %v", got)
	}
}

// --- the timing defences ----------------------------------------------------- //

// BOTH ARMS LAND ON ONE DEADLINE, MEASURED FROM ADMISSION.
//
// The decoy makes the two arms do the same SHAPE of work; only the pad makes
// them indistinguishable in TIME, because argon2id's own cost varies with load
// and with how many verifications are queued behind the verify cap, and a
// decoy's does not vary at all.
//
// THE ASSERTION IS ON THE DECISION rather than on the wall clock: what has to
// be equal is the instant both arms are told to answer at, and a case that
// measured real sleeps would be the flakiest thing in this tree.
func TestTheDecoyAndTheRealPathLandInsideOneDeadline(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// THE REAL ARM: an expensive verification, then the pad.
	heavy, heavyClock, heavyPad := newThrottle(t, newAttempts())
	heavyArrived := heavyClock.now()
	heavyClock.advance(180 * time.Millisecond)
	heavy.Pad(ctx, heavyArrived)

	// THE DECOY ARM: the fixed-cost HMAC for a subject that does not
	// exist, then the pad, from its own arrival.
	light, lightClock, lightPad := newThrottle(t, newAttempts())
	lightArrived := lightClock.now()
	light.Decoy("whatever-was-presented")
	lightClock.advance(time.Millisecond)
	light.Pad(ctx, lightArrived)

	real, decoy := heavyPad.all(), lightPad.all()
	if len(real) != 1 || len(decoy) != 1 {
		t.Fatalf("the two arms padded %d and %d times, want once each",
			len(real), len(decoy))
	}
	// Each arm sleeps EXACTLY the remainder of the deadline, so both
	// answer at arrival + deadline however long their own work took.
	if want := credential.PadDeadline - 180*time.Millisecond; real[0] != want {
		t.Errorf("the real arm slept %s, want %s — the pad must be the "+
			"REMAINDER of the deadline, not a fixed addition that leaks the "+
			"work's duration unchanged", real[0], want)
	}
	if want := credential.PadDeadline - time.Millisecond; decoy[0] != want {
		t.Errorf("the decoy arm slept %s, want %s", decoy[0], want)
	}
}

// THE PAD IS MEASURED FROM ADMISSION, THE LAST INSTANT BOTH ARMS SHARE.
//
// A fixed sleep added after the work leaks the work's duration unchanged. A
// deadline measured from when verification STARTED leaks how long the lookup
// before it took — which on the arm where the subject does not exist is a
// different lookup entirely.
func TestThePadIsMeasuredFromAdmissionAndNotFromTheWork(t *testing.T) {
	t.Parallel()
	th, clock, pad := newThrottle(t, newAttempts())
	arrived := clock.now()
	for _, spent := range []time.Duration{
		10 * time.Millisecond, 100 * time.Millisecond, 390 * time.Millisecond,
	} {
		clock.advance(spent)
		th.Pad(t.Context(), arrived)
		arrived = clock.now()
	}
	slept := pad.all()
	for i, want := range []time.Duration{
		credential.PadDeadline - 10*time.Millisecond,
		credential.PadDeadline - 100*time.Millisecond,
		credential.PadDeadline - 390*time.Millisecond,
	} {
		if slept[i] != want {
			t.Errorf("pad %d slept %s, want %s", i, slept[i], want)
		}
	}
}

// A REQUEST THAT ALREADY OVERRAN IS NOT EXTENDED.
//
// Sleeping a negative duration is a no-op and the arms separate — stated here
// rather than hidden, because at that point every request is slow, the node is
// at its verify cap, and the leak is one an attacker has to generate a load
// spike to open.
func TestAnOverrunRequestIsNotPaddedFurther(t *testing.T) {
	t.Parallel()
	th, clock, pad := newThrottle(t, newAttempts())
	arrived := clock.now()
	clock.advance(credential.PadDeadline + time.Second)
	th.Pad(t.Context(), arrived)
	if slept := pad.all(); len(slept) != 1 || slept[0] > 0 {
		t.Errorf("an overrun request was asked to sleep %v", slept)
	}
}

// AND THE SAME PROPERTY MEASURED, UNDER LOAD, ON THE WALL CLOCK.
//
// The case above asserts the DECISION — that each arm is told to sleep the
// remainder of the deadline — which is precise and would go on passing if the
// sleeping itself were broken. This one asserts the OBSERVABLE.
//
// WHAT IT MEASURES IS THE FAST TAIL, and that is the whole of the property
// worth measuring. A timing attack reads how SOON an answer comes back: the
// arm that skips work answers early, and an attacker takes the minimum over
// many requests to find it. So what has to hold is that NO request on either
// arm answers before the deadline — never that two goroutines on a shared CI
// runner finish within microseconds of each other, which is a claim about the
// scheduler and would be deleted as flaky within the month.
//
// THE SLOW TAIL IS DELIBERATELY UNASSERTED for the same reason: under enough
// load a real verification overruns the deadline and the arms separate, which
// this package's own head states rather than hides.
func TestTheDecoyAndTheRealPathLandInsideOneDeadlineUnderLoad(t *testing.T) {
	t.Parallel()
	const (
		deadline = 100 * time.Millisecond
		runs     = 6
	)
	th, err := credential.NewThrottle(credential.ThrottleDeps{
		Deadline: deadline,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build a throttle: %v", err)
	}
	// A cap of TWO, so the real arm's verifications queue behind each
	// other — which is the load the pad has to survive.
	hasher := credential.NewHasher(credential.Params{
		Memory: 8 * 1024, Time: 1, Threads: 1, KeyLen: 32,
	}, 2)
	verifier, err := hasher.Hash("a-long-enough-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	soonest := func(arm func()) time.Duration {
		var wg sync.WaitGroup
		took := make([]time.Duration, runs)
		for i := range runs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				arm()
				th.Pad(t.Context(), start)
				took[i] = time.Since(start)
			}()
		}
		wg.Wait()
		best := took[0]
		for _, d := range took[1:] {
			best = min(best, d)
		}
		return best
	}

	decoy := soonest(func() { th.Decoy("a-long-enough-password") })
	real := soonest(func() { hasher.Verify(verifier, "a-long-enough-password") })

	// THE CONTROL IS THIS LINE. The decoy's own work is microseconds, so
	// without the pad it answers in microseconds — and an attacker
	// taking the minimum over a few hundred requests reads the roster
	// straight off it.
	if decoy < deadline {
		t.Errorf("the fastest decoy request answered in %s, inside the %s "+
			"deadline — a subject that does not exist answers sooner, which "+
			"is the roster", decoy, deadline)
	}
	if real < deadline {
		t.Errorf("the fastest real request answered in %s, inside the %s "+
			"deadline", real, deadline)
	}
}
