package query_test

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/query"
)

// BenchmarkRevisionFacts is a revision's facts, timed whole through a git
// node's query server — the check of its two commits, its unit of the share,
// the merge base, the walk of the paths it touches, its patch written into a
// spool and `git patch-id` reading it back — over a change whose cumulative
// patch is diff= bytes: text files of 256 KiB, every fourth line of which the
// change rewrites, so its hunks run into one per file and its patch is every
// line of each, a quarter of them twice — a formatter's run over a tree, the
// widest diff a change of text carries for its size. The arm at the bound,
// MaxPatchIDDiffBytes, runs only under CREWLET_BENCH_LARGE.
//
// Each arm reports the p50, p95 and p99 of one read, and the patch's MB/s at
// the p50; it fails a read whose id was omitted. The figures decide
// MaxPatchIDDiffBytes — the bound's p99 inside the one budget a read's own
// git has, gitproc.ReadDeadline, with room for a node at its share — and so
// that a revision's facts need no deadline of their own.
func BenchmarkRevisionFacts(b *testing.B) {
	codetest.Quiet(b)
	for _, arm := range factsArms {
		b.Run("diff="+arm.name, func(b *testing.B) {
			if arm.large {
				gittest.Large(b, fmt.Sprintf("a change whose patch is %s, over %d files of 256 KiB", arm.name,
					arm.files))
			}
			r := newFactsRig(b, arm.files)
			took := make([]time.Duration, 0, b.N)
			for b.Loop() {
				start := time.Now()
				a, err := r.s.Read(b.Context(), factsQ(r.repo, r.head, r.base))
				took = append(took, time.Since(start))
				switch {
				case err != nil:
					b.Fatal(err)
				case a.RevisionFacts.PatchIDOmitted || a.RevisionFacts.PatchID == "":
					b.Fatalf("the facts of a %s patch carry no id: %+v", arm.name, a.RevisionFacts)
				}
			}
			p50 := report(b, took)
			b.ReportMetric(float64(r.patch)/(1<<20), "patch-MiB")
			if p50 > 0 {
				b.ReportMetric(float64(r.patch)/1e6/p50.Seconds(), "patch-MB/s")
			}
		})
	}
}

// factsArm is one diff= arm: a change rewriting files files of factsFileBytes.
type factsArm struct {
	name  string
	files int
	large bool
}

// factsArms are the diff= arms, each a little under its name in patch: a
// file's patch is some 1.27 times the file, its lines whole and a quarter of
// them twice, each behind its one-byte mark.
var factsArms = []factsArm{
	{name: "1MiB", files: 3},
	{name: "16MiB", files: 49},
	{name: "64MiB", files: 196},
	{name: "256MiB", files: 781, large: true},
}

const (
	// factsFileBytes is about each file's size, factsLines lines of the
	// rig's line, which is 51 bytes edited or not.
	factsFileBytes = 256 << 10
	factsLines     = factsFileBytes / 51
)

// factsRig is BenchmarkRevisionFacts's fixture: a fleet of one git node
// holding a change's base and head, its query server, and the patch's bytes.
type factsRig struct {
	s                *query.Server
	repo, base, head string
	patch            int
}

// newFactsRig is a fleet holding a base of files text files and a head that
// rewrites every fourth line of each, fast-imported and pushed as a person
// pushes them.
func newFactsRig(b *testing.B, files int) *factsRig {
	b.Helper()
	c := gittest.NewClient(b)
	src := c.Init(b)
	line := func(f, i int, edited bool) string {
		if edited && i%4 == 0 {
			return fmt.Sprintf("%05d %05d the quick brown fox was rewritten here\n", f, i)
		}
		return fmt.Sprintf("%05d %05d the quick brown fox jumps over the dog\n", f, i)
	}
	// ONE SESSION, two commits on main: the second takes the first as its
	// parent, as a session's commits to one branch do.
	gittest.FastImport(b, c, src, func(w *bufio.Writer) error {
		for n, edited := range []bool{false, true} {
			msg := []string{"base\n", "the change\n"}[n]
			fmt.Fprintf(w, "commit refs/heads/main\ncommitter Crewlet Bench <bench@example.com> %d +0000\ndata %d\n%s",
				1_767_225_600+n, len(msg), msg)
			for f := range files {
				var body strings.Builder
				for i := range factsLines {
					body.WriteString(line(f, i, edited))
				}
				fmt.Fprintf(w, "M 100644 inline src/f%05d.txt\ndata %d\n%s\n", f, body.Len(), body.String())
			}
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		return nil
	})
	revs := strings.Fields(c.Run(b, src, "rev-list", "--reverse", "main"))
	if len(revs) != 2 {
		b.Fatalf("the rig holds %d commits, want its base and its change", len(revs))
	}
	patch := len(c.Run(b, src, "diff-tree", "-r", "-p", "--full-index", "--no-renames", "--no-color", "-U3",
		revs[0], revs[1]))

	f := codetest.SoloNode(b, codetest.SoloOptions{People: []string{"ceo"}, SyncAlways: true})
	f.Declare(b, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	p := codetest.NewPerson(b, f, "ceo")
	url := f.Nodes()[0].URL() + "/git/" + codetest.RepoName
	for _, rev := range revs {
		if res := p.Push(b.Context(), src, url, rev+":refs/heads/main"); res.Err != nil {
			b.Fatalf("git push: %s", res)
		}
	}
	rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	n := f.Nodes()[0]
	s, err := query.New(query.Options{NodeID: n.ID(), Cache: n.Cache(), Git: n.Pool(),
		Share: code.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil), Resyncs: code.NewResyncs(nil),
		Roster: func() code.Roster { return nil }, Fetched: noFetched})
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("the pools run git %s; a change to %d files, its patch %d bytes", n.Pool().Version(), files, patch)
	return &factsRig{s: s, repo: rows.Repo.ID, base: revs[0], head: revs[1], patch: patch}
}
