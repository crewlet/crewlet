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
	"github.com/crewlet/crewlet/internal/estate"
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
	logs, err := countedGateLogs(t.Context(), s, fixedHolders{}, &routeRecorder{}, nil, "node-p", nil, away, true)
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
	logs, err = countedGateLogs(t.Context(), s, fixedHolders{}, &routeRecorder{}, nil, "node-p", nil, away, false)
	if err != nil {
		t.Fatalf("the eviction's logs: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("an eviction of a node counted nowhere concerns %d log(s), want none", len(logs))
	}
}

// A LOG THIS NODE CANNOT WRITE IS SENT TO A NODE THAT CAN, AND SAYS WHO WROTE IT.
//
// Only a node serving a log's partition writes that log — the write
// authority's gate 3 refuses anybody else — so every log the gesture concerns
// that this node does not serve, cannot tell whether it serves, or serves and
// does not run right now is sent through the estate's router as
// `statelog.gate` to a node that does, naming the log by its layout, domain
// and partition, with the gesture's node, operator and operation id. Left
// unwritten here, as it once was, the gesture was unfinished on every log of
// every partition this node does not serve, and a readmission's map part —
// which waits for every log — never landed on a fleet where no node serves
// them all. The answer names the node whose write authority gave it, and an
// answer this node gave itself names none.
func TestALogThisNodeCannotWriteIsSentToANodeThatCan(t *testing.T) {
	t.Parallel()
	const away = "node-away"
	everywhere := fixedHolders{
		gateTracker0: {{NodeID: away}}, gateTracker1: {{NodeID: away}}, gatePages0: {{NodeID: away}},
	}
	// SERVING tracker.000 AND RUNNING NO LOG: the one it serves is not
	// running, and the other two are not its partitions.
	s := aGateStateLog(t, statelog.ServesOnly(gateTracker0))
	route := &routeRecorder{writer: "node-q"}
	logs, err := countedGateLogs(t.Context(), s, everywhere, route, nil, "node-p", nil, away, false)
	if err != nil {
		t.Fatalf("the eviction's logs: %v", err)
	}
	if got := len(logs); got != 3 {
		t.Fatalf("the gesture concerns %d log(s), want all three the node is counted on", got)
	}
	for _, l := range logs {
		if l.write != nil || l.route == nil {
			t.Errorf("%s is written here (%v) or not sent anywhere (%v)", l.domain,
				l.write != nil, l.route == nil)
			continue
		}
		res, writer, err := l.publish(t.Context(), "ops", "op-"+l.domain, away, false)
		if err != nil || res.Outcome != statelog.OutcomeApplied || writer != "node-q" {
			t.Errorf("%s answered (%+v, %q, %v), want applied by node-q", l.domain, res, writer, err)
		}
	}
	want := []estate.GateArgs{
		{LogRef: estate.LogRef{Layout: 1, Domain: "tracker", Partition: "tracker.000"},
			Node: away, By: "ops", OpID: "op-tracker@tracker.000", Kind: estate.GateEvict},
		{LogRef: estate.LogRef{Layout: 1, Domain: "tracker", Partition: "tracker.001"},
			Node: away, By: "ops", OpID: "op-tracker@tracker.001", Kind: estate.GateEvict},
		{LogRef: estate.LogRef{Layout: 1, Domain: "pages", Partition: "pages.000"},
			Node: away, By: "ops", OpID: "op-pages@pages.000", Kind: estate.GateEvict},
	}
	if got := route.sent(); !slices.Equal(got, want) {
		t.Errorf("the router was sent\n%+v\nwant\n%+v", got, want)
	}

	// A NODE THAT CANNOT TELL WHAT IT SERVES sends every log too.
	blind := aGateStateLog(t, unknownHolding{err: coord.ErrUnavailable})
	logs, err = countedGateLogs(t.Context(), blind, everywhere, route, nil, "node-p", nil, away, false)
	if err != nil {
		t.Fatalf("the eviction's logs: %v", err)
	}
	for _, l := range logs {
		if l.route == nil {
			t.Errorf("%s on a node that cannot tell what it serves is not sent to a holder", l.domain)
		}
	}

	// AN ANSWER THIS NODE GAVE ITSELF — the router asks it first where it
	// serves the partition — names no other writer.
	self := &routeRecorder{writer: "node-p"}
	logs, err = countedGateLogs(t.Context(), s, everywhere, self, nil, "node-p", nil, away, false)
	if err != nil {
		t.Fatalf("the eviction's logs: %v", err)
	}
	if _, writer, _ := logs[0].publish(t.Context(), "ops", "op", away, false); writer != "" {
		t.Errorf("an answer this node gave names the writer %q, want none", writer)
	}
}

// routeRecorder is the estate's router as the gate sees it: it records every
// gate record sent, and answers each applied by writer — or with err.
type routeRecorder struct {
	writer string
	err    error

	mu      sync.Mutex
	records []estate.GateArgs
	bounds  []estate.LogRef
}

func (r *routeRecorder) Gate(_ context.Context, a estate.GateArgs) (statelog.Result, string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, a)
	if r.err != nil {
		return statelog.Result{}, "", r.err
	}
	return statelog.Result{Outcome: statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "s", Generation: 1, Seq: 4}}, r.writer, nil
}

// ReadmissionBound answers every log's bound as the zero one — nothing below
// it can be gone — or err.
func (r *routeRecorder) ReadmissionBound(_ context.Context, l estate.LogRef) (statelog.ReadmissionBound, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bounds = append(r.bounds, l)
	return statelog.ReadmissionBound{}, r.err
}

func (r *routeRecorder) sent() []estate.GateArgs {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.records)
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

func (m *mapRecorder) Bar(ctx context.Context, node, by, reason string) (EstateGesture, error) {
	if err := ctx.Err(); err != nil {
		// A COORDINATION STORE ASKED ON A DEAD CONTEXT answers nothing.
		return EstateGesture{}, err
	}
	return m.record("bar " + node + " by " + by + " as " + reason)
}

func (m *mapRecorder) Readmit(ctx context.Context, node, by string) (EstateGesture, error) {
	if err := ctx.Err(); err != nil {
		return EstateGesture{}, err
	}
	return m.record("readmit " + node + " by " + by)
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

// AN EVICTION BARS THE NODE FROM THE ESTATE MAP, AND A READMISSION PUTS IT
// BACK — after the logs.
//
// The bar is what stops the maintainer placing a partition on the node if it
// comes back, whatever became of its membership meanwhile, and it records why,
// so a surface can tell a node an operator judged gone from one taken out for
// maintenance; in lets it place one again. Both come once every log has
// answered, and their answer is the gesture's own part: a map that could not be
// written leaves the gesture incomplete however the logs answered — an
// eviction's included, whether or not the map holds the node, since a bar is
// written for a node the map has let go — and a readmission of a node the map
// keeps nothing of is finished. Under layout 0 there is no map, and no part of
// the answer about one.
func TestAnEvictionBarsTheNodeFromTheMapAndAReadmissionPutsItBack(t *testing.T) {
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
	if want := []string{"bar node-away by ops as evicted", "readmit node-away by ops"}; !slices.Equal(recorder.calls, want) {
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
	// A MAP THAT KEEPS NOTHING OF THE NODE has nothing for a readmission to
	// lift — and an eviction refused that way has NOT finished: the bar is
	// what must be written for a node the map has let go.
	recorder.err = membership.ErrUnknownMember
	if res, _ = g.Readmit(t.Context(), req); !res.Complete() {
		t.Errorf("a readmission the map keeps nothing of left the gesture incomplete: %+v", res.Map)
	}
	if res, _ = g.Evict(t.Context(), req); res.Complete() {
		t.Errorf("an eviction whose bar was refused reports complete: %+v", res.Map)
	}

	// UNDER LAYOUT 0, no map and no answer about one.
	g.estate = nil
	if res, _ = g.Evict(t.Context(), req); res.Map != nil || !res.Complete() {
		t.Errorf("a gesture with no map answered %+v, complete %v", res.Map, res.Complete())
	}
}

// A READMISSION PUTS THE NODE BACK IN THE MAP ONLY ONCE EVERY LOG HAS TAKEN IT
// BACK.
//
// A log that has not taken the node back holds its eviction still: an in made
// then lets the maintainer place that log's partition on the node, every write
// it decides there gated and the trim passing a holder it counts by its
// tombstone. So the map part waits, and says the same gesture under the same
// operation id finishes it — on any node, since every log is reached from any
// node: a log this node does not serve is sent to one that does, and one no
// holder answered for is the same gesture's to finish once one does. Once every
// log has answered, the in is made.
func TestAReadmissionPutsTheNodeBackOnlyOnceEveryLogHas(t *testing.T) {
	t.Parallel()
	written := 0
	done := func(domain string) gateLog {
		return gateLog{domain: domain, stream: domain,
			write: func(context.Context, string, string, string, bool) (statelog.Result, error) {
				return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
			}}
	}
	partition := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}
	unserved := routedGateLog(&routeRecorder{err: &estate.ErrPartitionUnserved{
		Partition: partition.String(), Detail: "node-q: no answer"}},
		partitionedTestLayout(), statelog.LogID{Domain: "tracker", Partition: partition},
		statelog.StreamSpec{Name: "tracker@tracker.001"}, "node-p")
	recorder := &mapRecorder{logsWritten: &written}
	g := &NodeGate{
		logs:         gateLogs(done("tracker@tracker.000"), unserved),
		estate:       recorder,
		live:         func(context.Context) ([]statelog.Presence, error) { return nil, nil },
		readmissible: func(context.Context, string) error { return nil },
		publishing:   func(string) error { return nil },
	}
	req := GateRequest{Node: "node-back", By: "ops", OpID: statelog.NewOpID(time.Now(), "readmit")}
	res, err := g.Readmit(t.Context(), req)
	if err != nil {
		t.Fatalf("readmit: %v", err)
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("the map was changed %v with a log still holding the node's eviction", recorder.calls)
	}
	if res.Complete() || res.Map == nil || !errors.Is(res.Map.Err, ErrMapAwaitsLogs) {
		t.Fatalf("the map part answered %+v, complete %v: want it waiting on the logs",
			res.Map, res.Complete())
	}
	if remedy := res.Map.Remedy(); !slices.Equal(remedy.Actions,
		[]statelog.GateAction{statelog.GateRetrySameOp}) {
		t.Errorf("a map part waiting on a log offers %+v, want the same gesture again", remedy)
	}
	unfinished := res.Domains[1]
	if remedy := unfinished.Remedy(); !errors.As(unfinished.Err, new(*estate.ErrPartitionUnserved)) ||
		!slices.Equal(remedy.Actions, []statelog.GateAction{statelog.GateRetrySameOp}) {
		t.Errorf("a log no holder answered for answered %v with remedy %+v, want the same "+
			"gesture again", unfinished.Err, remedy)
	}

	// A LOG WHOSE OUTCOME NOBODY COULD TELL is the same gesture's too.
	unknownHere := gateLog{domain: "tracker@tracker.001", stream: "tracker@tracker.001",
		write: func(context.Context, string, string, string, bool) (statelog.Result, error) {
			return statelog.Result{Outcome: statelog.OutcomeUnknown}, nil
		}}
	g.logs = gateLogs(done("tracker@tracker.000"), unknownHere)
	if res, err = g.Readmit(t.Context(), req); err != nil {
		t.Fatalf("readmit: %v", err)
	}
	if !errors.Is(res.Map.Err, ErrMapAwaitsLogs) ||
		!slices.Equal(res.Map.Remedy().Actions, []statelog.GateAction{statelog.GateRetrySameOp}) {
		t.Errorf("a map part waiting on an unknown log answered %+v, remedy %+v: want the "+
			"same gesture again", res.Map, res.Map.Remedy())
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("the map was changed %v with a log still unfinished", recorder.calls)
	}

	g.logs = gateLogs(done("tracker@tracker.000"), done("tracker@tracker.001"))
	res, err = g.Readmit(t.Context(), req)
	if err != nil {
		t.Fatalf("readmit again: %v", err)
	}
	if want := []string{"readmit node-back by ops"}; !slices.Equal(recorder.calls, want) || !res.Complete() {
		t.Errorf("once every log is done the map was changed %v, complete %v; want %v",
			recorder.calls, res.Complete(), want)
	}

	// AN EVICTION'S BAR DOES NOT WAIT: it errs in the safe direction whatever
	// the logs answered.
	g.logs = gateLogs(done("tracker@tracker.000"), unserved)
	if _, err := g.Evict(t.Context(), req); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if n := len(recorder.calls); n != 2 || !strings.HasPrefix(recorder.calls[1], "bar ") {
		t.Errorf("an eviction with a log unfinished made %v on the map, want the bar", recorder.calls)
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
		{"nothing to put back", MapGate{Gesture: "in", Err: membership.ErrUnknownMember}, true, nil,
			"keeps nothing of the node"},
		{"an out the map refused", MapGate{Gesture: "out", Err: membership.ErrUnknownMember}, false,
			[]statelog.GateAction{statelog.GateRetrySameOp}, "could not be read or written"},
		{"logs unfinished", MapGate{Gesture: "in", Err: ErrMapAwaitsLogs}, false,
			[]statelog.GateAction{statelog.GateRetrySameOp}, "every log has taken it back"},
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
