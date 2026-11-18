package host_test

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkEngineRebase measures what gitlet.RebaseBuildDeadline is read
// against: a rebase_change's build (its listing, its commits read, one
// merge-tree and one commit-tree a replayed commit) of commits=10, 100 and
// gitlet.MaxRebaseCommits commits, each touching its own file, onto a target
// moved by one commit, over files=1 000 files and, with CREWLET_GIT_LARGE=1
// (gittest.LargeEnv), 100 000. Each iteration moves the target again, so every
// one replays the whole change. ms/build is the host's `build` stage, the
// figure the deadline bounds; ms/write the whole write. Figures: package doc,
// "Measured" (WHAT A REBASE COSTS).
func BenchmarkEngineRebase(b *testing.B) {
	gitlettest.Quiet(b)
	for _, files := range []int{rebaseTreeFiles, rebaseTreeFilesLarge} {
		for _, commits := range []int{10, 100, gitlet.MaxRebaseCommits} {
			b.Run(fmt.Sprintf("files=%d/commits=%d", files, commits), func(b *testing.B) {
				if files == rebaseTreeFilesLarge && os.Getenv(gittest.LargeEnv) != "1" {
					gittest.Large(b, "a tree of 100 000 files rebased through the host")
				}
				r := newRebaseRig(b, files, commits)
				var builds, writes []time.Duration
				for b.Loop() {
					b.StopTimer()
					r.moveMain(b)
					r.fig.reset()
					b.StartTimer()
					began := time.Now()
					res, err := r.f.Nodes()[0].Host().Write(b.Context(), gitlet.EngineWrite{
						Kind: gitlet.WriteRebaseChange, RepoID: r.repo, Principal: pat(),
						WrittenFor: types.ForNobody(), OpID: opNow(),
						Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}, Ref: featureRef,
						ExpectedHead: r.head, Change: r.change})
					took := time.Since(began) //fixedwait:measure a benchmark reports this; no gate reads it
					b.StopTimer()
					if err != nil || res.Outcome != statelog.OutcomeApplied {
						b.Fatalf("the rebase did not land: %+v %v", res, err)
					}
					r.head = res.Commit
					writes = append(writes, took)
					builds = append(builds, r.fig.stage("build")...)
					b.StartTimer()
				}
				b.ReportMetric(medianMillis(builds), "ms/build")
				b.ReportMetric(medianMillis(writes), "ms/write")
			})
		}
	}
}

// The trees BenchmarkEngineRebase rebases over.
const (
	rebaseTreeFiles      = 1_000
	rebaseTreeFilesLarge = 100_000
)

// rebaseRig is one arm's fixture: one Gitlet node holding a repository whose
// main holds the arm's tree, and pat's change from feature of the arm's
// commits on it.
type rebaseRig struct {
	f      *gitlettest.SoloFleet
	fig    *writeFigures
	p      *gitlettest.Person
	work   string
	repo   string
	change int64
	head   string
	moves  int
}

// newRebaseRig builds the fixture with ONE fast-import: main's root commit of
// files files, then feature's commits on it, each adding a file of its own.
func newRebaseRig(b *testing.B, files, commits int) *rebaseRig {
	b.Helper()
	r := &rebaseRig{fig: newWriteFigures()}
	r.f = writeFleet(b, memobj.New(), r.fig)
	r.f.Declare(b, gitlet.Declared{Name: gitlettest.RepoName, Settings: gitlettest.Settings()})
	r.p = gitlettest.NewPerson(b, r.f, gitlettest.FixturePat)
	r.work = r.p.Init()
	now := time.Now().Unix()
	gittest.FastImport(b, r.p.Git(), r.work, func(w *bufio.Writer) error {
		if _, err := fmt.Fprintf(w, "commit refs/heads/main\nmark :1\ncommitter A <a@example.com> %d +0000\n"+
			"data 5\ntree\n", now); err != nil {
			return err
		}
		if err := commitTree(w, files); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "\n"); err != nil {
			return err
		}
		for i := range commits {
			from := ""
			if i == 0 {
				from = "from :1\n"
			}
			if _, err := fmt.Fprintf(w, "commit refs/heads/feature\nauthor Ada <ada@example.com> %d +0200\n"+
				"committer Ada <ada@example.com> %d +0200\ndata 10\nchange %02d\n%s", now+int64(i), now+int64(i),
				i%100, from); err != nil {
				return err
			}
			if err := inlineFile(w, fmt.Sprintf("feature/f%06d.txt", i)); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "\n"); err != nil {
				return err
			}
		}
		return nil
	})
	url := repoURL(r.f.Nodes()[0])
	mustPush(b, r.p, r.work, url, "main")
	r.head = strings.TrimSpace(r.p.Git().Run(b, r.work, "rev-parse", "feature"))
	if res := r.p.PushArgs(b.Context(), r.work, []string{"-o", "change"}, url, "feature"); res.Err != nil {
		b.Fatalf("open the change: %v\n%s", res.Err, res.Stderr)
	}
	rows, err := r.f.Rows(b.Context(), gitlettest.RepoName, gitlettest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	r.repo, r.change = rows.Repo.ID, 1
	return r
}

// moveMain moves main on by one commit of pat's, outside the measurement.
func (r *rebaseRig) moveMain(b *testing.B) {
	b.Helper()
	r.moves++
	r.p.Git().Run(b, r.work, "checkout", "-q", "main")
	r.p.Commit(r.work, map[string]string{fmt.Sprintf("main-%d.txt", r.moves): "moved\n"}, "main moves on")
	mustPush(b, r.p, r.work, repoURL(r.f.Nodes()[0]), "main")
}

// medianMillis is the median of samples in milliseconds.
func medianMillis(samples []time.Duration) float64 {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return quantile(sorted, 0.50)
}
