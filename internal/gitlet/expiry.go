package gitlet

import (
	"fmt"
	"time"
)

// THE REF LIFECYCLE'S RULE (M3-decisions M3-O62): which branches an expiry
// push lists are still a seat's IDLE branch when its committer decides it,
// asked of each command as rule 4b (decide.go) from the decide's snapshot.
// The retention pass lists them and proposes the push (retention_expiry.go).

// ExpiryRow is what of a ref's row the expiry judges: its target, the seat
// that set it in its own namespace ("" for none, [seatOf]), when, the composed
// position of its last hold record (0 for none; hold.go), whether it is the
// default branch, the live change it sources, and whether a live change
// merges into it. Not RefRow, the snapshot's wire row (wire.go).
type ExpiryRow struct {
	Target  string
	Seat    string
	At      time.Time
	Held    int64
	Default bool
	// OpenChange is the live change (draft or open) this branch is the source
	// of, as [ChangeRef] names it (web!12), "" for none. A NAME, not a flag, so
	// the keeping sentence says which review holds the branch (M4-D32);
	// code_changes_open_idx keeps it to one.
	OpenChange string
	// ChangeTarget is a live change proposing its source INTO this branch:
	// deleting it would strand that change with no branch to merge into.
	ChangeTarget bool
}

// The sentences [ExpiryRule] keeps a branch with, in the order it asks: each a
// reason the branch is no longer idle and its seat's alone. Held under
// maxDetailBytes by TestEveryExpirySentenceFitsADetail.
const (
	expiryKeptTouched = "pushed in this batch, after the expiry listed it"
	expiryKeptHeld    = "held by a live turn since the expiry listed it"
	expiryKeptDefault = "the repository's default branch"
	expiryKeptTarget  = "the target of a live change"
	expiryKeptSeat    = "no longer a branch its seat set: somebody else pushed it last"
)

// expiryKeptOpenChange is the sentence of a branch the live change change
// (web!12) is proposed from. "Live", not "open": a draft's branch is kept too
// ([ChangeState.Live]).
func expiryKeptOpenChange(change string) string { return "the source branch of live change " + change }

// expiryWindow is [RefExpiry] as a sentence says it.
var expiryWindow = fmt.Sprintf("%d days", int(RefExpiry/(24*time.Hour)))

// expiryKeptRecent is the sentence of a branch pushed within the window.
var expiryKeptRecent = "pushed within gitlet.RefExpiry (" + expiryWindow + ")"

// ExpiryRule judges one command of an expiry push against the ref's row in
// the decide's snapshot, after rule 4 (a target no longer Old is stale): "" for
// a seat's idle branch, else [RefusalStale] (the ref is not what the proposal
// was formed from; an expiry never reaches a git client) and why it is kept:
// touched (an earlier push of the batch set it); a hold recorded after the
// listed page, through being the proposal's composed Seen (M3-O61; equal
// predates the page); the DEFAULT branch, whose deletion leaves HEAD naming
// nothing (M3-O62); a live change's SOURCE (deleting it closes the change;
// M4-D32) or TARGET, which expiryListingSQL skips but may arise after the
// page; Seat "", a person's push; or the window. now is the pass's
// [Proposal.ReceivedAt], never the deciding node's clock, so the window shares
// the listing cutoff's clock ([RefHoldRecordAfter]). Pure over values.
func ExpiryRule(row ExpiryRow, touched bool, now time.Time, through int64) (Refusal, string) {
	switch {
	case touched:
		return RefusalStale, expiryKeptTouched
	case row.Held > through:
		return RefusalStale, expiryKeptHeld
	case row.Default:
		return RefusalStale, expiryKeptDefault
	case row.OpenChange != "":
		return RefusalStale, expiryKeptOpenChange(row.OpenChange)
	case row.ChangeTarget:
		return RefusalStale, expiryKeptTarget
	case row.Seat == "":
		return RefusalStale, expiryKeptSeat
	case row.At.After(now.Add(-RefExpiry)):
		return RefusalStale, expiryKeptRecent
	}
	return "", ""
}
