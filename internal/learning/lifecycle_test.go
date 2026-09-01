package learning

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/store"
)

// t0 is the instant every test calls "now". Ages are counted back from it, so
// a row's eligibility is a property of the fixture rather than of the clock.
var t0 = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

func daysAgo(n int) time.Time { return t0.AddDate(0, 0, -n) }

func rawEp(id string, at time.Time, tools ...string) Episode {
	return Episode{
		ID: id, Handle: "ceo", Role: "CEO", TurnID: "turn-" + id,
		StartedAt: at.Add(-time.Minute), EndedAt: at,
		PlanSummary: "plan " + id, TaskSummary: "task " + id,
		ToolSequence: tools, ReviewOutcome: "done",
		Duration: 2 * time.Second, WorkKey: "wk-" + id,
	}
}

// fakeSummarizer stands in for the model. Every property of the pass —
// clustering, exemplar choice, the transaction, the recovery sweep — is
// reachable through it, which is the whole point of the interface.
type fakeSummarizer struct {
	mu    sync.Mutex
	seen  []Cluster
	reply func(Cluster) (Summary, error)
}

func (f *fakeSummarizer) Summarize(_ context.Context, c Cluster) (Summary, error) {
	f.mu.Lock()
	f.seen = append(f.seen, c)
	reply := f.reply
	f.mu.Unlock()
	if reply == nil {
		return Summary{
			CommonTaskPattern: "posted the weekly update",
			CommonOutcome:     "done",
			SubjectsInvolved:  []string{"finance"},
			NotablePatterns:   "two of them escalated",
		}, nil
	}
	return reply(c)
}

func (f *fakeSummarizer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen)
}

func openLearningDB(t *testing.T, opts ...func(*store.Options)) *store.DB {
	t.Helper()
	o := store.Options{}
	for _, fn := range opts {
		fn(&o)
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "l.db"), o)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newLife(t *testing.T, o Options, opts ...func(*store.Options)) (*Lifecycle, *Episodes, *fakeSummarizer) {
	t.Helper()
	db := openLearningDB(t, opts...)
	sum := &fakeSummarizer{}
	return NewLifecycle(db, sum, o), NewEpisodes(db), sum
}

func write(t *testing.T, e *Episodes, eps ...Episode) {
	t.Helper()
	for _, ep := range eps {
		wrote, err := e.Append(context.Background(), ep)
		if err != nil {
			t.Fatalf("Append(%s): %v", ep.ID, err)
		}
		if !wrote {
			t.Fatalf("Append(%s) was collapsed as a duplicate", ep.ID)
		}
	}
}

// snapshot returns every episode the seat holds, split by kind.
func snapshot(t *testing.T, e *Episodes) (rawRows, compacted []Episode) {
	t.Helper()
	all, err := e.Recent(context.Background(), "ceo", 1000)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	for _, ep := range all {
		if ep.Kind == KindCompacted {
			compacted = append(compacted, ep)
		} else {
			rawRows = append(rawRows, ep)
		}
	}
	return rawRows, compacted
}

func idsOf(eps []Episode) []string {
	out := make([]string, len(eps))
	for i, ep := range eps {
		out[i] = ep.ID
	}
	slices.Sort(out)
	return out
}

// cluster of six similar turns, oldest first, all past the compaction age.
func sixSimilar(e *Episodes, t *testing.T) {
	t.Helper()
	for i := range 6 {
		write(t, e, rawEp(fmt.Sprintf("s%d", i), daysAgo(60-i), "slack_post", "jira_get"))
	}
}

func mustPass(t *testing.T, l *Lifecycle) PassResult {
	t.Helper()
	res, err := l.Pass(context.Background(), "ceo", t0)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return res
}

// ---- the fold ---------------------------------------------------------- //

func TestAPassFoldsAClusterIntoOneRowAndKeepsItsExemplars(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{})
	sixSimilar(e, t)
	// A seventh turn doing entirely different work: the counterfactual for
	// every assertion below. If the pass folded on age alone rather than on
	// similarity, this would disappear with the rest.
	write(t, e, rawEp("other", daysAgo(55), "gitlab_mr"))

	res := mustPass(t, l)

	if res.ClustersCompacted != 1 || res.RawReplaced != 4 {
		t.Errorf("result = %+v, want 1 cluster and 4 rows replaced", res)
	}
	if sum.calls() != 1 {
		t.Fatalf("summarizer called %d times, want 1", sum.calls())
	}
	// The seat travels on the cluster, so a summarizer resolving a per-role
	// model or attributing token usage does not have to reach into element
	// zero and assume the members are homogeneous.
	if got := sum.seen[0]; got.Handle != "ceo" || got.Role != "CEO" {
		t.Errorf("cluster identity = %q/%q, want ceo/CEO", got.Handle, got.Role)
	}
	if want := []string{"s0", "s1", "s2", "s3", "s4", "s5"}; !slices.Equal(idsOf(sum.seen[0].Episodes), want) {
		t.Errorf("cluster members = %v, want %v", idsOf(sum.seen[0].Episodes), want)
	}
	rawRows, compacted := snapshot(t, e)
	if len(compacted) != 1 {
		t.Fatalf("compacted rows = %d, want 1", len(compacted))
	}
	c := compacted[0]
	if c.Count != 6 {
		t.Errorf("count = %d, want the 6 turns the cluster represents", c.Count)
	}
	if c.CommonTaskPattern != "posted the weekly update" || c.NotablePatterns != "two of them escalated" {
		t.Errorf("the summary did not reach the row: %+v", c)
	}
	if c.SuccessRate != 1 || c.ReviewOutcome != "done" {
		t.Errorf("success rate %v / outcome %q, want 1 and done", c.SuccessRate, c.ReviewOutcome)
	}
	if c.Duration != 12*time.Second {
		t.Errorf("duration = %v, want the sum of the six", c.Duration)
	}
	if !c.StartedAt.Equal(daysAgo(60).Add(-time.Minute)) || !c.EndedAt.Equal(daysAgo(55)) {
		t.Errorf("span = %v..%v, want the cluster's own", c.StartedAt, c.EndedAt)
	}
	// The exemplars are the two most recent, and they are still readable —
	// a summary whose drill-down anchors were deleted is a dead end.
	if want := []string{"s4", "s5"}; !slices.Equal(sortedClone(c.ExemplarTurnIDs), want) {
		t.Errorf("exemplars = %v, want the two most recent %v", c.ExemplarTurnIDs, want)
	}
	if want := []string{"other", "s4", "s5"}; !slices.Equal(idsOf(rawRows), want) {
		t.Errorf("surviving raw rows = %v, want %v", idsOf(rawRows), want)
	}
	if !strings.HasPrefix(c.WorkKey, "compact:") {
		t.Errorf("work key = %q, want the fold namespace", c.WorkKey)
	}
	if c.TurnID != "" {
		t.Errorf("turn id = %q, want empty: a summary is not a turn", c.TurnID)
	}
}

func TestTheOutcomeAndRateComeFromTheDataNotTheModel(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{})
	// Four of six failed. A model calling the cluster "done" must not make
	// it answer an outcome filter for successful work.
	for i := range 6 {
		ep := rawEp(fmt.Sprintf("s%d", i), daysAgo(60-i), "slack_post")
		if i < 4 {
			ep.ReviewOutcome = "failed"
		}
		write(t, e, ep)
	}
	sum.reply = func(Cluster) (Summary, error) {
		return Summary{CommonTaskPattern: "went great", CommonOutcome: "done"}, nil
	}

	mustPass(t, l)

	_, compacted := snapshot(t, e)
	c := compacted[0]
	if c.SuccessRate != 2.0/6.0 {
		t.Errorf("success rate = %v, want 2/6 from the members", c.SuccessRate)
	}
	if c.ReviewOutcome != "failed" {
		t.Errorf("review outcome = %q, want failed: 1/3 of the cluster succeeded", c.ReviewOutcome)
	}
	if c.CommonOutcome != "done" {
		t.Errorf("common outcome = %q, want the model's own word kept as prose", c.CommonOutcome)
	}
}

func TestACompactedRowIsNeverFoldedIntoAnother(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{})
	// Enough old summaries to clear the minimum cluster size, with the tool
	// sequence that would pool them if kind were not checked.
	for i := range 4 {
		ep := rawEp(fmt.Sprintf("c%d", i), daysAgo(70-i), "slack_post", "jira_get")
		ep.Kind, ep.Count = KindCompacted, 5
		ep.WorkKey = fmt.Sprintf("compact:%d", i)
		write(t, e, ep)
	}

	res := mustPass(t, l)

	if sum.calls() != 0 {
		t.Fatalf("summarizer called %d times over compacted rows", sum.calls())
	}
	if res.ClustersCompacted != 0 || res.RawReplaced != 0 {
		t.Errorf("result = %+v, want an untouched table", res)
	}
	_, compacted := snapshot(t, e)
	if len(compacted) != 4 {
		t.Errorf("compacted rows = %d, want the original 4", len(compacted))
	}
}

func TestASecondPassLeavesAFoldedClusterAlone(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{})
	sixSimilar(e, t)

	mustPass(t, l)
	before, _ := snapshot(t, e)
	res := mustPass(t, l)

	if sum.calls() != 1 {
		t.Errorf("summarizer called %d times over two passes, want 1", sum.calls())
	}
	if res.ClustersCompacted != 0 || res.OrphansDropped != 0 {
		t.Errorf("second pass = %+v, want it to find nothing", res)
	}
	after, compacted := snapshot(t, e)
	if len(compacted) != 1 {
		t.Fatalf("compacted rows = %d, want 1", len(compacted))
	}
	if !slices.Equal(idsOf(before), idsOf(after)) {
		t.Errorf("raw rows moved: %v -> %v", idsOf(before), idsOf(after))
	}
}

func TestExemplarsAreRetiredFromEveryLaterCluster(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{})
	sixSimilar(e, t)
	mustPass(t, l)
	_, compacted := snapshot(t, e)
	first := compacted[0]

	// Four newer turns doing the same work, now old enough to compact. The
	// two surviving exemplars match their tool sequence exactly, so nothing
	// but the retirement rule keeps them out of this cluster.
	for i := range 4 {
		write(t, e, rawEp(fmt.Sprintf("n%d", i), daysAgo(40-i), "slack_post", "jira_get"))
	}
	mustPass(t, l)

	if sum.calls() != 2 {
		t.Fatalf("summarizer called %d times, want 2", sum.calls())
	}
	second := sum.seen[1]
	if len(second.Episodes) != 4 {
		t.Errorf("second cluster held %d turns, want the 4 new ones (%v)",
			len(second.Episodes), idsOf(second.Episodes))
	}
	rawRows, all := snapshot(t, e)
	if len(all) != 2 {
		t.Fatalf("compacted rows = %d, want one per pass", len(all))
	}
	// The first summary's anchors still resolve. Without retirement they
	// would have been folded into the second cluster and deleted, leaving
	// the first summary pointing at rows that no longer exist.
	live := idsOf(rawRows)
	for _, id := range first.ExemplarTurnIDs {
		if !slices.Contains(live, id) {
			t.Errorf("exemplar %s of the first summary is gone; live rows are %v", id, live)
		}
	}
	for _, c := range all {
		if c.Count > 6 {
			t.Errorf("a summary counts %d turns, more than any cluster held", c.Count)
		}
	}
}

func TestARetiredExemplarIsKeptEvenInsideAnotherSummarysSpan(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{})
	sixSimilar(e, t)
	mustPass(t, l)
	_, compacted := snapshot(t, e)
	kept := sortedClone(compacted[0].ExemplarTurnIDs)

	// A second summary claiming a WIDER span of the same work — what an
	// interleaved fold, or a re-fold after an eviction, leaves behind. Its
	// window contains the first summary's anchors and its tool shape matches
	// them, so every coverage test says "orphan" and only the retirement
	// check says "keep". Anchors come back in whatever order the database
	// chooses, so this must not depend on which of the two is seen first.
	wider := rawEp("wider", daysAgo(40), "slack_post", "jira_get")
	wider.StartedAt = daysAgo(70)
	wider.Kind, wider.Count, wider.WorkKey = KindCompacted, 5, "compact:wider"
	write(t, e, wider)

	res := mustPass(t, l)

	if res.OrphansDropped != 0 {
		t.Errorf("orphans dropped = %d, want the anchors kept", res.OrphansDropped)
	}
	rawRows, _ := snapshot(t, e)
	if !slices.Equal(idsOf(rawRows), kept) {
		t.Errorf("raw rows = %v, want the retained exemplars %v", idsOf(rawRows), kept)
	}
}

// ---- crash recovery ---------------------------------------------------- //

func TestASummaryWithoutItsDeletesIsRecoveredNotDoubleCounted(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{})
	sixSimilar(e, t)
	mustPass(t, l)

	// The state the transaction below is meant to make unreachable: the
	// summary committed, its members did not go away. Reached here by
	// putting them back, which is what a restore from a backup taken
	// mid-pass does.
	for i := range 4 {
		write(t, e, rawEp(fmt.Sprintf("s%d", i), daysAgo(60-i), "slack_post", "jira_get"))
	}
	res := mustPass(t, l)

	if res.OrphansDropped != 4 {
		t.Errorf("orphans dropped = %d, want the 4 restored rows", res.OrphansDropped)
	}
	if res.ClustersCompacted != 0 || sum.calls() != 1 {
		t.Errorf("a second summary was written over the same turns: %+v, %d calls",
			res, sum.calls())
	}
	rawRows, compacted := snapshot(t, e)
	if len(compacted) != 1 {
		t.Fatalf("compacted rows = %d, want the one that already covered them", len(compacted))
	}
	if compacted[0].Count != 6 {
		t.Errorf("count = %d, want the original 6 — the turns happened once",
			compacted[0].Count)
	}
	if want := []string{"s4", "s5"}; !slices.Equal(idsOf(rawRows), want) {
		t.Errorf("raw rows = %v, want only the exemplars %v", idsOf(rawRows), want)
	}
}

func TestTheOrphanSweepOnlyTakesWhatASummaryActuallyCovers(t *testing.T) {
	t.Parallel()
	// A minimum cluster of four, so the survivors below cannot pool into a
	// fold of their own and disappear that way instead.
	l, e, sum := newLife(t, Options{MinClusterSize: 4})
	sixSimilar(e, t)
	mustPass(t, l)
	_, compacted := snapshot(t, e)
	span := compacted[0]

	// Four rows the sweep must not touch, each failing exactly one of the
	// conditions that make a row an orphan.
	inWindowOtherTools := rawEp("wrong-tools", daysAgo(58), "gitlab_mr", "gitlab_note")
	onTheBoundary := rawEp("boundary", span.EndedAt, "slack_post", "jira_get")
	afterTheWindow := rawEp("after", daysAgo(50), "slack_post", "jira_get")
	beforeTheWindow := rawEp("before", daysAgo(70), "slack_post", "jira_get")
	write(t, e, inWindowOtherTools, onTheBoundary, afterTheWindow, beforeTheWindow)

	res := mustPass(t, l)

	if res.OrphansDropped != 0 {
		t.Errorf("orphans dropped = %d, want none of the four", res.OrphansDropped)
	}
	if sum.calls() != 1 {
		t.Errorf("summarizer called %d times, want 1: the survivors cannot make a "+
			"cluster of four", sum.calls())
	}
	rawRows, _ := snapshot(t, e)
	for _, id := range []string{"wrong-tools", "boundary", "after", "before"} {
		if !slices.Contains(idsOf(rawRows), id) {
			t.Errorf("%s was swept; surviving rows are %v", id, idsOf(rawRows))
		}
	}
}

func TestOnlySummariesActAsAnchors(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{})
	// Two turns of the same work minutes apart, which is ordinary for a busy
	// seat. Each raw row's span is its own minute, so without the kind filter
	// on the anchor query the earlier-ending one falls inside the later one's
	// window with an identical tool sequence — and is swept as an orphan of a
	// summary that does not exist. Nothing has been folded here at all.
	at := daysAgo(60)
	write(t, e,
		rawEp("early", at.Add(-10*time.Second), "slack_post", "jira_get"),
		rawEp("late", at, "slack_post", "jira_get"))

	res := mustPass(t, l)

	if res.OrphansDropped != 0 {
		t.Errorf("orphans dropped = %d with no summary in the table", res.OrphansDropped)
	}
	if rawRows, _ := snapshot(t, e); len(rawRows) != 2 {
		t.Errorf("raw rows = %v, want both turns kept", idsOf(rawRows))
	}
}

func TestAnotherSeatsSummaryIsNotAnAnchorForThisOne(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{})
	// A colleague folded the same kind of work over the same weeks. Its
	// summary counts ITS turns; ours are covered by nothing, and sweeping
	// them against it would delete a seat's memory on the strength of another
	// seat's fold.
	theirs := rawEp("theirs", daysAgo(40), "slack_post", "jira_get")
	theirs.Handle, theirs.WorkKey = "cfo", "compact:theirs"
	theirs.StartedAt = daysAgo(70)
	theirs.Kind, theirs.Count = KindCompacted, 8
	write(t, e, theirs)
	write(t, e,
		rawEp("mine-a", daysAgo(60), "slack_post", "jira_get"),
		rawEp("mine-b", daysAgo(59), "slack_post", "jira_get"))

	res := mustPass(t, l)

	if res.OrphansDropped != 0 {
		t.Errorf("orphans dropped = %d against another seat's summary", res.OrphansDropped)
	}
	if rawRows, _ := snapshot(t, e); len(rawRows) != 2 {
		t.Errorf("raw rows = %v, want both of this seat's turns kept", idsOf(rawRows))
	}
}

func TestAFailedDeleteTakesTheSummaryWithIt(t *testing.T) {
	t.Parallel()
	fault := &compactionFault{}
	l, e, sum := newLife(t, Options{}, func(o *store.Options) { o.WrapDriver = fault.wrap })
	sixSimilar(e, t)
	// A mid-state row the first sweep removes before the fold fails, so the
	// result carries something real to lose.
	iterating := rawEp("iterating", daysAgo(20), "slack_post")
	iterating.ReviewOutcome = "self_iterate"
	write(t, e, iterating)

	boom := errors.New("disk gave up mid-fold")
	fault.failOn("AND id IN (", boom)
	res, err := l.Pass(context.Background(), "ceo", t0)
	fault.disarm()

	if !errors.Is(err, boom) {
		t.Fatalf("Pass error = %v, want the injected %v", err, boom)
	}
	if res.ClustersCompacted != 0 || res.RawReplaced != 0 {
		t.Errorf("result = %+v, want nothing claimed by the fold", res)
	}
	// The sweeps that DID commit come back with the error. A caller that got
	// a zeroed result here would publish a completion event saying the pass
	// removed nothing, over rows that are gone.
	if res.NonTerminalDropped != 1 {
		t.Errorf("non-terminal dropped = %d, want the sweep that landed before "+
			"the failure", res.NonTerminalDropped)
	}
	if sum.calls() != 1 {
		t.Errorf("summarizer called %d times, want the one call that was spent", sum.calls())
	}
	// The claim being measured: the summary and the deletes commit together,
	// so the state the orphan sweep exists to recover is not reachable
	// through this code path.
	rawRows, compacted := snapshot(t, e)
	if len(compacted) != 0 {
		t.Errorf("a summary survived a failed delete: %+v", compacted)
	}
	if len(rawRows) != 6 {
		t.Errorf("raw rows = %d, want all 6 still there", len(rawRows))
	}
}

func TestAPassThatFindsNothingOpensNoTransaction(t *testing.T) {
	t.Parallel()
	fault := &compactionFault{}
	l, e, _ := newLife(t, Options{}, func(o *store.Options) { o.WrapDriver = fault.wrap })
	// Two similar old turns: past the age, under the minimum cluster size,
	// and with no summary anywhere to orphan them against.
	write(t, e, rawEp("s0", daysAgo(60), "slack_post"), rawEp("s1", daysAgo(59), "slack_post"))

	fault.reset()
	if res := mustPass(t, l); res != (PassResult{}) {
		t.Fatalf("quiet pass = %+v, want nothing done", res)
	}
	// The store has ONE writer. A pass over a seat with nothing to do runs on
	// every threshold crossing, and opening a transaction to delete an empty
	// list of rows takes that writer from whatever turn is trying to use it.
	if got := fault.begun(); got != 0 {
		t.Errorf("a pass that found nothing opened %d transactions", got)
	}

	for i := 2; i < 6; i++ {
		write(t, e, rawEp(fmt.Sprintf("s%d", i), daysAgo(60-i), "slack_post"))
	}
	fault.reset()
	mustPass(t, l)
	if got := fault.begun(); got != 1 {
		t.Errorf("a pass that folded one cluster opened %d transactions, want 1", got)
	}
}

func TestTwoNodesFoldingOneSeatWriteOneSummary(t *testing.T) {
	t.Parallel()
	db := openLearningDB(t)
	e := NewEpisodes(db)
	sixSimilar(e, t)
	// Two Lifecycles, because the single-flight map is per process: this is
	// the cross-node race, where the only guard left is the fold key on the
	// unique index.
	sumA, sumB := &fakeSummarizer{}, &fakeSummarizer{}
	a := NewLifecycle(db, sumA, Options{})
	b := NewLifecycle(db, sumB, Options{})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, l := range []*Lifecycle{a, b} {
		wg.Go(func() {
			_, errs[i] = l.Pass(context.Background(), "ceo", t0)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}

	rawRows, compacted := snapshot(t, e)
	if len(compacted) != 1 {
		t.Fatalf("compacted rows = %d, want exactly one for the one cluster", len(compacted))
	}
	if compacted[0].Count != 6 {
		t.Errorf("count = %d, want 6", compacted[0].Count)
	}
	if len(rawRows) != 2 {
		t.Errorf("raw rows = %d, want the two exemplars", len(rawRows))
	}
}

func TestAPassInFlightIsRefusedRatherThanQueued(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{})
	sixSimilar(e, t)

	inside, release := make(chan struct{}), make(chan struct{})
	sum.reply = func(Cluster) (Summary, error) {
		close(inside)
		<-release
		return Summary{CommonTaskPattern: "held"}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := l.Pass(context.Background(), "ceo", t0)
		done <- err
	}()
	<-inside

	if _, err := l.Pass(context.Background(), "ceo", t0); !errors.Is(err, ErrPassInFlight) {
		t.Errorf("concurrent pass error = %v, want ErrPassInFlight", err)
	}
	// A different seat is not blocked by this one: the guard is per seat,
	// and a company-wide one would serialise every seat behind the slowest.
	if _, err := l.Pass(context.Background(), "cfo", t0); err != nil {
		t.Errorf("pass for another seat: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("held pass: %v", err)
	}
	// And the claim is released, so the seat is workable again.
	if _, err := l.Pass(context.Background(), "ceo", t0); err != nil {
		t.Errorf("pass after the first finished: %v", err)
	}
	if rawRows, _ := snapshot(t, e); len(rawRows) != 2 {
		t.Errorf("raw rows = %d, want the fold that was held to have landed once", len(rawRows))
	}
}

func TestASummarizerFailureCostsOneClusterNotThePass(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{})
	sixSimilar(e, t)
	for i := range 4 {
		write(t, e, rawEp(fmt.Sprintf("g%d", i), daysAgo(52-i), "gitlab_mr", "gitlab_note"))
	}
	sum.reply = func(c Cluster) (Summary, error) {
		if slices.Contains(c.Episodes[0].ToolSequence, "slack_post") {
			return Summary{}, errors.New("model is down")
		}
		return Summary{CommonTaskPattern: "reviewed merge requests"}, nil
	}

	res := mustPass(t, l)

	if res.SummarizerFailures != 1 {
		t.Errorf("summarizer failures = %d, want 1", res.SummarizerFailures)
	}
	if res.ClustersCompacted != 1 || res.RawReplaced != 2 {
		t.Errorf("result = %+v, want the other cluster folded anyway", res)
	}
	rawRows, compacted := snapshot(t, e)
	if len(compacted) != 1 {
		t.Fatalf("compacted rows = %d, want 1", len(compacted))
	}
	// The failed cluster's rows are all still there, so the next pass can
	// try again — nothing is lost but the call.
	for i := range 6 {
		if !slices.Contains(idsOf(rawRows), fmt.Sprintf("s%d", i)) {
			t.Errorf("s%d was lost by the failed cluster; rows are %v", i, idsOf(rawRows))
		}
	}
}

func TestTheFoldKeyNamesTheMemberSetAndNothingElse(t *testing.T) {
	t.Parallel()
	members := []string{"c", "a", "b"}
	key := foldKey(members)
	if key == "" || !strings.HasPrefix(key, "compact:") {
		t.Fatalf("foldKey = %q, want a key in the fold namespace", key)
	}
	// Order-independent, because two nodes clustering one batch can pool the
	// same members in different orders and must still collide on the unique
	// index rather than each writing a summary.
	if got := foldKey([]string{"b", "c", "a"}); got != key {
		t.Errorf("foldKey is order-dependent: %q vs %q", got, key)
	}
	if got := foldKey([]string{"a", "b", "c", "c"}); got != key {
		t.Errorf("a repeated member changed the key: %q vs %q", got, key)
	}
	if got := foldKey([]string{"a", "b"}); got == key {
		t.Error("a different member set produced the same key")
	}
	// The namespace prefix is what keeps a fold out of the turn key space:
	// a turn's key is 32 hex characters, so no turn can mint this one.
	if strings.TrimPrefix(key, "compact:") == key {
		t.Error("the fold key is indistinguishable from a turn's work key")
	}
	// Empty answers empty rather than "compact:" — a bare prefix would be
	// one shared key that every empty fold collided on.
	if got := foldKey(nil); got != "" {
		t.Errorf("foldKey(nil) = %q, want empty", got)
	}
}

func TestKeepingMoreExemplarsThanTheClusterHasStillRemovesOne(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{MinClusterSize: 3, ExemplarCount: 9})
	for i := range 3 {
		write(t, e, rawEp(fmt.Sprintf("s%d", i), daysAgo(60-i), "slack_post"))
	}

	res := mustPass(t, l)

	// splitExemplars clamps to len-1, so one row always goes: the fold is
	// still worth its summary. What must never happen is a summary written
	// over a cluster where nothing was removed.
	if res.ClustersCompacted != 1 || res.RawReplaced != 1 {
		t.Errorf("result = %+v, want one row replaced", res)
	}
	if sum.calls() != 1 {
		t.Errorf("summarizer calls = %d, want 1", sum.calls())
	}
	rawRows, _ := snapshot(t, e)
	if len(rawRows) != 2 {
		t.Errorf("raw rows = %d, want the two exemplars the clamp allowed", len(rawRows))
	}
}

// ---- the cheap sweeps -------------------------------------------------- //

func TestMidStateRowsAreDroppedAndTerminalOnesAreNot(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{})

	iterating := rawEp("iterating", daysAgo(20), "slack_post")
	iterating.ReviewOutcome = "self_iterate"
	// An outcome outside the Review enum: written by another version, or by
	// hand. Naming 'self_iterate' in the sweep while allowing only
	// ('done','failed') into compaction leaves a row like this neither
	// droppable nor compactable, and it stays forever.
	unknown := rawEp("unknown", daysAgo(20), "slack_post")
	unknown.ReviewOutcome = "cancelled"
	fresh := rawEp("fresh", daysAgo(3), "slack_post")
	fresh.ReviewOutcome = "self_iterate"
	failed := rawEp("failed", daysAgo(20), "slack_post")
	failed.ReviewOutcome = "failed"
	// A summary must never be reachable by a raw-row sweep, whatever its
	// outcome column says.
	summary := rawEp("summary", daysAgo(20), "slack_post")
	summary.Kind, summary.Count, summary.ReviewOutcome = KindCompacted, 5, "self_iterate"
	write(t, e, iterating, unknown, fresh, failed, summary)

	res := mustPass(t, l)

	if res.NonTerminalDropped != 2 {
		t.Errorf("dropped %d mid-state rows, want the two past 14 days", res.NonTerminalDropped)
	}
	rawRows, compacted := snapshot(t, e)
	if want := []string{"failed", "fresh"}; !slices.Equal(idsOf(rawRows), want) {
		t.Errorf("surviving raw rows = %v, want %v", idsOf(rawRows), want)
	}
	if len(compacted) != 1 {
		t.Errorf("the summary was swept by a raw-row sweep")
	}
}

func TestTheConsolidationGraceIsMeasuredFromTheRowsEnd(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{ConsolidatedGrace: 30 * day})
	cutoff := t0.Add(-30 * day)

	past := rawEp("past", cutoff.Add(-time.Microsecond), "slack_post")
	onCutoff := rawEp("on-cutoff", cutoff, "slack_post")
	inside := rawEp("inside", cutoff.Add(time.Microsecond), "slack_post")
	// Same age, never consolidated: the counterfactual for the stamp being
	// what selects a row rather than its age.
	unstamped := rawEp("unstamped", cutoff.Add(-time.Hour), "slack_post")
	write(t, e, past, onCutoff, inside, unstamped)

	stamped, err := l.MarkConsolidated(context.Background(), "ceo", "skill-1",
		[]string{"past", "on-cutoff", "inside"})
	if err != nil {
		t.Fatalf("MarkConsolidated: %v", err)
	}
	if stamped != 3 {
		t.Fatalf("stamped %d rows, want 3", stamped)
	}

	res := mustPass(t, l)

	if res.ConsolidatedDropped != 1 {
		t.Errorf("dropped %d, want only the row strictly past the grace", res.ConsolidatedDropped)
	}
	rawRows, _ := snapshot(t, e)
	// The boundary is EXCLUSIVE: `ended_at < cutoff`, so a row landing
	// exactly on it survives one more pass.
	if want := []string{"inside", "on-cutoff", "unstamped"}; !slices.Equal(idsOf(rawRows), want) {
		t.Errorf("surviving rows = %v, want %v", idsOf(rawRows), want)
	}
}

func TestAConsolidationStampLandsOnceAndOnlyOnRawRows(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{})
	one := rawEp("one", daysAgo(5), "slack_post")
	summary := rawEp("summary", daysAgo(5), "slack_post")
	summary.Kind, summary.Count = KindCompacted, 4
	write(t, e, one, summary)

	ctx := context.Background()
	if n, err := l.MarkConsolidated(ctx, "ceo", "skill-1", []string{"one", "summary"}); err != nil || n != 1 {
		t.Fatalf("first stamp = %d, %v; want 1 row and no error (a summary is not absorbable)", n, err)
	}
	// A second skill drafting from the same turn must not repoint the audit
	// trail at itself.
	if n, err := l.MarkConsolidated(ctx, "ceo", "skill-2", []string{"one"}); err != nil || n != 0 {
		t.Fatalf("second stamp = %d, %v; want 0 rows and no error", n, err)
	}
	rows, _ := snapshot(t, e)
	for _, ep := range rows {
		if ep.ID == "one" && ep.ConsolidatedInto != "skill-1" {
			t.Errorf("stamp = %q, want the first skill to keep it", ep.ConsolidatedInto)
		}
	}
	if _, err := l.MarkConsolidated(ctx, "ceo", "", []string{"one"}); err == nil {
		t.Error("a stamp with no skill was accepted")
	}
	if _, err := l.MarkConsolidated(ctx, "", "skill-3", []string{"one"}); err == nil {
		t.Error("a stamp with no seat was accepted")
	}
	if n, err := l.MarkConsolidated(ctx, "ceo", "skill-3", nil); err != nil || n != 0 {
		t.Errorf("empty id list = %d, %v; want a no-op", n, err)
	}
	// A stamp is a deletion on a 30-day fuse, so another seat's id must land
	// nowhere rather than schedule that seat's turn for removal.
	other := rawEp("theirs", daysAgo(5), "slack_post")
	other.Handle, other.WorkKey = "cfo", "wk-theirs"
	write(t, e, other)
	if n, err := l.MarkConsolidated(ctx, "ceo", "skill-4", []string{"theirs"}); err != nil || n != 0 {
		t.Errorf("cross-seat stamp = %d, %v; want it to land nowhere", n, err)
	}
}

func TestEvictionTakesASummaryAndTheAnchorsItLeftBehind(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{CompactedMaxAge: 50 * day})
	sixSimilar(e, t)
	// A summary with NO anchors at all — one written before exemplars were
	// kept, or by an import. Evicting it must not turn into an `id IN ()`,
	// which is a syntax error that would take the whole sweep with it.
	anchorless := rawEp("anchorless", daysAgo(80), "confluence_page")
	anchorless.Kind, anchorless.Count, anchorless.WorkKey = KindCompacted, 3, "compact:old"
	write(t, e, anchorless)

	// The fold's summary carries the cluster's own ended_at — 55 days back,
	// already past the 50-day horizon. Eviction runs BEFORE the fold, so it
	// survives this pass and goes on the next one; evicting afterwards would
	// have deleted it in the same pass that paid for it.
	first := mustPass(t, l)
	if first.CompactedEvicted != 1 || first.ExemplarsEvicted != 0 {
		t.Fatalf("first pass = %+v, want only the anchorless summary evicted", first)
	}
	if first.ClustersCompacted != 1 {
		t.Fatalf("first pass = %+v, want the fold to have landed", first)
	}

	// A younger cluster, folded by the same pass configuration, that the
	// eviction cutoff must not reach.
	for i := range 4 {
		write(t, e, rawEp(fmt.Sprintf("n%d", i), daysAgo(40-i), "gitlab_mr"))
	}
	res := mustPass(t, l)

	if res.CompactedEvicted != 1 || res.ExemplarsEvicted != 2 {
		t.Errorf("eviction = %+v, want the old summary and its two anchors", res)
	}
	rawRows, compacted := snapshot(t, e)
	if len(compacted) != 1 || compacted[0].Count != 4 {
		t.Fatalf("compacted rows = %+v, want only the younger cluster", compacted)
	}
	// The exemplars are the one kind of raw row no other sweep can reach:
	// retired from compaction, terminal, unstamped. Leaving them behind
	// makes the hard-storage-cap knob a slow leak.
	if want := []string{"n2", "n3"}; !slices.Equal(idsOf(rawRows), want) {
		t.Errorf("raw rows = %v, want only the younger cluster's anchors %v",
			idsOf(rawRows), want)
	}
}

func TestEvictionIsOffByDefault(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{})
	sixSimilar(e, t)
	mustPass(t, l)

	// Ten years on, with no CompactedMaxAge configured, the summary is still
	// there: losing long-horizon signal is not something a default does.
	res, err := l.Pass(context.Background(), "ceo", t0.AddDate(10, 0, 0))
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if res.CompactedEvicted != 0 {
		t.Errorf("evicted %d summaries with the knob at zero", res.CompactedEvicted)
	}
	if _, compacted := snapshot(t, e); len(compacted) != 1 {
		t.Errorf("compacted rows = %d, want the summary kept", len(compacted))
	}
}

// ---- the trigger ------------------------------------------------------- //

func TestRawCountAnswersTheThresholdQuestion(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{Threshold: 3})
	ctx := context.Background()

	write(t, e, rawEp("a", daysAgo(1), "x"), rawEp("b", daysAgo(1), "x"))
	summary := rawEp("c", daysAgo(1), "x")
	summary.Kind, summary.Count = KindCompacted, 9
	write(t, e, summary)

	// The summary counts for nothing here: the threshold is about how many
	// rows recall has to scan per turn, and folding is what reduces it.
	n, due, err := l.RawCount(ctx, "ceo")
	if err != nil || n != 2 || due {
		t.Errorf("RawCount = %d, %v, %v; want 2, false, nil", n, due, err)
	}
	write(t, e, rawEp("d", daysAgo(1), "x"))
	if n, due, _ := l.RawCount(ctx, "ceo"); n != 3 || !due {
		t.Errorf("RawCount at the threshold = %d, %v; want 3, true", n, due)
	}
	if _, _, err := l.RawCount(ctx, ""); err == nil {
		t.Error("a count with no seat was accepted")
	}
	if _, _, err := l.RawCount(ctx, "nobody"); err != nil {
		t.Errorf("a seat with no rows: %v, want a zero count and no error", err)
	}
}

func TestAPassNeedsASeat(t *testing.T) {
	t.Parallel()
	l, _, _ := newLife(t, Options{})
	if _, err := l.Pass(context.Background(), "", t0); err == nil {
		t.Error("a pass over every seat at once was accepted")
	}
}

// ---- pure parts -------------------------------------------------------- //

func TestExemplarChoiceIsTheMostRecentAndTotallyOrdered(t *testing.T) {
	t.Parallel()
	at := t0.Add(-time.Hour)
	// Three of the four share an instant, so the id tie-break is the only
	// thing that makes the choice repeatable. Two nodes that kept different
	// exemplars would derive different fold keys and each write a summary.
	cluster := []Episode{
		{ID: "a", EndedAt: at},
		{ID: "z", EndedAt: at},
		{ID: "m", EndedAt: at},
		{ID: "old", EndedAt: at.Add(-time.Hour)},
	}
	rng := rand.New(rand.NewPCG(3, 4))
	for range 25 {
		perm := slices.Clone(cluster)
		rng.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
		exemplars, doomed := splitExemplars(perm, 2)
		if want := []string{"m", "z"}; !slices.Equal(sortedClone(idsOf(exemplars)), want) {
			t.Fatalf("exemplars = %v, want %v", idsOf(exemplars), want)
		}
		if want := []string{"a", "old"}; !slices.Equal(idsOf(doomed), want) {
			t.Fatalf("doomed = %v, want %v", idsOf(doomed), want)
		}
		// The doomed keep the cluster's own order, which is what the delete
		// list and the key are built from.
		if len(exemplars)+len(doomed) != len(perm) {
			t.Fatalf("split lost a member: %d + %d != %d", len(exemplars), len(doomed), len(perm))
		}
	}

	// Asking for more exemplars than the cluster can spare still leaves one
	// row to remove; otherwise a fold writes a summary and frees nothing.
	exemplars, doomed := splitExemplars(cluster, 99)
	if len(exemplars) != 3 || len(doomed) != 1 {
		t.Errorf("clamped split = %d kept, %d doomed; want 3 and 1", len(exemplars), len(doomed))
	}
	if exemplars, doomed := splitExemplars(cluster, 0); len(exemplars) != 0 || len(doomed) != 4 {
		t.Errorf("zero exemplars = %d kept, %d doomed; want 0 and 4", len(exemplars), len(doomed))
	}
	if exemplars, _ := splitExemplars(cluster, -3); len(exemplars) != 0 {
		t.Errorf("a negative exemplar count kept %d rows", len(exemplars))
	}
}

func TestToolJaccardIsSetOverlap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		a, b []string
		want float64
	}{
		{"identical", []string{"x", "y"}, []string{"x", "y"}, 1},
		{"disjoint", []string{"x"}, []string{"y"}, 0},
		{"half", []string{"x", "y"}, []string{"y", "z"}, 1.0 / 3.0},
		{"order is not part of it", []string{"y", "x"}, []string{"x", "y"}, 1},
		{"repeats collapse", []string{"x", "x", "x", "y"}, []string{"y", "x"}, 1},
		{"subset", []string{"x"}, []string{"x", "y", "z"}, 1.0 / 3.0},
		// Two turns that called nothing have nothing in common that this
		// function can see. Scoring them 1 would pool every tool-free turn
		// a seat ever ran into one cluster.
		{"both empty", nil, nil, 0},
		{"one empty", []string{"x"}, nil, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := toolJaccard(c.a, c.b); got != c.want {
				t.Errorf("toolJaccard(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
			if got := toolJaccard(c.b, c.a); got != c.want {
				t.Errorf("toolJaccard is not symmetric: %v vs %v", got, c.want)
			}
		})
	}
}

func TestClusteringIsGreedyAgainstTheRepresentative(t *testing.T) {
	t.Parallel()
	ep := func(id string, tools ...string) Episode {
		return Episode{ID: id, ToolSequence: tools}
	}
	// b matches a at 1/2 and c at 1/1, but a comes first, so b joins a and
	// c never sees it. Greedy against the FIRST member is what makes the
	// result depend on the input order — and what makes the candidate
	// query's total order load bearing.
	got := clusterByTools([]Episode{
		ep("a", "x", "y"), ep("b", "x"), ep("c", "x"),
	}, 0.4)
	if len(got) != 1 || len(got[0]) != 3 {
		t.Errorf("clusters = %v, want all three pooled", clusterIDs(got))
	}
	got = clusterByTools([]Episode{
		ep("a", "x", "y"), ep("b", "x"), ep("c", "x"),
	}, 0.6)
	if len(got) != 2 || len(got[0]) != 1 || len(got[1]) != 2 {
		t.Errorf("clusters at a stricter threshold = %v, want a alone and b+c", clusterIDs(got))
	}
	// NO CHAINING. a~b and b~c, but a and c share nothing. Comparing each
	// candidate against the cluster's LAST member instead of its first would
	// pool all three, and the summary's tool_sequence — which is taken from
	// the representative — would then describe a shape a third of its members
	// never had. Found by mutation: swapping cluster[0] for the last member
	// changed nothing any other case could see.
	got = clusterByTools([]Episode{
		ep("a", "x", "y"), ep("b", "y", "z"), ep("c", "z", "w"),
	}, 1.0/3.0)
	if len(got) != 2 || !slices.Equal(got[0][0].ToolSequence, []string{"x", "y"}) {
		t.Fatalf("clusters = %v, want b pooled with a and c on its own", clusterIDs(got))
	}
	if !slices.Equal(idsOf(got[0]), []string{"a", "b"}) || !slices.Equal(idsOf(got[1]), []string{"c"}) {
		t.Errorf("clusters chained: %v", clusterIDs(got))
	}

	// A turn that called no tools cannot be scored, so it is skipped
	// entirely — and therefore never compacted. See the package finding.
	got = clusterByTools([]Episode{ep("none"), ep("also-none"), ep("third")}, 0.6)
	if len(got) != 0 {
		t.Errorf("tool-free turns clustered into %v, want none", clusterIDs(got))
	}
}

func TestTheCandidateBatchHasATotalOrder(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{})
	at := daysAgo(60)
	// Written newest-first and out of id order, so neither insertion order
	// nor the ended_at column alone produces the wanted sequence.
	write(t, e,
		rawEp("m", at, "x"), rawEp("a", at, "x"), rawEp("z", at, "x"),
		rawEp("newer", at.Add(time.Hour), "x"))

	got, err := l.candidates(context.Background(), "ceo", t0.Add(-defaultMinAge))
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	want := []string{"a", "m", "z", "newer"}
	var order []string
	for _, ep := range got {
		order = append(order, ep.ID)
	}
	if !slices.Equal(order, want) {
		t.Errorf("candidate order = %v, want %v", order, want)
	}
}

func TestCandidatesExcludeEveryRowASweepOwns(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{})
	ctx := context.Background()

	tooNew := rawEp("too-new", daysAgo(29), "x")
	old := rawEp("old", daysAgo(31), "x")
	midState := rawEp("mid", daysAgo(31), "x")
	midState.ReviewOutcome = "self_iterate"
	stamped := rawEp("stamped", daysAgo(31), "x")
	summary := rawEp("summary", daysAgo(31), "x")
	summary.Kind, summary.Count = KindCompacted, 3
	write(t, e, tooNew, old, midState, stamped, summary)
	if _, err := l.MarkConsolidated(ctx, "ceo", "skill-1", []string{"stamped"}); err != nil {
		t.Fatalf("MarkConsolidated: %v", err)
	}

	got, err := l.candidates(ctx, "ceo", t0.Add(-defaultMinAge))
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	if want := []string{"old"}; !slices.Equal(idsOf(got), want) {
		t.Errorf("candidates = %v, want %v", idsOf(got), want)
	}
}

func TestDeletingEpisodesIsScopedToTheSeat(t *testing.T) {
	t.Parallel()
	l, e, _ := newLife(t, Options{})
	mine := rawEp("mine", daysAgo(1), "x")
	theirs := rawEp("theirs", daysAgo(1), "x")
	theirs.Handle, theirs.WorkKey = "cfo", "wk-theirs"
	write(t, e, mine, theirs)

	// The id lists that reach this helper are assembled in Go across three
	// queries. One clause standing between a bad list and another seat's
	// memory is what stops a bug there from being unrecoverable.
	n, err := l.tx(context.Background(), "test delete", func(tx *sql.Tx) (int64, error) {
		return deleteEpisodes(context.Background(), tx, "ceo", []string{"mine", "theirs"})
	})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want only this seat's", n)
	}
	got, err := e.Recent(context.Background(), "cfo", 10)
	if err != nil || len(got) != 1 {
		t.Errorf("the other seat's row = %d rows, %v; want it untouched", len(got), err)
	}
}

func TestACompactedRowRoundTripsThroughTheEpisodeScanner(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{}, func(o *store.Options) { o.EmbeddingDim = 4 })
	sixSimilar(e, t)
	sum.reply = func(Cluster) (Summary, error) {
		return Summary{
			CommonTaskPattern: "posted the weekly update",
			CommonOutcome:     "done",
			SubjectsInvolved:  []string{"finance", "legal"},
			NotablePatterns:   "escalated twice",
			Embedding:         []float32{0.1, 0.2, 0.3, 0.4},
		}, nil
	}
	mustPass(t, l)

	// The summary is written through the same statement Append binds, in a
	// hand-written argument list. A misordered argument lands values in the
	// wrong columns, which nothing but a full round trip notices.
	_, compacted := snapshot(t, e)
	c := compacted[0]
	if c.Handle != "ceo" || c.Role != "CEO" || c.Kind != KindCompacted || c.Count != 6 {
		t.Errorf("identity fields: %+v", c)
	}
	if !slices.Equal(c.ToolSequence, []string{"slack_post", "jira_get"}) {
		t.Errorf("tool sequence = %v", c.ToolSequence)
	}
	if !slices.Equal(c.SubjectsInvolved, []string{"finance", "legal"}) {
		t.Errorf("subjects = %v", c.SubjectsInvolved)
	}
	if c.NotablePatterns != "escalated twice" || c.CommonOutcome != "done" {
		t.Errorf("prose fields: %+v", c)
	}
	if len(c.Embedding) != 4 || c.Embedding[3] != 0.4 {
		t.Errorf("embedding = %v", c.Embedding)
	}
	if c.ConsolidatedInto != "" || c.ConversationKey != "" {
		t.Errorf("a summary spans conversations and belongs to no skill: %+v", c)
	}
	if len(c.SkillsUsed) != 0 || c.TaskSummary != "" || c.PlanSummary != "" {
		t.Errorf("per-turn fields leaked onto a summary: %+v", c)
	}
	// And it is recallable, which is the only reason the vector is carried.
	hits, err := e.Recall(context.Background(), RecallQuery{
		Handle: "ceo", Embedding: []float32{0.1, 0.2, 0.3, 0.4},
		Kinds: []Kind{KindCompacted},
	})
	if err != nil || len(hits) != 1 || hits[0].Episode.ID != c.ID {
		t.Errorf("recall over summaries = %d hits, %v", len(hits), err)
	}
}

func TestASummaryLandsEvenWhenItsVectorCannot(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{}, func(o *store.Options) { o.EmbeddingDim = 4 })
	sixSimilar(e, t)
	sum.reply = func(Cluster) (Summary, error) {
		// The company changed embedding model between the turns and the
		// fold. The summary is what the call bought; refusing the row would
		// spend it again next pass and fail the same way.
		return Summary{CommonTaskPattern: "still useful", Embedding: []float32{1, 2}}, nil
	}

	res := mustPass(t, l)

	if res.ClustersCompacted != 1 {
		t.Fatalf("result = %+v, want the fold to land", res)
	}
	_, compacted := snapshot(t, e)
	if len(compacted) != 1 || compacted[0].CommonTaskPattern != "still useful" {
		t.Fatalf("summary = %+v", compacted)
	}
	if compacted[0].Embedding != nil {
		t.Errorf("embedding = %v, want none stored", compacted[0].Embedding)
	}
}

func TestAnEmptyPatternIsLabelledRatherThanLeftBlank(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{})
	sixSimilar(e, t)
	sum.reply = func(Cluster) (Summary, error) { return Summary{CommonTaskPattern: "  "}, nil }

	mustPass(t, l)

	_, compacted := snapshot(t, e)
	if compacted[0].CommonTaskPattern != "(unspecified)" {
		t.Errorf("pattern = %q, want a label a reader can act on", compacted[0].CommonTaskPattern)
	}
}

// ---- the prompt and the parser ----------------------------------------- //

func TestTheModelSummarizerMakesOneCallAndParsesIt(t *testing.T) {
	t.Parallel()
	var gotSystem, gotUser string
	calls := 0
	s := NewSummarizer(func(_ context.Context, _, system, user string) (string, error) {
		calls++
		gotSystem, gotUser = system, user
		return `{"common_task_pattern":"posted updates","subjects_involved":["finance"]}`, nil
	})
	got, err := s.Summarize(context.Background(), Cluster{
		Handle: "ceo", Episodes: []Episode{rawEp("a", t0, "slack_post")},
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if calls != 1 {
		t.Errorf("model called %d times, want 1", calls)
	}
	if gotSystem != CompactorSystemPrompt || !strings.Contains(gotUser, "task a") {
		t.Errorf("prompts did not reach the model: %q / %q", gotSystem, gotUser)
	}
	if got.CommonTaskPattern != "posted updates" || !slices.Equal(got.SubjectsInvolved, []string{"finance"}) {
		t.Errorf("summary = %+v", got)
	}

	boom := errors.New("rate limited")
	s = NewSummarizer(func(context.Context, string, string, string) (string, error) { return "", boom })
	if _, err := s.Summarize(context.Background(), Cluster{Episodes: []Episode{rawEp("a", t0)}}); !errors.Is(err, boom) {
		t.Errorf("model error = %v, want it propagated", err)
	}
	if _, err := NewSummarizer(nil).Summarize(context.Background(), Cluster{Episodes: []Episode{rawEp("a", t0)}}); err == nil {
		t.Error("a summarizer with no model answered")
	}
	if _, err := s.Summarize(context.Background(), Cluster{}); err == nil {
		t.Error("an empty cluster was summarised")
	}
}

func TestParseSummaryToleratesWhatAModelActuallySends(t *testing.T) {
	t.Parallel()
	clean := `{"common_task_pattern":"p","common_outcome":"done",
		"subjects_involved":["a"," b ",""],"notable_patterns":"n"}`
	cases := []struct {
		name    string
		raw     string
		want    Summary
		wantErr bool
	}{
		{name: "clean", raw: clean, want: Summary{
			CommonTaskPattern: "p", CommonOutcome: "done",
			SubjectsInvolved: []string{"a", "b"}, NotablePatterns: "n"}},
		{name: "wrapped in prose", raw: "Sure! Here you go:\n" + clean + "\nHope that helps.",
			want: Summary{CommonTaskPattern: "p", CommonOutcome: "done",
				SubjectsInvolved: []string{"a", "b"}, NotablePatterns: "n"}},
		{name: "fenced", raw: "```json\n" + clean + "\n```",
			want: Summary{CommonTaskPattern: "p", CommonOutcome: "done",
				SubjectsInvolved: []string{"a", "b"}, NotablePatterns: "n"}},
		// The fields are read one at a time, so a wrong-typed one costs
		// itself and not the sentence the planner actually reads.
		{name: "a field of the wrong shape", raw: `{"common_task_pattern":"p","subjects_involved":"nobody"}`,
			want: Summary{CommonTaskPattern: "p"}},
		{name: "the heterogeneous escape hatch", raw: `{"common_task_pattern":"(heterogeneous)"}`,
			want: Summary{CommonTaskPattern: "(heterogeneous)"}},
		{name: "whitespace is trimmed", raw: `{"common_task_pattern":"  p\n"}`,
			want: Summary{CommonTaskPattern: "p"}},
		// `null` is valid JSON and is not an object. Without the check it
		// parses as a summary with every field empty.
		{name: "null", raw: "null", wantErr: true},
		{name: "an array", raw: `["a"]`, wantErr: true},
		{name: "a bare string", raw: `"done"`, wantErr: true},
		{name: "nothing", raw: "", wantErr: true},
		{name: "prose only", raw: "I could not summarise these turns.", wantErr: true},
		{name: "a broken object", raw: `{"common_task_pattern": }`, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseSummary(c.raw)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParseSummary(%q) = %+v, want an error", c.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSummary(%q): %v", c.raw, err)
			}
			if got.CommonTaskPattern != c.want.CommonTaskPattern ||
				got.CommonOutcome != c.want.CommonOutcome ||
				got.NotablePatterns != c.want.NotablePatterns ||
				!slices.Equal(got.SubjectsInvolved, c.want.SubjectsInvolved) {
				t.Errorf("ParseSummary = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestRenderClusterFlattensClampsAndCounts(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", perTurnDetail+50)
	first := rawEp("a", t0, "t1", "t2", "t3", "t4", "t5", "t6", "t7", "t8", "t9")
	first.TaskSummary = "line one\nline two"
	first.PlanSummary = long
	second := rawEp("b", t0.Add(-time.Hour))
	second.ReviewOutcome = "failed"

	got := RenderCluster(Cluster{Handle: "ceo", Episodes: []Episode{first, second}})

	if !strings.Contains(got, "Cluster of 2 similar agent turns") {
		t.Errorf("the count is not stated:\n%s", got)
	}
	// A turn summary carrying its own newlines would look like more turns
	// than there are, and a model that miscounts writes the pattern of a
	// cluster that does not exist.
	if !strings.Contains(got, "task: line one line two") {
		t.Errorf("newlines were not flattened:\n%s", got)
	}
	if strings.Contains(got, long) || !strings.Contains(got, "xxx...") {
		t.Errorf("the per-turn clamp did not apply:\n%s", got)
	}
	// Capped at 8, and it SAYS SO. The compactor is inferring "how this
	// agent does this kind of work", and a silently shortened tool sequence
	// reads as a shorter procedure — which is the pattern it then writes
	// down as the seat's own.
	if strings.Contains(got, "t9") || !strings.Contains(got, "t8") {
		t.Errorf("the tool list is not capped at 8:\n%s", got)
	}
	if !strings.Contains(got, "(+1 more)") {
		t.Errorf("the tool-list cut is silent:\n%s", got)
	}
	// And a turn under the cap carries no marker at all.
	under := rawEp("c", t0, "t1", "t2")
	if u := RenderCluster(Cluster{Handle: "ceo", Episodes: []Episode{under}}); strings.Contains(u, "more)") {
		t.Errorf("a short tool list claimed to be cut:\n%s", u)
	}
	if !strings.Contains(got, "tools: (none)") {
		t.Errorf("a tool-free turn is not labelled:\n%s", got)
	}
	if !strings.Contains(got, "[failed]") {
		t.Errorf("the per-turn outcome is missing:\n%s", got)
	}
	if !strings.Contains(got, t0.Format(time.DateOnly)) {
		t.Errorf("the date is missing:\n%s", got)
	}
	if strings.Count(got, "\n1. ") != 1 {
		t.Errorf("turns are not numbered once each:\n%s", got)
	}
}

func TestTheCompactorPromptDoesNotAskForWhatItComputes(t *testing.T) {
	t.Parallel()
	// The rate on the row is derived from the members' outcomes. Asking for
	// one spends tokens to invite a number contradicting the row it lands
	// on, so the ask is gone and the parser has nowhere to put one.
	if strings.Contains(CompactorSystemPrompt, "success_rate") {
		t.Error("the prompt asks for a success rate the row does not take")
	}
	for _, field := range []string{"common_task_pattern", "common_outcome",
		"subjects_involved", "notable_patterns"} {
		if !strings.Contains(CompactorSystemPrompt, field) {
			t.Errorf("the prompt does not name %s, which the parser reads", field)
		}
	}
}

// ---- knobs ------------------------------------------------------------- //

func TestDefaultsMatchTheKnobsAnOperatorEdits(t *testing.T) {
	t.Parallel()
	// Two packages carry these numbers: this one applies them, and config
	// is what an operator reads and edits. Disagreeing means an engine
	// built with an explicit config behaves differently from one built with
	// a zero Options, and nothing else would notice.
	want := config.DefaultEpisodeLifecycle()
	got := Options{}.withDefaults()

	cases := []struct {
		field    string
		got, cfg int
	}{
		{"threshold", got.Threshold, want.MaxRawEpisodesPerAgent},
		{"non_terminal_max_age_days", int(got.NonTerminalMaxAge / day), want.NonTerminalMaxAgeDays},
		{"consolidated_grace_days", int(got.ConsolidatedGrace / day), want.ConsolidatedGraceDays},
		{"compaction_min_age_days", int(got.MinAge / day), want.CompactionMinAgeDays},
		{"compaction_min_cluster_size", got.MinClusterSize, want.CompactionMinClusterSize},
		{"compaction_batch_size", got.BatchSize, want.CompactionBatchSize},
		{"exemplar_count", got.ExemplarCount, want.ExemplarCount},
		{"compacted_max_age_days", int(got.CompactedMaxAge / day), want.CompactedMaxAgeDays},
	}
	for _, c := range cases {
		if c.got != c.cfg {
			t.Errorf("%s: lifecycle default %d, config default %d", c.field, c.got, c.cfg)
		}
	}
	if got.JaccardThreshold != want.CompactionJaccardThreshold {
		t.Errorf("jaccard: lifecycle %v, config %v",
			got.JaccardThreshold, want.CompactionJaccardThreshold)
	}
	// And the one cross-knob rule config validates: keeping as many rows as
	// the smallest cluster holds makes a fold pure cost.
	if got.ExemplarCount >= got.MinClusterSize {
		t.Errorf("%d exemplars out of a minimum cluster of %d keeps every row",
			got.ExemplarCount, got.MinClusterSize)
	}
}

func TestHandBuiltOptionsAreClampedToSomethingThatDrains(t *testing.T) {
	t.Parallel()
	got := Options{MinClusterSize: 1, JaccardThreshold: 4, BatchSize: 2, ExemplarCount: -1}.withDefaults()
	if got.MinClusterSize != 2 {
		t.Errorf("min cluster size = %d, want 2: a cluster of one is a turn with a "+
			"summary written over it", got.MinClusterSize)
	}
	if got.JaccardThreshold != defaultJaccard {
		t.Errorf("jaccard = %v, want a threshold in (0,1]", got.JaccardThreshold)
	}
	if got.BatchSize < got.MinClusterSize {
		t.Errorf("batch size = %d, smaller than one cluster", got.BatchSize)
	}
	// Below one reads as unset: a fold that keeps no anchors writes
	// summaries nobody can open, and the zero value of this struct must not
	// be the configuration that does it.
	if got.ExemplarCount != defaultExemplarCount {
		t.Errorf("exemplar count = %d, want the default %d", got.ExemplarCount, defaultExemplarCount)
	}
	// An explicit value is never overwritten by a default.
	explicit := Options{Threshold: 7, MinAge: time.Hour, ExemplarCount: 1,
		JaccardThreshold: 0.9, BatchSize: 5, MinClusterSize: 3,
		NonTerminalMaxAge: time.Minute, ConsolidatedGrace: time.Minute,
		CompactedMaxAge: time.Minute}.withDefaults()
	if explicit.Threshold != 7 || explicit.MinAge != time.Hour || explicit.ExemplarCount != 1 ||
		explicit.JaccardThreshold != 0.9 || explicit.BatchSize != 5 ||
		explicit.NonTerminalMaxAge != time.Minute || explicit.ConsolidatedGrace != time.Minute ||
		explicit.CompactedMaxAge != time.Minute {
		t.Errorf("an explicit Options was overwritten: %+v", explicit)
	}
	if NewLifecycle(nil, nil, Options{}).Options().Threshold != defaultThreshold {
		t.Error("NewLifecycle did not apply the defaults")
	}
}

func TestTheBatchSizeBoundsOnePass(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{BatchSize: 4, MinClusterSize: 3, ExemplarCount: 1})
	for i := range 8 {
		write(t, e, rawEp(fmt.Sprintf("s%d", i), daysAgo(60-i), "slack_post"))
	}

	mustPass(t, l)

	if sum.calls() != 1 || len(sum.seen[0].Episodes) != 4 {
		t.Fatalf("first pass folded %d turns, want the batch of 4", len(sum.seen[0].Episodes))
	}
	// Oldest first, so the batch is the front of the queue and the rest wait
	// for the next pass rather than being skipped.
	if want := []string{"s0", "s1", "s2", "s3"}; !slices.Equal(idsOf(sum.seen[0].Episodes), want) {
		t.Errorf("batch = %v, want the oldest four %v", idsOf(sum.seen[0].Episodes), want)
	}
}

// ---- the gap this port does not close ---------------------------------- //

func TestTurnsThatCalledNoToolsAreNeverCompacted(t *testing.T) {
	t.Parallel()
	l, e, sum := newLife(t, Options{})
	// A chat-only seat: every turn reads a message and replies, calling
	// nothing. Tool-sequence Jaccard is the only similarity signal the pass
	// has, so none of these can ever be pooled — the seat's raw count grows
	// without bound and every threshold crossing fires a pass that does
	// nothing. Fixing it needs a second clustering signal (the rows carry
	// embeddings), which would also have to agree with what the skill
	// drafting pass calls "the same work".
	for i := range 10 {
		write(t, e, rawEp(fmt.Sprintf("chat%d", i), daysAgo(60-i)))
	}

	res := mustPass(t, l)

	if sum.calls() != 0 || res.ClustersCompacted != 0 {
		t.Fatalf("tool-free turns were compacted after all: %+v", res)
	}
	if rawRows, _ := snapshot(t, e); len(rawRows) != 10 {
		t.Errorf("raw rows = %d, want all 10 still there", len(rawRows))
	}
}

// ---- helpers ----------------------------------------------------------- //

func sortedClone(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

func clusterIDs(clusters [][]Episode) [][]string {
	out := make([][]string, len(clusters))
	for i, c := range clusters {
		out[i] = idsOf(c)
	}
	return out
}

// compactionFault fails one named statement at the driver, which is the only
// way to reach a half-applied write: closing the database fails everything at
// once, and that cannot leave a summary standing over rows it should have
// removed.
//
// The wrapped driver is the real one, so the schema, the SQL and the encoding
// are all genuine.
type compactionFault struct {
	mu     sync.Mutex
	match  string
	err    error
	begins int
}

// begun reports how many write transactions have been opened since the last
// reset. Opening one costs the single writer this store has, so "a pass that
// found nothing wrote nothing" is a property with a number behind it.
func (f *compactionFault) begun() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.begins
}

func (f *compactionFault) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins = 0
}

func (f *compactionFault) countBegin() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.begins++
}

func (f *compactionFault) failOn(match string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.match, f.err = match, err
}

func (f *compactionFault) disarm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.match, f.err = "", nil
}

func (f *compactionFault) intercept(query string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.match == "" || !strings.Contains(query, f.match) {
		return nil
	}
	return f.err
}

func (f *compactionFault) wrap(d driver.Driver) driver.Driver {
	return compactionFaultDriver{inner: d, fault: f}
}

type compactionFaultDriver struct {
	inner driver.Driver
	fault *compactionFault
}

func (d compactionFaultDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return compactionFaultConn{Conn: conn, fault: d.fault}, nil
}

// The store's connector requires ExecerContext, and database/sql picks its
// query path from the optional interfaces a conn implements — so each is
// forwarded explicitly. Embedding driver.Conn does not carry them.
type compactionFaultConn struct {
	driver.Conn
	fault *compactionFault
}

func (c compactionFaultConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	ex, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	if err := c.fault.intercept(q); err != nil {
		return nil, err
	}
	return ex.ExecContext(ctx, q, args)
}

func (c compactionFaultConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	qr, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return qr.QueryContext(ctx, q, args)
}

func (c compactionFaultConn) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	pc, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Conn.Prepare(q)
	}
	return pc.PrepareContext(ctx, q)
}

func (c compactionFaultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.fault.countBegin()
	bt, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		// The deprecated Begin is the CORRECT fallback for a driver that
		// never implemented ConnBeginTx — it is what database/sql itself
		// falls back to. A wrapper that refused it would work with fewer
		// drivers than the standard library.
		//nolint:staticcheck // SA1019: the fallback database/sql itself uses.
		return c.Conn.Begin()
	}
	return bt.BeginTx(ctx, opts)
}

// BenchmarkRecallScan is where defaultThreshold comes from. Recall is a brute
// scan of every embedded row a seat owns and it runs in the Plan phase of every
// turn, so the raw-row count IS a per-turn latency budget:
//
//	100 rows    6.0 ms
//	250 rows   16.0 ms
//	500 rows   31.7 ms   <- defaultThreshold
//	1000 rows  63.2 ms
//	2000 rows 122.4 ms
//
// Linear at ~62 µs per row, with nothing else in the system bounding it.
func BenchmarkRecallScan(b *testing.B) {
	const dim = 1536
	for _, n := range []int{100, 250, 500, 1000, 2000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			db, err := store.Open(b.Context(), filepath.Join(b.TempDir(), "l.db"),
				store.Options{EmbeddingDim: dim})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = db.Close() })
			e := NewEpisodes(db)
			rng := rand.New(rand.NewPCG(1, 2))
			vec := func() []float32 {
				v := make([]float32, dim)
				for i := range v {
					v[i] = rng.Float32()
				}
				return v
			}
			for i := range n {
				ep := rawEp(fmt.Sprintf("e%04d", i), t0.Add(time.Duration(i)*time.Minute))
				ep.Embedding = vec()
				if _, err := e.Append(b.Context(), ep); err != nil {
					b.Fatal(err)
				}
			}
			q := RecallQuery{Handle: "ceo", Embedding: vec(), Limit: 5}
			b.ResetTimer()
			for b.Loop() {
				if _, err := e.Recall(context.Background(), q); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// A turn that called NO TOOLS can never be compacted: clustering pools turns
// by tool-sequence overlap, and Jaccard over an empty set is undefined. So
// for a chat-only seat the fold that bounds every other seat's raw rows never
// fires, and the table grows for the life of the deployment — every row of it
// scanned and cosined on the Plan phase of every turn.
//
// This is the only bound on those rows, so it is the only thing standing
// between a chat-heavy seat and a recall that slows down for ever.
func TestAToolFreeTurnIsDroppedOnceItHasAgedOut(t *testing.T) {
	l, e, _ := newLife(t, Options{})

	// Well past the horizon, and deliberately terminal and unconsolidated
	// so no other sweep here could account for it going.
	write(t, e, rawEp("chat-old", daysAgo(120)))
	// Inside the horizon: a recent chat turn is still worth recalling.
	write(t, e, rawEp("chat-new", daysAgo(10)))
	// A tool-using turn of the same age must be untouched — that one has a
	// fold to be carried into, which is exactly why it is not swept.
	write(t, e, rawEp("tooled-old", daysAgo(120), "slack_post"))

	res := mustPass(t, l)
	if res.ToolFreeDropped != 1 {
		t.Fatalf("dropped %d tool-free turns, want 1", res.ToolFreeDropped)
	}

	rawRows, _ := snapshot(t, e)
	got := idsOf(rawRows)
	if slices.Contains(got, "chat-old") {
		t.Errorf("the aged-out chat turn survived: %v", got)
	}
	if !slices.Contains(got, "chat-new") {
		t.Errorf("a chat turn inside the horizon was dropped: %v", got)
	}
	if !slices.Contains(got, "tooled-old") {
		t.Errorf("a tool-using turn was swept by the tool-free horizon: %v", got)
	}
}

// The horizon is a knob with a shipped default, and a deployment that sets
// its own must be honoured — the whole reason this is an Option rather than
// a constant.
func TestTheToolFreeHorizonIsConfigurable(t *testing.T) {
	l, e, _ := newLife(t, Options{ToolFreeMaxAge: 5 * day})
	write(t, e, rawEp("chat", daysAgo(10)))

	res := mustPass(t, l)
	if res.ToolFreeDropped != 1 {
		t.Fatalf("a shorter horizon did not drop the turn past it: %+v", res)
	}
	if l.Options().ToolFreeMaxAge != 5*day {
		t.Errorf("the configured horizon was overwritten: %v", l.Options().ToolFreeMaxAge)
	}
}

// EVERY SIMILARITY DECISION IN THIS PACKAGE USES ONE FUNCTION, and they agree
// at the boundary.
//
// The overlap was written twice, under two names, and the copies were free to
// drift: `clusterByTools` and `clusterEpisodes` pooled by one of them while
// `mostSimilar` — the near-duplicate check that decides whether a drafted
// skill is a new one — used the other. A seat's skills clustered by one rule
// and deduplicated by another means a draft can be rejected as a duplicate of
// a skill it would never have been clustered with, which reads as "synthesis
// stopped producing anything" and is invisible from either side alone.
//
// {x,y} and {y,z} overlap at exactly 1/3, so a threshold AT it must pool and a
// threshold above it must not — on both paths, in the same direction.
func TestEverySimilarityDecisionAgreesAtTheThreshold(t *testing.T) {
	t.Parallel()
	left, right := []string{"x", "y"}, []string{"y", "z"}
	const atTheBoundary = 1.0 / 3.0
	const justAbove = 0.34

	for _, tc := range []struct {
		name      string
		threshold float64
		same      bool
	}{
		{"at the boundary", atTheBoundary, true},
		{"just above it", justAbove, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clustered := clusterByTools([]Episode{
				{ID: "a", ToolSequence: left}, {ID: "b", ToolSequence: right},
			}, tc.threshold)
			if pooled := len(clustered) == 1; pooled != tc.same {
				t.Errorf("clusterByTools pooled = %v, want %v", pooled, tc.same)
			}
			if _, duplicate := mostSimilar(left, [][]string{right}, tc.threshold); duplicate != tc.same {
				t.Errorf("mostSimilar duplicate = %v, want %v — the two paths disagree",
					duplicate, tc.same)
			}
		})
	}
}
