package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PARTITIONED TEST LAYOUT'S THREE PARTITIONS, named once for the cases
// below.
var (
	gateTracker0 = statelog.PartitionID{Space: statelog.SpaceTracker}
	gateTracker1 = statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}
	gatePages0   = statelog.PartitionID{Space: statelog.SpacePages}
)

// aGateStateLog is a state log at the partitioned test layout that runs no
// log, over a register holding rows: enough for the gesture to decide which
// logs it concerns and who writes each, and nothing that could write one.
func aGateStateLog(t *testing.T, holding statelog.Holding, rows ...coord.NodePositions) *stateLog {
	t.Helper()
	fleet := coordmem.NewFleet()
	for _, row := range rows {
		if err := fleet.PutPositions(t.Context(), row); err != nil {
			t.Fatalf("the register row of %s: %v", row.NodeID, err)
		}
	}
	return &stateLog{layout: partitionedTestLayout(), holding: holding, fleet: fleet}
}

// logKeys is each log's key, in order.
func logKeys(ids []statelog.LogID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// AN EVICTION IS WRITTEN ON EVERY LOG THE NODE IS COUNTED ON, AND ON NO OTHER.
//
// The trim waits for a node on a log whose partition the estate map names it
// a holder of — at zero, before it has reported anything there — and on a log
// its positions row names and it has not released. Those are exactly the logs
// an eviction has to lift its pin from: one it missed goes on counting a node
// the operator was told is gone, and one it wrote beyond them puts a tombstone
// on a log that never counted the node. So the node below holds tracker.001
// without having reported on it, reports on tracker.000 without the map naming
// it there, and has released pages.000 — and the gesture concerns the first
// two alone. Under layout 0 every identity-claiming log concerns it, which is
// the answer the gesture always had.
func TestAnEvictionIsWrittenOnEveryLogTheNodeIsCountedOn(t *testing.T) {
	t.Parallel()
	const away = "node-away"
	row := coord.NodePositions{NodeID: away, At: time.Now().UTC(), Layout: 1,
		Domains: map[string]coord.DomainPosition{
			"tracker@tracker.000": {Generation: 1, Seq: 5, AppliedThrough: 5},
			"pages@pages.000":     {Generation: 1, Seq: 3, AppliedThrough: 3, State: coord.LogReleased},
		}}
	s := aGateStateLog(t, statelog.ServesOnly(), row)
	holders := fixedHolders{
		gateTracker0: {{NodeID: "node-p"}},
		gateTracker1: {{NodeID: "node-p"}, {NodeID: away}},
		gatePages0:   {{NodeID: "node-p"}},
	}

	counted, err := s.countedOnLogs(t.Context(), holders, away)
	if err != nil {
		t.Fatalf("the logs %s is counted on: %v", away, err)
	}
	if got, want := logKeys(counted), []string{"tracker@tracker.000", "tracker@tracker.001"}; !slices.Equal(got, want) {
		t.Errorf("%s is counted on %v, want %v: the log its row names, the log the map "+
			"names it a holder of, and never the log it released", away, got, want)
	}

	// A HOLDER SET THAT CANNOT BE READ writes nothing: the map may name the
	// node on a log its row does not, and a gesture from the row alone would
	// leave it pinning that log while reporting itself complete.
	if _, err := s.countedOnLogs(t.Context(), unknownHolders{}, away); err == nil {
		t.Error("the logs a node is counted on were decided with the holders unreadable")
	}

	zero := &stateLog{layout: LayoutZero(), holding: statelog.ServesOnly(), fleet: coordmem.NewFleet()}
	counted, err = zero.countedOnLogs(t.Context(), nil, away)
	if err != nil {
		t.Fatalf("layout 0's logs: %v", err)
	}
	if got, want := logKeys(counted), []string{"tracker", "pages"}; !slices.Equal(got, want) {
		t.Errorf("under layout 0 a node with no row is counted on %v, want every "+
			"identity-claiming log %v", got, want)
	}
}

// A READMISSION IS WRITTEN ON EVERY IDENTITY-CLAIMING LOG.
//
// Where a node is evicted is a fact each log's own rows hold. A holder the
// eviction reached before it first reported was counted on a log its row
// never named, and the map lets an evicted node go — so by the time it is
// readmitted, neither the register nor the map says which logs hold its
// tombstone, and a readmission written from them would leave it evicted on a
// log that goes on to be placed on it. Every log is asked instead; one that
// never evicted the node changes no row.
func TestAReadmissionIsWrittenOnEveryIdentityLog(t *testing.T) {
	t.Parallel()
	const away = "node-away"
	s := aGateStateLog(t, statelog.ServesOnly())
	logs, err := countedGateLogs(t.Context(), s, fixedHolders{}, nil, "node-p", nil, away, true)
	if err != nil {
		t.Fatalf("the readmission's logs: %v", err)
	}
	var got []string
	for _, l := range logs {
		got = append(got, l.domain)
	}
	// THE REGISTER'S ORDER OF DOMAINS, each domain's logs by partition: the
	// order a gesture's answer has always listed its logs in.
	want := []string{"tracker@tracker.000", "tracker@tracker.001", "pages@pages.000"}
	if !slices.Equal(got, want) {
		t.Errorf("a readmission of a node counted nowhere concerns %v, want every "+
			"identity-claiming log %v", got, want)
	}
	logs, err = countedGateLogs(t.Context(), s, fixedHolders{}, nil, "node-p", nil, away, false)
	if err != nil {
		t.Fatalf("the eviction's logs: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("an eviction of a node counted nowhere concerns %d log(s), want none", len(logs))
	}
}

// A LOG THIS NODE DOES NOT SERVE IS LEFT TO A NODE THAT DOES, AND SAYS SO.
//
// Only a node serving a log's partition writes that log — the write
// authority's gate 3 refuses anybody else — so a gesture reaching a log it
// cannot write reports it unwritten with the reason and a remedy naming
// another node, rather than dropping it from the answer or trying the write:
// dropped, the gesture reported itself complete while the node went on
// pinning that log. A node that cannot tell whether it serves the partition
// says that instead, and one that serves it and does not run its log right
// now says that — each a remedy of its own.
func TestALogThisNodeDoesNotServeIsLeftToANodeThatDoes(t *testing.T) {
	t.Parallel()
	const away = "node-away"
	everywhere := fixedHolders{
		gateTracker0: {{NodeID: away}}, gateTracker1: {{NodeID: away}}, gatePages0: {{NodeID: away}},
	}
	s := aGateStateLog(t, statelog.ServesOnly(gateTracker0))
	logs, err := countedGateLogs(t.Context(), s, everywhere, nil, "node-p", nil, away, false)
	if err != nil {
		t.Fatalf("the eviction's logs: %v", err)
	}
	reasons := map[string]statelog.Reason{}
	for _, l := range logs {
		if l.write != nil || l.unwritten == nil {
			t.Errorf("%s has a writer on a node that runs no log", l.domain)
			continue
		}
		var refusal *statelog.Unavailable
		if errors.As(l.unwritten, &refusal) {
			reasons[l.domain] = refusal.Reason
		}
		remedy := DomainGate{Domain: l.domain, Stream: l.stream, Err: l.unwritten}.Remedy()
		switch l.domain {
		case "tracker@tracker.000":
			if !strings.Contains(l.unwritten.Error(), "does not run its log") {
				t.Errorf("the served log it does not run answered %v", l.unwritten)
			}
			if !remedy.Offers(statelog.GateRetrySameOp) {
				t.Errorf("a served log not running right now offers %v, want the same "+
					"gesture again once it runs", remedy.Actions)
			}
		default:
			if !errors.Is(l.unwritten, statelog.ErrNotHolder) {
				t.Errorf("%s, whose partition this node does not serve, answered %v, "+
					"want %v", l.domain, l.unwritten, statelog.ErrNotHolder)
			}
			if !slices.Equal(remedy.Actions, []statelog.GateAction{statelog.GateOtherNode}) {
				t.Errorf("%s's remedy is %v, want a node that serves its partition",
					l.domain, remedy.Actions)
			}
		}
	}
	if got := len(logs); got != 3 {
		t.Errorf("the gesture concerns %d log(s), want all three the node is counted on", got)
	}
	if reasons["tracker@tracker.001"] != statelog.ReasonNotHolder ||
		reasons["pages@pages.000"] != statelog.ReasonNotHolder {
		t.Errorf("the logs of partitions this node does not serve answered %v", reasons)
	}

	blind := aGateStateLog(t, unknownHolding{err: coord.ErrUnavailable})
	logs, err = countedGateLogs(t.Context(), blind, everywhere, nil, "node-p", nil, away, false)
	if err != nil {
		t.Fatalf("the eviction's logs: %v", err)
	}
	for _, l := range logs {
		var refusal *statelog.Unavailable
		if !errors.As(l.unwritten, &refusal) || refusal.Reason != statelog.ReasonHoldingUnknown {
			t.Errorf("%s on a node that cannot tell what it serves answered %v, want %s",
				l.domain, l.unwritten, statelog.ReasonHoldingUnknown)
		}
	}
}

// unknownHolders cannot tell who holds anything.
type unknownHolders struct{}

func (unknownHolders) Holders(context.Context,
	[]statelog.PartitionID) (map[statelog.PartitionID][]statelog.Presence, error) {
	return nil, coord.ErrUnavailable
}

// mapRecorder is the estate map's gestures, recording each and when it came
// relative to the logs' writes.
type mapRecorder struct {
	mu    sync.Mutex
	calls []string
	err   error

	// logsWritten is how many log writes had finished when each gesture
	// was made.
	logsWritten *int
	atGesture   []int
}

func (m *mapRecorder) Out(_ context.Context, node, by, reason string) (EstateGesture, error) {
	return m.record("out " + node + " by " + by + " as " + reason)
}

func (m *mapRecorder) In(_ context.Context, node, by string) (EstateGesture, error) {
	return m.record("in " + node + " by " + by)
}

func (m *mapRecorder) record(call string) (EstateGesture, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
	m.atGesture = append(m.atGesture, *m.logsWritten)
	if m.err != nil {
		return EstateGesture{}, m.err
	}
	return EstateGesture{Landed: true}, nil
}

// AN EVICTION TAKES THE NODE OUT OF THE ESTATE MAP, AND A READMISSION PUTS IT
// BACK — after the logs.
//
// Out is what stops the maintainer placing a partition on the node if it comes
// back, and it records why, so a surface can tell a node an operator judged
// gone from one taken out for maintenance; in lets it place one again. Both
// come once every log has answered, and their answer is the gesture's own
// part: a map that could not be written leaves the gesture incomplete however
// the logs answered, and a map that places nothing on the node already is
// finished. Under layout 0 there is no map, and no part of the answer about
// one.
func TestAnEvictionTakesTheNodeOutOfTheMapAndAReadmissionPutsItBack(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	written := 0
	log := func(domain string) gateLog {
		return gateLog{domain: domain, stream: domain,
			write: func(context.Context, string, string, string, bool) (statelog.Result, error) {
				mu.Lock()
				defer mu.Unlock()
				written++
				return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
			}}
	}
	recorder := &mapRecorder{logsWritten: &written}
	g := &NodeGate{
		logs:         gateLogs(log("tracker@tracker.000"), log("pages@pages.000")),
		estate:       recorder,
		live:         func(context.Context) ([]statelog.Presence, error) { return nil, nil },
		readmissible: func(context.Context, string) error { return nil },
		publishing:   func(string) error { return nil },
	}
	req := GateRequest{Node: "node-away", By: "ops",
		OpID: statelog.NewOpID(time.Now(), "gate-map")}

	evicted, err := g.Evict(t.Context(), req)
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	readmitted, err := g.Readmit(t.Context(), req)
	if err != nil {
		t.Fatalf("readmit: %v", err)
	}
	if want := []string{"out node-away by ops as evicted", "in node-away by ops"}; !slices.Equal(recorder.calls, want) {
		t.Errorf("the gestures made %v on the estate map, want %v", recorder.calls, want)
	}
	if want := []int{2, 4}; !slices.Equal(recorder.atGesture, want) {
		t.Errorf("the map was changed with %v log write(s) finished, want %v: after "+
			"every log of its own gesture", recorder.atGesture, want)
	}
	for _, res := range []GateResult{evicted, readmitted} {
		if res.Map == nil || !res.Map.Landed || res.Map.Err != nil || !res.Complete() {
			t.Errorf("the gesture's map answered %+v, complete %v, want it landed and "+
				"the gesture complete", res.Map, res.Complete())
		}
	}
	if evicted.Map.Gesture != "out" || readmitted.Map.Gesture != "in" {
		t.Errorf("the map's gestures were %q and %q, want out and in",
			evicted.Map.Gesture, readmitted.Map.Gesture)
	}

	// A MAP THAT COULD NOT BE WRITTEN is the gesture unfinished, with the
	// same gesture again as its remedy.
	recorder.err = ErrEstateUnavailable
	res, err := g.Evict(t.Context(), req)
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if res.Complete() || !res.Map.Remedy().Offers(statelog.GateRetrySameOp) {
		t.Errorf("an unwritten map reports complete %v with remedy %v, want incomplete "+
			"and the same gesture again", res.Complete(), res.Map.Remedy().Actions)
	}
	// A MAP THAT PLACES NOTHING ON THE NODE already has nothing to take it
	// off of.
	recorder.err = membership.ErrUnknownMember
	if res, _ = g.Evict(t.Context(), req); !res.Complete() {
		t.Errorf("a map that places nothing on the node left the gesture incomplete: %+v", res.Map)
	}

	// UNDER LAYOUT 0, no map and no answer about one.
	g.estate = nil
	if res, _ = g.Evict(t.Context(), req); res.Map != nil || !res.Complete() {
		t.Errorf("a gesture with no map answered %+v, complete %v", res.Map, res.Complete())
	}
}

// A NODE COUNTED ON NO LOG IS FINISHED BY THE MAP ALONE, AND ONLY WHERE THERE IS
// ONE.
//
// A holder the map names nowhere and whose row names nothing concerns no log,
// and taking it out of the map is the whole gesture. With no map as well there
// is nothing the gesture did at all, which is never reported as complete.
func TestAGateOnANodeCountedNowhereIsTheMapsAlone(t *testing.T) {
	t.Parallel()
	done := GateResult{Map: &MapGate{Gesture: "out", Landed: true}}
	if !done.Complete() {
		t.Error("a gesture whose only part, the map, landed reports incomplete")
	}
	unwritten := GateResult{Map: &MapGate{Gesture: "out", Err: ErrEstateUnavailable}}
	if unwritten.Complete() {
		t.Error("a gesture whose only part, the map, was not written reports complete")
	}
	if (GateResult{}).Complete() {
		t.Error("a gesture that wrote nothing anywhere reports complete")
	}
}

// THE LOGS OF ONE GESTURE ARE WRITTEN AT ONCE.
//
// One gesture writes every log it concerns under one budget, and each write
// waits up to two resolution budgets of its own; written one after another,
// a node serving a divided layout's many partitions would run out of budget
// part-way, leaving the node evicted on some logs and counted on the rest.
// Written at once, the gesture waits as long as its slowest log. So each log
// here waits for the other to start before it answers: in sequence, the
// first gives up.
func TestTheLogsOfOneGestureAreWrittenAtOnce(t *testing.T) {
	t.Parallel()
	started := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
	log := func(self, other string) gateLog {
		return gateLog{domain: self, stream: self,
			write: func(context.Context, string, string, string, bool) (statelog.Result, error) {
				close(started[self])
				select {
				case <-started[other]:
					return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
				case <-time.After(5 * time.Second):
					return statelog.Result{}, errors.New("the other log's write never started")
				}
			}}
	}
	g := &NodeGate{
		logs:       gateLogs(log("a", "b"), log("b", "a")),
		live:       func(context.Context) ([]statelog.Presence, error) { return nil, nil },
		publishing: func(string) error { return nil },
	}
	res, err := g.Evict(t.Context(), GateRequest{Node: "node-away", By: "ops",
		OpID: statelog.NewOpID(time.Now(), "gate-at-once")})
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	var got []string
	for _, d := range res.Domains {
		got = append(got, d.Domain)
		if d.Err != nil {
			t.Errorf("%s answered %v: its write waited for the other log's in vain", d.Domain, d.Err)
		}
	}
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("the answers came in the order %v, want the logs' own", got)
	}
}

// THE GATE GESTURES ON THE MAP ONLY UNDER A LAYOUT THAT HAS ONE, AND ASKS WITH AN
// UNTYPED NIL.
//
// Under layout 0 the estate map's duty writes nothing, and every gesture on it
// answers that there is no map — which a gate that made one would report as
// the eviction's unfinished part, for ever. And the gate asks whether it has a
// map to gesture on by comparing with nil, so a node with no coordination store
// must not hand it a nil control wrapped in the interface.
func TestTheGateGesturesOnTheMapOnlyUnderALayoutWithOne(t *testing.T) {
	t.Parallel()
	divided := partitionedTestLayout()
	e := &Engine{}
	if got := e.gateMembership(divided); got != nil {
		t.Errorf("a node with no coordination store gestures on the map through %#v", got)
	}
	e.estateControl = &EstateControl{running: divided, now: time.Now}
	if got := e.gateMembership(LayoutZero()); got != nil {
		t.Errorf("under layout 0 the gate gestures on the map through %#v", got)
	}
	if got := e.gateMembership(divided); got == nil {
		t.Error("under a divided layout the gate makes no gesture on the map")
	}
}

// EVERY WAY THE MAP'S PART CAN END SAYS WHETHER IT IS FINISHED AND WHAT DOES.
//
// The map is a gesture's own part, so the one judgement of it — finished, and
// if not, the remedy — is made once here and rendered by the route and the
// command alike. A map that places nothing on the node is finished with a
// sentence and no action; every refusal a later gesture clears keeps the
// operation, because a fresh one writes every log that already holds the
// record again; and a newer build's map is another node's to write.
func TestEveryWayTheMapsPartEndsSaysWhatFinishesIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		gate    MapGate
		done    bool
		actions []statelog.GateAction
		// says is what the remedy's sentence names, where the refusal
		// has a precondition of its own.
		says string
	}{
		{"landed", MapGate{Gesture: "out", Landed: true}, true, nil, ""},
		{"raced out", MapGate{Gesture: "out"}, false,
			[]statelog.GateAction{statelog.GateRetrySameOp}, "kept changing"},
		{"no member", MapGate{Gesture: "out", Err: membership.ErrUnknownMember}, true, nil,
			"places nothing on the node"},
		{"removed", MapGate{Gesture: "in", Err: membership.ErrRemovedMember}, true, nil,
			"places nothing on the node"},
		{"nowhere else", MapGate{Gesture: "out", Err: membership.ErrNothingPlaceable}, false,
			[]statelog.GateAction{statelog.GateRetrySameOp}, "add a data node"},
		{"no map yet", MapGate{Gesture: "out", Err: partmap.ErrNoMap}, false,
			[]statelog.GateAction{statelog.GateRetrySameOp}, "no estate map has been written"},
		{"newer build", MapGate{Gesture: "out", Err: ErrEstateNewerMap}, false,
			[]statelog.GateAction{statelog.GateOtherNode}, "newer build"},
		{"unavailable", MapGate{Gesture: "out", Err: ErrEstateUnavailable}, false,
			[]statelog.GateAction{statelog.GateRetrySameOp}, "could not be read or written"},
	} {
		remedy := c.gate.Remedy()
		if got := c.gate.done(); got != c.done {
			t.Errorf("%s: finished %v, want %v", c.name, got, c.done)
		}
		if !slices.Equal(remedy.Actions, c.actions) {
			t.Errorf("%s: actions %v, want %v", c.name, remedy.Actions, c.actions)
		}
		if !strings.Contains(remedy.Detail, c.says) {
			t.Errorf("%s: the remedy says %q, want it to name %q", c.name, remedy.Detail, c.says)
		}
		if c.gate.Landed != remedy.IsZero() {
			t.Errorf("%s: remedy %+v — only a map the gesture wrote has none", c.name, remedy)
		}
	}
}
