package codesearch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Estate is the code host's rows and its keyed git walk as a search reads
// them: the asker's repositories (code.ReadRepos and code.ReadRepo, held to
// the asker's grant by the node that serves them) and ONE repository's search
// (code.GitSearch through the walk, which asks the repository's git nodes in
// order). Declared here by its consumer; estate.CodeSeam answers both.
type Estate interface {
	Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error)
	GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error)
}

// Placement is every repository's git order, judged on one reading of who
// serves git (estate.Router.GitOrders): its first node answers for it, its
// second is its runner-up.
type Placement interface {
	GitOrders(repoIDs []string) (map[string][]string, error)
}

// AskFunc scatters one sealed request to every git node and hands each reply
// to each as it arrives, until ctx ends or each answers false
// (queue.EventQueue.AskEach on topics.CodeSearch, which the engine binds). A
// node with no broker hands it to its own answerer, its fleet being itself.
type AskFunc func(ctx context.Context, request []byte, each func(reply []byte) bool) error

// Query is a code search as a seat's tool or a person's route asks it.
type Query struct {
	// Search is the search: its text, mode and case, and its repo:, path:
	// and ref: filters — codeindex's one grammar.
	Search codeindex.Query
	// Limit is the most hits answered, 1 to code.MaxSearchHits; 0 is
	// code.MaxSearchHits.
	Limit int
	// Asker is who searches, REQUIRED; CeilingSeat and Turn are a seat's
	// read's as code.GitQuery's are. Every node judges each repository's
	// grant for them.
	Asker       code.Principal
	CeilingSeat string
	Turn        string
	// Rows is the level the asker's repositories are listed at — a seat's
	// and a person's linearizable — and Git the floor each copy searched is
	// brought to, as a read by object id's (code.GitQuery.Fresh).
	Rows statelog.Freshness
	Git  statelog.Freshness
}

// Answer is a search's lines, in the order of their repositories' names,
// then their paths' bytes, then their numbers, and what was and was not
// searched.
type Answer struct {
	Hits []Hit `json:"hits"`
	// Tips is the commit each repository with a hit was searched at, by its
	// name: what a hit's file is read at.
	Tips     map[string]string `json:"tips,omitempty"`
	Coverage Coverage          `json:"coverage"`
}

// Coverage is what a search covered. Every repository the search names — the
// repositories its repo: filters name, or, unfiltered, every `search:
// indexed` repository the asker may read — is in exactly ONE of Searched,
// Building, NotAnswering, NotPermitted, Stale and NoRef; GrepNotNamed is the
// repositories an unfiltered search leaves out, and NotIndexed names those
// of Searched that were read with git grep.
type Coverage struct {
	// Searched is how many repositories answered their lines.
	Searched int `json:"searched"`
	// NotIndexed is the `search: indexed` repositories among them read with
	// git grep, no index of their git nodes answering for the commit: the
	// same lines, read more slowly.
	NotIndexed RepoSet `json:"not_indexed"`
	// Building is the repositories left unanswered because the last git node
	// asked for them was building their index: the first index holder, where
	// their order named no runner-up to ask. A runner-up that was asked and
	// did not answer is named in NotAnswering instead — the coverage names
	// what the LAST node asked for a repository said of it.
	Building RepoSet `json:"building"`
	// NotAnswering is every git node that did not answer for a repository it
	// was asked for — silent, busy, failing or holding no key the request is
	// signed under — with the repositories left unanswered for it.
	NotAnswering []NodeGap `json:"not_answering"`
	// NotPermitted is the repositories a git node judged the asker's grant
	// not to cover: the chart moved since the asker's listing.
	NotPermitted RepoSet `json:"not_permitted"`
	// GrepNotNamed is the `search: grep` repositories the asker may read
	// that an unfiltered search leaves out: each is searched only when a
	// repo: filter names it, at most MaxGrepRepos at once.
	GrepNotNamed RepoSet `json:"grep_not_named"`
	// Stale is the repositories left unanswered because the last git node
	// asked for them was behind — its copy still syncing to the log, or its
	// chart behind the company's configuration: the runner-up, or the first
	// node where the order named no runner-up.
	Stale RepoSet `json:"stale"`
	// NoRef is the repositories holding no ref or commit the search's ref:
	// names: nothing to search in them at it.
	NoRef RepoSet `json:"no_ref"`
	// SkippedFiles is the files the searched repositories passed over for
	// their size or as binaries (code.SearchAnswer.SkippedFiles).
	SkippedFiles int `json:"skipped_files"`
	// Truncated says lines may be missing: a repository's search stopped at
	// its limit or its budget, or the merge cut the lines past Limit.
	Truncated bool `json:"truncated"`
}

// MaxCoverageNames is the most repository names a [RepoSet] lists: its count
// is whole. 100 names of at most 64 bytes is under 7 KiB, so all seven sets
// together stay under an eighth of a seat's tool answer (64 KiB) beside the
// hits.
const MaxCoverageNames = 100

// RepoSet is a set of repositories: how many, and the first
// [MaxCoverageNames] of their names in order.
type RepoSet struct {
	Count int      `json:"count"`
	Names []string `json:"names"`
}

// NodeGap is one git node that did not answer, why, and the repositories left
// unanswered for it.
type NodeGap struct {
	Node  string  `json:"node"`
	Why   string  `json:"why"`
	Repos RepoSet `json:"repos"`
}

// MaxGrepRepos is the most `search: grep` repositories one search reads.
// JUDGMENT: a monorepo's git grep costs one to two CPU-seconds of a git
// node's read lane, so eight is a search that can hold a node's seat share
// for most of its budget — more is a scan of the company, which the index is
// for.
const MaxGrepRepos = 8

// RoundBudget is how long one round of the scatter waits for its nodes:
// what one node's read of a repository may take — its wait for a unit of the
// seat git-work share (code.GitShareWait, 2 s), then the search's own budget
// (code.SearchBudget, 5 s) — and a reply's margin each way
// (code.ReplyMargin), so a node whose read ran its whole budget answers
// while the asker listens. A SUM OF WHOLE BUDGETS (M3-O6), 9 s; a round ends
// sooner, as soon as every node it asked has answered, and a search takes two
// rounds only where a node did not answer the first.
const RoundBudget = code.GitShareWait + code.SearchBudget + 2*code.ReplyMargin

// Searcher answers code searches over the fleet's git nodes.
type Searcher struct {
	estate    Estate
	placement Placement
	ask       AskFunc
	wire      *Wire
	metrics   *metrics.Recorder
	log       *slog.Logger
	now       func() time.Time
}

// Options builds a [Searcher].
type Options struct {
	Estate    Estate
	Placement Placement
	Ask       AskFunc
	Wire      *Wire
	// Metrics records crewlet.code.search.*; nil records nothing.
	Metrics *metrics.Recorder
	Log     *slog.Logger
}

// New is a searcher over o.
func New(o Options) (*Searcher, error) {
	if o.Estate == nil || o.Placement == nil || o.Ask == nil || o.Wire == nil {
		return nil, errors.New("codesearch: a searcher needs the code host's estate, the git placement, " +
			"a scatter and its envelopes")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Searcher{estate: o.Estate, placement: o.Placement, ask: o.Ask, wire: o.Wire, metrics: o.Metrics,
		log: o.Log, now: time.Now}, nil
}

// Search answers q.
//
// A query this package would refuse is refused before anything is asked —
// its grammar (codeindex.Query.Validate: past codeindex.MaxQueryBytes, the
// knowledge search's 400 bytes, refused, never cut), its limit, its asker, a
// repo: naming a repository the asker may not read, and more than
// MaxGrepRepos grep repositories — each code.ErrInvalid or the read's own
// refusal, a refusal over one repository a [RepoRefusal] naming it. ONE
// REPOSITORY is a read of it through the keyed walk (code.GitSearch), whose
// answer or refusal is the search's, as a [RepoRefusal]. More are ONE
// SCATTER: each repository asked of the first node of its git order, in one
// signed request on topics.CodeSearch, then once more of its runner-up where
// that node did not answer it — and the lines merged by (repository, path,
// line), the same answer whichever node answers first.
func (s *Searcher) Search(ctx context.Context, q Query) (Answer, error) {
	began := s.now()
	if err := q.validate(); err != nil {
		return Answer{}, err
	}
	repos, grepLeft, err := s.repos(ctx, q)
	if err != nil {
		return Answer{}, err
	}
	sq := code.SearchQuery{Text: q.Search.Text, Mode: q.Search.Mode, IgnoreCase: q.Search.IgnoreCase,
		Paths: q.Search.Paths, Ref: q.Search.Ref, Limit: q.Limit}
	var (
		out Answer
		via = "scatter"
	)
	if len(repos) == 1 {
		var read code.SearchVia
		if out, read, err = s.one(ctx, q, sq, repos[0]); err != nil {
			return Answer{}, err
		}
		via = string(read)
	} else if out, err = s.scatter(ctx, q, sq, repos); err != nil {
		return Answer{}, err
	}
	out.Coverage.GrepNotNamed = setOf(grepLeft)
	s.record(via, s.now().Sub(began), out.Coverage)
	return out, nil
}

// RepoRefusal is a search refused over ONE repository it names — a repo:
// filter naming one nobody declared or the asker may not read, or the one
// repository a search reads refusing it: the repository by name, and the
// read's own refusal, which errors.Is and errors.As reach.
//
// TYPED, because a surface answering the refusal names the repository in it
// (internal/api/queries' `not_declared` says which name is declared nowhere),
// and a search naming several cannot otherwise tell which one was refused.
type RepoRefusal struct {
	Repo string
	Err  error
}

// Error names the repository and the refusal.
func (e *RepoRefusal) Error() string { return fmt.Sprintf("codesearch: repo:%s: %v", e.Repo, e.Err) }

// Unwrap is the read's own refusal.
func (e *RepoRefusal) Unwrap() error { return e.Err }

// validate refuses what no node would answer.
func (q Query) validate() error {
	if q.Limit < 0 || q.Limit > code.MaxSearchHits {
		return fmt.Errorf("%w: limit: %d is not 1 to %d (0 is %d)", code.ErrInvalid, q.Limit, code.MaxSearchHits,
			code.MaxSearchHits)
	}
	if err := q.Search.Validate(); err != nil {
		return fmt.Errorf("%w: %w", code.ErrInvalid, err)
	}
	if err := q.Asker.Validate(); err != nil {
		return fmt.Errorf("asker: %w", err)
	}
	return nil
}

// repos is the repositories q searches, by name, and the grep repositories
// an unfiltered q leaves out.
func (s *Searcher) repos(ctx context.Context, q Query) (searched []code.RepoRef, grepLeft []string, err error) {
	if len(q.Search.Repos) > 0 {
		grep := 0
		// ONE REPOSITORY NAMED TWICE IS SEARCHED ONCE: codeindex.ParseQuery
		// names each once, and a Query built otherwise may not, where a
		// repository listed twice would be read twice by its node, its lines
		// taking two places in that node's first Limit, and counted twice in
		// the coverage every repository is in exactly once.
		seen := make(map[string]bool, len(q.Search.Repos))
		for _, name := range q.Search.Repos {
			if seen[name] {
				continue
			}
			seen[name] = true
			a, err := s.estate.Read(ctx, code.ReadQuery{Kind: code.ReadRepo, Repo: name, Asker: q.Asker,
				CeilingSeat: q.CeilingSeat, Fresh: q.Rows})
			if err != nil {
				return nil, nil, &RepoRefusal{Repo: name, Err: err}
			}
			if a.Repo == nil {
				return nil, nil, &RepoRefusal{Repo: name, Err: code.ErrRepoNotFound}
			}
			if a.Repo.Search != code.SearchIndexed {
				grep++
			}
			searched = append(searched, a.Repo.RepoRef)
		}
		if grep > MaxGrepRepos {
			return nil, nil, fmt.Errorf("%w: repo: names %d repositories searched with git grep, and a search "+
				"reads at most %d of them: name fewer, or search the indexed ones without a repo: filter",
				code.ErrInvalid, grep, MaxGrepRepos)
		}
		return searched, nil, nil
	}
	rq := code.ReadQuery{Kind: code.ReadRepos, Asker: q.Asker, CeilingSeat: q.CeilingSeat, Fresh: q.Rows,
		Limit: code.MaxReposPerPage}
	for {
		page, err := s.estate.Read(ctx, rq)
		if err != nil {
			return nil, nil, fmt.Errorf("codesearch: list the repositories the asker may read: %w", err)
		}
		for _, r := range page.Repos {
			if r.Search == code.SearchIndexed {
				searched = append(searched, r.RepoRef)
			} else {
				grepLeft = append(grepLeft, r.Name)
			}
		}
		if page.Next == "" {
			return searched, grepLeft, nil
		}
		rq.Cursor = page.Next
	}
}

// one searches one repository through the keyed walk.
func (s *Searcher) one(ctx context.Context, q Query, sq code.SearchQuery, repo code.RepoRef) (Answer,
	code.SearchVia, error) {
	got, err := s.estate.GitRead(ctx, code.GitQuery{Kind: code.GitSearch, RepoID: repo.ID, Asker: q.Asker,
		CeilingSeat: q.CeilingSeat, Turn: q.Turn, Fresh: q.Git, Search: &sq})
	if err != nil {
		return Answer{}, "", &RepoRefusal{Repo: repo.Name, Err: err}
	}
	if got.Search == nil {
		return Answer{}, "", fmt.Errorf("codesearch: repo:%s: the git node answered a search with no search",
			repo.Name)
	}
	ans := got.Search
	out := Answer{Hits: make([]Hit, len(ans.Hits)), Coverage: Coverage{Searched: 1,
		SkippedFiles: ans.SkippedFiles, Truncated: ans.Truncated}}
	for i, h := range ans.Hits {
		out.Hits[i] = Hit{Repo: repo.Name, SearchHit: h}
	}
	if len(out.Hits) > 0 {
		out.Tips = map[string]string{repo.Name: ans.Tip}
	}
	if repo.Search == code.SearchIndexed && ans.Via == code.SearchViaGrep {
		out.Coverage.NotIndexed = setOf([]string{repo.Name})
	}
	sortCoverage(&out.Coverage)
	return out, ans.Via, nil
}

// asked is one repository's place in a scatter: its order, which node of it
// is being asked, and what the last node asked answered.
type asked struct {
	repo  code.RepoRef
	order []string
	at    int
	// done is set once the repository's outcome is final.
	done   bool
	result RepoResult
	// silent is the last node asked not answering at all; why is the
	// sentence for a node that answered nothing for it.
	silent bool
	why    string
}

// scatter asks every repository of its order's first node, then its
// runner-up where that one did not answer it.
func (s *Searcher) scatter(ctx context.Context, q Query, sq code.SearchQuery, repos []code.RepoRef) (Answer,
	error) {
	out := Answer{Hits: []Hit{}}
	if len(repos) == 0 {
		return out, nil
	}
	ids := make([]string, len(repos))
	for i, r := range repos {
		ids[i] = r.ID
	}
	orders, err := s.placement.GitOrders(ids)
	if err != nil {
		return Answer{}, fmt.Errorf("codesearch: place %d repositories on the git nodes: %w", len(ids), err)
	}
	all := make([]*asked, len(repos))
	for i, r := range repos {
		all[i] = &asked{repo: r, order: orders[r.ID]}
	}
	var hits []Hit
	cut := false
	for round := range 2 {
		assign := map[string][]Assigned{}
		byNode := map[string][]*asked{}
		for _, a := range all {
			if a.done || a.at >= len(a.order) {
				continue
			}
			// THE FILL: the first node of the order is its home, which may
			// fill a copy; a runner-up may where the home passed the fill on
			// (ADR-0059's walk) or said nothing at all.
			home := round == 0 || a.silent || (a.result.Outcome == OutcomeUnready && a.result.Unready.PassesFill()) ||
				a.result.Outcome == OutcomeFailed
			node := a.order[a.at]
			assign[node] = append(assign[node], Assigned{RepoID: a.repo.ID, Name: a.repo.Name, Home: home})
			byNode[node] = append(byNode[node], a)
		}
		if len(assign) == 0 {
			break
		}
		replies, err := s.round(ctx, q, sq, assign)
		if err != nil {
			return Answer{}, err
		}
		for node, list := range byNode {
			rep, answered := replies[node]
			results := map[string]RepoResult{}
			if answered {
				for _, r := range rep.Repos {
					results[r.RepoID] = r
				}
				hits = append(hits, rep.Hits...)
				cut = cut || rep.Cut
			}
			for _, a := range list {
				r, ok := results[a.repo.ID]
				a.result, a.silent, a.why = r, !ok, ""
				switch {
				case answered && rep.Unverified != "":
					// RAN NOTHING, as a silent node did: its runner-up may
					// fill, and the sentence says what to fix.
					a.why = rep.Unverified
				case !ok && answered:
					a.why = "it answered for the other repositories it was asked for, and not this one"
				case !ok:
					a.why = fmt.Sprintf("it did not answer within %s", RoundBudget)
				}
				switch r.Outcome {
				case OutcomeSearched, OutcomeNotPermitted, OutcomeNoRef:
					a.done = ok
				}
				a.at++
			}
		}
	}
	tips := map[string]string{}
	for _, a := range all {
		classify(&out.Coverage, a, tips)
	}
	slices.SortFunc(hits, compareHits)
	hits = slices.CompactFunc(hits, func(x, y Hit) bool { return compareHits(x, y) == 0 })
	limit := sq.Hits()
	if len(hits) > limit {
		hits, cut = hits[:limit], true
	}
	out.Hits = append(out.Hits, hits...)
	out.Coverage.Truncated = out.Coverage.Truncated || cut
	for _, h := range out.Hits {
		if out.Tips == nil {
			out.Tips = map[string]string{}
		}
		out.Tips[h.Repo] = tips[h.Repo]
	}
	sortCoverage(&out.Coverage)
	return out, nil
}

// classify files one repository's final outcome in the coverage.
func classify(c *Coverage, a *asked, tips map[string]string) {
	name := a.repo.Name
	r := a.result
	if a.done {
		switch r.Outcome {
		case OutcomeSearched:
			c.Searched++
			c.SkippedFiles += r.SkippedFiles
			c.Truncated = c.Truncated || r.Truncated
			tips[name] = r.Tip
			if a.repo.Search == code.SearchIndexed && r.Via == code.SearchViaGrep {
				add(&c.NotIndexed, name)
			}
		case OutcomeNotPermitted:
			add(&c.NotPermitted, name)
		case OutcomeNoRef:
			add(&c.NoRef, name)
		}
		return
	}
	switch {
	case len(a.order) == 0:
		gap(c, "", "no git node serves the repository", name)
	case a.silent || r.Outcome != OutcomeUnready:
		gap(c, a.order[a.at-1], whyOf(a), name)
	case r.Unready == code.UnreadyIndexBuilding:
		add(&c.Building, name)
	case r.Unready == code.UnreadySyncing || r.Unready == code.UnreadyNoChart || r.Unready == code.UnreadyBehindConfig:
		add(&c.Stale, name)
	default:
		gap(c, a.order[a.at-1], whyOf(a), name)
	}
}

// whyOf is the sentence a node that did not answer a repository is named
// with.
func whyOf(a *asked) string {
	if a.why != "" {
		return a.why
	}
	if a.result.Detail != "" {
		return a.result.Detail
	}
	return string(a.result.Outcome)
}

// gap names one repository left unanswered for node.
func gap(c *Coverage, node, why, name string) {
	for i := range c.NotAnswering {
		if c.NotAnswering[i].Node == node {
			add(&c.NotAnswering[i].Repos, name)
			return
		}
	}
	g := NodeGap{Node: node, Why: why}
	add(&g.Repos, name)
	c.NotAnswering = append(c.NotAnswering, g)
}

// add puts name in set, counted whole and listed up to MaxCoverageNames; the
// names are sorted at the end ([sortCoverage]), so which are listed is the
// first in order whatever order they were added in.
func add(set *RepoSet, name string) {
	set.Count++
	set.Names = append(set.Names, name)
}

// setOf is names as a set.
func setOf(names []string) RepoSet {
	var s RepoSet
	for _, n := range names {
		add(&s, n)
	}
	s.cap()
	return s
}

// cap sorts a set's names and keeps the first MaxCoverageNames.
func (s *RepoSet) cap() {
	slices.Sort(s.Names)
	if len(s.Names) > MaxCoverageNames {
		s.Names = s.Names[:MaxCoverageNames]
	}
	if s.Names == nil {
		s.Names = []string{}
	}
}

// sortCoverage puts every set and the nodes in order.
func sortCoverage(c *Coverage) {
	for _, s := range []*RepoSet{&c.NotIndexed, &c.Building, &c.NotPermitted, &c.GrepNotNamed, &c.Stale, &c.NoRef} {
		s.cap()
	}
	for i := range c.NotAnswering {
		c.NotAnswering[i].Repos.cap()
	}
	slices.SortFunc(c.NotAnswering, func(x, y NodeGap) int {
		switch {
		case x.Node < y.Node:
			return -1
		case x.Node > y.Node:
			return 1
		}
		return 0
	})
	if c.NotAnswering == nil {
		c.NotAnswering = []NodeGap{}
	}
}

// round asks one round's assignment and answers the believed replies by
// node: a reply is believed only where it verifies, is bound to this round's
// request and names a node the round asked. The round ends once every node
// asked has answered, or at RoundBudget.
func (s *Searcher) round(ctx context.Context, q Query, sq code.SearchQuery,
	assign map[string][]Assigned) (map[string]Reply, error) {
	rctx, cancel := context.WithTimeout(ctx, RoundBudget)
	defer cancel()
	deadline, _ := rctx.Deadline()
	req := Request{Version: Protocol, ID: signedask.NewID(), Deadline: deadline, Asker: q.Asker,
		CeilingSeat: q.CeilingSeat, Turn: q.Turn, Fresh: q.Git, Search: sq, Assign: assign}
	sealed, err := s.wire.SealRequest(req)
	if err != nil {
		return nil, fmt.Errorf("codesearch: seal the search: %w", err)
	}
	replies := map[string]Reply{}
	err = s.ask(rctx, sealed, func(raw []byte) bool {
		rep, why, err := s.wire.OpenReply(raw, req.ID)
		switch {
		case err != nil:
			s.log.Warn("code_search_reply_unreadable", "error", err)
		case why != "":
			s.log.Warn("code_search_reply_refused", "why", why)
		case rep.Version != Protocol:
			s.log.Warn("code_search_reply_unreadable", "node", rep.Node, "version", rep.Version)
		case assign[rep.Node] == nil:
		default:
			if _, twice := replies[rep.Node]; !twice {
				replies[rep.Node] = rep
			}
		}
		return len(replies) < len(assign)
	})
	if err != nil && ctx.Err() == nil && rctx.Err() == nil {
		return nil, fmt.Errorf("codesearch: scatter the search: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return replies, nil
}

// record counts one search: by how it was read, its duration, and every
// repository its coverage names as not searched, by reason.
func (s *Searcher) record(via string, took time.Duration, c Coverage) {
	if s.metrics == nil {
		return
	}
	s.metrics.Add(metrics.CodeSearchQueries, 1, metrics.Attrs{"via": via})
	s.metrics.Observe(metrics.CodeSearchDuration, took, metrics.Attrs{"via": via})
	not := 0
	for _, g := range c.NotAnswering {
		not += g.Repos.Count
	}
	for reason, n := range map[string]int{"not_indexed": c.NotIndexed.Count, "building": c.Building.Count,
		"not_answering": not, "not_permitted": c.NotPermitted.Count, "grep_not_named": c.GrepNotNamed.Count,
		"stale": c.Stale.Count, "no_ref": c.NoRef.Count} {
		if n > 0 {
			s.metrics.Add(metrics.CodeSearchCoverage, uint64(n), metrics.Attrs{"reason": reason})
		}
	}
}
