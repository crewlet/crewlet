package memory

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the promotion ledger ---------------------------------------------- //

// promotionAddress is a record's address: its unit and its fingerprint.
type promotionAddress struct{ unit, fingerprint string }

// Promotions returns every record filed under one unit, ordered by
// fingerprint.
func (f *Fleet) Promotions(_ context.Context, unit string) ([]coord.PromotionRecord, error) {
	if unit == "" {
		return nil, errors.New("coord/memory: a promotion listing names the unit it reads")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []coord.PromotionRecord{}
	for addr, rec := range f.promotions {
		if addr.unit == unit {
			rec.Value = slices.Clone(rec.Value)
			out = append(out, rec)
		}
	}
	slices.SortFunc(out, func(a, b coord.PromotionRecord) int {
		return cmp.Compare(a.Fingerprint, b.Fingerprint)
	})
	return out, nil
}

// CreatePromotion files a new record, leaving an existing one alone.
func (f *Fleet) CreatePromotion(_ context.Context, rec coord.PromotionRecord) (coord.PromotionRecord, bool, error) {
	if err := rec.Validate(); err != nil {
		return coord.PromotionRecord{}, false, err
	}
	if err := withinCeiling("a promotion record", rec.Value); err != nil {
		return coord.PromotionRecord{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	addr := promotionAddress{rec.Unit, rec.Fingerprint}
	if _, exists := f.promotions[addr]; exists {
		return coord.PromotionRecord{}, false, nil
	}
	return f.storePromotionLocked(addr, rec), true, nil
}

// UpdatePromotion writes a record at the version it was read at.
func (f *Fleet) UpdatePromotion(_ context.Context, rec coord.PromotionRecord) (coord.PromotionRecord, bool, error) {
	if err := rec.Validate(); err != nil {
		return coord.PromotionRecord{}, false, err
	}
	if err := withinCeiling("a promotion record", rec.Value); err != nil {
		return coord.PromotionRecord{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	addr := promotionAddress{rec.Unit, rec.Fingerprint}
	current, ok := f.promotions[addr]
	if !ok || rec.Version == 0 || current.Version != rec.Version {
		return coord.PromotionRecord{}, false, nil
	}
	return f.storePromotionLocked(addr, rec), true, nil
}

// storePromotionLocked stores rec under a fresh version and returns it as
// stored. CLONED ON THE WAY IN AND OUT, for the reason the positions row is:
// a shared slice is the one aliasing bug a twin can have that the real
// backend cannot, because the real one serialises.
func (f *Fleet) storePromotionLocked(addr promotionAddress, rec coord.PromotionRecord) coord.PromotionRecord {
	if f.promotions == nil {
		f.promotions = map[promotionAddress]coord.PromotionRecord{}
	}
	f.version++
	rec.Version = f.version
	rec.Value = slices.Clone(rec.Value)
	f.promotions[addr] = rec
	out := rec
	out.Value = slices.Clone(rec.Value)
	return out
}
