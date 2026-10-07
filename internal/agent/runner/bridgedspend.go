package runner

import (
	"sync"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
)

// meter is where what a surface's own calls cost is counted: the turn its
// tools act as, whose auxiliary tally a rewrite one of them asks for adds to;
// the compactor its workers condense a dependency's answer through, on the
// same tally; and the spend its delegated workers report into, under mu.
//
// EVERY SURFACE THE RUNNER BUILDS NAMES ONE, because which segment pays for a
// tool call is decided where the surface is built and nowhere later. Nearly
// every surface is the runner's own ([Runner.ownMeter]): its calls run inside
// this segment, which charges what they cost when it ends. An agent-mode
// executor's is not — see [BridgedSpend].
type meter struct {
	turn    *turnctx.Turn
	compact compact.Bound
	spend   *Spend
	mu      *sync.Mutex
}

// ownMeter is this segment's meter: its turn, its compactor, its tally.
func (r *Runner) ownMeter() meter {
	return meter{turn: r.cfg.Turn.Context, compact: r.cfg.Compact, spend: &r.spend, mu: &r.mu}
}

// BridgedSpend is what an agent-mode executor's BRIDGED calls cost the
// engine: the auxiliary calls the seat's tools made for the coding agent and
// the workers it delegated to.
//
// ITS OWN METER, because those calls happen after the segment that launched
// the run has ended. The CLI calls the seat's tools over the bridge for as long
// as it runs — minutes or hours, often on a node that is not the one the turn
// resumes on — while the launching segment charged what it had spent the
// moment it suspended. Counted on that segment's tally, every bridged call
// added to a number nothing read again, and the task the turn was on was never
// charged for any of them. Counted here, the bridge writes the running total to
// the run's row with each call it records, and the segment that resumes from
// the run pays it.
//
// Safe for concurrent use: a coding agent calls tools in parallel.
type BridgedSpend struct {
	aux *auxspend.Tally

	mu    sync.Mutex
	spend Spend
}

// NewBridgedSpend is an empty meter.
func NewBridgedSpend() *BridgedSpend { return &BridgedSpend{aux: auxspend.NewTally()} }

// Bridged is a [BridgedSpend]'s running total.
type Bridged struct {
	Aux auxspend.Spent
	// Workers is how many delegated workers ran, and WorkerInput and
	// WorkerOutput what they cost between them.
	Workers      int
	WorkerInput  int
	WorkerOutput int
}

// Total is what the bridged calls have cost so far. Every figure only grows.
// Nil-safe: a run with no meter cost nothing it could count.
func (b *BridgedSpend) Total() Bridged {
	if b == nil {
		return Bridged{}
	}
	b.mu.Lock()
	workers, in, out := b.spend.Workers, b.spend.WorkerInput, b.spend.WorkerOutput
	b.mu.Unlock()
	return Bridged{Aux: b.aux.Total(), Workers: workers, WorkerInput: in, WorkerOutput: out}
}

// meterFor is the meter a bridged surface is built on: the turn and the
// compactor this runner has, tallied on b instead of on this segment.
func (r *Runner) meterFor(b *BridgedSpend) meter {
	return meter{
		turn:    r.cfg.Turn.Context.WithAuxSpend(b.aux),
		compact: r.cfg.Compact.Tallied(b.aux),
		spend:   &b.spend, mu: &b.mu,
	}
}
