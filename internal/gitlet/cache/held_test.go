package cache_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A FAILURE ITS REQUEST REPAIRS IS THAT REQUEST'S (report.go, HELD FOR THE
// REQUEST): a run's report is held for its reader while that reader may still
// repair it, so a report's scrub never runs beside the request's own repair.

// scrubIdle fails t when repoID's copy has a scrub due or running, or has run
// one: what a report held for its request leaves it.
func scrubIdle(t *testing.T, c *cache.Cache, repoID, when string) {
	t.Helper()
	if why, running := cache.ScrubDue(c, repoID); why != "" || running {
		t.Errorf("%s the copy's scrub is due for %q (running %t), want none: the report is the request's",
			when, why, running)
	}
	if at, _, _ := cache.Scrubbed(c, repoID); !at.IsZero() {
		t.Errorf("%s the copy was scrubbed at %v, want never", when, at)
	}
}

// A REPORT ITS REQUEST REPAIRS IS THAT REQUEST'S. A run the host makes for a
// request names the request's reader (Synced.Repairer), and what it reports
// (here a truncated pack index) is HELD for that request rather than making
// the scrub due: nothing is due while the request decides; its repair (a
// forced round fetching that one pack again) consumes the report, counted
// reports{part=pack,outcome=request}; its release hands nothing on; and no
// scrub reads the copy for a repair of one pack.
//
// Mutations: the observer ignores Ran.Repairer, and the scrub is due as the
// run ends; Rebuild consumes nothing, and the release hands the report on,
// counted `queued`, with the scrub due once the request is done.
func TestAReportItsRequestRepairsIsThatRequests(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t)
	s := w.sync(t, c, h.repo)
	gittest.Damage(t, s.Dir, gittest.IdxTruncated, h.commits[3])
	f := failureOf(t, w, s, "rev-list", "--objects", h.commits[3])
	scrubIdle(t, c, h.repo, "as the request's run failed,")
	if got := reportsTotal(w); got != 0 {
		t.Errorf("the request's report was counted %d times before its request decided, want none", got)
	}

	gets := w.objects.total()
	r, err := c.Rebuild(t.Context(), s, f)
	if err != nil {
		t.Fatalf("Rebuild(%s): %v", f.Reason, err)
	}
	if got := w.objects.total() - gets; got != 1 {
		t.Errorf("the repair downloaded %d bundles, want the one pack whose index was truncated", got)
	}
	if got := reports(w, "pack", "request"); got != 1 {
		t.Errorf("reports{part=pack,outcome=request} = %d, want the one report the request's repair answered", got)
	}
	r.Release()
	s.Release()
	if got := reportsTotal(w); got != 1 {
		t.Errorf("after the request let the copy go %d reports were counted, want only the request's one", got)
	}
	scrubIdle(t, c, h.repo, "once the request let the copy go,")
	for _, trigger := range []string{"report", "request"} {
		if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"file": "pack",
			"trigger": trigger}); got != 0 {
			t.Errorf("checks{file=pack,trigger=%s} = %d, want no pack body hashed", trigger, got)
		}
	}
}

// A REPORT ITS REQUEST DOES NOT REPAIR GOES ON WITH THE REQUEST'S PIN. A
// request that fails past its first byte (no status left to retry with), whose
// failure the host does not repair (a crash names nothing; a hint whose pack
// holds to its row asks nothing), or that never fails (git read around the
// damage) ends without asking, and its pin's release hands each held report
// to the ordinary rules as though just arrived: the scrub is due, counted
// reports{outcome=queued}, and not before. A derived file's report is never
// held: a round of its own drops it at once, racing no repair.
//
// Mutations: the release hands nothing on, and each arm's report is lost; hold
// a derived report, and the graph's drop is counted only at the release.
func TestAReportItsRequestDoesNotRepairGoesOnWithItsPin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		part string
		res  func(began time.Time, h history) gitproc.Result
	}{
		{"a pack failure past the first byte", "pack", func(began time.Time, _ history) gitproc.Result {
			return gitproc.Result{Began: began, Exit: 128,
				Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: gitproc.PartPack, Began: began}}
		}},
		{"an answer read around an object's damage", "object", func(began time.Time, _ history) gitproc.Result {
			return gitproc.Result{Began: began,
				Damage: &gitproc.Damage{Kind: gitproc.ErrMissingObject, Parts: gitproc.PartObject, Began: began}}
		}},
		{"a crash", "crash", func(began time.Time, _ history) gitproc.Result {
			return gitproc.Result{Began: began, Exit: -1, Crashed: true}
		}},
		{"a hint naming a sound pack", "hint", func(began time.Time, h history) gitproc.Result {
			return gitproc.Result{Began: began, Exit: 128, Trace: gitproc.Trace{Hints: []gitproc.Hint{
				{Object: strings.Repeat("ab", 20), Pack: h.older.GitChecksum}}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			h := w.measured(t)
			c := w.open(t)
			s := w.sync(t, c, h.repo)
			cache.Report(c, gitproc.Ran{Dir: s.Dir, Repairer: s.Repairer, Result: tc.res(time.Now(), h)})
			scrubIdle(t, c, h.repo, "while the request holds the copy,")
			if got := reportsTotal(w); got != 0 {
				t.Errorf("the held report was counted %d times before its request ended, want none", got)
			}
			s.Release()
			if got := reports(w, tc.part, "queued"); got != 1 {
				t.Errorf("reports{part=%s,outcome=queued} = %d after the request let the copy go, want its one "+
					"report handed on", tc.part, got)
			}
			if got := reportsTotal(w); got != 1 {
				t.Errorf("%d reports were counted, want the one", got)
			}
			if why, _ := cache.ScrubDue(c, h.repo); why != "report" {
				t.Errorf("after the request let the copy go its scrub is due for %q, want report", why)
			}
		})
	}

	t.Run("a derived file's report", func(t *testing.T) {
		t.Parallel()
		w, h, c, s := servedHere(t, newClock())
		defer s.Release()
		ran := derivedReport(s.Dir, gitproc.PartCommitGraph, time.Now())
		ran.Repairer = s.Repairer
		cache.Report(c, ran)
		if got := reports(w, "commit-graph", "queued"); got != 1 {
			t.Errorf("reports{part=commit-graph,outcome=queued} = %d while its request holds the copy, want "+
				"the drop asked at once", got)
		}
		if why, _ := cache.ScrubDue(c, h.repo); why != "" {
			t.Errorf("a derived report made the scrub due for %q, want none: a drop repairs it", why)
		}
	})
}

// TWO REQUESTS ON ONE COPY EACH HOLD THEIR OWN REPORTS. Every reader is named
// apart (Synced.Repairer), so the first request's repair consumes only its own
// report, nothing is due while the second has not decided, the first's release
// hands on nothing, and the second's hands its report on.
//
// Mutations: hold reports under the copy rather than its reader, and the
// first's repair consumes the second's report too, nothing ever due for it;
// hand on every reader's reports at a release, and the first's release makes
// the scrub due for the second's.
func TestTwoRequestsOnOneCopyEachHoldTheirOwnReports(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t)
	first := w.sync(t, c, h.repo)
	second := w.sync(t, c, h.repo)
	if cache.Incarnation(first) != cache.Incarnation(second) || first.Repairer == second.Repairer {
		t.Fatalf("the two readers hold incarnations %d and %d named %d and %d, want one copy, two readers",
			cache.Incarnation(first), cache.Incarnation(second), first.Repairer, second.Repairer)
	}
	gittest.Damage(t, first.Dir, gittest.IdxTruncated, h.commits[3])
	f := failureOf(t, w, first, "rev-list", "--objects", h.commits[3])
	began := time.Now()
	cache.Report(c, gitproc.Ran{Dir: second.Dir, Repairer: second.Repairer, Result: gitproc.Result{Began: began,
		Exit: 128, Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: gitproc.PartPack, Began: began}}})

	r, err := c.Rebuild(t.Context(), first, f)
	if err != nil {
		t.Fatalf("the first request's Rebuild: %v", err)
	}
	if got := reports(w, "pack", "request"); got != 1 {
		t.Errorf("reports{part=pack,outcome=request} = %d, want the first request's one", got)
	}
	r.Release()
	first.Release()
	if got := reportsTotal(w); got != 1 {
		t.Errorf("%d reports were counted once the first request let go, want only its own", got)
	}
	scrubIdle(t, c, h.repo, "while the second request holds the copy,")

	second.Release()
	if got := reports(w, "pack", "queued"); got != 1 {
		t.Errorf("reports{part=pack,outcome=queued} = %d once the second request let go, want its report", got)
	}
	if why, _ := cache.ScrubDue(c, h.repo); why != "report" {
		t.Errorf("after the second request let go the copy's scrub is due for %q, want report", why)
	}
}

// A REQUEST'S HELD REPORTS FOLLOW IT ACROSS AN IN-PLACE REPAIR. A derived
// repair hands back the same copy under a new reader and the host releases the
// failed pin; a crash reported beside the derived damage is no part a drop
// answers, so it is carried to that reader (Synced.Repairer), neither consumed
// nor handed on, and nothing is due. When the retry fails on a pack (the
// escalation the host's maxRepairs bounds), its repair answers the carried
// crash with the pack's report, both outcome=request, and no pack is hashed
// for a report.
//
// Mutation: drop the carry (Rebuild's in-place answer moves nothing), and the
// failed pin's release hands the crash on, counted queued, the scrub due while
// the request still holds the repaired copy.
func TestARequestsHeldReportsFollowItAcrossAnInPlaceRepair(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	h := w.measured(t)
	c := w.open(t)
	s := w.sync(t, c, h.repo)
	// The copy holds the commit-graph the repair is asked to drop: a report
	// reaches only a file the copy holds (image.holding), and damage of one it
	// does not hold is no derived file a drop ends.
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{h.repo}) {
		t.Fatalf("the maintainer ran %v, want the copy's commit-graph written", ran)
	}
	if _, _, graph := cache.Indexed(c, h.repo); !graph {
		t.Fatal("the copy holds no commit-graph")
	}
	began := time.Now()
	cache.Report(c, gitproc.Ran{Dir: s.Dir, Repairer: s.Repairer, Result: gitproc.Result{Began: began, Exit: -1,
		Crashed: true, Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: gitproc.PartCommitGraph,
			Began: began}}})
	r, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildDerived,
		Drops: gitproc.PartCommitGraph, Began: began})
	if err != nil {
		t.Fatalf("Rebuild(%s): %v", cache.RebuildDerived, err)
	}
	defer r.Release()
	if cache.Incarnation(r) != cache.Incarnation(s) || r.Repairer == s.Repairer {
		t.Fatalf("the derived repair answered incarnation %d named %d over %d named %d, want the same copy "+
			"handed to a reader of its own", cache.Incarnation(r), r.Repairer, cache.Incarnation(s), s.Repairer)
	}
	s.Release()
	scrubIdle(t, c, h.repo, "once the request let go of the pin it failed over,")
	if got := reports(w, "crash", "queued"); got != 0 {
		t.Errorf("reports{part=crash,outcome=queued} = %d while the request runs again, want the crash still "+
			"held for it", got)
	}

	gittest.Damage(t, r.Dir, gittest.IdxTruncated, h.commits[3])
	f := failureOf(t, w, r, "rev-list", "--objects", h.commits[3])
	again, err := c.Rebuild(t.Context(), r, f)
	if err != nil {
		t.Fatalf("Rebuild(%s) of the retry's failure: %v", f.Reason, err)
	}
	for _, part := range []string{"crash", "pack"} {
		if got := reports(w, part, "request"); got != 1 {
			t.Errorf("reports{part=%s,outcome=request} = %d, want the one the request's pack repair answered",
				part, got)
		}
	}
	again.Release()
	r.Release()
	if got := reports(w, "crash", "queued"); got != 0 {
		t.Errorf("reports{part=crash,outcome=queued} = %d once the request ended, want none: its repair "+
			"answered the crash", got)
	}
	scrubIdle(t, c, h.repo, "once the request ended,")
	if got := counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"file": "pack",
		"trigger": "report"}); got != 0 {
		t.Errorf("checks{file=pack,trigger=report} = %d, want no pack hashed for a report", got)
	}
}
