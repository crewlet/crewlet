// Package ledgerstore persists the turn engine's completion ledger: whether a
// trigger has already been worked.
//
// A subpackage rather than more files in ledger, so ledger keeps the property
// its own doc claims: it imports nothing from crewlet but a standard-library
// leaf. The turn loop, the runner and the parked state all hold ledger VALUES,
// and a package that dragged the coordination store behind it would be held
// by all three.
//
// THE LEDGER IS THE FLEET'S. "Has this trigger already been worked?" is only
// useful if every node gives the same answer, so the durable one is
// [FleetCompletions] over the coordination store. On the node's own database,
// a redelivery that landed on a peer found an empty ledger and the turn ran
// twice.
//
// IT FAILS OPEN IN BOTH DIRECTIONS. Not knowing whether work was done has one
// safe answer and it is the pre-ledger one — do the work. A read that failed
// closed would make a store blip look like a company that had already
// answered everything.
//
// What a conversation's next turn is told of its earlier turns is not kept
// here, nor on any one node: it is the executor log's (internal/executordomain),
// a replicated domain every data node holds identically, whose rows record how
// each turn ended and name the session it kept in the fleet's object store.
package ledgerstore

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("agent.ledgerstore")

// Completions answers "has this trigger already been worked?".
//
// THE SEAT IS NAMED BY ITS HANDLE, which is immutable (ADR-0013).
type Completions interface {
	// Worked returns the subset of keys already recorded for this seat.
	//
	// FAILS OPEN: an error yields no keys, so every trigger is treated as
	// unworked and runs. Duplicating a turn is recoverable; silently
	// answering nothing is not.
	Worked(ctx context.Context, handle string, keys []string) map[string]bool

	// WorkedAt is Worked with when each key was worked, zero where its
	// record states no instant. Fails open as Worked does.
	WorkedAt(ctx context.Context, handle string, keys []string) map[string]time.Time

	// Record marks a key worked. Best effort — see Worked.
	Record(ctx context.Context, handle, key, turnID string, at time.Time) error

	// There is NO Purge. The durable ledger is the coordination store's,
	// where the bucket's own age is the retention and the broker expires
	// the records — so a sweep here would have nothing to delete, and the
	// floor that matters (a record must outlast the scheduler's catchup
	// window, or a fire runs twice) is held by coord.LedgerRetention and
	// asserted by coordtest.
}

// FleetCompletions is the completion ledger on the fleet's coordination
// store.
//
// The ledger answers "has this trigger already been worked?", and the answer
// is only useful if every node gives the same one. On the node's own database
// it could not: a redelivery that landed on a peer found an empty ledger and
// the turn ran a second time — which is the single thing this ledger exists
// to prevent.
//
// # Where the fail-open decision is made
//
// [coord.Ledger] RAISES a read failure rather than swallowing it, because the
// choice to do the work anyway belongs to whoever is about to do it. This is
// that whoever: it logs the failure by name and answers "nothing is worked",
// so the trigger runs. Duplicating a turn is recoverable; silently answering
// nothing is not.
type FleetCompletions struct{ ledger coord.Ledger }

// NewFleetCompletions wraps a coordination store.
func NewFleetCompletions(ledger coord.Ledger) *FleetCompletions {
	return &FleetCompletions{ledger: ledger}
}

var _ Completions = (*FleetCompletions)(nil)

// Worked returns the subset of keys already recorded for this seat.
func (f *FleetCompletions) Worked(ctx context.Context, handle string, keys []string) map[string]bool {
	got, err := f.ledger.Worked(ctx, handle, keys)
	if err != nil {
		log.WarnContext(ctx, "completion_ledger_unreadable", "seat", handle, "error", err,
			"detail", "treating every trigger as unworked, which may repeat a turn a peer took")
		return nil
	}
	return got
}

// WorkedAt returns the keys already recorded for this seat, each with when.
func (f *FleetCompletions) WorkedAt(ctx context.Context, handle string, keys []string) map[string]time.Time {
	got, err := f.ledger.WorkedAt(ctx, handle, keys)
	if err != nil {
		log.WarnContext(ctx, "completion_ledger_unreadable", "seat", handle, "error", err,
			"detail", "treating every trigger as unworked, which may repeat a turn a peer took")
		return nil
	}
	return got
}

// Record marks a key worked. Best effort, for the same reason as Worked.
//
// An UNKEYED turn records nothing and is not an error: a trigger with no
// ledgerable key is legitimately unconstrained — it is the case the duplicate
// guard is meant to skip — while the coordination store refuses an empty key
// because a CALLER reaching it with one has a bug. The adapter is where those
// two truths meet.
func (f *FleetCompletions) Record(ctx context.Context, handle, key, turnID string, at time.Time) error {
	if handle == "" || key == "" {
		return nil
	}
	return f.ledger.Record(ctx, handle, key, turnID, at)
}
