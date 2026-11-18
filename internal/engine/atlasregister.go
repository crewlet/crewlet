package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// The atlas log, wired: its register entry, its two sides, and its history's
// retention through the trash reaper (ADR-0126). Every data node runs it.

// atlasRegistrations is the atlas domain's register entry, a strict,
// identity-claiming domain registered last. It has a barrier, so a read may
// ask for `linearizable`, but a read model of none: an edit decides from the
// entry's version in its own snapshot, and reads default to `session`. No
// record on it wakes anybody, so it has no wake feed.
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
		// Counted rather than assumed: nothing here reads at
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
		// The framework's one horizon: an edit is re-asked by its person, a
		// purge or expiry by its owner, on every write's rhythm.
		OpsRetention: statelog.OpsRetention,
		// Not derived from the disk, and scaled down to its floor where the
		// broker is short, as the access log's is ([config.DefaultAtlasLogMaxBytes]).
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

// AtlasReads is the atlas log's reads through the router, from this node's
// copy while it serves and a data node's otherwise, so a node without `data`
// reads too. False on an engine that reaches no copy.
func (e *Engine) AtlasReads() (estate.AtlasReader, bool) {
	if e.router != nil {
		return e.router.AtlasReads(), true
	}
	if r := e.AtlasReader(); r != nil {
		return r, true
	}
	return nil, false
}

// AtlasWrites is the atlas log's writes through the router, answered by a
// publishing data node, so a write is the same call from a node without
// `data`. False on an engine that reaches no writer.
func (e *Engine) AtlasWrites() (estate.AtlasWriter, bool) {
	if e.router != nil {
		return e.router.AtlasWrites(), true
	}
	if w := e.AtlasWriter(); w != nil {
		return w, true
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

// atlasRetention is how long the atlas keeps its history, asked at every
// reaper pass: `atlas.retention_days` of the fleet's activated revision, never
// this node's own, since an expiry is a record every data node applies
// ([Engine.assistantRetention]).
func (e *Engine) atlasRetention(ctx context.Context) (time.Duration, error) {
	company, err := e.servedFleetRevision(ctx)
	if err != nil {
		return 0, err
	}
	return atlasRetentionOf(company), nil
}

// atlasRetentionOf is the retention a company's document states, the default
// for a company built in code.
func atlasRetentionOf(c *Company) time.Duration {
	if c == nil || c.Config == nil {
		return config.Atlas{}.Retention()
	}
	return c.Config.Atlas.Retention()
}

// atlasTrash is the atlas log's retention as the trash reaper reaches it: the
// hours of history past `atlas.retention_days`, oldest first, one expiry each.
// Each operation derives from the reaper's slot and the hour, so two reapers in
// one slot publish the same record.
type atlasTrash struct {
	reader atlasHours
	writer atlasExpiries
	term   func(context.Context) (time.Duration, error)
}

// atlasHours is the slice of the atlas reader the reaper reads.
type atlasHours interface {
	ExpirableHours(ctx context.Context, cutoff time.Time, limit int) ([]time.Time, error)
}

// atlasExpiries is the slice of the atlas writer the reaper writes through.
type atlasExpiries interface {
	Expire(ctx context.Context, c atlasdomain.Call, hour, cutoff time.Time) (statelog.Result, error)
}

var _ trashreap.Termed = atlasTrash{}

func (atlasTrash) Name() string { return atlasdomain.Domain{}.Name() }

// Term is the history's retention, asked at every pass.
func (a atlasTrash) Term(ctx context.Context) (time.Duration, error) { return a.term(ctx) }

// TrashedBefore nominates the hours that hold history past cutoff.
func (a atlasTrash) TrashedBefore(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	hours, err := a.reader.ExpirableHours(ctx, cutoff, limit)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(hours))
	for _, hour := range hours {
		out = append(out, atlasdomain.Bucket(hour))
	}
	return out, nil
}

// PurgeTrashed expires one hour of history past cutoff, decided again in the
// write's own snapshot.
func (a atlasTrash) PurgeTrashed(ctx context.Context, id string, cutoff time.Time, opID string) error {
	hour, err := atlasdomain.ParseBucket(id)
	if err != nil {
		return fmt.Errorf("%w: the reaper listed %q: %w", trashreap.ErrNotEligible, id, err)
	}
	res, err := a.writer.Expire(ctx, atlasdomain.Call{By: reaperActor, OpID: opID}, hour, cutoff)
	return reapAnswer(res, err, atlasdomain.ErrNotDue)
}

// atlasSource is the atlas log's source for the trash reaper on this node,
// false where this node writes nothing.
func (e *Engine) atlasSource() (atlasTrash, bool) {
	c := e.core.Load()
	if c == nil || !e.mode.Publishes() || c.atlasReader == nil || c.atlasWriter == nil {
		return atlasTrash{}, false
	}
	return atlasTrash{reader: c.atlasReader, writer: c.atlasWriter, term: e.atlasRetention}, true
}
