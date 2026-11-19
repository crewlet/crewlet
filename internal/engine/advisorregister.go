package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The advisor log, wired: its register entry and its two sides. Every data
// node runs it; the Advisor's duty writes its findings, runs, reports and
// retention, and people's decisions are written from whichever node serves
// them.

// advisorRegistrations is the advisor domain's register entry, a strict,
// identity-claiming domain registered last. It has a barrier, so a read may
// ask for `linearizable`, but a read model of none: the duty decides each
// write in its own snapshot and the page reads at `session`. No record on it
// wakes anybody, so it has no wake feed.
func advisorRegistrations() []registration {
	return []registration{{
		Domain: advisordomain.Domain{},
		NewApplier: func(*stateLog) (statelog.Applier, error) {
			return advisordomain.NewApplier(), nil
		},
		NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
			runner *statelog.Runner) (writeSeams, error) {

			rows, err := advisordomain.NewRows(s.estate().Reader(), spec)
			if err != nil {
				return writeSeams{}, err
			}
			fence := advisordomain.NewFence(s.estate().Reader(), s.nodeID)
			name := advisordomain.Domain{}.Name()
			fence.Floor = s.floorOf(name)
			fence.Ends = s.logEndsOf(name, appendTo, runner)
			fence.Committed = runner.Committed
			return writeSeams{Rows: rows, Fence: fence,
				Gates: advisordomain.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
		},
		Barrier: advisordomain.EncodeBarrier,
		// Its arbitration anchors are replicated rows, removed below the trim
		// floor by a record every applier takes at one position (ADR-0080).
		Trimmed: advisordomain.EncodeTrimmed,
		// Counted rather than assumed: nothing here reads at
		// `linearizable`, so a reader that starts to meets `census_drift`.
		Reads:      &statelog.ReadModel{PerSeatDay: advisordomain.LinearizableReadsPerSeatDay},
		Generation: advisordomain.GenerationRecord{},
		NewGate: func(_ *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			w, err := advisordomain.NewGateWriter(publisher)
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, by iam.Principal, opID, node string,
				readmit bool) (statelog.Result, error) {

				actor := iam.ActorFor(by)
				if readmit {
					return w.ReadmitNode(ctx, actor, opID, node)
				}
				return w.EvictNode(ctx, actor, opID, node)
			}, nil
		},
		// The framework's one horizon: a person's decision is re-asked by the
		// person, and the duty's writes on its next tick.
		OpsRetention: statelog.OpsRetention,
		// Not derived from the disk: its default is its floor
		// ([config.DefaultAdvisorLogMaxBytes]).
		Ceiling: func(stream config.Stream, _ int64) domainCeiling {
			bytes, derived := stream.AdvisorMaxBytes()
			return domainCeiling{Bytes: bytes,
				Field: "stream.advisor_log_max_bytes", Explicit: !derived,
				Floor: config.AdvisorLogMaxBytesFloor, ScalesTo: config.AdvisorLogMaxBytesFloor}
		},
	}}
}

// openAdvisor builds the advisor estate's reader and writer over its running
// domain.
func (c *core) openAdvisor(sl *stateLog) error {
	running := sl.Domain(advisordomain.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no advisor domain: the domain is in " +
			"the register and its stream failed to come up")
	}
	reader, err := advisordomain.NewReader(advisordomain.ReaderOptions{
		DB: sl.estate().Reader(), Log: running.reader,
	})
	if err != nil {
		return fmt.Errorf("engine: advisor reader: %w", err)
	}
	writer, err := advisordomain.NewWriter(advisordomain.WriterDeps{
		Publisher: running.publisher, DB: sl.estate().Reader(),
	})
	if err != nil {
		return fmt.Errorf("engine: advisor writer: %w", err)
	}
	c.advisorReader, c.advisorWriter = reader, writer
	return nil
}

// AdvisorReads is the advisor log's reads through the router, from this
// node's copy while it serves and a data node's otherwise, so a node without
// `data` reads too. False on an engine that reaches no copy.
func (e *Engine) AdvisorReads() (estate.AdvisorReader, bool) {
	if e.router != nil {
		return e.router.AdvisorReads(), true
	}
	if r := e.AdvisorReader(); r != nil {
		return r, true
	}
	return nil, false
}

// AdvisorWrites is the advisor log's writes through the router, answered by a
// publishing data node, so a write is the same call from a node without
// `data`. False on an engine that reaches no writer.
func (e *Engine) AdvisorWrites() (estate.AdvisorWriter, bool) {
	if e.router != nil {
		return e.router.AdvisorWrites(), true
	}
	if w := e.AdvisorWriter(); w != nil {
		return w, true
	}
	return nil, false
}

// AdvisorReader is this node's advisor read side, or nil on an engine with no
// core runtime: a node without `data`.
func (e *Engine) AdvisorReader() *advisordomain.Reader {
	if c := e.core.Load(); c != nil {
		return c.advisorReader
	}
	return nil
}

// AdvisorWriter is this node's advisor write side, or nil on an engine with no
// core runtime.
func (e *Engine) AdvisorWriter() *advisordomain.Writer {
	if c := e.core.Load(); c != nil {
		return c.advisorWriter
	}
	return nil
}
