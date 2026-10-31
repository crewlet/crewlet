package compact_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A COMPACTOR IS BUILT WHOLE OR NOT AT ALL: every seam it cannot run without
// missing is refused by name, all of them in one error, before anything
// starts.
func TestACompactorMissingASeamIsRefusedByName(t *testing.T) {
	t.Parallel()
	_, err := compact.New(compact.Options{Limits: code.DefaultLimits})
	if !errors.Is(err, compact.ErrOptions) {
		t.Fatalf("New with no seams = %v, want ErrOptions", err)
	}
	for _, name := range []string{"Cache", "Objects", "Estate", "Maintainers", "NodeID"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("New with no seams = %q, which does not name %s", err, name)
		}
	}
	if strings.Contains(err.Error(), "Limits") {
		t.Errorf("New refused the Limits it was handed: %q", err)
	}
}

// A PLAN NOT DUE IS NOT PLANNED AGAIN UNTIL THE REPOSITORY MOVES: a repository
// the listing names but whose plan finds nothing worth merging is remembered
// at its row's version, so the next pass skips it with no sync and no plan —
// and a push, which moves the version, has it planned again.
//
// Mutation: drop the memory, and the second pass syncs and plans again.
func TestAPlanNotDueIsNotPlannedAgainUntilTheRepositoryMoves(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(2, 4<<10)
	cc := countedOver(x.node().Cache())
	c, rec, _ := x.compactorOf(func(o *compact.Options) {
		o.Cache, o.Estate = cc, &listed{Estate: served{x.fleet}}
	})
	for range 2 {
		if _, err := c.Poll(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if syncs, plans := cc.counts(); syncs != 1 || plans != 1 {
		t.Errorf("two passes over a repository not due and unmoved synced it %d times and planned it %d, "+
			"want once each", syncs, plans)
	}
	x.push(4 << 10)
	if _, err := c.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if syncs, plans := cc.counts(); syncs != 2 || plans != 2 {
		t.Errorf("a pass after a push synced the repository %d times in all and planned it %d, want twice "+
			"each", syncs, plans)
	}
	if n := counter(rec, metrics.CodeCompactRuns, "outcome", string(compact.OutcomeNotDue)); n != 2 {
		t.Errorf("%s{outcome=not_due} = %d, want one a plan made", metrics.CodeCompactRuns, n)
	}
}

// A COMPACTOR THAT CANNOT READ THE PLACEMENT COMPACTS NOTHING: a node that
// cannot tell whether it is a repository's maintainer does none of a
// maintainer's work — no sync, no build — for any repository of the pass, and
// says so once. It asks by the charge a publication of the repository is
// placed by, so the repository's compactions and its publications fall to one
// node.
//
// Mutation: treat the error as the maintainer, and the pass syncs; or ask by
// the compaction's own charge, and the charge asked is not the publication's.
func TestACompactorThatCannotReadThePlacementCompactsNothing(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(2, 4<<10)
	x.fleet.Declare(t, code.Declared{Name: "other", Settings: codetest.Settings()})
	other := x.p.Init()
	x.p.Commit(other, map[string]string{"README.md": "other\n"}, "other")
	if res := x.p.Push(t.Context(), other, x.node().URL()+"/git/other", "main"); res.Err != nil {
		t.Fatalf("the push to other: %s", res)
	}
	cc := countedOver(x.node().Cache())
	placement := errors.New("who serves git cannot be read")
	var asked []int64
	c, _, l := x.compactorOf(func(o *compact.Options) {
		o.Cache, o.Estate = cc, &listed{Estate: served{x.fleet}}
		o.Maintainers = maintainers{err: placement, asked: &asked}
	})
	report, err := c.Poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Listed != 2 {
		t.Fatalf("the pass listed %d repositories, want both", report.Listed)
	}
	if syncs, _ := cc.counts(); syncs != 0 || report.Maintained != 0 {
		t.Errorf("a pass that could not read the placement synced %d times and counted %d repositories its "+
			"own, want none", syncs, report.Maintained)
	}
	if n := l.count("code_compaction_maintainer_unknown", `"level":"WARN"`); n != 1 {
		t.Errorf("the pass logged code_compaction_maintainer_unknown %d times, want once:\n%s", n, l)
	}
	cand := x.candidate()
	if want := c.PlacedBy(cand.LiveObjects); len(asked) == 0 || asked[0] != want {
		t.Errorf("the pass asked who maintains the repository by the charges %v, want its maintenance's %d, "+
			"which every loop is placed by", asked, want)
	}
}

// listedGate is a compactor's placement that lists this node among the git
// nodes once a case says so, counting the times it was asked — and has this
// node maintain every repository.
type listedGate struct {
	mu     sync.Mutex
	listed bool
	asked  int
}

func (*listedGate) MaintainerOf(charges map[string]int64) (map[string]bool, error) {
	out := map[string]bool{}
	for id := range charges {
		out[id] = true
	}
	return out, nil
}

func (g *listedGate) Listed() (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.asked++
	return g.listed, nil
}

// list has every later question answered listed.
func (g *listedGate) list() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listed = true
}

// asks is how many times the gate was asked.
func (g *listedGate) asks() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.asked
}

// kindsRead is a compactor's reads of the code host, counting each kind read.
type kindsRead struct {
	compact.Estate
	mu    sync.Mutex
	kinds map[code.ReadKind]int
}

func (k *kindsRead) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	k.mu.Lock()
	k.kinds[q.Kind]++
	k.mu.Unlock()
	return k.Estate.Read(ctx, q)
}

// read is how many reads of kind it counted.
func (k *kindsRead) read(kind code.ReadKind) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.kinds[kind]
}

// THE LOOPS PASS ONCE THIS NODE IS LISTED, AND NOT BEFORE: a node no listing of
// the git nodes carries maintains nothing (internal/estate's
// Router.Maintainer), so Start's loops ask whether it is listed, every
// compact.ListedRecheck, and read no listing until it is — a booting node is
// listed only once its host beats, and a pass before that would find every
// repository placed elsewhere or nowhere, and log a placement it could read.
// Listed, every loop passes at once.
//
// Mutation: pass at Start without asking, and the loops list while this node
// is unlisted.
func TestTheLoopsPassOnceThisNodeIsListed(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	gate := &listedGate{}
	seam := &kindsRead{Estate: served{x.fleet}, kinds: map[code.ReadKind]int{}}
	c, _, _ := x.compactorOf(func(o *compact.Options) { o.Estate, o.Maintainers = seam, gate })
	c.Start()

	// SIX ASKS UNLISTED: each of the three loops' first, and a recheck
	// waited out by each loop, or by one of them twice — whichever, none may
	// have read a listing.
	await(t, "the loops to ask whether this node is listed six times", func() bool { return gate.asks() >= 6 })
	for _, kind := range []code.ReadKind{code.ReadCompactionDue, code.ReadPublicationDue, code.ReadExpungeDue} {
		if n := seam.read(kind); n != 0 {
			t.Errorf("the loops read %d %s listings with this node unlisted, want none", n, kind)
		}
	}
	gate.list()
	await(t, "every loop to list once this node is listed", func() bool {
		return seam.read(code.ReadCompactionDue) > 0 && seam.read(code.ReadPublicationDue) > 0 &&
			seam.read(code.ReadExpungeDue) > 0
	})
}

// await waits until done holds, failing t after a minute: what a case waits
// on here is a loop's next step, a second or two away.
func await(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("waited a minute for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A PASS JUDGES ITS MAINTAINERS AGAINST ONE LISTING: a pass asks who maintains
// what ONCE, for every listed repository it must judge — on a node that
// maintains none of them, every one, which no plan of it is ever made on to
// remember — and plans none it does not maintain; and once its maintainer has
// found a plan of each not due at its version, a pass asks nothing at all: a
// repository it remembers costs the pass a lookup.
//
// Mutation: ask who maintains a repository once a repository, and the first
// pass reads the listing twice.
func TestAPassJudgesItsMaintainersAgainstOneListing(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(2, 4<<10)
	x.fleet.Declare(t, code.Declared{Name: "other", Settings: codetest.Settings()})
	other := x.p.Init()
	x.p.Commit(other, map[string]string{"README.md": "other\n"}, "other")
	if res := x.p.Push(t.Context(), other, x.node().URL()+"/git/other", "main"); res.Err != nil {
		t.Fatalf("the push to other: %s", res)
	}
	cc := countedOver(x.node().Cache())
	m := &askedMaintainers{}
	c, _, _ := x.compactorOf(func(o *compact.Options) {
		o.Cache, o.Estate, o.Maintainers = cc, &listed{Estate: served{x.fleet}}, m
	})

	report, err := c.Poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Listed != 2 {
		t.Fatalf("the pass listed %d repositories, want both", report.Listed)
	}
	if got := m.readings(); len(got) != 1 || len(got[0]) != 2 {
		t.Errorf("a pass over two repositories on a node maintaining neither asked who maintains them %v, want "+
			"once, of both", got)
	}
	if syncs, _ := cc.counts(); syncs != 0 || report.Maintained != 0 {
		t.Errorf("a pass on a node maintaining neither repository synced %d times and counted %d its own, want "+
			"none", syncs, report.Maintained)
	}

	m.setMaintains(true)
	if report, err := c.Poll(t.Context()); err != nil || report.Maintained != 2 || report.Due != 0 {
		t.Fatalf("the pass on their maintainer = %+v, %v; want both planned and found not due", report, err)
	}
	if got := m.readings(); len(got) != 2 || len(got[1]) != 2 {
		t.Errorf("the second pass asked who maintains them %v, want once more, of both", got)
	}
	if _, err := c.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := m.readings(); len(got) != 2 {
		t.Errorf("a pass over two repositories remembered as not due asked who maintains them %v, want nothing "+
			"more", got)
	}
}

// A COMPACTION THE NODE CANNOT HOLD IS ADMITTED WHEN THE NODE GROWS: a build
// whose charge the node's memory refuses is counted and never remembered —
// the node's budget is read again at every pass, a container resized in place
// among the reasons it moves — so the next pass, with no push between, plans
// it again and, once the memory holds it, applies it.
//
// Mutation: remember the refusal as a plan not due, and the second pass skips
// the repository.
func TestACompactionTheNodeCannotHoldIsAdmittedWhenTheNodeGrows(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(code.CompactMinMerge, 4<<10)
	cc := countedOver(x.node().Cache())
	cc.answerAdmissions(noMemory)
	c, rec, _ := x.compactorOf(func(o *compact.Options) { o.Cache = cc })
	report, err := c.Poll(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if report.Refused != 1 {
		t.Fatalf("the pass over a repository the node's memory cannot compact reported %+v, want it refused",
			report)
	}
	if n := counter(rec, metrics.CodeCompactRefusals, "reason", "memory"); n != 1 {
		t.Errorf("%s{reason=memory} = %d, want 1", metrics.CodeCompactRefusals, n)
	}
	cc.answerAdmissions(nil)
	report = pollWhileBusy(t, c, rec)
	if report.Compacted != 1 {
		t.Fatalf("the pass once the node holds the build reported %+v, want it compacted", report)
	}
	if live := x.rows().Repo.LivePacks; live != 1 {
		t.Errorf("the repository holds %d live packs after its compaction, want the one output", live)
	}
}

// THE COMPACTION-BEHIND ALARM HOLDS WHILE ITS COMPACTION IS REFUSED: every pass
// names in the alarm reading every listed repository at code.MaxPacksPerRepo
// it maintains — before its memory of plans not due, and whatever its
// compaction then decides — because at the bound every push carrying a pack
// is refused and records nothing, so the repository's version stands still.
// One whose build the node's memory refuses at every pass is named after every
// pass; and so is one whose plan was found not due at its version, which the
// passes after skip.
//
// Mutation: name only the repositories a pass reached a compaction of, and the
// second arm's reading is empty from its second pass.
func TestTheCompactionBehindAlarmHoldsWhileItsCompactionIsRefused(t *testing.T) {
	t.Parallel()
	named := func(t *testing.T, c *compact.Compactor, pass int) {
		t.Helper()
		var r statelog.Reading
		c.AlarmReading(&r)
		if r.CodeCompactionBehind != 1 || r.CodeCompactionBehindRepo != codetest.RepoName ||
			r.CodeCompactionBehindLargest != code.MaxPacksPerRepo {
			t.Errorf("after pass %d the reading is %d behind, the most %s at %d; want %s at %d", pass,
				r.CodeCompactionBehind, r.CodeCompactionBehindRepo, r.CodeCompactionBehindLargest,
				codetest.RepoName, code.MaxPacksPerRepo)
		}
	}
	t.Run("its build refused for memory", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.pushN(code.CompactMinMerge, 4<<10)
		cc := countedOver(x.node().Cache())
		cc.answerAdmissions(noMemory)
		est := &listed{Estate: served{x.fleet}}
		est.showPacks(code.MaxPacksPerRepo)
		c, rec, _ := x.compactorOf(func(o *compact.Options) { o.Cache, o.Estate = cc, est })
		for pass := 1; pass <= 3; pass++ {
			if _, err := c.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			named(t, c, pass)
		}
		if n := counter(rec, metrics.CodeCompactRefusals, "reason", "memory"); n != 3 {
			t.Errorf("%s{reason=memory} = %d over three passes, want one each", metrics.CodeCompactRefusals, n)
		}
	})
	t.Run("its plan not due", func(t *testing.T) {
		t.Parallel()
		x := newFixture(t)
		x.pushN(2, 4<<10)
		cc := countedOver(x.node().Cache())
		est := &listed{Estate: served{x.fleet}}
		est.showPacks(code.MaxPacksPerRepo)
		c, _, _ := x.compactorOf(func(o *compact.Options) { o.Cache, o.Estate = cc, est })
		for pass := 1; pass <= 3; pass++ {
			if _, err := c.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			named(t, c, pass)
		}
		if _, plans := cc.counts(); plans != 1 {
			t.Errorf("three passes planned the repository %d times, want once: the plan not due is "+
				"remembered", plans)
		}
	})
}

// THE ADMISSION IS RELEASED BEFORE THE UPLOADS: once a build's last git process
// has ended, its maintenance slot and its memory charge are given back, so an
// upload the store paces — hours, for the largest — never idles the node's
// maintenance for every other repository. While a compaction's upload is held,
// another admission to the lane is granted.
//
// Mutation: release the admission when the compaction is answered, and the
// second admission is refused busy for as long as the upload is held.
func TestTheAdmissionIsReleasedBeforeTheUploads(t *testing.T) {
	t.Parallel()
	x := newFixture(t)
	x.pushN(code.CompactMinMerge, 16<<10)
	entered, release := make(chan struct{}, code.MaxCompactOutputs), make(chan struct{})
	rs := &recordingStore{Store: x.store}
	rs.hold = func(ctx context.Context) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return errHeld
		}
	}
	c, _, _ := x.compactorOf(func(o *compact.Options) { o.Objects = rs })
	cand := x.candidate()
	done := make(chan compact.Outcome, 1)
	go func() {
		out, _ := compactWhileBusy(t, c, cand)
		done <- out
	}()
	select {
	case <-entered:
	case out := <-done:
		t.Fatalf("the compaction ended %s before its upload", out)
	case <-time.After(time.Minute):
		t.Fatal("the compaction reached no upload in a minute")
	}

	s, err := x.node().Cache().Sync(t.Context(), cand.ID, codetest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	var adm *cache.Admission
	for deadline := time.Now().Add(10 * time.Second); ; {
		adm, err = x.node().Cache().AdmitCompaction(s, 1, code.DefaultLimits())
		if err == nil || !errors.Is(err, gitproc.ErrBusy) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Errorf("a second admission while the first compaction's upload is held = %v, want it granted", err)
	} else {
		adm.Release()
	}
	s.Release()
	close(release)
	if out := <-done; out != compact.OutcomeApplied {
		t.Errorf("the compaction once its upload was let go: %s, want applied", out)
	}
}

// A REPOSITORY'S STORED OUTPUTS ANSWER ITS OWN HOLD, on a store that holds a
// reservation as on any: a repository the listing goes on naming once its
// compaction landed — one keeping code.CompactMinMerge full packs, or one a
// capped roll leaves listed — whose first roll a full store refused is planned
// again at its next, smaller roll as soon as the room holds THAT roll, though
// the room is below what the first would have stored. The refusal it was held
// back for is answered by its own outputs, stored; a refusal of a later roll
// would set an estimate of its own.
//
// Mutation: keep a repository's own hold, on a store that holds a
// reservation, once its outputs are stored (Compactor.stored), and the second
// roll is skipped at every pass — held to the first roll's estimate, which the
// room no longer reaches.
func TestAStoredCompactionReleasesItsOwnHold(t *testing.T) {
	t.Parallel()
	full := &objstore.Full{Remedy: "the operator raises the bucket's reservation", Err: errors.New("no room")}
	x := newFixture(t)
	// THE FIRST ROLL: code.CompactMinMerge pushes of ten files each, thirteen
	// objects a push, so its output outweighs the next roll's pushes of one
	// file — four objects each — more than twice over, and git's split leaves
	// it out of that roll.
	heavy := make([]int, 10)
	for i := range heavy {
		heavy[i] = 4 << 10
	}
	x.pushN(code.CompactMinMerge, heavy...)
	var first int64
	for _, p := range x.rows().Packs {
		first += p.Size
	}

	var (
		mu   sync.Mutex
		room int64
	)
	setRoom := func(n int64) {
		mu.Lock()
		defer mu.Unlock()
		room = n
	}
	rs := &recordingStore{Store: x.store}
	rs.refuse = func() error { return full }
	rs.usage = func() (objstore.Usage, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return objstore.Usage{Bytes: 1 << 30, MaxBytes: 1<<30 + room}, true, nil
	}
	// LISTED WHATEVER ITS PACK COUNT, as a repository keeping
	// code.CompactMinMerge full packs beside its small ones is.
	comp, rec, _ := x.compactorOf(func(o *compact.Options) {
		o.Objects, o.Estate = rs, &listed{Estate: served{x.fleet}}
	})

	if report := pollWhileBusy(t, comp, rec); report.Refused != 1 {
		t.Fatalf("the pass whose outputs the store refused for room reported %+v, want it refused", report)
	}
	rs.set(func(r *recordingStore) { r.refuse = nil })
	setRoom(first)
	if report := pollWhileBusy(t, comp, rec); report.Compacted != 1 {
		t.Fatalf("the pass once the store had room for the first roll reported %+v, want it compacted", report)
	}

	// THE SECOND ROLL: every pack pushed since the first roll's output, which
	// its split leaves out.
	outputs := x.rows().Packs
	x.pushN(code.CompactMinMerge, 1<<10)
	var second int64
	for _, p := range x.rows().Packs {
		if !slices.ContainsFunc(outputs, func(o code.PackRow) bool { return o.PackID == p.PackID }) {
			second += p.Size
		}
	}
	if second >= first {
		t.Fatalf("the premise: the second roll weighs %d bytes, want it under the first's %d", second, first)
	}
	setRoom(first - 1)
	if report := pollWhileBusy(t, comp, rec); report.Compacted != 1 {
		t.Errorf("the pass over the next roll, %d bytes, with room for it and not for the first roll's %d "+
			"reported %+v, want it compacted: the repository's own outputs, stored, answered its hold", second,
			first, report)
	}
}
