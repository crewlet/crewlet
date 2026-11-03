package coordtest

import (
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// nodeStatusCases certify [coord.NodeStatus] across a real store round trip.
//
// In the suite rather than beside the decoder because its whole input is what
// a BACKEND hands back from a presence lease's Meta, and that is never what
// was written: the KV stores it as JSON, and the twin puts it through the same
// encoding at its door (memory's wireCopy), so an int comes back a float64, a
// typed slice an []any and a nil slice null. A decoder tested only against the
// Go values it was handed would pass and then read every peer on a real store
// as reporting nothing.
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

	{"a_status_carries_its_firing_alarms", func(h *harness) {
		// What is firing on each node is read by every peer off this
		// lease, so the marks have to come back from the store exactly as
		// published — kind, start and the longest-standing-first order —
		// and the three states have to stay three across it: an evaluated
		// table with nothing firing is an EMPTY list (which the store's
		// encoding hands back as an []any of nothing, where a nil list
		// would come back null, and a reader must still see as a list),
		// and a node with no table it has evaluated is no list at all,
		// never a quiet node.
		since := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
		want := map[string]coord.NodeStatus{
			"n1": {InFlight: 1, AlarmsEvaluated: true, Alarms: []coord.AlarmMark{
				{Kind: "backup_age", Since: since},
				{Kind: "apply_lag", Since: since.Add(2 * time.Minute)},
			}},
			"n2": {InFlight: 1, AlarmsEvaluated: true},
			"n3": {InFlight: 1},
		}
		for _, node := range []string{"n1", "n2", "n3"} {
			h.present(node, node+":a", want[node].Meta())
		}
		read := map[string]coord.Lease{}
		for _, node := range []string{"n1", "n2", "n3"} {
			read["Get "+node] = *h.mustHold(coord.NodeResource(node), node+":a")
		}
		for _, lease := range h.listLive(coord.ClassNode) {
			node, _ := coord.ClassNode.Name(lease.Resource)
			read["ListLive "+node] = lease
		}
		if len(read) != 2*len(want) {
			h.t.Fatalf("read %d leases, want each of the %d nodes by Get and by ListLive",
				len(read), len(want))
		}
		for what, lease := range read {
			node, _ := coord.ClassNode.Name(lease.Resource)
			got, ok := coord.StatusFromMeta(lease.Meta)
			if !ok {
				h.t.Fatalf("%s: a published status read back as absent", what)
			}
			if !reflect.DeepEqual(got, want[node]) {
				h.t.Errorf("%s: status read back as %+v, want %+v", what, got, want[node])
			}
		}
	}},

	{"a_presence_row_carries_the_git_budget_beside_its_status", func(h *harness) {
		// What a git node can give its background work is read by every
		// other git node off this lease, to place each repository's
		// maintenance on the first node of its order that holds the work —
		// so it has to come back from the store exactly as published: a
		// number the store hands back as a float64, at a size no int32 and
		// no rounding to a float32 would carry (the large git-node class's
		// background budget, about 68 GiB, and some odd bytes beside it).
		// It rides BESIDE the status, so a beat that carried no status still
		// says it (n1); a zero is written and comes back a claim, never an
		// absence (n2); and a row that writes no figure comes back a node
		// not saying (n3), whatever its status says.
		const budget = int64(68)<<30 + 12_345
		roles := []string{"data", "git", "ingress"}
		metas := map[string]map[string]any{
			"n1": {"roles": roles, coord.GitBackgroundKey: budget},
			"n2": {"roles": roles, coord.GitBackgroundKey: int64(0),
				coord.StatusKey: coord.NodeStatus{InFlight: 1}.Meta()},
			"n3": {"roles": roles, coord.StatusKey: coord.NodeStatus{InFlight: 1}.Meta()},
		}
		want := map[string]struct {
			budget int64
			says   bool
		}{"n1": {budget, true}, "n2": {0, true}, "n3": {0, false}}
		for _, node := range []string{"n1", "n2", "n3"} {
			h.claim(coord.NodeResource(node), coord.AcquireOptions{
				Owner: node + ":a", TTL: LongTTL, Preferred: node, Ungated: true, Meta: metas[node],
			})
		}
		read := map[string]coord.Lease{}
		for _, node := range []string{"n1", "n2", "n3"} {
			read["Get "+node] = *h.mustHold(coord.NodeResource(node), node+":a")
		}
		for _, lease := range h.listLive(coord.ClassNode) {
			node, _ := coord.ClassNode.Name(lease.Resource)
			read["ListLive "+node] = lease
		}
		if len(read) != 2*len(want) {
			h.t.Fatalf("read %d leases, want each of the %d nodes by Get and by ListLive",
				len(read), len(want))
		}
		for what, lease := range read {
			node, _ := coord.ClassNode.Name(lease.Resource)
			got, says := coord.GitBackgroundFromMeta(lease.Meta)
			if got != want[node].budget || says != want[node].says {
				h.t.Errorf("%s: the git budget read back as (%d, says %v), want (%d, says %v)", what, got,
					says, want[node].budget, want[node].says)
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
