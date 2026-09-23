package chart_test

import (
	"errors"
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

// A CLAIM ON AN ADDRESS SOMEBODY ELSE HOLDS IS REFUSED, NOT STALLED.
//
// The key is the table's PRIMARY KEY, so a claim that reached the apply
// against a row holding it raised `UNIQUE constraint failed: chart_units.key`
// — and an apply error is not one node's problem. Every node reads the same
// record, fails the same way and can never get past it, so one rename onto a
// taken address took the chart domain down across the fleet. Measured before
// this was checked: the write was accepted and the drain died on record 3.
func TestAClaimOnALiveAddressIsRefusedAtTheWrite(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-a", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))
	r.batch("op-b", op(chart.OpCreateUnit, chart.KindUnit, "infra", ""))

	_, err := r.applyRekey("op-rename", "infra", "platform")
	if err == nil {
		t.Fatal("a claim on a key another unit holds was accepted; its apply " +
			"raises on the primary key, on every node, for ever")
	}
	if !errors.Is(err, chart.ErrRefused) {
		t.Errorf("err = %v, want a refusal a caller can tell from a fault", err)
	}
	// AND BOTH UNITS ARE EXACTLY AS THEY WERE.
	if got := r.mustUnit("platform"); got.Key != "platform" {
		t.Errorf("platform = %q after the refused claim", got.Key)
	}
	if got := r.mustUnit("infra"); got.Key != "infra" {
		t.Errorf("infra = %q after the refused claim", got.Key)
	}
}

// AND A RETIRED ONE IS HELD TOO, UNTIL IT IS RELEASED.
//
// A former key goes on resolving, which is the whole of what the alias list
// buys: a `manages:` entry somebody wrote before the rename still reaches the
// unit. Letting a second object claim that address would re-point every one of
// those references, silently, at a unit that never had them — so the address
// is held until the alias falls off the end of [chart.MaxFormerKeys] or the
// object holding it goes.
func TestAFormerKeyIsRefusedForAnotherUnitUntilItIsReleased(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-a", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))
	r.batch("op-b", op(chart.OpCreateUnit, chart.KindUnit, "design", ""))
	if _, err := r.applyRekey("op-rename", "infrastructure", "platform"); err != nil {
		t.Fatalf("rename platform to infrastructure: %v", err)
	}

	// `platform` is nobody's live key now, and it still answers.
	if got := r.mustUnit("platform"); got.Key != "infrastructure" {
		t.Fatalf("the retired key resolves to %q, want the renamed unit", got.Key)
	}
	_, err := r.applyRekey("op-steal", "platform", "design")
	if err == nil {
		t.Fatal("a second unit took an address another one still answers to, " +
			"so every reference written before the rename now reaches a unit " +
			"that never had them")
	}
	if !errors.Is(err, chart.ErrRefused) {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// AND RENAMING BACK IS NOT A COLLISION WITH ITSELF.
//
// The control for both cases above: an object claiming an address IT used to
// answer to is claiming something that already resolves to it. A check that
// asked only "is this address held" would refuse exactly this, which is the
// one rename an operator is most likely to make — the undo.
func TestAUnitCanTakeBackAnAddressItUsedToAnswerTo(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-a", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))
	if _, err := r.applyRekey("op-rename", "infrastructure", "platform"); err != nil {
		t.Fatalf("rename platform to infrastructure: %v", err)
	}
	if _, err := r.applyRekey("op-undo", "platform", "infrastructure"); err != nil {
		t.Fatalf("rename infrastructure back to platform: %v — an object "+
			"cannot collide with its own retired address", err)
	}
	got := r.mustUnit("platform")
	if got.Key != "platform" {
		t.Errorf("key = %q, want the address it took back", got.Key)
	}
	// AND THE ORIGIN IS STILL THE ONE IT WAS CREATED UNDER, which the undo
	// must not disturb: it was frozen by the first rename and never moves.
	if got.OriginKey != "platform" {
		t.Errorf("origin = %q, want the key the unit was created under", got.OriginKey)
	}
}

// A SEAT'S HANDLE IS HELD ON THE SAME TERMS, and it has further to fall.
//
// chart_seats.handle is a primary key too, so the stall is identical — and a
// seat also carries the identity every durable thing it owns is keyed on, so
// a claim that landed would put two seats' rows in one row's place.
func TestAClaimOnALiveHandleIsRefusedAtTheWrite(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "dana-okafor", ""))

	_, err := r.writer.WriteRekey(t.Context(), "op-clash",
		chart.ObjectRef{Kind: chart.KindSeat, ID: "dana-okafor"}, "sarah-chen")
	if err == nil {
		t.Fatal("a claim on a handle another seat holds was accepted")
	}
	if !errors.Is(err, chart.ErrRefused) {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// AND THE ONE THE DECIDE CANNOT SEE IS DECLINED BY THE APPLY.
//
// A claim arbitrates on the ADDRESS's subject and a create on the STRUCTURE's,
// so the two never contend at the broker: a claim decided while an address was
// free can be applied after a create that took it. That ordering is legal and
// there is no way to make it not be — which is exactly why the apply asks
// again rather than trusting the decide.
//
// What it must NOT do is raise. The key is a primary key, so the UPDATE would
// fail the constraint on every node, identically, on a record none of them can
// ever get past — one lost rename becoming a stalled domain across the fleet.
// So the claim is dropped and everything else on the log goes on applying,
// which is what this asserts: the record AFTER the collision lands.
func TestAClaimThatLostTheRaceIsDroppedRatherThanStallingTheDomain(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-a", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))

	// PUBLISHED, NOT APPLIED: the estate still has no `infra`, so the claim
	// below is decided against a snapshot in which the address is free.
	if _, err := r.writer.WriteBatch(t.Context(), "op-b", chart.Batch{
		Operations: []chart.Operation{
			op(chart.OpCreateUnit, chart.KindUnit, "infra", ""),
		}}); err != nil {
		t.Fatalf("publish the create: %v", err)
	}
	if _, err := r.writer.WriteRekey(t.Context(), "op-rename",
		chart.ObjectRef{Kind: chart.KindUnit, ID: "infra"}, "platform"); err != nil {
		t.Fatalf("the claim was refused against a snapshot with no infra: %v", err)
	}
	// THE DRAIN ITSELF IS THE ASSERTION: it applies every record in turn and
	// fails the test on the first that raises, which is precisely the state
	// this guards against. Nothing can be queued BEHIND the claim first — a
	// second structural write is refused until this node has applied the one
	// ahead of it, which is the write authority doing its job.
	r.drain()

	if got := r.mustUnit("platform"); got.Key != "platform" {
		t.Errorf("platform = %q, want the claim dropped and the unit as it was",
			got.Key)
	}
	if got := r.mustUnit("infra"); got.Key != "infra" {
		t.Errorf("infra = %q, want the unit that won the address", got.Key)
	}
	// AND THE DOMAIN TOOK THE NEXT RECORD, which is what tells a dropped
	// claim from a stopped applier: a stalled domain cannot apply anything
	// again, ever, on any node.
	r.batch("op-c", op(chart.OpCreateUnit, chart.KindUnit, "design", ""))
	if got := r.mustUnit("design"); got.Key != "design" {
		t.Errorf("design = %q — the record behind the collision never landed, "+
			"which is the domain stalling rather than one rename being lost",
			got.Key)
	}
}

// THE ROW READ AND THE DETAIL READ NAME ONE SEAT, through a rename.
//
// [chart.Reader.SeatRow] is the per-request read — the seat a signed-in person
// is bound to, and every binding the dangling-binding alarm classifies — and it
// exists only so those paths stop reading a manages list and a history nobody
// renders. It must resolve an address exactly as [chart.Reader.Seat] does, or a
// person's sign-in and the page describing their seat would disagree about which
// seat a retired handle names.
func TestTheSeatRowResolvesAnAddressAsTheSeatReadDoes(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))
	r.applySeatRekey("op-rename", "sarah-okonkwo", "sarah-chen")
	reader := r.reader()

	for _, address := range []string{"sarah-okonkwo", "sarah-chen"} {
		row, err := reader.SeatRow(t.Context(), address, session())
		if err != nil {
			t.Fatalf("SeatRow(%q): %v", address, err)
		}
		detail, err := reader.Seat(t.Context(), address, session())
		if err != nil {
			t.Fatalf("Seat(%q): %v", address, err)
		}
		if row.Handle != "sarah-okonkwo" || row.Handle != detail.Seat.Handle {
			t.Errorf("%q names %q by its row and %q by its detail, want the "+
				"renamed seat both ways", address, row.Handle, detail.Seat.Handle)
		}
	}
	if _, err := reader.SeatRow(t.Context(), "nobody", session()); !errors.Is(err, chart.ErrNotFound) {
		t.Errorf("an address nothing answers to read as %v, want ErrNotFound", err)
	}
}
