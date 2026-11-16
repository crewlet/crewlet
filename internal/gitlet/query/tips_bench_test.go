package query_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// BenchmarkTips' history: main's commits, a three-commit branch forking every
// tipsBenchFork commits back, and an annotated tag every tipsBenchTagEvery.
const (
	tipsBenchCommits  = 20_000
	tipsBenchBranches = 80
	tipsBenchFork     = 250
	tipsBenchTags     = 20
	tipsBenchTagEvery = tipsBenchCommits / tipsBenchTags
)

// BenchmarkTips reads one screen of 80 branches and 20 annotated tags against
// main, every fact and count computed (cache=memoless) or from the memo
// (cache=memo), reporting p50, p95 and p99 of the screen and ms/tip.
func BenchmarkTips(b *testing.B) {
	gitlettest.Quiet(b)
	f := gitlettest.SoloNode(b, gitlettest.SoloOptions{People: []string{"ceo"}})
	f.Declare(b, gitlet.Declared{Name: gitlettest.RepoName, Settings: gitlettest.Settings()})
	p := gitlettest.NewPerson(b, f, "ceo")
	src := p.Git().Init(b)
	gittest.FastImport(b, p.Git(), src, func(w *bufio.Writer) error {
		const who = "Crewlet Bench <bench@example.com>"
		for i := 1; i <= tipsBenchCommits; i++ {
			body := fmt.Sprintf("line %d\n", i)
			fmt.Fprintf(w, "commit refs/heads/main\nmark :%d\ncommitter %s %d +0000\ndata %d\ncommit %d\n"+
				"M 100644 inline f\ndata %d\n%s\n", i, who, 1_700_000_000+i, len(fmt.Sprint("commit ", i)), i,
				len(body), body)
		}
		for i := range tipsBenchBranches {
			for j := range 3 {
				from := ""
				if j == 0 {
					from = fmt.Sprintf("from :%d\n", tipsBenchCommits-tipsBenchFork*i)
				}
				fmt.Fprintf(w, "commit refs/heads/b%03d\ncommitter %s %d +0000\ndata 6\nbranch\n%s"+
					"M 100644 inline g\ndata 2\n%d\n\n", i, who, 1_800_000_000+3*i+j, from, j)
			}
		}
		for i := range tipsBenchTags {
			msg := fmt.Sprintf("release %d", i)
			fmt.Fprintf(w, "tag v%02d\nfrom :%d\ntagger %s %d +0000\ndata %d\n%s\n", i, (i+1)*tipsBenchTagEvery,
				who, 1_900_000_000+i, len(msg), msg)
		}
		return nil
	})
	url := f.Nodes()[0].URL() + "/git/" + gitlettest.RepoName
	if res := p.Push(b.Context(), src, url, "refs/heads/*:refs/heads/*", "refs/tags/*:refs/tags/*"); res.Err != nil {
		b.Fatalf("git push: %s", res)
	}
	rows, err := f.Rows(b.Context(), gitlettest.RepoName, gitlettest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	repo, main := rows.Repo.ID, rows.Refs["refs/heads/main"]
	var oids []string
	for i := range tipsBenchBranches {
		oids = append(oids, rows.Refs[fmt.Sprintf("refs/heads/b%03d", i)])
	}
	oids = append(oids, strings.Fields(p.Git().Run(b, src, "for-each-ref", "--format=%(objectname)", "refs/tags"))...)
	if len(oids) != gitlet.MaxTips {
		b.Fatalf("the screen names %d tips, want %d", len(oids), gitlet.MaxTips)
	}
	n := f.Nodes()[0]
	graphed(b, n.Cache(), repo)
	for _, arm := range []struct {
		name  string
		cache query.Cache
	}{{"memoless", memoless{n.Cache()}}, {"memo", n.Cache()}} {
		b.Run(fmt.Sprintf("tips=%d/cache=%s", gitlet.MaxTips, arm.name), func(b *testing.B) {
			s, err := query.New(query.Options{NodeID: n.ID(), Cache: arm.cache, Git: n.Pool(),
				Share:   gitlet.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil),
				Resyncs: gitlet.NewResyncs(nil, nil), Roster: func() gitlet.Roster { return nil }, Fetched: noFetched})
			if err != nil {
				b.Fatal(err)
			}
			screen := func() {
				q := tipsQ(repo, main, oids...)
				got := 0
				for {
					a, err := s.Read(b.Context(), q)
					if err != nil {
						b.Fatal(err)
					}
					for _, tip := range a.Tips.Tips {
						if tip.Commit == nil || tip.Behind == nil {
							b.Fatalf("tip %s was answered %+v, want its commit counted", tip.OID, tip)
						}
					}
					got += len(a.Tips.Tips)
					if a.Next == "" {
						break
					}
					q.Cursor = a.Next
				}
				if got != len(oids) {
					b.Fatalf("the screen answered %d tips, want %d", got, len(oids))
				}
			}
			screen() // the memo arm's reads are warm from here on
			took := make([]time.Duration, 0, b.N)
			for b.Loop() {
				start := time.Now()
				screen()
				took = append(took, time.Since(start)) //fixedwait:measure a benchmark reports this; no gate reads it
			}
			p50 := report(b, took)
			b.ReportMetric(float64(p50)/float64(time.Millisecond)/float64(len(oids)), "ms/tip")
		})
	}
}

// graphed waits until c's copy of repo holds its commit-graph, which every
// maintained copy's counts walk by.
func graphed(b *testing.B, c *cache.Cache, repo string) {
	b.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for {
		s, err := c.Sync(b.Context(), repo, gitlettest.Linearizable)
		if err != nil {
			b.Fatal(err)
		}
		_, whole := os.Stat(filepath.Join(s.Dir, "objects", "info", "commit-graph"))
		_, chain := os.Stat(filepath.Join(s.Dir, "objects", "info", "commit-graphs", "commit-graph-chain"))
		s.Release()
		if whole == nil || chain == nil {
			return
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the benchmark
			b.Fatal("the copy holds no commit-graph five minutes on")
		}
		time.Sleep(50 * time.Millisecond) //fixedwait:liveness
	}
}
