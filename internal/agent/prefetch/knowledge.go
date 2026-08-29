package prefetch

import (
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Relevant knowledge: what the company has written down.
//
// # Searched live, never indexed
//
// There is no local copy and no embedding step. The knowledge base is
// queried at turn time, so the block always reflects what the page says
// now — which is the whole reason procedural knowledge lives in a wiki
// rather than in the engine. The engine's contribution is searching it on
// the agent's behalf, so a planner sees the runbook without having to think
// to go and look.
//
// # An auxiliary model writes the query
//
// A model is a far better search-query writer than raw trigger text: it
// distils keywords, keeps identifiers verbatim, and drops the conversational
// filler that makes a full-text search match nothing. One call per turn.

const (
	// AuxTimeout bounds one auxiliary call.
	//
	// The prefetch runs before the planner sees anything, so this is
	// latency a person is waiting through. Thirty seconds is generous for
	// a small fast model and short enough that a hung provider costs one
	// turn's start rather than the turn.
	AuxTimeout = 30 * time.Second

	// auxTemperature keeps these passes reproducible without going
	// greedy. Not zero: each is a judgement, and greedy decoding on a
	// judgement makes a model commit hard to a first token it would
	// otherwise reconsider.
	auxTemperature = 0.2

	// KnowledgeCharBudget caps the rendered block.
	KnowledgeCharBudget = 1500

	// knowledgeHits is how many pages are rendered.
	//
	// Six, against a block that is a pointer rather than the content: each
	// bullet is a title and a snippet, and the seat opens what looks
	// relevant. A dozen would crowd the task it was fetched for.
	knowledgeHits = 6

	// knowledgeQueryTokens is headroom for the query call. The visible
	// answer is one short line; the cap covers a thinking model's
	// reasoning, for the same reason the memory filter's does.
	knowledgeQueryTokens = 1000
)

// EmptyKnowledgeHint is what the block says when a search was skipped or
// found nothing.
//
// Framed as the expected next step rather than an apology: re-searching once
// the seat knows what the task actually needs is the normal pattern, not an
// escape hatch.
const EmptyKnowledgeHint = "(no team documents surfaced at turn start — " +
	"search your team knowledge base with a focused query once you have " +
	"gathered more context about what the task actually needs)"

// knowledgeQuerySystemPrompt turns a task into a search query.
const knowledgeQuerySystemPrompt = `You turn an AI agent's current task into a search query for its team's knowledge base.

Output format (strict):
- A single line of 2-8 search keywords or key phrases.
- No prose, no explanation, no quotes, no JSON — just the query text.
- Preserve exact identifiers verbatim: error codes, ticket keys, API, function and service names.
- Focus on the procedural or reference material the agent would look up — runbook topics, conventions, domain terms, methodologies.
- Drop conversational filler, and drop people's names unless a person is themselves the subject of the lookup.

Examples:
- Task: "investigate the latency spike on checkout after the Tuesday deploy" -> latency spike checkout deployment rollback incident response
- Task: "review the change adding the rate limiter" -> code review checklist rate limiting conventions`

// relevantKnowledge renders the block.
func (f *Fetcher) relevantKnowledge(ctx context.Context, r Request) string {
	if f.src.Knowledge == nil || strings.TrimSpace(r.Task) == "" {
		return ""
	}
	// THE CHEAP GATE FIRST. CanSearch does no I/O, and its whole job is to
	// let the query call be skipped when the search is a guaranteed no-op
	// — a gate that had to reach the network would cost more than the call
	// it saves.
	if !f.src.Knowledge.CanSearch(r.Seat, r.Org) {
		return ""
	}
	if r.RequiresRecon {
		// The trigger is a pointer, so there is nothing worth searching
		// on yet: a query built from "PR #42 got a comment" matches the
		// wrong pages or none. The hint says to look again after recon,
		// which is exactly when the trigger becomes searchable.
		return EmptyKnowledgeHint
	}
	query := f.knowledgeQuery(ctx, r)
	if query == "" {
		return ""
	}
	hits := f.src.Knowledge.Search(ctx, knowledge.Query{
		Text: query, Seat: r.Seat, Org: r.Org, Limit: knowledgeHits,
		// AUTO-DRAFTS HIDDEN. Those pages are unreviewed proposals a
		// synthesis pass wrote; a planner cannot tell one from a
		// ratified runbook, and following an unratified one is how a
		// draft becomes policy without anybody agreeing to it.
		ExcludeAncestors: []string{knowledge.AutoDraftedParent},
	})
	if len(hits) == 0 {
		return EmptyKnowledgeHint
	}
	bullets := make([]string, 0, len(hits)+1)
	for _, hit := range hits {
		bullets = append(bullets, renderHit(hit))
	}
	rendered := budget(bullets, KnowledgeCharBudget)
	if rendered == "" {
		return EmptyKnowledgeHint
	}
	// THE POINTER IS THE POINT: these are titles and snippets, not the
	// pages. A seat that acted on a snippet would be acting on the first
	// two hundred characters of a runbook.
	return rendered + "\nTo read any of these in full, look it up by title " +
		"with your knowledge-base tools."
}

// AfterPlan recovers the knowledge block a thin trigger's gate skipped.
//
// The Plan-time search is gated off when the trigger is a bare pointer,
// because searching on "PR #42 got a comment" is noise. But once Plan has
// done its recon the PLAN SUMMARY is a real, task-shaped query — so this
// runs the search the gate skipped and hands the result to Execute.
//
// Keyed on the summary ALONE, not on the summary plus the original task:
// on a thin trigger the task is the boilerplate the gate rejected, and
// including it would dilute the one good query this turn has.
//
// It is the ONE fetch here that cannot be frozen, and the reason is
// structural rather than a preference: its input does not exist until Plan
// has run. It happens exactly once, between the phases, so the Execute
// prompt is still fixed for the whole of Execute — including a suspend and
// resume, where the saved conversation carries it.
//
// EMPTY unless the trigger actually required recon. Otherwise the Plan-time
// prefetch already ran against a real trigger and Execute has nothing
// missing to recover; running anyway would spend a model call and a search
// to produce the block the planner already read.
func (f *Fetcher) AfterPlan(ctx context.Context, r Request, planSummary string) string {
	if f == nil || !r.RequiresRecon || f.src.Knowledge == nil {
		return ""
	}
	summary := strings.TrimSpace(planSummary)
	if summary == "" {
		return ""
	}
	// The recon flag is what gated the Plan-time search; clearing it here
	// is what lets the SAME renderer run, against the plan summary as the
	// task. One implementation, so the two blocks cannot come to disagree
	// about how a page is rendered or which drafts are hidden.
	recovered := r
	recovered.RequiresRecon = false
	recovered.Task = summary

	block := f.relevantKnowledge(ctx, recovered)
	if block == EmptyKnowledgeHint {
		// The hint is the PLAN prompt's answer — "look again after
		// recon". Repeating it in Execute tells a seat that has just
		// done the recon to go and do it again.
		return ""
	}
	return block
}

// knowledgeQuery asks the auxiliary model for a search query.
func (f *Fetcher) knowledgeQuery(ctx context.Context, r Request) string {
	answer, ok := f.auxCall(ctx, r.Seat, knowledgeQuerySystemPrompt,
		"Task the agent is about to plan:\n\""+truncate(r.Task, 1200)+
			"\"\n\nKnowledge-base search query:", knowledgeQueryTokens)
	if !ok {
		return ""
	}
	// THE FIRST REAL LINE, unquoted. A chatty model prefixes an
	// explanation or wraps the query in quotes, and both would be searched
	// verbatim — a quoted query matches nothing, and a query with a
	// sentence in front of it matches the sentence.
	for line := range strings.SplitSeq(answer, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return strings.Trim(trimmed, `"'`)
		}
	}
	return ""
}

// renderHit renders one page as a bullet.
//
// Falls back to the title alone where a page has no snippet — an empty page,
// or one whose body is a table the extractor could not read — because the
// title is still enough to decide whether to open it.
func renderHit(hit knowledge.Hit) string {
	title := strings.TrimSpace(hit.Title)
	snippet := collapse(hit.Snippet)
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

// auxRequest is the shape every auxiliary pass here sends.
func auxRequest(system, user string, maxTokens int) llm.Request {
	return llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: user},
		},
		// NO TOOLS. Each of these passes wants text back, and a tool on
		// the surface invites a model to call it and answer nothing —
		// there is no tool any of them could usefully use.
		Temperature: llm.Temp(auxTemperature),
		MaxTokens:   maxTokens,
	}
}
