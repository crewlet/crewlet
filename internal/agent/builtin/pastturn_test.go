package builtin_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// What query_episodes says about each past turn it returns.

// fixedEpisodes answers every read with the same rows.
type fixedEpisodes struct{ rows []learning.Episode }

func (f fixedEpisodes) Recent(context.Context, string, int) ([]learning.Episode, error) {
	return f.rows, nil
}

func (f fixedEpisodes) ForConversation(context.Context, string, string, int) ([]learning.Episode, error) {
	return f.rows, nil
}

// rewriter is an auxiliary model that answers every rewrite with one text.
type rewriter struct {
	mu     sync.Mutex
	answer string
	asked  []string
	uses   []auxspend.Use
}

func (r *rewriter) Model() string { return "aux-small" }

func (r *rewriter) Complete(_ context.Context, req llm.Request) (*llm.Completion, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, m := range req.Messages {
		b.WriteString(m.Content)
	}
	r.asked = append(r.asked, b.String())
	return &llm.Completion{Model: "aux-small", Content: r.answer}, nil
}

func (r *rewriter) Auxiliary(_ *org.Role, use auxspend.Use) (chain.Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uses = append(r.uses, use)
	return chain.Member{Key: "aux", Provider: r}, nil
}

func (r *rewriter) prompts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.asked...)
}

var whenever = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

// WHAT A PAST TURN DID IS IN THE ANSWER, beside what woke it. The label names
// the kind of event — every chat turn's is "Message from <someone>" — so an
// answer of labels and outcomes told a seat it had been messaged, and nothing
// of what it did about it.
func TestQueryEpisodesSaysWhatEachTurnDid(t *testing.T) {
	t.Parallel()
	tool := registered(t, builtin.Deps{Episodes: fixedEpisodes{rows: []learning.Episode{{
		Kind: learning.KindRaw, StartedAt: whenever,
		TaskSummary:   "Message from Ana: Slack message",
		PlanSummary:   "Rolled staging back to v41 and\nposted the runbook fix in #ops.",
		ReviewOutcome: "done",
	}}}}, builtin.QueryEpisodesTool)

	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{})
	want := "- 2026-09-01T09:00:00Z — Message from Ana: Slack message\n" +
		"    outcome: done\n" +
		"    what it did: Rolled staging back to v41 and posted the runbook fix in #ops."
	if !strings.Contains(res.Output, want) {
		t.Fatalf("output =\n%s\nwant it to contain\n%s", res.Output, want)
	}
}

// A LONG ACCOUNT IS CONDENSED BY THE SEAT'S AUXILIARY MODEL, never cut: an
// account is a turn's final answer when no review wrote one, and twenty-five of
// those whole would spend a phase's context on history.
func TestQueryEpisodesCondensesALongAccount(t *testing.T) {
	t.Parallel()
	model := &rewriter{answer: "fixed the cache key that broke the staging deploy"}
	long := "OPENING-" + strings.Repeat("the deploy log said ", 200) + "and the fix was the cache key."
	tool := registered(t, builtin.Deps{
		Episodes: fixedEpisodes{rows: []learning.Episode{{
			Kind: learning.KindRaw, StartedAt: whenever,
			TaskSummary: "Message from Ana: Slack message", PlanSummary: long,
		}}},
		Compact: compact.New(model, compact.NewCache()),
	}, builtin.QueryEpisodesTool)

	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{})
	if !strings.Contains(res.Output, "what it did: fixed the cache key that broke the staging deploy") ||
		!strings.Contains(res.Output, "condensed by a model") {
		t.Fatalf("output = %q, want the account condensed and marked as a rewrite", res.Output)
	}
	if strings.Contains(res.Output, "OPENING-") {
		t.Fatalf("the account's opening was passed off beside its rewrite: %q", res.Output)
	}
	if asked := model.prompts(); len(asked) != 1 || !strings.Contains(asked[0], "and the fix was the cache key.") {
		t.Fatalf("the rewrite was not shown the whole account: %d calls", len(asked))
	}
	// THE REWRITE IS THE TURN'S OWN SPEND, named for what it rewrote: the
	// seat asked for its past turns mid-turn, so the condensation is part
	// of this turn's cost — filed under its run and its unit of work.
	want := auxspend.Use{Stage: types.AuxStageTurn, Purpose: types.AuxCondense(string(compact.KindOutcome)),
		TurnID: "run-1", WorkKey: "wk-1"}
	if len(model.uses) != 1 || model.uses[0] != want {
		t.Fatalf("the rewrite was filed as %+v, want %+v", model.uses, want)
	}
}

// A COMPACTED ROW IS ITS PATTERN. It stands for a cluster of turns and has no
// label or account of its own, so rendered as a turn it showed nothing but
// its date; what it holds is the pattern, how many turns it stands for and how
// they ended — counted from the members, never the model's word — and what
// varied.
func TestQueryEpisodesRendersACompactedRowAsItsPattern(t *testing.T) {
	t.Parallel()
	tool := registered(t, builtin.Deps{Episodes: fixedEpisodes{rows: []learning.Episode{{
		Kind: learning.KindCompacted, Count: 12,
		StartedAt: whenever, EndedAt: whenever.AddDate(0, 1, 0),
		CommonTaskPattern: "Triaging a failed staging deploy",
		ReviewOutcome:     "done", SuccessRate: 0.75,
		NotablePatterns: "Two were handed to the SRE lead when the database was involved.",
	}}}}, builtin.QueryEpisodesTool)

	res := callFor(t, tool, turnFor(t, "agent-ceo"), map[string]any{})
	for _, want := range []string{
		"- 2026-09-01 to 2026-10-01 — 12 turns like this: Triaging a failed staging deploy",
		"outcome: done (9 of 12 done)",
		"what varied: Two were handed to the SRE lead when the database was involved.",
	} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("output =\n%s\nmissing %q", res.Output, want)
		}
	}
}
