package coord

import "context"

// UploadReservations is where the fleet keeps the bytes an artifact's upload
// intents have reserved and no record names yet (internal/artifactupload): one
// LEDGER of every hold the company has outstanding, and one RECORD per upload
// saying which keys it minted, which of them hold bytes, and how far it got.
//
// # Why the whole company has to agree on it
//
// An upload is reserved on one node and its bytes may be streamed to another
// and finalized on a third, wherever a load balancer sends each request; and
// the bound a reservation is held to (bytes outstanding per principal and per
// company) is a company's figure, so two nodes each counting their own would
// let a company hold the bound once per node. The bytes it counts sit in the
// object store the fleet shares, unnamed until a record names them, so the
// account of them is the fleet's too.
//
// # Two documents, each written by compare-and-set
//
// The ledger is ONE value, so a reservation that checks the company's total
// and adds to it is one conditional write, and two intents racing for the last
// of a budget cannot both win. A record is one value per upload, so a transfer
// claiming a key and a finalize claiming the upload are each one conditional
// write on that upload alone. Both values are opaque here: their owner
// (internal/artifactupload) encodes them, and the arithmetic over them is its
// own, judged on its own clock. This contract keeps what two nodes must agree
// on, and nothing else: which write came first.
//
// # Retention is the bucket's age
//
// Every value outlives its last write by [UploadRetention], which outlasts the
// longest hold a reservation keeps, so a value the store has aged out is one
// nothing needs any more: an abandoned upload's record goes on its own, with no
// sweep and no index of what to sweep.
//
// # It fails in the safe direction
//
// Every error is UNKNOWN. A caller that cannot read the ledger must not
// reserve, since a reservation it could not count is one that may pass the
// budget; and a caller that cannot write a release keeps the hold it meant to
// drop, which lapses on its own. Nothing here ever answers "nothing reserved"
// for "could not read".
type UploadReservations interface {
	// UploadLedger reads the company's reservation ledger and the revision
	// it was read at: nil and 0 where none is written, or where the last
	// one aged out.
	UploadLedger(ctx context.Context) ([]byte, uint64, error)

	// SwapUploadLedger writes value as the ledger only while the ledger is
	// still at revision (0 naming a ledger that is not there), answering
	// the revision it wrote. Zero with no error is a LOST RACE: another
	// write moved the ledger since it was read, and the caller reads it
	// again and decides again. An empty value is refused.
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
	// at revision, and reports whether it did: false with no error where the
	// record moved or is already gone. A revision of 0 names no record and
	// removes nothing.
	DeleteUploadRecord(ctx context.Context, upload string, revision uint64) (bool, error)
}
