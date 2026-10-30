package code

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// THE REF HOLD (M3-decisions M3-O61, M3-O64): a seat's branch a live turn
// depends on — one it read or wrote through a code tool, one a launch granted
// or resolved, one its box pushed — is HELD for as long as the turn is
// unended, on the same hold set, run row and reader as a scratch
// repository's hold ([Hold]), under a key of its own ([RefHoldKey]).
//
// A READ MOVES NO REF, so nothing on the repository's subject says a turn
// depends on a branch it only read; and a write that moves nothing — refused,
// a conflict, a no-op — appends no record at all. So a hold on a seat's branch
// unset [RefHoldRecordAfter] or more is also RECORDED: a [Hold] naming the ref
// on the repository's subject, whose apply moves the ref's
// code_refs.held_position and nothing that says the branch was used — never
// its idle clock (`at`), its delta cursor (`position`), its seat or its target.
// A deletion of the branch listed before the hold's read is then ordered
// against it PER REF, on the one subject every push to the repository
// arbitrates on: landed first, the hold's position is past the deletion's
// listing; landed second, the hold's decide finds the ref gone and answers
// ref_deleted, so the turn's request is refused before it read anything.
// Per ref and never per repository, because the repository's version moves on
// every push to any of its branches.

// refHoldDigestHex is how many hex digits of a ref's SHA-256 its hold key
// carries: 32, 128 bits. A collision between two refs of one repository could
// only keep the other branch held longer — the safe direction — and at 128
// bits none is expected among the at most MaxRefsPerRepo refs a repository
// holds.
const refHoldDigestHex = 32

// RefHoldKey is the hold key of one seat ref of a repository: the repository's
// id, ':', and the first 32 hex digits of the ref's SHA-256. ONE SPELLING for
// every taker and every reader — a turn's set, its run row, the fleet's ask of
// the running turns — so none holds a ref under a key another spells
// differently.
//
// FIXED WIDTH, 69 bytes beside a 36-byte repository id: a ref is up to
// MaxRefNameBytes that JSON may escape six-fold, and a run row carries up to
// turnctx.MaxHolds keys, so a key that held the ref itself could cost a row
// some 1.5 KiB a key. A scratch repository's own hold key is its bare id,
// which holds no ':', so the two kinds never collide.
func RefHoldKey(repoID, ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return repoID + ":" + hex.EncodeToString(sum[:])[:refHoldDigestHex]
}

// Holdable reports whether a hold on ref keeps anything: a branch under some
// seat's namespace ([SeatNamespaceRoot]<handle>/…), the only refs whose row
// can name a seat (code_refs.seat — the push apply sets it only where a seat
// set a ref inside its own namespace, [seatOf]). A hold on any other ref —
// main, a tag, a person's branch — keeps nothing, and none is taken.
func Holdable(ref string) bool {
	_, ok := seatNamespaceRef(ref)
	return ok
}

// RefHoldRecordAfter is how long a seat's branch must have gone unset before
// a turn's hold on it is also RECORDED ([Hold] with a Ref): [RefExpiry] less
// two hours, 334 h — [HoldRecordAfter]'s argument, with RefExpiry for
// idle_days.
//
// A hold the code host's retention reads AFTER it has fixed its cutoff and its
// page needs no record when the branch was set less than this before the hold,
// by the holding node's clock read after the hold was taken: the pass's cutoff
// and this threshold are each ONE node's clock compared with the same
// broker-stamped code_refs.at, so the broker's clock cancels and only the two
// nodes' disagreement matters, and a branch set later than the threshold is
// later than any in-flight pass's cutoff — RefExpiry before an instant that
// preceded the hold. Two hours covers that disagreement twice at
// objstore.AssumedClockSkew (an hour between two nodes), and exactly two nodes
// each within an hour of the broker, which `clock_skew` watches. Held by
// references.TestAHoldRecordCoversEveryPassInFlight.
const RefHoldRecordAfter = RefExpiry - 2*time.Hour
