package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// THE EXECUTOR LOG, wired: its entry in the register, its two sides over the
// running log and the store a session's sealed bytes stream through, its
// retention through the trash reaper, and a removed seat's sessions through
// the mailbox retirement (ADR-0072).
//
// EVERY DATA NODE RUNS IT, whatever its company keeps: the register runs every
// domain or none ([register]). Nothing in this build writes a session yet;
// the capture and the restore are the executor's, and they call the sides
// built here.

// executorRegistrations is the executor domain's entry in the register: the
// TENTH domain and the EIGHTH strict, identity-claiming one. Its declaration,
// applier, write seams, barrier, read model, generation record, node gate and
// Tier A ceiling (`stream.executor_log_max_bytes`, read through
// config.Stream.ExecutorMaxBytes).
//
// A BARRIER AND A READ MODEL, because a seat reads this log at `linearizable`:
// a conversation's next turn reads its head through the read index, so a node
// that has not applied the previous turn's end (published by another node)
// waits for it rather than serving an older head. And no wake feed: no record
// on it wakes a seat.
func executorRegistrations() []registration {
	return []registration{{
		Domain: executordomain.Domain{},
		NewApplier: func(*stateLog) (statelog.Applier, error) {
			return executordomain.NewApplier(), nil
		},
		NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
			runner *statelog.Runner) (writeSeams, error) {

			rows, err := executordomain.NewRows(s.estate().Reader(), spec)
			if err != nil {
				return writeSeams{}, err
			}
			fence := executordomain.NewFence(s.estate().Reader(), s.nodeID)
			name := executordomain.Domain{}.Name()
			fence.Floor = s.floorOf(name)
			fence.Ends = s.logEndsOf(name, appendTo, runner)
			fence.Committed = runner.Committed
			return writeSeams{Rows: rows, Fence: fence,
				Gates: executordomain.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
		},
		Barrier: executordomain.EncodeBarrier,
		// ITS OWN READ MODEL, counted from what a turn's launches read at
		// `linearizable` — see [executordomain.LinearizableReadsPerSeatDay],
		// which is also the barrier term in the log's ceiling
		// ([config.DefaultExecutorLogMaxBytes]).
		Reads:      &statelog.ReadModel{PerSeatDay: executordomain.LinearizableReadsPerSeatDay},
		Generation: executordomain.GenerationRecord{},
		NewGate: func(_ *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			// A GATE WRITER, which reads nothing of a seat's sessions: an
			// eviction names a machine.
			w, err := executordomain.NewGateWriter(publisher)
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
		// THE FRAMEWORK'S ONE HORIZON: ADR-0029 judges a retry against it.
		// A capture's or an end's operation is re-asked by the turn that
		// owns it, on the rhythm every turn re-asks its writes.
		OpsRetention: statelog.OpsRetention,
		// NOT DERIVED FROM THE DISK, like the assistant's: the log grows with
		// the turns the company's seats run, and a larger volume buys none of
		// them ([config.DefaultExecutorLogMaxBytes]).
		//
		// AND SCALED DOWN TO ITS FIELD'S FLOOR where the broker is short, for
		// the assistant's reason: counted at its 512 MiB on every fresh node
		// it would move the least a node boots in past the smallest data
		// volume crewlet-operator admits; scaled, it reserves its 64 MiB
		// floor there, inside the room the nine logs before it leave
		// (TestTheExecutorLogFitsBesideTheNineAtTheOperatorsSmallestVolumes).
		Ceiling: func(stream config.Stream, _ int64) domainCeiling {
			bytes, derived := stream.ExecutorMaxBytes()
			return domainCeiling{Bytes: bytes,
				Field: "stream.executor_log_max_bytes", Explicit: !derived,
				Floor: config.ExecutorLogMaxBytesFloor, ScalesTo: config.ExecutorLogMaxBytesFloor}
		},
	}}
}

// openExecutor builds the executor estate's reader and writer over its running
// domain. A node whose executor domain did not come up is a boot failure
// naming the domain.
func (c *core) openExecutor(sl *stateLog) error {
	running := sl.Domain(executordomain.Domain{}.Name())
	if running == nil {
		return fmt.Errorf("engine: this node runs no executor domain: the domain " +
			"is in the register and its stream failed to come up")
	}
	reader, err := executordomain.NewReader(executordomain.ReaderOptions{
		DB: sl.estate().Reader(), Log: running.reader,
	})
	if err != nil {
		return fmt.Errorf("engine: executor reader: %w", err)
	}
	writer, err := executordomain.NewWriter(executordomain.WriterDeps{
		Publisher: running.publisher, DB: sl.estate().Reader(),
	})
	if err != nil {
		return fmt.Errorf("engine: executor writer: %w", err)
	}
	c.executorReader, c.executorWriter = reader, writer
	return nil
}

// ExecutorReader is this node's executor read side, or nil on an engine with no
// core runtime: a node without `data`, which reaches the estate through one
// that has it.
func (e *Engine) ExecutorReader() *executordomain.Reader {
	if c := e.core.Load(); c != nil {
		return c.executorReader
	}
	return nil
}

// ExecutorWriter is this node's executor write side, or nil on an engine with
// no core runtime.
func (e *Engine) ExecutorWriter() *executordomain.Writer {
	if c := e.core.Load(); c != nil {
		return c.executorWriter
	}
	return nil
}

// ExecutorSessions is the store a session's sealed bytes stream into and out
// of: the fleet's object store, sealed under the Tier A keyring's stream
// format. Nil on a node with no object store, which captures and restores no
// session, and an error where the keyring cannot seal a stream (a key id no
// header can name), which is the operator's to fix.
//
// OVER THE TIER A KEYRING, the one that seals the company's secret store and
// signs every state-log record, so a keyring rotation is one gesture for all
// of them; a session sealed under a key the ring has since dropped reads as
// executordomain.ErrUnreadable, and the census GET /secrets keeps of sessions
// by key (`executor_values`) is what an operator waits out before dropping it.
func (e *Engine) ExecutorSessions() (*executordomain.SessionStore, error) {
	objects := e.Objects()
	if objects == nil || e.boot == nil {
		return nil, nil
	}
	ring, err := e.boot.Secrets.Keyring()
	if err != nil {
		return nil, fmt.Errorf("engine: executor sessions: %w", err)
	}
	sealer, err := secrets.NewStreamSealer(ring)
	if err != nil {
		return nil, fmt.Errorf("engine: executor sessions: %w", err)
	}
	return executordomain.NewSessionStore(objects, sealer)
}

// ExecutorSessionBudget is the per-seat byte budget a capture carries:
// `turn_engine.conversation_session.max_bytes_per_seat` of the revision the
// fleet's activation pointer names ([Engine.fleetCompany]), or why this node
// cannot know it.
//
// THE FLEET'S REVISION, NEVER THIS NODE'S OWN, for [Engine.executorRetention]'s
// reason: the budget decides which ended sessions every node releases, so a
// node serving a revision the fleet did not take must not decide it. A capture
// that cannot read it is not recorded, and its turn's next launch starts from
// the session the row already names.
func (e *Engine) ExecutorSessionBudget(ctx context.Context) (int64, error) {
	company, err := e.fleetCompany(ctx, e.Company())
	if err != nil {
		return 0, err
	}
	if company.Config == nil {
		return config.DefaultSessionBytesPerSeat, nil
	}
	return company.Config.TurnEngine.ConversationSession.SessionBudget(), nil
}

// executorRetention is how long a seat's executor session is kept after its
// row was last touched, asked at every reaper pass: the
// `turn_engine.conversation_session.retention_days` of the revision the fleet's
// activation pointer names, read on a node that runs that very activation
// ([Engine.fleetCompany]), or an error saying why this node cannot know it.
//
// THE FLEET'S REVISION, NEVER THIS NODE'S OWN, for [Engine.assistantRetention]'s
// reason: a retention delete is a record every data node applies, and a node
// that refused the fleet's revision, or booted on an import the fleet did not
// take, may read a term the company has since lengthened. An unknown term
// deletes nothing of this source ([trashreap.Termed]).
func (e *Engine) executorRetention(ctx context.Context) (time.Duration, error) {
	company, err := e.fleetCompany(ctx, e.Company())
	if err != nil {
		return 0, err
	}
	return executorRetentionOf(company), nil
}

// executorRetentionOf is the retention a company's document states, the
// block's own default where a company built in code states none.
func executorRetentionOf(c *Company) time.Duration {
	days := config.DefaultConversationSession().RetentionDays
	if c.Config != nil {
		days = c.Config.TurnEngine.ConversationSession.RetentionDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// executorReaper is the author of every retention delete: the reaper's own
// name, so an operator who finds one in the log finds the duty that made it.
var executorReaper = iam.Actor{Name: trashreap.Name, Kind: iam.ActorSystem}

// seatRetirement is the author of every removed seat's delete: the
// maintenance duty the mailbox retirement runs under.
var seatRetirement = iam.Actor{Name: maintenanceDutyName, Kind: iam.ActorSystem}

// executorTrash is the executor sessions' retention as the trash reaper
// reaches it: every turn whose row (or, once an absorption took the row, its
// tombstone) was last touched before the term, never one whose run is live.
// The reaper is the fleet singleton that already runs hourly with a lease, a
// report and derived operation ids, so a retention delete is decided by
// exactly the machinery every other domain's trash is.
type executorTrash struct {
	reader executorRows
	writer executorDeletes
	term   func(context.Context) (time.Duration, error)

	// live answers the turns whose runs the fleet's run records still hold.
	live func(context.Context) (map[string]bool, error)
}

// executorRows is the slice of the executor's reader the source reads.
type executorRows interface {
	Expired(ctx context.Context, cutoff time.Time, limit int) ([]executordomain.Expiry, error)
}

// executorDeletes is the slice of the executor's writer it writes through.
type executorDeletes interface {
	DeleteExpired(ctx context.Context, c executordomain.Call, handle, conversationKey,
		turn string, cutoff time.Time) (statelog.Result, error)
}

var _ trashreap.Termed = executorTrash{}

func (executorTrash) Name() string { return executordomain.Domain{}.Name() }

// Term is the session's retention, asked at every pass, or why this node
// cannot know it.
func (x executorTrash) Term(ctx context.Context) (time.Duration, error) { return x.term(ctx) }

// TrashedBefore nominates the turns last touched before cutoff, the longest
// untouched first, leaving out every turn whose run is still live.
//
// A RUN THE FLEET STILL HOLDS IS NEVER AGED OUT, whatever its row's age: a
// turn parked on a person's answer for longer than the retention is resumed
// from the session its row names. And a pass that cannot read the run records
// nominates nothing, because unknown is never "nothing is running".
func (x executorTrash) TrashedBefore(ctx context.Context, cutoff time.Time,
	limit int) ([]string, error) {

	live, err := x.live(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the fleet's live runs, without which no session "+
			"is aged out: %w", err)
	}
	expired, err := x.reader.Expired(ctx, cutoff, limit+len(live))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, min(limit, len(expired)))
	for _, e := range expired {
		if live[e.Turn] {
			continue
		}
		out = append(out, expiryItem(e))
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// PurgeTrashed deletes one turn past its retention as the reaper, under the
// operation the reaper derived from its slot and the turn.
func (x executorTrash) PurgeTrashed(ctx context.Context, id string, cutoff time.Time,
	opID string) error {

	handle, turn, conversationKey, ok := parseExpiryItem(id)
	if !ok {
		return fmt.Errorf("%w: the reaper listed %q, which names no seat's turn",
			trashreap.ErrNotEligible, id)
	}
	res, err := x.writer.DeleteExpired(ctx, executordomain.Call{By: executorReaper, OpID: opID},
		handle, conversationKey, turn, cutoff)
	return reapAnswer(res, err, executordomain.ErrNotDue, executordomain.ErrNotFound)
}

// expiryItem is the reaper's id for one turn: the seat, the turn and the
// conversation's key, in that order, a slash apart. A handle and a turn id hold
// no slash; the key, which may, is last and taken whole.
func expiryItem(e executordomain.Expiry) string {
	return e.Handle + "/" + e.Turn + "/" + e.ConversationKey
}

// parseExpiryItem is [expiryItem]'s inverse.
func parseExpiryItem(id string) (handle, turn, conversationKey string, ok bool) {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// executorSource is the executor sessions' source for the trash reaper on this
// node, false where this node writes nothing: a mode that publishes nothing, a
// core runtime without the executor's sides, or a node with no fleet store to
// read the live runs from.
func (e *Engine) executorSource() (executorTrash, bool) {
	c := e.core.Load()
	if c == nil || !e.mode.Publishes() || c.executorReader == nil ||
		c.executorWriter == nil || e.backends == nil || e.backends.Fleet == nil {
		return executorTrash{}, false
	}
	runs := sandbox.NewCoordStore(e.backends.Fleet)
	return executorTrash{reader: c.executorReader, writer: c.executorWriter,
		term: e.executorRetention, live: liveTurns(runs)}, true
}

// liveTurns reads every turn the fleet's run records still hold.
func liveTurns(runs interface {
	ListActive(ctx context.Context) ([]sandbox.PendingRun, error)
}) func(context.Context) (map[string]bool, error) {
	return func(ctx context.Context) (map[string]bool, error) {
		active, err := runs.ListActive(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]bool, len(active))
		for _, run := range active {
			out[run.TurnID] = true
		}
		return out, nil
	}
}

// retireSeatSessions removes a retired seat's executor sessions, for the
// mailbox retirement that has established the seat's absence past its grace
// ([maintenance.SeatSessionRetirer]).
//
// BY THE SEAT'S ID: a session row is keyed on the handle, and the record the
// retirement holds may carry none (a mailbox the broker alone remembered). So
// the handle it names is used where there is one, and otherwise every handle
// holding sessions here is matched against the seat's id under the running
// company's organisation (ADR-0013). A handle the running company still gives
// an agent seat is left alone either way: under a renamed organisation the
// same handle is another seat, whose sessions these are now.
//
// UNKNOWN DELETES NOTHING: a node that cannot read the revision the fleet
// runs, or whose delete's outcome is unknown, answers an error, and the
// retirement is undone and retried.
func (e *Engine) retireSeatSessions(ctx context.Context, seat uuid.UUID, handle string,
	absentSince time.Time) error {

	c := e.core.Load()
	if c == nil || c.executorReader == nil || c.executorWriter == nil {
		return fmt.Errorf("engine: this node runs no executor log, so it cannot remove "+
			"retired seat %s's sessions", seat)
	}
	company, err := e.fleetCompany(ctx, e.Company())
	if err != nil {
		return fmt.Errorf("engine: retired seat %s's sessions are kept until this node "+
			"applies the revision the fleet runs: %w", seat, err)
	}
	present := map[string]bool{}
	for _, s := range company.Seats() {
		present[s.Handle] = true
	}
	handles := []string{handle}
	if handle == "" {
		held, err := c.executorReader.Handles(ctx)
		if err != nil {
			return fmt.Errorf("engine: list the seats holding executor sessions: %w", err)
		}
		handles = handles[:0]
		for _, h := range held {
			if id, ok := org.DeriveAgentID(company.Org.Name, h); ok && id == seat {
				handles = append(handles, h)
			}
		}
	}
	for _, h := range handles {
		if present[h] {
			continue
		}
		res, err := c.executorWriter.DeleteSeat(ctx, executordomain.Call{
			By: seatRetirement, OpID: executordomain.SeatDeleteOpID(absentSince, h),
		}, h)
		if err != nil {
			return fmt.Errorf("engine: remove retired seat %s's sessions: %w", h, err)
		}
		if res.Outcome == statelog.OutcomeUnknown {
			return fmt.Errorf("engine: whether retired seat %s's sessions were removed is "+
				"unknown (operation %s); the next sweep asks again under it", h, res.OpID)
		}
	}
	return nil
}
