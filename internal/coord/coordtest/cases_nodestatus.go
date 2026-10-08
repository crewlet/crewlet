package coordtest

import (
	"reflect"

	"github.com/crewlet/crewlet/internal/coord"
)

// nodeStatusCases certify [coord.NodeStatus] across a real store round trip.
//
// In the suite rather than beside the decoder because its whole input is what
// a BACKEND hands back from a presence lease's Meta — and the two shipped
// backends hand it back in different Go types (the twin what was written, the
// KV a JSON round trip's []any and float64). A decoder tested only against the
// twin would pass and then read every peer on the real store as reporting
// nothing.
var nodeStatusCases = []testCase{
	{"a_node_status_round_trips_through_presence", func(h *harness) {
		want := coord.NodeStatus{
			InFlight: 2, Posture: "serve",
			MCP: []coord.MCPServerStatus{
				{Server: "github", Shared: true, Started: 1, Tools: 12},
				{Server: "jira", Started: 1, Failed: 1, Tools: 9, Error: "401", ErrorSeat: "pm"},
			},
		}
		h.present("n1", "n1:a", want.Meta())
		for what, lease := range map[string]coord.Lease{
			"Get":      *h.mustHold(coord.NodeResource("n1"), "n1:a"),
			"ListLive": h.listLive(coord.ClassNode)[0],
		} {
			got, ok := coord.StatusFromMeta(lease.Meta)
			if !ok {
				h.t.Fatalf("%s: a published status read back as absent", what)
			}
			if !reflect.DeepEqual(got, want) {
				h.t.Fatalf("%s: status read back as %+v, want %+v", what, got, want)
			}
		}
	}},

	{"a_status_key_this_build_does_not_know_is_ignored", func(h *harness) {
		// A successor that shares the fleet publishes what it adds beside
		// what this build reads, in whatever shape it chooses. The status
		// must still decode — a node whose status read as absent would be
		// drawn as "not saying" for as long as the successor ran.
		status := coord.NodeStatus{InFlight: 1, Posture: "serve"}.Meta()
		status["from_a_newer_build"] = []any{"a", map[string]any{"b": 1.5}}
		h.present("n1", "n1:a", status)
		got, ok := coord.StatusFromMeta(h.listLive(coord.ClassNode)[0].Meta)
		if !ok {
			h.t.Fatal("a status carrying a key this build does not know read as absent")
		}
		if want := (coord.NodeStatus{InFlight: 1, Posture: "serve"}); !reflect.DeepEqual(got, want) {
			h.t.Fatalf("status read back as %+v, want %+v", got, want)
		}
	}},
}

// present claims a node's presence lease carrying status as its heartbeat
// would.
func (h *harness) present(nodeID, owner string, status map[string]any) {
	h.t.Helper()
	meta := map[string]any{"roles": []string{"seats"}, coord.StatusKey: status}
	h.claim(coord.NodeResource(nodeID), coord.AcquireOptions{
		Owner: owner, TTL: LongTTL, Preferred: nodeID, Ungated: true, Meta: meta,
	})
}
