package coordtest

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

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
	rec, found, err := h.f.Mailbox(h.ctx, handle)
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
	gone, err := h.f.DeleteMailbox(h.ctx, handle, version)
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
		stored, created := h.createMailbox(coord.MailboxRecord{Handle: "swe"})
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
		stored, _ := h.createMailbox(coord.MailboxRecord{Handle: "swe"})
		stored.AbsentSince = h.now()
		stored.RetiringSince = h.now().Add(time.Minute)
		if _, ok := h.updateMailbox(stored); !ok {
			h.t.Fatal("an update at the version the create returned was refused")
		}
		if _, created := h.createMailbox(coord.MailboxRecord{Handle: "swe"}); created {
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
		first, _ := h.createMailbox(coord.MailboxRecord{Handle: "swe"})

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
		if _, ok := h.updateMailbox(coord.MailboxRecord{Handle: "missing", Version: 1}); ok {
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
		if _, ok := h.updateMailbox(coord.MailboxRecord{Handle: "swe"}); ok {
			h.t.Error("an update with no version created a record")
		}
		if _, found := h.mailbox("swe"); found {
			h.t.Error("an update with no version left a record behind")
		}
		h.createMailbox(coord.MailboxRecord{Handle: "swe"})
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
		stored, _ := h.createMailbox(coord.MailboxRecord{Handle: "swe"})
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
		again, created := h.createMailbox(coord.MailboxRecord{Handle: "swe"})
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
	name: "every record is listed, by handle",
	fn: func(h *fleetHarness) {
		for _, handle := range []string{"swe", "ceo", "pm"} {
			h.createMailbox(coord.MailboxRecord{Handle: handle})
		}
		records, err := h.f.Mailboxes(h.ctx)
		if err != nil {
			h.t.Fatalf("Mailboxes: %v", err)
		}
		var handles []string
		for _, rec := range records {
			handles = append(handles, rec.Handle)
			if rec.Version == 0 {
				h.t.Errorf("listed record %s carries no version", rec.Handle)
			}
		}
		if !slices.Equal(handles, []string{"ceo", "pm", "swe"}) {
			h.t.Errorf("records = %v, want every one in handle order", handles)
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
		stored, _ := h.createMailbox(coord.MailboxRecord{Handle: "swe", AbsentSince: absent})
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
	name: "an unnamed record is an error, not a lost race",
	fn: func(h *fleetHarness) {
		if _, created, err := h.f.CreateMailbox(h.ctx, coord.MailboxRecord{}); err == nil || created {
			h.t.Errorf("CreateMailbox with no handle = (%v, %v), want an error", created, err)
		}
		if _, ok, err := h.f.UpdateMailbox(h.ctx, coord.MailboxRecord{Version: 1}); err == nil || ok {
			h.t.Errorf("UpdateMailbox with no handle = (%v, %v), want an error", ok, err)
		}
		if gone, err := h.f.DeleteMailbox(h.ctx, "", 1); err == nil || gone {
			h.t.Errorf("DeleteMailbox with no handle = (%v, %v), want an error", gone, err)
		}
		if _, found, err := h.f.Mailbox(h.ctx, ""); err == nil || found {
			h.t.Errorf("Mailbox with no handle = (%v, %v), want an error", found, err)
		}
	},
}}
