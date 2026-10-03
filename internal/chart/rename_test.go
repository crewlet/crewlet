package chart_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog"
)

// applySeatRekey is [writeRig.applyRekey] for a seat.
func (r *writeRig) applySeatRekey(opID, handle, former string) {
	r.t.Helper()
	if _, err := r.publishRename(opID, chart.KindSeat, former, handle); err != nil {
		r.t.Fatalf("rename %s to %s: %v", former, handle, err)
	}
	r.drain()
}

// publishRename publishes a one-operation rename batch without applying it.
func (r *writeRig) publishRename(opID string, kind chart.ObjectKind,
	former, to string) (chart.WriteResult, error) {

	return r.writer.WriteBatch(r.t.Context(), opID, chart.Batch{
		Operations: []chart.Operation{renameOp(kind, former, to)}})
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

	_, err := r.publishRename("op-clash", chart.KindSeat, "sarah-chen", "dana-okafor")
	if err == nil {
		t.Fatal("a claim on a handle another seat holds was accepted")
	}
	if !errors.Is(err, chart.ErrRefused) {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// A CREATE ORDERED AFTER A RENAME NEVER MOVES THE RENAMED OBJECT.
//
// A rename used to be a claim on the new address's own subject, which contends
// with other claims on that address and with nothing else — so a create of the
// same address, arbitrated on the tree, was decided against a snapshot that had
// not applied the claim, both were accepted, and the log applied the create
// second: it met the renamed unit on its address and moved it under the
// creator's parent, clearing its lead, while the create itself landed as
// nothing. A rename is structure now, on the tree's one subject, so the create
// cannot be published until this node has applied the rename — and once it has,
// the create is refused, naming the unit that holds the address.
func TestACreateOrderedAfterARenameNeverMovesTheRenamedObject(t *testing.T) {
	t.Parallel()
	for _, kind := range []chart.ObjectKind{chart.KindUnit, chart.KindSeat} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			r := newWriteRig(t)
			r.batch("op-seed",
				op(chart.OpCreateUnit, chart.KindUnit, "engineering", ""),
				op(chart.OpCreateUnit, chart.KindUnit, "product", ""),
				chart.Operation{Kind: createOf(kind),
					Object: chart.ObjectRef{Kind: kind, ID: "platform"},
					Parent: "engineering", Lead: leadFor(kind, "sarah-chen"),
					SeatKind: kindFor(kind)})

			// PUBLISHED, NOT APPLIED: this node's snapshot has no infra yet.
			if _, err := r.publishRename("op-rename", kind, "platform", "infra"); err != nil {
				t.Fatalf("publish the rename: %v", err)
			}
			_, err := r.writer.WriteBatch(t.Context(), "op-create", chart.Batch{
				Operations: []chart.Operation{op(createOf(kind), kind, "infra", "product")}})
			if !errors.Is(err, statelog.ErrUnavailable) {
				t.Fatalf("a create of the renamed address was decided against a "+
					"snapshot that had not applied the rename (%v) — the two "+
					"arbitrate on one subject, so it must wait for the rename", err)
			}

			r.drain()
			_, err = r.writer.WriteBatch(t.Context(), "op-create-again", chart.Batch{
				Operations: []chart.Operation{op(createOf(kind), kind, "infra", "product")}})
			if ref := refusal(t, err); ref.Rule != chart.RuleKeyTaken {
				t.Errorf("rule = %q, want %q", ref.Rule, chart.RuleKeyTaken)
			}
			parent, lead := r.structureOf(kind, "infra")
			if parent != "engineering" || lead != leadFor(kind, "sarah-chen") {
				t.Errorf("the renamed %s sits under %q led by %q, want engineering "+
					"and %q", kind, parent, lead, leadFor(kind, "sarah-chen"))
			}
		})
	}
}

// AND A RENAME ORDERED AFTER A CREATE OF ITS ADDRESS IS REFUSED, NOT DROPPED.
//
// The other order used to be accepted and then declined at the apply, so the
// caller was told their rename had landed and the unit went on answering to
// its old key. On one subject the rename is decided against the create.
func TestARenameOntoAnAddressACreateTookIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seed", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))
	if _, err := r.writer.WriteBatch(t.Context(), "op-create", chart.Batch{
		Operations: []chart.Operation{
			op(chart.OpCreateUnit, chart.KindUnit, "infra", ""),
		}}); err != nil {
		t.Fatalf("publish the create: %v", err)
	}
	r.drain()

	_, err := r.publishRename("op-rename", chart.KindUnit, "platform", "infra")
	if ref := refusal(t, err); ref.Rule != chart.RuleKeyTaken {
		t.Errorf("rule = %q, want %q", ref.Rule, chart.RuleKeyTaken)
	}
	if got := r.mustUnit("platform"); got.Key != "platform" {
		t.Errorf("platform = %q after the refused rename", got.Key)
	}
}

// createOf is the create operation for an object of kind.
func createOf(kind chart.ObjectKind) chart.OperationKind {
	if kind == chart.KindUnit {
		return chart.OpCreateUnit
	}
	return chart.OpCreateSeat
}

// kindFor is the seat kind a create of kind states: an agent's for a seat,
// nothing for a unit.
func kindFor(kind chart.ObjectKind) chart.SeatKind {
	if kind == chart.KindSeat {
		return chart.SeatAgent
	}
	return ""
}

// leadFor is lead where kind has one — a unit — and nothing for a seat.
func leadFor(kind chart.ObjectKind, lead string) string {
	if kind == chart.KindUnit {
		return lead
	}
	return ""
}

// structureOf reads an object's parent and, for a unit, its lead, from the
// columns and the document both.
func (r *writeRig) structureOf(kind chart.ObjectKind, key string) (parent, lead string) {
	r.t.Helper()
	if kind == chart.KindSeat {
		seat := r.mustSeat(key)
		return seat.UnitKey, ""
	}
	unit := r.mustUnit(key)
	return unit.ParentKey, unit.Lead
}

// A SEAT IS FOUND BY THE HANDLE IT WAS CREATED UNDER, and by nothing else.
//
// [chart.Reader.SeatByIdentity] is the read a binding resolves through — the
// seat a signed-in person is bound to, and every binding the dangling-binding
// alarm classifies — and a binding names the seat's IDENTITY (ADR-0027). So it
// must find the renamed seat by the handle it was created under, and must NOT
// answer for the seat's current handle: that is an address, and a lookup that
// treated one as an identity is how a binding came to name a stranger.
func TestASeatIsFoundByTheHandleItWasCreatedUnder(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""))
	r.applySeatRekey("op-rename", "sarah-okonkwo", "sarah-chen")
	reader := r.reader()

	got, err := reader.SeatByIdentity(t.Context(), "sarah-chen", session())
	if err != nil {
		t.Fatalf("SeatByIdentity(sarah-chen): %v", err)
	}
	if got.Handle != "sarah-okonkwo" {
		t.Errorf("the identity names %q, want the renamed seat", got.Handle)
	}
	// THE CONTROL: the same seat by its ADDRESS reads through the detail
	// read, so the refusal below is about identities and not about a seat
	// the rig failed to make.
	if detail, err := reader.Seat(t.Context(), "sarah-okonkwo", session()); err != nil ||
		detail.Seat.Handle != "sarah-okonkwo" {
		t.Fatalf("the renamed seat does not read by its handle: %+v, %v", detail, err)
	}
	if _, err := reader.SeatByIdentity(t.Context(), "sarah-okonkwo",
		session()); !errors.Is(err, chart.ErrNotFound) {
		t.Errorf("the seat's CURRENT handle answered as an identity (%v), so a "+
			"binding made against it would follow the address to whoever "+
			"holds it next", err)
	}
	if _, err := reader.SeatByIdentity(t.Context(), "nobody", session()); !errors.Is(err, chart.ErrNotFound) {
		t.Errorf("an identity nothing was created under read as %v, want ErrNotFound", err)
	}
}

// renameTimes renames one seat n times, from its handle through h1, h2, ….
func (r *writeRig) renameTimes(handle string, n int) string {
	r.t.Helper()
	current := handle
	for i := range n {
		next := fmt.Sprintf("%s-%d", handle, i+1)
		r.applySeatRekey(fmt.Sprintf("op-rename-%d", i+1), next, current)
		current = next
	}
	return current
}

// THE IDENTITY OUTLIVES THE ALIAS CAP.
//
// The retired-handle list is capped at [chart.MaxFormerKeys], and the handle a
// seat was created under used to fall off it: after one rename too many it
// stopped resolving, and a rename of ANOTHER seat onto it was accepted — a
// second seat answering to the address the first one's mailbox, diary and
// every person bound to it are keyed on. The origin is resolved for ever,
// which is what makes that rename a refusal however long ago it was retired.
func TestTheIdentityOutlivesTheAliasCap(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "dana-okafor", ""))
	current := r.renameTimes("sarah-chen", chart.MaxFormerKeys+1)

	// THE PRECONDITION, or nothing below is about the cap.
	if slices.Contains(r.mustSeat(current).FormerHandles, "sarah-chen") {
		t.Fatalf("the alias list still holds the origin after %d renames",
			chart.MaxFormerKeys+1)
	}
	if got := r.mustSeat("sarah-chen"); got.Handle != current {
		t.Errorf("the origin resolves to %q, want %q — an identity a cap can "+
			"drop is no identity", got.Handle, current)
	}
	_, err := r.publishRename("op-steal", chart.KindSeat, "dana-okafor", "sarah-chen")
	if !errors.Is(err, chart.ErrRefused) {
		t.Errorf("a second seat took the first one's identity (%v)", err)
	}
	// THE CONTROL: the seat itself may still go back to it.
	if _, err := r.publishRename("op-back", chart.KindSeat, current, "sarah-chen"); err != nil {
		t.Errorf("the seat could not take back the handle it was created "+
			"under: %v", err)
	}
}

// A CREATION NEVER TAKES ANOTHER SEAT'S IDENTITY, AND MAY TAKE A RETIRED ALIAS.
//
// A new seat's identity is the handle it is created under, so a seat created on
// a renamed seat's origin is a second seat with the first one's identity: one
// mailbox, one lease and one diary between two agents (ADR-0026), and every
// person bound to the first seat bound to the second (ADR-0027). All three
// creation paths refuse or skip it — a batch at its decide, a lone content
// write at its decide, and an import, which decides nothing, at its apply.
//
// A retired alias that is NOT an identity stays takeable: it carries no
// durable state, and the claimant-wins rule for references is the chart's own.
func TestACreationNeverTakesAnotherSeatsIdentity(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "founder", ""))
	r.applySeatRekey("op-rename-1", "dana-founder", "founder")
	r.applySeatRekey("op-rename-2", "dana", "dana-founder")

	_, err := r.writer.WriteBatch(t.Context(), "op-steal", chart.Batch{
		Operations: []chart.Operation{
			op(chart.OpCreateSeat, chart.KindSeat, "founder", ""),
		}})
	if ref := refusal(t, err); ref.Rule != chart.RuleKeyTaken ||
		!strings.Contains(ref.Detail, `"dana"`) {
		t.Errorf("a batch created a seat on another seat's identity: %+v", ref)
	}
	_, err = r.writer.WriteSeat(t.Context(), "op-steal-content", chart.SeatContent{
		Handle: "founder", Name: "A Stranger",
	})
	if !errors.Is(err, chart.ErrRefused) {
		t.Errorf("a content write created a seat on another seat's identity: %v", err)
	}
	r.mustImport("op-steal-import", "rev-1", chart.Edge{
		Object: chart.ObjectRef{Kind: chart.KindSeat, ID: "founder"},
		Kind:   chart.SeatHuman,
	})
	if got := r.mustSeat("founder"); got.Handle != "dana" {
		t.Errorf("an import placed a new seat on another seat's identity: "+
			"%q now answers to it", got.Handle)
	}

	// THE CONTROL: the retired alias that is nobody's identity is taken.
	r.batch("op-alias", op(chart.OpCreateSeat, chart.KindSeat, "dana-founder", ""))
	if got := r.mustSeat("dana-founder"); got.Handle != "dana-founder" ||
		got.Origin() != "dana-founder" {
		t.Errorf("the retired alias was not taken by the new seat: %+v", got)
	}
}

// A REMOVED SEAT'S IDENTITY IS NEVER ISSUED AGAIN, and neither is its address.
//
// The removal tombstones the handle the seat held; it now tombstones the one it
// was created under as well, because a creation onto that one would hand a
// stranger the removed seat's diary and every person the directory still binds
// to it. And a RENAME onto a removed address is refused: the tombstone that
// drops the removed seat's old records would drop every later record on the
// renamed seat's own subject too, leaving a seat nobody could edit.
func TestARemovedSeatsIdentityIsNeverIssuedAgain(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "omar", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "lena", ""))
	r.applySeatRekey("op-rename", "ops-head", "omar")
	if _, err := r.writer.WithHolders(noHolders{}).WriteRemoval(t.Context(),
		"op-remove", chart.Batch{Reason: "left", Operations: []chart.Operation{
			op(chart.OpRemoveObject, chart.KindSeat, "ops-head", ""),
		}}); err != nil {
		t.Fatalf("remove the seat: %v", err)
	}
	r.drain()
	reader := r.reader()

	for _, address := range []string{"ops-head", "omar"} {
		if _, found, _, err := reader.Removed(t.Context(), chart.ObjectRef{
			Kind: chart.KindSeat, ID: address}, session()); err != nil || !found {
			t.Errorf("%q carries no tombstone (%v, %v)", address, found, err)
		}
		_, err := r.writer.WriteBatch(t.Context(), "op-recreate-"+address,
			chart.Batch{Operations: []chart.Operation{
				op(chart.OpCreateSeat, chart.KindSeat, address, ""),
			}})
		if ref := refusal(t, err); ref.Rule != chart.RuleKeyRemoved {
			t.Errorf("a seat was created on the removed seat's %q: %+v", address, ref)
		}
		_, err = r.publishRename("op-take-"+address, chart.KindSeat, "lena", address)
		if !errors.Is(err, chart.ErrRefused) {
			t.Errorf("a seat was renamed onto the removed seat's %q: %v", address, err)
		}
	}
	// THE CONTROL: the survivor can still be renamed somewhere free.
	r.applySeatRekey("op-free", "lena-ops", "lena")
}

// A MANAGER GOES ON MANAGING THE SEAT IT NAMED, WHOEVER TAKES THE OLD HANDLE.
//
// A creation may take a renamed seat's retired handle. While a rename left the
// `manages:` entries naming the seat as typed, the entry reached the seat only
// through that alias — so the moment a new seat took it, the manager managed
// the newcomer instead, with nothing to say so.
func TestAManagerGoesOnManagingTheSeatItNamedWhoeverTakesTheOldHandle(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "ana", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "vee", ""))
	// A HANDLE THAT IS NOBODY'S IDENTITY, which is the one a creation may
	// take: the seat was created as ana and answers to ana-2 when vee's
	// entry is written.
	r.applySeatRekey("op-rename-1", "ana-2", "ana")
	r.batch("op-vee", managesOp("vee", "ana-2"))
	r.applySeatRekey("op-rename-2", "ana-lopez", "ana-2")
	r.batch("op-newcomer", op(chart.OpCreateSeat, chart.KindSeat, "ana-2", ""))

	got, err := r.reader().Seat(t.Context(), "vee", session())
	if err != nil {
		t.Fatalf("read vee: %v", err)
	}
	if !slices.Equal(got.Manages, []string{"ana-lopez"}) {
		t.Errorf("vee manages %v, want [ana-lopez] — the seat the entry named, "+
			"not the newcomer who took its old handle", got.Manages)
	}
}

// A BATCH THAT RENAMES AN OBJECT AND THEN REMOVES IT REMOVES THE OBJECT.
//
// The removal is a record of its own, published without the batch's rename,
// so it applies against the rows as the batch found them — where the object
// still answers to its old address. It named the object by the address the
// rename moved it onto, so the apply tombstoned an address that had never held
// anything, deleted nothing and wrote no identity tombstone, and the caller was
// told the removal landed: a seat meant to be gone went on running with its
// credentials, and the new address was burned for ever. Measured before the
// fix: the rows still held the object and `chart_removed` held only the new
// address.
func TestARenameThenRemovalRemovesTheObjectAndNotItsNewAddress(t *testing.T) {
	t.Parallel()
	for _, kind := range []chart.ObjectKind{chart.KindUnit, chart.KindSeat} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			r := newWriteRig(t)
			r.batch("op-create", chart.Operation{Kind: createOf(kind),
				Object:   chart.ObjectRef{Kind: kind, ID: "platform"},
				SeatKind: kindFor(kind)})
			// AN EARLIER RENAME ON THE LOG, so the object's identity is
			// not the address it is removed from and the identity
			// tombstone is a separate row to look for.
			if _, err := r.publishRename("op-rename", kind, "platform",
				"platform-team"); err != nil {
				t.Fatalf("rename: %v", err)
			}
			r.drain()

			got, err := r.writer.WithHolders(noHolders{}).WriteBatch(t.Context(),
				"op-gone", chart.Batch{Operations: []chart.Operation{
					renameOp(kind, "platform-team", "infra"),
					op(chart.OpRemoveObject, kind, "infra", ""),
				}})
			if err != nil {
				t.Fatalf("write the batch: %v", err)
			}
			want := []chart.ObjectRef{{Kind: kind, ID: "platform-team"}}
			if !slices.Equal(got.Objects, want) {
				t.Errorf("the removal names %v, want %v — the address the "+
					"rows hold the object at", got.Objects, want)
			}
			r.drain()

			table := map[chart.ObjectKind]string{
				chart.KindUnit: `SELECT key FROM chart_units`,
				chart.KindSeat: `SELECT handle FROM chart_seats`,
			}[kind]
			if rows := r.column(table); len(rows) != 0 {
				t.Errorf("the chart still holds %v after its removal landed", rows)
			}
			if got := r.column(`SELECT object_id FROM chart_removed
				WHERE object_kind = ? ORDER BY object_id`, string(kind)); !slices.Equal(
				got, []string{"platform", "platform-team"}) {
				t.Errorf("the tombstones are %v, want the address the object "+
					"held and its identity — and never infra, which held "+
					"nothing", got)
			}

			// AND THE ADDRESS THE BATCH NAMED IS NOT BURNED: nothing ever
			// answered to it, so a creation takes it.
			r.batch("op-reuse", chart.Operation{Kind: createOf(kind),
				Object:   chart.ObjectRef{Kind: kind, ID: "infra"},
				SeatKind: kindFor(kind)})
			if got := r.column(table); !slices.Equal(got, []string{"infra"}) {
				t.Errorf("after creating infra the chart holds %v", got)
			}
		})
	}
}

// A REMOVAL ASKS THE DIRECTORY ABOUT THE SEAT BY ITS IDENTITY.
//
// A binding names the seat it was made to by the handle the seat was created
// under (ADR-0027), so a removal that asked about the handle the seat answers
// to now read a renamed seat as held by nobody — and removed it out from under
// the person holding it, which is the one mistake the check exists to catch.
func TestARemovalAsksTheDirectoryByTheSeatsIdentity(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "omar", ""))
	r.applySeatRekey("op-rename", "ops-head", "omar")

	_, err := r.writer.WithHolders(heldBy{handle: "omar", login: "omar.haddad"}).
		WriteRemoval(t.Context(), "op-remove", chart.Batch{
			Operations: []chart.Operation{
				op(chart.OpRemoveObject, chart.KindSeat, "ops-head", ""),
			}})
	if ref := refusal(t, err); ref.Rule != chart.RuleSeatHeld ||
		!strings.Contains(ref.Detail, "omar.haddad") {
		t.Errorf("a renamed seat somebody holds was removed: %+v", ref)
	}
}
