package coordtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// These checks must be PROVABLY able to fail, which a case reporting through a
// *testing.T cannot show, so each RETURNS what it found and this package's
// own test hands them twins built to get it wrong (the shape of
// [internal/statelog/statelogtest]):
//
//   - A claim's lapse is the invariant a ttl PARAMETER hid: the twin honoured
//     it, the KV let its bucket decide, and no case travelled past a deadline.
//   - The refusal of an unnamed record is the tri-state at its narrowest: "an
//     error" versus "false" is a dropped delivery in production.
//   - A create racing others over a record just REMOVED is the tri-state from
//     the other side (a lost race answered as an outage), visible only on a
//     replicated substrate, so it must be carried to a cluster.

// CheckClaimLapsesAtItsOwnTTL verifies that a delivery claim lapses at the
// deadline its own record carries — the `now` it was made at plus the ttl it
// asked for — and that a lapsed claim is taken over by exactly one caller.
//
// TWO LENGTHS IN ONE STORE: a webhook claims for [coord.ClaimTTL], the chat
// socket up to [coord.MaxClaimTTL]. Holding every claim for the bucket's age
// swallows a webhook replay for the socket's window; for the shortest horizon,
// a reconnecting seat delivers twice. A MINUTE AND THREE stay inside any
// ceiling ([coord.Claims] may refuse longer). It TRAVELS THE CLOCK it passes
// and never waits: the deadline is the record's, judged against the next
// caller's `now`, so a backend reaping on its own clock could not pass.
func CheckClaimLapsesAtItsOwnTTL(ctx context.Context, f coord.Fleet, at time.Time) []error {
	const (
		short = "gitlab|lapsing"
		long  = "mattermost|alice|p1"
	)
	shortTTL, longTTL := time.Minute, 3*time.Minute
	claim := func(key string, ttl time.Duration, now time.Time) (bool, error) {
		return f.Claim(ctx, key, ttl, now)
	}
	var errs []error
	for _, first := range []struct {
		key string
		ttl time.Duration
	}{{short, shortTTL}, {long, longTTL}} {
		if failed := landed(claim(first.key, first.ttl, at)).as(
			fmt.Sprintf("the first Claim of %q", first.key)); len(failed) > 0 {
			return failed
		}
	}
	// STILL HELD inside its own ttl, which is what stops this passing
	// against a backend that claims nothing at all.
	if again, err := claim(short, shortTTL, at.Add(shortTTL-time.Second)); err != nil {
		errs = append(errs, fmt.Errorf("a claim of %q inside its ttl: %w", short, err))
	} else if again {
		errs = append(errs, fmt.Errorf("a second caller claimed %q a second before its "+
			"%v ttl ran out", short, shortTTL))
	}
	// LAPSED AT ITS OWN TTL, and taken over once.
	past := at.Add(shortTTL + time.Second)
	switch took, err := claim(short, shortTTL, past); {
	case err != nil:
		errs = append(errs, fmt.Errorf("a claim of %q past its ttl: %w", short, err))
	case !took:
		errs = append(errs, fmt.Errorf("%q was still claimed a second past its own %v "+
			"ttl: a claim held for longer than it asked swallows the replay its "+
			"ttl was sized to let through", short, shortTTL))
	default:
		if twice, err := claim(short, shortTTL, past); err != nil {
			errs = append(errs, fmt.Errorf("a second takeover of %q: %w", short, err))
		} else if twice {
			errs = append(errs, fmt.Errorf("a lapsed claim of %q was taken over by two "+
				"callers at one instant: a takeover that is not a compare-and-set "+
				"delivers the duplicate the claim exists to stop", short))
		}
	}
	// AND THE LONG CLAIM IS STILL HELD past the short one's horizon — it
	// asked for longer.
	for _, inside := range []time.Duration{shortTTL + time.Second, longTTL - time.Second} {
		if again, err := claim(long, longTTL, at.Add(inside)); err != nil {
			errs = append(errs, fmt.Errorf("a claim of %q %v in: %w", long, inside, err))
		} else if again {
			errs = append(errs, fmt.Errorf("%q, claimed for %v, was taken again %v in: a "+
				"claim held for less than it asked lets the duplicate through", long,
				longTTL, inside))
		}
	}
	if took, err := claim(long, longTTL, at.Add(longTTL+time.Second)); err != nil {
		errs = append(errs, fmt.Errorf("a claim of %q past its ttl: %w", long, err))
	} else if !took {
		errs = append(errs, fmt.Errorf("%q was still claimed a second past its own %v ttl",
			long, longTTL))
	}
	return errs
}

// CheckUnnamedRecordsAreRefused verifies that a first-claim-wins claim
// answers an empty key with an ERROR rather than with false.
//
// The three-valued rule at its narrowest. A caller that named nothing has not
// lost a race to anybody, so "false" — which every caller of a claim reads as
// "somebody else has this" — drops the delivery or refuses the setup callback.
func CheckUnnamedRecordsAreRefused(ctx context.Context, f coord.Fleet, at time.Time) []error {
	var errs []error
	if ok, err := f.Claim(ctx, "", coord.ClaimTTL, at); err == nil {
		errs = append(errs, fmt.Errorf("an empty key reached Claim, which answered "+
			"%t rather than an error: a caller that named nothing has not lost a "+
			"race to anybody, and false reads as a delivery a peer already took", ok))
	}
	return errs
}

// Racers is how many callers [CheckCreatesOverARemovedRecordAreRaces] sends
// at a removed record at once, and RacedRounds how many times it removes one
// and races them.
//
// SIZED TO MEET THE RARE ANSWER, not to load the store. On a replicated KV
// stream only some of a race's losers are first told that a write to the
// record is still in process at the leader — the answer the check exists for,
// about a fifth of them, measured at three replicas — so one round's seven
// losers all miss it about a fifth of the time, and eight rounds' fifty-six
// about four times in a million.
const (
	Racers      = 8
	RacedRounds = 8
)

// CheckCreatesOverARemovedRecordAreRaces verifies that callers racing to write
// a record just REMOVED are each told what happened (every charge landed, one
// creator first, the rest told they lost), none that the store is down.
//
// A removal on a KV bucket leaves a MARKER, and a create over one is a
// compare-and-set on its revision. On a replicated stream the broker answered
// some losers with a refusal in neither client sentinel, which every create
// read as an outage (a delivery claim ran twice): the leader saying a write is
// still IN PROCESS, which decides nothing. So each race's set-up is held to
// having landed ([landed]), and the backend to waiting that answer out. EVERY
// VERB WHOSE RECORD CAN BE REMOVED is raced (one classifier serves all), and
// the token counters ([raceBudgets]), whose losers get the same.
func CheckCreatesOverARemovedRecordAreRaces(ctx context.Context, f coord.Fleet,
	at time.Time) []error {

	for round := range RacedRounds {
		for _, check := range []func(context.Context, coord.Fleet, int, time.Time) []error{
			raceClaims, raceBudgets, raceRuns, raceMailboxes, raceSecrets,
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
		out = append(out, fmt.Errorf("%s racing %d callers to create one record "+
			"answered an error — a caller that lost to a first writer, told the "+
			"store is down: %w", verb, Racers, err))
	}
	return out
}

// landed is what a race's set-up step is held to: it wrote, or removed, what
// it was asked to.
//
// A set-up step that answered "did nothing" for a record no other caller
// touches means the race after it is not over a record just removed, and the
// race then reports the wrong thing — "nobody won" is the right answer over a
// record that is still there. Reported here, the failure names the step that
// went wrong: a removal refused by a leader that had not finished the write
// before it was one, and it surfaced as a create race with no winner.
//
// Taken as the step's own two results, so a call reads as the step it checks:
// landed(f.Claim(ctx, key, coord.ClaimTTL, at)).as("the first Claim").
func landed(ok bool, err error) setUp { return setUp{ok: ok, err: err} }

// setUp is one set-up step's two results, waiting for the name it is
// reported under.
type setUp struct {
	ok  bool
	err error
}

// as is every error the step produced, named.
func (s setUp) as(step string) []error {
	switch {
	case s.err != nil:
		return []error{fmt.Errorf("%s: %w", step, s.err)}
	case !s.ok:
		return []error{fmt.Errorf("%s answered that it did nothing, for a record "+
			"no other caller touches", step)}
	}
	return nil
}

// firstOfMany is the error for a create race that did not have exactly one
// winner.
func firstOfMany(verb string, won int) error {
	return fmt.Errorf("%s racing %d callers over a record just removed: %d "+
		"were told they wrote it, want exactly one", verb, Racers, won)
}

func raceClaims(ctx context.Context, f coord.Fleet, round int, at time.Time) []error {
	key := fmt.Sprintf("gitlab|raced-%d", round)
	if errs := landed(f.Claim(ctx, key, coord.ClaimTTL, at)).as("the first Claim"); len(errs) > 0 {
		return errs
	}
	if err := f.Release(ctx, key); err != nil {
		return []error{fmt.Errorf("the release: %w", err)}
	}
	won, errs := race(func() (bool, error) { return f.Claim(ctx, key, coord.ClaimTTL, at) })
	if len(errs) > 0 {
		return raced("Claim", errs)
	}
	if won != 1 {
		return []error{firstOfMany("Claim", won)}
	}
	return nil
}

// raceBudgets races charges to a counter NOTHING HAS WRITTEN.
//
// The windowed counters have no removal verb: a window's allowance comes back
// by the roll inside the charge that crosses its boundary, and a record leaves
// only with its bucket's age (coord.BudgetRetention). So the create every
// racer but the first loses is a create at zero rather than one over a marker
// — and the leader answers it the same two ways, a decided refusal and a write
// still in process, which is why it is in this check. Every charge is
// uncapped, so a correct backend admits and counts every one of them; a
// backend that read a lost create as an outage fails a round that had room,
// which is a company's turn stopped by its own concurrency.
func raceBudgets(ctx context.Context, f coord.Fleet, round int, at time.Time) []error {
	seat := coord.AgentScope(fmt.Sprintf("raced-%d", round))
	windows := coord.WindowsAt(at, time.UTC)
	won, errs := race(func() (bool, error) {
		spend, err := f.Charge(ctx, coord.ChargeRequest{Seat: seat, Tokens: 1, Windows: windows})
		return spend.OK, err
	})
	if len(errs) > 0 {
		return raced("Charge", errs)
	}
	used, err := f.Used(ctx, seat, windows)
	if err != nil {
		return []error{fmt.Errorf("reading the counter back: %w", err)}
	}
	if counted := used.Windows[0].Used; won != Racers || counted != Racers {
		return []error{fmt.Errorf("%d uncapped charges racing to a counter nothing "+
			"had written: %d admitted and %d counted, want every one", Racers, won, counted)}
	}
	return nil
}

func raceRuns(ctx context.Context, f coord.Fleet, round int, _ time.Time) []error {
	turn := fmt.Sprintf("turn-raced-%d", round)
	created := landed(f.CreateSandboxRun(ctx, turn, []byte(`{"n":1}`)))
	if errs := created.as("the first CreateSandboxRun"); len(errs) > 0 {
		return errs
	}
	rec, found, err := f.SandboxRun(ctx, turn)
	if err != nil {
		return []error{fmt.Errorf("reading the run back: %w", err)}
	}
	if !found {
		return []error{errors.New("a run just created read back as absent")}
	}
	_, removed, err := f.MutateSandboxRun(ctx, turn, func(current coord.Record) (coord.SandboxRunChange, error) {
		return coord.SandboxRunChange{Delete: current.Version == rec.Version}, nil
	})
	if errs := landed(removed, err).as("the run's delete"); len(errs) > 0 {
		return errs
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

func raceMailboxes(ctx context.Context, f coord.Fleet, round int, _ time.Time) []error {
	handle := fmt.Sprintf("raced-%d", round)
	rec := seat(handle)
	stored, created, err := f.CreateMailbox(ctx, rec)
	if errs := landed(created, err).as("the first CreateMailbox"); len(errs) > 0 {
		return errs
	}
	removed := landed(f.DeleteMailbox(ctx, rec.Seat, stored.Version))
	if errs := removed.as("the mailbox's delete"); len(errs) > 0 {
		return errs
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
	if errs := landed(f.CreateSecret(ctx, rec)).as("the first CreateSecret"); len(errs) > 0 {
		return errs
	}
	if errs := landed(f.DeleteSecret(ctx, rec.Name)).as("the secret's delete"); len(errs) > 0 {
		return errs
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
