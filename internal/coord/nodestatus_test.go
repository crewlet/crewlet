package coord_test

import (
	"encoding/json"
	"testing"
	"time"

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
	want := coord.NodeStatus{
		InFlight: 3, Draining: true, Posture: "shed", StartedAt: started,
		GrantCeilingHash: "2f1a8c0d3b4e5f60",
	}
	got, ok := coord.StatusFromMeta(map[string]any{coord.StatusKey: want.Meta()})
	if !ok {
		t.Fatal("a published status read as absent")
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// THE SAME MAP IS READ BOTH SIDES OF A STORE ROUND TRIP, where an int
// becomes a float64. A reader that knew only the local shape would report
// every peer's in-flight count as zero — which is the one number this
// exists to carry.
func TestAStatusSurvivesTheJSONRoundTripTheLeaseStoreDoes(t *testing.T) {
	t.Parallel()
	want := coord.NodeStatus{InFlight: 7, Posture: "serve"}
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
	if got.InFlight != want.InFlight || got.Posture != want.Posture {
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
