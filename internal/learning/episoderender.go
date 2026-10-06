package learning

import (
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/compact"
)

// EpisodeAccountBytes bounds what a prompt a reader sees carries of one past
// turn's account of what it did ([Episode.PlanSummary]) before it is
// condensed.
//
// Six hundred: the three or four sentences a review's account of what landed
// runs to — the longest the engine's own prompts ask of it — so an account a
// review wrote is shown whole and what gets condensed is a long final answer.
// The recall block carries three of them, about a quarter of the room the
// thread block is given (8000 bytes) in the same system prompt, which is
// re-sent on every round of every phase.
const EpisodeAccountBytes = 600

// EpisodeRewriteTimeout bounds one rewrite of one past turn's account, wherever
// it is made — the turn-start block and the query_episodes tool alike.
//
// THIRTY SECONDS, the turn-start auxiliary calls' own bound
// (prefetch.AuxTimeout) and for its reason: a model is waiting on the answer —
// a turn starting, or a tool call in the middle of one — and a rewrite of a
// few kilobytes that has not come back in that long is a provider in trouble,
// whose reader is better served by the size note ([compact.Omitted]) now than
// by the rewrite later. Held HERE, inside the one function every caller
// renders through, so no caller can make a rewrite without it: the tool's
// renders had none and waited out the compactor's own minute, plus its retry,
// per account. A call condensing n accounts at most [compact.Parallel] at a
// time therefore waits at most ⌈n / Parallel⌉ × this.
const EpisodeRewriteTimeout = 30 * time.Second

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
