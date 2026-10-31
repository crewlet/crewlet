package code

import (
	"fmt"
	"time"
)

// THE REF LIFECYCLE'S RULE (M3-decisions M3-O62): which of the branches an
// expiry push lists is still a seat's IDLE branch when its committer decides
// it. The step that lists them and proposes the push is the retention pass's
// (retention_expiry.go); this is what its decide asks of each command, as
// rule 4b (decide.go), from the ref's row in the decide's own snapshot.

// ExpiryRow is what a ref's row holds that the expiry judges: its target, the
// seat that set it in its own namespace ("" when none did — [seatOf]), when,
// the composed position of the last hold record naming it (0 for none — the
// ref hold, hold.go), whether it is its repository's default branch, and
// whether a live change names it as its source or its target. NOT RefRow:
// that name is the snapshot's wire row (wire.go).
type ExpiryRow struct {
	Target  string
	Seat    string
	At      time.Time
	Held    int64
	Default bool
	Change  bool
}

// The sentences [ExpiryRule] keeps a branch with, in the order it asks: each a
// reason the branch is no longer what its listing read it as — idle, and its
// seat's alone. Every one is held under maxDetailBytes by the receiver-details
// test's sibling (expiry_test.go).
const (
	expiryKeptTouched = "pushed in this batch, after the expiry listed it"
	expiryKeptHeld    = "held by a live turn since the expiry listed it"
	expiryKeptDefault = "the repository's default branch"
	expiryKeptChange  = "the source or the target of a live change"
	expiryKeptSeat    = "no longer a branch its seat set: somebody else pushed it last"
)

// expiryWindow is [RefExpiry] as a sentence says it.
var expiryWindow = fmt.Sprintf("%d days", int(RefExpiry/(24*time.Hour)))

// expiryKeptRecent is the sentence of a branch pushed within the window.
var expiryKeptRecent = "pushed within code.RefExpiry (" + expiryWindow + ")"

// ExpiryRule judges one command of an expiry push against the ref's row as the
// decide's snapshot holds it, after rule 4 (the target no longer Old is
// stale): "" where the ref is still a seat's idle branch, else
// [RefusalStale] and one sentence saying why it is kept — stale, not a
// refusal of its own, because the meaning is the committer's own (the ref is
// not what the proposal was formed from), and an expiry is never printed to a
// git client.
//
//   - touched is an earlier push of the same batch having set the ref: its row
//     in the snapshot predates that push.
//   - through is the proposal's Seen, composed — the position of the page the
//     expiry was listed from — so a hold RECORDED after that page (row.Held >
//     through) keeps the ref: the pass read the live turns' holds after the
//     page and could not have seen it (M3-decisions M3-O61). Equal is a hold
//     taken before the page, which the pass's own read of the holds answered.
//   - A repository's DEFAULT branch is kept whatever its age: deleted, the
//     repository's HEAD names nothing and every clone checks nothing out
//     (M3-decisions M3-O62) — and a seat's branch may be made the default.
//   - The SOURCE OR TARGET OF A LIVE CHANGE is kept: deleting a source closes
//     its change (revisions.go), and deleting a target strands every change
//     into it, so a branch under review is not idle however long the review
//     takes.
//   - A ref no seat set ("") is no longer its seat's to lose: a person pushed
//     it last, even to the same commit.
//   - now is the instant the pass formed the proposal at ([Proposal.ReceivedAt]),
//     never the deciding node's clock: the window is judged on the same clock
//     the listing's cutoff was, so the only two clocks compared with the
//     broker-stamped `at` are the pass's and a hold's taker's, which
//     [RefHoldRecordAfter] is argued from.
//
// Pure, over values: a rule exercised only through a database is a rule
// nobody re-reads.
func ExpiryRule(row ExpiryRow, touched bool, now time.Time, through int64) (Refusal, string) {
	switch {
	case touched:
		return RefusalStale, expiryKeptTouched
	case row.Held > through:
		return RefusalStale, expiryKeptHeld
	case row.Default:
		return RefusalStale, expiryKeptDefault
	case row.Change:
		return RefusalStale, expiryKeptChange
	case row.Seat == "":
		return RefusalStale, expiryKeptSeat
	case row.At.After(now.Add(-RefExpiry)):
		return RefusalStale, expiryKeptRecent
	}
	return "", ""
}
