package credential_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam/credential"
)

// --- the rig ----------------------------------------------------------------- //

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

// newThrottle builds a throttle on a clock of its own, whose waits are
// recorded rather than slept.
func newThrottle(t *testing.T) (*credential.Throttle, *clockOf, *counting) {
	t.Helper()
	clock := &clockOf{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	pad := &counting{}
	return withSleep(t, clock, pad.sleep), clock, pad
}

// onTime builds a throttle whose waits TAKE the time they are for: a wait
// served inside the request moves the clock by its length, as it does on a
// real node, so a case measuring how long an attempt was held reads it off the
// clock.
func onTime(t *testing.T, clock *clockOf) *credential.Throttle {
	t.Helper()
	return withSleep(t, clock, func(_ context.Context, d time.Duration) {
		if d > 0 {
			clock.advance(d)
		}
	})
}

// withSleep builds a throttle with its own sleep.
func withSleep(t *testing.T, clock *clockOf,
	sleep func(context.Context, time.Duration)) *credential.Throttle {

	t.Helper()
	return credential.NewThrottle(credential.ThrottleDeps{Now: clock.now, Sleep: sleep})
}

// failOnce admits one attempt and fails it, reporting what the admission
// waited or refused with.
func failOnce(t *testing.T, th *credential.Throttle, a credential.Attempt) error {
	t.Helper()
	ticket, err := th.Admit(t.Context(), a)
	if err != nil {
		return err
	}
	ticket.Fail()
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
	th, clock, sleeps := newThrottle(t)
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
	ticket.Succeed()
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
		th, _, _ := newThrottle(t)
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
	th, _, sleeps := newThrottle(t)
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
	ticket.Succeed()
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
	th, clock, sleeps := newThrottle(t)
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
		ticket.Succeed()
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
	th, _, sleeps := newThrottle(t)
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
		ticket.Succeed()
	}
}

// A SECOND FACTOR CLIMBS THE PERSON'S CURVE, WHEREVER THE CODES COME FROM.
//
// Somebody holding a password and a block of addresses met a fresh pair on
// every address, so each guess at the six digits was the first on its curve and
// the verify cap was all that bounded them. A code is decided on the person's
// own curve too, which no address appears in: three wrong codes, and the
// fourth waits the four seconds three failures earn. The person's curve is its
// own key — the same person's password pair is untouched by it — and reports
// its ceiling, and a success lifts it.
//
// Mutations: report the ceiling at any count and the first failure reports it;
// leave the curve standing on a success and the next wrong code owes the
// ceiling rather than the first step; share one key between the person's curve
// and a pair and the password attempt afterwards waits.
func TestASecondFactorClimbsThePersonsCurveWhereverItComesFrom(t *testing.T) {
	t.Parallel()
	clock := &clockOf{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	th := onTime(t, clock)
	const person = "0192f00d-0000-7000-8000-00000000000a"

	for i := range 3 {
		ticket, err := th.AdmitSecondFactor(t.Context(), person)
		if err != nil {
			t.Fatalf("wrong code %d was refused: %v", i+1, err)
		}
		if ceiling := ticket.Fail(); ceiling {
			t.Errorf("wrong code %d reported the ceiling", i+1)
		}
	}
	// THE FOURTH waits the four seconds three wrong codes earn — served
	// inside the request, which on this rig moves the clock by what it
	// waited.
	before := clock.now()
	fourth, err := th.AdmitSecondFactor(t.Context(), person)
	if err != nil {
		t.Fatalf("the fourth code was refused: %v", err)
	}
	if waited := clock.now().Sub(before); waited != 4*time.Second {
		t.Fatalf("the fourth code waited %s, want the 4s three wrong codes "+
			"earn on the person's curve", waited)
	}
	if fourth.Fail() {
		t.Error("the fourth wrong code reported the ceiling")
	}
	// THE PASSWORD'S PAIR IS ANOTHER KEY: a person's code failures owe
	// their next password attempt from a new address nothing.
	before = clock.now()
	if ticket, err := th.Admit(t.Context(), credential.Attempt{
		Source: "198.51.100.77", Subject: person}); err != nil ||
		clock.now() != before {
		t.Errorf("the person's second-factor failures reached a pair: waited "+
			"%s (%v)", clock.now().Sub(before), err)
	} else {
		ticket.Release()
	}

	// ON TO THE CEILING, and it says so.
	var reported []int
	for i := 5; i <= credential.CurveSteps; i++ {
		clock.advance(credential.DelayCeiling)
		ticket, err := th.AdmitSecondFactor(t.Context(), person)
		if err != nil {
			t.Fatalf("wrong code %d after the wait was refused: %v", i, err)
		}
		if ticket.Fail() {
			reported = append(reported, i)
		}
	}
	if len(reported) != 1 || reported[0] != credential.CurveSteps {
		t.Errorf("the ceiling was reported at failures %v, want only at the %dth",
			reported, credential.CurveSteps)
	}

	// A SUCCESS LIFTS IT: the next wrong code is the curve's first step
	// again, not its seventh.
	clock.advance(credential.DelayCeiling)
	ticket, err := th.AdmitSecondFactor(t.Context(), person)
	if err != nil {
		t.Fatal(err)
	}
	ticket.Succeed()
	ticket, err = th.AdmitSecondFactor(t.Context(), person)
	if err != nil {
		t.Fatalf("after the right code the next attempt was refused: %v", err)
	}
	ticket.Fail()
	before = clock.now()
	if ticket, err := th.AdmitSecondFactor(t.Context(), person); err != nil ||
		clock.now().Sub(before) != credential.DelayFloor {
		t.Errorf("one wrong code after the right one owes %s (%v), want the "+
			"curve's first step of %s — the success did not lift the curve",
			clock.now().Sub(before), err, credential.DelayFloor)
	} else {
		ticket.Release()
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
	th, _, sleeps := newThrottle(t)
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
		ticket.Fail()
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
	th := credential.NewThrottle(credential.ThrottleDeps{
		Now: clock.now,
		Sleep: func(ctx context.Context, _ time.Duration) {
			asleep <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
		},
	})
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
	_, err := th.Admit(t.Context(), late)
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
	th, _, sleeps := newThrottle(t)
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
	th, _, _ := newThrottle(t)
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

// A FAILURE AGES OUT OF THE WINDOW.
//
// A window that never ended would hold a person to two typos for the life of
// the process, every later mistake a step further up the curve. Nothing sweeps
// it: a failure's own instant is what leaves it behind. Two failures, then a
// third just inside the window, and the next attempt owes the four seconds
// three earn; the same third just past it, and it owes the one second its own
// failure earns. Mutation: prune nothing and the second case owes four too.
func TestAFailureAgesOutOfTheWindow(t *testing.T) {
	t.Parallel()
	who := credential.Attempt{Source: "203.0.113.7", Subject: "sarah.chen"}
	for _, c := range []struct {
		name  string
		later time.Duration
		owes  time.Duration
	}{
		{"inside the window", credential.Window - 2*time.Second, 4 * time.Second},
		{"past the window", credential.Window + 2*time.Second, time.Second},
	} {
		th, clock, sleeps := newThrottle(t)
		for range 2 {
			if err := failOnce(t, th, who); err != nil {
				t.Fatal(err)
			}
		}
		clock.advance(c.later)
		if err := failOnce(t, th, who); err != nil {
			t.Fatal(err)
		}
		if got := owedBy(t, th, sleeps, who); got != c.owes {
			t.Errorf("a third failure %s the first two owes %s, want %s",
				c.name, got, c.owes)
		}
	}
}

// A RUN HELD AT THE CEILING STAYS AT THE CEILING.
//
// A guessing run that waits out every wait makes one attempt per
// [credential.DelayCeiling], and the window has to hold enough of those that the
// oldest aging out never takes the count below [credential.CurveSteps] — or the
// run slides back down the curve while it is being throttled, and a patient
// guesser earns shorter waits by being patient. Twenty minutes of it, longer
// than the window, and every attempt past the sixth owes the whole ceiling.
// Mutation: a window shorter than six ceilings — two minutes — and the waits
// fall back to eight seconds.
func TestARunHeldAtTheCeilingStaysAtTheCeiling(t *testing.T) {
	t.Parallel()
	clock := &clockOf{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	th := onTime(t, clock)
	who := credential.Attempt{Source: "203.0.113.7", Subject: "sarah.chen"}
	start := clock.now()
	for attempt := 1; clock.now().Sub(start) < 20*time.Minute; attempt++ {
		ticket, err := th.Admit(t.Context(), who)
		owed := credential.RetryAfter(err)
		if attempt > credential.CurveSteps && owed != credential.DelayCeiling {
			t.Fatalf("attempt %d, %s into a run at the ceiling, owes %s (%v), "+
				"want the whole %s", attempt, clock.now().Sub(start), owed, err,
				credential.DelayCeiling)
		}
		if err != nil {
			clock.advance(owed)
			if ticket, err = th.Admit(t.Context(), who); err != nil {
				t.Fatalf("attempt %d after its wait was refused: %v", attempt, err)
			}
		}
		ticket.Fail()
	}
}

// THE CURVE IS BOUNDED, SO THE THROTTLE IS NOT ITSELF A MEMORY EXHAUSTION.
//
// An unauthenticated caller can otherwise grow it one key per address they can
// reach it from, through the very mechanism that is supposed to bound them —
// and the map it replaced was walked whole on every failure.
func TestTheCurveIsBounded(t *testing.T) {
	t.Parallel()
	th, _, _ := newThrottle(t)
	for i := range credential.MaxPairs + 500 {
		source := fmt.Sprintf("10.%d.%d.%d", i>>16&0xff, i>>8&0xff, i&0xff)
		if err := failOnce(t, th, credential.Attempt{Source: source,
			Subject: "sarah.chen"}); err != nil {
			t.Fatal(err)
		}
	}
	if pairs := credential.PairsHeld(th); pairs > credential.MaxPairs {
		t.Errorf("the curve holds %d pairs, past its bound of %d",
			pairs, credential.MaxPairs)
	}
}

// AN UNIDENTIFIABLE SOURCE IS ADMITTED, NOT REFUSED — AND SO IS AN ATTEMPT
// THAT NAMES NOBODY.
//
// Refusing an unidentifiable source would mean a misconfigured proxy — one that
// strips the header the source is derived from — locks every person in the
// company out at once, which is an outage the throttle caused. What still
// bounds that caller is the password cost and the verify cap. And a
// credential that names nobody — an invitation link — has
// no pair to key, and keyed on its source alone it was a way to hold every
// invitation redeemed at an address shut. Either ticket is nil, and does
// nothing. Mutation: count a subject-less attempt against its source and the
// attempts below meet 429.
func TestAnUncountableAttemptIsAdmitted(t *testing.T) {
	t.Parallel()
	th, _, sleeps := newThrottle(t)
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
			ticket.Fail()
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
	heavy, heavyClock, heavyPad := newThrottle(t)
	heavyArrived := heavyClock.now()
	heavyClock.advance(180 * time.Millisecond)
	heavy.Pad(ctx, heavyArrived)

	// THE DECOY ARM: a subject that does not exist, whose decoy took a
	// millisecond, then the pad, from its own arrival.
	light, lightClock, lightPad := newThrottle(t)
	lightArrived := lightClock.now()
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
	th, clock, pad := newThrottle(t)
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
	th, clock, pad := newThrottle(t)
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
	th := credential.NewThrottle(credential.ThrottleDeps{Deadline: deadline})
	// A cap of TWO, and every request from a source of its own, so the
	// verifications queue behind each other for the cap — which is the load
	// the pad has to survive.
	hasher := credential.NewHasher(credential.Params{
		Memory: 8 * 1024, Time: 1, Threads: 1, KeyLen: 32,
	}, 2)
	verifier, err := hasher.Hash(t.Context(), "", "a-long-enough-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	soonest := func(arm func(source string)) time.Duration {
		var wg sync.WaitGroup
		took := make([]time.Duration, runs)
		for i := range runs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := time.Now()
				arm(fmt.Sprintf("198.51.100.%d", i))
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

	decoy := soonest(func(source string) {
		_ = hasher.Decoy(t.Context(), source, "a-long-enough-password")
	})
	real := soonest(func(source string) {
		_, _, _ = hasher.Verify(t.Context(), source, verifier, "a-long-enough-password")
	})

	// THE CONTROL IS THIS LINE. A decoy at this cost holds its turn for a
	// few milliseconds, so without the pad it answers in a few
	// milliseconds — and an attacker taking the minimum over a few hundred
	// requests reads the roster straight off it.
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
