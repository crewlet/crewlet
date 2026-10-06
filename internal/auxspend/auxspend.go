// Package auxspend is what the seats' AUXILIARY model spends, recorded: the
// attribution every caller of the auxiliary seam states, the per-turn tally a
// turn's work item is charged from, and the node's LEDGER that coalesces each
// call into an `auxiliary_spend` record.
//
// # Why the record is separate from the counter
//
// Every auxiliary completion — the turn-start memory filter, knowledge query
// and episode summary, each compaction rewrite, the reflection workers, the
// background learning passes, a person's answered question — is charged to the
// fleet's token counters, and until this package none reached an event, so
// every spend figure folded from events understated the counters by exactly
// that. The record is made at the same ONE SEAM the charge is, and
// independently of it: the counter exists only where a coordination store
// does and only for a scope a budget can name, and a figure a person reads
// must not depend on either. A call with no counter is still recorded; a call
// recorded is still charged wherever there is a counter.
//
// # Attribution is an argument
//
// A [Use] says whose cost a call is (its [types.AuxStage]), what it was for
// (its [types.AuxPurpose]) and the turn it served, and the seam refuses one
// that does not ([Use.Validate]). Never carried on a context: a goroutine
// shares whatever context it captured, so an ambient attribution charges a
// call to whichever turn last wrote it (internal/agent/turnctx says why at
// length). Every call site states it, and a gate in this package's suite holds
// that each one names a purpose constant.
//
// # One record per key per flush
//
// A [Ledger] holds a bucket per (stage, seat or person, turn, purpose, model,
// provider entry, company day) and publishes each as one record when it
// flushes — every [FlushInterval]; for one turn's buckets, before the turn's
// end and before its reflection pass's sentinel are published; and at the
// node's stop. A compaction of seventy rewrites is one record; an
// ordinary turn's prefetch is one per call it made, which is what it always
// was. The DAY IS IN THE KEY, so a bucket closes at midnight on the company's
// clock rather than straddling it, and the record is stamped with its last
// call's instant — the day the counters charged it in — however late its flush
// ran.
//
// # What a crash costs
//
// A bucket lives in memory until it is published, so a process that dies
// loses at most one flush interval of records: the counters already hold that
// spend, and the rollups are short by it. A publish that FAILS keeps its
// record — sealed, with its id and its instant, so the retry the next flush
// makes is the same event and every reader's de-duplication collapses a
// publish that in fact landed — up to [MaxPending] of them, past which the
// oldest is dropped and said.
package auxspend

import (
	"errors"
	"fmt"
	"sync"

	"github.com/crewlet/crewlet/internal/events/types"
)

// Use is who an auxiliary call is made for and why: the attribution every
// caller of the seam states.
//
// THE ZERO VALUE IS REFUSED ([Use.Validate]), not read as anything: a call
// filed under no stage and no purpose is spend that every breakdown would show
// as "unknown", and the seam is the one place that can stop it.
type Use struct {
	// Stage is whose cost the call is.
	Stage types.AuxStage

	// Purpose is what the call is for — one value per caller.
	Purpose types.AuxPurpose

	// TurnID is the run the call serves or learns from, and WorkKey the
	// unit of work behind it (ADR-0017). Empty for a call that serves no
	// turn — a background pass, a person's question — and for one whose
	// caller could not say, which is then filed on the seat's day alone.
	TurnID  string
	WorkKey string

	// Tally receives the call's spend for the turn's work item (ADR-0022):
	// set by a turn on its in-turn calls and on nothing else, and refused on
	// any other stage — reflection is the seat's learning, and charging it
	// to the item would make a task's cost depend on how much its seat had
	// to remember.
	Tally *Tally
}

// For is this attribution with one call's purpose — the shape a caller holding
// a turn's attribution takes for each call it makes.
func (u Use) For(purpose types.AuxPurpose) Use {
	u.Purpose = purpose
	return u
}

// ErrUnattributed is an auxiliary call whose attribution cannot file its
// spend.
var ErrUnattributed = errors.New("auxspend: the auxiliary call states no valid attribution")

// Validate refuses an attribution a record cannot be filed under.
func (u Use) Validate() error {
	switch {
	case !u.Stage.Valid():
		return fmt.Errorf("%w: stage %q is not one of %v", ErrUnattributed, u.Stage, types.AuxStages)
	case !u.Purpose.Valid():
		return fmt.Errorf("%w: purpose %q is not a purpose this build publishes", ErrUnattributed, u.Purpose)
	case u.Tally != nil && u.Stage != types.AuxStageTurn:
		return fmt.Errorf("%w: a %s call carries a turn's tally, which only the turn "+
			"stage is charged to the work item through (ADR-0022)", ErrUnattributed, u.Stage)
	}
	return nil
}

// Spent is what calls cost: how many, and their tokens. The cache counts are a
// BREAKDOWN of Input, never an addition: Input plus Output is the total.
type Spent struct {
	Calls      int
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

// Tokens is the total a token budget counts: input plus output.
func (s Spent) Tokens() int { return s.Input + s.Output }

func (s *Spent) add(o Spent) {
	s.Calls += o.Calls
	s.Input += o.Input
	s.Output += o.Output
	s.CacheRead += o.CacheRead
	s.CacheWrite += o.CacheWrite
}

// Tally is one turn segment's in-turn auxiliary spend, kept in process for the
// charge its work item takes when the segment ends.
//
// A TALLY RATHER THAN THE RECORDS, because the records are not where a
// segment's charge comes from: the charge is decided in the process that ran
// the segment, at its end, before any record of its last calls has been
// flushed — and the records are the fleet's, read back at query time. It only
// ever grows and authorises nothing, so a turn may hand it to everything it
// calls (internal/agent/turnctx's rule for what a turn may point at).
//
// A nil *Tally is meaningful: it records nothing and totals zero, which is
// every call that is not a turn's own.
type Tally struct {
	mu    sync.Mutex
	spent Spent
}

// NewTally is an empty tally.
func NewTally() *Tally { return &Tally{} }

// Add counts one call's spend.
func (t *Tally) Add(s Spent) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spent.add(s)
}

// Total is everything counted so far.
func (t *Tally) Total() Spent {
	if t == nil {
		return Spent{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spent
}
