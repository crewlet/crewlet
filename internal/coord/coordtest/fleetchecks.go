package coordtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// The checks in this file are the fleet cases that have to be PROVABLY able to
// fail.
//
// Every other case here reports through a *testing.T, which means nothing in
// this package can demonstrate that it fails — a case that quietly stopped
// asserting reads exactly like a case that passes. For most of them that is
// an acceptable trade, because a mutation run against the backends answers the
// question offline. For these three it is not, and the reason is their
// history:
//
//   - The bucket expiry is the invariant a ttl PARAMETER hid for the whole
//     life of both backends. The twin honoured the argument, the KV validated
//     it and let the bucket decide, and no case ever travelled past a
//     deadline — so the suite certified the divergence clean every time it
//     ran. A check nobody can hand a lying backend is how that happens again.
//   - The refusal of an unnamed record is the tri-state at its narrowest: the
//     difference between "an error" and "false" is invisible in a passing
//     suite and is a dropped delivery in production.
//   - A create racing others over a record just REMOVED is the tri-state from
//     the other side — a lost race answered as an outage — and it is
//     invisible on every substrate but a replicated one, so the check has to
//     be carried to a cluster rather than only run where [RunFleet] runs.
//
// So each one RETURNS what it found. [RunFleet] reports it through t like any
// other case, and this package's own test hands them twins built to get it
// wrong and asserts they are caught. The shape is [internal/statelog/statelogtest]'s,
// for the same reason it has it.

// CheckClaimExpiresWithItsBucket verifies that a delivery claim lapses on the
// age of the bucket it was written to, and on nothing else.
//
// age is what the backend under test was built with. The lapse itself is
// [reclaimed]'s, which both travels the clock it passes and waits out the real
// one — see there for why it takes both.
func CheckClaimExpiresWithItsBucket(ctx context.Context, f coord.Fleet,
	age time.Duration, at time.Time) []error {

	const key = "gitlab|lapsing"
	var errs []error
	first, err := f.Claim(ctx, key, at)
	if err != nil {
		return append(errs, fmt.Errorf("the first claim: %w", err))
	}
	if !first {
		return append(errs, fmt.Errorf("the first claim of %q was refused", key))
	}
	// STILL HELD before the age runs out, which is what stops this
	// passing against a backend that claims nothing at all.
	if again, err := f.Claim(ctx, key, at); err != nil {
		errs = append(errs, fmt.Errorf("a claim while the first was live: %w", err))
	} else if again {
		errs = append(errs, fmt.Errorf("a second caller claimed %q while the first "+
			"claim was inside the bucket's %v age", key, age))
	}

	if err := reclaimed(func(at time.Time) (bool, error) { return f.Claim(ctx, key, at) },
		"Claim", key, age, at); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// lapseSlack is how far past a bucket's age a record may still be readable,
// and therefore how long [reclaimed] will wait for one to go.
//
// MEASURED, not chosen, and measured twice. A real broker sweeps a stream's
// aged messages on a timer rather than removing them at the deadline, so a
// record outlives its bucket's age by however long the next sweep is away:
// internal/coord/kv's own broker measurements record 306ms past a one-second
// bucket, and 208ms past a 500ms one under this suite.
//
// It is a CEILING RATHER THAN A WAIT — [reclaimed] polls, so a backend that
// reaps promptly pays none of it — which is why it is set well above both
// numbers instead of just above them. A loaded runner pushes a timer out, and
// the failure that buys is a case that goes red for the machine it ran on
// rather than for the backend it certifies.
const lapseSlack = 2 * time.Second

// lapsePoll is how often [reclaimed] asks while it waits.
const lapsePoll = 20 * time.Millisecond

// reclaimed waits for a first-claim-wins record to lapse with its bucket, and
// says what it saw if it does not.
//
// IT TRAVELS THE CLOCK IT PASSES AND WAITS OUT THE REAL ONE, because the two
// certified backends expire a record on two different clocks and the contract
// is deliberately silent about which: a store of its own reaps whatever any
// caller's clock says, while the in-process twin has no clock but the
// caller's. Travelling alone would certify a backend that never expires
// anything; waiting alone would fail the twin for having no store.
func reclaimed(claim func(time.Time) (bool, error), verb, key string,
	age time.Duration, at time.Time) error {

	at = at.Add(age + lapseSlack)
	deadline := time.Now().Add(age + lapseSlack)
	for {
		ok, err := claim(at)
		switch {
		case err != nil:
			return fmt.Errorf("%s past the bucket's age: %w", verb, err)
		case ok:
			return nil
		case time.Now().After(deadline):
			return fmt.Errorf("%q was still claimed %v after it was recorded, and its "+
				"bucket is %v old: a record that outlives its bucket is one whose "+
				"horizon nobody can state — a caller asking for longer than the "+
				"bucket holds gets the bucket, silently, which is how a "+
				"fifteen-minute setup state lapsed on a five-minute bucket",
				key, age+lapseSlack, age)
		}
		time.Sleep(lapsePoll)
	}
}

// CheckUnnamedRecordsAreRefused verifies that every first-claim-wins verb
// answers an empty key with an ERROR rather than with false.
//
// The three-valued rule at its narrowest. A caller that named nothing has not
// lost a race to anybody, so "false" — which every one of these callers reads
// as "somebody else has this" — drops the delivery, refuses the setup
// callback, or reports a clean record for a caller nobody can throttle. The
// same argument fault reaches every verb, so the check sends it to every verb:
// a guard added to one of them and forgotten on the next is the ordinary way
// this reappears.
func CheckUnnamedRecordsAreRefused(ctx context.Context, f coord.Fleet, at time.Time) []error {
	var errs []error
	if ok, err := f.Claim(ctx, "", at); err == nil {
		errs = append(errs, fmt.Errorf("an empty key reached Claim, which answered "+
			"%t rather than an error: a caller that named nothing has not lost a "+
			"race to anybody, and false reads as a delivery a peer already took", ok))
	}
	if ok, err := f.ClaimSetup(ctx, "", at); err == nil {
		errs = append(errs, fmt.Errorf("an empty key reached ClaimSetup, which "+
			"answered %t rather than an error: false there refuses a callback "+
			"nobody has spent", ok))
	}
	if err := f.Fail(ctx, "", at); err == nil {
		errs = append(errs, errors.New("an empty subject reached Fail, which "+
			"answered no error: an attempt nobody can be throttled by reads as "+
			"a caller with a clean record"))
	}
	if got, err := f.Failures(ctx, "", at); err == nil {
		errs = append(errs, fmt.Errorf("an empty subject reached Failures, which "+
			"answered %+v rather than an error — the answer a throttle lets "+
			"through", got))
	}
	if err := f.Flush(ctx, ""); err == nil {
		errs = append(errs, errors.New("an empty subject reached Flush, which "+
			"answered no error: a flush that forgot nothing reported success"))
	}
	return errs
}

// Racers is how many callers [CheckCreatesOverARemovedRecordAreRaces] sends
// at a removed record at once, and RacedRounds how many times it removes one
// and races them.
//
// SIZED TO MEET THE RARE ANSWER, not to load the store. On a replicated KV
// stream only some of a race's losers come back as the bare refusal the check
// exists for — about a fifth, measured at three replicas — so one round's seven
// losers all miss it about a fifth of the time, and eight rounds' fifty-six
// about four times in a million.
const (
	Racers      = 8
	RacedRounds = 8
)

// CheckCreatesOverARemovedRecordAreRaces verifies that callers racing to write
// a record somebody has just REMOVED are each told what happened — every failed
// attempt counted, every charge landed, and one creator first with the rest
// told they lost — and none of them that the store is down.
//
// A removal on a KV bucket leaves a MARKER, and a create over one is a
// compare-and-set on the marker's revision, so every loser of that race is a
// caller a first writer beat. On a replicated stream the broker answered a
// share of those losers with a refusal the client wrapped in neither of its
// sentinels, and every create that matched a sentinel read it as an outage: a
// failed sign-in racing another node's to a record a success had just flushed
// went unrecorded and left its node's throttle on its own curve for half a
// minute, a delivery claim racing another to one just released answered
// "unknown" and was processed twice, a charge to a counter just reset failed.
//
// EVERY VERB WHOSE RECORD CAN BE REMOVED, because the refusal is the
// broker's and not any one verb's: the fix is one classifier every create
// asks, and a verb left out of the check is the one that stops asking it.
func CheckCreatesOverARemovedRecordAreRaces(ctx context.Context, f coord.Fleet,
	at time.Time) []error {

	for round := range RacedRounds {
		for _, check := range []func(context.Context, coord.Fleet, int, time.Time) []error{
			raceAttempts, raceClaims, raceBudgets, raceRuns, raceFollows,
			raceMailboxes, raceSecrets,
		} {
			if errs := check(ctx, f, round, at); len(errs) > 0 {
				return errs
			}
		}
	}
	return nil
}

// race runs write from [Racers] callers at once and answers how many were
// told they wrote, and every error any of them was told.
func race(write func() (bool, error)) (won int, errs []error) {
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for range Racers {
		wg.Go(func() {
			ok, err := write()
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				errs = append(errs, err)
			case ok:
				won++
			}
		})
	}
	wg.Wait()
	return won, errs
}

// raced names the verb every error from one race came back from.
func raced(verb string, errs []error) []error {
	out := make([]error, 0, len(errs))
	for _, err := range errs {
		out = append(out, fmt.Errorf("%s racing %d callers over a record just "+
			"removed answered an error — a caller that lost to a first writer, "+
			"told the store is down: %w", verb, Racers, err))
	}
	return out
}

// firstOfMany is the error for a create race that did not have exactly one
// winner.
func firstOfMany(verb string, won int) error {
	return fmt.Errorf("%s racing %d callers over a record just removed: %d "+
		"were told they wrote it, want exactly one", verb, Racers, won)
}

func raceAttempts(ctx context.Context, f coord.Fleet, round int, at time.Time) []error {
	subject := fmt.Sprintf("token:raced-%d", round)
	if err := f.Fail(ctx, subject, at); err != nil {
		return []error{fmt.Errorf("the first Fail: %w", err)}
	}
	if err := f.Flush(ctx, subject); err != nil {
		return []error{fmt.Errorf("the flush: %w", err)}
	}
	_, errs := race(func() (bool, error) { return true, f.Fail(ctx, subject, at) })
	if len(errs) > 0 {
		return raced("Fail", errs)
	}
	got, err := f.Failures(ctx, subject, at)
	if err != nil {
		return []error{fmt.Errorf("reading the window back: %w", err)}
	}
	if got.Count() != Racers {
		return []error{fmt.Errorf("%d failures racing to a flushed record left "+
			"%d counted, want every one", Racers, got.Count())}
	}
	return nil
}

func raceClaims(ctx context.Context, f coord.Fleet, round int, at time.Time) []error {
	key := fmt.Sprintf("gitlab|raced-%d", round)
	if _, err := f.Claim(ctx, key, at); err != nil {
		return []error{fmt.Errorf("the first Claim: %w", err)}
	}
	if err := f.Release(ctx, key); err != nil {
		return []error{fmt.Errorf("the release: %w", err)}
	}
	won, errs := race(func() (bool, error) { return f.Claim(ctx, key, at) })
	if len(errs) > 0 {
		return raced("Claim", errs)
	}
	if won != 1 {
		return []error{firstOfMany("Claim", won)}
	}
	return nil
}

func raceBudgets(ctx context.Context, f coord.Fleet, round int, _ time.Time) []error {
	scope := fmt.Sprintf("agent:raced-%d", round)
	if _, err := f.Charge(ctx, scope, 1, 0, 0); err != nil {
		return []error{fmt.Errorf("the first Charge: %w", err)}
	}
	// EVERY SCOPE, so the org's counter is a removed record too: both
	// halves of every charge below are a create over a marker.
	if _, err := f.Reset(ctx, ""); err != nil {
		return []error{fmt.Errorf("the reset: %w", err)}
	}
	won, errs := race(func() (bool, error) {
		spend, err := f.Charge(ctx, scope, 1, 0, 0)
		return spend.OK, err
	})
	if len(errs) > 0 {
		return raced("Charge", errs)
	}
	used, err := f.Used(ctx, scope)
	if err != nil {
		return []error{fmt.Errorf("reading the counter back: %w", err)}
	}
	if won != Racers || used != Racers {
		return []error{fmt.Errorf("%d unlimited charges racing to a reset counter: "+
			"%d admitted and %d counted, want every one", Racers, won, used)}
	}
	return nil
}

func raceRuns(ctx context.Context, f coord.Fleet, round int, _ time.Time) []error {
	turn := fmt.Sprintf("turn-raced-%d", round)
	if _, err := f.CreateSandboxRun(ctx, turn, []byte(`{"n":1}`)); err != nil {
		return []error{fmt.Errorf("the first CreateSandboxRun: %w", err)}
	}
	rec, found, err := f.SandboxRun(ctx, turn)
	if err != nil {
		return []error{fmt.Errorf("reading the run back: %w", err)}
	}
	if !found {
		return []error{errors.New("a run just created read back as absent")}
	}
	if _, err := f.DeleteSandboxRun(ctx, turn, rec.Version); err != nil {
		return []error{fmt.Errorf("the run's delete: %w", err)}
	}
	won, errs := race(func() (bool, error) {
		return f.CreateSandboxRun(ctx, turn, []byte(`{"n":2}`))
	})
	if len(errs) > 0 {
		return raced("CreateSandboxRun", errs)
	}
	if won != 1 {
		return []error{firstOfMany("CreateSandboxRun", won)}
	}
	return nil
}

func raceFollows(ctx context.Context, f coord.Fleet, round int, at time.Time) []error {
	thread := fmt.Sprintf("t-raced-%d", round)
	if _, err := f.FollowIfAbsent(ctx, "slack", "agent-swe", "C1", thread,
		"mention", at); err != nil {
		return []error{fmt.Errorf("the first FollowIfAbsent: %w", err)}
	}
	if _, err := f.Unfollow(ctx, "slack", "agent-swe", "C1", thread); err != nil {
		return []error{fmt.Errorf("the unfollow: %w", err)}
	}
	won, errs := race(func() (bool, error) {
		return f.FollowIfAbsent(ctx, "slack", "agent-swe", "C1", thread, "mention", at)
	})
	if len(errs) > 0 {
		return raced("FollowIfAbsent", errs)
	}
	if won != 1 {
		return []error{firstOfMany("FollowIfAbsent", won)}
	}
	return nil
}

func raceMailboxes(ctx context.Context, f coord.Fleet, round int, _ time.Time) []error {
	handle := fmt.Sprintf("raced-%d", round)
	rec := seat(handle)
	stored, _, err := f.CreateMailbox(ctx, rec)
	if err != nil {
		return []error{fmt.Errorf("the first CreateMailbox: %w", err)}
	}
	if _, err := f.DeleteMailbox(ctx, rec.Seat, stored.Version); err != nil {
		return []error{fmt.Errorf("the mailbox's delete: %w", err)}
	}
	won, errs := race(func() (bool, error) {
		_, created, err := f.CreateMailbox(ctx, rec)
		return created, err
	})
	if len(errs) > 0 {
		return raced("CreateMailbox", errs)
	}
	if won != 1 {
		return []error{firstOfMany("CreateMailbox", won)}
	}
	return nil
}

func raceSecrets(ctx context.Context, f coord.Fleet, round int, at time.Time) []error {
	rec := coord.SecretRecord{
		Name:  fmt.Sprintf("RACED_%d", round),
		Value: "v1:sealed", KeyID: "key-1", UpdatedAt: at,
	}
	if _, err := f.CreateSecret(ctx, rec); err != nil {
		return []error{fmt.Errorf("the first CreateSecret: %w", err)}
	}
	if _, err := f.DeleteSecret(ctx, rec.Name); err != nil {
		return []error{fmt.Errorf("the secret's delete: %w", err)}
	}
	won, errs := race(func() (bool, error) { return f.CreateSecret(ctx, rec) })
	if len(errs) > 0 {
		return raced("CreateSecret", errs)
	}
	if won != 1 {
		return []error{firstOfMany("CreateSecret", won)}
	}
	return nil
}
