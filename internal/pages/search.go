package pages

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
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
// Every failure path is an empty answer marked [knowledge.Answer.Failed], and
// a log line saying why. A turn must not die because an index was rebuilding
// — but "not indexed yet" and "nothing matched" are different facts, and [Searcher.Building] is how a caller tells
// them apart so a seat on a fresh node is not told the company has written
// nothing down.
//
// # Built only by [NewSearcher], which refuses a missing part
//
// A Searcher has no meaningful zero value, and neither half of its options
// may be absent: without the index there is nothing to search, and without
// the store no hit carries its parent chain, so the ancestor exclusion keeps
// out only the drafts whose titles carry the prefix. Both degrade without an
// error anywhere, so the constructor refuses them instead.
type Searcher struct {
	index *search.Indexer

	// fan is the bucket fan-out this search is answered through, and it
	// is ALWAYS present — a single node is the same coordinator with one
	// participant taking every bucket. One path rather than two, because
	// a solo search and a fanned-out one that took different code would
	// be two rankings to keep in step.
	fan *search.FanOut

	// skills names the tool-skills container, whose pages a search never
	// returns: they are machinery, and a seat told to read one would
	// follow it as an instruction.
	//
	// READ PER CALL, never captured. `knowledge.skills_container` is Tier
	// B, so an apply can move it — and a searcher holding the old key
	// would keep hiding a container that is now ordinary knowledge while
	// returning every page of the one that is now machinery, for as long
	// as the process ran.
	skills func() string

	// db holds the page rows a hit's parent chain is read from, which is
	// what [knowledge.Query.ExcludeAncestors] is judged against.
	db *store.DB
}

// SearcherOptions configure a searcher.
type SearcherOptions struct {
	// Index is this node's lexical index. REQUIRED.
	Index *search.Indexer

	// Node is this node's id, which is its name in the fan-out's
	// assignment table. Empty names it [soloNode], which is only right
	// where it has no peers and the name never travels.
	Node string

	// Peers and Roster are the fleet half of the fan-out; what each does
	// when nil is [search.FanOut]'s to say.
	Peers  search.Peers
	Roster func(ctx context.Context) ([]string, error)

	// Report is told what every answer covered and what it cost. Nil
	// counts nothing.
	Report func(search.Answer, time.Duration)

	// Enter is called when a search begins and its return when it ends,
	// so scans in flight can be counted. Nil counts nothing.
	Enter func() func()

	// Space is the embedding space the company's pages are embedded in,
	// asked once per search. Nil, or false, answers on words alone — a
	// company whose search has no semantic half; what each does is
	// [search.FanOut.Space]'s to say.
	Space func() (search.QuerySpace, bool)

	// SkillsContainer names the reserved tool-skills container, excluded
	// from every result. A FUNCTION because the value is live config; nil,
	// or one returning empty, excludes nothing — which is the company that
	// has turned tool skills off.
	SkillsContainer func() string

	// DB is the node's store, whose replicated estate holds `pages_heads`.
	// REQUIRED. Each hit's parent chain is read from it, as titles
	// outermost first, and [knowledge.Query.ExcludeAncestors] drops a hit
	// whose chain carries an excluded title — which is how a page under
	// [knowledge.AutoDraftedParent] stays out of every seat's search. The
	// same read says whether the page is still there and still published.
	DB *store.DB
}

// NewSearcher builds the native knowledge searcher, refusing options that
// lack its index or its store — see [Searcher] for what each would silently
// cost.
func NewSearcher(opts SearcherOptions) (*Searcher, error) {
	if opts.Index == nil {
		return nil, errors.New("pages: a knowledge searcher needs this node's " +
			"lexical index (SearcherOptions.Index) — without one it can search " +
			"nothing")
	}
	if opts.DB == nil {
		return nil, errors.New("pages: a knowledge searcher needs the node's " +
			"store (SearcherOptions.DB) — without it no hit carries its parent " +
			"chain, and a page under an excluded ancestor is returned unless its " +
			"title carries the auto-draft prefix")
	}
	return &Searcher{
		index: opts.Index, skills: opts.SkillsContainer, db: opts.DB,
		fan: &search.FanOut{
			Self:   cmp.Or(opts.Node, soloNode),
			Local:  search.NodeScanner{Index: opts.Index},
			Peers:  opts.Peers,
			Roster: opts.Roster,
			Corpus: opts.Index.Corpus,
			Report: opts.Report,
			Enter:  opts.Enter,
			Space:  opts.Space,
		},
	}, nil
}

// soloNode is what a node with no id calls itself in its own assignment table.
//
// It never travels: a fan-out with no peers scatters nothing, and the name is
// only ever compared against a table this same coordinator wrote. A node that
// HAS an id passes it, because then the name is what a peer matches its own
// row against.
const soloNode = "self"

var _ knowledge.Searcher = (*Searcher)(nil)

// Backend names the integration answering.
func (s *Searcher) Backend() string { return "native" }

// CanSearch is the cheap, no-I/O pre-gate.
//
// ALWAYS TRUE. Its only job is letting the prefetch skip the auxiliary model
// call when a search is a guaranteed no-op, and here none is: every seat can
// read every page — there is no per-seat credential to be missing, which is
// the condition that makes Confluence's gate answer false — and [NewSearcher]
// refuses a searcher with no index. An index that has not finished its first
// build is a different answer, and [Searcher.Building] gives it.
func (s *Searcher) CanSearch(*org.Role, *org.Organization) bool { return true }

// Building reports whether this node's index has yet to finish its first build
// of the page corpus.
//
// SEPARATE FROM CanSearch, because they answer different questions and a
// caller acts on them differently: CanSearch gates the expensive query
// generation, while this is what turns an empty block into "the index for
// this node is still building" rather than "the company has written nothing
// down". A seat on a freshly joined node would otherwise be told the second
// for the whole first index build.
//
// THE PAGE CORPUS ONLY, because that is the only source [Searcher.Search]
// asks for. The index can also cover the work items, and each corpus finishes
// its first build on its own — so asked about the whole index, a node whose
// pages are built would answer "building" for as long as its work items are
// not, and the prefetch, which asks this before it searches, would search none
// of the pages it could.
//
// It answers from the indexer's own walk and does no I/O, because the
// prefetch asks it at the start of a turn, before it searches.
func (s *Searcher) Building(_ context.Context) bool {
	return !s.index.ReadyFor(string(search.SourcePage))
}

// Search returns up to Limit ranked hits, and what the ranking behind them is
// missing. Best effort: every failure path is an empty answer marked failed.
func (s *Searcher) Search(ctx context.Context, q knowledge.Query) knowledge.Answer {
	if strings.TrimSpace(q.Text) == "" {
		return knowledge.Answer{}
	}
	scope := knowledge.Scope(scopeOf(q.Org))
	answer, err := s.fan.Search(ctx, search.FanQuery{
		Text:       q.Text,
		Containers: scope,
		Sources:    []string{string(search.SourcePage)},
		// OVER-FETCHED, because the exclusions below drop hits after
		// ranking: asking for exactly the limit and then removing three
		// skill pages would return five results where eight were
		// available.
		Limit:    q.Hits() * SearchOverfetch,
		Prefetch: q.Prefetch,
	})
	if err != nil {
		log.WarnContext(ctx, "pages_search_failed", "error", err.Error(),
			"detail", "the knowledge block degrades to empty; a turn must not "+
				"die because an index was slow")
		return knowledge.Answer{Failed: true}
	}
	// CARRIED, NEVER REFUSED. The block is best effort by contract, and an
	// answer over part of the corpus is better than none — but a short
	// result set is indistinguishable from a short corpus, so the answer
	// says what it is missing and every reader of the seam renders it.
	partial := partialOf(answer)
	if answer.Partial() {
		log.WarnContext(ctx, "pages_search_scoped",
			"buckets_answered", answer.BucketsAnswered,
			"buckets_missing", answer.BucketsMissing,
			"absent", strings.Join(answer.Absent, ","),
			"detail", "the answer carries what it did not search")
	}
	hits, err := s.index.Hydrate(ctx, answer.Hits, q.Text)
	if err != nil {
		log.WarnContext(ctx, "pages_search_failed", "error", err.Error(),
			"detail", "the fused answer could not be read back")
		return knowledge.Answer{Failed: true}
	}
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.ID)
	}
	// AN UNREADABLE CHAIN IS AN EMPTY ANSWER rather than hits with no
	// chain: the chain is what keeps a draft under the excluded parent out,
	// and an answer served without it is the one that would carry that
	// draft into a seat's prompt.
	chains, err := s.ancestry(ctx, ids)
	if err != nil {
		log.WarnContext(ctx, "pages_search_failed", "error", err.Error(),
			"detail", "the hits' parent chains could not be read, so the "+
				"ancestor exclusion could not be judged")
		return knowledge.Answer{Failed: true}
	}

	excluded := q.Excluded()
	out := make([]knowledge.Hit, 0, q.Hits())
	for _, hit := range hits {
		if s.isExcluded(hit.Container) {
			continue
		}
		chain, held := chains[hit.ID]
		if !held || !chain.published {
			// INDEXED AND GONE, or no longer published. The index is
			// behind this node's own rows by design and drops such a
			// page on its next orphan pass; until then the chain read,
			// which is the later of the two, is what knows. A purged
			// page is one nobody can open, and a draft or a trashed one
			// is not what a knowledge search answers with — the rule
			// [search.PageSource] indexes by.
			continue
		}
		found := knowledge.Hit{
			Title:          hit.Title,
			Container:      hit.Container,
			PageID:         hit.ID,
			Snippet:        hit.Snippet,
			Ancestors:      chain.titles,
			AncestorsKnown: chain.whole,
		}
		if knowledge.Excludes(found, excluded) {
			continue
		}
		out = append(out, found)
		if len(out) == q.Hits() {
			break
		}
	}
	return knowledge.Answer{Hits: out, Partial: partial}
}

// partialOf is what a fan-out answer is missing, on the seam's terms, or nil
// for a whole one.
func partialOf(a search.Answer) *knowledge.Partial {
	if a.Whole() {
		return nil
	}
	return &knowledge.Partial{
		BucketsAnswered: a.BucketsAnswered,
		BucketsMissing:  a.BucketsMissing,
		// NEVER NIL, so the wire reads `[]` rather than `null` for an
		// answer that is missing only its semantic half.
		AbsentNodes:     append([]string{}, a.Absent...),
		SemanticSkipped: a.SemanticSkipped,
	}
}

// hitChain is what one hit's own row said when its parent chain was read.
type hitChain struct {
	// titles is the parent chain, outermost first — empty for a page at the
	// top of its container.
	titles []string

	// whole says the walk ended where the chain does: at a page with no
	// parent, or back at a page it had already visited. False when it ran
	// into a parent this node holds no page for, which leaves everything
	// above that point unknown — see [knowledge.Hit.AncestorsKnown].
	whole bool

	// published is the page's status at the read, which can be later than
	// the index's.
	published bool
}

// ancestry is each hit's parent chain as titles, outermost first, read in one
// transaction by the same walk a page's breadcrumb is ([parentChains]). A page
// this node no longer holds has no entry.
func (s *Searcher) ancestry(ctx context.Context, ids []string) (map[string]hitChain, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make(map[string]hitChain, len(ids))
	err := s.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		chains := newParentChains(tx)
		for _, id := range ids {
			page, held, err := chains.page(ctx, id)
			if err != nil {
				return err
			}
			if !held {
				continue
			}
			above, looped, err := chains.above(ctx, id, page.ParentID)
			if err != nil {
				return err
			}
			// THE TOP OF THE WALK is the parent of the outermost page it
			// reached — the page's own parent when it reached none. Empty
			// there is the top of the container; anything else is a
			// parent the walk could not read, unless the walk stopped on
			// a loop, which names every page it can.
			top := page.ParentID
			if len(above) > 0 {
				top = above[0].ParentID
			}
			chain := hitChain{
				whole:     looped || top == "",
				published: page.Status == StatusPublished,
			}
			for _, ancestor := range above {
				chain.titles = append(chain.titles, ancestor.Title)
			}
			out[id] = chain
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("pages: read the parent chains of %d hits: %w",
			len(ids), err)
	}
	return out, nil
}

// SearchOverfetch is how many times the limit is asked for before the drops
// that follow the ranking.
//
// Three, a judgement. [Searcher.Search] drops four kinds of ranked hit after
// the fan-out has answered: a key this node's index holds no row for, which
// [search.Indexer.Hydrate] skips; a page in the tool-skills container; a page
// this node's rows no longer hold as published — indexed and gone, or trashed
// or unpublished since; and a page under an excluded ancestor. Together they
// leave an answer SHORT of the limit only when more than two in three of the
// hits the fan-out ranked are dropped — and short silently, because none of
// these drops is something [knowledge.Partial] counts. The first drop is the
// one that can be large, on a node whose index is still on its first build,
// and that node says it is building ([Searcher.Building]).
//
// EXPORTED BECAUSE IT IS HALF OF AN INVARIANT NOTHING ELSE CAN SEE: it
// multiplies the caller's limit into what the fan-out is asked for, and a
// fan-out refuses to be asked for more than
// [github.com/crewlet/crewlet/internal/search.FuseN]. internal/engine's
// TestNoSearchCallerAsksForMoreThanAFanOutCanAnswer holds every real caller's
// limit times this under that ceiling.
const SearchOverfetch = 3

// isExcluded reports a container a search never returns.
func (s *Searcher) isExcluded(container string) bool {
	if s.skills == nil {
		return false
	}
	key := strings.TrimSpace(s.skills())
	return key != "" && strings.EqualFold(container, key)
}

// scopeOf is the org-wide read scope, or nil for unscoped.
func scopeOf(o *org.Organization) []string {
	if o == nil {
		return nil
	}
	return o.KnowledgeScope
}
