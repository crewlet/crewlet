package learning

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/compact"
)

// EpisodeAccountBytes bounds what a prompt carries of one past turn's account
// of what it did ([Episode.PlanSummary]) before that account is condensed.
//
// Six hundred: the three or four sentences a review's account of what landed
// runs to — the longest the engine's own prompts ask of it — so an account a
// review wrote is shown whole and what gets condensed is a long final answer.
// The recall block carries three of them, about a quarter of the room the
// thread block is given (8000 bytes) in the same system prompt, which is
// re-sent on every round of every phase.
const EpisodeAccountBytes = 600

// EpisodeAccount is what a past turn DID, as a prompt carries it, on one line:
// whole within [EpisodeAccountBytes]; past it, rewritten to fit by the reading
// seat's auxiliary model and marked as a rewrite; and where no rewrite can be
// had, named by its size — never a fragment of it, which a reader takes for
// the whole account.
//
// "" for a turn that recorded nothing it did.
//
// WHY IT IS SHOWN AT ALL: the label beside it ([Episode.TaskSummary]) says
// what kind of event woke the turn — "Message from Ana: Slack message" — and
// nothing of what came of it, so a recall of three similar turns used to tell
// a seat three times that somebody had sent a message.
func EpisodeAccount(ctx context.Context, ep Episode, fit compact.Bound) string {
	account := flatten(ep.PlanSummary)
	if len(account) <= EpisodeAccountBytes {
		return account
	}
	res, err := fit.Fit(ctx, compact.KindOutcome, account, EpisodeAccountBytes)
	if err == nil {
		if rewritten := flatten(res.Text); rewritten != "" {
			return strings.TrimSpace(rewritten + " " + res.Note())
		}
	}
	if err != nil {
		log.DebugContext(ctx, "episode_account_not_condensed", "agent_handle", ep.Handle,
			"bytes", len(account), "error", err.Error())
	}
	return compact.Omitted(account)
}
