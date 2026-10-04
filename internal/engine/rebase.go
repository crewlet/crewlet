package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Where every turn's derived operation ids are minted.
//
// A turn's tracker and knowledge-base writes derive their operation ids from
// its identity — the unit of work, or the run where there is none — minted at
// the instant that identity BEGAN ([builtin.Actor.OperationSince]), so a
// re-run derives the ids its first attempt derived and the ledger collapses
// each repeated write onto the first copy. That instant can be further back
// than any node's operation ledger remembers, and then every new write under
// it is answered `unknown` on every node for good — see statelog's mint.go.
// Two paths reach that point:
//
//   - a DISPATCH whose trigger is that old: a seat's mailbox retains what is
//     published while nothing consumes it, so a seat nobody placed for a
//     month, or a fleet that was down, is handed a backlog whose work began
//     before the ledger's horizon;
//   - a RESUME that comes that long after the work began: a turn parked on a
//     coding run, or on a person's answer, for a month.
//
// Both are decided by ONE function, [rebaseFor], called from the one frame
// each path builds the turn its tools read from ([Engine.describeTurn],
// [Engine.describeResume]) — so neither path can hand its tools an instant
// the rule did not choose.
//
// EVERY ATTEMPT IS JUDGED AGAINST ITS OWN CLOCK. An attempt's writes are
// decided while the attempt runs, so an instant chosen by an earlier attempt
// and inherited by a retry days later has to be judged again: a failed resume
// retried by a person's next answer weeks on, or a completion re-fired for as
// long as a provider is down, is exactly as far past the ledger as if nobody
// had judged it before. The rule inherits while it can (statelog.MintAt) and
// rebases onto the attempt's own instant once it cannot.
//
// A REBASE IS RECORDED BEFORE THE ATTEMPT WRITES ANYTHING, in the fleet's
// [coord.Rebases] under the ids' seed, and every later attempt at the same
// work reads it back — the crash re-run of a dispatch, a retried resume, the
// next half of a turn a coding run parked, on whichever node takes it — so it
// derives the ids the attempt before it derived, and a repeated write lands
// once. The read is made only where the work's own start is past the horizon,
// which is the only place a rebase can have been recorded: the ordinary turn
// pays nothing for it.
//
// WHAT IT COSTS, stated rather than hidden. An attempt judged just short of
// the horizon and a retry judged just past it mint under two instants, so a
// write the first made and the second repeats is written twice — the cheaper
// failure by far, against every write the retry makes being lost. And during a
// rolling upgrade a build from before this rule mints every attempt at the
// start, reading no record: a retry that crosses between the two builds inside
// the retention's last day writes the earlier attempt's writes a second time,
// whichever build ran first, and past the retention the older build's writes
// are lost exactly as they always were on that build. No gate can make an
// older build read a record it does not know exists.

// rebaseStore is what [rebaseFor] needs of the coordination store — see
// [coord.Rebases], which the fleet store implements.
type rebaseStore interface {
	Rebase(ctx context.Context, seed string) (time.Time, uint64, error)
	RecordRebase(ctx context.Context, seed string, at time.Time, version uint64) (bool, error)
}

// rebaseRounds bounds [rebaseFor]'s read-decide-write loop.
//
// One round is the ordinary case, and a lost race costs one more: the loser
// reads the winner's instant, which is within the horizon of its own clock, so
// it inherits and writes nothing. A third round needs a third attempt at the
// same work recording between this one's read and its write, twice in a row,
// which nothing does. Four is that with room, and running out is an error —
// the attempt does not run — never a guess at the instant.
const rebaseRounds = 4

// rebaseFor is the instant the attempt at `at` of the turn with this identity
// mints its derived operation ids at, where that is not the start of the
// identity they are seeded from — and the zero instant where it is, which is
// every turn whose work began inside [statelog.MintHorizon].
//
// identity is the turn's seed and start, with no rebase of its own: its
// [builtin.Actor.OperationSeed] keys the record, and its
// [builtin.Actor.OperationSince] is the start the rule is judged against.
//
// THREE-VALUED, as every read of the coordination store is: an instant, the
// start (zero), or an error — and the error keeps the attempt from running at
// all, because an attempt that could not learn whether an earlier one rebased
// would have to choose an instant, and either choice is wrong for one of the
// two: the start loses every write past the horizon, and a new instant writes
// again whatever the earlier attempt wrote.
func rebaseFor(ctx context.Context, store rebaseStore, identity builtin.Actor,
	at time.Time) (time.Time, error) {

	start := identity.OperationSince()
	if _, rebases := statelog.MintAt(start, at); !rebases {
		return time.Time{}, nil
	}
	seed := identity.OperationSeed()
	if seed == "" {
		// NOTHING IS DERIVED from an empty seed: every id such a turn
		// writes under is minted fresh at its call (see builtin's opIDFor),
		// so there is no start to be past.
		return time.Time{}, nil
	}
	if store == nil {
		return time.Time{}, fmt.Errorf("engine: the work %q began at %s, past the "+
			"%s the operation ledger can vouch for, and this node has no "+
			"coordination store to record where its writes are minted instead",
			seed, start.Format(time.RFC3339), statelog.MintHorizon)
	}
	for range rebaseRounds {
		recorded, version, err := store.Rebase(ctx, seed)
		if err != nil {
			return time.Time{}, fmt.Errorf("engine: read where the writes of work "+
				"%q are minted — its start %s is past the operation ledger's "+
				"horizon, and minting without knowing whether an earlier attempt "+
				"rebased would lose its writes or repeat that attempt's: %w",
				seed, start.Format(time.RFC3339), err)
		}
		carried := start
		if version != 0 && recorded.After(start) {
			carried = recorded
		}
		mint, rebases := statelog.MintAt(carried, at)
		if !rebases {
			// INHERITED: an earlier attempt rebased, recently enough that
			// its writes are still in every ledger, and this one derives
			// the same ids.
			log.DebugContext(ctx, "turn_rebase_inherited", "seed", seed,
				"began", start, "rebased_to", mint)
			return mint, nil
		}
		won, err := store.RecordRebase(ctx, seed, mint, version)
		if err != nil {
			return time.Time{}, fmt.Errorf("engine: record where the writes of "+
				"work %q are minted: %w", seed, err)
		}
		if won {
			log.InfoContext(ctx, "turn_rebased", "seed", seed, "began", start,
				"inherited", carried, "rebased_to", mint,
				"detail", "this turn's work began longer ago than the operation "+
					"ledger can vouch for, so its writes are minted at this "+
					"attempt instead of at that start; a write an earlier "+
					"attempt made under the old instant and this one repeats "+
					"is a second write")
			return mint, nil
		}
		// LOST THE RACE: another attempt at the same work recorded first,
		// or moved the record since the read. Read what it wrote and
		// decide again — its instant is one this attempt inherits.
	}
	return time.Time{}, fmt.Errorf("engine: lost the race to record where the "+
		"writes of work %q are minted %d times running, so this attempt does not "+
		"run rather than guess", seed, rebaseRounds)
}

// rebases is the store every turn's rebase is recorded in: the fleet's, where
// every node reads it. Nil on an engine built without its backends — a test
// driving one frame — which [rebaseFor] refuses by name only where a rebase is
// actually needed.
func (e *Engine) rebases() rebaseStore {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return e.backends.Fleet
}
