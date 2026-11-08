package coordtest

var accessReviewCases = []fleetCase{
	{name: "an access review is created once, updated and deleted only at the revision read", fn: func(h *fleetHarness) {
		if _, _, found, err := h.f.AccessReview(h.ctx, "sre"); err != nil || found {
			h.t.Fatalf("an unwritten review read as found %v (%v)", found, err)
		}
		for i, want := range []bool{true, false} {
			if ok, err := h.f.CreateAccessReview(h.ctx, "sre", []byte(`{"digest":"a"}`)); err != nil || ok != want {
				h.t.Fatalf("create %d answered %v (%v), want %v", i, ok, err, want)
			}
		}
		value, read, found, readErr := h.f.AccessReview(h.ctx, "sre")
		if readErr != nil || !found || string(value) != `{"digest":"a"}` || read == 0 {
			h.t.Fatalf("read back %q at %d (%v, %v)", value, read, found, readErr)
		}
		if ok, err := h.f.UpdateAccessReview(h.ctx, "sre", []byte(`{"digest":"b"}`), read); err != nil || !ok {
			h.t.Fatalf("an update at the revision read answered %v (%v)", ok, err)
		}
		if ok, err := h.f.UpdateAccessReview(h.ctx, "sre", []byte(`{"digest":"c"}`), read); err != nil || ok {
			h.t.Fatalf("an update at a revision that moved answered %v (%v), want refused", ok, err)
		}
		if ok, err := h.f.DeleteAccessReview(h.ctx, "sre", read); err != nil || ok {
			h.t.Fatalf("a delete at a revision that moved answered %v (%v), want refused", ok, err)
		}
		if ok, err := h.f.CreateAccessReview(h.ctx, "pm", []byte(`{}`)); err != nil || !ok {
			h.t.Fatalf("create pm: %v (%v)", ok, err)
		}
		revisions, err := h.f.AccessReviewRevisions(h.ctx)
		if err != nil || len(revisions) != 2 || revisions["sre"] == 0 || revisions["pm"] == 0 {
			h.t.Fatalf("the reviews listed %v (%v), want sre and pm", revisions, err)
		}
		if ok, err := h.f.DeleteAccessReview(h.ctx, "sre", revisions["sre"]); err != nil || !ok {
			h.t.Fatalf("a delete at the revision read answered %v (%v)", ok, err)
		}
		if _, _, found, _ := h.f.AccessReview(h.ctx, "sre"); found {
			h.t.Fatal("a deleted review still reads")
		}
		if _, _, found, _ := h.f.AccessReview(h.ctx, "pm"); !found {
			h.t.Fatal("deleting one seat's review took another's")
		}
	}},
}
