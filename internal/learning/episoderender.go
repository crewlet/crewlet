package learning

import (
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/provenance"
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

// EpisodeRewriteTimeout bounds every rewrite ONE RENDER of past turns makes
// ([PastTurns]) — the turn-start block's and the query_episodes tool's alike —
// as ONE deadline from the render's start, never one per rewrite.
// Thirty seconds is the turn-start auxiliary calls' own bound
// (prefetch.AuxTimeout): a model is waiting, and rewrites not back by then mean
// a provider in trouble, better served by the size note ([compact.Omitted]).
// It is held inside the one function every caller renders through.
//
// One deadline, because [PastTurns] runs at most [compact.Parallel] rewrites at
// a time, so a per-rewrite deadline bounds each wave (six and a half minutes
// for twenty-five turns). A rewrite still queued at the deadline is never sent
// and is named by its size.
const EpisodeRewriteTimeout = 30 * time.Second

// PastTurn is what a prompt carries of one recalled turn's own words, each on
// one line within the caller's bound: what it was asked ([episodeAsk]) and what
// it did ([episodeAccount]). Both "" for a compacted row, which has neither and
// is rendered as its pattern.
type PastTurn struct {
	Ask, Account string
}

// PastTurns renders episodes' asks and accounts within bytes, positionally —
// every rewrite they need made concurrently, since each is a model call and a
// reader is waiting on all of them, but at most [compact.Parallel] at once, the
// compactor's own bound on how many rewrites one caller opens against a seat's
// provider, and ALL of them held to one deadline: [EpisodeRewriteTimeout] from
// now, or the caller's own when it is sooner — which is how a caller that
// renders more than once shares one budget between the renders.
//
// ONE IMPLEMENTATION for the turn-start block and the query_episodes tool, so
// the two cannot come to differ about how long a reader waits or how hard a
// seat's provider is pressed — and the only way to a rewrite of a past turn,
// so no caller can make one outside the deadline.
func PastTurns(ctx context.Context, episodes []Episode, fit compact.Bound, bytes int) []PastTurn {
	ctx, cancel := context.WithTimeout(ctx, EpisodeRewriteTimeout)
	defer cancel()
	out := make([]PastTurn, len(episodes))
	var group errgroup.Group
	group.SetLimit(compact.Parallel)
	for i, ep := range episodes {
		if ep.Kind == KindCompacted {
			continue
		}
		group.Go(func() error {
			out[i].Ask = episodeAsk(ctx, ep, fit, bytes)
			return nil
		})
		group.Go(func() error {
			out[i].Account = episodeAccount(ctx, ep, fit, bytes)
			return nil
		})
	}
	_ = group.Wait() // every render answers nil: a failed rewrite is a size note
	return out
}

// episodeAccount is what a past turn DID, as a prompt carries it, on one line:
// whole within bytes; past them, rewritten to fit by the reading seat's
// auxiliary model and marked as a rewrite; and where no rewrite can be had
// before ctx's deadline ([PastTurns]), named by its size — never a fragment of
// it, which a reader takes for the whole account. bytes is
// [EpisodeAccountBytes] for a block a reader is shown, or a caller's larger
// bound for text that is itself the input of a model call.
//
// "" for a turn that recorded nothing it did.
//
// It is shown because the label beside it ([Episode.TaskSummary]) says only
// what kind of event woke the turn, nothing of what came of it.
func episodeAccount(ctx context.Context, ep Episode, fit compact.Bound, bytes int) string {
	return episodeLine(ctx, fit, compact.KindOutcome, ep, ep.PlanSummary, bytes)
}

// episodeAsk is what a past turn was ASKED, by the rule [episodeAccount]
// states: whole within bytes, rewritten past them, named by its size where no
// rewrite can be had — and "" for a turn asked nothing.
//
// SHOWN BESIDE THE LABEL, never in its place: the label is the waking event's
// one line, and a seat that read it as the question it was asked learned
// nothing of what it was asked.
//
// A REWRITE OF AN ASK IS STILL ITS WRITERS' WORDS: the rewrite reads the ask
// under the label it was asked with ([Episode.AskProvenance]), and what comes
// back is fenced under that same label wherever it is shown ([RenderPastTurn]).
func episodeAsk(ctx context.Context, ep Episode, fit compact.Bound, bytes int) string {
	if ep.AskProvenance != nil {
		fit = fit.Labelled(*ep.AskProvenance)
	}
	return episodeLine(ctx, fit, compact.KindTask, ep, ep.Ask, bytes)
}

// RenderPastTurn is one recalled turn as a model reads it: THE ONE RENDERING,
// shared by the turn-start "## Similar prior work" block, the episode summary
// model's input and `query_episodes` (ADR-0040). past is what [PastTurns] made
// of its ask and account; b is the token the reader's fences carry.
//
// A raw turn is its start, wake label, outcome, tools, ask and account. The ask
// is usually somebody else's words, so it sits inside a fence of b under the
// label it was asked with ([Episode.AskProvenance]), neutralised where it was
// the company's or the seat's own. Everything else is the engine's label or the
// seat's account, neutralised so nothing opens or closes a fence. A turn with
// none of label, ask, account renders nothing. A compacted row is the pattern,
// its turn and done counts, and what varied (neutralised); it keeps no ask.
func RenderPastTurn(b provenance.Boundary, ep Episode, past PastTurn) string {
	var out strings.Builder
	if ep.Kind == KindCompacted {
		count := max(ep.Count, 1)
		fmt.Fprintf(&out, "- %s to %s — %d turns like this: %s\n",
			ep.StartedAt.Format(time.DateOnly), ep.EndedAt.Format(time.DateOnly), count,
			provenance.Neutralise(b, flatten(ep.CommonTaskPattern)))
		if ep.ReviewOutcome != "" {
			fmt.Fprintf(&out, "  outcome: %s (%d of %d done)\n",
				provenance.Neutralise(b, ep.ReviewOutcome), ep.DoneCount(), count)
		}
		if notable := flatten(ep.NotablePatterns); notable != "" {
			fmt.Fprintf(&out, "  what varied: %s\n", provenance.Neutralise(b, notable))
		}
		return strings.TrimRight(out.String(), "\n")
	}
	label := flatten(ep.TaskSummary)
	if label == "" && past.Ask == "" && past.Account == "" {
		return ""
	}
	out.WriteString("- ")
	if !ep.StartedAt.IsZero() {
		out.WriteString(ep.StartedAt.UTC().Format(time.RFC3339) + " — ")
	}
	out.WriteString("woken by: " + provenance.Neutralise(b, orElse(label, "(not recorded)")))
	var notes []string
	if ep.ReviewOutcome != "" {
		notes = append(notes, "outcome: "+ep.ReviewOutcome)
	}
	if len(ep.ToolSequence) > 0 {
		notes = append(notes, "tools: "+strings.Join(ep.ToolSequence, " → "))
	}
	if len(notes) > 0 {
		out.WriteString(" (" + provenance.Neutralise(b, strings.Join(notes, "; ")) + ")")
	}
	switch {
	case past.Ask == "":
	case ep.AskProvenance != nil:
		out.WriteString("\n  asked:\n" + ep.AskProvenance.Fence(b, past.Ask))
	default:
		out.WriteString("\n  asked: " + provenance.Neutralise(b, past.Ask))
	}
	if past.Account != "" {
		out.WriteString("\n  what it did: " + provenance.Neutralise(b, past.Account))
	}
	return out.String()
}

// episodeLine is one of a past turn's texts on one line, within bytes, its
// rewrite held to ctx's deadline — the render's ([PastTurns]).
func episodeLine(ctx context.Context, fit compact.Bound, kind compact.Kind, ep Episode,
	text string, bytes int,
) string {
	line := flatten(text)
	if len(line) <= bytes {
		return line
	}
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
