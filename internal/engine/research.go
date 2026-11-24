package engine

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/fleet"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
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
	searcher := e.researchSearcher()
	if searcher == nil {
		return research.Results{}, research.ErrSearchOff
	}
	hits, tokens, err := searcher.Search(ctx, query)
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
func (e *Engine) CanResearchSearch() bool { return e.researchSearcher() != nil }

// researchSearcher is the search run of the entry Ask Crewlet runs on now:
// its model's, or, on an LLM fleet, its home's or else the pick it would
// take ([helperPick], read without taking). Nil where that entry has none.
func (e *Engine) researchSearcher() *harness.Provider {
	c := e.Company()
	if c == nil || c.Config == nil || len(c.Searchers) == 0 {
		return nil
	}
	ref, err := c.Config.AssistantModel()
	if err != nil {
		return nil
	}
	key := ref
	if fleetKey, onFleet := config.FleetOf(ref); onFleet {
		key = c.Models.Home(fleet.Job{Fleet: fleetKey, Subject: "assistant", Job: "assistant"})
		if c.Searchers[key] == nil {
			run, err := helperPick(c, "assistant.model", ref, "assistant", false)
			if err != nil {
				return nil
			}
			key = run.key
		}
	}
	return c.Searchers[key]
}
