// Package knowledge is the backend-neutral seam the turn-start "relevant
// knowledge" prefetch and the search_knowledge builtin both read the team
// knowledge base through.
//
// Exactly ONE backend per company, chosen by `knowledge.backend` — which,
// left empty, derives from whether an `integrations.confluence` block is
// configured. Two would mean an agent's answer to "what do we already know
// about this" depends on which searcher happened to be asked, and neither
// would be wrong.
//
// # What the seam hides
//
// The container vocabulary. A caller passes a seat, a plain-text query and
// ancestor exclusions — never a CQL fragment, a space key or a project list.
//
// TWO IMPLEMENTATIONS SHIP: the engine's own pages ([internal/pages.Searcher],
// the default) and Confluence. The interface belongs to the package its
// readers share rather than to either backend, so each backend is one
// implementation of it, and no caller ever learns a backend's own narrowing
// vocabulary.
//
// The engine answers with the searcher of the backend its RUNNING
// configuration names, never with whichever one it happens to hold: a node
// keeps its native searcher for as long as it runs, so a company that moved
// to Confluence, or to none, has to stop being answered from it at once. See
// the engine's Knowledge method.
//
// # The rules every backend honours
//
//   - SCOPE LIVES BEHIND THE SEAM, and is derived per call from the org, so a
//     live config edit to the read scope takes effect with no refresh hook.
//   - WHAT AN EMPTY SCOPE MEANS IS THE BACKEND'S, and the two differ. On
//     Confluence a seat can authenticate as its own user, so an empty scope
//     there is unscoped only for such a seat, bounded by that account's own
//     ACLs, and NOTHING for a seat on the shared engine credential —
//     searching the whole instance on that credential is how one seat reads
//     what its own account never could ([Permitted]). The native backend has
//     no per-seat credential and no second account to read through, so an
//     empty scope there is the whole company.
//   - BEST EFFORT. Search never fails the caller: every failure path is an
//     empty result. A turn must not die because a wiki was slow.
//   - A BUILDING INDEX SAYS SO. A backend that answers from an index of its
//     own reports whether that index has finished its first build
//     ([Searcher.Building]), because "nothing matched" and "not indexed yet"
//     send a seat to opposite places. A backend with no index answers false.
package knowledge

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/textcut"
)

// AutoDraftedParent is the page every unreviewed auto-drafted skill is
// posted under, in the unit's own container. A unit lead publishes a draft by
// moving it out.
//
// Searchers drop hits beneath it when the caller lists it in the exclusions —
// which the prefetch does by default — so no agent follows an unvetted
// LLM-proposed procedure during the review window.
const AutoDraftedParent = "Auto-Drafted Skills"

// DraftPage is an auto-drafted page a promotion writer created or found.
//
// Declared here rather than in internal/learning because BOTH sides need it —
// the promotion pass that asks for a draft, and the writer that makes one,
// which is Confluence's: the native knowledge base has no promotion writer —
// and internal/knowledge is the package neither of them would have to import
// the other to reach.
type DraftPage struct {
	ID    string
	Title string
}

// AutoDraftTitlePrefix is stamped on every auto-drafted page's title.
//
// The FAIL-CLOSED BACKSTOP, not the primary mechanism: the ancestor
// exclusion is, and this covers a hit whose chain the backend could not read
// whole ([Hit.AncestorsKnown] false) — an outage then hides drafts rather than
// leaking them. A lead who moves a draft out of the parent without renaming it
// still publishes it, wherever it lands, because renaming is optional and
// moving is the gesture that means "reviewed": [Excludes] never consults the
// prefix for a hit whose whole chain was read.
const AutoDraftTitlePrefix = "[Auto-draft] "

// DefaultLimit is how many hits a query that names no limit gets
// ([Query.Hits]).
//
// Eight: past a handful the marginal hit is a page nobody will read, and a
// knowledge base that cannot put something useful in eight results will not
// put it in twenty either — it will bury it. The seat-facing callers each name
// their own, smaller limit.
const DefaultLimit = 8

// SnippetLimit bounds a hit's snippet, in bytes.
//
// One sentence's worth. The block exists to tell an agent WHICH page to go
// and read, not to be the page — and a longer snippet buys nothing while
// multiplying by the hit count and the round cap.
const SnippetLimit = 200

// Hit is one ranked page from a knowledge-base search.
type Hit struct {
	Title string

	// URL is a shareable human link, empty when the backend cannot build
	// one. Empty rather than a guess: a link that 404s costs an agent a
	// round to discover, where an absent one costs nothing.
	URL string

	// Container is the backend container the page lives in — a native
	// container's key or a Confluence space key — named neutrally because
	// the seam's whole job is that a caller never learns which backend
	// answered.
	Container string

	PageID  string
	Snippet string

	// Ancestors are the page's parent chain, outermost first — empty for a
	// page at the top of its container, and also wherever the backend could
	// not read the chain, which is why [Hit.AncestorsKnown] travels beside
	// it.
	Ancestors []string

	// AncestorsKnown says Ancestors is the page's WHOLE chain as the backend
	// read it, so an empty one means the page sits at the top of its
	// container rather than that the chain is missing.
	//
	// ITS OWN FIELD because the chain alone cannot say it: an empty list is
	// what a top-level page has AND what a lookup that did not come back
	// leaves, and those two call for opposite answers from [Excludes] — the
	// first page is not under the draft parent, the second might be. The
	// zero value is "not known", so a backend that sets nothing gets the
	// fail-closed reading rather than the permissive one.
	AncestorsKnown bool
}

// Query is one search, in the only terms a caller may use.
type Query struct {
	// Text is plain language. Never a backend query fragment.
	Text string

	// Seat is who is searching. On Confluence its own credential, when it
	// has one, is what the search runs as, and decides whether an unscoped
	// search runs at all ([Permitted]); nil there is a search with no
	// credential of its own. The native backend reads no credential: every
	// seat reads every page.
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
	// It lets a caller skip a search that is a guaranteed no-op, and say
	// why. The prefetch is the caller it matters most to: it skips the
	// auxiliary model call that writes the query, which is the expensive
	// half — a network round trip to an LLM before any wiki is touched — so
	// a gate that had to do I/O of its own to answer would cost more than
	// it saves.
	CanSearch(seat *org.Role, o *org.Organization) bool

	// Building reports that this node's own index has not finished its
	// first build, so a search here can miss pages that exist.
	//
	// REQUIRED OF EVERY BACKEND, a live one included, which answers false:
	// an optional method is one an adapter drops without anything failing,
	// and a seat behind such an adapter on a node still indexing is told
	// the company has written nothing down. NO I/O, for [CanSearch]'s
	// reason: the prefetch asks it before it spends the query call.
	Building(ctx context.Context) bool

	// Search returns up to Limit ranked hits.
	//
	// BEST EFFORT: it never reports an error. Every failure path is an
	// empty result, so a turn never fails because a wiki was slow.
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

// Permitted reports whether a search may run at all, and with what scope, on a
// backend whose seats may authenticate as themselves.
//
// THE UNSCOPED-VS-NOTHING RULE. There, an empty scope is not "search
// everything": it is "search everything THIS SEAT's own account can read",
// which is only meaningful when the seat has an account. A seat riding the
// shared engine credential and given an unscoped search reads whatever the
// engine can reach, which is how one seat sees a page its own account never
// could.
//
// Confluence is the backend that asks it. The native backend does not: it
// has no per-seat credential, so selfAuth has no meaning there and an empty
// scope is the whole company.
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
// THE ONE RULE, which every backend calls rather than restating, so a page is
// kept or dropped the same way whichever backend a company runs.
//
// Case-insensitive on the title, because a backend hands back whatever
// somebody typed and an exclusion that missed on capitalisation would leak
// exactly the drafts it exists to hide.
//
// The TITLE PREFIX is the fail-closed backstop, and it applies ONLY when the
// backend could not read the hit's whole chain ([Hit.AncestorsKnown] false):
// an answer that came back without the chain (a Confluence search whose
// `ancestors` expand is missing), or a chain that ran into a parent the
// backend no longer holds (natively). An outage must hide drafts rather than
// leak them.
//
// It deliberately does NOT apply to a hit whose whole chain was read and
// carries no excluded title — an empty chain included, which is a page at the
// top of its container. That page is not under the draft parent; if it was
// once, it has been MOVED out, and moving is the gesture that means reviewed.
// Renaming is optional, so a prefix check that outranked a known chain would
// leave every published draft invisible until somebody noticed the title.
//
// The prefix applies only while the caller excludes [AutoDraftedParent]: a
// caller who asked to see drafts means it, and one excluding some other page
// asked a different question.
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
	return draftsHidden && !h.AncestorsKnown &&
		strings.HasPrefix(h.Title, AutoDraftTitlePrefix)
}

// Snippet trims a page's text to `limit` bytes of plain summary.
//
// THE ONE PLACE a knowledge excerpt is shortened, and one of the few cuts in
// this engine that is correct: the block it feeds tells an agent WHICH page
// to open and says so in as many words, and the page is re-readable in full
// through the seat's own tools. A pointer that says it is a pointer is not the
// same thing as content that was quietly halved.
//
// Three properties:
//
//   - ALWAYS MARKED. Every cut ends in an ellipsis, including the no-space
//     fallback. An unmarked cut is indistinguishable from a page that really
//     does end there, which is how an agent concludes a runbook has no step 4.
//   - NEVER THROUGH A RUNE. A byte slice splits whatever multi-byte character
//     straddles the boundary and yields invalid UTF-8, which reaches a model as
//     a replacement character — a bug that appears the first time a page is not
//     ASCII.
//   - LENGTH ONLY. Nothing cuts at the first newline or sentence before the
//     limit applies, which would decapitate a page to its opening sentence even
//     when asked for an unlimited snippet.
//
// A limit of zero or less is unbounded.
func Snippet(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" || limit <= 0 || len(text) <= limit {
		return text
	}
	// THE BUDGET, backed up to a rune boundary, so every cut below is on
	// one: the two searches land on an ASCII byte, which is never inside a
	// multi-byte rune, and the fallback is the budget itself.
	head := textcut.Bytes(text, limit)
	// A sentence boundary inside the second half of the budget reads as a
	// summary rather than a cut, so it is preferred — and still marked,
	// because the page continues past it either way.
	if i := strings.LastIndexAny(head, ".!?"); i > limit/2 {
		return head[:i+1] + " …"
	}
	if i := strings.LastIndex(head, " "); i > 0 {
		return head[:i] + "…"
	}
	// No space at all — a URL, a CJK run.
	return head + "…"
}
