package code

import (
	"errors"
	"fmt"
)

// The sentinels. Each is a fact a caller acts on differently, which is why
// they are separate; each is errors.Is-comparable, and each keeps that
// identity across the estate wire once a caller on another node reads it.
var (
	// ErrRepoNotFound is a repository nobody holds under that name or id.
	ErrRepoNotFound = errors.New("code: no such repository")

	// ErrPurging is a repository being removed part by part: its name
	// returns when the purge completes.
	ErrPurging = errors.New("code: the repository is being purged; its name returns when the purge completes")

	// ErrNotYet is a deleted repository still inside its retention, so not
	// yet purged.
	ErrNotYet = errors.New("code: the repository is within its deleted retention")

	// ErrNotDeleted is a purge asked of a repository that is not deleted.
	ErrNotDeleted = errors.New("code: the repository is not deleted")

	// ErrNotConfigManaged is a configuration reconcile reaching a repository
	// its configuration does not manage.
	ErrNotConfigManaged = errors.New("code: the repository is not managed by configuration")

	// ErrNoActivation is a configuration write that does not name the
	// activation it came from.
	ErrNoActivation = errors.New("code: a repository's settings must name the instant their configuration was activated")

	// ErrSuperseded is a configuration reconcile whose activation a LATER
	// one has already fenced off: its decisions would be refused on every
	// node, so it makes none, and what it reports is the later activation's
	// to decide.
	ErrSuperseded = errors.New("code: a later configuration activation owns the declared repositories")

	// ErrInvalid is a request or a declaration this domain refuses by field,
	// wrapped with the field and the reason.
	ErrInvalid = errors.New("code: refused")

	// ErrInvalidProposal is a push proposal this domain cannot decide,
	// wrapped with the field and the reason ([Proposal.Validate]), or one
	// whose push id names a push to another repository. At a committer it is
	// never a refusal of the push — the receiver formed the proposal, and
	// held the client's commands (every one receive-pack acts on, read as it
	// reads them) to [Proposal.ValidateCommands] before any work, so it is
	// the receiver's bug. From ValidateCommands itself it is
	// the CLIENT'S push, past what one push may carry, which the receiver
	// refuses in the client's words.
	ErrInvalidProposal = errors.New("code: not a push this domain can decide")

	// ErrNotServing is a node's code half that RAN NOTHING: a committer that
	// decided nothing for this proposal and appended nothing — it is
	// stopping, its node publishes nothing (a maintenance mode, [Served]),
	// or the code log refused this NODE before anything was appended (an
	// eviction, a record it cannot decode, a floor it is below) — or a half
	// holding no reader to answer from. Another data node can commit the
	// same push under the same push id, which collapses a repeat, and can
	// answer the same read; the estate router asks it.
	ErrNotServing = errors.New("code: this node is not serving the code host")

	// ErrRecordCap is a decision past a record's byte or row budget — which
	// the committer's batching keeps from ever forming; the check makes the
	// invariant local to the record.
	ErrRecordCap = errors.New("code: the decision exceeds a record's budget")

	// ErrBadCursor is a cursor this reader did not issue.
	ErrBadCursor = errors.New("code: not a cursor this read issued")

	// ErrBadLevel is a freshness this read does not serve.
	ErrBadLevel = errors.New("code: not a freshness this read serves")

	// ErrSnapshotMoved is a later page of a snapshot that found the
	// repository's history moved under it — a tombstone a delta needed was
	// swept, or the log was reanchored or recreated. NOT A FAULT: the
	// consumer reads the snapshot again from its first page, which decides
	// full or delta afresh.
	ErrSnapshotMoved = errors.New("code: the repository's history moved under this snapshot; read it again from the first page")

	// ErrBadBundle is a pack bundle whose bytes are not the pack, index and
	// reverse index its row describes — a section's header, a count, or the
	// pack checksum the three must share disagrees. Returned by
	// [CheckBundle] on the receiving node before it proposes a push, and on
	// an installing cache before it renames anything into place; never a
	// value the applier sees, because a push whose bundle failed it is never
	// published.
	ErrBadBundle = errors.New("code: a pack bundle is not the one its row describes")
)

// TooWideError is a push whose entry can reach Bytes in its record, past the
// Room one push's entry may take ([EntryRoom]) — [ErrInvalidProposal] by
// identity. Typed, so the receiver tells the client what the push takes and
// what one push may, and the client pushes its refs in parts: from
// [Proposal.ValidateCommands] it is the client's push, and from
// [Proposal.Validate] at a committer the receiver's bug.
type TooWideError struct {
	// Bytes is what the push's entry can reach; Room what one entry may.
	Bytes, Room int
}

func (e *TooWideError) Error() string {
	return fmt.Sprintf("%v: commands: the push's entry can reach %d bytes in its record, past the %d one "+
		"entry may take (MaxRecordBytes less a record's own overhead) — a push this wide is pushed in parts",
		ErrInvalidProposal, e.Bytes, e.Room)
}

// Unwrap is ErrInvalidProposal.
func (e *TooWideError) Unwrap() error { return ErrInvalidProposal }
