package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Where every turn's derived operation ids are minted. A turn's tracker and
// knowledge-base writes derive their ids from the instant its identity began
// ([builtin.Actor.OperationSince]), so a re-run collapses onto the first
// attempt's writes. An old dispatch trigger, or a resume long after the work
// began, puts that instant past the ledger's horizon, where every new write is
// answered `unknown` for good (statelog's mint.go). Both paths decide through
// [rebaseFor] alone ([Engine.describeTurn], [Engine.describeResume]), judging
// each attempt by its own clock, since an inherited instant may since have
// passed the horizon. A rebase is recorded in [coord.Rebases] before the
// attempt writes, so later attempts on any node derive the same ids; it is read
// only past the horizon. Cost: attempts judged either side of the horizon mint
// under two instants, a duplicate write rather than lost ones.

// rebaseStore is what [rebaseFor] needs of [coord.Rebases].
type rebaseStore interface {
	Rebase(ctx context.Context, seed string) (time.Time, uint64, error)
	RecordRebase(ctx context.Context, seed string, at time.Time, version uint64) (bool, error)
}

// rebaseRounds bounds [rebaseFor]'s read-decide-write loop. A lost race costs
// one more round (the loser inherits the winner's instant); a third needs
// another attempt recording between read and write twice running. Four leaves
// room; running out is an error, never a guess at the instant.
const rebaseRounds = 4

// rebaseFor is the instant the attempt at `at` mints identity's derived
// operation ids at, or zero where that is the identity's own start (work begun
// inside [statelog.MintHorizon]). identity carries no rebase of its own: its
// [builtin.Actor.OperationSeed] keys the record and its
// [builtin.Actor.OperationSince] is the start judged against.
//
// An error keeps the attempt from running: not knowing whether an earlier
// attempt rebased, either choice is wrong — the start loses every write past
// the horizon, and a new instant repeats the earlier attempt's writes.
func rebaseFor(ctx context.Context, store rebaseStore, identity builtin.Actor,
	at time.Time) (_ time.Time, err error) {
	defer origin.Stamp(&err)

	start := identity.OperationSince()
	if _, rebases := statelog.MintAt(start, at); !rebases {
		return time.Time{}, nil
	}
	seed := identity.OperationSeed()
	if seed == "" {
		// Nothing is derived from an empty seed: builtin's opIDFor mints
		// each id fresh at its call.
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
			// Inherited: an earlier rebase is still within every ledger, so
			// this attempt derives the same ids.
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
		// Lost the race: another attempt recorded since the read; re-read
		// and inherit its instant.
	}
	return time.Time{}, fmt.Errorf("engine: lost the race to record where the "+
		"writes of work %q are minted %d times running, so this attempt does not "+
		"run rather than guess", seed, rebaseRounds)
}

// rebases is the fleet store rebases are recorded in, read by every node. Nil
// on an engine built without backends (a one-frame test), which [rebaseFor]
// refuses only where a rebase is needed.
func (e *Engine) rebases() rebaseStore {
	if e == nil || e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return e.backends.Fleet
}
