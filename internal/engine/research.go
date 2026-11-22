package engine

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/research"
)

// ResearchSearch searches the web for query through Ask Crewlet's search run
// of its own model ([buildSearcher]): the harness's web search and no other
// tool, handed the query alone. The results are the search tool's own, and
// the tokens the run spent are answered for the turn that asked to be charged.
func (e *Engine) ResearchSearch(ctx context.Context, query string) (research.Results, error) {
	query = strings.Join(strings.Fields(query), " ")
	if query == "" || utf8.RuneCountInString(query) > research.MaxQuery {
		return research.Results{}, research.ErrQuery
	}
	c := e.Company()
	if c == nil || c.Searcher == nil {
		return research.Results{}, research.ErrSearchOff
	}
	hits, tokens, err := c.Searcher.Search(ctx, query)
	if err != nil {
		return research.Results{}, fmt.Errorf("%w: %w", research.ErrSearch, err)
	}
	out := research.Results{Query: query, Results: make([]research.Result, 0, len(hits)),
		Tokens: tokens}
	for _, h := range hits {
		out.Results = append(out.Results, research.Result{Title: h.Title, URL: h.URL})
	}
	return out, nil
}

// CanResearchSearch reports whether Ask Crewlet's model has a search run.
func (e *Engine) CanResearchSearch() bool {
	c := e.Company()
	return c != nil && c.Searcher != nil
}
