package builtin

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
)

// SearchKnowledgeTool is the tool's wire name.
const SearchKnowledgeTool = "search_knowledge"

// searchHits is how many pages one call renders: six, as the turn-start
// prefetch block does, since a dozen titles and snippets crowd out the task.
// Not [knowledge.DefaultLimit], the seam's own default for a backend.
const searchHits = 6

// KnowledgeSearcher is the two methods of [knowledge.Searcher] this tool
// uses, declared by the consumer so a stub can stand in for a backend.
type KnowledgeSearcher interface {
	CanSearch(seat *org.Role, o *org.Organization) bool
	Search(ctx context.Context, q knowledge.Query) knowledge.Result
}

// searchKnowledge searches the team knowledge base on demand: a trigger that
// is a bare pointer ("PR #42 got a comment") is unsearchable at turn start,
// and the agent that has done the recon knows what to ask for.
//
// Best effort, like every read of the seam: a slow, unreachable or
// unconfigured backend yields an empty result and a sentence saying so, never
// a failed turn.
type searchKnowledge struct {
	search KnowledgeSearcher

	// granted reads whether the engine's grant to a seat on a lane is in
	// place ([Deps.GrantHeld]); nil knows nothing.
	granted func(ctx context.Context, handle, lane string) (held, known bool)

	// org answers which company is in scope when there is no turn: an
	// operator has no turn and no seat, so the seat is nil, which both
	// searchers read as the company's own account. A seat's search takes
	// both from its turn. Nil means every caller brings a turn (every seat
	// registry).
	org func() *org.Organization

	// events receives the `knowledge_read` a seat's search records; nil (a
	// registry built outside an engine) records nothing.
	events Telemetry
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
		"For a procedure you distilled from your own turns, use `" + UseSkillTool + "` " +
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
			"mode": map[string]any{
				"type": "string",
				"enum": []any{string(knowledge.ModeHybrid), string(knowledge.ModeKeyword),
					string(knowledge.ModeSemantic)},
				"description": "How to rank. `hybrid` (the default) ranks by the " +
					"words and by meaning together. `keyword` ranks by the words " +
					"alone — for an exact identifier, error code or name. " +
					"`semantic` ranks by meaning alone — to find a page that says " +
					"the same thing in other words.",
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
		return failed(SearchKnowledgeTool + " needs a `query`: a few keywords describing " +
			"what you are looking for."), nil
	}
	if knowledge.CheckQuery(query) != nil {
		// Refused, not cut, at [knowledge.MaxQueryBytes]: a search on a
		// truncated query is a different search, and the model would read its
		// results as the answer to the one it asked.
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return failed(fmt.Sprintf(SearchKnowledgeTool+"'s `query` is %d bytes and takes at "+
			"most %d — send 2-8 keywords or key phrases, identifiers verbatim, not the "+
			"task or a pasted thread.", len(query), knowledge.MaxQueryBytes)), nil
	}
	// An unknown mode is refused, not defaulted: a fallback answers a
	// different question from the one asked.
	mode, err := knowledge.ParseMode(argString(args, "mode"))
	if err != nil {
		return refusedBy(tools.RefusalInvalid, err, unknownMode(SearchKnowledgeTool,
			argString(args, "mode"))), nil
	}
	// The turn's org, or the wiring's on the operator surface, which has no
	// turn ([searchKnowledge.org]).
	var seat *org.Role
	var company *org.Organization
	switch {
	case turn != nil && turn.Org != nil:
		seat, company = turn.Seat, turn.Org
	case t.org != nil:
		company = t.org()
	}
	if company == nil {
		return refusedBy(tools.RefusalUnavailable, waits(),
			"No organization is in scope, so there is no knowledge base to search."), nil
	}
	// The cheap gate first, as the turn-start prefetch does: CanSearch does
	// no I/O, so a seat that could hit nothing is told so without a round trip.
	if !t.search.CanSearch(seat, company) {
		return tools.Result{Output: "Your team's knowledge base is not searchable from " +
			"this seat — either no knowledge backend is configured, or this seat has " +
			"no read scope and no credential of its own. Ask a colleague, or work " +
			"from what you have."}, nil
	}

	result := t.search.Search(ctx, knowledge.Query{
		Text: query, Seat: seat, Org: company, Limit: searchHits, Mode: mode,
		// Auto-drafts hidden, as the turn-start prefetch does: an agent
		// cannot tell an unreviewed synthesis draft from a ratified runbook,
		// and following one makes a draft policy nobody agreed to.
		ExcludeAncestors: []string{knowledge.AutoDraftedParent},
	})
	if result.VendorStatus != 0 {
		return t.vendorRefused(ctx, turn, result), nil
	}
	hits := result.Hits
	degraded := degradedNote(mode, result.Outcome)
	if len(hits) == 0 && result.ServedMode == "" {
		// Nothing ran, which is not "nothing matched": a seat told nothing
		// is written down writes a duplicate of a page that exists. A ranking
		// the backend cannot serve says which, so the seat asks another way.
		if degraded != "" {
			return tools.Result{Output: "Nothing was searched, so this says " +
				"nothing about whether a page exists." + degraded}, nil
		}
		return tools.Result{Output: "The knowledge base could not be searched just " +
			"now, so this says nothing about whether a page exists. Try again " +
			"shortly, or work from what you have."}, nil
	}
	partial := partialNote(result.Coverage) + degraded
	// The echoed query is the seat's own words, only neutralised, so a query
	// spelling the turn's token or a marker opens no fence beside the hits'.
	fence := turn.FenceBoundary()
	if len(hits) == 0 {
		return tools.Result{Output: fmt.Sprintf(
			"No team documents match %q. Try different keywords, or work from what "+
				"you have — not everything is written down.%s",
			provenance.Neutralise(fence, clip(query)), partial)}, nil
	}
	// Every hit shown, in order, recorded `via: search`: which pages searches
	// surface differs from which get opened, and a knowledge base is curated
	// against both.
	note(ctx, t.events, turn, knowledgeRead(turn, types.ReadViaSearch,
		hits[0].Backend, query, knowledge.ReadPages(hits)))

	// Fenced under the turn's token and the backend's label
	// ([knowledge.Render], ADR-0040), as the turn-start block renders them.
	var b strings.Builder
	fmt.Fprintf(&b, "%d team documents match %q:\n", len(hits),
		provenance.Neutralise(fence, clip(query)))
	if listed := knowledge.Render(fence, hits); listed != "" {
		b.WriteString(listed)
		b.WriteString("\n")
	}
	// The pointer matters: a seat acting on a snippet acts on the first two
	// hundred characters of a runbook.
	b.WriteString("\nTo read any of these in full, look it up by title with your " +
		"knowledge-base tools.")
	b.WriteString(partial)
	return tools.Result{Output: b.String()}, nil
}

// unknownMode is the refusal of an unknown `mode`, naming the known ones:
// composed here because the parser's error opens with "knowledge: ", which
// reads oddly after the tool's own name.
func unknownMode(tool, asked string) string {
	names := make([]string, len(knowledge.Modes))
	for i, m := range knowledge.Modes {
		names[i] = string(m)
	}
	return fmt.Sprintf("%s does not search in mode %q. It takes `mode` as one of %s.",
		tool, strings.TrimSpace(asked), words.Or(names))
}

// degradedNote is the sentence a search that ranked other than asked owes the
// seat, or nothing when it served the mode asked for. Said to the model for
// [partialNote]'s reason: a keyword answer read as a hybrid one has a seat
// conclude no page means what it typed. The rule is internal/knowledge's mode
// doc: a search never serves another ranking without saying so.
func degradedNote(asked knowledge.Mode, o knowledge.Outcome) string {
	switch o.Degraded {
	case knowledge.NotDegraded:
		return ""
	case knowledge.DegradedSemanticPartial:
		return "\n\nMeaning was ranked over only part of the knowledge base — " +
			"some of the fleet could not compare by meaning — so a page that " +
			"says this in other words may be missing here."
	}
	why := "this knowledge base has no ranking by meaning"
	switch o.Degraded {
	case knowledge.DegradedNoEmbeddings:
		why = "this company has no embeddings provider, so nothing ranks by meaning"
	case knowledge.DegradedEmbeddingFailed:
		why = "the query's meaning could not be computed just now; asking again may " +
			"rank by it"
	}
	if asked.Resolved() == knowledge.ModeSemantic {
		return "\n\nNo search by meaning ran: " + why + ". Search with mode " +
			"`keyword` to rank by the words instead."
	}
	return "\n\nRanked by the words alone: " + why + "."
}

// partialNote is the sentence a search over part of the knowledge base owes
// the seat, or nothing when it covered all of it. Said to the model, not only
// logged: a short list over part of the corpus reads like one over all of it,
// and a seat that concludes nothing is written down writes it again.
func partialNote(c knowledge.Coverage) string {
	if c.Complete || c.BucketsMissing == 0 {
		return ""
	}
	return "\n\nThis search covered only part of the knowledge base — some of the " +
		"fleet did not answer in time — so a page not listed here may still exist."
}

// vendorRefused is a vendor's refusal of the searching credential, said only
// from what the engine knows: the company's connection's refusal is the
// company's; on the agent's own credential a 401 is that credential, a 403 its
// grant does not hold is vendor_forbidden, and any other 403 is forbidden.
func (t *searchKnowledge) vendorRefused(ctx context.Context, turn *turnctx.Turn, result knowledge.Result) tools.Result {
	filing, _ := FilingOf(SearchKnowledgeTool)
	need := filing.Vendor
	// A revoked credential waits on an administrator, never on time; a 403 is forbidden.
	refusal := func(class tools.Refusal, cause error, words string) tools.Result {
		return refusedBy(class, cause, SearchKnowledgeTool+": "+need.Vendor+" "+words+
			" Nothing was searched, so this says nothing about whether a page exists.")
	}
	switch {
	case !result.VendorSelf && result.VendorStatus == http.StatusUnauthorized:
		return refusal(tools.RefusalUnavailable, waitless(), "refused the company's connection itself: it "+
			"is expired or revoked, and an administrator renews it in the "+need.Vendor+" integration.")
	case !result.VendorSelf:
		return refusal(tools.RefusalForbidden, nil, "refused the company's connection for this search: "+
			"the spaces its account may read may not include these.")
	case result.VendorStatus == http.StatusUnauthorized:
		return refusal(tools.RefusalUnavailable, waitless(), "refused this agent's credential itself: it "+
			"is expired or revoked, and an administrator renews it in the agent's "+need.Vendor+" access.")
	}
	held, known := false, false
	if seat := seatOf(turn); seat != nil && t.granted != nil {
		held, known = t.granted(ctx, seat.Handle(), need.Lane)
	}
	switch {
	case known && !held:
		return refusedBy(tools.RefusalVendorForbidden, nil, need.Refusal(SearchKnowledgeTool))
	case held:
		return refusal(tools.RefusalForbidden, nil, "refused the search although this agent's grant "+
			"includes reading it; a space's own permissions may withhold it.")
	}
	return refusal(tools.RefusalForbidden, nil, "refused this agent's credential, and whether its grant "+
		"is in place could not be read.")
}

// seatOf is the turn's seat, nil outside a turn.
func seatOf(turn *turnctx.Turn) *org.Role {
	if turn == nil {
		return nil
	}
	return turn.Seat
}
