package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
)

// SearchKnowledgeTool is the tool's wire name.
const SearchKnowledgeTool = "search_knowledge"

// SearchKnowledgeHits is how many pages one call renders.
//
// THE TURN-START BLOCK'S OWN COUNT, named rather than copied: each result is a
// pointer the agent uses to decide what to open, the same as a bullet in that
// block, and a dozen crowds out the task they were fetched for. Distinct from
// [knowledge.DefaultLimit], which is what the seam asks a backend for when
// nobody says.
//
// EXPORTED so internal/engine's ceiling test holds it under what a native
// search can answer, beside every other caller's ask.
const SearchKnowledgeHits = prefetch.KnowledgeHits

// KnowledgeSearcher is query-time search over the team knowledge base, as
// this tool needs it.
//
// Declared here rather than taking the seam's own interface, because the gate
// has to say MORE than the seam's does: [knowledge.Searcher.CanSearch] is a
// backend's own yes or no, and a node that is not serving the company's
// knowledge base at all has no backend to ask. So the engine's adapter answers
// which state a search that cannot run is in, and this tool names it.
//
// BUILDING IS REQUIRED, not asked for through an optional interface, so the
// compiler holds every adapter between the engine's searcher and this tool to
// forward it. An optional method is one an adapter drops with nothing failing,
// and a seat behind such an adapter on a node still indexing is told the
// company has written nothing down.
type KnowledgeSearcher interface {
	// CanSearch is the cheap, no-I/O gate. The zero refusal is a search
	// that can run.
	CanSearch(seat *org.Role, o *org.Organization) knowledge.Refusal
	Building(ctx context.Context) bool
	Search(ctx context.Context, q knowledge.Query) knowledge.Answer
}

// partialKnowledgeNote closes an answer found while this node's index is still
// on its first build.
//
// A NON-EMPTY ANSWER IS NOT A WHOLE ONE THEN. Where the fleet divides a
// search across its nodes, a node still building counts its own share of the
// buckets missing and drops any page a peer ranked that its own index has no
// row for yet — so what comes back is real and incomplete, and a seat reading
// it as complete concludes a page it did not see does not exist. The
// turn-start block says the same state in [prefetch.BuildingKnowledgeHint];
// this is its sentence for an answer that did find something.
const partialKnowledgeNote = "(this node is still indexing the knowledge " +
	"base, so this answer may be missing pages that exist — before " +
	"concluding a page does not exist, search again shortly or ask a " +
	"colleague who would know)"

// unsearchable is the answer to a search that cannot run: the one state it is
// in, in the refuser's own words, and what to do instead.
func unsearchable(r knowledge.Refusal) string {
	return "The team's knowledge base is not searchable here: " + r.Reason() +
		". Ask a colleague, or work from what you have."
}

// searchKnowledge searches the team knowledge base on demand.
//
// It serves the trigger the turn-start block cannot: a bare POINTER ("PR #42
// got a comment") is unsearchable at turn start, so that block skips its
// search, and the agent that has since done the recon knows what to search
// for, and asks.
//
// BEST EFFORT, like every other read of the seam: a backend that is slow,
// unreachable or unconfigured yields a sentence saying so, never a failed
// turn.
type searchKnowledge struct {
	search KnowledgeSearcher

	// org answers which company is in scope when there is no turn to ask.
	//
	// A SEAT'S SEARCH IS ITS SEAT'S, and the turn carries both halves. An
	// OPERATOR has no turn and no seat, so the org comes from the wiring
	// and the seat is nil, which each backend reads as the company's own
	// account rather than somebody's: every page natively, and on
	// Confluence the org credential, which searches only a declared
	// `knowledge.scope`. Nil here means a caller that must bring a turn,
	// which is every seat registry.
	org func() *org.Organization
}

var _ tools.SeatCallable = (*searchKnowledge)(nil)

func (t *searchKnowledge) Name() string { return SearchKnowledgeTool }

// Description names BOTH readers, each by what it is. The native knowledge
// base's is this engine's own `get_page`, which takes the page id a hit
// renders; a vendor wiki's is that vendor's server tool, which this engine does
// not name, because a prompt describes a capability and the model picks the
// tool (docs/concepts/tool-capabilities.md). One description for both, because
// one tool serves whichever backend answers the search.
func (t *searchKnowledge) Description() string {
	return "Search your team's knowledge base — the shared docs, runbooks " +
		"and conventions the company has written down. Returns the best " +
		"matches, most relevant first: each page's title, its container, " +
		"its page id and a one-line snippet, so you can decide what to " +
		"open. To read one in full, pass its page id to `get_page` — or, " +
		"where your company's knowledge base is a vendor wiki such as " +
		"Confluence, to that wiki's own page-read tool. Your prompt already " +
		"carries what a search on the trigger found at turn start, so use " +
		"this once you know what the task actually needs — above all when " +
		"the trigger was a bare pointer (a webhook naming an item, a thread " +
		"reply) and that block came back empty. For a procedure you " +
		"distilled from your own turns, use `use_skill` instead: those are " +
		"yours, not the team's."
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
	// [knowledge.MaxQueryBytes] for why this is a refusal rather than a cut.
	if len(query) > knowledge.MaxQueryBytes {
		return failed(fmt.Sprintf("That `query` is %d bytes and search_knowledge "+
			"takes at most %d. Send 2-8 keywords or key phrases — identifiers, "+
			"error codes, service names — rather than the task or the thread: "+
			"a ranker scores terms, and prose this long matches everything "+
			"weakly and nothing well.", len(query), knowledge.MaxQueryBytes)), nil
	}
	// THE TURN'S ORG, or the wiring's where there is no turn — see
	// [searchKnowledge.org]. Reading only the turn would refuse every call
	// on the operator surface, where this tool is registered and where
	// there is never a turn to read.
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
	// CanSearch does no I/O, and a search that could not hit anything is
	// told so instead of waiting on a round trip that was always going to
	// be empty.
	//
	// IT NAMES THE ONE STATE THAT IS TRUE, and never says "this seat": an
	// operator's search has no seat, and each state sends whoever reads it
	// to a different setting.
	if refused := t.search.CanSearch(seat, company); refused.Refused() {
		return tools.Result{Output: unsearchable(refused)}, nil
	}

	answer := t.search.Search(ctx, knowledge.Query{
		Text: query, Seat: seat, Org: company, Limit: SearchKnowledgeHits,
		// AUTO-DRAFTS HIDDEN, the same exclusion the turn-start prefetch
		// applies. Those pages are unreviewed proposals a synthesis pass
		// wrote; an agent cannot tell one from a ratified runbook, and
		// following an unratified one is how a draft becomes policy
		// without anybody agreeing to it.
		ExcludeAncestors: []string{knowledge.AutoDraftedParent},
	})
	// A SEARCH THAT FAILED IS NOT ONE THAT MATCHED NOTHING, and the two
	// send a seat opposite ways: "no match" invites other keywords against
	// a search that is not running, and a page written because none was
	// found. THE TURN-START BLOCK'S OWN SENTENCE, for
	// [prefetch.BuildingKnowledgeHint]'s reason.
	if answer.Failed {
		return failed(prefetch.FailedKnowledgeHint), nil
	}
	var bullets []string
	for _, hit := range answer.Hits {
		if bullet := prefetch.KnowledgeBullet(hit); bullet != "" {
			bullets = append(bullets, bullet)
		}
	}
	// THE TURN-START BLOCK'S OWN SENTENCES for an answer that is missing
	// part of what it searched, or that its backend cut short of its
	// ranking, on an empty answer and a full one alike: "nothing matched"
	// over half the corpus, or over matches an exclusion thinned, is not
	// "nothing matched".
	partial := prefetch.KnowledgeAnswerNote(answer)
	// ASKED AFTER THE SEARCH, WHATEVER IT FOUND. "Not indexed yet" is not
	// "nothing matched", and the two send a seat to opposite places: the
	// no-match answer invites different keywords, which on an index still
	// on its first build find nothing either, and a seat that concludes
	// nothing was written down acts on it by writing a page that already
	// exists. A non-empty answer on such a node is real and incomplete —
	// see [partialKnowledgeNote] — so it carries the caveat too.
	//
	// THE BUILDING SENTENCE OUTRANKS THE PARTIAL ONE on such a node: its
	// own share of a divided search is counted missing too, so both would
	// be true, and the building one names the cause the seat can wait out.
	// A truncated answer's note is kept beside it: that is a fact about the
	// ranking the backend read, which the index's build does not explain.
	building := t.search.Building(ctx)
	if building {
		partial = prefetch.KnowledgeAnswerNote(knowledge.Answer{Truncated: answer.Truncated})
	}
	if len(bullets) == 0 {
		if building {
			// THE TURN-START BLOCK'S OWN SENTENCE — one text for one
			// state, because the seat reads both.
			out := prefetch.BuildingKnowledgeHint
			if partial != "" {
				out += "\n\n" + partial
			}
			return tools.Result{Output: out}, nil
		}
		empty := fmt.Sprintf("No team documents match %q. Try different keywords, "+
			"or work from what you have — not everything is written down.",
			clip(query))
		if partial != "" {
			empty += "\n\n" + partial
		}
		return tools.Result{Output: empty}, nil
	}

	var b strings.Builder
	// THE BEST MATCHES, NOT A COUNT OF WHAT MATCHED. The answer is the top
	// of a ranking cut at [SearchKnowledgeHits], so "six documents match"
	// would tell a seat the company has six pages on the subject when it
	// may have sixty.
	fmt.Fprintf(&b, "Best matches for %q, most relevant first:\n", clip(query))
	for _, bullet := range bullets {
		b.WriteString(bullet)
		b.WriteString("\n")
	}
	if building {
		b.WriteString("\n" + partialKnowledgeNote + "\n")
	}
	if partial != "" {
		b.WriteString("\n" + partial + "\n")
	}
	// THE POINTER IS THE POINT: these are titles and snippets, not the
	// pages. A seat that acted on a snippet would be acting on the first
	// two hundred characters of a runbook.
	b.WriteString("\n" + prefetch.KnowledgeReadHint)
	return tools.Result{Output: b.String()}, nil
}
