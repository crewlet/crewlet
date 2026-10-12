package coordtest

// ---- the object store ------------------------------------------------- //

var objectStoreCases = []fleetCase{{
	// THE LAST REPORT IS THE ANSWER, whoever wrote it: the collector's duty
	// moves between nodes, and every node's status surface reads the latest.
	name: "the collector's last report replaces the one before",
	fn: func(h *fleetHarness) {
		if _, found, err := h.f.ObjectCollection(h.ctx); err != nil || found {
			h.t.Fatalf("a fleet with no collection reports one: %v, %v", found, err)
		}
		for _, report := range []string{`{"node":"a"}`, `{"node":"b"}`} {
			if err := h.f.RecordObjectCollection(h.ctx, []byte(report)); err != nil {
				h.t.Fatalf("record %s: %v", report, err)
			}
		}
		got, found, err := h.f.ObjectCollection(h.ctx)
		if err != nil || !found || string(got) != `{"node":"b"}` {
			h.t.Fatalf("the report reads %s (found %v, %v), want the last one", got, found, err)
		}
	},
}, {
	// THE FIRST BACKEND RECORDED IS THE FLEET'S, and every later node is
	// told it whatever it asked to record: a node that named another would
	// otherwise split the company's files between two stores.
	name: "the first object backend recorded is the one every node is told",
	fn: func(h *fleetHarness) {
		got, err := h.f.AgreeObjectBackend(h.ctx, "nats")
		if err != nil || got != "nats" {
			h.t.Fatalf("the first record = %q, %v; want nats", got, err)
		}
		got, err = h.f.AgreeObjectBackend(h.ctx, "s3:files/acme/")
		if err != nil || got != "nats" {
			h.t.Fatalf("a second node naming another backend was told %q, %v; want nats", got, err)
		}
		got, err = h.f.AgreeObjectBackend(h.ctx, "nats")
		if err != nil || got != "nats" {
			h.t.Fatalf("a node naming the recorded backend was told %q, %v", got, err)
		}
	},
}}
