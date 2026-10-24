package memory

import (
	"context"
	"errors"
	"slices"
)

// ---- the upload reservations ------------------------------------------ //

// versioned is one value of the upload reservations and the version it was
// written at, drawn from the store-wide counter (see [Fleet.version]) so a
// record deleted and written again never hands back a version an older
// incarnation of it used.
type versioned struct {
	value   []byte
	version uint64
}

// UploadLedger reads the reservation ledger and its version.
//
// The bucket's age is not modelled, for the completion ledger's reason: it is
// a property of the bucket the KV backend creates, held there, and a twin
// inside one test process never runs for a day.
func (f *Fleet) UploadLedger(context.Context) ([]byte, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploadLedger.version == 0 {
		return nil, 0, nil
	}
	return slices.Clone(f.uploadLedger.value), f.uploadLedger.version, nil
}

// SwapUploadLedger writes the ledger while it is still at revision.
func (f *Fleet) SwapUploadLedger(_ context.Context, value []byte, revision uint64) (uint64, error) {
	if len(value) == 0 {
		return 0, errors.New("coord/memory: an upload ledger needs a value")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploadLedger.version != revision {
		return 0, nil
	}
	f.version++
	f.uploadLedger = versioned{value: slices.Clone(value), version: f.version}
	return f.version, nil
}

// UploadRecord reads one upload's record and its version.
func (f *Fleet) UploadRecord(_ context.Context, upload string) ([]byte, uint64, error) {
	if upload == "" {
		return nil, 0, errors.New("coord/memory: an upload record needs an upload")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.uploads[upload]
	if !ok {
		return nil, 0, nil
	}
	return slices.Clone(rec.value), rec.version, nil
}

// SwapUploadRecord writes one upload's record while it is still at revision,
// 0 creating it.
func (f *Fleet) SwapUploadRecord(_ context.Context, upload string, value []byte, revision uint64) (uint64, error) {
	switch {
	case upload == "":
		return 0, errors.New("coord/memory: an upload record needs an upload")
	case len(value) == 0:
		return 0, errors.New("coord/memory: an upload record needs a value")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploads[upload].version != revision {
		return 0, nil
	}
	f.version++
	f.uploads[upload] = versioned{value: slices.Clone(value), version: f.version}
	return f.version, nil
}

// DeleteUploadRecord removes one upload's record while it is still at
// revision.
func (f *Fleet) DeleteUploadRecord(_ context.Context, upload string, revision uint64) (bool, error) {
	if upload == "" {
		return false, errors.New("coord/memory: an upload record needs an upload")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec, ok := f.uploads[upload]
	if !ok || revision == 0 || rec.version != revision {
		return false, nil
	}
	delete(f.uploads, upload)
	return true, nil
}
