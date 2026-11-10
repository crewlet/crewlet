package coordtest

import "github.com/crewlet/crewlet/internal/coord"

var boxReadingCases = []fleetCase{
	{name: "a box reading is kept per seat and cell, replaced by the next and dropped alone", fn: func(h *fleetHarness) {
		swe := seatID("swe")
		reading := func(seat, cell, basis string, memory int64, browser string) coord.BoxReading {
			return coord.BoxReading{Seat: seatID(seat), Cell: cell, Basis: basis, MemoryBytes: memory,
				Browser: browser, ContainerRuntime: "docker", At: h.now()}
		}
		for _, bad := range []coord.BoxReading{
			{Cell: "container", Basis: "b1", At: h.now()},
			{Seat: swe, Basis: "b1", At: h.now()},
			{Seat: swe, Cell: "container", At: h.now()},
			{Seat: swe, Cell: "container", Basis: "b1"},
			{Seat: swe, Cell: "container", Basis: "b1", MemoryBytes: -1, At: h.now()},
		} {
			if err := h.f.PutBoxReading(h.ctx, bad); err == nil {
				h.t.Errorf("PutBoxReading(%+v) was kept, want a refusal", bad)
			}
		}
		if _, ok, err := h.f.BoxReading(h.ctx, swe, "container"); err != nil || ok {
			h.t.Fatalf("a seat no run read answered %v (%v), want none", ok, err)
		}
		for _, r := range []coord.BoxReading{
			reading("swe", "container", "b1", 2<<30, ""),
			reading("swe", "container", "b2", 4<<30, "chromium"),
			reading("swe", "e2b", "b3", 8<<30, ""),
			reading("qa", "container", "b1", 2<<30, ""),
		} {
			if err := h.f.PutBoxReading(h.ctx, r); err != nil {
				h.t.Fatalf("PutBoxReading(%+v): %v", r, err)
			}
		}
		got, ok, err := h.f.BoxReading(h.ctx, swe, "container")
		if err != nil || !ok || got.Basis != "b2" || got.MemoryBytes != 4<<30 || got.Browser != "chromium" ||
			got.ContainerRuntime != "docker" || !got.At.Equal(h.now()) {
			h.t.Fatalf("swe's container reading is %+v, %v (%v), want the second one kept", got, ok, err)
		}
		if all, err := h.f.ListBoxReadings(h.ctx); err != nil || len(all) != 3 {
			h.t.Fatalf("every reading is %+v (%v), want swe's two and qa's one", all, err)
		}
		if err := h.f.DeleteBoxReading(h.ctx, swe, "container"); err != nil {
			h.t.Fatalf("DeleteBoxReading: %v", err)
		}
		if err := h.f.DeleteBoxReading(h.ctx, swe, "container"); err != nil {
			h.t.Fatalf("dropping a reading already gone: %v", err)
		}
		if _, ok, _ := h.f.BoxReading(h.ctx, swe, "container"); ok {
			h.t.Error("a dropped reading is still read")
		}
		for _, kept := range []struct {
			seat string
			cell string
		}{{"swe", "e2b"}, {"qa", "container"}} {
			if _, ok, err := h.f.BoxReading(h.ctx, seatID(kept.seat), kept.cell); err != nil || !ok {
				h.t.Errorf("dropping swe's container reading took %s's %s one (%v)", kept.seat, kept.cell, err)
			}
		}
	}},
}
