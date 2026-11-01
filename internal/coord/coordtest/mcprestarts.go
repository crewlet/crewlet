package coordtest

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// ---- MCP restarts -------------------------------------------------------- //

func (h *fleetHarness) restart(server string, seat uuid.UUID, at time.Time, by string) coord.MCPRestart {
	h.t.Helper()
	r, err := h.f.PutMCPRestart(h.ctx, coord.MCPRestart{
		Server: server, Seat: seat, At: at, By: by, ByKind: iam.ActorHuman, OperatorID: sessionOf(by),
	})
	if err != nil {
		h.t.Fatalf("PutMCPRestart(%s, %s): %v", server, seat, err)
	}
	return r
}

var restartSeat = uuid.MustParse("00000000-0000-7000-8000-00000000000a")

var mcpRestartCases = []fleetCase{{
	// THE LATER STAMP WINS, whichever write lands last: every node restarts
	// what began before the stamp, so a later request restarts a superset of
	// what an earlier one does, and an earlier one landing second must not
	// take that back. A server's record and one seat's are two records.
	name: "an MCP restart keeps the later stamp, per server and per seat",
	fn: func(h *fleetHarness) {
		later := h.restart("tracker", uuid.Nil, h.now().Add(time.Minute), "jane-doe")
		if later.Version == 0 || later.Server != "tracker" || later.Seat != uuid.Nil ||
			later.ByKind != iam.ActorHuman || later.OperatorID != sessionOf("jane-doe") ||
			!later.At.Equal(h.now().Add(time.Minute)) {
			h.t.Fatalf("stored %+v, want jane's restart of every instance, with a version", later)
		}
		if kept := h.restart("tracker", uuid.Nil, h.now(), "omar-ali"); kept.By != "jane-doe" ||
			kept.Version != later.Version {
			h.t.Fatalf("an earlier request landing second returned %+v, want jane's later one kept", kept)
		}
		seat := h.restart("tracker", restartSeat, h.now(), "omar-ali")
		if seat.Seat != restartSeat || seat.By != "omar-ali" || seat.Version == later.Version {
			h.t.Fatalf("one seat's restart = %+v, want a record of its own", seat)
		}
		entry := coord.MCPRestartEntry("tracker", restartSeat)
		read, found, err := h.f.MCPRestart(h.ctx, entry)
		if err != nil || !found || read.Version != seat.Version || read.Seat != restartSeat {
			h.t.Fatalf("MCPRestart(%s) = %+v (found %v, %v), want omar's", entry, read, found, err)
		}
		if read, found, _ := h.f.MCPRestart(h.ctx, "tracker"); !found || read.By != "jane-doe" {
			h.t.Fatalf("the server's own record = %+v (found %v), want jane's", read, found)
		}
		// Removal is at the version read, so a remove that read an older
		// record cannot take a newer one away.
		if gone, err := h.f.DeleteMCPRestart(h.ctx, entry, later.Version); err != nil || gone {
			h.t.Fatalf("a remove at another record's version = (%v, %v), want a lost race", gone, err)
		}
		if gone, err := h.f.DeleteMCPRestart(h.ctx, entry, 0); err != nil || gone {
			h.t.Fatalf("a remove with no version = (%v, %v), want a lost race", gone, err)
		}
		if gone, err := h.f.DeleteMCPRestart(h.ctx, entry, seat.Version); err != nil || !gone {
			h.t.Fatalf("a remove at the current version = (%v, %v)", gone, err)
		}
		if _, found, _ := h.f.MCPRestart(h.ctx, entry); found {
			h.t.Fatal("a removed restart still reads")
		}
		h.restart("tracker", restartSeat, h.now(), "jane-doe")
	},
}, {
	name: "an MCP restart names its server, instant and author",
	fn: func(h *fleetHarness) {
		for _, bad := range []coord.MCPRestart{
			{At: h.now(), By: "jane-doe", ByKind: iam.ActorHuman},
			{Server: "a@b", At: h.now(), By: "jane-doe", ByKind: iam.ActorHuman},
			{Server: "tracker", By: "jane-doe", ByKind: iam.ActorHuman},
			{Server: "tracker", At: h.now(), ByKind: iam.ActorHuman},
			{Server: "tracker", At: h.now(), By: "jane-doe"},
		} {
			if _, err := h.f.PutMCPRestart(h.ctx, bad); err == nil {
				h.t.Errorf("PutMCPRestart(%+v) was accepted", bad)
			}
		}
		if all, _ := h.f.ListMCPRestarts(h.ctx); len(all) != 0 {
			h.t.Errorf("a refused restart left %+v behind", all)
		}
	},
}, {
	// Every node builds its copy from this: the records that exist, the
	// marker, then each change, a removal included.
	name: "an MCP restart watch sees a restart and its removal, and lists by entry",
	fn: func(h *fleetHarness) {
		h.restart("wiki", uuid.Nil, h.now(), "jane-doe")
		h.restart("chat", restartSeat, h.now(), "jane-doe")
		all, err := h.f.ListMCPRestarts(h.ctx)
		if err != nil || len(all) != 2 || all[0].Server != "chat" || all[0].Seat != restartSeat ||
			all[1].Server != "wiki" {
			h.t.Fatalf("ListMCPRestarts = %+v, %v; want chat's seat then wiki", all, err)
		}

		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		updates, err := h.f.WatchMCPRestarts(ctx)
		if err != nil {
			h.t.Fatalf("WatchMCPRestarts: %v", err)
		}
		for _, want := range []string{coord.MCPRestartEntry("chat", restartSeat), "wiki"} {
			if u := next(h, updates, "restart"); u.Entry != want || u.Restart == nil || u.Current {
				h.t.Fatalf("update = %+v, want %s's existing restart", u, want)
			}
		}
		if u := next(h, updates, "restart"); !u.Current {
			h.t.Fatalf("update = %+v, want the marker", u)
		}
		stored := h.restart("tracker", uuid.Nil, h.now(), "omar-ali")
		u := next(h, updates, "restart")
		if u.Entry != "tracker" || u.Restart == nil || u.Restart.Version != stored.Version ||
			u.Restart.Server != "tracker" || u.Restart.Seat != uuid.Nil {
			h.t.Fatalf("update after a restart = %+v, want tracker's as stored", u)
		}
		if gone, err := h.f.DeleteMCPRestart(h.ctx, "tracker", stored.Version); err != nil || !gone {
			h.t.Fatalf("DeleteMCPRestart = (%v, %v)", gone, err)
		}
		if u = next(h, updates, "restart"); u.Entry != "tracker" || u.Restart != nil {
			h.t.Fatalf("update after a removal = %+v, want tracker's removed", u)
		}
	},
}}
