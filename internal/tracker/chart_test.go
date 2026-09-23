package tracker_test

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A PROJECT EXISTS BECAUSE THE CHART SAYS SO, and nothing else makes one.
//
// A create is a sequence: it takes the next number from its project's own
// counter and only then writes the task. So a project that is not an object
// refuses every write into it — which, before the chart apply was wired, is
// what a company that had just booted did with the first task anybody filed.
//
// Creating the project inside the create instead is the shape that gives two
// nodes two projects, two counters and two ENG-1s when they file at once, so
// the chart is the one writer.
func TestTheChartIsWhatMakesAProjectExist(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)

	// BEFORE: the project is not there and a create says so, as
	// unavailable rather than as a failure — this node cannot tell "never
	// created" from "not applied here yet", and only one of those is
	// worth retrying.
	_, err := r.writer.CreateTask(t.Context(), "op-early", newTask("t-early"), nil)
	if err == nil {
		t.Fatal("a task was filed into a project that does not exist")
	}

	wrote, err := r.writer.ApplyChart(t.Context(), activation(0), []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Purpose: "builds it", Unit: "Engineering"},
	})
	if err != nil {
		t.Fatalf("ApplyChart: %v", err)
	}
	if len(wrote) != 1 || wrote[0] != "ENG" {
		t.Fatalf("the chart apply wrote %v, want [ENG]", wrote)
	}
	r.drain()

	// AFTER: the same create lands, which is the whole point.
	got, err := r.writer.CreateTask(t.Context(), "op-late", newTask("t-late"), nil)
	if err != nil {
		t.Fatalf("CreateTask after the chart apply: %v", err)
	}
	if got.Key != "ENG-1" {
		t.Errorf("the first task in ENG was keyed %q", got.Key)
	}
}

// A SECOND APPLY OF ONE ACTIVATION WRITES NOTHING.
//
// The apply runs on every config apply and on every boot, on every node. If
// each of those were a record, a fleet of five restarting would put five
// identical projects on the log and the trim would carry them for its whole
// window — for a value nobody changed.
func TestReapplyingOneChartWritesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	chart := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}

	if _, err := r.writer.ApplyChart(t.Context(), activation(0), chart); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	r.drain()
	end := r.logEnd(t)
	wrote, err := r.writer.ApplyChart(t.Context(), activation(0), chart)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if len(wrote) != 0 {
		t.Errorf("re-applying one activation wrote %v — every boot of every node "+
			"would put a record on the log for a value nobody changed", wrote)
	}
	if got := r.logEnd(t); got != end {
		t.Errorf("re-applying one activation put %d record(s) on the log", got-end)
	}

	// A LATER ACTIVATION THAT CHANGES SOMETHING DOES write.
	changed := []tracker.ChartProject{{Key: "ENG", Name: "Engineering & Ops", Unit: "Eng"}}
	if wrote, err = r.writer.ApplyChart(t.Context(), activation(1), changed); err != nil {
		t.Fatalf("third apply: %v", err)
	}
	if len(wrote) != 1 {
		t.Errorf("a renamed project was not written: %v", wrote)
	}
	r.drain()

	// AND AN OLDER ACTIVATION ARRIVING LATE DOES NOT WALK IT BACK. Two
	// nodes applying two revisions is ordinary during a rollout, and the
	// node that is behind must not undo the one that is ahead.
	if wrote, err = r.writer.ApplyChart(t.Context(), activation(0), chart); err != nil {
		t.Fatalf("stale apply: %v", err)
	}
	if len(wrote) != 0 {
		t.Errorf("an older chart overwrote a newer one: %v", wrote)
	}
	r.drain()

	if name := r.projectName("ENG"); name != "Engineering & Ops" {
		t.Errorf("the project's name is %q — the newer chart lost", name)
	}
}

// A PROJECT IS STAMPED WITH THE INSTANT ITS CONFIGURATION WAS ACTIVATED, to
// the millisecond — never the instant of the apply, and never a second's
// resolution.
//
// Two activations inside one second are an ordinary thing for a script to do.
// At a second's resolution they share an epoch, and the guard lets an EQUAL
// epoch through — so a node still applying the first, landing after the
// second's record, walked the second's names back.
func TestAChartIsStampedWithItsActivationToTheMillisecond(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	first := activation(0)
	second := first.Add(300 * time.Millisecond)

	if _, err := r.writer.ApplyChart(t.Context(), second, []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering & Ops", Unit: "Eng"},
	}); err != nil {
		t.Fatalf("the later activation's apply: %v", err)
	}
	r.drain()
	if got, want := r.chartEpoch("ENG"), second.UnixMilli(); got != want {
		t.Fatalf("the project is stamped %d, want the activation's own %d", got, want)
	}

	// THE EARLIER ACTIVATION, arriving second, in the same second.
	wrote, err := r.writer.ApplyChart(t.Context(), first, []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "Eng"},
	})
	if err != nil {
		t.Fatalf("the earlier activation's apply: %v", err)
	}
	if len(wrote) != 0 {
		t.Fatalf("the earlier activation wrote %v — it was activated 300ms before "+
			"the one already applied, and walked its names back", wrote)
	}
	r.drain()
	if name := r.projectName("ENG"); name != "Engineering & Ops" {
		t.Errorf("the project's name is %q — the later activation lost", name)
	}
}

// A CHART APPLY MUST NAME ITS ACTIVATION. There is no honest default: the zero
// instant would stamp every project as older than any chart, and the apply's
// own clock is the defect the activation replaced.
func TestAChartApplyRefusesToGuessItsActivation(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	end := r.logEnd(t)
	wrote, err := r.writer.ApplyChart(t.Context(), time.Time{}, []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "Eng"},
	})
	if !errors.Is(err, tracker.ErrNoChartActivation) {
		t.Fatalf("an apply with no activation = (%v, %v), want ErrNoChartActivation",
			wrote, err)
	}
	if got := r.logEnd(t); got != end {
		t.Errorf("a refused apply put %d record(s) on the log", got-end)
	}
}

// A REAPPLY OF THE CURRENT ACTIVATION SETS RIGHT WHAT AN EQUAL EPOCH WALKED
// BACK — which is why a chart apply decides from the project's rows rather
// than letting the operation ledger answer it.
//
// Two activations inside one millisecond share an epoch, and the guard lets an
// equal epoch through, so a node applying the earlier one can land after the
// later one's record. The next apply of the later one — that node's own next
// tick, any node's next boot — has to put it back. An operation id derived
// from the activation is one the ledger already holds from the later one's
// first write, so it answered that reapply as done and the older names stood
// until the next activation.
func TestAReapplySetsRightWhatAnEqualEpochWalkedBack(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	at := activation(0)
	current := []tracker.ChartProject{{Key: "ENG", Name: "Engineering & Ops", Unit: "Eng"}}
	stale := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}

	if _, err := r.writer.ApplyChart(t.Context(), at, current); err != nil {
		t.Fatalf("the current activation's apply: %v", err)
	}
	r.drain()
	if _, err := r.writer.ApplyChart(t.Context(), at, stale); err != nil {
		t.Fatalf("the equal-epoch stale apply: %v", err)
	}
	r.drain()
	if name := r.projectName("ENG"); name != "Engineering" {
		t.Fatalf("the stale apply left %q — the premise of this case is an equal "+
			"epoch the guard lets through", name)
	}

	wrote, err := r.writer.ApplyChart(t.Context(), at, current)
	if err != nil {
		t.Fatalf("the reapply: %v", err)
	}
	r.drain()
	if len(wrote) != 1 {
		t.Errorf("the reapply wrote %v, want [ENG]", wrote)
	}
	if name := r.projectName("ENG"); name != "Engineering & Ops" {
		t.Errorf("after reapplying the current activation the project is named "+
			"%q — the stale names stand until somebody activates again", name)
	}
}

// A CHART WHOSE ACTIVATION IS OLDER THAN THE OPERATION LEDGER'S WATERMARK IS
// STILL DECIDED — which is most companies' steady state: a configuration
// nobody has changed for longer than the ledger keeps its rows.
//
// The operation is the APPLY, minted when it runs, not the activation. An id
// minted at the activation's instant is one the ledger can no longer vouch for
// once that instant is behind its watermark, so every boot of every node had
// every project answered `unknown` without being decided — and a project the
// chart names that is missing or wrong stayed so.
func TestAChartOlderThanTheLedgerIsStillDecided(t *testing.T) {
	t.Parallel()
	r := newRoundTripWithoutProject(t)
	// THE LEDGER HAS LOST ROWS UP TO A MINUTE AGO — a sweep, say — and the
	// activation is older than that. A minute rather than now, so the
	// apply's own mint, which an id resolves to the millisecond, is
	// unambiguously after it.
	if err := statelog.RecordLedgerLoss(t.Context(), r.db.Replicated(),
		tracker.Domain{}, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("record the ledger's watermark: %v", err)
	}
	wrote, err := r.writer.ApplyChart(t.Context(), activation(0), []tracker.ChartProject{
		{Key: "ENG", Name: "Engineering", Unit: "Eng"},
	})
	if err != nil {
		t.Fatalf("an apply of an activation older than the ledger's watermark: %v "+
			"— it is a reconcile decided from the project's rows, and nothing "+
			"about it needs the ledger to vouch", err)
	}
	if len(wrote) != 1 {
		t.Fatalf("the apply wrote %v, want [ENG]", wrote)
	}
	r.drain()
	if name := r.projectName("ENG"); name != "Engineering" {
		t.Errorf("the project is named %q", name)
	}
}

// TWO NODES APPLYING ONE ACTIVATION PUT ONE RECORD ON THE LOG — the second
// decided on rows that did not have the first's yet, lost the broker's
// arbitration, and re-decided on the rows the winner wrote.
func TestTwoNodesApplyingOneActivationWriteOnce(t *testing.T) {
	t.Parallel()
	a := newRoundTripWithoutProject(t)
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "node-b.db"),
		store.Options{PinnedWriters: 1})
	if err != nil {
		t.Fatalf("open node b's store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	b := newRoundTripOn(t, a.broker, a.log, db, "node-b")
	b.applyWhileWriting()
	chart := []tracker.ChartProject{{Key: "ENG", Name: "Engineering", Unit: "Eng"}}

	wroteA, err := a.writer.ApplyChart(t.Context(), activation(0), chart)
	if err != nil || len(wroteA) != 1 {
		t.Fatalf("node a's apply = (%v, %v), want [ENG]", wroteA, err)
	}
	end := a.logEnd(t)

	// NODE B HAS NOT APPLIED NODE A'S RECORD: it decides a create on rows
	// with no project in them.
	wroteB, err := b.writer.ApplyChart(t.Context(), activation(0), chart)
	if err != nil {
		t.Fatalf("node b's apply: %v — losing the arbitration to an identical "+
			"write is not a failure", err)
	}
	if len(wroteB) != 0 {
		t.Errorf("node b reports it wrote %v — its first decision lost the "+
			"arbitration and its second found the chart already there", wroteB)
	}
	if got := a.logEnd(t); got != end {
		t.Errorf("node b put %d record(s) on the log for a chart node a had "+
			"already written", got-end)
	}
	if name := b.projectName("ENG"); name != "Engineering" {
		t.Errorf("node b's project is named %q", name)
	}
}

// activation is the instant of the nth configuration activation of a case, in
// the order they were made.
func activation(n int) time.Time {
	return time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute)
}

// projectName reads one project's chart-owned name straight out of the rows,
// which is what a stale apply would have overwritten.
func (r *roundTrip) projectName(key string) string {
	r.t.Helper()
	var name string
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT name FROM tracker_projects WHERE key = ?`, key).Scan(&name)
	}); err != nil {
		r.t.Fatalf("read project %s: %v", key, err)
	}
	return name
}

// chartEpoch reads the epoch one project's chart-owned fields were last
// written at.
func (r *roundTrip) chartEpoch(key string) int64 {
	r.t.Helper()
	var epoch int64
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT chart_epoch FROM tracker_projects WHERE key = ?`, key).Scan(&epoch)
	}); err != nil {
		r.t.Fatalf("read project %s's chart epoch: %v", key, err)
	}
	return epoch
}
