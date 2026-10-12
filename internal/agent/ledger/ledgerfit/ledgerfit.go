// Package ledgerfit rewrites the payloads a prior-work ledger names as over
// budget, through the seat's auxiliary model.
//
// It is the half [github.com/crewlet/crewlet/internal/agent/ledger] cannot
// hold: that package imports nothing from crewlet, so it can only NAME the
// payloads a render needs fitted. This package fits them and says, per kind,
// what a reader is shown when no rewrite can be had — which is the decision
// that matters, because the obvious fallback is the cut the ledger used to
// make and must never make again.
//
//   - AN ARGUMENT OR AN ERROR that cannot be rewritten is OMITTED by size and
//     digest ([compact.Omitted]). It is a payload: the call line beside it
//     still says which tool ran, where, and whether it worked, and the
//     digest still tells two identical bodies from two different ones.
//     Carrying it whole would put a page of HTML on a line re-sent on every
//     round, and a fragment of it would read as the payload.
//   - A ROUND'S PRODUCED TEXT that cannot be rewritten renders WHOLE. It is
//     the turn's own draft, the thing the next round exists to revise, and
//     prompt weight is a smaller cost than a round that cannot see what it
//     is correcting.
package ledgerfit

import (
	"context"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/compact"
)

// Condensed is the marker a rewritten payload renders behind, so the reader
// never takes a model's paraphrase of a message for the message.
const Condensed = "(condensed) "

// Fitter is the one thing a fit needs: a compactor bound to the seat whose
// ledger this is.
type Fitter interface {
	Fit(ctx context.Context, kind compact.Kind, text string, budget int) (compact.Result, error)
}

// Fit rewrites every piece, in parallel, and returns what each renders as.
//
// It never fails: every piece ends up rewritten, omitted, or absent (whole),
// and which one is the per-kind rule in the package doc. The rewrites run
// [compact.Parallel] at a time, and a piece the compactor has already seen
// this turn is answered from its cache — so the ledger re-rendered at the top
// of every round pays for a payload once.
func Fit(ctx context.Context, fitter Fitter, pieces []ledger.Piece) ledger.Fitted {
	if len(pieces) == 0 {
		return nil
	}
	out := make(ledger.Fitted, len(pieces))
	var mu sync.Mutex
	var group errgroup.Group
	group.SetLimit(compact.Parallel)
	for _, piece := range pieces {
		group.Go(func() error {
			shown, ok := fit(ctx, fitter, piece)
			if ok {
				mu.Lock()
				out[piece] = shown
				mu.Unlock()
			}
			return nil
		})
	}
	_ = group.Wait()
	return out
}

// fit is one piece's rendering, or false to render it whole.
func fit(ctx context.Context, fitter Fitter, piece ledger.Piece) (string, bool) {
	kind, ok := kinds[piece.Kind]
	if !ok {
		// A kind this build does not know is rendered whole: it is the
		// only answer that cannot be wrong about what the text was.
		return "", false
	}
	var res compact.Result
	var err error
	if fitter != nil {
		res, err = fitter.Fit(ctx, kind, piece.Text, piece.Limit)
	} else {
		err = compact.ErrUnavailable
	}
	switch {
	case err == nil && piece.Kind == ledger.PieceProduced:
		return res.Note() + "\n" + res.Text, true
	case err == nil:
		return Condensed + res.Text, true
	case piece.Kind == ledger.PieceProduced:
		return "", false
	default:
		return compact.Omitted(piece.Text), true
	}
}

var kinds = map[ledger.PieceKind]compact.Kind{
	ledger.PieceArgument: compact.KindArgument,
	ledger.PieceError:    compact.KindToolError,
	ledger.PieceProduced: compact.KindProduced,
}
