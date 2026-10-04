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
}, {
	// A CHUNK LOCK EXCLUDES EVERY OTHER OWNER until its holder lets it go
	// — the whole of what stops the collector deleting a chunk a file is
	// re-putting.
	name: "a chunk lock excludes every other owner until it is let go",
	fn: func(h *fleetHarness) {
		const chunk = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		took, err := h.f.LockChunk(h.ctx, chunk, "collector")
		if err != nil || !took {
			h.t.Fatalf("the first take = %v, %v", took, err)
		}
		if took, err = h.f.LockChunk(h.ctx, chunk, "writer"); err != nil || took {
			h.t.Fatalf("a second owner took a held lock: %v, %v", took, err)
		}
		// A take retried by its own holder is not a second holder.
		if took, err = h.f.LockChunk(h.ctx, chunk, "collector"); err != nil || !took {
			h.t.Fatalf("the holder's own retried take = %v, %v", took, err)
		}
		// Another owner cannot let it go.
		if err = h.f.UnlockChunk(h.ctx, chunk, "writer"); err != nil {
			h.t.Fatalf("an unlock by a non-holder: %v", err)
		}
		if took, err = h.f.LockChunk(h.ctx, chunk, "writer"); err != nil || took {
			h.t.Fatalf("a non-holder's unlock let the lock go: %v, %v", took, err)
		}
		if err = h.f.UnlockChunk(h.ctx, chunk, "collector"); err != nil {
			h.t.Fatalf("the holder's unlock: %v", err)
		}
		if took, err = h.f.LockChunk(h.ctx, chunk, "writer"); err != nil || !took {
			h.t.Fatalf("a lock let go could not be taken: %v, %v", took, err)
		}
		// Letting go of a lock nobody holds, or held by another, is fine.
		if err = h.f.UnlockChunk(h.ctx, "ffff"+chunk[4:], "collector"); err != nil {
			h.t.Fatalf("an unlock of an unheld chunk: %v", err)
		}
	},
}, {
	// ONE CHUNK'S LOCK IS NOT ANOTHER'S: the collector working through a
	// batch must not stall every upload.
	name: "chunk locks are per chunk",
	fn: func(h *fleetHarness) {
		const a = "aaaa456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		const b = "bbbb456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		if took, err := h.f.LockChunk(h.ctx, a, "collector"); err != nil || !took {
			h.t.Fatalf("lock a = %v, %v", took, err)
		}
		if took, err := h.f.LockChunk(h.ctx, b, "writer"); err != nil || !took {
			h.t.Fatalf("another chunk's lock was refused while a was held: %v, %v", took, err)
		}
	},
}}
