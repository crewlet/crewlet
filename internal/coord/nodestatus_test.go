package coord_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// ABSENT IS NOT ZERO. A node that publishes no status (its status hook did not
// answer within its share of that beat, so its heartbeat carried the placement
// half alone) is not a node with no work in flight, and a confident 0 would
// draw an idle row for a process that is simply not saying.
func TestANodeThatPublishesNoStatusIsNotReadAsIdle(t *testing.T) {
	t.Parallel()
	for name, meta := range map[string]map[string]any{
		"no meta at all":             nil,
		"only placement":             {"roles": []string{"seats"}, "labels": map[string]any{}},
		"a status that is not a map": {coord.StatusKey: "busy"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, ok := coord.StatusFromMeta(meta); ok {
				t.Error("a node that said nothing was read as having reported")
			}
		})
	}
}

func TestAPublishedStatusRoundTrips(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	want := coord.NodeStatus{
		InFlight: 3, Draining: true, Posture: "shed", StartedAt: started,
		AdminPosture: iam.AdminServe, Build: "v1.2.3", ParkedStateVersion: 4, CaptureStampVersion: 1,
		AppliersReady: 2, AppliersTotal: 5,
	}
	got, ok := coord.StatusFromMeta(map[string]any{coord.StatusKey: want.Meta()})
	if !ok {
		t.Fatal("a published status read as absent")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A KEY THIS BUILD DOES NOT KNOW IS IGNORED. A successor sharing the fleet
// publishes what it adds beside what this build reads, in whatever shape it
// chooses, and a status that failed to decode over it would read as absent —
// "not saying" — for as long as the successor ran.
func TestAStatusKeyFromANewerBuildDecodesHarmlessly(t *testing.T) {
	t.Parallel()
	newer := map[string]any{
		"in_flight": 2, "draining": false, "posture": "serve",
		"features":           []any{"a_feature_from_a_newer_build"},
		"from_a_newer_build": map[string]any{"nested": []any{1.5, "x"}},
	}
	got, ok := coord.StatusFromMeta(map[string]any{coord.StatusKey: newer})
	if !ok {
		t.Fatal("a newer peer's status read as absent")
	}
	if want := (coord.NodeStatus{InFlight: 2, Posture: "serve"}); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A STATUS CARRIES THE ALARMS FIRING ON ITS NODE, and "nothing is firing" is a
// different claim from "this node cannot say". Each mark arrives as published
// (kind, since, longest-standing-first order) across the lease store's JSON
// round trip; an evaluated table with nothing firing publishes `[]`, never a
// nil slice's `null`; no evaluated table publishes no key. A mark naming no
// kind is dropped; an unparsable since keeps the mark.
//
// Mutation: publish `alarms` without an evaluation, publish a nil list for an
// evaluated table, read an absent key as evaluated, or drop the since, and
// this goes red.
func TestAStatusCarriesItsFiringAlarms(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	firing := coord.NodeStatus{InFlight: 1, AlarmsEvaluated: true, Alarms: []coord.AlarmMark{
		{Kind: "backup_age", Since: since},
		{Kind: "apply_lag", Since: since.Add(2 * time.Minute)},
	}}
	quiet := coord.NodeStatus{InFlight: 1, AlarmsEvaluated: true}
	silent := coord.NodeStatus{InFlight: 1}

	for name, want := range map[string]coord.NodeStatus{
		"firing": firing, "evaluated with nothing firing": quiet, "no table evaluated": silent,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(map[string]any{coord.StatusKey: want.Meta()})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			wire := string(raw)
			switch {
			case !want.AlarmsEvaluated && strings.Contains(wire, `"alarms"`):
				t.Errorf("a node with no evaluated table published alarms: %s", wire)
			case want.AlarmsEvaluated && len(want.Alarms) == 0 && !strings.Contains(wire, `"alarms":[]`):
				t.Errorf("an evaluated table with nothing firing is not an empty list on the wire: %s", wire)
			}
			var round map[string]any
			if err := json.Unmarshal(raw, &round); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for side, meta := range map[string]map[string]any{
				"as written":         {coord.StatusKey: want.Meta()},
				"after a round trip": round,
			} {
				got, ok := coord.StatusFromMeta(meta)
				if !ok {
					t.Fatalf("%s: a published status read as absent", side)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: read back %+v, want %+v", side, got, want)
				}
			}
		})
	}

	got, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: map[string]any{
		"alarms": []any{
			map[string]any{"since": since.Format(time.RFC3339)},
			"not a mark",
			map[string]any{"kind": "wal_large", "since": "yesterday"},
		},
	}})
	if want := []coord.AlarmMark{{Kind: "wal_large"}}; !got.AlarmsEvaluated ||
		!reflect.DeepEqual(got.Alarms, want) {
		t.Errorf("decoded (%v, %+v), want (true, %+v): a mark naming no kind is dropped, "+
			"and one whose since does not parse is kept without one", got.AlarmsEvaluated, got.Alarms, want)
	}
	if got, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: map[string]any{
		"alarms": "backup_age",
	}}); got.AlarmsEvaluated {
		t.Errorf("an alarms key that is not a list read as an evaluated table: %+v", got.Alarms)
	}
}

// THE SAME MAP IS READ BOTH SIDES OF A STORE ROUND TRIP, where an int
// becomes a float64. A reader that knew only the local shape would report
// every peer's in-flight count as zero — which is the one number this
// exists to carry.
func TestAStatusSurvivesTheJSONRoundTripTheLeaseStoreDoes(t *testing.T) {
	t.Parallel()
	want := coord.NodeStatus{
		InFlight: 7, Posture: "serve", AppliersReady: 1, AppliersTotal: 3, ParkedStateVersion: 2,
		CaptureStampVersion: 3,
	}
	raw, err := json.Marshal(map[string]any{coord.StatusKey: want.Meta()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := coord.StatusFromMeta(meta)
	if !ok {
		t.Fatal("a round-tripped status read as absent")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A GITLET NODE'S BUDGET IS READ ONLY WHERE ITS ROW SAYS ONE: zero is a claim
// of nothing to give, and a row with no figure, or one no build writes, is a
// node NOT SAYING, never a zero or a figure. It sits beside the status, so a
// beat whose status was dropped still says it and a key of that name inside a
// status says nothing; and it arrives exact across the lease store's JSON
// round trip, at a size no int32 or float32 would carry.
//
// Mutations: read an absent key as zero — the quiet rows are said, red; read
// the budget out of the status — the dropped status's row says nothing and the
// status's key is read, red.
func TestAGitBudgetIsReadOnlyWhereARowSaysOne(t *testing.T) {
	t.Parallel()
	const budget = int64(68)<<30 + 12_345
	for name, tc := range map[string]struct {
		meta   map[string]any
		budget int64
		says   bool
	}{
		"a figure, as written":   {map[string]any{coord.GitBackgroundKey: budget}, budget, true},
		"zero, which is a claim": {map[string]any{coord.GitBackgroundKey: int64(0)}, 0, true},
		"a figure beside a dropped status": {
			map[string]any{"roles": []string{"gitlet"}, coord.GitBackgroundKey: budget}, budget, true},
		"no figure at all": {map[string]any{"roles": []string{"gitlet"}}, 0, false},
		"no meta at all":   {nil, 0, false},
		"a figure inside the status, not beside": {
			map[string]any{coord.StatusKey: map[string]any{coord.GitBackgroundKey: budget}}, 0, false},
		"a negative figure":             {map[string]any{coord.GitBackgroundKey: int64(-1)}, 0, false},
		"a figure that is not a number": {map[string]any{coord.GitBackgroundKey: "64GiB"}, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(tc.meta)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var stored map[string]any
			if err := json.Unmarshal(raw, &stored); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for side, meta := range map[string]map[string]any{"as written": tc.meta, "as stored": stored} {
				got, says := coord.GitBackgroundFromMeta(meta)
				if got != tc.budget || says != tc.says {
					t.Errorf("%s: read (%d, says %v), want (%d, says %v)", side, got, says, tc.budget, tc.says)
				}
			}
		})
	}
}

// AN UNSET POSTURE IS OMITTED rather than published as "": a node before a
// control plane exists has no posture, and an empty string in the payload
// would make the reading side unable to tell that from one it did not
// understand.
func TestAnUnsetPostureIsNotPublished(t *testing.T) {
	t.Parallel()
	meta := coord.NodeStatus{InFlight: 1}.Meta()
	if _, present := meta["posture"]; present {
		t.Errorf("an unset posture was published: %+v", meta)
	}
	if _, present := meta["started_at"]; present {
		t.Errorf("an unset start time was published: %+v", meta)
	}
	if _, present := meta["mcp"]; present {
		t.Errorf("an empty mcp was published: %+v", meta)
	}
	// The two that are always meaningful stay, including their zeros: a
	// node reporting zero turns in flight IS saying something.
	if _, present := meta["in_flight"]; !present {
		t.Error("in_flight was omitted")
	}
	if _, present := meta["draining"]; !present {
		t.Error("draining was omitted, so a node that is not draining says nothing")
	}
}

// THE ADMIN POSTURE IS ABSENT RATHER THAN EMPTY on a node that declares none:
// a worker binds no API and has no posture, and an empty string would read as
// a peer that disagrees with every other node, on the signal an operator is
// meant to trust. Each posture survives the lease store's JSON round trip,
// under the key `/query/fleet` reads. Mutation: publish the field whatever its
// value and the first assertion goes red.
func TestANodeWithNoAdminPosturePublishesNone(t *testing.T) {
	t.Parallel()
	meta := coord.NodeStatus{InFlight: 1}.Meta()
	if _, present := meta["admin"]; present {
		t.Errorf("a node with no posture published one: %+v", meta)
	}
	for _, posture := range iam.AdminPostures {
		raw, err := json.Marshal(map[string]any{
			coord.StatusKey: coord.NodeStatus{AdminPosture: posture}.Meta(),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"admin":"`+string(posture)+`"`) {
			t.Errorf("the wire carries %s, want the posture under \"admin\"", raw)
		}
		var round map[string]any
		if err := json.Unmarshal(raw, &round); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got, ok := coord.StatusFromMeta(round)
		if !ok {
			t.Fatal("a published status read as absent")
		}
		if got.AdminPosture != posture {
			t.Errorf("admin posture = %q, want %q carried across the round trip",
				got.AdminPosture, posture)
		}
	}
}
