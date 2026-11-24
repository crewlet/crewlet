package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// A TRIM FLOOR is what the fleet's trim singleton concluded about one domain,
// published so every node can answer for it: which term came lowest, and for
// how long, are facts of one evaluation and of a series that no other node can
// re-derive (the duty moves on a lease and keeps no memory), so the tick
// writes them, as [NodeStatus] does for an apply.
//
// [TrimFloor.TrimTo] answers "is the trim moving" and may fall; [TrimFloor.Floor]
// answers "what may be gone" and never falls within a generation. Floor is not
// the counted minimum: a minimum that includes a node never exceeds it, so it
// would clear exactly the nodes a write fence exists to refuse. No TTL
// ([PositionRegister]): an expired floor would read as a fleet never trimmed.

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

	// Floor is the first sequence every data node must hold: every record below
	// it may have been removed, by this tick or an earlier one at this
	// generation. It is never lower than TrimTo, the previous row's Floor at
	// the same generation, or the stream's first sequence as the tick read it.
	//
	// WRITTEN BEFORE THE PURGE IT LICENSES, so it bounds what is gone rather
	// than reporting it: written after, a write fence reading it in the gap
	// would clear a node the purge has just left below the log.
	Floor uint64 `json:"floor"`

	// BlockedBy names the term that came lowest and permitted nothing, or
	// that could not be read. Empty while the trim is advancing.
	BlockedBy string `json:"blocked_by,omitempty"`

	// BlockedSince is when TrimTo last ADVANCED. Only the row can carry it:
	// the duty moves between nodes on a lease, so a value in the holder's
	// memory would reset on every flap and `trim_blocked`, which fires on a
	// block's duration, would never be reached.
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
// A key class in the positions bucket beside the node rows, holds and backup
// points: all four answer what the log may delete and need no retention, so a
// second bucket would cost a retention decision and a sweep for nothing.
//
// NOTHING REMOVES A FLOOR. The domains are compiled into the build with no
// run-time retirement, and each tick replaces its domain's row wholesale, so
// the register holds one row per domain a build has run.
type FloorRegister interface {
	// PutFloor writes one domain's floor, replacing it wholesale.
	//
	// LAST WRITER WINS: the trim is a fleet singleton, so a compare-and-set
	// would only make a lease handover fail on a value the new holder is
	// about to recompute.
	PutFloor(ctx context.Context, f TrimFloor) error

	// Floor reads ONE domain's row as the store's leader holds it: false where
	// the trim never wrote one (an answer), an error where the store did not
	// answer (unknown, never absent).
	//
	// A KEY READ, AND THE REGISTER HAS NO OTHER: every reader names its
	// domains, the write fence only AFTER its write's own read of its subject
	// (the floor theorem's order, statelog's package doc). A listing costs two
	// proposals to a clustered broker's metadata group, which under load ran
	// past the API timeout and refused writes `floor_unknown`.
	//
	// A row that does not decode, or names another domain (comparing two logs'
	// sequences), matches [ErrFloorUnreadable]; a silent store, [ErrUnavailable].
	Floor(ctx context.Context, domain string) (TrimFloor, bool, error)
}

// ErrFloorUnreadable is a domain's floor row that the register holds and
// cannot read as that domain's floor: its bytes do not decode, or it names
// another domain than the key it is filed under.
//
// A FAULT OF THE ROW, NEVER OF THE STORE, with the opposite remedy: a store
// that does not answer ([ErrUnavailable]) clears by itself, while such a row
// clears only once somebody removes it, since the trim carries a floor
// forward from the row it reads. A refusal names the two apart.
var ErrFloorUnreadable = errors.New("coord: the trim floor row does not read as its domain's floor")

// MaxTrimTerms bounds the terms a floor carries: internal/statelog evaluates
// six, and internal/engine holds the count to this.
const MaxTrimTerms = 8

// MaxTrimTermNameBytes bounds a term's name: a word of internal/statelog's
// TermName vocabulary.
const MaxTrimTermNameBytes = 64

// MaxTrimDetailBytes bounds a term's sentence as stored ([TextBytes]): the
// trim cuts one past it ([FitText]), and a floor's eight at their widest stay
// a small value (ADR-0175).
const MaxTrimDetailBytes = 1 << 10

// FloorKey is a domain's key in the register.
func FloorKey(domain string) string { return DocumentKey(FloorClass, domain) }

// FloorClass is the key class a trim floor is filed under in the positions
// register.
const FloorClass = "floor"

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
	switch {
	case !ValidDomainName(f.Domain):
		return fmt.Errorf("coord: a trim floor names %q, which is no domain's name", f.Domain)
	case len(f.Terms) > MaxTrimTerms:
		return fmt.Errorf("coord: the trim floor for %s carries %d terms, past the %d a row holds",
			f.Domain, len(f.Terms), MaxTrimTerms)
	case !plainWithin(f.BlockedBy, MaxTrimTermNameBytes) || !plainWithin(f.By, MaxNodeIDBytes):
		return fmt.Errorf("coord: the trim floor for %s names a blocking term or a node that is "+
			"no term's or node's name", f.Domain)
	}
	for _, t := range f.Terms {
		if !plainWithin(t.Name, MaxTrimTermNameBytes) || TextBytes(t.Detail) > MaxTrimDetailBytes {
			return fmt.Errorf("coord: the trim floor for %s carries a term %q that is no term's "+
				"name or whose sentence is past %d bytes as stored", f.Domain, t.Name, MaxTrimDetailBytes)
		}
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
