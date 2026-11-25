package coordtest

import (
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the object store ------------------------------------------------- //

var objectStoreCases = []fleetCase{{
	// A MOVE IS CONDITIONAL ON THE STORE IT MOVES FROM: the migration that
	// moved the files made it once, a retry of that move changes nothing,
	// and a move from a store that is not recorded is refused by name.
	name: "the object backend moves only from the store recorded",
	fn: func(h *fleetHarness) {
		if err := h.f.MoveObjectBackend(h.ctx, "nats", "s3:b/"); !errors.Is(err, coord.ErrObjectBackendMoved) {
			h.t.Fatalf("a move with nothing recorded answered %v, want ErrObjectBackendMoved", err)
		}
		if _, err := h.f.AgreeObjectBackend(h.ctx, "nats"); err != nil {
			h.t.Fatal(err)
		}
		if err := h.f.MoveObjectBackend(h.ctx, "s3:other/", "s3:b/"); !errors.Is(err, coord.ErrObjectBackendMoved) {
			h.t.Fatalf("a move from a store not recorded answered %v", err)
		}
		for range 2 {
			if err := h.f.MoveObjectBackend(h.ctx, "nats", "s3:b/"); err != nil {
				h.t.Fatalf("the move, or its retry: %v", err)
			}
		}
		got, err := h.f.AgreeObjectBackend(h.ctx, "nats")
		if err != nil || got != "s3:b/" {
			h.t.Fatalf("after the move a node naming nats is told %q, %v; want the bucket", got, err)
		}
	},
}, {
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
	// EVERY RECORD OF THE OBJECTS BUCKET IS A SMALL VALUE, on every backend:
	// the agreed store, the collector's report and the trash reaper's past
	// coord.SmallValueBytes are refused as too large and nothing is written,
	// and at it each is written. Mutation: hold the twin's collection report
	// to coord.MaxValueBytes, and its past case is written.
	name: "an objects record past a small value is refused, and at it is written",
	fn: func(h *fleetHarness) {
		at, past := strings.Repeat("s", coord.SmallValueBytes), strings.Repeat("s", coord.SmallValueBytes+1)
		_, err := h.f.AgreeObjectBackend(h.ctx, past)
		h.tooLarge("an object backend", err)
		h.tooLarge("a collection report", h.f.RecordObjectCollection(h.ctx, []byte(past)))
		h.tooLarge("a trash reap report", h.f.RecordTrashReap(h.ctx, []byte(past)))
		if got, err := h.f.AgreeObjectBackend(h.ctx, at); err != nil || got != at {
			h.t.Errorf("an object backend at a small value = %d bytes, %v, want it agreed", len(got), err)
		}
		if err := h.f.RecordObjectCollection(h.ctx, []byte(at)); err != nil {
			h.t.Errorf("a collection report at a small value = %v, want it written", err)
		}
		if err := h.f.RecordTrashReap(h.ctx, []byte(at)); err != nil {
			h.t.Errorf("a trash reap report at a small value = %v, want it written", err)
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
