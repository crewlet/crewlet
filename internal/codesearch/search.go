package codesearch

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Estate is Gitlet's rows and its keyed git walk as a search reads
// them: the asker's repositories (gitlet.ReadRepos and gitlet.ReadRepo, held to
// the asker's grant by the node that serves them) and ONE repository's search
// (gitlet.GitSearch through the walk, which asks the repository's Gitlet nodes in
// order). Declared here by its consumer; estate.CodeSeam answers both.
type Estate interface {
	Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error)
	GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error)
}

// Placement is every repository's git order, judged on one reading of who
// serves git (estate.Router.GitOrders): its first node answers for it, its
// second is its runner-up.
type Placement interface {
	GitOrders(repoIDs []string) (map[string][]string, error)
}

// AskFunc scatters one sealed request to every Gitlet node and hands each reply
// to each as it arrives, until ctx ends or each answers false
// (queue.EventQueue.AskEach on topics.CodeSearch, which the engine binds). A
// node with no broker hands it to its own answerer, its fleet being itself.
type AskFunc func(ctx context.Context, request []byte, each func(reply []byte) bool) error

// Query is a code search as a seat's tool or a person's route asks it.
type Query struct {
	// Search is the search: its text, mode and case, and its repo:, path:
	// and ref: filters — codeindex's one grammar.
	Search codeindex.Query
	// Limit is the most hits answered, 1 to gitlet.MaxSearchHits; 0 is
	// gitlet.MaxSearchHits.
	Limit int
	// After, where set, is the place in the search's order the answer
	// continues from ([Key]): the lines after it, Limit of them. A page of
	// a long search is asked after the last line of the page before.
	After *Key
	// Asker is who searches, REQUIRED; CeilingSeat and Turn are a seat's
	// read's as gitlet.GitQuery's are. Every node judges each repository's
	// grant for them.
	Asker       gitlet.Principal
	CeilingSeat string
	Turn        string
	// Rows is the level the asker's repositories are listed at — a seat's
	// and a person's linearizable — and Git the floor each copy searched is
	// brought to, as a read by object id's (gitlet.GitQuery.Fresh).
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
// Building, NotAnswering, NotPermitted, Stale, NoRef and PastLimit, but in a
// search continued after a [Key], which names those from the key's on: the
// ones before it were asked for the pages before, and are in none; GrepNotNamed is the
// repositories an unfiltered search leaves out, and NotIndexed names those
// of Searched that were read with git grep.
type Coverage struct {
	// Searched is how many repositories answered their lines.
	Searched int `json:"searched"`
	// NotIndexed is the `search: indexed` repositories among them read with
	// git grep, no index of their Gitlet nodes answering for the commit: the
	// same lines, read more slowly.
	NotIndexed RepoSet `json:"not_indexed"`
	// Building is the repositories left unanswered because the last Gitlet node
	// asked for them was building their index: the first index holder, where
	// their order named no runner-up to ask. A runner-up that was asked and
	// did not answer is named in NotAnswering instead — the coverage names
	// what the LAST node asked for a repository said of it.
	Building RepoSet `json:"building"`
	// NotAnswering is every Gitlet node that did not answer for a repository it
	// was asked for — silent, busy, failing or holding no key the request is
	// signed under — with the repositories left unanswered for it.
	NotAnswering []NodeGap `json:"not_answering"`
	// NotPermitted is the repositories a Gitlet node judged the asker's grant
	// not to cover: the chart moved since the asker's listing.
	NotPermitted RepoSet `json:"not_permitted"`
	// GrepNotNamed is the `search: grep` repositories the asker may read
	// that an unfiltered search leaves out: each is searched only when a
	// repo: filter names it, at most MaxGrepRepos at once.
	GrepNotNamed RepoSet `json:"grep_not_named"`
	// Stale is the repositories left unanswered because the last Gitlet node
	// asked for them was behind — its copy still syncing to the log, or its
	// chart behind the company's configuration: the runner-up, or the first
	// node where the order named no runner-up.
	Stale RepoSet `json:"stale"`
	// NoRef is the repositories holding no ref or commit the search's ref:
	// names: nothing to search in them at it.
	NoRef RepoSet `json:"no_ref"`
	// PastLimit is the repositories none of whose lines can be in the
	// answer: it holds Limit lines, every one of a repository named before
	// them. A Gitlet node passes over such a repository unread once its own
	// first Limit lines are of repositories named before it, so a search for
	// a common word reads the repositories that fill the limit rather than
	// the company; whether one was read or passed over turns on how the reads
	// interleaved, so it is named here by its name alone, either way.
	// Truncated is set beside it.
	PastLimit RepoSet `json:"past_limit"`
	// SkippedFiles is the files the searched repositories passed over for
	// their size or as binaries (gitlet.SearchAnswer.SkippedFiles).
	SkippedFiles int `json:"skipped_files"`
	// Truncated says lines may follow the last hit, or be missing: a
	// repository's search stopped at its limit, its answer's bound or its
	// budget, the merge cut the lines past Limit or past such a stop, or a
	// repository is past the limit (PastLimit). Every line before the last
	// hit is answered; the lines after it are what a search continued after
	// it answers, but for a repository whose budget ran out before its first
	// line, which no page reaches.
	Truncated bool `json:"truncated"`
}

// MaxCoverageText is the most bytes of WORDS a [Coverage] lists, as JSON
// encodes them: every repository name of every set — NotAnswering's
// included — and every node's reason, ONE BUDGET across them all. 4 KiB, a
// sixteenth of a seat's tool answer (internal/agent/builtin's
// ToolAnswerBytes, 64 KiB), since code_search sends the coverage beside every
// page of its lines and the lines are the answer. Every count stays whole:
// past the budget a set lists fewer of its names — the first, in order — and
// a reason is cut. The reasons are listed first, in the order of their nodes
// and in at most HALF the budget, so the names keep the other half whatever
// the fleet; then the names, the sets of repositories left unsearched before
// those searched another way.
//
// WHAT THE BUDGET DOES NOT HOLD is the coverage's shape — its fields' names
// and counts, under 600 bytes — and one entry for each Gitlet node that did not
// answer, its id (at most 64 bytes) and its count, under 140 bytes: every
// such node is named, which is the point of the entry. A fleet of sixteen
// silent Gitlet nodes so takes a coverage to under 7 KiB. Its own per-set cap of
// 100 names was an eighth of an answer by its arithmetic and three quarters
// of one by the names' real length, with NotAnswering's sets beside it
// growing with the fleet.
const MaxCoverageText = 4 << 10

// MaxGapWhyBytes is the most of one node's reason a coverage keeps, as JSON
// encodes it: two lines, what a node's sentence takes to say what to look
// at — one carrying an error's own words is cut there — and eight such fill
// the reasons' half of the budget.
const MaxGapWhyBytes = 256

// RepoSet is a set of repositories: how many, and the first of their names
// in order, as many as the coverage's budget lists ([MaxCoverageText]).
type RepoSet struct {
	Count int      `json:"count"`
	Names []string `json:"names"`
}

// NodeGap is one Gitlet node that did not answer, why, and the repositories left
// unanswered for it.
type NodeGap struct {
	Node  string  `json:"node"`
	Why   string  `json:"why"`
	Repos RepoSet `json:"repos"`
}

// MaxGrepRepos is the most `search: grep` repositories one search reads.
// JUDGMENT: a monorepo's git grep costs one to two CPU-seconds of a Gitlet
// node's read lane, so eight is a search that can hold a node's seat share
// for most of its budget — more is a scan of the company, which the index is
// for.
const MaxGrepRepos = 8

// RoundBudget is how long one round of the scatter waits for its nodes:
// what one node's read of a repository may take — its wait for a unit of the
// seat git-work share (gitlet.GitShareWait, 2 s), then the search's own budget
// (gitlet.SearchBudget, 5 s) — and a reply's margin each way
// (gitlet.ReplyMargin), so a node whose read ran its whole budget answers
// while the asker listens. A SUM OF WHOLE BUDGETS (M3-O6), 9 s; a round ends
// sooner, as soon as every node it asked has answered, and a search takes two
// rounds only where a node did not answer the first.
const RoundBudget = gitlet.GitShareWait + gitlet.SearchBudget + 2*gitlet.ReplyMargin

// Searcher answers code searches over the fleet's Gitlet nodes.
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
		return nil, errors.New("codesearch: a searcher needs Gitlet's estate, the git placement, " +
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
// MaxGrepRepos grep repositories — each gitlet.ErrInvalid or the read's own
// refusal, a refusal over one repository a [RepoRefusal] naming it. ONE
// REPOSITORY is a read of it through the keyed walk (gitlet.GitSearch), whose
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
	repos, grepLeft, head, err := s.repos(ctx, q)
	if err != nil {
		return Answer{}, err
	}
	sq := gitlet.SearchQuery{Text: q.Search.Text, Mode: q.Search.Mode, IgnoreCase: q.Search.IgnoreCase,
		Paths: q.Search.Paths, Ref: q.Search.Ref, Limit: q.Limit}
	var (
		out Answer
		via = "scatter"
		// ONE REPOSITORY OR A SCATTER BY WHAT THE SEARCH NAMES, decided
		// before a page's place leaves any out: a page of a search of
		// several is a scatter as its first page was, its refusals in the
		// coverage, never the one repository's refusal of the search.
		single = len(repos) == 1
	)
	if q.After != nil {
		// A REPOSITORY BEFORE THE PLACE IS ASKED NOTHING: every line of it
		// sorts before the place, and an earlier page answered them.
		repos = slices.DeleteFunc(repos, func(r gitlet.RepoRef) bool { return r.Name < q.After.Repo })
	}
	if single && len(repos) == 1 {
		var read gitlet.SearchVia
		if out, read, err = s.one(ctx, q, sq, head, repos[0]); err != nil {
			return Answer{}, err
		}
		via = string(read)
	} else if out, err = s.scatter(ctx, q, sq, head, repos); err != nil {
		return Answer{}, err
	}
	out.Coverage.GrepNotNamed = setOf(grepLeft)
	fitCoverage(&out.Coverage)
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
	if q.Limit < 0 || q.Limit > gitlet.MaxSearchHits {
		return fmt.Errorf("%w: limit: %d is not 1 to %d (0 is %d)", gitlet.ErrInvalid, q.Limit, gitlet.MaxSearchHits,
			gitlet.MaxSearchHits)
	}
	if err := q.Search.Validate(); err != nil {
		return fmt.Errorf("%w: %w", gitlet.ErrInvalid, err)
	}
	if err := q.Asker.Validate(); err != nil {
		return fmt.Errorf("asker: %w", err)
	}
	if q.After != nil {
		if !gitlet.ValidName(q.After.Repo) {
			return fmt.Errorf("%w: after.repo: %q is not a repository's name", gitlet.ErrInvalid, q.After.Repo)
		}
		if !gitref.ValidOID(q.After.Tip) {
			return fmt.Errorf("%w: after.tip: %q is not a commit's whole id", gitlet.ErrInvalid, q.After.Tip)
		}
		if _, err := q.After.Raw(); err != nil {
			return err
		}
	}
	return nil
}

// repos is the repositories q searches, by name, the grep repositories an
// unfiltered q leaves out, and the code log's HEAD as those reads found it
// where they were linearizable (headOf): what every repository's second
// look at the log's head syncs to (gitlet.GitQuery.Head).
func (s *Searcher) repos(ctx context.Context, q Query) (searched []gitlet.RepoRef, grepLeft []string,
	head statelog.Position, err error) {
	if len(q.Search.Repos) > 0 {
		searched, head, err = s.named(ctx, q)
		return searched, nil, head, err
	}
	rq := gitlet.ReadQuery{Kind: gitlet.ReadRepos, Asker: q.Asker, CeilingSeat: q.CeilingSeat, Fresh: q.Rows,
		Limit: gitlet.MaxReposPerPage}
	for {
		page, err := s.estate.Read(ctx, rq)
		if err != nil {
			return nil, nil, statelog.Position{}, fmt.Errorf("codesearch: list the repositories the asker may "+
				"read: %w", err)
		}
		for _, r := range page.Repos {
			if r.Search == gitlet.SearchIndexed {
				searched = append(searched, r.RepoRef)
			} else {
				grepLeft = append(grepLeft, r.Name)
			}
		}
		head = headOf(head, page)
		if page.Next == "" {
			return searched, grepLeft, head, nil
		}
		rq.Cursor = page.Next
	}
}

// ResolveReads is how many of a repo: filter's names are read at once.
//
// A name is one estate read of the repository's row (gitlet.ReadRepo) at the
// asker's level, and a seat's and a person's is linearizable: read one after
// another, a filter of dozens of names — a 400-byte query holds up to 57 —
// was as many barriers back to back before anything was searched. Read
// beside each other, the reads in flight at once join ONE barrier (a node's
// barrier is single-flighted per log: statelog.ReadIndex), so a filter costs
// a barrier a wave. MaxGrepRepos+1, nine, is the fewest answers that can
// show a filter past the grep bound — so one wave decides a filter of grep
// repositories that must be refused, and the rest are never read — and it
// resolves the most names a query can hold in seven waves.
const ResolveReads = MaxGrepRepos + 1

// named is the repositories q's repo: filters name, read BESIDE EACH OTHER,
// at most ResolveReads at once, and the head their reads found (headOf). A
// filter naming more grep repositories than MaxGrepRepos is refused AS THEIR
// ROWS ARRIVE: the reads in flight are cancelled and the rest never started.
// Otherwise every name is read, and a refusal is the FIRST NAME's, in the
// order the filter wrote them — whichever read answered first — so a search
// is refused the same however its reads interleave.
func (s *Searcher) named(ctx context.Context, q Query) ([]gitlet.RepoRef, statelog.Position, error) {
	// ONE REPOSITORY NAMED TWICE IS SEARCHED ONCE: codeindex.ParseQuery
	// names each once, and a Query built otherwise may not, where a
	// repository listed twice would be read twice by its node, its lines
	// taking two places in that node's first Limit, and counted twice in the
	// coverage every repository is in exactly once.
	var names []string
	seen := make(map[string]bool, len(q.Search.Repos))
	for _, name := range q.Search.Repos {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type read struct {
		a   gitlet.ReadAnswer
		err error
	}
	reads := make([]read, len(names))
	var (
		mu   sync.Mutex
		grep int
		over bool
		wg   sync.WaitGroup
	)
	slots := make(chan struct{}, ResolveReads)
	for i, name := range names {
		if rctx.Err() != nil {
			break
		}
		select {
		case slots <- struct{}{}:
		case <-rctx.Done():
		}
		if rctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-slots }()
			a, err := s.estate.Read(rctx, gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: name, Asker: q.Asker,
				CeilingSeat: q.CeilingSeat, Fresh: q.Rows})
			mu.Lock()
			defer mu.Unlock()
			reads[i] = read{a: a, err: err}
			if err == nil && a.Repo != nil && a.Repo.Search != gitlet.SearchIndexed {
				if grep++; grep > MaxGrepRepos && !over {
					over = true
					cancel()
				}
			}
		})
	}
	wg.Wait()
	if over {
		return nil, statelog.Position{}, fmt.Errorf("%w: repo: names more than %d repositories searched with "+
			"git grep, and a search reads at most %d of them: name fewer, or search the indexed ones without a "+
			"repo: filter", gitlet.ErrInvalid, MaxGrepRepos, MaxGrepRepos)
	}
	var (
		searched []gitlet.RepoRef
		head     statelog.Position
	)
	for i, r := range reads {
		switch {
		case r.err != nil:
			return nil, statelog.Position{}, &RepoRefusal{Repo: names[i], Err: r.err}
		case r.a.Repo == nil:
			// A read never started (ctx ended first) answers ctx's error.
			if err := ctx.Err(); err != nil {
				return nil, statelog.Position{}, &RepoRefusal{Repo: names[i], Err: err}
			}
			return nil, statelog.Position{}, &RepoRefusal{Repo: names[i], Err: gitlet.ErrRepoNotFound}
		}
		searched, head = append(searched, r.a.Repo.RepoRef), headOf(head, r.a)
	}
	return searched, head, nil
}

// headOf is the later of head and where a read of the rows answered, where
// that read was LINEARIZABLE: a position every write committed before the
// search asked is at or before, which each repository's second look can sync
// to as a floor (gitlet.GitQuery.Head) in place of a barrier of its own — so a
// company-wide search pays for its listing's barrier once, not for one a
// repository. A read at any other level found no head, and is not one.
func headOf(head statelog.Position, a gitlet.ReadAnswer) statelog.Position {
	if a.Level != statelog.ReadLinearizable || a.Position.IsZero() {
		return head
	}
	if head.IsZero() {
		return a.Position
	}
	if before, err := head.Before(a.Position); err == nil && before {
		return a.Position
	}
	return head
}

// one searches one repository through the keyed walk.
func (s *Searcher) one(ctx context.Context, q Query, sq gitlet.SearchQuery, head statelog.Position,
	repo gitlet.RepoRef) (Answer,
	gitlet.SearchVia, error) {
	q.After.continueIn(repo.Name, &sq)
	got, err := s.estate.GitRead(ctx, gitlet.GitQuery{Kind: gitlet.GitSearch, RepoID: repo.ID, Asker: q.Asker,
		CeilingSeat: q.CeilingSeat, Turn: q.Turn, Fresh: q.Git, Head: head, Search: &sq})
	if err != nil {
		return Answer{}, "", &RepoRefusal{Repo: repo.Name, Err: err}
	}
	if got.Search == nil {
		return Answer{}, "", fmt.Errorf("codesearch: repo:%s: the Gitlet node answered a search with no search",
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
	if repo.Search == gitlet.SearchIndexed && ans.Via == gitlet.SearchViaGrep {
		out.Coverage.NotIndexed = setOf([]string{repo.Name})
	}
	return out, ans.Via, nil
}

// asked is one repository's place in a scatter: its order, which node of it
// is being asked, and what the last node asked answered.
type asked struct {
	repo  gitlet.RepoRef
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
func (s *Searcher) scatter(ctx context.Context, q Query, sq gitlet.SearchQuery, head statelog.Position,
	repos []gitlet.RepoRef) (Answer,
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
		return Answer{}, fmt.Errorf("codesearch: place %d repositories on the Gitlet nodes: %w", len(ids), err)
	}
	all := make([]*asked, len(repos))
	for i, r := range repos {
		all[i] = &asked{repo: r, order: orders[r.ID]}
	}
	var (
		m    = newMerge(sq.Hits())
		ends prefixEnd
		cut  = false
	)
	for round := range 2 {
		assign := map[string][]Assigned{}
		byNode := map[string][]*asked{}
		for _, a := range all {
			if a.done || a.at >= len(a.order) {
				continue
			}
			if m.passed(a.repo.Name) {
				// PAST THE LIMIT ALREADY: the first round's lines fill the
				// limit before any of this repository's could come, so its
				// runner-up is not asked for lines the answer cannot hold.
				continue
			}
			// THE FILL: the first node of the order is its home, which may
			// fill a copy; a runner-up may where the home passed the fill on
			// (ADR-0089's walk) or said nothing at all.
			home := round == 0 || a.silent || (a.result.Outcome == OutcomeUnready && a.result.Unready.PassesFill()) ||
				a.result.Outcome == OutcomeFailed
			node := a.order[a.at]
			assign[node] = append(assign[node], Assigned{RepoID: a.repo.ID, Name: a.repo.Name, Home: home,
				After: q.After.of(a.repo.Name)})
			byNode[node] = append(byNode[node], a)
		}
		if len(assign) == 0 {
			break
		}
		replies, err := s.round(ctx, q, sq, head, assign)
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
				m.fold(rep.Hits)
				cut = cut || rep.Cut
				// A NODE THAT CUT its lines answered the first of them up to
				// its last; a repository that stopped early, its own first
				// lines up to ITS last (gitlet.SearchAnswer) — and no line
				// after either is known to follow every line before it.
				if rep.Cut && len(rep.Hits) > 0 {
					ends.at(rep.Hits[len(rep.Hits)-1])
				}
				stopped := map[string]bool{}
				for _, a := range list {
					if r, ok := results[a.repo.ID]; ok && r.Truncated {
						stopped[a.repo.Name] = true
					}
				}
				ends.stopped(rep.Hits, stopped)
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
				case OutcomeSearched, OutcomeNotPermitted, OutcomeNoRef, OutcomePastLimit:
					a.done = ok
				}
				a.at++
			}
		}
	}
	tips := map[string]string{}
	for _, a := range all {
		if m.passed(a.repo.Name) {
			// PAST THE LIMIT OF THE WHOLE SEARCH, whatever its node said or
			// whether one answered: decided by the name against the lines
			// every node's answers left, so the coverage is the same
			// whichever node answered first.
			a.done, a.result, cut = true, RepoResult{RepoID: a.repo.ID, Outcome: OutcomePastLimit}, true
		}
		classify(&out.Coverage, a, tips)
	}
	// The merge's first Limit lines, then no line past a stop: both cut a
	// prefix of the same order, so which runs first does not change what is
	// kept.
	hits, folded := m.lines()
	hits, past := ends.keep(hits)
	cut = cut || past
	out.Hits = append(out.Hits, hits...)
	out.Coverage.Truncated = out.Coverage.Truncated || cut || folded
	for _, h := range out.Hits {
		if out.Tips == nil {
			out.Tips = map[string]string{}
		}
		out.Tips[h.Repo] = tips[h.Repo]
	}
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
			if a.repo.Search == gitlet.SearchIndexed && r.Via == gitlet.SearchViaGrep {
				add(&c.NotIndexed, name)
			}
		case OutcomeNotPermitted:
			add(&c.NotPermitted, name)
		case OutcomeNoRef:
			add(&c.NoRef, name)
		case OutcomePastLimit:
			add(&c.PastLimit, name)
		}
		return
	}
	switch {
	case len(a.order) == 0:
		gap(c, "", "no Gitlet node serves the repository", name)
	case a.silent || r.Outcome != OutcomeUnready:
		gap(c, a.order[a.at-1], whyOf(a), name)
	case r.Unready == gitlet.UnreadyIndexBuilding:
		add(&c.Building, name)
	case r.Unready == gitlet.UnreadySyncing || r.Unready == gitlet.UnreadyNoChart || r.Unready == gitlet.UnreadyBehindConfig:
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

// add puts name in set, counted whole; the names are sorted and cut to the
// coverage's budget once it is whole ([fitCoverage]), so which are listed is
// the first in order whatever order they were added in.
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
	return s
}

// fitCoverage puts every set and the nodes in order and holds the coverage's
// words to [MaxCoverageText]: each node's reason, in the order of the nodes,
// cut to MaxGapWhyBytes and to what is left of the reasons' half of the
// budget; then each set's first names while the budget lasts, the sets of
// repositories left unsearched — NotAnswering's by node, then Building,
// Stale, NotPermitted, NoRef and PastLimit — before NotIndexed and
// GrepNotNamed, which were searched another way or left out by the search's
// own terms. Every count stays whole.
func fitCoverage(c *Coverage) {
	slices.SortFunc(c.NotAnswering, func(x, y NodeGap) int { return cmp.Compare(x.Node, y.Node) })
	if c.NotAnswering == nil {
		c.NotAnswering = []NodeGap{}
	}
	budget := MaxCoverageText
	reasons := MaxCoverageText / 2
	for i := range c.NotAnswering {
		g := &c.NotAnswering[i]
		// THE EMPTY REASON IS THE SHAPE'S, not the budget's: its two quotes
		// are counted with the node's entry.
		g.Why = fitText(g.Why, min(MaxGapWhyBytes, reasons)+jsonLen(""))
		spent := jsonLen(g.Why) - jsonLen("")
		reasons, budget = reasons-spent, budget-spent
	}
	sets := make([]*RepoSet, 0, len(c.NotAnswering)+8)
	for i := range c.NotAnswering {
		sets = append(sets, &c.NotAnswering[i].Repos)
	}
	sets = append(sets, &c.Building, &c.Stale, &c.NotPermitted, &c.NoRef, &c.PastLimit, &c.NotIndexed,
		&c.GrepNotNamed)
	for _, s := range sets {
		slices.Sort(s.Names)
		kept := 0
		for _, n := range s.Names {
			// A name costs its quoted bytes and the comma before it.
			cost := jsonLen(n) + 1
			if cost > budget {
				break
			}
			budget -= cost
			kept++
		}
		s.Names = s.Names[:kept:kept]
		if s.Names == nil {
			s.Names = []string{}
		}
	}
}

// fitText is s in at most room bytes as JSON encodes it, cut on a rune and
// marked where cut — or empty, quoted, where room holds no word of it.
func fitText(s string, room int) string {
	if jsonLen(s) <= room {
		return s
	}
	for most := room - jsonLen(""); most > len("…"); {
		cut := textcut.Within(s, most)
		over := jsonLen(cut) - room
		if over <= 0 {
			return cut
		}
		most -= over
	}
	return ""
}

// jsonLen is how many bytes JSON encodes s in, its quotes included.
func jsonLen(s string) int {
	b, err := json.Marshal(s)
	if err != nil {
		// A Go string always encodes: an invalid byte is written as U+FFFD.
		return len(s) + len(`""`)
	}
	return len(b)
}

// round asks one round's assignment and answers the believed replies by
// node: a reply is believed only where it verifies, is bound to this round's
// request and names a node the round asked. The round ends once every node
// asked has answered, or at RoundBudget.
func (s *Searcher) round(ctx context.Context, q Query, sq gitlet.SearchQuery, head statelog.Position,
	assign map[string][]Assigned) (map[string]Reply, error) {
	rctx, cancel := context.WithTimeout(ctx, RoundBudget)
	defer cancel()
	deadline, _ := rctx.Deadline()
	req := Request{Version: Protocol, ID: signedask.NewID(), Deadline: deadline, Asker: q.Asker,
		CeilingSeat: q.CeilingSeat, Turn: q.Turn, Fresh: q.Git, Head: head, Search: sq, Assign: assign}
	sealed, err := s.wire.SealRequest(req)
	if err != nil {
		return nil, fmt.Errorf("codesearch: seal the search: %w", err)
	}
	replies := map[string]Reply{}
	err = s.ask(rctx, sealed, func(raw []byte) bool {
		rep, why, openErr := s.wire.OpenReply(raw, req.ID)
		switch {
		case openErr != nil:
			s.log.Warn("code_search_reply_unreadable", "error", openErr)
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
		"stale": c.Stale.Count, "no_ref": c.NoRef.Count, "past_limit": c.PastLimit.Count} {
		if n > 0 {
			s.metrics.Add(metrics.CodeSearchCoverage, uint64(n), metrics.Attrs{"reason": reason})
		}
	}
}
