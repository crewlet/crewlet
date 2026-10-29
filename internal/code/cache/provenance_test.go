package cache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE INDEX OF PUSHES (provenance.go, backfill.go), held to the rows the log
// holds: what each push introduced and every move it landed, folded by the
// rounds, read back by the backfill, kept, linked and dropped as its
// declaration says.

// indexAgainstLog is every way repo's index in c, read through s, says what
// the push rows the log holds do not: within the coverage it claims, every
// commit a push introduced answers that push — the newest the coverage holds
// listing it — as its row names it whole (who, how, and a seat's run), and
// every ref's moves are the log's moves of it, in log order; outside its
// coverage, no commit answers any push; and it is level with the incarnation
// (its Through is s's position). None for an index that says exactly what the
// rows say.
func indexAgainstLog(c *cache.Cache, log *fakeLog, repo string, s cache.Synced) []string {
	p, err := c.Provenance(s)
	switch {
	case err != nil:
		return []string{fmt.Sprintf("it cannot be read: %v", err)}
	case !p.Indexed():
		return []string{"it keeps no index of pushes"}
	}
	cov := p.Coverage()
	from, through := cov.From.Packed(), cov.Through.Packed()
	gapFrom, gapThrough := cov.GapFrom.Packed(), cov.GapThrough.Packed()
	covered := func(at int64) bool {
		return at > from && at <= through && (gapThrough <= gapFrom || at <= gapFrom || at > gapThrough)
	}
	var problems []string
	if through != s.Position.Packed() {
		problems = append(problems, fmt.Sprintf("it ends at %d, and its incarnation is at %d", through,
			s.Position.Packed()))
	}
	// The commits in (position, push id) order, the order a run holds its
	// pushes in, whose last listing a commit is the one it answers; the moves
	// in the order the pushes LANDED, which group commit decided one after
	// another inside a record and the log wrote in that order.
	introducer := map[string]code.PushedBy{}
	listed := map[string]bool{}
	for _, r := range log.pushRows(repo) {
		for _, oid := range r.Introduced {
			listed[oid] = true
			if covered(r.Position.Packed()) {
				introducer[oid] = r.PushedBy
			}
		}
	}
	moves := map[string][]string{}
	for _, r := range log.landedRows(repo) {
		for _, m := range r.Moves {
			if _, ok := moves[m.Ref]; !ok {
				moves[m.Ref] = nil
			}
			if covered(r.Position.Packed()) {
				moves[m.Ref] = append(moves[m.Ref], movePrint(r.PushID, r.Position, m))
			}
		}
	}
	for _, oid := range sortedKeys(listed) {
		by, ok := p.Commit(oid)
		want, in := introducer[oid]
		switch {
		case in && (!ok || by != want):
			problems = append(problems, fmt.Sprintf("it answers %s as introduced by %+v (%t), and push %+v in "+
				"its coverage introduced it", oid, by, ok, want))
		case !in && ok:
			problems = append(problems, fmt.Sprintf("it answers %s as introduced by %s, and no push in its "+
				"coverage (%d, %d] less (%d, %d] lists it", oid, by.PushID, from, through, gapFrom, gapThrough))
		}
	}
	for _, ref := range sortedKeys(moves) {
		v := p.Moves(ref, cov.Through)
		var got []string
		for i := range v.Len() {
			m := v.At(i)
			got = append(got, movePrint(m.By.PushID, m.By.Position, m.RefMove))
		}
		if !slices.Equal(got, moves[ref]) {
			problems = append(problems, fmt.Sprintf("its moves of %s are %q, and the rows' in its coverage are %q",
				ref, got, moves[ref]))
		}
	}
	return problems
}

// movePrint is one move as a case compares it.
func movePrint(pushID string, at statelog.Position, m code.RefMove) string {
	return fmt.Sprintf("%s@%d %s>%s forced=%t", pushID, at.Packed(), m.Old[:7], m.New[:7], m.Forced)
}

// sortedKeys is m's keys, sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// introducedBy is the push c's index, read through s, answers as having
// introduced oid, or "".
func introducedBy(t testing.TB, c *cache.Cache, s cache.Synced, oid string) string {
	t.Helper()
	p, err := c.Provenance(s)
	if err != nil {
		t.Fatalf("reading the index of pushes: %v", err)
	}
	by, _ := p.Commit(oid)
	return by.PushID
}

// lastPush is the push row the log holds newest for repo.
func lastPush(t testing.TB, log *fakeLog, repo string) code.IntroducedRow {
	t.Helper()
	rows := log.pushRows(repo)
	if len(rows) == 0 {
		t.Fatalf("the log holds no push of %s", repo)
	}
	return rows[len(rows)-1]
}

// mustIndex is repo's index in c, failing t where it keeps none.
func mustIndex(t testing.TB, c *cache.Cache, repo string) cache.IndexState {
	t.Helper()
	idx, ok := cache.Index(c, repo)
	if !ok {
		t.Fatalf("%s keeps no index of pushes", repo)
	}
	return idx
}

// backfill takes every backfill step due, failing t on a page the log did not
// serve, and answers them — once its schedule is held to a look at every copy
// (cache.BackfillAudit), before the pass and after it.
func backfill(t testing.TB, c *cache.Cache) []string {
	t.Helper()
	if problems := cache.BackfillAudit(c); len(problems) > 0 {
		t.Fatalf("before the backfill, its schedule:\n\t%s", strings.Join(problems, "\n\t"))
	}
	steps, err := cache.BackfillPass(c)
	if err != nil {
		t.Fatalf("the backfill: %v", err)
	}
	if problems := cache.BackfillAudit(c); len(problems) > 0 {
		t.Fatalf("after the backfill, its schedule:\n\t%s", strings.Join(problems, "\n\t"))
	}
	return steps
}

// A DELTA FOLDS ITS PUSHES BEFORE IT ANSWERS: the sync that applied a push is
// answered from an index that already names it — the push that introduced
// its commit, and the move it landed — with no backfill having run, since a
// debounced index would miss a just-pushed change exactly when a reviewer
// asks about it. The one run it wrote covers exactly (the incarnation's
// position before, its position after].
//
// Mutation: skip the delta's fold (foldPushes answers at once) — the commit
// is answered by no push.
func TestADeltaFoldsItsPushesBeforeItAnswers(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	before := w.sync(t, c, repo).Position

	oid, _ := w.push(t, repo, "refs/heads/main", nil)
	pushed := lastPush(t, w.log, repo)
	s := w.sync(t, c, repo)
	if s.Kind != cache.SyncDelta {
		t.Fatalf("the sync after the push was %s, want a delta", s.Kind)
	}
	if got := introducedBy(t, c, s, oid); got != pushed.PushID {
		t.Errorf("the index names %q as introducing the pushed commit, want push %s", got, pushed.PushID)
	}
	p, err := c.Provenance(s)
	if err != nil {
		t.Fatal(err)
	}
	if v := p.Moves("refs/heads/main", s.Position); v.Len() != 1 || v.At(0).New != oid ||
		v.At(0).By.PushID != pushed.PushID {
		t.Errorf("main's moves in the index are %d, want the one push that moved it to %s", v.Len(), oid)
	}
	idx := mustIndex(t, c, repo)
	if len(idx.Runs) != 1 || idx.RunRanges[0] != [2]int64{before.Packed(), s.Position.Packed()} {
		t.Errorf("the index holds runs %v over %v, want one over (%d, %d]", idx.Runs, idx.RunRanges,
			before.Packed(), s.Position.Packed())
	}
	if reads := w.log.readsOf(repo); len(reads) != 0 {
		t.Errorf("the index read %d pages of pushes, want none: a delta's own pushes phase is all it needs",
			len(reads))
	}
	if problems := indexAgainstLog(c, w.log, repo, s); len(problems) > 0 {
		t.Errorf("the index after the delta:\n\t%v", problems)
	}
}

// THE INDEX NAMES THE TURN THAT PUSHED: a seat's push the engine built is
// answered, from the index alone, on the sync that folded it, with the whole
// run its row names — the turn, the work item's tracker and key, the model —
// and its way; a coding box's push git received with its run's launch and its
// coding agent beside them; a person's push git received with no run. So what
// a git node says of who pushed a commit (code_landed_in, every server-side
// read naming the push behind a commit) is all its push row says, never the
// credential alone.
//
// Mutations: write no work key into a run (pushPayload skipping tag 5) — the
// seat's push is answered without it; write no run id (skipping tag 7) — the
// box's push is answered as one no coding run made.
func TestTheProvenanceIndexNamesTheTurnThatPushed(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo)

	var seat, box, person bool
	for range 3 {
		oid, _ := w.push(t, repo, "refs/heads/main", nil)
		want := lastPush(t, w.log, repo).PushedBy
		s := w.sync(t, c, repo)
		p, err := c.Provenance(s)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := p.Commit(oid)
		if !ok || got != want {
			t.Errorf("the pushed commit is answered as by %+v (%t), want its row's %+v", got, ok, want)
		}
		switch run := want.Principal.Run; {
		case want.Principal.Kind == code.PrincipalAgent && want.Via == code.ViaEngine:
			if run.TurnID == "" || run.WorkBackend == "" || run.WorkKey == "" || run.Model == "" {
				t.Fatalf("the log's seat push %+v names no whole run the engine built: the case holds none", want)
			}
			seat = true
		case want.Principal.Kind == code.PrincipalAgent:
			if run.TurnID == "" || run.RunID == "" || run.CodingAgent == "" || want.Via != code.ViaGit {
				t.Fatalf("the log's box push %+v names no coding run git received: the case holds none", want)
			}
			box = true
		case want.Principal.Kind == code.PrincipalOperator:
			if !got.Principal.Run.IsZero() {
				t.Errorf("a person's push is answered with the run %+v", got.Principal.Run)
			}
			person = true
		}
	}
	if !seat || !box || !person {
		t.Fatalf("three pushes held a seat's (%t), a coding box's (%t) and a person's (%t): the case needs "+
			"all three", seat, box, person)
	}
}

// THE INDEX IS LEVEL WITH ITS INCARNATION after every round that answered: a
// first full read, a delta that served pushes, a delta that served none (the
// log moved on another repository), a delta of a group commit's record, and a
// restart's full read — after each, the index ends exactly where the copy
// does, and says exactly what the rows do within what it covers.
//
// Mutation: leave Through where it was on a delta that served no pushes — the
// index ends below its incarnation.
func TestTheIndexIsLevelWithItsIncarnation(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo, other := w.repo(t, 1), w.repo(t, 2)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	level := func(when string) {
		t.Helper()
		s := w.sync(t, c, repo)
		if idx := mustIndex(t, c, repo); idx.Through != s.Position.Packed() {
			t.Errorf("after %s the index ends at %d, and the copy is at %d", when, idx.Through, s.Position.Packed())
		}
		// The runs a previous process left are read once the backfill has
		// checked them.
		backfill(t, c)
		if problems := indexAgainstLog(c, w.log, repo, s); len(problems) > 0 {
			t.Errorf("after %s:\n\t%v", when, problems)
		}
	}
	level("the first full read")
	w.push(t, repo, "refs/heads/main", nil)
	level("a delta that served a push")
	w.push(t, other, "refs/heads/main", nil)
	level("a delta that served no push")
	a, packA := w.commit(t, repo, nil, "")
	b, packB := w.commit(t, repo, nil, "")
	w.log.writePushes(repo,
		fakePush{packs: []code.PackRow{packA}, refs: map[string]string{"refs/heads/main": a}, introduced: []string{a}},
		fakePush{packs: []code.PackRow{packB}, refs: map[string]string{"refs/heads/main": b}, introduced: []string{b}})
	level("a delta of one record holding two pushes")
	c = w.reopen(t, c)
	level("a restart's full read")
}

// A FULL ROUND LEAVES A GAP, AND THE BACKFILL FILLS IT. Pushes made while the
// node was down are past what its index read; the restart's full read reads
// no pushes, so it leaves the range between to the index's gap — and until the
// backfill reads it, no commit pushed there is answered by any push, never by
// a wrong one. The backfill reads the gap at the session floor of its end, so
// whichever data node answers holds every push in it, and closes it.
//
// Mutation: close the gap without reading it (gapPage commits its move with no
// rows) — the three commits are answered by no push.
func TestAFullRoundLeavesAGapTheBackfillFills(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	was := w.sync(t, c, repo).Position.Packed()
	c.Stop(context.Background())

	var oids []string
	for range 3 {
		oid, _ := w.push(t, repo, "refs/heads/main", nil)
		oids = append(oids, oid)
	}
	c = w.open(t)
	s := w.sync(t, c, repo)
	idx := mustIndex(t, c, repo)
	if idx.GapFrom != was || idx.GapThrough != s.Position.Packed() || idx.Through != s.Position.Packed() {
		t.Fatalf("after the restart the index is %+v, want a gap (%d, %d] it has not read", idx, was,
			s.Position.Packed())
	}
	for _, oid := range oids {
		if got := introducedBy(t, c, s, oid); got != "" {
			t.Errorf("a commit pushed in the gap is answered by %s before the gap is read", got)
		}
	}

	backfill(t, c)
	if idx := mustIndex(t, c, repo); idx.GapThrough != 0 {
		t.Errorf("after the backfill the index still has a gap: %+v", idx)
	}
	if problems := indexAgainstLog(c, w.log, repo, s); len(problems) > 0 {
		t.Errorf("after the backfill:\n\t%v", problems)
	}
	reads := w.log.readsOf(repo)
	if len(reads) == 0 {
		t.Fatal("the backfill read no page of pushes")
	}
	if f := reads[0].Fresh; f.Level != statelog.ReadSession || f.Floor(topics.CodeLogStream) != s.Position {
		t.Errorf("the gap's page was read at %s, floor %s: want session at the gap's end %s", f.Level,
			f.Floors, s.Position)
	}
}

// A PAGE CUT INSIDE A RECORD LEAVES THE RECORD WHOLE. Group commit lands
// several pushes in one record, at one position, and a page — bounded by its
// rows — ends inside one: the run a page makes is complete over its range, so
// the record's pushes are all in one run and none is skipped, whether the
// page ends inside the record or every row of a page is the record's.
//
// Mutation: let a page's run cover the position its last row is at
// (gapPage's upTo the last row's position, its rows kept) — the push of that
// record on the next page is answered by no push.
func TestAPageCutInsideARecordLeavesItWhole(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo)
	c.Stop(context.Background())

	w.push(t, repo, "refs/heads/main", nil)
	var group []fakePush
	for _, ref := range []string{"refs/heads/a", "refs/heads/b", "refs/heads/c"} {
		oid, pack := w.commit(t, repo, nil, "")
		group = append(group, fakePush{packs: []code.PackRow{pack}, refs: map[string]string{ref: oid},
			introduced: []string{oid}})
	}
	w.log.writePushes(repo, group...)
	w.push(t, repo, "refs/heads/main", nil)
	w.log.set(func(f *fakeLog) { f.pageRows = 2 })

	c = w.open(t)
	s := w.sync(t, c, repo)
	backfill(t, c)
	if problems := indexAgainstLog(c, w.log, repo, s); len(problems) > 0 {
		t.Errorf("after the backfill read the gap two rows a page:\n\t%v", problems)
	}
	if idx := mustIndex(t, c, repo); idx.GapThrough != 0 {
		t.Errorf("after the backfill the index still has a gap: %+v", idx)
	}
}

// ONLY A WANTED HISTORY IS BACKFILLED. A copy's index begins at its first full
// read: every holder folds every delta and fills its own gap, but reads the
// history before its first read only once a seat's read of the repository has
// been answered here as its home (Cache.WantHistory), so a repository's 181
// days are read once per fleet. Once wanted, the backfill reads it, newest
// first, at the session floor of where the index begins, down to its start:
// the index is Complete, and every commit any push introduced is answered.
//
// Mutation: read the history whether or not it is wanted (backfillDue's
// wantHistory test dropped) — the unwanted arm reads history pages.
func TestOnlyAWantedHistoryIsBackfilled(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	var oids []string
	for range 3 {
		oid, _ := w.push(t, repo, "refs/heads/main", nil)
		oids = append(oids, oid)
	}
	w.log.set(func(f *fakeLog) { f.pageRows = 1 })
	c := w.open(t)
	s := w.sync(t, c, repo)
	from := mustIndex(t, c, repo).From

	steps := backfill(t, c)
	if slices.Contains(steps, "history") {
		t.Errorf("the backfill took %v for a history nobody wanted", steps)
	}
	for _, q := range w.log.readsOf(repo) {
		if q.Introduced.Descending {
			t.Errorf("the backfill read a history page (%+v) nobody wanted", *q.Introduced)
		}
	}
	if got := introducedBy(t, c, s, oids[0]); got != "" {
		t.Errorf("a commit pushed before the index began is answered by %s with no history read", got)
	}

	c.WantHistory(repo)
	steps = backfill(t, c)
	if !slices.Contains(steps, "history") {
		t.Fatalf("the backfill took %v once the history was wanted, want history pages", steps)
	}
	idx := mustIndex(t, c, repo)
	if !idx.Complete {
		t.Errorf("the index after its history was read is %+v, want it Complete", idx)
	}
	if problems := indexAgainstLog(c, w.log, repo, s); len(problems) > 0 {
		t.Errorf("after the history was read:\n\t%v", problems)
	}
	for _, q := range w.log.readsOf(repo) {
		if q.Introduced.Descending && (q.Fresh.Level != statelog.ReadSession || q.Fresh.Floor(topics.CodeLogStream).Packed() > from) {
			t.Errorf("a history page was read at %s floor %s: want session at a floor the index has seen, at "+
				"most %d", q.Fresh.Level, q.Fresh.Floors, from)
		}
	}
}

// THE BACKFILL PACES BY THE FLEET'S RATIO: after each page it waits
// BackfillIdle × ⌈live git nodes ÷ live data nodes⌉ × the page's own duration,
// the counts read from the fleet after every page, so the fleet's backfills
// hold under half of one reader of each data node however many git nodes
// share them — and counts it cannot read are waited on and read again, never
// paced on a guess.
//
// Mutation: pace by BackfillIdle × the page alone (drop the ratio) — the wait
// is 20 ms, not 120 ms.
func TestTheBackfillPacesByTheFleetsRatio(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, repo)
	c.Stop(context.Background())
	w.push(t, repo, "refs/heads/main", nil)

	// Every page the backfill reads takes 10 ms on the cache's clock.
	const page = 10 * time.Millisecond
	w.log.set(func(f *fakeLog) {
		f.readHook = func(code.ReadQuery) error {
			clk.Add(page)
			return nil
		}
	})
	var mu sync.Mutex
	fleetFails := 1
	waits := make(chan time.Duration, 16)
	c = w.open(t, clk.on, func(o *cache.Options) {
		o.Fleet = func() (int, int, error) {
			mu.Lock()
			defer mu.Unlock()
			if fleetFails > 0 {
				fleetFails--
				return 0, 0, errors.New("the presence view is not answering")
			}
			return 16, 3, nil
		}
		cache.BackfillWakesWith(o, func(d time.Duration) <-chan time.Time {
			if d >= time.Hour {
				// Parked until the next horizon: nothing left to see.
				return nil
			}
			select {
			case waits <- d:
			default:
			}
			wake := make(chan time.Time, 1)
			wake <- clk.Now()
			return wake
		})
	})
	w.sync(t, c, repo)
	c.Start(t.Context())

	first := <-waits
	if first < cache.BackfillRetry*4/5 || first > cache.BackfillRetry*6/5 {
		t.Errorf("with the fleet's counts unreadable the backfill waited %s, want its first retry, %s ±20%%",
			first, cache.BackfillRetry)
	}
	if paced, want := <-waits, time.Duration(cache.BackfillIdle*6)*page; paced != want {
		t.Errorf("after a 10 ms page with 16 git nodes over 3 data nodes the backfill waited %s, want %s "+
			"(BackfillIdle × ⌈16÷3⌉ × the page)", paced, want)
	}
}

// THE HORIZON DROPS WHAT THE ROWS DROPPED. Once a day the backfill asks the
// rows for the oldest push they keep; the index then holds nothing below it:
// a run wholly below is retired at once, its file moved out of the copy, and
// a commit only pushes below it introduced is answered by no push — so the
// index holds the push history the rows do, and never grows past it.
//
// Mutation: keep a run wholly below the horizon (horizonPage drops no run) —
// its file stays in the copy.
func TestTheHorizonDropsWhatTheRowsDropped(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, repo)

	// A run of one push, then a run of three: two runs, short of the
	// ProvenanceMergeWidth a merge of the backfill takes, so it merges
	// neither into the other.
	old, _ := w.push(t, repo, "refs/heads/main", nil)
	expired := lastPush(t, w.log, repo).Position
	w.sync(t, c, repo)
	var kept []string
	for range 3 {
		oid, _ := w.push(t, repo, "refs/heads/main", nil)
		kept = append(kept, oid)
	}
	s := w.sync(t, c, repo)
	backfill(t, c) // the horizon's first ask, with nothing yet expired
	idx := mustIndex(t, c, repo)
	if len(idx.Runs) != 2 {
		t.Fatalf("the index holds runs %v over %v, want the two the deltas wrote", idx.Runs, idx.RunRanges)
	}
	gone := filepath.Join(s.Dir, cache.RunRel(idx.Runs[0]))

	w.log.expirePushes(repo, expired)
	clk.Add(cache.HorizonEvery)
	s = w.sync(t, c, repo)
	if steps := backfill(t, c); !slices.Contains(steps, "horizon") {
		t.Fatalf("a day on, the backfill took %v, want the horizon", steps)
	}
	idx = mustIndex(t, c, repo)
	if len(idx.Runs) != 1 || idx.From < expired.Packed() || !idx.Complete {
		t.Errorf("past the horizon the index is %+v, want the newer run alone, From at or past %d, Complete", idx,
			expired.Packed())
	}
	if statOK(t, gone) {
		t.Errorf("the run wholly below the horizon is still at %s", gone)
	}
	if got := introducedBy(t, c, s, old); got != "" {
		t.Errorf("a commit only an expired push introduced is answered by %s", got)
	}
	for _, oid := range kept {
		if got := introducedBy(t, c, s, oid); got == "" {
			t.Errorf("a commit a kept push introduced (%s) is answered by no push", oid)
		}
	}
}

// A RUN'S TRAILER IS CHECKED BEFORE ANYTHING READS IT. A run a previous
// process left is no file this process wrote: until the backfill has held it
// to its own trailer, a reader is answered without the index rather than
// reading it, and a run that fails is the copy's repair's — its next round
// drops the index and makes it anew, and the backfill reads again what the
// dropped one held.
//
// Mutation: map a run with no vouch held for it (mapRun's vouch test
// dropped) — the damaged run is read before any check.
func TestARunsTrailerIsChecked(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo)
	oid, _ := w.push(t, repo, "refs/heads/main", nil)
	s := w.sync(t, c, repo)
	idx := mustIndex(t, c, repo)
	if len(idx.Runs) != 1 {
		t.Fatalf("the index holds runs %v, want the delta's one", idx.Runs)
	}
	run := filepath.Join(s.Dir, cache.RunRel(idx.Runs[0]))
	c.Stop(context.Background())
	flipInPlace(t, run)

	c = w.open(t)
	s = w.sync(t, c, repo)
	if _, err := c.Provenance(s); err == nil {
		t.Fatal("the index of a run no check has read yet was read")
	}
	if steps := backfill(t, c); len(steps) == 0 || steps[0] != "check" {
		t.Fatalf("the backfill took %v, want the run's check first", steps)
	}
	if got := counter(w.rec, metrics.CodeCacheChecks, "file", "provenance"); got == 0 {
		t.Error("the run's check was counted under no file=provenance")
	}
	s = w.sync(t, c, repo) // the repair round the check asked for has run before this answers
	idx = mustIndex(t, c, repo)
	if len(idx.Runs) != 0 || idx.From != s.Position.Packed() {
		t.Fatalf("after the damaged run's repair the index is %+v, want one made anew at %d", idx,
			s.Position.Packed())
	}
	if statOK(t, run) {
		t.Errorf("the damaged run is still at %s", run)
	}
	c.WantHistory(repo)
	backfill(t, c)
	if got := introducedBy(t, c, s, oid); got != lastPush(t, w.log, repo).PushID {
		t.Errorf("after the history was read again the commit is answered by %q, want push %s", got,
			lastPush(t, w.log, repo).PushID)
	}
}

// A REBUILD LINKS THE INDEX. The index is a function of the rows, which a
// rebuild reads again unchanged, so the rebuild's new copy links every run of
// the copy it replaces — the same inode — with its record, and reads no page
// of pushes for it: its history and its gap are what they were.
//
// Mutation: seed nothing (seedIndex returns at once) — the new copy's index
// is empty and its commits answered by no push.
func TestARebuildLinksTheIndex(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo)
	var oids []string
	for range 2 {
		oid, _ := w.push(t, repo, "refs/heads/main", nil)
		oids = append(oids, oid)
		w.sync(t, c, repo)
	}
	s, err := c.Sync(t.Context(), repo, linearizable)
	if err != nil {
		t.Fatal(err)
	}
	before := mustIndex(t, c, repo)
	old := filepath.Join(s.Dir, cache.RunRel(before.Runs[0]))
	rebuilt, err := cache.RebuildWhole(t.Context(), c, s, cache.Failure{Reason: cache.RebuildCorrupt,
		Began: time.Now()})
	s.Release()
	if err != nil {
		t.Fatalf("the rebuild: %v", err)
	}
	defer rebuilt.Release()
	if rebuilt.Dir == s.Dir {
		t.Fatal("the rebuild answered the copy it was to replace")
	}
	after := mustIndex(t, c, repo)
	if !slices.Equal(after.Runs, before.Runs) || after.From != before.From {
		t.Errorf("the rebuilt copy's index is %+v, want the replaced one's runs %v", after, before.Runs)
	}
	oldInfo, err1 := os.Stat(old)
	newInfo, err2 := os.Stat(filepath.Join(rebuilt.Dir, cache.RunRel(before.Runs[0])))
	if err1 == nil && err2 == nil && !os.SameFile(oldInfo, newInfo) {
		t.Error("the rebuilt copy's run is another file than the replaced copy's: a rebuild links it")
	} else if err2 != nil {
		t.Errorf("the rebuilt copy holds no run %d: %v", before.Runs[0], err2)
	}
	for _, oid := range oids {
		if got := introducedBy(t, c, rebuilt, oid); got == "" {
			t.Errorf("after the rebuild the commit %s is answered by no push", oid)
		}
	}
	if reads := w.log.readsOf(repo); len(reads) != 0 {
		t.Errorf("the rebuild read %d pages of pushes, want none", len(reads))
	}
}

// A LOG GONE BACK DROPS THE INDEX. A log recreated under the cache — another
// stream — or restored from an older copy on the same stream reuses positions
// for other records, so the full read that converges the copy to it drops
// every run the index held before anything reads the copy, and makes an index
// anew at its own position: nothing the old history said is answered.
//
// Mutation: drop nothing on a history gone (OnHistoryGone Keep) — the runs of
// the old history are still read.
func TestARewoundLogDropsTheIndex(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"recreated", "restored"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t)
			w.sync(t, c, repo)
			earlier := w.log.state()
			oid, _ := w.push(t, repo, "refs/heads/main", nil)
			w.sync(t, c, repo)
			if idx := mustIndex(t, c, repo); len(idx.Runs) != 1 {
				t.Fatalf("the index holds runs %v, want the delta's one", idx.Runs)
			}
			if how == "recreated" {
				w.log.recreate(earlier)
			} else {
				w.log.restore(earlier)
			}
			s := w.sync(t, c, repo)
			idx := mustIndex(t, c, repo)
			if len(idx.Runs) != 0 || idx.From != s.Position.Packed() || idx.Through != s.Position.Packed() {
				t.Errorf("after the log was %s the index is %+v, want one made anew at %d", how, idx,
					s.Position.Packed())
			}
			if got := introducedBy(t, c, s, oid); got != "" {
				t.Errorf("a commit of the history the log no longer holds is answered by %s", got)
			}
		})
	}
}

// A MAINTENANCE RUN KEEPS THE INDEX'S RECORD. The index has no file whose
// presence is its own — its record names its runs — so what a maintenance run
// and a round's index record of the derived files they measured leaves the
// index's coverage and runs whole, where reset by the main-file rule it was
// wiped at every pass.
//
// Mutation: reset a declaration with no main file as one whose main file is
// gone (recordDerived) — the index's record is wiped by the pass.
func TestAMaintenanceRunKeepsTheProvenanceState(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	for range caseRoundPacks {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t, clk.on, cache.RoundPacksAt(caseRoundPacks))
	w.sync(t, c, repo)
	w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo)
	before := mustIndex(t, c, repo)
	if len(before.Runs) != 1 {
		t.Fatalf("the index holds runs %v, want the delta's one", before.Runs)
	}
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want the repository", ran)
	}
	if after := mustIndex(t, c, repo); !slices.Equal(after.Runs, before.Runs) || after.Through != before.Through ||
		after.Bytes != before.Bytes {
		t.Errorf("after a maintenance run the index is %+v, want it as it was: %+v", after, before)
	}
	cache.MarkWhole(c, repo, cache.RebuildShape, 0) // a full round, whose own index measures again
	w.sync(t, c, repo)
	if after := mustIndex(t, c, repo); !slices.Equal(after.Runs, before.Runs) || after.Bytes != before.Bytes {
		t.Errorf("after a full round's index the index is %+v, want its runs as they were: %+v", after, before)
	}
}

// A FILE UNDER crewlet/ IS COUNTED. The index's runs live in the engine's own
// root of the incarnation, not under objects/ (M3-decisions M3-O77), so a boot
// that measures a copy with no state walks every root a declaration owns:
// the run's bytes are in what the layout holds, and in what the opened cache
// holds itself to its budget by.
//
// Mutation: walk objects/ alone (derivedRoots answering objects/) — the run's
// bytes are counted by neither.
func TestAFileUnderCrewletIsCountedInTheBudget(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo)
	w.push(t, repo, "refs/heads/main", nil)
	s := w.sync(t, c, repo)
	run := filepath.Join(s.Dir, cache.RunRel(mustIndex(t, c, repo).Runs[0]))
	info, err := os.Stat(run)
	if err != nil {
		t.Fatal(err)
	}
	c.Stop(context.Background())
	if err := os.Remove(filepath.Join(s.Dir, "crewlet-state.json")); err != nil {
		t.Fatal(err)
	}

	held := func() int64 {
		t.Helper()
		layout, err := cache.Prepare(t.Context(), w.layout.Dir)
		if err != nil {
			t.Fatal(err)
		}
		defer layout.Release()
		return layout.Held
	}
	with := held()
	c = w.open(t)
	total := cache.Total(c)
	c.Stop(context.Background())
	if err := os.Remove(run); err != nil {
		t.Fatal(err)
	}
	if without := held(); with-without != info.Size() {
		t.Errorf("the layout held %d bytes with the run and %d without, want the run's %d between them", with,
			without, info.Size())
	}
	if total < info.Size() {
		t.Errorf("the opened cache held itself to %d bytes, less than the run's %d", total, info.Size())
	}
}

// A CACHE NEVER STARTED BACKFILLS NOTHING, and a started one does: the backfill
// is background work, begun by Start beside the janitor, the maintainer and the
// scrubber — never by Open, so a cache opened to answer one command does no
// work nobody asked for.
//
// Mutation: start the backfill in Open — the unstarted cache reads the gap.
func TestACacheNeverStartedBackfillsNothing(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo)
	c.Stop(context.Background())
	w.push(t, repo, "refs/heads/main", nil)
	c = w.open(t)
	w.sync(t, c, repo)
	time.Sleep(200 * time.Millisecond)
	if reads := w.log.readsOf(repo); len(reads) != 0 {
		t.Fatalf("a cache never started read %d pages of pushes", len(reads))
	}
	c.Start(t.Context())
	deadline := time.Now().Add(30 * time.Second)
	for mustIndex(t, c, repo).GapThrough != 0 {
		if time.Now().After(deadline) {
			t.Fatal("a started cache's backfill never closed the gap")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A FAILING COPY HOLDS UP NO OTHER. A step that fails waits on its own
// repository: it is not taken again until its retry window ends — five
// seconds, doubling with each failure in a row — while every other copy's
// steps are taken meanwhile; and once the rows serve again, the copy's gap
// closes at the end of its window. Sharing one wait, the copy used most
// recently was picked again after every wait, so one repository the data
// nodes could not serve — a record deferred on every data node through a
// rolling upgrade, a row this build refuses — held every other copy's gap
// open for good.
//
// Mutation: take a copy's steps inside its window (backfillDue reading no
// bfRetryAt) — the failing copy is picked again at once, and the pass never
// reaches the other copy's gap.
func TestAFailingCopyHoldsUpNoOther(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	failing, other := w.repo(t, 1), w.repo(t, 2)
	repos := []string{failing, other}
	for _, repo := range repos {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t, clk.on)
	for _, repo := range repos {
		w.sync(t, c, repo)
	}
	for _, repo := range repos {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c = w.reopen(t, c, clk.on)
	w.sync(t, c, other)
	clk.Add(time.Second)
	w.sync(t, c, failing) // used last: the copy the backfill takes first
	refused := errors.New("a record this build cannot read")
	var serving atomic.Bool
	w.log.set(func(f *fakeLog) {
		f.readHook = func(q code.ReadQuery) error {
			if q.Introduced.RepoID == failing && !serving.Load() {
				return refused
			}
			return nil
		}
	})
	attempts := func() int {
		t.Helper()
		return len(w.log.readsOf(failing))
	}

	steps, err := cache.BackfillPass(c)
	if !errors.Is(err, refused) {
		t.Fatalf("the pass took %v and answered %v, want the failing copy's refusal among its steps", steps, err)
	}
	if idx := mustIndex(t, c, other); idx.GapThrough != 0 {
		t.Errorf("the other copy's gap is open after the pass: %+v", idx)
	}
	if n := attempts(); n != 1 {
		t.Errorf("the failing copy's gap was asked %d times inside its window, want once", n)
	}
	// Its window ends, and the next failure doubles it.
	clk.Add(cache.BackfillRetry * 6 / 5)
	if _, err := cache.BackfillPass(c); !errors.Is(err, refused) {
		t.Fatalf("once its window ended the pass answered %v, want the copy asked again and refused", err)
	}
	clk.Add(cache.BackfillRetry * 6 / 5)
	if _, err := cache.BackfillPass(c); err != nil || attempts() != 2 {
		t.Errorf("a window into its second failure the pass answered %v after %d asks, want none taken, two "+
			"asks in all: the window doubled", err, attempts())
	}
	// The rows serve again: the gap closes once the window ends.
	serving.Store(true)
	clk.Add(2 * cache.BackfillRetry)
	backfill(t, c)
	if idx := mustIndex(t, c, failing); idx.GapThrough != 0 {
		t.Errorf("once its rows serve again and its window ended, the copy's gap is open: %+v", idx)
	}
}

// A RUN GONE FROM THE DISK IS LOST, NOT TAKEN AGAIN FOR EVER. A run its index
// names that something removed under the cache fails the merge that maps it,
// and a check that met it gone answered "replaced" and nothing else: either
// way the next look found the same step due, and the backfill took it again
// at once, for ever — ten thousand merges in under a second, a warning each,
// until a full round made the index anew. Now the merge's failure waits out
// the copy's window, the run's vouch forgotten as the merge finds it gone,
// and the run's check once the window ends finds it LOST, as a full round
// finds one: the copy's repair round drops the index and makes it anew.
//
// Mutation: answer a run its index names that is gone "replaced"
// (checkRuns) — its check is taken again at once, and the pass never ends.
func TestARunGoneFromTheDiskIsLostNotTakenAgainForEver(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, repo)
	var s cache.Synced
	for range cache.ProvenanceMergeWidth {
		w.push(t, repo, "refs/heads/main", nil)
		s = w.sync(t, c, repo)
	}
	idx := mustIndex(t, c, repo)
	if len(idx.Runs) != cache.ProvenanceMergeWidth {
		t.Fatalf("the index holds runs %v, want the deltas' %d small runs, their merge due", idx.Runs,
			cache.ProvenanceMergeWidth)
	}
	if err := os.Remove(filepath.Join(s.Dir, cache.RunRel(idx.Runs[0]))); err != nil {
		t.Fatal(err)
	}

	steps, err := cache.BackfillPass(c)
	if !slices.Equal(steps, []string{"merge"}) || err == nil {
		t.Fatalf("the pass took %v and answered %v, want the one merge, failing", steps, err)
	}
	clk.Add(cache.BackfillRetry * 6 / 5)
	steps, err = cache.BackfillPass(c)
	if err != nil || len(steps) == 0 || steps[0] != "check" || slices.Contains(steps, "merge") {
		t.Fatalf("once its window ended the pass took %v and answered %v, want the gone run's check first "+
			"and no merge", steps, err)
	}
	// The repair round the check asked has run before this answers: a delta
	// would have kept the run beside the lost one.
	s = w.sync(t, c, repo)
	if idx = mustIndex(t, c, repo); len(idx.Runs) != 0 || idx.From != s.Position.Packed() {
		t.Errorf("after its repair the index is %+v, want one made anew at %d", idx, s.Position.Packed())
	}
}

// THE BEHIND GAUGE COUNTS WHAT THE BACKFILL OWES. crewlet.code.cache.
// provenance.behind is the copies whose index has a gap, or whose history is
// wanted and not read to its start — counted by the schedule as it places
// each copy, never by a walk of them all: the three copies a restart left
// gaps in, the log having moved under each; none once the backfill has read
// the gaps; one once a seat's read wants a copy's history; none again once
// that history is read.
//
// Mutation: count a copy behind once and never again (place leaving the count
// where a copy stops being behind) — the gauge stays at three.
func TestTheBehindGaugeCountsWhatTheBackfillOwes(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repos := []string{w.repo(t, 1), w.repo(t, 2), w.repo(t, 3)}
	for _, repo := range repos {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t)
	for _, repo := range repos {
		w.sync(t, c, repo)
	}
	for _, repo := range repos {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c = w.reopen(t, c)
	for _, repo := range repos {
		w.sync(t, c, repo)
	}
	behind := func(want float64, when string) {
		t.Helper()
		if _, err := cache.BackfillPick(c); err != nil {
			t.Fatal(err)
		}
		if got := gauge(t, w.rec, metrics.CodeCacheProvenanceBehind); got != want {
			t.Errorf("%s, crewlet.code.cache.provenance.behind is %v, want %v", when, got, want)
		}
	}
	behind(3, "with the three copies a restart left gaps in")
	backfill(t, c)
	behind(0, "once the backfill has read the gaps")
	c.WantHistory(repos[2])
	behind(1, "with a copy's history wanted")
	backfill(t, c)
	behind(0, "once that history is read")
}

// THE BACKFILL TAKES THE COPY USED LAST FIRST. Among the copies with a step
// of one kind due, the backfill takes the one a reader used most recently —
// the repositories people are asking about — and a read of a copy whose step
// is due moves it to the front of its queue as it is made (touch), however it
// is answered: a read answered from the copy as it stands, with no round
// behind it, is a use like any other.
//
// Mutation: leave the queue's key at the use its copy was placed with (used
// moving no queued copy) — the copy read last is not the one taken.
func TestTheBackfillTakesTheCopyUsedLastFirst(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repos := []string{w.repo(t, 1), w.repo(t, 2)}
	for _, repo := range repos {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t, clk.on)
	for _, repo := range repos {
		w.sync(t, c, repo)
	}
	// A restart with a push of each meanwhile: each copy's first round
	// leaves a gap, a step of the same kind due for both.
	for _, repo := range repos {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c = w.reopen(t, c, clk.on)
	for _, repo := range repos {
		w.sync(t, c, repo)
		if idx := mustIndex(t, c, repo); idx.GapThrough == 0 {
			t.Fatalf("%s's index has no gap: %+v", repo, idx)
		}
	}
	for _, read := range []string{repos[0], repos[1], repos[0]} {
		clk.Add(time.Second)
		s, err := c.Sync(t.Context(), read, stale)
		if err != nil {
			t.Fatal(err)
		}
		if s.Kind != cache.SyncNone {
			t.Fatalf("a stale read of a converged copy was a %s round, want one answered as it stands", s.Kind)
		}
		s.Release()
		got, err := cache.BackfillPick(c)
		if err != nil {
			t.Fatal(err)
		}
		if got != read {
			t.Errorf("with %s read last, the backfill takes %s first", read, got)
		}
	}
}

// SMALL RUNS MERGE A STRETCH AT A TIME. The runs one-push deltas fold wait
// until ProvenanceMergeWidth of them stand side by side, and then merge as one
// — one merge, where merged a pair at a time they merged on nearly every
// push, four syncs and a state write each on every node holding the copy —
// and the run that merge writes answers everything the eight did.
//
// Mutation: merge spans of two (mergeDue reading a width of two) — the
// backfill merges after the second delta.
func TestSmallRunsMergeAStretchAtATime(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	var (
		mu     sync.Mutex
		merges []int
	)
	c := w.open(t, func(o *cache.Options) {
		cache.MergesWith(o, func(runs int) {
			mu.Lock()
			defer mu.Unlock()
			merges = append(merges, runs)
		})
	})
	merged := func() []int {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(merges)
	}
	w.sync(t, c, repo)
	var (
		oids []string
		s    cache.Synced
	)
	for i := range cache.ProvenanceMergeWidth {
		oid, _ := w.push(t, repo, "refs/heads/main", nil)
		oids = append(oids, oid)
		s = w.sync(t, c, repo)
		backfill(t, c)
		if got := merged(); i < cache.ProvenanceMergeWidth-1 && len(got) > 0 {
			t.Fatalf("after %d deltas of a push each the backfill merged %v, want nothing yet", i+1, got)
		}
	}
	if got := merged(); !slices.Equal(got, []int{cache.ProvenanceMergeWidth}) {
		t.Errorf("the backfill merged %v, want the %d small runs once, as one", got, cache.ProvenanceMergeWidth)
	}
	if idx := mustIndex(t, c, repo); len(idx.Runs) != 1 {
		t.Errorf("after the merge the index holds runs %v, want one", idx.Runs)
	}
	for _, oid := range oids {
		if got := introducedBy(t, c, s, oid); got == "" {
			t.Errorf("after the merge the commit %s is answered by no push", oid)
		}
	}
	if problems := indexAgainstLog(c, w.log, repo, s); len(problems) > 0 {
		t.Errorf("after the merge:\n\t%v", problems)
	}
}

// A RETIRED RUN LEAVES ONCE A WRITTEN STATE NO LONGER NAMES IT. A merge
// retires the runs it replaces — the index stops naming them — but the state
// on the disk names them until the next is written, so a crash before then
// must find them there; and once a state that no longer names them is
// written, by whichever writer, they go. Moved by the writer that retired
// them once ITS state was written, runs whose state write failed stayed in
// crewlet/ for good: named by nothing once a later write succeeded, and
// counted by no record and no budget until a full round listed the directory.
//
// Mutation: forget the runs a failed write would have taken (persist dropping
// its retired runs on a failure) — the merge's inputs are still on the disk
// after the next write succeeds.
func TestARetiredRunLeavesOnceAWrittenStateNoLongerNamesIt(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	var failing atomic.Bool
	refused := errors.New("the disk is full")
	c := w.open(t, func(o *cache.Options) {
		cache.StateWriteFails(o, func(_ string, round bool) error {
			if failing.Load() && !round {
				return refused
			}
			return nil
		})
	})
	w.sync(t, c, repo)
	var s cache.Synced
	for range cache.ProvenanceMergeWidth {
		w.push(t, repo, "refs/heads/main", nil)
		s = w.sync(t, c, repo)
	}
	idx := mustIndex(t, c, repo)
	if len(idx.Runs) != cache.ProvenanceMergeWidth {
		t.Fatalf("the index holds runs %v, want the deltas' %d small runs, their merge due", idx.Runs,
			cache.ProvenanceMergeWidth)
	}
	var inputs []string
	for _, n := range idx.Runs {
		inputs = append(inputs, filepath.Join(s.Dir, cache.RunRel(n)))
	}

	failing.Store(true)
	if steps := backfill(t, c); !slices.Contains(steps, "merge") {
		t.Fatalf("the backfill took %v, want the merge of the two runs", steps)
	}
	if len(w.logs.lines(t, "code_cache_provenance_unpersisted")) == 0 {
		t.Fatal("the merge's state write was refused and nothing said so")
	}
	if got := mustIndex(t, c, repo); len(got.Runs) != 1 {
		t.Fatalf("after the merge the index holds runs %v, want the merged one", got.Runs)
	}
	for _, in := range inputs {
		if !statOK(t, in) {
			t.Errorf("the merge's input %s left the disk while the state there still names it", in)
		}
	}

	// The next state written — a round's — names neither, and takes both.
	failing.Store(false)
	oid, _ := w.push(t, repo, "refs/heads/main", nil)
	s = w.sync(t, c, repo)
	for _, in := range inputs {
		if statOK(t, in) {
			t.Errorf("the merge's input %s is still on the disk once a state that no longer names it was written",
				in)
		}
	}
	if got := introducedBy(t, c, s, oid); got != lastPush(t, w.log, repo).PushID {
		t.Errorf("the index answers the last commit with %q, want its push", got)
	}
}

// THE RUNS A NODE HOLDS MAPPED STAY UNDER ITS BOUND. Every run a reader maps
// is one mapping of the engine's process, which the kernel counts against
// vm.max_map_count — one count for the whole node — and past it every mapping
// the process asks for fails, the runtime's own heap growth among them, which
// is fatal. Kept for as long as its index named it, a run of every copy ever
// read stayed mapped, the count growing with the copies a node's seats read.
// So twenty copies of two runs each, read one after another under a bound of
// eight, hold at most eight runs mapped — the kernel's own count of them where
// it says, Linux's /proc/self/maps — and every read is answered from its
// index, the first copy's again once the bound has unmapped its runs; a
// reader that finds every mapping held by readers is answered without the
// index, and one let go makes room for it.
//
// Mutation: keep every run let go (runMaps.reserve unmapping none, its bound
// unchecked) — the twenty copies hold forty runs mapped.
func TestTheRunsANodeHoldsMappedStayUnderItsBound(t *testing.T) {
	t.Parallel()
	const bound, copies = 8, 20
	w := newSyncWorld(t, onMemory)
	var repos []string
	for i := 1; i <= copies; i++ {
		repo := w.repo(t, i)
		w.push(t, repo, "refs/heads/main", nil)
		repos = append(repos, repo)
	}
	c := w.open(t, cache.MappedRunsAt(bound))
	pin := func(repo string) cache.Synced {
		t.Helper()
		s, err := c.Sync(t.Context(), repo, linearizable)
		if err != nil {
			t.Fatalf("sync of %s: %v", repo, err)
		}
		return s
	}
	// Two runs a copy: the two deltas folded after its first full read.
	tips := map[string]string{}
	for _, repo := range repos {
		pin(repo).Release()
		for range 2 {
			tips[repo], _ = w.push(t, repo, "refs/heads/main", nil)
			pin(repo).Release()
		}
	}
	answers := func(repo string) {
		t.Helper()
		s := pin(repo)
		defer s.Release()
		p, err := c.Provenance(s)
		if err != nil {
			t.Fatalf("reading %s's index: %v", repo, err)
		}
		if by, ok := p.Commit(tips[repo]); !ok || by.PushID != lastPush(t, w.log, repo).PushID {
			t.Errorf("%s's index answers its last commit with %q (%t), want its last push", repo, by.PushID, ok)
		}
	}
	for _, repo := range repos {
		answers(repo)
		if now, most := cache.MappedRuns(c); now > bound || most != bound {
			t.Fatalf("after reading %s the node holds %d runs mapped, under a bound of %d (want %d)", repo, now,
				most, bound)
		}
	}
	if runtime.GOOS == "linux" {
		// The kernel names a mapping by its file's path with every link
		// resolved; and this cache's runs are the ones under its directory,
		// whatever the cases beside it map.
		dir, err := filepath.EvalSymlinks(w.layout.Dir)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile("/proc/self/maps")
		if err != nil {
			t.Fatal(err)
		}
		mine := 0
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, dir+"/") && strings.Contains(line, "/crewlet/provenance.") {
				mine++
			}
		}
		if mine == 0 || mine > bound {
			t.Errorf("the kernel counts %d mappings of this cache's runs, want some — the last copies read kept "+
				"for their next reader — and at most its bound of %d", mine, bound)
		}
	}
	answers(repos[0]) // its runs unmapped by the bound since, and mapped again

	// Every mapping held: a reader past the bound is answered without the
	// index, and one let go makes its room.
	var held []cache.Synced
	for _, repo := range repos[:bound/2] {
		s := pin(repo)
		if _, err := c.Provenance(s); err != nil {
			t.Fatalf("reading %s's index under a bound with room: %v", repo, err)
		}
		held = append(held, s)
	}
	s := pin(repos[copies-1])
	defer s.Release()
	if _, err := c.Provenance(s); err == nil {
		t.Error("a reader past a bound its readers hold whole read the index")
	}
	held[0].Release()
	if _, err := c.Provenance(s); err != nil {
		t.Errorf("a reader once another let its runs go: %v", err)
	}
	for _, h := range held[1:] {
		h.Release()
	}
}

// THE BACKFILL'S PICK READS A COPY'S LAST USE UNDER ITS LOCK. Among the copies
// with a step due it takes the most recently used, and every reader's pin and
// release writes that use under the entry's lock (used). Read once the lock
// had gone, the walk raced every request that pinned a copy while the backfill
// had work — on a node that has just joined, every request — which the race
// detector reported in the cache's own suite, and a torn time ranks the copies
// on a value no reader wrote.
//
// Mutation: read inc.lastUsed in nextBackfill after e.mu.Unlock — the race
// detector reports the walk's read against a pin's write.
func TestTheBackfillsPickReadsACopysLastUseUnderItsLock(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repos := []string{w.repo(t, 1), w.repo(t, 2)}
	for _, repo := range repos {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t)
	for _, repo := range repos {
		w.sync(t, c, repo)
	}
	c = w.reopen(t, c)
	for _, repo := range repos {
		w.push(t, repo, "refs/heads/main", nil)
		w.sync(t, c, repo)
		if idx := mustIndex(t, c, repo); idx.GapThrough == 0 {
			t.Fatalf("%s's index has no gap for the backfill to pick: %+v", repo, idx)
		}
	}

	// The walk on a goroutine of its own, as the backfill's is, beside the
	// requests' pins; the picks it made are counted, so a case whose walk
	// found nothing due — and so read no copy's use — fails rather than
	// passes.
	stop := make(chan struct{})
	type result struct {
		picks int
		err   error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		defer func() { done <- r }()
		for {
			select {
			case <-stop:
				return
			default:
			}
			repo, err := cache.BackfillPick(c)
			if err != nil {
				r.err = err
				return
			}
			if repo != "" {
				r.picks++
			}
		}
	}()
	var failed error
	for i := 0; i < 100 && failed == nil; i++ {
		for _, repo := range repos {
			s, err := c.Sync(t.Context(), repo, statelog.Freshness{Level: statelog.ReadLinearizable})
			if err != nil {
				failed = fmt.Errorf("sync of %s: %w", repo, err)
				break
			}
			s.Release()
		}
	}
	close(stop)
	r := <-done
	switch {
	case failed != nil:
		t.Fatal(failed)
	case r.err != nil:
		t.Fatalf("the backfill's pick: %v", r.err)
	case r.picks == 0:
		t.Fatal("the backfill's walk picked no step while both gaps were due")
	}
}
