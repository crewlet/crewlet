package coordtest

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the placement maps ------------------------------------------------ //

// mapFamily is one placement map's three verbs, reduced to the bytes and the
// version, so the object map and the estate map are held to ONE set of
// compare-and-set cases: they differ in what they place, never in how a
// version is arbitrated, and two copies of the cases would drift the way two
// copies of the implementation would.
type mapFamily struct {
	read   func(h *fleetHarness) ([]byte, uint64, bool, error)
	create func(h *fleetHarness, value []byte) (uint64, bool, error)
	update func(h *fleetHarness, value []byte, version uint64) (uint64, bool, error)
}

var objectMaps = mapFamily{
	read: func(h *fleetHarness) ([]byte, uint64, bool, error) {
		r, found, err := h.f.ObjectMap(h.ctx)
		return r.Value, r.Version, found, err
	},
	create: func(h *fleetHarness, value []byte) (uint64, bool, error) {
		r, ok, err := h.f.CreateObjectMap(h.ctx, value)
		return r.Version, ok, err
	},
	update: func(h *fleetHarness, value []byte, version uint64) (uint64, bool, error) {
		r, ok, err := h.f.UpdateObjectMap(h.ctx, value, version)
		return r.Version, ok, err
	},
}

var estateMaps = mapFamily{
	read: func(h *fleetHarness) ([]byte, uint64, bool, error) {
		r, found, err := h.f.EstateMap(h.ctx)
		return r.Value, r.Version, found, err
	},
	create: func(h *fleetHarness, value []byte) (uint64, bool, error) {
		r, ok, err := h.f.CreateEstateMap(h.ctx, value)
		return r.Version, ok, err
	},
	update: func(h *fleetHarness, value []byte, version uint64) (uint64, bool, error) {
		r, ok, err := h.f.UpdateEstateMap(h.ctx, value, version)
		return r.Version, ok, err
	},
}

// casCases are the compare-and-set cases every placement map passes.
func casCases(m mapFamily) []fleetCase {
	return []fleetCase{{
		// The reason the map is here: every node places by it, and the
		// node that wrote it is one of many that read it.
		name: "a map one node wrote is readable by every node",
		fn: func(h *fleetHarness) {
			if _, _, found, err := m.read(h); err != nil || found {
				h.t.Fatalf("a fresh fleet reports a map (found=%v, err=%v)", found, err)
			}
			stored, created, err := m.create(h, []byte(`{"epoch":1}`))
			if err != nil || !created {
				h.t.Fatalf("the first map was not written (created=%v, err=%v)", created, err)
			}
			if stored == 0 {
				h.t.Fatal("the written map carries no version, so no change can be conditioned on it")
			}
			value, version, found, err := m.read(h)
			if err != nil || !found {
				h.t.Fatalf("the written map is unreadable (found=%v, err=%v)", found, err)
			}
			if !bytes.Equal(value, []byte(`{"epoch":1}`)) || version != stored {
				h.t.Errorf("read %q at %d, wrote %q at %d", value, version, `{"epoch":1}`, stored)
			}
		},
	}, {
		// TWO DUTY HOLDERS ON AN EMPTY FLEET write one map between them:
		// the second create loses, and it must not replace the first.
		name: "a second first map loses to the first",
		fn: func(h *fleetHarness) {
			if _, created, err := m.create(h, []byte(`{"epoch":1}`)); err != nil || !created {
				h.t.Fatalf("first create: created=%v err=%v", created, err)
			}
			if _, created, err := m.create(h, []byte(`{"epoch":9}`)); err != nil || created {
				h.t.Fatalf("second create: created=%v err=%v, want a lost race", created, err)
			}
			value, _, _, _ := m.read(h)
			if !bytes.Equal(value, []byte(`{"epoch":1}`)) {
				h.t.Fatalf("the losing create replaced the map with %q", value)
			}
		},
	}, {
		// A CHANGE IS CONDITIONED ON WHAT ITS WRITER READ. A duty that
		// moved mid-change must lose to the holder that wrote after it
		// read, or two maps would each be somebody's truth for an instant.
		name: "a map change at a stale version loses",
		fn: func(h *fleetHarness) {
			first, _, _ := m.create(h, []byte(`{"epoch":1}`))
			second, ok, err := m.update(h, []byte(`{"epoch":2}`), first)
			if err != nil || !ok {
				h.t.Fatalf("an update at the current version lost (ok=%v, err=%v)", ok, err)
			}
			if _, ok, err := m.update(h, []byte(`{"epoch":3}`), first); err != nil || ok {
				h.t.Fatalf("an update at a stale version won (ok=%v, err=%v)", ok, err)
			}
			if _, ok, err := m.update(h, []byte(`{"epoch":3}`), 0); err != nil || ok {
				h.t.Fatalf("an update at no version won (ok=%v, err=%v)", ok, err)
			}
			value, version, _, _ := m.read(h)
			if !bytes.Equal(value, []byte(`{"epoch":2}`)) || version != second {
				h.t.Fatalf("the map is %q at %d, want the second write", value, version)
			}
		},
	}, {
		// An update of a map nobody wrote is a lost race, never a create:
		// a writer that read no map has nothing to have read a version of.
		name: "an update of a map that does not exist loses",
		fn: func(h *fleetHarness) {
			if _, ok, err := m.update(h, []byte(`{"epoch":1}`), 1); err != nil || ok {
				h.t.Fatalf("an update created the map (ok=%v, err=%v)", ok, err)
			}
			if _, _, found, err := m.read(h); err != nil || found {
				h.t.Fatalf("the fleet reports a map after a lost update (found=%v, err=%v)", found, err)
			}
		},
	}}
}

var objectMapCases = casCases(objectMaps)

// watchWait bounds how long a case waits for a watch to deliver one version.
// Far above what either backend takes — a write lands on the twin's queue
// under the write's own lock, and on the broker in a round trip — so a case
// that times out is a version the watch never delivered, and far below the
// stall budget, so a missing one fails its own case rather than the suite.
const watchWait = 15 * time.Second

// watch opens an estate map watch that ends with the case.
func (h *fleetHarness) watch() (<-chan coord.EstateMapRecord, context.CancelFunc) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(h.ctx)
	h.t.Cleanup(cancel)
	ch, err := h.f.WatchEstateMap(ctx)
	if err != nil {
		cancel()
		h.t.Fatalf("WatchEstateMap: %v", err)
	}
	return ch, cancel
}

// next is the watch's next delivery, failing the case when none arrives or
// the watch ended.
func (h *fleetHarness) next(ch <-chan coord.EstateMapRecord) coord.EstateMapRecord {
	h.t.Helper()
	select {
	case rec, ok := <-ch:
		if !ok {
			h.t.Fatal("the watch ended while a version was still to come")
		}
		return rec
	case <-time.After(watchWait):
		h.t.Fatalf("the watch delivered nothing within %v", watchWait)
	}
	return coord.EstateMapRecord{}
}

// writeEstate creates the estate map, or updates it at version, and answers
// the version written.
func (h *fleetHarness) writeEstate(value string, version uint64) uint64 {
	h.t.Helper()
	var (
		rec coord.EstateMapRecord
		ok  bool
		err error
	)
	if version == 0 {
		rec, ok, err = h.f.CreateEstateMap(h.ctx, []byte(value))
	} else {
		rec, ok, err = h.f.UpdateEstateMap(h.ctx, []byte(value), version)
	}
	if err != nil || !ok {
		h.t.Fatalf("writing %s at %d: ok=%v err=%v", value, version, ok, err)
	}
	return rec.Version
}

// requireDelivery fails the case unless rec is value at version.
func (h *fleetHarness) requireDelivery(rec coord.EstateMapRecord, value string, version uint64) {
	h.t.Helper()
	if string(rec.Value) != value || rec.Version != version {
		h.t.Fatalf("the watch delivered %s at %d, want %s at %d",
			rec.Value, rec.Version, value, version)
	}
}

// through reads deliveries until the one at version want, failing the case
// unless every one it was handed was a version written — at the value written
// at it — and newer than the one before: a watch may skip a version, but it
// never hands over one that was not written or an older one after a newer.
func (h *fleetHarness) through(ch <-chan coord.EstateMapRecord, written map[uint64]string, want uint64) {
	h.t.Helper()
	var last uint64
	for {
		rec := h.next(ch)
		value, ok := written[rec.Version]
		switch {
		case !ok:
			h.t.Fatalf("the watch delivered version %d, which nobody wrote", rec.Version)
		case string(rec.Value) != value:
			h.t.Fatalf("the watch delivered %s at %d, where %s was written", rec.Value,
				rec.Version, value)
		case rec.Version <= last:
			h.t.Fatalf("the watch delivered version %d after %d", rec.Version, last)
		case rec.Version > want:
			h.t.Fatalf("the watch delivered version %d past %d, the newest written", rec.Version, want)
		}
		if last = rec.Version; last == want {
			return
		}
	}
}

var estateMapCases = append(casCases(estateMaps), []fleetCase{{
	// A NODE THAT STARTS WATCHING IS HANDED WHERE THE MAP IS, then how it
	// moves: a router routes by the current map at once, and a joiner
	// learns it was named by the write that named it, not an interval
	// later. The version it is handed first is the CURRENT one, never an
	// older one it would act on and then have to undo — and each later one
	// written while it was reading arrives in turn.
	name: "a watch delivers the current map and then each later version",
	fn: func(h *fleetHarness) {
		v1 := h.writeEstate(`{"epoch":1}`, 0)
		v2 := h.writeEstate(`{"epoch":2}`, v1)
		ch, _ := h.watch()
		h.requireDelivery(h.next(ch), `{"epoch":2}`, v2)
		v3 := h.writeEstate(`{"epoch":3}`, v2)
		h.requireDelivery(h.next(ch), `{"epoch":3}`, v3)
		v4 := h.writeEstate(`{"epoch":4}`, v3)
		h.requireDelivery(h.next(ch), `{"epoch":4}`, v4)
	},
}, {
	// A FLEET WITH NO MAP YET is watched like any other: the watch waits,
	// and the first map written is the first thing it delivers — nothing
	// invented before it.
	name: "a watch opened before the first map delivers the first map",
	fn: func(h *fleetHarness) {
		ch, _ := h.watch()
		v1 := h.writeEstate(`{"epoch":1}`, 0)
		h.requireDelivery(h.next(ch), `{"epoch":1}`, v1)
	},
}, {
	// THE NEWEST, IN ORDER. A reader that fell behind may be spared a
	// version superseded while it was busy — the store keeps one — but is
	// never handed an older version after a newer, nor one nobody wrote,
	// and always arrives at the newest: a node acts on the newest map it
	// holds, and one that acted on an older after a newer would undo a
	// join or a leave the fleet had moved past.
	name: "a watch whose reader falls behind arrives at the newest version in order",
	fn: func(h *fleetHarness) {
		version := h.writeEstate(`{"epoch":0}`, 0)
		written := map[uint64]string{version: `{"epoch":0}`}
		ch, _ := h.watch()
		for i := 1; i <= 40; i++ {
			value := fmt.Sprintf(`{"epoch":%d}`, i)
			version = h.writeEstate(value, version)
			written[version] = value
		}
		h.through(ch, written, version)
	},
}, {
	// Every node watches, so a second watch is not a second reader of one
	// feed: each arrives at the newest version on its own.
	name: "every watch arrives at the newest version",
	fn: func(h *fleetHarness) {
		first, _ := h.watch()
		second, _ := h.watch()
		v1 := h.writeEstate(`{"epoch":1}`, 0)
		v2 := h.writeEstate(`{"epoch":2}`, v1)
		written := map[uint64]string{v1: `{"epoch":1}`, v2: `{"epoch":2}`}
		for _, ch := range []<-chan coord.EstateMapRecord{first, second} {
			h.through(ch, written, v2)
		}
	},
}, {
	// THE VERSION A WATCH HANDS OVER IS ONE A CHANGE CAN BE CONDITIONED ON:
	// the maintainer reading the map it watched, and an operator's gesture
	// served from a watched copy, write at that version without a read in
	// between.
	name: "a watched version conditions a change",
	fn: func(h *fleetHarness) {
		ch, _ := h.watch()
		h.writeEstate(`{"epoch":1}`, 0)
		rec := h.next(ch)
		if _, ok, err := h.f.UpdateEstateMap(h.ctx, []byte(`{"epoch":2}`), rec.Version); err != nil || !ok {
			h.t.Fatalf("an update at the watched version lost (ok=%v, err=%v)", ok, err)
		}
	},
}, {
	// A WATCH ENDS WITH ITS CONTEXT, and says so by closing: a watch
	// that went on after its reader left would hold a goroutine, and on
	// the broker a consumer, for the life of the process.
	name: "a watch ends when its context ends",
	fn: func(h *fleetHarness) {
		ch, cancel := h.watch()
		h.writeEstate(`{"epoch":1}`, 0)
		cancel()
		deadline := time.After(watchWait)
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return
				}
			case <-deadline:
				h.t.Fatalf("the watch was still open %v after its context ended", watchWait)
			}
		}
	},
}}...)
