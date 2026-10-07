package coord_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ABSENT IS NOT ZERO. A node that publishes no status (its status hook overran
// its budget on that beat, so its heartbeat carried the placement half alone)
// is not a node with no work in flight, and a confident 0 would draw an idle
// row for a process that is simply not saying.
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
		MCP: []coord.MCPServerStatus{
			{Server: "github", Shared: true, Started: 1, Tools: 12},
			{Server: "jira", Started: 2, Failed: 1, Tools: 9,
				Error: "exec: jira-mcp: not found", ErrorSeat: "pm"},
		},
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

// THE HEARTBEAT IS RE-SENT EVERY BEAT, so a server's failure text is bounded
// on the wire: a child's stderr can be any length, and every byte of it would
// ride every renew of the node's presence.
func TestAnMCPFailureIsClippedOnTheWire(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("e", 4*coord.MaxMCPErrorBytes)
	status := coord.NodeStatus{MCP: []coord.MCPServerStatus{{Server: "jira", Failed: 1, Error: long}}}
	got, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: status.Meta()})
	if len(got.MCP) != 1 {
		t.Fatalf("decoded %d mcp rows, want 1", len(got.MCP))
	}
	if n := len(got.MCP[0].Error); n > coord.MaxMCPErrorBytes {
		t.Errorf("the error travelled as %d bytes, over the %d-byte bound", n, coord.MaxMCPErrorBytes)
	}
	if !strings.HasSuffix(got.MCP[0].Error, "…") {
		t.Errorf("a clipped error carries no marker: %q", got.MCP[0].Error)
	}
}

// THE SAME MAP IS READ BOTH SIDES OF A STORE ROUND TRIP, where an int
// becomes a float64. A reader that knew only the local shape would report
// every peer's in-flight count as zero — which is the one number this
// exists to carry.
func TestAStatusSurvivesTheJSONRoundTripTheLeaseStoreDoes(t *testing.T) {
	t.Parallel()
	want := coord.NodeStatus{
		InFlight: 7, Posture: "serve",
		MCP: []coord.MCPServerStatus{{Server: "github", Shared: true, Started: 1, Tools: 4}},
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
