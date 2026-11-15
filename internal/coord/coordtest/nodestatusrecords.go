package coordtest

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// RunNodeStatusRecords is the contract suite every [coord.NodeStatusRecords]
// must pass, beside the lease backend whose presence a record is joined to.
//
// A THIRD ENTRY POINT, because its one rule spans both surfaces: a record
// counts only while its node holds its presence at the record's epoch
// ([coord.LiveNodeStatuses]), and that is a question about what a backend's
// presence leases say after a release and a re-claim as much as about what the
// records say. open makes the fresh pair one case runs against: an EMPTY lease
// store, for [Run]'s reason, and an empty record store, both on the same
// fleet as a node reaches them.
func RunNodeStatusRecords(t *testing.T, open func(t *testing.T) (coord.Backend, coord.NodeStatusRecords)) {
	t.Helper()
	for _, c := range nodeStatusRecordCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var records coord.NodeStatusRecords
			h := newHarness(t, func(t *testing.T) coord.Backend {
				b, r := open(t)
				records = r
				return b
			})
			if records == nil {
				t.Fatal("open returned a nil record store")
			}
			c.fn(&recordHarness{harness: h, r: records})
		})
	}
}

// recordHarness is a lease harness and the record store beside it.
type recordHarness struct {
	*harness
	r coord.NodeStatusRecords
}

type recordCase struct {
	name string
	fn   func(h *recordHarness)
}

// put writes a record and fails the case unless the store took it.
func (h *recordHarness) put(r coord.NodeStatusRecord) {
	h.t.Helper()
	if err := h.r.PutNodeStatus(h.ctx, r); err != nil {
		h.t.Fatalf("PutNodeStatus(%s at %d): %v", r.Node, r.Epoch, err)
	}
}

// all is every record the store holds.
func (h *recordHarness) all() []coord.NodeStatusRecord {
	h.t.Helper()
	got, err := h.r.NodeStatusRecords(h.ctx)
	if err != nil {
		h.t.Fatalf("NodeStatusRecords: %v", err)
	}
	return got
}

// live is the records a reader counts now: every record joined to the live
// presence the backend lists.
func (h *recordHarness) live() map[string]coord.NodeStatusRecord {
	h.t.Helper()
	return coord.LiveNodeStatuses(h.all(), h.listLive(coord.ClassNode))
}

// presence claims a node's presence lease as its heartbeat would.
func (h *recordHarness) presence(node, owner string) *coord.Lease {
	h.t.Helper()
	return h.claim(coord.NodeResource(node), coord.AcquireOptions{
		Owner: owner, TTL: LongTTL, Preferred: node, Ungated: true,
		Meta: map[string]any{"roles": []string{"seats"}},
	})
}

var nodeStatusRecordCases = []recordCase{
	{"a_node_status_record_round_trips_through_the_store", func(h *recordHarness) {
		// What a store hands back is never what was written: the KV keeps
		// bytes, and the twin keeps the same bytes, so an int comes back a
		// float64 and an instant a string. Every part has to arrive whole,
		// the node stamped from the key it is filed under, and the listing
		// has to answer in node order.
		at := time.Date(2026, 11, 15, 9, 30, 0, 123456789, time.UTC)
		want := []coord.NodeStatusRecord{{
			Node: "n1", Epoch: 4, At: at,
			MCP: []coord.MCPServerStatus{{Server: "github", Shared: true, Started: 1, Tools: 12}},
			Models: []coord.ModelStatus{{Key: "default", SignIn: "token", Resolves: true,
				SittingOut: &coord.ModelSitOut{Until: at.Add(time.Hour), Since: at, Window: "session", Said: "limit"}}},
			Runners: &coord.RunnerStatus{Direct: coord.RunnerCell{State: coord.RunnerReady, CheckedAt: at}},
			Infra:   coord.NodeInfra{Address: "10.0.0.4", ObjectsBackend: "nats"},
		}, {Node: "n2", Epoch: 1, At: at}}
		h.put(want[1])
		h.put(want[0])
		if got := h.all(); !reflect.DeepEqual(got, want) {
			h.t.Fatalf("read back %+v, want %+v", got, want)
		}
		// A NODE'S RECORD IS ITS OWN TO REPLACE, wholly: a rewrite that
		// carries no rows leaves none of the earlier ones behind.
		h.put(coord.NodeStatusRecord{Node: "n1", Epoch: 4, At: at.Add(time.Minute)})
		if got := h.all(); len(got) != 2 || got[0].MCP != nil || got[0].Models != nil || got[0].Runners != nil {
			h.t.Fatalf("after a rewrite with no rows, read back %+v", got)
		}
	}},

	{"a_node_status_record_is_counted_only_while_its_node_holds_presence_at_its_epoch", func(h *recordHarness) {
		// THE JOIN EVERY READER MAKES. Live at the epoch it was written
		// under: counted. Its node's presence released: not counted, though
		// the record is still in the store until its age passes. Its node
		// back at a NEW epoch: the old incarnation's record is not counted
		// until the live one writes, which it does at once.
		first := h.presence("n1", "n1:a")
		h.put(coord.NodeStatusRecord{Node: "n1", Epoch: first.Epoch,
			Models: []coord.ModelStatus{{Key: "default", SignIn: "token"}}})
		if got := h.live(); len(got) != 1 || got["n1"].Epoch != first.Epoch {
			h.t.Fatalf("a record at its node's live epoch counted as %v, want it", got)
		}

		if !h.release(coord.NodeResource("n1"), "n1:a", first.Epoch) {
			h.t.Fatal("the presence was not released")
		}
		if len(h.all()) != 1 {
			h.t.Fatal("the record left the store with its node's presence: the case reads nothing")
		}
		if got := h.live(); len(got) != 0 {
			h.t.Errorf("a record whose node holds no presence counted as %v", got)
		}

		again := h.presence("n1", "n1:b")
		if again.Epoch == first.Epoch {
			h.t.Fatalf("a presence claimed again kept epoch %d: the re-claim arm reads nothing", first.Epoch)
		}
		if got := h.live(); len(got) != 0 {
			h.t.Errorf("an earlier incarnation's record (epoch %d) counted for the node back at epoch %d: %v",
				first.Epoch, again.Epoch, got)
		}
		h.put(coord.NodeStatusRecord{Node: "n1", Epoch: again.Epoch})
		if got := h.live(); len(got) != 1 || got["n1"].Epoch != again.Epoch || got["n1"].Models != nil {
			h.t.Errorf("the live incarnation's record counted as %v, want its own", got)
		}
	}},

	{"a_deleted_node_status_record_is_gone_and_deleting_none_is_no_error", func(h *recordHarness) {
		// A node that stops cleanly removes its record rather than leave
		// its rows to the age; a stop after a crash finds nothing to remove,
		// and that is not a failure.
		h.put(coord.NodeStatusRecord{Node: "n1", Epoch: 1})
		h.put(coord.NodeStatusRecord{Node: "n2", Epoch: 1})
		if err := h.r.DeleteNodeStatus(h.ctx, "n1"); err != nil {
			h.t.Fatalf("DeleteNodeStatus: %v", err)
		}
		if got := h.all(); len(got) != 1 || got[0].Node != "n2" {
			h.t.Errorf("after removing n1, the store holds %+v", got)
		}
		if err := h.r.DeleteNodeStatus(h.ctx, "n1"); err != nil {
			h.t.Errorf("removing a record that is not there: %v", err)
		}
	}},

	{"a_node_status_record_naming_no_node_or_epoch_is_refused", func(h *recordHarness) {
		// A record no reader could ever count is refused at the door,
		// never filed under an empty key.
		for name, r := range map[string]coord.NodeStatusRecord{
			"no node": {Epoch: 1}, "no epoch": {Node: "n1"},
		} {
			if err := h.r.PutNodeStatus(h.ctx, r); !errors.Is(err, coord.ErrNoNodeStatus) {
				h.t.Errorf("%s: PutNodeStatus = %v, want ErrNoNodeStatus", name, err)
			}
		}
		if err := h.r.DeleteNodeStatus(h.ctx, ""); !errors.Is(err, coord.ErrNoNodeStatus) {
			h.t.Errorf("DeleteNodeStatus(\"\") = %v, want ErrNoNodeStatus", err)
		}
		if got := h.all(); len(got) != 0 {
			h.t.Errorf("refused records were filed: %+v", got)
		}
	}},
}
