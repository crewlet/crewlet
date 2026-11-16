package engine

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/research"
)

// ResearchSearch searches the company's listed hosts through this node's research
// instance of its search server (ADR-0151): resolved here as a shared server is,
// and run on a bridge of its own, never a seat's.
func (e *Engine) ResearchSearch(ctx context.Context, query string) (research.Results, error) {
	c := e.Company()
	if c == nil || c.Config.Assistant.Research == nil || c.Config.Assistant.Research.Search == nil {
		return research.Results{}, research.ErrSearchOff
	}
	r := c.Config.Assistant.Research
	i := slices.IndexFunc(c.Config.MCPServers, func(m config.MCPServer) bool {
		return m.Name == r.Search.MCPServer
	})
	if i < 0 {
		return research.Results{}, research.ErrSearchOff
	}
	server := &c.Config.MCPServers[i]
	values, missing := resolveServer(server, "", server.Name, nil, e.resolver(), nil)
	if missing != nil {
		return research.Results{}, fmt.Errorf("%w: %w", research.ErrSearch, missing)
	}
	spec, err := serverSpec(server, server.Name, values)
	if err != nil {
		return research.Results{}, fmt.Errorf("%w: %w", research.ErrSearch, err)
	}
	return e.researchSearcher().Search(ctx, spec, r.Hosts, query)
}

// researchSearcher is this node's research instance, made at its first search.
func (e *Engine) researchSearcher() *research.Searcher {
	e.researchOnce.Do(func() { e.research = research.NewSearcher() })
	return e.research
}
