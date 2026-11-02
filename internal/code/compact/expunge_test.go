package compact_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// AN EXPUNGE ON THE WHOLE PATH: a solo fleet's committer deciding and applying
// a person's purge request, the maintainer's begin and its commit, the git
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

// purge is a person's purge of codetest.RepoName, asked now and applied: the
// request its maintainer serves.
func (x *fixture) purge() code.ExpungeResult {
	x.t.Helper()
	req := code.ExpungeRequest{RepoID: x.rows().Repo.ID,
		RequestID: statelog.NewOpID(time.Now(), "code_purge_unreachable"), By: pushRef}
	res, err := x.fleet.Data().Served().ProposeExpunge(x.c.Ctx, code.ExpungeProposal{Request: &req})
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		x.t.Fatalf("the purge request: %+v, %v", res, err)
	}
	return res
}

// owed is codetest.RepoName as the expunge listing names it, read
// linearizable, and whether it names it at all.
func (x *fixture) owed() (code.ExpungeCandidate, bool) {
	x.t.Helper()
	a, err := x.fleet.Data().Served().Read(x.c.Ctx, code.ReadQuery{Kind: code.ReadExpungeDue,
		Fresh: codetest.Linearizable})
	if err != nil {
		x.t.Fatalf("the expunge listing: %v", err)
	}
	for _, cand := range a.Expunge {
		if cand.Name == codetest.RepoName {
			return cand, true
		}
	}
	return code.ExpungeCandidate{}, false
}

// mustOwe is [fixture.owed], failing the case where the listing names none.
func (x *fixture) mustOwe() code.ExpungeCandidate {
	x.t.Helper()
	cand, ok := x.owed()
	if !ok {
		x.t.Fatalf("the expunge listing names no %s", codetest.RepoName)
	}
	return cand
}

// expungeWhileBusy is c's expunge of cand, asked again while it answers busy:
// the node's maintainer keeps the one slot of the maintenance lane an
// expunge's build may take for a moment at a time, as compactWhileBusy waits
// it out, for a minute at the most.
func expungeWhileBusy(t testing.TB, c *compact.Compactor, cand code.ExpungeCandidate) (compact.Outcome, error) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := c.Expunge(t.Context(), cand)
		if out != compact.OutcomeBusy || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// served runs c's expunges of codetest.RepoName until the listing names it no
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
	x.t.Fatalf("a dozen expunges left %s's purge unserved", codetest.RepoName)
}

// fresh rebuilds the git node's cache from the rows alone — the node killed,
// its cache's directory removed, the node restarted over the rest — and
// answers the copy's directory, synced linearizable: what any git node the
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
	s, err := x.node().Cache().Sync(x.c.Ctx, x.rows().Repo.ID, codetest.Linearizable)
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
		if row.State == code.PackLive && slices.Contains(x.objectIDs(row), oid) {
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
	x.command(code.Command{Ref: ref, Old: code.ZeroOID, New: oid}, epoch)
}

// command is one command proposed with no pack through the data node's
// committer, checked at drop epoch epoch, and applied.
func (x *fixture) command(cmd code.Command, epoch int64) {
	x.t.Helper()
	res, err := x.fleet.Data().Served().Propose(x.c.Ctx, code.Proposal{
		PushID: uuid.Must(uuid.NewV7()).String(), RepoID: x.rows().Repo.ID, Principal: pushRef, Via: code.ViaGit,
		Node: x.node().ID(), ReceivedAt: time.Now().UTC(), DropEpoch: epoch, Commands: []code.Command{cmd},
	})
	if err != nil || res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
		res.Answer.Commands[0].Result != code.ResultOK {
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
		if row.State != code.PackLive {
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

// AN EXPUNGE HOLDS ITS REPOSITORY FROM EVERY OTHER BUILD ON ITS NODE, from its
// scan to its commit, and waits for one running: its begin retires inputs
// chosen among every live pack and drops the published layers over them, so
// a merge or a layer planned beside it would have one of the two refused
// stale — the begin after its whole build. Unlike a compaction beside a
// publication, which merges around the publication's reservation, nothing
// builds beside an expunge, and each answers busy having synced nothing.
//
// Mutations: drop the expunge from claimCompaction's refusal, and the merge
// asked while the expunge is held syncs the repository; drop the publication
// from claimExpunge's, and the expunge asked while a publication is held does.
func TestAnExpungeHoldsItsRepositoryFromEveryOtherBuild(t *testing.T) {
	t.Parallel()
	t.Run("a compaction and a publication while the expunge runs", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.pushN(code.CompactMinMerge, 4<<10)
		x.leak("feature")
		x.purge()
		cand := x.mustOwe()
		script := &codefault.Script{}
		held, reached := make(chan struct{}), make(chan struct{})
		script.Arm(codefault.ExpungeAfterBuild, codefault.Action{Once: true, Block: held, Reached: reached})
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
		case <-time.After(time.Minute):
			t.Fatal("the expunge built nothing in a minute")
		}
		syncs, _ := cc.counts()
		if out, err := c.Compact(t.Context(), x.candidate()); out != compact.OutcomeBusy {
			t.Errorf("a compaction while an expunge of its repository runs: %s, %v; want busy", out, err)
		}
		if out, err := c.Publish(t.Context(), x.publicationCandidate()); out != compact.OutcomeBusy {
			t.Errorf("a publication while an expunge of its repository runs: %s, %v; want busy", out, err)
		}
		if again, _ := cc.counts(); again != syncs {
			t.Errorf("the builds refused busy synced the repository %d times, want none", again-syncs)
		}
		close(held)
		if out := <-done; out != compact.OutcomeApplied {
			t.Fatalf("the expunge once let go: %s, want applied", out)
		}
		if out, err := compactWhileBusy(t, c, x.candidate()); out == compact.OutcomeBusy {
			t.Errorf("a compaction once the expunge committed: %s, %v; want it no longer busy", out, err)
		}
	})
	// Each other build is asked of its listing on the test's goroutine, and
	// run on its own until its point holds it; once it has landed, the copy
	// is let settle where its node does more in the background — a
	// publication's layer, which the installer places by swapping the copy
	// — since an expunge planned over an incarnation a swap then replaced
	// fails its build that cycle (its scratch reads the live set the swap
	// carried on), and is asked again at the next pass.
	for _, other := range []struct {
		name   string
		point  codefault.Point
		build  func(x *fixture, c *compact.Compactor) func() compact.Outcome
		settle func(x *fixture)
	}{
		{"an expunge while a compaction runs", codefault.CompactAfterUpload,
			func(x *fixture, c *compact.Compactor) func() compact.Outcome {
				cand := x.candidate()
				return func() compact.Outcome {
					out, _ := compactWhileBusy(x.t, c, cand)
					return out
				}
			}, func(*fixture) {}},
		{"an expunge while a publication runs", codefault.PublishAfterUpload,
			func(x *fixture, c *compact.Compactor) func() compact.Outcome {
				cand := x.publicationCandidate()
				return func() compact.Outcome {
					out, _ := publishWhileBusy(x.t, c, cand)
					return out
				}
			}, func(x *fixture) { x.awaitInstalled(1) }},
	} {
		t.Run(other.name, func(t *testing.T) {
			t.Parallel()
			x := newFixture(t)
			x.pushN(code.CompactMinMerge, 4<<10)
			x.leak("feature")
			x.purge()
			script := &codefault.Script{}
			held, reached := make(chan struct{}), make(chan struct{})
			script.Arm(other.point, codefault.Action{Once: true, Block: held, Reached: reached})
			cc := countedOver(x.node().Cache())
			c, _, _ := x.compactorOf(func(o *compact.Options) { o.Cache, o.Faults = cc, script })
			run := other.build(x, c)
			done := make(chan compact.Outcome, 1)
			go func() { done <- run() }()
			select {
			case <-reached:
			case out := <-done:
				t.Fatalf("the build ended %s before its upload", out)
			case <-time.After(time.Minute):
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
			other.settle(x)
			if out, err := expungeWhileBusy(t, c, x.mustOwe()); out != compact.OutcomeApplied {
				t.Errorf("the expunge once the build landed: %s, %v; want applied", out, err)
			}
		})
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
	script := &codefault.Script{}
	held, reached := make(chan struct{}), make(chan struct{})
	script.Arm(codefault.ExpungeAfterBuild, codefault.Action{Once: true, Block: held, Reached: reached})
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
// ref drop's object for code.PruneGrace, as an expunge on its maintainer's own
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
		{"a grace keeps it", code.PruneGrace, true},
		{"a purge keeps none", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			x := newFixture(t)
			x.push(4 << 10)
			leaked := x.leak("feature")
			x.purge()
			script := &codefault.Script{}
			held, reached := make(chan struct{}), make(chan struct{})
			script.Arm(codefault.ExpungeAfterBuild, codefault.Action{Once: true, Block: held, Reached: reached})
			stop := errors.New("the case stops the cycle at its begin")
			script.Arm(codefault.ExpungeAfterBegin, codefault.Action{Once: true, Err: stop})
			c, _, _ := x.compactorOf(func(o *compact.Options) { o.Faults = script })
			cand := x.mustOwe()
			done := make(chan compact.Outcome, 1)
			go func() {
				out, _ := expungeWhileBusy(t, c, cand)
				done <- out
			}()
			<-reached
			x.revive("refs/heads/revived", leaked, 0)
			x.command(code.Command{Ref: "refs/heads/revived", Old: leaked, New: code.ZeroOID}, 0)
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
			for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
				out, err = compact.FinishExpunge(t.Context(), c, open, tc.grace)
				if out != compact.OutcomeBusy || time.Now().After(deadline) {
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
	for _, p := range codefault.ExpungePath() {
		t.Run(string(p), func(t *testing.T) {
			t.Parallel()
			x := newFixture(t)
			x.push(4 << 10)
			leaked := x.leak("feature")
			x.push(4 << 10)
			x.purge()
			script := &codefault.Script{}
			crash := fmt.Errorf("the case crashed the maintainer at %s", p)
			script.Arm(p, codefault.Action{Once: true, Err: crash})
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
// (code.PackRow.Content) and none pushed after it, whose row it leaves live
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
	script := &codefault.Script{}
	script.Arm(codefault.ExpungeAfterBegin, codefault.Action{Once: true, Err: errors.New("stop at the begin")})
	stopped, _, _ := x.compactorOf(func(o *compact.Options) { o.Faults = script })
	if out, err := expungeWhileBusy(t, stopped, x.mustOwe()); out != compact.OutcomeFailed {
		t.Fatalf("the second cycle answered %s, %v; want failed at its begin", out, err)
	}
	stopped.Stop(context.Background())
	open := x.mustOwe()
	if out, err := compact.FinishExpunge(t.Context(), c, open, code.PruneGrace); out != compact.OutcomeApplied {
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
// on the whole path: a person's merge through the git node's code host, built
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
	pat := code.Principal{Kind: code.PrincipalOperator, OperatorID: iam.TokenLogin(codetest.FixturePat),
		Seat: codetest.FixturePat}
	ans, err := x.fleet.Code(t).Read(x.c.Ctx, code.ReadQuery{Kind: code.ReadChanges, Asker: pat,
		Repo: codetest.RepoName, Fresh: codetest.Linearizable})
	if err != nil {
		t.Fatalf("read the changes: %v", err)
	}
	i := slices.IndexFunc(ans.Changes, func(ch code.ChangeSummary) bool {
		return ch.SourceRef == "refs/heads/feature" && ch.State.Live()
	})
	if i < 0 {
		t.Fatalf("no live change heads refs/heads/feature: %+v", ans.Changes)
	}
	ch := ans.Changes[i]
	res, err := x.node().Host().Write(x.c.Ctx, code.EngineWrite{Kind: code.WriteMergeChange,
		RepoID: x.rows().Repo.ID, Principal: pat, OpID: statelog.NewOpID(time.Now(), ""),
		Fresh: codetest.Linearizable, Ref: "refs/heads/main", ExpectedHead: ch.Head, Change: ch.Number,
		Method: code.MergeMethodMerge, Override: "merged past its gate to hold the expunge's epoch"})
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Merge == nil || res.Merge.Refusal != "" {
		t.Fatalf("the merge after the expunge answered %+v %v (%+v), want it applied", res, err, res.Merge)
	}
	if res.Merge.Recomputes != 0 {
		t.Errorf("the merge after the expunge was built %d times again, want none: its first build carried "+
			"the epoch its copy held", res.Merge.Recomputes)
	}
}
