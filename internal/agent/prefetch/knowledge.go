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

// BuildingKnowledgeHint is what the block says while this node's own index is
// still catching up.
//
// A DIFFERENT SENTENCE from [EmptyKnowledgeHint], and the difference is what
// it tells the seat to do. "Nothing surfaced" invites a focused re-search,
// which on a building index will also find nothing; this says the search
// itself cannot answer yet, so the seat should ask a colleague rather than
// conclude from an empty result that the company has written nothing down —
// which on a freshly joined node is true for the whole first index build and
// false about the company.
const BuildingKnowledgeHint = "(the knowledge base is not searchable from " +
	"this node yet — it is still indexing. Pages that exist will not be " +
	"found by a search right now, so ask a colleague who would know rather " +
	"than concluding nothing has been written down)"

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

// relevantKnowledge renders the block, and reports how many pages it put in
// it.
//
// The COUNT is not derivable from the block: an empty search renders
// EmptyKnowledgeHint, which is non-empty prose. Without the count, telemetry
// cannot tell a search that ran and found nothing from one that surfaced six
// runbooks — the exact distinction an operator checking whether the knowledge
// backend is wired up needs.
func (f *Fetcher) relevantKnowledge(ctx context.Context, r Request) (string, int) {
	if f.src.Knowledge == nil || strings.TrimSpace(r.Task) == "" {
		return "", 0
	}
	// THE CHEAP GATE FIRST. CanSearch does no I/O, and its whole job is to
	// let the query call be skipped when the search is a guaranteed no-op
	// — a gate that had to reach the network would cost more than the call
	// it saves.
	if !f.src.Knowledge.CanSearch(r.Seat, r.Org) {
		return "", 0
	}
	// A BACKEND THAT KEEPS AN INDEX can be behind its own projection, and
	// during that window every search answers empty. Said out loud rather
	// than searched anyway, and BEFORE the query call: the auxiliary model
	// round trip would be spent producing a query nothing can match, which
	// is the same waste CanSearch's gate exists to avoid.
	//
	// An optional interface rather than a method on [knowledge.Searcher]:
	// a live vendor search has no index and nothing to report, so
	// requiring it of every backend would be implementations of "false".
	if builder, ok := f.src.Knowledge.(interface {
		Building(ctx context.Context) bool
	}); ok && builder.Building(ctx) {
		return BuildingKnowledgeHint, 0
	}
	if r.RequiresRecon {
		// The trigger is a pointer, so there is nothing worth searching
		// on yet: a query built from "PR #42 got a comment" matches the
		// wrong pages or none. The hint says to look again once the seat
		// knows what the task needs, which is exactly what the executor's
		// search_knowledge tool is for.
		return EmptyKnowledgeHint, 0
	}
	query := f.knowledgeQuery(ctx, r)
	if query == "" {
		return "", 0
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
		return EmptyKnowledgeHint, 0
	}
	bullets := make([]string, 0, len(hits)+1)
	for _, hit := range hits {
		bullets = append(bullets, renderHit(hit))
	}
	rendered := joinBullets(bullets)
	if rendered == "" {
		return EmptyKnowledgeHint, 0
	}
	// THE POINTER IS THE POINT: these are titles and snippets, not the
	// pages. A seat that acted on a snippet would be acting on the first
	// two hundred characters of a runbook.
	return rendered + "\nTo read any of these in full, look it up by title " +
		"with your knowledge-base tools.", len(hits)
}

// knowledgeQuery asks the auxiliary model for a search query.
func (f *Fetcher) knowledgeQuery(ctx context.Context, r Request) string {
	answer, ok := f.auxCall(ctx, r.Seat, knowledgeQuerySystemPrompt,
		"Task the agent is about to work on:\n\""+r.Task+
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
