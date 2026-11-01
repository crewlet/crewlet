package codesearch_test

import (
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
	"github.com/crewlet/crewlet/internal/statelog"
)

var (
	fleetRing = statelog.OneKey("k1", "the code search suite's fleet key material")
	person    = code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ceo", Seat: "ceo"}
)

func wire(t *testing.T, ring statelog.Keyring) *codesearch.Wire {
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
}

func indexed(name string, order ...string) repo {
	return repo{ref: code.RepoRef{ID: "id-" + name, Name: name, Search: code.SearchIndexed}, order: order}
}

// estate answers the listing and the walk from the fake company.
type estate struct {
	mu    sync.Mutex
	repos []repo
	reads int
}

func (e *estate) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reads++
	switch q.Kind {
	case code.ReadRepos:
		var out code.ReadAnswer
		for _, r := range e.repos {
			out.Repos = append(out.Repos, code.RepoSummary{RepoRef: r.ref})
		}
		return out, nil
	case code.ReadRepo:
		for _, r := range e.repos {
			if r.ref.Name == q.Repo {
				return code.ReadAnswer{Repo: &code.RepoDetail{RepoSummary: code.RepoSummary{RepoRef: r.ref}}}, nil
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
	for _, r := range e.repos {
		if r.ref.ID == q.RepoID {
			return code.GitAnswer{Kind: code.GitSearch, Search: &code.SearchAnswer{Hits: r.lines, Tip: tip(r),
				Via: code.SearchViaIndex}}, nil
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

// node is one git node's reads: it answers every repository it is asked for
// with its lines, or with the error its fault names.
type node struct {
	e     *estate
	fault map[string]error // by repository name
	asked []string
	mu    sync.Mutex
}

func (n *node) Read(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	for _, r := range n.e.repos {
		if r.ref.ID != q.RepoID {
			continue
		}
		n.mu.Lock()
		n.asked = append(n.asked, r.ref.Name)
		n.mu.Unlock()
		if err := n.fault[r.ref.Name]; err != nil {
			return code.GitAnswer{}, err
		}
		hits := r.lines
		if len(hits) > q.Search.Hits() {
			hits = hits[:q.Search.Hits()]
		}
		return code.GitAnswer{Kind: code.GitSearch, Search: &code.SearchAnswer{Hits: hits, Tip: tip(r),
			Via: code.SearchViaIndex}}, nil
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

func newFleet(t *testing.T, e *estate, nodes map[string]*node) *fleet {
	t.Helper()
	f := &fleet{servers: map[string]*codesearch.Server{}, silent: map[string]bool{}}
	for id, n := range nodes {
		n.e = e
		s, err := codesearch.NewServer(wire(t, fleetRing), id, n)
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
	srv, err := codesearch.NewServer(wire(t, fleetRing), "git-a", n)
	if err != nil {
		t.Fatal(err)
	}
	req := codesearch.Request{Version: codesearch.Protocol, ID: "req-1", Deadline: time.Now().Add(time.Minute),
		Asker: person, Search: code.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral},
		Assign: map[string][]codesearch.Assigned{"git-a": {{RepoID: r.ref.ID, Name: "alpha", Home: true}}}}
	stranger := statelog.OneKey("k1", "somebody else's key material")
	forged, err := wire(t, stranger).SealRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if reply, err := srv.Answer(context.Background(), forged); err == nil || reply != nil {
		t.Errorf("a request the fleet did not sign answered %q, %v", reply, err)
	}
	other := statelog.OneKey("k2", "a key this node does not hold")
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
