package coordtest

import "bytes"

// ---- the object placement map ------------------------------------------ //

var objectMapCases = []fleetCase{{
	// The reason the map is here: every node places by it, and the node that
	// wrote it is one of many that read it.
	name: "an object map one node wrote is readable by every node",
	fn: func(h *fleetHarness) {
		if _, found, err := h.f.ObjectMap(h.ctx); err != nil || found {
			h.t.Fatalf("a fresh fleet reports a map (found=%v, err=%v)", found, err)
		}
		stored, created, err := h.f.CreateObjectMap(h.ctx, []byte(`{"epoch":1}`))
		if err != nil || !created {
			h.t.Fatalf("the first map was not written (created=%v, err=%v)", created, err)
		}
		if stored.Version == 0 {
			h.t.Fatal("the written map carries no version, so no change can be conditioned on it")
		}
		read, found, err := h.f.ObjectMap(h.ctx)
		if err != nil || !found {
			h.t.Fatalf("the written map is unreadable (found=%v, err=%v)", found, err)
		}
		if !bytes.Equal(read.Value, []byte(`{"epoch":1}`)) || read.Version != stored.Version {
			h.t.Errorf("read %q at %d, wrote %q at %d", read.Value, read.Version,
				`{"epoch":1}`, stored.Version)
		}
	},
}, {
	// TWO DUTY HOLDERS ON AN EMPTY FLEET write one map between them: the
	// second create loses, and it must not replace the first.
	name: "a second first map loses to the first",
	fn: func(h *fleetHarness) {
		if _, created, err := h.f.CreateObjectMap(h.ctx, []byte(`{"epoch":1}`)); err != nil || !created {
			h.t.Fatalf("first create: created=%v err=%v", created, err)
		}
		if _, created, err := h.f.CreateObjectMap(h.ctx, []byte(`{"epoch":9}`)); err != nil || created {
			h.t.Fatalf("second create: created=%v err=%v, want a lost race", created, err)
		}
		read, _, _ := h.f.ObjectMap(h.ctx)
		if !bytes.Equal(read.Value, []byte(`{"epoch":1}`)) {
			h.t.Fatalf("the losing create replaced the map with %q", read.Value)
		}
	},
}, {
	// A CHANGE IS CONDITIONED ON WHAT ITS WRITER READ. A duty that moved
	// mid-change must lose to the holder that wrote after it read, or two
	// maps would each be somebody's truth for an instant.
	name: "an object map change at a stale version loses",
	fn: func(h *fleetHarness) {
		first, _, _ := h.f.CreateObjectMap(h.ctx, []byte(`{"epoch":1}`))
		second, ok, err := h.f.UpdateObjectMap(h.ctx, []byte(`{"epoch":2}`), first.Version)
		if err != nil || !ok {
			h.t.Fatalf("an update at the current version lost (ok=%v, err=%v)", ok, err)
		}
		if _, ok, err := h.f.UpdateObjectMap(h.ctx, []byte(`{"epoch":3}`), first.Version); err != nil || ok {
			h.t.Fatalf("an update at a stale version won (ok=%v, err=%v)", ok, err)
		}
		if _, ok, err := h.f.UpdateObjectMap(h.ctx, []byte(`{"epoch":3}`), 0); err != nil || ok {
			h.t.Fatalf("an update at no version won (ok=%v, err=%v)", ok, err)
		}
		read, _, _ := h.f.ObjectMap(h.ctx)
		if !bytes.Equal(read.Value, []byte(`{"epoch":2}`)) || read.Version != second.Version {
			h.t.Fatalf("the map is %q at %d, want the second write", read.Value, read.Version)
		}
	},
}}
