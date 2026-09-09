package pages

import (
	"cmp"
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/search"
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
}

// SearcherOptions configure a searcher.
type SearcherOptions struct {
	Index *search.Indexer

	// Node is this node's id, which is its name in the fan-out's
	// assignment table. Empty on an embedded engine and in every test,
	// where there is one participant and its name never travels.
	Node string

	// Peers and Roster are the fleet half of the fan-out. BOTH NIL IS THE
	// ORDINARY DEPLOYMENT — one node, an embedded engine, every test —
	// and it means this node takes every bucket, exactly as it did before
	// the fan-out existed.
	Peers  search.Peers
	Roster func(ctx context.Context) ([]string, error)

	// Report is told what every answer covered and what it cost. Nil
	// counts nothing, which is what an embedded engine with no recorder
	// gets.
	Report func(search.Answer, time.Duration)

	// SkillsContainer names the reserved tool-skills container, excluded
	// from every result. A FUNCTION because the value is live config; nil,
	// or one returning empty, excludes nothing — which is the company that
	// has turned tool skills off.
	SkillsContainer func() string
}

// NewSearcher builds the native knowledge searcher.
func NewSearcher(opts SearcherOptions) *Searcher {
	s := &Searcher{index: opts.Index, skills: opts.SkillsContainer}
	if opts.Index != nil {
		s.fan = &search.FanOut{
			Self:   cmp.Or(opts.Node, soloNode),
			Local:  search.NodeScanner{Index: opts.Index},
			Peers:  opts.Peers,
			Roster: opts.Roster,
			Corpus: opts.Index.Corpus,
			Report: opts.Report,
		}
	}
	return s
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
	answer, err := s.fan.Search(ctx, search.FanQuery{
		Text:       q.Text,
		Containers: scope,
		Sources:    []string{string(search.SourcePage)},
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
	if answer.Partial() {
		// LOGGED, NEVER REFUSED. The block is best effort by contract,
		// and an answer over part of the corpus is better than none —
		// but a short result set is indistinguishable from a short
		// corpus, so the one place that knows says so.
		log.WarnContext(ctx, "pages_search_scoped",
			"buckets_answered", answer.BucketsAnswered,
			"buckets_missing", answer.BucketsMissing,
			"absent", strings.Join(answer.Absent, ","),
			"detail", "the answer was complete for what was searched and "+
				"silent about what was not")
	}
	hits, err := s.index.Hydrate(ctx, answer.Hits, q.Text)
	if err != nil {
		log.WarnContext(ctx, "pages_search_failed", "error", err.Error(),
			"detail", "the fused answer could not be read back")
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
