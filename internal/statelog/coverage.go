package statelog

import (
	"fmt"
	"slices"
)

// WHAT A GATHERED ANSWER COVERED.
//
// # Why every gathered answer carries one
//
// A read that addresses several partitions is answered partition by partition,
// each by a node that serves it, and a partition can fail to answer: nobody
// serves it this instant, the node that does is behind the asker's own writes,
// it went silent, or the read failed there. An answer assembled from the rest
// is still worth having — and it is a SHORTER answer, which is
// indistinguishable from a company with less in it unless it says so. A seat
// told "these are your three open tasks" when one partition of four did not
// answer acts on three, and files the fourth again.
//
// So the answer names what it addressed, what answered and what did not and
// why, and the cut it was answered at — and every surface renders a non-empty
// [Coverage.Missing] as [Coverage.Notice] rather than as the list alone.
//
// # Declared here rather than beside the router
//
// The answer types that carry one are the domains' own — a tracker board, a
// person's day, a listing of pages — and the router that fills it imports
// those domains. The one package below all of them that already says where a
// read was answered ([Cut], [Position]) is this one.
//
// # The zero value
//
// An answer that states NO coverage: read below the router — a domain's own
// tests, a reader handed rows directly — or from a backend that has no
// partitions at all. It names nothing missing, so it renders no notice, which
// is exactly what such a read can honestly say.

// Coverage is what a gathered answer covered.
type Coverage struct {
	// Addressed is how many partitions the read addressed, which is
	// Answered and Missing together.
	Addressed int `json:"addressed"`

	// Answered is every partition that answered, by its id (`tracker.007`).
	Answered []string `json:"answered"`

	// Missing is every partition that did not, and why.
	Missing []MissingPartition `json:"missing,omitempty"`

	// At is where each log of an answering partition was when its read
	// began — so everything at or below it is in the answer.
	//
	// A LOWER BOUND, deliberately: what a caller does with a cut is mark
	// what it has SEEN (an inbox's read mark is one position per log), and
	// a mark past what the read included would hide an entry nobody was
	// shown. Empty for a read that is not of a log's rows — a search reads
	// an index each node builds behind its applier, so no position
	// describes it.
	At Cut `json:"at,omitempty"`
}

// MissingPartition is one partition a gathered read could not answer from.
type MissingPartition struct {
	Partition string        `json:"partition"`
	Reason    MissingReason `json:"reason"`

	// Detail is who was asked and what each said, in order — the sentence
	// an operator follows to the node that has the problem.
	Detail string `json:"detail,omitempty"`
}

// MissingReason is why a partition did not answer, and each names a different
// thing to look at.
type MissingReason string

const (
	// MissingUnserved — no node serves the partition: the placement names
	// no holder, or every holder runs no backend for the read, admits
	// nothing yet, or cannot tell whether it serves it.
	MissingUnserved MissingReason = "unserved"

	// MissingUnreachable — holders are named and none of them answered.
	MissingUnreachable MissingReason = "unreachable"

	// MissingBehind — a holder answered and could not reach the asker's
	// own writes on the partition's logs within the read budget.
	MissingBehind MissingReason = "behind"

	// MissingNotHolder — every holder asked said it does not serve the
	// partition: the estate map is moving, and the asker's view or theirs
	// is behind it.
	MissingNotHolder MissingReason = "not_holder"

	// MissingError — the read ran there and failed.
	MissingError MissingReason = "error"
)

// MissingReasons are the five.
var MissingReasons = []MissingReason{
	MissingUnserved, MissingUnreachable, MissingBehind, MissingNotHolder, MissingError,
}

// Valid reports whether a reason off the wire is one this build knows. An
// unknown one is still a missing partition — [Coverage.Notice] counts it — and
// is never refused on decode: a newer build may name a reason this one has not
// learned, and the partition is missing either way.
func (r MissingReason) Valid() bool { return slices.Contains(MissingReasons, r) }

// Complete reports an answer from which nothing it addressed is missing.
func (c Coverage) Complete() bool { return len(c.Missing) == 0 }

// Notice is the ONE sentence a non-empty [Coverage.Missing] is rendered as, on
// every surface: a seat's tool, the turn-start block, an operator's assistant.
// Empty when nothing is missing.
//
// A SENTENCE RATHER THAN A FLAG, because the right response is a behaviour —
// do not conclude something is absent — and a boolean beside a list reads as
// metadata about it rather than as a caveat on it.
func (c Coverage) Notice() string {
	if len(c.Missing) == 0 {
		return ""
	}
	return fmt.Sprintf("%d of %d partitions did not answer; this list may be incomplete",
		len(c.Missing), c.Addressed)
}
