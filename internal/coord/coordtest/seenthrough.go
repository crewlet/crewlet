package coordtest

var seenThroughCases = []fleetCase{
	{name: "a seen-through mark is written once, then only over the revision read", fn: func(h *fleetHarness) {
		if ok, err := h.f.MarkSeenThrough(h.ctx, "swe", "", "worklet@1:40", 0); err == nil || ok {
			h.t.Errorf("a mark with no conversation answered (%v, %v), want a refusal", ok, err)
		}
		if mark, rev, err := h.f.SeenThrough(h.ctx, "swe", "work:W-1"); err != nil || mark != "" || rev != 0 {
			h.t.Fatalf("an unwritten mark reads (%q, %d, %v), want none", mark, rev, err)
		}
		if ok, err := h.f.MarkSeenThrough(h.ctx, "swe", "work:W-1", "worklet@1:40", 0); err != nil || !ok {
			h.t.Fatalf("the first mark: (%v, %v)", ok, err)
		}
		if ok, err := h.f.MarkSeenThrough(h.ctx, "swe", "work:W-1", "worklet@1:35", 0); err != nil || ok {
			h.t.Fatalf("a second first mark: (%v, %v), want it refused as moved", ok, err)
		}
		mark, rev, err := h.f.SeenThrough(h.ctx, "swe", "work:W-1")
		if err != nil || mark != "worklet@1:40" || rev == 0 {
			h.t.Fatalf("the mark reads (%q, %d, %v), want worklet@1:40 at a revision", mark, rev, err)
		}
		if ok, err := h.f.MarkSeenThrough(h.ctx, "swe", "work:W-1", "worklet@1:44", rev+1); err != nil || ok {
			h.t.Fatalf("a mark over a revision never read: (%v, %v), want it refused", ok, err)
		}
		if ok, err := h.f.MarkSeenThrough(h.ctx, "swe", "work:W-1", "worklet@1:44", rev); err != nil || !ok {
			h.t.Fatalf("a mark over the revision read: (%v, %v)", ok, err)
		}
		if other, _, err := h.f.SeenThrough(h.ctx, "qa", "work:W-1"); err != nil || other != "" {
			h.t.Errorf("another seat's mark reads (%q, %v), want none", other, err)
		}
	}},
}
