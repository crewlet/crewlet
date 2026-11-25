package engine

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// seatOnlyEngine is a node holding seats and no data, as a satellite or an
// operator's seat pod runs: no state log, so no retention heartbeat.
func seatOnlyEngine(roles ...string) *Engine {
	b := config.DefaultBootstrap()
	b.Node.Roles = roles
	return &Engine{boot: &b, testNode: "sat-1", objectives: newObjectives(nil), backends: &Backends{}}
}

// failCarries records n launches whose session a fault lost: bad events of
// session_carry, the objective a seat's run on this node measures.
func failCarries(e *Engine, n int) {
	for range n {
		e.objectives.RecordOutcome(objectives.SessionCarry, false)
	}
}

// standing is the kinds this node's own table has up, failing where it has
// never evaluated one.
func standing(t *testing.T, e *Engine) []statelog.Kind {
	t.Helper()
	kinds, evaluated := e.Alarms()
	if !evaluated {
		t.Fatal("the node has evaluated no alarm table")
	}
	return kinds
}

// presenceSaid is this node's status as its presence beat publishes it and a
// peer reads it back.
func presenceSaid(t *testing.T, e *Engine) coord.NodeStatus {
	t.Helper()
	status, err := e.nodeStatus(t.Context())
	if err != nil {
		t.Fatalf("the heartbeat's status: %v", err)
	}
	got, ok := coord.StatusFromMeta(map[string]any{coord.StatusKey: status.Meta()})
	if !ok {
		t.Fatal("the heartbeat carried no status")
	}
	return got
}

// A SEAT-ONLY NODE WHOSE OBJECTIVE BURNS RAISES objective_burn ON ITS OWN
// TABLE, and says so on its presence beat: the marks the heartbeat publishes
// ([coord.NodeStatus.Alarms]) are the tracker's, evaluated, with the kind.
// It is the node that measured, so nobody else can raise it.
//
// Mutation: start no loop on a node without data, or judge a reading without
// the meter's burns, and the alarm is never raised.
func TestASeatOnlyNodesObjectiveBurnIsRaisedOnItsOwnTable(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := seatOnlyEngine("seats")
		failCarries(e, 10)
		e.startLocalAlarms(t.Context())
		defer e.stopLocalAlarms()
		synctest.Wait()
		if kinds := standing(t, e); !slices.Contains(kinds, statelog.KindObjectiveBurn) {
			t.Fatalf("a burning session_carry raised %v, want objective_burn", kinds)
		}
		if got := presenceSaid(t, e); !got.AlarmsEvaluated || !slices.ContainsFunc(got.Alarms,
			func(m coord.AlarmMark) bool { return m.Kind == string(statelog.KindObjectiveBurn) }) {
			t.Errorf("the presence beat carries %v (evaluated %v), want objective_burn", got.Alarms,
				got.AlarmsEvaluated)
		}
	})
}

// A SEAT-ONLY NODE WITH NOTHING AMISS SAYS SO: its table is evaluated and holds
// nothing, which a reader reads as a quiet node, never as one not saying.
//
// Mutation: publish the tracker only once something fires, and the node reads
// as not saying.
func TestASeatOnlyNodeWithNothingAmissSaysSoAndRaisesNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := seatOnlyEngine("seats")
		e.startLocalAlarms(t.Context())
		defer e.stopLocalAlarms()
		synctest.Wait()
		if kinds := standing(t, e); len(kinds) != 0 {
			t.Errorf("a node with nothing amiss raised %v", kinds)
		}
		if got := presenceSaid(t, e); !got.AlarmsEvaluated || len(got.Alarms) != 0 {
			t.Errorf("the presence beat carries %v (evaluated %v), want an empty list said", got.Alarms,
				got.AlarmsEvaluated)
		}
	})
}

// A SEAT-ONLY NODE'S BURN CLEARS ONCE ITS SHORT WINDOW HEALS, as a data node's
// does: the loop keeps evaluating, and a fault that has ended is no alarm. The
// clear is kept as an episode under the node's own name, as the raise is.
//
// Mutation: evaluate once at start and never again, or hand the tracker no
// node, and a check fails.
func TestASeatOnlyNodesBurnClearsOnceItsShortWindowHeals(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		e := seatOnlyEngine("seats")
		failCarries(e, 10)
		episodes := &episodeLog{done: make(chan struct{})}
		measures := nodeMeasures{objectives: e.objectives}
		e.newLocalAlarms(measures, func() (estate.AdvisorWriter, bool) { return episodes, true }).
			start(t.Context(), e)
		defer e.stopLocalAlarms()
		synctest.Wait()
		if kinds := standing(t, e); !slices.Contains(kinds, statelog.KindObjectiveBurn) {
			t.Fatalf("premise: a burning session_carry raised %v", kinds)
		}
		// PAST BOTH SHORT WINDOWS, the slow pair's thirty minutes the longer:
		// the bad events are behind them, so neither pair fires.
		time.Sleep(31 * time.Minute)
		synctest.Wait()
		if kinds := standing(t, e); slices.Contains(kinds, statelog.KindObjectiveBurn) {
			t.Fatalf("a burn whose short windows healed still stands: %v", kinds)
		}
		<-episodes.done
		episodes.mu.Lock()
		defer episodes.mu.Unlock()
		if len(episodes.raised) != 1 || episodes.raised[0].Node != "sat-1" ||
			episodes.raised[0].Kind != string(statelog.KindObjectiveBurn) {
			t.Errorf("the raise was kept as %+v, want objective_burn on sat-1", episodes.raised)
		}
		if len(episodes.cleared) != 1 || !strings.HasPrefix(episodes.cleared[0], "sat-1/objective_burn/") {
			t.Errorf("the clear was kept as %v, want objective_burn on sat-1", episodes.cleared)
		}
	})
}

// A SEAT-ONLY NODE WHOSE VOLUME IS LOW RAISES volume_low, from the one measure
// of its store a data node's reading takes too ([nodeMeasures]).
//
// Mutation: leave the store out of the node's measures, and nothing is raised.
func TestASeatOnlyNodeWhoseVolumeIsLowRaisesVolumeLow(t *testing.T) {
	t.Parallel()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "node.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	e := seatOnlyEngine("seats")
	measures := nodeMeasures{db: db, volumes: func(string) (string, int64, error) { return "scratch", 1, nil }}
	l := e.newLocalAlarms(measures, func() (estate.AdvisorWriter, bool) { return nil, false })
	l.beat(t.Context())
	kinds, evaluated := l.tracker.Standing()
	if !evaluated || !slices.Contains(kinds, statelog.KindVolumeLow) {
		t.Errorf("a store on a volume with a byte free raised %v (evaluated %v), want volume_low", kinds,
			evaluated)
	}
}

// A DATA NODE RUNS NO SECOND TABLE: its retention heartbeat evaluates every
// kind, and a second loop beside it would raise each local kind twice, each
// with its own wakes and episodes.
//
// Mutation: start the loop on every node, and a data node runs one.
func TestADataNodeRunsNoSecondTable(t *testing.T) {
	t.Parallel()
	for _, roles := range [][]string{{"data", "seats"}, {"data", "ingress", "seats", "workers"}} {
		e := seatOnlyEngine(roles...)
		e.startLocalAlarms(t.Context())
		if e.localAlarms.Load() != nil || e.alarms.Load() != nil {
			e.stopLocalAlarms()
			t.Errorf("a node with roles %v started a table of its own beside its retention heartbeat", roles)
		}
	}
}

// BenchmarkALocalAlarmPass is one pass of a seat-only node's table: its own
// measures read (a store's files and volume, the recorder's window, the
// meter's burns) and the local rules judged and observed. Its figure is
// stated at [localAlarms.run].
func BenchmarkALocalAlarmPass(b *testing.B) {
	db := storetest.OpenNode(b, filepath.Join(b.TempDir(), "node.db"), store.Options{})
	b.Cleanup(func() { _ = db.Close() })
	recorder, err := metrics.New()
	if err != nil {
		b.Fatalf("recorder: %v", err)
	}
	e := seatOnlyEngine("seats")
	e.metrics, e.objectives = recorder, newObjectives(recorder)
	for i := range 200 {
		recorder.Observe(metrics.StorePoolWait, time.Duration(i)*time.Millisecond, metrics.Attrs{"file": "node"})
		e.objectives.RecordOutcome(objectives.SessionCarry, i%50 != 0)
	}
	l := e.newLocalAlarms(nodeMeasures{db: db, metrics: recorder, objectives: e.objectives},
		func() (estate.AdvisorWriter, bool) { return nil, false })
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		l.beat(ctx)
	}
}
