package artifactdata

import (
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The sentinels. A write the APPLIER refuses is not one of these: it is a
// [WriteResult] whose verdict says why, because the record landed and its
// refusal is a fact every node recorded. These are what is refused before
// anything is appended, and what a read cannot answer.
var (
	// ErrNotFound reports a store, a collection or a document that does
	// not exist, or that the reader may not see, which a caller is told
	// alike.
	ErrNotFound = errors.New("artifactdata: no such document")

	// ErrInvalid reports input the domain refuses, naming the field and
	// what to change.
	ErrInvalid = errors.New("artifactdata: invalid")

	// ErrForbidden reports a write the caller's own standing refuses
	// before it is appended: an undo of somebody else's write by a
	// non-editor, or an agent loosening a schema's authority controls.
	ErrForbidden = errors.New("artifactdata: forbidden")

	// ErrGone reports a store its artifact's purge removed for good.
	ErrGone = errors.New("artifactdata: the datastore was purged")

	// ErrConflict reports a schema change that lost its race on the store
	// too many times: somebody keeps writing it, so read it again.
	ErrConflict = errors.New("artifactdata: the datastore kept changing under this schema change")

	// ErrNotUndoable reports a write the history cannot undo: refused, an
	// undo itself past its own, a schema change, pruned from the hot
	// window, or holding a value past the history's bound.
	ErrNotUndoable = errors.New("artifactdata: that write cannot be undone")

	// ErrMigrationTooLarge reports a schema change that would rewrite more
	// documents than one record may ([artifacts.MaxMigrationDocs]).
	ErrMigrationTooLarge = errors.New("artifactdata: the schema change rewrites too many documents")

	// ErrBadCursor refuses a cursor this reader did not mint.
	ErrBadCursor = errors.New("artifactdata: that cursor is not one this listing minted")
)

// RateLimited is a write refused by this node's cost-weighted limits before it
// was appended (429 with Retry-After). A per-node guard that protects the node
// from one client; never a lockout, and never a fleet quota.
type RateLimited struct {
	// Scope says which bucket refused: `principal` (the caller on this
	// store) or `store` (everybody on this store).
	Scope string

	// Cost is what the write weighed: the rows its apply writes (each named
	// document's row, touch and field index, deleted and written again),
	// in units of sixteen ([Cost]).
	Cost int

	// RetryAfter is when the bucket holds the cost again.
	RetryAfter time.Duration
}

// Error says the wait as a person plans by ("holds it again in 2m"), rounded
// up ([statelog.SpokenWait]), never as a Duration's own "1m23.456s".
func (e *RateLimited) Error() string {
	return fmt.Sprintf("artifactdata: this write weighs %d and the %s's write budget on this "+
		"node holds it again in %s; retry then", e.Cost, e.Scope, statelog.SpokenWait(e.RetryAfter))
}
