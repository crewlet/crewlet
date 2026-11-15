package queries

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/research"
)

// ResearchReader reads one page from a host the company lists (ADR-0151).
type ResearchReader interface {
	Read(ctx context.Context, hosts []string, url string, from int) (research.Page, error)
}

// researchPage answers `research_page`: one part of a web page on a host
// `assistant.research` lists, read by the engine, never by the caller.
func (s Sources) researchPage(ctx context.Context, p Params) (any, error) {
	var hosts []string
	if company, _ := s.Company(); company != nil {
		hosts = company.Assistant.ResearchHosts()
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%w: web research is off: an administrator lists the hosts "+
			"Ask Crewlet may read in assistant.research.hosts", ErrBadParams)
	}
	url := strings.TrimSpace(p.String("url"))
	page, err := s.Research.Read(ctx, hosts, url, p.Int("from", 0))
	switch {
	case errors.Is(err, research.ErrNotListed), errors.Is(err, research.ErrNotPage):
		return nil, fmt.Errorf("%w: %w", ErrBadParams, err)
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return page, nil
}
