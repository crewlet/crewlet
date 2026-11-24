package coordtest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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

// PutNodeStatus writes every row of r as a node's writer would, its own row
// last, and removes each row of r's node the store lists at r's epoch that r
// no longer carries: a whole record replaced, for a case that sets one up.
func PutNodeStatus(ctx context.Context, store coord.NodeStatusRecords, r coord.NodeStatusRecord) error {
	rows, err := r.Rows()
	if err != nil {
		return err
	}
	kept := map[coord.NodeStatusRowKey]bool{}
	for _, row := range rows {
		if putErr := store.PutNodeStatusRow(ctx, row); putErr != nil {
			return putErr
		}
		kept[row.Key] = true
	}
	listed, err := store.NodeStatusRecords(ctx, everyRow)
	if err != nil {
		return err
	}
	for _, was := range listed {
		if was.Node != r.Node {
			continue
		}
		var gone []coord.NodeStatusRowKey
		for _, m := range was.MCP {
			gone = append(gone, coord.NodeStatusRowKey{Class: coord.NodeStatusMCP, Node: r.Node, Name: m.Server})
		}
		for _, m := range was.Models {
			gone = append(gone, coord.NodeStatusRowKey{Class: coord.NodeStatusModel, Node: r.Node, Name: m.Key})
		}
		if was.Growth != nil {
			gone = append(gone, coord.NodeStatusRowKey{Class: coord.NodeStatusGrowth, Node: r.Node})
		}
		for _, key := range gone {
			if kept[key] {
				continue
			}
			if err := store.RemoveNodeStatusRow(ctx, key, r.Epoch); err != nil {
				return err
			}
		}
	}
	return nil
}

// everyRow reads every class of the node status bucket.
var everyRow = coord.NodeStatusRead{MCP: true, Models: true, Growth: true}

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
	if err := PutNodeStatus(h.ctx, h.r, r); err != nil {
		h.t.Fatalf("PutNodeStatus(%s at %d): %v", r.Node, r.Epoch, err)
	}
}

// putRow writes one row and fails the case unless the store took it.
func (h *recordHarness) putRow(row coord.NodeStatusRow) {
	h.t.Helper()
	if err := h.r.PutNodeStatusRow(h.ctx, row); err != nil {
		h.t.Fatalf("PutNodeStatusRow(%s): %v", row.Key.Key(), err)
	}
}

// read is every record the store holds, read for the classes read names.
func (h *recordHarness) read(read coord.NodeStatusRead) []coord.NodeStatusRecord {
	h.t.Helper()
	got, err := h.r.NodeStatusRecords(h.ctx, read)
	if err != nil {
		h.t.Fatalf("NodeStatusRecords: %v", err)
	}
	return got
}

// all is every record the store holds, every class read.
func (h *recordHarness) all() []coord.NodeStatusRecord {
	h.t.Helper()
	return h.read(everyRow)
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

// paddedRow is a row value at epoch of exactly size bytes.
func paddedRow(epoch int64, size int) []byte {
	head := fmt.Sprintf(`{"epoch":%d,"pad":"`, epoch)
	return []byte(head + strings.Repeat("x", size-len(head)-len(`"}`)) + `"}`)
}

// rowsOf is r's rows, failing the case where it has none to give.
func (h *recordHarness) rowsOf(r coord.NodeStatusRecord) map[coord.NodeStatusClass]coord.NodeStatusRow {
	h.t.Helper()
	rows, err := r.Rows()
	if err != nil {
		h.t.Fatalf("the rows of %s: %v", r.Node, err)
	}
	out := map[coord.NodeStatusClass]coord.NodeStatusRow{}
	for _, row := range rows {
		out[row.Key.Class] = row
	}
	return out
}

// everyPart is a record of node at epoch with one row of every class.
func everyPart(node string, epoch int64, at time.Time) coord.NodeStatusRecord {
	return coord.NodeStatusRecord{
		Node: node, Epoch: epoch, At: at,
		MCP: []coord.MCPServerStatus{{Server: "github", Shared: true, Started: 1, Tools: 12}},
		Models: []coord.ModelStatus{{Key: "default", SignIn: "token", Resolves: true,
			SittingOut: &coord.ModelSitOut{Until: at.Add(time.Hour), Since: at, Window: "session", Said: "limit"}}},
		Runners: &coord.RunnerStatus{Direct: coord.RunnerCell{State: coord.RunnerReady, CheckedAt: at}},
		Infra:   coord.NodeInfra{Address: "10.0.0.4", ObjectsBackend: "nats"},
		Growth: &coord.GrowthSignals{ClockSkews: []coord.GrowthClockSkew{{Product: "apple", Vendor: "Apple",
			OffsetSeconds: 300, Since: at}}},
	}
}

var nodeStatusRecordCases = []recordCase{
	{"a_node_status_record_round_trips_through_the_store", func(h *recordHarness) {
		// What a store hands back is never what was written: the KV keeps
		// bytes, and the twin keeps the same bytes, so an int comes back a
		// float64 and an instant a string. Every part has to arrive whole,
		// the node and each row's name stamped from the key it is filed
		// under, and the listing has to answer in node order.
		at := time.Date(2026, 11, 15, 9, 30, 0, 123456789, time.UTC)
		want := []coord.NodeStatusRecord{everyPart("n1", 4, at), {Node: "n2", Epoch: 1, At: at}}
		h.put(want[1])
		h.put(want[0])
		if got := h.all(); !reflect.DeepEqual(got, want) {
			h.t.Fatalf("read back %+v, want %+v", got, want)
		}
		// A READER OF ONE PART READS THAT PART ALONE: the node's own row
		// always, every other class only where asked for.
		for read, check := range map[coord.NodeStatusRead]func(coord.NodeStatusRecord) bool{
			{}: func(r coord.NodeStatusRecord) bool {
				return r.MCP == nil && r.Models == nil && r.Growth == nil && r.Runners != nil
			},
			{MCP: true}: func(r coord.NodeStatusRecord) bool {
				return len(r.MCP) == 1 && r.Models == nil && r.Growth == nil
			},
			{Models: true}: func(r coord.NodeStatusRecord) bool {
				return r.MCP == nil && len(r.Models) == 1 && r.Growth == nil
			},
			{Growth: true}: func(r coord.NodeStatusRecord) bool {
				return r.MCP == nil && r.Models == nil && r.Growth != nil
			},
		} {
			if got := h.read(read); len(got) != 2 || !check(got[0]) {
				h.t.Errorf("a listing reading %+v answered %+v", read, got)
			}
		}
		// A NODE'S RECORD IS ITS OWN TO REPLACE, wholly: a rewrite that
		// carries no rows, with the rows it dropped removed, leaves none of
		// the earlier ones behind.
		h.put(coord.NodeStatusRecord{Node: "n1", Epoch: 4, At: at.Add(time.Minute)})
		if got := h.all(); len(got) != 2 || got[0].MCP != nil || got[0].Models != nil || got[0].Runners != nil ||
			got[0].Growth != nil {
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
		// The live incarnation's own row alone: the earlier one's model row,
		// still in the store, is at another epoch and never read beside it.
		h.putRow(h.rowsOf(coord.NodeStatusRecord{Node: "n1", Epoch: again.Epoch})[coord.NodeStatusOwn])
		if got := h.live(); len(got) != 1 || got["n1"].Epoch != again.Epoch || got["n1"].Models != nil {
			h.t.Errorf("the live incarnation's record counted as %v, want its own", got)
		}
	}},

	{"a_row_is_read_only_at_its_nodes_own_epoch_and_never_without_it", func(h *recordHarness) {
		// AN EPOCH MISMATCH AND AN ORPHAN. A row an earlier incarnation left
		// under a name the live one still writes no row for, or one written
		// after the own row moved on, is at another epoch than the node's
		// own row and is never read with it; rows whose node has no own row
		// are no record at all.
		at := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
		stale := h.rowsOf(everyPart("n1", 3, at))
		for _, class := range []coord.NodeStatusClass{coord.NodeStatusMCP, coord.NodeStatusModel,
			coord.NodeStatusGrowth} {
			h.putRow(stale[class])
		}
		if got := h.all(); len(got) != 0 {
			h.t.Fatalf("rows with no own row of their node read as %+v, want no record", got)
		}
		h.putRow(h.rowsOf(coord.NodeStatusRecord{Node: "n1", Epoch: 4, At: at})[coord.NodeStatusOwn])
		got := h.all()
		if len(got) != 1 || got[0].Epoch != 4 || got[0].MCP != nil || got[0].Models != nil || got[0].Growth != nil {
			h.t.Fatalf("epoch-3 rows beside an epoch-4 own row read as %+v, want the own row alone", got)
		}
		h.putRow(h.rowsOf(everyPart("n1", 4, at))[coord.NodeStatusMCP])
		if got := h.all(); len(got) != 1 || len(got[0].MCP) != 1 || got[0].Models != nil {
			h.t.Errorf("an MCP row at the own row's epoch beside stale ones read as %+v, want it alone", got)
		}
	}},

	{"a_node_with_a_row_at_a_later_epoch_than_its_own_is_not_counted", func(h *recordHarness) {
		// ITS WRITER HAS BEGUN ITS NEXT EPOCH, its other rows first: the
		// own row still names the earlier one, and a reader that joined the
		// rows left at it would count the node with part of its rows.
		at := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
		h.put(everyPart("n1", 4, at))
		h.putRow(h.rowsOf(everyPart("n1", 5, at))[coord.NodeStatusMCP])
		if got := h.all(); len(got) != 0 {
			h.t.Errorf("a node with an epoch-5 row beside its epoch-4 own row read as %+v, want it not counted",
				got)
		}
		h.putRow(h.rowsOf(everyPart("n1", 5, at))[coord.NodeStatusOwn])
		if got := h.all(); len(got) != 1 || got[0].Epoch != 5 || len(got[0].MCP) != 1 || got[0].Models != nil {
			h.t.Errorf("once its own row moved to epoch 5 the node read as %+v, want its epoch-5 rows", got)
		}
	}},

	{"an_earlier_incarnations_write_never_lands_over_a_later_ones_row", func(h *recordHarness) {
		// TWO PROCESSES UNDER ONE NODE ID: the earlier one's pass still in
		// flight writes every row at epoch 3 after the later one wrote them
		// at 4. The later rows stand, and so they do through the earlier
		// one's stop, which removes up to its own epoch.
		at := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
		later := everyPart("n1", 4, at)
		h.put(later)
		earlier := everyPart("n1", 3, at.Add(time.Minute))
		earlier.MCP[0].Failed, earlier.Models[0].SignIn = 1, "env"
		rows, err := earlier.Rows()
		if err != nil {
			h.t.Fatal(err)
		}
		for _, row := range rows {
			h.putRow(row)
		}
		if got := h.all(); len(got) != 1 || !reflect.DeepEqual(got[0], later) {
			h.t.Fatalf("after an earlier incarnation's late writes the store reads %+v, want the epoch-4 record "+
				"whole", got)
		}
		if err := h.r.DeleteNodeStatus(h.ctx, "n1", 3); err != nil {
			h.t.Fatalf("DeleteNodeStatus through the earlier epoch: %v", err)
		}
		if got := h.all(); len(got) != 1 || !reflect.DeepEqual(got[0], later) {
			h.t.Errorf("after the earlier incarnation's stop the store reads %+v, want the epoch-4 record whole",
				got)
		}
	}},

	{"a_removed_row_is_gone_and_one_past_the_removers_epoch_stays", func(h *recordHarness) {
		// A SERVER NO LONGER CONFIGURED: its row is removed, the rest of the
		// record stands. A row a later incarnation wrote under the same key
		// is left, and removing what is not there is no error.
		at := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
		h.put(everyPart("n1", 4, at))
		key := coord.NodeStatusRowKey{Class: coord.NodeStatusMCP, Node: "n1", Name: "github"}
		if err := h.r.RemoveNodeStatusRow(h.ctx, key, 4); err != nil {
			h.t.Fatalf("RemoveNodeStatusRow: %v", err)
		}
		if got := h.all(); len(got) != 1 || got[0].MCP != nil || len(got[0].Models) != 1 || got[0].Growth == nil {
			h.t.Fatalf("after removing the MCP row, read back %+v", got)
		}
		if err := h.r.RemoveNodeStatusRow(h.ctx, key, 4); err != nil {
			h.t.Errorf("removing a row that is not there: %v", err)
		}
		h.put(everyPart("n1", 6, at))
		if err := h.r.RemoveNodeStatusRow(h.ctx, key, 5); err != nil {
			h.t.Fatalf("RemoveNodeStatusRow through an earlier epoch: %v", err)
		}
		if got := h.all(); len(got) != 1 || len(got[0].MCP) != 1 {
			h.t.Errorf("an earlier incarnation's removal left %+v, want the epoch-6 MCP row", got)
		}
	}},

	{"a_deleted_node_status_record_is_gone_and_deleting_none_is_no_error", func(h *recordHarness) {
		// A node that stops cleanly removes every row rather than leave them
		// to the age; a stop after a crash finds nothing to remove, and
		// that is not a failure. Another node's rows stand.
		at := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
		h.put(everyPart("n1", 1, at))
		h.put(everyPart("n2", 1, at))
		if err := h.r.DeleteNodeStatus(h.ctx, "n1", 1); err != nil {
			h.t.Fatalf("DeleteNodeStatus: %v", err)
		}
		if got := h.all(); len(got) != 1 || got[0].Node != "n2" || len(got[0].MCP) != 1 {
			h.t.Errorf("after removing n1, the store holds %+v", got)
		}
		// NO ROW OF n1 IS LEFT, not merely its own: an own row written again
		// at the same epoch finds nothing beside it.
		h.putRow(h.rowsOf(coord.NodeStatusRecord{Node: "n1", Epoch: 1, At: at})[coord.NodeStatusOwn])
		if got := h.all(); len(got) != 2 || got[0].MCP != nil || got[0].Models != nil || got[0].Growth != nil {
			h.t.Errorf("a removal left rows of n1 behind: %+v", got)
		}
		if err := h.r.DeleteNodeStatus(h.ctx, "n3", 1); err != nil {
			h.t.Errorf("removing a record that is not there: %v", err)
		}
	}},

	{"a_node_status_record_is_removed_only_up_to_the_epoch_its_remover_wrote", func(h *recordHarness) {
		// A STOPPING INCARNATION NEVER REMOVES A LATER ONE'S RECORD: the
		// same node id started again before the old process finished
		// stopping writes at a later epoch, and the old one's removal leaves
		// every row of it standing. Through, not at: a row at an epoch below
		// the one named is removed, as a write the remover saw fail may have
		// landed.
		at := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
		h.put(everyPart("n1", 5, at))
		if err := h.r.DeleteNodeStatus(h.ctx, "n1", 4); err != nil {
			h.t.Fatalf("DeleteNodeStatus through an earlier epoch: %v", err)
		}
		if got := h.all(); len(got) != 1 || !reflect.DeepEqual(got[0], everyPart("n1", 5, at)) {
			h.t.Fatalf("an earlier incarnation's removal left %+v, want the epoch-5 record whole", got)
		}
		if err := h.r.DeleteNodeStatus(h.ctx, "n1", 7); err != nil {
			h.t.Fatalf("DeleteNodeStatus through a later epoch: %v", err)
		}
		h.putRow(h.rowsOf(coord.NodeStatusRecord{Node: "n1", Epoch: 5, At: at})[coord.NodeStatusOwn])
		if got := h.all(); len(got) != 1 || got[0].MCP != nil || got[0].Models != nil || got[0].Growth != nil {
			h.t.Errorf("a removal through epoch 7 left %+v, want every epoch-5 row gone", got)
		}
	}},

	{"a_node_status_row_naming_no_node_epoch_or_name_is_refused", func(h *recordHarness) {
		// A row no reader could ever count is refused at the door, never
		// filed under an empty key: no node, no epoch, a named class with
		// no name, a node's own row with one, or a class nobody reads.
		value := []byte(`{"epoch":1}`)
		for name, row := range map[string]coord.NodeStatusRow{
			"no node":     {Key: coord.NodeStatusRowKey{Class: coord.NodeStatusOwn}, Value: value},
			"no epoch":    {Key: coord.NodeStatusRowKey{Class: coord.NodeStatusOwn, Node: "n1"}, Value: []byte(`{}`)},
			"epoch zero":  {Key: coord.NodeStatusRowKey{Class: coord.NodeStatusOwn, Node: "n1"}, Value: []byte(`{"epoch":0}`)},
			"no name":     {Key: coord.NodeStatusRowKey{Class: coord.NodeStatusMCP, Node: "n1"}, Value: value},
			"a named own": {Key: coord.NodeStatusRowKey{Class: coord.NodeStatusOwn, Node: "n1", Name: "x"}, Value: value},
			"no class":    {Key: coord.NodeStatusRowKey{Class: "seat_status", Node: "n1"}, Value: value},
		} {
			if err := h.r.PutNodeStatusRow(h.ctx, row); !errors.Is(err, coord.ErrNoNodeStatus) {
				h.t.Errorf("%s: PutNodeStatusRow = %v, want ErrNoNodeStatus", name, err)
			}
		}
		if err := h.r.DeleteNodeStatus(h.ctx, "", 1); !errors.Is(err, coord.ErrNoNodeStatus) {
			h.t.Errorf("DeleteNodeStatus(\"\") = %v, want ErrNoNodeStatus", err)
		}
		if err := h.r.RemoveNodeStatusRow(h.ctx, coord.NodeStatusRowKey{Class: coord.NodeStatusModel, Node: "n1"},
			1); !errors.Is(err, coord.ErrNoNodeStatus) {
			h.t.Errorf("RemoveNodeStatusRow naming no entry = %v, want ErrNoNodeStatus", err)
		}
		if got := h.all(); len(got) != 0 {
			h.t.Errorf("refused rows were filed: %+v", got)
		}
	}},

	{"a_status_rows_numbers_read_back_exactly_or_as_unsaid", func(h *recordHarness) {
		// THROUGH THE BYTES A STORE KEEPS: an epoch and counts past 2^53,
		// which a float64 would round, read back as written; a count that is
		// no integer, or past int64's range, reads as unsaid (zero), never
		// truncated or wrapped into one nobody wrote.
		const epoch = int64(1<<60 + 1)
		big := 1<<53 + 1
		used := 0.1234567890123
		want := coord.NodeStatusRecord{Node: "n1", Epoch: epoch, MCP: []coord.MCPServerStatus{{Server: "jira",
			Started: big, Restarts: big + 2, RefusedCount: big + 4}},
			Models: []coord.ModelStatus{{Key: "default", SignIn: "token", ParkedCount: big,
				Parked: []coord.ParkedSeat{{Seat: uuid.MustParse("6f1c2a3b-4d5e-5f60-8172-839405a6b7c8")}},
				Limits: []coord.ModelLimit{{Window: "session", State: "ok", Hours: big, Used: &used}}}},
			Growth: &coord.GrowthSignals{ClockSkews: []coord.GrowthClockSkew{{Product: "apple",
				OffsetSeconds: -big}}}}
		h.put(want)
		got := h.all()
		if len(got) != 1 || got[0].Epoch != epoch || got[0].MCP[0].Started != big ||
			got[0].MCP[0].Restarts != big+2 || got[0].MCP[0].RefusedCount != big+4 ||
			got[0].Models[0].ParkedCount != big || got[0].Models[0].Limits[0].Hours != big ||
			*got[0].Models[0].Limits[0].Used != used || got[0].Growth.ClockSkews[0].OffsetSeconds != -big {
			h.t.Fatalf("numbers past 2^53 read back as %+v, want them exact", got)
		}
		h.putRow(coord.NodeStatusRow{Key: coord.NodeStatusRowKey{Class: coord.NodeStatusMCP, Node: "n1",
			Name: "jira"}, Value: []byte(fmt.Sprintf(`{"epoch":%d,"started":1.5,"failed":9223372036854775808,`+
			`"tools":1e3,"restarts":-0.5,"waiting":2}`, epoch))})
		row := h.all()[0].MCP[0]
		if row.Started != 0 || row.Failed != 0 || row.Tools != 0 || row.Restarts != 0 || row.Waiting != 2 {
			h.t.Errorf("a row of a fraction, an overflow and an exponent read back as %+v, want each "+
				"unsaid and its integer whole", row)
		}
	}},

	{"every_node_status_row_past_its_bound_is_refused_and_at_it_is_written", func(h *recordHarness) {
		// EVERY CLASS AT coord.MaxNodeStatusRowBytes: one byte past is
		// refused as too large with nothing written, never as an outage, and
		// at the bound it is written and read back. A twin left unbounded
		// would certify a row the KV refuses.
		bound := coord.MaxNodeStatusRowBytes
		for _, key := range []coord.NodeStatusRowKey{
			{Class: coord.NodeStatusOwn, Node: "n1"},
			{Class: coord.NodeStatusMCP, Node: "n1", Name: "github"},
			{Class: coord.NodeStatusModel, Node: "n1", Name: "default"},
			{Class: coord.NodeStatusGrowth, Node: "n1"},
		} {
			err := h.r.PutNodeStatusRow(h.ctx, coord.NodeStatusRow{Key: key, Value: paddedRow(1, bound+1)})
			switch {
			case !errors.Is(err, coord.ErrValueTooLarge):
				h.t.Errorf("%s one byte past its bound = %v, want coord.ErrValueTooLarge", key.Key(), err)
			case errors.Is(err, coord.ErrUnavailable), errors.Is(err, coord.ErrWriteUnknown):
				h.t.Errorf("%s past its bound = %v: a refusal read as an outage", key.Key(), err)
			}
		}
		if got := h.all(); len(got) != 0 {
			h.t.Fatalf("refused rows were filed: %+v", got)
		}
		for _, key := range []coord.NodeStatusRowKey{
			{Class: coord.NodeStatusMCP, Node: "n1", Name: "github"},
			{Class: coord.NodeStatusModel, Node: "n1", Name: "default"},
			{Class: coord.NodeStatusGrowth, Node: "n1"},
			{Class: coord.NodeStatusOwn, Node: "n1"},
		} {
			h.putRow(coord.NodeStatusRow{Key: key, Value: paddedRow(1, bound)})
		}
		got := h.all()
		if len(got) != 1 || len(got[0].MCP) != 1 || len(got[0].Models) != 1 || got[0].Growth != nil {
			h.t.Errorf("rows at their bound read back as %+v, want the node with its MCP and model rows", got)
		}
	}},
}
