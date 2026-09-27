package coordtest

import (
	"bytes"
	"errors"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the promotion ledger ---------------------------------------------- //

func (h *fleetHarness) createPromotion(rec coord.PromotionRecord) (coord.PromotionRecord, bool) {
	h.t.Helper()
	stored, created, err := h.f.CreatePromotion(h.ctx, rec)
	if err != nil {
		h.t.Fatalf("CreatePromotion(%s/%s): %v", rec.Unit, rec.Fingerprint, err)
	}
	return stored, created
}

func (h *fleetHarness) updatePromotion(rec coord.PromotionRecord) (coord.PromotionRecord, bool) {
	h.t.Helper()
	stored, ok, err := h.f.UpdatePromotion(h.ctx, rec)
	if err != nil {
		h.t.Fatalf("UpdatePromotion(%s/%s): %v", rec.Unit, rec.Fingerprint, err)
	}
	return stored, ok
}

func (h *fleetHarness) promotions(unit string) []coord.PromotionRecord {
	h.t.Helper()
	got, err := h.f.Promotions(h.ctx, unit)
	if err != nil {
		h.t.Fatalf("Promotions(%s): %v", unit, err)
	}
	return got
}

// promotion is a record of one unit's convergence carrying value.
func promotion(unit, fingerprint, value string) coord.PromotionRecord {
	return coord.PromotionRecord{Unit: unit, Fingerprint: fingerprint, Value: []byte(value)}
}

var promotionCases = []fleetCase{{
	// The reason the ledger is in the coordination store: the promotion pass
	// is a singleton on a lease, and the node that holds it tomorrow must see
	// what today's holder drafted and what a lead rejected.
	name: "a record one node filed is listed under its unit for every node",
	fn: func(h *fleetHarness) {
		stored, created := h.createPromotion(promotion("Platform", "fp-1", `{"state":"drafted"}`))
		if !created {
			h.t.Fatal("the first filing lost")
		}
		if stored.Version == 0 {
			h.t.Fatal("the created record carries no version, so no write can be conditioned on it")
		}
		got := h.promotions("Platform")
		if len(got) != 1 {
			h.t.Fatalf("the unit lists %d record(s), want the 1 filed — a record nobody "+
				"can list is a rejection the next holder of the duty drafts again", len(got))
		}
		if got[0].Unit != "Platform" || got[0].Fingerprint != "fp-1" ||
			string(got[0].Value) != `{"state":"drafted"}` {
			h.t.Errorf("record = %+v, want the one filed", got[0])
		}
		if got[0].Version != stored.Version {
			h.t.Errorf("listed version %d, the create returned %d: a caller conditioning "+
				"on what it read would lose against itself", got[0].Version, stored.Version)
		}
	},
}, {
	// A record that exists may say a lead rejected the convergence, so a
	// second filing of the same address must not reset it to a draft.
	name: "a second filing does not overwrite a record",
	fn: func(h *fleetHarness) {
		stored, _ := h.createPromotion(promotion("Platform", "fp-1", "drafted"))
		stored.Value = []byte("rejected")
		if _, ok := h.updatePromotion(stored); !ok {
			h.t.Fatal("an update at the version the create returned was refused")
		}
		if _, created := h.createPromotion(promotion("Platform", "fp-1", "drafted")); created {
			h.t.Error("a second create reported itself as new")
		}
		if got := h.promotions("Platform"); len(got) != 1 || string(got[0].Value) != "rejected" {
			h.t.Errorf("records = %+v: a create wiped a rejection", got)
		}
	},
}, {
	// Two holders of a duty that moved mid-pass both write one record, and
	// the one that read it first must be told rather than allowed to write
	// over the other.
	name: "a stale version loses, and the version a write returns wins",
	fn: func(h *fleetHarness) {
		first, _ := h.createPromotion(promotion("Platform", "fp-1", "drafting"))
		drafted := first
		drafted.Value = []byte("drafted")
		second, ok := h.updatePromotion(drafted)
		if !ok {
			h.t.Fatal("an update at the version just read was refused")
		}
		if second.Version == first.Version {
			h.t.Fatal("the version did not move, so the next write cannot be conditioned")
		}
		stale := first
		stale.Value = []byte("drafting again")
		if _, ok := h.updatePromotion(stale); ok {
			h.t.Error("two writers both won one version")
		}
		rejected := second
		rejected.Value = []byte("rejected")
		if _, ok := h.updatePromotion(rejected); !ok {
			h.t.Error("an update at the version the previous write returned was refused")
		}
		if got := h.promotions("Platform"); len(got) != 1 || string(got[0].Value) != "rejected" {
			h.t.Errorf("records = %+v, want the last winning write", got)
		}
	},
}, {
	// A caller that never read a record has no version to offer, and a store
	// that read zero as "unconditional" would let it create one — or write
	// over a rejection it never saw.
	name: "a write carrying no version, or to a record that is not there, is a lost race",
	fn: func(h *fleetHarness) {
		if _, ok := h.updatePromotion(promotion("Platform", "fp-1", "drafted")); ok {
			h.t.Error("an update with no version created a record")
		}
		missing := promotion("Platform", "fp-2", "drafted")
		missing.Version = 1
		if _, ok := h.updatePromotion(missing); ok {
			h.t.Error("an update invented a record")
		}
		if got := h.promotions("Platform"); len(got) != 0 {
			h.t.Errorf("records = %+v: a lost race left a record behind", got)
		}
		held, _ := h.createPromotion(promotion("Platform", "fp-1", "rejected"))
		held.Version = 0
		held.Value = []byte("drafted")
		if _, ok := h.updatePromotion(held); ok {
			h.t.Error("an update with no version overwrote a record")
		}
		if got := h.promotions("Platform"); len(got) != 1 || string(got[0].Value) != "rejected" {
			h.t.Errorf("records = %+v, want the rejection untouched", got)
		}
	},
}, {
	// ONE UNIT'S LISTING IS THAT UNIT'S. A unit's name is prose, so one can
	// carry a dot, a space or another's name as its prefix, and a listing
	// that read by string prefix — or split a dotted name into two segments —
	// would hold one team's rejection against another team's work.
	name: "a listing reads its own unit and nothing else in the register",
	fn: func(h *fleetHarness) {
		for _, unit := range []string{"Platform", "Platform.Infra", "Plat", "Site Reliability"} {
			h.createPromotion(promotion(unit, "fp-1", unit))
		}
		// THE OTHER CLASSES OF THE POSITIONS REGISTER, which a KV backend
		// files in the same bucket: none of them is a promotion, and a
		// promotion is none of them.
		if err := h.f.PutPositions(h.ctx, coord.NodePositions{NodeID: "Platform"}); err != nil {
			h.t.Fatalf("PutPositions: %v", err)
		}
		for _, unit := range []string{"Platform", "Platform.Infra", "Plat", "Site Reliability"} {
			got := h.promotions(unit)
			if len(got) != 1 || got[0].Unit != unit || string(got[0].Value) != unit {
				h.t.Errorf("unit %q lists %+v, want its own one record", unit, got)
			}
		}
		rows, err := h.f.Positions(h.ctx)
		if err != nil {
			h.t.Fatalf("Positions: %v", err)
		}
		if len(rows) != 1 || rows[0].NodeID != "Platform" {
			h.t.Errorf("positions = %+v: a promotion record was read as a node's "+
				"positions, which the trim reads as a node that applied nothing", rows)
		}
	},
}, {
	// THE OPERATOR'S READ OF THE WHOLE LEDGER. A record filed under a unit
	// the company has since renamed is still in the ledger and still holds
	// its convergence, so a read that walked the units the company runs now
	// would hide exactly the record an operator is looking for — and a read
	// that took the register's other classes for records would show a
	// node's positions as a convergence.
	name: "the whole ledger lists every unit's records in order, and nothing else",
	fn: func(h *fleetHarness) {
		for _, rec := range []coord.PromotionRecord{
			promotion("Site Reliability", "fp-2", "b"),
			promotion("Platform", "fp-9", "c"),
			promotion("Site Reliability", "fp-1", "a"),
			promotion("Platform.Infra", "fp-1", "d"),
		} {
			h.createPromotion(rec)
		}
		if err := h.f.PutPositions(h.ctx, coord.NodePositions{NodeID: "Platform"}); err != nil {
			h.t.Fatalf("PutPositions: %v", err)
		}
		got, err := h.f.AllPromotions(h.ctx)
		if err != nil {
			h.t.Fatalf("AllPromotions: %v", err)
		}
		var listed []string
		for _, rec := range got {
			if rec.Version == 0 {
				h.t.Errorf("%s/%s is listed with no version, so it cannot be "+
					"cleared at the version read", rec.Unit, rec.Fingerprint)
			}
			listed = append(listed, rec.Unit+"/"+rec.Fingerprint+"="+string(rec.Value))
		}
		want := []string{"Platform/fp-9=c", "Platform.Infra/fp-1=d",
			"Site Reliability/fp-1=a", "Site Reliability/fp-2=b"}
		if !slices.Equal(listed, want) {
			h.t.Errorf("the ledger lists %v, want %v", listed, want)
		}
	},
}, {
	// A CLEAR IS CONDITIONED ON WHAT ITS CALLER READ. Between an operator's
	// read and their clear the pass may have recorded a lead's rejection,
	// and an unconditional delete would erase a decision nobody saw.
	name: "a record is deleted only at the version read, and can then be filed again",
	fn: func(h *fleetHarness) {
		first, _ := h.createPromotion(promotion("Platform", "fp-1", "drafted"))
		moved := first
		moved.Value = []byte("rejected")
		second, ok := h.updatePromotion(moved)
		if !ok {
			h.t.Fatal("an update at the version the create returned was refused")
		}
		for _, stale := range []uint64{0, first.Version} {
			deleted, err := h.f.DeletePromotion(h.ctx, "Platform", "fp-1", stale)
			if err != nil {
				h.t.Fatalf("DeletePromotion at %d: %v", stale, err)
			}
			if deleted {
				h.t.Errorf("a delete at version %d removed a record now at %d",
					stale, second.Version)
			}
		}
		if got := h.promotions("Platform"); len(got) != 1 || string(got[0].Value) != "rejected" {
			h.t.Fatalf("records = %+v, want the rejection untouched", got)
		}
		deleted, err := h.f.DeletePromotion(h.ctx, "Platform", "fp-1", second.Version)
		if err != nil || !deleted {
			h.t.Fatalf("a delete at the version read = %v, %v", deleted, err)
		}
		if got := h.promotions("Platform"); len(got) != 0 {
			h.t.Fatalf("records = %+v after the delete", got)
		}
		if deleted, err := h.f.DeletePromotion(h.ctx, "Platform", "fp-1", second.Version); err != nil || deleted {
			h.t.Errorf("a second delete of a gone record = %v, %v, want a lost race", deleted, err)
		}
		if _, created := h.createPromotion(promotion("Platform", "fp-1", "drafting")); !created {
			h.t.Error("an address whose record was deleted could not be filed again")
		}
		if _, err := h.f.DeletePromotion(h.ctx, "", "fp-1", 1); err == nil {
			h.t.Error("a delete naming no unit was accepted")
		}
	},
}, {
	name: "a unit with no records lists empty, and a listing naming no unit is refused",
	fn: func(h *fleetHarness) {
		if got := h.promotions("Nobody"); len(got) != 0 {
			h.t.Errorf("an empty unit listed %+v", got)
		}
		if _, err := h.f.Promotions(h.ctx, ""); err == nil {
			h.t.Error("a listing with no unit answered, which reads exactly as a unit " +
				"with no records")
		}
		if _, _, err := h.f.CreatePromotion(h.ctx, promotion("", "fp-1", "x")); err == nil {
			h.t.Error("a record with no unit was filed where no listing reads it back")
		}
		if _, _, err := h.f.CreatePromotion(h.ctx, promotion("Platform", "", "x")); err == nil {
			h.t.Error("a record with no fingerprint was filed where no listing reads it back")
		}
	},
}, {
	// A record is one message, so it is bounded by the transport — and the
	// twin must refuse at the same length the broker does, never store what
	// production could not.
	name: "a record at the ceiling is stored whole, and one past it is refused",
	fn: func(h *fleetHarness) {
		whole := bytes.Repeat([]byte("p"), coord.MaxRecordBytes)
		stored, created, err := h.f.CreatePromotion(h.ctx,
			coord.PromotionRecord{Unit: "Platform", Fingerprint: "fp-1", Value: whole})
		if err != nil || !created {
			h.t.Fatalf("a record of exactly coord.MaxRecordBytes = %v, %v", created, err)
		}
		if got := h.promotions("Platform"); len(got) != 1 || !bytes.Equal(got[0].Value, whole) {
			h.t.Fatal("the record at the ceiling did not come back whole")
		}
		refused := func(what string, err error) {
			h.t.Helper()
			switch {
			case err == nil:
				h.t.Errorf("%s one byte past coord.MaxRecordBytes was accepted", what)
			case !errors.Is(err, coord.ErrTooLarge) || errors.Is(err, coord.ErrUnavailable):
				h.t.Errorf("%s past the ceiling = %v, want coord.ErrTooLarge and not "+
					"coord.ErrUnavailable", what, err)
			}
		}
		stored.Value = append(whole, 'z')
		_, _, err = h.f.UpdatePromotion(h.ctx, stored)
		refused("an update", err)
		_, _, err = h.f.CreatePromotion(h.ctx,
			coord.PromotionRecord{Unit: "Platform", Fingerprint: "fp-2", Value: append(whole, 'z')})
		refused("a create", err)
		if got := h.promotions("Platform"); len(got) != 1 || !bytes.Equal(got[0].Value, whole) {
			h.t.Error("a refused write changed what the unit holds")
		}
	},
}}
