package pages

import (
	"cmp"
	"context"
	"fmt"
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

	// Enter is called when a search begins and its return when it ends,
	// so scans in flight can be counted. Nil counts nothing.
	Enter func() func()

	// Vectors computes a query's embedding for the semantic ranking. NIL
	// IS A SEARCH WITH NO MEANING HALF — every hybrid answer then says it
	// served keyword, and every semantic one says why it served nothing.
	// Shared with the item search on the same node, so a string either
	// one embedded is a cache hit for the other.
	Vectors *search.QueryVectors

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
			Self:    cmp.Or(opts.Node, soloNode),
			Local:   search.NodeScanner{Index: opts.Index},
			Peers:   opts.Peers,
			Roster:  opts.Roster,
			Corpus:  opts.Index.Corpus,
			Report:  opts.Report,
			Enter:   opts.Enter,
			Vectors: opts.Vectors,
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

// Backend is what this knowledge base calls itself wherever a reader has to
// know which backend a page id is an address in — the searcher's own answer,
// and every record of a seat reading one of these pages.
const Backend = "native"

// Backend names the integration answering.
func (s *Searcher) Backend() string { return Backend }

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
// It answers from the indexer's own walk and does no I/O: it is asked on
// every empty search, which is a path every turn's knowledge block takes.
func (s *Searcher) Building(_ context.Context) bool {
	return s.index != nil && !s.index.Ready()
}

// Search returns up to Limit ranked hits, and what the search did. Best
// effort: every failure path is an empty result — and an outcome that says so.
//
// WHAT IT COVERED RIDES IN THE ANSWER. It used to be a log line and nothing
// else, so a caller holding a result over two thirds of the corpus had no way
// to say so and a screen drew it as the company's whole answer. Every caller
// now holds [knowledge.Outcome.Coverage] and decides what to say; the log line
// went with the reason for it.
func (s *Searcher) Search(ctx context.Context, q knowledge.Query) knowledge.Result {
	answer, err := s.Answer(ctx, q)
	if err != nil {
		log.WarnContext(ctx, "pages_search_failed", "error", err.Error(),
			"detail", "the knowledge block says the search did not run; a turn "+
				"must not die because an index was slow")
		return s.failed(err)
	}
	return answer
}

// Answer is [Searcher.Search] with its failure kept: an ERROR rather than the
// empty result Search degrades to, for a caller that answers somebody else —
// the estate router, which carries a search between nodes and has to tell the
// asking node "the knowledge base could not be searched" rather than hand it
// an empty answer that reads as "nothing matched". The two send a seat to
// different places: one to try again, the other to write down what it was
// looking for, duplicating a page that exists.
//
// THE FUSED ORDER IS WALKED PAST WHAT CANNOT BE SHOWN, rather than cut first
// and filtered after: a tool-skill page among the leaders, or one the index
// named and could not read back, costs the answer that page and never a place,
// so a company with many skills is not handed a short list. That is what the
// over-fetch factor this replaced only approximated — three times the limit,
// cut, then filtered — and a company whose skills filled two thirds of the cut
// got the short list anyway.
func (s *Searcher) Answer(ctx context.Context, q knowledge.Query) (knowledge.Result, error) {
	if s.index == nil || strings.TrimSpace(q.Text) == "" {
		// THE PROBE: nothing runs and nothing is read, but the answer
		// still says which modes this node would serve as asked and how
		// the asked one would degrade — a configuration fact, so a screen
		// can offer the modes honestly before anybody has typed.
		return knowledge.Result{Outcome: knowledge.Outcome{
			Modes:    s.fan.Modes(),
			Degraded: s.fan.ProbeDegradation(q.Mode),
			Coverage: knowledge.Coverage{Nodes: []knowledge.NodeCoverage{}},
		}}, nil
	}
	scope := knowledge.Scope(scopeOf(q.Org))
	answer, err := s.fan.Search(ctx, search.FanQuery{
		Text:       q.Text,
		Containers: scope,
		Sources:    []string{string(search.SourcePage)},
		Mode:       q.Mode,
		// NOT THE CALLER'S LIMIT: what is walked below is the candidates,
		// each method's top FuseN whatever the limit, and the fused cut
		// the fan-out also makes is not read here.
		Limit: search.FuseN,
	})
	if err != nil {
		return knowledge.Result{}, err
	}
	out := knowledge.Result{Hits: make([]knowledge.Hit, 0, q.Hits()), Outcome: answer.Outcome()}
	keys := answer.Candidates.Keys()
	if len(keys) == 0 {
		return out, nil
	}
	hits, err := s.index.Hydrate(ctx, keys, q.Text)
	if err != nil {
		return knowledge.Result{}, fmt.Errorf("pages: read back the search's candidates: %w", err)
	}
	shown := make(map[string]knowledge.Hit, len(hits))
	for _, hit := range hits {
		if s.isExcluded(hit.Container) {
			continue
		}
		shown[hit.Key] = knowledge.Hit{
			Title:     hit.Title,
			Container: hit.Container,
			PageID:    hit.ID,
			Snippet:   hit.Snippet,
			Backend:   Backend,
		}
	}
	for _, key := range answer.Candidates.Fused() {
		hit, ok := shown[key]
		if !ok {
			continue
		}
		out.Hits = append(out.Hits, hit)
		if len(out.Hits) == q.Hits() {
			break
		}
	}
	return out, nil
}

// failed is the answer to a search this node could not run at all: no hits,
// nothing served, and THIS node named as the participant that did not cover
// its range — the coordinator always scans, so a failure here is its own.
func (s *Searcher) failed(err error) knowledge.Result {
	return knowledge.Result{Outcome: knowledge.Outcome{
		Modes: s.fan.Modes(),
		Coverage: knowledge.Coverage{
			Nodes: []knowledge.NodeCoverage{{
				ID: s.fan.Self, Error: "the search could not run here: " + err.Error(),
			}},
			BucketsMissing: search.SearchShards,
		},
	}}
}

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
