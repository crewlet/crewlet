package coord

import "context"

// UploadReservations holds the bytes upload intents have reserved and no record
// names yet (internal/artifactupload): one LEDGER of the company's holds, and
// one RECORD per upload of the keys it minted, which hold bytes, how far it got.
//
// It is the company's: an upload's requests may land on three nodes, and its
// bound per principal and per company would be granted once per node if counted
// per node. Each value is one compare-and-set write, so two intents racing for
// the last of a budget cannot both win; both are opaque here. A value outlives
// its last write by [UploadRetention], longer than any hold, so an abandoned
// upload ages out with no sweep. Every error is UNKNOWN, never "nothing
// reserved": a caller that cannot read the ledger must not reserve, and a hold
// whose release it cannot write lapses on its own.
type UploadReservations interface {
	// UploadLedger reads the company's reservation ledger and the revision
	// it was read at: nil and 0 where none is written, or where the last
	// one aged out.
	UploadLedger(ctx context.Context) ([]byte, uint64, error)

	// SwapUploadLedger writes value as the ledger only while the ledger is
	// still at revision (0 naming a ledger that is not there), answering
	// the revision it wrote. Zero with no error is a LOST RACE: the caller
	// reads again and decides again. An empty value is refused.
	SwapUploadLedger(ctx context.Context, value []byte, revision uint64) (uint64, error)

	// UploadRecord reads one upload's record and the revision it was read
	// at: nil and 0 where there is none.
	UploadRecord(ctx context.Context, upload string) ([]byte, uint64, error)

	// SwapUploadRecord writes one upload's record only while it is still at
	// revision (0 creating it, refused where a record is there), answering
	// the revision it wrote, zero with no error being a lost race. An empty
	// upload id or value is refused.
	SwapUploadRecord(ctx context.Context, upload string, value []byte, revision uint64) (uint64, error)

	// DeleteUploadRecord removes one upload's record only while it is still
	// at revision, reporting false where it moved or is gone. A revision of
	// 0 removes nothing.
	DeleteUploadRecord(ctx context.Context, upload string, revision uint64) (bool, error)
}

// SaveAllowances is where the fleet keeps each seat's ledger of the executor
// sessions its runs saved as they worked (internal/sandbox): one value per
// seat, opaque here, written by compare-and-set, in the uploads bucket beside
// the [UploadReservations], whose age is the hold it needs. Every error is
// UNKNOWN. The package doc says why the whole company agrees on it and why it
// lives in that bucket.
type SaveAllowances interface {
	// SaveLedger reads one seat's save ledger and the revision it was read
	// at: nil and 0 where none is written, or where the last one aged out.
	// An empty seat is refused.
	SaveLedger(ctx context.Context, seat string) ([]byte, uint64, error)

	// SwapSaveLedger writes one seat's save ledger only while it is still
	// at revision (0 creating it, refused where a ledger is there),
	// answering the revision it wrote. Zero with no error is a LOST RACE:
	// another save moved the ledger since it was read, and the caller reads
	// it again and decides again. An empty seat or value is refused.
	SwapSaveLedger(ctx context.Context, seat string, value []byte, revision uint64) (uint64, error)
}
