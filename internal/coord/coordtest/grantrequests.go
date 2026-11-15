package coordtest

import (
	"maps"
	"slices"
)

// ---- the agents' grant requests ---------------------------------------- //

func (h *fleetHarness) putGrantRequest(id, value string) {
	h.t.Helper()
	if err := h.f.PutGrantRequest(h.ctx, id, []byte(value)); err != nil {
		h.t.Fatalf("PutGrantRequest(%q): %v", id, err)
	}
}

func (h *fleetHarness) grantRequestIDs() []string {
	h.t.Helper()
	got, err := h.f.GrantRequests(h.ctx)
	if err != nil {
		h.t.Fatalf("GrantRequests: %v", err)
	}
	return slices.Sorted(maps.Keys(got))
}

var grantRequestCases = []fleetCase{
	{name: "a request reads back byte for byte under its id", fn: func(h *fleetHarness) {
		// OPAQUE to this store: a backend that re-encoded would hand the
		// person a request the agent never made.
		const rec = `{"id":"abcdefghij","permission":"Origin Write"}`
		h.putGrantRequest("abcdefghij", rec)
		got, found, err := h.f.GrantRequest(h.ctx, "abcdefghij")
		if err != nil || !found || string(got) != rec {
			h.t.Fatalf("GrantRequest = %q, %v, %v; want %q", got, found, err, rec)
		}
	}},

	{name: "a request never asked is no request, and nothing else lists as one", fn: func(h *fleetHarness) {
		// THE CLASS IS LOAD-BEARING: a seat access record listed as a
		// request would be granted from a record nobody asked.
		h.putSeatAccess("cloudflare", "sre", `{}`)
		h.putGrantRequest("abcdefghij", `{}`)
		if _, found, err := h.f.GrantRequest(h.ctx, "zzzzzzzzzz"); err != nil || found {
			h.t.Fatalf("an unknown id read as found=%v, err=%v", found, err)
		}
		if got := h.grantRequestIDs(); !slices.Equal(got, []string{"abcdefghij"}) {
			h.t.Fatalf("listed %v, want the one request", got)
		}
	}},

	{name: "a dropped request is gone, the rest survive, and dropping again is fine", fn: func(h *fleetHarness) {
		h.putGrantRequest("aaaaaaaaaa", `{}`)
		h.putGrantRequest("bbbbbbbbbb", `{}`)
		for range 2 {
			if err := h.f.DeleteGrantRequest(h.ctx, "aaaaaaaaaa"); err != nil {
				h.t.Fatalf("DeleteGrantRequest: %v", err)
			}
		}
		if got := h.grantRequestIDs(); !slices.Equal(got, []string{"bbbbbbbbbb"}) {
			h.t.Fatalf("listed %v after the drop, want the other", got)
		}
	}},
}
