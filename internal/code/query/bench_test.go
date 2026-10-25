package query_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkCodeRead is a seat's read by object id, timed whole through a git
// node's query server — its sync at the copy as it stands, the check of what
// it names, its unit of the share and its kind's git — over gittest.History's
// composition pushed a session at a time, as a repository of that history
// comes to a host:
//
//   - history=32MB is about 2 800 files in sixty directories and 13 000
//     commits; history=1GB, about 85 000 files and 400 000 commits, runs only
//     under CREWLET_BENCH_LARGE.
//   - kind is the read: the root's listing, the whole tree's first page
//     (recursive), a file's first page and its fiftieth (a 3 MB file of
//     lines added on top), the stat of one session's pushes — what a
//     pull request of that size diffs — one file's patch from it, and a
//     comparison of the history's middle with its tip.
//   - cache=warm reads the node that took the pushes, its copy current;
//     cache=cold reads a git node emptied before every iteration, whose first
//     read waits statelog.ReadBudget on the download and answers what it can.
//
// Each warm arm reports the p50, p95 and p99 of one read; the cold arm the
// time to its answer and the share of answers unready. The figures decide
// code.GitShareWait — max(2 s, the slowest warm kind's p95 rounded up to a
// second), under gitproc.ReadDeadline/4 — and confirm the read deadline's
// terms (a warm read well inside gitproc.ReadDeadline, a cold one answered
// inside statelog.ReadBudget and its margin).
func BenchmarkCodeRead(b *testing.B) {
	codetest.Quiet(b)
	for _, size := range []struct {
		name  string
		bytes int64
	}{{"32MB", 32 << 20}, {"1GB", 1 << 30}} {
		b.Run("history="+size.name, func(b *testing.B) {
			if size.name == "1GB" {
				gittest.Large(b, "a 1 GB history fast-imported and pushed a session at a time")
			}
			r := newReadRig(b, size.bytes)
			for _, k := range r.kinds(b) {
				b.Run("kind="+k.name+"/cache=warm", func(b *testing.B) { r.warm(b, k.q) })
			}
			b.Run("kind=tree-root/cache=cold", func(b *testing.B) { r.cold(b) })
		})
	}
}

// readRig is BenchmarkCodeRead's fixture: a fleet of two git nodes holding a
// history, its commits, and a query server over the first node.
type readRig struct {
	f       *codetest.SoloFleet
	repo    string
	commits []string
	tip     string
	s       *query.Server
}

// bigLines is the lines of the file a page-50 read pages through: about
// 3 MB, so its fiftieth 56 KiB page is near its end.
const bigLines = 60_000

func newReadRig(b *testing.B, size int64) *readRig {
	b.Helper()
	f := codetest.SoloNode(b, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: 2}, People: []string{"ceo"},
		SyncAlways: true})
	f.Declare(b, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	p := codetest.NewPerson(b, f, "ceo")
	src, commits := gittest.History(b, p.Git(), size)
	url := f.Nodes()[0].URL() + "/git/" + codetest.RepoName
	for s := 1; s <= gittest.HistorySessions; s++ {
		if res := p.Push(b.Context(), src, url, commits[s*len(commits)/gittest.HistorySessions-1]+
			":refs/heads/main"); res.Err != nil {
			b.Fatalf("git push: %s", res)
		}
	}
	work := filepath.Join(b.TempDir(), "work")
	p.Git().Run(b, "", "clone", "--quiet", src, work)
	if err := os.RemoveAll(src); err != nil {
		b.Fatal(err)
	}
	var big strings.Builder
	for i := range bigLines {
		fmt.Fprintf(&big, "%06d the quick brown fox jumps over the lazy dog\n", i)
	}
	tip := p.Commit(work, map[string]string{"big.txt": big.String()}, "big")
	if res := p.Push(b.Context(), work, url, "main"); res.Err != nil {
		b.Fatalf("git push: %s", res)
	}
	rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	n := f.Nodes()[0]
	s, err := query.New(query.Options{NodeID: n.ID(), Cache: n.Cache(), Git: n.Pool(),
		Share:  code.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil),
		Roster: func() code.Roster { return nil }})
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("the pools run git %s; %d commits in %d packs", n.Pool().Version(), len(commits)+1, len(rows.Packs))
	return &readRig{f: f, repo: rows.Repo.ID, commits: commits, tip: tip, s: s}
}

// kind is one warm arm's read.
type kind struct {
	name string
	q    code.GitQuery
}

// kinds is every warm arm's read, its cursors and paths read off the history
// first.
func (r *readRig) kinds(b *testing.B) []kind {
	b.Helper()
	ctx := b.Context()
	page := func(q code.GitQuery, n int) code.GitQuery {
		for range n - 1 {
			a, err := r.s.Read(ctx, q)
			if err != nil || a.Next == "" {
				b.Fatalf("paging to page %d: %v (next %q)", n, err, a.Next)
			}
			q.Cursor = a.Next
		}
		return q
	}
	session := r.commits[len(r.commits)-len(r.commits)/gittest.HistorySessions-1]
	last := r.commits[len(r.commits)-1]
	stat, err := r.s.Read(ctx, diffQ(r.repo, code.DiffQuery{Base: session, Head: last}))
	if err != nil || len(stat.Diff.Files) == 0 {
		b.Fatalf("the session's stat: %v", err)
	}
	file := slices.MaxFunc(stat.Diff.Files, func(a, c code.DiffFile) int { return a.Added - c.Added }).Path
	return []kind{
		{"tree-root", treeQ(r.repo, code.TreeQuery{Rev: r.tip})},
		{"tree-recursive", treeQ(r.repo, code.TreeQuery{Rev: r.tip, Recursive: true})},
		{"blob-first-page", blobQ(r.repo, code.BlobQuery{Rev: r.tip, Path: "big.txt"})},
		{"blob-page-50", page(blobQ(r.repo, code.BlobQuery{Rev: r.tip, Path: "big.txt"}), 50)},
		{"diff-stat", diffQ(r.repo, code.DiffQuery{Base: session, Head: last})},
		{"diff-one-file", diffQ(r.repo, code.DiffQuery{Base: session, Head: last, File: file})},
		{"compare", compareQ(r.repo, r.commits[len(r.commits)/2], last)},
	}
}

// warm times q over the first node, reporting its percentiles.
func (r *readRig) warm(b *testing.B, q code.GitQuery) {
	took := make([]time.Duration, 0, b.N)
	for b.Loop() {
		start := time.Now()
		if _, err := r.s.Read(b.Context(), q); err != nil {
			b.Fatal(err)
		}
		took = append(took, time.Since(start))
	}
	report(b, took)
}

// cold times the root's listing on the second node, emptied before every
// iteration, with the production sync wait.
func (r *readRig) cold(b *testing.B) {
	var took []time.Duration
	unreadies := 0
	g := r.f.GitNode(1)
	for range b.N {
		b.StopTimer()
		dir := g.Node().CacheDir()
		g.Stop(b.Context())
		if err := os.RemoveAll(dir); err != nil {
			b.Fatal(err)
		}
		g.Restart(b)
		n := g.Node()
		s, err := query.New(query.Options{NodeID: n.ID(), Cache: n.Cache(), Git: n.Pool(),
			Share:  code.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil),
			Roster: func() code.Roster { return nil }})
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		start := time.Now()
		_, err = s.Read(b.Context(), treeQ(r.repo, code.TreeQuery{Rev: r.tip}))
		took = append(took, time.Since(start))
		switch {
		case unready(err, code.UnreadySyncing):
			unreadies++
		case err != nil:
			b.Fatal(err)
		}
		if last := took[len(took)-1]; last > statelog.ReadBudget+code.ReplyMargin {
			b.Errorf("a cold read answered after %s, past the %s it waits and its margin", last,
				statelog.ReadBudget)
		}
	}
	report(b, took)
	b.ReportMetric(float64(unreadies)/float64(len(took)), "unready/op")
}

// report reports took's p50, p95 and p99 in milliseconds.
func report(b *testing.B, took []time.Duration) {
	if len(took) == 0 {
		return
	}
	slices.Sort(took)
	at := func(q float64) float64 {
		return float64(took[min(len(took)-1, int(q*float64(len(took))))]) / float64(time.Millisecond)
	}
	b.ReportMetric(at(0.50), "p50-ms")
	b.ReportMetric(at(0.95), "p95-ms")
	b.ReportMetric(at(0.99), "p99-ms")
}
