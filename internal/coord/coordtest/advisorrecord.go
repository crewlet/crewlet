package coordtest

var advisorRecordCases = []fleetCase{
	{name: "an advisor record is created once and moved only at the revision read", fn: func(h *fleetHarness) {
		if _, _, found, err := h.f.AdvisorRecord(h.ctx, "last_run"); err != nil || found {
			h.t.Fatalf("an unwritten record read as found %v (%v)", found, err)
		}
		for i, want := range []bool{true, false} {
			if ok, err := h.f.CreateAdvisorRecord(h.ctx, "last_run", []byte(`{"n":1}`)); err != nil || ok != want {
				h.t.Fatalf("create %d answered %v (%v), want %v", i, ok, err, want)
			}
		}
		value, read, found, err := h.f.AdvisorRecord(h.ctx, "last_run")
		if err != nil || !found || string(value) != `{"n":1}` || read == 0 {
			h.t.Fatalf("read back %q at %d (%v, %v)", value, read, found, err)
		}
		if ok, err := h.f.UpdateAdvisorRecord(h.ctx, "last_run", []byte(`{"n":2}`), read); err != nil || !ok {
			h.t.Fatalf("an update at the revision read answered %v (%v)", ok, err)
		}
		if ok, err := h.f.UpdateAdvisorRecord(h.ctx, "last_run", []byte(`{"n":3}`), read); err != nil || ok {
			h.t.Fatalf("an update at a revision that moved answered %v (%v), want refused", ok, err)
		}
		if value, _, _, err = h.f.AdvisorRecord(h.ctx, "last_run"); err != nil || string(value) != `{"n":2}` {
			h.t.Fatalf("after the refused write the record reads %q (%v)", value, err)
		}
		if _, _, found, err := h.f.AdvisorRecord(h.ctx, "answers"); err != nil || found {
			h.t.Fatalf("another record read as found %v (%v)", found, err)
		}
	}},
}
