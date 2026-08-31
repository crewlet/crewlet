package maintenance_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/maintenance"
)

var base = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// recorder is a job that remembers the cutoff it was handed.
type recorder struct {
	mu      sync.Mutex
	cutoffs []time.Time
	nows    []time.Time
	rows    int64
	err     error
}

func (r *recorder) run(_ context.Context, now, cutoff time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nows = append(r.nows, now)
	r.cutoffs = append(r.cutoffs, cutoff)
	return r.rows, r.err
}

func (r *recorder) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cutoffs)
}

func (r *recorder) lastCutoff(t *testing.T) time.Time {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cutoffs) == 0 {
		t.Fatal("the job never ran")
	}
	return r.cutoffs[len(r.cutoffs)-1]
}

func fixed(at time.Time) func() time.Time { return func() time.Time { return at } }

func TestTheCutoffIsNowLessTheHorizon(t *testing.T) {
	var r recorder
	r.rows = 3
	w := maintenance.New(maintenance.Options{
		Now: fixed(base),
		Jobs: []maintenance.Job{
			{Name: "rows", Horizon: 2 * time.Hour, Run: r.run},
		},
	})

	swept, err := w.Tick(t.Context())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := r.lastCutoff(t); !got.Equal(base.Add(-2 * time.Hour)) {
		t.Fatalf("cutoff %s, want %s", got, base.Add(-2*time.Hour))
	}
	if swept["rows"] != 3 {
		t.Fatalf("swept = %v", swept)
	}
	// A job that removed nothing is absent rather than zero, so the log
	// line names only the tables that actually moved.
	r.rows = 0
	swept, _ = w.Tick(t.Context())
	if len(swept) != 0 {
		t.Fatalf("a job that removed nothing was reported: %v", swept)
	}
}

// A job carrying its own horizon declares none here and is handed a cutoff
// it ignores — the event log's retention is a property of the log.
func TestAJobWithNoHorizonStillRuns(t *testing.T) {
	var r recorder
	r.rows = 1
	w := maintenance.New(maintenance.Options{
		Now:  fixed(base),
		Jobs: []maintenance.Job{{Name: "events", Run: r.run}},
	})

	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := r.lastCutoff(t); !got.Equal(base) {
		t.Fatalf("a horizon-less job got cutoff %s, want now", got)
	}
}

// The module's invariant: the tick is shorter than every horizon. A horizon
// below it would let a table sit past its own horizon for the difference,
// and the horizon would stop describing the table.
func TestAHorizonBelowTheTickIsRaisedToIt(t *testing.T) {
	var r recorder
	w := maintenance.New(maintenance.Options{
		Now:      fixed(base),
		Interval: time.Hour,
		Jobs: []maintenance.Job{
			{Name: "shallow", Horizon: time.Minute, Run: r.run},
		},
	})

	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := r.lastCutoff(t); !got.Equal(base.Add(-time.Hour)) {
		t.Fatalf("cutoff %s, want the raised horizon %s", got, base.Add(-time.Hour))
	}
}

// Every job runs even when one fails. They are independent tables, and one
// unreachable store must not stop the housekeeping for all of them — that is
// the failure this package exists to fix, arrived at from the other side.
func TestOneFailingJobDoesNotStopTheRest(t *testing.T) {
	boom := errors.New("store unreachable")
	var first, third recorder
	first.rows, third.rows = 2, 5
	failing := recorder{err: boom}
	w := maintenance.New(maintenance.Options{
		Now: fixed(base),
		Jobs: []maintenance.Job{
			{Name: "first", Horizon: time.Hour, Run: first.run},
			{Name: "second", Horizon: time.Hour, Run: failing.run},
			{Name: "third", Horizon: time.Hour, Run: third.run},
		},
	})

	swept, err := w.Tick(t.Context())
	if !errors.Is(err, boom) {
		t.Fatalf("the failure was not reported: %v", err)
	}
	if first.calls() != 1 || third.calls() != 1 {
		t.Fatalf("the other jobs were skipped: first=%d third=%d",
			first.calls(), third.calls())
	}
	if swept["first"] != 2 || swept["third"] != 5 {
		t.Fatalf("swept = %v", swept)
	}
	if _, reported := swept["second"]; reported {
		t.Fatal("the failing job reported rows")
	}
}

// The duty is claimed per tick, not held. A node that does not hold it must
// not sweep — N nodes deleting the same rows is what the singleton exists
// to prevent.
func TestANodeWithoutTheDutyDoesNotSweep(t *testing.T) {
	var r recorder
	var holds atomic.Bool
	w := maintenance.New(maintenance.Options{
		Now:       fixed(base),
		Jobs:      []maintenance.Job{{Name: "rows", Horizon: time.Hour, Run: r.run}},
		ClaimDuty: func(context.Context) (bool, error) { return holds.Load(), nil },
	})

	swept, err := w.Tick(t.Context())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if r.calls() != 0 {
		t.Fatal("a node without the duty swept anyway")
	}
	// nil, NOT an empty map: "somebody else swept" and "nothing needed
	// sweeping" are different facts and an empty map merges them.
	if swept != nil {
		t.Fatalf("a skipped tick reported %v, want nil", swept)
	}

	holds.Store(true)
	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if r.calls() != 1 {
		t.Fatal("the duty holder did not sweep")
	}
}

// FAILS CLOSED on an unreachable coordination store, and this is the cheap
// direction: a skipped tick costs one interval, which the next recovers in
// full because a range delete over a horizon is not incremental.
func TestAnUnknownDutySkipsTheTick(t *testing.T) {
	var r recorder
	w := maintenance.New(maintenance.Options{
		Now:  fixed(base),
		Jobs: []maintenance.Job{{Name: "rows", Horizon: time.Hour, Run: r.run}},
		ClaimDuty: func(context.Context) (bool, error) {
			return false, errors.New("coordination store unreachable")
		},
	})

	swept, err := w.Tick(t.Context())
	if err != nil {
		t.Fatalf("an unreachable duty store surfaced as a tick failure: %v", err)
	}
	if r.calls() != 0 || swept != nil {
		t.Fatal("the sweep ran without knowing it held the duty")
	}
}

// A cancelled context is NOT a claim failure to be shrugged off: it is the
// worker stopping, and reporting it lets the loop exit quietly instead of
// logging a spurious warning on every shutdown.
func TestACancelledTickReportsCancellation(t *testing.T) {
	w := maintenance.New(maintenance.Options{
		Now:  fixed(base),
		Jobs: []maintenance.Job{{Name: "rows", Horizon: time.Hour, Run: (&recorder{}).run}},
		ClaimDuty: func(ctx context.Context) (bool, error) {
			return false, ctx.Err()
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := w.Tick(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Tick reported %v, want context.Canceled", err)
	}
}

func TestTheLoopSweepsAndStops(t *testing.T) {
	var r recorder
	r.rows = 1
	w := maintenance.New(maintenance.Options{
		Now:      fixed(base),
		Interval: time.Millisecond,
		Jobs:     []maintenance.Job{{Name: "rows", Run: r.run}},
	})

	w.Start(t.Context())
	deadline := time.Now().Add(2 * time.Second)
	for r.calls() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	w.Stop()
	if r.calls() < 3 {
		t.Fatalf("the loop ran %d times in two seconds", r.calls())
	}

	// Stop WAITS for the in-flight tick, so nothing runs after it returns.
	settled := r.calls()
	time.Sleep(20 * time.Millisecond)
	if r.calls() != settled {
		t.Fatalf("the loop kept running after Stop: %d then %d", settled, r.calls())
	}
	// And Stop is idempotent — a node shutting down twice must not block.
	w.Stop()
}

// A deployment with no store wires no jobs, which is correct rather than an
// error: an in-memory twin prunes itself inline, because a process-local map
// dies with the process.
// Stop WAITS for the in-flight tick. Returning while a sweep is mid-flight
// means a process exits with range deletes in progress against a store it is
// about to close — and the caller has no way to tell, because Stop returned.
func TestStopWaitsForTheTickInFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	var once sync.Once
	w := maintenance.New(maintenance.Options{
		Now:      fixed(base),
		Interval: time.Millisecond,
		Jobs: []maintenance.Job{{
			Name: "slow",
			Run: func(context.Context, time.Time, time.Time) (int64, error) {
				once.Do(func() {
					close(entered)
					<-release
					finished.Store(true)
				})
				return 0, nil
			},
		}},
	})

	w.Start(t.Context())
	if !w.Running() {
		t.Fatal("Start did not run the loop")
	}
	<-entered

	stopped := make(chan struct{})
	go func() { w.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a tick was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop never returned")
	}
	if !finished.Load() {
		t.Fatal("Stop returned before the tick finished")
	}
	if w.Running() {
		t.Fatal("the worker still reports running after Stop")
	}
}

func TestAWorkerWithNoJobsIsANoOp(t *testing.T) {
	w := maintenance.New(maintenance.Options{Now: fixed(base)})
	if got := w.Jobs(); len(got) != 0 {
		t.Fatalf("Jobs = %v", got)
	}
	w.Start(t.Context()) // must not start a goroutine or panic
	if w.Running() {
		t.Fatal("a worker with nothing to sweep started a loop")
	}
	w.Stop()
	if swept, err := w.Tick(t.Context()); err != nil || len(swept) != 0 {
		t.Fatalf("Tick = %v, %v", swept, err)
	}
}

// A job with no name or no function is dropped rather than carried as a
// half-built entry that panics on the first tick.
func TestAnIncompleteJobIsDropped(t *testing.T) {
	var r recorder
	w := maintenance.New(maintenance.Options{
		Now: fixed(base),
		Jobs: []maintenance.Job{
			{Name: "", Horizon: time.Hour, Run: r.run},
			{Name: "nameless", Horizon: time.Hour},
			{Name: "real", Horizon: time.Hour, Run: r.run},
		},
	})
	if got := w.Jobs(); !slices.Equal(got, []string{"real"}) {
		t.Fatalf("Jobs = %v, want only the complete one", got)
	}
	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func TestStartIsIdempotent(t *testing.T) {
	var r recorder
	w := maintenance.New(maintenance.Options{
		Now:      fixed(base),
		Interval: time.Millisecond,
		Jobs:     []maintenance.Job{{Name: "rows", Run: r.run}},
	})
	w.Start(t.Context())
	w.Start(t.Context()) // a second loop would double every delete
	defer w.Stop()

	time.Sleep(50 * time.Millisecond)
	w.Stop()
	settled := r.calls()
	time.Sleep(20 * time.Millisecond)
	if r.calls() != settled {
		t.Fatal("a second loop survived Stop")
	}
}

// A retention of zero means "use the default", never "delete everything on
// the next tick". The engine's own config validation refuses a value below
// one day, so this floor is for a caller that built its stores directly.
func TestAZeroConversationRetentionTakesTheDefault(t *testing.T) {
	for _, asked := range []time.Duration{0, -time.Hour} {
		jobs := maintenance.LedgerJobs(stubConversations{}, asked)
		var found bool
		for _, j := range jobs {
			if j.Name != "conversation_sessions" {
				continue
			}
			found = true
			if j.Horizon != maintenance.ConversationRetention {
				t.Fatalf("a retention of %v became %v, want the default %v",
					asked, j.Horizon, maintenance.ConversationRetention)
			}
		}
		if !found {
			t.Fatalf("a retention of %v dropped the job entirely", asked)
		}
	}
	// A real horizon is used as written.
	jobs := maintenance.LedgerJobs(stubConversations{}, 72*time.Hour)
	for _, j := range jobs {
		if j.Name == "conversation_sessions" && j.Horizon != 72*time.Hour {
			t.Fatalf("a configured horizon became %v", j.Horizon)
		}
	}
}

// stubDiary remembers the clock it was swept on.
type stubDiary struct {
	nows []time.Time
	caps []int
}

func (d *stubDiary) Expire(_ context.Context, now time.Time) (int64, error) {
	d.nows = append(d.nows, now)
	return 3, nil
}

func (d *stubDiary) TrimLong(_ context.Context, cap int) (int64, error) {
	d.caps = append(d.caps, cap)
	return 2, nil
}

// The diary sweep hands the tick's own clock through, not a cutoff: each
// short-term entry carries its deadline in the row, so there is no horizon
// to derive one from, and a sweep that invented one would delete on the
// wrong clock.
func TestTheDiarySweepRunsOnNowNotACutoff(t *testing.T) {
	d := &stubDiary{}
	jobs := maintenance.LearningJobs(d)
	if len(jobs) != 2 || jobs[0].Name != "agent_diary" || jobs[1].Name != "agent_diary_long" {
		t.Fatalf("jobs = %+v, want the diary's expiry and its durable trim", jobs)
	}
	// The durable half is bounded by a COUNT, so it has no horizon and
	// ignores both clocks: a fact the agent marked durable has no deadline
	// to pass.
	if jobs[1].Horizon != 0 {
		t.Errorf("the trim declares a horizon (%v); it is capped, not aged", jobs[1].Horizon)
	}
	if n, err := jobs[1].Run(context.Background(), base, base); err != nil || n != 2 {
		t.Fatalf("trim Run = (%d, %v), want (2, nil)", n, err)
	}
	// Zero means "the shipped cap", decided by the diary rather than here.
	if len(d.caps) != 1 || d.caps[0] != 0 {
		t.Errorf("the trim passed caps %v, want the store's own default", d.caps)
	}
	if jobs[0].Horizon != 0 {
		t.Fatalf("Horizon = %v, want 0: each entry carries its own deadline", jobs[0].Horizon)
	}
	n, err := jobs[0].Run(context.Background(), base, base.Add(-time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("Run = (%d, %v), want (3, nil)", n, err)
	}
	if len(d.nows) != 1 || !d.nows[0].Equal(base) {
		t.Fatalf("Expire saw %v, want the tick's now %v", d.nows, base)
	}
}

// A missing store contributes no job rather than one that fails every tick:
// a deployment without one is real, and its in-memory twins prune inline.
func TestAbsentLedgersContributeNoJobs(t *testing.T) {
	if jobs := maintenance.LedgerJobs(nil, time.Hour); len(jobs) != 0 {
		t.Fatalf("nil stores produced %d jobs", len(jobs))
	}
	if jobs := maintenance.StoreJobs(nil); len(jobs) != 0 {
		t.Fatalf("a nil database produced %d jobs", len(jobs))
	}
	if jobs := maintenance.ChannelJobs(nil); len(jobs) != 0 {
		t.Fatalf("a nil channel store produced %d jobs", len(jobs))
	}
	if jobs := maintenance.ScheduleJobs(nil); len(jobs) != 0 {
		t.Fatalf("a nil schedule ledger produced %d jobs", len(jobs))
	}
	if jobs := maintenance.LearningJobs(nil); len(jobs) != 0 {
		t.Fatalf("a nil diary produced %d jobs", len(jobs))
	}
}
