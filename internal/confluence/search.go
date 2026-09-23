package confluence

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
)

// The knowledge backend: live CQL search behind the neutral seam.
//
// # It searches as the ASKING SEAT wherever it can
//
// A seat with its own Atlassian credential searches as itself, so Confluence
// enforces that account's page permissions natively and the engine keeps no
// restricted-page bookkeeping of its own. A seat without one falls back to
// the org account — which sees the whole instance, and is exactly why an
// unscoped search is then refused rather than run.
//
// That asymmetry is the whole of [knowledge.Permitted], and it is stated in
// the seam rather than here so both backends cannot answer it differently.

// overfetch is how many extra rows are asked for beyond the caller's limit.
//
// The ancestor filter drops rows AFTER the server has truncated, so without
// headroom a search whose top hits are all auto-drafts comes back empty —
// which reads as a knowledge base with nothing in it.
const overfetch = 5

// SeatClient resolves a seat's own Confluence client, reporting whether the
// seat authenticates as ITSELF.
//
// Two results rather than one, because a nil client and a client that is the
// ORG's are different facts with opposite consequences: the org's can search
// but may not search unscoped.
type SeatClient func(seat *org.Role) (*Client, bool)

// Searcher implements [knowledge.Searcher] over Confluence's CQL search.
type Searcher struct {
	org     *Client
	forSeat SeatClient

	// skillsSpace holds the tool-skill pages. They are machinery rather
	// than knowledge, and an agent told to read one would follow an
	// instruction written for a different phase.
	skillsSpace string

	// siteURL is the human base for the links on a hit. Empty omits them:
	// a Cloud gateway address is not somewhere a browser goes, and a link
	// that 404s costs an agent a round to discover.
	siteURL string
}

// SearcherOptions configure a [Searcher].
type SearcherOptions struct {
	Org         *Client
	ForSeat     SeatClient
	SkillsSpace string
	SiteURL     string
}

// NewSearcher builds the searcher.
func NewSearcher(opts SearcherOptions) *Searcher {
	return &Searcher{
		org: opts.Org, forSeat: opts.ForSeat,
		skillsSpace: strings.ToUpper(strings.TrimSpace(opts.SkillsSpace)),
		siteURL:     strings.TrimRight(strings.TrimSpace(opts.SiteURL), "/"),
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
		return nil
	}
	cql := BuildCQL(q.Text, scope, self)
	if cql == "" {
		// Belt and braces on the rule above: an empty CQL and a refused
		// permission are the same condition, and running a search with
		// neither would search the whole instance.
		return nil
	}

	pages, err := client.Search(ctx, cql, q.Hits()+overfetch)
	if err != nil {
		log.WarnContext(ctx, "confluence_search_failed", "error", err.Error(),
			"detail", "the turn gets an empty knowledge block")
		return nil
	}
	return s.hits(pages, q)
}

// hits filters and renders what came back.
//
// THE EXCLUSION IS [knowledge.Excludes], the rule the native backend applies
// too, so a page is kept or dropped the same way whichever backend a company
// runs. What this backend contributes is how much of the chain it knows.
func (s *Searcher) hits(pages []Page, q knowledge.Query) []knowledge.Hit {
	excluded := q.Excluded()
	out := make([]knowledge.Hit, 0, q.Hits())
	for _, page := range pages {
		if len(out) >= q.Hits() {
			break
		}
		if s.skillsSpace != "" && strings.EqualFold(page.Space, s.skillsSpace) {
			continue
		}
		hit := knowledge.Hit{
			Title: page.Title, URL: s.link(page), Container: page.Space,
			PageID: page.ID, Ancestors: page.Ancestors,
			// KNOWN ONLY WHEN IT CAME BACK NON-EMPTY. The chain is the
			// search's `ancestors` expand, and an empty one is either a
			// page at the top of its space or an answer that lost the
			// expand — which [Page] does not tell apart. Claimed as
			// known, the second would publish every draft whose parent
			// the site left out; left unknown, a draft at the top of its
			// space is judged by its title prefix, which fails closed.
			AncestorsKnown: len(page.Ancestors) > 0,
		}
		if knowledge.Excludes(hit, excluded) {
			continue
		}
		hit.Snippet = knowledge.Snippet(Flatten(page.Body), knowledge.SnippetLimit)
		out = append(out, hit)
	}
	return out
}

// link is the address a person opens, or empty.
func (s *Searcher) link(page Page) string {
	if s.siteURL == "" || page.Space == "" || page.ID == "" {
		return ""
	}
	return s.siteURL + "/wiki/spaces/" + page.Space + "/pages/" + page.ID
}

// clientFor picks the credential a search runs under.
func (s *Searcher) clientFor(seat *org.Role) (*Client, bool) {
	if s.forSeat != nil && seat != nil {
		if client, self := s.forSeat(seat); client != nil {
			return client, self
		}
	}
	return s.org, false
}

// authenticatesAsSelf reports a seat with its own Confluence credential.
//
// NO I/O: the gate above must stay free, so this asks the resolver whether a
// client COULD be built rather than building one and using it.
func (s *Searcher) authenticatesAsSelf(seat *org.Role) bool {
	if s.forSeat == nil || seat == nil {
		return false
	}
	client, self := s.forSeat(seat)
	return client != nil && self
}

// scopeOf is the org-wide read scope for this backend.
//
// ROLE-INDEPENDENT: a unit's own space is its IDENTITY — where its pages
// live and where its events route — and letting an identity double as a read
// scope is how an agent ends up unable to read the page it was told to
// follow.
func scopeOf(o *org.Organization) []string {
	if o == nil {
		return nil
	}
	return knowledge.Scope(o.KnowledgeScope)
}
