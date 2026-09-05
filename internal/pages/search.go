package pages

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/projection"
)

// Searcher answers the company's knowledge search over its own pages.
//
// It implements [knowledge.Searcher], which is what makes the native backend
// a drop-in for Confluence: the turn-start prefetch and the `search_knowledge`
// builtin both read through that seam, so neither knows which answered.
//
// # What "unscoped" means here, and why it differs from Confluence
//
// On Confluence an empty read scope means "whatever the ASKING SEAT's own
// account can read", and a credential-less seat gets nothing — because an
// unscoped query on the shared org token is how one seat reads a page its own
// account never could.
//
// Natively there is no second account. Every reader is a seat of one company,
// the engine IS the boundary, and there is nothing to launder a read through.
// So an empty scope means the whole company, and [knowledge.Permitted] is not
// consulted: its selfAuth question has no meaning on a backend where identity
// is not a credential.
//
// # Best effort, as the seam requires
//
// Every failure path is an empty result and a log line. A turn must not die
// because an index was rebuilding — but "not indexed yet" and "nothing
// matched" are different facts, and [Searcher.Building] is how a caller tells
// them apart so a seat on a fresh node is not told the company has written
// nothing down.
type Searcher struct {
	index *projection.Indexer

	// excluded are the containers a search never returns: the tool-skills
	// container, whose pages are machinery a seat told to read one would
	// follow as an instruction.
	excluded []string
}

// SearcherOptions configure a searcher.
type SearcherOptions struct {
	Index *projection.Indexer

	// SkillsContainer is the reserved tool-skills container, excluded from
	// every result. Empty for a company that has turned tool skills off,
	// where nothing is reserved and nothing is excluded.
	SkillsContainer string
}

// NewSearcher builds the native knowledge searcher.
func NewSearcher(opts SearcherOptions) *Searcher {
	s := &Searcher{index: opts.Index}
	if key := strings.ToUpper(strings.TrimSpace(opts.SkillsContainer)); key != "" {
		s.excluded = append(s.excluded, key)
	}
	return s
}

var _ knowledge.Searcher = (*Searcher)(nil)

// Backend names the integration answering.
func (s *Searcher) Backend() string { return "native" }

// CanSearch is the cheap, no-I/O pre-gate.
//
// TRUE WHENEVER THERE IS AN INDEX, because on this backend every seat can
// read every page: there is no per-seat credential to be missing, which is
// the condition that makes Confluence's gate answer false. Its only job is
// letting the prefetch skip the auxiliary model call when the search is a
// guaranteed no-op, and here that is only "no index at all".
func (s *Searcher) CanSearch(*org.Role, *org.Organization) bool { return s.index != nil }

// Building reports whether the index is still catching up with the projection.
//
// SEPARATE FROM CanSearch, because they answer different questions and a
// caller acts on them differently: CanSearch gates the expensive query
// generation, while this is what turns an empty block into "the index for
// this node is still building" rather than "the company has written nothing
// down". A seat on a freshly joined node would otherwise be told the second
// for the whole first index build.
//
// It does I/O — one indexed count — which is why it is not the gate.
func (s *Searcher) Building(ctx context.Context) bool {
	if s.index == nil {
		return false
	}
	ready, err := s.index.Ready(ctx)
	if err != nil {
		// UNKNOWN READS AS BUILDING. A caller that renders "still
		// building" when the store hiccupped has told a seat something
		// harmless; one that renders "nothing written down" has told it
		// something false that it will act on.
		return true
	}
	return !ready
}

// Search returns up to Limit ranked hits. Best effort: every failure path is
// an empty result.
func (s *Searcher) Search(ctx context.Context, q knowledge.Query) []knowledge.Hit {
	if s.index == nil || strings.TrimSpace(q.Text) == "" {
		return nil
	}
	scope := knowledge.Scope(scopeOf(q.Org))
	hits, err := s.index.Search(ctx, projection.SearchQuery{
		Text:       q.Text,
		Containers: scope,
		Sources:    []string{"page"},
		// OVER-FETCHED, because the exclusions below drop hits after
		// ranking: asking for exactly the limit and then removing three
		// skill pages would return five results where eight were
		// available.
		Limit: q.Hits() * searchOverfetch,
	})
	if err != nil {
		log.WarnContext(ctx, "pages_search_failed", "error", err.Error(),
			"detail", "the knowledge block degrades to empty; a turn must not "+
				"die because an index was slow")
		return nil
	}

	out := make([]knowledge.Hit, 0, q.Hits())
	for _, hit := range hits {
		if s.isExcluded(hit.Container) {
			continue
		}
		out = append(out, knowledge.Hit{
			Title:     hit.Title,
			Container: hit.Container,
			PageID:    hit.ID,
			Snippet:   hit.Snippet,
		})
		if len(out) == q.Hits() {
			break
		}
	}
	return out
}

// searchOverfetch is how many times the limit is asked for before exclusions.
//
// Three. The exclusions drop a bounded fraction — tool-skill pages in one
// reserved container — so a wider factor buys nothing and a narrower one
// returns short result sets on a company with many skills.
const searchOverfetch = 3

// isExcluded reports a container a search never returns.
func (s *Searcher) isExcluded(container string) bool {
	for _, key := range s.excluded {
		if strings.EqualFold(container, key) {
			return true
		}
	}
	return false
}

// scopeOf is the org-wide read scope, or nil for unscoped.
func scopeOf(o *org.Organization) []string {
	if o == nil {
		return nil
	}
	return o.KnowledgeScope
}
