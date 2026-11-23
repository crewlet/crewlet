package coord

import "context"

// SeatAccess is the seat access ledger: the fleet's record of every account
// the engine created at a vendor for an agent seat, and of each seat's last
// access sync (internal/seataccess).
//
// The whole company's: the sync runs as the integration loop's worker duty or
// on the node that served Sync, and the routes read it wherever the dashboard
// is served, so a node-local record would lose the accounts a moved duty
// created, the orphans the ledger exists to reach. The value is OPAQUE, as
// [Integrations]'s is: internal/seataccess's vocabulary. No age (an expiry
// would forget an account still live at the vendor) and no compare-and-set:
// every writer holds the kind's provisioning lease, as [Integrations] argues,
// so last write wins.
type SeatAccess interface {
	// SeatAccessRecords returns every record for one integration, keyed by
	// the seat's handle. RAISES on an unreachable store: "no accounts" and
	// "cannot tell" send a sync down opposite paths.
	SeatAccessRecords(ctx context.Context, kind string) (map[string][]byte, error)

	// SeatAccessRecord is one seat's record at one integration, false where
	// it has none: one key, where a listing would carry every other seat's
	// record too. RAISES on an unreachable store, as the listing does.
	SeatAccessRecord(ctx context.Context, kind, handle string) ([]byte, bool, error)

	// PutSeatAccessRecord records one seat's record at one integration,
	// replacing any prior one.
	PutSeatAccessRecord(ctx context.Context, kind, handle string, value []byte) error

	// DeleteSeatAccessRecord drops one seat's record. Deleting one that is
	// not there is the outcome asked for.
	DeleteSeatAccessRecord(ctx context.Context, kind, handle string) error
}

// SeatAccessClass is the key class a record is filed under in the positions
// register.
const SeatAccessClass = "seat_access"

// MaxSeatAccessBytes bounds one seat's record at one integration as stored.
// Every turn of the seat reads it, and the sync's read of it decides what the
// vendor is told, so its reply crosses a leaf's link at the slowest pace in
// 10 s. A lane holds its access twice (applied and observed): 20 grants
// (internal/config's MaxSeatGrants) with whole plain reasons take 85 KB, and
// 256 KiB holds some 700 scope entries of 60 bytes beside them that the vendor
// names apart from their ids, 1,400 it does not; a typical record is a few
// KiB. A lane whose access would outgrow it takes no step, and is told why
// (internal/seataccess's syncer.fits).
const MaxSeatAccessBytes = 256 << 10

// SeatAccessKey is one seat's key at one integration.
func SeatAccessKey(kind, handle string) string {
	return DocumentKey(SeatAccessClass, kind, handle)
}

// SeatAccessFilter selects every record of one integration.
func SeatAccessFilter(kind string) string { return DocumentFilter(SeatAccessClass, kind) }

// SeatAccessHandle recovers the seat's handle from a key of kind's records. It
// reports false for a key of another class or integration, and for one this
// grammar did not write.
func SeatAccessHandle(key, kind string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 3 || segments[0] != SeatAccessClass || segments[1] != kind || segments[2] == "" {
		return "", false
	}
	return segments[2], true
}
