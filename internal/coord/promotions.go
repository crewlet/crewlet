package coord

import (
	"context"
	"fmt"
)

// A PROMOTION RECORD is what the fleet remembers about one convergence the
// skill-promotion pass acted on: the draft page it wrote for a unit lead, and
// whether that lead rejected it.
//
// # Why the fleet holds it
//
// The pass is a fleet singleton on a lease, so the node that runs it tomorrow
// is not reliably the node that ran it today, and a record in one node's own
// database would be a record the next holder of the duty cannot see — it
// would draft again what the last holder drafted, or what a lead rejected. The
// answer to "has this team's convergence been drafted, or rejected" is one the
// whole company has to agree on, now, which is this package's question.
//
// # The address is a unit and a fingerprint, and the fingerprint is not ours
//
// A record is filed under the unit's name and a fingerprint of the
// convergence, each a segment of its key, so one unit's records are a class of
// their own that a listing reads without reading any other unit's. What the
// fingerprint is computed from, and how a record is matched to a convergence,
// is internal/learning's decision (see its promote.go), and so is everything
// in the value: coordination stores the bytes that package composed, for the
// reason [SandboxRuns] stores a run's.
//
// # No retention
//
// The KV backend files a record in the positions register, the bucket with no
// age, and the memory twin ages nothing: a
// rejection is a person's decision rather than an observation that goes stale,
// and a clock that forgot it would draft again, on a schedule nobody chose,
// the procedure the lead said no to. Nothing removes a record. They are
// bounded by what is written: the pass files at most one per unit per tick —
// a draft, or the model's decline — and a record changes state rather than
// multiplying.
//
// # An older build
//
// A build without this class files no record and consults none. Its listings
// of the positions register each filter on their own class, so a record here
// is invisible to it rather than misread; but while such a build holds the
// promotion duty during a rolling upgrade, it drafts as it always did — by
// the title the model chose — and nothing written here can stop it.

// PromotionRecord is one convergence's record, as the fleet holds it.
type PromotionRecord struct {
	// Unit is the name of the unit whose seats converged.
	Unit string

	// Fingerprint names the convergence within its unit.
	Fingerprint string

	// Value is the record's body, composed by internal/learning. At most
	// [MaxRecordBytes] bytes: a record is one message.
	Value []byte

	// Version is the store's version of the record as it was read. OPAQUE,
	// like [Record.Version]: pass back exactly what a read or a write handed
	// you. Ignored by CreatePromotion.
	Version uint64
}

// Validate reports why a record cannot be filed: its unit and its fingerprint
// are its address, and an address with an empty segment is one no listing
// reads back.
func (r PromotionRecord) Validate() error {
	if r.Unit == "" || r.Fingerprint == "" {
		return fmt.Errorf("coord: a promotion record names its unit and its "+
			"fingerprint, and this one names unit %q and fingerprint %q",
			r.Unit, r.Fingerprint)
	}
	return nil
}

// Promotions is the fleet's ledger of the convergences the skill-promotion
// pass has acted on.
//
// # RAISES rather than answering empty
//
// "This unit has no records" is the answer that lets the pass pay for a model
// call and draft again a procedure a lead rejected, so a read that failed must
// never be able to give it.
//
// # CREATE-ONLY, then COMPARE-AND-SET
//
// A convergence is filed once, and a record that exists is changed only by a
// write conditioned on the version its writer read. The pass is a singleton,
// but a duty that moves between nodes can leave two holders mid-pass for a
// moment, and the create is what stops both of them drafting the same
// convergence: exactly one of them files it.
type Promotions interface {
	// Promotions returns every record filed under one unit, ordered by
	// fingerprint. None is an empty slice and no error. An empty unit is an
	// error: it would compose a filter that matches nothing, which a listing
	// reports exactly as a unit with no records.
	Promotions(ctx context.Context, unit string) ([]PromotionRecord, error)

	// CreatePromotion files a new record and returns it as stored, with the
	// version its next write must carry. An address that already holds a
	// record is left alone and reports false. A record
	// [PromotionRecord.Validate] refuses is an error, and a value longer than
	// [MaxRecordBytes] is [ErrTooLarge]: stored neither whole nor cut.
	CreatePromotion(ctx context.Context, rec PromotionRecord) (PromotionRecord, bool, error)

	// UpdatePromotion writes rec at rec.Version and returns it as stored,
	// reporting false when that version no longer holds, when the record is
	// gone, and when rec carries no version at all — every one a lost race
	// to re-read, never an unconditional write. Refused as CreatePromotion
	// refuses.
	UpdatePromotion(ctx context.Context, rec PromotionRecord) (PromotionRecord, bool, error)
}

// promotionClass is the class of a [PromotionRecord]'s key in the positions
// register.
const promotionClass = "promotion"

// PromotionKey is a record's key: its class, its unit and its fingerprint,
// each a segment, so one unit's records are a class [PromotionsFilter]
// selects.
func PromotionKey(unit, fingerprint string) string {
	return DocumentKey(promotionClass, unit, fingerprint)
}

// PromotionsFilter selects the records of one unit.
func PromotionsFilter(unit string) string { return DocumentFilter(promotionClass, unit) }

// PromotionOf recovers the unit and the fingerprint a [PromotionKey] names,
// reporting false for any other key — every other class of the register among
// them.
func PromotionOf(key string) (unit, fingerprint string, ok bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 3 || segments[0] != promotionClass {
		return "", "", false
	}
	return segments[1], segments[2], true
}
