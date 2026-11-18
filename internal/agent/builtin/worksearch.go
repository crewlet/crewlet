package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Finding a work item by what it SAYS, descriptions included, which the
// board's `text` (a substring of key or title) cannot see.
//
// Not folded into `list_work_items`: a board narrows a list in its own order,
// while this ranks and its answer IS the order, so `sort` would mean two things
// and a keyset cursor cannot page a ranking. Not `search_knowledge` either:
// that seam has Confluence behind it, which has no work items
// (worklet/search.go).

// WorkSearcher ranks work items by text, declared by the consumer.
//
// Nil with no index (a company on Jira, or an index not wired), and the tool is
// then omitted rather than refusing, on [Register]'s rule.
type WorkSearcher interface {
	Search(ctx context.Context, q worklet.SearchQuery) (worklet.SearchAnswer, error)
}

// ---- search_work_items --------------------------------------------------- //

type searchWorkItems struct{ deps WorkDeps }

var _ tools.SeatCallable = (*searchWorkItems)(nil)

func (t *searchWorkItems) Name() string { return worklet.SearchWorkItemsTool }

func (t *searchWorkItems) Description() string {
	return "Find work items by what they SAY — ranked over every item's title " +
		"and description, not a substring match. Use this when you remember " +
		"what a piece of work was about but not which item it is: \"the " +
		"retry backoff on the payment client\". " + ListWorkItemsTool + " is the other " +
		"half — it filters a board by assignee, status, project and the rest, " +
		"and its `text` only matches a key or a title."
}

func (t *searchWorkItems) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("What the work is about, in plain words "+
					"— at most %d bytes. Not a query language. Exactly one task "+
					"key answers that task first, but "+GetWorkItemTool+" reads a "+
					"key you already hold whole.",
					knowledge.MaxQueryBytes),
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many to return, 1..%d (default %d).",
					worklet.MaxSearchLimit, worklet.SearchLimit),
			},
			"mode": map[string]any{
				"type": "string",
				"enum": []any{string(knowledge.ModeHybrid), string(knowledge.ModeKeyword),
					string(knowledge.ModeSemantic)},
				"description": "How to rank. `hybrid` (the default) ranks by the " +
					"words and by meaning together. `keyword` ranks by the words " +
					"alone — for an exact name, identifier or error text. " +
					"`semantic` ranks by meaning alone — to find an item that " +
					"describes the same work in other words.",
			},
		},
		"required": []any{"text"},
	}
}

func (t *searchWorkItems) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *searchWorkItems) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.actor(ctx, turn)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return unattributed(worklet.SearchWorkItemsTool, err), nil
	}
	if t.deps.Search == nil {
		return unconfigured(worklet.SearchWorkItemsTool), nil
	}
	text := strings.TrimSpace(argString(args, "text"))
	if text == "" {
		return failed(SearchWorkItemsTool + " needs `text` — what the work is " +
			"about, in plain words."), nil
	}
	if knowledge.CheckQuery(text) != nil {
		// Refused, not cut, at [knowledge.MaxQueryBytes]: the search behind
		// refuses it as an error a model reads as the tracker failing, and
		// this says what to send instead.
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return failed(fmt.Sprintf(SearchWorkItemsTool+"' `text` is %d bytes and takes at "+
			"most %d — say what the work is about in a phrase, not the task or a "+
			"pasted thread.", len(text), knowledge.MaxQueryBytes)), nil
	}
	// The mode vocabulary is the one `search_knowledge` and the dashboard
	// share. An unknown mode is refused, naming the known ones: falling back
	// to the default would answer a different question.
	mode, err := knowledge.ParseMode(argString(args, "mode"))
	if err != nil {
		return refusedBy(tools.RefusalInvalid, err, unknownMode(SearchWorkItemsTool,
			argString(args, "mode"))), nil
	}
	answer, err := t.deps.Search.Search(ctx, worklet.SearchQuery{
		Text: text, Limit: argInt(args, "limit", 0), Mode: mode,
	})
	switch {
	case errors.Is(err, worklet.ErrIndexBuilding):
		// Not an empty answer: a model acts on "there is nothing" by
		// filing a duplicate, and a node still building cannot say yet.
		return refusedBy(tools.RefusalUnavailable, waits(), "This node is still building "+
			"its search index, so it cannot answer that yet — it says nothing "+
			"about whether the work exists. Try again shortly, or narrow it "+
			"with "+ListWorkItemsTool+"."), nil
	case err != nil:
		return readFailure(ctx, worklet.SearchWorkItemsTool, err), nil
	}
	hits := answer.Hits
	if hits == nil {
		hits = []worklet.Ranked{}
	}
	out := map[string]any{"query": text, "matches": hits, "count": len(hits),
		"mode": string(answer.ServedMode)}
	// A ranking other than the one asked for is said, or a seat reads a
	// keyword or empty semantic answer as "nothing describes this"
	// (internal/knowledge's mode doc).
	if note := workDegradedNote(mode, answer.Outcome); note != "" {
		out["degraded"] = note
	}
	if !answer.Coverage.Complete && answer.Coverage.BucketsMissing > 0 {
		// Said, because a ranking over part of the corpus reads exactly
		// like one over all of it.
		out["partial"] = "this search covered only part of the company's " +
			"work — some of the fleet did not answer in time — so an item " +
			"not listed here may still exist"
	}
	// Each snippet is fenced as the description it was cut from, as
	// get_work_item fences it (ADR-0040). The index keeps no author and only
	// company principals write here, so it is a colleague's text with nobody
	// named ([provenance.TrustForPartial]).
	read := newForeignRead(turn, actor.Handle)
	for i, hit := range hits {
		read.fence(fieldPath{}.key("matches").index(i).key("snippet"),
			provenance.SourceWorkDescription, nil, true, hit.Snippet)
	}
	// Every title is a name nobody is recorded as writing.
	if len(hits) > 0 {
		namesBy(read)
	}
	return read.answer(ctx, worklet.SearchWorkItemsTool, out, "Ask for fewer with `limit`.")
}

// workDegradedNote is what a work search that ranked other than it was asked to
// owes the caller, or "" when it served the mode asked for.
func workDegradedNote(asked knowledge.Mode, o knowledge.Outcome) string {
	switch o.Degraded {
	case knowledge.NotDegraded:
		return ""
	case knowledge.DegradedSemanticPartial:
		return "meaning was ranked over only part of the company's work — " +
			"some of the fleet could not compare by meaning — so an item " +
			"that describes this in other words may be missing here"
	}
	why := "nothing ranks work items by meaning here"
	switch o.Degraded {
	case knowledge.DegradedNoEmbeddings:
		why = "this company has no embeddings provider, so nothing ranks by meaning"
	case knowledge.DegradedEmbeddingFailed:
		why = "the query's meaning could not be computed just now; asking " +
			"again may rank by it"
	}
	if asked.Resolved() == knowledge.ModeSemantic {
		return "no search by meaning ran, so this says nothing about whether " +
			"the work exists: " + why + ". Search with mode `keyword` to rank " +
			"by the words instead"
	}
	return "ranked by the words alone: " + why
}
