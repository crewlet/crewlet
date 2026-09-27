package kv

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the promotion ledger ---------------------------------------------- //
//
// IN THE POSITIONS REGISTER, under a key class of its own, because that is
// the one bucket with no age and a promotion record must not have one: see
// [coord.Promotions]. Every listing over the register filters on its own
// class, which is what lets this class share it.

// Promotions returns every record filed under one unit, ordered by
// fingerprint.
//
// ONE PASS through [FleetStore.eachUnder], narrowed by the broker to the one
// unit's keys, and never the client's key lister: that one stops on the nil a
// closed subscription yields, so a listing cut off part way comes back short
// with no error — and a short listing here is a rejected convergence the pass
// drafts again.
func (f *FleetStore) Promotions(ctx context.Context, unit string) ([]coord.PromotionRecord, error) {
	if unit == "" {
		return nil, errors.New("coord/kv: a promotion listing names the unit it reads")
	}
	out := []coord.PromotionRecord{}
	err := f.eachUnder(ctx, f.positions, coord.PromotionsFilter(unit),
		"the promotion records of unit "+unit,
		func(kve jetstream.KeyValueEntry) error {
			held, fingerprint, ok := coord.PromotionOf(kve.Key())
			if !ok || held != unit {
				// A key this grammar did not write, or a filter and a
				// grammar that have drifted apart. Skipped rather than
				// guessed at: a record filed under the wrong unit would
				// hold one team's rejection against another's work.
				return nil
			}
			out = append(out, coord.PromotionRecord{
				Unit: held, Fingerprint: fingerprint,
				Value: bytes.Clone(kve.Value()), Version: kve.Revision(),
			})
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.PromotionRecord) int {
		return cmp.Compare(a.Fingerprint, b.Fingerprint)
	})
	return out, nil
}

// AllPromotions returns every record of every unit, ordered by unit and then
// by fingerprint.
//
// ONE PASS under the class's own filter, for the reason [FleetStore.Promotions]
// gives: a listing cut off part way must fail rather than come back short.
func (f *FleetStore) AllPromotions(ctx context.Context) ([]coord.PromotionRecord, error) {
	out := []coord.PromotionRecord{}
	err := f.eachUnder(ctx, f.positions, coord.AllPromotionsFilter(),
		"the promotion records",
		func(kve jetstream.KeyValueEntry) error {
			unit, fingerprint, ok := coord.PromotionOf(kve.Key())
			if !ok {
				// A key this grammar did not write: skipped rather than
				// guessed at, as the one-unit listing skips it.
				return nil
			}
			out = append(out, coord.PromotionRecord{
				Unit: unit, Fingerprint: fingerprint,
				Value: bytes.Clone(kve.Value()), Version: kve.Revision(),
			})
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.PromotionRecord) int {
		return cmp.Or(cmp.Compare(a.Unit, b.Unit), cmp.Compare(a.Fingerprint, b.Fingerprint))
	})
	return out, nil
}

// DeletePromotion removes a record at the version it was read at.
//
// A PURGE rather than a delete marker, so the address holds nothing a listing
// has to step over and a later create files it afresh.
func (f *FleetStore) DeletePromotion(ctx context.Context, unit, fingerprint string, version uint64) (bool, error) {
	if err := (coord.PromotionRecord{Unit: unit, Fingerprint: fingerprint}).Validate(); err != nil {
		return false, err
	}
	if version == 0 {
		// The client drops a LastRevision of 0 and purges unconditionally,
		// so a caller that never read a version would delete whatever is
		// there — a lead's rejection included.
		return false, nil
	}
	err := f.positions.Purge(ctx, coord.PromotionKey(unit, fingerprint),
		jetstream.LastRevision(version))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
		return false, nil
	default:
		return false, unavailable("delete a promotion record", err)
	}
}

// CreatePromotion files a new record, leaving an existing one alone.
//
// Create rather than Put, so the first writer wins and every other is told:
// a duty that moved mid-pass can leave two holders drafting the same
// convergence, and this is what makes one of them stop.
func (f *FleetStore) CreatePromotion(ctx context.Context, rec coord.PromotionRecord) (coord.PromotionRecord, bool, error) {
	if err := rec.Validate(); err != nil {
		return coord.PromotionRecord{}, false, err
	}
	if err := withinCeiling("a promotion record", rec.Value); err != nil {
		return coord.PromotionRecord{}, false, err
	}
	revision, err := f.positions.Create(ctx, coord.PromotionKey(rec.Unit, rec.Fingerprint), rec.Value)
	switch {
	case err == nil:
	case lostCreateRace(err):
		return coord.PromotionRecord{}, false, nil
	default:
		return coord.PromotionRecord{}, false,
			f.writeRefusal(err, "create a promotion record", "a promotion record", len(rec.Value))
	}
	return storedPromotion(rec, revision), true, nil
}

// UpdatePromotion writes a record at the version it was read at.
func (f *FleetStore) UpdatePromotion(ctx context.Context, rec coord.PromotionRecord) (coord.PromotionRecord, bool, error) {
	if err := rec.Validate(); err != nil {
		return coord.PromotionRecord{}, false, err
	}
	if err := withinCeiling("a promotion record", rec.Value); err != nil {
		return coord.PromotionRecord{}, false, err
	}
	if rec.Version == 0 {
		// NO VERSION IS A LOST RACE, never an unconditional write. The
		// client reads an expected revision of 0 as "the key must not exist
		// yet", so passing one through would CREATE a record for a caller
		// that never read one, where the contract says it must lose.
		return coord.PromotionRecord{}, false, nil
	}
	revision, err := f.positions.Update(ctx, coord.PromotionKey(rec.Unit, rec.Fingerprint),
		rec.Value, rec.Version)
	switch {
	case err == nil:
	case lostUpdateRace(err):
		return coord.PromotionRecord{}, false, nil
	default:
		return coord.PromotionRecord{}, false,
			f.writeRefusal(err, "update a promotion record", "a promotion record", len(rec.Value))
	}
	return storedPromotion(rec, revision), true, nil
}

// storedPromotion is what a successful write stored: the caller's record, its
// own copy of the value, and the revision the store assigned.
func storedPromotion(rec coord.PromotionRecord, revision uint64) coord.PromotionRecord {
	rec.Version = revision
	rec.Value = bytes.Clone(rec.Value)
	return rec
}
