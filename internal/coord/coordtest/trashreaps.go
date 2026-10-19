package coordtest

// ---- the trash reaper ------------------------------------------------- //

var trashReapCases = []fleetCase{{
	// THE LAST REPORT IS THE ANSWER, whoever wrote it: the reaper's duty moves
	// between nodes, and the node that takes it reads when the next pass is
	// due from the latest pass, not from its own.
	name: "the trash reaper's last report replaces the one before",
	fn: func(h *fleetHarness) {
		if _, found, err := h.f.TrashReap(h.ctx); err != nil || found {
			h.t.Fatalf("a fleet nobody reaped reports a pass: %v, %v", found, err)
		}
		for _, report := range []string{`{"node":"a"}`, `{"node":"b"}`} {
			if err := h.f.RecordTrashReap(h.ctx, []byte(report)); err != nil {
				h.t.Fatalf("record %s: %v", report, err)
			}
		}
		got, found, err := h.f.TrashReap(h.ctx)
		if err != nil || !found || string(got) != `{"node":"b"}` {
			h.t.Fatalf("the report reads %s (found %v, %v), want the last one", got, found, err)
		}
	},
}, {
	// AN EMPTY REPORT IS REFUSED rather than stored: read back it would be a
	// pass that said nothing, which no reaper writes and no reader can decode.
	name: "an empty trash reap report is refused",
	fn: func(h *fleetHarness) {
		if err := h.f.RecordTrashReap(h.ctx, nil); err == nil {
			h.t.Fatal("an empty report was recorded")
		}
		if _, found, err := h.f.TrashReap(h.ctx); err != nil || found {
			h.t.Fatalf("a refused report reads back: %v, %v", found, err)
		}
	},
}, {
	// THE REPORT IS ITS OWN RECORD, beside the object collector's in the
	// same family: recording one never answers for the other, in either
	// direction.
	name: "the trash reap report and the object collection are separate records",
	fn: func(h *fleetHarness) {
		if err := h.f.RecordTrashReap(h.ctx, []byte(`{"reap":1}`)); err != nil {
			h.t.Fatalf("record the reap: %v", err)
		}
		if _, found, err := h.f.ObjectCollection(h.ctx); err != nil || found {
			h.t.Fatalf("a reap report reads as a collection: %v, %v", found, err)
		}
		if err := h.f.RecordObjectCollection(h.ctx, []byte(`{"collect":1}`)); err != nil {
			h.t.Fatalf("record the collection: %v", err)
		}
		got, found, err := h.f.TrashReap(h.ctx)
		if err != nil || !found || string(got) != `{"reap":1}` {
			h.t.Fatalf("the reap report reads %s (found %v, %v) after a collection", got, found, err)
		}
	},
}}
