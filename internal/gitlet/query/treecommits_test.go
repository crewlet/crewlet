package query_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A DIRECTORY'S LAST COMMITS (treecommits.go): each entry's commit as `git log
// -1 -- <entry>` names it, the oracle every case here holds a read to.

// treeCommitsQ is a person's read of a directory's last commits.
func treeCommitsQ(repo, rev, path string) gitlet.GitQuery {
	return gitlet.GitQuery{Kind: gitlet.GitTreeCommits, RepoID: repo, Asker: person, Home: true,
		TreeCommits: &gitlet.TreeCommitsQuery{Rev: rev, Path: path}}
}

// histNode is one path of a commit's tree: its mode and its content, or for a
// directory and a submodule the object it names.
type histNode struct{ mode, data string }

func file(data string) histNode { return histNode{mode: "100644", data: data} }

// histCommit is one commit of a history fast-import writes: its ref, its
// parents (indices of earlier commits, the first its first parent), its whole
// tree and its date.
type histCommit struct {
	ref     string
	parents []int
	tree    map[string]histNode
	date    int64
	// ident and msg are its author and committer and its message, a fixed
	// one's where empty.
	ident, msg string
}

// writeHistory writes commits into dir with ONE fast-import session, commit i
// marked i+1, and answers each commit's id.
func writeHistory(t testing.TB, git *gittest.Client, dir string, commits []histCommit) []string {
	t.Helper()
	gittest.FastImport(t, git, dir, func(w *bufio.Writer) error {
		for i, c := range commits {
			msg, ident := fmt.Sprintf("commit %d", i), "T <t@example.com>"
			if c.msg != "" {
				msg, ident = c.msg, c.ident
			}
			if len(c.parents) == 0 {
				fmt.Fprintf(w, "reset %s\n\n", c.ref) // a root, wherever its ref was
			}
			fmt.Fprintf(w, "commit %s\nmark :%d\n", c.ref, i+1)
			fmt.Fprintf(w, "author %s %d +0000\ncommitter %s %d +0000\n", ident, c.date, ident, c.date)
			fmt.Fprintf(w, "data %d\n%s\n", len(msg), msg)
			for j, p := range c.parents {
				if j == 0 {
					fmt.Fprintf(w, "from :%d\n", p+1)
				} else {
					fmt.Fprintf(w, "merge :%d\n", p+1)
				}
			}
			w.WriteString("deleteall\n")
			for _, path := range slices.Sorted(maps.Keys(c.tree)) {
				n := c.tree[path]
				switch n.mode {
				case "040000", "160000":
					fmt.Fprintf(w, "M %s %s %s\n", n.mode, n.data, quotedPath(path))
				default:
					fmt.Fprintf(w, "M %s inline %s\ndata %d\n%s\n", n.mode, quotedPath(path), len(n.data), n.data)
				}
			}
			w.WriteString("\n")
		}
		// A ref a commit, so its id can be read back; never pushed.
		for i := range commits {
			fmt.Fprintf(w, "reset refs/marks/%d\nfrom :%d\n\n", i, i+1)
		}
		return nil
	})
	ids := make([]string, len(commits))
	for _, line := range strings.Split(strings.TrimSpace(git.Run(t, dir, "for-each-ref",
		"--format=%(refname) %(objectname)", "refs/marks/")), "\n") {
		ref, id, _ := strings.Cut(line, " ")
		i, err := strconv.Atoi(strings.TrimPrefix(ref, "refs/marks/"))
		if err != nil || i >= len(ids) {
			t.Fatalf("fast-import wrote ref %q", ref)
		}
		ids[i] = id
	}
	return ids
}

// quotedPath is path as fast-import reads one: C-style quoted, every byte
// outside printable ASCII an octal escape.
func quotedPath(path string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := range len(path) {
		switch c := path[i]; {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// treeCase is a repository a case reads directories of: the work tree its
// history was written in, which the oracle runs in, and the server it reads.
type treeCase struct {
	git  *gittest.Client
	work string
	repo string
	s    *query.Server
}

// lastCommit is the oracle: `git log -1` of path from commit, in the work
// tree, "" where git names none.
func (c treeCase) lastCommit(t testing.TB, commit, path string) string {
	t.Helper()
	return strings.TrimSpace(c.git.Run(t, c.work, "log", "-1", "--format=%H", commit, "--", ":(literal)"+path))
}

// rawPath is an entry's path as its bytes.
func rawPath(t testing.TB, path, b64 string) string {
	t.Helper()
	if b64 == "" {
		return path
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// entriesOf is every entry pages answer, in their order, and each commit they
// carry by id, holding each page to carrying exactly its entries' commits,
// once each.
func entriesOf(t *testing.T, ps []gitlet.GitAnswer) ([]gitlet.TreeCommit, map[string]gitlet.LogCommit) {
	t.Helper()
	var out []gitlet.TreeCommit
	commits := map[string]gitlet.LogCommit{}
	for _, p := range ps {
		tc := p.TreeCommits
		named := map[string]bool{}
		for _, c := range tc.Commits {
			if named[c.OID] {
				t.Errorf("a page carries commit %s twice", c.OID)
			}
			named[c.OID] = true
			commits[c.OID] = c
		}
		used := map[string]bool{}
		for _, e := range tc.Entries {
			if e.Commit != "" && !named[e.Commit] {
				t.Errorf("entry %q names commit %s, which its page does not carry", e.Path, e.Commit)
			}
			used[e.Commit] = true
		}
		for id := range named {
			if !used[id] {
				t.Errorf("a page carries commit %s, which no entry of it names", id)
			}
		}
		out = append(out, tc.Entries...)
	}
	return out, commits
}

// listing is every entry of the directory dir at rev, as code_tree lists it.
func listing(t *testing.T, s *query.Server, repo, rev, dir string) []gitlet.TreeEntry {
	t.Helper()
	var out []gitlet.TreeEntry
	for _, p := range pages(t, s, treeQ(repo, gitlet.TreeQuery{Rev: rev, Path: dir}), gitlet.MaxGitAnswerBytes) {
		out = append(out, p.Tree.Entries...)
	}
	return out
}

// held reads dir's last commits at rev from the cursor e:<from> to the end
// and holds every entry to the oracle and to code_tree's listing, and every
// commit a page carries to code_log's, answering the entries.
func (c treeCase) held(t *testing.T, rev, dir string, from int) []gitlet.TreeCommit {
	t.Helper()
	q := treeCommitsQ(c.repo, rev, dir)
	if from > 0 {
		q.Cursor = "e:" + strconv.Itoa(from)
	}
	ps := pages(t, c.s, q, gitlet.MaxGitAnswerBytes)
	got, commits := entriesOf(t, ps)
	want := listing(t, c.s, c.repo, rev, dir)
	if len(want) <= from && from > 0 {
		t.Fatalf("the premise: %q at %s lists %d entries, from %d", dir, rev, len(want), from)
	}
	want = want[min(from, len(want)):]
	if len(got) != len(want) {
		t.Fatalf("%q at %s from e:%d: %d entries answered, want the listing's %d", dir, rev, from, len(got),
			len(want))
	}
	for i, e := range got {
		if e.Path != want[i].Path || e.PathB64 != want[i].PathB64 {
			t.Errorf("%q at %s: entry %d is %q (%q), want the listing's %q (%q)", dir, rev, from+i, e.Path,
				e.PathB64, want[i].Path, want[i].PathB64)
			continue
		}
		if oracle := c.lastCommit(t, rev, rawPath(t, e.Path, e.PathB64)); e.Commit != oracle || e.OutOfTime {
			t.Errorf("%q at %s: entry %q answered %q (out of time %t), want git's %q", dir, rev, e.Path,
				e.Commit, e.OutOfTime, oracle)
		}
	}
	for _, p := range ps {
		if p.TreeCommits.Total != len(want)+from || p.TreeCommits.OutOfTime {
			t.Errorf("%q at %s: a page's total is %d (out of time %t), want the listing's %d", dir, rev,
				p.TreeCommits.Total, p.TreeCommits.OutOfTime, len(want)+from)
		}
	}
	for id, commit := range commits {
		logged := mustRead(t, c.s, logQ(c.repo, gitlet.LogQuery{Rev: id, Limit: 1})).Log.Commits
		if len(logged) != 1 || !reflect.DeepEqual(logged[0], commit) {
			t.Errorf("commit %s is carried as %+v, want code_log's %+v", id, commit, logged)
		}
	}
	return got
}

// A MERGE TAKES THE ENTRY ITS SIDE KEPT: main changes d/b and reverts it while a
// branch changes d/a, then merges the branch, whose d/ the merge's equals; d/b's
// commit is main's revert and d/a's the branch's. A merge resolving d/c by hand,
// both sides having changed it, is d/c's own commit.
// Mutation: route d/b down the directory's own history, or never answer a merge.
func TestAMergeTakesTheEntryItsSideKept(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	tree := func(a, b, c string) map[string]histNode {
		return map[string]histNode{"d/a": file(a), "d/b": file(b), "d/c": file(c), "top": file("t")}
	}
	const main, side = "refs/heads/main", "refs/heads/side"
	ids := writeHistory(t, x.p.Git(), x.work, []histCommit{
		{ref: main, tree: tree("1", "1", "1"), date: 1_700_000_000},                       // 0
		{ref: side, parents: []int{0}, tree: tree("2", "1", "1"), date: 1_700_000_100},    // 1: side changes d/a
		{ref: main, parents: []int{0}, tree: tree("1", "2", "1"), date: 1_700_000_200},    // 2: main changes d/b
		{ref: main, parents: []int{2}, tree: tree("1", "1", "1"), date: 1_700_000_300},    // 3: and reverts it
		{ref: main, parents: []int{3, 1}, tree: tree("2", "1", "1"), date: 1_700_000_400}, // 4: the merge
		{ref: side, parents: []int{4}, tree: tree("2", "1", "side"), date: 1_700_000_500}, // 5
		{ref: main, parents: []int{4}, tree: tree("2", "1", "main"), date: 1_700_000_600}, // 6
		{ref: main, parents: []int{6, 5}, tree: tree("2", "1", "both"), date: 1_700_000_700},
	})
	x.push(gitlettest.RepoName, "refs/heads/*:refs/heads/*")
	c := treeCase{git: x.p.Git(), work: x.work, repo: x.repo(gitlettest.RepoName), s: x.server(0, query.Options{})}
	for _, at := range []struct {
		commit    int
		a, b, c   int
		whatHolds string
	}{
		{4, 1, 3, 0, "the first merge"},
		{7, 1, 3, 7, "the hand-resolved merge"},
	} {
		got := c.held(t, ids[at.commit], "d", 0)
		want := map[string]string{"d/a": ids[at.a], "d/b": ids[at.b], "d/c": ids[at.c]}
		for _, e := range got {
			if e.Commit != want[e.Path] {
				t.Errorf("at %s, %s answered %s, want %s", at.whatHolds, e.Path, e.Commit, want[e.Path])
			}
		}
	}
	c.held(t, ids[7], "", 0)
}

// A DIRECTORY THAT WAS A SUBMODULE IS READ: d/ and d/s/ were submodules, whose
// gitlinks a walk of `d/` lists, and each entry still answers git's own commit.
// Mutation: read the gitlink at d as an entry's change.
func TestADirectoryThatWasASubmoduleIsRead(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	link := func(n int) histNode { return histNode{mode: "160000", data: fmt.Sprintf("%040x", n)} }
	const main = "refs/heads/main"
	ids := writeHistory(t, x.p.Git(), x.work, []histCommit{
		{ref: main, tree: map[string]histNode{"d": link(1), "top": file("t")}, date: 1_700_000_000},
		{ref: main, parents: []int{0}, tree: map[string]histNode{"d": link(2), "top": file("t")},
			date: 1_700_000_100},
		{ref: main, parents: []int{1}, tree: map[string]histNode{"d/a": file("1"), "d/s": link(3), "top": file("t")},
			date: 1_700_000_200},
		{ref: main, parents: []int{2}, tree: map[string]histNode{"d/a": file("2"), "d/s/f": file("1"),
			"top": file("t")}, date: 1_700_000_300},
		{ref: main, parents: []int{3}, tree: map[string]histNode{"d/a": file("2"), "d/s/f": file("2"),
			"top": file("t")}, date: 1_700_000_400},
	})
	x.push(gitlettest.RepoName, "refs/heads/*:refs/heads/*")
	c := treeCase{git: x.p.Git(), work: x.work, repo: x.repo(gitlettest.RepoName), s: x.server(0, query.Options{})}
	want := map[string]string{"d/a": ids[3], "d/s": ids[4], "d/s/f": ids[4]}
	for _, dir := range []string{"d", "d/s"} {
		for _, e := range c.held(t, ids[4], dir, 0) {
			if e.Commit != want[e.Path] {
				t.Errorf("%s answered %s, want %s", e.Path, e.Commit, want[e.Path])
			}
		}
	}
	c.held(t, ids[2], "d", 0)
	c.held(t, ids[4], "", 0)
}

// treeGen writes a seeded history of a directory, d/, on up to five
// branches: every kind of change git's path rule meets.
type treeGen struct {
	rng     *rand.Rand
	commits []histCommit
	tips    map[string]int // a branch's tip, by its ref
	date    int64
	holder  string // a tree holding only the empty tree: a directory with no file
}

// treeGenNames are the entries d/ draws from: files, a directory of files, an
// executable, a link, a submodule, names not UTF-8 or beginning with `-` or
// `:`, a directory holding no file, and one whose files a commit may remove.
var treeGenNames = []string{"a", "b", "c", "s", "x.sh", "ln", "sub", "caf\xe9", "-x", ":y", "e", "g", "r1", "r2"}

// entryPaths is the paths of tree under entry x of d/.
func entryPaths(tree map[string]histNode, x string) map[string]histNode {
	out := map[string]histNode{}
	for p, n := range tree {
		if p == "d/"+x || strings.HasPrefix(p, "d/"+x+"/") {
			out[p] = n
		}
	}
	return out
}

// setEntry replaces entry x of tree with paths, moved from the name they had.
func setEntry(tree map[string]histNode, x string, paths map[string]histNode, from string) {
	for p := range entryPaths(tree, x) {
		delete(tree, p)
	}
	for p, n := range paths {
		tree["d/"+x+strings.TrimPrefix(p, "d/"+from)] = n
	}
}

// entries is the names of d/'s entries in tree.
func entries(tree map[string]histNode) []string {
	var out []string
	for _, x := range treeGenNames {
		if len(entryPaths(tree, x)) > 0 {
			out = append(out, x)
		}
	}
	return out
}

func (g *treeGen) token() string { return fmt.Sprintf("v%d\n", g.rng.IntN(4)) }

// gitlink is a submodule naming one of three commits.
func (g *treeGen) gitlink() histNode {
	return histNode{mode: "160000", data: fmt.Sprintf("%040x", g.rng.IntN(3)+1)}
}

// content is a fresh version of entry x as its kind holds it.
func (g *treeGen) content(x string) map[string]histNode {
	switch x {
	case "s":
		return map[string]histNode{"d/s/1": file(g.token()), "d/s/2": file(g.token())}
	case "x.sh":
		mode := "100755"
		if g.rng.IntN(4) == 0 {
			mode = "100644" // the executable bit alone changed
		}
		return map[string]histNode{"d/x.sh": {mode: mode, data: g.token()}}
	case "ln":
		return map[string]histNode{"d/ln": {mode: "120000", data: strings.TrimSpace(g.token())}}
	case "sub":
		return map[string]histNode{"d/sub": g.gitlink()}
	case "e":
		return map[string]histNode{"d/e": {mode: "040000", data: g.holder}}
	case "g":
		if g.rng.IntN(3) == 0 {
			return map[string]histNode{"d/g": {mode: "040000", data: g.holder}}
		}
		return map[string]histNode{"d/g/f": file(g.token()), "d/g/keep/e": {mode: "040000", data: g.holder}}
	}
	return map[string]histNode{"d/" + x: file(g.token())}
}

// commit adds a commit to ref of parents and tree, dated on or, now and then,
// back; a file under d/ replaces a submodule at d.
func (g *treeGen) commit(ref string, parents []int, tree map[string]histNode) {
	for p := range tree {
		if strings.HasPrefix(p, "d/") {
			delete(tree, "d")
			break
		}
	}
	g.date += 100
	if g.rng.IntN(8) == 0 {
		g.date -= 5000
	}
	g.commits = append(g.commits, histCommit{ref: ref, parents: parents, tree: tree, date: g.date})
	g.tips[ref] = len(g.commits) - 1
}

// step adds one commit: an edit, a deletion, a rename, a type change, a copy
// of another branch's change, a revert, an edit outside d/, a merge of two or
// three branches, or a new branch.
func (g *treeGen) step() {
	refs := slices.Sorted(maps.Keys(g.tips))
	ref := refs[g.rng.IntN(len(refs))]
	if len(refs) < 5 && g.rng.IntN(6) == 0 {
		ref = fmt.Sprintf("refs/heads/b%d", len(refs))
		g.tips[ref] = g.rng.IntN(len(g.commits))
	}
	at := g.tips[ref]
	tree := maps.Clone(g.commits[at].tree)
	present := entries(tree)
	pick := func() string {
		if len(present) == 0 {
			return treeGenNames[g.rng.IntN(len(treeGenNames))]
		}
		return present[g.rng.IntN(len(present))]
	}
	parents := []int{at}
	switch op := g.rng.IntN(10); {
	case op < 3:
		for range 1 + g.rng.IntN(2) {
			x := treeGenNames[g.rng.IntN(len(treeGenNames))]
			setEntry(tree, x, g.content(x), x)
		}
	case op == 3:
		tree["top"] = file(g.token())
	case op == 4:
		setEntry(tree, pick(), nil, "")
	case op == 5:
		from, to := pick(), []string{"r1", "r2", "a"}[g.rng.IntN(3)]
		if from != to {
			moved := entryPaths(tree, from)
			setEntry(tree, from, nil, "")
			setEntry(tree, to, moved, from)
		}
	case op == 6:
		x := []string{"a", "b", "s", "d"}[g.rng.IntN(4)]
		switch {
		case x == "d": // d/ becomes a submodule, until a later commit adds a file under it
			for _, e := range present {
				setEntry(tree, e, nil, "")
			}
			tree["d"] = g.gitlink()
		case x == "s" && g.rng.IntN(2) == 0:
			setEntry(tree, x, map[string]histNode{"d/s": g.gitlink()}, x)
		case x == "s":
			setEntry(tree, x, map[string]histNode{"d/s": file(g.token())}, x)
		default:
			setEntry(tree, x, map[string]histNode{"d/" + x + "/in": file(g.token())}, x)
		}
	case op == 7:
		other := g.commits[g.tips[refs[g.rng.IntN(len(refs))]]].tree
		x := pick()
		setEntry(tree, x, entryPaths(other, x), x)
	case op == 8:
		x := pick()
		setEntry(tree, x, entryPaths(g.commits[g.rng.IntN(at+1)].tree, x), x)
	default:
		for _, r := range refs {
			if t := g.tips[r]; r != ref && !slices.Contains(parents, t) && len(parents) < 3 &&
				(len(parents) == 1 || g.rng.IntN(2) == 0) {
				parents = append(parents, t)
			}
		}
		for _, x := range treeGenNames {
			switch c := g.rng.IntN(len(parents) + 2); {
			case c < len(parents):
				setEntry(tree, x, entryPaths(g.commits[parents[c]].tree, x), x)
			case c == len(parents) && len(entryPaths(tree, x)) > 0:
				setEntry(tree, x, g.content(x), x) // the merge's own content
			case g.rng.IntN(8) == 0:
				setEntry(tree, x, nil, "") // dropped
			}
		}
	}
	g.commit(ref, parents, tree)
}

// newTreeGen is seed's history: 30 to 60 commits, the first on main holding
// every entry.
func newTreeGen(seed uint64, holder string) *treeGen {
	g := &treeGen{rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), tips: map[string]int{},
		date: 1_700_000_000, holder: holder}
	tree := map[string]histNode{"top": file("t\n"), "other/f": file("o\n")}
	for _, x := range treeGenNames[:12] {
		maps.Copy(tree, g.content(x))
	}
	g.commit("refs/heads/main", nil, tree)
	for range 29 + g.rng.IntN(31) {
		g.step()
	}
	return g
}

// emptyHolder writes, in dir, a tree holding only the empty tree: what a
// directory that holds no file is, since fast-import drops the empty tree
// itself.
func emptyHolder(t testing.TB, git *gittest.Client, dir string) string {
	t.Helper()
	empty, _, err := git.Try(t, dir, strings.NewReader(""), "mktree")
	if err != nil {
		t.Fatal(err)
	}
	holder, _, err := git.Try(t, dir, strings.NewReader("040000 tree "+strings.TrimSpace(empty)+"\tsub\n"), "mktree")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(holder)
}

// treeReach is what the equivalence's reads reached, over every seed.
type treeReach struct {
	mu                                                             sync.Mutex
	merge, side, none, hollow, notUTF8, narrowed, answered, linked int
}

// A DIRECTORY'S LAST COMMITS ARE GIT'S OWN: over 24 treeGen histories, every
// entry of d/, d/s/ and the root, read from a random cursor with and without a
// commit-graph on scaled bounds, answers `git log -1 -- <entry>`.
// Mutation: answer a merge's entry by its first parent, never answer a merge,
// credit a directory that never held a file, trust a window for a name it does
// not hold, or refuse the gitlink a walk of a once-submodule directory names.
func TestADirectorysLastCommitsAreGitsOwn(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	reach := &treeReach{}
	const seeds = 24
	for seed := range seeds {
		x.f.Declare(t, gitlet.Declared{Name: fmt.Sprintf("tc%d", seed), Settings: gitlettest.Settings()})
	}
	t.Run("seeds", func(t *testing.T) {
		for seed := range uint64(seeds) {
			t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
				t.Parallel()
				name := fmt.Sprintf("tc%d", seed)
				git := x.p.Git()
				work := git.Init(t)
				g := newTreeGen(seed, emptyHolder(t, git, work))
				ids := writeHistory(t, git, work, g.commits)
				if res := x.p.PushArgs(t.Context(), work, []string{"--force"}, x.url(0, name),
					"refs/heads/*:refs/heads/*"); res.Err != nil {
					t.Fatalf("git push: %s", res)
				}
				rows, err := x.f.Rows(t.Context(), name, gitlettest.Linearizable)
				if err != nil {
					t.Fatal(err)
				}
				if seed%2 == 1 {
					graphedFor(t, x, rows.Repo.ID)
				}
				rec := &recording{Pool: x.f.Nodes()[0].Pool()}
				c := treeCase{git: git, work: work, repo: rows.Repo.ID, s: x.server(0, query.Options{Git: rec})}
				query.SetTreeWalk(c.s, query.TreeWalk{Span: 5, Window: 4, NarrowRecords: 3, NarrowEntries: 2,
					NarrowWalks: 2})
				merges := map[string]bool{}
				for i, hc := range g.commits {
					if len(hc.parents) > 1 {
						merges[ids[i]] = true
					}
				}
				rng := rand.New(rand.NewPCG(seed, 7))
				for i, hc := range g.commits {
					if rng.IntN(2) == 0 {
						continue
					}
					line, linked := map[string]bool{}, map[string]bool{}
					for j := i; ; j = g.commits[j].parents[0] {
						line[ids[j]] = true
						for _, dir := range []string{"d", "d/s"} {
							linked[dir] = linked[dir] || g.commits[j].tree[dir].mode == "160000"
						}
						if len(g.commits[j].parents) == 0 {
							break
						}
					}
					for _, dir := range []string{"", "d", "d/s"} {
						n := 0
						for p := range hc.tree {
							if dir == "" || strings.HasPrefix(p, dir+"/") {
								n++
							}
						}
						if n == 0 {
							continue
						}
						entries := listing(t, c.s, c.repo, ids[i], dir)
						got := c.held(t, ids[i], dir, rng.IntN(len(entries)))
						reach.mu.Lock()
						if linked[dir] {
							reach.linked++
						}
						for _, e := range got {
							reach.answered++
							switch {
							case e.Commit == "":
								reach.none++
							case merges[e.Commit]:
								reach.merge++
							case !line[e.Commit]:
								reach.side++
							}
							if e.PathB64 != "" {
								reach.notUTF8++
							}
							if n := hc.tree["d/g"]; e.Path == "d/g" && n.mode == "040000" && e.Commit != "" {
								reach.hollow++
							}
						}
						reach.mu.Unlock()
					}
				}
				reach.mu.Lock()
				for _, r := range rec.ran() {
					if r.Args[0] == "log" && r.Args[1] == "--first-parent" && slices.Contains(r.Args, "--") &&
						!strings.HasSuffix(r.Args[len(r.Args)-1], "/") {
						reach.narrowed++
					}
				}
				reach.mu.Unlock()
			})
		}
	})
	probed := counter(x.f.Nodes()[0].Metrics(), metrics.CodeCacheMemoLookups, map[string]string{"outcome": "probe_hit"})
	t.Logf("reached: %d entries, %d answered by a merge, %d on a merge's other side, %d none, %d hollowed, "+
		"%d not UTF-8, %d narrowed walks, %d window probes hit, %d reads of a directory that was a submodule",
		reach.answered, reach.merge, reach.side, reach.none, reach.hollow, reach.notUTF8, reach.narrowed, probed,
		reach.linked)
	for what, n := range map[string]int{"an entry answered by a merge itself": reach.merge,
		"an entry answered on a merge's other side": reach.side, "an entry git names no commit for": reach.none,
		"a directory whose files a commit removed": reach.hollow, "a name that is not UTF-8": reach.notUTF8,
		"a narrowed walk": reach.narrowed, "an answer from a probed window": int(probed),
		"a directory that was a submodule": reach.linked} {
		if n == 0 {
			t.Errorf("no seed reached %s", what)
		}
	}
}

// graphedFor waits until node 0's copy of repo holds a commit-graph, as
// fixture.graphed does, on t.
func graphedFor(t *testing.T, x *fixture, repo string) {
	t.Helper()
	c := x.f.Nodes()[0].Cache()
	deadline := time.Now().Add(time.Minute)
	for {
		s, err := c.Sync(t.Context(), repo, gitlettest.Linearizable)
		if err != nil {
			t.Fatal(err)
		}
		_, whole := os.Stat(filepath.Join(s.Dir, "objects", "info", "commit-graph"))
		_, chain := os.Stat(filepath.Join(s.Dir, "objects", "info", "commit-graphs", "commit-graph-chain"))
		s.Release()
		if whole == nil || chain == nil {
			return
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("node 0's copy of %s holds no commit-graph a minute on", repo)
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
	}
}

// walksOf is what runs ran of a directory's walks: its group and narrowed
// walks (`log --first-parent`), and the entries it walked alone (`log -1`).
func walksOf(runs []run) (lines, narrowed int, alone []string) {
	for _, r := range runs {
		a := r.cmd.Args
		switch {
		case a[0] != "log":
		case a[1] == "--first-parent":
			lines++
			if slices.Contains(a, "--") && !strings.HasSuffix(a[len(a)-1], "/") {
				narrowed++
			}
		case a[1] == "-1":
			alone = append(alone, a[len(a)-1])
		}
	}
	return lines, narrowed, alone
}

// A DIRECTORY'S LAST COMMITS PAGE AS ITS LISTING DOES: from each cursor and
// Next, every entry once in order, a span at most a page, Total the listing's;
// bad cursors, paths, revisions and queries are refused as such.
// Mutation: skip or repeat an entry at a page's end, pass the span, or count a
// page as the total.
func TestATreeCommitsPageFollowsTheListing(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{"top": "t\n"}
	for i := range 230 {
		files[fmt.Sprintf("d/f%03d", i)] = "1\n"
	}
	first := x.commit(files, "first")
	for i := range 230 {
		if i%3 == 0 {
			files[fmt.Sprintf("d/f%03d", i)] = "2\n"
		}
	}
	head := x.commit(files, "second")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	query.SetPageBytes(s, 8<<10+x.decoration(treeQ(repo, gitlet.TreeQuery{Rev: head, Path: "d"})))
	c := treeCase{git: x.p.Git(), work: x.work, repo: repo, s: s}
	all := c.held(t, head, "d", 0)
	if len(all) != 230 {
		t.Fatalf("the directory's last commits are %d entries, want 230", len(all))
	}
	cursors := []string{""}
	for _, p := range pages(t, s, treeQ(repo, gitlet.TreeQuery{Rev: head, Path: "d"}), gitlet.MaxGitAnswerBytes) {
		if p.Next != "" {
			cursors = append(cursors, p.Next)
		}
	}
	if len(cursors) < 3 {
		t.Fatalf("the premise: code_tree cut the listing into %d pages, want several", len(cursors))
	}
	for _, cur := range cursors {
		q := treeCommitsQ(repo, head, "d")
		q.Cursor = cur
		ps := pages(t, s, q, gitlet.MaxGitAnswerBytes)
		got, _ := entriesOf(t, ps)
		from, _ := strconv.Atoi(strings.TrimPrefix(cur, "e:"))
		if !slices.Equal(got, all[from:]) {
			t.Errorf("read from %q: %d entries, want the listing's %d from there, each once in order", cur,
				len(got), len(all)-from)
		}
		for _, p := range ps {
			if n := len(p.TreeCommits.Entries); n > 100 || n == 0 || p.TreeCommits.Total != 230 {
				t.Errorf("a page from %q holds %d entries of total %d, want 1 to 100 of 230", cur, n,
					p.TreeCommits.Total)
			}
		}
	}
	tree := strings.TrimSpace(x.git("rev-parse", head+"^{tree}"))
	for _, tc := range []struct {
		name string
		q    gitlet.GitQuery
		want error
		says string
	}{
		{"a cursor at the end", func() gitlet.GitQuery {
			q := treeCommitsQ(repo, head, "d")
			q.Cursor = "e:230"
			return q
		}(), gitlet.ErrBadCursor, "230"},
		{"a cursor of another kind", func() gitlet.GitQuery {
			q := treeCommitsQ(repo, head, "d")
			q.Cursor = "l:3"
			return q
		}(), gitlet.ErrBadCursor, "e:"},
		{"a file", treeCommitsQ(repo, head, "d/f000"), gitlet.ErrNotATree, "d/f000"},
		{"a tree as the revision", treeCommitsQ(repo, tree, "d"), gitlet.ErrInvalid, "tree_commits.rev"},
		{"a ref", treeCommitsQ(repo, "main", "d"), gitlet.ErrInvalid, "tree_commits.rev"},
		{"an unclean path", treeCommitsQ(repo, head, "d/../d"), gitlet.ErrInvalid, "tree_commits.path"},
		{"both paths", func() gitlet.GitQuery {
			q := treeCommitsQ(repo, head, "d")
			q.TreeCommits.PathB64 = base64.StdEncoding.EncodeToString([]byte("d"))
			return q
		}(), gitlet.ErrInvalid, "tree_commits.path"},
	} {
		if _, err := s.Read(t.Context(), tc.q); !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v, want %v naming %q", tc.name, err, tc.want, tc.says)
		}
	}
	bare := treeCommitsQ(repo, first, "d")
	bare.Unstamped = true
	if a := mustRead(t, s, bare); a.RepoID != "" || len(a.TreeCommits.Entries) == 0 ||
		a.TreeCommits.Entries[0].Commit != first {
		t.Errorf("an unstamped read answered %+v, want the page with no stamp", a)
	}
}

// A WARM PAGE RUNS NO GIT, AND A COMMIT THAT LEFT THE DIRECTORY ALONE IS
// DERIVED: a repeat read is the memo's, and a later commit off d/ finds its
// windows at the group walk's first record.
// Mutation: keep no page or no window, key windows by the read's commit or its
// cursor.
func TestAWarmTreeCommitsPageRunsNoGit(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	x.commit(map[string]string{"d/a": "1\n", "d/b": "1\n", "top": "1\n"}, "one")
	x.commit(map[string]string{"d/a": "2\n"}, "two")
	third := x.commit(map[string]string{"top": "3\n"}, "three")
	fourth := x.commit(map[string]string{"top": "4\n"}, "four")
	repo := x.repo(gitlettest.RepoName)
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	query.SetTreeWalk(s, query.TreeWalk{Span: 1, Window: 4})
	c := treeCase{git: x.p.Git(), work: x.work, repo: repo, s: s}
	probes := func(outcome string) uint64 {
		return counter(x.f.Nodes()[0].Metrics(), metrics.CodeCacheMemoLookups, map[string]string{"outcome": outcome})
	}
	subs := func(runs []run) []string {
		var out []string
		for _, r := range runs {
			if r.cmd.Args[0] != "cat-file" && r.cmd.Args[0] != "rev-parse" {
				out = append(out, r.cmd.Args[0]+" "+r.cmd.Args[1])
			}
		}
		return out
	}
	n := rec.count()
	a := mustRead(t, s, treeCommitsQ(repo, third, "d"))
	want := []string{"ls-tree -z", "log --first-parent", "log -z"}
	if got := subs(rec.since(n)); !slices.Equal(got, want) {
		t.Errorf("the first read ran %v, want %v", got, want)
	}
	n = rec.count()
	if again := mustRead(t, s, treeCommitsQ(repo, third, "d")); !again.Memo || rec.count() != n ||
		!reflect.DeepEqual(unstamped(again), unstamped(a)) {
		t.Errorf("the same read again ran %d processes, memo %t, want the memo's page with none", rec.count()-n,
			again.Memo)
	}
	hits, misses := probes("probe_hit"), probes("probe_miss")
	n = rec.count()
	q := treeCommitsQ(repo, fourth, "d")
	q.Cursor = "e:1"
	got := mustRead(t, s, q)
	if ran := subs(rec.since(n)); !slices.Equal(ran, want) {
		t.Errorf("a read at a commit that left d/ alone ran %v, want %v", ran, want)
	}
	if probes("probe_hit") == hits || probes("probe_miss") != misses {
		t.Errorf("a read at a commit that left d/ alone probed %d hits and %d misses, want its windows found at "+
			"its walk's first record", probes("probe_hit")-hits, probes("probe_miss")-misses)
	}
	if e := got.TreeCommits.Entries; len(e) != 1 || e[0].Commit != c.lastCommit(t, fourth, "d/b") {
		t.Errorf("the derived page is %+v, want d/b's commit", e)
	}
}

// A PUSHED COMMIT'S PAGE IS DERIVED FROM ITS PARENT'S WINDOWS: across entries
// added and removed at and off window starts, a merge and a commit off d/, every
// page equals a cold read's, derived ones by a single two-record group walk.
// Mutation: key windows by page, trust a window for names it does not hold,
// or walk the history again where a window holds the answers.
func TestAPushedCommitsPageIsDerivedFromItsParents(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	var names []string
	for i := range 40 {
		names = append(names, fmt.Sprintf("n%02d", i))
	}
	pickName := func(prefix string, begins bool) string {
		for i := range 1000 {
			if n := fmt.Sprintf("%s%03d", prefix, i); query.WindowStart(n, 4) == begins {
				return n
			}
		}
		t.Fatal("no name found")
		return ""
	}
	base := map[string]histNode{"top": file("t")}
	for _, n := range names {
		base["d/"+n] = file("0")
	}
	hist := []histCommit{{ref: "refs/heads/main", tree: base, date: 1_700_000_000}}
	edit := func(ref string, from int, change func(map[string]histNode)) int {
		tree := maps.Clone(hist[from].tree)
		change(tree)
		hist = append(hist, histCommit{ref: ref, parents: []int{from}, tree: tree,
			date: 1_700_000_000 + int64(len(hist))*100})
		return len(hist) - 1
	}
	at := 0
	for _, n := range names {
		at = edit("refs/heads/main", at, func(tr map[string]histNode) { tr["d/"+n] = file("1") })
	}
	p := at
	added := pickName("n00", false)
	t1 := edit("refs/heads/main", p, func(tr map[string]histNode) { tr["d/"+added] = file("t") })
	var unstarted, started []string
	for _, n := range names[1:] {
		if query.WindowStart(n, 4) {
			started = append(started, n)
		} else {
			unstarted = append(unstarted, n)
		}
	}
	if len(started) < 2 || len(unstarted) < 2 {
		t.Fatalf("the premise: %d names begin windows of four and %d do not", len(started), len(unstarted))
	}
	t2 := edit("refs/heads/main", t1, func(tr map[string]histNode) { delete(tr, "d/"+unstarted[0]) })
	splits := pickName("n05", true)
	t3 := edit("refs/heads/main", t2, func(tr map[string]histNode) { tr["d/"+splits] = file("t") })
	side := edit("refs/heads/side", t3, func(tr map[string]histNode) {
		tr["d/n10"], tr["d/n20"] = file("side"), file("side")
	})
	t4 := edit("refs/heads/main", t3, func(map[string]histNode) {})
	hist[t4].parents = append(hist[t4].parents, side)
	hist[t4].tree["d/n10"], hist[t4].tree["d/n20"] = file("side"), file("side")
	t5 := edit("refs/heads/main", t4, func(tr map[string]histNode) { delete(tr, "d/"+started[len(started)-1]) })
	t6 := edit("refs/heads/main", t5, func(tr map[string]histNode) { tr["top"] = file("6") })
	ids := writeHistory(t, x.p.Git(), x.work, hist)
	x.push(gitlettest.RepoName, "refs/heads/*:refs/heads/*")
	repo := x.repo(gitlettest.RepoName)
	bounds := query.TreeWalk{Span: 10, Window: 4, NarrowRecords: 3}
	rec := &recording{}
	s := x.server(0, query.Options{Git: rec.on(x, 0)})
	query.SetTreeWalk(s, bounds)
	cold := x.server(0, query.Options{Cache: memoless{x.f.Nodes()[0].Cache()}})
	query.SetTreeWalk(cold, bounds)
	c := treeCase{git: x.p.Git(), work: x.work, repo: repo, s: s}

	readAll := func(name string, commit int, derived bool) []string {
		t.Helper()
		var alone []string
		q := treeCommitsQ(repo, ids[commit], "d")
		for range 100 {
			n := rec.count()
			a := mustRead(t, s, q)
			lines, narrowed, walked := walksOf(rec.since(n))
			alone = append(alone, walked...)
			if derived && (lines != 1 || narrowed != 0) {
				t.Errorf("%s: the page at %q ran %d walks of the line, %d narrowed, want one, derived", name,
					q.Cursor, lines, narrowed)
			}
			b := mustRead(t, cold, q)
			if !reflect.DeepEqual(unstamped(a), unstamped(b)) {
				t.Errorf("%s: the page at %q is %+v, want the cold read's %+v", name, q.Cursor, a.TreeCommits,
					b.TreeCommits)
			}
			for _, e := range a.TreeCommits.Entries {
				if want := c.lastCommit(t, ids[commit], e.Path); e.Commit != want {
					t.Errorf("%s: %s answered %s, want git's %s", name, e.Path, e.Commit, want)
				}
			}
			if a.Next == "" {
				return alone
			}
			q.Cursor = a.Next
		}
		t.Fatalf("%s: the pages never ended", name)
		return nil
	}
	readAll("P", p, false)
	readAll("T, an entry added", t1, true)
	readAll("T2, one removed", t2, true)
	readAll("T3, a window's start added", t3, true)
	alone := readAll("T4, a merge", t4, true)
	slices.Sort(alone)
	if want := []string{":(literal)d/n10", ":(literal)d/n20"}; !slices.Equal(alone, want) {
		t.Errorf("T4's walks alone were %v, want each merged entry's once", alone)
	}
	readAll("T5, a window's start removed", t5, false)
	readAll("T6, after it", t6, true)
}

// A TREE WALK NARROWS TO THE ENTRIES LEFT: five untouched entries go on from
// the tenth record's first parent in walks of two, the page's first; seven read
// on to the root; a pathspec past the walks' bytes walks alone.
// Mutation: read on where the walk may narrow, narrow past the bounds, or start
// a narrowed walk anywhere but the last record's first parent.
func TestATreeWalkNarrowsToTheEntriesLeft(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	build := func(untouched int, branch string) []string {
		work := x.p.Git().Init(t)
		tree := map[string]histNode{}
		for i := range 10 {
			tree[fmt.Sprintf("d/b%d", i)] = file("0")
		}
		for i := range untouched {
			tree[fmt.Sprintf("d/u%d", i)] = file("0")
		}
		hist := []histCommit{{ref: "refs/heads/main", tree: tree, date: 1_700_000_000}}
		for k := 1; k < 40; k++ {
			tr := maps.Clone(hist[k-1].tree)
			tr[fmt.Sprintf("d/b%d", k%10)] = file(strconv.Itoa(k))
			hist = append(hist, histCommit{ref: "refs/heads/main", parents: []int{k - 1}, tree: tr,
				date: 1_700_000_000 + int64(k)*100})
		}
		ids := writeHistory(t, x.p.Git(), work, hist)
		if res := x.p.PushArgs(t.Context(), work, []string{"--force"}, x.url(0, gitlettest.RepoName),
			"refs/heads/main:refs/heads/"+branch); res.Err != nil {
			t.Fatalf("git push: %s", res)
		}
		return ids
	}
	ids := build(5, "main")
	repo := x.repo(gitlettest.RepoName)
	bounds := query.TreeWalk{NarrowRecords: 10, NarrowEntries: 2, NarrowWalks: 3}
	read := func(b query.TreeWalk, commit, cursor string) []gitproc.Cmd {
		t.Helper()
		rec := &recording{}
		s := x.server(0, query.Options{Git: rec.on(x, 0), Cache: unrecalled{x.f.Nodes()[0].Cache()}})
		query.SetTreeWalk(s, b)
		q := treeCommitsQ(repo, commit, "d")
		q.Cursor = cursor
		for _, e := range mustRead(t, s, q).TreeCommits.Entries {
			if strings.HasPrefix(e.Path, "d/u") && e.Commit != ids[0] {
				t.Errorf("%s answered %s, want the first commit", e.Path, e.Commit)
			}
		}
		var walks []gitproc.Cmd
		for _, c := range rec.ran() {
			if c.Args[0] == "log" && c.Args[1] == "--first-parent" {
				walks = append(walks, c)
			}
		}
		return walks
	}
	specs := func(c gitproc.Cmd) (from string, entries []string) {
		i := slices.Index(c.Args, "--")
		for _, a := range c.Args[i+1:] {
			entries = append(entries, strings.TrimPrefix(a, ":(literal)d/"))
		}
		return c.Args[i-1], entries
	}
	walks := read(bounds, ids[39], "e:12")
	if len(walks) != 4 {
		t.Fatalf("the read ran %d walks, want the group walk and three narrowed: %v", len(walks), walks)
	}
	var narrowed [][]string
	for _, w := range walks[1:] {
		from, entries := specs(w)
		if from != ids[29] {
			t.Errorf("a narrowed walk began at %s, want the tenth record's first parent %s", from, ids[29])
		}
		narrowed = append(narrowed, entries)
	}
	if want := [][]string{{"u2", "u3"}, {"u4", "u0"}, {"u1"}}; !reflect.DeepEqual(narrowed, want) {
		t.Errorf("the narrowed walks named %v, want %v: the page's own entries first, two a walk", narrowed, want)
	}
	if one := read(query.TreeWalk{NarrowRecords: 10, NarrowEntries: 2, NarrowWalks: 3, NarrowBytes: 1}, ids[39],
		""); len(one) != 6 {
		t.Errorf("with a pathspec past the walks' bytes the read ran %d walks, want the group walk and five of "+
			"one entry each", len(one))
	}
	ids = build(7, "seven")
	if walks := read(bounds, ids[39], ""); len(walks) != 1 {
		t.Errorf("with seven untouched the read ran %d walks, want the group walk alone, to the first commit",
			len(walks))
	}
}

// treeCut is a query.Pool on which a directory's group walk writes its first
// records (git's --max-count) and then answers that its budget ended it, every
// later walk answers so before git wrote anything, and dueing ends the group
// walk with its context, as the answer falling due would.
type treeCut struct {
	query.Pool
	records int
	lines   atomic.Int64
	due     bool
}

func (p *treeCut) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	switch {
	case c.Args[0] != "log" || c.Args[1] == "-z":
		return p.Pool.Run(ctx, c)
	case p.due:
		<-ctx.Done()
		return gitproc.Result{}, context.Cause(ctx)
	case c.Args[1] == "--first-parent" && p.lines.Add(1) == 1:
		c.Args = slices.Insert(slices.Clone(c.Args), 1, "--max-count="+strconv.Itoa(p.records))
		res, err := p.Pool.Run(ctx, c)
		if err == nil {
			return res, fmt.Errorf("%w: the read's budget, as the test's pool tells it", gitproc.ErrDeadline)
		}
		return res, err
	}
	return gitproc.Result{}, fmt.Errorf("%w: the read's budget, before git wrote a record", gitproc.ErrDeadline)
}

// A PAGE ITS BUDGET ENDED GOES ON: a walk cut in its fourth record answers what
// the three before reached, out of time, unkept, Next at the first unanswered
// (past a lone first entry); reading on matches a whole read; due is unready.
// Mutation: keep a cut page, refuse it, stall on its first entry, drop its
// Next, name a Next past the listing, or leave it unmarked.
func TestABudgetEndedTreeCommitsPageGoesOn(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	files := map[string]string{"d2/only": "1\n"}
	for i := range 10 {
		files[fmt.Sprintf("d/e%d", i)] = "0\n"
	}
	x.commit(files, "all")
	var head string
	for i := 9; i >= 0; i-- {
		head = x.commit(map[string]string{fmt.Sprintf("d/e%d", i): "1\n"}, fmt.Sprintf("e%d", i))
	}
	repo := x.repo(gitlettest.RepoName)
	c := treeCase{git: x.p.Git(), work: x.work, repo: repo, s: x.server(0, query.Options{})}
	cutRead := func(records int, dir, cursor string) gitlet.GitAnswer {
		t.Helper()
		pool := &treeCut{Pool: x.f.Nodes()[0].Pool(), records: records}
		q := treeCommitsQ(repo, head, dir)
		q.Cursor = cursor
		a := mustRead(t, x.server(0, query.Options{Git: pool}), q)
		if a.Memo || !a.TreeCommits.Cut() {
			t.Errorf("a page its budget ended from %q is memo %t, cut %t, want neither kept nor unmarked", cursor,
				a.Memo, a.TreeCommits.Cut())
		}
		return a
	}
	a := cutRead(4, "d", "")
	var got []string
	for _, e := range a.TreeCommits.Entries {
		got = append(got, e.Path)
		if e.OutOfTime || e.Commit == "" {
			t.Errorf("entry %s of the cut page is out of time or unanswered", e.Path)
		}
	}
	if !slices.Equal(got, []string{"d/e0", "d/e1", "d/e2"}) || a.Next != "e:3" {
		t.Errorf("a walk cut inside its fourth record answered %v, next %q, want d/e0 to d/e2 and e:3", got,
			a.Next)
	}
	again := &treeCut{Pool: x.f.Nodes()[0].Pool(), records: 4}
	s := x.server(0, query.Options{Git: again})
	mustRead(t, s, treeCommitsQ(repo, head, "d"))
	if b := mustRead(t, s, treeCommitsQ(repo, head, "d")); b.Memo || again.lines.Load() != 2 {
		t.Errorf("a cut page read again was memo %t after %d walks, want it read again", b.Memo, again.lines.Load())
	}
	first := cutRead(4, "d", "e:5")
	if e := first.TreeCommits.Entries; len(e) != 1 || e[0].Path != "d/e5" || !e[0].OutOfTime || e[0].Commit != "" ||
		first.Next != "e:6" {
		t.Errorf("a page whose first entry the walk did not reach is %+v, next %q, want d/e5 out of time alone, "+
			"next e:6", e, first.Next)
	}
	only := cutRead(0, "d2", "")
	if e := only.TreeCommits.Entries; len(e) != 1 || !e[0].OutOfTime || only.Next != "" {
		t.Errorf("a one-entry directory the walk did not reach is %+v, next %q, want it out of time, no next",
			e, only.Next)
	}
	for _, next := range []string{a.Next, first.Next} {
		from, _ := strconv.Atoi(strings.TrimPrefix(next, "e:"))
		c.held(t, head, "d", from)
	}
	ctx, cancel := gitlet.AnswerBy(t.Context(), time.Now().Add(gitlet.ReplyMargin+2*time.Second))
	defer cancel()
	due := x.server(0, query.Options{Git: &treeCut{Pool: x.f.Nodes()[0].Pool(), due: true}})
	if _, err := due.Read(ctx, treeCommitsQ(repo, head, "d")); !unready(err, gitlet.UnreadyShare) {
		t.Errorf("a walk its answer's due instant ended = %v, want unready, share", err)
	}
}

// aloneEnds is a query.Pool on which every entry's walk alone (`log -1`) fails
// with err, or makes the read's answer fall due where due is set.
type aloneEnds struct {
	query.Pool
	err error
	due context.CancelCauseFunc
	ran atomic.Int32
}

func (p *aloneEnds) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if c.Args[0] != "log" || c.Args[1] != "-1" {
		return p.Pool.Run(ctx, c)
	}
	p.ran.Add(1)
	if p.due != nil {
		p.due(gitlet.ErrAnswerDue)
		return gitproc.Result{}, context.Cause(ctx)
	}
	return gitproc.Result{}, p.err
}

// unrecalled is a query.Cache that keeps what a read puts and finds none of
// it, so each read runs every walk, the window-only ones included.
type unrecalled struct{ query.Cache }

func (unrecalled) MemoGet(cache.Synced, cache.MemoKey) ([]byte, bool)   { return nil, false }
func (unrecalled) MemoProbe(cache.Synced, cache.MemoKey) ([]byte, bool) { return nil, false }

// AN ANSWERED PAGE OUTLASTS ITS WINDOWS' WALKS: a window-only walk ended by the
// budget, a busy lane or the due instant leaves the answered page whole and
// kept; the same end of a page entry's walk still fails or cuts the read.
// Mutation: run the page's log after the window-only walks, or fail on their end.
func TestAnAnsweredPageOutlastsItsWindowsWalks(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	tree := func(a, b string) map[string]histNode { return map[string]histNode{"d/a": file(a), "d/b": file(b)} }
	const main = "refs/heads/main"
	ids := writeHistory(t, x.p.Git(), x.work, []histCommit{
		{ref: main, tree: tree("1", "1"), date: 1_700_000_000},
		{ref: "refs/heads/side", parents: []int{0}, tree: tree("2", "1"), date: 1_700_000_100},
		{ref: main, parents: []int{0, 1}, tree: tree("2", "1"), date: 1_700_000_200}, // d/a from the side
		{ref: main, parents: []int{2}, tree: tree("2", "2"), date: 1_700_000_300},
	})
	x.push(gitlettest.RepoName, "refs/heads/*:refs/heads/*")
	repo := x.repo(gitlettest.RepoName)
	x.settle(0, repo)
	read := func(ctx context.Context, pool *aloneEnds, memo bool, cursor string) (gitlet.GitAnswer, error) {
		o := query.Options{Git: pool, Cache: unrecalled{x.f.Nodes()[0].Cache()}}
		if !memo {
			o.Cache = memoless{x.f.Nodes()[0].Cache()}
		}
		s := x.server(0, o)
		query.SetTreeWalk(s, query.TreeWalk{Span: 1})
		q := treeCommitsQ(repo, ids[3], "d")
		q.Cursor = cursor
		return s.Read(ctx, q)
	}
	busy := fmt.Errorf("%w: as the test's pool tells it", gitproc.ErrBusy)
	deadline := fmt.Errorf("%w: as the test's pool tells it", gitproc.ErrDeadline)
	for _, end := range []struct {
		name string
		err  error
		due  bool
	}{{"busy", busy, false}, {"budget", deadline, false}, {"due", nil, true}} {
		ctx, due := context.WithCancelCause(t.Context())
		pool := &aloneEnds{Pool: x.f.Nodes()[0].Pool(), err: end.err}
		if end.due {
			pool.due = due
		}
		a, err := read(ctx, pool, true, "e:1")
		due(nil)
		if pool.ran.Load() == 0 {
			t.Errorf("%s: no walk alone ran for the entry off the page", end.name)
		}
		if err != nil {
			t.Fatalf("%s: a page answered before an entry off it was walked = %v, want the page", end.name, err)
		}
		if e := a.TreeCommits.Entries; len(e) != 1 || e[0].Path != "d/b" || e[0].Commit != ids[3] ||
			a.TreeCommits.Cut() || a.Next != "" {
			t.Errorf("%s: the page is %+v (cut %t), next %q, want d/b at the tip whole", end.name, e,
				a.TreeCommits.Cut(), a.Next)
		}
	}
	q := treeCommitsQ(repo, ids[3], "d")
	q.Cursor = "e:1"
	s := x.server(0, query.Options{})
	query.SetTreeWalk(s, query.TreeWalk{Span: 1}) // the span keys the page
	if a := mustRead(t, s, q); !a.Memo {
		t.Errorf("a page whose windows' walk ended was not kept")
	}
	ctx, due := context.WithCancelCause(t.Context())
	defer due(nil)
	if _, err := read(ctx, &aloneEnds{Pool: x.f.Nodes()[0].Pool(), due: due}, false, ""); !unready(err,
		gitlet.UnreadyShare) {
		t.Errorf("a page entry's walk its answer's due instant ended = %v, want unready, share", err)
	}
	if _, err := read(t.Context(), &aloneEnds{Pool: x.f.Nodes()[0].Pool(), err: busy}, false, ""); !unready(err,
		gitlet.UnreadyShare) {
		t.Errorf("a page entry's walk a busy lane refused = %v, want unready, share", err)
	}
	a, err := read(t.Context(), &aloneEnds{Pool: x.f.Nodes()[0].Pool(), err: deadline}, false, "")
	if err != nil || !a.TreeCommits.Cut() {
		t.Errorf("a page entry's walk its budget ended = %v, want the page cut", err)
	}
}

// A TREE WALK CANNOT BE FORGED BY A PATH: entries at the root named a commit
// id, a line feed and a status, a status, `:x`, `--output=x` and one of 4 000
// bytes, and a merge of 1 800 parents whose %P is past a token's bound, each
// answer git's own.
// Mutation: read a path as an id or a status, or hold %P to the token bound.
func TestATreeWalkCannotBeForgedByAPath(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	names := []string{strings.Repeat("ab", 20), "\nM", "M", "A", ":x", "--output=x", "L" + strings.Repeat("o", 3999)}
	tree := map[string]histNode{}
	for _, n := range names {
		tree[n] = file("1")
	}
	const parents = 1800
	hist := []histCommit{{ref: "refs/heads/main", tree: tree, date: 1_700_000_000}}
	for i := range parents - 1 {
		hist = append(hist, histCommit{ref: "refs/heads/root", tree: map[string]histNode{"p": file("p")},
			date: 1_600_000_000 + int64(i)})
	}
	merged := maps.Clone(tree)
	merged["M"] = file("2")
	hist = append(hist, histCommit{ref: "refs/heads/main", tree: merged, date: 1_700_000_100})
	for i := range parents {
		hist[len(hist)-1].parents = append(hist[len(hist)-1].parents, i)
	}
	tip := writeHistory(t, x.p.Git(), x.work, hist)[len(hist)-1]
	x.push(gitlettest.RepoName, "refs/heads/main:refs/heads/main")
	c := treeCase{git: x.p.Git(), work: x.work, repo: x.repo(gitlettest.RepoName), s: x.server(0, query.Options{})}
	got := c.held(t, tip, "", 0)
	if len(got) != len(names) {
		t.Fatalf("the root's last commits are %d entries, want %d", len(got), len(names))
	}
	for _, e := range got {
		if (e.Path == "M") != (e.Commit == tip) {
			t.Errorf("%q answered %s; only M is the merge's", e.Path, e.Commit)
		}
	}
}

// A WINDOWLESS DIRECTORY IS READ IN BOUNDED STEPS: 2 000 entries whose names
// begin no window are cut every 256, so a read at e:1000 keeps [768, 1280):
// its page holds at most a span, its log names at most a span of commits, no
// argument list passes 16 KiB, and every answer is git's.
// Mutation: let a window grow with the directory.
func TestAWindowlessDirectoryIsReadInBoundedSteps(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	var names []string
	for i := 0; len(names) < 2000; i++ {
		if n := fmt.Sprintf("w%05d", i); !query.WindowStart(n, 64) {
			names = append(names, n)
		}
	}
	if got := query.WindowStarts(names, 64); len(got) != 8 || got[3] != 768 || got[5] != 1280 {
		t.Fatalf("2 000 names beginning none begin windows at %v, want every 256", got)
	}
	tree := map[string]histNode{}
	for _, n := range names {
		tree["d/"+n] = file("0")
	}
	hist := []histCommit{{ref: "refs/heads/main", tree: tree, date: 1_700_000_000}}
	for k := 1; k <= 200; k++ {
		tr := maps.Clone(hist[k-1].tree)
		tr["d/"+names[900+k]] = file("1")
		hist = append(hist, histCommit{ref: "refs/heads/main", parents: []int{k - 1}, tree: tr,
			date: 1_700_000_000 + int64(k)*100})
	}
	tip := writeHistory(t, x.p.Git(), x.work, hist)[200]
	x.push(gitlettest.RepoName, "refs/heads/main:refs/heads/main")
	rec := &recording{}
	c := treeCase{git: x.p.Git(), work: x.work, repo: x.repo(gitlettest.RepoName),
		s: x.server(0, query.Options{Git: rec.on(x, 0)})}
	q := treeCommitsQ(c.repo, tip, "d")
	q.Cursor = "e:1000"
	a := mustRead(t, c.s, q)
	if n := len(a.TreeCommits.Entries); n == 0 || n > 100 || len(a.TreeCommits.Commits) > 100 {
		t.Errorf("the page holds %d entries and %d commits, want at most a span of each", n,
			len(a.TreeCommits.Commits))
	}
	for _, e := range a.TreeCommits.Entries {
		if want := c.lastCommit(t, tip, e.Path); e.Commit != want {
			t.Errorf("%s answered %s, want git's %s", e.Path, e.Commit, want)
		}
	}
	for _, r := range rec.ran() {
		size := 0
		for _, arg := range r.Args {
			size += len(arg)
		}
		if size > 16<<10 {
			t.Errorf("git %s ran with %d bytes of arguments, past 16 KiB", r.Args[0], size)
		}
	}
}

// A FAR-FUTURE COMMIT FAILS ITS READ, NOT ITS NODE: a commit dated past the year
// 9999, the last of an entry of d/, fails the read of d/ as output that does not
// parse, and the read of another directory still answers.
// Mutation: reach jsonLen with the date, which panics.
func TestAFarFutureCommitFailsItsReadNotItsNode(t *testing.T) {
	t.Parallel()
	x := newFixture(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	before := x.commit(map[string]string{"d/a": "1\n", "e/b": "1\n"}, "before")
	x.git("rm", "-q", "--cached", "d/a")
	blob, _, err := x.p.Git().Try(t, x.work, strings.NewReader("2\n"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	x.git("update-index", "--add", "--cacheinfo", "100644,"+strings.TrimSpace(blob)+",d/a")
	tree := strings.TrimSpace(x.git("write-tree"))
	body := fmt.Sprintf("tree %s\nparent %s\nauthor T <t@example.com> 253402300800 +0000\n"+
		"committer T <t@example.com> 253402300800 +0000\n\nfar\n", tree, before)
	far, _, err := x.p.Git().Try(t, x.work, strings.NewReader(body), "hash-object", "-t", "commit", "-w", "--stdin")
	if err != nil {
		t.Fatal(err)
	}
	far = strings.TrimSpace(far)
	x.git("update-ref", "refs/heads/main", far)
	x.push(gitlettest.RepoName, "main")
	repo := x.repo(gitlettest.RepoName)
	s := x.server(0, query.Options{})
	if _, err := s.Read(t.Context(), treeCommitsQ(repo, far, "d")); err == nil ||
		!strings.Contains(err.Error(), "does not parse as asked") || !strings.Contains(err.Error(), "9999") {
		t.Errorf("the read of d/, whose last commit is dated past the year 9999 = %v, want output that does not "+
			"parse", err)
	}
	if e := mustRead(t, s, treeCommitsQ(repo, far, "e")).TreeCommits.Entries; len(e) != 1 || e[0].Commit != before {
		t.Errorf("the read of e/ answered %+v, want e/b's commit", e)
	}
}
