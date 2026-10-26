package cache_test

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// HEAT DECAYS OUT OF ITS WINDOW, and out of nothing else. What a node served
// of a repository counts in the hour's window for an hour and in the day's
// for a day, each to within a twenty-fourth of its span, and then leaves it —
// a bucket the ring comes round to again starts from nothing rather than
// adding a day-old count to the hour's — and a repository nothing used within
// the day is gone from the heat altogether: neither hot, nor named.
//
// The uses here land at the first instant of the clock's first slot, so each
// leaves its window at exactly the window's length, never a bucket sooner.
//
// Mutation: count every written bucket whatever slot it counts (ring.live),
// and the hour still counts the first fetch past its hour; keep a bucket's
// count when a later slot comes round to it (servedWindow.add), and the hour
// counts it twice.
func TestHeatDecaysOutOfItsWindow(t *testing.T) {
	t.Parallel()
	clk := newClock()
	start := clk.Now()
	h := cache.NewHeat(clk.Now)
	const repo = "0190f3c4-5b6a-7c8d-9e0f-0123456789ab"

	h.Add(repo, "web", cache.Use{Fetches: 1, Clones: 1, BytesOut: 4000})
	h.Add(repo, "web", cache.Use{Pushes: 1, BytesOut: 200})
	first := cache.Use{Fetches: 1, Clones: 1, Pushes: 1, BytesOut: 4200}
	if got := h.Of(repo); got.Name != "web" || got.Hour != first || got.Day != first {
		t.Fatalf("the heat just counted = %+v, want %+v in both windows, named web", got, first)
	}

	clk.Set(start.Add(30 * time.Minute))
	h.Add(repo, "web", cache.Use{Fetches: 1, BytesOut: 300})
	both := cache.Use{Fetches: 2, Clones: 1, Pushes: 1, BytesOut: 4500}
	if got := h.Of(repo); got.Hour != both || got.Day != both {
		t.Fatalf("half an hour on = %+v, want %+v in both windows", got, both)
	}

	clk.Set(start.Add(cache.HeatHour - time.Second))
	if got := h.Of(repo); got.Hour != both {
		t.Fatalf("a second short of the hour, the hour counts %+v, want every use still in it: %+v", got.Hour, both)
	}
	clk.Set(start.Add(cache.HeatHour))
	later := cache.Use{Fetches: 1, BytesOut: 300}
	if got := h.Of(repo); got.Hour != later || got.Day != both {
		t.Fatalf("an hour on = hour %+v, day %+v; want the first uses out of the hour (%+v) and in the day (%+v)",
			got.Hour, got.Day, later, both)
	}

	// THE RING COMES ROUND: the hour's first bucket counts this use now, and
	// nothing of what it counted an hour ago.
	h.Add(repo, "web", cache.Use{Fetches: 1})
	if got := h.Of(repo); got.Hour.Fetches != 2 || got.Day.Fetches != 3 {
		t.Fatalf("a fetch an hour on counts %d in the hour and %d in the day, want 2 and 3",
			got.Hour.Fetches, got.Day.Fetches)
	}
	if hot := h.Hot(cache.HotRepos); len(hot) != 1 || hot[0].ID != repo || hot[0].Name != "web" {
		t.Fatalf("the hot list within the day = %+v, want the one repository, named", hot)
	}

	clk.Set(start.Add(cache.HeatDay + cache.HeatHour))
	if got := h.Of(repo); got != (cache.RepoHeat{ID: repo}) {
		t.Fatalf("a day past its last use the repository's heat = %+v, want nothing in either window and no name", got)
	}
	if hot := h.Hot(cache.HotRepos); len(hot) != 0 {
		t.Fatalf("a day past every use the hot list = %+v, want none", hot)
	}
}

// THE HOT LIST HOLDS EACH WINDOW'S HOTTEST, in one list read either way: the
// n hottest of the hour and the n hottest of the day, a window's heat its git
// operations — fetches and pushes — with its bytes out breaking a tie and the
// id the next, those hottest in the hour first. A repository with nothing in a
// window is never one of that window's hottest, so a list cut by either
// window's order reads that window's n hottest exactly — and a repository
// below the n-th of both is in neither, while its own heat is still answered.
//
// Mutation: rank by bytes out before git operations, and the hour's order puts
// the heavy fetch first; take one window's hottest alone, and the day's
// hottest falls out of the list.
func TestTheHotListHoldsEachWindowsHottest(t *testing.T) {
	t.Parallel()
	clk := newClock()
	start := clk.Now()
	h := cache.NewHeat(clk.Now)
	add := func(repo string, fetches, pushes, bytes int64) {
		h.Add(repo, repo, cache.Use{Fetches: fetches, Pushes: pushes, BytesOut: bytes})
	}

	// Two hours before the look: the day's busiest, quiet in the hour.
	add("day-busiest", 40, 2, 1000)
	add("day-second", 20, 0, 100)
	add("day-third", 5, 0, 100)
	// Within the hour.
	clk.Set(start.Add(2*cache.HeatHour + time.Minute))
	add("hour-busiest", 9, 3, 50)
	add("hour-heavy", 1, 0, 1<<30)
	add("hour-light", 1, 0, 10)

	const n = 2
	hot := h.Hot(n)
	var ids []string
	for _, r := range hot {
		ids = append(ids, r.ID)
	}
	want := []string{"hour-busiest", "hour-heavy", "day-busiest", "day-second"}
	if !slices.Equal(ids, want) {
		t.Fatalf("Hot(%d) = %v, want the hour's two hottest, then the day's: %v", n, ids, want)
	}
	if got := hot[2].Day; got != (cache.Use{Fetches: 40, Pushes: 2, BytesOut: 1000}) || hot[2].Hour != (cache.Use{}) {
		t.Errorf("day-busiest reads hour %+v, day %+v; want nothing in the hour and all of it in the day",
			hot[2].Hour, got)
	}
	if got := h.Of("day-third"); got.Day.Fetches != 5 || got.Name != "day-third" {
		t.Errorf("a repository below both windows' hottest is answered %+v, want its own heat", got)
	}
	if got := h.Hot(0); got != nil {
		t.Errorf("Hot(0) = %+v, want nothing", got)
	}
}

// A HEAT COUNT NEVER WRAPS, AND NEVER GOES BELOW NOTHING: a bucket counts at
// most four billion, and saturates there rather than reading as a quiet
// repository; a negative use counts nothing; and a use that counts nothing
// keeps no repository at all.
//
// Mutation: add a bucket's counts with plain addition, and the hour reads
// 4 294 967 294.
func TestAHeatCountNeverWrapsOrGoesBelowNothing(t *testing.T) {
	t.Parallel()
	clk := newClock()
	h := cache.NewHeat(clk.Now)
	h.Add("r", "r", cache.Use{Fetches: math.MaxUint32})
	h.Add("r", "r", cache.Use{Fetches: math.MaxUint32, Pushes: -5, BytesOut: -1})
	if got := h.Of("r").Hour; got.Fetches != math.MaxUint32 || got.Pushes != 0 || got.BytesOut != 0 {
		t.Fatalf("the hour after two full buckets' fetches and a negative push = %+v, want %d fetches, no "+
			"push and no bytes", got, uint32(math.MaxUint32))
	}
	h.Add("nothing", "nothing", cache.Use{})
	h.Add("below", "below", cache.Use{Fetches: -1, BytesOut: -10})
	h.Add("", "unnamed", cache.Use{Fetches: 1})
	for _, r := range h.Hot(cache.HotRepos) {
		if r.ID != "r" {
			t.Errorf("the heat keeps %q, which nothing was counted for", r.ID)
		}
	}
}

// A STATUS COUNTS WHAT THE CACHE SPENT ON ITS OWN ACCOUNT, beside what it
// holds: the bytes against the budget and the repositories it holds a copy
// of, the copy in service of each — its incarnation, its position, its
// packs, its readers — and, over the hour and the day, the copies its budget
// evicted and the repairs it counted, the very ones crewlet.code.cache.rebuilds
// counts. An evicted repository has no copy in service, and its eviction
// leaves the hour an hour later and the day a day later — the hour's bucket
// starting again from nothing for an eviction an hour on.
//
// Mutation: count no eviction in evict, or count a repair into the counter
// alone (rebuilt), and the status's windows read nothing; keep a churn
// bucket's count when a later slot comes round to it (churnWindow.add), and
// the hour counts the first evictions with the later one.
func TestAStatusCountsWhatTheCacheSpentOnItsOwnAccount(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	start := clk.Now()
	repo, other := w.repo(t, 1), w.repo(t, 2)
	w.push(t, repo, "refs/heads/main", nil)
	earlier := w.log.state()
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, other, "refs/heads/main", nil)
	c := w.open(t, clk.on)

	s := w.sync(t, c, repo)
	cp, ok := c.Copy(repo)
	if !ok || cp.Incarnation != cache.Incarnation(s) || cp.Position != s.Position || cp.Packs != 2 ||
		cp.Readers != 1 || cp.Bytes <= 0 || !cp.LastUsed.Equal(start) {
		t.Fatalf("the copy in service while a reader holds it = %+v (%t), want incarnation %d at %s, two "+
			"packs, one reader, its bytes and its last use %s", cp, ok, cache.Incarnation(s), s.Position, start)
	}
	s.Release()
	w.sync(t, c, repo).Release() // converged: the next round is a delta
	w.sync(t, c, other).Release()
	if cp, _ = c.Copy(repo); cp.Readers != 0 {
		t.Errorf("a copy nobody holds reads %d readers", cp.Readers)
	}

	// A REPAIR: the log recreated under the cache, so the next round reads
	// the repository whole again, counted as a stream rebuild.
	w.log.recreate(earlier)
	w.sync(t, c, repo).Release()
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "stream"); got != 1 {
		t.Fatalf("%s{reason=stream} = %d, want the one whole read", metrics.CodeCacheRebuilds, got)
	}
	st := c.Status()
	if !st.Since.Equal(start) || st.Warm != 2 || st.Bytes != cache.Total(c) || st.Budget <= 0 ||
		st.Hour != (cache.Churn{Rebuilds: 1}) || st.Day != (cache.Churn{Rebuilds: 1}) {
		t.Fatalf("the status after one repair = %+v, want two warm repositories, the total %d and one rebuild "+
			"in each window, counting since %s", st, cache.Total(c), start)
	}

	cache.SetMaxBytes(c, 1)
	cache.JanitorPass(c)
	st = c.Status()
	if st.Warm != 0 || st.Budget != 1 || st.Hour.Evictions != 2 || st.Day.Evictions != 2 {
		t.Fatalf("the status after the budget evicted both = %+v, want none warm and two evictions in each "+
			"window", st)
	}
	if cp, ok := c.Copy(repo); ok {
		t.Errorf("an evicted repository has a copy in service: %+v", cp)
	}

	clk.Set(start.Add(cache.HeatHour))
	if st = c.Status(); st.Hour != (cache.Churn{}) || st.Day != (cache.Churn{Evictions: 2, Rebuilds: 1}) {
		t.Fatalf("an hour on the status spent hour %+v, day %+v; want nothing in the hour and all of it in "+
			"the day", st.Hour, st.Day)
	}

	// ANOTHER EVICTION AN HOUR ON lands in the hour's bucket the first ones
	// counted in, which starts again from nothing.
	cache.SetMaxBytes(c, 1<<30)
	w.sync(t, c, repo).Release()
	cache.SetMaxBytes(c, 1)
	cache.JanitorPass(c)
	if st = c.Status(); st.Hour != (cache.Churn{Evictions: 1}) || st.Day != (cache.Churn{Evictions: 3, Rebuilds: 1}) {
		t.Fatalf("an eviction an hour on reads hour %+v, day %+v; want it alone in the hour and beside the "+
			"first ones in the day", st.Hour, st.Day)
	}
	clk.Set(start.Add(cache.HeatDay))
	if st = c.Status(); st.Day != (cache.Churn{Evictions: 1}) {
		t.Errorf("a day after the first evictions the day reads %+v, want the later one alone", st.Day)
	}
	clk.Set(start.Add(cache.HeatDay + cache.HeatHour))
	if st = c.Status(); st.Day != (cache.Churn{}) {
		t.Errorf("a day after the last eviction the day reads %+v, want nothing", st.Day)
	}
}
