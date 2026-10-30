package coord

import "context"

// THE SEAT ACCESS LEDGER is the fleet's record of every account the engine
// created at a vendor for an agent seat, and of each seat's last access sync
// (internal/seataccess).
//
// # Who has to agree on it
//
// The whole company, now. The sync that writes it runs as the integration
// loop's worker duty, or on whichever node served the person who pressed
// Sync; the access routes read it on whichever node serves the dashboard. On a
// node's own database a record would be invisible from the node that shows
// it, and a duty that moved would forget every account it had created, which
// is the orphan the ledger exists to prevent: a seat that left the document
// keeps a live account at the vendor only the ledger can still reach.
//
// # The value is OPAQUE, as [Integrations]'s is
//
// A record is internal/seataccess's vocabulary (an account, a sealed
// credential's name, each lane's desired, observed and last sync), and a copy
// of it here would have to be kept equal for ever for no reader's benefit.
//
// # No age, and no compare-and-set
//
// It lives in the positions register under its own key class: an expiring
// record would forget an account that still exists at the vendor, on a timer
// nobody chose. Every writer holds the kind's provisioning lease while it
// writes, the argument [Integrations] makes in full, so last write wins is
// safe.
type SeatAccess interface {
	// SeatAccessRecords returns every record for one integration, keyed by
	// the seat's handle. RAISES on an unreachable store: "no accounts" and
	// "cannot tell" send a sync down opposite paths.
	SeatAccessRecords(ctx context.Context, kind string) (map[string][]byte, error)

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
