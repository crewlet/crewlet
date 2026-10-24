package coordtest

import (
	"reflect"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
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
			InFlight: 2, Posture: "serve", AdminPosture: iam.AdminRefuse,
			MCP: []coord.MCPServerStatus{
				{Server: "github", Shared: true, Started: 1, Tools: 12},
				// The seat a failed instance was launched for travels by its
				// ID, as a string a JSON round trip hands back as one.
				{Server: "jira", Started: 1, Failed: 1, Tools: 9, Error: "401",
					ErrorSeat: uuid.MustParse("6f1c2a3b-4d5e-5f60-8172-839405a6b7c8"),
					Unlisted:  []string{"jira_add_coment", "linear_get_issue"}},
				// A seat's child held back for its provisioned credential
				// travels as a count beside the others, with its sentence
				// and its seat as a failure's do.
				{Server: "mattermost", Started: 1, Waiting: 2, Tools: 4,
					Wait:     "not started: waiting for the mattermost integration to mint ${MM_CTO}",
					WaitSeat: uuid.MustParse("0a1b2c3d-4e5f-5a6b-8c7d-8e9fa0b1c2d3")},
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
