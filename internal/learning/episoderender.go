package learning

import (
	"context"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/compact"
)

// EpisodeAccountBytes bounds what a prompt a reader sees carries of one past
// turn's own words — its account of what it did ([Episode.PlanSummary]), and
// what it was asked ([Episode.Ask]) — before each is condensed.
//
// Six hundred: the three or four sentences a review's account of what landed
// runs to — the longest the engine's own prompts ask of it — so an account a
// review wrote is shown whole and what gets condensed is a long final answer,
// and an ask of a chat message or two is shown whole and what gets condensed
// is a task's long description. The recall block carries three turns, about a
// quarter to a half of the room the thread block is given (8000 bytes) in the
// same system prompt, which is re-sent on every round of every phase.
const EpisodeAccountBytes = 600

// EpisodeRewriteTimeout bounds one rewrite of one past turn's account or ask,
// wherever it is made — the turn-start block and the query_episodes tool alike.
//
// THIRTY SECONDS, the turn-start auxiliary calls' own bound
// (prefetch.AuxTimeout) and for its reason: a model is waiting on the answer —
// a turn starting, or a tool call in the middle of one — and a rewrite of a
// few kilobytes that has not come back in that long is a provider in trouble,
// whose reader is better served by the size note ([compact.Omitted]) now than
// by the rewrite later. Held HERE, inside the one function every caller
// renders through, so no caller can make a rewrite without it: the tool's
// renders had none and waited out the compactor's own minute, plus its retry,
// per account. [PastTurns] makes at most [compact.Parallel] rewrites at a time,
// so n turns wait at most ⌈2n / Parallel⌉ × this.
const EpisodeRewriteTimeout = 30 * time.Second

// PastTurn is what a prompt carries of one recalled turn's own words, each on
// one line within the caller's bound: what it was asked ([EpisodeAsk]) and what
// it did ([EpisodeAccount]). Both "" for a compacted row, which has neither and
// is rendered as its pattern.
type PastTurn struct {
	Ask, Account string
}

// PastTurns renders episodes' asks and accounts within bytes, positionally —
// every rewrite they need made concurrently, since each is a model call and a
// reader is waiting on all of them, but at most [compact.Parallel] at once, the
// compactor's own bound on how many rewrites one caller opens against a seat's
// provider, and each held to [EpisodeRewriteTimeout].
//
// ONE IMPLEMENTATION for the turn-start block and the query_episodes tool, so
// the two cannot come to differ about how long a reader waits or how hard a
// seat's provider is pressed.
func PastTurns(ctx context.Context, episodes []Episode, fit compact.Bound, bytes int) []PastTurn {
	out := make([]PastTurn, len(episodes))
	var group errgroup.Group
	group.SetLimit(compact.Parallel)
	for i, ep := range episodes {
		if ep.Kind == KindCompacted {
			continue
		}
		group.Go(func() error {
			out[i].Ask = EpisodeAsk(ctx, ep, fit, bytes)
			return nil
		})
		group.Go(func() error {
			out[i].Account = EpisodeAccount(ctx, ep, fit, bytes)
			return nil
		})
	}
	_ = group.Wait() // every render answers nil: a failed rewrite is a size note
	return out
}

// EpisodeAccount is what a past turn DID, as a prompt carries it, on one line:
// whole within bytes; past them, rewritten to fit by the reading seat's
// auxiliary model and marked as a rewrite; and where no rewrite can be had
// inside [EpisodeRewriteTimeout], named by its size — never a fragment of it,
// which a reader takes for the whole account. bytes is [EpisodeAccountBytes]
// for a block a reader is shown, or a caller's larger bound for text that is
// itself the input of a model call.
//
// "" for a turn that recorded nothing it did.
//
// WHY IT IS SHOWN AT ALL: the label beside it ([Episode.TaskSummary]) says
// what kind of event woke the turn — "Message from Ana: Slack message" — and
// nothing of what came of it, so a recall of three similar turns used to tell
// a seat three times that somebody had sent a message.
func EpisodeAccount(ctx context.Context, ep Episode, fit compact.Bound, bytes int) string {
	return episodeLine(ctx, fit, compact.KindOutcome, ep, ep.PlanSummary, bytes)
}

// EpisodeAsk is what a past turn was ASKED, by the rule [EpisodeAccount]
// states: whole within bytes, rewritten past them, named by its size where no
// rewrite can be had — and "" for a turn asked nothing, or one written before
// its ask was stored (node migration 0042).
//
// SHOWN BESIDE THE LABEL, never in its place: the label is the waking event's
// one line, and a seat that read it as the question it was asked learned
// nothing of what it was asked.
func EpisodeAsk(ctx context.Context, ep Episode, fit compact.Bound, bytes int) string {
	return episodeLine(ctx, fit, compact.KindTask, ep, ep.Ask, bytes)
}

// episodeLine is one of a past turn's texts on one line, within bytes.
func episodeLine(ctx context.Context, fit compact.Bound, kind compact.Kind, ep Episode,
	text string, bytes int,
) string {
	line := flatten(text)
	if len(line) <= bytes {
		return line
	}
	ctx, cancel := context.WithTimeout(ctx, EpisodeRewriteTimeout)
	defer cancel()
	res, err := fit.Fit(ctx, kind, line, bytes)
	if err == nil {
		if rewritten := flatten(res.Text); rewritten != "" {
			return strings.TrimSpace(rewritten + " " + res.Note())
		}
	}
	if err != nil {
		log.DebugContext(ctx, "episode_text_not_condensed", "agent_handle", ep.Handle,
			"kind", string(kind), "bytes", len(line), "error", err.Error())
	}
	return compact.Omitted(line)
}
