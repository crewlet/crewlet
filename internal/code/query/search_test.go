package query_test

import (
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/statelog"
)

// searchQ is a search of repo by the fixture's person.
func searchQ(repo string, s code.SearchQuery) code.GitQuery {
	return code.GitQuery{Kind: code.GitSearch, RepoID: repo, Asker: person, Home: true, Search: &s}
}

// grepOnly is a node's cache keeping no code index and no memo: every search
// a server over it answers is read with git grep, by a run of its own.
type grepOnly struct{ query.Cache }

func (grepOnly) IndexTip(cache.Synced) (cache.IndexedTip, bool)      { return cache.IndexedTip{}, false }
func (grepOnly) CodeIndex(cache.Synced) (*cache.CodeIndexView, bool) { return nil, false }
func (grepOnly) MemoGet(cache.Synced, cache.MemoKey) ([]byte, bool)  { return nil, false }
func (grepOnly) Memo(cache.Synced) *cache.MemoBatch                  { return nil }

// building is a node's cache on one of the repository's index holders whose
// indexer has not yet indexed the tip: it keeps no index, and says it holds
// one — the first of the holders, with a second after it, where first is set.
type building struct {
	query.Cache
	first bool
}

func (b building) IndexTip(cache.Synced) (cache.IndexedTip, bool) {
	return cache.IndexedTip{Holder: true, PassesOn: b.first, Files: -1}, true
}
func (building) CodeIndex(cache.Synced) (*cache.CodeIndexView, bool) { return nil, false }

// searchFixture is a fixture whose default branch holds what a search reads
// and passes over: matches in two directories, a line only Unicode's case
// folding matches, a binary and a file past code.MaxIndexedFileBytes that
// both hold the text searched, a file holding none of it, and a line of the
// syntax of git's expressions, which a search reads as text.
func searchFixture(t *testing.T) (*fixture, string, string) {
	t.Helper()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	big := strings.Repeat("status of a large file\n", code.MaxIndexedFileBytes/23+64)
	tip := x.commit(map[string]string{
		"a/one.go":    "package a\n\n// status is ok\nvar Status = 1\n// ſtatus, the long s\n",
		"b/two.txt":   "status first\nnothing here\nthe status again\n",
		"b/bin.dat":   "status\x00a binary holding the text\n",
		"b/large.txt": big,
		"c/three.go":  "package c\n\nfunc none() {}\n",
		"d/meta.txt":  "x = count+1 (a|b)\n",
	}, "files a search reads and passes over")
	return x, x.repo(codetest.RepoName), tip
}

// codeIndexed builds the first git node's code index of repo at its default
// branch's tip.
func (x *fixture) codeIndexed(repo string) {
	x.t.Helper()
	c := x.f.Nodes()[0].Cache()
	s, err := c.Sync(x.t.Context(), repo, codetest.Linearizable)
	if err != nil {
		x.t.Fatalf("sync node 0's copy of %s: %v", repo, err)
	}
	defer s.Release()
	if got, err := c.Index(x.t.Context(), s); err != nil || got != cache.IndexBuilt {
		x.t.Fatalf("Index = (%q, %v), want %q", got, err, cache.IndexBuilt)
	}
}

// INDEX AND GREP ANSWER THE SAME HITS (M6-D13). A repository's default branch
// read through its code index and the same commit read with git grep answer
// one list of lines — RE2's matches, in path order and then line order — over
// a literal, an expression, case folded beyond ASCII, a path filter and a
// literal of git's expression syntax; and both pass over the binary and the
// file past the bound that hold the text.
//
// Mutation: answer the index's candidates unconfirmed (viaIndex without
// Confirm) — the index path answers every line of a candidate; drop the
// large file's lines by nothing (viaGrep's large) — the grep path answers the
// large file's lines; escape nothing of the strings git is handed
// (codeindex.Plan.Grep's ereMeta) — git reads count+1 (a|b) as an expression
// and the grep path misses its line.
func TestIndexAndGrepAnswerTheSameHits(t *testing.T) {
	t.Parallel()
	x, repo, tip := searchFixture(t)
	x.codeIndexed(repo)
	viaIndex := x.server(0, query.Options{})
	viaGrep := x.server(0, query.Options{Cache: grepOnly{x.f.Nodes()[0].Cache()}})
	for _, tc := range []struct {
		name  string
		q     code.SearchQuery
		lines []string // "path:line", the hits both answer
	}{
		{"literal", code.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral},
			[]string{"a/one.go:3", "b/two.txt:1", "b/two.txt:3"}},
		{"ignoring case", code.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral, IgnoreCase: true},
			[]string{"a/one.go:3", "a/one.go:4", "a/one.go:5", "b/two.txt:1", "b/two.txt:3"}},
		{"expression", code.SearchQuery{Text: `the (status|long) \w+`, Mode: codeindex.ModeRegex},
			[]string{"a/one.go:5", "b/two.txt:3"}},
		{"a path", code.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral, Paths: []string{"b/"}},
			[]string{"b/two.txt:1", "b/two.txt:3"}},
		{"git's syntax", code.SearchQuery{Text: "count+1 (a|b)", Mode: codeindex.ModeLiteral},
			[]string{"d/meta.txt:1"}},
		{"nothing", code.SearchQuery{Text: "absent from every file", Mode: codeindex.ModeLiteral}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := mustRead(t, viaIndex, searchQ(repo, tc.q)).Search
			g := mustRead(t, viaGrep, searchQ(repo, tc.q)).Search
			if a.Via != code.SearchViaIndex || g.Via != code.SearchViaGrep {
				t.Fatalf("the two reads went via %q and %q, want the index and grep", a.Via, g.Via)
			}
			if a.Tip != tip || g.Tip != tip || a.Ref != "refs/heads/main" {
				t.Errorf("the reads searched %s and %s (%s), want main's tip %s", a.Tip, g.Tip, a.Ref, tip)
			}
			var got []string
			for _, h := range a.Hits {
				got = append(got, h.Path+":"+strconv.Itoa(h.Line))
				if h.Blob == "" || !strings.Contains(strings.ToLower(h.Text), "tatus") &&
					!strings.Contains(h.Text, "long") && !strings.Contains(h.Text, "count+1") {
					t.Errorf("hit %+v names no blob or holds no match", h)
				}
			}
			if !reflect.DeepEqual(got, tc.lines) {
				t.Errorf("the index answers %v, want %v", got, tc.lines)
			}
			if !reflect.DeepEqual(a.Hits, g.Hits) {
				t.Errorf("the index answers %+v and git grep %+v", a.Hits, g.Hits)
			}
			if a.Truncated || g.Truncated {
				t.Errorf("a search of a few files answered truncated (index %t, grep %t)", a.Truncated,
					g.Truncated)
			}
			// The large file is passed over by its size on both paths, the
			// binary by the index alone (git grep -I passes it over unsaid).
			if wantIndex, wantGrep := skippedIn(tc.q, 2), skippedIn(tc.q, 1); a.SkippedFiles != wantIndex ||
				g.SkippedFiles != wantGrep {
				t.Errorf("the index skipped %d files and git grep %d, want %d and %d", a.SkippedFiles,
					g.SkippedFiles, wantIndex, wantGrep)
			}
		})
	}
}

// skippedIn is how many of the fixture's skipped files q's paths admit, of n
// under b/.
func skippedIn(q code.SearchQuery, n int) int {
	if len(q.Paths) == 0 || q.Paths[0] == "b/" {
		return n
	}
	return 0
}

// A SEARCH STOPS AT ITS BUDGET AND SAYS SO. A search that runs
// code.SearchBudget out while its asker still listens answers what it found
// by then, Truncated — never a refusal — and keeps nothing in the memo: its
// page is no function of its arguments, so the next search of the same
// commit reads it again and answers every hit, which is then memoized and
// answered from the memo.
//
// Mutation: answer a budget's end as the read's failure (searchRead's
// budget case) — the read is refused; memoize an answer the budget cut
// (searchRead's !budget) — the next search is answered the truncated page
// from the memo.
func TestASearchStopsAtItsBudgetAndSaysSo(t *testing.T) {
	t.Parallel()
	x, repo, _ := searchFixture(t)
	q := searchQ(repo, code.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral})
	cut := x.server(0, query.Options{})
	query.SetSearchBudget(cut, time.Nanosecond)
	a := mustRead(t, cut, q)
	if !a.Search.Truncated || len(a.Search.Hits) != 0 || a.Memo {
		t.Fatalf("a search out of its budget answered %d hits, truncated %t, from the memo %t: want none, "+
			"truncated", len(a.Search.Hits), a.Search.Truncated, a.Memo)
	}
	whole := x.server(0, query.Options{})
	b := mustRead(t, whole, q)
	if b.Search.Truncated || len(b.Search.Hits) != 3 || b.Memo {
		t.Fatalf("the next search answered %d hits, truncated %t, from the memo %t: want all 3, read anew",
			len(b.Search.Hits), b.Search.Truncated, b.Memo)
	}
	c := mustRead(t, whole, q)
	if !c.Memo || !reflect.DeepEqual(c.Search, b.Search) {
		t.Errorf("the third search answered %+v (from the memo %t), want the second's from the memo",
			c.Search, c.Memo)
	}
}

// AN UNHELD INDEX SENDS THE WALK ON (M6-D12). The first of an indexed
// repository's two index holders, asked as its walk's home, that keeps no
// index of the default branch's tip yet refuses a search of that branch as
// unready, index_building — which passes no fill
// (code.UnreadyReason.PassesFill), so the walk asks the second holder rather
// than this node read it on the slow path a moment before its index. Every
// node after it answers: the second holder building its own, the first asked
// past the home, and a node keeping none all grep at once — as the first does
// a search of any other commit, which no index answers for — so a walk always
// ends at an answer.
//
// Mutation: drop the index_building refusal (searchRead's holder case) — the
// first holder greps the default branch; refuse on any holder (Holder for
// PassesOn) — the second holder refuses; refuse past the home (drop
// r.q.Home) — the first holder asked past the home refuses.
func TestAnUnheldIndexSendsTheWalkOn(t *testing.T) {
	t.Parallel()
	x, repo, tip := searchFixture(t)
	node := x.f.Nodes()[0].Cache()
	holder := x.server(0, query.Options{Cache: building{Cache: node, first: true}})
	for _, ref := range []string{"", "main", "refs/heads/main"} {
		_, err := holder.Read(t.Context(), searchQ(repo, code.SearchQuery{Text: "status",
			Mode: codeindex.ModeLiteral, Ref: ref}))
		if !unready(err, code.UnreadyIndexBuilding) {
			t.Errorf("a holder building its index answered a search at %q with %v, want unready %s", ref, err,
				code.UnreadyIndexBuilding)
		}
		var u *code.ErrGitUnready
		if errors.As(err, &u) && u.Reason.PassesFill() {
			t.Errorf("index_building passes a fill: the walk would have the next node download a copy")
		}
	}
	at := mustRead(t, holder, searchQ(repo, code.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral,
		Ref: tip})).Search
	if at.Via != code.SearchViaGrep || len(at.Hits) != 3 || at.Ref != "" {
		t.Errorf("the holder answered a search at the commit %s via %q with %d hits (ref %q), want grep's 3",
			tip, at.Via, len(at.Hits), at.Ref)
	}
	pastHome := searchQ(repo, code.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral})
	pastHome.Home = false
	for name, read := range map[string]func() (code.GitAnswer, error){
		"the first holder past the home": func() (code.GitAnswer, error) {
			return holder.Read(t.Context(), pastHome)
		},
		"the second holder": func() (code.GitAnswer, error) {
			return x.server(0, query.Options{Cache: building{Cache: node}}).Read(t.Context(),
				searchQ(repo, code.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral}))
		},
		"a node keeping none": func() (code.GitAnswer, error) {
			return x.server(0, query.Options{}).Read(t.Context(), searchQ(repo, code.SearchQuery{Text: "status",
				Mode: codeindex.ModeLiteral}))
		},
	} {
		a, err := read()
		if err != nil || a.Search.Via != code.SearchViaGrep || len(a.Search.Hits) != 3 {
			t.Errorf("%s answered the default branch's search with %v (%+v), want grep's 3 hits", name, err,
				a.Search)
		}
	}
}

// A SEARCH'S REFUSAL IS JUDGED AT THE LOG'S HEAD, as every read's is
// (objects): a git node whose copy predates the activation that made a
// repository `visibility: company` would refuse a seat outside its unit from
// the row it holds, so it syncs once at the log's head and searches from the
// row that admits the seat. A search the head refuses too is refused after
// that one look.
//
// Mutation: judge the grant on the copy as it stands (searchTarget's first
// refusal returned at once) — the seat is refused once the repository is
// company-wide, and the refused search takes no second look.
func TestASearchRefusalIsJudgedAtTheLogsHead(t *testing.T) {
	t.Parallel()
	settings := codetest.Settings()
	settings.Visibility = code.VisibilityUnit
	x := newFixture(t, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: 2}}, settings)
	x.commit(map[string]string{"f.txt": "the status line\n"}, "one")
	repo := x.repo(codetest.RepoName)
	counting := &countingCache{Cache: grepOnly{x.f.Nodes()[1].Cache()}}
	s := x.server(1, query.Options{Cache: counting, Roster: company})
	mustRead(t, s, searchQ(repo, code.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral}))
	outside := searchQ(repo, code.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral})
	outside.Asker = seat("sales-rep")
	if _, err := s.Read(t.Context(), outside); !errors.Is(err, code.ErrNotPermitted) {
		t.Fatalf("a seat outside the unit = %v, want ErrNotPermitted", err)
	}
	if got := counting.take(); !slices.Equal(got, []statelog.ReadLevel{statelog.ReadStale, statelog.ReadStale,
		statelog.ReadLinearizable}) {
		t.Errorf("syncs %v, want the person's and the seat's as the copy stood, then the seat's at the log's "+
			"head once", got)
	}

	settings.Visibility = code.VisibilityCompany
	x.f.Declare(t, code.Declared{Name: codetest.RepoName, Settings: settings})
	a, err := s.Read(t.Context(), outside)
	if err != nil || len(a.Search.Hits) != 1 {
		t.Errorf("the same seat's search once the repository is company-wide = %v (%+v), want its one hit "+
			"from the log's head", err, a.Search)
	}
	if got := counting.take(); !slices.Equal(got, []statelog.ReadLevel{statelog.ReadStale,
		statelog.ReadLinearizable}) {
		t.Errorf("syncs %v, want the copy as it stood, then one at the log's head", got)
	}
}
