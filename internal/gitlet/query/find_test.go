package query_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// findQ is a person's find of query over rev's files in repo, asked as the
// other reads here are, answering at most limit (0 is the default).
func findQ(repo, rev, query string, limit int) gitlet.GitQuery {
	return gitlet.GitQuery{Kind: gitlet.GitFind, RepoID: repo, Asker: person, Home: true,
		Find: &gitlet.FindQuery{Rev: rev, Query: query, Limit: limit}}
}

// findOf is s's find of q, failing the case where it answers no find.
func findOf(t *testing.T, s *query.Server, q gitlet.GitQuery) *gitlet.FindAnswer {
	t.Helper()
	a := mustRead(t, s, q)
	if a.Kind != gitlet.GitFind || a.Find == nil {
		t.Fatalf("the find of %q answered %+v, want a find", q.Find.Query, a)
	}
	return a.Find
}

// foundPaths is each match's path, in the answer's order.
func foundPaths(f *gitlet.FindAnswer) []string {
	out := make([]string, len(f.Matches))
	for i, m := range f.Matches {
		out[i] = m.Path
	}
	return out
}

// A FIND RANKS THE BASENAME FIRST AND BY CONTIGUITY, each query answering in
// the rank's order with its spans; of three files tied but for their bytes and
// a better one after them, the two best are the better one and the first.
//
// Mutations: drop the tier, the gaps, the start, the excess or the length
// from the rank, or put the bytes before the length → red; drop the bytes
// from the rank → the tie evicted is the first, red.
func TestAFindRanksTheBasenameFirstAndByContiguity(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	for _, p := range []string{"cmd/web/main.go", "src/domain.go", "main/readme.md", "x/main.go", "main.go.orig",
		"readme.md", "rdm.txt", "rxdxm", "a/rdm.md", "aa/rdm.md", "b/rdm.md", "ties/a/qx.md", "ties/b/qx.md",
		"ties/c/qx.md", "ties/zz/qx.m"} {
		files[p] = p + "\n"
	}
	head := x.commit(files, "files")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})

	type found struct {
		path  string
		spans [][2]int
	}
	for _, tc := range []struct {
		query string
		want  []found
	}{
		{"main", []found{{"x/main.go", [][2]int{{2, 6}}}, {"cmd/web/main.go", [][2]int{{8, 12}}},
			{"main.go.orig", [][2]int{{0, 4}}}, {"src/domain.go", [][2]int{{6, 10}}},
			{"main/readme.md", [][2]int{{0, 4}}}}},
		{"main.go", []found{{"x/main.go", [][2]int{{2, 9}}}, {"cmd/web/main.go", [][2]int{{8, 15}}},
			{"main.go.orig", [][2]int{{0, 7}}}, {"src/domain.go", [][2]int{{6, 13}}}}},
		{"rdm", []found{{"a/rdm.md", [][2]int{{2, 5}}}, {"b/rdm.md", [][2]int{{2, 5}}},
			{"aa/rdm.md", [][2]int{{3, 6}}}, {"rdm.txt", [][2]int{{0, 3}}},
			{"rxdxm", [][2]int{{0, 1}, {2, 3}, {4, 5}}}, {"readme.md", [][2]int{{0, 1}, {3, 5}}},
			{"main/readme.md", [][2]int{{5, 6}, {8, 10}}},
			{"src/domain.go", [][2]int{{1, 2}, {4, 5}, {6, 7}}}}},
	} {
		f := findOf(t, s, findQ(repo, head, tc.query, 0))
		var got []found
		for _, m := range f.Matches {
			got = append(got, found{m.Path, m.Spans})
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("the find of %q answered\n%v\nwant\n%v", tc.query, got, tc.want)
		}
		if f.Total != len(tc.want) {
			t.Errorf("the find of %q counts %d matches, want %d", tc.query, f.Total, len(tc.want))
		}
	}

	ties := strings.TrimSpace(x.git("rev-parse", head+":ties"))
	f := findOf(t, s, findQ(repo, ties, "qx", 2))
	if got := foundPaths(f); !slices.Equal(got, []string{"zz/qx.m", "a/qx.md"}) || f.Total != 4 || f.Tree != ties {
		t.Errorf("the find of qx in tree %s answered %v of %d in %s, want zz/qx.m and a/qx.md of 4", ties, got,
			f.Total, f.Tree)
	}
}

// A FIND FOLDS CASE AS strings.EqualFold DOES: README finds readme.md, `k`
// finds U+212A KELVIN SIGN, and `ß` neither finds `ss` nor is found by it.
//
// Mutations: fold the query alone (lower-case it, the paths as they are) →
// license finds nothing, red; fold ß to ss, as full case mapping does →
// STRASS finds straße.md, red.
func TestAFindFoldsCaseAsEqualFoldDoes(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	head := x.commit(map[string]string{"readme.md": "r\n", "LICENSE": "l\n", "\u212Aelvin.txt": "k\n",
		"strasse.txt": "s\n", "straße.md": "s\n"}, "files")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"README", []string{"readme.md"}},
		{"license", []string{"LICENSE"}},
		{"kel", []string{"\u212Aelvin.txt"}},
		{"KELVIN", []string{"\u212Aelvin.txt"}},
		{"straß", []string{"straße.md"}},
		{"STRASS", []string{"strasse.txt"}},
	} {
		if got := foundPaths(findOf(t, s, findQ(repo, head, tc.query, 0))); !slices.Equal(got, tc.want) {
			t.Errorf("the find of %q answered %q, want %q", tc.query, got, tc.want)
		}
	}
}

// A FIND MATCHES NO BYTE THAT IS NOT TEXT: such a run is one position, found
// by the characters around it and by neither its Latin-1 reading nor U+FFFD.
//
// Mutations: decode an invalid byte as U+FFFD and match it → the U+FFFD
// query finds both, red; count each invalid byte as a position → the second
// file's spans are [[0,1],[3,4]], red.
func TestAFindMatchesNoByteThatIsNotText(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	head := x.commit(map[string]string{"caf\xe9.txt": "latin-1\n", "a\xff\xfeb.txt": "a run\n"}, "files")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})

	f := findOf(t, s, findQ(repo, head, "caf", 0))
	want := gitlet.FindMatch{Path: "caf\uFFFD.txt", PathB64: "Y2Fm6S50eHQ=", Mode: "100644", Spans: [][2]int{{0, 3}}}
	if len(f.Matches) != 1 {
		t.Fatalf("the find of caf answered %+v, want the Latin-1 file", f.Matches)
	}
	got := f.Matches[0]
	got.OID = ""
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the find of caf answered %+v, want %+v", got, want)
	}
	f = findOf(t, s, findQ(repo, head, "ab", 0))
	if len(f.Matches) != 1 || f.Matches[0].Path != "a\uFFFDb.txt" ||
		!reflect.DeepEqual(f.Matches[0].Spans, [][2]int{{0, 1}, {2, 3}}) {
		t.Errorf("the find of ab answered %+v, want a\uFFFDb.txt with spans [[0,1],[2,3]]", f.Matches)
	}
	for _, q := range []string{"café", "\uFFFD"} {
		if f := findOf(t, s, findQ(repo, head, q, 0)); len(f.Matches) != 0 || f.Total != 0 || f.Files != 2 {
			t.Errorf("the find of %q answered %+v (%d of %d files), want nothing of 2", q, f.Matches, f.Total,
				f.Files)
		}
	}
}

// A FIND KEEPS ITS LIMIT AND COUNTS EVERY MATCH: 120 of 300 files match, the
// best 50 (or 100) are answered in rank order, a link is a file and a
// submodule is not, and an invalid find is refused as it is validated.
//
// Mutations: count a submodule as a file → files is 301, red; keep the first
// matches the listing passes rather than the best → the link is missing, red;
// answer every match → 120, red.
func TestAFindKeepsItsLimitAndCountsEveryMatch(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	groups := [3][]string{}
	for i := range 119 {
		name := fmt.Sprintf("%s-%03d.txt", []string{"hit", "xhit", "h-i-t"}[i%3], i)
		files[name] = "a hit\n"
		groups[i%3] = append(groups[i%3], name)
	}
	for i := range 180 {
		files[fmt.Sprintf("miss-%03d.txt", i)] = "a miss\n"
	}
	x.commit(files, "files")
	if err := os.Symlink("hit-000.txt", filepath.Join(x.work, "hit-link")); err != nil {
		t.Fatal(err)
	}
	x.git("add", "-A")
	x.git("update-index", "--add", "--cacheinfo", "160000,"+strings.TrimSpace(x.git("rev-parse", "HEAD"))+",hit-sub")
	x.git("commit", "-q", "-m", "a link and a submodule")
	x.push(gitlettest.RepoName, "main")
	head := strings.TrimSpace(x.git("rev-parse", "HEAD"))
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})

	ranked := []string{"hit-link"}
	for _, g := range groups {
		ranked = append(ranked, slices.Sorted(slices.Values(g))...)
	}
	for _, tc := range []struct {
		limit, want int
	}{{0, gitlet.DefaultFindLimit}, {gitlet.MaxFindLimit, gitlet.MaxFindLimit}, {7, 7}} {
		f := findOf(t, s, findQ(repo, head, "hit", tc.limit))
		if got := foundPaths(f); !slices.Equal(got, ranked[:tc.want]) {
			t.Errorf("the find of hit with limit %d answered %v, want the first %d of %v", tc.limit, got, tc.want,
				ranked)
		}
		if f.Total != 120 || f.Files != 300 {
			t.Errorf("the find of hit with limit %d counts %d matches of %d files, want 120 of 300", tc.limit,
				f.Total, f.Files)
		}
		if m := f.Matches[0]; m.Mode != "120000" {
			t.Errorf("the link is answered with mode %q, want 120000", m.Mode)
		}
	}

	at := strings.Repeat("a", gitlet.MaxFindQueryBytes)
	for _, tc := range []struct {
		find  gitlet.FindQuery
		field string
	}{
		{gitlet.FindQuery{Rev: head}, "find.query"},
		{gitlet.FindQuery{Rev: head, Query: at + "a"}, "find.query"},
		{gitlet.FindQuery{Rev: head, Query: "a\x00b"}, "find.query"},
		{gitlet.FindQuery{Rev: head, Query: "caf\xe9"}, "find.query"},
		{gitlet.FindQuery{Rev: head, Query: "a", Limit: -1}, "find.limit"},
		{gitlet.FindQuery{Rev: head, Query: "a", Limit: gitlet.MaxFindLimit + 1}, "find.limit"},
		{gitlet.FindQuery{Rev: "main", Query: "a"}, "find.rev"},
	} {
		q := findQ(repo, head, "", 0)
		q.Find = &tc.find
		if err := q.Validate(); !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("a find of %+v validated with %v, want ErrInvalid naming %s", tc.find, err, tc.field)
		}
	}
	for _, fq := range []gitlet.FindQuery{{Rev: head, Query: at, Limit: gitlet.MaxFindLimit},
		{Rev: head[:7], Query: " ", Limit: 1}} {
		q := findQ(repo, head, "", 0)
		q.Find = &fq
		if err := q.Validate(); err != nil {
			t.Errorf("a find of %+v refused: %v", fq, err)
		}
	}
	tree := gitlet.GitQuery{Kind: gitlet.GitTree, RepoID: repo, Asker: person, Tree: &gitlet.TreeQuery{Rev: head},
		Find: &gitlet.FindQuery{Rev: head, Query: "a"}}
	if err := tree.Validate(); !errors.Is(err, gitlet.ErrInvalid) {
		t.Errorf("a tree read carrying a find validated with %v, want ErrInvalid", err)
	}
	blob := strings.TrimSpace(x.git("rev-parse", head+":hit-000.txt"))
	if _, err := s.Read(t.Context(), findQ(repo, blob, "hit", 0)); !errors.Is(err, gitlet.ErrInvalid) ||
		!strings.Contains(err.Error(), "find.rev") {
		t.Errorf("a find of a blob answered %v, want ErrInvalid naming find.rev", err)
	}
}

// memoEntries is how many entries the memo of every copy under the node's
// cache directory dir holds: its cohorts' files (crewlet/memo/<n>/<key>).
func memoEntries(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && filepath.Base(filepath.Dir(filepath.Dir(p))) == "memo" &&
			filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p)))) == "crewlet" {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// lsTrees is how many of runs listed a tree recursively.
func lsTrees(runs []run) int {
	n := 0
	for _, r := range runs {
		if slices.Equal(r.cmd.Args[:min(4, len(r.cmd.Args))], []string{"ls-tree", "-z", "-l", "-r"}) {
			n++
		}
	}
	return n
}

// A FIND KEEPS NOTHING OF ITS OWN AND RUNS NO GIT ONCE ITS TREE IS LISTED: one
// listing by the tree's id serves every later query and every commit holding
// the tree, and the memo holds the listing's pages and no answer.
//
// Mutations: memoize a find's answer → the memo grows with each query, red;
// key the pages by the commit → the other commit's find lists the tree again,
// red; take the warm path through git → a later query lists it, red.
func TestAFindKeepsNothingOfItsOwnAndRunsNoGitOnceItsTreeIsListed(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	for i := range 200 {
		files[fmt.Sprintf("dir-%d/file-with-a-long-name-%03d.go", i%7, i)] = "x\n"
	}
	head := x.commit(files, "files")
	other := x.commit(nil, "the same tree")
	tree := strings.TrimSpace(x.git("rev-parse", head+"^{tree}"))
	repo := x.repo(gitlettest.RepoName)
	// Every answer stamped the same from here: the room its matches are cut
	// against is the same read to read.
	x.settle(0, repo)
	rec := &recording{}
	const bound = 4 << 10
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	query.SetPageBytes(s, bound)
	fresh := x.server(0, query.Options{Cache: memoless{x.f.Nodes()[0].Cache()}})
	query.SetPageBytes(fresh, bound)
	dir := x.f.Nodes()[0].CacheDir()

	before := memoEntries(t, dir)
	check := func(rev, q string, listings int) {
		t.Helper()
		from := rec.count()
		a := mustRead(t, s, findQ(repo, rev, q, 0))
		runs := rec.since(from)
		if n := lsTrees(runs); n != listings {
			t.Errorf("the find of %q at %s listed the tree %d times, want %d", q, rev, n, listings)
		}
		if len(runs) != listings+1 {
			t.Errorf("the find of %q at %s ran %d processes, want the check and %d listings", q, rev, len(runs),
				listings)
		}
		for _, r := range runs {
			if r.cmd.Args[0] == "ls-tree" && r.cmd.Args[len(r.cmd.Args)-1] != tree {
				t.Errorf("the find listed %v, want the root tree %s by its id", r.cmd.Args, tree)
			}
		}
		want := mustRead(t, fresh, findQ(repo, rev, q, 0))
		if !reflect.DeepEqual(a.Find, want.Find) || a.Rev != rev || a.Memo {
			t.Errorf("the find of %q at %s answered %+v (rev %s), want a fresh node's %+v", q, rev, a.Find, a.Rev,
				want.Find)
		}
		if a.Find.Files != 200 {
			t.Errorf("the find of %q at %s counts %d files, want 200", q, rev, a.Find.Files)
		}
	}
	check(head, "f", 1)
	listed := memoEntries(t, dir)
	if listed-before < 3 {
		t.Fatalf("the first find left %d entries in the memo, want the listing's pages", listed-before)
	}
	check(head, "d3f1", 0)
	check(head, "LONG-NAME-19", 0)
	check(other, "go", 0)
	if n := memoEntries(t, dir); n != listed {
		t.Errorf("the memo holds %d entries after four finds, want the %d of the listing's pages", n, listed)
	}

	// THE PAGES ARE THE LISTING'S: an unstamped recursive listing of the
	// tree's id, paged through, is answered from them with no git.
	from := rec.count()
	q := treeQ(repo, gitlet.TreeQuery{Rev: tree, Recursive: true})
	q.Unstamped = true
	entries := 0
	for _, p := range pages(t, s, q, bound) {
		entries += len(p.Tree.Entries)
	}
	if runs := rec.since(from); lsTrees(runs) != 0 || entries != 200 {
		t.Errorf("the unstamped listing of tree %s ran %d listings for %d entries, want the memo's 200", tree,
			lsTrees(runs), entries)
	}
}

// missOnce is a node's cache whose memo misses one lookup: the n-th once it
// is armed, as though that page's cohort had been trimmed under the read.
type missOnce struct {
	query.Cache
	mu     sync.Mutex
	armed  bool
	n      int
	missed bool
}

func (c *missOnce) MemoGet(s cache.Synced, k cache.MemoKey) ([]byte, bool) {
	c.mu.Lock()
	miss := false
	if c.armed && !c.missed {
		if c.n--; c.n == 0 {
			miss, c.missed = true, true
		}
	}
	c.mu.Unlock()
	if miss {
		return nil, false
	}
	return c.Cache.MemoGet(s, k)
}

// A FIND GOES ON FROM THE PAGE ITS MEMO LACKS: with the second page gone it
// lists once from there, answers as before and puts the pages back.
//
// Mutations: refuse the find, or rank the pages the walk read alone, at a
// page the memo lacks → red; list the tree from its first entry and hand the
// pages read again → the counts double, red.
func TestAFindGoesOnFromThePageItsMemoLacks(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{}
	for i := range 120 {
		files[fmt.Sprintf("pkg/file-with-a-long-name-%03d.go", i)] = "x\n"
	}
	head := x.commit(files, "files")
	repo := x.repo(gitlettest.RepoName)
	x.settle(0, repo)
	rec := &recording{}
	c := &missOnce{Cache: x.f.Nodes()[0].Cache()}
	s := x.server(0, query.Options{Git: rec.on(x, 0), Cache: c})
	query.SetPageBytes(s, 4<<10)

	first := mustRead(t, s, findQ(repo, head, ".go", 0))
	if first.Find.Total != 120 || first.Find.Files != 120 {
		t.Fatalf("the premise: the first find counts %d matches of %d files, want 120 of 120", first.Find.Total,
			first.Find.Files)
	}
	c.mu.Lock()
	c.armed, c.n = true, 2
	c.mu.Unlock()
	from := rec.count()
	again := mustRead(t, s, findQ(repo, head, ".go", 0))
	if n := lsTrees(rec.since(from)); n != 1 || !c.missed {
		t.Errorf("the find missing its second page listed the tree %d times (missed %t), want once", n, c.missed)
	}
	if !reflect.DeepEqual(again.Find, first.Find) {
		t.Errorf("the find missing its second page answered %+v, want %+v", again.Find, first.Find)
	}
	from = rec.count()
	mustRead(t, s, findQ(repo, head, ".go", 0))
	if n := lsTrees(rec.since(from)); n != 0 {
		t.Errorf("the find after the listing put its pages back listed the tree %d times, want none", n)
	}
}

// A FIND IS ONE ANSWER CUT BY ITS BYTES: of a hundred matching paths near
// gitlet.MaxGitPathBytes it answers the best that fit, and counts all.
//
// Mutation: answer the limit's matches whatever they encode to → red.
func TestAFindIsOneAnswerCutByItsBytes(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	deep := strings.Repeat(strings.Repeat("<", 250)+"/", 13)
	files := map[string]string{}
	for i := range 100 {
		files[fmt.Sprintf("%sf%03d", deep, i)] = "x\n"
	}
	head := x.commit(files, "deep files")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	a := mustRead(t, s, findQ(repo, head, "f", gitlet.MaxFindLimit))
	if n := encoded(t, a); n > gitlet.MaxGitAnswerBytes {
		t.Fatalf("the find encodes to %d bytes, past %d", n, gitlet.MaxGitAnswerBytes)
	}
	got := foundPaths(a.Find)
	if len(got) == 0 || len(got) == 100 || a.Find.Total != 100 {
		t.Fatalf("the find answered %d matches of %d, want some of the 100 it counts", len(got), a.Find.Total)
	}
	for i, p := range got {
		if want := fmt.Sprintf("%sf%03d", deep, i); p != want {
			t.Errorf("match %d is %q, want the %d-th best", i, p[len(deep):], i)
		}
	}
}

// A PATH NO READ NAMES IS NO FILE A FIND HOLDS. Beside b/a.txt, a blob at a
// path of about 10 KiB, past gitlet.MaxGitPathBytes, that matches best: the
// find answers b/a.txt and counts one file.
//
// Mutation: rank the long path → it fills the answer alone, nothing fits, red.
func TestAFindPassesAPathNoReadNames(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	x.commit(map[string]string{"b/a.txt": "x\n"}, "a file")
	long := strings.Repeat(strings.Repeat("<", 250)+"/", 40) + "a"
	blob := strings.TrimSpace(x.git("hash-object", "-w", "b/a.txt"))
	x.git("update-index", "--add", "--cacheinfo", "100644,"+blob+","+long)
	x.git("commit", "-q", "-m", "a path past the bound")
	x.push(gitlettest.RepoName, "main")
	head := x.p.Head(x.work)
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	f := findOf(t, s, findQ(repo, head, "a", 0))
	if got := foundPaths(f); !slices.Equal(got, []string{"b/a.txt"}) || f.Total != 1 || f.Files != 1 {
		t.Errorf("the find of a answered %q (%d of %d files), want b/a.txt, 1 of 1", got, f.Total, f.Files)
	}
}
