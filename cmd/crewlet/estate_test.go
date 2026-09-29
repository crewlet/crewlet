package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// estateGen is the fixture map's generation.
var estateGen = uuid.MustParse("7d3e2a10-4b6c-4e8f-9a1d-5c2b8f0e6a4d")

// estateFixture is a placed map over three members asking two copies:
// company.000 settled on data-a and data-b, tracker.000 with data-c joining
// beside them and data-b leaving, and tracker.001 moved off data-b by ops.
func estateFixture() partmap.MapState {
	member := func(node string) placement.Member {
		return placement.Member{Node: node, Weight: 1, Share: placement.DefaultShare(1)}
	}
	h := func(node string, state partmap.HolderState, since uint64) partmap.Holder {
		return partmap.Holder{Node: node, State: state, Since: since}
	}
	return partmap.MapState{
		Map: partmap.Map{
			Generation: estateGen, Epoch: 5, Replicas: 2,
			Layout: statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
				{Space: statelog.SpaceTracker, Partitions: 2, Domains: []string{"tracker"}},
				{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{"tracker"}},
			}},
			Members: []placement.Member{member("data-a"), member("data-b"), member("data-c")},
			Partitions: []partmap.Partition{
				{ID: "company.000", Holders: []partmap.Holder{
					h("data-a", partmap.Serving, 1), h("data-b", partmap.Serving, 1)}},
				{ID: "tracker.000", Holders: []partmap.Holder{
					h("data-a", partmap.Serving, 1), h("data-b", partmap.Leaving, 5),
					h("data-c", partmap.Joining, 5)}},
				{ID: "tracker.001", Holders: []partmap.Holder{
					h("data-a", partmap.Serving, 1), h("data-b", partmap.Serving, 1)}},
			},
			Moves: map[string]map[string]membership.Gesture{
				"tracker.001": {"data-b": {By: "ops", Reason: "disk", At: objectsAt}},
			},
		},
		Balance: partmap.Balance{Tolerance: 0.05, BalanceReport: placement.BalanceReport{
			Rounds: 2, Deviation: 0.01, Converged: true}},
	}
}

// estateFixtureLeases are the fixture's live estate leases, every node healthy
// at layout 1 and acting on epoch 5.
func estateFixtureLeases(t *testing.T) []coord.Lease {
	t.Helper()
	layout, healthy := 1, true
	var out []coord.Lease
	for node, parts := range map[string]map[string]partmap.PartitionState{
		"data-a": {"company.000": partmap.PartServing, "tracker.000": partmap.PartServing,
			"tracker.001": partmap.PartServing},
		"data-b": {"company.000": partmap.PartServing, "tracker.000": partmap.PartDraining,
			"tracker.001": partmap.PartServing},
		"data-c": {"tracker.000": partmap.PartAdopting},
	} {
		meta, err := partmap.Meta{Weight: 1, Layout: &layout, Healthy: &healthy,
			MapGeneration: estateGen, MapEpoch: 5, Partitions: parts}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, coord.Lease{Resource: coord.EstateResource(node), Meta: meta})
	}
	return out
}

// fakeEstateNode answers GET /estate and the gesture routes through the API's
// OWN renderers, applying partmap's own pure gestures to the map it holds — or
// refusing every one as a layout-0 node does, when it holds none.
type fakeEstateNode struct {
	server *httptest.Server
	leases []coord.Lease

	mu    sync.Mutex
	state partmap.MapState
	// whole makes it a layout-0 fleet: no map, and the data nodes' leases.
	whole bool
	// answer, when set, is served for GET /estate instead.
	answer *queries.FleetEstate
	asked  []*http.Request
}

func newFakeEstateNode(t *testing.T) *fakeEstateNode {
	t.Helper()
	n := &fakeEstateNode{state: estateFixture(), leases: estateFixtureLeases(t)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /estate", func(w http.ResponseWriter, _ *http.Request) {
		n.mu.Lock()
		defer n.mu.Unlock()
		var body queries.FleetEstate
		switch {
		case n.answer != nil:
			body = *n.answer
		case n.whole:
			zero, healthy := 0, true
			meta, _ := partmap.Meta{Weight: 1, Layout: &zero, Healthy: &healthy,
				Partitions: map[string]partmap.PartitionState{"estate.000": partmap.PartServing}}.Encode()
			body = queries.RenderEstateWhole(engine.LayoutZero(),
				[]coord.Lease{{Resource: coord.NodeResource("data-a")},
					{Resource: coord.NodeResource("data-b")}},
				[]coord.Lease{{Resource: coord.EstateResource("data-a"), Meta: meta}})
		default:
			body = queries.RenderEstate(n.state, n.leases, objectsAt)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	gesture := func(change func(r *http.Request, s partmap.MapState) (partmap.MapState, error),
		node func(*http.Request) string, partition func(*http.Request) string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			n.mu.Lock()
			defer n.mu.Unlock()
			n.asked = append(n.asked, r)
			w.Header().Set("Content-Type", "application/json")
			next, err := n.state, engine.ErrEstateWhole
			if !n.whole {
				next, err = change(r, n.state)
			}
			if refusal, ok := api.RenderEstateRefusal(err); ok {
				w.WriteHeader(refusal.Status)
				_ = json.NewEncoder(w).Encode(refusal.Body)
				return
			}
			n.state = next
			_ = json.NewEncoder(w).Encode(api.RenderEstateGesture(
				engine.EstateGesture{Landed: true, State: next}, node(r), partition(r), objectsAt))
		}
	}
	pathNode := func(r *http.Request) string { return r.PathValue("node") }
	fromNode := func(r *http.Request) string { return r.URL.Query().Get("from") }
	none := func(*http.Request) string { return "" }
	part := func(r *http.Request) string { return r.PathValue("partition") }
	parsed := func(r *http.Request) statelog.PartitionID {
		p, _ := statelog.ParsePartitionID(r.PathValue("partition"))
		return p
	}
	mux.HandleFunc("POST /estate/out/{node}", gesture(func(r *http.Request, s partmap.MapState) (partmap.MapState, error) {
		return partmap.Out(s, r.PathValue("node"), "ops", r.URL.Query().Get("reason"), objectsAt)
	}, pathNode, none))
	mux.HandleFunc("POST /estate/in/{node}", gesture(func(r *http.Request, s partmap.MapState) (partmap.MapState, error) {
		return partmap.In(s, r.PathValue("node"))
	}, pathNode, none))
	// A HOLD AND A RELEASE ARE JUDGED AGAINST THE MAP, as the engine judges
	// them — so a layout-0 node answers estate_whole whatever they repeat.
	confirmed := func(r *http.Request, s partmap.MapState) error {
		generation, err := uuid.Parse(r.URL.Query().Get("confirm"))
		switch {
		case err != nil:
			return engine.ErrEstateUnconfirmed
		case generation != s.Map.Generation:
			return engine.ErrEstateOtherMap
		}
		return nil
	}
	mux.HandleFunc("POST /estate/hold", gesture(func(r *http.Request, s partmap.MapState) (partmap.MapState, error) {
		if err := confirmed(r, s); err != nil {
			return s, err
		}
		d, _ := time.ParseDuration(r.URL.Query().Get("for"))
		return partmap.HoldFor(s, d, "ops", r.URL.Query().Get("reason"), objectsAt)
	}, none, none))
	mux.HandleFunc("POST /estate/release", gesture(func(r *http.Request, s partmap.MapState) (partmap.MapState, error) {
		if err := confirmed(r, s); err != nil {
			return s, err
		}
		return partmap.Release(s)
	}, none, none))
	mux.HandleFunc("POST /estate/move/{partition}", gesture(func(r *http.Request, s partmap.MapState) (partmap.MapState, error) {
		return partmap.Move(s, parsed(r), r.URL.Query().Get("from"), "ops", r.URL.Query().Get("reason"), objectsAt)
	}, fromNode, part))
	mux.HandleFunc("POST /estate/move/{partition}/cancel", gesture(func(r *http.Request, s partmap.MapState) (partmap.MapState, error) {
		return partmap.CancelMove(s, parsed(r), r.URL.Query().Get("from"))
	}, fromNode, part))
	n.server = httptest.NewServer(mux)
	t.Cleanup(n.server.Close)
	return n
}

// requests is every gesture the node received.
func (n *fakeEstateNode) requests() []*http.Request {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]*http.Request(nil), n.asked...)
}

// AT LAYOUT 0 THE MAP SAYS SO IN THE SENTENCE EVERY SURFACE GIVES, and lists
// the data nodes that hold the whole estate — one with no estate lease named
// as such rather than dropped.
func TestEstateMapAtLayoutZeroSaysEveryDataNodeHoldsTheWhole(t *testing.T) {
	node := newFakeEstateNode(t)
	node.whole = true
	out, stderr, err := cli(t, "estate", "map", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatalf("estate map: %v\n%s", err, stderr)
	}
	for _, want := range []string{partmap.WholeEstate, "ESTATE.000", "data-a", "serving",
		"data-b", "no estate lease"} {
		if !strings.Contains(out, want) {
			t.Errorf("the layout-0 map does not say %q:\n%s", want, out)
		}
	}
}

// A PLACED MAP NAMES WHAT IS NOT SETTLED, partition by partition, and nothing
// that is — unless asked for every partition.
func TestEstateMapNamesEveryPartitionThatIsNotSettled(t *testing.T) {
	node := newFakeEstateNode(t)
	out, _, err := cli(t, "estate", "map", bootstrapForURL(t, node.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Estate map, layout 1, epoch 5 · 2 of 2 copies of each partition · generation " +
			estateGen.String(),
		"3 partitions: tracker 2, company 1.",
		"NODE", "data-c", "moved off 1",
		"Partitions unserved 0 · short of copies 1 · holders joining 1 · leaving 1 · " +
			"moves in force 1",
		"tracker.000  1/2",
		"tracker.000", "data-b:leaving(draining)", "data-c:joining(adopting)",
		"tracker.001", "off data-b (ops)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the map does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "company.000") {
		t.Errorf("a settled partition is listed without -all:\n%s", out)
	}
	all, _, err := cli(t, "estate", "map", bootstrapForURL(t, node.server.URL), "-all")
	if err != nil || !strings.Contains(all, "company.000") {
		t.Errorf("-all does not list the settled partition (%v):\n%s", err, all)
	}
}

// A MOVE THE MEMBERS LEFT NO ROOM FOR SAYS IT WAITS: with data-c taken out, the
// two members left hold tracker.001's two copies, so its move off data-b is on
// the map but not in effect — data-b is back in the target — and the line says
// so rather than reading as a move the target ignores.
func TestEstateMapSaysWhichMovesWait(t *testing.T) {
	node := newFakeEstateNode(t)
	cfg := bootstrapForURL(t, node.server.URL)
	out, _, err := cli(t, "estate", "map", cfg)
	if err != nil || !strings.Contains(out, "off data-b (ops)") || strings.Contains(out, "waiting") {
		t.Fatalf("a move in effect printed (%v):\n%s", err, out)
	}
	node.mu.Lock()
	node.state, err = partmap.Out(node.state, "data-c", "ops", "", objectsAt)
	node.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	out, _, err = cli(t, "estate", "map", cfg)
	if err != nil || !strings.Contains(out, "off data-b (ops, waiting)") {
		t.Errorf("a waiting move printed (%v):\n%s", err, out)
	}
}

// THE STATES WITH NOTHING TO SHOW say their sentence: a map not written yet
// is a wait, and a map nobody could read is a failure.
func TestEstateMapSaysTheStatesWithNothingToShow(t *testing.T) {
	node := newFakeEstateNode(t)
	node.answer = &queries.FleetEstate{State: queries.EstateMapNone, Layout: 1,
		Detail: partmap.Unplaced(1)}
	out, _, err := cli(t, "estate", "map", bootstrapForURL(t, node.server.URL))
	if err != nil || !strings.Contains(out, partmap.Unplaced(1)) {
		t.Errorf("a fleet with no map yet printed (%v):\n%s", err, out)
	}
	node.answer = &queries.FleetEstate{State: queries.EstateMapUnavailable,
		Detail: "the estate map or the estate leases could not be read"}
	if _, _, err := cli(t, "estate", "map", bootstrapForURL(t, node.server.URL)); err == nil ||
		!strings.Contains(err.Error(), "could not be read") {
		t.Errorf("an unavailable map = %v, want its sentence as the failure", err)
	}
}

// EVERY GESTURE THAT MOVES A NODE'S COPIES IS CONFIRMED BEFORE IT IS SENT, by
// repeating the node — and a hold names its length before it is sent. (A hold's
// and a release's generation is the node's to judge: the next test.)
func TestEveryEstateGestureIsConfirmedBeforeItIsSent(t *testing.T) {
	node := newFakeEstateNode(t)
	cfg := bootstrapForURL(t, node.server.URL)
	for _, args := range [][]string{
		{"estate", "out", "data-a", cfg},
		{"estate", "out", "data-a", cfg, "-confirm", "data-b"},
		{"estate", "in", "data-a", cfg},
		{"estate", "hold", cfg, "-confirm", estateGen.String()},
		{"estate", "move", "tracker.001", cfg, "-from", "data-b"},
		{"estate", "move", "tracker.001", cfg, "-from", "data-b", "-confirm", "data-a"},
	} {
		if _, _, err := cli(t, args...); err == nil {
			t.Errorf("%v ran unconfirmed", args)
		}
	}
	if asked := node.requests(); len(asked) != 0 {
		t.Errorf("an unconfirmed gesture reached the node: %d requests", len(asked))
	}
}

// A HOLD OR A RELEASE WITH NO GENERATION IS THE NODE'S TO JUDGE, since only a
// map has one: where there is a map the command says, in its own words, to
// repeat the generation `map` prints — and the map is unchanged — and at layout
// 0, where `map` prints none, the node's own refusal says there is nothing to
// hold.
func TestAHoldOrAReleaseWithNoGenerationIsTheNodesToJudge(t *testing.T) {
	node := newFakeEstateNode(t)
	cfg := bootstrapForURL(t, node.server.URL)
	for _, args := range [][]string{
		{"estate", "hold", cfg, "-for", "1h"},
		{"estate", "release", cfg},
		{"estate", "release", cfg, "-confirm", "yes"},
	} {
		_, _, err := cli(t, args...)
		if err == nil || !strings.Contains(err.Error(), "-confirm") ||
			!strings.Contains(err.Error(), "crewlet estate map") {
			t.Errorf("%v under a map = %v, want this command's own words naming -confirm", args, err)
		}
	}
	if len(node.requests()) != 3 {
		t.Errorf("the node judged %d of the three, want all of them", len(node.requests()))
	}
	node.mu.Lock()
	unchanged := reflect.DeepEqual(node.state, estateFixture())
	node.whole = true
	node.mu.Unlock()
	if !unchanged {
		t.Error("an unconfirmed gesture changed the map")
	}

	for _, args := range [][]string{
		{"estate", "hold", cfg, "-for", "1h"},
		{"estate", "release", cfg},
	} {
		_, _, err := cli(t, args...)
		if err == nil || !strings.Contains(err.Error(), "estate_whole") ||
			!strings.Contains(err.Error(), partmap.WholeEstate) {
			t.Errorf("%v at layout 0 = %v, want the node's estate_whole refusal", args, err)
		}
	}
}

// A CONFIRMED GESTURE IS SENT AS THE ROUTE READS IT, and says what the map now
// does.
func TestAConfirmedEstateGestureReachesItsRoute(t *testing.T) {
	node := newFakeEstateNode(t)
	cfg := bootstrapForURL(t, node.server.URL)
	out, _, err := cli(t, "estate", "move", "tracker.000", cfg, "-from", "data-a",
		"-confirm", "data-a", "-reason", "rebalancing")
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if !strings.Contains(out, "tracker.000 moves off data-a") {
		t.Errorf("the move printed:\n%s", out)
	}
	if _, _, err := cli(t, "estate", "move", "tracker.000", cfg, "-from", "data-a",
		"-confirm", "data-a", "-cancel"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	out, _, err = cli(t, "estate", "hold", cfg, "-for", "2h", "-confirm", estateGen.String())
	if err != nil || !strings.Contains(out, "held until 2026-09-01T14:00:00Z") {
		t.Errorf("hold (%v):\n%s", err, out)
	}
	if _, _, err := cli(t, "estate", "release", cfg, "-confirm", estateGen.String()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, _, err := cli(t, "estate", "out", "data-c", cfg, "-confirm", "data-c"); err != nil {
		t.Fatalf("out: %v", err)
	}
	want := []string{
		"/estate/move/tracker.000?confirm=data-a&from=data-a&reason=rebalancing",
		"/estate/move/tracker.000/cancel?confirm=data-a&from=data-a",
		"/estate/hold?confirm=" + estateGen.String() + "&for=2h0m0s",
		"/estate/release?confirm=" + estateGen.String(),
		"/estate/out/data-c?confirm=data-c",
	}
	asked := node.requests()
	if len(asked) != len(want) {
		t.Fatalf("the node received %d gestures, want %d", len(asked), len(want))
	}
	for i, r := range asked {
		if got := r.URL.RequestURI(); got != want[i] {
			t.Errorf("gesture %d reached %s, want %s", i, got, want[i])
		}
	}
}

// AT LAYOUT 0 A GESTURE FAILS WITH THE NODE'S OWN WORDS, which say there is
// nothing to move rather than something to wait for.
func TestAnEstateGestureAtLayoutZeroSaysThereIsNothingToMove(t *testing.T) {
	node := newFakeEstateNode(t)
	node.whole = true
	_, _, err := cli(t, "estate", "out", "data-a", bootstrapForURL(t, node.server.URL),
		"-confirm", "data-a")
	if err == nil || !strings.Contains(err.Error(), "estate_whole") ||
		!strings.Contains(err.Error(), partmap.WholeEstate) {
		t.Errorf("an out at layout 0 = %v, want the node's estate_whole refusal", err)
	}
}

// AN OUT THAT WOULD DROP A COPY FAILS WITH WHAT TO CHANGE: with data-c out,
// data-a and data-b are all the map's two copies have, so taking either out
// would leave every partition a copy short rather than move it — refused by
// the node, in words naming the field that means fewer, and nothing changed.
func TestAnEstateOutWithNowhereToRebuildSaysWhatToChange(t *testing.T) {
	node := newFakeEstateNode(t)
	cfg := bootstrapForURL(t, node.server.URL)
	if _, _, err := cli(t, "estate", "out", "data-c", cfg, "-confirm", "data-c"); err != nil {
		t.Fatalf("taking out the member to spare: %v", err)
	}
	_, _, err := cli(t, "estate", "out", "data-a", cfg, "-confirm", "data-a")
	if err == nil || !strings.Contains(err.Error(), "nowhere_to_rebuild") ||
		!strings.Contains(err.Error(), "lower estate.replicas") {
		t.Errorf("an out with nowhere to rebuild = %v, want the node's nowhere_to_rebuild refusal", err)
	}
	if m, _ := node.state.Map.Draw().Member("data-a"); m.Out {
		t.Error("a refused out took the member out")
	}
}
