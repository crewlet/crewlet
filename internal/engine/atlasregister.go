package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ATLAS LOG, wired: its entry in the register and its two sides over the
// running log (ADR-0126). Every data node runs it, whatever its company keeps.

// atlasRegistrations is the atlas domain's entry in the register: the
// THIRTEENTH domain and the ELEVENTH strict, identity-claiming one, registered
// last.
//
// A BARRIER, so a read may ask for `linearizable`, and a read model of none: a
// person's edit decides from the entry's version in its own snapshot and every
// read of the atlas is at its caller's level, `session` by default. No wake
// feed: no record on this log wakes anybody.
func atlasRegistrations() []registration {
	return []registration{{
		Domain: atlasdomain.Domain{},
		NewApplier: func(*stateLog) (statelog.Applier, error) {
			return atlasdomain.NewApplier(), nil
		},
		NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
			runner *statelog.Runner) (writeSeams, error) {

			rows, err := atlasdomain.NewRows(s.estate().Reader(), spec)
			if err != nil {
				return writeSeams{}, err
			}
			fence := atlasdomain.NewFence(s.estate().Reader(), s.nodeID)
			name := atlasdomain.Domain{}.Name()
			fence.Floor = s.floorOf(name)
			fence.Ends = s.logEndsOf(name, appendTo, runner)
			fence.Committed = runner.Committed
			return writeSeams{Rows: rows, Fence: fence,
				Gates: atlasdomain.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
		},
		Barrier: atlasdomain.EncodeBarrier,
		// Its arbitration anchors are replicated rows, removed below the trim
		// floor by a record every applier takes at one position (ADR-0080).
		Trimmed: atlasdomain.EncodeTrimmed,
		// NO READ TAKES A BARRIER ON THIS LOG, counted rather than assumed,
		// for the identity estate's reason: nothing here reads at
		// `linearizable`, so a reader that starts to meets `census_drift`.
		Reads:      &statelog.ReadModel{PerSeatDay: atlasdomain.LinearizableReadsPerSeatDay},
		Generation: atlasdomain.GenerationRecord{},
		NewGate: func(_ *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			w, err := atlasdomain.NewGateWriter(publisher)
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
		// The framework's one horizon: a person's edit is re-asked by the
		// person, and a purge or an expiry by whoever owns it, on the rhythm
		// every write re-asks.
		OpsRetention: statelog.OpsRetention,
		// NOT DERIVED FROM THE DISK, and scaled down to its field's floor where
		// the broker is short, for the access log's reason
		// ([config.DefaultAtlasLogMaxBytes]).
		Ceiling: func(stream config.Stream, _ int64) domainCeiling {
			bytes, derived := stream.AtlasMaxBytes()
			return domainCeiling{Bytes: bytes,
				Field: "stream.atlas_log_max_bytes", Explicit: !derived,
				Floor: config.AtlasLogMaxBytesFloor, ScalesTo: config.AtlasLogMaxBytesFloor}
		},
	}}
}

// openAtlas builds the atlas estate's reader and writer over its running domain.
func (c *core) openAtlas(sl *stateLog) error {
	running := sl.Domain(atlasdomain.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no atlas domain: the domain is in " +
			"the register and its stream failed to come up")
	}
	reader, err := atlasdomain.NewReader(atlasdomain.ReaderOptions{
		DB: sl.estate().Reader(), Log: running.reader,
	})
	if err != nil {
		return fmt.Errorf("engine: atlas reader: %w", err)
	}
	writer, err := atlasdomain.NewWriter(atlasdomain.WriterDeps{
		Publisher: running.publisher, DB: sl.estate().Reader(),
	})
	if err != nil {
		return fmt.Errorf("engine: atlas writer: %w", err)
	}
	c.atlasReader, c.atlasWriter = reader, writer
	return nil
}

// AtlasReads is the atlas log's reads as this node reaches them: through the
// router, which answers from this node's own copy while it serves and from a
// data node's otherwise, so a node without `data` reads the atlas too. False
// on an engine that reaches no copy at all.
func (e *Engine) AtlasReads() (estate.AtlasReader, bool) {
	if e.router != nil {
		return e.router.AtlasReads(), true
	}
	if r := e.AtlasReader(); r != nil {
		return r, true
	}
	return nil, false
}

// AtlasReader is this node's atlas read side, or nil on an engine with no core
// runtime: a node without `data`.
func (e *Engine) AtlasReader() *atlasdomain.Reader {
	if c := e.core.Load(); c != nil {
		return c.atlasReader
	}
	return nil
}

// AtlasWriter is this node's atlas write side, or nil on an engine with no core
// runtime.
func (e *Engine) AtlasWriter() *atlasdomain.Writer {
	if c := e.core.Load(); c != nil {
		return c.atlasWriter
	}
	return nil
}
