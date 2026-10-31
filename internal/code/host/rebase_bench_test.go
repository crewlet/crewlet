package host_test

import (
	"bufio"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkEngineRebase measures what code.RebaseBuildDeadline is read
// against: the build of a rebase_change — its listing, its commits read, and
// one merge-tree and one commit-tree a replayed commit — of a change of
// commits=10, 100 and code.MaxRebaseCommits commits, each touching a file of
// its own, onto a target that moved by one commit, over a tree of files=1 000
// files and — with CREWLET_GIT_LARGE=1 (gittest.LargeEnv) — 100 000. Each
// iteration moves the target again and rebases the change's head anew, so
// every one replays the whole change; ms/build is the host's own `build`
// stage, the figure the deadline bounds, and ms/write the whole write.
//
// MEASURED (git 2.56.0 on two E-cores of a Core Ultra 7 265U, the machine
// loaded beside another session, -benchtime 1x and one count — ONE rebase an
// arm, the bound of this machine's validation, so the figure is one sample
// and no percentile): see internal/code/host's doc.go, "What a rebase costs".
func BenchmarkEngineRebase(b *testing.B) {
	codetest.Quiet(b)
	for _, files := range []int{rebaseTreeFiles, rebaseTreeFilesLarge} {
		for _, commits := range []int{10, 100, code.MaxRebaseCommits} {
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
					res, err := r.f.Nodes()[0].Host().Write(b.Context(), code.EngineWrite{
						Kind: code.WriteRebaseChange, RepoID: r.repo, Principal: pat(), OpID: opNow(),
						Fresh: statelog.Freshness{Level: statelog.ReadLinearizable}, Ref: featureRef,
						ExpectedHead: r.head, Change: r.change})
					took := time.Since(began)
					b.StopTimer()
					if err != nil || res.Outcome != statelog.OutcomeApplied {
						b.Fatalf("the rebase did not land: %+v %v", res, err)
					}
					r.head = res.Commit
					writes = append(writes, took)
					builds = append(builds, r.fig.stage("build")...)
					b.StartTimer()
				}
				b.ReportMetric(medianOf(builds), "ms/build")
				b.ReportMetric(medianOf(writes), "ms/write")
			})
		}
	}
}

// The trees BenchmarkEngineRebase rebases over.
const (
	rebaseTreeFiles      = 1_000
	rebaseTreeFilesLarge = 100_000
)

// rebaseRig is one arm's fixture: one git node holding a repository whose main
// holds a tree of the arm's files, and a change from feature of the arm's
// commits on it, opened by pat.
type rebaseRig struct {
	f      *codetest.SoloFleet
	fig    *writeFigures
	p      *codetest.Person
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
	r.f.Declare(b, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	r.p = codetest.NewPerson(b, r.f, codetest.FixturePat)
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
	rows, err := r.f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
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

// medianOf is the median of samples in milliseconds — one sample at the
// bound this machine runs an arm at.
func medianOf(samples []time.Duration) float64 {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return quantile(sorted, 0.50)
}
