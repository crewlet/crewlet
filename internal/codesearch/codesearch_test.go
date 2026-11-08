package codesearch_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

var (
	fleetRing = recordseal.OneKey("k1", "the code search suite's fleet key material")
	person    = code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ceo", Seat: "ceo"}
)

func wire(t *testing.T, ring recordseal.Keyring) *codesearch.Wire {
	t.Helper()
	w, err := codesearch.NewWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// repo is one repository of the fake company: its row, its git order, and
// the lines each node answers for it.
type repo struct {
	ref   code.RepoRef
	order []string
	lines []code.SearchHit
	// stops, where set, is a search of the repository that stops early —
	// at its answer's bound or its budget — after stopAt of its lines.
	stops  bool
	stopAt int
}

// answerOf is r's search as a Gitlet node answers sq: its lines after sq's
// place, the first sq.Hits() of them, Truncated where it left lines out.
func answerOf(r repo, sq *code.SearchQuery) *code.SearchAnswer {
	var lines []code.SearchHit
	for _, h := range r.lines {
		if sq.After != nil {
			raw, _ := sq.After.Raw()
			if h.Path < raw || h.Path == raw && h.Line <= sq.After.Line {
				continue
			}
		}
		lines = append(lines, h)
	}
	a := &code.SearchAnswer{Tip: tip(r), Via: code.SearchViaIndex}
	limit := sq.Hits()
	if r.stops && r.stopAt < limit {
		limit = r.stopAt
	}
	if len(lines) > limit {
		lines, a.Truncated = lines[:limit], true
	}
	a.Hits = lines
	return a
}

func indexed(name string, order ...string) repo {
	return repo{ref: code.RepoRef{ID: "id-" + name, Name: name, Search: code.SearchIndexed}, order: order}
}

// estate answers the listing and the walk from the fake company; where head
// is set, its reads of the rows answer at it, at level (linearizable where
// unset), and the walk records the head each read names.
type estate struct {
	mu    sync.Mutex
	repos []repo
	reads int
	// at is every repository the walk was asked to search, as
	// "<name>@<ref>".
	at    []string
	head  statelog.Position
	level statelog.ReadLevel
	heads []statelog.Position
}

func (e *estate) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reads++
	var out code.ReadAnswer
	if !e.head.IsZero() {
		out.Position, out.Level = e.head, cmp.Or(e.level, statelog.ReadLinearizable)
	}
	switch q.Kind {
	case code.ReadRepos:
		for _, r := range e.repos {
			out.Repos = append(out.Repos, code.RepoSummary{RepoRef: r.ref})
		}
		return out, nil
	case code.ReadRepo:
		for _, r := range e.repos {
			if r.ref.Name == q.Repo {
				out.Repo = &code.RepoDetail{RepoSummary: code.RepoSummary{RepoRef: r.ref}}
				return out, nil
			}
		}
		return code.ReadAnswer{}, code.ErrRepoNotFound
	}
	return code.ReadAnswer{}, fmt.Errorf("the fake reads no %s", q.Kind)
}

func (e *estate) GitRead(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reads++
	e.heads = append(e.heads, q.Head)
	for _, r := range e.repos {
		if r.ref.ID == q.RepoID {
			e.at = append(e.at, r.ref.Name+"@"+q.Search.Ref)
			return code.GitAnswer{Kind: code.GitSearch, Search: answerOf(r, q.Search)}, nil
		}
	}
	return code.GitAnswer{}, code.ErrRepoNotFound
}

func (e *estate) GitOrders(ids []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, r := range e.repos {
		if slices.Contains(ids, r.ref.ID) {
			out[r.ref.ID] = r.order
		}
	}
	return out, nil
}

func tip(r repo) string { return strings.Repeat("a", 39) + r.ref.Name[:1] }

// node is one Gitlet node's reads: it answers every repository it is asked for
// with its lines, or with the error its fault names.
type node struct {
	e     *estate
	fault map[string]error // by repository name
	asked []string
	// at is every repository asked, as "<name>@<ref>": the commit or ref
	// it was searched at.
	at    []string
	heads []statelog.Position
	mu    sync.Mutex
}

func (n *node) Read(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	for _, r := range n.e.repos {
		if r.ref.ID != q.RepoID {
			continue
		}
		n.mu.Lock()
		n.asked = append(n.asked, r.ref.Name)
		n.at = append(n.at, r.ref.Name+"@"+q.Search.Ref)
		n.heads = append(n.heads, q.Head)
		n.mu.Unlock()
		if err := n.fault[r.ref.Name]; err != nil {
			return code.GitAnswer{}, err
		}
		return code.GitAnswer{Kind: code.GitSearch, Search: answerOf(r, q.Search)}, nil
	}
	return code.GitAnswer{}, code.ErrRepoNotFound
}

// fleet is the scatter's broker: every node's server is handed every
// request, a silent node's answer is dropped, and the replies are handed to
// the asker in the order order puts them in.
type fleet struct {
	servers map[string]*codesearch.Server
	silent  map[string]bool
	order   func(replies [][]byte) [][]byte
}

func (f *fleet) ask(ctx context.Context, request []byte, each func([]byte) bool) error {
	var replies [][]byte
	for id, s := range f.servers {
		reply, err := s.Answer(ctx, request)
		if err != nil || f.silent[id] {
			continue
		}
		replies = append(replies, reply)
	}
	if f.order != nil {
		replies = f.order(replies)
	}
	for _, r := range replies {
		if !each(r) {
			break
		}
	}
	return nil
}

// shareUnits is every fake node's seat git-work share: four units, a Gitlet
// node of eight cores (code.ShareOf).
const shareUnits = 4

func newFleet(t *testing.T, e *estate, nodes map[string]*node) *fleet {
	t.Helper()
	f := &fleet{servers: map[string]*codesearch.Server{}, silent: map[string]bool{}}
	for id, n := range nodes {
		n.e = e
		s, err := codesearch.NewServer(wire(t, fleetRing), id, n, shareUnits)
		if err != nil {
			t.Fatal(err)
		}
		f.servers[id] = s
	}
	return f
}

func searcher(t *testing.T, e *estate, f *fleet) *codesearch.Searcher {
	t.Helper()
	s, err := codesearch.New(codesearch.Options{Estate: e, Placement: e, Ask: f.ask, Wire: wire(t, fleetRing)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func query(text string) codesearch.Query {
	return codesearch.Query{Search: codeindex.Query{Text: text, Mode: codeindex.ModeLiteral}, Asker: person}
}

func line(path string, n int) code.SearchHit {
	return code.SearchHit{Path: path, Line: n, Text: "needle " + path, Blob: strings.Repeat("b", 40)}
}

// A SILENT NODE IS NAMED IN THE COVERAGE: a repository whose first node does
// not answer is asked of its runner-up, which answers it; one with no
// runner-up is left unanswered and named under the silent node; and the
// repositories the answering node holds are searched.
//
// Mutations: drop the runner-up round, and alpha is unanswered, red; leave a
// silent node out of NotAnswering, and git-b is named nowhere, red.
func TestASilentNodeIsNamedInTheCoverage(t *testing.T) {
	t.Parallel()
	alpha, beta, gamma := indexed("alpha", "git-b", "git-a"), indexed("beta", "git-b"), indexed("gamma", "git-a")
	alpha.lines = []code.SearchHit{line("a.go", 1)}
	gamma.lines = []code.SearchHit{line("g.go", 2)}
	e := &estate{repos: []repo{alpha, beta, gamma}}
	a, b := &node{}, &node{}
	f := newFleet(t, e, map[string]*node{"git-a": a, "git-b": b})
	f.silent["git-b"] = true
	got, err := searcher(t, e, f).Search(context.Background(), query("needle"))
	if err != nil {
		t.Fatal(err)
	}
	c := got.Coverage
	if c.Searched != 2 || len(got.Hits) != 2 {
		t.Errorf("searched %d repositories with %d hits, want alpha by its runner-up and gamma: %+v", c.Searched,
			len(got.Hits), got)
	}
	want := []codesearch.NodeGap{{Node: "git-b", Repos: codesearch.RepoSet{Count: 1, Names: []string{"beta"}}}}
	for i := range c.NotAnswering {
		c.NotAnswering[i].Why = ""
	}
	if !reflect.DeepEqual(c.NotAnswering, want) {
		t.Errorf("NotAnswering = %+v, want git-b named with beta", c.NotAnswering)
	}
	if !slices.Contains(a.asked, "alpha") {
		t.Errorf("git-a was asked %v, want alpha in the runner-up round", a.asked)
	}
}

// THE MERGE IS EXACT WHATEVER ANSWERS FIRST: three nodes' replies handed to
// the asker in every order give one answer, in (repository, path, line)
// order, and a limit cuts exactly the first lines of the whole search — what
// every repository's lines sorted together and cut would be.
//
// Mutations: keep the replies' order instead of sorting, red; cut each node's
// lines before the merge at a limit below the search's, red.
func TestTheMergeIsExactWhateverAnswersFirst(t *testing.T) {
	t.Parallel()
	var repos []repo
	var all []codesearch.Hit
	for i, name := range []string{"cobalt", "amber", "basalt", "dune", "ember", "flint"} {
		r := indexed(name, []string{"git-a", "git-b", "git-c"}[i%3])
		for j := range 4 {
			h := line(fmt.Sprintf("p%d.go", (j*7+i)%5), j+1)
			r.lines = append(r.lines, h)
		}
		slices.SortFunc(r.lines, func(x, y code.SearchHit) int {
			return strings.Compare(fmt.Sprintf("%s:%04d", x.Path, x.Line), fmt.Sprintf("%s:%04d", y.Path, y.Line))
		})
		for _, h := range r.lines {
			all = append(all, codesearch.Hit{Repo: name, SearchHit: h})
		}
		repos = append(repos, r)
	}
	slices.SortFunc(all, func(x, y codesearch.Hit) int {
		return strings.Compare(fmt.Sprintf("%s:%s:%04d", x.Repo, x.Path, x.Line),
			fmt.Sprintf("%s:%s:%04d", y.Repo, y.Path, y.Line))
	})
	e := &estate{repos: repos}
	f := newFleet(t, e, map[string]*node{"git-a": {}, "git-b": {}, "git-c": {}})
	s := searcher(t, e, f)
	perms := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	// 3 is under what amber's node holds of amber alone, so a node that cut
	// its lines below the search's limit would lose the first lines of the
	// whole search; 7 takes them from two nodes.
	for _, limit := range []int{0, 3, 7} {
		want := all
		if limit > 0 {
			want = all[:limit]
		}
		var first codesearch.Answer
		for i, p := range perms {
			f.order = func(replies [][]byte) [][]byte {
				out := make([][]byte, 0, len(replies))
				for _, k := range p {
					if k < len(replies) {
						out = append(out, replies[k])
					}
				}
				return out
			}
			q := query("needle")
			q.Limit = limit
			got, err := s.Search(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Hits, want) {
				t.Fatalf("limit %d, replies in order %v: hits %v, want %v", limit, p, got.Hits, want)
			}
			if got.Coverage.Truncated != (limit > 0) {
				t.Errorf("limit %d: Truncated %t", limit, got.Coverage.Truncated)
			}
			if i == 0 {
				first = got
			} else if !reflect.DeepEqual(got, first) {
				t.Fatalf("replies in order %v answered %+v, and in order %v %+v", p, got, perms[0], first)
			}
		}
	}
}

// A CODE QUERY IS BOUNDED AS KNOWLEDGE IS: a search's text past knowledge
// search's bound is refused before anything is listed or asked, and one at it
// is searched.
//
// Mutation: drop the grammar's check from validate, and the long query is
// searched, red.
func TestACodeQueryIsBoundedAsKnowledge(t *testing.T) {
	t.Parallel()
	r := indexed("alpha", "git-a")
	e := &estate{repos: []repo{r}}
	s := searcher(t, e, newFleet(t, e, map[string]*node{"git-a": {}}))
	_, err := s.Search(context.Background(), query(strings.Repeat("q", knowledge.MaxQueryBytes+1)))
	if !errors.Is(err, code.ErrInvalid) || !errors.Is(err, codeindex.ErrQueryTooLong) {
		t.Fatalf("a %d-byte search = %v, want ErrInvalid naming its length", knowledge.MaxQueryBytes+1, err)
	}
	if e.reads != 0 {
		t.Fatalf("a refused search read the estate %d times", e.reads)
	}
	if _, err := s.Search(context.Background(), query(strings.Repeat("q", knowledge.MaxQueryBytes))); err != nil {
		t.Fatalf("a %d-byte search = %v, want it searched", knowledge.MaxQueryBytes, err)
	}
}

// A REPOSITORY NAMED TWICE IS SEARCHED ONCE: a repo: filter naming alpha
// twice beside beta reads alpha once, answers each of its lines once and
// counts two repositories searched — every repository the search names is in
// exactly one place of the coverage, once.
//
// Mutation: read every name the filter lists, and alpha is read twice and
// counted twice, red.
func TestARepositoryNamedTwiceIsSearchedOnce(t *testing.T) {
	t.Parallel()
	alpha, beta := indexed("alpha", "git-a"), indexed("beta", "git-a")
	alpha.lines = []code.SearchHit{line("a.go", 1)}
	beta.lines = []code.SearchHit{line("b.go", 2)}
	e := &estate{repos: []repo{alpha, beta}}
	a := &node{}
	s := searcher(t, e, newFleet(t, e, map[string]*node{"git-a": a}))
	got, err := s.Search(context.Background(), codesearch.Query{Search: codeindex.Query{Text: "needle",
		Mode: codeindex.ModeLiteral, Repos: []string{"alpha", "beta", "alpha"}}, Asker: person})
	if err != nil {
		t.Fatal(err)
	}
	if got.Coverage.Searched != 2 || len(got.Hits) != 2 {
		t.Errorf("searched %d repositories with %d hits, want alpha and beta once each: %+v",
			got.Coverage.Searched, len(got.Hits), got)
	}
	slices.Sort(a.asked)
	if !slices.Equal(a.asked, []string{"alpha", "beta"}) {
		t.Errorf("git-a read %v, want alpha and beta once each", a.asked)
	}
}

// AN UNSIGNED SEARCH IS REFUSED: a request sealed under a key that is not the
// fleet's, and the same request replayed, run nothing and answer nothing; a
// request signed under a key the node does not hold runs nothing and answers
// that it could not verify it; and the fleet's own request is answered.
//
// Mutation: answer a request whatever Admit concluded, and the forged one
// reads alpha as the person it names, red.
func TestAnUnsignedSearchIsRefused(t *testing.T) {
	t.Parallel()
	r := indexed("alpha", "git-a")
	r.lines = []code.SearchHit{line("a.go", 1)}
	n := &node{e: &estate{repos: []repo{r}}}
	srv, err := codesearch.NewServer(wire(t, fleetRing), "git-a", n, shareUnits)
	if err != nil {
		t.Fatal(err)
	}
	req := codesearch.Request{Version: codesearch.Protocol, ID: "req-1", Deadline: time.Now().Add(time.Minute),
		Asker: person, Search: code.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral},
		Assign: map[string][]codesearch.Assigned{"git-a": {{RepoID: r.ref.ID, Name: "alpha", Home: true}}}}
	stranger := recordseal.OneKey("k1", "somebody else's key material")
	forged, err := wire(t, stranger).SealRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if reply, err := srv.Answer(context.Background(), forged); err == nil || reply != nil {
		t.Errorf("a request the fleet did not sign answered %q, %v", reply, err)
	}
	other := recordseal.OneKey("k2", "a key this node does not hold")
	unknown, err := wire(t, other).SealRequest(codesearch.Request{Version: req.Version, ID: "req-2",
		Deadline: req.Deadline, Asker: person, Search: req.Search, Assign: req.Assign})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := srv.Answer(context.Background(), unknown)
	if err != nil {
		t.Fatalf("a request under a key this node does not hold = %v, want an answer that says so", err)
	}
	rep, why, err := wire(t, fleetRing).OpenReply(raw, "req-2")
	if err != nil || why != "" || rep.Unverified == "" || len(rep.Repos) != 0 {
		t.Errorf("the answer to a request under an unknown key = %+v (%q, %v), want Unverified and nothing read",
			rep, why, err)
	}
	if len(n.asked) != 0 {
		t.Fatalf("an unverified request read %v", n.asked)
	}
	sealed, err := wire(t, fleetRing).SealRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = srv.Answer(context.Background(), sealed)
	if err != nil {
		t.Fatalf("the fleet's request = %v", err)
	}
	if rep, _, _ := wire(t, fleetRing).OpenReply(raw, "req-1"); len(rep.Hits) != 1 || n.asked[0] != "alpha" {
		t.Fatalf("the fleet's request answered %+v after reading %v", rep, n.asked)
	}
	if reply, err := srv.Answer(context.Background(), sealed); err == nil || reply != nil || len(n.asked) != 1 {
		t.Errorf("a replayed request answered %q, %v after reading %v", reply, err, n.asked)
	}
}

// refusingWalk is the fake company whose keyed walk refuses every read as
// not permitted: its listing answers, its Gitlet nodes do not.
type refusingWalk struct{ *estate }

func (refusingWalk) GitRead(context.Context, code.GitQuery) (code.GitAnswer, error) {
	return code.GitAnswer{}, code.ErrNotPermitted
}

// A REFUSAL NAMES ITS REPOSITORY: a repo: filter naming a repository nobody
// declared, beside one that is, is refused as a [codesearch.RepoRefusal]
// naming the undeclared one and wrapping the read's own refusal; and a search
// of the one repository whose read refuses it names that repository too —
// what a surface answering the refusal says the name with.
//
// Mutation: wrap the listing's refusal in a plain error, or the walk's, and
// the name is lost, red.
func TestARefusalNamesItsRepository(t *testing.T) {
	t.Parallel()
	e := &estate{repos: []repo{indexed("alpha", "git-a")}}
	s := searcher(t, e, newFleet(t, e, map[string]*node{"git-a": {}}))
	_, err := s.Search(context.Background(), codesearch.Query{Search: codeindex.Query{Text: "needle",
		Mode: codeindex.ModeLiteral, Repos: []string{"alpha", "nope"}}, Asker: person})
	var refused *codesearch.RepoRefusal
	if !errors.As(err, &refused) || refused.Repo != "nope" || !errors.Is(err, code.ErrRepoNotFound) {
		t.Fatalf("a search naming an undeclared repository = %v, want a RepoRefusal of nope, not found", err)
	}

	walk, err := codesearch.New(codesearch.Options{Estate: refusingWalk{e}, Placement: e,
		Ask: newFleet(t, e, map[string]*node{"git-a": {}}).ask, Wire: wire(t, fleetRing)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = walk.Search(context.Background(), codesearch.Query{Search: codeindex.Query{Text: "needle",
		Mode: codeindex.ModeLiteral, Repos: []string{"alpha"}}, Asker: person})
	refused = nil
	if !errors.As(err, &refused) || refused.Repo != "alpha" || !errors.Is(err, code.ErrNotPermitted) {
		t.Fatalf("a one-repository search its walk refused = %v, want a RepoRefusal of alpha, not permitted", err)
	}
}

// walk is s's search q read a page of limit lines at a time, each page asked
// after the last line of the page before, until a page says nothing follows:
// every line every page answered, in order, and how many pages.
func walk(t *testing.T, s *codesearch.Searcher, q codesearch.Query, limit int) ([]codesearch.Hit, int) {
	t.Helper()
	q.Limit = limit
	var all []codesearch.Hit
	for page := 1; page <= 20; page++ {
		a, err := s.Search(context.Background(), q)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		all = append(all, a.Hits...)
		if len(a.Hits) == 0 || !a.Coverage.Truncated {
			return all, page
		}
		last := a.Hits[len(a.Hits)-1]
		key := codesearch.KeyOf(last, a.Tips[last.Repo])
		q.After = &key
	}
	t.Fatalf("twenty pages of %v did not end", q.Search)
	return nil, 0
}

// lines is r with six lines, three of each of two files.
func lines(r repo) repo {
	for _, p := range []string{"a.go", "b.go"} {
		for n := range 3 {
			r.lines = append(r.lines, line(p, n+1))
		}
	}
	return r
}

// hitsOf is every line of rs, in the search's order.
func hitsOf(rs ...repo) []codesearch.Hit {
	var out []codesearch.Hit
	for _, r := range rs {
		for _, h := range r.lines {
			out = append(out, codesearch.Hit{Repo: r.ref.Name, SearchHit: h})
		}
	}
	return out
}

// A PAGE CONTINUES AFTER ITS KEY (codesearch.Key): a search read two lines a
// page answers every line once and in order — a scatter of three
// repositories over two nodes, and the walk of the one repository a search
// names — and each page asks only what follows its key: no repository before
// the key's is asked again, the key's own is read at the key's commit and
// every one after it at its ref. A page of a search of several repositories
// is a scatter, whichever repository its key leaves; and a key naming no
// commit is refused before anything is asked.
//
// Mutations: ask every repository on every page (no cut by the key's
// repository) — amber is asked on every page, red; read the key's repository
// at the search's ref (continueIn's Ref) — amber is asked at its branch,
// red; drop the key's place (continueIn's After) — a page repeats lines and
// the walk never ends, red; decide one repository or a scatter after the
// key's cut — the two-repository search's last page is read by the walk,
// red; drop the tip's check — a key naming a branch is searched, red.
func TestAPageContinuesAfterItsKey(t *testing.T) {
	t.Parallel()
	amber, basalt, cobalt := lines(indexed("amber", "git-a")), lines(indexed("basalt", "git-b")),
		lines(indexed("cobalt", "git-a"))
	e := &estate{repos: []repo{amber, basalt, cobalt}}
	a := &node{}
	s := searcher(t, e, newFleet(t, e, map[string]*node{"git-a": a, "git-b": {}}))

	got, pages := walk(t, s, query("needle"), 2)
	if want := hitsOf(amber, basalt, cobalt); !reflect.DeepEqual(got, want) || pages != 9 {
		t.Fatalf("two lines a page answered %d pages of %v, want 9 of %v", pages, got, want)
	}
	// amber's six lines are three pages, and the fourth, keyed by amber's
	// last line, asks it once more — at the commit those lines were read at.
	pinned := "amber@" + tip(amber)
	wantAmber := []string{"amber@", pinned, pinned, pinned}
	var askedAmber, askedCobalt []string
	for _, at := range a.at {
		switch {
		case strings.HasPrefix(at, "amber@"):
			askedAmber = append(askedAmber, at)
		case strings.HasPrefix(at, "cobalt@"):
			askedCobalt = append(askedCobalt, at)
		}
	}
	if !reflect.DeepEqual(askedAmber, wantAmber) {
		t.Errorf("amber was asked %v, want %v: never again once a page's key left it", askedAmber, wantAmber)
	}
	if len(askedCobalt) == 0 || askedCobalt[0] != "cobalt@" {
		t.Errorf("cobalt was asked %v, want its ref first: no line of it was answered before", askedCobalt)
	}

	one := query("needle")
	one.Search.Repos = []string{"basalt"}
	if got, _ := walk(t, s, one, 2); !reflect.DeepEqual(got, hitsOf(basalt)) {
		t.Errorf("the walk of basalt two lines a page answered %v, want %v", got, hitsOf(basalt))
	}
	walked := len(e.at)
	two := query("needle")
	two.Search.Repos = []string{"amber", "basalt"}
	if got, _ := walk(t, s, two, 4); !reflect.DeepEqual(got, hitsOf(amber, basalt)) {
		t.Errorf("amber and basalt four lines a page answered %v, want %v", got, hitsOf(amber, basalt))
	}
	if len(e.at) != walked {
		t.Errorf("a search of two repositories was read by the walk on a later page: %v", e.at[walked:])
	}

	bad := query("needle")
	bad.After = &codesearch.Key{Repo: "amber", Tip: "main", SearchAfter: code.SearchAfter{Path: "a.go", Line: 1}}
	if _, err := s.Search(context.Background(), bad); !errors.Is(err, code.ErrInvalid) {
		t.Errorf("a key naming a branch for its commit = %v, want ErrInvalid", err)
	}
}

// A REPOSITORY THAT STOPPED EARLY ENDS THE MERGE: amber's search stops after
// one line on every page (its answer's bound, or its budget), and basalt, on
// another node, answers its every line; a page keeps no line of basalt past
// amber's last, so the next page, asked after that line, reads amber on —
// and paging answers every line of both, once and in order. A repository
// whose budget ran out before its first line ends nothing: the merge goes on
// past it, saying lines may be missing.
//
// Mutation: drop the stopped repositories' ends from the merge (scatter's
// ends.stopped) — the first page answers basalt's lines after amber's first,
// the next page is asked after basalt's last, and amber's other lines are
// answered on no page, red.
func TestARepositoryThatStoppedEarlyEndsTheMerge(t *testing.T) {
	t.Parallel()
	amber, basalt := lines(indexed("amber", "git-a")), lines(indexed("basalt", "git-b"))
	amber.stops, amber.stopAt = true, 1
	e := &estate{repos: []repo{amber, basalt}}
	s := searcher(t, e, newFleet(t, e, map[string]*node{"git-a": {}, "git-b": {}}))
	first, err := s.Search(context.Background(), query("needle"))
	if err != nil {
		t.Fatal(err)
	}
	if want := hitsOf(amber)[:1]; !reflect.DeepEqual(first.Hits, want) || !first.Coverage.Truncated {
		t.Errorf("the first page answered %v (truncated %t), want amber's first line alone, truncated",
			first.Hits, first.Coverage.Truncated)
	}
	if got, _ := walk(t, s, query("needle"), 0); !reflect.DeepEqual(got, hitsOf(amber, basalt)) {
		t.Errorf("paging answered %v, want %v", got, hitsOf(amber, basalt))
	}

	cobalt, dune := lines(indexed("cobalt", "git-a")), lines(indexed("dune", "git-b"))
	amber.stopAt = 0
	e = &estate{repos: []repo{amber, cobalt, dune}}
	s = searcher(t, e, newFleet(t, e, map[string]*node{"git-a": {}, "git-b": {}}))
	got, err := s.Search(context.Background(), query("needle"))
	if err != nil {
		t.Fatal(err)
	}
	if want := hitsOf(cobalt, dune); !reflect.DeepEqual(got.Hits, want) || !got.Coverage.Truncated {
		t.Errorf("past a repository with no line read answered %v (truncated %t), want %v, truncated",
			got.Hits, got.Coverage.Truncated, want)
	}
}

// A NODE HOLDS THE FIRST LIMIT LINES, NEVER EVERY ANSWER: a node assigned
// many repositories, each answering the search's whole limit of lines, reads
// them in the order of their names and folds each answer in as it returns —
// so once the first answers fill the limit, every repository named after
// them is passed over unread, and the node reads at most the reads one
// request runs at once, whatever it is assigned. The answer is the first
// repository's lines, and every other repository is named past the limit,
// by its name, however the reads interleaved.
//
// Mutations: collect every answer and cut once the last returns, and the
// node reads every repository, red; fold without cutting at the limit, and
// nothing is ever passed, red.
func TestANodeHoldsTheFirstLimitLinesNotEveryAnswer(t *testing.T) {
	t.Parallel()
	const limit = 5
	at := codesearch.ScatterReads(shareUnits)
	n := 3*at + 7
	var repos []repo
	for i := range n {
		r := indexed(fmt.Sprintf("repo-%03d", i), "git-a")
		for j := range limit {
			r.lines = append(r.lines, line(fmt.Sprintf("f%d.go", j), 1))
		}
		repos = append(repos, r)
	}
	e := &estate{repos: repos}
	a := &node{}
	s := searcher(t, e, newFleet(t, e, map[string]*node{"git-a": a}))
	q := query("needle")
	q.Limit = limit
	got, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.asked) > at {
		t.Errorf("the node read %d of its %d repositories, want at most the %d it reads at once",
			len(a.asked), n, at)
	}
	var want []codesearch.Hit
	for _, h := range repos[0].lines {
		want = append(want, codesearch.Hit{Repo: "repo-000", SearchHit: h})
	}
	if !reflect.DeepEqual(got.Hits, want) {
		t.Errorf("hits %v, want repo-000's %d lines", got.Hits, limit)
	}
	c := got.Coverage
	if c.Searched != 1 || c.PastLimit.Count != n-1 || !c.Truncated {
		t.Errorf("searched %d, past the limit %d, truncated %t; want 1, %d and true", c.Searched,
			c.PastLimit.Count, c.Truncated, n-1)
	}
}

// sharedReads is a Gitlet node's reads as query's take the node's share: each
// read's search holds a unit of it until the test lets it go, and how many
// reads have begun is counted.
type sharedReads struct {
	share   *code.GitShare
	release chan struct{}
	mu      sync.Mutex
	began   int
}

func (r *sharedReads) Read(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	r.mu.Lock()
	r.began++
	r.mu.Unlock()
	done, err := r.share.Take(ctx, q.SeatWork())
	if err != nil {
		return code.GitAnswer{}, err
	}
	defer done()
	<-r.release
	return code.GitAnswer{Kind: code.GitSearch, Search: &code.SearchAnswer{Hits: []code.SearchHit{},
		Tip: strings.Repeat("c", 40), Via: code.SearchViaIndex}}, nil
}

func (r *sharedReads) begun() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.began
}

// grep is a `search: grep` repository of the fake company.
func grep(name string, order ...string) repo {
	r := indexed(name, order...)
	r.ref.Search = code.SearchGrep
	return r
}

// A FILTER PAST THE GREP BOUND IS REFUSED UNREAD: a repo: filter naming thirty
// grep repositories is refused once more than MaxGrepRepos of their rows have
// arrived, the reads in flight cancelled and the rest never begun — not after
// a read of every name.
//
// Mutation: read every name and count the grep repositories after, and all
// thirty are read, red.
func TestAFilterPastTheGrepBoundIsRefusedUnread(t *testing.T) {
	t.Parallel()
	var repos []repo
	var names []string
	for i := range 30 {
		r := grep(fmt.Sprintf("scratch-%02d", i), "git-a")
		repos, names = append(repos, r), append(names, r.ref.Name)
	}
	e := &estate{repos: repos}
	s := searcher(t, e, newFleet(t, e, map[string]*node{"git-a": {}}))
	q := query("needle")
	q.Search.Repos = names
	if _, err := s.Search(context.Background(), q); !errors.Is(err, code.ErrInvalid) ||
		!strings.Contains(err.Error(), "searched with git grep") {
		t.Fatalf("a filter naming %d grep repositories = %v, want refused past the bound", len(names), err)
	}
	if most := codesearch.MaxGrepRepos + 1 + codesearch.ResolveReads; e.reads > most {
		t.Errorf("the refused filter read %d of its %d names, want at most %d", e.reads, len(names), most)
	}
}

// wave is the fake company whose rows a repository's read answers only once
// every one of a wave's reads has begun: a read made alone waits for company
// that never comes, and is refused.
type wave struct {
	*estate
	want int
	mu   sync.Mutex
	in   int
	all  chan struct{}
}

func (w *wave) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	if q.Kind == code.ReadRepo {
		w.mu.Lock()
		if w.in++; w.in == w.want {
			close(w.all)
		}
		w.mu.Unlock()
		select {
		case <-w.all:
		case <-time.After(10 * time.Second):
			return code.ReadAnswer{}, errors.New("the row's read began alone: no other name's began beside it")
		}
	}
	return w.estate.Read(ctx, q)
}

// A FILTER'S NAMES ARE READ BESIDE EACH OTHER: a repo: filter naming six
// repositories reads their six rows at once — one wave, whose linearizable
// reads join one barrier — never one after another, a barrier each.
//
// Mutation: read the names one at a time, and the first waits alone and is
// refused, red.
func TestAFiltersNamesAreReadBesideEachOther(t *testing.T) {
	t.Parallel()
	var repos []repo
	var names []string
	for i := range 6 {
		r := indexed(fmt.Sprintf("repo-%d", i), "git-a")
		repos, names = append(repos, r), append(names, r.ref.Name)
	}
	e := &estate{repos: repos}
	w := &wave{estate: e, want: len(names), all: make(chan struct{})}
	s, err := codesearch.New(codesearch.Options{Estate: w, Placement: e,
		Ask: newFleet(t, e, map[string]*node{"git-a": {}}).ask, Wire: wire(t, fleetRing)})
	if err != nil {
		t.Fatal(err)
	}
	q := query("needle")
	q.Search.Repos = names
	got, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("a filter of %d names = %v, want them read in one wave", len(names), err)
	}
	if got.Coverage.Searched != len(names) {
		t.Errorf("searched %d repositories, want %d", got.Coverage.Searched, len(names))
	}
}

// EVERY REPOSITORY LOOKS AT THE HEAD ITS LISTING READ: a search whose
// listing of the asker's repositories was linearizable hands the position it
// answered at to every repository's read — the scatter's on every node, and a
// one-repository search's through the walk — as the head a second look syncs
// to, so the fleet takes no barrier a repository; a listing at another level
// found no head, and hands none.
//
// Mutations: build the scatter's request without the head, and the nodes'
// reads name none, red; take a listing's position at any level for a head,
// and the stale listing's names one, red.
func TestEveryRepositoryLooksAtTheHeadItsListingRead(t *testing.T) {
	t.Parallel()
	head := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 42}
	e := &estate{repos: []repo{indexed("alpha", "git-a"), indexed("beta", "git-b"), indexed("gamma", "git-a")},
		head: head}
	a, b := &node{}, &node{}
	s := searcher(t, e, newFleet(t, e, map[string]*node{"git-a": a, "git-b": b}))
	if _, err := s.Search(context.Background(), query("needle")); err != nil {
		t.Fatal(err)
	}
	got := slices.Concat(a.heads, b.heads)
	if len(got) != 3 || slices.ContainsFunc(got, func(p statelog.Position) bool { return p != head }) {
		t.Errorf("the scatter's reads named the heads %v, want %v for each of the three", got, head)
	}
	one := query("needle")
	one.Search.Repos = []string{"beta"}
	if _, err := s.Search(context.Background(), one); err != nil {
		t.Fatal(err)
	}
	if len(e.heads) != 1 || e.heads[0] != head {
		t.Errorf("the one repository's walk named the heads %v, want %v", e.heads, head)
	}

	// A LISTING AT THE STALE LEVEL answered where its node's copy stood,
	// which is no head of the log.
	stale := &estate{repos: e.repos, head: head, level: statelog.ReadStale}
	c := &node{}
	if _, err := searcher(t, stale, newFleet(t, stale, map[string]*node{"git-a": c, "git-b": c})).Search(
		context.Background(), query("needle")); err != nil {
		t.Fatal(err)
	}
	if len(c.heads) != 3 || slices.ContainsFunc(c.heads, func(p statelog.Position) bool { return !p.IsZero() }) {
		t.Errorf("a stale listing's reads named the heads %v, want none", c.heads)
	}
}

// A SEAT'S READ FINDS A UNIT WHILE A SEARCH RUNS: a Gitlet node answering a
// search of many repositories, each of whose reads holds a unit of the node's
// seat git-work share while it searches, starts no more of them at once than
// all but one of the share's units — so a seat's ordinary read, taking its
// unit of the same share meanwhile, is granted one at once rather than
// queued behind the search's reads — and the search still reads every
// repository once the units come free.
//
// Mutation: bound a request's reads by the queue's answer slots instead of
// the share, and the search takes every unit and queues behind them, so the
// seat's read waits out the share's wait and is answered unready, red.
func TestASeatReadFindsAUnitWhileASearchRuns(t *testing.T) {
	t.Parallel()
	const readSlots = 8
	share := code.NewGitShare("git-a", readSlots, nil)
	units := share.Units()
	bound := codesearch.ScatterReads(units)
	reads := &sharedReads{share: share, release: make(chan struct{})}
	srv, err := codesearch.NewServer(wire(t, fleetRing), "git-a", reads, units)
	if err != nil {
		t.Fatal(err)
	}
	var assign []codesearch.Assigned
	for i := range 4 * units {
		assign = append(assign, codesearch.Assigned{RepoID: fmt.Sprintf("id-%02d", i), Name: fmt.Sprintf("r%02d", i),
			Home: true})
	}
	req := codesearch.Request{Version: codesearch.Protocol, ID: "req-share", Deadline: time.Now().Add(time.Minute),
		Asker: person, Search: code.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral},
		Assign: map[string][]codesearch.Assigned{"git-a": assign}}
	sealed, err := wire(t, fleetRing).SealRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	answered := make(chan []byte, 1)
	go func() {
		raw, _ := srv.Answer(context.Background(), sealed)
		answered <- raw
	}()
	deadline := time.Now().Add(30 * time.Second)
	for share.Held() < bound {
		if time.Now().After(deadline) {
			t.Fatalf("the search holds %d of the share's %d units, and never reached %d", share.Held(), units, bound)
		}
		time.Sleep(time.Millisecond)
	}
	// A READ PAST THE BOUND WOULD START AT ONCE: every one waits on nothing
	// but its slot.
	for settle := time.Now().Add(200 * time.Millisecond); time.Now().Before(settle); time.Sleep(time.Millisecond) {
		if n := reads.begun(); n > bound {
			t.Fatalf("the search began %d reads at once, past the %d of the share's %d units it may hold", n, bound,
				units)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), code.GitShareWait)
	defer cancel()
	done, err := share.Take(ctx, code.SeatWorkRead)
	if err != nil {
		t.Fatalf("a seat's read beside the search = %v, want a unit at once", err)
	}
	done()
	close(reads.release)
	rep, why, err := wire(t, fleetRing).OpenReply(<-answered, "req-share")
	if err != nil || why != "" {
		t.Fatalf("the search's reply: %v %q", err, why)
	}
	for _, r := range rep.Repos {
		if r.Outcome != codesearch.OutcomeSearched {
			t.Errorf("repository %s: %s (%s), want it searched once the units came free", r.RepoID, r.Outcome,
				r.Detail)
		}
	}
	if len(rep.Repos) != len(assign) || reads.begun() != len(assign) {
		t.Errorf("the reply names %d repositories and %d reads began, want %d each", len(rep.Repos), reads.begun(),
			len(assign))
	}
}
