package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// THE ACCESS LOG, wired: its entry in the register, its two sides over the
// running log, and its retention and a removed seat's forgetting through the
// trash reaper (ADR-0103). Every data node runs it, whatever its company keeps.

// accessRegistrations is the access domain's entry in the register: the
// TWELFTH domain and the TENTH strict, identity-claiming one, registered last.
//
// A BARRIER AND A READ MODEL, because a turn's dispatch reads its seat's
// baseline at `linearizable`; no wake feed, because a seat is told at its next
// turn rather than woken.
func accessRegistrations() []registration {
	return []registration{{
		Domain: accessdomain.Domain{},
		NewApplier: func(*stateLog) (statelog.Applier, error) {
			return accessdomain.NewApplier(), nil
		},
		NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
			runner *statelog.Runner) (writeSeams, error) {

			rows, err := accessdomain.NewRows(s.estate().Reader(), spec)
			if err != nil {
				return writeSeams{}, err
			}
			fence := accessdomain.NewFence(s.estate().Reader(), s.nodeID)
			name := accessdomain.Domain{}.Name()
			fence.Floor = s.floorOf(name)
			fence.Ends = s.logEndsOf(name, appendTo, runner)
			fence.Committed = runner.Committed
			return writeSeams{Rows: rows, Fence: fence,
				Gates: accessdomain.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
		},
		Barrier: accessdomain.EncodeBarrier,
		// Its arbitration anchors are replicated rows, removed below the trim
		// floor by a record every applier takes at one position (ADR-0080).
		Trimmed:    accessdomain.EncodeTrimmed,
		Reads:      &statelog.ReadModel{PerSeatDay: accessdomain.LinearizableReadsPerSeatDay},
		Generation: accessdomain.GenerationRecord{},
		NewGate: func(_ *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			w, err := accessdomain.NewGateWriter(publisher)
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
		// The framework's one horizon: every write here is re-asked by the
		// turn or the duty that owns it, on the rhythm every write re-asks.
		OpsRetention: statelog.OpsRetention,
		// NOT DERIVED FROM THE DISK, and scaled down to its field's floor where
		// the broker is short, for the assistant's and the executor log's
		// reason ([config.DefaultAccessLogMaxBytes]).
		Ceiling: func(stream config.Stream, _ int64) domainCeiling {
			bytes, derived := stream.AccessMaxBytes()
			return domainCeiling{Bytes: bytes,
				Field: "stream.access_log_max_bytes", Explicit: !derived,
				Floor: config.AccessLogMaxBytesFloor, ScalesTo: config.AccessLogMaxBytesFloor}
		},
	}}
}

// openAccess builds the access estate's reader and writer over its running
// domain, the writer sealing a command's text under the fleet keyring.
func (c *core) openAccess(e *Engine, sl *stateLog) error {
	running := sl.Domain(accessdomain.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no access domain: the domain is in " +
			"the register and its stream failed to come up")
	}
	sealer, err := accessdomain.NewSealer(e.cipher)
	if err != nil {
		return fmt.Errorf("engine: access sealer: %w", err)
	}
	reader, err := accessdomain.NewReader(accessdomain.ReaderOptions{
		DB: sl.estate().Reader(), Log: running.reader,
	})
	if err != nil {
		return fmt.Errorf("engine: access reader: %w", err)
	}
	writer, err := accessdomain.NewWriter(accessdomain.WriterDeps{
		Publisher: running.publisher, DB: sl.estate().Reader(), Sealer: sealer,
	})
	if err != nil {
		return fmt.Errorf("engine: access writer: %w", err)
	}
	c.accessReader, c.accessWriter, c.accessSealer = reader, writer, sealer
	return nil
}

// AccessReader is this node's access read side, or nil on an engine with no
// core runtime: a node without `data`.
func (e *Engine) AccessReader() *accessdomain.Reader {
	if c := e.core.Load(); c != nil {
		return c.accessReader
	}
	return nil
}

// AccessKeyring is the access log's half of a keyring rotation, or nil on an
// engine with no core runtime: the SSH commands and the verdicts it recorded,
// each one's sealed words moved by a record of its own ([accessdomain.Writer.Reseal]).
func (e *Engine) AccessKeyring() *AccessKeyring {
	reader, writer := e.AccessReader(), e.AccessWriter()
	if reader == nil || writer == nil {
		return nil
	}
	return &AccessKeyring{reader: reader, writer: writer}
}

// AccessKeyring counts and moves the access log's sealed commands and verdicts: see
// [Engine.AccessKeyring].
type AccessKeyring struct {
	reader *accessdomain.Reader
	writer *accessdomain.Writer
}

// SealedKeys counts the commands and verdicts by the keyring key each is sealed under.
func (k *AccessKeyring) SealedKeys(ctx context.Context) (accessdomain.SealedCount, error) {
	return k.reader.SealedKeys(ctx)
}

// Reseal moves every command's text and verdict's words onto this node's
// active keyring key, as the engine.
func (k *AccessKeyring) Reseal(ctx context.Context) (accessdomain.ResealReport, error) {
	return k.writer.Reseal(ctx, sshAuditor, k.reader)
}

// AccessWriter is this node's access write side, or nil on an engine with no
// core runtime.
func (e *Engine) AccessWriter() *accessdomain.Writer {
	if c := e.core.Load(); c != nil {
		return c.accessWriter
	}
	return nil
}

// accessRetention is how long an access record is kept, asked at every reaper
// pass: the `access_history.retention_days` of the revision the fleet's
// activation pointer names, read on a node that runs that very activation.
// The fleet's revision, never this node's own: a retention delete is a record
// every data node applies.
func (e *Engine) accessRetention(ctx context.Context) (time.Duration, error) {
	company, err := e.servedFleetRevision(ctx)
	if err != nil {
		return 0, err
	}
	return accessRetentionOf(company), nil
}

// accessRetentionOf is the retention a company's document states, the block's
// own default where a company built in code states none.
func accessRetentionOf(c *Company) time.Duration {
	if c == nil || c.Config == nil {
		return config.AccessHistory{}.Retention()
	}
	return c.Config.AccessHistory.Retention()
}

// accessReaper is the author of every retention delete and every forgetting:
// the reaper's own name, so an operator finds the duty that made it.
var accessReaper = iam.Actor{Name: trashreap.Name, Kind: iam.ActorSystem}

// forgetItem prefixes the reaper's id of a removed seat whose told records go,
// beside an hour's. A seat's handle holds no colon and an hour's id begins
// with its year.
const forgetItem = "seat:"

// accessTrash is the access log's retention as the trash reaper reaches it:
// the seats the company no longer has, whose told records go, and the hours of
// records past `access_history.retention_days`, oldest first. The reaper
// derives each operation from its slot and the item, so two reapers in one slot
// publish the same record.
type accessTrash struct {
	reader accessRows
	writer accessDeletes
	term   func(context.Context) (time.Duration, error)

	// seats is every agent seat's handle in the revision the fleet runs, or why
	// this node cannot read it.
	seats func(context.Context) (map[string]bool, error)
}

// accessRows is the slice of the access reader the reaper reads.
type accessRows interface {
	Handles(ctx context.Context) ([]string, error)
	ExpirableHours(ctx context.Context, cutoff time.Time, limit int) ([]time.Time, error)
}

// accessDeletes is the slice of the access writer the reaper writes through.
type accessDeletes interface {
	Forget(ctx context.Context, c accessdomain.Call, handle string) (statelog.Result, error)
	Expire(ctx context.Context, c accessdomain.Call, hour, cutoff time.Time) (statelog.Result, error)
}

var _ trashreap.Termed = accessTrash{}

func (accessTrash) Name() string { return accessdomain.Domain{}.Name() }

// Term is the records' retention, asked at every pass.
func (a accessTrash) Term(ctx context.Context) (time.Duration, error) { return a.term(ctx) }

// TrashedBefore nominates every seat holding a told record that the fleet's
// revision no longer has, then the hours past cutoff.
func (a accessTrash) TrashedBefore(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	gone, err := a.removedSeats(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, limit)
	for _, handle := range gone {
		if len(out) == limit {
			return out, nil
		}
		out = append(out, forgetItem+handle)
	}
	hours, err := a.reader.ExpirableHours(ctx, cutoff, limit-len(out))
	if err != nil {
		return nil, err
	}
	for _, hour := range hours {
		out = append(out, accessdomain.Bucket(hour))
	}
	return out, nil
}

// removedSeats is every seat holding a told record that the company no longer
// gives an agent seat.
func (a accessTrash) removedSeats(ctx context.Context) ([]string, error) {
	held, err := a.reader.Handles(ctx)
	if err != nil || len(held) == 0 {
		return nil, err
	}
	present, err := a.seats(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, handle := range held {
		if !present[handle] {
			out = append(out, handle)
		}
	}
	return out, nil
}

// PurgeTrashed forgets one removed seat's told records, decided again against
// the fleet's revision, or expires one hour past cutoff.
func (a accessTrash) PurgeTrashed(ctx context.Context, id string, cutoff time.Time, opID string) error {
	call := accessdomain.Call{By: accessReaper, OpID: opID}
	if handle, ok := strings.CutPrefix(id, forgetItem); ok {
		present, err := a.seats(ctx)
		if err != nil {
			return err
		}
		if present[handle] {
			return fmt.Errorf("%w: seat %s is back in the company", trashreap.ErrNotEligible, handle)
		}
		res, err := a.writer.Forget(ctx, call, handle)
		return reapAnswer(res, err, accessdomain.ErrInvalid)
	}
	hour, err := accessdomain.ParseBucket(id)
	if err != nil {
		return fmt.Errorf("%w: the reaper listed %q: %w", trashreap.ErrNotEligible, id, err)
	}
	res, err := a.writer.Expire(ctx, call, hour, cutoff)
	return reapAnswer(res, err, accessdomain.ErrNotDue)
}

// accessSource is the access log's source for the trash reaper on this node,
// false where this node writes nothing.
func (e *Engine) accessSource() (accessTrash, bool) {
	c := e.core.Load()
	if c == nil || !e.mode.Publishes() || c.accessReader == nil || c.accessWriter == nil {
		return accessTrash{}, false
	}
	return accessTrash{reader: c.accessReader, writer: c.accessWriter,
		term: e.accessRetention, seats: e.fleetAgentSeats}, true
}

// fleetAgentSeats is every agent seat's handle in the revision the fleet runs,
// read on a node serving that very activation; unknown is an error, never an
// empty company, which would forget every seat.
func (e *Engine) fleetAgentSeats(ctx context.Context) (map[string]bool, error) {
	company, err := e.servedFleetRevision(ctx)
	if err != nil {
		return nil, err
	}
	if company == nil {
		return nil, errors.New("engine: the fleet runs no revision this node can read")
	}
	seats := company.Seats()
	out := make(map[string]bool, len(seats))
	for _, s := range seats {
		out[s.Handle] = true
	}
	return out, nil
}
