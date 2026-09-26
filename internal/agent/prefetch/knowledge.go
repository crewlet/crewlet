package prefetch

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Relevant knowledge: what the company has written down.
//
// # Searched at turn start, through the one seam
//
// The block is a search run on the agent's behalf, so an executor sees the
// runbook without having to think to go and look. What answers it is the
// company's one knowledge backend: natively, this node's own index over the
// pages the fleet's log wrote, which can be behind those rows and says so while
// it is still on its first build; on Confluence, a live query against the
// site, with no local copy at all.
//
// # An auxiliary model writes the query
//
// A model is a far better search-query writer than raw trigger text: it
// distils keywords, keeps identifiers verbatim, and drops the conversational
// filler that makes a full-text search match nothing. One call per turn.

const (
	// AuxTimeout bounds one auxiliary call.
	//
	// The prefetch runs before the executor sees anything, so this is
	// latency a person is waiting through. Thirty seconds is generous for
	// a small fast model and short enough that a hung provider costs one
	// turn's start rather than the turn.
	AuxTimeout = 30 * time.Second

	// auxTemperature keeps these passes reproducible without going
	// greedy. Not zero: each is a judgement, and greedy decoding on a
	// judgement makes a model commit hard to a first token it would
	// otherwise reconsider.
	auxTemperature = 0.2

	// KnowledgeHits is how many pages the block renders.
	//
	// Six, against a block that is a pointer rather than the content: each
	// bullet is a title, where the page is and a snippet, and the seat opens
	// what looks relevant. A dozen would crowd the task it was fetched for.
	//
	// EXPORTED because two other readers show the same slice and name this
	// value rather than a copy of it: a seat's own `search_knowledge`, and
	// the dashboard's knowledge search, which shows an operator what an
	// agent would be shown.
	KnowledgeHits = 6

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
// still catching up — and what the `search_knowledge` tool answers when a
// search on such a node comes back empty. ONE SENTENCE FOR BOTH, because a seat
// reads both, and two wordings of one state read as two states.
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

// FailedKnowledgeHint is what the block says when its search failed — and what
// the `search_knowledge` tool answers when one of its searches does. ONE
// SENTENCE FOR BOTH, for [BuildingKnowledgeHint]'s reason.
//
// A DIFFERENT SENTENCE from [EmptyKnowledgeHint]: a search that failed found
// nothing because it did not run, so "try other keywords" is advice for the
// wrong problem and "nothing is written down" is a conclusion it cannot
// support.
const FailedKnowledgeHint = "(the knowledge search failed, so it says " +
	"nothing about whether a page exists — search again shortly, or ask a " +
	"colleague who would know)"

// PartialKnowledgeNote is what the block, and the `search_knowledge` tool, say
// about an answer that is missing part of what it searched — or "" for a
// whole one.
//
// ONE SENTENCE FOR BOTH, for [BuildingKnowledgeHint]'s reason. It says what is
// missing rather than only that something is, because the two causes send a
// seat to different places: a slice of the knowledge base that did not answer
// is found by asking again shortly, and a search that matched on words alone
// is helped by the words a page on the subject would use.
func PartialKnowledgeNote(p *knowledge.Partial) string {
	if p == nil {
		return ""
	}
	var missing []string
	if p.BucketsMissing > 0 {
		missing = append(missing, fmt.Sprintf("%d of the knowledge base's %d "+
			"slices were not searched, because the node searching them did not "+
			"answer in time or is still indexing — searching again shortly may "+
			"find pages this answer does not name",
			p.BucketsMissing, p.BucketsAnswered+p.BucketsMissing))
	}
	if p.SemanticSkipped {
		missing = append(missing, "the half of the search that matches by "+
			"meaning did not run, so a page that says the same thing in other "+
			"words may be missing — try the words such a page would use")
	}
	if len(missing) == 0 {
		return ""
	}
	return "(this answer is partial: " + strings.Join(missing, "; and ") +
		". Before concluding a page does not exist, search again or ask a " +
		"colleague who would know)"
}

// TruncatedKnowledge is what the block, the `search_knowledge` tool and the
// dashboard's knowledge search say about an answer its backend cut short of
// its ranking ([knowledge.Answer.Truncated]): fewer hits than it was asked
// for, while the backend ranks more matches than it read.
//
// ONE SENTENCE FOR EVERY READER, for [BuildingKnowledgeHint]'s reason; a seat
// reads it as [TruncatedKnowledgeNote], set apart as the block's other notes
// are. Its OWN sentence rather than a clause of [PartialKnowledgeNote], because
// nothing was left unsearched: the whole knowledge base was ranked, and pages
// the search leaves out took places among the matches it read. A short list
// with nothing beside it reads as everything that matched, which is the
// conclusion this exists to stop; the rest of the ranking is reached by asking
// in more specific words, which ranks the pages that were below it higher.
const TruncatedKnowledge = "this answer has fewer pages than were asked for " +
	"because pages the search leaves out, such as unreviewed drafts, took " +
	"places among the matches it read, and the knowledge base ranks more " +
	"matches than it read. Before concluding a page does not exist, search " +
	"again in more specific words"

// TruncatedKnowledgeNote is [TruncatedKnowledge] as a seat reads it.
const TruncatedKnowledgeNote = "(" + TruncatedKnowledge + ")"

// KnowledgeAnswerNote is every note an answer carries about what its hits do
// not cover — [PartialKnowledgeNote] and [TruncatedKnowledgeNote], each where
// it applies, one per line — or "" for an answer that covers everything that
// matched. The block and the `search_knowledge` tool both render it, so an
// answer is qualified the same way wherever a seat reads it.
func KnowledgeAnswerNote(answer knowledge.Answer) string {
	var notes []string
	if partial := PartialKnowledgeNote(answer.Partial); partial != "" {
		notes = append(notes, partial)
	}
	if answer.Truncated {
		notes = append(notes, TruncatedKnowledgeNote)
	}
	return strings.Join(notes, "\n")
}

// KnowledgeReadHint closes every rendering of hits, the block's and the
// `search_knowledge` tool's alike: these are pointers, and here is how to
// follow one.
//
// THE CAPABILITY, NOT A TOOL NAME. The reader is the engine's own page tool on
// the native backend and a vendor server's tool on Confluence, and each
// bullet carries the page id either one takes — so the sentence names what to
// pass rather than which tool to pass it to.
const KnowledgeReadHint = "To read one in full, open it by its page id with " +
	"your knowledge base's page-read tool."

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
	// AN INDEX STILL ON ITS FIRST BUILD can miss any page, and on a single
	// node it answers every search empty. Said out loud rather than searched
	// anyway, and BEFORE the query call: the auxiliary model round trip
	// would be spent producing a query whose answer the block could not
	// vouch for, which is the same waste CanSearch's gate exists to avoid.
	if f.src.Knowledge.Building(ctx) {
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
	if len(query) > knowledge.MaxQueryBytes {
		// REFUSED, NOT CUT, on the seam's rule — see
		// [knowledge.MaxQueryBytes]. The model was asked for one line of
		// keywords and wrote prose, and a search on its first four hundred
		// bytes would put pages about half of it in front of the seat as
		// what the company knows. The block says to search again instead,
		// which the seat does with words of its own.
		log.WarnContext(ctx, "prefetch_knowledge_query_refused",
			"seat", r.Seat.Handle(), "bytes", len(query),
			"limit", knowledge.MaxQueryBytes)
		return EmptyKnowledgeHint, 0
	}
	answer := f.src.Knowledge.Search(ctx, knowledge.Query{
		Text: query, Seat: r.Seat, Org: r.Org, Limit: KnowledgeHits,
		// AUTO-DRAFTS HIDDEN. Those pages are unreviewed proposals a
		// synthesis pass wrote; an executor cannot tell one from a
		// ratified runbook, and following an unratified one is how a
		// draft becomes policy without anybody agreeing to it.
		ExcludeAncestors: []string{knowledge.AutoDraftedParent},
		// THE TURN'S OWN SEARCH, said so. It changes nothing about the
		// answer; it files the search's duration apart from somebody's
		// deliberate search, because the two are held to different
		// latency figures and a series holding both describes neither
		// (see [knowledge.Query.Prefetch]).
		Prefetch: true,
	})
	if answer.Failed {
		return FailedKnowledgeHint, 0
	}
	bullets := make([]string, 0, len(answer.Hits))
	for _, hit := range answer.Hits {
		if bullet := KnowledgeBullet(hit); bullet != "" {
			bullets = append(bullets, bullet)
		}
	}
	// A PARTIAL OR TRUNCATED ANSWER SAYS SO, found or not: "nothing surfaced"
	// over half the knowledge base, or over the matches a draft exclusion
	// thinned, is not "nothing surfaced".
	partial := KnowledgeAnswerNote(answer)
	if len(bullets) == 0 {
		if partial != "" {
			return EmptyKnowledgeHint + "\n" + partial, 0
		}
		return EmptyKnowledgeHint, 0
	}
	// THE POINTER IS THE POINT: these are titles and snippets, not the
	// pages. A seat that acted on a snippet would be acting on the first
	// two hundred characters of a runbook.
	//
	// THE COUNT IS OF WHAT RENDERED, not of what came back: a hit with
	// nothing to show and nothing to open is dropped above, and counting it
	// would report a page the block never surfaced.
	block := joinBullets(bullets) + "\n"
	if partial != "" {
		block += partial + "\n"
	}
	return block + KnowledgeReadHint, len(bullets)
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

// KnowledgeBullet renders one hit as a bullet: its title, where the page is,
// and its snippet.
//
// ONE RENDERER for the block and the `search_knowledge` tool, so a seat reads
// one shape for one thing wherever it meets it.
//
// WHERE THE PAGE IS travels with it — its container and its page id — because
// a pointer is only as good as what it takes to follow it: the page-read tool
// on either backend takes the id, and a bullet without it costs the seat a
// listing round to find what it was just shown.
//
// Falls back to what there is. A page with no snippet — an empty page, or one
// whose body is a table the extractor could not read — keeps its title and
// its id, which are still enough to decide whether to open it; a hit with
// nothing to show and nothing to open renders as nothing.
func KnowledgeBullet(hit knowledge.Hit) string {
	title := strings.TrimSpace(hit.Title)
	snippet := collapse(hit.Snippet)
	where := pageLocation(hit)
	if title == "" && snippet == "" && where == "" {
		return ""
	}
	if title == "" {
		title = "(untitled)"
	}
	bullet := "- **" + title + "**"
	if where != "" {
		bullet += " (" + where + ")"
	}
	if snippet != "" {
		bullet += ": " + snippet
	}
	return bullet
}

// pageLocation names a hit's container and page id, whichever it carries.
func pageLocation(hit knowledge.Hit) string {
	container := strings.TrimSpace(hit.Container)
	id := strings.TrimSpace(hit.PageID)
	switch {
	case container != "" && id != "":
		return container + ", page id " + id
	case id != "":
		return "page id " + id
	default:
		return container
	}
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
