package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
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

// evictThroughTheRouter evicts node from a node that serves nothing and runs no
// log, over s's register, through a router whose one serving holder is s's
// node — serving every partition from s, with its own write authority on each
// log.
func evictThroughTheRouter(t *testing.T, e *Engine, s *stateLog, node string) GateResult {
	t.Helper()
	q, ok := e.backends.Queue.(interface {
		estate.Asker
		estate.Server
	})
	if !ok {
		t.Fatalf("the queue %T neither asks nor serves", e.backends.Queue)
	}
	placement := servedBy{layout: s.layout, node: s.nodeID}
	stop, err := estate.Serve(t.Context(), q, s.nodeID, &servesAll{e: e, s: s}, placement,
		estate.ServerSeams{})
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
	res, err := gate.Evict(t.Context(), GateRequest{Node: node, By: "ops",
		OpID: statelog.NewOpID(time.Now(), "evict-"+node)})
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	return res
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

// servesAll is a node that serves every partition, answering only the
// `statelog.gate` operation's half — its own write authority on each log.
type servesAll struct {
	e    *Engine
	s    *stateLog
	cpus estate.CPUs
}

func (l *servesAll) For(_ context.Context, p statelog.PartitionID) (estate.Backend, bool, error) {
	return estate.Backend{Gates: l.e.partitionGates(l.s, p)}, true, nil
}

func (l *servesAll) CPUs() *estate.CPUs { return &l.cpus }

// THE GESTURE'S BUDGET COVERS A WALK PAST EVERY HOLDER A LOG HAS BY DEFAULT.
//
// A log this node does not write is asked of its partition's holders one at a
// time, each for at most one [estate.AppendAttempt]; at the company's default
// copies a partition has that many holders this node is not, and one whose copy
// lags is asked again last. A budget shorter than that walk ends a gesture on a
// fleet with one silent holder before the answer of the holder that would have
// written the log.
func TestTheGateBudgetCoversAWalkPastEveryDefaultHolder(t *testing.T) {
	t.Parallel()
	walk := time.Duration(config.DefaultEstateReplicas+1) * estate.AppendAttempt
	if GateBudget < walk {
		t.Errorf("GateBudget is %v, and a walk past %d holders with one asked again "+
			"last takes %v", GateBudget, config.DefaultEstateReplicas, walk)
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
