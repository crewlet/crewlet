package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Finding a work item by what it SAYS.
//
// # Why this is a verb of its own beside `list_work_items`
//
// The board's `text` argument is a substring of the key or the title, composed
// into the board query with every other filter. It is the right thing for "the
// item whose key I am half sure of", and it cannot see a description at all —
// so the whole of what somebody wrote down about a piece of work was
// unreachable from a seat, while the engine had been paying to embed every one
// of those descriptions the entire time.
//
// The two are not one verb because they answer differently shaped questions. A
// board narrows a list and keeps the board's own order; this ranks a corpus
// and its answer IS the order. Folding ranking into the board would make the
// sort argument mean two things depending on whether `text` was set, and
// paging a ranked list through a keyset cursor over a board's sort is not a
// thing that works.
//
// # And why it is not `search_knowledge`
//
// That seam is backend-neutral with Confluence behind it, and Confluence has
// no work items. See tracker/search.go.

// WorkSearcher ranks work items by text, declared by the consumer.
//
// Nil on a build with no index — a company on Jira, or a node whose index has
// not been wired — and the tool is then OMITTED rather than refusing at the
// call, on [Register]'s own rule.
type WorkSearcher interface {
	Search(ctx context.Context, text string, limit int) ([]tracker.Ranked, error)
}

// ---- search_work_items --------------------------------------------------- //

type searchWorkItems struct{ deps WorkDeps }

var _ tools.SeatCallable = (*searchWorkItems)(nil)

func (t *searchWorkItems) Name() string { return tracker.SearchWorkItemsTool }

func (t *searchWorkItems) Description() string {
	return "Find work items by what they SAY — ranked over every item's title " +
		"and description, not a substring match. Use this when you remember " +
		"what a piece of work was about but not which item it is: \"the " +
		"retry backoff on the payment client\". list_work_items is the other " +
		"half — it filters a board by assignee, status, project and the rest, " +
		"and its `text` only matches a key or a title."
}

func (t *searchWorkItems) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{
				"type": "string",
				"description": "What the work is about, in plain words. Not a " +
					"query language and not a key.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many to return, 1..%d (default %d).",
					tracker.MaxSearchLimit, tracker.SearchLimit),
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

	if _, err := t.deps.actor(ctx, turn); err != nil {
		//nolint:nilerr // A tool failure is a RESULT the caller reads.
		return notInATurn(tracker.SearchWorkItemsTool), nil
	}
	if t.deps.Search == nil {
		return unconfigured(tracker.SearchWorkItemsTool), nil
	}
	text := strings.TrimSpace(argString(args, "text"))
	if text == "" {
		return failed("search_work_items needs `text` — what the work is " +
			"about, in plain words."), nil
	}
	hits, err := t.deps.Search.Search(ctx, text, argInt(args, "limit", 0))
	switch {
	case errors.Is(err, tracker.ErrIndexBuilding):
		// NOT AN EMPTY ANSWER. "There is nothing" is what a model acts
		// on by filing a duplicate, and the honest answer while a node
		// is still building its index is that it cannot say yet.
		return failed("This node is still building its search index, so it " +
			"cannot answer that yet — it says nothing about whether the work " +
			"exists. Try again shortly, or narrow it with list_work_items."), nil
	case err != nil:
		return failed(readFailure(tracker.SearchWorkItemsTool, err)), nil
	}
	return jsonAnswer(map[string]any{
		"query": text, "matches": hits, "count": len(hits),
	}, "Ask for fewer with `limit`.")
}
