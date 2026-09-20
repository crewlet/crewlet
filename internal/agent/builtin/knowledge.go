package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// SearchKnowledgeTool is the tool's wire name.
const SearchKnowledgeTool = "search_knowledge"

// searchHits is how many pages one call renders.
//
// Six, matching the turn-start prefetch block: each result is a title and a
// snippet the agent uses to decide what to open, and a dozen crowds out the
// task they were fetched for. Distinct from [knowledge.DefaultLimit], which
// is what the seam asks a backend for when nobody says.
const searchHits = 6

// searchQueryMax bounds the query a model may send.
//
// A query reaches a backend's own query language through the seam, and a
// model that pasted a whole thread in would search on prose no ranker can
// use. Four hundred bytes is a long sentence and several keywords.
//
// REFUSED, NOT CUT, which is the same call [ToolAnswerBytes] makes one
// direction over and the same one [github.com/crewlet/crewlet/internal/tracker.MaxQueryText]
// makes on the query it bounds. A cut query is not a shorter query — it is a
// DIFFERENT one, and the hits that come back are a plausible answer to
// something the model never asked, which it has no way to detect: the results
// are real pages, ranked, about the half of the sentence that survived. The
// cut was silent besides, so the tool's own `clip` sits two files away
// refusing to shorten an echoed argument for exactly this reason — "a
// shortened echo names a query the model never sent". A refusal naming the
// field costs one round and is the one failure a model reliably fixes.
const searchQueryMax = 400

// KnowledgeSearcher is query-time search over the team knowledge base, as
// this tool needs it.
//
// The two methods of [knowledge.Searcher] a search actually uses. Declared
// here rather than taking the seam's own interface so the tool can be
// exercised with a stub, and so this package never grows a second opinion
// about what a knowledge backend is.
type KnowledgeSearcher interface {
	CanSearch(seat *org.Role, o *org.Organization) bool
	Search(ctx context.Context, q knowledge.Query) []knowledge.Hit
}

// searchKnowledge searches the team knowledge base on demand.
//
// It replaces the engine's mid-turn re-fetch seam, which existed for one
// case the three-phase turn could not otherwise serve: a trigger that was a
// bare POINTER ("PR #42 got a comment") is unsearchable at turn start, so the
// turn-start block was skipped and the engine re-ran the search between the
// planning and acting phases, keyed on the plan the first had written. With
// one phase deciding and acting there is nothing between them and nothing to
// key on, and there no longer needs to be: the agent that just did the recon
// knows what to search for, and asks.
//
// BEST EFFORT, like every other read of the seam: a backend that is slow,
// unreachable or unconfigured yields an empty result and a sentence saying
// so, never a failed turn.
type searchKnowledge struct {
	search KnowledgeSearcher

	// org answers which company is in scope when there is no turn to ask.
	//
	// A SEAT'S SEARCH IS ITS SEAT'S, and the turn carries both halves. An
	// OPERATOR has no turn and no seat — they hold the company's own
	// credential and read everything — so the org comes from the wiring
	// and the seat is nil, which both searchers already read as "the
	// company's own account rather than somebody's". Nil here means a
	// caller that must bring a turn, which is every seat registry.
	org func() *org.Organization
}

var _ tools.SeatCallable = (*searchKnowledge)(nil)

func (t *searchKnowledge) Name() string { return SearchKnowledgeTool }

func (t *searchKnowledge) Description() string {
	return "Search your team's knowledge base — the shared docs, runbooks " +
		"and conventions the company has written down. Returns ranked " +
		"page titles with a one-line snippet each, so you can decide what " +
		"to open; read a page in full with your knowledge-base MCP tools. " +
		"Your prompt already carries what a search on the trigger found at " +
		"turn start, so use this once you know what the task actually " +
		"needs — above all when the trigger was a bare pointer (a webhook " +
		"naming an item, a thread reply) and that block came back empty. " +
		"For a procedure you distilled from your own turns, use `use_skill` " +
		"instead: those are yours, not the team's."
}

func (t *searchKnowledge) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type": "string",
				"description": "2-8 keywords or key phrases describing what you " +
					"are looking for. Keep identifiers verbatim — error codes, " +
					"ticket keys, service and function names. Not a question, " +
					"and not the whole task.",
			},
		},
		"required": []any{"query"},
	}
}

// Call without a turn cannot search: the scope and the credential are the
// seat's. Reported as a failed result rather than an error, for the same
// reason lookup_colleague does — the model asked for something reasonable in
// a context that cannot serve it.
func (t *searchKnowledge) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *searchKnowledge) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any,
) (tools.Result, error) {
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return failed("search_knowledge needs a `query`: a few keywords describing " +
			"what you are looking for."), nil
	}
	// NAMES THE FIELD AND THE BOUND, and says what to send instead — see
	// [searchQueryMax] for why this is a refusal rather than a cut. The
	// bound is bytes because that is the unit a backend's own limit is
	// measured in, and the refusal reports bytes so the two agree.
	if len(query) > searchQueryMax {
		return failed(fmt.Sprintf("That `query` is %d bytes and search_knowledge "+
			"takes at most %d. Send 2-8 keywords or key phrases — identifiers, "+
			"error codes, service names — rather than the task or the thread: "+
			"a ranker scores terms, and prose this long matches everything "+
			"weakly and nothing well.", len(query), searchQueryMax)), nil
	}
	// THE TURN'S ORG, or the wiring's where there is no turn — see
	// [searchKnowledge.org]. Reading the turn unconditionally made this
	// tool refuse every call on the operator surface, where it is
	// registered and where there is never a turn to read.
	var seat *org.Role
	var company *org.Organization
	switch {
	case turn != nil && turn.Org != nil:
		seat, company = turn.Seat, turn.Org
	case t.org != nil:
		company = t.org()
	}
	if company == nil {
		return failed("No organization is in scope, so there is no knowledge base to search."), nil
	}
	// THE CHEAP GATE FIRST, exactly as the turn-start prefetch does it:
	// CanSearch does no I/O, and a seat whose search could not hit anything
	// is told so instead of waiting on a round trip that was always going
	// to be empty.
	if !t.search.CanSearch(seat, company) {
		return tools.Result{Output: "Your team's knowledge base is not searchable from " +
			"this seat — either no knowledge backend is configured, or this seat has " +
			"no read scope and no credential of its own. Ask a colleague, or work " +
			"from what you have."}, nil
	}

	hits := t.search.Search(ctx, knowledge.Query{
		Text: query, Seat: seat, Org: company, Limit: searchHits,
		// AUTO-DRAFTS HIDDEN, the same exclusion the turn-start prefetch
		// applies. Those pages are unreviewed proposals a synthesis pass
		// wrote; an agent cannot tell one from a ratified runbook, and
		// following an unratified one is how a draft becomes policy
		// without anybody agreeing to it.
		ExcludeAncestors: []string{knowledge.AutoDraftedParent},
	})
	if len(hits) == 0 {
		return tools.Result{Output: fmt.Sprintf(
			"No team documents match %q. Try different keywords, or work from what "+
				"you have — not everything is written down.", clip(query))}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d team documents match %q:\n", len(hits), clip(query))
	for _, hit := range hits {
		if line := renderHit(hit); line != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	// THE POINTER IS THE POINT: these are titles and snippets, not the
	// pages. A seat that acted on a snippet would be acting on the first
	// two hundred characters of a runbook.
	b.WriteString("\nTo read any of these in full, look it up by title with your " +
		"knowledge-base tools.")
	return tools.Result{Output: b.String()}, nil
}

// renderHit renders one page as a bullet.
//
// Falls back to the title alone where a page has no snippet — an empty page,
// or one whose body is a table the extractor could not read — because the
// title is still enough to decide whether to open it.
func renderHit(hit knowledge.Hit) string {
	title := strings.TrimSpace(hit.Title)
	snippet := strings.Join(strings.Fields(hit.Snippet), " ")
	switch {
	case title == "" && snippet == "":
		return ""
	case title == "":
		title = "(untitled)"
	}
	if snippet == "" {
		return "- **" + title + "**"
	}
	return "- **" + title + "**: " + snippet
}
