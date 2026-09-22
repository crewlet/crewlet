package credential_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam/credential"
)

// --- the rig ----------------------------------------------------------------- //

// attempts is a coord.Attempts that can be made unreachable.
type attempts struct {
	mu     sync.Mutex
	counts map[string]int
	err    error
	reads  int
}

func newAttempts() *attempts { return &attempts{counts: map[string]int{}} }

func (a *attempts) Fail(_ context.Context, subject string, _ time.Time) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return 0, a.err
	}
	a.counts[subject]++
	return a.counts[subject], nil
}

func (a *attempts) Failures(_ context.Context, subject string, _ time.Time) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reads++
	if a.err != nil {
		return 0, a.err
	}
	return a.counts[subject], nil
}

func (a *attempts) Flush(_ context.Context, subject string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	delete(a.counts, subject)
	return nil
}

func (a *attempts) breaks(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.err = err
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

// counting records what each Pad was asked to sleep, so the timing properties
// are asserted on the DECISION rather than on the wall clock — a test that
// measured real sleeps would be the flakiest thing in this tree.
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

func newThrottle(t *testing.T, a coord.Attempts) (*credential.Throttle, *clockOf, *counting) {
	t.Helper()
	clock := &clockOf{at: time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)}
	pad := &counting{}
	th, err := credential.NewThrottle(credential.ThrottleDeps{
		Attempts: a, Now: clock.now, Sleep: pad.sleep,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("build a throttle: %v", err)
	}
	return th, clock, pad
}

// --- the cases --------------------------------------------------------------- //

// ADMISSION IS KEYED ON THE SOURCE AND HAPPENS BEFORE THE SUBJECT RESOLVES.
//
// THE ORACLE THIS WHOLE FILE EXISTS FOR. A throttle keyed on who you CLAIM to
// be is one only real subjects can trigger, so the 429 becomes the roster: an
// attacker submits six attempts per name and reads off which names start
// refusing. Keyed on the source, every name refuses at the same point and the
// answer says nothing about anybody.
func TestAdmissionRefusesBeforeTheSubjectIsResolved(t *testing.T) {
	t.Parallel()
	store := newAttempts()
	th, _, _ := newThrottle(t, store)
	ctx := t.Context()

	// Six failures from one source, every one against a DIFFERENT name.
	for _, name := range []string{
		"sarah.chen", "nobody.here", "also.nobody", "jane.doe",
		"not.a.person", "still.nobody",
	} {
		if err := th.Admit(ctx, "203.0.113.7"); err != nil {
			t.Fatalf("admission refused too early while trying %s: %v", name, err)
		}
		th.Fail(ctx, "203.0.113.7")
	}

	// The source is now refused, whoever it claims to be next.
	for _, name := range []string{"sarah.chen", "a.name.that.does.not.exist"} {
		if err := th.Admit(ctx, "203.0.113.7"); !errors.Is(err, credential.ErrThrottled) {
			t.Errorf("after six failures the source was admitted for %s: %v",
				name, err)
		}
	}
	// And a different source is untouched, which is what makes the
	// throttle a throttle rather than a company-wide lockout.
	if err := th.Admit(ctx, "198.51.100.4"); err != nil {
		t.Errorf("a second source was refused for the first's failures: %v", err)
	}
	// THE STORE WAS NEVER ASKED ABOUT A SUBJECT. Every read it saw was
	// keyed on the source; a subject reaching it at all would be the
	// oracle back again one layer down.
	if store.reads == 0 {
		t.Error("the fleet window was never read, so this case is asserting " +
			"about a throttle that is not consulting it")
	}
	for subject := range store.counts {
		if subject != "203.0.113.7" && subject != "198.51.100.4" {
			t.Errorf("the fleet window holds a record for %q, which is not a "+
				"source — a throttle that counts subjects is the roster it "+
				"exists to hide", subject)
		}
	}
}

// A SUCCESSFUL AUTHENTICATION LIFTS THE LOCKOUT, FLEET-WIDE.
func TestASuccessfulAuthenticationFlushesTheWindow(t *testing.T) {
	t.Parallel()
	store := newAttempts()
	th, _, _ := newThrottle(t, store)
	ctx := t.Context()
	for range 6 {
		th.Fail(ctx, "203.0.113.7")
	}
	if err := th.Admit(ctx, "203.0.113.7"); !errors.Is(err, credential.ErrThrottled) {
		t.Fatalf("the source was not throttled: %v", err)
	}
	th.Flush(ctx, "203.0.113.7")
	if err := th.Admit(ctx, "203.0.113.7"); err != nil {
		t.Errorf("the lockout survived the credential that proves the caller "+
			"is not who it was protecting against: %v", err)
	}
}

// AN UNREACHABLE STORE KEEPS THE LOCAL CURVE, AND NEVER FAILS CLOSED.
//
// Failing closed would lock every operator out of /config, /secrets and the
// dashboard — the one surface an incident is fixed from — at the moment
// coordination is already unwell. What is left is this node's own count, which
// is not nothing: a guessing run against ONE node still meets the same limit,
// and one spread across N nodes gets N times the attempts rather than
// unlimited ones.
func TestAnUnreachableStoreKeepsTheLocalCurve(t *testing.T) {
	t.Parallel()
	store := newAttempts()
	th, clock, _ := newThrottle(t, store)
	ctx := t.Context()

	store.breaks(errors.New("the coordination store is unreachable"))

	// It fails OPEN: the first attempt is admitted although nothing can
	// be read.
	if err := th.Admit(ctx, "203.0.113.7"); err != nil {
		t.Fatalf("the first attempt was refused while the store was down: %v", err)
	}
	// And the local curve still counts.
	for range 6 {
		th.Fail(ctx, "203.0.113.7")
	}
	if err := th.Admit(ctx, "203.0.113.7"); !errors.Is(err, credential.ErrThrottled) {
		t.Errorf("with the store down a source made six failures and was "+
			"still admitted: %v — an unreachable coordination store would be "+
			"an unlimited guessing run", err)
	}
	// The window still ends. An attempt that aged out stops counting
	// locally exactly as it would in the fleet.
	clock.advance(coord.AttemptWindow + time.Minute)
	if err := th.Admit(ctx, "203.0.113.7"); err != nil {
		t.Errorf("the local lockout outlived the window: %v", err)
	}
}

// BOTH ARMS LAND ON ONE DEADLINE, MEASURED FROM ARRIVAL.
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

// THE PAD IS MEASURED FROM ARRIVAL, WHICH IS THE ONLY INSTANT BOTH ARMS SHARE.
//
// A fixed sleep added after the work leaks the work's duration unchanged. A
// deadline measured from when verification STARTED leaks how long the lookup
// before it took — which on the arm where the subject does not exist is a
// different lookup entirely.
func TestThePadIsMeasuredFromArrivalAndNotFromTheWork(t *testing.T) {
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

// AN UNIDENTIFIABLE SOURCE IS ADMITTED, NOT REFUSED.
//
// Refusing would mean a misconfigured proxy — one that strips the header the
// source is derived from — locks every person in the company out at once,
// which is an outage the throttle caused. What still bounds that caller is the
// password cost and the verify cap.
func TestAnUnidentifiableSourceIsAdmitted(t *testing.T) {
	t.Parallel()
	th, _, _ := newThrottle(t, newAttempts())
	for range 20 {
		th.Fail(t.Context(), "")
	}
	if err := th.Admit(t.Context(), ""); err != nil {
		t.Errorf("a request with no identifiable source was refused: %v — a "+
			"proxy that stops setting the header would lock the company out",
			err)
	}
}

// THE LOCAL CURVE IS BOUNDED, SO THE THROTTLE IS NOT ITSELF A MEMORY
// EXHAUSTION.
//
// An unauthenticated caller can otherwise grow the map one entry per address
// they can spoof, reached through the very mechanism that is supposed to bound
// them.
func TestTheLocalCurveDoesNotGrowWithoutBound(t *testing.T) {
	t.Parallel()
	store := newAttempts()
	store.breaks(errors.New("down"))
	th, clock, _ := newThrottle(t, store)
	ctx := t.Context()

	for i := range 500 {
		th.Fail(ctx, "203.0.113."+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	// Every one of those ages out, and the next write is what collects
	// them: the map is bounded by pruning rather than by a cap somebody
	// had to choose.
	clock.advance(coord.AttemptWindow + time.Minute)
	th.Fail(ctx, "198.51.100.4")
	if n := credential.LocalSources(th); n > 2 {
		t.Errorf("the local curve holds %d sources after every one of 500 "+
			"aged out — an unauthenticated caller grows this one entry per "+
			"address they can spoof", n)
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
