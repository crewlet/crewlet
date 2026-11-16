package query_test

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkTreeCommits' histories: treeBenchCommits commits each, over a root
// of 40 dotfiles and LICENSE untouched since the first commit, beside
// Makefile, README.md and go.mod (changed now and then), src/ of 400 busy
// files and internal/a/b/c/ of 300.
const (
	treeBenchCommits   = 100_000
	treeBenchUntouched = 40
	treeBenchWide      = 400
	treeBenchDeep      = 300
)

// treeBenchPath is the i-th busy file: src/'s, then internal/a/b/c/'s.
func treeBenchPath(i int) string {
	if i < treeBenchWide {
		return fmt.Sprintf("src/w%03d.go", i)
	}
	return fmt.Sprintf("internal/a/b/c/d%03d.go", i-treeBenchWide)
}

// treeBenchHistory writes shape's history into dir: linear, each commit
// changing one to three files; or merges, the same changes in runs of one to
// four commits on a side branch merged by a merge carrying the side's tree, a
// third of them pushed straight to main. The last commit is a direct one that
// changes a file of src/ and one of internal/a/b/c/.
func treeBenchHistory(b *testing.B, git *gittest.Client, dir, shape string) {
	rng := rand.New(rand.NewPCG(8, 8))
	const who = "Crewlet Bench <bench@example.com>"
	gittest.FastImport(b, git, dir, func(w *bufio.Writer) error {
		mark, date := 0, int64(1_600_000_000)
		commit := func(ref string, from, merge int, files map[string]string) int {
			mark++
			date += 60
			msg := fmt.Sprintf("commit %d", mark)
			fmt.Fprintf(w, "commit %s\nmark :%d\ncommitter %s %d +0000\ndata %d\n%s\n", ref, mark, who, date,
				len(msg), msg)
			if from > 0 {
				fmt.Fprintf(w, "from :%d\n", from)
			}
			if merge > 0 {
				fmt.Fprintf(w, "merge :%d\n", merge)
			}
			for path, body := range files {
				fmt.Fprintf(w, "M 100644 inline %s\ndata %d\n%s\n", path, len(body), body)
			}
			w.WriteString("\n")
			return mark
		}
		change := func() map[string]string {
			files := map[string]string{}
			for range 1 + rng.IntN(3) {
				files[treeBenchPath(rng.IntN(treeBenchWide+treeBenchDeep))] = fmt.Sprintf("%d\n", rng.Int())
			}
			if rng.IntN(500) == 0 {
				files[[]string{"Makefile", "README.md", "go.mod"}[rng.IntN(3)]] = fmt.Sprintf("%d\n", rng.Int())
			}
			return files
		}
		first := map[string]string{"LICENSE": "license\n", "Makefile": "all:\n", "README.md": "readme\n",
			"go.mod": "module bench\n"}
		for i := range treeBenchUntouched {
			first[fmt.Sprintf(".f%02d", i)] = "dot\n"
		}
		for i := range treeBenchWide + treeBenchDeep {
			first[treeBenchPath(i)] = "0\n"
		}
		tip := commit("refs/heads/main", 0, 0, first)
		for mark < treeBenchCommits-1 {
			if shape == "linear" || rng.IntN(3) == 0 {
				tip = commit("refs/heads/main", tip, 0, change())
				continue
			}
			side, merged := tip, map[string]string{}
			for range 1 + rng.IntN(4) {
				files := change()
				side = commit("refs/heads/side", side, 0, files)
				for p, body := range files {
					merged[p] = body
				}
			}
			tip = commit("refs/heads/main", tip, side, merged)
		}
		commit("refs/heads/main", tip, 0, map[string]string{treeBenchPath(0): "tip\n",
			treeBenchPath(treeBenchWide): "tip\n"})
		return nil
	})
}

// counting is a query.Pool that counts the processes it runs.
type counting struct {
	query.Pool
	runs atomic.Int64
}

func (c *counting) Run(ctx context.Context, cmd gitproc.Cmd) (gitproc.Result, error) {
	c.runs.Add(1)
	return c.Pool.Run(ctx, cmd)
}

// readOnly is a node's cache whose memo is read and never written: every
// timed read of a derived arm misses its page and keeps nothing.
type readOnly struct{ query.Cache }

func (readOnly) Memo(cache.Synced) *cache.MemoBatch { return nil }

// graphless is a node's cache serving a copy of its directory with no
// commit-graph: what a read meets before a copy's first maintenance.
type graphless struct {
	query.Cache
	dir string
}

func (g graphless) Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error) {
	s, err := g.Cache.Sync(ctx, repoID, fresh)
	s.Dir = g.dir
	return s, err
}

// unGraphed is a copy of dir, its files hard links, with no commit-graph.
func unGraphed(b *testing.B, dir string) string {
	b.Helper()
	to := filepath.Join(b.TempDir(), "graphless")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		switch {
		case rel == filepath.Join("objects", "info", "commit-graph"):
			return nil
		case rel == filepath.Join("objects", "info", "commit-graphs"):
			return filepath.SkipDir
		case d.IsDir():
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		return os.Link(path, filepath.Join(to, rel))
	})
	if err != nil {
		b.Fatal(err)
	}
	return to
}

// BenchmarkTreeCommits times a first page over 100 000 commits by dir (root,
// src/, a deep one), cache (cold, or derived from the parent's windows) and
// commit-graph, reporting p50/p95/p99, processes/op and out-of-time/op.
func BenchmarkTreeCommits(b *testing.B) {
	gitlettest.Quiet(b)
	for _, shape := range []string{"linear", "merges"} {
		b.Run("shape="+shape, func(b *testing.B) {
			f := gitlettest.SoloNode(b, gitlettest.SoloOptions{People: []string{"ceo"}})
			f.Declare(b, gitlet.Declared{Name: gitlettest.RepoName, Settings: gitlettest.Settings()})
			p := gitlettest.NewPerson(b, f, "ceo")
			src := p.Git().Init(b)
			treeBenchHistory(b, p.Git(), src, shape)
			url := f.Nodes()[0].URL() + "/git/" + gitlettest.RepoName
			if res := p.Push(b.Context(), src, url, "refs/heads/main:refs/heads/main"); res.Err != nil {
				b.Fatalf("git push: %s", res)
			}
			rows, err := f.Rows(b.Context(), gitlettest.RepoName, gitlettest.Linearizable)
			if err != nil {
				b.Fatal(err)
			}
			repo, tip := rows.Repo.ID, rows.Refs["refs/heads/main"]
			parent := strings.TrimSpace(p.Git().Run(b, src, "rev-parse", tip+"^"))
			n := f.Nodes()[0]
			graphed(b, n.Cache(), repo)
			s, err := n.Cache().Sync(b.Context(), repo, gitlettest.Linearizable)
			if err != nil {
				b.Fatal(err)
			}
			bare := unGraphed(b, s.Dir)
			s.Release()
			for _, dir := range []struct{ name, path string }{{"root", ""}, {"wide", "src"},
				{"deep", "internal/a/b/c"}} {
				for _, arm := range []struct{ cache, graph string }{{"cold", "no"}, {"cold", "yes"},
					{"derived", "yes"}} {
					b.Run(fmt.Sprintf("dir=%s/cache=%s/graph=%s", dir.name, arm.cache, arm.graph), func(b *testing.B) {
						var c query.Cache = n.Cache()
						if arm.graph == "no" {
							c = graphless{Cache: c, dir: bare}
						}
						at := parent
						if arm.cache == "cold" {
							c = unrecalled{c} // keeps its windows, as a cold read does, and finds none
						} else {
							at = tip
							warm := treeBenchServer(b, n, c, &counting{Pool: n.Pool()})
							if _, err := warm.Read(b.Context(), treeCommitsQ(repo, parent, dir.path)); err != nil {
								b.Fatal(err)
							}
							c = readOnly{c}
						}
						pool := &counting{Pool: n.Pool()}
						server := treeBenchServer(b, n, c, pool)
						took := make([]time.Duration, 0, b.N)
						cut := 0
						for b.Loop() {
							start := time.Now()
							a, err := server.Read(b.Context(), treeCommitsQ(repo, at, dir.path))
							took = append(took, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
							if err != nil {
								b.Fatal(err)
							}
							if a.TreeCommits.Cut() {
								cut++
							}
						}
						report(b, took)
						b.ReportMetric(float64(pool.runs.Load())/float64(len(took)), "processes/op")
						b.ReportMetric(float64(cut)/float64(len(took)), "out-of-time/op")
					})
				}
			}
		})
	}
}

// treeBenchServer is a query server of n over c and pool.
func treeBenchServer(b *testing.B, n gitlettest.Node, c query.Cache, pool query.Pool) *query.Server {
	b.Helper()
	s, err := query.New(query.Options{NodeID: n.ID(), Cache: c, Git: pool,
		Share:   gitlet.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil),
		Resyncs: gitlet.NewResyncs(nil, nil), Roster: func() gitlet.Roster { return nil }, Fetched: noFetched})
	if err != nil {
		b.Fatal(err)
	}
	return s
}
