package code

import "errors"

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
