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
//   - BEST EFFORT, AND A FAILURE SAYS SO. Search never fails the caller:
//     every failure path is an empty answer marked [Answer.Failed]. A turn
//     must not die because a wiki was slow, and a seat must not read a
//     search that failed as one that matched nothing.
//   - A BUILDING INDEX SAYS SO. A backend that answers from an index of its
//     own reports whether that index has finished its first build
//     ([Searcher.Building]), because "nothing matched" and "not indexed yet"
//     send a seat to opposite places. A backend with no index answers false.
//   - A PARTIAL ANSWER SAYS SO. An answer ranked over part of what it was
//     asked about carries [Answer.Partial], and every reader renders it:
//     a short list with nothing beside it reads as everything that matched.
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

// MaxQueryBytes bounds [Query.Text], and EVERY CALLER REFUSES a longer one,
// naming the limit — none cuts it.
//
// A cut query is not a shorter query, it is a DIFFERENT one: the hits that
// come back are real pages, ranked, about the half of the text that survived,
// and whoever asked has no way to tell them from hits for what they wrote.
// Refused, it costs a model one round, and a refusal naming the field is the
// one failure a model reliably fixes.
//
// FOUR HUNDRED BYTES: a long sentence, or a dozen keywords — several times
// what a model is asked for (the tool's `query` and the turn-start query both
// ask for 2-8 keywords), so a model doing what it was asked never meets it,
// and one that sent a task or a thread instead is told to send keywords.
const MaxQueryBytes = 400

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

// Answer is one search's hits, and what they were ranked from.
type Answer struct {
	Hits []Hit

	// Partial is what the ranking behind these hits is missing, or nil for
	// a whole answer. A backend that answers from one place in one call —
	// a live vendor search — is whole whenever it answers at all.
	Partial *Partial

	// Failed is a search that did not complete. Its hits are empty and say
	// nothing about what the knowledge base holds; the backend's own log
	// line says why.
	//
	// A MARK RATHER THAN AN ERROR, for the seam's best-effort rule: a
	// caller still gets an answer it can render, and what it must not
	// render is "nothing matches" — which sends a seat to try other words
	// against a search that is not running, or to write a page that
	// already exists.
	Failed bool
}

// Partial is what a ranked answer is missing.
//
// THE FOUR FACTS A DIVIDED SEARCH COUNTS, so every reader can say how much was
// searched rather than only that something was not. The hits in a partial
// answer are real; what a reader must not conclude from them is that a page
// they do not name does not exist.
type Partial struct {
	// BucketsAnswered and BucketsMissing partition the corpus's search
	// buckets: the answer ranked the documents in the first and none of
	// those in the second.
	BucketsAnswered int `json:"buckets_answered"`
	BucketsMissing  int `json:"buckets_missing"`

	// AbsentNodes names the fleet nodes whose share went unscanned — one
	// that did not answer in time, or answered that its own index is still
	// building — so an operator has somewhere to look.
	AbsentNodes []string `json:"absent_nodes"`

	// SemanticSkipped says the half of a hybrid search that matches by
	// meaning did not run over some or all of what was searched, so a page
	// that says the same thing in other words can be missing.
	SemanticSkipped bool `json:"semantic_skipped"`
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

	// Prefetch marks the search a turn makes for its own context before its
	// first round, as opposed to somebody's deliberate search: a person's
	// on the dashboard, an operator's assistant's, or a seat's own
	// `search_knowledge`. The zero value is the deliberate search, which is
	// every caller but the prefetch.
	//
	// It changes nothing about the answer. The native backend files each
	// scan's duration under it, because a deliberate search and the
	// prefetch are held to different latency figures, and one series
	// holding both would describe neither.
	Prefetch bool
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

	// Search returns up to Limit ranked hits, and what the ranking behind
	// them is missing.
	//
	// BEST EFFORT: it never reports an error. Every failure path is an
	// empty answer marked [Answer.Failed], so a turn never fails because a
	// wiki was slow.
	Search(ctx context.Context, q Query) Answer
}

// Unsearchable is why a knowledge search cannot run, as a closed set a caller
// branches on rather than string-matching a sentence.
type Unsearchable string

// The states. The zero value is none of them: a search that can run.
const (
	// NoBackend is a company that runs no knowledge base
	// (`knowledge.backend: none`).
	NoBackend Unsearchable = "no_backend"

	// NotServed is a company that runs a knowledge base this node is not
	// serving: a Confluence connection that did not build or whose org
	// credential did not resolve, or the engine's own knowledge base on a
	// node that did not start with it.
	NotServed Unsearchable = "not_served"

	// NoScope is a backend this node serves that has nothing this search
	// may read: no `knowledge.scope` is declared, and the search has no
	// credential of its own to search unscoped with ([Permitted]).
	NoScope Unsearchable = "no_scope"
)

// Valid reports whether u is a state this build names.
func (u Unsearchable) Valid() bool {
	switch u {
	case NoBackend, NotServed, NoScope:
		return true
	}
	return false
}

// Refusal says a knowledge search cannot run: which of the [Unsearchable]
// states it is in, and a sentence for a person naming what to change. The
// zero value refuses nothing.
//
// THE STATE AND THE SENTENCE TRAVEL TOGETHER, because the three states send a
// reader to three different places — a backend to choose, a connection to
// repair, a scope to declare — and a reply that named the wrong one sends
// them to check a setting that is already right.
type Refusal struct {
	State  Unsearchable
	Detail string
}

// Refused reports a search that cannot run.
func (r Refusal) Refused() bool { return r.State != "" }

// Reason is the refusal's sentence for a person: its own detail where the
// refuser wrote one, else what its state means.
func (r Refusal) Reason() string {
	if detail := strings.TrimSpace(r.Detail); detail != "" {
		return detail
	}
	switch r.State {
	case NoBackend:
		return "the company runs no knowledge base (`knowledge.backend: none`)"
	case NotServed:
		return "the company runs a knowledge base this node is not serving"
	case NoScope:
		return "no read scope (`knowledge.scope`) is declared, and this search " +
			"has no credential of its own to search unscoped with"
	}
	return ""
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
