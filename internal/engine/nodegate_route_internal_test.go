package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN EVICTION ON A LOG THIS NODE DOES NOT SERVE REACHES A NODE THAT SERVES IT.
//
// The node an operator asks serves no partition of the layout and runs no log;
// node-p serves and runs them all. The gesture is judged once, on the asking
// node, and each log's record is carried by the estate's router as
// `statelog.gate` to node-p, whose own write authority on that log publishes
// it — so the eviction lands on every log, and each answer names node-p as the
// log's writer. Before the router carried the operation, every one of these
// logs answered not_holder and the gesture was unfinished everywhere.
//
// node-p answers through THE ENGINE'S OWN BACKENDS — the [localEstate] a data
// node's server is handed, and the backend [Engine.partitionBackend] builds per
// request — so the serving half is the wiring production runs, not a copy of
// it: a partition backend that left out its gate writers, or a local estate
// refusing a sound copy, fails here.
//
// Under layout 0 — what every fleet runs today, where a data node whose copy is
// wrong serves nothing — the case also reads node-p's rows: the evicted node's
// tombstone is on every log. (Under a divided layout the tracker's eviction
// rows are not yet keyed by its partition's log, which is the tracker's own
// partitioning to come, so only the answers are read there.)
func TestAnEvictionOnALogThisNodeDoesNotServeReachesAServingHolder(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		boot     func(t *testing.T) (*Engine, *stateLog)
		logs     []string
		readRows bool
	}{
		{"layout 0", aLayoutZeroStateLog, []string{"tracker", "pages"}, true},
		{"a divided layout", func(t *testing.T) (*Engine, *stateLog) {
			e, s, _ := aPartitionedStateLog(t)
			return e, s
		}, []string{"tracker@tracker.000", "tracker@tracker.001", "pages@pages.000"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, s := c.boot(t)
			res := evictThroughTheRouter(t, e, s, "node-gone")
			var reached []string
			for _, d := range res.Domains {
				if d.Err != nil || d.Outcome != statelog.OutcomeApplied || d.Writer != s.nodeID {
					t.Errorf("%s answered %s by %q (%v), want applied by %s", d.Domain,
						d.Outcome, d.Writer, d.Err, s.nodeID)
					continue
				}
				reached = append(reached, d.Domain)
			}
			if !slices.Equal(reached, c.logs) || !res.Complete() {
				t.Fatalf("the eviction reached %v, complete %v; want every log %v", reached,
					res.Complete(), c.logs)
			}
			if !c.readRows {
				return
			}
			for _, id := range s.layout.AllLogs() {
				domain, err := registeredDomain(id.Domain)
				if err != nil || !domain.ClaimsIdentity() {
					continue
				}
				tombs, read := logTombstones(t.Context(), e.backends.Store, domain, id.Partition, 0)
				if !read || !slices.ContainsFunc(tombs, func(tomb statelog.Tombstone) bool {
					return tomb.NodeID == "node-gone" && tomb.By == "ops"
				}) {
					t.Errorf("%s's rows on %s hold %+v (read %v), want node-gone evicted by ops",
						id, s.nodeID, tombs, read)
				}
			}
		})
	}
}

// A READMISSION IS JUDGED ON EVERY LOG IT WRITES, THE ONES SERVED ELSEWHERE TOO.
//
// The node an operator asks runs no log, so every log's record goes through the
// router to node-p — and so must every log's JUDGEMENT. One log has a trim
// floor the readmitted node is below (it never reported a position, and the
// map names it a holder there, so it would be counted at zero): the gesture is
// refused naming that log, and nothing is written on any log. Judged only on
// the logs the asking node runs — none — the readmission went through, and its
// routed records put back on every log the very pin the eviction lifted, with
// nobody told. The bound is read where the record would be written: node-p's
// fence, at the generation node-p runs the log at.
func TestAReadmissionIsJudgedOnTheLogsItSendsElsewhere(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		boot    func(t *testing.T) (*Engine, *stateLog)
		trimmed string
	}{
		{"layout 0", aLayoutZeroStateLog, "tracker"},
		{"a divided layout", func(t *testing.T) (*Engine, *stateLog) {
			e, s, _ := aPartitionedStateLog(t)
			return e, s
		}, "tracker@tracker.000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e, s := c.boot(t)
			const node = "node-stranger"
			gate := aGateServedElsewhere(t, e, s, node)
			evict := func(name string) {
				t.Helper()
				evicted, err := gate.Evict(t.Context(), GateRequest{Node: node, By: "ops",
					OpID: statelog.NewOpID(time.Now(), name)})
				if err != nil || !evicted.Complete() {
					t.Fatalf("evict %s through node-p = %+v (%v), want complete", node, evicted, err)
				}
			}
			// THE CONTROL: with no floor anywhere, the same routed judgement
			// clears the node and every log takes it back through node-p.
			evict("evict-first")
			back, err := gate.Readmit(t.Context(), GateRequest{Node: node, By: "ops",
				OpID: statelog.NewOpID(time.Now(), "readmit-first")})
			if err != nil || !back.Complete() {
				t.Fatalf("with no floor the readmission answered %+v (%v), want complete", back, err)
			}
			evict("evict-again")
			running := s.Log(c.trimmed)
			at := running.runner.Committed()
			if err := s.fleet.PutFloor(t.Context(), coord.TrimFloor{
				Domain: running.key, Layout: s.layout.Number, Generation: at.Generation,
				TrimTo: 1000, Floor: 1000,
			}); err != nil {
				t.Fatalf("publish a floor on %s: %v", running.key, err)
			}
			before := endsByKey(t, s)

			res, err := gate.Readmit(t.Context(), GateRequest{Node: node, By: "ops",
				OpID: statelog.NewOpID(time.Now(), "readmit-again")})
			var refusal *statelog.ReadmissionRefusal
			if !errors.As(err, &refusal) || refusal.Domain != c.trimmed {
				t.Fatalf("the readmission of a node below %s's floor answered %+v (%v), "+
					"want refused on %s", c.trimmed, res, err, c.trimmed)
			}
			if refusal.Bound.Floor != 1000 || refusal.Bound.Generation != at.Generation {
				t.Errorf("the refusal judged against %+v, want node-p's floor 1000 at "+
					"generation %d", refusal.Bound, at.Generation)
			}
			if after := endsByKey(t, s); !maps.Equal(after, before) {
				t.Errorf("a refused readmission wrote: the logs ended at %v and now at %v",
					before, after)
			}
		})
	}
}

// endsByKey is every identity-claiming log s runs, at its last sequence on the
// broker, by the log's key — two logs of one domain under a divided layout.
func endsByKey(t *testing.T, s *stateLog) map[string]uint64 {
	t.Helper()
	ends := map[string]uint64{}
	for _, running := range s.running() {
		if !running.domain.ClaimsIdentity() {
			continue
		}
		_, last, err := running.log.Bounds(t.Context())
		if err != nil {
			t.Fatalf("read %s's end: %v", running.key, err)
		}
		ends[running.key] = last
	}
	return ends
}

// evictThroughTheRouter evicts node from a node that serves nothing and runs no
// log, over s's register, through a router whose one serving holder is s's
// node — serving every partition from s through the engine's own backends.
func evictThroughTheRouter(t *testing.T, e *Engine, s *stateLog, node string) GateResult {
	t.Helper()
	gate := aGateServedElsewhere(t, e, s, node)
	res, err := gate.Evict(t.Context(), GateRequest{Node: node, By: "ops",
		OpID: statelog.NewOpID(time.Now(), "evict-"+node)})
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	return res
}

// aGateServedElsewhere is the node gate of "node-a", a node that serves no
// partition of s's layout and runs no log, over s's register — whose every log
// is reached through the estate's router, at s's node, which serves every
// partition and answers each request through THE ENGINE'S OWN BACKENDS: the
// [localEstate] its server is handed in production ([Engine.serveEstate]), over
// a native runtime whose state log is s. So what answers a routed gate record
// is [localEstate.For] and [Engine.partitionBackend] as a data node runs them,
// never a backend the test built. node is named a holder of every partition.
func aGateServedElsewhere(t *testing.T, e *Engine, s *stateLog, node string) *NodeGate {
	t.Helper()
	q, ok := e.backends.Queue.(interface {
		estate.Asker
		estate.Server
	})
	if !ok {
		t.Fatalf("the queue %T neither asks nor serves", e.backends.Queue)
	}
	e.native.Store(&native{nodeID: s.nodeID, log: s})
	t.Cleanup(func() { e.native.Store(nil) })
	placement := servedBy{layout: s.layout, node: s.nodeID}
	stop, err := estate.Serve(t.Context(), q, s.nodeID, newLocalEstate(e, s.holding), placement,
		e.serverSeams())
	if err != nil {
		t.Fatalf("serve %s: %v", s.nodeID, err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	router, err := estate.NewRouter(estate.RouterOptions{Self: "node-a", Queue: q,
		Placement: placement, Session: estate.NewSession()})
	if err != nil {
		t.Fatal(err)
	}
	asker := &stateLog{layout: s.layout, holding: statelog.ServesOnly(), fleet: s.fleet,
		mode: s.mode}
	everywhere := fixedHolders{}
	for _, p := range s.layout.Partitions() {
		everywhere[p] = []statelog.Presence{{NodeID: node}}
	}
	gate, err := newNodeGate(asker, e.backends.Coord, everywhere, nil, router,
		e.backends.Store, "node-a", nil)
	if err != nil {
		t.Fatalf("the node gate: %v", err)
	}
	return gate
}

// aLayoutZeroStateLog boots a node with no company and starts a state log at
// layout 0 over its backends: one partition, every log.
func aLayoutZeroStateLog(t *testing.T) (*Engine, *stateLog) {
	t.Helper()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	e, err := New(t.Context(), Options{Bootstrap: &b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	s, err := e.startStateLogAt(t.Context(), &b, "node-p", nil, LayoutZero())
	if err != nil {
		t.Fatalf("start the state log: %v", err)
	}
	t.Cleanup(s.Stop)
	return e, s
}

// servedBy is a placement in which one node serves every partition of layout.
type servedBy struct {
	layout statelog.Layout
	node   string
}

func (p servedBy) Layout() (statelog.Layout, error) { return p.layout, nil }

func (p servedBy) Serving(statelog.PartitionID) ([]string, uint64, error) {
	return []string{p.node}, 1, nil
}

func (servedBy) Refresh(context.Context) error { return nil }
func (servedBy) Unanswered(string)             {}

// EACH PHASE'S BUDGET COVERS WHAT THAT PHASE WAITS ON.
//
// The logs: a log this node does not write is asked of its partition's serving
// holders one at a time, each for at most one [estate.AppendAttempt]. At the
// company's default copies a partition mid-move has one serving holder more —
// the joiner serves before the leaver stops — and a budget shorter than a walk
// past all of them ends the gesture before the last one's answer, with some of
// its logs written. The map: one election of the coordination store's group,
// jsprovision's clustered ask term, the one thing that stalls its round trips.
// The judgement: that election, and a bound read past one silent holder with
// room for the next one's answer — a judgement cut short has written nothing.
func TestTheGateBudgetsCoverWhatEachPhaseWaitsOn(t *testing.T) {
	t.Parallel()
	election := jsprovision.AskTerm(true)
	walk := time.Duration(config.DefaultEstateReplicas+1) * estate.AppendAttempt
	if GateLogBudget < walk {
		t.Errorf("GateLogBudget is %v, and a walk past the %d serving holders of a "+
			"partition mid-move takes %v", GateLogBudget, config.DefaultEstateReplicas+1, walk)
	}
	if GateMapBudget < election {
		t.Errorf("GateMapBudget is %v, shorter than one election of a replicated group (%v)",
			GateMapBudget, election)
	}
	if stalls := election + estate.ReadAttempt; GateJudgeBudget <= stalls {
		t.Errorf("GateJudgeBudget is %v, which an election's stall and one silent "+
			"holder (%v) spend before the next holder can answer", GateJudgeBudget, stalls)
	}
}

// THE MAP'S PART IS WRITTEN WHATEVER THE LOGS' WALK TOOK.
//
// The logs and the map have budgets of their own. Here a log's holders are
// silent for the logs' whole budget — a walk that spent all of it, as one past
// every holder of a partition mid-move can — and the eviction's bar still
// lands, on a context of its own. Bounded by what the walk left of one shared
// deadline, the map was handed an expired context and the bar was never
// written, and a retry walked the same silent holders first and missed it
// again.
func TestTheMapsPartIsWrittenWhateverTheLogsWalkTook(t *testing.T) {
	t.Parallel()
	written := 0
	recorder := &mapRecorder{logsWritten: &written}
	silent := gateLog{domain: "tracker@tracker.000", stream: "tracker@tracker.000",
		route: func(ctx context.Context, _, _, _ string, _ bool) (statelog.Result, string, error) {
			<-ctx.Done()
			return statelog.Result{}, "", ctx.Err()
		}}
	g := &NodeGate{
		logs:         gateLogs(silent),
		estate:       recorder,
		live:         func(context.Context) ([]statelog.Presence, error) { return nil, nil },
		readmissible: func(context.Context, string) error { return nil },
		publishing:   func(string) error { return nil },
		budgets:      gateBudgets{logs: 50 * time.Millisecond},
	}
	res, err := g.Evict(t.Context(), GateRequest{Node: "node-away", By: "ops",
		OpID: statelog.NewOpID(time.Now(), "evict-past-the-walk")})
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if res.Domains[0].Err == nil {
		t.Fatalf("the silent log answered %+v, want the walk spent", res.Domains[0])
	}
	if res.Map == nil || !res.Map.Landed || res.Map.Err != nil {
		t.Errorf("after a walk that spent the logs' budget the map answered %+v, want the "+
			"bar written", res.Map)
	}
}

// A GATE RECORD THAT IS NOT A READMISSION IS NOT THEREBY AN EVICTION.
//
// The serving half reads the sign of a record that arrived over the wire from
// its named kind, and a kind this build does not write — none, or one a later
// build adds — is refused rather than published as the eviction a missing flag
// used to decode to. The remedy says nothing landed and names the node that
// refused it.
func TestAGateRecordOfAnUnknownKindIsNeverAnEviction(t *testing.T) {
	t.Parallel()
	for kind, want := range map[estate.GateKind]bool{estate.GateEvict: false, estate.GateReadmit: true} {
		if got, err := readmits(kind); err != nil || got != want {
			t.Errorf("a record of kind %q reads as readmit=%v (%v), want %v", kind, got, err, want)
		}
		if gateKind(want) != kind {
			t.Errorf("readmit=%v is sent as %q, want %q", want, gateKind(want), kind)
		}
	}
	for _, kind := range []estate.GateKind{"", "release", "Evict"} {
		if got, err := readmits(kind); !errors.Is(err, estate.ErrGateKind) {
			t.Errorf("a record of kind %q reads as readmit=%v (%v), want ErrGateKind", kind, got, err)
		}
	}
	d := DomainGate{Domain: "tracker", Stream: "CREWLET_TRACKER_LOG", Writer: "node-old",
		Err: fmt.Errorf("refused: %w", estate.ErrGateKind)}
	remedy := d.Remedy()
	if !remedy.Offers(statelog.GateRetrySameOp) || !strings.Contains(remedy.Detail, "node-old") {
		t.Errorf("a kind the writer refused is remedied %+v, want the same gesture again, "+
			"naming node-old", remedy)
	}
}
