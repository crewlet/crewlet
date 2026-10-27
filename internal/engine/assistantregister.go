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

// THE DASHBOARD ASSISTANT'S STATE LOG, wired: its entry in the register, its
// two sides over the running log, its retention through the trash reaper, and
// a person's erasure from it (ADR-0053). The orchestrator that reads and
// writes through these sides is internal/assistant, wired in assistant.go.
//
// EVERY DATA NODE RUNS IT, whatever its company keeps: the register runs every
// domain or none ([register]), and a node serving the dashboard is a data node
// (`ingress` requires `data`), so the person's drawer reads its own node's
// copy.

// assistantRegistrations is the assistant domain's entry in the register: its
// declaration, applier, write seams, generation record, node gate and Tier A
// ceiling (`stream.assistant_log_max_bytes`, read through
// config.Stream.AssistantMaxBytes).
//
// A STRICT, IDENTITY-CLAIMING DOMAIN, so it states its own generation record
// and the node gate's writer: the trim counts nodes on this log, so one the
// gate could not write would keep an evicted node counted here for ever. It
// declares a BARRIER, as every identity-claiming log does. A person never reads
// the log at `linearizable` (they read their own conversation through the node
// their own writes waited on, and an approval is arbitrated at the broker
// rather than read); the object collector does, before it judges a file's
// object (assistantdomain.ObjectEstate), and only when it has one to judge, so
// a log nobody wrote to gets no barrier at boot. The barrier is also the
// ordinary record the reserve and the eviction machinery are proved against on
// every identity log, this one included. And no wake feed: no record on it
// wakes a seat.
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
		Barrier:    assistantdomain.EncodeBarrier,
		Generation: assistantdomain.GenerationRecord{},
		NewGate: func(s *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			// A GATE WRITER, which holds no keyring: an eviction names a
			// machine and carries nothing a person said.
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
		// NOT DERIVED FROM THE DISK, like the identity estate's and the
		// usage log's: the log grows with how many people ask the
		// assistant, and a larger volume buys none of them
		// ([config.DefaultAssistantLogMaxBytes]).
		//
		// BUT SCALED DOWN TO ITS FIELD'S FLOOR where the broker is short,
		// which the identity estate's and the usage log's are not: the
		// assistant is a feature a company may never turn on, and a fixed
		// 512 MiB counted in full on every fresh node raised the least a
		// node boots in past the 9 GiB a stated broker limit commonly
		// gives, so a deployment sized for everything else stopped
		// booting. Scaled, it reserves its 64 MiB floor there (a month at
		// ten people) and its default wherever the broker has the room.
		Ceiling: func(stream config.Stream, _ int64) domainCeiling {
			bytes, derived := stream.AssistantMaxBytes()
			return domainCeiling{Bytes: bytes,
				Field: "stream.assistant_log_max_bytes", Explicit: !derived,
				Floor: config.AssistantLogMaxBytesFloor, ScalesTo: config.AssistantLogMaxBytesFloor}
		},
	}}
}

// openAssistant builds the assistant estate's reader and writer over its
// running domain, both over the fleet keyring: the writer seals what a person
// says and is shown, and the reader opens it for them. A node whose assistant
// domain did not come up is a boot failure naming the domain.
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
	// A NIL RECORDER STAYS A NIL INTERFACE: one wrapped in it would be a
	// recorder the reader calls into.
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

// assistantRetention is how long a conversation is kept after it was last
// used, read from the company this node runs NOW, at every reaper pass: the
// company document's `assistant.retention_days`, with its default and its
// bounds applied by the block itself ([config.Assistant.Retention]), so an
// apply that changes it reaches the next pass without a restart.
func (e *Engine) assistantRetention() time.Duration {
	return assistantRetentionOf(e.Company())
}

// assistantRetentionOf is the retention a running company keeps, and the
// block's own default on a node running none yet: a node that boots with no
// company still reaps what an earlier company left, at the term an unset
// setting means.
func assistantRetentionOf(c *Company) time.Duration {
	if c == nil || c.Config == nil {
		return config.Assistant{}.Retention()
	}
	return c.Config.Assistant.Retention()
}

// assistantActor is the author of every assistant record the engine writes on
// nobody's behalf: a retention purge and an erasure. The reaper's own name, so
// an operator who finds a purge in the log finds the duty that made it.
var assistantActor = iam.Actor{Name: trashreap.Name, Kind: iam.ActorSystem}

// ownerItem prefixes the reaper's id of a person whose conversations their
// erasure takes, beside a conversation's own id. A conversation's id is a uuid
// and never begins with it.
const ownerItem = "owner:"

// assistantTrash is the assistant's retention and erasure as the trash reaper
// reaches them: conversations untouched past their retention, and the
// conversations of a person the identity estate has removed. The reaper is the
// fleet singleton that already runs hourly with a lease, a report and derived
// operation ids, so a retention purge and an erasure are decided by exactly
// the machinery every other domain's trash is.
type assistantTrash struct {
	reader assistantRows
	writer assistantPurges
	term   func() time.Duration

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

// Term is the conversation's retention, asked at every pass.
func (a assistantTrash) Term() time.Duration { return a.term() }

// TrashedBefore nominates, first, every person the identity estate removed who
// still owns a conversation here (their erasure), and then the conversations
// untouched since before cutoff, the longest untouched first.
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
			Owner: parsed, By: assistantActor, OpID: opID,
		})
		return reapAnswer(res, err)
	}
	parsed, err := uuid.Parse(id)
	if err != nil {
		return fmt.Errorf("%w: the reaper listed %q: %w", trashreap.ErrNotEligible, id, err)
	}
	res, err := a.writer.PurgeInactive(ctx, assistantActor, opID, parsed, cutoff)
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

// assistantSource is the assistant's source for the trash reaper on this node,
// false where this node writes nothing: a mode that publishes nothing, or a
// core runtime without the assistant's or the identity estate's sides.
func (e *Engine) assistantSource() (assistantTrash, bool) {
	c := e.core.Load()
	if c == nil || !e.mode.Publishes() || c.assistantReader == nil ||
		c.assistantWriter == nil || c.iamReader == nil {
		return assistantTrash{}, false
	}
	return assistantTrash{reader: c.assistantReader, writer: c.assistantWriter,
		term: e.assistantRetention, removed: c.iamReader.Removed}, true
}

// nudgeAssistantErasure tells this node's erasure loop that the identity
// applier removed somebody. It never blocks: it runs on the identity apply
// loop's own goroutine, and a signal already pending covers this one.
func (e *Engine) nudgeAssistantErasure() {
	select {
	case e.assistantErasure <- struct{}{}:
	default:
	}
}

// runAssistantErasure erases a removed person's conversations as soon as the
// identity applier reports the removal, until ctx ends. ONE GOROUTINE, owned
// by the core's run context.
//
// THE REAPER IS THE BACKSTOP, not this: a node that was down when the removal
// applied, or one in a mode that publishes nothing, leaves the erasure to the
// reaper's next pass, which nominates the same people. Every data node hears
// the removal from its own applier and asks; the erasure is arbitrated on the
// owner's subject, so the first lands and the rest find it done.
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
				Owner: parsed, By: assistantActor,
				OpID: statelog.NewOpID(time.Now(), "assistant-erasure"),
			}); err != nil && ctx.Err() == nil {
				log.WarnContext(ctx, "assistant_erasure_failed", "owner", owner,
					"error", err, "detail", "the trash reaper's next pass erases them")
			}
		}
	}
}
