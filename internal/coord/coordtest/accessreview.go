package coordtest

var accessReviewCases = []fleetCase{
	{name: "an access review reads back byte for byte, last write wins, and deletes", fn: func(h *fleetHarness) {
		if _, found, err := h.f.AccessReview(h.ctx, "sre"); err != nil || found {
			h.t.Fatalf("an unwritten review read as found %v (%v)", found, err)
		}
		for _, rec := range []string{`{"digest":"a"}`, `{"digest":"b"}`} {
			if err := h.f.PutAccessReview(h.ctx, "sre", []byte(rec)); err != nil {
				h.t.Fatalf("PutAccessReview: %v", err)
			}
		}
		if err := h.f.PutAccessReview(h.ctx, "pm", []byte(`{}`)); err != nil {
			h.t.Fatalf("PutAccessReview: %v", err)
		}
		got, found, err := h.f.AccessReview(h.ctx, "sre")
		if err != nil || !found || string(got) != `{"digest":"b"}` {
			h.t.Fatalf("read back %q (%v, %v), want the second write", got, found, err)
		}
		for range 2 {
			if err := h.f.DeleteAccessReview(h.ctx, "sre"); err != nil {
				h.t.Fatalf("DeleteAccessReview: %v", err)
			}
		}
		if _, found, _ := h.f.AccessReview(h.ctx, "sre"); found {
			h.t.Fatal("a deleted review still reads")
		}
		if _, found, _ := h.f.AccessReview(h.ctx, "pm"); !found {
			h.t.Fatal("deleting one seat's review took another's")
		}
	}},
}
