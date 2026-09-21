package coordtest

import (
	"cmp"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// seatID is the id of the seat a case calls name.
//
// The registry keys on a seat's ID rather than its handle, so a case that
// wants "the swe's mailbox" needs a uuid it can write twice and read back the
// same. The engine's own derivation is not reachable from here, and a suite
// that imported the org model to build a coordination key would be certifying
// the org model — so this mints its own stable v5: what a case needs is a
// value that is a uuid and is the same on every run.
func seatID(name string) uuid.UUID {
	return uuid.NewSHA1(uuid.MustParse("6f1c2a4e-9d3b-4a71-8f5e-2b0c7d81a940"),
		[]byte(name))
}

// seat is one seat's record as a case names it: the id it is filed under and
// the handle a person reads.
func seat(handle string) coord.MailboxRecord {
	return coord.MailboxRecord{Seat: seatID(handle), Handle: handle}
}

// withVersion and withAbsent set one field on a record a case built with
// [seat], so the id and the handle stay paired in one place.
func withVersion(rec coord.MailboxRecord, version uint64) coord.MailboxRecord {
	rec.Version = version
	return rec
}

func withAbsent(rec coord.MailboxRecord, at time.Time) coord.MailboxRecord {
	rec.AbsentSince = at
	return rec
}

// ---- the seat mailbox registry ----------------------------------------- //

func (h *fleetHarness) createMailbox(rec coord.MailboxRecord) (coord.MailboxRecord, bool) {
	h.t.Helper()
	stored, created, err := h.f.CreateMailbox(h.ctx, rec)
	if err != nil {
		h.t.Fatalf("CreateMailbox(%s): %v", rec.Handle, err)
	}
	return stored, created
}

func (h *fleetHarness) mailbox(handle string) (coord.MailboxRecord, bool) {
	h.t.Helper()
	rec, found, err := h.f.Mailbox(h.ctx, seatID(handle))
	if err != nil {
		h.t.Fatalf("Mailbox(%s): %v", handle, err)
	}
	return rec, found
}

func (h *fleetHarness) updateMailbox(rec coord.MailboxRecord) (coord.MailboxRecord, bool) {
	h.t.Helper()
	stored, ok, err := h.f.UpdateMailbox(h.ctx, rec)
	if err != nil {
		h.t.Fatalf("UpdateMailbox(%s): %v", rec.Handle, err)
	}
	return stored, ok
}

func (h *fleetHarness) deleteMailbox(handle string, version uint64) bool {
	h.t.Helper()
	gone, err := h.f.DeleteMailbox(h.ctx, seatID(handle), version)
	if err != nil {
		h.t.Fatalf("DeleteMailbox(%s): %v", handle, err)
	}
	return gone
}

var mailboxCases = []fleetCase{{
	// The reason the registry is in the coordination store. Every node
	// records the seats of the revision it applied, and the node whose sweep
	// later retires a removed seat's mailbox is usually not the node that
	// created it.
	name: "a mailbox one node registered is readable by every node",
	fn: func(h *fleetHarness) {
		stored, created := h.createMailbox(seat("swe"))
		if !created {
			h.t.Fatal("the first registration lost")
		}
		if stored.Version == 0 {
			h.t.Fatal("the created record carries no version, so no write can be conditioned on it")
		}
		read, found := h.mailbox("swe")
		if !found {
			h.t.Fatal("a registered mailbox is invisible, so a removed seat's mailbox is never retired")
		}
		if read.Handle != "swe" || !read.Present() || read.Retiring() {
			h.t.Errorf("record = %+v, want a present seat named swe", read)
		}
		if read.Version != stored.Version {
			h.t.Errorf("read version %d, the create returned %d: a caller conditioning on "+
				"what it wrote would lose against itself", read.Version, stored.Version)
		}
	},
}, {
	// A record that exists may be mid-way through a retirement, so a second
	// registration must not reset it. Only a conditional update may.
	name: "a second registration does not overwrite a record",
	fn: func(h *fleetHarness) {
		stored, _ := h.createMailbox(seat("swe"))
		stored.AbsentSince = h.now()
		stored.RetiringSince = h.now().Add(time.Minute)
		if _, ok := h.updateMailbox(stored); !ok {
			h.t.Fatal("an update at the version the create returned was refused")
		}
		if _, created := h.createMailbox(seat("swe")); created {
			h.t.Error("a second create reported itself as new")
		}
		read, _ := h.mailbox("swe")
		if !read.Retiring() {
			h.t.Errorf("record = %+v: a create wiped a retirement in flight", read)
		}
	},
}, {
	// The whole concurrency story. A returning seat's registration and a
	// sweep that read the record a moment earlier both write it, and the
	// loser must be told rather than allowed to overwrite.
	name: "a stale version loses, and the version a write returns wins",
	fn: func(h *fleetHarness) {
		first, _ := h.createMailbox(seat("swe"))

		marked := first
		marked.AbsentSince = h.now()
		second, ok := h.updateMailbox(marked)
		if !ok {
			h.t.Fatal("an update at the version just read was refused")
		}
		if second.Version == first.Version {
			h.t.Fatal("the version did not move, so the next write cannot be conditioned")
		}
		// The sweep that read the first version and is about to act on it.
		stale := first
		stale.RetiringSince = h.now()
		if _, ok := h.updateMailbox(stale); ok {
			h.t.Error("two writers both won one version: a sweep overwrote a newer record")
		}
		cleared := second
		cleared.AbsentSince = time.Time{}
		if _, ok := h.updateMailbox(cleared); !ok {
			h.t.Error("an update at the version the previous write returned was refused")
		}
		read, _ := h.mailbox("swe")
		if !read.Present() {
			h.t.Errorf("record = %+v, want the last winning write", read)
		}
	},
}, {
	name: "an update to a record that is gone is a lost race, not an error",
	fn: func(h *fleetHarness) {
		if _, ok := h.updateMailbox(withVersion(seat("missing"), 1)); ok {
			h.t.Error("an update invented a record")
		}
		if _, found := h.mailbox("missing"); found {
			h.t.Error("Mailbox invented a record")
		}
	},
}, {
	// A caller that never read a record has no version to offer, and a store
	// that read zero as "unconditional" would let it create a record, or
	// delete one mid-retirement, where it must lose.
	name: "a write carrying no version is a lost race",
	fn: func(h *fleetHarness) {
		if _, ok := h.updateMailbox(seat("swe")); ok {
			h.t.Error("an update with no version created a record")
		}
		if _, found := h.mailbox("swe"); found {
			h.t.Error("an update with no version left a record behind")
		}
		h.createMailbox(seat("swe"))
		if h.deleteMailbox("swe", 0) {
			h.t.Error("a delete with no version removed a record")
		}
		if _, found := h.mailbox("swe"); !found {
			h.t.Error("a delete with no version removed a record")
		}
	},
}, {
	// A retirement that lost its race must not take the record a returning
	// seat just rewrote with it.
	name: "a delete is conditional, and a deleted handle can be registered again",
	fn: func(h *fleetHarness) {
		stored, _ := h.createMailbox(seat("swe"))
		if h.deleteMailbox("swe", stored.Version+1) {
			h.t.Error("a delete at a version that never existed reported success")
		}
		if !h.deleteMailbox("swe", stored.Version) {
			h.t.Fatal("a delete at the version just read was refused")
		}
		if _, found := h.mailbox("swe"); found {
			h.t.Fatal("the record survived its delete")
		}
		if h.deleteMailbox("swe", stored.Version) {
			h.t.Error("a second delete of one version reported success")
		}
		// A seat added again under the handle of a retired one.
		again, created := h.createMailbox(seat("swe"))
		if !created {
			h.t.Fatal("a handle whose record was deleted cannot be registered again")
		}
		// A version from the deleted record must not act on its successor:
		// a sweep still holding it would retire the returning seat's
		// mailbox.
		if again.Version == stored.Version {
			h.t.Errorf("the new record reused version %d of the one deleted before it", again.Version)
		}
		if h.deleteMailbox("swe", stored.Version) {
			h.t.Error("a version from the deleted record removed its successor")
		}
		if _, found := h.mailbox("swe"); !found {
			h.t.Error("the successor record was removed by its predecessor's version")
		}
	},
}, {
	// ORDERED BY THE KEY, which is the seat's id: what a sweep needs is that
	// two backends hand it the same list in the same order, and the id is
	// the only thing on the record that both of them file it under. Ordering
	// by the HANDLE would be ordering by a label that is allowed to be
	// stale, so two nodes could disagree about it for one record.
	name: "every record is listed, in one order both backends agree on",
	fn: func(h *fleetHarness) {
		handles := []string{"swe", "ceo", "pm"}
		for _, handle := range handles {
			h.createMailbox(seat(handle))
		}
		records, err := h.f.Mailboxes(h.ctx)
		if err != nil {
			h.t.Fatalf("Mailboxes: %v", err)
		}
		var got []string
		for _, rec := range records {
			got = append(got, rec.Handle)
			if rec.Version == 0 {
				h.t.Errorf("listed record %s carries no version", rec.Handle)
			}
			if rec.Seat != seatID(rec.Handle) {
				h.t.Errorf("record %s is filed under %s, want the seat's own id",
					rec.Handle, rec.Seat)
			}
		}
		want := slices.Clone(handles)
		slices.SortFunc(want, func(a, b string) int {
			return cmp.Compare(seatID(a).String(), seatID(b).String())
		})
		if !slices.Equal(got, want) {
			h.t.Errorf("records = %v, want %v — every one, in seat-id order", got, want)
		}
	},
}, {
	// The stamps are what the sweep measures a grace period against, so a
	// backend that shifted a zone or dropped precision would retire early or
	// late depending on which backend answered.
	name: "the stamps round-trip in UTC, and a zero stamp stays zero",
	fn: func(h *fleetHarness) {
		zone := time.FixedZone("UTC+5", 5*60*60)
		absent := h.now().Add(123456789 * time.Nanosecond).In(zone)
		stored, _ := h.createMailbox(withAbsent(seat("swe"), absent))
		read, _ := h.mailbox("swe")
		for name, rec := range map[string]coord.MailboxRecord{"create": stored, "read": read} {
			if !rec.AbsentSince.Equal(absent) {
				h.t.Errorf("%s: AbsentSince = %v, want %v", name, rec.AbsentSince, absent)
			}
			if rec.AbsentSince.Location() != time.UTC {
				h.t.Errorf("%s: AbsentSince is in %v, want UTC", name, rec.AbsentSince.Location())
			}
			if !rec.RetiringSince.IsZero() {
				h.t.Errorf("%s: an unset RetiringSince came back as %v", name, rec.RetiringSince)
			}
		}
	},
}, {
	name: "an unidentified record is an error, not a lost race",
	fn: func(h *fleetHarness) {
		if _, created, err := h.f.CreateMailbox(h.ctx, coord.MailboxRecord{}); err == nil || created {
			h.t.Errorf("CreateMailbox with no seat id = (%v, %v), want an error", created, err)
		}
		if _, ok, err := h.f.UpdateMailbox(h.ctx, coord.MailboxRecord{Version: 1}); err == nil || ok {
			h.t.Errorf("UpdateMailbox with no seat id = (%v, %v), want an error", ok, err)
		}
		if gone, err := h.f.DeleteMailbox(h.ctx, uuid.Nil, 1); err == nil || gone {
			h.t.Errorf("DeleteMailbox with no seat id = (%v, %v), want an error", gone, err)
		}
		if _, found, err := h.f.Mailbox(h.ctx, uuid.Nil); err == nil || found {
			h.t.Errorf("Mailbox with no seat id = (%v, %v), want an error", found, err)
		}
		// A HANDLE IS NOT AN IDENTITY HERE: a record naming one and no
		// seat is refused, because the key is what the mailbox subject is
		// built from and a handle cannot build one.
		if _, created, err := h.f.CreateMailbox(h.ctx,
			coord.MailboxRecord{Handle: "swe"}); err == nil || created {

			h.t.Errorf("CreateMailbox with a handle and no seat id = (%v, %v), "+
				"want an error", created, err)
		}
	},
}}
