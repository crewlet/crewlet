package query_test

import (
	"bufio"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/query"
)

// BenchmarkReanchor is a re-anchoring timed whole through a Gitlet node's query
// server — the check of what it names, its unit of the share and its one
// diff-pairs — of one anchor on the LAST line of a file whose next version
// changes every other line above it: the PATHOLOGICAL pair, whose diff is a
// hunk every two lines and every one of them above the anchor, so none is
// passed over unread. Each arm reports the engine's own B/op and allocs/op
// (git's are another process's), which must not grow with the hunks — the
// placings are fed as the hunks stream and none is kept — and the hunks/op
// the diff held.
func BenchmarkReanchor(b *testing.B) {
	gitlettest.Quiet(b)
	for _, hunks := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("hunks=%d", hunks), func(b *testing.B) {
			r := newReanchorRig(b, hunks)
			b.ReportAllocs()
			for b.Loop() {
				a, err := r.s.Read(b.Context(), reanchorQ(r.repo, r.q))
				switch {
				case err != nil:
					b.Fatal(err)
				case a.Reanchor.Anchors[0] != r.want:
					b.Fatalf("the anchor was placed at %+v, want %+v", a.Reanchor.Anchors[0], r.want)
				}
			}
			b.ReportMetric(float64(hunks), "hunks/op")
		})
	}
}

// reanchorRig is BenchmarkReanchor's fixture: a fleet of one Gitlet node holding
// the two versions, its query server, the re-anchoring asked and its answer.
type reanchorRig struct {
	s    *query.Server
	repo string
	q    gitlet.ReanchorQuery
	want gitlet.Reanchored
}

// newReanchorRig is a fleet holding a file of 2×hunks+1 lines and a next
// version rewriting every other line but the last, fast-imported and pushed
// as a person pushes them.
func newReanchorRig(b *testing.B, hunks int) *reanchorRig {
	b.Helper()
	c := gittest.NewClient(b)
	src := c.Init(b)
	lines := 2*hunks + 1
	gittest.FastImport(b, c, src, func(w *bufio.Writer) error {
		for n, edited := range []bool{false, true} {
			msg := []string{"base\n", "every other line\n"}[n]
			fmt.Fprintf(w, "commit refs/heads/main\ncommitter Crewlet Bench <bench@example.com> %d +0000\ndata %d\n%s",
				1_767_225_600+n, len(msg), msg)
			var body strings.Builder
			for i := range lines {
				if edited && i%2 == 0 && i < lines-1 {
					body.WriteString("y\n")
					continue
				}
				body.WriteString("x\n")
			}
			fmt.Fprintf(w, "M 100644 inline f.txt\ndata %d\n%s\n", body.Len(), body.String())
			if err := w.WriteByte('\n'); err != nil {
				return err
			}
		}
		return nil
	})
	revs := strings.Fields(c.Run(b, src, "rev-list", "--reverse", "main"))
	if len(revs) != 2 {
		b.Fatalf("the rig holds %d commits, want its two versions", len(revs))
	}
	blob := strings.TrimSpace(c.Run(b, src, "rev-parse", revs[0]+":f.txt"))

	f := gitlettest.SoloNode(b, gitlettest.SoloOptions{People: []string{"ceo"}, SyncAlways: true})
	f.Declare(b, gitlet.Declared{Name: gitlettest.RepoName, Settings: gitlettest.Settings()})
	p := gitlettest.NewPerson(b, f, "ceo")
	url := f.Nodes()[0].URL() + "/git/" + gitlettest.RepoName
	for _, rev := range revs {
		if res := p.Push(b.Context(), src, url, rev+":refs/heads/main"); res.Err != nil {
			b.Fatalf("git push: %s", res)
		}
	}
	rows, err := f.Rows(b.Context(), gitlettest.RepoName, gitlettest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	n := f.Nodes()[0]
	s, err := query.New(query.Options{NodeID: n.ID(), Cache: n.Cache(), Git: n.Pool(),
		Share: gitlet.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil), Resyncs: gitlet.NewResyncs(nil, nil),
		Roster: func() gitlet.Roster { return nil }, Fetched: noFetched})
	if err != nil {
		b.Fatal(err)
	}
	return &reanchorRig{s: s, repo: rows.Repo.ID,
		q: gitlet.ReanchorQuery{Head: revs[1], Anchors: []gitlet.LineAnchor{{ID: "last", Path: "f.txt",
			Side: gitlet.SideNew, BlobOID: blob, Start: lines, End: lines}}},
		want: gitlet.Reanchored{ID: "last", Start: lines, End: lines}}
}
