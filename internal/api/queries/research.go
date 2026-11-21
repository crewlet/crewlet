package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/research"
)

// ResearchReader reads one part of a public https page.
type ResearchReader interface {
	Read(ctx context.Context, url string, from int) (research.Page, error)
}

// ResearchSearcher searches the web through Ask Crewlet's search run.
type ResearchSearcher interface {
	ResearchSearch(ctx context.Context, query string) (research.Results, error)
}

// researchSearch answers `research_search`: the results Ask Crewlet's search
// run's search tool returned, and the tokens the run spent.
//
// ASKED THROUGH ASK CREWLET ALONE: the search spends model tokens, which the
// turn that asked is charged, and nothing else would charge them.
func (s Sources) researchSearch(ctx context.Context, p Params) (any, error) {
	principal, err := caller(ctx, "research_search")
	if err != nil {
		return nil, err
	}
	if _, assisted := principal.Assisted(); !assisted {
		return nil, fmt.Errorf("%w: a web search is Ask Crewlet's, asked in a turn", ErrUnauthorized)
	}
	results, err := s.Searcher.ResearchSearch(ctx, p.String("q"))
	switch {
	case errors.Is(err, research.ErrSearchOff), errors.Is(err, research.ErrQuery):
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return results, nil
}

// researchPage answers `research_page`: one part of a public https page, read
// by the engine, never by the caller.
func (s Sources) researchPage(ctx context.Context, p Params) (any, error) {
	url := strings.TrimSpace(p.String("url"))
	page, err := s.Research.Read(ctx, url, p.Int("from", 0))
	switch {
	case errors.Is(err, research.ErrNotPublic), errors.Is(err, research.ErrNotPage),
		errors.Is(err, httpx.ErrNotPublic):
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return page, nil
}
