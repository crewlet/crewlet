package coordtest

import "github.com/crewlet/crewlet/internal/coord"

var atlasWakeCases = []fleetCase{
	{name: "an atlas wake is created once per seat and entry, and moved only at the revision read", fn: func(h *fleetHarness) {
		const entry, other = "0d4b2f0e-7c1a-8b3d-9e5f-6a7b8c9d0e1f", "1e5c3a1f-8d2b-8c4e-af60-7b8c9d0e1f2a"
		if all, err := h.f.AtlasWakes(h.ctx); err != nil || len(all) != 0 {
			h.t.Fatalf("an unwritten bucket listed %v (%v)", all, err)
		}
		for i, want := range []bool{true, false} {
			if ok, err := h.f.CreateAtlasWake(h.ctx, "sre", entry, []byte(`{"day":"a"}`)); err != nil || ok != want {
				h.t.Fatalf("create %d answered %v (%v), want %v", i, ok, err, want)
			}
		}
		// ANOTHER ENTRY, AND ANOTHER SEAT, ARE RECORDS OF THEIR OWN.
		for _, pair := range [][2]string{{"sre", other}, {"pm", entry}} {
			if ok, err := h.f.CreateAtlasWake(h.ctx, pair[0], pair[1], []byte(`{}`)); err != nil || !ok {
				h.t.Fatalf("create %v answered %v (%v)", pair, ok, err)
			}
		}
		all, listErr := h.f.AtlasWakes(h.ctx)
		key := coord.AtlasWakeKey("sre", entry)
		if listErr != nil || len(all) != 3 || string(all[key].Value) != `{"day":"a"}` || all[key].Revision == 0 {
			h.t.Fatalf("the wakes listed %v (%v), want three with sre's first as written", all, listErr)
		}
		value, read, found, readErr := h.f.AtlasWake(h.ctx, "sre", entry)
		if readErr != nil || !found || string(value) != `{"day":"a"}` || read != all[key].Revision {
			h.t.Fatalf("one record read back %q at %d (%v, %v), want the listing's", value, read, found, readErr)
		}
		if _, _, found, err := h.f.AtlasWake(h.ctx, "sre", "none"); err != nil || found {
			h.t.Fatalf("an unwritten record read as found %v (%v)", found, err)
		}
		if ok, err := h.f.UpdateAtlasWake(h.ctx, "sre", entry, []byte(`{"day":"b"}`), read); err != nil || !ok {
			h.t.Fatalf("an update at the revision read answered %v (%v)", ok, err)
		}
		if ok, err := h.f.UpdateAtlasWake(h.ctx, "sre", entry, []byte(`{"day":"c"}`), read); err != nil || ok {
			h.t.Fatalf("an update at a revision that moved answered %v (%v), want refused", ok, err)
		}
		if ok, err := h.f.DeleteAtlasWake(h.ctx, "sre", entry, read); err != nil || ok {
			h.t.Fatalf("a delete at a revision that moved answered %v (%v), want refused", ok, err)
		}
		all, listErr = h.f.AtlasWakes(h.ctx)
		if listErr != nil || string(all[key].Value) != `{"day":"b"}` {
			h.t.Fatalf("after the refused writes the record reads %q (%v)", all[key].Value, listErr)
		}
		if ok, err := h.f.DeleteAtlasWake(h.ctx, "sre", entry, all[key].Revision); err != nil || !ok {
			h.t.Fatalf("a delete at the revision read answered %v (%v)", ok, err)
		}
		if all, listErr = h.f.AtlasWakes(h.ctx); listErr != nil || len(all) != 2 {
			h.t.Fatalf("deleting one record left %v (%v), want the other two", all, listErr)
		}
	}},
}
