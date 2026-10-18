package kv

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the upload reservations ------------------------------------------ //

// The uploads bucket holds two classes of key: the ONE ledger of every hold the
// company has outstanding, and one record per upload. Two segments for a record
// and one for the ledger, so no upload id can ever spell the ledger's key.
const uploadLedgerKey = "ledger"

// uploadRecordKey is one upload's record key.
func uploadRecordKey(upload string) string { return coord.DocumentKey("upload", upload) }

// UploadLedger reads the reservation ledger from the stream LEADER, for the
// reason every single-key read here does ([getLatest]): a replica behind the
// last reservation would hand the next one a total it has already spent, and
// its compare-and-set would then lose for nothing.
func (f *FleetStore) UploadLedger(ctx context.Context) ([]byte, uint64, error) {
	return f.uploadValue(ctx, uploadLedgerKey, "read the upload ledger")
}

// SwapUploadLedger writes the ledger while it is still at revision
// ([coord.UploadReservations]).
func (f *FleetStore) SwapUploadLedger(ctx context.Context, value []byte, revision uint64) (bool, error) {
	if len(value) == 0 {
		return false, errors.New("coord/kv: an upload ledger needs a value")
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
func (f *FleetStore) SwapUploadRecord(ctx context.Context, upload string, value []byte, revision uint64) (bool, error) {
	switch {
	case upload == "":
		return false, errors.New("coord/kv: an upload record needs an upload")
	case len(value) == 0:
		return false, errors.New("coord/kv: an upload record needs a value")
	}
	return f.swapUpload(ctx, uploadRecordKey(upload), value, revision, "write the upload record")
}

// DeleteUploadRecord removes one upload's record while it is still at revision.
//
// A DELETE, NOT A PURGE: the bucket ages every message, its marker included, so
// the marker goes on its own and nothing sweeps it (the purge rule is for the
// buckets no clock ages, markers.go).
func (f *FleetStore) DeleteUploadRecord(ctx context.Context, upload string, revision uint64) (bool, error) {
	if upload == "" {
		return false, errors.New("coord/kv: an upload record needs an upload")
	}
	if revision == 0 {
		// The client drops a LastRevision of 0 and deletes unconditionally,
		// so a caller that never read a revision would remove whatever was
		// there: a record another node had just claimed.
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
func (f *FleetStore) swapUpload(ctx context.Context, key string, value []byte, revision uint64, what string) (bool, error) {
	if revision == 0 {
		_, err := f.create(ctx, f.uploads, key, value)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, jetstream.ErrKeyExists):
			return false, nil
		default:
			return false, unavailable(what, err)
		}
	}
	_, err := f.uploads.Update(ctx, key, value, revision)
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	default:
		return false, unavailable(what, err)
	}
}
