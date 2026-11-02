package compact_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
)

// AN EXPUNGE ON THE MAINTAINER'S OWN SCHEDULE: the `expunge_due` listing's
// weekly arm, the scan that judges a repository's unreachable share, and the
// cycle that keeps every pack younger than code.PruneGrace whole — driven on
// a solo fleet with a compactor whose clock stands past the grace, so what a
// case pushed reads old, and what a drop left is past its grace.

// past is a compactor of x whose clock stands a grace and an hour after the
// wall's — every pack pushed so far arrived before its grace, and every drop
// made so far is past it — expunging on its schedule from a single byte.
func (x *fixture) past() *compact.Compactor {
	x.t.Helper()
	c, _, _ := x.compactorOf(func(o *compact.Options) {
		o.Now = func() time.Time { return time.Now().Add(code.PruneGrace + time.Hour) }
	})
	compact.ExpungeFrom(c, 1)
	return c
}

// weekly is the repository named name as the expunge listing's weekly arm
// names it, asked from before, read linearizable, and whether it does.
func (x *fixture) weekly(name string, before time.Time) (code.ExpungeCandidate, bool) {
	x.t.Helper()
	q := code.ReadQuery{Kind: code.ReadExpungeDue, ExpungedBefore: before, Fresh: codetest.Linearizable}
	for {
		a, err := x.fleet.Data().Served().Read(x.c.Ctx, q)
		if err != nil {
			x.t.Fatalf("the expunge listing: %v", err)
		}
		for _, cand := range a.Expunge {
			if cand.Name == name && cand.Open == "" && cand.Request == "" {
				return cand, true
			}
		}
		if a.Next == "" {
			return code.ExpungeCandidate{}, false
		}
		q.Cursor = a.Next
	}
}

// leakBytes commits a file of n seeded random bytes — incompressible — on a
// branch named name, pushes it and deletes the branch, the work tree back
// where it was: a commit no ref reaches, of about n bytes. It answers the
// commit.
func (x *fixture) leakBytes(name string, n int) string {
	x.t.Helper()
	base := x.p.Head(x.work)
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(x.rng.Uint32())
	}
	leaked := x.p.Commit(x.work, map[string]string{fmt.Sprintf("leak-%s.bin", name): string(b)}, "leak "+name)
	x.c.MustPush(x.p, x.work, "HEAD:refs/heads/"+name)
	x.p.Git().Run(x.t, x.work, "reset", "--hard", base)
	x.c.MustPush(x.p, x.work, ":refs/heads/"+name)
	return leaked
}

// A WEEKLY EXPUNGE WAITS FOR A QUARTER: a repository whose unreachable bytes
// are under a quarter of its live bytes is scanned and left alone — not due,
// every pack where it was — and once they pass a quarter, the same scan finds
// it due and one cycle takes them out: no live pack holds what was leaked, a
// cache rebuilt from the rows passes git fsck, and the weekly arm no longer
// names the repository until a week after the commit.
//
// Mutation: judge due at any unreachable byte (drop the share), and the first
// scan expunges; judge the share against the unreachable bytes rather than
// the live ones, and the second never does.
func TestAWeeklyExpungeWaitsForAQuarter(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(96 << 10)
	small := x.leakBytes("small", 16<<10)
	c := x.past()
	before := time.Now().Add(code.PruneGrace + time.Hour - code.ExpungeInterval)
	cand, ok := x.weekly(codetest.RepoName, before)
	if !ok {
		t.Fatalf("the weekly arm names no %s, never expunged", codetest.RepoName)
	}
	if out, err := expungeWhileBusy(t, c, cand); out != compact.OutcomeNotDue {
		t.Fatalf("a scan of a repository a sixth of whose bytes are unreachable answered %s, %v; want not due",
			out, err)
	}
	if !x.liveHold(small) {
		t.Fatal("a scan that found the repository not due took its unreachable commit out")
	}

	big := x.leakBytes("big", 64<<10)
	cand, _ = x.weekly(codetest.RepoName, before)
	if out, err := expungeWhileBusy(t, c, cand); out != compact.OutcomeApplied {
		t.Fatalf("a scan of a repository past a quarter unreachable answered %s, %v; want an expunge applied",
			out, err)
	}
	for name, oid := range map[string]string{"small": small, "big": big} {
		if x.liveHold(oid) {
			t.Errorf("the %s leaked commit %s is still in a live pack after the weekly expunge", name, oid)
		}
	}
	if dir := x.fresh(); x.holds(dir, big) {
		t.Errorf("a cache rebuilt from the rows holds the leaked commit %s", big)
	}
	// The commit stamped the repository expunged at its record's broker
	// instant, the wall's now — not the compactor's clock, a grace ahead —
	// so a maintainer asking now names it no more, and one asking a week and
	// a minute on names it again.
	if _, ok := x.weekly(codetest.RepoName, time.Now().Add(-code.ExpungeInterval)); ok {
		t.Error("the weekly arm still names the repository its expunge has just committed in")
	}
	if _, ok := x.weekly(codetest.RepoName, time.Now().Add(time.Minute)); !ok {
		t.Error("the weekly arm names no repository a week and a minute after its expunge committed")
	}
}

// AN OBJECT YOUNGER THAN THE GRACE IS KEPT: a weekly expunge takes only the
// packs whose newest content arrived code.PruneGrace or longer before its
// clock, so a pack of objects no ref and no drop has ever reached — a push in
// flight's, whose ref has not landed yet — survives the expunge that takes
// out an older leak beside it, whole; and the next weekly expunge whose clock
// stands a grace past that pack's arrival takes it.
//
// Mutation: plan the schedule's inputs without their arrival (every live
// pack), and the young pack's objects go with the first expunge.
func TestAnObjectYoungerThanTheGraceIsKept(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(96 << 10)
	old := x.leakBytes("old", 64<<10)
	time.Sleep(100 * time.Millisecond)
	mid := time.Now()
	time.Sleep(100 * time.Millisecond)
	h := x.handmade()
	b := make([]byte, 64<<10)
	for i := range b {
		b[i] = byte(x.rng.Uint32())
	}
	young, objects := h.commit("refs/heads/young", map[string]string{"young.bin": string(b)})
	bare, cs := h.pack(objects)
	x.proposePack(bare, cs, "refs/heads/anchor", x.p.Head(x.work))

	first, _, _ := x.compactorOf(func(o *compact.Options) {
		o.Now = func() time.Time { return mid.Add(code.PruneGrace) }
	})
	compact.ExpungeFrom(first, 1)
	cand, ok := x.weekly(codetest.RepoName, mid.Add(code.PruneGrace-code.ExpungeInterval))
	if !ok {
		t.Fatalf("the weekly arm names no %s, never expunged", codetest.RepoName)
	}
	if out, err := expungeWhileBusy(t, first, cand); out != compact.OutcomeApplied {
		t.Fatalf("an expunge a grace past the old leak answered %s, %v; want applied", out, err)
	}
	if x.liveHold(old) {
		t.Errorf("the leak %s, arrived and dropped before the grace, is still in a live pack", old)
	}
	if !x.liveHold(young) {
		t.Fatalf("the commit %s, whose pack arrived within the grace, left the live packs", young)
	}

	second := x.past()
	cand, ok = x.weekly(codetest.RepoName, time.Now().Add(code.PruneGrace+time.Hour-code.ExpungeInterval))
	if !ok {
		t.Fatalf("a week past its expunge the weekly arm names no %s", codetest.RepoName)
	}
	if out, err := expungeWhileBusy(t, second, cand); out != compact.OutcomeApplied {
		t.Fatalf("an expunge a grace past the young pack answered %s, %v; want applied", out, err)
	}
	if x.liveHold(young) {
		t.Errorf("the commit %s, a grace past its arrival and reached by nothing, is still in a live pack", young)
	}
	x.fresh()
}

// A CLEAN REPOSITORY IS SCANNED WEEKLY: a repository whose scan on the
// maintainer's own schedule found it not due is left unscanned by every pass
// for code.ExpungeInterval after — however often the weekly arm lists it, and
// whatever is pushed meanwhile — and scanned again once the interval has
// passed: its whole history is walked at most once a week.
//
// Mutation: remember no clean scan (cleanAt false), or remember it by the
// row's version (a push moves it), and the pass a day later scans again.
func TestACleanRepositoryIsScannedWeekly(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(16 << 10)
	var mu sync.Mutex
	clock := time.Now()
	at := func(to time.Time) {
		mu.Lock()
		defer mu.Unlock()
		clock = to
	}
	c, _, l := x.compactorOf(func(o *compact.Options) {
		o.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return clock
		}
	})
	compact.ExpungeFrom(c, 1)
	scanned := func() int { return l.count("code_expunge_not_due", `"repo":"`+codetest.RepoName+`"`) }
	// pass is one expunge pass, made again while its one scan answered busy
	// (expungeWhileBusy's reason), for a minute at the most.
	pass := func() {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for {
			busy := l.count("code_expunge_busy")
			if _, err := c.ExpungePass(t.Context()); err != nil {
				t.Fatalf("an expunge pass: %v", err)
			}
			if l.count("code_expunge_busy") == busy || time.Now().After(deadline) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	began := clock
	pass()
	if got := scanned(); got != 1 {
		t.Fatalf("the first pass over a clean repository scanned it %d times, want once:\n%s", got, l)
	}
	x.push(16 << 10)
	at(began.Add(24 * time.Hour))
	pass()
	at(began.Add(code.ExpungeInterval - time.Minute))
	pass()
	if got := scanned(); got != 1 {
		t.Fatalf("passes within a week of a not-due scan scanned the repository %d times in all, want once:\n%s",
			got, l)
	}
	at(began.Add(code.ExpungeInterval + time.Minute))
	pass()
	if got := scanned(); got != 2 {
		t.Fatalf("a pass a week after the not-due scan scanned the repository %d times in all, want twice:\n%s",
			got, l)
	}
}
