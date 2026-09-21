package chart_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
)

// applySeatRekey is [writeRig.applyRekey] for a seat.
func (r *writeRig) applySeatRekey(opID, handle, former string) {
	r.t.Helper()
	if _, err := r.writer.WriteRekey(r.t.Context(), opID,
		chart.ObjectRef{Kind: chart.KindSeat, ID: handle}, former); err != nil {

		r.t.Fatalf("rekey %s to %s: %v", former, handle, err)
	}
	r.drain()
}

// A RENAME FREEZES THE ORIGIN ONCE AND NEVER AGAIN.
//
// The origin is the seat's IDENTITY — its mailbox, its lease, its diary and
// its schedule ledger are all keyed on the id derived from it — so a second
// rename overwriting it would move every one of them, which is the whole bug
// this exists to not have. The first rename is the last moment the create
// address is still known, which is why that is where it is recorded.
func TestTheFirstRenameFreezesTheOriginAndLaterOnesLeaveItAlone(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))

	// Before any rename the seat carries no origin at all, and reads as its
	// own handle: a seat that has never been renamed answers to the handle
	// it was created under.
	before := r.mustSeat("sarah-chen")
	if before.OriginHandle != "" || before.Origin() != "sarah-chen" {
		t.Errorf("a seat nobody renamed carries origin_handle %q and reads %q, "+
			"want an empty column reading as its own handle",
			before.OriginHandle, before.Origin())
	}

	r.applySeatRekey("op-rename-1", "sarah-okonkwo", "sarah-chen")
	once := r.mustSeat("sarah-okonkwo")
	if once.OriginHandle != "sarah-chen" || once.Origin() != "sarah-chen" {
		t.Fatalf("after one rename the origin is %q (reads %q), want the handle "+
			"the seat was created under", once.OriginHandle, once.Origin())
	}

	r.applySeatRekey("op-rename-2", "sarah-o", "sarah-okonkwo")
	twice := r.mustSeat("sarah-o")
	if twice.OriginHandle != "sarah-chen" {
		t.Errorf("a second rename moved the origin to %q. Everything durable "+
			"this seat owns is keyed on the id derived from it, so a rename "+
			"that moves it is a seat that lost its mailbox, its lease and its "+
			"diary — which is the bug the origin exists to remove",
			twice.OriginHandle)
	}
	if got := twice.FormerHandles; len(got) != 2 ||
		got[0] != "sarah-okonkwo" || got[1] != "sarah-chen" {

		t.Errorf("former handles = %v, want both retired addresses newest first", got)
	}
}

// AND A UNIT, on the same terms.
func TestTheFirstRenameFreezesAUnitsOriginToo(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-build", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))
	if _, err := r.applyRekey("op-rename-1", "infrastructure", "platform"); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	if _, err := r.applyRekey("op-rename-2", "core", "infrastructure"); err != nil {
		t.Fatalf("second rekey: %v", err)
	}
	unit := r.mustUnit("core")
	if unit.OriginKey != "platform" || unit.Origin() != "platform" {
		t.Errorf("origin_key = %q (reads %q), want the key the unit was created "+
			"under, kept through every later rename",
			unit.OriginKey, unit.Origin())
	}
}

// mustSeat reads one seat's stored document, failing the test if it is gone.
func (r *writeRig) mustSeat(handle string) chart.Seat {
	r.t.Helper()
	got, err := r.reader().Seat(r.t.Context(), handle, session())
	if err != nil {
		r.t.Fatalf("read seat %s: %v", handle, err)
	}
	return got.Seat
}

// mustUnit is [writeRig.mustSeat] for a unit.
func (r *writeRig) mustUnit(key string) chart.Unit {
	r.t.Helper()
	got, err := r.reader().Unit(r.t.Context(), key, session())
	if err != nil {
		r.t.Fatalf("read unit %s: %v", key, err)
	}
	return got.Unit
}
