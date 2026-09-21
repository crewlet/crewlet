package coordtest

import (
	"context"
	"fmt"
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
// question offline. For these two it is not, and the reason is their history:
//
//   - The bucket expiry is the invariant a ttl PARAMETER hid for the whole
//     life of both backends. The twin honoured the argument, the KV validated
//     it and let the bucket decide, and no case ever travelled past a
//     deadline — so the suite certified the divergence clean every time it
//     ran. A check nobody can hand a lying backend is how that happens again.
//   - The refusal of an unnamed record is the tri-state at its narrowest: the
//     difference between "an error" and "false" is invisible in a passing
//     suite and is a dropped delivery in production.
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
		return append(errs, fmt.Errorf("Claim: %w", err))
	}
	if !first {
		return append(errs, fmt.Errorf("the first claim of %q was refused", key))
	}
	// STILL HELD before the age runs out, which is what stops this
	// passing against a backend that claims nothing at all.
	if again, err := f.Claim(ctx, key, at); err != nil {
		errs = append(errs, fmt.Errorf("Claim while the first was live: %w", err))
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
		errs = append(errs, fmt.Errorf("Claim accepted an empty key and answered %t: a "+
			"caller that named nothing has not lost a race to anybody, and false "+
			"reads as a delivery a peer already took", ok))
	}
	if ok, err := f.ClaimSetup(ctx, "", at); err == nil {
		errs = append(errs, fmt.Errorf("ClaimSetup accepted an empty key and answered %t: "+
			"false there refuses a callback nobody has spent", ok))
	}
	if n, err := f.Fail(ctx, "", at); err == nil {
		errs = append(errs, fmt.Errorf("Fail accepted an empty subject and answered %d: an "+
			"attempt nobody can be throttled by reads as a caller with a clean "+
			"record", n))
	}
	if n, err := f.Failures(ctx, "", at); err == nil {
		errs = append(errs, fmt.Errorf("Failures accepted an empty subject and answered %d, "+
			"which is the answer a throttle lets through", n))
	}
	if err := f.Flush(ctx, ""); err == nil {
		errs = append(errs, fmt.Errorf("Flush accepted an empty subject: a flush that "+
			"forgot nothing reported success"))
	}
	return errs
}
