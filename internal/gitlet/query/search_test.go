package query_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/query"
	"github.com/crewlet/crewlet/internal/statelog"
)

// searchQ is a search of repo by the fixture's person.
func searchQ(repo string, s gitlet.SearchQuery) gitlet.GitQuery {
	return gitlet.GitQuery{Kind: gitlet.GitSearch, RepoID: repo, Asker: person, Home: true, Search: &s}
}

// grepOnly is a node's cache keeping no code index and no memo: every search
// a server over it answers is read with git grep, by a run of its own.
type grepOnly struct{ query.Cache }

func (grepOnly) IndexTip(cache.Synced) (cache.IndexedTip, bool) { return cache.IndexedTip{}, false }
func (grepOnly) CodeIndex(context.Context, cache.Synced) (*cache.CodeIndexView, bool) {
	return nil, false
}
func (grepOnly) MemoGet(cache.Synced, cache.MemoKey) ([]byte, bool)   { return nil, false }
func (grepOnly) MemoProbe(cache.Synced, cache.MemoKey) ([]byte, bool) { return nil, false }
func (grepOnly) Memo(cache.Synced) *cache.MemoBatch                   { return nil }

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
func (building) CodeIndex(context.Context, cache.Synced) (*cache.CodeIndexView, bool) {
	return nil, false
}

// searchFixture is a fixture whose default branch holds what a search reads
// and passes over: matches in two directories, a line only Unicode's case
// folding matches, a binary and a file past gitlet.MaxIndexedFileBytes that
// both hold the text searched, a file holding none of it, a line of the
// syntax of git's expressions, which a search reads as text, and a file whose
// needles are near each other and at its unterminated last line.
func searchFixture(t *testing.T) (*fixture, string, string) {
	t.Helper()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	big := strings.Repeat("status of a large file\n", gitlet.MaxIndexedFileBytes/23+64)
	tip := x.commit(map[string]string{
		"a/one.go":    "package a\n\n// status is ok\nvar Status = 1\n// ſtatus, the long s\n",
		"b/two.txt":   "status first\nnothing here\nthe status again\n",
		"b/bin.dat":   "status\x00a binary holding the text\n",
		"b/large.txt": big,
		"c/three.go":  "package c\n\nfunc none() {}\n",
		"d/meta.txt":  "x = count+1 (a|b)\n",
		"e/near.txt":  "line 1\nneedle two\nneedle three\nline 4\nline 5\nline 6\nline 7\nline 8\nneedle and needle",
	}, "files a search reads and passes over")
	return x, x.repo(gitlettest.RepoName), tip
}

// codeIndexed builds the first Gitlet node's code index of repo at its default
// branch's tip.
func (x *fixture) codeIndexed(repo string) {
	x.t.Helper()
	c := x.f.Nodes()[0].Cache()
	s, err := c.Sync(x.t.Context(), repo, gitlettest.Linearizable)
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
// Both also answer where each line matched, the lines around each where
// context is asked, a line two near hits share carried once, and each file's
// total where per_file is asked.
//
// Mutation: answer the index's candidates unconfirmed (viaIndex without
// Confirm) — the index path answers every line of a candidate; drop the
// large file's lines by nothing (viaGrep's large) — the grep path answers the
// large file's lines; escape nothing of the strings git is handed
// (codeindex.Plan.Grep's ereMeta) — git reads count+1 (a|b) as an expression
// and the grep path misses its line. Drop the grep path's context pass, carry
// a line twice (withContext's carried), or count FileMatches over the kept
// hits alone: red.
func TestIndexAndGrepAnswerTheSameHits(t *testing.T) {
	t.Parallel()
	x, repo, tip := searchFixture(t)
	x.codeIndexed(repo)
	viaIndex := x.server(0, query.Options{})
	viaGrep := x.server(0, query.Options{Cache: grepOnly{x.f.Nodes()[0].Cache()}})
	near := []string{"e/near.txt:2", "e/near.txt:3", "e/near.txt:9"}
	for _, tc := range []struct {
		name  string
		q     gitlet.SearchQuery
		lines []string // "path:line", the hits both answer
	}{
		{"literal", gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral},
			[]string{"a/one.go:3", "b/two.txt:1", "b/two.txt:3"}},
		{"ignoring case", gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral, IgnoreCase: true},
			[]string{"a/one.go:3", "a/one.go:4", "a/one.go:5", "b/two.txt:1", "b/two.txt:3"}},
		{"expression", gitlet.SearchQuery{Text: `the (status|long) \w+`, Mode: codeindex.ModeRegex},
			[]string{"a/one.go:5", "b/two.txt:3"}},
		{"a path", gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral, Paths: []string{"b/"}},
			[]string{"b/two.txt:1", "b/two.txt:3"}},
		{"git's syntax", gitlet.SearchQuery{Text: "count+1 (a|b)", Mode: codeindex.ModeLiteral},
			[]string{"d/meta.txt:1"}},
		{"nothing", gitlet.SearchQuery{Text: "absent from every file", Mode: codeindex.ModeLiteral}, nil},
		{"context 1", gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral, Context: 1}, near},
		{"context 2", gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral, Context: 2}, near},
		{"context 3", gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral, Context: 3}, near},
		{"per file 1", gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral, PerFile: 1},
			[]string{"e/near.txt:2"}},
		{"per file and context", gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral, PerFile: 2,
			Context: 3}, []string{"e/near.txt:2", "e/near.txt:3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := mustRead(t, viaIndex, searchQ(repo, tc.q)).Search
			g := mustRead(t, viaGrep, searchQ(repo, tc.q)).Search
			if a.Via != gitlet.SearchViaIndex || g.Via != gitlet.SearchViaGrep {
				t.Fatalf("the two reads went via %q and %q, want the index and grep", a.Via, g.Via)
			}
			if a.Tip != tip || g.Tip != tip || a.Ref != "refs/heads/main" {
				t.Errorf("the reads searched %s and %s (%s), want main's tip %s", a.Tip, g.Tip, a.Ref, tip)
			}
			var got []string
			for _, h := range a.Hits {
				got = append(got, h.Path+":"+strconv.Itoa(h.Line))
				if h.Blob == "" || !strings.Contains(strings.ToLower(h.Text), "tatus") &&
					!strings.Contains(h.Text, "long") && !strings.Contains(h.Text, "count+1") &&
					!strings.Contains(h.Text, "needle") {
					t.Errorf("hit %+v names no blob or holds no match", h)
				}
				if len(h.Matches) == 0 {
					t.Errorf("hit %+v names no match", h)
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
			// The lines around each hit, each carried once, and the file's
			// total, both as each case asks them.
			for _, h := range a.Hits {
				if tc.q.Context == 0 && (h.Before != nil || h.After != nil) ||
					tc.q.PerFile == 0 && h.FileMatches != 0 || tc.q.PerFile > 0 && h.FileMatches != 3 {
					t.Errorf("hit %s:%d answered before %q, after %q, file_matches %d to a search asking "+
						"context %d and per_file %d", h.Path, h.Line, h.Before, h.After, h.FileMatches,
						tc.q.Context, tc.q.PerFile)
				}
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

// A NEAR HIT'S LINES ARE CARRIED ONCE. Three lines of context around
// e/near.txt's needles (lines 2, 3 and 9 of nine, the last unterminated)
// answer line 1 before line 2, nothing between lines 2 and 3, which are both
// hits, lines 4 to 6 after line 3, and lines 7 and 8 before line 9, never
// line 6 again; the two needles of line 9 are two matches. A per-file bound of
// one answers line 2 alone, its total 3, and lines 3 to 5 after it: a line
// past the bound is not answered, so it is a context line. A limit of one
// answers line 2 with nothing after it: line 3 is the next page's first hit,
// never this page's context. Both paths alike.
//
// Mutation: bound After by the next line that matched rather than the next
// answered (withContext's next), or let Before reach back past the line
// before's After (carried): red.
func TestANearHitsLinesAreCarriedOnce(t *testing.T) {
	t.Parallel()
	x, repo, _ := searchFixture(t)
	x.codeIndexed(repo)
	type lines struct {
		line          int
		before, after []string
		matches       [][2]int
		total         int
	}
	for _, tc := range []struct {
		name string
		q    gitlet.SearchQuery
		want []lines
	}{
		{"context 3", gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral, Context: 3}, []lines{
			{2, []string{"line 1"}, nil, [][2]int{{0, 6}}, 0},
			{3, nil, []string{"line 4", "line 5", "line 6"}, [][2]int{{0, 6}}, 0},
			{9, []string{"line 7", "line 8"}, nil, [][2]int{{0, 6}, {11, 17}}, 0},
		}},
		{"per file 1", gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral, Context: 3, PerFile: 1},
			[]lines{{2, []string{"line 1"}, []string{"needle three", "line 4", "line 5"}, [][2]int{{0, 6}}, 3}}},
		{"limit 1", gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral, Context: 3, Limit: 1},
			[]lines{{2, []string{"line 1"}, nil, [][2]int{{0, 6}}, 0}}},
	} {
		for name, s := range map[string]*query.Server{"index": x.server(0, query.Options{}),
			"grep": x.server(0, query.Options{Cache: grepOnly{x.f.Nodes()[0].Cache()}})} {
			var got []lines
			for _, h := range mustRead(t, s, searchQ(repo, tc.q)).Search.Hits {
				got = append(got, lines{h.Line, h.Before, h.After, h.Matches, h.FileMatches})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s, %s: answered %+v, want %+v", tc.name, name, got, tc.want)
			}
		}
	}
}

// A SEARCH CONTINUED AFTER A PLACE ANSWERS THE LINES PAST IT, and only those
// (gitlet.SearchQuery.After): walked a line a page, each page continued after
// the last line the one before answered, the index and git grep each answer
// every line once and in order — a place inside a file and one between
// files alike — and a second walk, answered from the memo where the node
// keeps one, answers the same, since a page is memoized under its place.
// A place past every file holding the text answers nothing, and passes over
// no file before it unsaid: the files it skips for their size or as
// binaries are behind it, never counted.
//
// Mutation: drop the place from the line check (searching.add) — the index
// answers a/one.go:3 again on every page; key the memo without the place —
// every page is the first's; count the large file before the place (viaGrep)
// — git grep says it skipped a file a page past it never read.
func TestASearchContinuedAfterAPlaceAnswersTheLinesPastIt(t *testing.T) {
	t.Parallel()
	x, repo, _ := searchFixture(t)
	x.codeIndexed(repo)
	servers := map[string]*query.Server{"index": x.server(0, query.Options{}),
		"grep": x.server(0, query.Options{Cache: grepOnly{x.f.Nodes()[0].Cache()}})}
	q := gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral, IgnoreCase: true}
	want := []string{"a/one.go:3", "a/one.go:4", "a/one.go:5", "b/two.txt:1", "b/two.txt:3"}
	for name, s := range servers {
		for walk := range 2 {
			var got []string
			page := q
			page.Limit = 1
			for range len(want) + 1 {
				a := mustRead(t, s, searchQ(repo, page)).Search
				for _, h := range a.Hits {
					got = append(got, h.Path+":"+strconv.Itoa(h.Line))
				}
				if len(a.Hits) == 0 {
					break
				}
				after := gitlet.SearchAfterOf(a.Hits[len(a.Hits)-1])
				page.After = &after
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s, walk %d: a line a page answered %v, want %v", name, walk, got, want)
			}
		}
		between := q
		between.After = &gitlet.SearchAfter{Path: "b", Line: 1}
		if a := mustRead(t, s, searchQ(repo, between)).Search; len(a.Hits) != 2 || a.Hits[0].Path != "b/two.txt" {
			t.Errorf("%s: continued after b, answered %+v, want b/two.txt's two lines", name, a.Hits)
		}
		past := q
		past.After = &gitlet.SearchAfter{Path: "c", Line: 1}
		if a := mustRead(t, s, searchQ(repo, past)).Search; len(a.Hits) != 0 || a.SkippedFiles != 0 {
			t.Errorf("%s: continued after c, answered %d hits and %d skipped files, want none of either", name,
				len(a.Hits), a.SkippedFiles)
		}
	}
}

// skippedIn is how many of the fixture's skipped files q's paths admit, of n
// under b/.
func skippedIn(q gitlet.SearchQuery, n int) int {
	if len(q.Paths) == 0 || q.Paths[0] == "b/" {
		return n
	}
	return 0
}

// A SEARCH STOPS AT ITS BUDGET AND SAYS SO. A search that runs
// gitlet.SearchBudget out while its asker still listens answers what it found
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
	q := searchQ(repo, gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral})
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
// (gitlet.UnreadyReason.PassesFill), so the walk asks the second holder rather
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
		_, err := holder.Read(t.Context(), searchQ(repo, gitlet.SearchQuery{Text: "status",
			Mode: codeindex.ModeLiteral, Ref: ref}))
		if !unready(err, gitlet.UnreadyIndexBuilding) {
			t.Errorf("a holder building its index answered a search at %q with %v, want unready %s", ref, err,
				gitlet.UnreadyIndexBuilding)
		}
		var u *gitlet.ErrGitUnready
		if errors.As(err, &u) && u.Reason.PassesFill() {
			t.Errorf("index_building passes a fill: the walk would have the next node download a copy")
		}
	}
	at := mustRead(t, holder, searchQ(repo, gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral,
		Ref: tip})).Search
	if at.Via != gitlet.SearchViaGrep || len(at.Hits) != 3 || at.Ref != "" {
		t.Errorf("the holder answered a search at the commit %s via %q with %d hits (ref %q), want grep's 3",
			tip, at.Via, len(at.Hits), at.Ref)
	}
	pastHome := searchQ(repo, gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral})
	pastHome.Home = false
	for name, read := range map[string]func() (gitlet.GitAnswer, error){
		"the first holder past the home": func() (gitlet.GitAnswer, error) {
			return holder.Read(t.Context(), pastHome)
		},
		"the second holder": func() (gitlet.GitAnswer, error) {
			return x.server(0, query.Options{Cache: building{Cache: node}}).Read(t.Context(),
				searchQ(repo, gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral}))
		},
		"a node keeping none": func() (gitlet.GitAnswer, error) {
			return x.server(0, query.Options{}).Read(t.Context(), searchQ(repo, gitlet.SearchQuery{Text: "status",
				Mode: codeindex.ModeLiteral}))
		},
	} {
		a, err := read()
		if err != nil || a.Search.Via != gitlet.SearchViaGrep || len(a.Search.Hits) != 3 {
			t.Errorf("%s answered the default branch's search with %v (%+v), want grep's 3 hits", name, err,
				a.Search)
		}
	}
}

// A SEARCH'S REFUSAL IS JUDGED AT THE LOG'S HEAD, as every read's is
// (objects): a Gitlet node whose copy predates the activation that made a
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
	settings := gitlettest.Settings()
	settings.Visibility = gitlet.VisibilityUnit
	x := newFixture(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{GitletNodes: 2}}, settings)
	x.commit(map[string]string{"f.txt": "the status line\n"}, "one")
	repo := x.repo(gitlettest.RepoName)
	counting := &countingCache{Cache: grepOnly{x.f.Nodes()[1].Cache()}}
	s := x.server(1, query.Options{Cache: counting, Roster: company})
	mustRead(t, s, searchQ(repo, gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral}))
	outside := searchQ(repo, gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral})
	outside.Asker = seat("sales-rep")
	if _, err := s.Read(t.Context(), outside); !errors.Is(err, gitlet.ErrNotPermitted) {
		t.Fatalf("a seat outside the unit = %v, want ErrNotPermitted", err)
	}
	if got := counting.take(); !slices.Equal(got, []statelog.ReadLevel{statelog.ReadStale, statelog.ReadStale,
		statelog.ReadLinearizable}) {
		t.Errorf("syncs %v, want the person's and the seat's as the copy stood, then the seat's at the log's "+
			"head once", got)
	}

	settings.Visibility = gitlet.VisibilityCompany
	x.f.Declare(t, gitlet.Declared{Name: gitlettest.RepoName, Settings: settings})
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

// A SECOND LOOK SYNCS TO THE HEAD ITS ASKER READ: a person's search naming a
// commit a Gitlet node's copy predates, and one naming a ref nobody pushed, each
// carrying the head its asker read after the push, look once more at that
// head — at session, a floor, with no barrier of the node's own — and the
// first finds its commit's line there while the second is answered not found;
// the same search with no head looks at a barrier of its own. A company-wide
// search naming a ref most repositories lack so costs the one barrier its
// listing took, not one a repository.
//
// Mutation: ignore the asker's head (lookAtHead syncing at linearizable
// whatever it names) — each look takes a barrier of its own, red.
func TestASecondLookSyncsToTheHeadItsAskerRead(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{GitletNodes: 2}}, gitlettest.Settings())
	x.commit(map[string]string{"f.txt": "the status line\n"}, "one")
	repo := x.repo(gitlettest.RepoName)
	counting := &countingCache{Cache: grepOnly{x.f.Nodes()[1].Cache()}}
	s := x.server(1, query.Options{Cache: counting})
	mustRead(t, s, searchQ(repo, gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral}))
	second := x.commit(map[string]string{"f.txt": "a later status line\n"}, "two")
	asked := treeQ(repo, gitlet.TreeQuery{Rev: second})
	asked.Fresh = gitlettest.Linearizable
	head := mustRead(t, x.server(0, query.Options{}), asked).Position
	counting.take()

	at := searchQ(repo, gitlet.SearchQuery{Text: "later", Mode: codeindex.ModeLiteral, Ref: second})
	at.Head = head
	if a := mustRead(t, s, at).Search; a.Tip != second || len(a.Hits) != 1 {
		t.Errorf("the search of %s = %+v, want its one line at it", second, a)
	}
	if got := counting.take(); !slices.Equal(got, []statelog.ReadLevel{statelog.ReadStale,
		statelog.ReadSession}) {
		t.Errorf("syncs %v, want the copy as it stood, then one to the asker's head", got)
	}

	missing := searchQ(repo, gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral, Ref: "refs/heads/nope"})
	missing.Head = head
	if _, err := s.Read(t.Context(), missing); !errors.Is(err, gitlet.ErrObjectNotFound) {
		t.Fatalf("a search of a ref nobody pushed = %v, want ErrObjectNotFound", err)
	}
	if got := counting.take(); !slices.Equal(got, []statelog.ReadLevel{statelog.ReadStale,
		statelog.ReadSession}) {
		t.Errorf("syncs %v, want the copy as it stood, then one to the asker's head", got)
	}
	missing.Head = statelog.Position{}
	if _, err := s.Read(t.Context(), missing); !errors.Is(err, gitlet.ErrObjectNotFound) {
		t.Fatalf("the same search with no head = %v, want ErrObjectNotFound", err)
	}
	if got := counting.take(); !slices.Equal(got, []statelog.ReadLevel{statelog.ReadStale,
		statelog.ReadLinearizable}) {
		t.Errorf("syncs %v, want the copy as it stood, then one at a barrier of the node's own", got)
	}
}

// A SEARCH HIT NAMES WHERE ITS LINE MATCHED, on both paths alike: a literal
// twice in a line is two matches, a case-folded match is where it folded, and
// a match after a run of invalid bytes is where it reads in the text answered,
// the run one replacement character of three bytes.
//
// Mutation: answer the raw line's offsets (hitText), red on the invalid run.
func TestASearchHitNamesWhereItsLineMatched(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	x.commit(map[string]string{"m.txt": "abc xyz abc\nxx ABC\n\xff\xfe abc\n"}, "lines matched in several places")
	repo := x.repo(gitlettest.RepoName)
	x.codeIndexed(repo)
	want := map[int][][2]int{1: {{0, 3}, {8, 11}}, 2: {{3, 6}}, 3: {{4, 7}}}
	for name, s := range map[string]*query.Server{"index": x.server(0, query.Options{}),
		"grep": x.server(0, query.Options{Cache: grepOnly{x.f.Nodes()[0].Cache()}})} {
		a := mustRead(t, s, searchQ(repo, gitlet.SearchQuery{Text: "abc", Mode: codeindex.ModeLiteral,
			IgnoreCase: true})).Search
		if len(a.Hits) != len(want) {
			t.Fatalf("%s answered %+v, want 3 lines", name, a.Hits)
		}
		for _, h := range a.Hits {
			if !reflect.DeepEqual(h.Matches, want[h.Line]) {
				t.Errorf("%s: line %d (%q) matched at %v, want %v", name, h.Line, h.Text, h.Matches, want[h.Line])
			}
		}
	}
}

// A PER-FILE SEARCH COUNTS EVERY LINE OF A FILE. Two lines a file at the most
// answers the first two of a file of seven and the one of a file of one, each
// naming its file's total, untruncated; a page continued after the first
// file's first line answers its second, still naming 7, since the lines before
// the place count in the total. Both paths alike. A grep whose git ends after
// the file's third line answers its two lines with no total, Truncated: a
// count short of the file's is never answered.
//
// Mutation: count from the place rather than the file's first line (drop the
// place's own file's lines before add on the grep path, or count after the
// answered check); leave the file a cut grep stopped in uncut (viaGrep): red.
func TestAPerFileSearchCountsEveryLineOfAFile(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	x.commit(map[string]string{"f/a.txt": strings.Repeat("hit\n", 7), "f/b.txt": "hit\n"}, "files of seven and one")
	repo := x.repo(gitlettest.RepoName)
	x.codeIndexed(repo)
	q := gitlet.SearchQuery{Text: "hit", Mode: codeindex.ModeLiteral, PerFile: 2, Limit: 3}
	for name, s := range map[string]*query.Server{"index": x.server(0, query.Options{}),
		"grep": x.server(0, query.Options{Cache: grepOnly{x.f.Nodes()[0].Cache()}})} {
		lines := func(a *gitlet.SearchAnswer) []string {
			var out []string
			for _, h := range a.Hits {
				out = append(out, h.Path+":"+strconv.Itoa(h.Line)+"/"+strconv.Itoa(h.FileMatches))
			}
			return out
		}
		a := mustRead(t, s, searchQ(repo, q)).Search
		if got, want := lines(a), []string{"f/a.txt:1/7", "f/a.txt:2/7", "f/b.txt:1/1"}; !reflect.DeepEqual(got,
			want) || a.Truncated {
			t.Errorf("%s answered %v (truncated %t), want %v untruncated", name, got, a.Truncated, want)
		}
		page := q
		page.After = &gitlet.SearchAfter{Path: "f/a.txt", Line: 1}
		b := mustRead(t, s, searchQ(repo, page)).Search
		if got, want := lines(b), []string{"f/a.txt:2/7", "f/b.txt:1/1"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s continued after f/a.txt:1 answered %v, want %v", name, got, want)
		}
	}
	cut := x.server(0, query.Options{Git: cutGrep{Pool: x.f.Nodes()[0].Pool(), lines: 3},
		Cache: grepOnly{x.f.Nodes()[0].Cache()}})
	a := mustRead(t, cut, searchQ(repo, q)).Search
	var got []string
	for _, h := range a.Hits {
		got = append(got, h.Path+":"+strconv.Itoa(h.Line)+"/"+strconv.Itoa(h.FileMatches))
	}
	if want := []string{"f/a.txt:1/0", "f/a.txt:2/0"}; !reflect.DeepEqual(got, want) || !a.Truncated {
		t.Errorf("a grep cut after three lines answered %v (truncated %t), want %v truncated", got, a.Truncated, want)
	}
}

// cutGrep is a query.Pool whose git grep passes on its first lines alone and
// then ends as a process the read's git budget ended.
type cutGrep struct {
	query.Pool
	lines int
}

func (p cutGrep) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if c.Args[0] != "grep" {
		return p.Pool.Run(ctx, c)
	}
	var all bytes.Buffer
	out := c.Stdout
	c.Stdout = &all
	res, err := p.Pool.Run(ctx, c)
	if err != nil {
		return res, err
	}
	recs := bytes.SplitAfter(all.Bytes(), []byte{'\n'})
	for _, r := range recs[:min(p.lines, len(recs))] {
		if _, err := out.Write(r); err != nil {
			return res, err
		}
	}
	return res, fmt.Errorf("git grep ran past its deadline: %w", gitproc.ErrDeadline)
}

// A CONTEXT PASS CUT SHORT ANSWERS THE FILES IT READ. A grep search asking
// context reads its hits' files after git grep ends (withHeldContext); a pass
// that the read's git running out, or its answer falling due, ends after its
// first file answers that file's hit with its context, Truncated, and
// memoizes nothing: the next search reads anew and answers every hit.
//
// Mutation: answer a pass the due instant cut as the read's failure (drop
// passCut from searchRead's budget case), or memoize a cut pass (searchRead's
// !budget), red.
func TestAContextPassCutShortAnswersTheFilesItRead(t *testing.T) {
	t.Parallel()
	x, repo, _ := searchFixture(t)
	q := searchQ(repo, gitlet.SearchQuery{Text: "status", Mode: codeindex.ModeLiteral, Context: 1})
	lines := func(a gitlet.GitAnswer) []string {
		var out []string
		for _, h := range a.Search.Hits {
			out = append(out, h.Path+":"+strconv.Itoa(h.Line)+":"+strings.Join(h.After, "|"))
		}
		return out
	}
	for _, due := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if due {
			// Two seconds on: far past a warm grep of the fixture, so the
			// instant falls during the pass.
			ctx, cancel = gitlet.AnswerBy(t.Context(), time.Now().Add(gitlet.ReplyMargin+2*time.Second))
		}
		s := x.server(0, query.Options{Git: &cutPass{Pool: x.f.Nodes()[0].Pool(), due: due}})
		a, err := s.Read(ctx, q)
		cancel()
		if err != nil {
			t.Fatalf("a search whose context pass was cut (due %t) = %v, want the files it read", due, err)
		}
		if got := lines(a); !reflect.DeepEqual(got, []string{"a/one.go:3:var Status = 1"}) ||
			!a.Search.Truncated || a.Memo {
			t.Errorf("a search whose context pass was cut (due %t) answered %v, truncated %t, from the memo %t: "+
				"want a/one.go's hit with its context, truncated", due, got, a.Search.Truncated, a.Memo)
		}
	}
	b := mustRead(t, x.server(0, query.Options{}), q)
	if got := lines(b); len(got) != 3 || b.Search.Truncated || b.Memo {
		t.Errorf("the next search answered %v, truncated %t, from the memo %t: want all 3, read anew", got,
			b.Search.Truncated, b.Memo)
	}
}

// cutPass is a query.Pool whose cat-file after a git grep (the grep path's
// context pass) reads the first blob asked, then ends: at the read's due
// instant where due is set, otherwise as a process the read's git budget ended.
type cutPass struct {
	query.Pool
	due     bool
	mu      sync.Mutex
	grepped bool
}

func (p *cutPass) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	p.mu.Lock()
	pass := p.grepped && c.Args[0] == "cat-file"
	p.grepped = p.grepped || c.Args[0] == "grep"
	p.mu.Unlock()
	if !pass {
		return p.Pool.Run(ctx, c)
	}
	first, err := bufio.NewReader(c.Stdin).ReadString('\n')
	if err != nil {
		return gitproc.Result{}, err
	}
	c.Stdin = strings.NewReader(first)
	res, err := p.Pool.Run(ctx, c)
	switch {
	case err != nil:
		return res, err
	case !p.due:
		return res, fmt.Errorf("git cat-file ran past its deadline: %w", gitproc.ErrDeadline)
	}
	<-ctx.Done()
	return res, context.Cause(ctx)
}

// A SEARCH ANSWER HOLDS ITS BOUND (gitlet.MaxGitAnswerBytes), on both paths:
// hundreds of matching lines of gitlet.MaxSearchLineBytes, each with more
// matches than a hit names and a line between each two, answer at most the
// bound of JSON, Truncated, their hits the first lines of the search's order;
// so does the same search asking three lines of context. And a search asking
// context with no limit answers at most gitlet.MaxSearchContextHits.
//
// Mutation: weigh a hit before its context is attached (flush's emit before
// withContext), and the answer passes the bound, red.
func TestASearchAnswerHoldsItsBound(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	hit := strings.Repeat("needle ", 36) + "abcd"
	filler := strings.Repeat("f", gitlet.MaxSearchLineBytes)
	var long, short strings.Builder
	for range 600 {
		long.WriteString(hit + "\n" + filler + "\n")
		short.WriteString("needle\nother\n")
	}
	x.commit(map[string]string{"long.txt": long.String(), "short.txt": short.String()}, "many long matches")
	repo := x.repo(gitlettest.RepoName)
	x.codeIndexed(repo)
	for name, s := range map[string]*query.Server{"index": x.server(0, query.Options{}),
		"grep": x.server(0, query.Options{Cache: grepOnly{x.f.Nodes()[0].Cache()}})} {
		for _, around := range []int{0, 3} {
			a := mustRead(t, s, searchQ(repo, gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral,
				Paths: []string{"long.txt"}, Context: around}))
			raw, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) > gitlet.MaxGitAnswerBytes || !a.Search.Truncated || len(a.Search.Hits) == 0 {
				t.Errorf("%s, context %d: %d bytes, %d hits, truncated %t: want within %d, truncated", name, around,
					len(raw), len(a.Search.Hits), a.Search.Truncated, gitlet.MaxGitAnswerBytes)
			}
			for i, h := range a.Search.Hits {
				if h.Line != 2*i+1 || len(h.Matches) != gitlet.MaxSearchHitMatches ||
					around > 0 && len(h.After) != 1 {
					t.Errorf("%s, context %d: hit %d is line %d with %d matches and after %d lines, want line "+
						"%d, %d matches and its filler", name, around, i, h.Line, len(h.Matches), len(h.After),
						2*i+1, gitlet.MaxSearchHitMatches)
					break
				}
			}
		}
		a := mustRead(t, s, searchQ(repo, gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral,
			Paths: []string{"short.txt"}, Context: 1})).Search
		if len(a.Hits) != gitlet.MaxSearchContextHits || !a.Truncated {
			t.Errorf("%s: a search asking context answered %d hits (truncated %t), want %d, truncated", name,
				len(a.Hits), a.Truncated, gitlet.MaxSearchContextHits)
		}
	}
}

// A SEARCH'S PAGE IS MEMOIZED UNDER ITS CONTEXT AND ITS PER-FILE BOUND: the
// same search asking context, or another bound, is another answer, read anew,
// never the first's page from the memo.
//
// Mutation: key the page without Context or PerFile (memoKey), red.
func TestASearchsContextAndBoundKeyItsMemo(t *testing.T) {
	t.Parallel()
	x, repo, _ := searchFixture(t)
	x.codeIndexed(repo)
	s := x.server(0, query.Options{})
	q := gitlet.SearchQuery{Text: "needle", Mode: codeindex.ModeLiteral}
	mustRead(t, s, searchQ(repo, q))
	if a := mustRead(t, s, searchQ(repo, q)); !a.Memo {
		t.Fatal("the same search was not answered from the memo: this test asserts about nothing")
	}
	for _, other := range []gitlet.SearchQuery{{Text: "needle", Mode: codeindex.ModeLiteral, Context: 2},
		{Text: "needle", Mode: codeindex.ModeLiteral, PerFile: 1}, {Text: "needle", Mode: codeindex.ModeLiteral,
			PerFile: 2}} {
		if a := mustRead(t, s, searchQ(repo, other)); a.Memo {
			t.Errorf("a search asking context %d, per_file %d was answered the plain search's page from the memo",
				other.Context, other.PerFile)
		}
	}
}
