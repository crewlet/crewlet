package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
	"github.com/crewlet/crewlet/internal/placement"
)

// objectsAt is the instant every fixture here is stamped at.
var objectsAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// settledObjects is a map every member of which holds what it places, with
// data-c taken out and emptied — the state in which a node may be stopped.
func settledObjects() objstore.MapState {
	member := func(node string, zone string) placement.Member {
		return placement.Member{Node: node, Weight: 1,
			Share: placement.DefaultShare(1), Domain: zone}
	}
	out := member("data-c", "eu-3")
	out.Out = true
	return objstore.MapState{
		Map: objplacement.Map{
			Generation: uuid.MustParse("5b0c1f7e-9c1d-4f5e-8a3b-2d7c6e1f0a9b"),
			Epoch:      7, Replicas: 2, PGBits: objplacement.MinPGBits,
			FailureDomain: "zone",
			Members: []placement.Member{
				member("data-a", "eu-1"), member("data-b", "eu-2"), out,
			},
		},
		TakenOut: map[string]objstore.Gesture{"data-c": {By: "ops",
			Reason: "decommission", At: objectsAt}},
	}
}

// settledLeases are the leases of [settledObjects]: everyone repaired at the
// map's epoch with nothing pending, and data-c holding no strays.
func settledLeases() map[string]objstore.ObjectsMeta {
	none := 0
	done := func() *objstore.ObjectsRepair {
		return &objstore.ObjectsRepair{Epoch: 7, Completed: true, Placed: 500, Held: 500,
			At: objectsAt}
	}
	return map[string]objstore.ObjectsMeta{
		"data-a": {Weight: 1, Health: &objstore.ObjectsHealth{State: "ok", UsedPercent: 30},
			Repair: done(), Strays: &none},
		"data-b": {Weight: 1, Health: &objstore.ObjectsHealth{State: "ok", UsedPercent: 31},
			Repair: done(), Strays: &none},
		"data-c": {Weight: 1, Health: &objstore.ObjectsHealth{State: "ok", UsedPercent: 5},
			Repair: done(), Strays: &none},
	}
}

// fakeObjectsNode answers the fleet view and the gesture routes through the
// API's OWN renderers — queries.RenderObjects for the block, and
// api.RenderObjectsGesture and api.RenderObjectsRefusal for the gestures —
// applying upkeep's own pure gestures to the map it holds. A fixture spelling
// this command's idea of the shape agrees with the command whatever a node
// sends.
type fakeObjectsNode struct {
	server *httptest.Server

	mu     sync.Mutex
	state  objstore.MapState
	leases map[string]objstore.ObjectsMeta
	// block, when set, is served in place of the rendered map — a state
	// that is not placed, or no objects key at all when it is empty.
	block *queries.FleetObjects
	// omit serves a fleet view with no objects key: a node that runs no
	// object store.
	omit bool
	// lose makes every gesture lose its race; hangUp drops the connection
	// after recording the request.
	lose, hangUp bool
	asked        []*http.Request
}

func newFakeObjectsNode(t *testing.T) *fakeObjectsNode {
	t.Helper()
	n := &fakeObjectsNode{state: settledObjects(), leases: settledLeases()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fleet", func(w http.ResponseWriter, _ *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		body := map[string]any{"nodes": []any{}}
		switch {
		case n.omit:
		case n.block != nil:
			body["objects"] = n.block
		default:
			var leases []coord.Lease
			for node, meta := range n.leases {
				leases = append(leases, coord.Lease{
					Resource: coord.ObjectsResource(node), Meta: meta.Encode()})
			}
			body["objects"] = queries.RenderObjects(n.state, n.state.Map.Layout(),
				leases, objectsAt)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	gesture := func(node string, change func(objstore.MapState) (objstore.MapState, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			n.mu.Lock()
			defer n.mu.Unlock()
			n.asked = append(n.asked, r)
			if n.hangUp {
				if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			next, err := change(n.state)
			if refusal, ok := api.RenderObjectsRefusal(err); ok {
				w.WriteHeader(refusal.Status)
				_ = json.NewEncoder(w).Encode(refusal.Body)
				return
			}
			g := engine.ObjectsGesture{State: n.state}
			if !n.lose {
				n.state = next
				g = engine.ObjectsGesture{Landed: true, State: next}
			}
			_ = json.NewEncoder(w).Encode(api.RenderObjectsGesture(g,
				r.PathValue(node), objectsAt))
		}
	}
	mux.HandleFunc("POST /objects/out/{node}", func(w http.ResponseWriter, r *http.Request) {
		gesture("node", func(s objstore.MapState) (objstore.MapState, error) {
			return upkeep.Out(s, r.PathValue("node"), "ops", r.URL.Query().Get("reason"),
				objectsAt)
		})(w, r)
	})
	mux.HandleFunc("POST /objects/in/{node}", func(w http.ResponseWriter, r *http.Request) {
		gesture("node", func(s objstore.MapState) (objstore.MapState, error) {
			return upkeep.In(s, r.PathValue("node"))
		})(w, r)
	})
	mux.HandleFunc("POST /objects/hold", func(w http.ResponseWriter, r *http.Request) {
		d, _ := time.ParseDuration(r.URL.Query().Get("for"))
		gesture("", func(s objstore.MapState) (objstore.MapState, error) {
			return upkeep.HoldFor(s, d, "ops", r.URL.Query().Get("reason"), objectsAt)
		})(w, r)
	})
	mux.HandleFunc("POST /objects/release", gesture("",
		func(s objstore.MapState) (objstore.MapState, error) { return upkeep.Release(s), nil }))
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// requests is every gesture the node received.
func (n *fakeObjectsNode) requests() []*http.Request {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]*http.Request(nil), n.asked...)
}

// A SETTLED FLEET SAYS SO, and says of a member taken out and emptied that it
// may be stopped for good — the last step of the decommission runbook.
func TestObjectsStatusSaysWhenANodeMayBeStopped(t *testing.T) {
	node := newFakeObjectsNode(t)
	out, stderr, err := cli(t, "objects", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("objects status: %v\n%s", err, stderr)
	}
	for _, want := range []string{
		"Placement map epoch 7 · 2 of 2 copies of every chunk · 256 groups",
		"Copies are spread across zone",
		"NODE", "data-a", "eu-1", "50.0%",
		"Settled at epoch 7",
		"data-c is out and holds no strays",
		"may be stopped for good",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the status does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "wait for") {
		t.Errorf("a settled fleet is told to wait:\n%s", out)
	}
}

// AN UNSETTLED FLEET NAMES WHAT IT IS WAITING FOR, member by member, in words:
// a member gone, one whose last repair ran at an older epoch, one still
// fetching, and a member taken out that still holds strays. A reader who has
// to know which column decides it has not been told.
func TestObjectsStatusNamesWhatTheFleetIsWaitingFor(t *testing.T) {
	node := newFakeObjectsNode(t)
	strays := 9
	node.state.Absence = map[string]objstore.Absence{"data-a": {Ticks: 5, Since: objectsAt,
		Reason: objstore.ReasonAbsent}}
	delete(node.leases, "data-a")
	b := node.leases["data-b"]
	b.Repair = &objstore.ObjectsRepair{Epoch: 6, Completed: true, Pending: 0}
	node.leases["data-b"] = b
	c := node.leases["data-c"]
	c.Strays = &strays
	node.leases["data-c"] = c

	out, _, err := cli(t, "objects", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Before stopping another data node, wait for:",
		"data-a to come back: it has been counted gone 5 of the 40 ticks",
		"data-b to complete a repair at epoch 7: its last pass ran at epoch 6",
		"degraded groups to have every copy on a present member",
		"DEGRADED:",
		"data-c is out and still holds 9 strays",
		"5/40", "0 @6",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the status does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Settled") || strings.Contains(out, "stopped for good") {
		t.Errorf("an unsettled fleet reads as settled:\n%s", out)
	}

	// STILL FETCHING, and a hold, each said in words.
	node.leases["data-a"] = settledLeases()["data-a"]
	node.state.Absence = nil
	b.Repair = &objstore.ObjectsRepair{Epoch: 7, Completed: true, Pending: 12}
	node.leases["data-b"] = b
	node.state.Hold = &objstore.Hold{Until: objectsAt.Add(time.Hour), By: "ops",
		Reason: "kernel upgrade", At: objectsAt}
	out, _, err = cli(t, "objects", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"data-b to fetch the 12 chunks the map places on it",
		"HELD until 2026-09-01T13:00:00Z by ops (kernel upgrade)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the status does not say %q:\n%s", want, out)
		}
	}

	// NO COUNT YET, said as a wait for the collection that owes one rather
	// than as the hourly schedule: the lease carries strays only from a
	// collection that walked every slot at the map's epoch, and that is the
	// one each node runs once the fleet settles there.
	c.Strays = nil
	node.leases["data-c"] = c
	out, _, err = cli(t, "objects", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"data-c is out and has not reported its strays at epoch 7 yet",
		"within about half a minute of the fleet settling there",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the status does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "every 1h") {
		t.Errorf("an absent count reads as the hourly collection's:\n%s", out)
	}
}

// A MEMBER ON PROBATION, A NODE REMOVED AND NOT SEEN SINCE, AND WHAT A SCRUB
// COULD NOT READ are each said in words: the first is read from and placed on
// nothing, which no column alone says; the second rejoins on probation the
// moment it is seen, which is not "placed on again"; and a chunk the scrub
// steps past shows nowhere else.
func TestObjectsStatusSaysProbationRemovalsAndWhatTheScrubCouldNotRead(t *testing.T) {
	node := newFakeObjectsNode(t)
	back := placement.Member{Node: "data-d", Weight: 1, Share: placement.DefaultShare(1),
		Domain: "eu-2", Probation: true}
	node.state.Map.Members = append(node.state.Map.Members, back)
	node.state.Removed = map[string]objstore.Removal{
		"data-d": {Present: 12, At: objectsAt, Reason: objstore.ReasonAbsent},
		"data-e": {Gone: 6, At: objectsAt, Reason: objstore.ReasonUnhealthy,
			Detail: "probe: read-only filesystem"},
	}
	// JUST BACK: no repair pass reported yet, which a member the map places
	// on would be waited for.
	kept := 37
	node.leases["data-d"] = objstore.ObjectsMeta{Weight: 1,
		Health: &objstore.ObjectsHealth{State: "ok", UsedPercent: 20}, Strays: &kept}
	a := node.leases["data-a"]
	a.Scrub = &objstore.ObjectsScrub{CycleStarted: objectsAt, Progress: 0.5, Verified: 90,
		Unreadable: 2, Error: "walk slots [16384, 16640): permission denied"}
	node.leases["data-a"] = a

	out, _, err := cli(t, "objects", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"probation 12/40", "kept 37",
		"ON PROBATION: data-d, removed for being absent and back 12 of the 40 ticks",
		"read from and repaired from meanwhile and placed on nothing",
		"REMOVED and remembered: data-e (unhealthy: probe: read-only filesystem), not seen for 6 ticks",
		"rejoins on probation", "gone 34 more ticks, it is forgotten",
		"SCRUB data-a: 0 chunks rotten and 2 its disk would not read this cycle",
		"SCRUB data-a: the scrub stopped (walk slots [16384, 16640): permission denied)",
		// A MEMBER ON PROBATION IS NOT WAITED FOR: the map places nothing on
		// it, which is how every node judges a settled fleet.
		"Settled at epoch 7",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the status does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "placed on again once present") {
		t.Errorf("a removed node not seen since reads as counting its way back:\n%s", out)
	}
}

// HOW EVENLY THE MAP SPREADS ITS COPIES IS SAID IN WORDS, from the balance the
// map recorded: a balance that ran out of rounds writes the best map it
// measured either way, so this line is the one place the command says a
// member's weight is not being kept. A map nothing has measured says nothing
// rather than "balanced", and a measurement of an older placement says which.
func TestObjectsStatusSaysHowEvenlyTheMapSpreadsItsCopies(t *testing.T) {
	report := func(epoch uint64, rounds int, deviation float64, converged bool) objstore.Balance {
		return objstore.Balance{Epoch: epoch, BalanceReport: placement.BalanceReport{
			Rounds: rounds, Deviation: deviation, Converged: converged}}
	}
	for name, tc := range map[string]struct {
		balance objstore.Balance
		want    string
	}{
		"converged": {report(7, 6, 0.015, true),
			"Balanced within 1.5%: every placeable member holds that close"},
		"out of rounds": {report(7, placement.DefaultMaxRounds, 0.061, false),
			"NOT CONVERGED: after 60 rounds a member is still 6.1% off the copies its " +
				"weight entitles it to, where a balance aims within 2%"},
		"a split left as it was": {report(7, 0, 0.031, false),
			"Within 3.1% of every member's weight: a split's placement, measured and left as it was"},
		"an older placement": {report(6, 6, 0.015, true),
			"Balance last measured at epoch 6: the map's placement changed since"},
	} {
		node := newFakeObjectsNode(t)
		node.state.Balance = tc.balance
		out, _, err := cli(t, "objects", "status", bootstrapForURL(t, node.server.URL))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: the status does not say %q:\n%s", name, tc.want, out)
		}
	}
	node := newFakeObjectsNode(t)
	out, _, err := cli(t, "objects", "status", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "alanced") || strings.Contains(out, "CONVERGED") {
		t.Errorf("a map nothing has measured says something about its balance:\n%s", out)
	}
}

// -json IS THE BLOCK AS THE NODE ANSWERED IT, for a script waiting to stop the
// next data node: re-encoded, a field this build does not know would be lost.
func TestObjectsStatusJSONIsTheBlockAsAnswered(t *testing.T) {
	node := newFakeObjectsNode(t)
	out, _, err := cli(t, "objects", "status", "-json", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	var block queries.FleetObjects
	if err := json.Unmarshal([]byte(out), &block); err != nil {
		t.Fatalf("-json printed %q: %v", out, err)
	}
	if block.State != queries.ObjectMapPlaced || block.PlacedObjects == nil ||
		len(block.Members) != 3 || block.Epoch != 7 {
		t.Errorf("-json printed %+v", block)
	}
}

// EACH STATE THAT IS NOT A MAP IS SAID APART, and a node with no object store
// is told to ask another rather than shown an empty table.
func TestObjectsStatusNamesEachStateThatIsNotAMap(t *testing.T) {
	for _, tc := range []struct {
		block *queries.FleetObjects
		omit  bool
		fails bool
		says  string
	}{
		{&queries.FleetObjects{State: queries.ObjectMapNone}, false, false, "No placement map yet"},
		{&queries.FleetObjects{State: queries.ObjectMapUnavailable}, false, true, "could not be read"},
		{&queries.FleetObjects{State: queries.ObjectMapUnreadable}, false, true, "newer build"},
		{nil, true, true, "runs no object store"},
	} {
		node := newFakeObjectsNode(t)
		node.block, node.omit = tc.block, tc.omit
		out, _, err := cli(t, "objects", "status", bootstrapForURL(t, node.server.URL))
		said := out
		if err != nil {
			said = err.Error()
		}
		if (err != nil) != tc.fails || !strings.Contains(said, tc.says) {
			t.Errorf("%+v: answered %q (err %v), want %q", tc.block, out, err, tc.says)
		}
	}
}

// TAKING A MEMBER OUT MUST REPEAT ITS NAME, and nothing is sent until it does.
func TestObjectsOutWithoutTheNodeRepeatedIsRefused(t *testing.T) {
	node := newFakeObjectsNode(t)
	for _, args := range [][]string{
		{"objects", "out", "data-a"},
		{"objects", "out", "data-a", "-confirm", "data-b"},
		{"objects", "in", "-confirm", "data-a"},
	} {
		if _, _, err := cli(t, append(args, bootstrapForURL(t, node.server.URL))...); err == nil ||
			!strings.Contains(err.Error(), "-confirm") {
			t.Errorf("%v: %v, want a refusal naming -confirm", args, err)
		}
	}
	if asked := node.requests(); len(asked) != 0 {
		t.Errorf("an unconfirmed gesture reached the node: %d requests", len(asked))
	}
}

// OUT SENDS THE CONFIRMATION AND THE REASON, and says what to wait for before
// the node is stopped — which is the whole point of taking it out first.
func TestObjectsOutTakesTheMemberOutAndSaysWhatComesNext(t *testing.T) {
	node := newFakeObjectsNode(t)
	out, stderr, err := cli(t, "objects", "out", "data-a", "-confirm", "data-a",
		"-reason", "disk swap", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("objects out: %v\n%s", err, stderr)
	}
	asked := node.requests()
	if len(asked) != 1 {
		t.Fatalf("the node was asked %d times", len(asked))
	}
	if q := asked[0].URL.Query(); q.Get("confirm") != "data-a" || q.Get("reason") != "disk swap" {
		t.Errorf("the gesture carried %v", q)
	}
	for _, want := range []string{"data-a is out of the placement map at epoch 8",
		"no strays on data-a"} {
		if !strings.Contains(out, want) {
			t.Errorf("out says:\n%s\nwant %q", out, want)
		}
	}
	if m, _ := node.state.Map.Member("data-a"); !m.Out {
		t.Error("the member is not out on the node's map")
	}
}

// PUTTING BACK A REMOVED NODE says it is placed on at its next sighting,
// rather than claiming a member the map does not yet have.
func TestObjectsInOfARemovedNodeSaysWhenItIsPlacedOn(t *testing.T) {
	node := newFakeObjectsNode(t)
	node.state.Removed = map[string]objstore.Removal{"data-e": {At: objectsAt,
		Reason: objstore.ReasonAbsent}}
	out, _, err := cli(t, "objects", "in", "data-e", "-confirm", "data-e",
		bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "data-e is no longer held back") {
		t.Errorf("in of a removed node says:\n%s", out)
	}
	out, _, err = cli(t, "objects", "in", "data-c", "-confirm", "data-c",
		bootstrapForURL(t, node.server.URL))
	if err != nil || !strings.Contains(out, "data-c is back in the placement map at epoch 8") {
		t.Errorf("in of an out member said %q (%v)", out, err)
	}
}

// A HOLD NAMES ITS LENGTH, and none is sent without one.
func TestObjectsHoldNamesItsLength(t *testing.T) {
	node := newFakeObjectsNode(t)
	if _, _, err := cli(t, "objects", "hold", bootstrapForURL(t, node.server.URL)); err == nil ||
		!strings.Contains(err.Error(), "-for") {
		t.Errorf("a hold with no length: %v", err)
	}
	if len(node.requests()) != 0 {
		t.Fatal("a hold with no length reached the node")
	}
	out, _, err := cli(t, "objects", "hold", "-for", "2h", "-reason", "kernel upgrade",
		bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if q := node.requests()[0].URL.Query(); q.Get("for") != "2h0m0s" ||
		q.Get("reason") != "kernel upgrade" {
		t.Errorf("the hold carried %v", q)
	}
	if !strings.Contains(out, "held until 2026-09-01T14:00:00Z") {
		t.Errorf("hold says:\n%s", out)
	}
	out, _, err = cli(t, "objects", "release", bootstrapForURL(t, node.server.URL))
	if err != nil || !strings.Contains(out, "The hold is released") || node.state.Hold != nil {
		t.Errorf("release said %q (%v), hold now %+v", out, err, node.state.Hold)
	}
}

// A REFUSAL CARRIES WHAT TO DO, and a gesture that lost every race is not
// reported as done.
func TestObjectsGestureRefusalsAndLostRacesAreErrors(t *testing.T) {
	node := newFakeObjectsNode(t)
	_, _, err := cli(t, "objects", "out", "data-z", "-confirm", "data-z",
		bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "unknown_member") ||
		!strings.Contains(err.Error(), "name a node the fleet view's object placement lists") {
		t.Errorf("an unknown member: %v", err)
	}

	// A NODE THE MAP REMOVED IS NOT A NAME TO RETYPE: the answer says it is
	// put back rather than taken out.
	node.state.Removed = map[string]objstore.Removal{"data-e": {Gone: 2, At: objectsAt,
		Reason: objstore.ReasonAbsent}}
	_, _, err = cli(t, "objects", "out", "data-e", "-confirm", "data-e",
		bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "removed_member") ||
		!strings.Contains(err.Error(), "putting it back vouches for it now") {
		t.Errorf("a removed node taken out: %v", err)
	}

	node.lose = true
	out, _, err := cli(t, "objects", "out", "data-a", "-confirm", "data-a",
		bootstrapForURL(t, node.server.URL))
	if err == nil || !strings.Contains(err.Error(), "did not land") || out != "" {
		t.Errorf("a lost race printed %q and answered %v", out, err)
	}
}

// A GESTURE NOBODY ANSWERED says how to find out what happened, and that
// sending it again is safe — and a HOLD says what sending it again does, since
// it is the one gesture a repeat is not a no-op for: it replaces the hold in
// force, its length counted from the resend.
func TestAnUnansweredObjectsGestureSaysItIsSafeToRepeat(t *testing.T) {
	node := newFakeObjectsNode(t)
	node.hangUp = true
	for _, tc := range []struct {
		args        []string
		says, never string
	}{
		{[]string{"objects", "release"}, "running the same gesture again is safe, since one " +
			"the map already says changes nothing", "replaces"},
		{[]string{"objects", "out", "data-a", "-confirm", "data-a"},
			"running the same gesture again is safe", "replaces"},
		{[]string{"objects", "hold", "-for", "2h"}, "running the same hold again is safe, but " +
			"it replaces the one in force: its length is counted from the resend", "writes nothing"},
	} {
		_, stderr, err := cli(t, append(tc.args, bootstrapForURL(t, node.server.URL))...)
		if err == nil {
			t.Fatalf("%v: an unanswered gesture reported success", tc.args)
		}
		if !strings.Contains(stderr, tc.says) || strings.Contains(stderr, tc.never) {
			t.Errorf("%v: stderr says:\n%s", tc.args, stderr)
		}
	}
}
