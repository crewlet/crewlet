package maintenance_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/maintenance"
)

var base = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// logs is where this package's own log lines go while the suite runs.
//
// A TESTMAIN OWNS THE PROCESS'S LOGGING, which is what [logging.Configure]
// says about itself, and this package needs the lines rather than merely
// wanting them quiet: [maintenance.New] answers a horizon it had to raise
// with a WARNING and nothing else — the value it raises to is the one the
// caller already asked for — so the line IS the behaviour.
var logs = &syncBuffer{}

// syncBuffer is a writer a logger and a test may both touch.
//
// Go resumes a parallel test only once every sequential one has finished, so
// the reader here never overlaps a case that logs — but the detector reads
// the handler's own goroutine, and a buffer with no lock is a race whether or
// not anything ever interleaves.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// since returns what has been logged since a mark, and a mark for next time.
func (b *syncBuffer) since(mark int) (string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	all := b.buf.String()
	return all[mark:], len(all)
}

func TestMain(m *testing.M) {
	logging.Configure(slog.LevelWarn, logging.FormatText, logs)
	os.Exit(m.Run())
}

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
	w := newWorker(t, maintenance.Options{
		Now: fixed(base),
		Jobs: []maintenance.Job{
			{Name: "rows", Scope: maintenance.Fleet, Horizon: 2 * time.Hour, Run: r.run},
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
	w := newWorker(t, maintenance.Options{
		Now:  fixed(base),
		Jobs: []maintenance.Job{{Name: "events", Scope: maintenance.Fleet, Run: r.run}},
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
	w := newWorker(t, maintenance.Options{
		Now:      fixed(base),
		Interval: time.Hour,
		Jobs: []maintenance.Job{
			{Name: "shallow", Scope: maintenance.Fleet, Horizon: time.Minute, Run: r.run},
		},
	})

	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := r.lastCutoff(t); !got.Equal(base.Add(-time.Hour)) {
		t.Fatalf("cutoff %s, want the raised horizon %s", got, base.Add(-time.Hour))
	}
}

// A horizon sitting exactly ON the tick is the boundary the module's doc
// names, and it is reported rather than accepted in silence.
//
// Raising it changes no number — the value already IS the tick — so the
// warning is the whole of the behaviour, and that is the point: a retention
// equal to the sweep's own cadence is one the SWEEP decides rather than the
// caller, and the caller is the only one who will ever size a table from it.
// The condition was strictly below, so this case passed without a word; the
// state-log suite refuses a domain for exactly the same value.
//
// NOT PARALLEL, because it reads a process-wide log sink.
func TestAHorizonExactlyAtTheTickIsReported(t *testing.T) {
	_, mark := logs.since(0)
	newWorker(t, maintenance.Options{
		Now:      fixed(base),
		Interval: time.Hour,
		Jobs: []maintenance.Job{
			{Name: "on_the_floor", Scope: maintenance.Fleet, Horizon: time.Hour, Run: (&recorder{}).run},
		},
	})
	written, _ := logs.since(mark)
	if !strings.Contains(written, "maintenance_horizon_raised_to_the_tick") ||
		!strings.Contains(written, "on_the_floor") {
		t.Fatalf("a horizon equal to the tick logged %q, want the raise warning "+
			"naming the job — at the floor the value is the sweep's answer "+
			"rather than the caller's, and nothing else ever says so", written)
	}
}

// And a horizon comfortably above the tick says nothing, so the warning above
// is a verdict rather than a line every worker prints.
//
// NOT PARALLEL, for [TestAHorizonExactlyAtTheTickIsReported]'s reason.
func TestAHorizonAboveTheTickIsNotReported(t *testing.T) {
	_, mark := logs.since(0)
	newWorker(t, maintenance.Options{
		Now:      fixed(base),
		Interval: time.Hour,
		Jobs: []maintenance.Job{
			{Name: "roomy", Scope: maintenance.Fleet, Horizon: time.Hour + time.Second, Run: (&recorder{}).run},
		},
	})
	if written, _ := logs.since(mark); strings.Contains(written, "roomy") {
		t.Fatalf("a horizon above the tick logged %q, want silence", written)
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
	partial := recorder{rows: 3, err: boom}
	w := newWorker(t, maintenance.Options{
		Now: fixed(base),
		Jobs: []maintenance.Job{
			{Name: "first", Scope: maintenance.Fleet, Horizon: time.Hour, Run: first.run},
			{Name: "second", Scope: maintenance.Fleet, Horizon: time.Hour, Run: failing.run},
			{Name: "third", Scope: maintenance.Fleet, Horizon: time.Hour, Run: third.run},
			{Name: "partial", Scope: maintenance.Fleet, Horizon: time.Hour, Run: partial.run},
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
	// A job that touched rows before it failed keeps them: a retirement
	// that deleted three mailboxes and failed on a fourth deleted three.
	if swept["partial"] != 3 {
		t.Fatalf("a job that failed part-way reported %d rows, want the 3 it touched", swept["partial"])
	}
}

// The duty is claimed per tick, not held. A node that does not hold it must
// not sweep — N nodes deleting the same rows is what the singleton exists
// to prevent.
func TestANodeWithoutTheDutyDoesNotSweep(t *testing.T) {
	var r recorder
	var holds atomic.Bool
	w := newWorker(t, maintenance.Options{
		Now:       fixed(base),
		Jobs:      []maintenance.Job{{Name: "rows", Scope: maintenance.Fleet, Horizon: time.Hour, Run: r.run}},
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
	w := newWorker(t, maintenance.Options{
		Now:  fixed(base),
		Jobs: []maintenance.Job{{Name: "rows", Scope: maintenance.Fleet, Horizon: time.Hour, Run: r.run}},
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
	w := newWorker(t, maintenance.Options{
		Now:  fixed(base),
		Jobs: []maintenance.Job{{Name: "rows", Scope: maintenance.Fleet, Horizon: time.Hour, Run: (&recorder{}).run}},
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
	w := newWorker(t, maintenance.Options{
		Now:      fixed(base),
		Interval: time.Millisecond,
		Jobs:     []maintenance.Job{{Name: "rows", Scope: maintenance.Fleet, Run: r.run}},
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
	w := newWorker(t, maintenance.Options{
		Now:      fixed(base),
		Interval: time.Millisecond,
		Jobs: []maintenance.Job{{
			Name:  "slow",
			Scope: maintenance.Fleet,
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
	w := newWorker(t, maintenance.Options{Now: fixed(base)})
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

// A JOB THE WIRING GOT WRONG IS REFUSED, AND THE REFUSAL NAMES ALL OF THEM.
//
// It used to be dropped and the worker started without it, which is the
// silence this whole type exists to remove: the log lists the jobs it kept,
// the one it did not keep is not mentioned anywhere, and the table that job
// was for grows for the life of the deployment.
//
// The third case is the one that motivated the change. An unset Scope is not
// a malformed job in any way a compiler or a reviewer can see — it is a
// perfectly good job that silently became a fleet singleton, and six of the
// seven local sweeps in this package were exactly that.
func TestAnIncompleteJobIsRefused(t *testing.T) {
	var r recorder
	_, err := maintenance.New(maintenance.Options{
		Now: fixed(base),
		Jobs: []maintenance.Job{
			{Name: "", Scope: maintenance.Fleet, Horizon: time.Hour, Run: r.run},
			{Name: "nameless", Scope: maintenance.Fleet, Horizon: time.Hour},
			{Name: "unscoped", Horizon: time.Hour, Run: r.run},
			{Name: "real", Scope: maintenance.Fleet, Horizon: time.Hour, Run: r.run},
		},
	})
	if err == nil {
		t.Fatal("three malformed jobs were accepted, so a table each one was " +
			"for is swept by nothing and nothing says so")
	}
	// EVERY OFFENDER IN ONE MESSAGE. Reporting the first would make fixing
	// three a loop of three builds.
	for _, want := range []string{"no Name", "no Run", "Scope is"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "unscoped") {
		t.Errorf("the refusal does not name the unscoped job: %v", err)
	}
	if strings.Contains(err.Error(), "real") {
		t.Errorf("the refusal names the job that is fine: %v", err)
	}

	// AND THE CONTROL: the same list minus the offenders is accepted, so
	// this test fails on a New that refuses everything just as loudly as on
	// one that refuses nothing.
	w, err := maintenance.New(maintenance.Options{
		Now:  fixed(base),
		Jobs: []maintenance.Job{{Name: "real", Scope: maintenance.Fleet, Horizon: time.Hour, Run: r.run}},
	})
	if err != nil {
		t.Fatalf("a well-formed job was refused: %v", err)
	}
	if got := w.Jobs(); !slices.Equal(got, []string{"real"}) {
		t.Fatalf("Jobs = %v, want the one complete job", got)
	}
}

// THE SCOPE DECIDES WHERE A JOB RUNS, and this is the half that was broken.
// newWorker builds a worker for a test, failing at the call site rather than
// returning the error: every construction below is meant to succeed, and the
// one test that is about a refusal calls maintenance.New directly.
func newWorker(t *testing.T, opts maintenance.Options) *maintenance.Worker {
	t.Helper()
	w, err := maintenance.New(opts)
	if err != nil {
		t.Fatalf("build the worker: %v", err)
	}
	return w
}

func TestScopeValidHasNoValidZero(t *testing.T) {
	t.Parallel()
	for _, s := range []maintenance.Scope{maintenance.Fleet, maintenance.NodeLocal} {
		if !s.Valid() {
			t.Errorf("%q is one of the two answers and Valid says otherwise", s)
		}
	}
	for _, s := range []maintenance.Scope{"", "per_node", "PerNode", "node-local"} {
		if s.Valid() {
			t.Errorf("%q is not one of the two answers and Valid accepted it", s)
		}
	}
}

func TestStartIsIdempotent(t *testing.T) {
	var r recorder
	w := newWorker(t, maintenance.Options{
		Now:      fixed(base),
		Interval: time.Millisecond,
		Jobs:     []maintenance.Job{{Name: "rows", Scope: maintenance.Fleet, Run: r.run}},
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

// EACH LEDGER'S OWN HORIZON REACHES ITS OWN JOB, which is the whole of the
// repair: the sweep used to be handed ONE number for every domain, so a
// domain committing an order of magnitude more than the tracker kept an order
// of magnitude more table for the same thirty days. The ledger states it now,
// and a job carrying its neighbour's number is indistinguishable from a
// correct one until somebody measures the disk.
func TestEachOperationLedgerGetsItsOwnHorizon(t *testing.T) {
	t.Parallel()
	jobs := maintenance.StatelogJobs(map[string]maintenance.OpsLedger{
		"chatter": stubLedger{horizon: 7 * 24 * time.Hour},
		"census":  stubLedger{horizon: 30 * 24 * time.Hour},
	})
	want := map[string]time.Duration{
		// SORTED BY DOMAIN, so every node's sweep prints the same
		// order — a map's would differ between peers.
		"census_ops":  30 * 24 * time.Hour,
		"chatter_ops": 7 * 24 * time.Hour,
	}
	if len(jobs) != len(want) {
		t.Fatalf("built %d jobs for %d ledgers", len(jobs), len(want))
	}
	if jobs[0].Name != "census_ops" || jobs[1].Name != "chatter_ops" {
		t.Fatalf("jobs are %q then %q, want them sorted by domain",
			jobs[0].Name, jobs[1].Name)
	}
	for _, j := range jobs {
		if j.Horizon != want[j.Name] {
			t.Errorf("%s keeps rows for %v, want its ledger's own %v",
				j.Name, j.Horizon, want[j.Name])
		}
		if j.Scope != maintenance.NodeLocal {
			t.Errorf("%s is scoped %q — the rows record what THIS applier "+
				"wrote, so a singleton tidies one node and lets every peer "+
				"grow for ever", j.Name, j.Scope)
		}
	}
}

// stubLedger is an operation ledger that states a horizon and counts nothing.
type stubLedger struct{ horizon time.Duration }

func (s stubLedger) OpsRetention() time.Duration { return s.horizon }

func (stubLedger) PurgeOps(context.Context, time.Time) (int64, error) { return 0, nil }

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

// A PER-NODE JOB RUNS WITHOUT THE DUTY, AND A FLEET JOB DOES NOT.
//
// # The failure this exists to catch
//
// The duty exists to stop N nodes deleting the same rows, which is right for
// state the fleet shares and wrong for a table each node owns its own copy of.
// Under the singleton alone, a per-node job is swept on ONE node and grows for
// ever on all the others — and that looks identical to a sweep that is
// working, to the operator who checks the node that holds the duty.
func TestAPerNodeJobRunsWithoutTheDuty(t *testing.T) {
	t.Parallel()
	var fleet, mine int
	w := newWorker(t, maintenance.Options{
		ClaimDuty: func(context.Context) (bool, error) { return false, nil },
		Jobs: []maintenance.Job{
			{Name: "shared", Scope: maintenance.Fleet, Run: func(context.Context, time.Time, time.Time) (int64, error) {
				fleet++
				return 1, nil
			}},
			{Name: "mine", Scope: maintenance.NodeLocal,
				Run: func(context.Context, time.Time, time.Time) (int64, error) {
					mine++
					return 1, nil
				}},
		},
	})
	if _, err := w.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if fleet != 0 {
		t.Errorf("the fleet's own job ran %d time(s) on a node without the "+
			"duty, which is the N-nodes-deleting-one-row case the duty is for",
			fleet)
	}
	if mine != 1 {
		t.Errorf("this node's own job ran %d time(s) without the duty — its "+
			"table is this machine's, and skipping it lets every node but the "+
			"duty holder grow for ever", mine)
	}
}

// A GATE THAT CANNOT BE READ SKIPS ITS JOB AND SAYS SO.
//
// "I could not tell whether there is work" is not "there is no work", and
// treating it as the second is how a duty stops running with nothing to show
// for it.
func TestAnUnreadableGateIsReportedRatherThanReadAsNoWork(t *testing.T) {
	t.Parallel()
	ran := 0
	w := newWorker(t, maintenance.Options{
		Jobs: []maintenance.Job{
			{Name: "gated", Scope: maintenance.Fleet,
				Gate: func(context.Context) (bool, error) {
					return false, errors.New("the store is unreachable")
				},
				Run: func(context.Context, time.Time, time.Time) (int64, error) {
					ran++
					return 1, nil
				}},
			{Name: "quiet", Scope: maintenance.Fleet,
				Gate: func(context.Context) (bool, error) { return false, nil },
				Run: func(context.Context, time.Time, time.Time) (int64, error) {
					t.Error("a job whose gate said there is no work ran anyway")
					return 0, nil
				}},
		},
	})
	_, err := w.Tick(t.Context())
	if err == nil {
		t.Fatal("a gate that could not be read was reported as no work, so a " +
			"duty that has stopped running looks exactly like a quiet one")
	}
	if !strings.Contains(err.Error(), "gated") {
		t.Errorf("the error is %v and does not name the job whose gate failed", err)
	}
	if ran != 0 {
		t.Errorf("the job ran %d time(s) behind a gate that errored", ran)
	}
}
