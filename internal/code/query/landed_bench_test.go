package query_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkLandedIn is a landed-in read (landed.go) through a git node's query
// server — its resolution of the commit and the tip, the ancestry check
// against the tip, and the segmented search of the branch's moves in the
// node's index of pushes — over a branch of many moves:
//
//   - moves=2000 is gittest.History's 32 MB composition (about 14 000
//     commits) with a branch moved through it two thousand times;
//     moves=360000, the large model's busiest branch — about 2 000 pushes a
//     day for 181 days — through its gigabyte (455 680 commits), runs only
//     under gittest.LargeEnv. The commit asked about landed half the moves
//     ago, as the large model's commit ten thousand pushes old is a few
//     percent of its moves.
//   - forced=F is F forced moves of the branch since the commit landed — each
//     one a segment the search must look past, two checks a segment — 0, 3,
//     and MaxLandedProbes/2 + 2, which takes the search past its bound: that
//     read runs every one of MaxLandedProbes's checks, answers no push and
//     says so, and its ns/op is what the bound costs a read whole — which must
//     fit the read's budget of git (gitproc.ReadDeadline), or the budget, not
//     the bound, ends the search and the arm fails saying so.
//   - graph=with reads the copy of a git node that read the repository
//     whole, which writes its commit-graph at once (code-host.md, "The
//     commit-graph first"), and has read the repository's history back into
//     its index; graph=without is one of the search's ancestry checks — the
//     commit against the branch's tip, the longest walk — run by git over
//     the same history holding no commit-graph: what each check would cost
//     the search on a copy whose graph is not written yet.
//
// ns/op is the read whole; probes the checks its search ran beside the tip's
// own, probe-ns what each cost on average, and check-p99-ns and check-max-ns
// the 99th percentile and the slowest of one check's git — the tip's
// included, its run alone, which is what the read's budget is charged — the
// figure MaxLandedProbes is derived from (landed.go).
func BenchmarkLandedIn(b *testing.B) {
	codetest.Quiet(b)
	for _, shape := range []struct {
		name  string
		bytes int64
		moves int
	}{{"moves=2000", 32 << 20, 2_000}, {"moves=360000", 1 << 30, 360_000}} {
		b.Run(shape.name, func(b *testing.B) {
			if shape.moves > 2_000 {
				gittest.Large(b, "a 1 GB history fast-imported and pushed, and three branches of 360 000 moves each")
			}
			r := newLandedRig(b, shape.bytes, shape.moves)
			for _, forced := range landedForced {
				b.Run(fmt.Sprintf("forced=%d/graph=with", forced), func(b *testing.B) { r.read(b, forced) })
			}
			b.Run("forced=0/graph=without", r.withoutGraph)
		})
	}
}

// landedForced is BenchmarkLandedIn's forced arms: none, a few, and enough to
// take the search past MaxLandedProbes (landedPast).
var landedForced = []int{0, 3, query.MaxLandedProbes/2 + 2}

// landedPast reports whether a branch forced that many times since the
// commit landed takes the search past MaxLandedProbes: each forced move costs
// it two checks, one reaching the segment the move began and one asking the
// move's old value.
func landedPast(forced int) bool { return 2*forced >= query.MaxLandedProbes }

// landedRig is BenchmarkLandedIn's fixture: a fleet of two git nodes, a
// history pushed, one branch a forced arm moved through it, the commit asked
// about, and a query server over the second node, whose ancestry checks it
// times.
type landedRig struct {
	f      *codetest.SoloFleet
	src    string
	git    *gittest.Client
	repo   string
	commit string
	// landing is the push each branch landed the commit with.
	landing map[int]string
	at      statelog.Position
	tips    map[int]string
	s       *query.Server
	checks  *checkTimes
}

// landedBranch is the branch a forced arm's moves are made on.
func landedBranch(forced int) string { return fmt.Sprintf("refs/heads/landed-f%d", forced) }

func newLandedRig(b *testing.B, size int64, moves int) *landedRig {
	b.Helper()
	f := codetest.SoloNode(b, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: 2}, People: []string{"ceo"}})
	f.Declare(b, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	p := codetest.NewPerson(b, f, "ceo")
	src, commits := gittest.History(b, p.Git(), size)
	url := f.Nodes()[0].URL() + "/git/" + codetest.RepoName
	for s := 1; s <= gittest.HistorySessions; s++ {
		if res := p.Push(b.Context(), src, url, commits[s*len(commits)/gittest.HistorySessions-1]+
			":refs/heads/import"); res.Err != nil {
			b.Fatalf("git push: %s", res)
		}
	}
	rows, err := f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	r := &landedRig{f: f, src: src, git: p.Git(), repo: rows.Repo.ID, landing: map[int]string{},
		tips: map[int]string{}}
	// Each branch moves through the history a step of commits at a time —
	// move k to the k-th step's last commit — and then by force back a step
	// and forward again, forced times: every move a push of its own, no pack,
	// since every commit is the repository's already.
	step := len(commits) / moves
	at := func(k int) string { return commits[(k+1)*step-1] }
	r.commit = at(moves/2 - 1)
	pusher := code.Principal{Kind: code.PrincipalOperator, OperatorID: "bench", Seat: "ceo"}
	landings := make([]string, len(landedForced))
	eg, ctx := errgroup.WithContext(b.Context())
	for i, forced := range landedForced {
		eg.Go(func() error {
			ref, old := landedBranch(forced), code.ZeroOID
			// move is one push moving the branch to next, answering its id —
			// the proposal's, which the row it applies as keeps.
			move := func(next string, force bool) (string, error) {
				id := uuid.Must(uuid.NewV7()).String()
				res, err := f.Data().Served().Propose(ctx, code.Proposal{
					PushID: id, RepoID: r.repo, Principal: pusher, Via: code.ViaGit, Node: "bench",
					ReceivedAt: time.Now().UTC(), Commands: []code.Command{{Ref: ref, Old: old, New: next, Forced: force}},
				})
				switch {
				case err != nil:
					return "", fmt.Errorf("%s to %s: %w", ref, next, err)
				case res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
					res.Answer.Commands[0].Result != code.ResultOK:
					return "", fmt.Errorf("%s to %s was answered %s: %+v", ref, next, res.Outcome,
						res.Answer.Commands)
				}
				old = next
				return id, nil
			}
			for k := range moves {
				id, err := move(at(k), false)
				if err != nil {
					return err
				}
				if k == moves/2-1 {
					landings[i] = id
				}
			}
			for range forced {
				if _, err := move(at(moves-2), true); err != nil {
					return err
				}
				if _, err := move(at(moves-1), false); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		b.Fatalf("moving the branches: %v", err)
	}
	if rows, err = f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable); err != nil {
		b.Fatal(err)
	}
	r.at = rows.Position
	for i, forced := range landedForced {
		r.tips[forced], r.landing[forced] = rows.Refs[landedBranch(forced)], landings[i]
	}
	r.s = r.coldIndexed(b)
	b.Logf("the pools run git %s; %d commits, %d moves a branch", f.Nodes()[1].Pool().Version(), len(commits),
		moves)
	return r
}

// coldIndexed is a query server over the second git node once it has read the
// repository whole, written its commit-graph, and read the repository's
// history back into its index: a node a seat's walk asks first about it.
func (r *landedRig) coldIndexed(b *testing.B) *query.Server {
	b.Helper()
	n := r.f.Nodes()[1]
	c := n.Cache()
	deadline := time.Now().Add(10 * time.Minute)
	c.WantHistory(r.repo)
	for graph, complete := false, false; !graph || !complete; {
		if time.Now().After(deadline) {
			b.Fatalf("the second node held its commit-graph %v and its whole history %v ten minutes on", graph,
				complete)
		}
		s, err := c.Sync(b.Context(), r.repo, codetest.Linearizable)
		if err != nil {
			b.Fatal(err)
		}
		_, gerr := os.Stat(filepath.Join(s.Dir, "objects", "info", "commit-graph"))
		graph = gerr == nil
		p, perr := c.Provenance(s)
		complete = perr == nil && p.Indexed() && p.Coverage().Complete
		s.Release()
		c.WantHistory(r.repo)
		time.Sleep(50 * time.Millisecond)
	}
	r.checks = &checkTimes{Pool: n.Pool()}
	s, err := query.New(query.Options{NodeID: n.ID(), Cache: c, Git: r.checks,
		Share: code.NewGitShare(n.ID(), n.Pool().Slots(gitproc.ClassRead), nil), Resyncs: code.NewResyncs(nil),
		Roster: func() code.Roster { return nil }, Fetched: noFetched})
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// read times the landed-in read of the forced arm's branch.
func (r *landedRig) read(b *testing.B, forced int) {
	q := landedQ(r.repo, r.commit, landedBranch(forced), r.tips[forced], r.at, false)
	past := landedPast(forced)
	r.checks.reset()
	probes := 0
	for b.Loop() {
		a, err := r.s.Read(b.Context(), q)
		if err != nil {
			b.Fatal(err)
		}
		l := a.LandedIn
		probes = l.Probes
		switch {
		case !l.Landed:
			b.Fatalf("the commit is not landed in %s", landedBranch(forced))
		case !past && (l.LandedBy == nil || l.LandedBy.PushID != r.landing[forced]):
			b.Fatalf("landed by %+v (%s), want push %s", l.LandedBy, l.Detail, r.landing[forced])
		case past && l.LandedBy != nil:
			b.Fatalf("past its bound the search named push %s", l.LandedBy.PushID)
		case past && l.Probes != query.MaxLandedProbes:
			b.Fatalf("the read's %s of git ended the search %d checks in, before its bound of %d (%s): the "+
				"bound does not fit the budget here, one check's p99 being %v", gitproc.ReadDeadline, l.Probes,
				query.MaxLandedProbes, l.Detail, r.checks.at(0.99))
		}
	}
	b.ReportMetric(float64(probes), "probes")
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(probes+1), "probe-ns")
	b.ReportMetric(float64(r.checks.at(0.99).Nanoseconds()), "check-p99-ns")
	b.ReportMetric(float64(r.checks.at(1).Nanoseconds()), "check-max-ns")
}

// checkTimes is the rig's query.Pool: every process runs on the git node's
// own pool, and the run of each ancestry check is kept — what the read's
// budget of git is charged for it (gitproc.Result.Took), never its wait for a
// read-lane slot — for the percentile of one check MaxLandedProbes is derived
// from.
type checkTimes struct {
	query.Pool
	mu   sync.Mutex
	took []time.Duration
}

func (c *checkTimes) Run(ctx context.Context, cmd gitproc.Cmd) (gitproc.Result, error) {
	res, err := c.Pool.Run(ctx, cmd)
	if cmd.Args[0] == "merge-base" {
		c.mu.Lock()
		c.took = append(c.took, res.Took)
		c.mu.Unlock()
	}
	return res, err
}

// reset forgets every check kept so far.
func (c *checkTimes) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.took = c.took[:0]
}

// at is the q-th quantile of the checks kept since the last reset — the run q
// of the way up them in order, as report takes one, the slowest at 1 — and
// zero where none was kept.
func (c *checkTimes) at(q float64) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.took) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(c.took))
	return sorted[min(len(sorted)-1, int(q*float64(len(sorted))))]
}

// withoutGraph times the longest of the search's ancestry checks — the commit
// against the branch's tip — run by git over the history it was pushed from,
// which holds no commit-graph.
func (r *landedRig) withoutGraph(b *testing.B) {
	if _, err := os.Stat(filepath.Join(r.src, "objects", "info", "commit-graph")); err == nil {
		b.Fatal("the history's repository holds a commit-graph")
	}
	tip := r.tips[0]
	for b.Loop() {
		if _, stderr, err := r.git.TryContext(b.Context(), r.src, nil, "merge-base", "--is-ancestor", r.commit,
			tip); err != nil {
			b.Fatalf("merge-base --is-ancestor: %v\n%s", err, stderr)
		}
	}
}
