package coord_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// ABSENT IS NOT ZERO. A node that publishes no status (a peer running a build
// older than the field) is not a node with no work in flight, and a confident 0
// would draw an idle row for a process that is simply not saying.
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
	pm := uuid.MustParse("6f1c2a3b-4d5e-5f60-8172-839405a6b7c8")
	want := coord.NodeStatus{
		InFlight: 3, Draining: true, Posture: "shed", StartedAt: started,
		GrantCeilingHash: "2f1a8c0d3b4e5f60",
		Features:         []coord.Feature{coord.FeatureMCPStatus, "a_feature_from_a_newer_build"},
		MCP: []coord.MCPServerStatus{
			{Server: "github", Shared: true, Started: 1, Tools: 12},
			{Server: "jira", Started: 2, Failed: 1, Tools: 9,
				Error: "exec: jira-mcp: not found", ErrorSeat: pm},
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

// AN OLDER PEER'S STATUS HONOURS NOTHING. A build that predates the feature
// list publishes a status without one, and it must read as a node that can
// carry none of the gated gestures — not as a node that did not report,
// which a gate would answer "try again" for as long as it ran.
func TestAnOlderPeersStatusDecodesWithNoFeatures(t *testing.T) {
	t.Parallel()
	older := map[string]any{"in_flight": 2, "draining": false, "posture": "serve"}
	got, ok := coord.StatusFromMeta(map[string]any{coord.StatusKey: older})
	if !ok {
		t.Fatal("an older peer's status read as absent")
	}
	if len(got.Features) != 0 || len(got.MCP) != 0 {
		t.Errorf("an older status decoded features %v and mcp %v, want neither", got.Features, got.MCP)
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
		Features: []coord.Feature{coord.FeatureMCPStatus},
		MCP:      []coord.MCPServerStatus{{Server: "github", Shared: true, Started: 1, Tools: 4}},
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
	for _, key := range []string{"features", "mcp"} {
		if _, present := meta[key]; present {
			t.Errorf("an empty %s was published: %+v", key, meta)
		}
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

// THE GRANT CEILING IS ABSENT RATHER THAN EMPTY on a node that declares none.
//
// It is the same rule as the posture above and it matters more here, because
// what an empty cell would mean is the opposite of the truth: a worker node
// binds no API and therefore has no ceiling to state, while an empty string on
// the wire reads as a peer whose ceiling differs from every other node's — a
// disagreement drawn across a fleet that has none, on exactly the signal an
// operator is meant to trust.
func TestANodeWithNoGrantCeilingPublishesNone(t *testing.T) {
	t.Parallel()
	meta := coord.NodeStatus{InFlight: 1}.Meta()
	if _, present := meta["grant_ceiling"]; present {
		t.Errorf("a node with no ceiling published one: %+v", meta)
	}
	// The counterfactual: a node that HAS one publishes it, and it
	// survives the JSON round trip the lease store does.
	raw, err := json.Marshal(map[string]any{
		coord.StatusKey: coord.NodeStatus{GrantCeilingHash: "abc123"}.Meta(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := coord.StatusFromMeta(round)
	if !ok {
		t.Fatal("a published status read as absent")
	}
	if got.GrantCeilingHash != "abc123" {
		t.Errorf("grant ceiling = %q, want it carried across the round trip",
			got.GrantCeilingHash)
	}
}

// AN MCP FAILURE NAMES ITS SEAT BY ID, and a value that is not one names no
// seat. The row is read by every peer against its own chart, so a handle on
// the wire would point a reader at whoever answers to it after a rename; and a
// value no build of this one wrote — a handle, a mis-cased id — is decoded as
// "did not say" rather than as a seat a reader would then go looking for.
func TestAnMCPFailureNamesItsSeatByIDOrNotAtAll(t *testing.T) {
	t.Parallel()
	seat := uuid.MustParse("0b6f2c1e-9a4d-5c3b-8e7f-1a2b3c4d5e6f")
	status := coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "jira", Failed: 1, Error: "401", ErrorSeat: seat},
	}}
	meta := status.Meta()
	rows, _ := meta["mcp"].([]map[string]any)
	if len(rows) != 1 || rows[0]["error_seat"] != seat.String() {
		t.Fatalf("published %+v, want the seat's id on the row", meta["mcp"])
	}
	for name, wire := range map[string]any{
		"a handle":          "pm",
		"an upper-cased id": strings.ToUpper(seat.String()),
		"not a string":      42,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: map[string]any{
				"mcp": []any{map[string]any{"server": "jira", "failed": 1, "error_seat": wire}},
			}})
			if len(got.MCP) != 1 || got.MCP[0].ErrorSeat != uuid.Nil {
				t.Errorf("decoded %+v from %v, want the row with no seat named", got.MCP, wire)
			}
		})
	}
}
