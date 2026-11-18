package gitlet

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// THE REF HOLD (M3-decisions M3-O61, M3-O64): a seat's branch a live turn
// depends on (read or written through a code tool, granted or resolved by a
// launch, pushed by its box) is HELD while the turn is unended, on a scratch
// repository's [Hold] set, run row and reader, keyed by [RefHoldKey].
//
// A read moves no ref and a no-op write appends no record, so a hold on a
// branch unset [RefHoldRecordAfter] or more is also RECORDED: a [Hold] naming
// the ref on the repository's subject, moving only code_refs.held_position,
// nothing saying the branch was used (not `at`, `position`, seat or target). A
// deletion listed before the hold's read is ordered against it PER REF (the
// repository's version moves on every push): a hold landed first is past the
// deletion's listing; second, it answers ref_deleted before the turn reads.

// refHoldDigestHex is the hex digits of a ref's SHA-256 a hold key carries:
// 32, 128 bits, so no collision is expected among MaxRefsPerRepo refs, and
// one would only keep a branch held longer, the safe direction.
const refHoldDigestHex = 32

// RefHoldKey is the hold key of one seat ref of a repository: the repository's
// id, ':', and the first 32 hex digits of the ref's SHA-256. ONE SPELLING for
// every taker and reader (a turn's set, its run row, the fleet's ask).
//
// FIXED WIDTH, 69 bytes beside a 36-byte repository id: a ref of
// MaxRefNameBytes JSON may escape six-fold, so holding the ref itself could
// cost a run row of turnctx.MaxHolds keys some 1.5 KiB a key. A scratch repository's key is its bare id, with no
// ':', so the two kinds never collide.
func RefHoldKey(repoID, ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return repoID + ":" + hex.EncodeToString(sum[:])[:refHoldDigestHex]
}

// Holdable reports whether a hold on ref keeps anything: only a branch under a
// seat's namespace ([SeatNamespaceRoot]<handle>/…) has a row that can name a
// seat (code_refs.seat, set only where a seat pushed inside its own namespace,
// [seatOf]). Main, a tag or a person's branch takes no hold.
func Holdable(ref string) bool {
	_, ok := seatNamespaceRef(ref)
	return ok
}

// RefHoldRecordAfter is how long a seat's branch must have gone unset before
// a turn's hold on it is also RECORDED ([Hold] with a Ref): [RefExpiry] less
// two hours, 334 h ([HoldRecordAfter]'s argument, RefExpiry for idle_days).
//
// A hold retention reads after fixing its cutoff needs no record when the
// branch was set less than this before it, by the holding node's clock read
// after the hold was taken: the cutoff and this threshold each compare ONE
// node's clock with the same broker-stamped code_refs.at, so only the two
// nodes' disagreement matters, and such a branch is past any in-flight pass's
// cutoff. Two hours is twice objstore.AssumedClockSkew (an hour between two
// nodes), or two nodes each an hour off the broker, which `clock_skew`
// watches. Held by references.TestAHoldRecordCoversEveryPassInFlight.
const RefHoldRecordAfter = RefExpiry - 2*time.Hour
