package plane

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
)

// The knowledge backend: workspace page search behind the neutral seam.
//
// # The server ANDs the query, and that is the whole design problem
//
// A page search here matches every token, case-insensitively, against a
// page's name and its stripped body. An auxiliary model asked to turn "the
// deploy keeps failing on staging" into a query produces five or six terms,
// and a conjunction of six terms matches nothing in a knowledge base of
// forty pages — so the common outcome of a naive search is an empty block
// and a planner that concludes the company knows nothing about deploys.

// MaxQueryTokens is the server's own distinct-token ceiling.
//
// Trimmed HERE rather than discovered as an error, because going over is a
// rejected request — which turns a wordy query into no knowledge at all
// rather than into a looser match.
const MaxQueryTokens = 16

// relaxPrefixes are the looser conjunctions tried when the full query
// matches nothing.
//
// LEADING PREFIXES rather than dropping terms from the middle: the prompt
// that generates these queries is told to lead with the topical terms and
// trail with qualifiers, so the front of the query is the part worth keeping.
//
// At most three searches in total, which bounds the Plan phase's latency. A
// single-token attempt is deliberately absent: one term matches half the
// corpus and the endpoint ranks by RECENCY rather than relevance, so the
// result reads as noise wearing the shape of knowledge.
var relaxPrefixes = []int{4, 2}

// overfetch is how many extra rows are asked for beyond the caller's limit.
//
// The post-filters — the skills project, the auto-draft parent — drop rows
// after the server has already truncated, so without headroom a search whose
// top hits are all drafts comes back empty. The window extends toward OLDER
// pages, since the ordering is by recency.
const overfetch = 5

// Searcher implements [knowledge.Searcher] over Plane's page search.
type Searcher struct {
	// engine is the client used when a seat has no key of its own.
	engine *Client
	// forSeat resolves a seat's own API key. A seat WITH one searches as
	// itself, bounded by what its account can read; a seat without one is
	// on the shared engine credential and may not search unscoped.
	forSeat func(seat *org.Role) (*Client, bool)
	cache   *ProjectCache
	// skillsProject is the engine-managed project holding tool skills. Its
	// pages are machinery rather than knowledge, and a planner told to
	// read them would follow an instruction meant for a different phase.
	skillsProject string
}

// SearcherOptions configure a [Searcher].
type SearcherOptions struct {
	Engine        *Client
	ForSeat       func(seat *org.Role) (*Client, bool)
	Cache         *ProjectCache
	SkillsProject string
}

// NewSearcher builds the searcher.
func NewSearcher(opts SearcherOptions) *Searcher {
	return &Searcher{
		engine: opts.Engine, forSeat: opts.ForSeat, cache: opts.Cache,
		skillsProject: strings.ToUpper(strings.TrimSpace(opts.SkillsProject)),
	}
}

// Backend implements [knowledge.Searcher].
func (s *Searcher) Backend() string { return Backend }

// CanSearch implements [knowledge.Searcher].
//
// NO I/O, which is its entire job: the prefetch skips the auxiliary model
// call that generates a query when the search is a guaranteed no-op, and a
// gate that had to reach the network to answer would cost more than the call
// it saves.
func (s *Searcher) CanSearch(seat *org.Role, o *org.Organization) bool {
	if s == nil {
		return false
	}
	allowed, _ := knowledge.Permitted(scopeOf(o), s.authenticatesAsSelf(seat))
	return allowed
}

// Search implements [knowledge.Searcher].
//
// BEST EFFORT: it never reports an error. Every failure path is an empty
// result and the prefetch degrades to an empty block — a turn must not die
// because a wiki was slow.
func (s *Searcher) Search(ctx context.Context, q knowledge.Query) []knowledge.Hit {
	if s == nil || strings.TrimSpace(q.Text) == "" {
		return nil
	}
	client, self := s.clientFor(q.Seat)
	if client == nil {
		return nil
	}
	scope := scopeOf(q.Org)
	allowed, _ := knowledge.Permitted(scope, self)
	if !allowed {
		// An empty scope plus a seat on the shared engine credential.
		// Searching the whole instance here is how one seat reads what
		// its own account never could.
		return nil
	}

	// PRIME THE CACHE before anything is filtered. The admission rules
	// below compare a hit's project IDENTIFIER — the skills project, an
	// operator's exclusions — and an unprimed cache answers "" for every
	// hit, which silently turns every one of those rules off. It costs
	// one request per refetch floor, not one per search.
	s.cache.Identifier(ctx, "prime")

	var projectIDs []string
	if len(scope) > 0 {
		projectIDs = s.cache.IDsFor(ctx, scope)
		if len(projectIDs) == 0 {
			// The operator named a scope and none of it resolved.
			// Searching UNSCOPED here would quietly ignore the
			// narrowing they asked for, so this returns nothing and
			// says why.
			log.Warn("plane_knowledge_scope_unresolved", "scope", scope)
			return nil
		}
	}

	pages := s.relaxed(ctx, client, q.Text, projectIDs, q.Hits()+overfetch)
	return s.admit(pages, q)
}

// relaxed runs the search, loosening the conjunction on zero hits.
func (s *Searcher) relaxed(ctx context.Context, client *Client, query string, projectIDs []string, limit int) []Page {
	tokens := strings.Fields(query)
	if len(tokens) > MaxQueryTokens {
		tokens = tokens[:MaxQueryTokens]
	}
	attempts := [][]string{tokens}
	for _, n := range relaxPrefixes {
		// Only a prefix that is actually SHORTER than the last attempt:
		// re-running the same conjunction spends a round trip to
		// discover the same nothing.
		if n < len(attempts[len(attempts)-1]) {
			attempts = append(attempts, tokens[:n])
		}
	}
	for _, attempt := range attempts {
		if len(attempt) == 0 {
			continue
		}
		pages, err := client.SearchPages(ctx, strings.Join(attempt, " "), projectIDs, limit)
		if err != nil {
			log.Warn("plane_search_failed", "error", err.Error())
			return nil
		}
		if len(pages) > 0 {
			return pages
		}
	}
	return nil
}

// admit filters and shapes what the server returned.
func (s *Searcher) admit(pages []Page, q knowledge.Query) []knowledge.Hit {
	excluded := q.Excluded()
	out := make([]knowledge.Hit, 0, min(len(pages), q.Hits()))
	for _, page := range pages {
		if len(out) >= q.Hits() {
			break
		}
		identifier := s.identifierOf(page.ProjectID)
		// The skills project is machinery: a planner told to read a
		// tool-skill page would follow an instruction meant for a
		// different phase of a different turn.
		if s.skillsProject != "" && strings.EqualFold(identifier, s.skillsProject) {
			continue
		}
		hit := knowledge.Hit{
			Title: page.Name, Container: identifier, PageID: page.ID,
			Snippet: knowledge.Snippet(page.Description),
			// NO ANCESTORS. Plane pages have no parent chain, which is
			// exactly why the auto-draft title prefix exists as the
			// backstop — see knowledge.Excludes.
		}
		if page.ProjectID != "" && page.ID != "" {
			hit.URL = s.engineURL(page)
		}
		if knowledge.Excludes(hit, excluded) {
			continue
		}
		out = append(out, hit)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (s *Searcher) engineURL(page Page) string {
	client := s.engine
	if client == nil {
		return ""
	}
	return client.URL() + "/" + client.Workspace() +
		"/projects/" + page.ProjectID + "/pages/" + page.ID
}

func (s *Searcher) identifierOf(projectID string) string {
	if s.cache == nil || projectID == "" {
		return ""
	}
	// The cache's own context: this runs inside a search that already has
	// one, and threading it here would let a filter decide to make a
	// network call — which the filter has no business doing.
	return s.cache.cached(projectID)
}

// clientFor picks the credential a seat searches under, and reports whether
// it is the seat's OWN.
func (s *Searcher) clientFor(seat *org.Role) (*Client, bool) {
	if s.forSeat != nil {
		if client, ok := s.forSeat(seat); ok && client != nil {
			return client, true
		}
	}
	return s.engine, false
}

// authenticatesAsSelf is the no-I/O half of the same question.
func (s *Searcher) authenticatesAsSelf(seat *org.Role) bool {
	if s.forSeat == nil {
		return false
	}
	client, ok := s.forSeat(seat)
	return ok && client != nil
}

// scopeOf is the org-wide read scope for this backend.
func scopeOf(o *org.Organization) []string {
	if o == nil {
		return nil
	}
	return knowledge.Scope(o.PlaneProjects)
}

// cached is the lookup with no fetch, for a filter that must not do I/O.
func (p *ProjectCache) cached(projectID string) string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.byID[strings.ToLower(projectID)]
}
