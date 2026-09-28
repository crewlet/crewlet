package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/objstore"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// objectsGolden is the object store's renderings — the fleet view's block in
// each of its states, every gesture answer and every refusal — committed, and
// read by the dashboard's own suite as its fixture: see [api.ObjectsAnswer].
const objectsGolden = "testdata/objects_answer.json"

// objectsSince is the instant every fixture's gestures and absences are
// stamped at, so the golden is stable.
var objectsSince = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// objectsFleet is a map of five members across three zones asking two copies:
// data-a at twice the weight, data-b gone for twelve ticks and back for three,
// data-c up, data-d taken out for a decommission, data-f removed for absence
// and back on probation for twelve ticks — and data-e, removed for absence,
// remembered, and gone for six ticks since. The balance that set its shares
// converged in six rounds.
func objectsFleet() objstore.MapState {
	member := func(node string, weight int, zone string) placement.Member {
		return placement.Member{Node: node, Weight: weight,
			Share: placement.DefaultShare(weight), Domain: zone}
	}
	out := member("data-d", 1, "eu-3")
	out.Out = true
	probation := member("data-f", 1, "eu-2")
	probation.Probation = true
	return objstore.MapState{
		Map: objplacement.Map{
			Generation: uuid.MustParse("5b0c1f7e-9c1d-4f5e-8a3b-2d7c6e1f0a9b"),
			Epoch:      7, Replicas: 2, PGBits: objplacement.MinPGBits,
			FailureDomain: "zone",
			Members: []placement.Member{
				member("data-a", 2, "eu-1"), member("data-b", 1, "eu-2"),
				member("data-c", 1, "eu-3"), out, probation,
			},
		},
		Absence: map[string]objstore.Absence{"data-b": {Ticks: 12, Present: 3,
			Since: objectsSince, Reason: objstore.ReasonAbsent}},
		TakenOut: map[string]objstore.Gesture{"data-d": {By: "founder",
			Reason: "decommission", At: objectsSince}},
		Removed: map[string]objstore.Removal{
			"data-e": {Gone: 6, At: objectsSince, Reason: objstore.ReasonUnhealthy,
				Detail: "probe: read-only filesystem"},
			"data-f": {Present: 12, At: objectsSince.Add(-time.Hour),
				Reason: objstore.ReasonAbsent},
		},
		Balance: objstore.Balance{Epoch: 7, BalanceReport: placement.BalanceReport{
			Rounds: 6, Deviation: 0.015, Converged: true}},
	}
}

// objectsLeases are the live objects leases [objectsFleet]'s members hold:
// data-b holds none, data-c's store is nearly full and its last repair did not
// finish, data-d is emptying, and data-f, on probation, keeps every copy it
// held when it went. data-a's scrub has met two chunks its disk would not
// read.
func objectsLeases() []coord.Lease {
	strays, none, kept := 14, 0, 37
	lease := func(node string, meta objstore.ObjectsMeta) coord.Lease {
		return coord.Lease{Resource: coord.ObjectsResource(node), Owner: node + ":1",
			Meta: meta.Encode()}
	}
	return []coord.Lease{
		lease("data-a", objstore.ObjectsMeta{Weight: 2,
			Labels: map[string]string{"zone": "eu-1"},
			Health: &objstore.ObjectsHealth{State: "ok", UsedPercent: 41.5},
			Repair: &objstore.ObjectsRepair{Epoch: 7, Completed: true, Placed: 900,
				Held: 900, At: objectsSince.Add(4 * time.Minute)},
			Scrub: &objstore.ObjectsScrub{CycleStarted: objectsSince.Add(-48 * time.Hour),
				Progress: 0.25, Verified: 300, Unreadable: 2},
			Strays: &none}),
		lease("data-c", objstore.ObjectsMeta{Weight: 1,
			Health: &objstore.ObjectsHealth{State: "nearfull", UsedPercent: 88.2,
				Detail: "88.2% of the volume is in use"},
			Repair: &objstore.ObjectsRepair{Epoch: 6, Completed: false, Placed: 430,
				Held: 410, Pending: 20, Unreachable: 20, At: objectsSince.Add(time.Minute)}}),
		lease("data-d", objstore.ObjectsMeta{Weight: 1,
			Health: &objstore.ObjectsHealth{State: "ok", UsedPercent: 12},
			Repair: &objstore.ObjectsRepair{Epoch: 7, Completed: true,
				At: objectsSince.Add(4 * time.Minute)},
			Strays: &strays}),
		lease("data-f", objstore.ObjectsMeta{Weight: 1,
			Labels: map[string]string{"zone": "eu-2"},
			Health: &objstore.ObjectsHealth{State: "ok", UsedPercent: 23},
			Repair: &objstore.ObjectsRepair{Epoch: 7, Completed: true,
				At: objectsSince.Add(4 * time.Minute)},
			Strays: &kept}),
	}
}

// fakeObjects applies the engine's own pure gestures to a map it holds, the
// way [engine.ObjectsControl] applies them to the stored one.
type fakeObjects struct {
	mu    sync.Mutex
	state objstore.MapState
	found bool

	// err is answered before anything is read — the store not answering,
	// or a map a newer build wrote.
	err error

	// lose makes every compare-and-set lose, as a map written faster than
	// the gesture can land.
	lose bool

	// asked is every gesture made, as "verb node by reason".
	asked []string
}

func newFakeObjects() *fakeObjects {
	return &fakeObjects{state: objectsFleet(), found: true}
}

func (f *fakeObjects) apply(asked string,
	change func(objstore.MapState) (objstore.MapState, error)) (engine.ObjectsGesture, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, asked)
	if f.err != nil {
		return engine.ObjectsGesture{}, f.err
	}
	if !f.found {
		return engine.ObjectsGesture{}, upkeep.ErrNoMap
	}
	next, err := change(f.state)
	if err != nil {
		return engine.ObjectsGesture{State: f.state}, err
	}
	if f.lose {
		return engine.ObjectsGesture{State: f.state}, nil
	}
	f.state = next
	return engine.ObjectsGesture{Landed: true, State: next}, nil
}

func (f *fakeObjects) Out(_ context.Context, node, by, reason string) (engine.ObjectsGesture, error) {
	return f.apply(strings.Join([]string{"out", node, by, reason}, " "),
		func(s objstore.MapState) (objstore.MapState, error) {
			return upkeep.Out(s, node, by, reason, objectsSince)
		})
}

func (f *fakeObjects) In(_ context.Context, node, by string) (engine.ObjectsGesture, error) {
	return f.apply(strings.Join([]string{"in", node, by}, " "),
		func(s objstore.MapState) (objstore.MapState, error) { return upkeep.In(s, node) })
}

func (f *fakeObjects) Hold(_ context.Context, d time.Duration, by, reason string) (engine.ObjectsGesture, error) {
	return f.apply(strings.Join([]string{"hold", d.String(), by, reason}, " "),
		func(s objstore.MapState) (objstore.MapState, error) {
			return upkeep.HoldFor(s, d, by, reason, clock)
		})
}

func (f *fakeObjects) Release(_ context.Context, by string) (engine.ObjectsGesture, error) {
	return f.apply("release "+by, func(s objstore.MapState) (objstore.MapState, error) {
		return upkeep.Release(s), nil
	})
}

// gestures is every gesture the fake was asked to make.
func (f *fakeObjects) gestures() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

// objectsApp is an authenticated node mounting the gesture routes over f.
func objectsApp(t *testing.T, f *fakeObjects) *api.App {
	t.Helper()
	b := closedPosture()
	opts := api.Options{Bootstrap: &b}
	if f != nil {
		opts.Objects = f
	}
	return newApp(t, opts)
}

// postObjects runs one authenticated gesture.
func postObjects(t *testing.T, a *api.App, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s answered %d with a body that is not JSON: %q", path, rec.Code,
			rec.Body.String())
	}
	return rec.Code, body
}

// A NODE THAT RUNS NO OBJECT STORE MOUNTS NO GESTURE, rather than routes that
// refuse: there is no map for it to change, and a surface that answered 503
// would tell an operator to wait for one.
func TestTheObjectGesturesAreAbsentWithoutAnObjectStore(t *testing.T) {
	t.Parallel()
	a := objectsApp(t, nil)
	for _, path := range []string{"/objects/out/data-a?confirm=data-a",
		"/objects/in/data-a?confirm=data-a", "/objects/hold?for=1h", "/objects/release"} {
		if status, body := postObjects(t, a, path); status != http.StatusNotFound ||
			body["error"] != "no_route" {
			t.Errorf("%s answered %d %v on a node with no object store", path, status, body)
		}
	}
}

// TAKING A MEMBER OUT MUST REPEAT ITS NAME, and nothing is asked of the map
// until it does: the gesture moves the member's whole share across the fleet.
func TestTakingAMemberOutOrInNeedsItsNameRepeated(t *testing.T) {
	t.Parallel()
	f := newFakeObjects()
	a := objectsApp(t, f)
	for _, path := range []string{"/objects/out/data-a", "/objects/out/data-a?confirm=data-b",
		"/objects/in/data-d?confirm="} {
		status, body := postObjects(t, a, path)
		if status != http.StatusBadRequest || body["error"] != "confirm_required" ||
			body["detail"] == "" || body["hint"] == "" {
			t.Errorf("%s answered %d %v, want 400 confirm_required with a detail and a hint",
				path, status, body)
		}
	}
	if asked := f.gestures(); len(asked) != 0 {
		t.Errorf("an unconfirmed gesture still reached the map: %v", asked)
	}
}

// OUT, THEN IN: each lands, moves the epoch, and names who made it — the
// operator on the credential, never a name the caller chose.
func TestAMemberTakenOutAndPutBackLandsUnderTheOperator(t *testing.T) {
	t.Parallel()
	f := newFakeObjects()
	a := objectsApp(t, f)

	status, body := postObjects(t, a, "/objects/out/data-a?confirm=data-a&reason=disk+swap")
	if status != http.StatusOK || body["landed"] != true || body["epoch"] != float64(8) {
		t.Fatalf("out answered %d %v, want a landed gesture at epoch 8", status, body)
	}
	member, _ := body["member"].(map[string]any)
	if member["node"] != "data-a" || member["out"] != true || member["out_by"] != "founder" ||
		member["out_reason"] != "disk swap" {
		t.Errorf("the member taken out reads %v", member)
	}
	if got := f.gestures(); len(got) != 1 || got[0] != "out data-a founder disk swap" {
		t.Errorf("the gesture reached the map as %v", got)
	}

	status, body = postObjects(t, a, "/objects/in/data-a?confirm=data-a")
	member, _ = body["member"].(map[string]any)
	if status != http.StatusOK || body["landed"] != true || body["epoch"] != float64(9) ||
		member["out"] != false || member["out_by"] != nil {
		t.Errorf("in answered %d %v", status, body)
	}
}

// PUTTING BACK A NODE THE MAP REMOVED VOUCHES FOR IT, and the answer says it is
// not a member yet rather than rendering a member it does not have.
func TestPuttingBackARemovedNodeSaysWhenItIsPlacedOn(t *testing.T) {
	t.Parallel()
	f := newFakeObjects()
	status, body := postObjects(t, objectsApp(t, f), "/objects/in/data-e?confirm=data-e")
	if status != http.StatusOK || body["landed"] != true || body["member"] != nil ||
		!strings.Contains(fmt.Sprint(body["hint"]), "next time") {
		t.Errorf("in of a removed node answered %d %v", status, body)
	}
	if _, remembered := f.state.Removed["data-e"]; remembered {
		t.Error("the removed node is still remembered after an operator vouched for it")
	}
}

// A HOLD NAMES HOW LONG, AND IS REFUSED PAST A DAY OR WITHOUT ONE: a hold pins
// every gone member in the map, its groups a copy short, so the length is the
// operator's to state and never this route's to guess.
func TestAHoldStatesItsLengthAndEnds(t *testing.T) {
	t.Parallel()
	f := newFakeObjects()
	a := objectsApp(t, f)
	for _, path := range []string{"/objects/hold", "/objects/hold?for=soon",
		"/objects/hold?for=25h", "/objects/hold?for=-1m"} {
		status, body := postObjects(t, a, path)
		if status != http.StatusBadRequest || body["error"] != "invalid_hold" ||
			body["hint"] == "" {
			t.Errorf("%s answered %d %v, want 400 invalid_hold", path, status, body)
		}
	}
	if f.state.Hold != nil {
		t.Fatalf("a refused hold was placed: %+v", f.state.Hold)
	}
	// A LENGTH THAT IS NOT ONE never reaches the map: only the two that parse
	// were judged there, against the ceiling that is the engine's to hold.
	if asked := f.gestures(); len(asked) != 2 {
		t.Errorf("the map was asked %v — an unparsable length reached it", asked)
	}

	status, body := postObjects(t, a, "/objects/hold?for=2h&reason=kernel+upgrade")
	hold, _ := body["hold"].(map[string]any)
	if status != http.StatusOK || body["landed"] != true || body["epoch"] != float64(7) ||
		hold["by"] != "founder" || hold["reason"] != "kernel upgrade" ||
		hold["until"] != clock.Add(2*time.Hour).Format(time.RFC3339Nano) {
		t.Errorf("hold answered %d %v — a hold places nothing, so the epoch stays", status, body)
	}

	status, body = postObjects(t, a, "/objects/release")
	if status != http.StatusOK || body["landed"] != true || body["hold"] != nil {
		t.Errorf("release answered %d %v", status, body)
	}
}

// EACH REFUSAL SENDS AN OPERATOR SOMEWHERE DIFFERENT, so each has its own code
// and every one says what is wrong and what to do.
func TestEachObjectRefusalIsNamedApart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		fake   func(*fakeObjects)
		path   string
		status int
		code   string
	}{
		{"a name the map does not hold", nil, "/objects/out/data-z?confirm=data-z",
			http.StatusNotFound, "unknown_member"},
		{"a name the map neither holds nor remembers, put back", nil,
			"/objects/in/data-z?confirm=data-z", http.StatusNotFound, "unknown_member"},
		{"a node the map removed and has not seen back, taken out", nil,
			"/objects/out/data-e?confirm=data-e", http.StatusConflict, "removed_member"},
		{"the last present member", func(f *fakeObjects) {
			// data-d is out and data-f on probation, so neither takes
			// copies; with data-b and data-c both counted gone by the
			// LATEST tick — an open run that has seen the member back
			// since is not an absence — data-a is the last member a
			// write could land on.
			f.state.Absence["data-b"] = objstore.Absence{Ticks: 12, Since: objectsSince,
				Reason: objstore.ReasonAbsent}
			f.state.Absence["data-c"] = objstore.Absence{Ticks: 1,
				Reason: objstore.ReasonUnhealthy}
		}, "/objects/out/data-a?confirm=data-a", http.StatusConflict, "objects_refused"},
		{"a fleet with no map", func(f *fakeObjects) { f.found = false },
			"/objects/hold?for=1h", http.StatusServiceUnavailable, "no_object_map"},
		{"a store that did not answer", func(f *fakeObjects) {
			f.err = fmt.Errorf("%w: nats: timeout", engine.ErrObjectsUnavailable)
		}, "/objects/release", http.StatusServiceUnavailable, "objects_unavailable"},
		{"a map a newer build wrote", func(f *fakeObjects) {
			f.err = fmt.Errorf("%w: unknown field", engine.ErrObjectsNewerMap)
		}, "/objects/in/data-d?confirm=data-d", http.StatusConflict, "objects_newer_map"},
		{"an unclassified failure", func(f *fakeObjects) {
			f.err = errors.New("encode: impossible")
		}, "/objects/release", http.StatusInternalServerError, "objects_failed"},
	} {
		f := newFakeObjects()
		if tc.fake != nil {
			tc.fake(f)
		}
		status, body := postObjects(t, objectsApp(t, f), tc.path)
		if status != tc.status || body["error"] != tc.code || body["detail"] == "" ||
			body["hint"] == "" {
			t.Errorf("%s: %s answered %d %v, want %d %s with a detail and a hint",
				tc.name, tc.path, status, body, tc.status, tc.code)
		}
	}
}

// THE LAST PRESENT MEMBER IS JUDGED BY THE LATEST TICK, not by whether a run of
// absence is open: a member back from a missed tick keeps its run open for a
// whole grace while it proves itself stable — placed on, serving every chunk —
// and reading the open run alone as "gone" refused taking out a second member
// beside it for all of that time. The pair is what pins it: the same fleet
// with data-b back is answered as landed, and with data-b gone as of the
// latest tick is refused.
func TestTheLastPresentMemberIsJudgedByTheLatestTick(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		present int
		status  int
	}{
		{"data-b back for three ticks is somewhere to write", 3, http.StatusOK},
		{"data-b gone as of the latest tick is not", 0, http.StatusConflict},
	} {
		f := newFakeObjects()
		f.state.Absence["data-b"] = objstore.Absence{Ticks: 12, Present: tc.present,
			Since: objectsSince, Reason: objstore.ReasonAbsent}
		f.state.Absence["data-c"] = objstore.Absence{Ticks: 1, Reason: objstore.ReasonUnhealthy}
		status, body := postObjects(t, objectsApp(t, f), "/objects/out/data-a?confirm=data-a")
		if status != tc.status {
			t.Errorf("%s: out data-a answered %d %v, want %d", tc.name, status, body, tc.status)
		}
	}
}

// A GESTURE THE MAP ALREADY SAYS IS ANSWERED AS LANDED AND CHANGES NOTHING —
// the record of who took a member out, why and when included — so an operator
// re-sending one whose answer was lost overwrites nobody's reason. A HOLD IS
// THE EXCEPTION, and says so: sent again it replaces the hold in force, ending
// its length after the resend.
func TestAGestureTheMapAlreadySaysChangesNothingButAHoldRestarts(t *testing.T) {
	t.Parallel()
	f := newFakeObjects()
	a := objectsApp(t, f)
	before := f.state.Clone()

	status, body := postObjects(t, a, "/objects/out/data-d?confirm=data-d&reason=again")
	member, _ := body["member"].(map[string]any)
	if status != http.StatusOK || body["landed"] != true || body["epoch"] != float64(7) ||
		member["out_by"] != "founder" || member["out_reason"] != "decommission" {
		t.Errorf("out of a member already out answered %d %v — the first gesture's "+
			"record is the one that stands", status, body)
	}
	if status, body := postObjects(t, a, "/objects/in/data-a?confirm=data-a"); status != http.StatusOK ||
		body["landed"] != true || body["epoch"] != float64(7) {
		t.Errorf("in of a member placed on answered %d %v", status, body)
	}
	if status, body := postObjects(t, a, "/objects/release"); status != http.StatusOK ||
		body["landed"] != true {
		t.Errorf("release with no hold answered %d %v", status, body)
	}
	if !reflect.DeepEqual(f.state, before) {
		t.Errorf("a gesture the map already said changed it:\n%+v\nwant\n%+v", f.state, before)
	}

	postObjects(t, a, "/objects/hold?for=2h")
	_, body = postObjects(t, a, "/objects/hold?for=30m&reason=nearly+done")
	hold, _ := body["hold"].(map[string]any)
	if hold["until"] != clock.Add(30*time.Minute).Format(time.RFC3339Nano) ||
		hold["reason"] != "nearly done" {
		t.Errorf("a hold sent again reads %v — the newest statement of the length stands", hold)
	}
}

// A GESTURE THAT LOST EVERY RACE IS AN ANSWER, not a refusal and not a fault:
// the map as it now stands, that nothing was written, and what to do.
func TestAGestureThatLostEveryRaceSaysSo(t *testing.T) {
	t.Parallel()
	f := newFakeObjects()
	f.lose = true
	status, body := postObjects(t, objectsApp(t, f), "/objects/out/data-a?confirm=data-a")
	member, _ := body["member"].(map[string]any)
	if status != http.StatusOK || body["landed"] != false || body["epoch"] != float64(7) ||
		member["out"] != false || body["hint"] == nil {
		t.Errorf("a lost gesture answered %d %v", status, body)
	}
}

// renderObjectsScenarios is the golden's content: the fleet block in each of
// its states, and every gesture answer and refusal, each rendered by the
// route's own renderer.
func renderObjectsScenarios(t *testing.T) []byte {
	t.Helper()
	fleet := objectsFleet()
	held := objectsFleet()
	held.Hold = &objstore.Hold{Until: objectsSince.Add(2 * time.Hour), By: "founder",
		Reason: "kernel upgrade", At: objectsSince}
	// A BALANCE THAT RAN OUT OF ROUNDS short of its tolerance: the map it
	// wrote is the best it measured, and this is the one place that says
	// the weights are not being kept.
	unbalanced := objectsFleet()
	unbalanced.Balance = objstore.Balance{Epoch: 7, BalanceReport: placement.BalanceReport{
		Rounds: placement.DefaultMaxRounds, Deviation: 0.061}}
	render := func(s objstore.MapState) queries.FleetObjects {
		return queries.RenderObjects(s, s.Map.Layout(), objectsLeases(), objectsSince)
	}
	blocks := map[string]queries.FleetObjects{
		"placed":      render(fleet),
		"held":        render(held),
		"unbalanced":  render(unbalanced),
		"unavailable": {State: queries.ObjectMapUnavailable},
		"no_map":      {State: queries.ObjectMapNone},
		"unreadable":  {State: queries.ObjectMapUnreadable},
	}

	gesture := func(change func(objstore.MapState) (objstore.MapState, error)) engine.ObjectsGesture {
		next, err := change(objectsFleet())
		if err != nil {
			t.Fatal(err)
		}
		return engine.ObjectsGesture{Landed: true, State: next}
	}
	answers := map[string]api.ObjectsAnswer{
		"out": api.RenderObjectsGesture(gesture(func(s objstore.MapState) (objstore.MapState, error) {
			return upkeep.Out(s, "data-a", "founder", "disk swap", objectsSince)
		}), "data-a", objectsSince),
		"in": api.RenderObjectsGesture(gesture(func(s objstore.MapState) (objstore.MapState, error) {
			return upkeep.In(s, "data-d")
		}), "data-d", objectsSince),
		"in_removed": api.RenderObjectsGesture(gesture(func(s objstore.MapState) (objstore.MapState, error) {
			return upkeep.In(s, "data-e")
		}), "data-e", objectsSince),
		"in_probation": api.RenderObjectsGesture(gesture(func(s objstore.MapState) (objstore.MapState, error) {
			return upkeep.In(s, "data-f")
		}), "data-f", objectsSince),
		"hold": api.RenderObjectsGesture(gesture(func(s objstore.MapState) (objstore.MapState, error) {
			return upkeep.HoldFor(s, 2*time.Hour, "founder", "kernel upgrade", objectsSince)
		}), "", objectsSince),
		"release": api.RenderObjectsGesture(engine.ObjectsGesture{Landed: true,
			State: upkeep.Release(held)}, "", objectsSince),
		"not_landed": api.RenderObjectsGesture(engine.ObjectsGesture{State: fleet},
			"data-a", objectsSince),
	}

	refusals := map[string]api.ObjectsRefusal{}
	for name, err := range map[string]error{
		"unknown_member":      fmt.Errorf("%w: %q", upkeep.ErrUnknownMember, "data-z"),
		"removed_member":      fmt.Errorf("%w: %q", upkeep.ErrRemovedMember, "data-e"),
		"objects_refused":     fmt.Errorf("%w: %s", upkeep.ErrNothingPlaceable, "data-a"),
		"invalid_hold":        fmt.Errorf("%w: asked for %s", upkeep.ErrHoldRange, 25*time.Hour),
		"no_object_map":       upkeep.ErrNoMap,
		"objects_unavailable": fmt.Errorf("%w: nats: timeout", engine.ErrObjectsUnavailable),
		"objects_newer_map":   fmt.Errorf("%w: unknown field", engine.ErrObjectsNewerMap),
	} {
		refusal, ok := api.RenderObjectsRefusal(err)
		if !ok || refusal.Body.Error != name {
			t.Fatalf("%s renders as %+v", name, refusal)
		}
		refusals[name] = refusal
	}
	type goldenRefusal struct {
		Status int                    `json:"status"`
		Body   api.ObjectsRefusalBody `json:"body"`
	}
	bodies := map[string]goldenRefusal{}
	for name, r := range refusals {
		bodies[name] = goldenRefusal{Status: r.Status, Body: r.Body}
	}
	out, err := json.MarshalIndent(map[string]any{
		"fleet": blocks, "answers": answers, "refusals": bodies,
	}, "", "  ")
	if err != nil {
		t.Fatalf("encode the objects answers: %v", err)
	}
	return append(out, '\n')
}

// THE OBJECT STORE'S RENDERINGS ARE PINNED IN ONE FILE BOTH SIDES READ, for the
// reason the gate answer's are: a screen whose suite types its own fixtures
// agrees with itself whatever the engine sends. A change to the rendering is a
// failing test until the golden is regenerated (`make objects-answer`) and a
// failing Vitest suite until the fleet screen follows it.
func TestTheObjectsAnswerMatchesItsGoldenFile(t *testing.T) {
	t.Parallel()
	got := renderObjectsScenarios(t)
	if os.Getenv("CREWLET_REGENERATE_OBJECTS_ANSWER") == "1" {
		if err := os.MkdirAll(filepath.Dir(objectsGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(objectsGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(objectsGolden)
	if err != nil {
		t.Fatalf("read %s: %v — `make objects-answer` writes it", objectsGolden, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the objects answer no longer matches %s. Run `make objects-answer`, "+
			"read the diff, and follow it in the dashboard's fleet screen — its suite "+
			"loads this file — and in docs/reference/api-endpoints.md's GET /fleet "+
			"example, which is held to it.\n--- rendered now ---\n%s", objectsGolden, got)
	}
}

// THE REFERENCE'S EXAMPLES ARE THE RENDERER'S OWN ANSWERS. The GET /fleet
// example was once written by hand, and taught a reader that taking a member out
// changes neither `copies` nor `distinct_domains` and that shares need not sum to
// a hundred — four numbers the renderer cannot produce for the map it showed. A
// script or an alert built from it misreads the first real answer. So its
// `objects` block, and the gesture answer beside it, are held to what the
// renderers write for the golden's own map: a change to either fails here until
// the page is updated with it.
func TestTheReferenceExamplesAreTheRenderersOwnAnswers(t *testing.T) {
	t.Parallel()
	page, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "reference",
		"api-endpoints.md"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Fleet   map[string]any `json:"fleet"`
		Answers map[string]any `json:"answers"`
	}
	if err := json.Unmarshal(renderObjectsScenarios(t), &golden); err != nil {
		t.Fatal(err)
	}
	fleet := exampleUnder(t, string(page), "### `GET /fleet`")
	if got := fleet["objects"]; !reflect.DeepEqual(got, golden.Fleet["held"]) {
		t.Errorf("the GET /fleet example's objects block is not what the renderer writes "+
			"for that map.\n--- documented ---\n%s\n--- rendered ---\n%s",
			indent(t, got), indent(t, golden.Fleet["held"]))
	}
	answer := exampleUnder(t, string(page), "### Gestures on the placement map")
	if !reflect.DeepEqual(any(answer), golden.Answers["out"]) {
		t.Errorf("the gesture example is not what the renderer answers for that out."+
			"\n--- documented ---\n%s\n--- rendered ---\n%s",
			indent(t, answer), indent(t, golden.Answers["out"]))
	}
}

// exampleUnder is the first JSON example in the section a heading opens, decoded.
func exampleUnder(t *testing.T, page, heading string) map[string]any {
	t.Helper()
	at := strings.Index(page, "\n"+heading+"\n")
	if at < 0 {
		t.Fatalf("the reference has no section %q", heading)
	}
	section := page[at+len(heading)+2:]
	if next := strings.Index(section, "\n### "); next >= 0 {
		section = section[:next]
	}
	open := strings.Index(section, "```json\n")
	if open < 0 {
		t.Fatalf("the section %q has no JSON example", heading)
	}
	body := section[open+len("```json\n"):]
	body = body[:strings.Index(body, "\n```")]
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the example under %q is not JSON: %v", heading, err)
	}
	return out
}

func indent(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// NO HINT NAMES A COMMAND-LINE FLAG: the dashboard renders every hint word for
// word, and a sentence telling it to pass `-confirm` is one it cannot act on.
func TestNoObjectsHintNamesACommandLineFlag(t *testing.T) {
	t.Parallel()
	var golden struct {
		Answers  map[string]api.ObjectsAnswer `json:"answers"`
		Refusals map[string]struct {
			Body api.ObjectsRefusalBody `json:"body"`
		} `json:"refusals"`
	}
	if err := json.Unmarshal(renderObjectsScenarios(t), &golden); err != nil {
		t.Fatal(err)
	}
	hints := map[string]string{}
	for name, a := range golden.Answers {
		hints["answer "+name] = a.Hint
	}
	for name, r := range golden.Refusals {
		hints["refusal "+name] = r.Body.Hint
	}
	for name, hint := range hints {
		for _, flag := range []string{"-confirm", "-for", "-reason", "-url", "crewlet "} {
			if strings.Contains(hint, flag) {
				t.Errorf("%s's hint names %q, which only the command line has: %s",
					name, flag, hint)
			}
		}
	}
}

// EVERY HOLD LENGTH THE DASHBOARD OFFERS IS ONE THE ENGINE ACCEPTS, AND THE
// LONGEST IS ITS CEILING.
//
// A length past upkeep.MaxHold is a choice the route answers `invalid_hold`
// on every press; a list that stopped short of it hides the day-long hold a
// long maintenance needs. The copy exists because the dashboard is its own
// build — see internal/clientsource — so this side holds it.
func TestTheDashboardOffersHoldLengthsTheEngineAccepts(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Declaration(clientsource.Tree(t),
		`export const HOLD_LENGTHS = \[([^\]]*)\] as const;`)
	if err != nil {
		t.Fatal(err)
	}
	lengths := clientsource.Strings(body)
	if len(lengths) == 0 {
		t.Fatal("the dashboard declares no hold lengths at all, so this gate certifies nothing")
	}
	longest := time.Duration(0)
	for _, l := range lengths {
		d, err := time.ParseDuration(l)
		if err != nil || d <= 0 || d > upkeep.MaxHold {
			t.Errorf("the dashboard offers a hold of %q, which the engine refuses: it "+
				"takes more than nothing and at most %s", l, upkeep.MaxHold)
		}
		longest = max(longest, d)
	}
	if longest != upkeep.MaxHold {
		t.Errorf("the dashboard's longest hold is %s and the engine's ceiling %s",
			longest, upkeep.MaxHold)
	}
}
