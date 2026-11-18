package query_test

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// BenchmarkFind times a find through a node's query server over findFiles
// files: query=first lists the tree and stages its pages, query=typing walks
// them from the memo. Each reports p50, p95 and p99.
func BenchmarkFind(b *testing.B) {
	gitlettest.Quiet(b)
	r := newFindRig(b)
	b.Run("query=first", func(b *testing.B) {
		took := make([]time.Duration, 0, b.N)
		bound := gitlet.MaxGitAnswerBytes
		for b.Loop() {
			b.StopTimer()
			bound--
			s := r.server(b)
			query.SetPageBytes(s, bound)
			b.StartTimer()
			start := time.Now()
			a, err := s.Read(b.Context(), findQ(r.repo, r.commit, "cmd", 0))
			took = append(took, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
			if err != nil {
				b.Fatal(err)
			}
			if a.Find.Files != findFiles {
				b.Fatalf("the find counts %d files, want %d", a.Find.Files, findFiles)
			}
		}
		report(b, took)
	})
	b.Run("query=typing", func(b *testing.B) {
		s := r.server(b)
		if _, err := s.Read(b.Context(), findQ(r.repo, r.commit, findTyping[0], 0)); err != nil {
			b.Fatal(err)
		}
		took := make([]time.Duration, 0, b.N)
		i := 0
		for b.Loop() {
			q := findTyping[i%len(findTyping)]
			i++
			start := time.Now()
			a, err := s.Read(b.Context(), findQ(r.repo, r.commit, q, 0))
			took = append(took, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
			switch {
			case err != nil:
				b.Fatal(err)
			case a.Find.Total == 0:
				b.Fatalf("the find of %q found nothing", q)
			}
		}
		report(b, took)
	})
}

const (
	// findFiles and findDirs are the benchmark's tree: a large monorepo's
	// hundred thousand files, a hundred to a directory.
	findFiles = 100_000
	findDirs  = 1_000
)

// findTyping is a person typing `cmd/web/ma`, a query a keystroke.
var findTyping = []string{"c", "cm", "cmd", "cmd/", "cmd/w", "cmd/web/ma"}

// findPath is the path of the tree's f-th file: a directory under one of five
// roots, then one of five names.
func findPath(f int) string {
	d := f / (findFiles / findDirs)
	roots := []string{"cmd/web", "internal/api", "pkg/store", "web/ui", "docs/guide"}
	names := []string{"main", "server", "handler", "model", "util"}
	return fmt.Sprintf("%s%03d/%s%02d.go", roots[d%len(roots)], d, names[f%len(names)], f%(findFiles/findDirs))
}

// findRig is BenchmarkFind's fixture: a fleet of one Gitlet node holding the
// tree's one commit.
type findRig struct {
	f      *gitlettest.SoloFleet
	repo   string
	commit string
}

// newFindRig fast-imports the tree as one commit and pushes it once.
func newFindRig(b *testing.B) *findRig {
	b.Helper()
	c := gittest.NewClient(b)
	src := c.Init(b)
	gittest.FastImport(b, c, src, func(w *bufio.Writer) error {
		const body = "package find\n"
		fmt.Fprintf(w, "blob\nmark :1\ndata %d\n%s\n", len(body), body)
		const msg = "a tree of a hundred thousand files\n"
		fmt.Fprintf(w, "commit refs/heads/main\ncommitter Crewlet Bench <bench@example.com> 1767225600 +0000\n"+
			"data %d\n%s", len(msg), msg)
		for f := range findFiles {
			fmt.Fprintf(w, "M 100644 :1 %s\n", findPath(f))
		}
		return w.WriteByte('\n')
	})
	commit := strings.TrimSpace(c.Run(b, src, "rev-parse", "main"))
	f := gitlettest.SoloNode(b, gitlettest.SoloOptions{People: []string{"ceo"}, CacheMaxBytes: readCacheBytes,
		SyncAlways: true})
	f.Declare(b, gitlet.Declared{Name: gitlettest.RepoName, Settings: gitlettest.Settings()})
	p := gitlettest.NewPerson(b, f, "ceo")
	if res := p.Push(b.Context(), src, f.Nodes()[0].URL()+"/git/"+gitlettest.RepoName,
		commit+":refs/heads/main"); res.Err != nil {
		b.Fatalf("git push: %s", res)
	}
	rows, err := f.Rows(b.Context(), gitlettest.RepoName, gitlettest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("the pool runs git %s; one commit of %d files in %d directories", f.Nodes()[0].Pool().Version(),
		findFiles, findDirs)
	return &findRig{f: f, repo: rows.Repo.ID, commit: commit}
}

// server is a query server over the node's cache, its memo kept.
func (r *findRig) server(b *testing.B) *query.Server {
	b.Helper()
	n := r.f.Nodes()[0]
	s, err := query.New(query.Options{NodeID: n.ID(), Cache: n.Cache(), Git: n.Pool(),
		Share: gitlet.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil), Resyncs: gitlet.NewResyncs(nil, nil),
		Roster: func() gitlet.Roster { return nil }, Fetched: noFetched})
	if err != nil {
		b.Fatal(err)
	}
	return s
}
