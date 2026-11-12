package compact_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// AN EXPUNGE ON THE WHOLE PATH: a solo fleet's committer deciding and applying
// a person's purge request, the maintainer's begin and its commit, the Gitlet
// node's cache sorting the repository's packs and serving the dropping ones
// between the two, and the store holding every pack — the compactor driven
// one cycle at a time, as a pass runs it.

// leak commits a file of a secret on the work tree's branch, pushes it to
// refs/heads/<name>, and deletes that branch — the work tree back where it
// was — so the commit is one no ref reaches, its objects in the push's pack:
// what a purge is asked to take out. It answers the commit.
func (x *fixture) leak(name string) string {
	x.t.Helper()
	base := x.p.Head(x.work)
	leaked := x.p.Commit(x.work, map[string]string{"secret-" + name + ".txt": "a secret of " + name + "\n"},
		"leak "+name)
	x.c.MustPush(x.p, x.work, "HEAD:refs/heads/"+name)
	x.p.Git().Run(x.t, x.work, "reset", "--hard", base)
	x.c.MustPush(x.p, x.work, ":refs/heads/"+name)
	return leaked
}

// purge is a person's purge of gitlettest.RepoName, asked now and applied: the
// request its maintainer serves.
func (x *fixture) purge() gitlet.ExpungeResult {
	x.t.Helper()
	req := gitlet.ExpungeRequest{RepoID: x.rows().Repo.ID,
		RequestID: statelog.NewOpID(time.Now(), "code_purge_unreachable"), By: pushRef}
	res, err := x.fleet.Data().Served().ProposeExpunge(x.c.Ctx, gitlet.ExpungeProposal{Request: &req})
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		x.t.Fatalf("the purge request: %+v, %v", res, err)
	}
	return res
}

// owed is gitlettest.RepoName as the expunge listing names it, read
// linearizable, and whether it names it at all.
func (x *fixture) owed() (gitlet.ExpungeCandidate, bool) {
	x.t.Helper()
	a, err := x.fleet.Data().Served().Read(x.c.Ctx, gitlet.ReadQuery{Kind: gitlet.ReadExpungeDue,
		Fresh: gitlettest.Linearizable})
	if err != nil {
		x.t.Fatalf("the expunge listing: %v", err)
	}
	for _, cand := range a.Expunge {
		if cand.Name == gitlettest.RepoName {
			return cand, true
		}
	}
	return gitlet.ExpungeCandidate{}, false
}

// mustOwe is [fixture.owed], failing the case where the listing names none.
func (x *fixture) mustOwe() gitlet.ExpungeCandidate {
	x.t.Helper()
	cand, ok := x.owed()
	if !ok {
		x.t.Fatalf("the expunge listing names no %s", gitlettest.RepoName)
	}
	return cand
}

// expungeWhileBusy is c's expunge of cand, asked again while it answers busy:
// the node's maintainer keeps the one slot of the maintenance lane an
// expunge's build may take for a moment at a time, as compactWhileBusy waits
// it out, for a minute at the most.
//
// ASKED AGAIN AS THE NEXT PASS ASKS IT, of what the listing names then
// (compact.Listed), never of cand again: a cycle is two records, and its busy
// answer may come after its BEGIN has applied — its rescue's admission
// refused while the maintainer holds the lane, its copy still holding the
// begin's inputs — so the listing names that expunge open from then on. A
// cycle asked of cand again, which names none, plans over a copy whose every
// live pack the begin dated at its scan: a purge's finds its request in hand
// and answers not_due (#840). A compaction's and a publication's busy answers
// come before their one record, which is why compactWhileBusy and
// publishWhileBusy ask the same candidate again. A listing that no longer
// names the repository after a busy answer is answered as the error, with
// that busy answer.
func expungeWhileBusy(t testing.TB, c *compact.Compactor, cand gitlet.ExpungeCandidate) (compact.Outcome, error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := c.Expunge(t.Context(), cand)
		if out != compact.OutcomeBusy || time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: its caller judges the outcome it answers
			return out, err
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
		listed, ok, lerr := compact.Listed(t.Context(), c, cand.ID)
		switch {
		case lerr != nil:
			return out, fmt.Errorf("the expunge listing, read again after %s answered busy (%w): %w", cand.Name, err,
				lerr)
		case !ok:
			return out, fmt.Errorf("the expunge listing names %s no more after it answered busy: %w", cand.Name, err)
		}
		cand = listed
	}
}

// served runs c's expunges of gitlettest.RepoName until the listing names it no
// more — its purge served — failing the case on an outcome a healthy fleet
// never gives, or where a dozen cycles do not serve it.
func (x *fixture) served(c *compact.Compactor) {
	x.t.Helper()
	for range 12 {
		cand, ok := x.owed()
		if !ok {
			return
		}
		switch out, err := expungeWhileBusy(x.t, c, cand); out {
		case compact.OutcomeApplied, compact.OutcomePending, compact.OutcomeUnknown:
		default:
			x.t.Fatalf("an expunge of %+v answered %s, %v", cand, out, err)
		}
	}
	x.t.Fatalf("a dozen expunges left %s's purge unserved", gitlettest.RepoName)
}

// fresh rebuilds the Gitlet node's cache from the rows alone — the node killed,
// its cache's directory removed, the node restarted over the rest — and
// answers the copy's directory, synced linearizable: what any Gitlet node the
// store serves would build of the repository. It runs `git fsck
// --connectivity-only --no-dangling` over it first, failing the case where a
// ref reaches an object the copy lacks.
func (x *fixture) fresh() string {
	x.t.Helper()
	node := x.node()
	dir := node.CacheDir()
	x.fleet.Kill(x.t, node.ID())
	if err := os.RemoveAll(dir); err != nil {
		x.t.Fatalf("remove %s: %v", dir, err)
	}
	x.fleet.Restart(x.t, node.ID())
	s, err := x.node().Cache().Sync(x.c.Ctx, x.rows().Repo.ID, gitlettest.Linearizable)
	if err != nil {
		x.t.Fatalf("sync the rebuilt cache: %v", err)
	}
	x.t.Cleanup(s.Release)
	if out, stderr, err := x.p.Git().Try(x.t, "", nil, "--git-dir="+s.Dir, "fsck", "--connectivity-only",
		"--no-dangling"); err != nil {
		x.t.Fatalf("git fsck of a cache rebuilt from the rows: %v\n%s%s", err, out, stderr)
	}
	return s.Dir
}

// holds reports whether the git repository at dir holds the object oid.
func (x *fixture) holds(dir, oid string) bool {
	x.t.Helper()
	_, _, err := x.p.Git().Try(x.t, "", nil, "--git-dir="+dir, "cat-file", "-e", oid)
	return err == nil
}

// liveHold reports whether any live pack of the repository's rows names oid —
// read back from the store, whatever any cache holds.
func (x *fixture) liveHold(oid string) bool {
	x.t.Helper()
	for _, row := range x.rows().Packs {
		if row.State == gitlet.PackLive && slices.Contains(x.objectIDs(row), oid) {
			return true
		}
	}
	return false
}

// revive is a push creating ref at oid — an object the repository holds and no
// ref reaches — checked at drop epoch epoch, carrying no pack: what an engine
// write that creates a branch at an old commit lands, its receiver relying on
// an object it found in the copy.
func (x *fixture) revive(ref, oid string, epoch int64) {
	x.t.Helper()
	x.command(gitlet.Command{Ref: ref, Old: gitlet.ZeroOID, New: oid}, epoch)
}

// command is one command proposed with no pack through the data node's
// committer, checked at drop epoch epoch, and applied.
func (x *fixture) command(cmd gitlet.Command, epoch int64) {
	x.t.Helper()
	res, err := x.fleet.Data().Served().Propose(x.c.Ctx, gitlet.Proposal{
		PushID: uuid.Must(uuid.NewV7()).String(), RepoID: x.rows().Repo.ID, Principal: pushRef, Via: gitlet.ViaGit,
		Node: x.node().ID(), ReceivedAt: time.Now().UTC(), DropEpoch: epoch, Commands: []gitlet.Command{cmd},
	})
	if err != nil || res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
		res.Answer.Commands[0].Result != gitlet.ResultOK {
		x.t.Fatalf("the command %+v: %+v, %v", cmd, res, err)
	}
}

// AN EXPUNGE KEEPS EVERY OBJECT A REFERENCE REACHES, and nothing else of what
// it rewrote: a person's purge of a repository holding a commit no ref reaches
// is served in one cycle — begun, rescued, committed — after which no live
// pack holds the commit, a cache rebuilt from the rows alone holds every
// object every ref reaches (git fsck) and not the commit, and every ref is
// where it was.
//
// Mutation: keep the dropping packs live at the commit (retire nothing) and
// the commit is still held; plan the kept build without `--local` against
// the rest and the build packs objects the rest holds, which the begin's
// arithmetic refuses.
func TestAnExpungeKeepsEveryObjectAReferenceReaches(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(4 << 10)
	leaked := x.leak("feature")
	x.push(4 << 10)
	refs := x.rows().Refs
	if !x.liveHold(leaked) {
		t.Fatalf("the premise: the leaked commit %s is in no live pack before the purge", leaked)
	}
	x.purge()
	c, rec, _ := x.compactorOf(nil)
	cand := x.mustOwe()
	if out, err := expungeWhileBusy(t, c, cand); out != compact.OutcomeApplied {
		t.Fatalf("the purge's expunge answered %s, %v; want applied", out, err)
	}
	if _, ok := x.owed(); ok {
		t.Fatal("the purge is still owed after its expunge committed")
	}
	if x.liveHold(leaked) {
		t.Errorf("a live pack still holds the leaked commit %s after the purge", leaked)
	}
	for _, row := range x.rows().Packs {
		if row.State != gitlet.PackLive {
			t.Errorf("pack %s is %s after the commit, which retires every dropping pack", row.PackID, row.State)
		}
	}
	if got := x.rows().Refs; !maps.Equal(got, refs) {
		t.Errorf("the refs moved under the expunge: %v, were %v", got, refs)
	}
	if n := counter(rec, metrics.CodeExpungeRuns, "outcome", string(compact.OutcomeApplied)); n != 1 {
		t.Errorf("%s{outcome=applied} = %d, want the one cycle", metrics.CodeExpungeRuns, n)
	}
	dir := x.fresh()
	if x.holds(dir, leaked) {
		t.Errorf("a cache rebuilt from the rows holds the leaked commit %s", leaked)
	}
}

// THE MAINTAINER DOWNLOADS NOTHING ITS EXPUNGE BUILT: once the begin has
// applied, the node that built it adopts every output from the build's
// scratch — the kept packs live, the dropping packs into its copy's dropping
// view — and the rescue too once the commit has: so across a whole cycle,
// the rescue's sync and every round to the commit among it, that node GETs
// none of the objects the cycle stored. The dropping packs are the cruft, up
// to compact.ExpungeInputByteCap a cycle, which the commit retires minutes
// later.
//
// Mutation: skip the dropping packs' adoption in the cycle, and the rescue's
// sync downloads every dropping pack.
func TestTheMaintainerDownloadsNothingItsExpungeBuilt(t *testing.T) {
	t.Parallel()
	gets := &gotten{}
	x := newFixtureWith(t, func(o *gitlettest.SoloOptions) {
		o.WrapObjects = func(_ string, b objstore.Backend) objstore.Backend {
			return gettingFrom{Backend: b, gets: gets}
		}
	})
	x.pushN(gitlet.CompactMinMerge, 4<<10)
	x.leak("feature")
	x.purge()
	rs := &recordingStore{Store: x.store}
	c, _, _ := x.compactorOf(func(o *compact.Options) { o.Objects = rs })
	if out, err := expungeWhileBusy(t, c, x.mustOwe()); out != compact.OutcomeApplied {
		t.Fatalf("the purge's expunge answered %s, %v; want applied", out, err)
	}
	stored, _ := rs.stored()
	if len(stored) < 2 {
		t.Fatalf("the premise: the cycle stored %d objects, want its kept and its dropping packs at least",
			len(stored))
	}
	got := gets.names()
	for _, obj := range stored {
		if n := got[obj.Key.Name()]; n > 0 {
			t.Errorf("the maintainer downloaded %s, which its own cycle built and stored, %d times", obj.Key, n)
		}
	}
}

// gotten is every object name a Gitlet node's backend was asked to GET, and how
// many times.
type gotten struct {
	mu  sync.Mutex
	got map[string]int
}

// names is a copy of what g counted.
func (g *gotten) names() map[string]int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return maps.Clone(g.got)
}

// gettingFrom is a Gitlet node's backend with every GET it makes counted, by
// name, in gets.
type gettingFrom struct {
	objstore.Backend
	gets *gotten
}

func (g gettingFrom) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	g.gets.mu.Lock()
	if g.gets.got == nil {
		g.gets.got = map[string]int{}
	}
	g.gets.got[name]++
	g.gets.mu.Unlock()
	return g.Backend.Get(ctx, name, off, n)
}

// A COMPACTION MERGES BESIDE AN EXPUNGE, NEVER ITS INPUTS: an expunge's walk,
// its build over up to compact.ExpungeInputByteCap of inputs, its upload and
// its rescue run for minutes on a large repository, and a busy repository
// gains packs at up to three hundred a minute, so a repository whose merges
// waited for its own expunge would pass gitlet.MaxPacksPerRepo and have its
// pushes refused compaction_behind while it is purged. A compaction asked
// while an expunge of its repository is in flight on the same node — held
// here once its build is done, nothing stored — merges the roll pushed since
// the expunge's plan, and never one of its inputs; and both records apply,
// the begin's inputs all still live. Once an expunge's rescue has synced past
// its begin — one resumed from the rows here, which has planned nothing — a
// compaction merges beside it as though none ran, and the commit applies.
// Only while an expunge syncs and plans does a compaction answer busy, having
// synced nothing, since it could merge packs the plan is about to choose.
//
// Mutations: refuse a compaction while an expunge of its repository runs at
// all, and the merge beside the build answers busy; plan it over the
// expunge's inputs as well, and it merges them, so the begin is refused
// stale; never mark the expunge begun, and the merge beside the resumed
// rescue answers busy; let a compaction claim the repository while the
// expunge has no reservation, and it syncs.
func TestACompactionMergesBesideAnExpungeNeverItsInputs(t *testing.T) {
	t.Parallel()
	t.Run("beside its build", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.pushN(gitlet.CompactMinMerge, 4<<10)
		x.leak("feature")
		x.purge()
		inputs := map[string]bool{}
		for _, p := range x.rows().Packs {
			inputs[p.PackID] = true
		}
		script := &gitletfault.Script{}
		held, reached := make(chan struct{}), make(chan struct{})
		script.Arm(gitletfault.ExpungeAfterBuild, gitletfault.Action{Once: true, Block: held, Reached: reached})
		c, _, _ := x.compactorOf(func(o *compact.Options) { o.Faults = script })
		cand := x.mustOwe()
		done := make(chan compact.Outcome, 1)
		go func() {
			out, _ := expungeWhileBusy(t, c, cand)
			done <- out
		}()
		select {
		case <-reached:
		case out := <-done:
			t.Fatalf("the expunge ended %s before its build was stored", out)
		case <-time.After(time.Minute): //fixedwait:liveness
			t.Fatal("the expunge built nothing in a minute")
		}

		// A ROLL PUSHED SINCE THE EXPUNGE'S PLAN, merged while it is held.
		x.pushN(gitlet.CompactMinMerge, 4<<10)
		before := x.rows()
		if out, err := compactBesideSlots(t, c, x.candidate()); out != compact.OutcomeApplied {
			close(held)
			<-done
			t.Fatalf("a compaction while its repository's expunge is in flight: %s, %v; want applied", out, err)
		}
		// WHAT IT RETIRED: every live row before it that the rows no longer hold.
		retired := outputs(x.rows(), before)
		for _, p := range retired {
			if inputs[p.PackID] {
				t.Errorf("the compaction merged pack %s, which the expunge in flight chose as an input", p.PackID)
			}
		}
		if len(retired) != gitlet.CompactMinMerge {
			t.Errorf("the compaction merged %d packs, want the %d pushed since the expunge's plan", len(retired),
				gitlet.CompactMinMerge)
		}
		close(held)
		if out := <-done; out != compact.OutcomeApplied {
			t.Fatalf("the expunge once let go, the compaction beside it applied: %s, want applied", out)
		}
	})
	t.Run("beside a resumed expunge's rescue", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.pushN(gitlet.CompactMinMerge, 4<<10)
		x.leak("feature")
		x.purge()
		stop := &gitletfault.Script{}
		stop.Arm(gitletfault.ExpungeAfterBegin, gitletfault.Action{Once: true, Err: errors.New("stop at the begin")})
		stopped, _, _ := x.compactorOf(func(o *compact.Options) { o.Faults = stop })
		if out, err := expungeWhileBusy(t, stopped, x.mustOwe()); out != compact.OutcomeFailed {
			t.Fatalf("the first cycle answered %s, %v; want failed at its begin", out, err)
		}
		stopped.Stop(context.Background())
		open := x.mustOwe()
		if open.Open == "" {
			t.Fatal("the premise: the stopped cycle left no expunge open")
		}
		script := &gitletfault.Script{}
		held, reached := make(chan struct{}), make(chan struct{})
		script.Arm(gitletfault.ExpungeAfterRescueUpload, gitletfault.Action{Once: true, Block: held, Reached: reached})
		c, _, _ := x.compactorOf(func(o *compact.Options) { o.Faults = script })
		done := make(chan compact.Outcome, 1)
		go func() {
			out, _ := expungeWhileBusy(t, c, open)
			done <- out
		}()
		select {
		case <-reached:
		case out := <-done:
			t.Fatalf("the resumed expunge ended %s before its rescue was stored", out)
		case <-time.After(time.Minute): //fixedwait:liveness
			t.Fatal("the resumed expunge stored no rescue in a minute")
		}
		x.pushN(gitlet.CompactMinMerge, 4<<10)
		if out, err := compactBesideSlots(t, c, x.candidate()); out != compact.OutcomeApplied {
			close(held)
			<-done
			t.Fatalf("a compaction beside a resumed expunge's rescue: %s, %v; want applied", out, err)
		}
		close(held)
		if out := <-done; out != compact.OutcomeApplied {
			t.Fatalf("the resumed expunge once let go, the compaction beside it applied: %s, want applied", out)
		}
	})
	t.Run("busy while the expunge plans", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.pushN(gitlet.CompactMinMerge, 4<<10)
		x.leak("feature")
		x.purge()
		entered, release := make(chan struct{}), make(chan struct{})
		h := &heldSync{counted: countedOver(x.node().Cache()), entered: entered, release: release}
		c, _, _ := x.compactorOf(func(o *compact.Options) { o.Cache = h })
		cand := x.mustOwe()
		done := make(chan compact.Outcome, 1)
		go func() {
			out, _ := expungeWhileBusy(t, c, cand)
			done <- out
		}()
		select {
		case <-entered:
		case <-time.After(time.Minute): //fixedwait:liveness
			t.Fatal("the expunge did not sync in a minute")
		}
		syncs, _ := h.counts()
		if out, err := c.Compact(t.Context(), x.candidate()); out != compact.OutcomeBusy {
			t.Errorf("a compaction while an expunge of its repository plans: %s, %v; want busy", out, err)
		}
		if again, _ := h.counts(); again != syncs {
			t.Errorf("the compaction refused busy synced the repository %d times, want none", again-syncs)
		}
		close(release)
		if out := <-done; out != compact.OutcomeApplied {
			t.Fatalf("the expunge once let go: %s, want applied", out)
		}
	})
}

// AN EXPUNGE IS NEVER PLANNED BESIDE ANOTHER BUILD OF ITS REPOSITORY, AND NO
// PUBLICATION BUILDS BESIDE ONE, on its node: a publication asked while an
// expunge of its repository runs answers busy having synced nothing — the
// begin drops every layer published over its inputs and the decide refuses a
// layer while an expunge is open, so either would be refused stale after its
// whole build — and an expunge asked while a compaction or a publication of
// its repository runs answers busy having synced nothing, since its plan
// would choose among packs that build's record retires or covers. Each lands
// once the other is done.
//
// Mutations: drop the expunge from claimPublication's refusal, and the
// publication asked while the expunge is held syncs the repository; drop the
// compaction or the publication from claimExpunge's, and the expunge asked
// while one is held does.
func TestAnExpungeIsNeverPlannedBesideAnotherBuild(t *testing.T) {
	t.Parallel()
	t.Run("a publication while the expunge runs", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.pushN(gitlet.CompactMinMerge, 4<<10)
		x.leak("feature")
		x.purge()
		cand := x.mustOwe()
		script := &gitletfault.Script{}
		held, reached := make(chan struct{}), make(chan struct{})
		script.Arm(gitletfault.ExpungeAfterBuild, gitletfault.Action{Once: true, Block: held, Reached: reached})
		cc := countedOver(x.node().Cache())
		c, _, _ := x.compactorOf(func(o *compact.Options) { o.Cache, o.Faults = cc, script })
		done := make(chan compact.Outcome, 1)
		go func() {
			out, _ := expungeWhileBusy(t, c, cand)
			done <- out
		}()
		select {
		case <-reached:
		case out := <-done:
			t.Fatalf("the expunge ended %s before its build was stored", out)
		case <-time.After(time.Minute): //fixedwait:liveness
			t.Fatal("the expunge built nothing in a minute")
		}
		syncs, _ := cc.counts()
		if out, err := c.Publish(t.Context(), x.publicationCandidate()); out != compact.OutcomeBusy {
			t.Errorf("a publication while an expunge of its repository runs: %s, %v; want busy", out, err)
		}
		if again, _ := cc.counts(); again != syncs {
			t.Errorf("the publication refused busy synced the repository %d times, want none", again-syncs)
		}
		close(held)
		if out := <-done; out != compact.OutcomeApplied {
			t.Fatalf("the expunge once let go: %s, want applied", out)
		}
	})
	// Each other build is asked of its listing on the test's goroutine, and
	// run on its own until its point holds it. Once it has landed, the
	// expunge that follows is made again while it is busy, with no wait for
	// what the node does in the background: a publication's layer, which the
	// installer places by swapping the copy, may swap the one the expunge's
	// sync pinned, and a plan from that copy is refused busy
	// (cache.ErrSuperseded), never read as a repository holding no pack. A
	// busy answer may come after the expunge's begin, too — its rescue's
	// admission refused while the node's maintainer holds the lane — which is
	// why it is asked again of what the listing names then (expungeWhileBusy).
	for _, other := range []struct {
		name  string
		point gitletfault.Point
		build func(x *fixture, c *compact.Compactor) func() compact.Outcome
	}{
		{"an expunge while a compaction runs", gitletfault.CompactAfterUpload,
			func(x *fixture, c *compact.Compactor) func() compact.Outcome {
				cand := x.candidate()
				return func() compact.Outcome {
					out, _ := compactWhileBusy(x.t, c, cand)
					return out
				}
			}},
		{"an expunge while a publication runs", gitletfault.PublishAfterUpload,
			func(x *fixture, c *compact.Compactor) func() compact.Outcome {
				cand := x.publicationCandidate()
				return func() compact.Outcome {
					out, _ := publishWhileBusy(x.t, c, cand)
					return out
				}
			}},
	} {
		t.Run(other.name, func(t *testing.T) {
			t.Parallel()
			x := newFixture(t)
			x.pushN(gitlet.CompactMinMerge, 4<<10)
			x.leak("feature")
			x.purge()
			script := &gitletfault.Script{}
			held, reached := make(chan struct{}), make(chan struct{})
			script.Arm(other.point, gitletfault.Action{Once: true, Block: held, Reached: reached})
			cc := countedOver(x.node().Cache())
			c, _, _ := x.compactorOf(func(o *compact.Options) { o.Cache, o.Faults = cc, script })
			run := other.build(x, c)
			done := make(chan compact.Outcome, 1)
			go func() { done <- run() }()
			select {
			case <-reached:
			case out := <-done:
				t.Fatalf("the build ended %s before its upload", out)
			case <-time.After(time.Minute): //fixedwait:liveness
				t.Fatal("the build stored nothing in a minute")
			}
			syncs, _ := cc.counts()
			if out, err := c.Expunge(t.Context(), x.mustOwe()); out != compact.OutcomeBusy {
				t.Errorf("%s: %s, %v; want busy", other.name, out, err)
			}
			if again, _ := cc.counts(); again != syncs {
				t.Errorf("the expunge refused busy synced the repository %d times, want none", again-syncs)
			}
			close(held)
			if out := <-done; out != compact.OutcomeApplied {
				t.Fatalf("the build once let go: %s, want applied", out)
			}
			if out, err := expungeWhileBusy(t, c, x.mustOwe()); out != compact.OutcomeApplied {
				t.Errorf("the expunge once the build landed: %s, %v; want applied", out, err)
			}
		})
	}
}

// A PASS OVER A COPY A SWAP REPLACED PLANS NOTHING: a swap between a pass's
// sync and its read of the live packs leaves the copy it pinned none, which
// the cache refuses (cache.ErrSuperseded) rather than answer as a repository
// holding no pack. A purge's expunge, a compaction and a publication are each
// busy for it — planned over nothing, the expunge would have found the purge
// served, and a weekly scan the repository clean for a week — and the next
// pass, syncing past the swap, plans over what the copy serving holds.
//
// Mutation: read the refusal as no pack (the expunge's Packs error dropped) →
// the purge's expunge answers not due, red.
func TestAPassOverACopyASwapReplacedPlansNothing(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(gitlet.CompactMinMerge, 4<<10)
	x.leak("feature")
	x.purge()
	cc := countedOver(x.node().Cache())
	cc.superseded = true
	c, _, l := x.compactorOf(func(o *compact.Options) { o.Cache = cc })
	if out, err := c.Expunge(t.Context(), x.mustOwe()); out != compact.OutcomeBusy ||
		!errors.Is(err, cache.ErrSuperseded) {
		t.Errorf("the purge's expunge over a replaced copy answered %s, %v; want busy", out, err)
	}
	if n := l.count("code_expunge_not_due"); n != 0 {
		t.Errorf("the expunge over a replaced copy judged the repository not due %d times, want none:\n%s", n, l)
	}
	if out, err := c.Compact(t.Context(), x.candidate()); out != compact.OutcomeBusy ||
		!errors.Is(err, cache.ErrSuperseded) {
		t.Errorf("a compaction over a replaced copy answered %s, %v; want busy", out, err)
	}
	if out, err := c.Publish(t.Context(), x.publicationCandidate()); out != compact.OutcomeBusy ||
		!errors.Is(err, cache.ErrSuperseded) {
		t.Errorf("a publication over a replaced copy answered %s, %v; want busy", out, err)
	}
	cc.mu.Lock()
	cc.superseded = false
	cc.mu.Unlock()
	if out, err := expungeWhileBusy(t, c, x.mustOwe()); out != compact.OutcomeApplied {
		t.Errorf("the next pass, past the swap, answered %s, %v; want the purge's expunge applied", out, err)
	}
}

// AN EXPUNGE BUSY PAST ITS BEGIN IS FINISHED BY THE NEXT ASK OF THE LISTING
// (#840): a cycle's busy answer may come after its begin has applied — its
// rescue's admission refused while the node's maintainer holds the one slot
// of the maintenance lane an expunge's build may take, as it may at any of its
// ticks — leaving the expunge open, which the listing names from then on. The
// next ask, of what the listing names, as a pass asks (expungeWhileBusy),
// finishes it from its rows: the one begin committed, the purge served, the
// leaked commit in no live pack.
//
// Mutation: ask expungeWhileBusy's next cycle of the candidate it was first
// given, which names no expunge open, and that cycle plans over a copy whose
// every live pack the begin dated at its scan: the purge's expunge answers
// not_due.
func TestAnExpungeBusyPastItsBeginIsFinishedByTheNextAsk(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(4 << 10)
	leaked := x.leak("feature")
	x.purge()
	// THE MAINTAINER'S RUN AT THE RESCUE: the first admission over a copy past
	// the begin — at the drop epoch the begin moved it to — is refused as the
	// cache refuses one while the maintainer's run holds the lane; every other
	// is the cache's own.
	var refused atomic.Bool
	cc := countedOver(x.node().Cache())
	cc.answerExpunges(func(s cache.Synced, objects int64, limits gitlet.Limits) (*cache.Admission, error) {
		if s.DropEpoch > 0 && refused.CompareAndSwap(false, true) {
			return nil, fmt.Errorf("cache: expunge %s: %w: a case's maintenance run holds the lane",
				gitlettest.RepoName, gitproc.ErrBusy)
		}
		return x.node().Cache().AdmitExpunge(s, objects, limits)
	})
	c, rec, l := x.compactorOf(func(o *compact.Options) { o.Cache = cc })
	if out, err := expungeWhileBusy(t, c, x.mustOwe()); out != compact.OutcomeApplied {
		t.Fatalf("the purge's expunge, busy past its begin: %s, %v; want applied", out, err)
	}
	if !refused.Load() {
		t.Fatal("the premise: no admission over a copy past the begin was refused")
	}
	if n := counter(rec, metrics.CodeExpungeRuns, "outcome", string(compact.OutcomeBusy)); n == 0 {
		t.Errorf("%s{outcome=busy} = 0, want the cycle the refused rescue left open counted busy",
			metrics.CodeExpungeRuns)
	}
	for _, msg := range []string{"code_expunge_begun", "code_expunge_committed"} {
		if n := l.count(msg); n != 1 {
			t.Errorf("%s logged %d times, want once: the one begin, finished:\n%s", msg, n, l)
		}
	}
	if _, ok := x.owed(); ok {
		t.Error("the purge is still owed after the expunge its busy answer left open was finished")
	}
	if x.liveHold(leaked) {
		t.Errorf("a live pack still holds the leaked commit %s after the purge", leaked)
	}
}

// A RESCUE HOLDS WHAT A PUSH REVIVED BEFORE THE BEGIN: a push that lands
// between the expunge's scan and its begin, checked at the epoch before it,
// creating a ref at the commit the scan found unreachable — an engine write
// creating a branch at an old commit — leaves that commit in a dropping pack
// once the begin applies; the rescue, reading the refs at its own sync, keeps
// it live, so after the commit the ref's whole closure is held (git fsck of a
// cache rebuilt from the rows) and the purge is served.
//
// Mutation: skip the rescue — commit retiring the dropping packs with no
// rescue — and the revived ref names a commit no pack holds: fsck fails.
func TestARescueHoldsWhatAPushRevivedBeforeTheBegin(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(4 << 10)
	leaked := x.leak("feature")
	x.purge()
	script := &gitletfault.Script{}
	held, reached := make(chan struct{}), make(chan struct{})
	script.Arm(gitletfault.ExpungeAfterBuild, gitletfault.Action{Once: true, Block: held, Reached: reached})
	c, _, _ := x.compactorOf(func(o *compact.Options) { o.Faults = script })
	cand := x.mustOwe()
	done := make(chan compact.Outcome, 1)
	go func() {
		out, _ := expungeWhileBusy(t, c, cand)
		done <- out
	}()
	select {
	case <-reached:
	case out := <-done:
		t.Fatalf("the expunge answered %s before its build was held", out)
	}
	x.revive("refs/heads/revived", leaked, 0)
	close(held)
	if out := <-done; out != compact.OutcomeApplied {
		t.Fatalf("the expunge answered %s; want applied", out)
	}
	if _, ok := x.owed(); ok {
		t.Fatal("the purge is still owed after its expunge committed")
	}
	if !x.liveHold(leaked) {
		t.Errorf("no live pack holds the revived commit %s after the commit", leaked)
	}
	if dir := x.fresh(); !x.holds(dir, leaked) {
		t.Errorf("a cache rebuilt from the rows lacks the revived commit %s", leaked)
	}
}

// A RESCUE KEEPS A DROP MADE BETWEEN SCAN AND BEGIN, for as long as its
// expunge's grace keeps a drop: a ref created at a commit the scan found
// unreachable and deleted again, both between the scan and the begin, leaves
// that commit dropping with no ref reaching it — and a rescue that keeps a
// ref drop's object for gitlet.PruneGrace, as an expunge on its maintainer's own
// schedule does, keeps it live. A purge's own rescue keeps no drop: the same
// commit leaves the repository at its commit.
//
// Mutation: leave the drops out of the rescue's tips, and the grace's
// expunge drops the commit.
func TestARescueKeepsADropMadeBetweenScanAndBegin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		grace time.Duration
		kept  bool
	}{
		{"a grace keeps it", gitlet.PruneGrace, true},
		{"a purge keeps none", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			x := newFixture(t)
			x.push(4 << 10)
			leaked := x.leak("feature")
			x.purge()
			script := &gitletfault.Script{}
			held, reached := make(chan struct{}), make(chan struct{})
			script.Arm(gitletfault.ExpungeAfterBuild, gitletfault.Action{Once: true, Block: held, Reached: reached})
			stop := errors.New("the case stops the cycle at its begin")
			script.Arm(gitletfault.ExpungeAfterBegin, gitletfault.Action{Once: true, Err: stop})
			c, _, _ := x.compactorOf(func(o *compact.Options) { o.Faults = script })
			cand := x.mustOwe()
			done := make(chan compact.Outcome, 1)
			go func() {
				out, _ := expungeWhileBusy(t, c, cand)
				done <- out
			}()
			<-reached
			x.revive("refs/heads/revived", leaked, 0)
			x.command(gitlet.Command{Ref: "refs/heads/revived", Old: leaked, New: gitlet.ZeroOID}, 0)
			close(held)
			if out := <-done; out != compact.OutcomeFailed {
				t.Fatalf("the cycle answered %s; want failed at the begin the case stopped it at", out)
			}
			open := x.mustOwe()
			if open.Open == "" {
				t.Fatalf("the listing names %+v, with no expunge open after the begin", open)
			}
			var out compact.Outcome
			var err error
			for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) { //fixedwait:liveness
				out, err = compact.FinishExpunge(t.Context(), c, open, tc.grace)
				if out != compact.OutcomeBusy || time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: the outcome is asserted after it
					break
				}
			}
			if out != compact.OutcomeApplied {
				t.Fatalf("the finish answered %s, %v; want applied", out, err)
			}
			if got := x.liveHold(leaked); got != tc.kept {
				t.Errorf("after the commit a live pack holds the dropped commit %s: %v, want %v", leaked, got,
					tc.kept)
			}
			x.fresh()
		})
	}
}

// AN EXPUNGE RESUMES FROM ITS ROWS AFTER A CRASH at each of its points: a
// cycle that stops at its build, at its begin, at its rescue's upload or at
// its commit leaves what the rows say — nothing, an expunge open, an expunge
// open with a rescue stored that no row names, an expunge committed — and a
// compactor started afresh over the same cache, knowing nothing of the cycle,
// serves the purge from the rows: the leaked commit gone from every live pack,
// a cache rebuilt from the rows whole.
//
// Mutation: let a pass begin a fresh cycle where the listing names an
// expunge open, and its begin is refused stale at every pass — the purge is
// never served.
func TestAnExpungeResumesFromItsRowsAfterACrash(t *testing.T) {
	t.Parallel()
	for _, p := range gitletfault.ExpungePath() {
		t.Run(string(p), func(t *testing.T) {
			t.Parallel()
			x := newFixture(t)
			x.push(4 << 10)
			leaked := x.leak("feature")
			x.push(4 << 10)
			x.purge()
			script := &gitletfault.Script{}
			crash := fmt.Errorf("the case crashed the maintainer at %s", p)
			script.Arm(p, gitletfault.Action{Once: true, Err: crash})
			first, _, _ := x.compactorOf(func(o *compact.Options) { o.Faults = script })
			if out, err := expungeWhileBusy(t, first, x.mustOwe()); out != compact.OutcomeFailed ||
				!errors.Is(err, crash) {
				t.Fatalf("the cycle answered %s, %v; want failed at %s", out, err, p)
			}
			first.Stop(context.Background())
			if script.Hits(p) != 1 {
				t.Fatalf("%s was hit %d times, want once", p, script.Hits(p))
			}
			afresh, _, _ := x.compactorOf(nil)
			x.served(afresh)
			if x.liveHold(leaked) {
				t.Errorf("a live pack still holds the leaked commit %s after the purge resumed", leaked)
			}
			if dir := x.fresh(); x.holds(dir, leaked) {
				t.Errorf("a cache rebuilt from the rows holds the leaked commit %s", leaked)
			}
		})
	}
}

// A PURGE REWRITES ONLY PACKS DATED BEFORE ITS REQUEST: a cycle serving a
// request takes the live packs whose content dates from before it
// (gitlet.PackRow.Content) and none pushed after it, whose row it leaves live
// under its own id — and the request is served by that one cycle.
//
// Mutation: take every live pack as an input, and the pack pushed after the
// request is rewritten.
func TestAPurgeRewritesOnlyPacksDatedBeforeItsRequest(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(2, 4<<10)
	x.leak("feature")
	var before []string
	for _, row := range x.rows().Packs {
		before = append(before, row.PackID)
	}
	x.purge()
	x.push(4 << 10)
	rows := x.rows()
	var after string
	for _, row := range rows.Packs {
		if !slices.Contains(before, row.PackID) {
			after = row.PackID
		}
	}
	if after == "" {
		t.Fatalf("the premise: no pack was pushed after the request (rows %v)", rows.Packs)
	}
	c, _, _ := x.compactorOf(nil)
	if out, err := expungeWhileBusy(t, c, x.mustOwe()); out != compact.OutcomeApplied {
		t.Fatalf("the purge's expunge answered %s, %v; want applied", out, err)
	}
	if _, ok := x.owed(); ok {
		t.Fatal("the purge is still owed after one cycle over every pack dated before it")
	}
	var live []string
	for _, row := range x.rows().Packs {
		live = append(live, row.PackID)
	}
	if !slices.Contains(live, after) {
		t.Errorf("the pack pushed after the request, %s, left the live packs: %v", after, live)
	}
	for _, id := range before {
		if slices.Contains(live, id) {
			t.Errorf("pack %s, dated before the request, is still live: %v", id, live)
		}
	}
}

// AN EXPUNGE'S TIPS LEAVE OUT WHAT A PURGE TOOK: a drop whose object an
// earlier purge took out of the repository is no tip of a later expunge that
// keeps a grace — its scan finds the object in no pack and walks from the
// rest — so the later expunge commits rather than refuse its build at every
// pass.
//
// Mutation: hand the build every drop of the grace unchecked, and the walk
// from the purged commit refuses the build.
func TestAnExpungesTipsLeaveOutWhatAPurgeTook(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(4 << 10)
	gone := x.leak("first")
	x.purge()
	c, _, _ := x.compactorOf(nil)
	x.served(c)
	if x.liveHold(gone) {
		t.Fatalf("the premise: the first purge left %s live", gone)
	}
	// A SECOND begin, open on a purge's request, finished as one keeping the
	// grace: the first leak's drop is within it, its object purged.
	x.leak("second")
	x.purge()
	script := &gitletfault.Script{}
	script.Arm(gitletfault.ExpungeAfterBegin, gitletfault.Action{Once: true, Err: errors.New("stop at the begin")})
	stopped, _, _ := x.compactorOf(func(o *compact.Options) { o.Faults = script })
	if out, err := expungeWhileBusy(t, stopped, x.mustOwe()); out != compact.OutcomeFailed {
		t.Fatalf("the second cycle answered %s, %v; want failed at its begin", out, err)
	}
	stopped.Stop(context.Background())
	open := x.mustOwe()
	if out, err := compact.FinishExpunge(t.Context(), c, open, gitlet.PruneGrace); out != compact.OutcomeApplied {
		t.Fatalf("the finish keeping the grace answered %s, %v; want applied", out, err)
	}
	x.fresh()
}

// AN OPEN CHANGE'S OLD REVISION SURVIVES A PURGE (M6-D2): a change opened at
// one commit and force-pushed to another holds two revisions, and its
// reviewers compare them — an interdiff of revision 1 and 2 reads both trees —
// though no ref reaches the first any more and a purge keeps no drop. The
// purge's scan walks from what rows hold in flight too, so its expunge keeps
// the first revision's commit while it drops a leaked commit nothing names: a
// cache rebuilt from the rows alone passes git fsck, holds both revisions, and
// diffs one against the other.
//
// Mutation: drop the revisions' phase from the `expunge_tips` read, and the
// purge drops the first revision's commit: the rebuilt cache lacks it and the
// interdiff fails.
func TestAnOpenChangesOldRevisionSurvivesAPurge(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	base := x.push(4 << 10)
	first := x.p.Commit(x.work, map[string]string{"retry.txt": "retry once\n"}, "retry the upload")
	if res := x.p.PushArgs(x.c.Ctx, x.work, []string{"-o", "change"}, x.c.URL(0),
		"HEAD:refs/heads/feature"); !res.OK("refs/heads/feature") {
		t.Fatalf("the push opening a change: %s", res)
	}
	x.p.Git().Run(t, x.work, "reset", "--hard", base)
	second := x.p.Commit(x.work, map[string]string{"retry.txt": "retry twice\n"}, "retry the upload twice")
	x.c.MustPush(x.p, x.work, "+HEAD:refs/heads/feature")
	x.p.Git().Run(t, x.work, "reset", "--hard", base)
	leaked := x.leak("secret")
	if !x.liveHold(first) || !x.liveHold(leaked) {
		t.Fatalf("the premise: the first revision %s and the leak %s are live before the purge", first, leaked)
	}

	x.purge()
	c, _, _ := x.compactorOf(nil)
	x.served(c)
	if x.liveHold(leaked) {
		t.Errorf("a live pack still holds the leaked commit %s after the purge", leaked)
	}
	if !x.liveHold(first) {
		t.Errorf("the purge dropped the first revision %s of an open change", first)
	}
	dir := x.fresh()
	for _, oid := range []string{first, second} {
		if !x.holds(dir, oid) {
			t.Errorf("a cache rebuilt from the rows lacks revision commit %s", oid)
		}
	}
	if out, stderr, err := x.p.Git().Try(t, "", nil, "--git-dir="+dir, "diff", first, second); err != nil ||
		!strings.Contains(out, "retry twice") {
		t.Fatalf("the interdiff of revision 1 and 2 after the purge: %v\n%s%s", err, out, stderr)
	}
}

// A MERGE BUILT AFTER AN EXPUNGE CARRIES ITS EPOCH AND LANDS (M6-D4): once a
// purge has run, its repository's drop epoch counts the expunge, and the
// committer refuses `expunging` a merge whose builder's copy held another
// epoch. The builder proposes the epoch of the copy its landing pinned, so a
// change merged after the expunge lands on its first build, with no rebuild —
// on the whole path: a person's merge through Gitlet on the Gitlet node, built
// in the cache the expunge left, its record decided by the data node's
// committer.
//
// Mutation: propose the merge at drop epoch 0, as a builder that dropped its
// copy's epoch would, and every build is refused expunging until the merge is
// answered expunging.
func TestAMergeBuiltAfterAnExpungeCarriesItsEpoch(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(4 << 10)
	x.p.Commit(x.work, map[string]string{"fix.txt": "fixed\n"}, "fix the upload")
	if res := x.p.PushArgs(x.c.Ctx, x.work, []string{"-o", "change"}, x.c.URL(0),
		"HEAD:refs/heads/feature"); !res.OK("refs/heads/feature") {
		t.Fatalf("the push opening a change: %s", res)
	}
	x.purge()
	c, _, _ := x.compactorOf(nil)
	x.served(c)
	if epoch := x.rows().Repo.DropEpoch; epoch < 1 {
		t.Fatalf("the premise: the repository is at drop epoch %d after a purge, want the expunge counted", epoch)
	}

	// THE FIXTURE CHART'S PERSON, whose seat signs the merge's commit.
	pat := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: iam.TokenLogin(gitlettest.FixturePat),
		Seat: gitlettest.FixturePat}
	ans, err := x.fleet.Code(t).Read(x.c.Ctx, gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: pat,
		Repo: gitlettest.RepoName, Fresh: gitlettest.Linearizable})
	if err != nil {
		t.Fatalf("read the changes: %v", err)
	}
	i := slices.IndexFunc(ans.Changes, func(ch gitlet.ChangeSummary) bool {
		return ch.SourceRef == "refs/heads/feature" && ch.State.Live()
	})
	if i < 0 {
		t.Fatalf("no live change heads refs/heads/feature: %+v", ans.Changes)
	}
	ch := ans.Changes[i]
	res, err := x.node().Host().Write(x.c.Ctx, gitlet.EngineWrite{Kind: gitlet.WriteMergeChange,
		RepoID: x.rows().Repo.ID, Principal: pat, OpID: statelog.NewOpID(time.Now(), ""),
		Fresh: gitlettest.Linearizable, Ref: "refs/heads/main", ExpectedHead: ch.Head, Change: ch.Number,
		Method: gitlet.MergeMethodMerge, Override: "merged past its gate to hold the expunge's epoch"})
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Merge == nil || res.Merge.Refusal != "" {
		t.Fatalf("the merge after the expunge answered %+v %v (%+v), want it applied", res, err, res.Merge)
	}
	if res.Merge.Recomputes != 0 {
		t.Errorf("the merge after the expunge was built %d times again, want none: its first build carried "+
			"the epoch its copy held", res.Merge.Recomputes)
	}
}
