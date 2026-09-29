package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
)

// estateGolden is the estate map's renderings — the estate question's answer
// in each of its states, every gesture answer and every refusal — committed,
// and read by the dashboard's Estate screen suite as its fixture: see
// [api.EstateAnswer].
const estateGolden = "testdata/estate_answer.json"

// estateSince is the instant every fixture's gestures and absences are stamped
// at, so the golden is stable.
var estateSince = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// estateGeneration is the fixture map's lineage.
var estateGeneration = uuid.MustParse("7d3e2a10-4b6c-4e8f-9a1d-5c2b8f0e6a4d")

// estateLayout is a partitioned layout small enough to read partition by
// partition: four tracker partitions, one pages partition and the company's.
var estateLayout = statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
	{Space: statelog.SpaceTracker, Partitions: 4, Domains: []string{"tracker", "vectors"}},
	{Space: statelog.SpacePages, Partitions: 1, Domains: []string{"pages", "vectors"}},
	{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{"tracker"}},
}}

// estateFleet is a map of five members across three zones asking two copies —
// data-a at twice the weight; data-b gone for twelve ticks and back for three;
// data-c up; data-d taken out and leaving what it held; data-f removed for
// absence and back on probation — and data-e, removed and remembered. Its six
// partitions show every holder state: company.000 whole, pages.000 short while
// data-b is gone, tracker.000 with a joiner and a leaver, tracker.001 served by
// nobody that can answer, tracker.002 with a copy an operator moved off
// data-c, and tracker.003 on the member back on probation.
func estateFleet() partmap.MapState {
	member := func(node string, weight int, zone string) placement.Member {
		return placement.Member{Node: node, Weight: weight,
			Share: placement.DefaultShare(weight), Domain: zone}
	}
	out := member("data-d", 1, "eu-1")
	out.Out = true
	probation := member("data-f", 1, "eu-2")
	probation.Probation = true
	h := func(node string, state partmap.HolderState, since uint64) partmap.Holder {
		return partmap.Holder{Node: node, State: state, Since: since}
	}
	return partmap.MapState{
		Map: partmap.Map{
			Generation: estateGeneration, Epoch: 9, Layout: estateLayout,
			Replicas: 2, FailureDomain: "zone",
			Members: []placement.Member{
				member("data-a", 2, "eu-1"), member("data-b", 1, "eu-2"),
				member("data-c", 1, "eu-3"), out, probation,
			},
			Partitions: []partmap.Partition{
				{ID: "company.000", Holders: []partmap.Holder{
					h("data-a", partmap.Serving, 2), h("data-c", partmap.Serving, 2)}},
				{ID: "pages.000", Holders: []partmap.Holder{
					h("data-a", partmap.Serving, 2), h("data-b", partmap.Serving, 3)}},
				{ID: "tracker.000", Holders: []partmap.Holder{
					h("data-a", partmap.Joining, 9), h("data-c", partmap.Serving, 2),
					h("data-d", partmap.Leaving, 8)}},
				{ID: "tracker.001", Holders: []partmap.Holder{
					h("data-b", partmap.Serving, 2), h("data-c", partmap.Joining, 7)}},
				{ID: "tracker.002", Holders: []partmap.Holder{
					h("data-a", partmap.Serving, 2), h("data-c", partmap.Serving, 2)}},
				{ID: "tracker.003", Holders: []partmap.Holder{
					h("data-a", partmap.Serving, 4), h("data-f", partmap.Serving, 3)}},
			},
			Moves: map[string]map[string]membership.Gesture{
				"tracker.002": {"data-c": {By: "founder", Reason: "disk swap", At: estateSince}},
			},
		},
		State: membership.State{
			Absence: map[string]membership.Absence{"data-b": {Ticks: 12, Present: 3,
				Since: estateSince, Reason: membership.ReasonAbsent}},
			TakenOut: map[string]membership.Gesture{"data-d": {By: "founder",
				Reason: "decommission", At: estateSince}},
			Removed: map[string]membership.Removal{
				"data-e": {Gone: 6, At: estateSince, Reason: membership.ReasonUnhealthy,
					Detail: "its store says it cannot hold partitions"},
				"data-f": {Present: 12, At: estateSince.Add(-time.Hour),
					Reason: membership.ReasonAbsent},
			},
			Config: membership.ConfigSource{Epoch: 4},
		},
		Balance: partmap.Balance{Tolerance: 0.05, BalanceReport: placement.BalanceReport{
			Rounds: 4, Deviation: 0.031, Converged: true}},
	}
}

// otherGeneration is another lineage of the estate map than the fixture's: one
// written before the map was written again from nothing.
var otherGeneration = uuid.MustParse("0b9f4c2e-6a1d-4f3b-8e7c-2d5a9f1b3c6e")

// estateLeases are the live estate leases [estateFleet]'s members hold: data-b
// holds none, data-d is draining what it was told to leave, data-c is still
// catching up on the partition it joins, data-f is back and serving what it
// kept, having last acted on epoch 14 of ANOTHER LINEAGE of the map — no proof
// of having read this one, so its epoch is not this map's — and data-g, a node
// the map does not hold yet, which the maintainer adds at its next tick and the
// answer ignores.
func estateLeases(t *testing.T) []coord.Lease {
	t.Helper()
	layout, healthy := estateLayout.Number, true
	lease := func(node string, weight int, epoch uint64, parts map[string]partmap.PartitionState) coord.Lease {
		generation := estateGeneration
		if node == "data-f" {
			generation = otherGeneration
		}
		meta, err := partmap.Meta{Weight: weight, Layout: &layout, Healthy: &healthy,
			MapGeneration: generation, MapEpoch: epoch, Partitions: parts,
			FreeBytes: 512 << 30}.Encode()
		if err != nil {
			t.Fatal(err)
		}
		return coord.Lease{Resource: coord.EstateResource(node), Owner: node + ":1", Meta: meta}
	}
	return []coord.Lease{
		lease("data-a", 2, 9, map[string]partmap.PartitionState{
			"company.000": partmap.PartServing, "pages.000": partmap.PartServing,
			"tracker.000": partmap.PartAdopting, "tracker.002": partmap.PartServing,
			"tracker.003": partmap.PartServing}),
		lease("data-c", 1, 9, map[string]partmap.PartitionState{
			"company.000": partmap.PartServing, "tracker.000": partmap.PartServing,
			"tracker.001": partmap.PartCatchingUp, "tracker.002": partmap.PartServing}),
		lease("data-d", 1, 8, map[string]partmap.PartitionState{
			"tracker.000": partmap.PartDraining}),
		lease("data-f", 1, 14, map[string]partmap.PartitionState{
			"tracker.003": partmap.PartServing}),
		lease("data-g", 1, 0, map[string]partmap.PartitionState{}),
	}
}

// wholeLeases are a layout-0 fleet's presence and estate leases: two data nodes
// — data-a claiming its estate lease and serving the whole estate, data-b a
// build from before the lease — and a stateless node, which holds nothing.
// data-a's lease still names a map epoch it once acted on, which layout 0 —
// with no map to have acted on — never renders.
func wholeLeases(t *testing.T) (presence, estate []coord.Lease) {
	t.Helper()
	node := func(id string, roles ...string) coord.Lease {
		meta := map[string]any{}
		if len(roles) > 0 {
			meta["roles"] = roles
		}
		return coord.Lease{Resource: coord.NodeResource(id), Owner: id + ":1", Meta: meta}
	}
	zero, healthy := 0, true
	meta, err := partmap.Meta{Weight: 1, Layout: &zero, Healthy: &healthy,
		MapGeneration: otherGeneration, MapEpoch: 7,
		Partitions: map[string]partmap.PartitionState{"estate.000": partmap.PartServing},
		FreeBytes:  96 << 30}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return []coord.Lease{node("data-b", "data", "seats"), node("data-a"), node("edge-1", "seats")},
		[]coord.Lease{{Resource: coord.EstateResource("data-a"), Owner: "data-a:1", Meta: meta}}
}

// fakeEstate applies the engine's own pure gestures to a map it holds, the way
// [engine.EstateControl] applies them to the stored one — and refuses as it
// does where there is none.
type fakeEstate struct {
	mu      sync.Mutex
	state   partmap.MapState
	found   bool
	running statelog.Layout

	// err is answered before anything is read; lose makes every
	// compare-and-set lose.
	err  error
	lose bool

	// asked is every gesture made, as "verb args".
	asked []string
}

func newFakeEstate() *fakeEstate {
	return &fakeEstate{state: estateFleet(), found: true, running: engine.LayoutZero()}
}

func (f *fakeEstate) Running() statelog.Layout { return f.running }

func (f *fakeEstate) EstateMap(context.Context) (coord.EstateMapRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.found {
		return coord.EstateMapRecord{}, false, f.err
	}
	raw, err := f.state.Encode()
	return coord.EstateMapRecord{Value: raw, Version: 3}, true, errors.Join(err, f.err)
}

// apply is one gesture, judged in the engine's order: the map's existence
// first, and only then a confirmation that names the map — confirm is nil for
// a gesture confirmed by its node, which the route checked.
func (f *fakeEstate) apply(asked string, confirm *string,
	change func(partmap.MapState) (partmap.MapState, error)) (engine.EstateGesture, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, asked)
	switch {
	case f.err != nil:
		return engine.EstateGesture{}, f.err
	case !f.found && f.running.Number == 0:
		return engine.EstateGesture{}, engine.ErrEstateWhole
	case !f.found:
		return engine.EstateGesture{}, fmt.Errorf("%w: %s", partmap.ErrNoMap,
			partmap.Unplaced(f.running.Number))
	}
	if confirm != nil {
		generation, err := uuid.Parse(*confirm)
		switch {
		case err != nil:
			return engine.EstateGesture{State: f.state}, engine.ErrEstateUnconfirmed
		case generation != f.state.Map.Generation:
			return engine.EstateGesture{State: f.state}, engine.ErrEstateOtherMap
		}
	}
	next, err := change(f.state)
	if err != nil {
		return engine.EstateGesture{State: f.state}, err
	}
	if f.lose {
		return engine.EstateGesture{State: f.state}, nil
	}
	f.state = next
	return engine.EstateGesture{Landed: true, State: next}, nil
}

func (f *fakeEstate) Out(_ context.Context, node, by, reason string) (engine.EstateGesture, error) {
	return f.apply(strings.Join([]string{"out", node, by, reason}, " "), nil,
		func(s partmap.MapState) (partmap.MapState, error) {
			return partmap.Out(s, node, by, reason, estateSince)
		})
}

func (f *fakeEstate) In(_ context.Context, node, by string) (engine.EstateGesture, error) {
	return f.apply(strings.Join([]string{"in", node, by}, " "), nil,
		func(s partmap.MapState) (partmap.MapState, error) { return partmap.In(s, node) })
}

func (f *fakeEstate) Hold(_ context.Context, confirm string, d time.Duration,
	by, reason string) (engine.EstateGesture, error) {
	return f.apply(strings.Join([]string{"hold", d.String(), by, reason}, " "), &confirm,
		func(s partmap.MapState) (partmap.MapState, error) {
			return partmap.HoldFor(s, d, by, reason, clock)
		})
}

func (f *fakeEstate) Release(_ context.Context, confirm, by string) (engine.EstateGesture, error) {
	return f.apply("release "+by, &confirm, partmap.Release)
}

func (f *fakeEstate) Move(_ context.Context, p statelog.PartitionID, node, by, reason string) (engine.EstateGesture, error) {
	return f.apply(strings.Join([]string{"move", p.String(), node, by, reason}, " "), nil,
		func(s partmap.MapState) (partmap.MapState, error) {
			return partmap.Move(s, p, node, by, reason, estateSince)
		})
}

func (f *fakeEstate) CancelMove(_ context.Context, p statelog.PartitionID, node, by string) (engine.EstateGesture, error) {
	return f.apply(strings.Join([]string{"cancel", p.String(), node, by}, " "), nil,
		func(s partmap.MapState) (partmap.MapState, error) {
			return partmap.CancelMove(s, p, node)
		})
}

// gestures is every gesture the fake was asked to make.
func (f *fakeEstate) gestures() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// estateApp is an authenticated node mounting the estate map over f, with a
// lease table listing leases.
func estateApp(t *testing.T, f *fakeEstate, leases ...coord.Lease) *api.App {
	t.Helper()
	b := closedPosture()
	opts := api.Options{Bootstrap: &b}
	if f != nil {
		opts.Estate = f
		opts.Sources.Coord = leaseTable(t, leases...)
	}
	return newApp(t, opts)
}

// leaseTable is a coordination store holding these live leases.
func leaseTable(t *testing.T, leases ...coord.Lease) coord.Backend {
	t.Helper()
	backend := coordmemory.New()
	for _, l := range leases {
		if _, refused, err := backend.TryAcquire(t.Context(), l.Resource, coord.AcquireOptions{
			Owner: l.Owner, TTL: time.Hour, Meta: l.Meta, Ungated: true}); err != nil || refused != "" {
			t.Fatalf("setup: hold %s: (%s, %v)", l.Resource, refused, err)
		}
	}
	return backend
}

// generation is the fixture map's generation as a confirmation.
var generation = "confirm=" + estateGeneration.String()

// A NODE WITH NO COORDINATION STORE MOUNTS NO GESTURE AND ASKS NO QUESTION,
// rather than routes that refuse: there is no map for it to read or change.
func TestTheEstateSurfacesAreAbsentWithoutAStore(t *testing.T) {
	t.Parallel()
	a := estateApp(t, nil)
	for _, path := range []string{"/estate/out/data-a?confirm=data-a",
		"/estate/in/data-a?confirm=data-a", "/estate/hold?for=1h&" + generation,
		"/estate/release?" + generation,
		"/estate/move/tracker.002?from=data-a&confirm=data-a",
		"/estate/move/tracker.002/cancel?from=data-a&confirm=data-a"} {
		if status, body := postObjects(t, a, path); status != http.StatusNotFound ||
			body["error"] != "no_route" {
			t.Errorf("%s answered %d %v on a node with no coordination store", path, status, body)
		}
	}
	if status, _ := getAs(t, a, "/estate"); status != http.StatusNotFound {
		t.Errorf("GET /estate answered %d on a node with no coordination store, want 404", status)
	}
}

// EVERY GESTURE IS CONFIRMED, and none changes the map until it is: the four
// that move a node's copies repeat the node, checked before the map is asked
// at all, and the two that act on the whole map repeat its generation, judged
// against the map itself.
func TestEveryEstateGestureNeedsItsConfirmation(t *testing.T) {
	t.Parallel()
	f := newFakeEstate()
	a := estateApp(t, f)
	byNode := []string{
		"/estate/out/data-a", "/estate/out/data-a?confirm=data-b",
		"/estate/in/data-d?confirm=",
		"/estate/move/tracker.002?from=data-c", "/estate/move/tracker.002?confirm=data-c",
		"/estate/move/tracker.002?from=data-c&confirm=data-a",
		"/estate/move/tracker.002/cancel?from=data-c",
	}
	byMap := []string{
		"/estate/hold?for=1h", "/estate/hold?for=1h&confirm=data-a",
		"/estate/release", "/estate/release?confirm=release",
	}
	for _, path := range append(byNode, byMap...) {
		status, body := postObjects(t, a, path)
		if status != http.StatusBadRequest || body["error"] != "confirm_required" ||
			body["detail"] == "" || body["hint"] == "" {
			t.Errorf("%s answered %d %v, want 400 confirm_required with a detail and a hint",
				path, status, body)
		}
	}
	for _, asked := range f.gestures() {
		if !strings.HasPrefix(asked, "hold ") && !strings.HasPrefix(asked, "release ") {
			t.Errorf("a gesture unconfirmed by its node still reached the map: %q", asked)
		}
	}
	if !reflect.DeepEqual(f.state, estateFleet()) {
		t.Error("an unconfirmed gesture changed the map")
	}
}

// A MOVE NAMES A PARTITION, and one that names nothing a partition could be is
// refused before the map is asked — as is a hold for no length at all.
func TestAMoveNamesAPartitionAndAHoldALength(t *testing.T) {
	t.Parallel()
	f := newFakeEstate()
	a := estateApp(t, f)
	for path, code := range map[string]string{
		"/estate/move/tracker.7?from=data-c&confirm=data-c":          "partition_invalid",
		"/estate/move/estate/cancel?from=data-c&confirm=data-c":      "partition_invalid",
		"/estate/hold?" + generation:                                 "invalid_hold",
		"/estate/hold?for=soon&" + generation:                        "invalid_hold",
		"/estate/move/tracker.009?from=data-c&confirm=data-c":        "unknown_partition",
		"/estate/move/tracker.001?from=data-a&confirm=data-a":        "not_a_holder",
		"/estate/hold?for=25h&" + generation:                         "invalid_hold",
		"/estate/out/data-z?confirm=data-z":                          "unknown_member",
		"/estate/out/data-e?confirm=data-e":                          "removed_member",
		"/estate/hold?for=1h&confirm=" + uuid.Nil.String():           "other_estate_map",
		"/estate/release?confirm=" + uuid.NewString():                "other_estate_map",
		"/estate/move/tracker.002/cancel?from=data-z&confirm=data-z": "",
	} {
		status, body := postObjects(t, a, path)
		if code == "" {
			// A CANCEL OF A MOVE THAT DOES NOT EXIST answers the map as
			// it is, so a resent cancel writes nothing.
			if status != http.StatusOK || body["landed"] != true {
				t.Errorf("%s answered %d %v, want the map as it is", path, status, body)
			}
			continue
		}
		if body["error"] != code || body["detail"] == "" || body["hint"] == "" {
			t.Errorf("%s answered %d %v, want %s with a detail and a hint", path, status, body, code)
		}
	}
	if !reflect.DeepEqual(f.state, estateFleet()) {
		t.Error("a refused gesture changed the map")
	}
}

// A GESTURE THAT LANDS IS ANSWERED WITH WHAT THE MAP NOW SAYS: a move with the
// partition's new target and who asked, a cancel with no move, an out with the
// member out — and no epoch moved, since a gesture changes targets and never
// the holder table.
func TestAnEstateGestureAnswersWhatTheMapNowSays(t *testing.T) {
	t.Parallel()
	f := newFakeEstate()
	a := estateApp(t, f)
	status, body := postObjects(t, a, "/estate/move/tracker.003?from=data-f&confirm=data-f&reason=probation")
	move, _ := body["move"].(map[string]any)
	target, _ := body["target"].([]any)
	if status != http.StatusOK || body["landed"] != true || body["epoch"] != float64(9) ||
		move["by"] != "founder" || move["reason"] != "probation" || len(target) == 0 {
		t.Fatalf("a move answered %d %v", status, body)
	}
	for _, node := range target {
		if node == "data-f" {
			t.Errorf("the moved partition's target still names the node it moved off: %v", target)
		}
	}
	status, body = postObjects(t, a, "/estate/move/tracker.003/cancel?from=data-f&confirm=data-f")
	if status != http.StatusOK || body["landed"] != true || body["move"] != nil {
		t.Errorf("a cancel answered %d %v, want no move in force", status, body)
	}
	status, body = postObjects(t, a, "/estate/out/data-c?confirm=data-c&reason=retiring")
	member, _ := body["member"].(map[string]any)
	if status != http.StatusOK || member["out"] != true || member["out_reason"] != "retiring" ||
		body["epoch"] != float64(9) {
		t.Errorf("an out answered %d %v", status, body)
	}
	status, body = postObjects(t, a, "/estate/hold?for=2h&reason=rack+work&"+generation)
	hold, _ := body["hold"].(map[string]any)
	if status != http.StatusOK || hold["until"] != clock.Add(2*time.Hour).Format(time.RFC3339Nano) ||
		body["generation"] != estateGeneration.String() {
		t.Errorf("a hold answered %d %v", status, body)
	}
	if asked := f.gestures(); asked[0] != "move tracker.003 data-f founder probation" {
		t.Errorf("the gestures reached the map as %v", asked)
	}
}

// UNDER LAYOUT 0 EVERY GESTURE IS REFUSED IN THE LAYOUT'S OWN WORDS, and not as
// a map to wait for: `estate_whole`, 409, the sentence every surface gives it.
// At a partitioned layout with no map yet the same gesture is a wait.
//
// A HOLD AND A RELEASE REACH IT WITH WHATEVER THEY WERE CONFIRMED BY — nothing,
// or a word that is no generation — because a fleet with no map has no
// generation to repeat: asked for one first, an operator at layout 0 could
// never be told why there is nothing to hold.
func TestUnderLayoutZeroEveryEstateGestureIsRefusedWhole(t *testing.T) {
	t.Parallel()
	f := newFakeEstate()
	f.found = false
	a := estateApp(t, f)
	unconfirmed := []string{"/estate/hold?for=1h", "/estate/hold?for=1h&confirm=whole",
		"/estate/release", "/estate/release?confirm=whole"}
	for _, path := range append([]string{"/estate/out/data-a?confirm=data-a",
		"/estate/in/data-a?confirm=data-a", "/estate/hold?for=1h&" + generation,
		"/estate/release?" + generation,
		"/estate/move/tracker.002?from=data-a&confirm=data-a",
		"/estate/move/tracker.002/cancel?from=data-a&confirm=data-a"}, unconfirmed...) {
		status, body := postObjects(t, a, path)
		if status != http.StatusConflict || body["error"] != "estate_whole" ||
			!strings.Contains(body["detail"].(string), partmap.WholeEstate) {
			t.Errorf("%s under layout 0 answered %d %v", path, status, body)
		}
	}
	f.running = engine.DefaultLayoutOne()
	for _, path := range append([]string{"/estate/out/data-a?confirm=data-a"}, unconfirmed...) {
		status, body := postObjects(t, a, path)
		if status != http.StatusServiceUnavailable || body["error"] != "no_estate_map" {
			t.Errorf("%s on a partitioned fleet with no map answered %d %v, want 503 "+
				"no_estate_map", path, status, body)
		}
	}
}

// THE READ IS OPERATOR-ONLY, like the fleet's: which node holds which partition
// is the deployment's shape. And it answers the map's own rendering.
func TestTheEstateReadIsTheRenderersAnswerForAnOperator(t *testing.T) {
	t.Parallel()
	f := newFakeEstate()
	// ON A NODE THAT SERVES ANONYMOUS READS, which is what makes the
	// registration load-bearing: a guarded node refuses every anonymous
	// read whatever the question.
	open := newApp(t, api.Options{Bootstrap: guarded(), Estate: f,
		Sources: queries.Sources{Coord: leaseTable(t, estateLeases(t)...)}})
	if status, body := get(t, open, "/estate"); status != http.StatusUnauthorized {
		t.Errorf("an anonymous GET /estate on a node serving anonymous reads answered %d %v, "+
			"want 401", status, body)
	}
	a := estateApp(t, f, estateLeases(t)...)
	status, body := getAs(t, a, "/estate")
	if status != http.StatusOK {
		t.Fatalf("GET /estate answered %d %v", status, body)
	}
	want, err := json.Marshal(queries.RenderEstate(estateFleet(), estateLeases(t), clock))
	if err != nil {
		t.Fatal(err)
	}
	var wantBody map[string]any
	if err := json.Unmarshal(want, &wantBody); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body, wantBody) {
		t.Errorf("GET /estate is not the renderer's answer:\n%v\nwant\n%v", body, wantBody)
	}
}

// renderEstateScenarios is the golden's content: the estate question's answer
// in each of its states, and every gesture answer and refusal, each rendered by
// the route's own renderer.
func renderEstateScenarios(t *testing.T) []byte {
	t.Helper()
	fleet := estateFleet()
	held := estateFleet()
	held.Hold = &membership.Hold{Until: estateSince.Add(2 * time.Hour), By: "founder",
		Reason: "kernel upgrade", At: estateSince}
	// THE FLEET WITH data-b TAKEN OUT AS WELL: two members left to place
	// two copies on, so tracker.002's move off data-c has nobody to rebuild
	// the copy on and waits, data-c back in its target.
	waiting, err := partmap.Out(fleet, "data-b", "founder", "decommission", estateSince)
	if err != nil {
		t.Fatal(err)
	}
	if !waiting.Map.MoveWaiting(statelog.PartitionID{Space: statelog.SpaceTracker, Index: 2}, "data-c") {
		t.Fatal("the premise: with data-b out, the move off data-c waits")
	}
	presence, wholeEstate := wholeLeases(t)
	maps := map[string]queries.FleetEstate{
		"placed":  queries.RenderEstate(fleet, estateLeases(t), estateSince),
		"held":    queries.RenderEstate(held, estateLeases(t), estateSince),
		"waiting": queries.RenderEstate(waiting, estateLeases(t), estateSince),
		"whole":   queries.RenderEstateWhole(engine.LayoutZero(), presence, wholeEstate),
	}
	// THE THREE STATES WITH NOTHING TO SHOW, as the question answers them.
	for name, src := range map[string]*fakeEstate{
		"no_map":      {running: engine.DefaultLayoutOne()},
		"unavailable": {running: engine.LayoutZero(), err: errors.New("nats: timeout")},
		"unreadable":  {running: engine.LayoutZero(), found: true},
	} {
		sources := queries.Sources{Estate: src, Coord: leaseTable(t)}
		if name == "unreadable" {
			sources.Estate = unreadableEstate{src}
		}
		r := queries.NewRegistry()
		queries.Register(r, sources)
		got, err := r.AnswerWith(t.Context(), "estate", queries.Params{}, "founder")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		maps[name] = got.(queries.FleetEstate)
	}

	gesture := func(change func(partmap.MapState) (partmap.MapState, error)) engine.EstateGesture {
		next, err := change(estateFleet())
		if err != nil {
			t.Fatal(err)
		}
		return engine.EstateGesture{Landed: true, State: next}
	}
	answers := map[string]api.EstateAnswer{
		"out": api.RenderEstateGesture(gesture(func(s partmap.MapState) (partmap.MapState, error) {
			return partmap.Out(s, "data-c", "founder", "disk swap", estateSince)
		}), "data-c", "", estateSince),
		"in": api.RenderEstateGesture(gesture(func(s partmap.MapState) (partmap.MapState, error) {
			return partmap.In(s, "data-d")
		}), "data-d", "", estateSince),
		"in_removed": api.RenderEstateGesture(gesture(func(s partmap.MapState) (partmap.MapState, error) {
			return partmap.In(s, "data-e")
		}), "data-e", "", estateSince),
		"hold": api.RenderEstateGesture(gesture(func(s partmap.MapState) (partmap.MapState, error) {
			return partmap.HoldFor(s, 2*time.Hour, "founder", "kernel upgrade", estateSince)
		}), "", "", estateSince),
		"release": api.RenderEstateGesture(gesture(func(partmap.MapState) (partmap.MapState, error) {
			return partmap.Release(held)
		}), "", "", estateSince),
		"move": api.RenderEstateGesture(gesture(func(s partmap.MapState) (partmap.MapState, error) {
			return partmap.Move(s, statelog.PartitionID{Space: statelog.SpaceTracker, Index: 0},
				"data-c", "founder", "disk swap", estateSince)
		}), "data-c", "tracker.000", estateSince),
		"cancel": api.RenderEstateGesture(gesture(func(s partmap.MapState) (partmap.MapState, error) {
			return partmap.CancelMove(s, statelog.PartitionID{Space: statelog.SpaceTracker, Index: 2},
				"data-c")
		}), "data-c", "tracker.002", estateSince),
		"not_landed": api.RenderEstateGesture(engine.EstateGesture{State: fleet},
			"data-c", "", estateSince),
	}

	// THE GESTURES' REFUSALS AS THE ESTATE MAP ANSWERS THEM — through its own
	// gestures, so each detail is the text an operator is shown.
	refusedBy := func(what string, gesture func() (partmap.MapState, error)) error {
		t.Helper()
		_, err := gesture()
		if err == nil {
			t.Fatalf("%s was not refused", what)
		}
		return err
	}
	tracker := func(i uint16) statelog.PartitionID {
		return statelog.PartitionID{Space: statelog.SpaceTracker, Index: i}
	}
	lone := fleet.Clone()
	lone.Map.Members = lone.Map.Members[:3]
	lone.Map.Members[1].Out, lone.Map.Members[2].Out = true, true
	lone.TakenOut = nil
	// AS MANY PLACEABLE MEMBERS AS COPIES: data-c taken out as well, so a
	// move off either of the two left has nowhere to rebuild the copy.
	tight := fleet.Clone()
	tight.Map.Members[2].Out = true
	tight.TakenOut["data-c"] = membership.Gesture{By: "founder", At: estateSince}
	refusals := map[string]api.EstateRefusal{}
	for name, err := range map[string]error{
		"unknown_member": refusedBy("out of a stranger", func() (partmap.MapState, error) {
			return partmap.Out(fleet, "data-z", "founder", "", estateSince)
		}),
		"removed_member": refusedBy("out of a removed node", func() (partmap.MapState, error) {
			return partmap.Out(fleet, "data-e", "founder", "", estateSince)
		}),
		"estate_refused": refusedBy("out of the last member", func() (partmap.MapState, error) {
			return partmap.Out(lone, "data-a", "founder", "", estateSince)
		}),
		"invalid_hold": refusedBy("a hold past a day", func() (partmap.MapState, error) {
			return partmap.HoldFor(fleet, 25*time.Hour, "founder", "", estateSince)
		}),
		"unknown_partition": refusedBy("a move of a partition the layout lacks", func() (partmap.MapState, error) {
			return partmap.Move(fleet, tracker(9), "data-a", "founder", "", estateSince)
		}),
		"not_a_holder": refusedBy("a move off a node holding nothing of it", func() (partmap.MapState, error) {
			return partmap.Move(fleet, tracker(1), "data-a", "founder", "", estateSince)
		}),
		"nowhere_to_move": refusedBy("a move with no member to rebuild on", func() (partmap.MapState, error) {
			return partmap.Move(tight, tracker(2), "data-a", "founder", "", estateSince)
		}),
		"estate_whole":       engine.ErrEstateWhole,
		"no_estate_map":      fmt.Errorf("%w: %s", partmap.ErrNoMap, partmap.Unplaced(1)),
		"confirm_required":   fmt.Errorf("%w: %q is not a map generation", engine.ErrEstateUnconfirmed, "data-a"),
		"other_estate_map":   fmt.Errorf("%w: it was confirmed for generation %s", engine.ErrEstateOtherMap, uuid.Nil),
		"estate_unavailable": fmt.Errorf("%w: nats: timeout", engine.ErrEstateUnavailable),
		"estate_newer_map":   fmt.Errorf("%w: unknown field", engine.ErrEstateNewerMap),
	} {
		refusal, ok := api.RenderEstateRefusal(err)
		if !ok || refusal.Body.Error != name {
			t.Fatalf("%s renders as %+v (%v)", name, refusal, err)
		}
		refusals[name] = refusal
	}
	type goldenRefusal struct {
		Status int                   `json:"status"`
		Body   api.EstateRefusalBody `json:"body"`
	}
	bodies := map[string]goldenRefusal{}
	for name, r := range refusals {
		bodies[name] = goldenRefusal{Status: r.Status, Body: r.Body}
	}
	out, err := json.MarshalIndent(map[string]any{
		"estate": maps, "answers": answers, "refusals": bodies,
	}, "", "  ")
	if err != nil {
		t.Fatalf("encode the estate answers: %v", err)
	}
	return append(out, '\n')
}

// unreadableEstate answers a stored record no build could decode.
type unreadableEstate struct{ *fakeEstate }

func (u unreadableEstate) EstateMap(context.Context) (coord.EstateMapRecord, bool, error) {
	return coord.EstateMapRecord{Value: []byte(`{"map":{"epoch":"nine"}}`), Version: 3}, true, nil
}

// THE ESTATE MAP'S RENDERINGS ARE PINNED IN ONE FILE BOTH SIDES READ, for the
// object map's reason: a screen whose suite types its own fixtures agrees with
// itself whatever the engine sends. A change to the rendering is a failing test
// until the golden is regenerated (`make estate-answer`) and a failing Vitest
// suite until the Estate screen follows it.
func TestTheEstateAnswerMatchesItsGoldenFile(t *testing.T) {
	t.Parallel()
	got := renderEstateScenarios(t)
	if os.Getenv("CREWLET_REGENERATE_ESTATE_ANSWER") == "1" {
		if err := os.MkdirAll(filepath.Dir(estateGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(estateGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(estateGolden)
	if err != nil {
		t.Fatalf("read %s: %v — `make estate-answer` writes it", estateGolden, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the estate answer no longer matches %s. Run `make estate-answer`, read "+
			"the diff, and follow it in the dashboard's Estate screen — its suite loads this "+
			"file — and in docs/reference/api-endpoints.md's estate examples, which are held "+
			"to it.\n--- rendered now ---\n%s", estateGolden, got)
	}
}

// THE REFERENCE'S EXAMPLES ARE THE RENDERER'S OWN ANSWERS, for the object map's
// reason: an example written by hand teaches a reader a shape the renderer
// cannot produce. The GET /estate example is layout 0's — what every fleet on
// this build answers — and the gesture example is a move.
func TestTheEstateReferenceExamplesAreTheRenderersOwnAnswers(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "reference",
		"api-endpoints.md"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Estate  map[string]map[string]any `json:"estate"`
		Answers map[string]map[string]any `json:"answers"`
	}
	if err := json.Unmarshal(renderEstateScenarios(t), &golden); err != nil {
		t.Fatal(err)
	}
	if got := exampleUnder(t, string(page), "### `GET /estate`"); !reflect.DeepEqual(got, golden.Estate["whole"]) {
		t.Errorf("the GET /estate example is not what the renderer writes at layout 0."+
			"\n--- documented ---\n%s\n--- rendered ---\n%s",
			indent(t, got), indent(t, golden.Estate["whole"]))
	}
	if got := exampleUnder(t, string(page), "### Gestures on the estate map"); !reflect.DeepEqual(got, golden.Answers["move"]) {
		t.Errorf("the estate gesture example is not what the renderer answers for that move."+
			"\n--- documented ---\n%s\n--- rendered ---\n%s",
			indent(t, got), indent(t, golden.Answers["move"]))
	}
}

// NO HINT NAMES A COMMAND-LINE FLAG: the dashboard renders every hint word for
// word, and a sentence telling it to pass `-confirm` is one it cannot act on.
func TestNoEstateHintNamesACommandLineFlag(t *testing.T) {
	t.Parallel()
	var golden struct {
		Estate   map[string]queries.FleetEstate `json:"estate"`
		Answers  map[string]api.EstateAnswer    `json:"answers"`
		Refusals map[string]struct {
			Body api.EstateRefusalBody `json:"body"`
		} `json:"refusals"`
	}
	if err := json.Unmarshal(renderEstateScenarios(t), &golden); err != nil {
		t.Fatal(err)
	}
	said := map[string]string{}
	for name, e := range golden.Estate {
		said["the "+name+" answer"] = e.Detail
	}
	for name, a := range golden.Answers {
		said["answer "+name] = a.Hint
	}
	for name, r := range golden.Refusals {
		said["refusal "+name] = r.Body.Hint
	}
	for name, text := range said {
		for _, flag := range []string{"-confirm", "-for", "-reason", "-from", "-url", "crewlet "} {
			if strings.Contains(text, flag) {
				t.Errorf("%s names %q, which only the command line has: %s", name, flag, text)
			}
		}
	}
}
