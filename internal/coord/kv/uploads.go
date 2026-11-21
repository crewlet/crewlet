package kv

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the upload reservations ------------------------------------------ //

// The uploads bucket holds the ONE ledger of every hold the company has
// outstanding, one record per upload and one save ledger per seat
// ([coord.SaveAllowances]). The ledger's key has one segment and the others
// two, each under its own class, so no upload id or seat spells another's key.
const uploadLedgerKey = "ledger"

// uploadRecordKey is one upload's record key.
func uploadRecordKey(upload string) string { return coord.DocumentKey("upload", upload) }

// saveLedgerKey is one seat's save ledger key.
func saveLedgerKey(seat string) string { return coord.DocumentKey("save", seat) }

// UploadLedger reads the reservation ledger from the stream LEADER
// ([getLatest]): a replica behind the last reservation would hand the next one
// a total already spent, and its compare-and-set would lose for nothing.
func (f *FleetStore) UploadLedger(ctx context.Context) ([]byte, uint64, error) {
	return f.uploadValue(ctx, uploadLedgerKey, "read the upload ledger")
}

// SwapUploadLedger writes the ledger while it is still at revision
// ([coord.UploadReservations]).
func (f *FleetStore) SwapUploadLedger(ctx context.Context, value []byte, revision uint64) (uint64, error) {
	if len(value) == 0 {
		return 0, errors.New("coord/kv: an upload ledger needs a value")
	}
	return f.swapUpload(ctx, uploadLedgerKey, value, revision, "write the upload ledger")
}

// UploadRecord reads one upload's record from the stream leader.
func (f *FleetStore) UploadRecord(ctx context.Context, upload string) ([]byte, uint64, error) {
	if upload == "" {
		return nil, 0, errors.New("coord/kv: an upload record needs an upload")
	}
	return f.uploadValue(ctx, uploadRecordKey(upload), "read the upload record")
}

// SwapUploadRecord writes one upload's record while it is still at revision,
// 0 creating it.
func (f *FleetStore) SwapUploadRecord(ctx context.Context, upload string, value []byte, revision uint64) (uint64, error) {
	switch {
	case upload == "":
		return 0, errors.New("coord/kv: an upload record needs an upload")
	case len(value) == 0:
		return 0, errors.New("coord/kv: an upload record needs a value")
	}
	return f.swapUpload(ctx, uploadRecordKey(upload), value, revision, "write the upload record")
}

// DeleteUploadRecord removes one upload's record while it is still at revision.
//
// A DELETE, NOT A PURGE: this bucket ages every message, its marker included,
// so nothing need sweep it (the purge rule is for buckets no clock ages,
// markers.go).
func (f *FleetStore) DeleteUploadRecord(ctx context.Context, upload string, revision uint64) (bool, error) {
	if upload == "" {
		return false, errors.New("coord/kv: an upload record needs an upload")
	}
	if revision == 0 {
		// The client drops a LastRevision of 0 and deletes unconditionally,
		// which would remove a record another node had just claimed.
		return false, nil
	}
	err := f.uploads.Delete(ctx, uploadRecordKey(upload), jetstream.LastRevision(revision))
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	default:
		return false, unavailable("delete the upload record", err)
	}
}

// SaveLedger reads one seat's save ledger from the stream leader, for the
// upload ledger's reason: a replica behind the last save would hand the next
// one a count it has already spent.
func (f *FleetStore) SaveLedger(ctx context.Context, seat string) ([]byte, uint64, error) {
	if seat == "" {
		return nil, 0, errors.New("coord/kv: a save ledger needs a seat")
	}
	return f.uploadValue(ctx, saveLedgerKey(seat), "read the seat's save ledger")
}

// SwapSaveLedger writes one seat's save ledger while it is still at revision,
// 0 creating it ([coord.SaveAllowances]).
func (f *FleetStore) SwapSaveLedger(ctx context.Context, seat string, value []byte, revision uint64) (uint64, error) {
	switch {
	case seat == "":
		return 0, errors.New("coord/kv: a save ledger needs a seat")
	case len(value) == 0:
		return 0, errors.New("coord/kv: a save ledger needs a value")
	}
	return f.swapUpload(ctx, saveLedgerKey(seat), value, revision, "write the seat's save ledger")
}

// uploadValue reads one key of the uploads bucket as the leader holds it,
// answering nothing for a key never written, aged out or deleted.
func (f *FleetStore) uploadValue(ctx context.Context, key, what string) ([]byte, uint64, error) {
	entry, err := f.get(ctx, f.uploads, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, 0, nil
	case err != nil:
		return nil, 0, unavailable(what, err)
	}
	return entry.Value(), entry.Revision(), nil
}

// swapUpload writes one key of the uploads bucket conditionally: a create
// through [createKey] at revision 0, so a key whose last message is a delete
// marker is created over it, and an update at the revision otherwise.
func (f *FleetStore) swapUpload(ctx context.Context, key string, value []byte, revision uint64, what string) (uint64, error) {
	if revision == 0 {
		wrote, err := f.create(ctx, f.uploads, key, value)
		switch {
		case err == nil:
			return wrote, nil
		case errors.Is(err, jetstream.ErrKeyExists):
			return 0, nil
		default:
			return 0, unavailable(what, err)
		}
	}
	wrote, err := f.uploads.Update(ctx, key, value, revision)
	switch {
	case err == nil:
		return wrote, nil
	case lostUpdateRace(err):
		return 0, nil
	default:
		return 0, unavailable(what, err)
	}
}
