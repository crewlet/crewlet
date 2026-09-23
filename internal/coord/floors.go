package coord

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// A TRIM FLOOR is what the fleet's trim singleton concluded about one domain,
// published so that every node can answer for it.
//
// # Why the conclusion is published rather than re-derived
//
// The six terms the trim takes a minimum over are readable from anywhere: the
// positions register, the holds, the backup floor and the stream's own bounds
// are all fleet-wide facts. Two of the answers are NOT. Which term came
// lowest is a property of one evaluation, and HOW LONG it has been the lowest
// is a property of a series of them — and both are exactly what an operator
// asks for when a log is growing. A node re-deriving the first would get a
// second opinion about one event (the failure [statelog]'s alarm table is
// written against), and nothing at all could re-derive the second, because a
// duty that moves between nodes on a lease carries no memory across the move.
//
// So the tick writes what it concluded and every other surface reads it. That
// is the same shape [NodeStatus] uses for a config apply, and for the same
// reason: the node that did the work is the only one that can say what it
// found.
//
// # Two sequences, because they answer two different questions
//
// TrimTo is what ONE TICK concluded, and it moves in both directions: it is
// zero on every blocked tick, and it falls whenever the lowest counted node
// is lower than it was — a readmitted node, a node restored from an old
// backup, a node that came up on its own history because nobody could donate.
// That is the right figure for "is the trim moving", and it is the wrong one
// for "what may already be gone", because a record an EARLIER tick licensed
// removing does not come back when a later tick concludes less.
//
// Floor is that second answer: everything below it may have been removed. It
// never moves down within a generation, and the trim writes it BEFORE the
// purge it licenses — so at every instant, including while a purge is in
// flight and after one that was delayed or retried, no record the trim has
// ever removed or may yet remove sits at or above it. A floor derived from
// the counted minimum alone would fail that for exactly the nodes a write
// fence exists to refuse: a minimum that includes a node can never exceed it.
//
// # It has no TTL, for the register's own reason
//
// This row lives in the positions bucket, which is the one bucket in the
// estate with no age at all — see [PositionRegister]. A floor that expired
// would read as a fleet that has never trimmed, which is indistinguishable
// from one whose duty has been blocked since the day it started, and those are
// opposite answers to the only question anyone reads this for. A domain that
// stops existing has its row removed by the same operator gesture that removes
// it, never by a clock.

// TrimTerm is one term's answer as the tick evaluated it.
//
// A COPY of the arithmetic's own term rather than a reference to it: this
// struct is on the wire between two builds, and the arithmetic's shape is free
// to change inside a release where a published record's is not.
type TrimTerm struct {
	// Name is which term this is — the vocabulary is [statelog]'s TermName.
	Name string `json:"name"`

	// Seq is the highest sequence this term permits removing up to,
	// exclusive, and is meaningful only when Known.
	Seq uint64 `json:"seq"`

	// Known reports whether the term could be evaluated at all. A term
	// nobody could read BLOCKS, so false here is a blocking answer rather
	// than a missing one.
	Known bool `json:"known"`

	// Absent marks a term this domain does not have — a domain that
	// declares no wake feed — which renders `n/a` rather than zero.
	Absent bool `json:"absent,omitempty"`

	// Detail is the term's own sentence, which is what an operator reads
	// when this term is the one blocking.
	Detail string `json:"detail,omitempty"`
}

// TrimFloor is one domain's published floor.
type TrimFloor struct {
	// Domain is which log this is about, and the key.
	Domain string `json:"domain"`

	// Generation is the domain's generation at the tick that wrote this —
	// each log has its own. A floor at a generation the reader is not on
	// names another number space: one BELOW the reader's is a dead space
	// in which nothing the reader could name has been licensed for
	// removal, and one ABOVE it is a space the reader has not reached and
	// cannot compare against at all.
	Generation uint32 `json:"generation"`

	// TrimTo is the exclusive sequence THIS tick concluded may be removed
	// up to. Zero while blocked, and lower than the previous tick's
	// whenever the counted minimum fell — which is why no reader that
	// asks "what may be gone" reads it.
	TrimTo uint64 `json:"trim_to"`

	// Floor is the first sequence every node must hold: every record below
	// it may have been removed, by this tick or any earlier one at this
	// generation. It is never lower than TrimTo, never lower than the
	// previous row's Floor at the same generation, and never lower than
	// the stream's own first sequence as the tick read it.
	//
	// WRITTEN BEFORE THE PURGE IT LICENSES, which is what makes it an
	// upper bound on what is gone rather than a report of it: published
	// after, it would trail a purge by however long the write took to
	// succeed, and a write fence reading it inside that gap clears a node
	// the purge has just left below the log.
	Floor uint64 `json:"floor"`

	// BlockedBy names the term that came lowest and permitted nothing, or
	// that could not be read. Empty while the trim is advancing.
	BlockedBy string `json:"blocked_by,omitempty"`

	// BlockedSince is when TrimTo last ADVANCED, and it is the field
	// nothing else can supply: the duty moves between nodes on a lease, so
	// a value held in the holder's memory would reset on every flap and
	// the twenty-four-hour condition would never be reached.
	BlockedSince time.Time `json:"blocked_since,omitzero"`

	// Terms is every term's answer, so a blocked trim can be read without
	// running the tick again.
	Terms []TrimTerm `json:"terms,omitempty"`

	// At is when this tick ran and By is which node held the duty. Both
	// are for the operator: a floor nobody has rewritten for an hour is a
	// duty that is not running, which no field inside the decision says.
	At time.Time `json:"at"`
	By string    `json:"by,omitempty"`
}

// FloorRegister is the fleet's record of what its trim concluded.
//
// It is a key class in the positions bucket, beside the node rows, the holds
// and the backup points, because all four answer one question — what may the
// log delete — and all four need the same retention, which is none. A second
// bucket would cost a retention decision, a replica count and a place in every
// sweep for a fact that is always read with the other three.
type FloorRegister interface {
	// PutFloor writes one domain's floor, replacing it wholesale.
	//
	// LAST WRITER WINS, and here that is a statement about the duty rather
	// than about the key: the trim is a fleet singleton, so there is one
	// writer at a time and a compare-and-set would only make a lease
	// handover fail on a value the new holder is about to recompute.
	PutFloor(ctx context.Context, f TrimFloor) error

	// Floors reads every domain's row.
	//
	// The whole set, because every caller wants it: `crewlet retention
	// status` prints one line per registered domain, and a per-domain read
	// would be one round trip per line.
	Floors(ctx context.Context) ([]TrimFloor, error)

	// ForgetFloor removes one, for a domain that no longer exists.
	ForgetFloor(ctx context.Context, domain string) error
}

// FloorKey is a domain's key in the register.
func FloorKey(domain string) string { return DocumentKey("floor", domain) }

// Validate reports why a floor cannot be written.
func (f TrimFloor) Validate() error {
	if strings.TrimSpace(f.Domain) == "" {
		return fmt.Errorf("coord: a trim floor names no domain — it is the key, " +
			"so a floor without one would be written over another domain's " +
			"conclusion and read as that domain's")
	}
	if f.BlockedBy == "" && f.TrimTo == 0 && len(f.Terms) > 0 {
		return fmt.Errorf("coord: the trim floor for %s permits removing up to 0 "+
			"and names no blocking term: a tick that removed nothing did so for "+
			"a reason, and an empty `blocked_by` is read as a healthy fleet",
			f.Domain)
	}
	if f.Floor < f.TrimTo {
		return fmt.Errorf("coord: the trim floor for %s licenses removing up to "+
			"%d and tells every reader it need only hold from %d — the purge "+
			"this row licenses would leave a node the write fence clears below "+
			"the log", f.Domain, f.TrimTo, f.Floor)
	}
	return nil
}

// Blocked reports whether this domain's trim is advancing.
func (f TrimFloor) Blocked() bool { return f.BlockedBy != "" }
