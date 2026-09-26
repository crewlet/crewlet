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

// draftOverfetch is how many times the caller's limit a search asks the site
// for.
//
// THE ANCESTOR EXCLUSION IS THE DROP IT PAYS FOR, and it is the one drop the
// site cannot make for this searcher: whether a hit is an unreviewed draft is
// judged on the parent chain the site returns with it ([knowledge.Excludes]),
// after the site has already cut its ranking at the depth it was asked for.
// Without headroom a search whose top rows are all drafts comes back empty,
// which reads as a knowledge base with nothing in it. The tool-skills space is
// excluded in the query itself ([Searcher.query]), so no row of the headroom
// is spent on it.
//
// A MULTIPLE OF THE LIMIT rather than a fixed allowance, because drafts are a
// share of what the site ranks: the rows they take grow with the depth asked
// for, and a fixed allowance tolerates a smaller share the larger the limit.
// Three leaves an answer short only when more than two in three of the rows
// the site ranked were dropped — the tolerance the native backend's
// over-fetch gives ([github.com/crewlet/crewlet/internal/pages.SearchOverfetch])
// — and an answer left short while the site may rank more is logged; see
// [Searcher.Search].
const draftOverfetch = 3

// SeatClient resolves a seat's own Confluence client, reporting whether the
// seat authenticates as ITSELF.
//
// Two results rather than one, because a nil client and a client that is the
// ORG's are different facts with opposite consequences: the org's can search
// but may not search unscoped.
type SeatClient func(seat *org.Role) (*Client, bool)

// Searcher implements [knowledge.Searcher] over Confluence's CQL search.
//
// ITS ANSWER IS THE SEAM'S OWN, and nothing between this searcher and a reader
// rewrites it: the engine's adapter that resolves the current searcher per call
// forwards the answer unchanged. So a failed search reaches every reader marked
// [knowledge.Answer.Failed], never as a search that matched nothing.
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

var _ knowledge.Searcher = (*Searcher)(nil)

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

// Building implements [knowledge.Searcher], and is always false: this search
// is a live query against the site, with no index of this node's own to build.
func (s *Searcher) Building(context.Context) bool { return false }

// Search implements [knowledge.Searcher].
//
// BEST EFFORT: it never reports an error, and a turn never dies because a wiki
// was slow. Every path on which the query did not run against the site answers
// an empty answer MARKED FAILED, with a log line saying why: an unmarked empty
// answer reads as a search that matched nothing, which sends a seat to try
// other words against a search that is not running, or to write the page it
// could not find. Text with nothing in it is the one empty answer that is not
// a failure, because nothing was asked.
func (s *Searcher) Search(ctx context.Context, q knowledge.Query) knowledge.Answer {
	if strings.TrimSpace(q.Text) == "" {
		return knowledge.Answer{}
	}
	if s == nil {
		log.WarnContext(ctx, "confluence_search_failed",
			"error", "no Confluence searcher is wired",
			"detail", "the search did not run")
		return knowledge.Answer{Failed: true}
	}
	client, self := s.clientFor(q.Seat)
	if client == nil {
		log.WarnContext(ctx, "confluence_search_failed",
			"error", "no credential to search with: the seat holds no "+
				"Confluence credential of its own and no org client is wired",
			"detail", "the search did not run; integrations.confluence.token is "+
				"the credential a seat without its own searches with")
		return knowledge.Answer{Failed: true}
	}
	scope := scopeOf(q.Org)
	allowed, _ := knowledge.Permitted(scope, self)
	// AN EMPTY CQL IS THE SAME CONDITION as a refused permission, and it is
	// checked as well because running a search with neither would search the
	// whole instance. A caller reaches either only by skipping CanSearch,
	// which answers this rule without I/O.
	cql := ""
	if allowed {
		cql = s.query(q.Text, scope, self)
	}
	if cql == "" {
		log.WarnContext(ctx, "confluence_search_failed",
			"error", "no knowledge.scope is declared and the search holds no "+
				"credential of its own to search unscoped with",
			"detail", "the search did not run; knowledge.Permitted refuses it, "+
				"which CanSearch reports before any search is made")
		return knowledge.Answer{Failed: true}
	}

	asked := q.Hits() * draftOverfetch
	pages, err := client.Search(ctx, cql, asked)
	if err != nil {
		log.WarnContext(ctx, "confluence_search_failed", "error", err.Error(),
			"detail", "the search did not complete, and its answer is marked failed")
		return knowledge.Answer{Failed: true}
	}
	hits := s.hits(pages, q)
	if shortOfTheSite(len(hits), q.Hits(), len(pages), asked) {
		// THE SITE ANSWERED EVERY ROW ASKED FOR, so it may rank more than
		// were fetched, and the exclusions left fewer than the limit: the
		// pages ranked below the fetched depth are missing from this
		// answer, and nothing on the seam's answer can say so. The log
		// line is where it is said.
		log.WarnContext(ctx, "confluence_search_short", "hits", len(hits),
			"limit", q.Hits(), "ranked", len(pages),
			"detail", "the site returned every row asked for and the draft "+
				"exclusion dropped enough of them to leave the answer short of "+
				"its limit, so pages the site ranked lower are missing from it")
	}
	// NO FAN-OUT AND NO SECOND RANKER: one live query against one site, so
	// there is nothing for a [knowledge.Partial] to count.
	return knowledge.Answer{Hits: hits}
}

// shortOfTheSite reports an answer the exclusions left short of its limit
// while the site may rank more than it was asked for: it answered every row
// asked for, so its ranking can go on past what was fetched.
//
// A site that answers fewer rows than asked has run out of matches, or capped
// its own page below what was asked. The rows cannot tell those two apart, so
// such an answer is not reported.
func shortOfTheSite(hits, limit, ranked, asked int) bool {
	return hits < limit && ranked >= asked
}

// query renders the CQL a search sends, or "" for one that may not run.
//
// THE TOOL-SKILLS SPACE IS EXCLUDED IN THE QUERY, so the site never ranks a
// page from it: dropped from what came back instead, every skill page among
// the site's top rows would take a row of [draftOverfetch] that a knowledge
// page could have had. Appended to the rendered search rather than subtracted
// from the scope list, because a scope that named only that space would then
// be an EMPTY scope — which is the unscoped search, on a seat with its own
// credential, of the whole instance.
func (s *Searcher) query(text string, scope []string, self bool) string {
	cql := BuildCQL(text, scope, self)
	if cql == "" || s.skillsSpace == "" {
		return cql
	}
	return cql + ` AND space != "` + EscapeCQL(s.skillsSpace) + `"`
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
		// A SKILL PAGE THE SITE RETURNED ANYWAY. The query excludes the
		// space ([Searcher.query]); this keeps one out of a seat's prompt
		// from a site that answered it regardless, because a skill page
		// read as knowledge is followed as an instruction.
		if s.skillsSpace != "" && strings.EqualFold(page.Space, s.skillsSpace) {
			continue
		}
		hit := knowledge.Hit{
			Title: page.Title, URL: s.link(page), Container: page.Space,
			PageID: page.ID, Ancestors: page.Ancestors,
			// KNOWN WHEN THE ANSWER CARRIED THE CHAIN, an empty one
			// included: `"ancestors": []` is a page at the top of its
			// space, so a draft a lead moved there is published by the
			// move like one moved under another page. An answer that lost
			// the expand carries no key at all, and that one stays unknown
			// — claimed as known, it would publish every draft whose
			// parent the site left out.
			AncestorsKnown: page.AncestorsKnown,
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
