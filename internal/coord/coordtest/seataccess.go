package coordtest

import (
	"maps"
	"slices"
)

// ---- the seat access ledger -------------------------------------------- //

func (h *fleetHarness) putSeatAccess(kind, handle, value string) {
	h.t.Helper()
	if err := h.f.PutSeatAccessRecord(h.ctx, kind, handle, []byte(value)); err != nil {
		h.t.Fatalf("PutSeatAccessRecord(%q, %q): %v", kind, handle, err)
	}
}

func (h *fleetHarness) seatAccess(kind string) map[string][]byte {
	h.t.Helper()
	got, err := h.f.SeatAccessRecords(h.ctx, kind)
	if err != nil {
		h.t.Fatalf("SeatAccessRecords(%q): %v", kind, err)
	}
	return got
}

func (h *fleetHarness) seatAccessHandles(kind string) []string {
	h.t.Helper()
	return slices.Sorted(maps.Keys(h.seatAccess(kind)))
}

var seatAccessCases = []fleetCase{
	{name: "a record reads back byte for byte under its seat", fn: func(h *fleetHarness) {
		// OPAQUE to this store, like an integration status: a backend
		// that re-encoded would hand the sync a record it never wrote.
		const rec = `{"kind":"datadog","handle":"sre","account":{"vendor_account_id":"u-1"}}`
		h.putSeatAccess("datadog", "sre", rec)
		if got := string(h.seatAccess("datadog")["sre"]); got != rec {
			h.t.Fatalf("read back %q, want %q", got, rec)
		}
	}},

	{name: "records are listed per integration and never across", fn: func(h *fleetHarness) {
		// THE CLASS AND THE KIND ARE BOTH LOAD-BEARING: one seat holds an
		// account at several vendors, and a listing that leaked another
		// vendor's record would have a sync disable an account it never
		// created.
		h.putSeatAccess("datadog", "sre", `{}`)
		h.putSeatAccess("datadog", "cto", `{}`)
		h.putSeatAccess("gitlab", "sre", `{}`)
		h.putIntegration("datadog", `{}`)
		if got, want := h.seatAccessHandles("datadog"), []string{"cto", "sre"}; !slices.Equal(got, want) {
			h.t.Fatalf("datadog listed %v, want %v", got, want)
		}
		if got, want := h.seatAccessHandles("gitlab"), []string{"sre"}; !slices.Equal(got, want) {
			h.t.Fatalf("gitlab listed %v, want %v", got, want)
		}
		if got := h.seatAccessHandles("slack"); len(got) != 0 {
			h.t.Fatalf("slack listed %v, want nothing", got)
		}
	}},

	{name: "the last write wins", fn: func(h *fleetHarness) {
		// Safe because every writer holds the kind's provisioning lease;
		// see coord.SeatAccess.
		h.putSeatAccess("gitlab", "sre", `{"n":1}`)
		h.putSeatAccess("gitlab", "sre", `{"n":2}`)
		if got := string(h.seatAccess("gitlab")["sre"]); got != `{"n":2}` {
			h.t.Fatalf("read back %q, want the second write", got)
		}
	}},

	{name: "a deleted record is gone, the rest survive, and deleting again is fine", fn: func(h *fleetHarness) {
		h.putSeatAccess("mattermost", "sre", `{}`)
		h.putSeatAccess("mattermost", "cto", `{}`)
		for range 2 {
			if err := h.f.DeleteSeatAccessRecord(h.ctx, "mattermost", "sre"); err != nil {
				h.t.Fatalf("DeleteSeatAccessRecord: %v", err)
			}
		}
		if got, want := h.seatAccessHandles("mattermost"), []string{"cto"}; !slices.Equal(got, want) {
			h.t.Fatalf("listed %v, want %v", got, want)
		}
	}},

	{name: "a handle with a dot survives the round trip as one seat", fn: func(h *fleetHarness) {
		// A dot is the key separator: an unescaped one would split one
		// seat into two segments and the listing would drop it.
		h.putSeatAccess("slack", "nova.1", `{}`)
		if got, want := h.seatAccessHandles("slack"), []string{"nova.1"}; !slices.Equal(got, want) {
			h.t.Fatalf("listed %v, want %v", got, want)
		}
	}},

	{name: "a record the caller mutates afterwards does not change the store", fn: func(h *fleetHarness) {
		value := []byte(`{"n":1}`)
		if err := h.f.PutSeatAccessRecord(h.ctx, "datadog", "sre", value); err != nil {
			h.t.Fatalf("PutSeatAccessRecord: %v", err)
		}
		copy(value, `{"n":9}`)
		if got := string(h.seatAccess("datadog")["sre"]); got != `{"n":1}` {
			h.t.Fatalf("read back %q, want what was written", got)
		}
	}},
}
