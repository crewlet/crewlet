package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// The dashboard assistant's state log, wired: its register entry, its two
// sides, its retention through the trash reaper and a person's erasure
// (ADR-0053); internal/assistant reads and writes through them (assistant.go).
// Every data node runs it: the register runs every domain or none
// ([register]), and `ingress` requires `data`, so the drawer reads its own
// node's copy.

// assistantRegistrations is the assistant domain's register entry, its Tier A
// ceiling `stream.assistant_log_max_bytes`.
//
// A strict, identity-claiming domain, so it states its generation record and
// the node gate's writer: the trim counts nodes on this log, and one the gate
// could not write would keep an evicted node counted for ever. It declares a
// barrier, as every identity-claiming log does; its one linearizable reader is
// the object collector (assistantdomain.ObjectEstate), only when it has a file
// to judge, so an unwritten log takes no barrier at boot. No record on it wakes
// a seat, so it has no wake feed.
func assistantRegistrations() []registration {
	return []registration{{
		Domain: assistantdomain.Domain{},
		NewApplier: func(*stateLog) (statelog.Applier, error) {
			return assistantdomain.NewApplier(), nil
		},
		NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
			runner *statelog.Runner) (writeSeams, error) {

			rows, err := assistantdomain.NewRows(s.estate().Reader(), spec)
			if err != nil {
				return writeSeams{}, err
			}
			fence := assistantdomain.NewFence(s.estate().Reader(), s.nodeID)
			name := assistantdomain.Domain{}.Name()
			fence.Floor = s.floorOf(name)
			fence.Ends = s.logEndsOf(name, appendTo, runner)
			fence.Committed = runner.Committed
			return writeSeams{Rows: rows, Fence: fence,
				Gates: assistantdomain.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
		},
		Barrier: assistantdomain.EncodeBarrier,
		Trimmed: assistantdomain.EncodeTrimmed,
		// No seat reads this log, so its read model is zero; the object
		// collector's pins count as the engine's own
		// ([Engine.backgroundBarriers]). A seat reading it at `linearizable`
		// meets `census_drift` on its first barrier.
		Reads:      &statelog.ReadModel{PerSeatDay: 0},
		Generation: assistantdomain.GenerationRecord{},
		NewGate: func(s *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			// No keyring: an eviction names a machine, nothing a person said.
			w, err := assistantdomain.NewGateWriter(publisher)
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
		OpsRetention: statelog.OpsRetention,
		// Not derived from the disk: the log grows with how many people ask
		// the assistant, not with the volume ([config.DefaultAssistantLogMaxBytes]).
		// But scaled down to its 64 MiB floor (a month at ten people) where
		// the broker is short: a feature a company may never turn on must not
		// raise the least a node boots in past the smallest data volume
		// crewlet-operator admits.
		Ceiling: func(stream config.Stream, _ int64) domainCeiling {
			bytes, derived := stream.AssistantMaxBytes()
			return domainCeiling{Bytes: bytes,
				Field: "stream.assistant_log_max_bytes", Explicit: !derived,
				Floor: config.AssistantLogMaxBytesFloor, ScalesTo: config.AssistantLogMaxBytesFloor}
		},
	}}
}

// openAssistant builds the assistant estate's reader and writer over the
// fleet keyring: the writer seals what a person says and is shown, the reader
// opens it. A domain that did not come up is a boot failure.
func (c *core) openAssistant(e *Engine, sl *stateLog) error {
	running := sl.Domain(assistantdomain.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no assistant domain: the domain " +
			"is in the register and its stream failed to come up")
	}
	sealer, err := assistantdomain.NewSealer(e.cipher)
	if err != nil {
		return fmt.Errorf("engine: assistant sealer: %w", err)
	}
	opts := assistantdomain.ReaderOptions{
		DB: sl.estate().Reader(), Log: running.reader, Sealer: sealer,
	}
	// A nil recorder stays a nil interface, not a typed nil the reader calls.
	if e.metrics != nil {
		opts.Metrics = e.metrics
	}
	reader, err := assistantdomain.NewReader(opts)
	if err != nil {
		return fmt.Errorf("engine: assistant reader: %w", err)
	}
	writer, err := assistantdomain.NewWriter(assistantdomain.WriterDeps{
		Publisher: running.publisher, DB: sl.estate().Reader(), Sealer: sealer,
	})
	if err != nil {
		return fmt.Errorf("engine: assistant writer: %w", err)
	}
	c.assistantReader, c.assistantWriter = reader, writer
	return nil
}

// AssistantReader is this node's assistant read side, or nil on an engine with
// no core runtime: a node without `data`, which serves no dashboard.
func (e *Engine) AssistantReader() *assistantdomain.Reader {
	if c := e.core.Load(); c != nil {
		return c.assistantReader
	}
	return nil
}

// AssistantWriter is this node's assistant write side, or nil on an engine with
// no core runtime.
func (e *Engine) AssistantWriter() *assistantdomain.Writer {
	if c := e.core.Load(); c != nil {
		return c.assistantWriter
	}
	return nil
}

// ownerItem prefixes the reaper's id for a person's erasure, beside a
// conversation's id, a uuid that never begins with it.
const ownerItem = "owner:"

// assistantTrash is the assistant's retention and erasure as the trash reaper
// reaches them: conversations untouched past their retention, and those of a
// person the identity estate removed, decided by the machinery every other
// domain's trash is.
//
// One source for both, so a pass that cannot read the term skips its erasures
// too. That is safe because the reaper only backs up
// [Engine.runAssistantErasure], which every data node runs on the removal.
type assistantTrash struct {
	reader assistantRows
	writer assistantPurges

	// removed answers which of the ids the identity estate holds a removal
	// tombstone for ([iamdomain.Reader.Removed]).
	removed func(ctx context.Context, ids []string) ([]string, error)
}

// assistantRows is the slice of the assistant's reader the reaper's source and
// the erasure loop read.
type assistantRows interface {
	Owners(ctx context.Context) ([]uuid.UUID, error)
	InactiveBefore(ctx context.Context, cutoff time.Time, limit int) ([]uuid.UUID, error)
}

// assistantPurges is the slice of the assistant's writer they write through.
type assistantPurges interface {
	EraseOwner(ctx context.Context, c assistantdomain.Call) (statelog.Result, error)
	PurgeInactive(ctx context.Context, by iam.Actor, opID string, id uuid.UUID,
		cutoff time.Time) (statelog.Result, error)
}

var _ trashreap.Termed = assistantTrash{}

func (assistantTrash) Name() string { return assistantdomain.Domain{}.Name() }

// Term is a conversation's retention ([assistantdomain.Retention]), the same
// on every node.
func (assistantTrash) Term(context.Context) (time.Duration, error) {
	return assistantdomain.Retention, nil
}

// TrashedBefore nominates first every removed person who still owns a
// conversation here, then the conversations untouched since before cutoff,
// the longest untouched first.
func (a assistantTrash) TrashedBefore(ctx context.Context, cutoff time.Time,
	limit int) ([]string, error) {

	gone, err := removedOwners(ctx, a.reader, a.removed)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, limit)
	for _, owner := range gone {
		if len(out) == limit {
			return out, nil
		}
		out = append(out, ownerItem+owner)
	}
	ids, err := a.reader.InactiveBefore(ctx, cutoff, limit-len(out))
	if err != nil || len(out) == limit {
		return out, err
	}
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out, nil
}

// PurgeTrashed erases one removed person's conversations, or purges one
// conversation past its retention, as the engine, under the operation the
// reaper derived.
func (a assistantTrash) PurgeTrashed(ctx context.Context, id string, cutoff time.Time,
	opID string) error {

	if owner, ok := strings.CutPrefix(id, ownerItem); ok {
		parsed, err := uuid.Parse(owner)
		if err != nil {
			return fmt.Errorf("%w: the reaper listed %q: %w", trashreap.ErrNotEligible, id, err)
		}
		res, err := a.writer.EraseOwner(ctx, assistantdomain.Call{
			Owner: parsed, By: reaperActor, OpID: opID,
		})
		return reapAnswer(res, err)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("%w: the reaper listed %q: %w", trashreap.ErrNotEligible, id, err)
	}
	res, err := a.writer.PurgeInactive(ctx, reaperActor, opID, parsed, cutoff)
	return reapAnswer(res, err, assistantdomain.ErrNotDue, assistantdomain.ErrNotFound,
		assistantdomain.ErrPurged)
}

// removedOwners is every owner of a conversation here whom the identity estate
// has removed, by id.
func removedOwners(ctx context.Context, reader assistantRows,
	removed func(context.Context, []string) ([]string, error)) ([]string, error) {

	owners, err := reader.Owners(ctx)
	if err != nil || len(owners) == 0 {
		return nil, err
	}
	ids := make([]string, len(owners))
	for i, owner := range owners {
		ids[i] = owner.String()
	}
	return removed(ctx, ids)
}

// assistantSource is the assistant's trash reaper source on this node, false
// where it writes nothing: a mode that publishes nothing, or a core runtime
// missing the assistant's or identity estate's sides.
func (e *Engine) assistantSource() (assistantTrash, bool) {
	c := e.core.Load()
	if c == nil || !e.mode.Publishes() || c.assistantReader == nil ||
		c.assistantWriter == nil || c.iamReader == nil {
		return assistantTrash{}, false
	}
	return assistantTrash{reader: c.assistantReader, writer: c.assistantWriter,
		removed: c.iamReader.Removed}, true
}

// nudgeAssistantErasure tells the erasure loop the identity applier removed
// somebody. It never blocks the apply loop it runs on; a pending signal
// covers this one.
func (e *Engine) nudgeAssistantErasure() {
	select {
	case e.assistantErasure <- struct{}{}:
	default:
	}
}

// runAssistantErasure erases a removed person's conversations as soon as the
// identity applier reports the removal, until ctx ends; one goroutine, owned
// by the core's run context. Every data node asks, and the owner's subject
// arbitrates, so the first lands. A node down or not publishing leaves it to
// the reaper's next pass ([assistantTrash]).
func (e *Engine) runAssistantErasure(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.assistantErasure:
		}
		src, ok := e.assistantSource()
		if !ok {
			continue
		}
		gone, err := removedOwners(ctx, src.reader, src.removed)
		if err != nil {
			if ctx.Err() == nil {
				log.WarnContext(ctx, "assistant_erasure_unread", "error", err,
					"detail", "the trash reaper's next pass erases them")
			}
			continue
		}
		for _, owner := range gone {
			parsed, err := uuid.Parse(owner)
			if err != nil {
				continue
			}
			if _, err := src.writer.EraseOwner(ctx, assistantdomain.Call{
				Owner: parsed, By: reaperActor,
				OpID: statelog.NewOpID(time.Now(), "assistant-erasure"),
			}); err != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "assistant_erasure_failed", "owner", owner,
					"error", err, "detail", "the trash reaper's next pass erases them")
			}
		}
	}
}
