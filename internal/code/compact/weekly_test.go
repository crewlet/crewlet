package compact_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/statelog"
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
	q := code.ReadQuery{Kind: code.ReadExpungeDue, ScannedBefore: before, Fresh: codetest.Linearizable}
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

// steppedClock is a clock a case moves on by hand from the wall's instant
// where it began. Its fleet's applier reads every record's broker instant off
// it (stampedBy), so the instant a pack ARRIVED at is one the case stated, and
// a compactor's clock set from it judges the grace against such instants
// alone.
type steppedClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *steppedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *steppedClock) step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// steppedFixture is newFixture over a fleet whose data node stamps every
// record it applies with clock's instant (stampedBy).
func steppedFixture(t *testing.T, clock *steppedClock) *fixture {
	t.Helper()
	return newFixtureWith(t, func(o *codetest.SoloOptions) { o.WrapApplier = stampedBy(clock.now) })
}

// between is the instant between the packs x has pushed and the ones it pushes
// next, on x's stepped clock: the fleet levelled, so every record appended so
// far is applied and stamped at the clock's instant, then the clock stepped an
// hour on to it and an hour past it. A pack pushed so far arrived an hour
// before it, and one pushed from here on arrives an hour after. Never two
// sleeps around a reading of the wall clock, which hold the instants apart
// only once the clock has moved between them, and not at all on a coarse
// one.
func (x *fixture) between(clock *steppedClock) time.Time {
	x.t.Helper()
	if err := x.fleet.Level(x.c.Ctx, ""); err != nil {
		x.t.Fatalf("level the fleet: %v", err)
	}
	clock.step(time.Hour)
	mid := clock.now()
	clock.step(time.Hour)
	return mid
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
// EVERY INSTANT IS THE CASE'S (steppedClock): the old leak arrives and is
// dropped an hour before mid, the young pack arrives an hour after it, and
// each expunge's clock is a grace past an instant on the same clock, so the
// grace is judged against what the case stated and no wall clock has to move.
//
// Mutations: plan the schedule's inputs without their arrival (every live
// pack), and the young pack's objects go with the first expunge; date a pack's
// arrival by the applying node's own clock rather than its record's broker
// instant, and the young pack reads as arrived before mid, and goes with it.
func TestAnObjectYoungerThanTheGraceIsKept(t *testing.T) {
	t.Parallel()
	clock := &steppedClock{at: time.Now()}
	x := steppedFixture(t, clock)
	x.push(96 << 10)
	old := x.leakBytes("old", 96<<10)
	mid := x.between(clock)
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

	// THE NEXT WEEKLY EXPUNGE, its clock a grace and an hour past the young
	// pack's arrival, on the case's clock.
	clock.step(time.Hour)
	at := clock.now().Add(code.PruneGrace)
	second, _, _ := x.compactorOf(func(o *compact.Options) { o.Now = func() time.Time { return at } })
	compact.ExpungeFrom(second, 1)
	cand, ok = x.weekly(codetest.RepoName, at.Add(-code.ExpungeInterval))
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

// A CLEAN REPOSITORY IS SCANNED WEEKLY, WHICHEVER NODE MAINTAINS IT: a scan
// on the maintainer's own schedule that finds a repository not due records so
// on the code log (code.ExpungeScan), so the weekly arm leaves it out for
// code.ExpungeInterval on every node — a second compactor, as a restarted
// node or a new maintainer runs, scans it no sooner, whatever is pushed
// meanwhile — and lists it again, to be scanned again, once the interval has
// passed: its whole history is walked at most once a week, and a deploy of
// the whole fleet rescans nothing.
//
// Mutation: record no scan (judgedClean proposes nothing), and the second
// compactor's pass a day later scans again; have the scan's apply leave
// expunge_scanned_at where it was, likewise.
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
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	first, _, l1 := x.compactorOf(func(o *compact.Options) { o.Now = now })
	compact.ExpungeFrom(first, 1)
	scanned := func() int {
		return l1.count("code_expunge_not_due", `"repo":"`+codetest.RepoName+`"`)
	}
	// pass is one expunge pass of c, made again while its one scan answered
	// busy (expungeWhileBusy's reason), for a minute at the most.
	pass := func(c *compact.Compactor, l *logs) {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for {
			busy := l.count("code_expunge_busy")
			if _, err := c.ExpungePass(t.Context()); err != nil {
				t.Fatalf("an expunge pass: %v", err)
			}
			if l.count("code_expunge_busy") == busy || time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: the scans are counted after it
				return
			}
			time.Sleep(20 * time.Millisecond) //fixedwait:liveness
		}
	}

	began := clock
	pass(first, l1)
	if got := scanned(); got != 1 {
		t.Fatalf("the first pass over a clean repository scanned it %d times, want once:\n%s", got, l1)
	}
	if n := l1.count("code_expunge_scan_unrecorded"); n != 0 {
		t.Fatalf("the clean scan's record was not appended:\n%s", l1)
	}

	// A RESTARTED NODE: a compactor of its own, with nothing in its memory.
	second, _, l2 := x.compactorOf(func(o *compact.Options) { o.Now = now })
	compact.ExpungeFrom(second, 1)
	rescanned := func() int {
		return l2.count("code_expunge_not_due", `"repo":"`+codetest.RepoName+`"`)
	}
	x.push(16 << 10)
	at(began.Add(24 * time.Hour))
	pass(second, l2)
	at(began.Add(code.ExpungeInterval - time.Minute))
	pass(second, l2)
	if got := rescanned(); got != 0 {
		t.Fatalf("a second compactor's passes within a week of the first's not-due scan scanned the repository "+
			"%d times, want none:\n%s", got, l2)
	}
	at(began.Add(code.ExpungeInterval + time.Minute))
	pass(second, l2)
	if got := rescanned(); got != 1 {
		t.Fatalf("a pass a week after the not-due scan scanned the repository %d times, want once:\n%s",
			got, l2)
	}
}

// A WEEKLY EXPUNGE THAT WOULD DROP NOTHING BEGINS NOTHING: a repository whose
// unreachable bytes are past a quarter of its live bytes, every one of them in
// a pack younger than code.PruneGrace — a push in flight's, or a compaction's
// output that merged an old leak with a later push and arrived when the push
// did — while every pack old enough to take holds only what its refs reach,
// is NOT DUE: the scan weighs what the packs a cycle may take would drop, and
// they would drop nothing — so the scan itself finds it not due, never a plan
// that comes up empty after it. No begin is proposed, every live pack stays
// where it was, and the young pack's objects are still live.
//
// EVERY INSTANT IS THE CASE'S (steppedClock): the reachable pack arrives an
// hour before mid and the young one an hour after it, and the scan's clock is
// a grace past mid on the same clock.
//
// Mutations: weigh every live pack rather than the eligible ones, and the scan
// finds the repository due (code_expunge_due); take into the plan a pack that
// frees nothing as well, and the cycle rewrites the old, reachable packs to
// free nothing — applied; date a pack's arrival by the applying node's own
// clock rather than its record's broker instant, and the young pack is weighed
// as old, and found due.
func TestAWeeklyExpungeThatWouldDropNothingBeginsNothing(t *testing.T) {
	t.Parallel()
	clock := &steppedClock{at: time.Now()}
	x := steppedFixture(t, clock)
	x.push(96 << 10)
	mid := x.between(clock)
	h := x.handmade()
	b := make([]byte, 96<<10)
	for i := range b {
		b[i] = byte(x.rng.Uint32())
	}
	young, objects := h.commit("refs/heads/young", map[string]string{"young.bin": string(b)})
	bare, cs := h.pack(objects)
	x.proposePack(bare, cs, "refs/heads/anchor", x.p.Head(x.work))
	live := func() []string {
		var out []string
		for _, row := range x.rows().Packs {
			if row.State == code.PackLive {
				out = append(out, row.PackID)
			}
		}
		slices.Sort(out)
		return out
	}
	before := live()

	c, _, l := x.compactorOf(func(o *compact.Options) {
		o.Now = func() time.Time { return mid.Add(code.PruneGrace) }
	})
	compact.ExpungeFrom(c, 1)
	cand, ok := x.weekly(codetest.RepoName, mid.Add(code.PruneGrace-code.ExpungeInterval))
	if !ok {
		t.Fatalf("the weekly arm names no %s, never expunged", codetest.RepoName)
	}
	if out, err := expungeWhileBusy(t, c, cand); out != compact.OutcomeNotDue {
		t.Fatalf("a scan whose old packs hold only what a ref reaches answered %s, %v; want not due:\n%s",
			out, err, l)
	}
	if n := l.count("code_expunge_due"); n != 0 {
		t.Errorf("the scan of a repository whose old packs would drop nothing found it due %d times:\n%s", n, l)
	}
	if n := l.count("code_expunge_begun"); n != 0 {
		t.Errorf("a weekly expunge that would drop nothing began %d cycles", n)
	}
	if after := live(); !slices.Equal(after, before) {
		t.Errorf("the live packs moved from %v to %v under a weekly expunge that would drop nothing",
			before, after)
	}
	if !x.liveHold(young) {
		t.Errorf("the commit %s, in a pack younger than the grace, left the live packs", young)
	}
}

// A REPOSITORY UNDER THE MINIMUM IS JUDGED FROM ITS ROW: a repository whose
// live bytes are under the least an expunge frees can never be due — what no
// tip reaches is at most its live bytes — so a pass answers it not due and
// records the scan without syncing it (no download of a repository no cache
// holds, no admission, no walk), and the weekly arm leaves it out for a week.
//
// Mutation: drop the row's check, and the scan syncs the small repository.
func TestARepositoryUnderTheMinimumIsJudgedFromItsRow(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(16 << 10)
	counted := &syncs{}
	c, _, l := x.compactorOf(func(o *compact.Options) {
		counted.Cache = o.Cache
		o.Cache = counted
	})
	compact.ExpungeFrom(c, 1<<30)
	cand, ok := x.weekly(codetest.RepoName, time.Now())
	if !ok {
		t.Fatalf("the weekly arm names no %s, never judged", codetest.RepoName)
	}
	if out, err := expungeWhileBusy(t, c, cand); out != compact.OutcomeNotDue {
		t.Fatalf("a repository of %d live bytes under a least of a gibibyte answered %s, %v; want not due:\n%s",
			cand.LiveBytes, out, err, l)
	}
	if n := counted.n.Load(); n != 0 {
		t.Errorf("judging a repository under the least synced it %d times, want none", n)
	}
	if _, ok := x.weekly(codetest.RepoName, time.Now().Add(-time.Minute)); ok {
		t.Error("the weekly arm still names the repository judged a moment ago")
	}
}

// A QUARTER UNDER THE MINIMUM IS NOT DUE: a repository whose packs old enough
// to take hold more than a quarter of its live bytes unreachable, but fewer
// unreachable bytes than the least an expunge frees, is scanned and left
// alone — not due, its leak still in a live pack.
//
// Mutation: drop the least from the due rule (`unreachable >= least`), and
// the scan expunges the leak.
func TestAQuarterUnderTheMinimumIsNotDue(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.push(96 << 10)
	leaked := x.leakBytes("leak", 64<<10)
	c := x.past()
	compact.ExpungeFrom(c, 128<<10)
	cand, ok := x.weekly(codetest.RepoName, time.Now().Add(code.PruneGrace+time.Hour-code.ExpungeInterval))
	if !ok {
		t.Fatalf("the weekly arm names no %s, never judged", codetest.RepoName)
	}
	if cand.LiveBytes < 128<<10 {
		t.Fatalf("the repository holds %d live bytes, under the least of 128 KiB the case scans past", cand.LiveBytes)
	}
	if out, err := expungeWhileBusy(t, c, cand); out != compact.OutcomeNotDue {
		t.Fatalf("a scan of 64 KiB unreachable under a least of 128 KiB answered %s, %v; want not due", out, err)
	}
	if !x.liveHold(leaked) {
		t.Error("a scan that found the repository not due took its leak out")
	}
}

// syncs is a compact.Cache that counts the syncs a compactor asks of it.
type syncs struct {
	compact.Cache
	n atomic.Int64
}

func (s *syncs) Sync(ctx context.Context, repoID string, fresh statelog.Freshness) (cache.Synced, error) {
	s.n.Add(1)
	return s.Cache.Sync(ctx, repoID, fresh)
}
