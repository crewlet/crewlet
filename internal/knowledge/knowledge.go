// Package knowledge is the backend-neutral seam the Plan-phase "relevant
// knowledge" prefetch talks to the team knowledge base through.
//
// Exactly ONE backend per company, chosen by which integration is
// configured. Two would mean an agent's answer to "what do we already know
// about this" depends on which searcher happened to be asked, and neither
// would be wrong.
//
// # What the seam hides
//
// The container vocabulary. A caller passes a seat, a plain-text query and
// ancestor exclusions — never a CQL fragment, a space key or a project list.
// That is the whole point: the two backends narrow reads in completely
// different terms, and a caller that knew either one would be a caller that
// has to change when the company switches backends.
//
// # The rules every backend honours
//
//   - SCOPE LIVES BEHIND THE SEAM, and is derived per call from the org, so a
//     live config edit to the read scope takes effect with no refresh hook.
//   - UNSCOPED IS NOT THE SAME AS UNBOUNDED. An empty scope plus a seat that
//     authenticates as its own backend user means an unscoped search, bounded
//     by that account's own ACLs. An empty scope plus a seat with no
//     credential of its own means NO results — searching the whole instance
//     on a shared engine credential is how one seat reads what its own
//     account never could.
//   - BEST EFFORT. Search never fails the caller: every failure path is an
//     empty result and the prefetch degrades to an empty block. A turn must
//     not die because a wiki was slow.
package knowledge

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// AutoDraftedParent is the page every unreviewed auto-drafted skill is
// posted under, in the unit's own container. A unit lead publishes a draft by
// moving it out.
//
// Searchers drop hits beneath it when the caller lists it in the exclusions —
// which the prefetch does by default — so no agent follows an unvetted
// LLM-proposed procedure during the review window.
const AutoDraftedParent = "Auto-Drafted Skills"

// AutoDraftTitlePrefix is stamped on every auto-drafted page's title.
//
// The FAIL-CLOSED BACKSTOP, not the primary mechanism: the ancestor
// exclusion is, and this covers the backends whose parent lookup can fail —
// an outage then hides drafts rather than leaking them. A lead who moves a
// draft out of the parent without renaming it still publishes it, because
// renaming is optional and moving is the gesture that means "reviewed".
const AutoDraftTitlePrefix = "[Auto-draft] "

// DefaultLimit is how many hits the prefetch asks for.
//
// Eight, against a block that is re-sent on every round of the Plan phase:
// the cost is the hit count times the round cap, and past a handful the
// marginal hit is a page the planner will not read anyway. A knowledge base
// that cannot put something useful in eight results will not put it in
// twenty either — it will bury it.
const DefaultLimit = 8

// SnippetLimit bounds a hit's snippet, in bytes.
//
// One sentence's worth. The block exists to tell a planner WHICH page to go
// and read, not to be the page — and a longer snippet buys nothing while
// multiplying by the hit count and the round cap.
const SnippetLimit = 200

// Hit is one ranked page from a knowledge-base search.
type Hit struct {
	Title string

	// URL is a shareable human link, empty when the backend cannot build
	// one. Empty rather than a guess: a link that 404s costs a planner a
	// round to discover, where an absent one costs nothing.
	URL string

	// Container is the Confluence space key or Plane project identifier
	// the page lives in — named neutrally because the seam's whole job is
	// that a caller never learns which.
	Container string

	PageID  string
	Snippet string

	// Ancestors are the page's parent chain, outermost first. Empty on a
	// backend with no such chain, which is why the auto-draft title
	// prefix exists as a backstop.
	Ancestors []string
}

// Query is one search, in the only terms a caller may use.
type Query struct {
	// Text is plain language. Never a backend query fragment.
	Text string

	// Seat is who is searching; its credentials decide whether an
	// unscoped search is allowed at all. Nil is a search with no seat,
	// which is a search with no credential.
	Seat *org.Role

	// Org supplies the read scope, per call so a live config edit takes
	// effect without a refresh hook.
	Org *org.Organization

	// Limit caps the hits; zero takes [DefaultLimit].
	Limit int

	// ExcludeAncestors drops hits whose parent chain matches any of these
	// titles. Nil takes the default exclusion ([AutoDraftedParent]); an
	// EMPTY non-nil slice disables it, which is the distinction that lets
	// a caller deliberately search drafts and a caller who passed nothing
	// get the safe behaviour.
	ExcludeAncestors []string
}

// Excluded is the ancestor exclusion this query asks for, applying the
// default when the caller expressed none.
func (q Query) Excluded() []string {
	if q.ExcludeAncestors == nil {
		return []string{AutoDraftedParent}
	}
	return q.ExcludeAncestors
}

// Hits is the number of results to ask for.
func (q Query) Hits() int {
	if q.Limit <= 0 {
		return DefaultLimit
	}
	return q.Limit
}

// Searcher is query-time search over a seat's accessible scope.
type Searcher interface {
	// Backend names the integration answering, for logs and for the
	// operator surface that reports which one a company wired.
	Backend() string

	// CanSearch is a cheap, NO-I/O pre-gate: could a search possibly hit
	// anything at all?
	//
	// Its only job is letting the prefetch skip the auxiliary model call
	// that generates the query, when the search is a guaranteed no-op.
	// That call is the expensive half — a network round trip to an LLM
	// before any wiki is touched — so a gate that had to do I/O of its
	// own to answer would cost more than it saves.
	CanSearch(seat *org.Role, o *org.Organization) bool

	// Search returns up to Limit ranked hits.
	//
	// BEST EFFORT: it never reports an error. Every failure path is an
	// empty result, and the prefetch degrades to an empty block rather
	// than failing a turn because a wiki was slow.
	Search(ctx context.Context, q Query) []Hit
}

// Scope normalises an org-wide read scope: trimmed, uppercased, deduped,
// order preserved.
//
// ROLE-INDEPENDENT, deliberately. A unit's own space or project is its
// IDENTITY — where its webhooks route and where it writes — and letting an
// identity double as a read scope is how an agent ends up unable to read the
// page it was told to follow. An agent searches everything its own account
// can read, not just its team's corner.
//
// Order is preserved because it is the operator's, and a backend that
// renders the scope into a query puts it in front of a person eventually.
func Scope(containers []string) []string {
	if len(containers) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(containers))
	out := make([]string, 0, len(containers))
	for _, c := range containers {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Permitted reports whether a search may run at all, and with what scope.
//
// THE UNSCOPED-VS-NOTHING RULE, in one place rather than once per backend.
// An empty scope is not "search everything": it is "search everything THIS
// SEAT's own account can read", which is only meaningful when the seat has
// an account. A seat riding the shared engine credential and given an
// unscoped search reads whatever the engine can reach, which is how one seat
// sees a page its own account never could.
//
// selfAuth is the backend's own question — does this seat authenticate as
// itself here? — because only the backend knows which credential field
// carries it.
func Permitted(scope []string, selfAuth bool) (allowed bool, unscoped bool) {
	if len(scope) > 0 {
		return true, false
	}
	return selfAuth, selfAuth
}

// Excludes reports whether a hit sits under any excluded ancestor.
//
// Case-insensitive on the title, because a backend hands back whatever
// somebody typed and an exclusion that missed on capitalisation would leak
// exactly the drafts it exists to hide.
//
// The TITLE PREFIX is the fail-closed backstop, and it applies ONLY when the
// hit has no ancestor chain to judge by — a lookup that failed, or a backend
// that has no such chain at all. An outage must hide drafts rather than leak
// them.
//
// It deliberately does NOT apply to a hit whose chain came back and does not
// match: that page has been MOVED out of the draft parent, and moving is the
// gesture that means reviewed. Renaming is optional, so a prefix check that
// outranked a known-good chain would leave every published draft invisible
// until somebody noticed the title.
//
// Both halves are gated on the auto-draft parent actually being excluded: a
// caller who asked to see drafts means it.
func Excludes(h Hit, ancestors []string) bool {
	if len(ancestors) == 0 {
		return false
	}
	var draftsHidden bool
	for _, want := range ancestors {
		if strings.EqualFold(strings.TrimSpace(want), AutoDraftedParent) {
			draftsHidden = true
		}
		for _, got := range h.Ancestors {
			if strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(want)) {
				return true
			}
		}
	}
	return draftsHidden && len(h.Ancestors) == 0 &&
		strings.HasPrefix(h.Title, AutoDraftTitlePrefix)
}

// Snippet trims a page's text to one sentence's worth of plain summary.
//
// Cut at a sentence boundary when there is one inside the budget, because a
// snippet ending mid-word reads as a rendering fault rather than a summary.
func Snippet(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return ""
	}
	if len(text) <= SnippetLimit {
		return text
	}
	head := text[:SnippetLimit]
	if i := strings.LastIndexAny(head, ".!?"); i > SnippetLimit/2 {
		return head[:i+1]
	}
	if i := strings.LastIndex(head, " "); i > 0 {
		return head[:i] + "…"
	}
	return head
}
