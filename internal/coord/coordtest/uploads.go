package coordtest

import (
	"fmt"
	"sync"
)

// ---- the upload reservations ------------------------------------------ //

func (h *fleetHarness) uploadLedger() (string, uint64) {
	h.t.Helper()
	value, rev, err := h.f.UploadLedger(h.ctx)
	if err != nil {
		h.t.Fatalf("UploadLedger: %v", err)
	}
	return string(value), rev
}

func (h *fleetHarness) swapLedger(value string, rev uint64) bool {
	h.t.Helper()
	ok, err := h.f.SwapUploadLedger(h.ctx, []byte(value), rev)
	if err != nil {
		h.t.Fatalf("SwapUploadLedger(%s at %d): %v", value, rev, err)
	}
	return ok
}

func (h *fleetHarness) uploadRecord(upload string) (string, uint64) {
	h.t.Helper()
	value, rev, err := h.f.UploadRecord(h.ctx, upload)
	if err != nil {
		h.t.Fatalf("UploadRecord(%s): %v", upload, err)
	}
	return string(value), rev
}

func (h *fleetHarness) swapRecord(upload, value string, rev uint64) bool {
	h.t.Helper()
	ok, err := h.f.SwapUploadRecord(h.ctx, upload, []byte(value), rev)
	if err != nil {
		h.t.Fatalf("SwapUploadRecord(%s at %d): %v", upload, rev, err)
	}
	return ok
}

func (h *fleetHarness) deleteRecord(upload string, rev uint64) bool {
	h.t.Helper()
	ok, err := h.f.DeleteUploadRecord(h.ctx, upload, rev)
	if err != nil {
		h.t.Fatalf("DeleteUploadRecord(%s at %d): %v", upload, rev, err)
	}
	return ok
}

var uploadReservationCases = []fleetCase{{
	// A FLEET THAT NEVER RESERVED READS AS NOTHING, at revision zero, which
	// is the revision the first reservation is written against.
	name: "a fleet that never reserved holds no ledger and no record",
	fn: func(h *fleetHarness) {
		if value, rev := h.uploadLedger(); value != "" || rev != 0 {
			h.t.Fatalf("a fresh fleet's ledger reads %q at %d, want nothing at 0", value, rev)
		}
		if value, rev := h.uploadRecord("u-1"); value != "" || rev != 0 {
			h.t.Fatalf("a fresh fleet's record reads %q at %d, want nothing at 0", value, rev)
		}
	},
}, {
	// THE LEDGER IS ONE COMPARE-AND-SET: a write against the revision read
	// lands, and one against any other is a lost race the caller re-reads
	// and re-decides, never a write that clobbers a reservation it did not
	// count.
	name: "the ledger is written only at the revision it was read at",
	fn: func(h *fleetHarness) {
		if !h.swapLedger(`{"holds":1}`, 0) {
			h.t.Fatal("the first ledger write was refused")
		}
		value, rev := h.uploadLedger()
		if value != `{"holds":1}` || rev == 0 {
			h.t.Fatalf("the ledger reads %q at %d after its first write", value, rev)
		}
		if h.swapLedger(`{"holds":"again"}`, 0) {
			h.t.Fatal("a second create of the ledger landed over the first")
		}
		if h.swapLedger(`{"holds":"stale"}`, rev+1000) {
			h.t.Fatal("a ledger write at a revision nobody read landed")
		}
		if !h.swapLedger(`{"holds":2}`, rev) {
			h.t.Fatal("a ledger write at the revision read was refused")
		}
		value, next := h.uploadLedger()
		if value != `{"holds":2}` || next <= rev {
			h.t.Fatalf("the ledger reads %q at %d after a write at %d", value, next, rev)
		}
		if h.swapLedger(`{"holds":"lost"}`, rev) {
			h.t.Fatal("a write at the superseded revision landed")
		}
	},
}, {
	// RACING RESERVATIONS SERIALISE: of every writer that read one revision,
	// exactly one lands, or two intents could each spend the same headroom.
	name: "racing ledger writes at one revision land exactly once",
	fn: func(h *fleetHarness) {
		if !h.swapLedger(`{"holds":0}`, 0) {
			h.t.Fatal("the first ledger write was refused")
		}
		_, rev := h.uploadLedger()
		nodes := h.nodes(4)
		const writers = 8
		var (
			wg     sync.WaitGroup
			mu     sync.Mutex
			landed int
		)
		for i := range writers {
			node := nodes[i%len(nodes)]
			wg.Go(func() {
				ok, err := node.SwapUploadLedger(h.ctx, fmt.Appendf(nil, `{"holds":%d}`, i), rev)
				if err != nil {
					h.t.Errorf("SwapUploadLedger: %v", err)
					return
				}
				if ok {
					mu.Lock()
					landed++
					mu.Unlock()
				}
			})
		}
		wg.Wait()
		if landed != 1 {
			h.t.Fatalf("%d racing writes at one revision landed, want exactly one", landed)
		}
	},
}, {
	// A RECORD IS CREATED ONCE AND MOVED BY COMPARE-AND-SET: a transfer
	// claiming a key and a finalize claiming the upload each win or lose on
	// the revision they read.
	name: "an upload record is created once and moved only at its revision",
	fn: func(h *fleetHarness) {
		if !h.swapRecord("u-2", "open", 0) {
			h.t.Fatal("the record's create was refused")
		}
		if h.swapRecord("u-2", "again", 0) {
			h.t.Fatal("a second create landed over a live record")
		}
		value, rev := h.uploadRecord("u-2")
		if value != "open" || rev == 0 {
			h.t.Fatalf("the record reads %q at %d", value, rev)
		}
		if !h.swapRecord("u-2", "finalizing", rev) {
			h.t.Fatal("a write at the record's revision was refused")
		}
		if h.swapRecord("u-2", "lost", rev) {
			h.t.Fatal("a write at the superseded revision landed")
		}
		if value, _ := h.uploadRecord("u-2"); value != "finalizing" {
			h.t.Fatalf("the record reads %q, want the write that won", value)
		}
	},
}, {
	// A DELETE IS CONDITIONAL TOO, and a revision of zero names no record:
	// an early release that read an open record must not remove one a
	// finalize claimed after the read.
	name: "an upload record is removed only at its revision",
	fn: func(h *fleetHarness) {
		h.swapRecord("u-3", "open", 0)
		_, rev := h.uploadRecord("u-3")
		if h.deleteRecord("u-3", 0) {
			h.t.Fatal("a delete naming no revision removed the record")
		}
		h.swapRecord("u-3", "claimed", rev)
		if h.deleteRecord("u-3", rev) {
			h.t.Fatal("a delete at a superseded revision removed the record")
		}
		value, now := h.uploadRecord("u-3")
		if value != "claimed" {
			h.t.Fatalf("the record reads %q after refused deletes", value)
		}
		if !h.deleteRecord("u-3", now) {
			h.t.Fatal("a delete at the record's revision was refused")
		}
		if value, rev := h.uploadRecord("u-3"); value != "" || rev != 0 {
			h.t.Fatalf("a deleted record reads %q at %d", value, rev)
		}
		if h.deleteRecord("u-3", now) {
			h.t.Fatal("a second delete reported removing a record that is gone")
		}
	},
}, {
	// A RECORD WRITTEN AGAIN AFTER A DELETE NEVER HANDS BACK AN OLD
	// REVISION: a caller still holding the old one must lose against a
	// record it never read.
	name: "a record created again after a delete is at a revision no caller holds",
	fn: func(h *fleetHarness) {
		h.swapRecord("u-4", "first", 0)
		_, old := h.uploadRecord("u-4")
		if !h.deleteRecord("u-4", old) {
			h.t.Fatal("the delete was refused")
		}
		if !h.swapRecord("u-4", "second", 0) {
			h.t.Fatal("a create after the delete was refused")
		}
		if _, rev := h.uploadRecord("u-4"); rev == old {
			h.t.Fatalf("the new record is at %d, the deleted one's revision", rev)
		}
		if h.swapRecord("u-4", "stale", old) {
			h.t.Fatal("a write at the deleted record's revision landed on the new one")
		}
	},
}, {
	// THE LEDGER AND THE RECORDS ARE SEPARATE KEYS: no upload id, the word
	// "ledger" included, ever writes the ledger.
	name: "no upload record is the ledger",
	fn: func(h *fleetHarness) {
		h.swapRecord("ledger", "a record", 0)
		if value, rev := h.uploadLedger(); value != "" || rev != 0 {
			h.t.Fatalf("a record named ledger reads as the ledger: %q at %d", value, rev)
		}
		h.swapLedger("the ledger", 0)
		if value, _ := h.uploadRecord("ledger"); value != "a record" {
			h.t.Fatalf("writing the ledger moved the record named ledger: %q", value)
		}
	},
}, {
	name: "an empty value or upload id is an error, not a write",
	fn: func(h *fleetHarness) {
		if ok, err := h.f.SwapUploadLedger(h.ctx, nil, 0); err == nil || ok {
			h.t.Errorf("an empty ledger answered (%v, %v), want an error", ok, err)
		}
		if ok, err := h.f.SwapUploadRecord(h.ctx, "", []byte("x"), 0); err == nil || ok {
			h.t.Errorf("an unnamed record answered (%v, %v), want an error", ok, err)
		}
		if ok, err := h.f.SwapUploadRecord(h.ctx, "u-5", nil, 0); err == nil || ok {
			h.t.Errorf("an empty record answered (%v, %v), want an error", ok, err)
		}
		if _, _, err := h.f.UploadRecord(h.ctx, ""); err == nil {
			h.t.Error("a read of an unnamed record answered no error")
		}
		if ok, err := h.f.DeleteUploadRecord(h.ctx, "", 1); err == nil || ok {
			h.t.Errorf("a delete of an unnamed record answered (%v, %v)", ok, err)
		}
	},
}}
